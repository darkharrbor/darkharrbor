package rangecache

// HR1.3 gates: one upstream read for eligible overlap (DG-05/DG-07),
// cancellation isolation, disconnect cleanup, and demand-join accounting.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gateSource blocks every Fetch until released, so concurrency is
// deterministic rather than timing-dependent.
type gateSource struct {
	key     string
	payload []byte
	release chan struct{}
	entered chan struct{}

	fetches   atomic.Int64
	cancelled atomic.Int64
	failWith  error
	global    bool
}

func (s *gateSource) Key() string        { return s.key }
func (s *gateSource) Chunks() []ChunkRef { return nil }
func (s *gateSource) ExactSizes() bool   { return true }

func (s *gateSource) PayloadKey(ref ChunkRef) string {
	if s.global {
		return "global|" + ref.Key
	}
	return s.key + "|" + ref.Key
}

func (s *gateSource) Fetch(ctx context.Context, _ ChunkRef, _ FetchClass) ([]byte, error) {
	s.fetches.Add(1)
	select {
	case s.entered <- struct{}{}:
	default:
	}
	select {
	case <-s.release:
	case <-ctx.Done():
		s.cancelled.Add(1)
		return nil, ctx.Err()
	}
	if s.failWith != nil {
		return nil, s.failWith
	}
	return append([]byte(nil), s.payload...), nil
}

func newGateSource(key string, payload string) *gateSource {
	return &gateSource{
		key: key, payload: []byte(payload),
		release: make(chan struct{}), entered: make(chan struct{}, 1),
		global: true,
	}
}

func testCache(t *testing.T) *Cache {
	t.Helper()
	c := New(Config{DiskCachePath: t.TempDir(), DiskCacheSizeMB: 8, DiskCacheTTLMin: 60},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(c.Close)
	return c
}

func waitJoins(t *testing.T, c *Cache, n int) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if c.coalescedJoins.Load() >= int64(n) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d joins, have %d", n, c.coalescedJoins.Load())
}

func waitInflight(t *testing.T, c *Cache, n int) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if c.inflightLen() == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d in-flight entries, have %d", n, c.inflightLen())
}

// TestCoalescedConcurrentMissesCauseOneUpstreamRead is the row's headline
// gate: N simultaneous callers for the same payload, one origin read.
func TestCoalescedConcurrentMissesCauseOneUpstreamRead(t *testing.T) {
	c := testCache(t)
	src := newGateSource("item-a", "shared-payload")
	ref := ChunkRef{Index: 0, Key: "seg-1", Size: 14}

	const callers = 8
	var wg sync.WaitGroup
	results := make([][]byte, callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = c.GetChunk(context.Background(), src, ModeDisk, ref)
		}(i)
	}
	<-src.entered
	waitInflight(t, c, 1)
	waitJoins(t, c, callers-1)
	close(src.release)
	wg.Wait()

	if got := src.fetches.Load(); got != 1 {
		t.Fatalf("expected exactly one upstream read, got %d", got)
	}
	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if string(results[i]) != "shared-payload" {
			t.Fatalf("caller %d got %q", i, results[i])
		}
	}
	if c.coalescedJoins.Load() != callers-1 {
		t.Fatalf("expected %d joins, got %d", callers-1, c.coalescedJoins.Load())
	}
	if c.inflightLen() != 0 {
		t.Fatalf("in-flight map not drained: %d", c.inflightLen())
	}
}

// TestCoalescedFollowersGetIndependentBuffers: no two callers may share a
// mutable slice, preserving the pre-HR1.3 guarantee.
func TestCoalescedFollowersGetIndependentBuffers(t *testing.T) {
	c := testCache(t)
	src := newGateSource("item-a", "abcdefgh")
	ref := ChunkRef{Index: 0, Key: "seg-1", Size: 8}

	var wg sync.WaitGroup
	out := make([][]byte, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out[i], _ = c.GetChunk(context.Background(), src, ModeDisk, ref)
		}(i)
	}
	<-src.entered
	waitInflight(t, c, 1)
	close(src.release)
	wg.Wait()

	out[0][0] = 'Z'
	for i := 1; i < 4; i++ {
		if out[i][0] == 'Z' {
			t.Fatalf("caller %d aliases caller 0's buffer", i)
		}
	}
}

