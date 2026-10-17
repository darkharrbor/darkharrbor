package api

import (
	"net/url"
	"sync"
	"time"
)

// cdnBreaker is a provider-wide circuit breaker for the torrent CDN.
//
// PERF-4's per-URL fail-fast (originRecentlyUnreachable) only remembers the
// exact URL that just failed. When the provider's edge drops this host
// entirely -- measured 2026-10-17: TCP to every *.tb-cdn.io node across two
// address ranges timed out for hours while ICMP and the Cloudflare-fronted
// API still answered -- every new stream, ffprobe relay and repair attempt
// still paid a fresh connect timeout against a fresh node, and each failure
// also fired TriggerRepairAsync. That kept a steady stream of doomed
// connections (and API re-adds) pointed at the provider for as long as it
// lasted.
//
// When connect-level failures hit cdnBreakerHosts distinct CDN hosts within
// cdnBreakerWindow, the breaker opens for cdnBreakerOpenFor: CDN attempts
// fail fast without dialling and without triggering per-item repair (the
// failure is not about any one item). After that it lets traffic through
// again; one success closes it, renewed failures re-open it.
type cdnBreaker struct {
	mu        sync.Mutex
	now       func() time.Time
	failures  map[string]time.Time // host -> last connect-level failure
	openUntil time.Time
	opened    int // times opened, for logging/tests
}

const (
	cdnBreakerWindow  = 2 * time.Minute
	cdnBreakerHosts   = 3
	cdnBreakerOpenFor = 2 * time.Minute
)

func newCDNBreaker(now func() time.Time) *cdnBreaker {
	if now == nil {
		now = time.Now
	}
	return &cdnBreaker{now: now, failures: map[string]time.Time{}}
}

func cdnHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// Open reports whether CDN attempts should currently fail fast. Nil-safe.
func (b *cdnBreaker) Open() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.now().Before(b.openUntil)
}

// NoteConnectFailure records a connect-level failure (the origin was never
// reached) for rawURL's host and reports whether this failure opened the
// breaker. Status-level failures must not be fed here: they prove the CDN
// is reachable.
func (b *cdnBreaker) NoteConnectFailure(rawURL string) (opened bool) {
	if b == nil {
		return false
	}
	host := cdnHost(rawURL)
	if host == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.failures[host] = now
	distinct := 0
	for h, t := range b.failures {
		if now.Sub(t) > cdnBreakerWindow {
			delete(b.failures, h)
			continue
		}
		distinct++
	}
	if distinct >= cdnBreakerHosts && !now.Before(b.openUntil) {
		b.openUntil = now.Add(cdnBreakerOpenFor)
		b.opened++
		return true
	}
	return false
}

// NoteSuccess records that a CDN origin answered; it closes the breaker and
// forgets recent failures.
func (b *cdnBreaker) NoteSuccess() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = map[string]time.Time{}
	b.openUntil = time.Time{}
}
