package rangecache

// TS-1.4: budgeted hot-head pinned tier gates.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// fakeChunkSource is a minimal in-memory ChunkSource for pinned-tier tests.
// Each Fetch call increments fetches[ref.Index] so a test can prove a
// pinned chunk is served from disk on the second call rather than
// refetched from origin.
type fakeChunkSource struct {
	key  string
	data [][]byte
	// mu guards fetches. Every pre-NS-5.5 test in this file only ever calls
	// Fetch sequentially, but NS-5.5's adaptive-readahead tests genuinely
	// drive concurrent Fetch calls (that is what they are proving/gating),
	// so this shared fixture needs to be safe under -race.
	mu      sync.Mutex
	fetches map[int]int
}

func newFakeChunkSource(key string, chunkSizes ...int) *fakeChunkSource {
	f := &fakeChunkSource{key: key, fetches: map[int]int{}}
	for i, n := range chunkSizes {
		b := make([]byte, n)
		for j := range b {
			b[j] = byte((i*97 + j*31 + 11) % 251)
		}
		f.data = append(f.data, b)
	}
	return f
}

func (f *fakeChunkSource) Key() string { return f.key }

func (f *fakeChunkSource) Chunks() []ChunkRef {
	refs := make([]ChunkRef, len(f.data))
	for i, d := range f.data {
		refs[i] = ChunkRef{Index: i, Key: fmt.Sprintf("c%d", i), Size: int64(len(d))}
	}
	return refs
}

func (f *fakeChunkSource) Fetch(_ context.Context, ref ChunkRef, _ FetchClass) ([]byte, error) {
	f.mu.Lock()
	f.fetches[ref.Index]++
	f.mu.Unlock()
	out := make([]byte, len(f.data[ref.Index]))
	copy(out, f.data[ref.Index])
	return out, nil
}

func (f *fakeChunkSource) ExactSizes() bool { return true }

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestGetChunkPinnedServesFromCacheWithoutRefetch proves a pinned chunk is
// fetched from origin exactly once, then served from disk on subsequent
// calls — the basic pinned-tier hit path.
func TestGetChunkPinnedServesFromCacheWithoutRefetch(t *testing.T) {
	c := New(Config{DiskCachePath: t.TempDir(), DiskCacheSizeMB: 8, PinnedBudgetMB: 8},
		testLogger())
	defer c.Close()

	src := newFakeChunkSource("pinned-src", 100, 100)
	ctx := context.Background()
	ref := src.Chunks()[0]

	if _, err := c.GetChunkPinned(ctx, src, ref); err != nil {
		t.Fatalf("first GetChunkPinned: %v", err)
	}
	if _, err := c.GetChunkPinned(ctx, src, ref); err != nil {
		t.Fatalf("second GetChunkPinned: %v", err)
	}
	if got := src.fetches[0]; got != 1 {
		t.Fatalf("expected exactly 1 origin fetch for a pinned+re-requested chunk, got %d", got)
	}

	c.mu.Lock()
	key := c.payloadKey(src, ref)
	entry, ok := c.diskEntries[key]
	c.mu.Unlock()
	if !ok || !entry.pinned {
		t.Fatalf("expected a pinned disk entry for the fetched chunk, got ok=%v pinned=%v", ok, entry.pinned)
	}
}

