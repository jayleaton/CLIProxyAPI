package auth

import (
	"context"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func soonestResetClaudeAuth(id string, signals map[string]string) *Auth {
	return &Auth{ID: id, Provider: "claude", Quota: QuotaState{Signals: signals}}
}

func unixSignal(t time.Time) string {
	return strconv.FormatInt(t.Unix(), 10)
}

func TestSoonestResetSelector_PrefersEarliestWeeklyReset(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	selector := &SoonestResetSelector{nowFunc: func() time.Time { return now }}
	auths := []*Auth{
		soonestResetClaudeAuth("a-late", map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Reset":       unixSignal(now.Add(72 * time.Hour)),
			"Anthropic-Ratelimit-Unified-7d-Utilization": "0.10",
			"Anthropic-Ratelimit-Unified-5h-Reset":       unixSignal(now.Add(10 * time.Minute)),
		}),
		soonestResetClaudeAuth("b-soon", map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Reset":       unixSignal(now.Add(5 * time.Hour)),
			"Anthropic-Ratelimit-Unified-7d-Utilization": "0.60",
			"Anthropic-Ratelimit-Unified-5h-Reset":       unixSignal(now.Add(4 * time.Hour)),
		}),
		soonestResetClaudeAuth("c-unknown", nil),
	}

	got, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got.ID != "b-soon" {
		t.Fatalf("Pick() = %s, want b-soon", got.ID)
	}
}

func TestSoonestResetSelector_SkipsExhaustedWindowAndFallsBackToUnknown(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	selector := &SoonestResetSelector{nowFunc: func() time.Time { return now }}
	auths := []*Auth{
		soonestResetClaudeAuth("a-exhausted", map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Reset":  unixSignal(now.Add(time.Hour)),
			"Anthropic-Ratelimit-Unified-5h-Reset":  unixSignal(now.Add(30 * time.Minute)),
			"Anthropic-Ratelimit-Unified-5h-Status": "rejected",
		}),
		soonestResetClaudeAuth("c-unknown", nil),
		soonestResetClaudeAuth("b-unknown", nil),
	}

	got, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got.ID != "b-unknown" {
		t.Fatalf("Pick() = %s, want b-unknown (unknown credentials in ID order)", got.ID)
	}
}

func TestSoonestResetSelector_RolledOverWindowIsUnknown(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	selector := &SoonestResetSelector{nowFunc: func() time.Time { return now }}
	auths := []*Auth{
		// Every observed window already reset, so the stale rejection must not count.
		soonestResetClaudeAuth("a-rolled", map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Reset":  unixSignal(now.Add(-time.Minute)),
			"Anthropic-Ratelimit-Unified-7d-Status": "rejected",
		}),
		soonestResetClaudeAuth("b-active", map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Reset": unixSignal(now.Add(100 * time.Hour)),
		}),
	}

	got, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got.ID != "b-active" {
		t.Fatalf("Pick() = %s, want b-active", got.ID)
	}
	if rank := soonestResetRankFor(auths[0], now); rank.tier != soonestResetTierUnknown {
		t.Fatalf("rolled-over window tier = %d, want unknown", rank.tier)
	}
}

func TestSoonestResetSelector_TieBreaksOnSessionReset(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	weekly := unixSignal(now.Add(48 * time.Hour))
	selector := &SoonestResetSelector{nowFunc: func() time.Time { return now }}
	auths := []*Auth{
		soonestResetClaudeAuth("a", map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Reset": weekly,
			"Anthropic-Ratelimit-Unified-5h-Reset": unixSignal(now.Add(3 * time.Hour)),
		}),
		soonestResetClaudeAuth("b", map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Reset": weekly,
			"Anthropic-Ratelimit-Unified-5h-Reset": unixSignal(now.Add(time.Hour)),
		}),
	}

	got, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got.ID != "b" {
		t.Fatalf("Pick() = %s, want b", got.ID)
	}
}

func TestSoonestResetSelector_SkipsCoolingCredential(t *testing.T) {
	now := time.Now()
	selector := &SoonestResetSelector{}
	cooling := soonestResetClaudeAuth("a-cooling", map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Reset": unixSignal(now.Add(time.Hour)),
	})
	cooling.Quota.Exceeded = true
	cooling.Quota.Reason = "credential_quota"
	cooling.Quota.NextRecoverAt = now.Add(time.Hour)
	auths := []*Auth{cooling, soonestResetClaudeAuth("b-idle", nil)}

	got, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got.ID != "b-idle" {
		t.Fatalf("Pick() = %s, want b-idle", got.ID)
	}
}

