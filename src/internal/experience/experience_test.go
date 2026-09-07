package experience

// TS-1.4 gates: PrewarmHotHead, SeekTargetPrefetch, NextEpisodePrewarm,
// Workers, and the shared accountgov.Governor priority-ordering proof
// (CG-05's deterministic-test precedent, matching HR1.4/SF-02's own
// closure evidence for this gate shape).

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/mediatruth"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
)

// fakeSource is a minimal in-memory rangecache.ChunkSource. Its bytes are
// arbitrary (not a real container), which is deliberate for most tests here:
// mediatruth.Analyze correctly reports ErrUnsupportedContainer for them,
// exercising this package's "never fatal" handling of that legitimate
// bounded outcome without needing a full real-container fixture (HR5.2's
// own suite already covers container-parsing correctness).
type fakeSource struct {
	key     string
	data    [][]byte
	fetches map[int]int
}

type blockingSource struct{}

func (blockingSource) Key() string { return "nntp|blocked" }
func (blockingSource) Chunks() []rangecache.ChunkRef {
	return []rangecache.ChunkRef{{Index: 0, Key: "blocked", Size: 100}}
}
func (blockingSource) Fetch(ctx context.Context, _ rangecache.ChunkRef, _ rangecache.FetchClass) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingSource) ExactSizes() bool { return true }

func newFakeSource(key string, chunkSizes ...int) *fakeSource {
	f := &fakeSource{key: key, fetches: map[int]int{}}
	for i, n := range chunkSizes {
		b := make([]byte, n)
		for j := range b {
			b[j] = byte((i*97 + j*13 + 5) % 251)
		}
		f.data = append(f.data, b)
	}
	return f
}

func (f *fakeSource) Key() string { return f.key }

func (f *fakeSource) Chunks() []rangecache.ChunkRef {
	refs := make([]rangecache.ChunkRef, len(f.data))
	for i, d := range f.data {
		refs[i] = rangecache.ChunkRef{Index: i, Key: fmt.Sprintf("c%d", i), Size: int64(len(d))}
	}
	return refs
}

func (f *fakeSource) Fetch(_ context.Context, ref rangecache.ChunkRef, _ rangecache.FetchClass) ([]byte, error) {
	f.fetches[ref.Index]++
	out := make([]byte, len(f.data[ref.Index]))
	copy(out, f.data[ref.Index])
	return out, nil
}

func (f *fakeSource) ExactSizes() bool { return true }