// TestPinnedEntriesExemptFromMainLRU proves the master-plan requirement
// directly: a pinned entry survives evictLRU even when the main disk
// budget is deliberately overrun by other (non-pinned) chunks.
func TestPinnedEntriesExemptFromMainLRU(t *testing.T) {
	// Small main budget (1 chunk's worth) but a generous pinned budget, so
	// the ordinary LRU sweep is guaranteed to run under pressure while the
	// pinned tier has plenty of room.
	c := New(Config{DiskCachePath: t.TempDir(), DiskCacheSizeMB: 1, PinnedBudgetMB: 64},
		testLogger())
	defer c.Close()

	pinnedSrc := newFakeChunkSource("pinned-src", 1000)
	ctx := context.Background()
	if _, err := c.GetChunkPinned(ctx, pinnedSrc, pinnedSrc.Chunks()[0]); err != nil {
		t.Fatalf("pin: %v", err)
	}

	// Push several large ordinary (non-pinned) chunks through the SAME
	// small main budget, forcing evictLRU to run repeatedly.
	ordinarySrc := newFakeChunkSource("ordinary-src", make([]int, 20)...)
	for i, ref := range ordinarySrc.Chunks() {
		ordinarySrc.data[i] = make([]byte, 100000)
		if _, err := c.GetChunk(ctx, ordinarySrc, ModeFull, ref); err != nil {
			t.Fatalf("ordinary chunk %d: %v", i, err)
		}
	}

	pinnedKey := c.payloadKey(pinnedSrc, pinnedSrc.Chunks()[0])
	c.mu.Lock()
	_, stillPresent := c.diskEntries[pinnedKey]
	c.mu.Unlock()
	if !stillPresent {
		t.Fatal("pinned entry was evicted by the ordinary LRU sweep — it must be exempt")
	}

	// And the pinned chunk must still be servable without a re-fetch.
	if _, err := c.GetChunkPinned(ctx, pinnedSrc, pinnedSrc.Chunks()[0]); err != nil {
		t.Fatalf("re-fetch pinned after LRU pressure: %v", err)
	}
	if got := pinnedSrc.fetches[0]; got != 1 {
		t.Fatalf("pinned chunk should never be refetched from origin, got %d fetches", got)
	}
}

// TestPinnedTierOwnBudgetEviction proves the pinned tier is itself bounded:
// pinning more than PinnedBudgetMB evicts the oldest pinned entries (never
// growing unboundedly), independent of the main disk budget.
func TestPinnedTierOwnBudgetEviction(t *testing.T) {
	c := New(Config{DiskCachePath: t.TempDir(), DiskCacheSizeMB: 64, PinnedBudgetMB: 1}, // 1 MiB pinned budget
		testLogger())
	defer c.Close()

	src := newFakeChunkSource("many-pins", 300000, 300000, 300000, 300000, 300000) // 5 x ~300KB > 1MiB budget
	ctx := context.Background()
	for _, ref := range src.Chunks() {
		if _, err := c.GetChunkPinned(ctx, src, ref); err != nil {
			t.Fatalf("pin chunk %d: %v", ref.Index, err)
		}
		time.Sleep(time.Millisecond) // ensure distinct lastUsed ordering
	}

	c.mu.Lock()
	pinnedBytes := c.pinnedSizeB
	c.mu.Unlock()
	budget := int64(1) * 1024 * 1024
	if pinnedBytes > budget {
		t.Fatalf("pinned tier grew past its own budget: %d bytes > %d budget", pinnedBytes, budget)
	}

	// The earliest-pinned chunk should have been evicted to make room.
	firstKey := c.payloadKey(src, src.Chunks()[0])
	c.mu.Lock()
	_, firstStillPresent := c.diskEntries[firstKey]
	c.mu.Unlock()
	if firstStillPresent {
		t.Fatal("expected the oldest pinned entry to be evicted once the pinned budget was exceeded")
	}
}

// TestPinningDisabledFallsBackToOrdinaryCache proves PinnedBudgetMB<=0
// disables pinning specifically: GetChunkPinned still fetches and caches,
// but through the ordinary (evictable) disk budget instead.
func TestPinningDisabledFallsBackToOrdinaryCache(t *testing.T) {
	c := New(Config{DiskCachePath: t.TempDir(), DiskCacheSizeMB: 8, PinnedBudgetMB: 0},
		testLogger())
	defer c.Close()

	src := newFakeChunkSource("no-pin-src", 100)
	ctx := context.Background()
	ref := src.Chunks()[0]
	if _, err := c.GetChunkPinned(ctx, src, ref); err != nil {
		t.Fatalf("GetChunkPinned with pinning disabled: %v", err)
	}

	key := c.payloadKey(src, ref)
	c.mu.Lock()
	entry, ok := c.diskEntries[key]
	pinnedBytes := c.pinnedSizeB
	c.mu.Unlock()
	if !ok {
		t.Fatal("expected the chunk to still be cached")
	}
	if entry.pinned {
		t.Fatal("expected entry.pinned=false when PinnedBudgetMB<=0")
	}
	if pinnedBytes != 0 {
		t.Fatalf("expected pinnedSizeB to stay 0 when pinning is disabled, got %d", pinnedBytes)
	}
}

