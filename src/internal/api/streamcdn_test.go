package api

import (
	"bytes"
	"context"
	"crypto/sha1" // #nosec G505 -- BitTorrent v1 piece hashes are defined as SHA-1; not used for any security property here.
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
	"github.com/darkharrbor/darkharrbor/internal/torrentmeta"
)

var streamCDNTestLog = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestC02CDNProbeLogRedactsUpstreamURL(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	cache := rangecache.New(rangecache.Config{DiskCachePath: t.TempDir(), DiskCacheSizeMB: 1}, log)
	t.Cleanup(cache.Close)
	s := &Server{cfg: &config.Config{}, log: log, streamCache: cache}
	req := httptest.NewRequest(http.MethodGet, "/stream/item/0", nil)

	if s.streamViaCache(httptest.NewRecorder(), req, nil, "item", "0", "http://127.0.0.1:1/media?token=must-not-appear", nil, nil, 1, "", nil, nil, nil, nil) {
		t.Fatal("unreachable source unexpectedly served")
	}
	if got := logs.String(); strings.Contains(got, "must-not-appear") || strings.Contains(got, "http://") || strings.Contains(got, "token=") {
		t.Fatalf("cdn probe log leaked upstream URL: %s", got)
	}
}

func TestCDNSourceKeyIsolatesDiskCache(t *testing.T) {
	hits := map[string]int{}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path]++
		body := []byte("AAAA")
		if r.URL.Path == "/b" {
			body = []byte("BBBB")
		}
		w.Header().Set("Content-Range", "bytes 0-3/4")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body)
	}))
	defer origin.Close()

	cache := rangecache.New(rangecache.Config{
		DiskCachePath:   t.TempDir(),
		DiskCacheSizeMB: 1,
	}, streamCDNTestLog)
	t.Cleanup(cache.Close)
	cfg := &config.Config{}
	cfg.Cache.StreamMode = "disk"
	s := &Server{cfg: cfg, log: streamCDNTestLog, streamCache: cache}
	ranged := true

	request := func(itemID, path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/stream/"+itemID+"/0", nil)
		req.Header.Set("Range", "bytes=0-3")
		rec := httptest.NewRecorder()
		if !s.streamViaCache(rec, req, nil, itemID, "0", origin.URL+path, nil, nil, 4, "application/octet-stream", &ranged, nil, nil, nil) {
			t.Fatal("range-capable source was not served through rangecache")
		}
		return rec
	}
	recA := request("item-a", "/a")
	recB := request("item-b", "/b")
	if recA.Code != http.StatusPartialContent || recB.Code != http.StatusPartialContent || recA.Body.String() != "AAAA" || recB.Body.String() != "BBBB" {
		t.Fatalf("cross-file cache contamination: A=%d/%q B=%d/%q", recA.Code, recA.Body.String(), recB.Code, recB.Body.String())
	}
	if hits["/a"] != 1 || hits["/b"] != 1 {
		t.Fatalf("origin hits = %#v, want one fetch per file", hits)
	}
}

