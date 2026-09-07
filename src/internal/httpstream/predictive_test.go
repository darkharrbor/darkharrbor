package httpstream

// HR3.2 deterministic gates: predictive midstream re-resolve. These tests
// exercise the same real NewHTTPClient/httptest origin the HR1.1 bytesource
// tests use; only the resolver is specialized to distinguish forced
// (predictive/reactive) calls from the initial one and to optionally return
// a mismatched identity so rejection can be proven.

import (
	"context"
	"sync"
	"testing"
	"time"
)

// predictiveResolver hands back configurable, mutable coordinates and counts
// total vs forced Resolve calls so tests can assert exactly when a
// background predictive attempt fires.
type predictiveResolver struct {
	mu           sync.Mutex
	calls        int
	forcedCalls  int
	url          string
	selector     string
	size         int64
	expiresAt    time.Time
	mismatchNext bool // next forced call returns a different Selector
}

func (r *predictiveResolver) Resolve(ctx context.Context, forced bool) (ResolvedFile, error) {
	r.mu.Lock()
	r.calls++
	if forced {
		r.forcedCalls++
	}
	sel := r.selector
	if forced && r.mismatchNext {
		sel = "sel-mismatch"
		r.mismatchNext = false
	}
	rf := ResolvedFile{
		Selector:      sel,
		Name:          "fixture.mp4",
		URL:           r.url,
		Size:          r.size,
		SupportsRange: true,
		ExpiresAt:     r.expiresAt,
	}
	r.mu.Unlock()
	return rf, nil
}

func (r *predictiveResolver) counts() (calls, forcedCalls int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.forcedCalls
}

func (r *predictiveResolver) setExpiresAt(t time.Time) {
	r.mu.Lock()
	r.expiresAt = t
	r.mu.Unlock()
}

// waitForForcedCalls polls real wall-clock time (the predictive attempt runs
// on a detached background goroutine, not the injected test clock) for the
// forced-call count to reach want, bounded so a failure to fire reports
// promptly instead of hanging.
func waitForForcedCalls(t *testing.T, r *predictiveResolver, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, forced := r.counts(); forced >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, forced := r.counts()
	t.Fatalf("timed out waiting for forced resolve calls: want %d, got %d", want, forced)
}

// assertNoForcedCallsSoon gives a background goroutine, if one were
// (incorrectly) spawned, ample real time to show up, then asserts it didn't.
func assertNoForcedCallsSoon(t *testing.T, r *predictiveResolver) {
	t.Helper()
	time.Sleep(300 * time.Millisecond)
	if _, forced := r.counts(); forced != 0 {
		t.Fatalf("expected no predictive resolve, got %d forced calls", forced)
	}
}

