package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// HS-3.11 (A18): the full HEAD/Range/refresh/status matrix. These tests
// drive relayHTTPSource directly (bypassing streamHTTPItem's own
// resolve-key/range-vs-persisted-size plumbing, which is exercised
// elsewhere) to prove the one-forced-refresh-before-headers cycle and its
// final status mapping.

// sequenceHandler.Resolve returns urls[call-1] (pinned at the last entry
// once exhausted) each time it is called, and never returns an error itself
// — the *destination* server is what fails, not the handler, matching a
// live "forced refresh got a fresh URL that's still bad" scenario.
type sequenceHandler struct {
	calls int64
	urls  []string
}

func (h *sequenceHandler) Name() string { return "test" }
func (h *sequenceHandler) Search(context.Context, httpstream.StreamQuery) ([]httpstream.SearchResult, error) {
	return nil, nil
}
func (h *sequenceHandler) Resolve(ctx context.Context, req httpstream.ResolveRequest) ([]httpstream.ResolvedFile, error) {
	i := atomic.AddInt64(&h.calls, 1) - 1
	if int(i) >= len(h.urls) {
		i = int64(len(h.urls) - 1)
	}
	return []httpstream.ResolvedFile{{Selector: "sel", URL: h.urls[i]}}, nil
}

func relayTestServer(t *testing.T) *Server {
	t.Helper()
	s := httpTestServer("0123456789abcdef0123456789abcdef", true)
	s.httpResolveCoord = newHTTPResolveCoordinator()
	return s
}

func relayTestItemAndFile() (*store.Item, httpstream.PersistedFile) {
	item := &store.Item{ID: "item1"}
	pf := httpstream.PersistedFile{FileID: "file1", Selector: "sel", Name: "f.mp4", Size: 100, ContentType: "video/mp4", RangeVerified: true}
	return item, pf
}

// Refreshable failure (403) on the first attempt, success on the retry
// after exactly one forced refresh: the client sees a clean 200 and never
// learns the first attempt happened.
func TestRelayHTTPSourceRefreshesOnceThenServes(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
	}))
	defer good.Close()

	s := relayTestServer(t)
	s.httpClientOnce.Do(func() {
		s.httpProbeClient = bad.Client()
		s.httpRelayClient = bad.Client()
	})
	h := &sequenceHandler{urls: []string{good.URL + "/f"}}
	item, pf := relayTestItemAndFile()
	rf := httpstream.ResolvedFile{Selector: "sel", URL: bad.URL + "/f"}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stream/x/f", nil)
	s.relayHTTPSource(rec, req, item, pf, rf, nil, h, testKey())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if body, _ := io.ReadAll(rec.Body); string(body) != "hello" {
		t.Fatalf("body = %q, want %q", body, "hello")
	}
	if got := atomic.LoadInt64(&h.calls); got != 1 {
		t.Fatalf("expected exactly 1 handler.Resolve call (the forced refresh — the initial attempt uses rf directly), got %d", got)
	}
}

// Both attempts return a malformed 206 (bad Content-Range) — exhausted
// after the one allowed refresh, final status is 502, never relayed raw.
func TestRelayHTTPSourceMalformedRangeExhaustedIsBadGateway(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", "bytes 5-10/1000") // does not match requested 0-9
		w.WriteHeader(http.StatusPartialContent)
	}))
	defer bad.Close()

	s := relayTestServer(t)
	s.httpClientOnce.Do(func() {
		s.httpProbeClient = bad.Client()
		s.httpRelayClient = bad.Client()
	})
	h := &sequenceHandler{urls: []string{bad.URL + "/f"}}
	item, pf := relayTestItemAndFile()
	rf := httpstream.ResolvedFile{Selector: "sel", URL: bad.URL + "/f"}
	rng := &httpstream.ByteRange{Start: 0, End: 9}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stream/x/f", nil)
	s.relayHTTPSource(rec, req, item, pf, rf, rng, h, testKey())

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt64(&h.calls); got != 1 {
		t.Fatalf("expected exactly 1 handler.Resolve call (the forced refresh), got %d", got)
	}
}