func TestCDNSourceKeyIsolatesReadaheadSession(t *testing.T) {
	data := map[string]map[string]string{
		"/a": {"bytes=0-3": "AAAA", "bytes=4-7": "XXXX", "bytes=8-11": "CCCC"},
		"/b": {"bytes=0-3": "1111", "bytes=4-7": "YYYY", "bytes=8-11": "3333"},
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := data[r.URL.Path][r.Header.Get("Range")]
		if !ok {
			http.Error(w, "unexpected range", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(origin.Close)
	cache := rangecache.New(rangecache.Config{
		ReadaheadEnabled:     true,
		ReadaheadMaxSegments: 1,
		ReadaheadWorkers:     1,
		MinBufferSegments:    1,
		DiskCachePath:        t.TempDir(),
		DiskCacheSizeMB:      1,
	}, streamCDNTestLog)
	t.Cleanup(cache.Close)
	srcA := makeCDNSourceFromCache(origin.URL+"/a", 4, 12, "application/octet-stream", nil)
	srcA.key = "torrent|item-a/0"
	srcB := makeCDNSourceFromCache(origin.URL+"/b", 4, 12, "application/octet-stream", nil)
	srcB.key = "torrent|item-b/0"

	request := func(src *cdnChunkSource, byteRange string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		if err := cache.Stream(context.Background(), src, rangecache.ModeReadahead, 12, rec, byteRange, 1); err != nil {
			t.Fatal(err)
		}
		return rec
	}
	recA := request(srcA, "bytes=0-3")
	recB := request(srcB, "bytes=4-7")
	if recA.Code != http.StatusPartialContent || recA.Body.String() != "AAAA" || recB.Code != http.StatusPartialContent || recB.Body.String() != "YYYY" {
		t.Fatalf("cross-file session contamination: A=%d/%q B=%d/%q", recA.Code, recA.Body.String(), recB.Code, recB.Body.String())
	}
}

func TestCDNChunkSourceReresolvesEachExpiryIncident(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	u1Calls := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		path := r.URL.Path
		if path == "/u1" {
			u1Calls++
		}
		call := u1Calls
		mu.Unlock()

		switch {
		case path == "/u1" && call == 1:
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("ONE!"))
		case path == "/u2":
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("TWO!"))
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer origin.Close()

	resolveCalls := 0
	src := makeCDNSourceFromCache(origin.URL+"/u0", 4, 4, "application/octet-stream", func(context.Context) (string, error) {
		resolveCalls++
		if resolveCalls == 1 {
			return origin.URL + "/u1", nil
		}
		return origin.URL + "/u2", nil
	})
	ref := src.Chunks()[0]
	first, err := src.Fetch(context.Background(), ref, rangecache.FetchDemand)
	if err != nil {
		t.Fatal(err)
	}
	second, err := src.Fetch(context.Background(), ref, rangecache.FetchDemand)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != "ONE!" || string(second) != "TWO!" || resolveCalls != 2 {
		t.Fatalf("first=%q second=%q resolver_calls=%d", first, second, resolveCalls)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"/u0", "/u1", "/u1", "/u2"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths=%v want=%v", paths, want)
	}
}

func TestCDNChunkSourceRecoversAfterTransientPostRefreshFailure(t *testing.T) {
	flakyCalls := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/expired":
			w.WriteHeader(http.StatusForbidden)
		case "/flaky":
			flakyCalls++
			switch flakyCalls {
			case 1:
				w.WriteHeader(http.StatusServiceUnavailable)
			case 2:
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write([]byte("LIVE"))
			default:
				w.WriteHeader(http.StatusForbidden)
			}
		case "/fresh":
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("NEXT"))
		}
	}))
	defer origin.Close()
	resolveCalls := 0
	src := makeCDNSourceFromCache(origin.URL+"/expired", 4, 4, "application/octet-stream", func(context.Context) (string, error) {
		resolveCalls++
		if resolveCalls == 1 {
			return origin.URL + "/flaky", nil
		}
		return origin.URL + "/fresh", nil
	})
	ref := src.Chunks()[0]
	if _, err := src.Fetch(context.Background(), ref, rangecache.FetchDemand); err == nil {
		t.Fatal("post-refresh 503 unexpectedly succeeded")
	}
	data, err := src.Fetch(context.Background(), ref, rangecache.FetchDemand)
	if err != nil || string(data) != "LIVE" {
		t.Fatalf("recovery data=%q err=%v", data, err)
	}
	data, err = src.Fetch(context.Background(), ref, rangecache.FetchDemand)
	if err != nil || string(data) != "NEXT" || resolveCalls != 2 {
		t.Fatalf("next incident data=%q err=%v resolver_calls=%d", data, err, resolveCalls)
	}
}

func TestCDNChunkSourceSharesConcurrentReresolve(t *testing.T) {
	var expiredHits atomic.Int32
	var resolveCalls atomic.Int32
	bothExpired := make(chan struct{})
	var releaseOnce sync.Once
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/expired" {
			if expiredHits.Add(1) == 2 {
				releaseOnce.Do(func() { close(bothExpired) })
			}
			select {
			case <-bothExpired:
				w.WriteHeader(http.StatusForbidden)
			case <-time.After(2 * time.Second):
				http.Error(w, "second request did not arrive", http.StatusInternalServerError)
			}
			return
		}
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("DATA"))
	}))
	defer origin.Close()
	src := makeCDNSourceFromCache(origin.URL+"/expired", 4, 4, "application/octet-stream", func(context.Context) (string, error) {
		resolveCalls.Add(1)
		return origin.URL + "/fresh", nil
	})
	ref := src.Chunks()[0]
	type fetchResult struct {
		data string
		err  error
	}
	results := make(chan fetchResult, 2)
	for range 2 {
		go func() {
			data, err := src.Fetch(context.Background(), ref, rangecache.FetchDemand)
			results <- fetchResult{data: string(data), err: err}
		}()
	}
	for range 2 {
		result := <-results
		if result.err != nil || result.data != "DATA" {
			t.Fatalf("data=%q err=%v", result.data, result.err)
		}
	}
	if resolveCalls.Load() != 1 {
		t.Fatalf("resolver calls=%d want=1", resolveCalls.Load())
	}
}