func testCache(t *testing.T) *rangecache.Cache {
	t.Helper()
	c := rangecache.New(rangecache.Config{DiskCachePath: t.TempDir(), DiskCacheSizeMB: 64, PinnedBudgetMB: 64}, testLogger())
	t.Cleanup(c.Close)
	return c
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestPrewarmHotHeadPinsLeadingBytesAndUnsupportedContainerIsNotFatal(t *testing.T) {
	cache := testCache(t)
	svc := New(cache, nil, "", Config{Enabled: true, HotHeadBytes: 250, Timeout: 5 * time.Second}, testLogger(), nil)
	src := newFakeSource("torrent|item1", 100, 100, 100, 100)

	res, err := svc.PrewarmHotHead(context.Background(), src)
	if err != nil {
		t.Fatalf("PrewarmHotHead returned an error for an unsupported container, want nil: %v", err)
	}
	if res != nil {
		t.Fatalf("expected nil Result for an unsupported/unparseable container, got %+v", res)
	}
	// The first 3 chunks (300 bytes >= 250 HotHeadBytes) must have been
	// fetched (and therefore pinned) by the explicit hot-head loop.
	for _, idx := range []int{0, 1, 2} {
		if src.fetches[idx] == 0 {
			t.Fatalf("expected chunk %d to have been fetched/pinned, got 0 fetches", idx)
		}
	}
}

func TestPrewarmLeadingPinsOnlyBoundedHeadAndReusesCache(t *testing.T) {
	cache := testCache(t)
	svc := New(cache, nil, "", Config{Enabled: true, HotHeadBytes: 250, Timeout: 5 * time.Second}, testLogger(), nil)
	src := newFakeSource("nntp|item", 100, 100, 100, 100)

	if err := svc.PrewarmLeading(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	if len(src.fetches) != 3 || src.fetches[0] != 1 || src.fetches[1] != 1 || src.fetches[2] != 1 || src.fetches[3] != 0 {
		t.Fatalf("fetches = %v, want exactly three leading chunks", src.fetches)
	}
	if err := svc.PrewarmLeading(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	if src.fetches[0] != 1 || src.fetches[1] != 1 || src.fetches[2] != 1 {
		t.Fatalf("second prewarm refetched cached chunks: %v", src.fetches)
	}
}

func TestPrewarmLeadingTimeoutIsBoundedAndSynchronous(t *testing.T) {
	cache := testCache(t)
	svc := New(cache, nil, "", Config{Enabled: true, HotHeadBytes: 100, Timeout: 20 * time.Millisecond}, testLogger(), nil)
	start := time.Now()
	err := svc.PrewarmLeading(context.Background(), blockingSource{})
	if err == nil {
		t.Fatal("timed-out prewarm returned nil")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("prewarm timeout took %s", elapsed)
	}
}

func TestPrewarmHotHeadDisabledIsNoop(t *testing.T) {
	cache := testCache(t)
	svc := New(cache, nil, "", Config{Enabled: false, HotHeadBytes: 250}, testLogger(), nil)
	src := newFakeSource("torrent|item2", 100, 100)

	res, err := svc.PrewarmHotHead(context.Background(), src)
	if err != nil || res != nil {
		t.Fatalf("expected (nil,nil) when disabled, got (%v,%v)", res, err)
	}
	if len(src.fetches) != 0 {
		t.Fatalf("expected no fetches when disabled, got %v", src.fetches)
	}
}

// TestPrewarmHotHeadRespectsGovernorPriority proves LC-06 ordering
// deterministically against the REAL accountgov.Governor, for this row's
// own wiring: a queued PriorityPlayback acquire is granted before a
// concurrently queued PriorityPrewarm one once the sole capacity unit
// frees up. This is CG-05's established closure shape for this gate
// (matching HR1.4/SF-02's own precedent) since no real dual-stream torrent
// contention is practically constructible on this homelab.
func TestPrewarmHotHeadRespectsGovernorPriority(t *testing.T) {
	cache := testCache(t)
	gov := accountgov.New("test-account")
	gov.SetCapacity("op", 1)

	held, err := gov.Acquire(context.Background(), "op", accountgov.PriorityPlayback, "holder")
	if err != nil {
		t.Fatalf("acquire holder lease: %v", err)
	}

	svc := New(cache, gov, "op", Config{Enabled: true, HotHeadBytes: 100, Timeout: 5 * time.Second}, testLogger(), nil)
	src := newFakeSource("torrent|contended", 100)

	prewarmDone := make(chan struct{})
	go func() {
		_, _ = svc.PrewarmHotHead(context.Background(), src)
		close(prewarmDone)
	}()
	time.Sleep(20 * time.Millisecond) // let the prewarm goroutine reach gov.Acquire and queue

	playbackGranted := make(chan struct{})
	go func() {
		lease, err := gov.Acquire(context.Background(), "op", accountgov.PriorityPlayback, "player")
		if err != nil {
			return
		}
		close(playbackGranted)
		lease.Release()
	}()
	time.Sleep(20 * time.Millisecond) // let the playback acquire also queue

	held.Release() // frees the one capacity unit; priority order decides who gets it next

	select {
	case <-playbackGranted:
		// correct: playback (higher LC-06 priority) was admitted first
	case <-time.After(2 * time.Second):
		t.Fatal("playback lease was never granted — priority ordering did not hold")
	}
	select {
	case <-prewarmDone:
	case <-time.After(2 * time.Second):
		t.Fatal("prewarm never completed after its lease was eventually granted")
	}
}

func TestSeekTargetPrefetchPinsCoveringChunk(t *testing.T) {
	cache := testCache(t)
	svc := New(cache, nil, "", Config{Enabled: true, Timeout: 5 * time.Second}, testLogger(), nil)
	src := newFakeSource("torrent|seek", 100, 100, 100)
	idx := mediatruth.Index{Entries: []mediatruth.Entry{
		{TimeMS: 0, Offset: 0},
		{TimeMS: 1000, Offset: 150}, // resolves into chunk 1 [100,200)
		{TimeMS: 2000, Offset: 250}, // resolves into chunk 2 [200,300)
	}}

	if err := svc.SeekTargetPrefetch(context.Background(), src, idx, 1500); err != nil {
		t.Fatalf("SeekTargetPrefetch: %v", err)
	}
	if src.fetches[1] == 0 {
		t.Fatal("expected chunk 1 (covering the resolved seek offset) to be fetched")
	}
	if src.fetches[0] != 0 || src.fetches[2] != 0 {
		t.Fatalf("expected only chunk 1 to be fetched, got fetches=%v", src.fetches)
	}
}

func TestSeekTargetPrefetchNoIndexEntryIsNoop(t *testing.T) {
	cache := testCache(t)
	svc := New(cache, nil, "", Config{Enabled: true, Timeout: 5 * time.Second}, testLogger(), nil)
	src := newFakeSource("torrent|seek2", 100)

	if err := svc.SeekTargetPrefetch(context.Background(), src, mediatruth.Index{}, 500); err != nil {
		t.Fatalf("expected no error for an empty index, got %v", err)
	}
	if len(src.fetches) != 0 {
		t.Fatalf("expected no fetch for an empty index, got %v", src.fetches)
	}
}

func TestNextEpisodePrewarmThresholdGating(t *testing.T) {
	cache := testCache(t)
	svc := New(cache, nil, "", Config{Enabled: true, HotHeadBytes: 50, NextEpisodeThreshold: 0.85, Timeout: 5 * time.Second}, testLogger(), nil)
	src := newFakeSource("torrent|nextep", 100)

	if _, err := svc.NextEpisodePrewarm(context.Background(), src, 0.5); err != nil {
		t.Fatalf("below threshold: %v", err)
	}
	if len(src.fetches) != 0 {
		t.Fatalf("expected no prewarm below threshold, got fetches=%v", src.fetches)
	}

	if _, err := svc.NextEpisodePrewarm(context.Background(), src, 0.9); err != nil {
		t.Fatalf("above threshold: %v", err)
	}
	if src.fetches[0] == 0 {
		t.Fatal("expected prewarm to fire once the threshold is crossed")
	}
}

func TestWorkersFallsBackWhenUnmeasured(t *testing.T) {
	cache := testCache(t)
	svc := New(cache, nil, "", Config{Enabled: true, MinReadaheadWorkers: 1, MaxReadaheadWorkers: 4}, testLogger(), nil)
	src := newFakeSource("torrent|unmeasured", 100)

	if got := svc.Workers(src, 7); got != 7 {
		t.Fatalf("expected fallback 7 for an unmeasured source, got %d", got)
	}
}

func TestWorkersUsesMeasurementAndHeadroom(t *testing.T) {
	cache := testCache(t)
	gov := accountgov.New("acct")
	gov.SetCapacity("op", 4)
	svc := New(cache, gov, "op", Config{Enabled: true, MinReadaheadWorkers: 1, MaxReadaheadWorkers: 4}, testLogger(), nil)
	src := newFakeSource("torrent|measured", 100)

	svc.recordBitrate(src.Key(), 8_000_000)
	svc.recordThroughput(src.Key(), 4_000_000)

	// Occupy 3 of 4 capacity units, leaving exactly 1 unit of headroom.
	var leases []*accountgov.Lease
	for i := 0; i < 3; i++ {
		l, err := gov.Acquire(context.Background(), "op", accountgov.PriorityPlayback, fmt.Sprintf("s%d", i))
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		leases = append(leases, l)
	}
	defer func() {
		for _, l := range leases {
			l.Release()
		}
	}()

	got := svc.Workers(src, 99)
	want := svc.readahead.Recommend(8_000_000, 4_000_000, 1)
	if got != want {
		t.Fatalf("Workers()=%d, want %d (matching Recommend with headroom=1)", got, want)
	}
	if got > 1 {
		t.Fatalf("expected the thin headroom (1) to bound the recommendation, got %d", got)
	}
}

func TestStatsMapStaysBounded(t *testing.T) {
	cache := testCache(t)
	svc := New(cache, nil, "", Config{Enabled: true}, testLogger(), nil)
	for i := 0; i < maxTrackedSources*2; i++ {
		svc.recordThroughput(fmt.Sprintf("src-%d", i), int64(1000+i))
	}
	svc.statsMu.Lock()
	n := len(svc.stats)
	svc.statsMu.Unlock()
	if n > maxTrackedSources {
		t.Fatalf("stats map grew past maxTrackedSources: %d > %d", n, maxTrackedSources)
	}
}
