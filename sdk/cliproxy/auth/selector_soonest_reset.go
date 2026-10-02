package auth

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// SoonestResetSelector prefers the credential whose usage window resets soonest while it
// still has usage remaining, so quota that is about to expire is consumed first.
//
// Ranking uses the passive quota snapshot (Auth.Quota.Signals) captured from upstream
// response headers (Claude anthropic-ratelimit-unified-*, Codex x-codex-primary/secondary-*).
// The weekly window reset is the primary key because unused weekly quota is lost at reset;
// the 5h window reset breaks ties. Credentials
// without a usable observation (never used, provider without signals, or every observed reset
// already in the past) rank after observed credentials in deterministic ID order, so idle
// accounts are only started when no active window has headroom. Credentials whose observed
// window is exhausted rank last; cooldown normally removes them before selection.
//
// Combine with session affinity so a bound session never moves while its credential remains
// available.
type SoonestResetSelector struct {
	// nowFunc overrides the clock in tests.
	nowFunc func() time.Time
}

const (
	soonestResetTierObserved = iota
	soonestResetTierUnknown
	soonestResetTierExhausted
)

type soonestResetRank struct {
	tier          int
	primaryReset  time.Time
	secondary     time.Time
	primaryUsedPc float64
}

type quotaWindow struct {
	reset     time.Time
	used      float64
	hasUsed   bool
	exhausted bool
}

// Pick selects the available credential with the soonest observed quota reset.
func (s *SoonestResetSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	if s != nil && s.nowFunc != nil {
		now = s.nowFunc()
	}
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	if len(available) == 1 {
		return available[0], nil
	}
	ranks := make(map[string]soonestResetRank, len(available))
	for _, auth := range available {
		ranks[auth.ID] = soonestResetRankFor(auth, now)
	}
	ordered := append([]*Auth(nil), available...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return soonestResetLess(ranks[ordered[i].ID], ranks[ordered[j].ID], ordered[i].ID, ordered[j].ID)
	})
	return ordered[0], nil
}

func soonestResetLess(a, b soonestResetRank, aID, bID string) bool {
	if a.tier != b.tier {
		return a.tier < b.tier
	}
	if a.tier == soonestResetTierObserved {
		if !a.primaryReset.Equal(b.primaryReset) {
			return a.primaryReset.Before(b.primaryReset)
		}
		if !a.secondary.Equal(b.secondary) {
			if a.secondary.IsZero() || b.secondary.IsZero() {
				return !a.secondary.IsZero()
			}
			return a.secondary.Before(b.secondary)
		}
		if a.primaryUsedPc != b.primaryUsedPc {
			return a.primaryUsedPc < b.primaryUsedPc
		}
	}
	return aID < bID
}

// soonestResetRankFor derives a ranking from the credential-level quota snapshot.
func soonestResetRankFor(auth *Auth, now time.Time) soonestResetRank {
	if auth == nil || len(auth.Quota.Signals) == 0 {
		return soonestResetRank{tier: soonestResetTierUnknown}
	}
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude":
		return claudeSoonestResetRank(auth.Quota.Signals, now)
	case "codex":
		return codexSoonestResetRank(auth.Quota.Signals, auth.Quota.ObservedAt, now)
	default:
		return soonestResetRank{tier: soonestResetTierUnknown}
	}
}

// rankFromWindows orders by the long (weekly) window reset and breaks ties on the short
// (session) window reset. Either window may be absent.
func rankFromWindows(long, short quotaWindow) soonestResetRank {
	if long.exhausted || short.exhausted {
		return soonestResetRank{tier: soonestResetTierExhausted}
	}
	switch {
	case !long.reset.IsZero():
		return soonestResetRank{
			tier:          soonestResetTierObserved,
			primaryReset:  long.reset,
			secondary:     short.reset,
			primaryUsedPc: long.used,
		}
	case !short.reset.IsZero():
		return soonestResetRank{
			tier:          soonestResetTierObserved,
			primaryReset:  short.reset,
			primaryUsedPc: short.used,
		}
	}
	return soonestResetRank{tier: soonestResetTierUnknown}
}

func claudeSoonestResetRank(signals map[string]string, now time.Time) soonestResetRank {
	rank := rankFromWindows(claudeQuotaWindow(signals, "7d", now), claudeQuotaWindow(signals, "5h", now))
	if rank.tier != soonestResetTierUnknown {
		return rank
	}
	if unified, ok := parseQuotaResetTime(quotaSignal(signals, "Anthropic-Ratelimit-Unified-Reset")); ok && unified.After(now) {
		status := strings.ToLower(quotaSignal(signals, "Anthropic-Ratelimit-Unified-Status"))
		if status == "rejected" {
			return soonestResetRank{tier: soonestResetTierExhausted}
		}
		return soonestResetRank{tier: soonestResetTierObserved, primaryReset: unified}
	}
	return rank
}

