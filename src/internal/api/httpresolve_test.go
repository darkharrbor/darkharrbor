package api

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/httpstream"
)

type countingHandler struct {
	calls   int64
	delay   time.Duration
	files   []httpstream.ResolvedFile
	err     error
	onCall  func(op httpstream.ResolveOperation)
	blocker chan struct{} // if non-nil, Resolve blocks until closed
}

func (h *countingHandler) Name() string { return "test" }
func (h *countingHandler) Search(ctx context.Context, q httpstream.StreamQuery) ([]httpstream.SearchResult, error) {
	return nil, nil
}
func (h *countingHandler) Resolve(ctx context.Context, req httpstream.ResolveRequest) ([]httpstream.ResolvedFile, error) {
	atomic.AddInt64(&h.calls, 1)
	if h.onCall != nil {
		h.onCall(req.Operation)
	}
	if h.blocker != nil {
		select {
		case <-h.blocker:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if h.delay > 0 {
		select {
		case <-time.After(h.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return h.files, h.err
}

func testKey() httpstream.ResolveKey {
	return httpstream.ResolveKey{
		Version: httpstream.ResolveKeyVersion, BackendID: "b", Handler: "test",
		Kind: "movie", IDs: map[string]string{"x": "1"}, Selector: "sel",
	}
}

func oneFile(url string) []httpstream.ResolvedFile {
	return []httpstream.ResolvedFile{{Selector: "sel", URL: url}}
}

func TestHTTPResolveCoordinatorCoalescesConcurrentNormalCalls(t *testing.T) {
	h := &countingHandler{
		blocker: make(chan struct{}),
		files:   oneFile("https://cdn.example/a"),
	}
	c := newHTTPResolveCoordinator()

	const n = 20
	var wg sync.WaitGroup
	results := make([]httpstream.ResolvedFile, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = c.Resolve(context.Background(), "item1", "file1", h, testKey(), "sel", false, "")
		}(i)
	}
	// Give every goroutine a chance to arrive and join the single in-flight call.
	time.Sleep(50 * time.Millisecond)
	close(h.blocker)
	wg.Wait()

	if got := atomic.LoadInt64(&h.calls); got != 1 {
		t.Fatalf("expected exactly 1 backend call for %d concurrent waiters, got %d", n, got)
	}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("waiter %d: unexpected error %v", i, errs[i])
		}
		if results[i].URL != "https://cdn.example/a" {
			t.Fatalf("waiter %d: unexpected result %+v", i, results[i])
		}
	}
}

func TestHTTPResolveCoordinatorDoesNotCoalesceDifferentKeys(t *testing.T) {
	h := &countingHandler{files: oneFile("https://cdn.example/a")}
	c := newHTTPResolveCoordinator()

	if _, err := c.Resolve(context.Background(), "item1", "fileA", h, testKey(), "sel", false, ""); err != nil {
		t.Fatalf("resolve fileA: %v", err)
	}
	if _, err := c.Resolve(context.Background(), "item1", "fileB", h, testKey(), "sel", false, ""); err != nil {
		t.Fatalf("resolve fileB: %v", err)
	}
	if got := atomic.LoadInt64(&h.calls); got != 2 {
		t.Fatalf("expected 2 backend calls for 2 distinct files, got %d", got)
	}
}

func TestHTTPResolveCoordinatorForcedRefreshStartsFreshCall(t *testing.T) {
	var ops []httpstream.ResolveOperation
	var mu sync.Mutex
	h := &countingHandler{
		files: oneFile("https://cdn.example/a"),
		onCall: func(op httpstream.ResolveOperation) {
			mu.Lock()
			ops = append(ops, op)
			mu.Unlock()
		},
	}
	c := newHTTPResolveCoordinator()

	if _, err := c.Resolve(context.Background(), "item1", "file1", h, testKey(), "sel", false, ""); err != nil {
		t.Fatalf("normal resolve: %v", err)
	}
	if _, err := c.Resolve(context.Background(), "item1", "file1", h, testKey(), "sel", true, "ctx-1"); err != nil {
		t.Fatalf("forced resolve: %v", err)
	}
	if got := atomic.LoadInt64(&h.calls); got != 2 {
		t.Fatalf("expected forced refresh to always make its own fresh call, got %d total calls", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ops) != 2 || ops[0] != httpstream.ResolveNormal || ops[1] != httpstream.ResolveForceRefresh {
		t.Fatalf("unexpected operation sequence: %+v", ops)
	}
}

func TestHTTPResolveCoordinatorFailureCooldown(t *testing.T) {
	h := &countingHandler{err: errors.New("backend down")}
	c := newHTTPResolveCoordinator()

	if _, err := c.Resolve(context.Background(), "item1", "file1", h, testKey(), "sel", false, ""); err == nil {
		t.Fatal("expected first call to fail")
	}
	// A second NORMAL caller within the cooldown window must not hit the
	// backend again — it gets the same cached failure immediately.
	if _, err := c.Resolve(context.Background(), "item1", "file1", h, testKey(), "sel", false, ""); err == nil {
		t.Fatal("expected cooldown-short-circuited failure")
	}
	if got := atomic.LoadInt64(&h.calls); got != 1 {
		t.Fatalf("expected exactly 1 backend call during cooldown, got %d", got)
	}

	// Forced refresh always bypasses the cooldown.
	if _, err := c.Resolve(context.Background(), "item1", "file1", h, testKey(), "sel", true, ""); err == nil {
		t.Fatal("expected forced call to also fail (handler still errors)")
	}
	if got := atomic.LoadInt64(&h.calls); got != 2 {
		t.Fatalf("expected forced refresh to bypass cooldown and make a 2nd call, got %d", got)
	}
}

func TestHTTPResolveCoordinatorWaiterCancelIsIndependent(t *testing.T) {
	h := &countingHandler{
		blocker: make(chan struct{}),
		files:   oneFile("https://cdn.example/a"),
	}
	c := newHTTPResolveCoordinator()

	waiterCtx, cancel := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		_, err := c.Resolve(waiterCtx, "item1", "file1", h, testKey(), "sel", false, "")
		waiterDone <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel() // this waiter gives up...

	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled for the cancelled waiter, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled waiter did not return promptly")
	}

	// ...but the leader call itself must still be running and completable
	// by a second, still-live waiter — the first waiter's cancellation must
	// not have killed the shared in-flight call.
	secondDone := make(chan error, 1)
	go func() {
		_, err := c.Resolve(context.Background(), "item1", "file1", h, testKey(), "sel", false, "")
		secondDone <- err
	}()
	time.Sleep(20 * time.Millisecond)
	close(h.blocker)

	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("second waiter: unexpected error %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("second waiter did not complete")
	}
	if got := atomic.LoadInt64(&h.calls); got != 1 {
		t.Fatalf("expected the cancelled waiter to have joined the same single backend call, got %d calls", got)
	}
}

func TestHTTPResolveCoordinatorPurgeRemovesEntry(t *testing.T) {
	c := newHTTPResolveCoordinator()
	e1 := c.entryFor("item1", "file1")
	c.purge("item1", "file1")
	e2 := c.entryFor("item1", "file1")
	if e1 == e2 {
		t.Fatal("expected purge to drop the entry so a fresh one is allocated")
	}
}

// ── HS-3.9 (A20) cache tests ─────────────────────────────────────────────────

func TestHTTPResolveCoordinatorCacheHitAvoidsBackendCall(t *testing.T) {
	h := &countingHandler{files: oneFile("https://cdn.example/a")} // no ExpiresAt -> missing-TTL cache
	c := newHTTPResolveCoordinator()

	if _, err := c.Resolve(context.Background(), "item1", "file1", h, testKey(), "sel", false, ""); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if _, err := c.Resolve(context.Background(), "item1", "file1", h, testKey(), "sel", false, ""); err != nil {
		t.Fatalf("second resolve (expected cache hit): %v", err)
	}
	if got := atomic.LoadInt64(&h.calls); got != 1 {
		t.Fatalf("expected the second resolve to be served from cache with no backend call, got %d calls", got)
	}
}

func TestHTTPResolveCoordinatorForcedRefreshBypassesCache(t *testing.T) {
	h := &countingHandler{files: oneFile("https://cdn.example/a")}
	c := newHTTPResolveCoordinator()

	if _, err := c.Resolve(context.Background(), "item1", "file1", h, testKey(), "sel", false, ""); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if _, err := c.Resolve(context.Background(), "item1", "file1", h, testKey(), "sel", true, ""); err != nil {
		t.Fatalf("forced resolve: %v", err)
	}
	if got := atomic.LoadInt64(&h.calls); got != 2 {
		t.Fatalf("expected forced refresh to always bypass the cache, got %d calls", got)
	}
}

func TestHTTPResolveCoordinatorCoalescesConcurrentForcedRefresh(t *testing.T) {
	h := &countingHandler{
		blocker: make(chan struct{}),
		files:   oneFile("https://cdn.example/fresh"),
	}
	c := newHTTPResolveCoordinator()

	const n = 20
	start := make(chan struct{})
	ready := make(chan struct{}, n)
	errs := make(chan error, n)
	for range n {
		go func() {
			ready <- struct{}{}
			<-start
			_, err := c.Resolve(context.Background(), "item1", "file1", h, testKey(), "sel", true, "opaque-refresh")
			errs <- err
		}()
	}
	for range n {
		<-ready
	}
	close(start)
	time.Sleep(50 * time.Millisecond)
	close(h.blocker)
	for range n {
		if err := <-errs; err != nil {
			t.Fatalf("forced refresh waiter: %v", err)
		}
	}
	if got := atomic.LoadInt64(&h.calls); got != 1 {
		t.Fatalf("expected %d concurrent forced refreshes to share one handler call, got %d", n, got)
	}
}

func TestHTTPResolveCoordinatorReusesNewerGenerationForLateForcedRefresh(t *testing.T) {
	h := &countingHandler{files: []httpstream.ResolvedFile{{
		Selector:       "sel",
		URL:            "https://cdn.example/stale",
		RefreshContext: "stale-generation",
	}}}
	c := newHTTPResolveCoordinator()

	if _, err := c.Resolve(context.Background(), "item1", "file1", h, testKey(), "sel", false, ""); err != nil {
		t.Fatalf("initial resolve: %v", err)
	}
	h.files = []httpstream.ResolvedFile{{
		Selector:       "sel",
		URL:            "https://cdn.example/fresh",
		RefreshContext: "fresh-generation",
	}}
	if _, err := c.Resolve(context.Background(), "item1", "file1", h, testKey(), "sel", true, "stale-generation"); err != nil {
		t.Fatalf("first forced refresh: %v", err)
	}
	got, err := c.Resolve(context.Background(), "item1", "file1", h, testKey(), "sel", true, "stale-generation")
	if err != nil {
		t.Fatalf("late forced refresh: %v", err)
	}
	if got.URL != "https://cdn.example/fresh" {
		t.Fatalf("late forced refresh URL=%q", got.URL)
	}
	if calls := atomic.LoadInt64(&h.calls); calls != 2 {
		t.Fatalf("late stale generation caused a third handler call: calls=%d", calls)
	}
}

func TestHTTPResolveEffectiveTTL(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)

	// Missing expiresAt -> short conservative TTL, cacheable.
	if ttl, ok := httpResolveEffectiveTTL(time.Time{}, now); !ok || ttl != httpResolveMissingTTL {
		t.Fatalf("missing expiresAt: got ttl=%v ok=%v, want %v/true", ttl, ok, httpResolveMissingTTL)
	}

	// Past expiresAt -> never cached.
	if _, ok := httpResolveEffectiveTTL(now.Add(-time.Minute), now); ok {
		t.Fatal("past expiresAt must never be cached")
	}

	// Within skew of "now" -> effectively past, never cached.
	if _, ok := httpResolveEffectiveTTL(now.Add(httpResolveExpirySkew/2), now); ok {
		t.Fatal("expiresAt inside the skew margin must never be cached")
	}

	// Valid near-future expiresAt -> ttl = remaining - skew.
	future := now.Add(2 * time.Minute)
	ttl, ok := httpResolveEffectiveTTL(future, now)
	if !ok {
		t.Fatal("expected a valid near-future expiresAt to be cacheable")
	}
	want := 2*time.Minute - httpResolveExpirySkew
	if ttl != want {
		t.Fatalf("got ttl=%v, want %v", ttl, want)
	}

	// Far-future expiresAt (a lying backend) -> capped at the handler max.
	farFuture := now.Add(24 * time.Hour)
	ttl, ok = httpResolveEffectiveTTL(farFuture, now)
	if !ok || ttl != httpResolveMaxTTL {
		t.Fatalf("far-future expiresAt: got ttl=%v ok=%v, want %v/true (capped)", ttl, ok, httpResolveMaxTTL)
	}
}

func TestHTTPResolveCacheableCopyBoundsOversizedHeaders(t *testing.T) {
	big := make(map[string]string)
	big["X-Huge"] = string(make([]byte, httpResolveMaxHeaderBytes+1))
	f := httpstream.ResolvedFile{Selector: "sel", URL: "https://cdn.example/a", RequestHeaders: big}

	capped := httpResolveCacheableCopy(f)
	if capped.RequestHeaders != nil {
		t.Fatal("expected oversized header set to be dropped from the cacheable copy")
	}
	if f.RequestHeaders == nil {
		t.Fatal("the original ResolvedFile used for THIS request must be unaffected")
	}
}

func TestHTTPResolveCoordinatorInvalidateForcesFreshResolve(t *testing.T) {
	h := &countingHandler{files: oneFile("https://cdn.example/a")}
	c := newHTTPResolveCoordinator()

	if _, err := c.Resolve(context.Background(), "item1", "file1", h, testKey(), "sel", false, ""); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	c.Invalidate("item1", "file1")
	if _, err := c.Resolve(context.Background(), "item1", "file1", h, testKey(), "sel", false, ""); err != nil {
		t.Fatalf("post-invalidate resolve: %v", err)
	}
	if got := atomic.LoadInt64(&h.calls); got != 2 {
		t.Fatalf("expected Invalidate to force a fresh backend call on the next normal resolve, got %d calls", got)
	}
}
