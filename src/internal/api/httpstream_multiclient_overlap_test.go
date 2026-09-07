package api

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// HR3.5 — Shield origins by coalescing eligible multi-client overlaps while
// preserving independent ranges.
//
// HR1.3 built the generic mechanism (global in-flight payload coalescing
// plus sparse block identity) and proved it against a standalone ByteSource
// fixture, explicitly deferring "multi-client overlap policy" and any proof
// through the real HTTP playback ladder to this row (see WORKLOG "HR1.3
// closed" and blocksource.go's own doc comment). HR3.1 wired GetChunk/Stream
// calls into streamHTTPViaCache with the deployed HARRBOR_STREAM_CACHE_MODE
// (disk in this deployment), but no test exercises two concurrent real
// requests through httpLadderChunkSource/newHTTPLadderChunkSource end to
// end. This test closes exactly that gap: it proves, through the actual
// production call path, that (a) a byte region wanted by more than one
// concurrent client causes exactly one upstream origin read, and (b) each
// client still receives precisely the bytes IT asked for -- overlap
// sharing never bleeds one client's window into another's response.

// blockGatedOrigin serves ranged GETs from data. Any request whose range
// falls within the shared "gated" byte window is delayed until release is
// closed, guaranteeing that if two callers both need that window, their
// origin requests (if more than one occurred) would be in flight
// simultaneously -- the condition eligible coalescing must catch.
func blockGatedOrigin(t *testing.T, data []byte, gateStart, gateEnd int64, release <-chan struct{}, calls *atomic.Int32, gatedCalls *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
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
		if start < gateEnd && end >= gateStart {
			gatedCalls.Add(1)
			<-release
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
}

func hr35RequestRange(t *testing.T, s *Server, item *store.Item, pf httpstream.PersistedFile, initial httpstream.ResolvedFile, handler httpstream.Handler, key httpstream.ResolveKey, start, end int64) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/stream/"+item.ID+"/"+pf.FileID, nil)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	rec := httptest.NewRecorder()
	rng, ok := httpstream.ParseRange(req.Header.Get("Range"), pf.Size)
	if !ok {
		t.Fatal("test range unexpectedly invalid")
	}
	s.streamHTTPViaCache(rec, req, item, pf, initial, rng, handler, key)
	return rec
}

func TestHR35MultiClientOverlapCoalescesSharedBlockPreservesIndependentRanges(t *testing.T) {
	const blockSize = rangecache.DefaultBlockBytes // 4 MiB; production block geometry, unmodified by this row
	total := blockSize * 3
	data := make([]byte, total)
	for i := range data {
		data[i] = byte(i)
	}

	// Client A wants blocks 0-1 (bytes 0 .. 5MiB-1).
	// Client B wants blocks 1-2 (bytes 4MiB .. 9MiB-1).
	// Block 1 (4MiB..8MiB) is the shared, gated region both clients need.
	aStart, aEnd := int64(0), blockSize+blockSize/2-1
	bStart, bEnd := blockSize, total-1

	var calls, gatedCalls atomic.Int32
	release := make(chan struct{})
	origin := blockGatedOrigin(t, data, blockSize, blockSize*2, release, &calls, &gatedCalls)
	defer origin.Close()

	s, _ := newHR31TestServer(t, rangecache.ModeDisk, origin.Client())
	// Real concurrency, not governor-serialized: two real simultaneous
	// clients must be able to hold leases at once for this to be a
	// meaningful concurrent-overlap proof.
	s.httpGov.SetCapacity(HTTPSourceGovOp, 4)

	item := &store.Item{ID: "item-hr35"}
	pf := httpstream.PersistedFile{FileID: "hf000000000002", Selector: "movie", Name: "movie.mp4", Size: total, ContentType: "video/mp4", RangeVerified: true}
	handler := &ladderTestHandler{file: httpstream.ResolvedFile{Selector: "movie", Name: "movie.mp4", Size: total, URL: origin.URL, SupportsRange: true}}
	key := httpstream.ResolveKey{Version: httpstream.ResolveKeyVersion, BackendID: "test", Handler: "test", Kind: "movie", IDs: map[string]string{"test": "one"}}
	initial := handler.file

	var wg sync.WaitGroup
	var recA, recB *httptest.ResponseRecorder
	wg.Add(2)
	go func() {
		defer wg.Done()
		recA = hr35RequestRange(t, s, item, pf, initial, handler, key, aStart, aEnd)
	}()
	go func() {
		defer wg.Done()
		recB = hr35RequestRange(t, s, item, pf, initial, handler, key, bStart, bEnd)
	}()

	// Give both goroutines a chance to reach the gated block-1 fetch before
	// releasing the origin -- if coalescing failed, TWO gated requests would
	// be in flight now; releasing lets us observe how many actually landed.
	deadline := time.After(2 * time.Second)
	for gatedCalls.Load() < 1 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for the shared block-1 fetch to reach the origin")
		case <-time.After(5 * time.Millisecond):
		}
	}
	// Wait for the second client to resolve its side of block 1: either it
	// wrongly reaches the origin on its own (gatedCalls becomes 2) or it
	// correctly joins client A's in-flight fetch (coalesced_joins becomes
	// >= 1). Poll for either outcome instead of a fixed sleep -- a fixed
	// 100ms window is a flaky proxy for "long enough" under CPU contention
	// from concurrently running packages (observed failing under
	// `go test ./...` on a constrained/shared runner while 100% reliable
	// in isolation across 15 runs); this keeps the same 2s budget without
	// assuming a specific scheduler latency.
	settleDeadline := time.After(2 * time.Second)
