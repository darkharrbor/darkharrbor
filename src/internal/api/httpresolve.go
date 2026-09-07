package api

import (
	"context"
	"sync"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/httpstream"
)

// HS-3.8 (A19): coordinated cold resolve and forced refresh per
// (itemID,fileID). Without this, N simultaneous Jellyfin probes/plays for
// the same file — a common burst pattern (HEAD then GET, or several
// players/clients probing at once) — each independently call the protocol
// handler, multiplying backend load for zero benefit: every caller wants
// the identical answer.
//
// Design, per the audit's Finding 19 recommendation:
//   - Coalesce concurrent operations under one in-flight call per key.
//   - The in-flight call runs on a bounded, SERVER-owned context, never a
//     caller's own request context — an early-disconnecting caller must
//     never cancel the resolve for every other waiter. (The existing
//     debrid-lane resolveGroup in router.go has exactly this flaw —
//     "its leader uses the first request's context" per the audit; this
//     coordinator deliberately does not repeat it.)
//   - Each waiter can cancel independently: its own context ending stops
//     only that one wait, never the leader or other waiters.
//   - A generation counter makes forced refresh unambiguous. Forced refresh
//     supersedes a normal in-flight call; concurrent forced refreshes share
//     that same fresh call, so an HLS expiry burst cannot fan out. A slow
//     older call's eventual result can never clobber state a newer
//     generation already established.
//   - A short per-key failure cooldown absorbs Jellyfin retry bursts
//     against a genuinely failing backend without hammering it further.
//     Forced refresh always bypasses the cooldown — it is inherently an
//     explicit, singular caller intent, never a burst.
//
// HS-3.9 (A20) extends the same per-key entry with an expiry-aware cache of
// the single matched ResolvedFile, so a repeat play/seek within the file's
// own effective expiry window is a pure cache hit — no backend call, not
// even a coalesced one. Untrusted-input handling, per the audit:
//   - Effective TTL = min(file.ExpiresAt - skew, a handler-wide maximum) —
//     a backend that lies with a far-future expiresAt can never pin a
//     broken URL past the maximum.
//   - A past (or skew-exhausted) expiresAt is never cached at all — this
//     request's resolve still succeeds and streams, but nothing is stored
//     for reuse.
//   - A missing (zero) expiresAt gets a short, conservative TTL rather than
//     being treated as "cache forever" or "never cache" — IA and Stremio
//     never set one; the short TTL still saves a re-resolve for a rapid
//     seek burst on the same file without risking a long-lived staleness
//     window on a handler that supplies no signal at all.
//   - TTL is never extended on access: a cache hit returns the entry
//     unchanged, it does not push the expiry out.
//   - Header bytes are bounded before caching: an oversized proxyHeaders
//     set is still honored for the current request but is never retained.
//   - Total entries are bounded with best-effort eviction of already-
//     expired, idle entries when the map grows past the cap; no exchange
//     of correctness for a hard cap.
const (
	httpResolveOpTimeout       = 30 * time.Second
	httpResolveFailureCooldown = 5 * time.Second

	// httpResolveExpirySkew is subtracted from any backend-supplied
	// expiresAt before it is trusted as a cache boundary (A20 clock-skew
	// margin).
	httpResolveExpirySkew = 30 * time.Second
	// httpResolveMaxTTL bounds a valid expiresAt regardless of how far in
	// the future a backend claims it to be.
	httpResolveMaxTTL = 10 * time.Minute
	// httpResolveMissingTTL is used when a handler supplies no expiresAt at
	// all (IA, Stremio) — short and conservative, never indefinite.
	httpResolveMissingTTL = 60 * time.Second
	// httpResolveMaxHeaderBytes bounds the transient proxy-header set kept
	// in the cache; an oversized set is still used for the triggering
	// request but is never retained for reuse.
	httpResolveMaxHeaderBytes = 8 << 10
	// httpResolveMaxEntries is a soft cap: creating a new entry past this
	// count triggers a best-effort sweep of already-expired, idle entries.
	// Growth past the cap is never blocked — bounding memory must never
	// cost correctness.
	httpResolveMaxEntries = 4096
)

type httpResolveCoordinator struct {
	mu      sync.Mutex
	entries map[string]*httpResolveEntry
}

type httpResolveEntry struct {
	mu         sync.Mutex
	generation int64
	inflight   *httpResolveCall
	failedAt   time.Time
	failErr    error

	// cached* implement HS-3.9: a live-resolved file good until cachedUntil.
	// Populated only by the still-current generation's successful call;
	// never extended by a cache hit (access does not push cachedUntil out).
	cachedFile       httpstream.ResolvedFile
	cachedUntil      time.Time
	cachedGeneration int64
}

type httpResolveCall struct {
	generation int64
	forced     bool
	done       chan struct{}
	file       httpstream.ResolvedFile
	err        error
}