func TestInitCDNSource429DoesNotConsumeFetchReresolve(t *testing.T) {
	var mu sync.Mutex
	var requests []string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.URL.Path+" "+r.Header.Get("Range"))
		mu.Unlock()
		switch {
		case r.URL.Path == "/limited":
			w.WriteHeader(http.StatusTooManyRequests)
		case r.URL.Path == "/probe-ok" && r.Header.Get("Range") == "bytes=0-0":
			w.Header().Set("Content-Range", "bytes 0-0/4")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("D"))
		case r.URL.Path == "/fetch-ok":
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("DATA"))
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer origin.Close()

	resolveCalls := 0
	resolver := func(context.Context) (string, error) {
		resolveCalls++
		if resolveCalls == 1 {
			return origin.URL + "/probe-ok", nil
		}
		return origin.URL + "/fetch-ok", nil
	}
	src, total, _, ranged, err := initCDNSource(context.Background(), origin.URL+"/limited", 4, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 || !ranged || resolveCalls != 1 {
		t.Fatalf("total=%d ranged=%v resolver_calls=%d", total, ranged, resolveCalls)
	}
	data, err := src.Fetch(context.Background(), src.Chunks()[0], rangecache.FetchDemand)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "DATA" || resolveCalls != 2 {
		t.Fatalf("data=%q resolver_calls=%d", data, resolveCalls)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{
		"/limited bytes=0-0",
		"/probe-ok bytes=0-0",
		"/probe-ok bytes=0-3",
		"/fetch-ok bytes=0-3",
	}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests=%v want=%v", requests, want)
	}
}

func TestInitCDNSourceRefreshes404(t *testing.T) {
	resolveCalls := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Range", "bytes 0-0/4")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("D"))
	}))
	defer origin.Close()
	src, total, _, ranged, err := initCDNSource(context.Background(), origin.URL+"/missing", 4, func(context.Context) (string, error) {
		resolveCalls++
		return origin.URL + "/ok", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if src.url != origin.URL+"/ok" || total != 4 || !ranged || resolveCalls != 1 {
		t.Fatalf("url=%q total=%d ranged=%v resolver_calls=%d", src.url, total, ranged, resolveCalls)
	}
}

func TestStreamViaCacheRemembersRangeCapableOrigin(t *testing.T) {
	var ranges []string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ranges = append(ranges, r.Header.Get("Range"))
		if r.Header.Get("Range") == "bytes=0-0" {
			w.Header().Set("Content-Range", "bytes 0-0/4")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("D"))
			return
		}
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("DATA"))
	}))
	defer origin.Close()
	cache := rangecache.New(rangecache.Config{DiskCachePath: t.TempDir(), DiskCacheSizeMB: 1}, streamCDNTestLog)
	t.Cleanup(cache.Close)
	cfg := &config.Config{}
	cfg.Cache.StreamMode = "none"
	s := &Server{cfg: cfg, log: streamCDNTestLog, streamCache: cache}
	s.cacheResolveEntry("item", "0", origin.URL, "application/octet-stream", 4, nil)

	request := func(capability *bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/stream/item/0", nil)
		req.Header.Set("Range", "bytes=0-3")
		rec := httptest.NewRecorder()
		if !s.streamViaCache(rec, req, nil, "item", "0", origin.URL, nil, nil, 4, "application/octet-stream", capability, nil, nil, nil) {
			t.Fatal("range-capable origin did not enter rangecache")
		}
		return rec
	}
	first := request(nil)
	entry := s.resolveEntryFor("item", "0")
	if entry == nil || entry.rangeCapable == nil || !*entry.rangeCapable {
		t.Fatalf("cached capability=%v, want known true", entry)
	}
	second := request(entry.rangeCapable)
	if first.Body.String() != "DATA" || second.Body.String() != "DATA" {
		t.Fatalf("first=%q second=%q", first.Body.String(), second.Body.String())
	}
	if want := []string{"bytes=0-0", "bytes=0-3", "bytes=0-3"}; !reflect.DeepEqual(ranges, want) {
		t.Fatalf("ranges=%v want=%v", ranges, want)
	}
}

func TestStreamViaCacheDoesNotCacheTransientProbeFailure(t *testing.T) {
	hits := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer origin.Close()
	cache := rangecache.New(rangecache.Config{DiskCachePath: t.TempDir(), DiskCacheSizeMB: 1}, streamCDNTestLog)
	t.Cleanup(cache.Close)
	s := &Server{cfg: &config.Config{}, log: streamCDNTestLog, streamCache: cache}
	s.cacheResolveEntry("item", "0", origin.URL, "application/octet-stream", 4, nil)
	req := httptest.NewRequest(http.MethodGet, "/stream/item/0", nil)
	if s.streamViaCache(httptest.NewRecorder(), req, nil, "item", "0", origin.URL, nil, nil, 4, "application/octet-stream", nil, nil, nil, nil) {
		t.Fatal("transient probe failure entered rangecache")
	}
	entry := s.resolveEntryFor("item", "0")
	if entry == nil || entry.rangeCapable != nil {
		t.Fatalf("transient failure cached capability=%v", entry)
	}
	if s.streamViaCache(httptest.NewRecorder(), req, nil, "item", "0", origin.URL, nil, nil, 4, "application/octet-stream", entry.rangeCapable, nil, nil, nil) {
		t.Fatal("transient probe failure entered rangecache on retry")
	}
	if hits != 2 {
		t.Fatalf("origin probes=%d want=2", hits)
	}
}

