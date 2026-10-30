package auth

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// quotaUsageTestExecutor answers every Execute with the selected auth ID and serves usage
// probes from fetchFn.
type quotaUsageTestExecutor struct {
	*claudeCancellationTestExecutor

	mu      sync.Mutex
	fetched []string
	fetchFn func(context.Context, *Auth) (http.Header, error)
}

func newQuotaUsageTestExecutor(fetchFn func(context.Context, *Auth) (http.Header, error)) *quotaUsageTestExecutor {
	base := &claudeCancellationTestExecutor{}
	base.executeFn = func(_ context.Context, auth *Auth) (cliproxyexecutor.Response, error) {
		return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
	}
	return &quotaUsageTestExecutor{claudeCancellationTestExecutor: base, fetchFn: fetchFn}
}

func (e *quotaUsageTestExecutor) FetchQuotaUsage(ctx context.Context, auth *Auth) (http.Header, error) {
	e.mu.Lock()
	e.fetched = append(e.fetched, auth.ID)
	e.mu.Unlock()
	return e.fetchFn(ctx, auth)
}

func (e *quotaUsageTestExecutor) fetchedIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	ids := append([]string(nil), e.fetched...)
	sort.Strings(ids)
	return ids
}

func claudeUsageHeaders(weeklyReset time.Time, weeklyUsed float64, sessionReset time.Time) http.Header {
	headers := http.Header{}
	headers.Set("Anthropic-Ratelimit-Unified-7d-Reset", unixSignal(weeklyReset))
	headers.Set("Anthropic-Ratelimit-Unified-7d-Utilization", strconv.FormatFloat(weeklyUsed, 'f', -1, 64))
	headers.Set("Anthropic-Ratelimit-Unified-5h-Reset", unixSignal(sessionReset))
	return headers
}

type quotaProbeFixture struct {
	manager  *Manager
	executor *quotaUsageTestExecutor
	model    string
}

func newQuotaProbeFixture(t *testing.T, selector Selector, fetchFn func(context.Context, *Auth) (http.Header, error), auths ...*Auth) *quotaProbeFixture {
	t.Helper()
	fixture := &quotaProbeFixture{
		manager:  NewManager(nil, selector, NoopHook{}),
		executor: newQuotaUsageTestExecutor(fetchFn),
		model:    "claude-quota-probe-" + uuid.NewString(),
	}
	fixture.manager.SetRetryConfig(0, 0, 0)
	fixture.manager.RegisterExecutor(fixture.executor)
	for _, auth := range auths {
		auth.Provider = "claude"
		auth.Attributes = map[string]string{"auth_kind": "oauth"}
		auth.Metadata = map[string]any{"access_token": "access-token"}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: fixture.model}})
		authID := auth.ID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
		if _, errRegister := fixture.manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}
	return fixture
}

func (f *quotaProbeFixture) execute(t *testing.T) string {
	t.Helper()
	resp, errExecute := f.manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: f.model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	return string(resp.Payload)
}

// Reproduces the reported routing: the only account the proxy has routed through keeps
// winning even though an idle account's weekly window resets sooner.
func TestQuotaUsageProbe_IdleAccountWithSoonerWeeklyResetWins(t *testing.T) {
	now := time.Now()
	usage := map[string]http.Header{
		"jayleaton":    claudeUsageHeaders(now.Add(22*time.Hour), 0.29, now.Add(3*time.Hour)),
		"jayleatonbot": claudeUsageHeaders(now.Add(5*24*time.Hour), 0, now.Add(5*time.Hour)),
	}
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &SoonestResetSelector{}, TTL: time.Hour})
	defer selector.Stop()
	fixture := newQuotaProbeFixture(t, selector,
		func(_ context.Context, auth *Auth) (http.Header, error) { return usage[auth.ID], nil },
		&Auth{ID: "oldbettiegaming", Quota: QuotaState{
			ObservedAt: now,
			Signals: map[string]string{
				"Anthropic-Ratelimit-Unified-7d-Reset":       unixSignal(now.Add(35 * time.Hour)),
				"Anthropic-Ratelimit-Unified-7d-Utilization": "0.94",
				"Anthropic-Ratelimit-Unified-5h-Reset":       unixSignal(now.Add(time.Hour)),
			},
		}},
		&Auth{ID: "jayleaton"},
		&Auth{ID: "jayleatonbot"},
	)

	if got := fixture.execute(t); got != "oldbettiegaming" {
		t.Fatalf("Execute() before probe used %s, want oldbettiegaming (only observed account)", got)
	}

	fixture.manager.probeQuotaUsage(context.Background(), now).Wait()

	if got := fixture.executor.fetchedIDs(); len(got) != 2 || got[0] != "jayleaton" || got[1] != "jayleatonbot" {
		t.Fatalf("probed %v, want only the idle accounts [jayleaton jayleatonbot]", got)
	}
	if got := fixture.execute(t); got != "jayleaton" {
		t.Fatalf("Execute() after probe used %s, want jayleaton (soonest weekly reset)", got)
	}
}

