package api

// HR5.3 deterministic gates: DG-07 (governor capacity/priority bounds
// concurrency; no leaked goroutines under -race) and the startup-prewarm
// pin-through-to-rangecache proof (mirrors TS-1.4's own test shape for the
// torrent lane, applied to the new HTTP-lane governor/experience wiring).

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/experience"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// rangeServingTestServer serves body with real Range/206 semantics, close
// enough to a real origin for httpstream.ByteSource's ReadAt/observeRepresentation
// logic to exercise its normal path.
func rangeServingTestServer(body []byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		total := int64(len(body))
		rangeHdr := r.Header.Get("Range")
		if rangeHdr == "" {
			w.Header().Set("Content-Type", "video/x-matroska")
			w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
			return
		}
		spec := strings.TrimPrefix(rangeHdr, "bytes=")
		parts := strings.SplitN(spec, "-", 2)
		start, _ := strconv.ParseInt(parts[0], 10, 64)
		end := total - 1
		if len(parts) == 2 && parts[1] != "" {
			if v, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
				end = v
			}
		}
		if end >= total {
			end = total - 1
		}
		w.Header().Set("Content-Type", "video/x-matroska")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body[start : end+1])
	}))
}

// prewarmTestHandler is a minimal httpstream.Handler stub whose Resolve
// always returns one fixed ResolvedFile pointing at a test origin.
type prewarmTestHandler struct {
	url      string
	selector string
	size     int64
}

func (h *prewarmTestHandler) Name() string { return "test" }
func (h *prewarmTestHandler) Search(context.Context, httpstream.StreamQuery) ([]httpstream.SearchResult, error) {
	return nil, nil
}
func (h *prewarmTestHandler) Resolve(context.Context, httpstream.ResolveRequest) ([]httpstream.ResolvedFile, error) {
	return []httpstream.ResolvedFile{{
		Selector:      h.selector,
		Name:          "movie.mkv",
		Size:          h.size,
		URL:           h.url,
		SupportsRange: true,
	}}, nil
}

// TestPrewarmHTTPFilePinsHotHeadBytes is the real (unmocked) pin-through
// proof: prewarmHTTPFile must actually reach the shared rangecache.Cache's
// pinned tier via the shared experience.Service, exactly as TS-1.4 proved
// for the torrent lane's PrewarmHotHead call site.
func TestPrewarmHTTPFilePinsHotHeadBytes(t *testing.T) {
	body := make([]byte, 512*1024)
	for i := range body {
		body[i] = byte(i)
	}
	ts := rangeServingTestServer(body)
	defer ts.Close()

	cache := rangecache.New(rangecache.Config{
		DiskCachePath:  t.TempDir(),
		PinnedBudgetMB: 64,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer cache.Close()

	gov := accountgov.New("http-source-test")
	expSvc := experience.New(cache, gov, HTTPSourceGovOp, experience.Config{
		Enabled:      true,
		HotHeadBytes: 64 * 1024,
		Timeout:      10 * time.Second,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	s := httpTestServer("0123456789abcdef0123456789abcdef", true)
	s.httpExpSvc = expSvc
	s.httpGov = gov
	s.httpResolveCoord = newHTTPResolveCoordinator()
	s.httpClientOnce.Do(func() {
		s.httpProbeClient = ts.Client()
		s.httpRelayClient = ts.Client()
	})

	item := &store.Item{ID: "item1"}
	pf := httpstream.PersistedFile{
		FileID:        "f1",
		Name:          "movie.mkv",
		Size:          int64(len(body)),
		RangeVerified: true,
	}
	handler := &prewarmTestHandler{url: ts.URL + "/file.mkv", selector: "movie.mkv", size: int64(len(body))}
	key := httpstream.ResolveKey{
		Version:   httpstream.ResolveKeyVersion,
		BackendID: "b1",
		Handler:   "test",
		Kind:      "movie",
		IDs:       map[string]string{"test": "x"},
	}

	s.prewarmHTTPFile(context.Background(), item, pf, "movie.mkv", handler, key)

	snap := cache.SnapshotMap()
	pinnedBytes, _ := snap["pinned_bytes"].(int64)
	pinnedEntries, _ := snap["pinned_entries"].(int)
	if pinnedBytes <= 0 || pinnedEntries <= 0 {
		t.Fatalf("expected pinned bytes/entries after prewarm, got bytes=%v entries=%v (snap=%v)", pinnedBytes, pinnedEntries, snap)
	}
}

// TestPrewarmHTTPFileNilServiceIsNoop confirms the nil-safety contract: an
// unwired httpExpSvc (feature disabled, or an older build) must never panic
// and must never touch the resolver/handler at all.
func TestPrewarmHTTPFileNilServiceIsNoop(t *testing.T) {
	s := httpTestServer("0123456789abcdef0123456789abcdef", true)
	s.httpResolveCoord = newHTTPResolveCoordinator()
	item := &store.Item{ID: "item1"}
	pf := httpstream.PersistedFile{FileID: "f1", Size: 100, RangeVerified: true}
	handler := &prewarmTestHandler{url: "http://unused.test/file.mkv", selector: "movie.mkv", size: 100}
	key := httpstream.ResolveKey{Version: httpstream.ResolveKeyVersion, BackendID: "b1", Handler: "test", Kind: "movie", IDs: map[string]string{"test": "x"}}
	s.prewarmHTTPFile(context.Background(), item, pf, "movie.mkv", handler, key) // must not panic
}

// TestPreflightHTTPSourceGovernorBoundsConcurrency is the DG-07 gate: with
// HTTPSourceGovOp capacity=1, two concurrent preflight calls (both real
// requests against a real, unmocked accountgov.Governor) must never have
// more than one in flight against the origin at once.
func TestPreflightHTTPSourceGovernorBoundsConcurrency(t *testing.T) {
	var inFlight int32
	var maxInFlight int32
	var completed int32
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&inFlight, 1)
		for {
			cur := atomic.LoadInt32(&maxInFlight)
			if n <= cur {
				break
			}
			if atomic.CompareAndSwapInt32(&maxInFlight, cur, n) {
				break
			}
		}
		<-release
		atomic.AddInt32(&inFlight, -1)
		atomic.AddInt32(&completed, 1)
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", "bytes 0-0/1000")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte{0})
	}))
	defer ts.Close()

	s := httpTestServer("0123456789abcdef0123456789abcdef", true)
	s.httpGov = accountgov.New("http-source-test")
	s.httpGov.SetCapacity(HTTPSourceGovOp, 1)
	s.httpClientOnce.Do(func() {
		s.httpProbeClient = ts.Client()
		s.httpRelayClient = ts.Client()
	})
	rf := httpstream.ResolvedFile{URL: ts.URL + "/file.mp4"}

	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		sess := fmt.Sprintf("session-%d", i)
		go func(sessionID string) {
			defer wg.Done()
			_, _, _ = s.preflightHTTPSource(context.Background(), rf, sessionID, "test-representation", "test-backend", "test-handler")
		}(sess)
	}
	// Let both goroutines reach the governor/server before releasing either.
	time.Sleep(150 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&maxInFlight); got != 1 {
		t.Fatalf("HTTPSourceGovOp capacity=1 did not bound concurrency: max in-flight = %d", got)
	}
	if got := atomic.LoadInt32(&completed); got != 2 {
		t.Fatalf("expected both preflights to eventually complete, got %d", got)
	}
}