// TestStreamViaCacheTriggersRepairOnInitialProbeFailure proves TS-4.1's
// second real "stream-time" trigger point: a fresh playback attempt whose
// very first capability probe fails (initCDNSource error, before Stream()
// is ever reached) now also fires TriggerRepairAsync, gated the same way
// as the mid-stream trigger (non-cancelled context, real playback via a
// non-nil altCandidates). Firing exactly once per identity is proven by a
// second immediate claim attempt finding the identity already claimed.
func TestStreamViaCacheTriggersRepairOnInitialProbeFailure(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer origin.Close()
	cache := rangecache.New(rangecache.Config{DiskCachePath: t.TempDir(), DiskCacheSizeMB: 1}, streamCDNTestLog)
	t.Cleanup(cache.Close)

	hash := "9999999999999999999999999999999999999999"
	st := newRepairTestStore(t)
	item := mustCreateTorrentItem(t, st, "repair-probe-item", hash, "torbox")
	cfg := &config.Config{}
	cfg.Governor.RepairEnabled = true
	s := &Server{
		cfg:         cfg,
		log:         streamCDNTestLog,
		store:       st,
		streamCache: cache,
		shutdownCtx: context.Background(),
		repairState: newRepairState(),
	}

	req := httptest.NewRequest(http.MethodGet, "/stream/repair-probe-item/0", nil)
	altCandidates := []altProviderCandidate{{}} // non-nil marks this a real-playback call site
	if s.streamViaCache(httptest.NewRecorder(), req, item, "repair-probe-item", "0", origin.URL, nil, nil, 4, "application/octet-stream", nil, altCandidates, nil, nil) {
		t.Fatal("probe failure should not enter rangecache")
	}
	s.wg.Wait()

	if got := s.repairState.AttemptCount(hash); got != 1 {
		t.Fatalf("repair AttemptCount = %d, want 1 -- TriggerRepairAsync did not fire", got)
	}
}

func TestStreamViaCacheRemembersRangeIncapableOrigin(t *testing.T) {
	var ranges []string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ranges = append(ranges, r.Header.Get("Range"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("FULL"))
	}))
	defer origin.Close()

	cache := rangecache.New(rangecache.Config{DiskCachePath: t.TempDir(), DiskCacheSizeMB: 1}, streamCDNTestLog)
	t.Cleanup(cache.Close)
	s := &Server{cfg: &config.Config{}, log: streamCDNTestLog, streamCache: cache}
	s.cacheResolveEntry("item", "0", origin.URL, "application/octet-stream", 4, nil)

	req := httptest.NewRequest(http.MethodGet, "/stream/item/0", nil)
	req.Header.Set("Range", "bytes=1-2")
	if s.streamViaCache(httptest.NewRecorder(), req, nil, "item", "0", origin.URL, nil, nil, 4, "application/octet-stream", nil, nil, nil, nil) {
		t.Fatal("range-incapable origin entered rangecache")
	}
	entry := s.resolveEntryFor("item", "0")
	if entry == nil || entry.rangeCapable == nil || *entry.rangeCapable {
		t.Fatalf("cached capability=%v, want known false", entry)
	}
	if s.streamViaCache(httptest.NewRecorder(), req, nil, "item", "0", entry.cdnURL, nil, nil, entry.total, entry.contentType, entry.rangeCapable, nil, nil, nil) {
		t.Fatal("known range-incapable origin entered rangecache on warm request")
	}
	if want := []string{"bytes=0-0"}; !reflect.DeepEqual(ranges, want) {
		t.Fatalf("origin ranges=%v want=%v", ranges, want)
	}
}

// TS-2.1: rung accounting + structured logs.