func newHTTPResolveCoordinator() *httpResolveCoordinator {
	return &httpResolveCoordinator{entries: make(map[string]*httpResolveEntry)}
}

func httpResolveEntryKey(itemID, fileID string) string { return itemID + ":" + fileID }

// Resolve coordinates one logical resolve/refresh for (itemID, fileID) and
// returns the single ResolvedFile matching selector. callerCtx governs only
// THIS caller's wait — cancelling it returns ctx.Err() to this caller alone
// and never affects the leader or any other waiter. forced=true starts a
// fresh call (bumping the generation) or joins the already-running forced
// call for this item/file. A caller identifying an already-replaced opaque
// generation reuses the newer cached answer; empty refresh contexts retain
// the legacy unconditional-force behavior. It never joins an older normal
// resolve.
func (c *httpResolveCoordinator) Resolve(
	callerCtx context.Context,
	itemID, fileID string,
	handler httpstream.Handler,
	key httpstream.ResolveKey,
	selector string,
	forced bool,
	refreshContext string,
) (httpstream.ResolvedFile, error) {
	entry := c.entryFor(itemID, fileID)

	entry.mu.Lock()
	// An HLS resource graph carries the opaque refresh context of the
	// generation from which it was derived. If that generation has already
	// been replaced, a late waiter must reuse the newer cached resolve
	// instead of starting another forced backend call. Empty contexts retain
	// the legacy "always force" behavior used by progressive streaming.
	if forced && refreshContext != "" &&
		!entry.cachedUntil.IsZero() && time.Now().Before(entry.cachedUntil) &&
		entry.cachedFile.RefreshContext != "" &&
		entry.cachedFile.RefreshContext != refreshContext {
		f := entry.cachedFile
		entry.mu.Unlock()
		return f, nil
	}
	if !forced {
		// HS-3.9 cache hit: never extended on access, just returned as-is.
		if !entry.cachedUntil.IsZero() && time.Now().Before(entry.cachedUntil) {
			f := entry.cachedFile
			entry.mu.Unlock()
			return f, nil
		}
		// A recent, still-cooling-down failure short-circuits new NORMAL
		// callers without hitting the backend again. Forced refresh always
		// bypasses this — it is a deliberate, singular retry intent, never
		// a burst.
		if entry.inflight == nil && !entry.failedAt.IsZero() &&
			time.Since(entry.failedAt) < httpResolveFailureCooldown {
			err := entry.failErr
			entry.mu.Unlock()
			return httpstream.ResolvedFile{}, err
		}
	}

	call := entry.inflight
	// A forced refresh supersedes a normal resolve already in flight, but
	// concurrent forced refreshes for the same item/file share one handler
	// call. HLS resources tend to expire as a group; fanning one refresh
	// out per segment would turn a single expiry into a backend storm.
	startNew := call == nil || (forced && !call.forced)
	if startNew {
		entry.generation++
		call = &httpResolveCall{generation: entry.generation, forced: forced, done: make(chan struct{})}
		entry.inflight = call
		if forced {
			entry.cachedUntil = time.Time{}
			entry.failedAt = time.Time{}
			entry.failErr = nil
		}
		go c.run(entry, call, handler, key, selector, forced, refreshContext)
	}
	entry.mu.Unlock()

	select {
	case <-call.done:
		return call.file, call.err
	case <-callerCtx.Done():
		return httpstream.ResolvedFile{}, callerCtx.Err()
	}
}

func (c *httpResolveCoordinator) entryFor(itemID, fileID string) *httpResolveEntry {
	k := httpResolveEntryKey(itemID, fileID)
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[k]
	if !ok {
		if len(c.entries) >= httpResolveMaxEntries {
			c.evictExpiredLocked()
		}
		e = &httpResolveEntry{}
		c.entries[k] = e
	}
	return e
}

// evictExpiredLocked drops entries that are idle (no in-flight call) and
// whose cache has already lapsed (or was never populated) and which are not
// currently cooling down from a failure. Called with c.mu held. Best-effort
// only — an entry that does not qualify is simply left in place; the cap is
// a memory-bounding target, never a correctness requirement.
func (c *httpResolveCoordinator) evictExpiredLocked() {
	now := time.Now()
	for k, e := range c.entries {
		e.mu.Lock()
		idle := e.inflight == nil
		staleCache := e.cachedUntil.IsZero() || !now.Before(e.cachedUntil)
		coolingDown := !e.failedAt.IsZero() && now.Sub(e.failedAt) < httpResolveFailureCooldown
		e.mu.Unlock()
		if idle && staleCache && !coolingDown {
			delete(c.entries, k)
		}
	}
}