// codexSoonestResetRank reads the credential-level Codex windows. Codex reports up to two
// windows ("primary", "secondary") whose length varies by plan, so the longer live window is
// treated as the weekly window. Additional per-model limits are ignored.
func codexSoonestResetRank(signals map[string]string, observedAt, now time.Time) soonestResetRank {
	var live []codexWindow
	for _, name := range []string{"Primary", "Secondary"} {
		if window, ok := readCodexQuotaWindow(signals, name, observedAt, now); ok {
			live = append(live, window)
		}
	}
	if len(live) == 0 {
		return soonestResetRank{tier: soonestResetTierUnknown}
	}
	limitReached := strings.EqualFold(quotaSignal(signals, "X-Codex-Limit-Reached"), "true") ||
		strings.EqualFold(quotaSignal(signals, "X-Codex-Allowed"), "false")
	if limitReached {
		return soonestResetRank{tier: soonestResetTierExhausted}
	}
	if len(live) == 1 {
		return rankFromWindows(live[0].quotaWindow, quotaWindow{})
	}
	long, short := live[0], live[1]
	if short.minutes > long.minutes {
		long, short = short, long
	}
	return rankFromWindows(long.quotaWindow, short.quotaWindow)
}

type codexWindow struct {
	quotaWindow
	minutes int64
}

// readCodexQuotaWindow reads one Codex window. Reset-At is absolute; Reset-After-Seconds is
// relative to when the snapshot was observed. Windows that already reset are skipped.
func readCodexQuotaWindow(signals map[string]string, name string, observedAt, now time.Time) (codexWindow, bool) {
	prefix := "X-Codex-" + name + "-"
	reset, ok := parseQuotaResetTime(quotaSignal(signals, prefix+"Reset-At"))
	if !ok && !observedAt.IsZero() {
		if after, errParse := strconv.ParseFloat(quotaSignal(signals, prefix+"Reset-After-Seconds"), 64); errParse == nil && after >= 0 {
			reset, ok = observedAt.Add(time.Duration(after*float64(time.Second))), true
		}
	}
	if !ok || !reset.After(now) {
		return codexWindow{}, false
	}
	window := codexWindow{quotaWindow: quotaWindow{reset: reset}}
	if used, errParse := strconv.ParseFloat(quotaSignal(signals, prefix+"Used-Percent"), 64); errParse == nil && !math.IsNaN(used) && !math.IsInf(used, 0) {
		window.used = used / 100
		window.hasUsed = true
		window.exhausted = window.used >= 1
	}
	if minutes, errParse := strconv.ParseInt(quotaSignal(signals, prefix+"Window-Minutes"), 10, 64); errParse == nil {
		window.minutes = minutes
	}
	return window, true
}

// claudeQuotaWindow reads one Anthropic unified window. A window whose reset already passed
// has rolled over, so its stale utilization and status are ignored.
func claudeQuotaWindow(signals map[string]string, name string, now time.Time) quotaWindow {
	prefix := "Anthropic-Ratelimit-Unified-" + name + "-"
	reset, ok := parseQuotaResetTime(quotaSignal(signals, prefix+"Reset"))
	if !ok || !reset.After(now) {
		return quotaWindow{}
	}
	window := quotaWindow{reset: reset}
	if raw := quotaSignal(signals, prefix+"Utilization"); raw != "" {
		if used, errParse := strconv.ParseFloat(raw, 64); errParse == nil && !math.IsNaN(used) && !math.IsInf(used, 0) {
			window.used = used
			window.hasUsed = true
		}
	}
	status := strings.ToLower(quotaSignal(signals, prefix+"Status"))
	window.exhausted = status == "rejected" || (window.hasUsed && window.used >= 1)
	return window
}

func quotaSignal(signals map[string]string, name string) string {
	if value, ok := signals[http.CanonicalHeaderKey(name)]; ok {
		return strings.TrimSpace(value)
	}
	for key, value := range signals {
		if strings.EqualFold(key, name) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func parseQuotaResetTime(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	if sec, errParse := strconv.ParseFloat(raw, 64); errParse == nil && sec > 0 {
		whole := int64(sec)
		return time.Unix(whole, int64((sec-float64(whole))*1e9)), true
	}
	if parsed, errParse := time.Parse(time.RFC3339, raw); errParse == nil {
		return parsed, true
	}
	return time.Time{}, false
}