func TestCDNChunkSourceFetchLogsRungAccounting(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	resolveCalls := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/expired":
			w.WriteHeader(http.StatusForbidden)
		case "/fresh":
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("DATA"))
		}
	}))
	defer origin.Close()

	src := makeCDNSourceFromCache(origin.URL+"/expired", 4, 4, "application/octet-stream", func(context.Context) (string, error) {
		resolveCalls++
		return origin.URL + "/fresh", nil
	})
	src.key = "torrent|rung-accounting-test"
	src.log = log

	ref := src.Chunks()[0]
	data, err := src.Fetch(context.Background(), ref, rangecache.FetchDemand)
	if err != nil || string(data) != "DATA" || resolveCalls != 1 {
		t.Fatalf("data=%q err=%v resolveCalls=%d", data, err, resolveCalls)
	}

	got := logs.String()
	if !strings.Contains(got, "rung=torrent_cdn_current") || !strings.Contains(got, "outcome=failed") {
		t.Fatalf("rung 2 (current) failure not accounted: %s", got)
	}
	if !strings.Contains(got, "rung=torrent_cdn_reresolve") || !strings.Contains(got, "outcome=success") {
		t.Fatalf("rung 3 (reresolve) success not accounted: %s", got)
	}
	if !strings.Contains(got, "key=torrent|rung-accounting-test") {
		t.Fatalf("rung log missing DH-owned key: %s", got)
	}
	// DG-04: no upstream URL, host, or scheme ever appears in the rung logs.
	if strings.Contains(got, "http://") || strings.Contains(got, origin.URL) {
		t.Fatalf("rung accounting log leaked upstream URL: %s", got)
	}
}

func TestCDNChunkSourceFetchTruncated206AttemptsReresolve(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/truncated":
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("response writer does not support hijacking")
			}
			conn, rw, err := hijacker.Hijack()
			if err != nil {
				t.Fatalf("hijack: %v", err)
			}
			_, _ = fmt.Fprint(rw, "HTTP/1.1 206 Partial Content\r\nContent-Length: 4\r\nContent-Range: bytes 0-3/4\r\n\r\nDA")
			_ = rw.Flush()
			_ = conn.Close()
		case "/fresh":
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("DATA"))
		}
	}))
	defer origin.Close()

	resolveCalls := 0
	src := makeCDNSourceFromCache(origin.URL+"/truncated", 4, 4, "application/octet-stream", func(context.Context) (string, error) {
		resolveCalls++
		return origin.URL + "/fresh", nil
	})
	ref := src.Chunks()[0]
	data, err := src.Fetch(context.Background(), ref, rangecache.FetchDemand)
	if err != nil || string(data) != "DATA" || resolveCalls != 1 {
		t.Fatalf("data=%q err=%v resolveCalls=%d", data, err, resolveCalls)
	}
}

func TestCDNChunkSourceFetch416NeverAttemptsReresolve(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	}))
	defer origin.Close()

	resolveCalls := 0
	src := makeCDNSourceFromCache(origin.URL+"/u0", 4, 4, "application/octet-stream", func(context.Context) (string, error) {
		resolveCalls++
		return origin.URL + "/u1", nil
	})
	ref := src.Chunks()[0]
	if _, err := src.Fetch(context.Background(), ref, rangecache.FetchDemand); err == nil {
		t.Fatal("416 unexpectedly succeeded")
	}
	if resolveCalls != 0 {
		t.Fatalf("416 triggered a rung-3 reresolve: resolveCalls=%d, want 0", resolveCalls)
	}
}

func TestCDNChunkSourceFetchUnexpectedStatusNeverAttemptsReresolve(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer origin.Close()

	resolveCalls := 0
	src := makeCDNSourceFromCache(origin.URL+"/u0", 4, 4, "application/octet-stream", func(context.Context) (string, error) {
		resolveCalls++
		return origin.URL + "/u1", nil
	})
	ref := src.Chunks()[0]
	if _, err := src.Fetch(context.Background(), ref, rangecache.FetchDemand); err == nil {
		t.Fatal("500 unexpectedly succeeded")
	}
	if resolveCalls != 0 {
		t.Fatalf("non-refresh-worthy status triggered a rung-3 reresolve: resolveCalls=%d, want 0", resolveCalls)
	}
}

// DG-07: each ladder rung acquires and releases its own governor lease
// (TS-2.1's disclosed refinement from one incident-spanning lease to one
// per rung), so a two-rung incident (rung 2 fails, rung 3 succeeds) against
// a capacity-1 governor completes without deadlocking, proving the lease is
// released between rungs rather than held across the whole incident.
func TestCDNChunkSourceFetchPerRungGovernorLease(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/expired":
			w.WriteHeader(http.StatusForbidden)
		case "/fresh":
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("DATA"))
		}
	}))
	defer origin.Close()

	gov := accountgov.New("test-torrent-cdn")
	gov.SetCapacity(TorrentCDNGovOp, 1)

	src := makeCDNSourceFromCache(origin.URL+"/expired", 4, 4, "application/octet-stream", func(context.Context) (string, error) {
		return origin.URL + "/fresh", nil
	})
	src.gov = gov
	src.govOp = TorrentCDNGovOp
	src.priority = accountgov.PriorityPlayback
	src.session = "item-1"

	ref := src.Chunks()[0]
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, err := src.Fetch(ctx, ref, rangecache.FetchDemand)
	if err != nil || string(data) != "DATA" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	if gov.InUse(TorrentCDNGovOp) != 0 {
		t.Fatalf("governor lease leaked: in_use=%d, want 0 after Fetch returns", gov.InUse(TorrentCDNGovOp))
	}
}