// TestCoalescedWaiterCancellationIsolation: one caller abandoning must not
// disturb the shared fetch or any other caller.
func TestCoalescedWaiterCancellationIsolation(t *testing.T) {
	c := testCache(t)
	src := newGateSource("item-a", "payload")
	ref := ChunkRef{Index: 0, Key: "seg-1", Size: 7}

	stayCtx := context.Background()
	goCtx, goCancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	var stayData []byte
	var stayErr, leaveErr error

	wg.Add(1)
	go func() { defer wg.Done(); stayData, stayErr = c.GetChunk(stayCtx, src, ModeDisk, ref) }()
	<-src.entered
	waitInflight(t, c, 1)

	wg.Add(1)
	go func() { defer wg.Done(); _, leaveErr = c.GetChunk(goCtx, src, ModeDisk, ref) }()
	// Let the second caller join before it abandons.
	for i := 0; i < 200 && c.coalescedJoins.Load() == 0; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	goCancel()
	close(src.release)
	wg.Wait()

	if stayErr != nil {
		t.Fatalf("remaining caller must succeed, got %v", stayErr)
	}
	if string(stayData) != "payload" {
		t.Fatalf("remaining caller got %q", stayData)
	}
	if leaveErr == nil {
		t.Fatal("abandoning caller should observe its own cancellation")
	}
	if src.cancelled.Load() != 0 {
		t.Fatal("shared fetch was cancelled by a departing waiter")
	}
	if got := src.fetches.Load(); got != 1 {
		t.Fatalf("expected one upstream read, got %d", got)
	}
}

// TestCoalescedLeaderCancellationDoesNotKillFollowers: the FIRST caller
// leaving is the resolveGroup flaw the audit called out; it must not happen
// here.
func TestCoalescedLeaderCancellationDoesNotKillFollowers(t *testing.T) {
	c := testCache(t)
	src := newGateSource("item-a", "payload")
	ref := ChunkRef{Index: 0, Key: "seg-1", Size: 7}

	leadCtx, leadCancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var followData []byte
	var followErr error

	wg.Add(1)
	go func() { defer wg.Done(); _, _ = c.GetChunk(leadCtx, src, ModeDisk, ref) }()
	<-src.entered
	waitInflight(t, c, 1)

	wg.Add(1)
	go func() {
		defer wg.Done()
		followData, followErr = c.GetChunk(context.Background(), src, ModeDisk, ref)
	}()
	for i := 0; i < 200 && c.coalescedJoins.Load() == 0; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	leadCancel() // the leader's own caller disconnects
	close(src.release)
	wg.Wait()

	if followErr != nil {
		t.Fatalf("follower must survive the leader's disconnect, got %v", followErr)
	}
	if string(followData) != "payload" {
		t.Fatalf("follower got %q", followData)
	}
	if src.cancelled.Load() != 0 {
		t.Fatal("leader's disconnect cancelled the shared fetch")
	}
}

// TestCoalescedLastWaiterAbandonCancelsWork is the frozen plan's
// "disconnect cleanup": no orphan upstream read once everyone has left.
func TestCoalescedLastWaiterAbandonCancelsWork(t *testing.T) {
	c := testCache(t)
	src := newGateSource("item-a", "payload")
	ref := ChunkRef{Index: 0, Key: "seg-1", Size: 7}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _, _ = c.GetChunk(ctx, src, ModeDisk, ref) }()
	<-src.entered
	waitInflight(t, c, 1)
	cancel()
	<-done

	for i := 0; i < 200 && src.cancelled.Load() == 0; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if src.cancelled.Load() != 1 {
		t.Fatal("last waiter leaving must cancel the orphaned fetch")
	}
	if c.coalescedAborts.Load() != 1 {
		t.Fatalf("expected one abort, got %d", c.coalescedAborts.Load())
	}
	close(src.release)
}

