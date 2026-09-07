package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/contentproof"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

type ladderTestHandler struct {
	file  httpstream.ResolvedFile
	calls atomic.Int32
}

type governorTestChunkSource struct{}

func (governorTestChunkSource) Key() string { return "governor-test" }
func (governorTestChunkSource) Chunks() []rangecache.ChunkRef {
	return []rangecache.ChunkRef{{Index: 0, Key: "0", Size: 1}}
}
func (governorTestChunkSource) Fetch(context.Context, rangecache.ChunkRef, rangecache.FetchClass) ([]byte, error) {
	return []byte{1}, nil
}
func (governorTestChunkSource) ExactSizes() bool { return true }

func (h *ladderTestHandler) Name() string { return "test" }
func (h *ladderTestHandler) Search(context.Context, httpstream.StreamQuery) ([]httpstream.SearchResult, error) {
	return nil, nil
}
func (h *ladderTestHandler) Resolve(context.Context, httpstream.ResolveRequest) ([]httpstream.ResolvedFile, error) {
	h.calls.Add(1)
	return []httpstream.ResolvedFile{h.file}, nil
}

func newHR31TestServer(t *testing.T, mode rangecache.Mode, client *http.Client) (*Server, *store.Store) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "hr3.1.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	st := store.New(db)
	cache := rangecache.New(rangecache.Config{
		DiskCachePath:   t.TempDir(),
		DiskCacheSizeMB: 64,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(cache.Close)
	ledger, err := contentproof.NewLedger(st, contentproof.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Cache.StreamMode = string(mode)
	cfg.Cache.StreamMinBufferSegments = 1
	s := &Server{
		cfg:              cfg,
		log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:            st,
		streamCache:      cache,
		httpGov:          accountgov.New("http-source-test"),
		httpContinuity:   ledger,
		httpResolveCoord: newHTTPResolveCoordinator(),
	}
	s.httpGov.SetCapacity(HTTPSourceGovOp, 1)
	s.httpClientOnce.Do(func() {
		s.httpProbeClient = client
		s.httpRelayClient = client
	})
	return s, st
}

func hr31RangeOrigin(body func() []byte, status int, calls *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if status != http.StatusPartialContent {
			w.Header().Set("Retry-After", "10")
			w.WriteHeader(status)
			return
		}
		data := body()
		spec := strings.TrimPrefix(r.Header.Get("Range"), "bytes=")
		parts := strings.SplitN(spec, "-", 2)
		start, _ := strconv.ParseInt(parts[0], 10, 64)
		end := int64(len(data) - 1)
		if len(parts) == 2 && parts[1] != "" {
			end, _ = strconv.ParseInt(parts[1], 10, 64)
		}
		if end >= int64(len(data)) {
			end = int64(len(data) - 1)
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
}

func hr31Request(t *testing.T, s *Server, item *store.Item, pf httpstream.PersistedFile, initial httpstream.ResolvedFile, handler httpstream.Handler, key httpstream.ResolveKey) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/stream/"+item.ID+"/"+pf.FileID, nil)
	req.Header.Set("Range", "bytes=0-"+strconv.FormatInt(pf.Size-1, 10))
	rec := httptest.NewRecorder()
	rng, ok := httpstream.ParseRange(req.Header.Get("Range"), pf.Size)
	if !ok {
		t.Fatal("test range unexpectedly invalid")
	}
	s.streamHTTPViaCache(rec, req, item, pf, initial, rng, handler, key)
	return rec
}

func TestHR31CacheCurrentReresolveAndProofGate(t *testing.T) {
	data := bytes.Repeat([]byte("verified-http-block"), 4096)
	var badCalls, goodCalls atomic.Int32
	bad := hr31RangeOrigin(func() []byte { return data }, http.StatusNotFound, &badCalls)
	defer bad.Close()
	good := hr31RangeOrigin(func() []byte { return data }, http.StatusPartialContent, &goodCalls)
	defer good.Close()

	s, st := newHR31TestServer(t, rangecache.ModeDisk, good.Client())
	item := &store.Item{ID: "item-hr31"}
	pf := httpstream.PersistedFile{FileID: "hf000000000001", Selector: "movie", Name: "movie.mp4", Size: int64(len(data)), ContentType: "video/mp4", RangeVerified: true}
	handler := &ladderTestHandler{file: httpstream.ResolvedFile{Selector: "movie", Name: "movie.mp4", Size: int64(len(data)), URL: good.URL, SupportsRange: true}}
	key := httpstream.ResolveKey{Version: httpstream.ResolveKeyVersion, BackendID: "test", Handler: "test", Kind: "movie", IDs: map[string]string{"test": "one"}}
	initial := handler.file
	initial.URL = bad.URL

	first := hr31Request(t, s, item, pf, initial, handler, key)
	if first.Code != http.StatusPartialContent || !bytes.Equal(first.Body.Bytes(), data) {
		t.Fatalf("first playback = %d/%d bytes, want 206/%d", first.Code, first.Body.Len(), len(data))
	}
	if badCalls.Load() != 1 || goodCalls.Load() != 1 || handler.calls.Load() != 1 {
		t.Fatalf("current/re-resolve calls = bad:%d good:%d resolve:%d, want 1/1/1", badCalls.Load(), goodCalls.Load(), handler.calls.Load())
	}

	second := hr31Request(t, s, item, pf, initial, handler, key)
	if second.Code != http.StatusPartialContent || !bytes.Equal(second.Body.Bytes(), data) {
		t.Fatalf("cache playback = %d/%d bytes, want 206/%d", second.Code, second.Body.Len(), len(data))
	}
	if badCalls.Load() != 1 || goodCalls.Load() != 1 || handler.calls.Load() != 1 {
		t.Fatalf("cache hit moved origin/backend: bad:%d good:%d resolve:%d", badCalls.Load(), goodCalls.Load(), handler.calls.Load())
	}

	repID := httpRepresentationID(item.ID, pf.FileID)
	history, err := s.httpContinuity.History(context.Background(), repID)
	if err != nil || len(history) == 0 || history[0].Relation != contentproof.ContinuityTOFU {
		t.Fatalf("TOFU continuity history = %#v, %v", history, err)
	}
	if strings.Contains(repID, "://") || strings.ContainsAny(repID, "?&") {
		t.Fatalf("representation ID is not opaque: %q", repID)
	}

	graph, err := contentproof.New(st, contentproof.Options{})
	if err != nil {
		t.Fatal(err)
	}
	conflicting := sha256.Sum256([]byte("different authoritative bytes"))
	if err := graph.Record(context.Background(), contentproof.Evidence{
		RepresentationID: repID,
		Scope:            contentproof.ScopeBlock,
		Offset:           0,
		Length:           int64(len(data)),
		Kind:             contentproof.KindAuthoritative,
		Algorithm:        contentproof.AlgorithmSHA256,
		Digest:           conflicting[:],
		Provenance:       contentproof.ProvenanceHTTPDigest,
		ObservedAt:       time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	rejected := hr31Request(t, s, item, pf, initial, handler, key)
	if rejected.Code != http.StatusBadGateway || bytes.Contains(rejected.Body.Bytes(), data[:32]) {
		t.Fatalf("conflicting cache hit = status %d body %q, want sanitized 502", rejected.Code, rejected.Body.String())
	}
	if goodCalls.Load() != 1 {
		t.Fatalf("proof rejection unexpectedly reached origin: %d calls", goodCalls.Load())
	}
}

func TestHR31MutationAndRateLimitFailBeforeHeaders(t *testing.T) {
	t.Run("mutation", func(t *testing.T) {
		firstData := bytes.Repeat([]byte("a"), 64*1024)
		secondData := bytes.Repeat([]byte("b"), len(firstData))
		current := firstData
		var calls atomic.Int32
		origin := hr31RangeOrigin(func() []byte { return current }, http.StatusPartialContent, &calls)
		defer origin.Close()
		s, _ := newHR31TestServer(t, rangecache.ModeNone, origin.Client())
		item := &store.Item{ID: "item-mutation"}
		pf := httpstream.PersistedFile{FileID: "hf000000000002", Selector: "movie", Name: "movie.mp4", Size: int64(len(current)), RangeVerified: true}
		rf := httpstream.ResolvedFile{Selector: "movie", Name: "movie.mp4", Size: int64(len(current)), URL: origin.URL, SupportsRange: true}
		handler := &ladderTestHandler{file: rf}
		key := httpstream.ResolveKey{Version: httpstream.ResolveKeyVersion, BackendID: "test", Handler: "test", Kind: "movie", IDs: map[string]string{"test": "two"}}
		if rec := hr31Request(t, s, item, pf, rf, handler, key); rec.Code != http.StatusPartialContent {
			t.Fatalf("first status = %d", rec.Code)
		}
		current = secondData
		rec := hr31Request(t, s, item, pf, rf, handler, key)
		if rec.Code != http.StatusBadGateway || bytes.Contains(rec.Body.Bytes(), secondData[:32]) {
			t.Fatalf("mutation status/body = %d/%q, want sanitized 502", rec.Code, rec.Body.String())
		}
	})

	t.Run("rate limit and cancellation", func(t *testing.T) {
		var calls atomic.Int32
		origin := hr31RangeOrigin(func() []byte { return nil }, http.StatusTooManyRequests, &calls)
		defer origin.Close()
		s, _ := newHR31TestServer(t, rangecache.ModeDisk, origin.Client())
		item := &store.Item{ID: "item-rate"}
		pf := httpstream.PersistedFile{FileID: "hf000000000003", Selector: "movie", Name: "movie.mp4", Size: 4096, RangeVerified: true}
		rf := httpstream.ResolvedFile{Selector: "movie", Name: "movie.mp4", Size: 4096, URL: origin.URL, SupportsRange: true}
		handler := &ladderTestHandler{file: rf}
		key := httpstream.ResolveKey{Version: httpstream.ResolveKeyVersion, BackendID: "test", Handler: "test", Kind: "movie", IDs: map[string]string{"test": "three"}}
		rec := hr31Request(t, s, item, pf, rf, handler, key)
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "300" {
			t.Fatalf("429 result = %d Retry-After=%q", rec.Code, rec.Header().Get("Retry-After"))
		}
		if calls.Load() != 1 || handler.calls.Load() != 0 {
			t.Fatalf("429 account stop = origin:%d resolve:%d, want 1/0", calls.Load(), handler.calls.Load())
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
		rec = httptest.NewRecorder()
		rng := &httpstream.ByteRange{Start: 0, End: pf.Size - 1}
		s.streamHTTPViaCache(rec, req, item, pf, rf, rng, handler, key)
		if !errors.Is(ctx.Err(), context.Canceled) || calls.Load() != 1 {
			t.Fatalf("cancelled request reached origin: calls=%d", calls.Load())
		}
	})
}

func TestHR31PlaybackLeaseOutranksPrewarmWithoutLeaks(t *testing.T) {
	for cycle := 0; cycle < 2; cycle++ {
		gov := accountgov.New("http-source-test")
		gov.SetCapacity(HTTPSourceGovOp, 1)
		held, err := gov.Acquire(context.Background(), HTTPSourceGovOp, accountgov.PriorityGrab, "holder")
		if err != nil {
			t.Fatal(err)
		}
		order := make(chan string, 2)
		done := make(chan error, 2)
		go func() {
			lease, err := gov.Acquire(context.Background(), HTTPSourceGovOp, accountgov.PriorityPrewarm, "prewarm")
			if err == nil {
				order <- "prewarm"
				lease.Release()
			}
			done <- err
		}()
		waitForHTTPGovQueue(t, gov, 1)

		src := newHTTPLadderChunkSource(governorTestChunkSource{}, gov, nil, "http:test", "playback")
		go func() {
			_, err := src.Fetch(context.Background(), src.Chunks()[0], rangecache.FetchDemand)
			if err == nil {
				order <- "playback"
			}
			done <- err
		}()
		waitForHTTPGovQueue(t, gov, 2)
		held.Release()

		if first := <-order; first != "playback" {
			t.Fatalf("cycle %d: first admitted operation = %q, want playback", cycle, first)
		}
		<-order
		for i := 0; i < 2; i++ {
			if err := <-done; err != nil {
				t.Fatalf("cycle %d: lease operation failed: %v", cycle, err)
			}
		}
		if gov.InUse(HTTPSourceGovOp) != 0 || gov.Waiting(HTTPSourceGovOp) != 0 {
			t.Fatalf("cycle %d leaked governor state: in_use=%d waiting=%d", cycle, gov.InUse(HTTPSourceGovOp), gov.Waiting(HTTPSourceGovOp))
		}
	}
}

func waitForHTTPGovQueue(t *testing.T, gov *accountgov.Governor, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for gov.Waiting(HTTPSourceGovOp) != want {
		if time.Now().After(deadline) {
			t.Fatalf("governor waiting=%d, want %d", gov.Waiting(HTTPSourceGovOp), want)
		}
		time.Sleep(time.Millisecond)
	}
}