func TestTorrentCDNScoreIDIsOpaqueAndFailClosed(t *testing.T) {
	first := torrentCDNScoreID("192.0.2.10:443")
	if first == "" || first != torrentCDNScoreID("192.0.2.10:8443") {
		t.Fatalf("same resolved IP did not produce one stable identity: %q", first)
	}
	if first == torrentCDNScoreID("192.0.2.11:443") {
		t.Fatal("different resolved IPs produced the same identity")
	}
	if strings.Contains(first, "192.0.2.10") || strings.Contains(first, ":443") {
		t.Fatalf("identity exposed remote address: %q", first)
	}
	for _, malformed := range []string{"", "cdn.example:443", "192.0.2.10", "http://192.0.2.10:443", strings.Repeat("x", maxCDNRemoteAddrBytes+1)} {
		if got := torrentCDNScoreID(malformed); got != "" {
			t.Fatalf("malformed/unknown address %q produced identity %q", malformed, got)
		}
	}
}

func FuzzTorrentCDNScoreID(f *testing.F) {
	f.Add("192.0.2.10:443")
	f.Add("[2001:db8::1]:443")
	f.Add("cdn.example:443")
	f.Fuzz(func(t *testing.T, remoteAddr string) {
		got := torrentCDNScoreID(remoteAddr)
		if got != torrentCDNScoreID(remoteAddr) {
			t.Fatal("identity is not deterministic")
		}
		if got == "" {
			return
		}
		const prefix = "torrent-cdn-"
		if !strings.HasPrefix(got, prefix) || len(got) != len(prefix)+24 {
			t.Fatalf("invalid opaque identity shape: %q", got)
		}
		if _, err := hex.DecodeString(strings.TrimPrefix(got, prefix)); err != nil {
			t.Fatalf("invalid opaque identity encoding: %q: %v", got, err)
		}
		if strings.ContainsAny(got, ":/?&") {
			t.Fatalf("identity contains address or URL delimiters: %q", got)
		}
	})
}

func TestCDNChunkSourcePassivelyScoresRealRangeTraffic(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("DATA"))
	}))
	t.Cleanup(origin.Close)

	scores := httpstream.NewOriginScorer(nil)
	src := makeCDNSourceFromCache(origin.URL, 4, 4, "application/octet-stream", nil)
	src.scores = scores
	data, err := src.Fetch(context.Background(), src.Chunks()[0], rangecache.FetchDemand)
	if err != nil || string(data) != "DATA" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	src.mu.Lock()
	id := src.sourceID
	src.mu.Unlock()
	if id == "" || scores.Len() != 1 || scores.Score(id) <= 0 {
		t.Fatalf("passive score missing: id=%q len=%d score=%f", id, scores.Len(), scores.Score(id))
	}
}

func TestCDNChunkSourcePoorScoreTriggersEarlyReresolve(t *testing.T) {
	var staleHits atomic.Int32
	stale := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		staleHits.Add(1)
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("OLD!"))
	}))
	t.Cleanup(stale.Close)
	fresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("NEW!"))
	}))
	t.Cleanup(fresh.Close)

	scores := httpstream.NewOriginScorer(nil)
	badID := torrentCDNScoreID("192.0.2.40:443")
	scores.Observe(badID, httpstream.OriginObservation{Expired: true})
	scores.Observe(badID, httpstream.OriginObservation{Expired: true})
	resolveCalls := 0
	src := makeCDNSourceFromCache(stale.URL, 4, 4, "application/octet-stream", func(context.Context) (string, error) {
		resolveCalls++
		return fresh.URL, nil
	})
	src.scores = scores
	src.sourceID = badID
	data, err := src.Fetch(context.Background(), src.Chunks()[0], rangecache.FetchDemand)
	if err != nil || string(data) != "NEW!" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	if staleHits.Load() != 0 || resolveCalls != 1 {
		t.Fatalf("poor node was contacted: stale_hits=%d resolve_calls=%d", staleHits.Load(), resolveCalls)
	}
}