func TestCoalescedErrorReachesEveryCaller(t *testing.T) {
	c := testCache(t)
	src := newGateSource("item-a", "payload")
	src.failWith = errors.New("upstream exploded")
	ref := ChunkRef{Index: 0, Key: "seg-1", Size: 7}

	var wg sync.WaitGroup
	errs := make([]error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); _, errs[i] = c.GetChunk(context.Background(), src, ModeDisk, ref) }(i)
	}
	<-src.entered
	waitInflight(t, c, 1)
	waitJoins(t, c, 4)
	close(src.release)
	wg.Wait()

	for i, err := range errs {
		if err == nil {
			t.Fatalf("caller %d should have received the upstream error", i)
		}
	}
	if got := src.fetches.Load(); got != 1 {
		t.Fatalf("a failing fetch must not be repeated per caller, got %d", got)
	}
}

// TestCoalescedDistinctPayloadsStayIndependent: non-overlap must not be
// serialised or merged.
func TestCoalescedDistinctPayloadsStayIndependent(t *testing.T) {
	c := testCache(t)
	src := newGateSource("item-a", "payload")

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = c.GetChunk(context.Background(), src, ModeDisk,
				ChunkRef{Index: i, Key: "seg-" + string(rune('a'+i)), Size: 7})
		}(i)
	}
	waitInflight(t, c, 3)
	close(src.release)
	wg.Wait()

	if got := src.fetches.Load(); got != 3 {
		t.Fatalf("distinct payloads must each fetch, got %d", got)
	}
	if c.coalescedJoins.Load() != 0 {
		t.Fatalf("distinct payloads must not coalesce, joins=%d", c.coalescedJoins.Load())
	}
}

// TestCoalescedDemandJoinsReadaheadIsAccounted: demand joining lower-priority
// work in flight is allowed (it consumes no extra capacity) and is counted.
func TestCoalescedDemandJoinsReadaheadIsAccounted(t *testing.T) {
	c := testCache(t)
	src := newGateSource("item-a", "payload")
	ref := ChunkRef{Index: 0, Key: "seg-1", Size: 7}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _, _ = c.PrefetchChunk(context.Background(), src, ModeDisk, ref) }()
	<-src.entered
	waitInflight(t, c, 1)

	wg.Add(1)
	var demandData []byte
	go func() { defer wg.Done(); demandData, _ = c.GetChunk(context.Background(), src, ModeDisk, ref) }()
	for i := 0; i < 200 && c.coalescedJoins.Load() == 0; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	close(src.release)
	wg.Wait()

	if string(demandData) != "payload" {
		t.Fatalf("demand caller got %q", demandData)
	}
	if got := src.fetches.Load(); got != 1 {
		t.Fatalf("expected one upstream read, got %d", got)
	}
}

func TestCoalescedNoGoroutineLeak(t *testing.T) {
	base := runtime.NumGoroutine()
	c := testCache(t)
	for round := 0; round < 5; round++ {
		src := newGateSource("item-a", "payload")
		ref := ChunkRef{Index: round, Key: "seg-" + string(rune('a'+round)), Size: 7}
		var wg sync.WaitGroup
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); _, _ = c.GetChunk(context.Background(), src, ModeDisk, ref) }()
		}
		<-src.entered
		waitInflight(t, c, 1)
		close(src.release)
		wg.Wait()
	}
	c.Close()
	settled := runtime.NumGoroutine()
	for i := 0; i < 40 && settled > base+2; i++ {
		time.Sleep(25 * time.Millisecond)
		settled = runtime.NumGoroutine()
	}
	if settled > base+2 {
		t.Fatalf("goroutine leak: base %d, settled %d", base, settled)
	}
}