// TestPredictiveResolveAdoptsFreshCoordinatesBeforeExpiry proves the core
// HR3.2 behavior: once a read lands inside the configured lead window, a
// background predictive re-resolve fires, and once it adopts a same-identity
// result with a later ExpiresAt, the OLD expiry boundary no longer triggers
// the reactive path at all.
func TestPredictiveResolveAdoptsFreshCoordinatesBeforeExpiry(t *testing.T) {
	data := fixtureBytes(4096)
	_, client := startOrigin(t, &bsOrigin{data: data})
	clk := newBSClock()
	start := clk.Now()
	res := &predictiveResolver{
		url:       originURL("/media.mp4"),
		selector:  "sel-1",
		size:      int64(len(data)),
		expiresAt: start.Add(2 * time.Minute), // start+120s
	}
	// PredictiveLeadMin (90s) is deliberately well above byteSourceExpirySkew
	// (30s fixed): the predictive window (now+lead >= ExpiresAt, i.e. now
	// >= 30s here) must open well before the reactive-expiry skew window
	// (now >= 90s here), or the reactive path would win the race every time.
	bs := mustSource(t, ByteSourceConfig{
		Key: "http|predictive-adopt", Size: int64(len(data)),
		RangeVerified: true, Client: client, Resolve: res.Resolve, Now: clk.Now,
		PredictiveLeadMin: 90 * time.Second,
	})

	if _, err := bs.ReadAt(context.Background(), make([]byte, 64), 0); err != nil {
		t.Fatalf("first read: %v", err)
	}
	if calls, forced := res.counts(); calls != 1 || forced != 0 {
		t.Fatalf("expected one unforced resolve, got calls=%d forced=%d", calls, forced)
	}

	// now=40s: inside the predictive lead window (40+90=130>=120) but
	// outside the reactive skew window (40+30=70<120), so sourceFor's
	// cache-hit path (not the reactive fallback) is the one that runs and
	// triggers maybeTriggerPredictive. Give the resolver a fresh, later
	// expiry for that predictive attempt to pick up.
	clk.Advance(40 * time.Second)
	res.setExpiresAt(clk.Now().Add(2 * time.Minute)) // new expiry = start+160s
	if _, err := bs.ReadAt(context.Background(), make([]byte, 64), 128); err != nil {
		t.Fatalf("second read: %v", err)
	}
	waitForForcedCalls(t, res, 1)

	// now=125s: past the ORIGINAL expiry (120s) but comfortably inside both
	// the new expiry (160s) and clear of its own 30s skew boundary (130s).
	// If the predictive result was adopted, this read must not trigger
	// another (reactive) resolve.
	clk.Advance(85 * time.Second)
	if _, err := bs.ReadAt(context.Background(), make([]byte, 64), 256); err != nil {
		t.Fatalf("third read: %v", err)
	}
	// The new cur may itself legitimately re-enter its own predictive
	// window by now (its lead window opens well before 130s), so the exact
	// call count isn't pinned -- what matters is that every resolve after
	// the first was a forced/predictive one, never an unforced reactive
	// fallback, proving the old boundary never had to be caught reactively.
	calls, forced := res.counts()
	if calls < 2 || forced < 1 || calls != forced+1 {
		t.Fatalf("expected only forced/predictive resolves after the first, got calls=%d forced=%d", calls, forced)
	}
}

// TestPredictiveResolveDropsSelectorMismatch proves a predictive result that
// would change playback identity is never adopted: the reactive path still
// fires, unaffected, at the original expiry.
func TestPredictiveResolveDropsSelectorMismatch(t *testing.T) {
	data := fixtureBytes(4096)
	_, client := startOrigin(t, &bsOrigin{data: data})
	clk := newBSClock()
	start := clk.Now()
	res := &predictiveResolver{
		url:          originURL("/media.mp4"),
		selector:     "sel-1",
		size:         int64(len(data)),
		expiresAt:    start.Add(2 * time.Minute), // start+120s
		mismatchNext: true,
	}
	bs := mustSource(t, ByteSourceConfig{
		Key: "http|predictive-mismatch", Size: int64(len(data)),
		RangeVerified: true, Client: client, Resolve: res.Resolve, Now: clk.Now,
		PredictiveLeadMin: 90 * time.Second,
	})

	if _, err := bs.ReadAt(context.Background(), make([]byte, 64), 0); err != nil {
		t.Fatalf("first read: %v", err)
	}

	// now=40s: inside the predictive window, outside the reactive skew
	// window (see the sibling adopt test for the exact arithmetic).
	clk.Advance(40 * time.Second)
	if _, err := bs.ReadAt(context.Background(), make([]byte, 64), 128); err != nil {
		t.Fatalf("second read: %v", err)
	}
	waitForForcedCalls(t, res, 1)

	// now=95s: past the original expiry's own reactive skew boundary
	// (90s). Since the mismatched predictive result was dropped, cur is
	// still the original, so the reactive path must fire here.
	clk.Advance(55 * time.Second)
	if _, err := bs.ReadAt(context.Background(), make([]byte, 64), 256); err != nil {
		t.Fatalf("third read: %v", err)
	}
	calls, forced := res.counts()
	if calls != 3 {
		t.Fatalf("expected the reactive path to still fire after a dropped mismatch, got %d calls", calls)
	}
	if forced != 1 {
		t.Fatalf("expected exactly the one predictive forced call plus an unforced reactive one, got forced=%d", forced)
	}
}