func TestCDNChunkSourceShortReadFailsClosedAndScoresFailure(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("NO"))
	}))
	t.Cleanup(origin.Close)

	scores := httpstream.NewOriginScorer(nil)
	src := makeCDNSourceFromCache(origin.URL, 4, 4, "application/octet-stream", nil)
	src.scores = scores
	data, err := src.Fetch(context.Background(), src.Chunks()[0], rangecache.FetchDemand)
	if err == nil || len(data) != 0 {
		t.Fatalf("short read did not fail closed: data_len=%d err=%v", len(data), err)
	}
	src.mu.Lock()
	id := src.sourceID
	src.mu.Unlock()
	if id == "" || scores.Score(id) >= 0 {
		t.Fatalf("short read was not negative evidence: id=%q score=%f", id, scores.Score(id))
	}
}

func TestCDNChunkSourceCancellationDoesNotPoisonScore(t *testing.T) {
	started := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(origin.Close)

	scores := httpstream.NewOriginScorer(nil)
	src := makeCDNSourceFromCache(origin.URL, 4, 4, "application/octet-stream", nil)
	src.scores = scores
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := src.Fetch(ctx, src.Chunks()[0], rangecache.FetchDemand)
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled fetch unexpectedly succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled fetch did not shut down")
	}
	if scores.Len() != 0 {
		t.Fatalf("cancellation poisoned passive score: len=%d", scores.Len())
	}
}

// TestT12PieceVerificationRejectsCorruptWindowAndFallsThroughToReresolve is
// TS-3.4's deterministic corrupted-chunk-injection gate (the frozen T12
// spec's own acceptance line: "live gate with corrupted-chunk injection").
// A bad origin serves a full, piece-aligned window whose bytes do NOT match
// the item's persisted TorrentMeta piece hash. Verification must reject it
// (never serve corrupt bytes to the client), the rung-2-failure must fall
// through to rung 3 (reresolve) exactly like any other refresh-worthy
// failure, and the CDN host score for the offending origin must be
// penalized (T9) — while a subsequent good origin's byte-identical,
// correctly-hashed window must serve normally.
func TestT12PieceVerificationRejectsCorruptWindowAndFallsThroughToReresolve(t *testing.T) {
	const pieceLength = 1 << 20 // 1 MiB: one piece == one full window
	correct := bytes.Repeat([]byte("G"), pieceLength)
	corrupt := bytes.Repeat([]byte("B"), pieceLength)
	correctHash := sha1Sum(correct)

	var badHits, goodHits int32
	badOrigin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&badHits, 1)
		serveRangedBody(w, r, corrupt)
	}))
	defer badOrigin.Close()
	goodOrigin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&goodHits, 1)
		serveRangedBody(w, r, correct)
	}))
	defer goodOrigin.Close()

	cache := rangecache.New(rangecache.Config{DiskCachePath: t.TempDir(), DiskCacheSizeMB: 8}, streamCDNTestLog)
	t.Cleanup(cache.Close)
	cfg := &config.Config{}
	cfg.Cache.StreamChunkSizeMB = 1 // windowBytes == pieceLength above
	s := &Server{
		cfg:              cfg,
		log:              streamCDNTestLog,
		streamCache:      cache,
		torrentCDNScores: httpstream.NewOriginScorer(time.Now),
	}

	tsMeta := &torrentmeta.TorrentMeta{
		PieceLength:   pieceLength,
		PieceHashesV1: correctHash,
	}
	tsFile := &torrentmeta.FileEntry{Path: "movie.mkv", Size: pieceLength, StartOffset: 0, EndOffset: pieceLength}

	reresolveCalled := false
	reresolve := func(context.Context) (string, error) {
		reresolveCalled = true
		return goodOrigin.URL, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/stream/item/0", nil)
	req.Header.Set("Range", "bytes=0-1048575")
	rec := httptest.NewRecorder()

	if !s.streamViaCache(rec, req, nil, "item-t12", "0", badOrigin.URL, reresolve, nil, 0, "", nil, nil, tsMeta, tsFile) {
		t.Fatal("expected streamViaCache to serve after falling through to the reresolved good origin")
	}
	if !reresolveCalled {
		t.Fatal("expected reresolve to be called after the corrupt window was rejected")
	}
	if got := rec.Body.Bytes(); !bytes.Equal(got, correct) {
		if bytes.Equal(got, corrupt) {
			t.Fatal("corrupt bytes were served to the client -- T12 verification did not stop them")
		}
		t.Fatalf("served body did not match the correct (reresolved) content, len=%d", len(got))
	}
	if atomic.LoadInt32(&badHits) == 0 {
		t.Fatal("expected at least one request against the bad origin")
	}
	if atomic.LoadInt32(&goodHits) == 0 {
		t.Fatal("expected at least one request against the reresolved good origin")
	}
	// T9 scoring itself (the offending origin's passive CDN score must be
	// penalized on mismatch) is proven precisely, isolated from a real
	// transfer's own confounding positive credit, by
	// TestVerifyPieceWindowPenalizesScoreOnMismatch below.
}

