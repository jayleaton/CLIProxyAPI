package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// ErrQuotaUsageUnsupported reports that a credential has no usage endpoint (for example an
// API-key credential), so it is never probed again for the lifetime of the probe entry.
var ErrQuotaUsageUnsupported = errors.New("quota usage endpoint not supported for credential")

// QuotaUsageFetcher is implemented by executors that can read a credential's live quota
// windows from a usage endpoint that does not consume model quota.
type QuotaUsageFetcher interface {
	// FetchQuotaUsage returns the credential's quota windows expressed as the upstream
	// response headers an inference call would carry, so the result feeds the same passive
	// snapshot (Auth.Quota.Signals) that response observation fills.
	FetchQuotaUsage(ctx context.Context, auth *Auth) (http.Header, error)
}

const (
	// quotaUsageProbeTick is how often the probe loop looks for stale credentials.
	quotaUsageProbeTick = time.Minute
	// quotaUsageStaleAfter is the snapshot age after which a credential is probed. Credentials
	// carrying traffic refresh their snapshot from response headers and are never probed.
	quotaUsageStaleAfter = 5 * time.Minute
	// quotaUsageMaxBackoff caps the retry delay after consecutive probe failures.
	quotaUsageMaxBackoff = time.Hour
	// quotaUsageProbeConcurrency bounds concurrent usage fetches per tick so large credential
	// pools do not burst the provider usage endpoints.
	quotaUsageProbeConcurrency = 4
)

type quotaProbeEntry struct {
	nextAt   time.Time
	failures int
	inFlight bool
}

// selectorUsesQuotaReset reports whether selection ranks credentials by quota reset, which is
// the only consumer of probed usage.
func selectorUsesQuotaReset(selector Selector) bool {
	switch typed := selector.(type) {
	case *SoonestResetSelector:
		return true
	case *SessionAffinitySelector:
		return typed != nil && selectorUsesQuotaReset(typed.fallback)
	default:
		return false
	}
}

// runQuotaUsageProbe periodically refreshes the quota snapshot of credentials that carry no
// recent observation, so soonest-reset can rank idle accounts by their real windows. It runs
// beside the auto-refresh loop and never touches the request path.
func (m *Manager) runQuotaUsageProbe(ctx context.Context) {
	ticker := time.NewTicker(quotaUsageProbeTick)
	defer ticker.Stop()
	for {
		m.probeQuotaUsage(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// probeQuotaUsage starts one asynchronous usage fetch per stale credential and returns
// without waiting for them. The returned WaitGroup completes when those fetches finish.
func (m *Manager) probeQuotaUsage(ctx context.Context, now time.Time) *sync.WaitGroup {
	var wg sync.WaitGroup
	if m == nil || !selectorUsesQuotaReset(m.Selector()) {
		return &wg
	}
	type probeTarget struct {
		auth    *Auth
		fetcher QuotaUsageFetcher
	}
	var targets []probeTarget
	m.mu.RLock()
	for _, auth := range m.auths {
		if auth == nil || auth.Disabled || auth.Status == StatusDisabled {
			continue
		}
		if !auth.Quota.ObservedAt.IsZero() && now.Sub(auth.Quota.ObservedAt) < quotaUsageStaleAfter {
			continue
		}
		executor, ok := m.executorLocked(executorKeyFromAuth(auth))
		if !ok {
			continue
		}
		fetcher, ok := executor.(QuotaUsageFetcher)
		if !ok {
			continue
		}
		targets = append(targets, probeTarget{auth: auth.Clone(), fetcher: fetcher})
	}
	m.mu.RUnlock()

	slots := make(chan struct{}, quotaUsageProbeConcurrency)
	for _, target := range targets {
		if !m.beginQuotaProbe(target.auth.ID, now) {
			continue
		}
		wg.Add(1)
		go func(auth *Auth, fetcher QuotaUsageFetcher) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			headers, errFetch := fetcher.FetchQuotaUsage(ctx, auth)
			// The probe start time stamps the snapshot, so any response observed while the
			// fetch was in flight is newer and keeps precedence.
			applied := errFetch == nil && m.applyQuotaUsage(auth.ID, auth.Provider, headers, now)
			m.finishQuotaProbe(auth.ID, now, errFetch, applied)
		}(target.auth, target.fetcher)
	}
	return &wg
}

func (m *Manager) beginQuotaProbe(authID string, now time.Time) bool {
	m.quotaProbeMu.Lock()
	defer m.quotaProbeMu.Unlock()
	if m.quotaProbes == nil {
		m.quotaProbes = make(map[string]*quotaProbeEntry)
	}
	entry := m.quotaProbes[authID]
	if entry == nil {
		entry = &quotaProbeEntry{}
		m.quotaProbes[authID] = entry
	}
	if entry.inFlight || now.Before(entry.nextAt) {
		return false
	}
	entry.inFlight = true
	return true
}

// finishQuotaProbe schedules the next probe: the stale interval after a success, exponential
// backoff after failures, and the maximum backoff for credentials without a usage endpoint.
func (m *Manager) finishQuotaProbe(authID string, now time.Time, errFetch error, applied bool) {
	m.quotaProbeMu.Lock()
	defer m.quotaProbeMu.Unlock()
	entry := m.quotaProbes[authID]
	if entry == nil {
		return
	}
	entry.inFlight = false
	switch {
	case errors.Is(errFetch, ErrQuotaUsageUnsupported):
		entry.nextAt = now.Add(quotaUsageMaxBackoff)
	case errFetch != nil || !applied:
		entry.failures++
		backoff := quotaUsageStaleAfter << min(entry.failures-1, 8)
		if backoff > quotaUsageMaxBackoff {
			backoff = quotaUsageMaxBackoff
		}
		entry.nextAt = now.Add(backoff)
		if errFetch != nil && !errors.Is(errFetch, context.Canceled) {
			log.Debugf("quota usage probe failed for auth %s (retry in %s): %v", authID, backoff, errFetch)
		}
	default:
		entry.failures = 0
		entry.nextAt = now.Add(quotaUsageStaleAfter)
	}
}

// applyQuotaUsage folds fetched usage into the live credential unless a newer snapshot exists.
func (m *Manager) applyQuotaUsage(authID, provider string, headers http.Header, fetchedAt time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	auth := m.auths[authID]
	if auth == nil || auth.Provider != provider {
		return false
	}
	if !auth.Quota.ObservedAt.IsZero() && !fetchedAt.After(auth.Quota.ObservedAt) {
		return true
	}
	return auth.Quota.ObserveResponseHeadersForProvider(provider, headers, fetchedAt)
}

// forgetQuotaProbe drops probe state for a removed credential.
func (m *Manager) forgetQuotaProbe(authID string) {
	m.quotaProbeMu.Lock()
	delete(m.quotaProbes, authID)
	m.quotaProbeMu.Unlock()
}
