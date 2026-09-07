package rangecache

// NS-5.5: adaptive readahead width (steady worker recommendation) plus the
// bounded seek-triggered widen mechanism. These are white-box tests (same
// package) so they assert directly on readaheadSession internal state,
// which is far more precise and less flaky than inferring behavior purely
// from fetch-timing observation.

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// steadySource wraps fakeChunkSource with a fixed WorkerLimiter ceiling and
// SteadyWorkerRecommender resting recommendation, so a test can control both
// axes independently.
type steadySource struct {
	*fakeChunkSource
	ceiling int
	steady  int
	// hasSteady lets a test simulate a source that does NOT implement
	// SteadyWorkerRecommender at all (the pre-NS-5.5 shape).
	hasSteady bool
}

func (s *steadySource) MaxWorkers() int { return s.ceiling }

// SteadyWorkers is only "present" (satisfies the interface at the type
// level always, since Go interfaces are structural) but a test wanting to
// simulate "doesn't implement it" uses plainSteadySource below instead,
// which simply omits this method.
func (s *steadySource) SteadyWorkers() int { return s.steady }

// plainSteadySource implements only WorkerLimiter, never SteadyWorkerRecommender
// — the exact shape of every pre-NS-5.5 source (CDN, HTTP).
type plainSteadySource struct {
	*fakeChunkSource
	ceiling int
}

func (s *plainSteadySource) MaxWorkers() int { return s.ceiling }

func newSteadyFakeSource(key string, n, chunkSize int) *fakeChunkSource {
	sizes := make([]int, n)
	for i := range sizes {
		sizes[i] = chunkSize
	}
	return newFakeChunkSource(key, sizes...)
}

func sessionFor(c *Cache, src ChunkSource, firstChunkKey string) (*readaheadSession, bool) {
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	sess, ok := c.sessions[sha256hex(src.Key()+"|"+firstChunkKey)]
	return sess, ok
}