settle:
	for {
		if gatedCalls.Load() >= 2 {
			break settle
		}
		if joins, ok := s.streamCache.SnapshotMap()["coalesced_joins"].(int64); ok && joins >= 1 {
			break settle
		}
		select {
		case <-settleDeadline:
			break settle
		case <-time.After(5 * time.Millisecond):
		}
	}
	close(release)
	wg.Wait()

	if recA.Code != http.StatusPartialContent {
		t.Fatalf("client A status = %d, want 206", recA.Code)
	}
	if recB.Code != http.StatusPartialContent {
		t.Fatalf("client B status = %d, want 206", recB.Code)
	}
	wantA := data[aStart : aEnd+1]
	wantB := data[bStart : bEnd+1]
	if !bytes.Equal(recA.Body.Bytes(), wantA) {
		t.Fatalf("client A got %d bytes not matching its own requested range (%d bytes) -- overlap bled across clients", recA.Body.Len(), len(wantA))
	}
	if !bytes.Equal(recB.Body.Bytes(), wantB) {
		t.Fatalf("client B got %d bytes not matching its own requested range (%d bytes) -- overlap bled across clients", recB.Body.Len(), len(wantB))
	}

	// The shared block (block 1) must have reached the origin exactly once:
	// eligible overlap coalesced into a single upstream read.
	if gatedCalls.Load() != 1 {
		t.Fatalf("gated (shared block 1) origin calls = %d, want exactly 1 -- multi-client overlap was not shielded", gatedCalls.Load())
	}
	// Total origin calls: block 0 (A only) + block 1 (shared, once) + block 2
	// (B only) = 3. More would mean an independent range was needlessly
	// re-fetched; fewer would mean independent ranges were incorrectly
	// merged into one shared fetch.
	if calls.Load() != 3 {
		t.Fatalf("total origin calls = %d, want exactly 3 (block0 + shared block1 once + block2)", calls.Load())
	}

	joins := s.streamCache.SnapshotMap()["coalesced_joins"]
	if n, ok := joins.(int64); !ok || n < 1 {
		t.Fatalf("coalesced_joins = %v, want >= 1", joins)
	}
}

// TestHR35NonOverlappingClientsStayFullyIndependent proves the negative
// half of the same contract: two clients whose ranges never touch the same
// block cause zero coalescing and zero cross-talk.
func TestHR35NonOverlappingClientsStayFullyIndependent(t *testing.T) {
	const blockSize = rangecache.DefaultBlockBytes
	total := blockSize * 4
	data := make([]byte, total)
	for i := range data {
		data[i] = byte(i)
	}

	var calls atomic.Int32
	origin := hr31RangeOrigin(func() []byte { return data }, http.StatusPartialContent, &calls)
	defer origin.Close()

	s, _ := newHR31TestServer(t, rangecache.ModeDisk, origin.Client())
	s.httpGov.SetCapacity(HTTPSourceGovOp, 4)

	item := &store.Item{ID: "item-hr35b"}
	pf := httpstream.PersistedFile{FileID: "hf000000000003", Selector: "movie", Name: "movie.mp4", Size: total, ContentType: "video/mp4", RangeVerified: true}
	handler := &ladderTestHandler{file: httpstream.ResolvedFile{Selector: "movie", Name: "movie.mp4", Size: total, URL: origin.URL, SupportsRange: true}}
	key := httpstream.ResolveKey{Version: httpstream.ResolveKeyVersion, BackendID: "test", Handler: "test", Kind: "movie", IDs: map[string]string{"test": "one"}}
	initial := handler.file

	aStart, aEnd := int64(0), blockSize-1
	bStart, bEnd := blockSize*3, total-1

	var wg sync.WaitGroup
	var recA, recB *httptest.ResponseRecorder
	wg.Add(2)
	go func() {
		defer wg.Done()
		recA = hr35RequestRange(t, s, item, pf, initial, handler, key, aStart, aEnd)
	}()
	go func() {
		defer wg.Done()
		recB = hr35RequestRange(t, s, item, pf, initial, handler, key, bStart, bEnd)
	}()
	wg.Wait()

	if !bytes.Equal(recA.Body.Bytes(), data[aStart:aEnd+1]) {
		t.Fatalf("client A body mismatch, len=%d want=%d", recA.Body.Len(), aEnd-aStart+1)
	}
	if !bytes.Equal(recB.Body.Bytes(), data[bStart:bEnd+1]) {
		t.Fatalf("client B body mismatch, len=%d want=%d", recB.Body.Len(), bEnd-bStart+1)
	}
	if calls.Load() != 2 {
		t.Fatalf("total origin calls = %d, want exactly 2 (fully independent, non-overlapping blocks)", calls.Load())
	}
}
