package nntp

import (
	"sync"
	"time"
)

// Default bounds for the per-provider negative cache (NS-1.1).
const (
	// defaultNegCacheMaxEntries bounds memory. A single dead post can
	// contribute one entry per segment (thousands for a large file), so
	// this is a real bound, not a formality.
	defaultNegCacheMaxEntries = 50000
)

// negativeCache remembers message-ids that a provider has definitively
// answered it does not carry, so a repeat demand for the same dead segment
// short-circuits instead of spending another BODY command on a certain
// failure.
//
// Scope is in-memory and per-process by operator decision: the cache dies
// with a restart, which costs exactly one re-fetch per segment afterwards.
// Nothing is persisted and no migration is involved.
//
// Keying: one negativeCache instance belongs to one provider (one account,
// one host), and the instance is shared by every Pool that provider owns —
// the direct pool plus the segment cache's demand and readahead pools —
// so a dead segment discovered by a demand fetch is immediately known to
// readahead. The (provider, message-id) key space named by NS-1.1 is
// therefore realized as "which instance you ask" plus "message-id"; when
// NS-1.2 adds a second provider rung it gets its own instance, and the two
// never share verdicts.
//
// Correctness argument: an entry is only ever written from a definitive
// 430/423 answer from the provider itself (ErrArticleMissing), never from
// a transient failure, a timeout, or a heuristic. The cache can therefore
// only ever cause a skipped fetch of an article the provider has stated it
// does not have. The one stale case — the article reappears within the TTL
// — costs a delayed recovery bounded by the TTL, never a wrong answer to
// the client and never corrupt bytes. The cache never claims an article is
// PRESENT, so it cannot short-circuit a fetch into false success.
//
// A nil *negativeCache is a valid disabled cache: every method is
// nil-receiver safe, so call sites need no branch.
type negativeCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	entries map[string]time.Time

	// now is injectable so TTL expiry is testable without sleeping.
	now func() time.Time

	hits   uint64
	writes uint64
}

// newNegativeCache returns a negative cache with the given TTL, or nil
// (disabled, behavior identical to before NS-1.1) when ttl <= 0.
func newNegativeCache(ttl time.Duration, max int) *negativeCache {
	if ttl <= 0 {
		return nil
	}
	if max <= 0 {
		max = defaultNegCacheMaxEntries
	}
	return &negativeCache{
		ttl:     ttl,
		max:     max,
		entries: make(map[string]time.Time),
		now:     time.Now,
	}
}

// Has reports whether messageID is known-missing and not yet expired. An
// expired entry is dropped on read, so a lookup after the TTL behaves
// exactly as if the article had never been recorded.
func (n *negativeCache) Has(messageID string) bool {
	if n == nil || messageID == "" {
		return false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	exp, ok := n.entries[messageID]
	if !ok {
		return false
	}
	if !n.now().Before(exp) {
		delete(n.entries, messageID)
		return false
	}
	n.hits++
	return true
}

// Put records a definitive missing-article verdict for messageID.
func (n *negativeCache) Put(messageID string) {
	if n == nil || messageID == "" {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.entries) >= n.max {
		n.evictLocked()
	}
	n.entries[messageID] = n.now().Add(n.ttl)
	n.writes++
}

// evictLocked drops expired entries first, then — only if still at the
// bound — arbitrary live entries down to a low-water mark. Evicting a live
// entry is safe by construction: it costs one redundant fetch, never a
// wrong result.
func (n *negativeCache) evictLocked() {
	now := n.now()
	for k, exp := range n.entries {
		if !now.Before(exp) {
			delete(n.entries, k)
		}
	}
	low := n.max * 9 / 10
	for k := range n.entries {
		if len(n.entries) <= low {
			break
		}
		delete(n.entries, k)
	}
}

// Stats reports current size and lifetime counters, for NS-1.3 accounting
// and live-gate evidence.
func (n *negativeCache) Stats() (entries int, hits, writes uint64) {
	if n == nil {
		return 0, 0, 0
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.entries), n.hits, n.writes
}