// rangeHeaderFor builds a "bytes=start-end" Range header selecting exactly
// one chunk's declared byte span, given uniform chunkSize.
func rangeHeaderFor(chunkIdx, chunkSize int) string {
	start := chunkIdx * chunkSize
	end := start + chunkSize - 1
	return "bytes=" + itoa(start) + "-" + itoa(end)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TestSteadyWorkerRecommenderSetsInitialActiveLimit proves a fresh session
// initializes activeLimit to the source's SteadyWorkers() value (not the
// ceiling) when widening is enabled and the source implements the
// interface.
func TestSteadyWorkerRecommenderSetsInitialActiveLimit(t *testing.T) {
	c := New(Config{
		DiskCachePath:        t.TempDir(),
		ReadaheadEnabled:     true,
		ReadaheadWorkers:     8, // ignored: WorkerLimiter caps it to src.ceiling below
		ReadaheadMaxSegments: 10,
		MinBufferSegments:    1,
		SeekWidenDuration:    500 * time.Millisecond,
	}, testLogger())
	defer c.Close()

	src := &steadySource{fakeChunkSource: newSteadyFakeSource("steady-src", 40, 10), ceiling: 4, steady: 1, hasSteady: true}
	ctx := context.Background()
	rec := httptest.NewRecorder()
	if err := c.Stream(ctx, src, ModeReadahead, 400, rec, rangeHeaderFor(0, 10), 1); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	sess, ok := sessionFor(c, src, "c0")
	if !ok {
		t.Fatal("expected a readahead session to exist after Stream")
	}
	if sess.ceiling != 4 {
		t.Fatalf("ceiling = %d, want 4 (from MaxWorkers)", sess.ceiling)
	}
	if sess.steady != 1 {
		t.Fatalf("steady = %d, want 1 (from SteadyWorkers)", sess.steady)
	}
	if got := sess.activeLimit.Load(); got != 1 {
		t.Fatalf("initial activeLimit = %d, want 1 (steady, not ceiling)", got)
	}
}

// TestSourceWithoutSteadyWorkerRecommenderIsUnaffected proves a source that
// implements only WorkerLimiter (every pre-NS-5.5 lane: CDN, HTTP) gets
// steady==ceiling and activeLimit==ceiling always — a complete no-op for
// this row's new gating mechanism, matching pre-NS-5.5 behavior exactly.
func TestSourceWithoutSteadyWorkerRecommenderIsUnaffected(t *testing.T) {
	c := New(Config{
		DiskCachePath:        t.TempDir(),
		ReadaheadEnabled:     true,
		ReadaheadWorkers:     8,
		ReadaheadMaxSegments: 10,
		MinBufferSegments:    1,
		SeekWidenDuration:    500 * time.Millisecond, // enabled, but source can't use it
	}, testLogger())
	defer c.Close()

	src := &plainSteadySource{fakeChunkSource: newSteadyFakeSource("plain-src", 40, 10), ceiling: 4}
	ctx := context.Background()
	rec := httptest.NewRecorder()
	if err := c.Stream(ctx, src, ModeReadahead, 400, rec, rangeHeaderFor(0, 10), 1); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	sess, ok := sessionFor(c, src, "c0")
	if !ok {
		t.Fatal("expected a readahead session to exist after Stream")
	}
	if sess.ceiling != 4 {
		t.Fatalf("ceiling = %d, want 4", sess.ceiling)
	}
	if sess.steady != sess.ceiling {
		t.Fatalf("steady = %d, want == ceiling (%d) for a source without SteadyWorkerRecommender", sess.steady, sess.ceiling)
	}
	if got := sess.activeLimit.Load(); got != sess.ceiling {
		t.Fatalf("activeLimit = %d, want == ceiling (%d)", got, sess.ceiling)
	}
}

// TestSeekWidenDurationZeroDisablesGating proves SeekWidenDuration<=0 keeps
// steady==ceiling even for a source that DOES implement
// SteadyWorkerRecommender with a lower value — the documented "0 disables
// widening: activeLimit stays pinned at the ceiling always" contract.
func TestSeekWidenDurationZeroDisablesGating(t *testing.T) {
	c := New(Config{
		DiskCachePath:        t.TempDir(),
		ReadaheadEnabled:     true,
		ReadaheadWorkers:     8,
		ReadaheadMaxSegments: 10,
		MinBufferSegments:    1,
		SeekWidenDuration:    0, // disabled
	}, testLogger())
	defer c.Close()

	src := &steadySource{fakeChunkSource: newSteadyFakeSource("disabled-src", 40, 10), ceiling: 4, steady: 1, hasSteady: true}
	ctx := context.Background()
	rec := httptest.NewRecorder()
	if err := c.Stream(ctx, src, ModeReadahead, 400, rec, rangeHeaderFor(0, 10), 1); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	sess, ok := sessionFor(c, src, "c0")
	if !ok {
		t.Fatal("expected a readahead session to exist after Stream")
	}
	if sess.steady != sess.ceiling {
		t.Fatalf("steady = %d, want == ceiling (%d) when SeekWidenDuration<=0", sess.steady, sess.ceiling)
	}
	if got := sess.activeLimit.Load(); got != sess.ceiling {
		t.Fatalf("activeLimit = %d, want == ceiling (%d) when SeekWidenDuration<=0", got, sess.ceiling)
	}
}

// TestSeekFlushWidensThenDecays proves the core NS-5.5 contract: a seek
// beyond ReadaheadMaxSegments widens activeLimit to the ceiling for
// SeekWidenDuration, and a later session touch after that window elapses
// decays it back to steady.
func TestSeekFlushWidensThenDecays(t *testing.T) {
	fakeNow := time.Now()
	c := New(Config{
		DiskCachePath:        t.TempDir(),
		ReadaheadEnabled:     true,
		ReadaheadWorkers:     8,
		ReadaheadMaxSegments: 10,
		MinBufferSegments:    1,
		SeekWidenDuration:    500 * time.Millisecond,
	}, testLogger())
	c.now = func() time.Time { return fakeNow }
	defer c.Close()

	src := &steadySource{fakeChunkSource: newSteadyFakeSource("widen-src", 60, 10), ceiling: 4, steady: 1, hasSteady: true}
	ctx := context.Background()

	// Request 1: establishes the session at chunk 0. activeLimit starts at steady (1).
	if err := c.Stream(ctx, src, ModeReadahead, 600, httptest.NewRecorder(), rangeHeaderFor(0, 10), 1); err != nil {
		t.Fatalf("Stream 1: %v", err)
	}
	sess, ok := sessionFor(c, src, "c0")
	if !ok {
		t.Fatal("expected a session after request 1")
	}
	if got := sess.activeLimit.Load(); got != 1 {
		t.Fatalf("activeLimit after request 1 = %d, want 1 (steady)", got)
	}

	// Request 2: seeks to chunk 30 — a jump of 30 chunks, well beyond
	// ReadaheadMaxSegments (10), so this must trigger the seek-flush +
	// widen path.
	if err := c.Stream(ctx, src, ModeReadahead, 600, httptest.NewRecorder(), rangeHeaderFor(30, 10), 1); err != nil {
		t.Fatalf("Stream 2 (seek): %v", err)
	}
	if got := sess.activeLimit.Load(); got != sess.ceiling {
		t.Fatalf("activeLimit after seek = %d, want == ceiling (%d)", got, sess.ceiling)
	}
	if wu := sess.widenUntil.Load(); wu == 0 {
		t.Fatal("expected widenUntil to be set (nonzero) right after a seek widen")
	}

	// Advance the injected clock past SeekWidenDuration, then touch the
	// session again (no further seek — same chunk) to trigger decay.
	fakeNow = fakeNow.Add(600 * time.Millisecond)
	if err := c.Stream(ctx, src, ModeReadahead, 600, httptest.NewRecorder(), rangeHeaderFor(30, 10), 1); err != nil {
		t.Fatalf("Stream 3 (post-widen touch): %v", err)
	}
	if got := sess.activeLimit.Load(); got != sess.steady {
		t.Fatalf("activeLimit after decay = %d, want == steady (%d)", got, sess.steady)
	}
	if wu := sess.widenUntil.Load(); wu != 0 {
		t.Fatalf("expected widenUntil to be cleared (0) after decay, got %d", wu)
	}
}

// trackedSteadySource is a ChunkSource that tracks concurrent in-flight
// Fetch calls (for TestActiveLimitGatesConcurrentFetches) while also
// implementing WorkerLimiter + SteadyWorkerRecommender.
type trackedSteadySource struct {
	*fakeChunkSource
	ceiling int
	steady  int
	delay   time.Duration

	mu          sync.Mutex
	inFlight    int
	maxInFlight int
}

func (s *trackedSteadySource) MaxWorkers() int    { return s.ceiling }
func (s *trackedSteadySource) SteadyWorkers() int { return s.steady }

// Fetch only tracks concurrency for FetchReadahead-class calls: the
// write loop's own synchronous demand fetch for the request's own first
// chunk legitimately overlaps with readahead goroutines fetching AHEAD of
// it (different chunk indices, no gate applies to demand fetches at all —
// only readaheadGoroutine's own claims are gated by activeLimit). Mixing
// that expected overlap into the metric would falsely fail the steady-state
// assertion below.
func (s *trackedSteadySource) Fetch(ctx context.Context, ref ChunkRef, class FetchClass) ([]byte, error) {
	track := class == FetchReadahead
	if track {
		s.mu.Lock()
		s.inFlight++
		if s.inFlight > s.maxInFlight {
			s.maxInFlight = s.inFlight
		}
		s.mu.Unlock()
	}
	time.Sleep(s.delay)
	// fakeChunkSource.fetches is an unsynchronized plain map (fine for the
	// pinned-tier tests, which never call Fetch concurrently); guard it
	// here since this test genuinely drives concurrent readahead calls.
	s.mu.Lock()
	data, err := s.fakeChunkSource.Fetch(ctx, ref, class)
	s.mu.Unlock()
	if track {
		s.mu.Lock()
		s.inFlight--
		s.mu.Unlock()
	}
	return data, err
}

// TestActiveLimitGatesConcurrentFetches is a functional (not just
// state-inspection) proof: with activeLimit held at steady, no more than
// `steady` chunks are ever fetched concurrently by the readahead
// goroutines, even though `ceiling` goroutines exist and are all trying.
func TestActiveLimitGatesConcurrentFetches(t *testing.T) {
	c := New(Config{
		DiskCachePath:        t.TempDir(),
		ReadaheadEnabled:     true,
		ReadaheadWorkers:     8,
		ReadaheadMaxSegments: 20,
		MinBufferSegments:    1,
		SeekWidenDuration:    500 * time.Millisecond,
	}, testLogger())
	defer c.Close()

	tsrc := &trackedSteadySource{
		fakeChunkSource: newSteadyFakeSource("concurrency-src", 60, 10),
		ceiling:         4,
		steady:          1,
		delay:           40 * time.Millisecond,
	}
	ctx := context.Background()

	if err := c.Stream(ctx, tsrc, ModeReadahead, 600, httptest.NewRecorder(), rangeHeaderFor(0, 10), 1); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	// Let the readahead goroutines run for a while at steady=1.
	time.Sleep(250 * time.Millisecond)
	tsrc.mu.Lock()
	steadyMax := tsrc.maxInFlight
	tsrc.mu.Unlock()
	if steadyMax > 1 {
		t.Fatalf("max concurrent fetches during steady phase = %d, want <= 1 (activeLimit gate not enforced)", steadyMax)
	}

	// Seek far enough to trigger widen, then give the now-active goroutines
	// time to actually run concurrently.
	if err := c.Stream(ctx, tsrc, ModeReadahead, 600, httptest.NewRecorder(), rangeHeaderFor(30, 10), 1); err != nil {
		t.Fatalf("Stream (seek): %v", err)
	}
	time.Sleep(250 * time.Millisecond)
	tsrc.mu.Lock()
	widenedMax := tsrc.maxInFlight
	tsrc.mu.Unlock()
	if widenedMax <= steadyMax {
		t.Fatalf("max concurrent fetches after widen = %d, want > steady-phase max (%d)", widenedMax, steadyMax)
	}
	if widenedMax > 4 {
		t.Fatalf("max concurrent fetches after widen = %d, want <= ceiling (4)", widenedMax)
	}
}