// run executes the actual handler.Resolve + validate + selector-match on a
// bounded, server-owned context — detached from whichever caller happened
// to trigger it — so an early-disconnecting leader never kills the resolve
// for other waiters.
func (c *httpResolveCoordinator) run(
	entry *httpResolveEntry,
	call *httpResolveCall,
	handler httpstream.Handler,
	key httpstream.ResolveKey,
	selector string,
	forced bool,
	refreshContext string,
) {
	ctx, cancel := context.WithTimeout(context.Background(), httpResolveOpTimeout)
	defer cancel()

	op := httpstream.ResolveNormal
	if forced {
		op = httpstream.ResolveForceRefresh
	}
	files, err := handler.Resolve(ctx, httpstream.ResolveRequest{
		Key: key, Operation: op, RefreshContext: refreshContext,
	})
	var matched httpstream.ResolvedFile
	if err == nil {
		files, err = httpstream.ValidateResolvedFiles(files, time.Now())
	}
	if err == nil {
		matched, err = httpstream.MatchSelector(files, selector)
	}

	entry.mu.Lock()
	// Only the entry's currently-tracked call may clear entry.inflight — a
	// forced refresh that started a newer call while this one was still
	// running must not have this (now-superseded) call's completion wipe
	// out the newer call's slot.
	if entry.inflight == call {
		entry.inflight = nil
	}
	// Only the still-current generation may publish failure/success/cache
	// state — a slow, now-superseded call's outcome must never resurrect a
	// cooldown (or a cache entry) that a newer generation already settled,
	// and a failed preflight/stream response elsewhere invalidates only the
	// generation that failed (bumping entry.generation makes any older
	// call's late completion a no-op here).
	if call.generation == entry.generation {
		if err != nil {
			entry.failedAt = time.Now()
			entry.failErr = err
			entry.cachedUntil = time.Time{}
		} else {
			entry.failedAt = time.Time{}
			entry.failErr = nil
			if ttl, cacheable := httpResolveEffectiveTTL(matched.ExpiresAt, time.Now()); cacheable {
				entry.cachedFile = httpResolveCacheableCopy(matched)
				entry.cachedUntil = time.Now().Add(ttl)
				entry.cachedGeneration = call.generation
			} else {
				entry.cachedUntil = time.Time{}
			}
		}
	}
	entry.mu.Unlock()

	call.file, call.err = matched, err
	close(call.done)
}

// httpResolveEffectiveTTL implements the A20 untrusted-expiresAt contract:
// min(valid expiresAt - skew, handler-wide max); past/skew-exhausted values
// are never cached; a missing (zero) value gets a short conservative TTL.
func httpResolveEffectiveTTL(expiresAt time.Time, now time.Time) (ttl time.Duration, cacheable bool) {
	if expiresAt.IsZero() {
		return httpResolveMissingTTL, true
	}
	remaining := expiresAt.Sub(now) - httpResolveExpirySkew
	if remaining <= 0 {
		return 0, false
	}
	if remaining > httpResolveMaxTTL {
		remaining = httpResolveMaxTTL
	}
	return remaining, true
}

// httpResolveCacheableCopy bounds the transient header bytes retained in
// the cache (A20). An oversized proxyHeaders set is still used for the
// triggering request (the caller gets the original, unbounded `matched`
// value) but is never retained for reuse by a later cache hit.
func httpResolveCacheableCopy(f httpstream.ResolvedFile) httpstream.ResolvedFile {
	if httpHeaderBytes(f.RequestHeaders)+httpHeaderBytes(f.ResponseHeaders) > httpResolveMaxHeaderBytes {
		f.RequestHeaders = nil
		f.ResponseHeaders = nil
	}
	return f
}

func httpHeaderBytes(h map[string]string) int {
	n := 0
	for k, v := range h {
		n += len(k) + len(v)
	}
	return n
}

// Invalidate drops any cached entry and in-flight failure cooldown for
// (itemID,fileID) and bumps the generation, without itself triggering a new
// resolve. A caller that detects a play-time failure against an already-
// cached/resolved URL (HS-3.11's status-matrix forced-refresh path) uses
// this before calling Resolve(forced=true) so the next resolve is
// unambiguously a fresh generation. Safe to call even with a call in
// flight: the in-flight call's own generation is now stale and its
// eventual completion will no longer be allowed to publish (see run()).
func (c *httpResolveCoordinator) Invalidate(itemID, fileID string) {
	entry := c.entryFor(itemID, fileID)
	entry.mu.Lock()
	entry.generation++
	entry.cachedUntil = time.Time{}
	entry.failedAt = time.Time{}
	entry.failErr = nil
	entry.mu.Unlock()
}

// purge drops the coordinator entry for (itemID, fileID) — called on item
// deletion so memory does not grow unbounded across the library's lifetime
// and a recycled ID starts with a clean slate.
func (c *httpResolveCoordinator) purge(itemID, fileID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, httpResolveEntryKey(itemID, fileID))
}