// Both attempts return 403 — exhausted after the one allowed refresh, final
// status is a sanitized 503 with the default bounded Retry-After. Upstream
// body is never relayed.
func TestRelayHTTPSourceExhaustedNonMalformedIsServiceUnavailable(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "you shall not pass, upstream secret leak test", http.StatusForbidden)
	}))
	defer bad.Close()

	s := relayTestServer(t)
	s.httpClientOnce.Do(func() {
		s.httpProbeClient = bad.Client()
		s.httpRelayClient = bad.Client()
	})
	h := &sequenceHandler{urls: []string{bad.URL + "/f", bad.URL + "/f"}}
	item, pf := relayTestItemAndFile()
	rf := httpstream.ResolvedFile{Selector: "sel", URL: h.urls[0]}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stream/x/f", nil)
	s.relayHTTPSource(rec, req, item, pf, rf, nil, h, testKey())

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "300" {
		t.Fatalf("Retry-After = %q, want 300 (default)", got)
	}
	if strings.Contains(rec.Body.String(), "upstream secret leak test") {
		t.Fatal("upstream error body was relayed to the client — must never happen")
	}
}

// A 429 with a bounded upstream Retry-After is honored (capped, never
// passed through unbounded) once refresh is exhausted.
func TestRelayHTTPSourceUpstream429HonorsBoundedRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name      string
		upstream  string
		wantAfter string
	}{
		{"within bound", "120", "120"},
		{"exceeds bound capped", "99999", "300"},
		{"garbage falls back to default", "not-a-number", "300"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", tc.upstream)
				http.Error(w, "slow down", http.StatusTooManyRequests)
			}))
			defer bad.Close()

			s := relayTestServer(t)
			s.httpClientOnce.Do(func() {
				s.httpProbeClient = bad.Client()
				s.httpRelayClient = bad.Client()
			})
			h := &sequenceHandler{urls: []string{bad.URL + "/f", bad.URL + "/f"}}
			item, pf := relayTestItemAndFile()
			rf := httpstream.ResolvedFile{Selector: "sel", URL: h.urls[0]}

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/stream/x/f", nil)
			s.relayHTTPSource(rec, req, item, pf, rf, nil, h, testKey())

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", rec.Code)
			}
			if got := rec.Header().Get("Retry-After"); got != tc.wantAfter {
				t.Fatalf("Retry-After = %q, want %q", got, tc.wantAfter)
			}
		})
	}
}

// Upstream returning its own 416 is terminal — never treated as a refresh
// trigger (A18: "never refresh ... a valid 416"), matching only one
// handler call and a clean 416 to the client.
func TestRelayHTTPSourceUpstream416NeverRefreshed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes */100")
		http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
	}))
	defer srv.Close()

	s := relayTestServer(t)
	s.httpClientOnce.Do(func() {
		s.httpProbeClient = srv.Client()
		s.httpRelayClient = srv.Client()
	})
	h := &sequenceHandler{urls: []string{srv.URL + "/f", srv.URL + "/f"}}
	item, pf := relayTestItemAndFile()
	rf := httpstream.ResolvedFile{Selector: "sel", URL: h.urls[0]}
	rng := &httpstream.ByteRange{Start: 0, End: 9}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stream/x/f", nil)
	s.relayHTTPSource(rec, req, item, pf, rf, rng, h, testKey())

	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want 416", rec.Code)
	}
	if got := atomic.LoadInt64(&h.calls); got != 0 {
		t.Fatalf("expected upstream 416 to never trigger a refresh, got %d handler calls", got)
	}
}

// Client cancellation before headers must never trigger a refresh either.
func TestRelayHTTPSourceClientCancelNeverRefreshes(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // hang until the client gives up
	}))
	defer srv.Close()
	defer close(block)

	s := relayTestServer(t)
	s.httpClientOnce.Do(func() {
		s.httpProbeClient = srv.Client()
		s.httpRelayClient = srv.Client()
	})
	h := &sequenceHandler{urls: []string{srv.URL + "/f", srv.URL + "/f"}}
	item, pf := relayTestItemAndFile()
	rf := httpstream.ResolvedFile{Selector: "sel", URL: h.urls[0]}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/stream/x/f", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		s.relayHTTPSource(rec, req, item, pf, rf, nil, h, testKey())
		close(done)
	}()
	cancel()
	<-done

	if got := atomic.LoadInt64(&h.calls); got != 0 {
		t.Fatalf("expected client cancellation to never trigger a refresh, got %d handler calls", got)
	}
}

func TestBoundedRetryAfterSeconds(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "300"},
		{"   ", "300"},
		{"0", "300"},
		{"-5", "300"},
		{"garbage", "300"},
		{"120", "120"},
		{"300", "300"},
		{"301", "300"},
		{"99999", "300"},
		{"1", "1"},
	}
	for _, c := range cases {
		if got := boundedRetryAfterSeconds(c.in); got != c.want {
			t.Fatalf("boundedRetryAfterSeconds(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