func TestQuotaUsageProbe_FailedFetchKeepsPassiveRankingAndBacksOff(t *testing.T) {
	now := time.Now()
	fixture := newQuotaProbeFixture(t, &SoonestResetSelector{},
		func(context.Context, *Auth) (http.Header, error) { return nil, errors.New("status 503") },
		// Stale but still-valid observation: the failed refresh must not discard it.
		&Auth{ID: "a-stale", Quota: QuotaState{
			ObservedAt: now.Add(-time.Hour),
			Signals:    map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": unixSignal(now.Add(40 * time.Hour))},
		}},
		&Auth{ID: "b-idle"},
	)

	fixture.manager.probeQuotaUsage(context.Background(), now).Wait()
	if got := fixture.executor.fetchedIDs(); len(got) != 2 {
		t.Fatalf("first probe fetched %v, want both stale credentials", got)
	}
	if got := fixture.execute(t); got != "a-stale" {
		t.Fatalf("Execute() used %s, want a-stale (passive ranking after failed probe)", got)
	}

	fixture.manager.probeQuotaUsage(context.Background(), now.Add(quotaUsageStaleAfter-time.Second)).Wait()
	if got := fixture.executor.fetchedIDs(); len(got) != 2 {
		t.Fatalf("probe inside backoff fetched %v, want no new fetches", got)
	}
	fixture.manager.probeQuotaUsage(context.Background(), now.Add(quotaUsageStaleAfter)).Wait()
	if got := fixture.executor.fetchedIDs(); len(got) != 4 {
		t.Fatalf("probe after backoff fetched %v, want a retry for both", got)
	}
	// The second consecutive failure doubles the delay.
	retryAt := now.Add(quotaUsageStaleAfter)
	fixture.manager.probeQuotaUsage(context.Background(), retryAt.Add(2*quotaUsageStaleAfter-time.Second)).Wait()
	if got := fixture.executor.fetchedIDs(); len(got) != 4 {
		t.Fatalf("probe inside doubled backoff fetched %v, want no new fetches", got)
	}
}

func TestQuotaUsageProbe_DoesNotBlockSelection(t *testing.T) {
	now := time.Now()
	started := make(chan struct{})
	release := make(chan struct{})
	fixture := newQuotaProbeFixture(t, &SoonestResetSelector{},
		func(ctx context.Context, _ *Auth) (http.Header, error) {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return claudeUsageHeaders(now.Add(time.Hour), 0.1, now.Add(time.Hour)), nil
		},
		&Auth{ID: "a-idle"},
	)

	probe := fixture.manager.probeQuotaUsage(context.Background(), now)
	<-started

	selected := make(chan string, 1)
	go func() {
		resp, errExecute := fixture.manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: fixture.model}, cliproxyexecutor.Options{})
		if errExecute != nil {
			selected <- "error: " + errExecute.Error()
			return
		}
		selected <- string(resp.Payload)
	}()
	select {
	case got := <-selected:
		if got != "a-idle" {
			t.Fatalf("Execute() during probe = %s, want a-idle", got)
		}
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("Execute() blocked on an in-flight usage probe")
	}

	// A second tick while the fetch is in flight must not start a duplicate.
	fixture.manager.probeQuotaUsage(context.Background(), now.Add(quotaUsageStaleAfter)).Wait()
	if got := fixture.executor.fetchedIDs(); len(got) != 1 {
		t.Fatalf("fetched %v, want a single in-flight probe", got)
	}
	close(release)
	probe.Wait()
}

func TestQuotaUsageProbe_ExhaustedAccountRanksLast(t *testing.T) {
	now := time.Now()
	usage := map[string]http.Header{
		"a-exhausted": claudeUsageHeaders(now.Add(2*time.Hour), 1, now.Add(time.Hour)),
		"b-headroom":  claudeUsageHeaders(now.Add(90*time.Hour), 0.5, now.Add(time.Hour)),
	}
	fixture := newQuotaProbeFixture(t, &SoonestResetSelector{},
		func(_ context.Context, auth *Auth) (http.Header, error) { return usage[auth.ID], nil },
		&Auth{ID: "a-exhausted"},
		&Auth{ID: "b-headroom"},
	)

	fixture.manager.probeQuotaUsage(context.Background(), now).Wait()
	if got := fixture.execute(t); got != "b-headroom" {
		t.Fatalf("Execute() used %s, want b-headroom (exhausted ranks last)", got)
	}
}

func TestQuotaUsageProbe_ResponseObservedDuringFetchWins(t *testing.T) {
	now := time.Now()
	fixture := newQuotaProbeFixture(t, &SoonestResetSelector{}, nil, &Auth{ID: "a"})
	fresh := claudeUsageHeaders(now.Add(10*time.Hour), 0.2, now.Add(time.Hour))
	fixture.manager.mu.Lock()
	fixture.manager.auths["a"].Quota.ObserveResponseHeadersForProvider("claude", fresh, now.Add(time.Second))
	fixture.manager.mu.Unlock()

	stale := claudeUsageHeaders(now.Add(99*time.Hour), 0.9, now.Add(time.Hour))
	if !fixture.manager.applyQuotaUsage("a", "claude", stale, now) {
		t.Fatal("applyQuotaUsage() = false, want true (newer snapshot kept)")
	}
	auth, _ := fixture.manager.GetByID("a")
	if got := auth.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Reset"]; got != unixSignal(now.Add(10*time.Hour)) {
		t.Fatalf("7d reset = %s, want the newer response observation", got)
	}
}

func TestQuotaUsageProbe_OnlyRunsForSoonestReset(t *testing.T) {
	fixture := newQuotaProbeFixture(t, &RoundRobinSelector{},
		func(context.Context, *Auth) (http.Header, error) { return http.Header{}, nil },
		&Auth{ID: "a-idle"},
	)
	fixture.manager.probeQuotaUsage(context.Background(), time.Now()).Wait()
	if got := fixture.executor.fetchedIDs(); len(got) != 0 {
		t.Fatalf("round-robin probe fetched %v, want none", got)
	}
}