// TestPredictiveResolveDisabledWhenLeadZero proves the default
// (PredictiveLeadMin unset) leaves behavior byte-identical to pre-HR3.2:
// no background attempt ever fires, only the existing reactive path.
func TestPredictiveResolveDisabledWhenLeadZero(t *testing.T) {
	data := fixtureBytes(4096)
	_, client := startOrigin(t, &bsOrigin{data: data})
	clk := newBSClock()
	start := clk.Now()
	res := &predictiveResolver{
		url:       originURL("/media.mp4"),
		selector:  "sel-1",
		size:      int64(len(data)),
		expiresAt: start.Add(2 * time.Minute), // start+120s
	}
	bs := mustSource(t, ByteSourceConfig{
		Key: "http|predictive-disabled", Size: int64(len(data)),
		RangeVerified: true, Client: client, Resolve: res.Resolve, Now: clk.Now,
		// PredictiveLeadMin intentionally left at its zero value.
	})

	if _, err := bs.ReadAt(context.Background(), make([]byte, 64), 0); err != nil {
		t.Fatalf("first read: %v", err)
	}
	// now=40s: the same point that triggers a predictive attempt in the
	// sibling tests above (once PredictiveLeadMin is set), and still well
	// clear of the 90s reactive-expiry skew boundary, so any extra resolve
	// observed here can only be a wrongly-enabled predictive one.
	clk.Advance(40 * time.Second)
	if _, err := bs.ReadAt(context.Background(), make([]byte, 64), 128); err != nil {
		t.Fatalf("second read: %v", err)
	}
	assertNoForcedCallsSoon(t, res)
	if calls, _ := res.counts(); calls != 1 {
		t.Fatalf("expected no re-resolve before expiry, got %d calls", calls)
	}
}

// TestPredictiveResolveVelocityGateSkipsNearEOF proves that once the
// smoothed playhead velocity indicates playback will reach EOF well before
// the current token expires, a predictive attempt is skipped even while
// otherwise inside the lead window -- avoiding a wasted resolve nobody will
// use.
func TestPredictiveResolveVelocityGateSkipsNearEOF(t *testing.T) {
	data := fixtureBytes(4096)
	_, client := startOrigin(t, &bsOrigin{data: data})
	clk := newBSClock()
	start := clk.Now()
	res := &predictiveResolver{
		url:       originURL("/media.mp4"),
		selector:  "sel-1",
		size:      int64(len(data)),
		expiresAt: start.Add(5 * time.Minute),
	}
	bs := mustSource(t, ByteSourceConfig{
		Key: "http|predictive-velocity", Size: int64(len(data)),
		RangeVerified: true, Client: client, Resolve: res.Resolve, Now: clk.Now,
		PredictiveLeadMin: 3 * time.Minute,
	})

	if _, err := bs.ReadAt(context.Background(), make([]byte, 64), 0); err != nil {
		t.Fatalf("first read: %v", err)
	}
	// Establish a velocity sample that puts ETA-to-EOF well before the
	// (still far-future) expiry, while landing inside the lead window
	// (now+lead=2:30+3:00=5:30 >= 5:00 expiry) and comfortably clear of the
	// 30s reactive-expiry skew boundary.
	clk.Advance(150 * time.Second) // now = start+2:30
	if _, err := bs.ReadAt(context.Background(), make([]byte, 32), 3900); err != nil {
		t.Fatalf("second read: %v", err)
	}
	assertNoForcedCallsSoon(t, res)
	if calls, _ := res.counts(); calls != 1 {
		t.Fatalf("expected no re-resolve, got %d calls", calls)
	}
}