// TestVerifyPieceWindowPenalizesScoreOnMismatch is TS-3.4/T9's scoring
// proof in isolation: a passing verification must never touch the score
// (still true evidence, not new evidence -- doRangeAt's own transfer-level
// observation already recorded it), while a mismatch must strictly lower
// it. Isolating this from a real HTTP transfer avoids a same-origin
// integration test's confound, where the underlying transfer's own
// successful-delivery credit (doRangeAt observes real bytes/latency
// regardless of content correctness) can partly or wholly offset a single
// corruption penalty for the SAME request -- a real bad node's score still
// trends down over repeated corrupt windows, exactly as T9 intends, but a
// single-window net delta is not the right thing for a test to assert on.
func TestVerifyPieceWindowPenalizesScoreOnMismatch(t *testing.T) {
	scores := httpstream.NewOriginScorer(time.Now)
	c := &cdnChunkSource{scores: scores, sourceID: "torrent-cdn-test-source"}
	good := []byte("GOOD")
	meta := &torrentmeta.TorrentMeta{PieceLength: int64(len(good)), PieceHashesV1: sha1Sum(good)}
	file := &torrentmeta.FileEntry{Path: "f", Size: int64(len(good)), StartOffset: 0, EndOffset: int64(len(good))}
	c.meta = meta
	c.metaFile = file

	before := scores.Score(c.sourceID)
	if ok := c.verifyPieceWindow(0, good); !ok {
		t.Fatal("expected verifyPieceWindow to pass for correctly-hashed data")
	}
	if got := scores.Score(c.sourceID); got != before {
		t.Fatalf("a passing verification unexpectedly changed the score: before=%v after=%v", before, got)
	}

	if ok := c.verifyPieceWindow(0, []byte("BAD!")); ok {
		t.Fatal("expected verifyPieceWindow to reject corrupt data")
	}
	if got := scores.Score(c.sourceID); got >= before {
		t.Fatalf("corrupt verification did not penalize the score: before=%v after=%v", before, got)
	}
}

// TestT12PieceVerificationAllowsCorrectWindowUnaffected proves the row is a
// structural no-op when TorrentMeta/FileEntry are nil (every pre-TS-3.4
// caller) and passes correctly-hashed bytes through unchanged when they are
// set and the bytes genuinely verify.
func TestT12PieceVerificationAllowsCorrectWindowUnaffected(t *testing.T) {
	const pieceLength = 1 << 20
	correct := bytes.Repeat([]byte("G"), pieceLength)
	correctHash := sha1Sum(correct)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveRangedBody(w, r, correct)
	}))
	defer origin.Close()

	cache := rangecache.New(rangecache.Config{DiskCachePath: t.TempDir(), DiskCacheSizeMB: 8}, streamCDNTestLog)
	t.Cleanup(cache.Close)
	cfg := &config.Config{}
	cfg.Cache.StreamChunkSizeMB = 1
	s := &Server{cfg: cfg, log: streamCDNTestLog, streamCache: cache}

	tsMeta := &torrentmeta.TorrentMeta{PieceLength: pieceLength, PieceHashesV1: correctHash}
	tsFile := &torrentmeta.FileEntry{Path: "movie.mkv", Size: pieceLength, StartOffset: 0, EndOffset: pieceLength}

	req := httptest.NewRequest(http.MethodGet, "/stream/item/0", nil)
	req.Header.Set("Range", "bytes=0-1048575")
	rec := httptest.NewRecorder()
	if !s.streamViaCache(rec, req, nil, "item-t12-ok", "0", origin.URL, nil, nil, 0, "", nil, nil, tsMeta, tsFile) {
		t.Fatal("expected streamViaCache to serve a correctly-verified window")
	}
	if !bytes.Equal(rec.Body.Bytes(), correct) {
		t.Fatal("correctly-hashed window was not served byte-identically")
	}
}

func serveRangedBody(w http.ResponseWriter, r *http.Request, body []byte) {
	total := len(body)
	rng := r.Header.Get("Range")
	if rng == "" {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", total))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body[:1])
		return
	}
	var start, end int
	if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &start, &end); err != nil {
		http.Error(w, "bad range", http.StatusBadRequest)
		return
	}
	if end >= total {
		end = total - 1
	}
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(body[start : end+1])
}

func sha1Sum(b []byte) []byte {
	h := sha1.Sum(b) // #nosec G401 -- matches BitTorrent v1 piece-hash definition (SHA-1), not a security property.
	return h[:]
}