// TestSweepOnceSkipsPinnedEntries proves the TTL sweeper never evicts a
// pinned entry even when its age exceeds every configured TTL.
func TestSweepOnceSkipsPinnedEntries(t *testing.T) {
	c := New(Config{
		DiskCachePath:   t.TempDir(),
		DiskCacheSizeMB: 8,
		PinnedBudgetMB:  8,
		DiskCacheTTLMin: 1,
		FullEvictMode:   "ttl",
		FullEvictTTLMin: 1,
	}, testLogger())
	defer c.Close()

	src := newFakeChunkSource("ttl-src", 100)
	ctx := context.Background()
	ref := src.Chunks()[0]
	if _, err := c.GetChunkPinned(ctx, src, ref); err != nil {
		t.Fatalf("pin: %v", err)
	}

	// Force every entry to look far older than any configured TTL.
	c.mu.Lock()
	for k, e := range c.diskEntries {
		e.lastUsed = time.Now().Add(-24 * time.Hour)
		c.diskEntries[k] = e
	}
	c.mu.Unlock()

	evicted := c.sweepOnce(time.Now())
	if evicted != 0 {
		t.Fatalf("expected the pinned entry to survive the TTL sweep, but sweepOnce evicted %d entries", evicted)
	}
	key := c.payloadKey(src, ref)
	c.mu.Lock()
	_, stillPresent := c.diskEntries[key]
	c.mu.Unlock()
	if !stillPresent {
		t.Fatal("pinned entry was removed by the TTL sweep")
	}
}

// TestPinnedStatusSurvivesRestart proves the durable ".pin" marker: a fresh
// Cache instance constructed over the same disk path (simulating a
// container restart) restores the pinned designation, not just the bytes —
// so a pinned hot-head entry stays exempt from the ordinary LRU/TTL sweep
// after a restart rather than silently reverting to an ordinary evictable
// entry.
func TestPinnedStatusSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	c1 := New(Config{DiskCachePath: dir, DiskCacheSizeMB: 8, PinnedBudgetMB: 8}, testLogger())

	src := newFakeChunkSource("restart-src", 100)
	ctx := context.Background()
	ref := src.Chunks()[0]
	if _, err := c1.GetChunkPinned(ctx, src, ref); err != nil {
		t.Fatalf("pin: %v", err)
	}
	key := c1.payloadKey(src, ref)
	c1.mu.Lock()
	before := c1.pinnedSizeB
	c1.mu.Unlock()
	if before == 0 {
		t.Fatal("expected pinnedSizeB > 0 before restart")
	}
	c1.Close()

	// Simulate a restart: a fresh Cache over the same DiskCachePath.
	c2 := New(Config{DiskCachePath: dir, DiskCacheSizeMB: 8, PinnedBudgetMB: 8}, testLogger())
	defer c2.Close()

	c2.mu.Lock()
	entry, ok := c2.diskEntries[key]
	afterPinnedSizeB := c2.pinnedSizeB
	c2.mu.Unlock()
	if !ok {
		t.Fatal("expected the restored entry to still be present after restart")
	}
	if !entry.pinned {
		t.Fatal("expected the restored entry to still be marked pinned after restart")
	}
	if afterPinnedSizeB != before {
		t.Fatalf("pinnedSizeB after restart = %d, want %d", afterPinnedSizeB, before)
	}

	// And it must still be exempt from the ordinary LRU sweep after restart.
	c2.evictLRU()
	c2.mu.Lock()
	_, stillPresent := c2.diskEntries[key]
	c2.mu.Unlock()
	if !stillPresent {
		t.Fatal("restored pinned entry was evicted by evictLRU — restart must preserve the exemption")
	}
}
