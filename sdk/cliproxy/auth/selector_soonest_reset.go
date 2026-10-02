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
// response headers. For Claude OAuth credentials the weekly (7d) window reset is the primary
// key because unused weekly quota is lost at reset; the 5h window reset breaks ties. Credentials
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
	unknown := soonestResetRank{tier: soonestResetTierUnknown}
	if auth == nil || len(auth.Quota.Signals) == 0 {
		return unknown
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") {
		return unknown
	}
	signals := auth.Quota.Signals
	weekly := claudeQuotaWindow(signals, "7d", now)
	session := claudeQuotaWindow(signals, "5h", now)
	if weekly.exhausted || session.exhausted {
		return soonestResetRank{tier: soonestResetTierExhausted}
	}
	switch {
	case !weekly.reset.IsZero():
		return soonestResetRank{
			tier:          soonestResetTierObserved,
			primaryReset:  weekly.reset,
			secondary:     session.reset,
			primaryUsedPc: weekly.used,
		}
	case !session.reset.IsZero():
		return soonestResetRank{
			tier:          soonestResetTierObserved,
			primaryReset:  session.reset,
			primaryUsedPc: session.used,
		}
	}
	if unified, ok := parseQuotaResetTime(quotaSignal(signals, "Anthropic-Ratelimit-Unified-Reset")); ok && unified.After(now) {
		status := strings.ToLower(quotaSignal(signals, "Anthropic-Ratelimit-Unified-Status"))
		if status == "rejected" {
			return soonestResetRank{tier: soonestResetTierExhausted}
		}
		return soonestResetRank{tier: soonestResetTierObserved, primaryReset: unified}
	}
	return unknown
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