func TestSoonestResetSelector_SessionAffinityKeepsBindingUntilUnavailable(t *testing.T) {
	now := time.Now()
	soon := soonestResetClaudeAuth("a-soon", map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Reset": unixSignal(now.Add(2 * time.Hour)),
	})
	later := soonestResetClaudeAuth("b-later", map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Reset": unixSignal(now.Add(50 * time.Hour)),
	})
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &SoonestResetSelector{},
		TTL:      time.Hour,
	})
	defer selector.Stop()
	headers := http.Header{}
	headers.Set("X-Claude-Code-Session-Id", "11111111-2222-3333-4444-555555555555")
	opts := cliproxyexecutor.Options{Headers: headers}

	first, err := selector.Pick(context.Background(), "claude", "claude-sonnet", opts, []*Auth{soon, later})
	if err != nil || first.ID != "a-soon" {
		t.Fatalf("first Pick() = %v, %v; want a-soon", first, err)
	}

	// A credential with an even sooner reset appears; the bound session must not move.
	sooner := soonestResetClaudeAuth("0-sooner", map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Reset": unixSignal(now.Add(time.Hour)),
	})
	second, err := selector.Pick(context.Background(), "claude", "claude-sonnet", opts, []*Auth{sooner, soon, later})
	if err != nil || second.ID != "a-soon" {
		t.Fatalf("second Pick() = %v, %v; want a-soon (sticky)", second, err)
	}

	// The bound credential is exhausted; fail over to the next soonest reset.
	soon.Quota.Exceeded = true
	soon.Quota.Reason = "credential_quota"
	soon.Quota.NextRecoverAt = now.Add(time.Hour)
	third, err := selector.Pick(context.Background(), "claude", "claude-sonnet", opts, []*Auth{sooner, soon, later})
	if err != nil || third.ID != "0-sooner" {
		t.Fatalf("third Pick() = %v, %v; want 0-sooner after failover", third, err)
	}
}

func TestManagerSoonestResetFollowsObservedQuotaHeaders(t *testing.T) {
	now := time.Now()
	model := "claude-soonest-reset-" + uuid.NewString()
	resets := map[string]time.Time{
		"soonest-a": now.Add(50 * time.Hour),
		"soonest-b": now.Add(2 * time.Hour),
	}
	var exhausted atomic.Bool
	executor := &claudeCancellationTestExecutor{}
	executor.executeFn = func(ctx context.Context, auth *Auth) (cliproxyexecutor.Response, error) {
		headers := http.Header{}
		headers.Set("Anthropic-Ratelimit-Unified-7d-Reset", unixSignal(resets[auth.ID]))
		if auth.ID == "soonest-b" && exhausted.Load() {
			headers.Set("Anthropic-Ratelimit-Unified-7d-Status", "rejected")
		}
		// Real executors publish upstream headers through the request context.
		internallogging.SetResponseHeaders(ctx, headers)
		return cliproxyexecutor.Response{Payload: []byte(auth.ID), Headers: headers}, nil
	}
	manager := NewManager(nil, &SoonestResetSelector{}, NoopHook{})
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(executor)
	for id, reset := range resets {
		auth := &Auth{
			ID:         id,
			Provider:   "claude",
			Attributes: map[string]string{"auth_kind": "oauth"},
			Metadata:   map[string]any{"access_token": "access-token"},
			Quota: QuotaState{Signals: map[string]string{
				"Anthropic-Ratelimit-Unified-7d-Reset": unixSignal(reset),
			}},
		}
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: model}})
		authID := id
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", id, errRegister)
		}
	}

	execute := func() string {
		t.Helper()
		resp, errExecute := manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
		if errExecute != nil {
			t.Fatalf("Execute() error = %v", errExecute)
		}
		return string(resp.Payload)
	}

	if got := execute(); got != "soonest-b" {
		t.Fatalf("first Execute() used %s, want soonest-b", got)
	}
	// The next response reports the weekly window as rejected; selection must move on.
	exhausted.Store(true)
	if got := execute(); got != "soonest-b" {
		t.Fatalf("second Execute() used %s, want soonest-b", got)
	}
	if got := execute(); got != "soonest-a" {
		t.Fatalf("third Execute() used %s, want soonest-a after exhaustion was observed", got)
	}
}
