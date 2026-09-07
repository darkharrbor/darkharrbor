package rangecache

// Global in-flight payload coalescing (HR1.3; HTTP-plan HR-D12 "in-flight
// range coalescing", the next origin-shielding step after the resolve-call
// coalescing that already exists).
//
// Before this, a disk miss went straight to src.Fetch. Two callers that
// wanted the SAME payload at the same time therefore caused TWO upstream
// reads: a demand read racing its own readahead, a second player joining a
// stream mid-flight, or — since NS-0.3 gave payloads a global identity —
// two different items whose NZBs share an article. The disk cache only
// deduplicated AFTER the first fetch had already completed, so a
// simultaneous burst was never shielded.
//
// Coalescing is keyed by the same payloadKey the disk cache uses, so
// eligibility is exactly "these callers want identical bytes". The design
// follows the httpResolveCoordinator precedent (HS-3.8) rather than the
// older resolveGroup flaw the audit called out:
//
//   - The leader's fetch runs on a CACHE-owned context, never on the
//     context of whichever caller happened to arrive first. One caller
//     disconnecting can never cancel the fetch every other caller is
//     waiting on.
//   - Each waiter cancels independently: its own context ending returns
//     ctx.Err() to that caller alone and leaves the fetch running for the
//     others.
//   - The work is cancelled only when the LAST waiter abandons it, so a
//     disconnect cleans up promptly instead of stranding an orphan fetch
//     (the frozen plan's "disconnect cleanup" gate).
//   - A demand caller may join an in-flight readahead fetch. That never
//     starves demand: it consumes no additional provider capacity and
//     returns sooner than a fresh fetch would, while the refcount above
//     guarantees the readahead requester leaving cannot cancel work a
//     demand caller is still waiting on. Pool-level priority remains
//     HR1.4's accountgov lease, which is unchanged here.
//
// Each waiter receives its own copy of the payload; only the leader gets the
// fetched slice itself. That preserves today's guarantee exactly — every
// caller already owned its own buffer, since a disk hit re-reads the file
// and a miss allocates per fetch — and it is strictly less memory than the
// status quo, which allocated one buffer PER redundant upstream fetch.

import (
	"context"
)

// inflightFetch is one upstream fetch shared by every caller wanting the
// same payload identity.
type inflightFetch struct {
	done   chan struct{}
	cancel context.CancelFunc

	// waiters and demandJoined are guarded by Cache.inflightMu.
	waiters      int
	demandJoined bool

	// data and err are written once, before done is closed, and read only
	// after done is closed.
	data []byte
	err  error
}

// fetchCoalesced performs src.Fetch for ref, collapsing concurrent callers
// that want the same payload into a single upstream read. It returns the
// payload, an error, and whether this caller led the fetch (the leader owns
// persisting the result).
func (c *Cache) fetchCoalesced(ctx context.Context, src ChunkSource, ref ChunkRef, key string, class FetchClass) ([]byte, error, bool) {
	c.inflightMu.Lock()
	if e, ok := c.inflight[key]; ok {
		e.waiters++
		if class == FetchDemand && !e.demandJoined {
			e.demandJoined = true
			c.demandJoins.Add(1)
		}
		c.inflightMu.Unlock()
		c.coalescedJoins.Add(1)
		data, err := c.awaitInflight(ctx, key, e)
		if err != nil {
			return nil, err, false
		}
		// Follower copy: never hand two callers the same mutable slice.
		return append([]byte(nil), data...), nil, false
	}

	fetchCtx, cancel := context.WithCancel(c.fetchCtx)
	e := &inflightFetch{
		done:         make(chan struct{}),
		cancel:       cancel,
		waiters:      1,
		demandJoined: class == FetchDemand,
	}
	c.inflight[key] = e
	c.inflightMu.Unlock()

	go func() {
		data, err := src.Fetch(fetchCtx, ref, class)
		if data == nil && err == nil && class == FetchReadahead {
			// Readahead pool unavailable ((nil,nil) contract); fall back to
			// a demand fetch, exactly as the uncoalesced path did.
			data, err = src.Fetch(fetchCtx, ref, FetchDemand)
		}
		e.data, e.err = data, err
		// Deliberately NOT removed from the map here. The entry is released
		// by the last waiter instead, which keeps it joinable across the
		// window between the bytes arriving and the leader persisting them
		// to disk. Removing it here left that gap uncovered, so a caller
		// arriving mid-window found neither an in-flight entry nor a disk
		// entry and started a second upstream read for the same payload —
		// exactly what this row exists to prevent.
		close(e.done)
		cancel()
	}()

	data, err := c.awaitInflight(ctx, key, e)
	if err != nil {
		return nil, err, false
	}
	return data, nil, true
}

// awaitInflight waits for the shared fetch or for this caller's own context
// to end, whichever comes first. Abandoning never affects another waiter.
func (c *Cache) awaitInflight(ctx context.Context, key string, e *inflightFetch) ([]byte, error) {
	select {
	case <-e.done:
		c.releaseWaiter(key, e, false)
		if e.err != nil {
			return nil, e.err
		}
		return e.data, nil
	case <-ctx.Done():
		c.releaseWaiter(key, e, true)
		return nil, ctx.Err()
	}
}

// releaseWaiter drops this caller's claim. When the last waiter abandons an
// unfinished fetch, the shared work is cancelled so a disconnect does not
// strand an orphan upstream read.
// releaseWaiter drops this caller's claim. The entry lives exactly as long as
// someone references it: the last waiter to leave removes it from the map, so
// a completed result stays joinable until nobody wants it, and an abandoned
// fetch is cancelled rather than left running for no one.
func (c *Cache) releaseWaiter(key string, e *inflightFetch, abandoned bool) {
	c.inflightMu.Lock()
	e.waiters--
	last := e.waiters <= 0
	if last {
		if cur, still := c.inflight[key]; still && cur == e {
			delete(c.inflight, key)
		}
	}
	c.inflightMu.Unlock()
	if abandoned && last {
		c.coalescedAborts.Add(1)
		e.cancel()
	}
}

// inflightLen reports the number of distinct payloads currently being
// fetched. Test and health evidence only.
func (c *Cache) inflightLen() int {
	c.inflightMu.Lock()
	defer c.inflightMu.Unlock()
	return len(c.inflight)
}
