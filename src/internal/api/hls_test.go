package api

// HR4.2 deterministic gates: DG-03 (malformed/oversized negatives), DG-04
// (no upstream URL ever reaches a durable/client surface), DG-05
// (deterministic mutation/expiry-shaped behavior), and the CG-09 foundation
// smoke shape (real store + real hlssession.Graph + a controlled fixture
// HTTP origin, restart persistence, no downstream player/lane assumed).

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/hls"
	"github.com/darkharrbor/darkharrbor/internal/hlssession"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
	"github.com/darkharrbor/darkharrbor/internal/sidecar"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// hlsTestHandler is a minimal httpstream.Handler stub whose Resolve always
// returns one fixed ResolvedFile pointing at a controlled fixture origin —
// the same shape as prewarmTestHandler in httpstream_prewarm_test.go.
type hlsTestHandler struct {
	url      string
	selector string
}

func TestHLSContentSteeringConsumesRanksAndRedacts(t *testing.T) {
	var steeringStatus atomic.Int64
	steeringStatus.Store(http.StatusOK)
	segment := []byte("same-real-media-bytes")
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/master.m3u8":
			_, _ = io.WriteString(w, "#EXTM3U\n"+
				"#EXT-X-CONTENT-STEERING:SERVER-URI=\"/steering.json?private=one\",PATHWAY-ID=\"A\"\n"+
				"#EXT-X-STREAM-INF:BANDWIDTH=1,PATHWAY-ID=\"A\",STABLE-VARIANT-ID=\"v1\"\n/a/index.m3u8?private=two\n"+
				"#EXT-X-STREAM-INF:BANDWIDTH=1,PATHWAY-ID=\"B\",STABLE-VARIANT-ID=\"v1\"\n/b/index.m3u8?private=three\n")
		case "/steering.json":
			switch int(steeringStatus.Load()) {
			case http.StatusGone:
				w.WriteHeader(http.StatusGone)
			case http.StatusTooManyRequests:
				w.Header().Set("Retry-After", "999999")
				w.WriteHeader(http.StatusTooManyRequests)
			case http.StatusNotFound:
				w.WriteHeader(http.StatusNotFound)
			default:
				_, _ = io.WriteString(w, `{"VERSION":1,"TTL":300,"RELOAD-URI":"https://invalid.example/reload?private=four","PATHWAY-PRIORITY":["A","B","CLONE"],"PATHWAY-CLONES":[{"ID":"CLONE","BASE-ID":"A","URI-REPLACEMENT":{"HOST":"invalid.example"}}]}`)
			}
		case "/a/index.m3u8", "/b/index.m3u8":
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nseg.ts?private=five\n#EXT-X-ENDLIST\n")
		case "/a/seg.ts", "/b/seg.ts":
			_, _ = w.Write(segment)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(origin.Close)

	s, _ := newHLSTestServer(t)
	s.httpClientOnce.Do(func() {
		s.httpProbeClient = origin.Client()
		s.httpRelayClient = origin.Client()
	})
	s.httpOriginScores = httpstream.NewOriginScorer(s.nowUTC)
	item := createHLSTestItem(t, s, "f1", "master", origin.URL+"/master.m3u8")
	playURL, err := s.CreateHLSPlaybackSession(context.Background(), item.ID, "f1")
	if err != nil {
		t.Fatalf("CreateHLSPlaybackSession: %v", err)
	}
	rootID, _, _ := strings.Cut(strings.TrimPrefix(playURL, "/hls/"), ".")
	root, err := s.hlsSessions.Resolve(context.Background(), rootID)
	if err != nil {
		t.Fatal(err)
	}
	mux := hlsMux(s)
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		mux.ServeHTTP(rec, req)
		return rec
	}

	master := get(playURL)
	if master.Code != http.StatusOK {
		t.Fatalf("master status=%d", master.Code)
	}
	for _, forbidden := range []string{"private=", origin.URL, "invalid.example"} {
		if strings.Contains(master.Body.String(), forbidden) {
			t.Fatal("master exposed forbidden upstream material")
		}
	}
	tag, found, err := hls.ParseContentSteering(master.Body.String())
	if err != nil || !found || !strings.HasSuffix(tag.ServerURI, ".steering.json") {
		t.Fatal("localized steering tag invalid")
	}
	variants, err := hls.ParseMaster(master.Body.String())
	if err != nil || len(variants) != 2 {
		t.Fatal("localized variants invalid")
	}

	neutral := get(tag.ServerURI)
	if neutral.Code != http.StatusOK ||
		neutral.Body.String() != "{\"VERSION\":1,\"TTL\":300,\"PATHWAY-PRIORITY\":[\"A\",\"B\"]}\n" {
		t.Fatal("neutral steering response invalid")
	}
	for _, forbidden := range []string{"private=", "RELOAD-URI", "PATHWAY-CLONES", "CLONE", "invalid.example"} {
		if strings.Contains(neutral.Body.String(), forbidden) {
			t.Fatal("steering response exposed forbidden upstream material")
		}
	}

	var pathwayB string
	for _, variant := range variants {
		if variant.PathwayID == "B" {
			pathwayB = variant.URI
		}
	}
	media := get(pathwayB)
	mediaEntries, err := hls.ParseMedia(media.Body.String())
	if media.Code != http.StatusOK || err != nil || len(mediaEntries) != 1 {
		t.Fatal("pathway B media response invalid")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	canceledReq := httptest.NewRequest(http.MethodGet, mediaEntries[0].URI, nil).WithContext(canceled)
	mux.ServeHTTP(httptest.NewRecorder(), canceledReq)
	if s.httpOriginScores.Len() != 0 {
		t.Fatal("canceled pathway request changed passive score state")
	}
	leaf := get(mediaEntries[0].URI)
	if leaf.Code != http.StatusOK || !bytes.Equal(leaf.Body.Bytes(), segment) {
		t.Fatal("pathway B leaf response invalid")
	}
	if s.httpOriginScores.Score(hlsPathwayScoreID(root.SessionID, "B")) <=
		s.httpOriginScores.Score(hlsPathwayScoreID(root.SessionID, "A")) {
		t.Fatal("passive pathway observation did not affect score")
	}
	ranked := get(tag.ServerURI)
	if ranked.Body.String() != "{\"VERSION\":1,\"TTL\":300,\"PATHWAY-PRIORITY\":[\"B\",\"A\"]}\n" {
		t.Fatal("measured steering response invalid")
	}

	resources, err := s.hlsSessions.ListResources(context.Background(), root.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	foundPersistedB := false
	for _, resource := range resources {
		if resource.Reference.PathwayID == "B" {
			foundPersistedB = true
		}
	}
	if !foundPersistedB {
		t.Fatal("pathway coordinate was not persisted on existing session resources")
	}

	steeringStatus.Store(http.StatusTooManyRequests)
	throttled := get(tag.ServerURI)
	if throttled.Code != http.StatusTooManyRequests ||
		throttled.Header().Get("Retry-After") != strconv.Itoa(hls.MaxSteeringTTL) {
		t.Fatal("steering throttle semantics invalid")
	}
	steeringStatus.Store(http.StatusGone)
	if gone := get(tag.ServerURI); gone.Code != http.StatusGone {
		t.Fatalf("steering gone status=%d", gone.Code)
	}
	steeringStatus.Store(http.StatusNotFound)
	if missing := get(tag.ServerURI); missing.Code != http.StatusServiceUnavailable {
		t.Fatalf("steering missing status=%d", missing.Code)
	}

	steeringStatus.Store(http.StatusOK)
	before := len(resources)
	var wg sync.WaitGroup
	results := make(chan string, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := get(tag.ServerURI)
			if rec.Code != http.StatusOK {
				results <- "status"
				return
			}
			results <- rec.Body.String()
		}()
	}
	wg.Wait()
	close(results)
	for got := range results {
		if got != ranked.Body.String() {
			t.Fatal("concurrent steering response changed")
		}
	}
	resources, err = s.hlsSessions.ListResources(context.Background(), root.SessionID)
	if err != nil || len(resources) != before {
		t.Fatal("steering reload changed bounded session graph")
	}
}

func (h *hlsTestHandler) Name() string { return "generic" }
func (h *hlsTestHandler) Search(context.Context, httpstream.StreamQuery) ([]httpstream.SearchResult, error) {
	return nil, nil
}
func (h *hlsTestHandler) Resolve(context.Context, httpstream.ResolveRequest) ([]httpstream.ResolvedFile, error) {
	return []httpstream.ResolvedFile{{Selector: h.selector, Name: "master.m3u8", URL: h.url}}, nil
}

type rotatingHLSHandler struct {
	selector       string
	staleURL       string
	freshURL       string
	forceBlock     chan struct{}
	forceStarted   chan struct{}
	forceStartOnce sync.Once
	normalCalls    atomic.Int64
	forcedCalls    atomic.Int64
	refreshContext chan string
}

func (h *rotatingHLSHandler) Name() string { return "generic" }
func (h *rotatingHLSHandler) Search(context.Context, httpstream.StreamQuery) ([]httpstream.SearchResult, error) {
	return nil, nil
}
func (h *rotatingHLSHandler) Resolve(ctx context.Context, req httpstream.ResolveRequest) ([]httpstream.ResolvedFile, error) {
	liveURL := h.staleURL
	nextContext := "opaque-stale-context"
	if req.Operation == httpstream.ResolveForceRefresh {
		h.forcedCalls.Add(1)
		liveURL = h.freshURL
		nextContext = "opaque-fresh-context"
		if h.refreshContext != nil {
			h.refreshContext <- req.RefreshContext
		}
		if h.forceStarted != nil {
			h.forceStartOnce.Do(func() { close(h.forceStarted) })
		}
		if h.forceBlock != nil {
			select {
			case <-h.forceBlock:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	} else {
		h.normalCalls.Add(1)
	}
	return []httpstream.ResolvedFile{{
		Selector:       h.selector,
		Name:           "master.m3u8",
		URL:            liveURL,
		RefreshContext: nextContext,
	}}, nil
}

// hlsFixtureOrigin serves a small, real master -> media -> (absent) segment
// HLS tree — a self-generated fixture origin, the same kind of controlled
// source CG-04/CG-09 explicitly authorize in place of a real backend.
func hlsFixtureOrigin() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"subs\",NAME=\"English\",URI=\"subs/index.m3u8\"\n#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"direct\",NAME=\"Direct\",URI=\"subs/direct.vtt\"\n#EXT-X-STREAM-INF:BANDWIDTH=1000000,RESOLUTION=640x360,SUBTITLES=\"subs\"\nlow/index.m3u8\n"))
	})
	mux.HandleFunc("/low/index.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6.0,\nseg0.ts\n#EXT-X-ENDLIST\n"))
	})
	mux.HandleFunc("/low/seg0.ts", func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "seg0.ts", time.Time{}, bytes.NewReader([]byte("segment-zero")))
	})
	mux.HandleFunc("/subs/index.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6.0,\ncue.vtt\n#EXT-X-ENDLIST\n"))
	})
	mux.HandleFunc("/subs/cue.vtt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/vtt")
		_, _ = w.Write([]byte("WEBVTT\n\n00:00.000 --> 00:01.000\nHello\n"))
	})
	mux.HandleFunc("/subs/direct.vtt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/vtt")
		_, _ = w.Write([]byte("WEBVTT\n\n00:01.000 --> 00:02.000\nPersisted\n"))
	})
	mux.HandleFunc("/oversized.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		big := make([]byte, 0)
		big = append(big, []byte("#EXT-X-STREAM-INF:BANDWIDTH=1\n")...)
		for int64(len(big)) <= 8<<20 {
			big = append(big, []byte(strings.Repeat("x", 4096)+"\n")...)
		}
		_, _ = w.Write(big)
	})
	mux.HandleFunc("/notaplaylist.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("this is not an HLS playlist at all"))
	})
	return httptest.NewServer(mux)
}

func expiringHLSOrigin(expired *atomic.Bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stale := strings.HasPrefix(r.URL.Path, "/stale/")
		if stale && expired.Load() {
			http.Error(w, "expired", http.StatusForbidden)
			return
		}
		switch strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/stale"), "/fresh") {
		case "/master.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"subs\",NAME=\"English\",URI=\"subs.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=1000000,SUBTITLES=\"subs\"\nmedia.m3u8\n")
		case "/media.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXT-X-DISCONTINUITY\n#EXTINF:6,\nseg.ts\n#EXT-X-ENDLIST\n")
		case "/subs.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXTINF:6,\ncue.vtt\n#EXT-X-ENDLIST\n")
		case "/key.bin":
			http.ServeContent(w, r, "key.bin", time.Time{}, bytes.NewReader([]byte("0123456789abcdef")))
		case "/init.mp4":
			http.ServeContent(w, r, "init.mp4", time.Time{}, bytes.NewReader([]byte("init-map")))
		case "/seg.ts":
			http.ServeContent(w, r, "seg.ts", time.Time{}, bytes.NewReader([]byte("equivalent-segment")))
		case "/cue.vtt":
			http.ServeContent(w, r, "cue.vtt", time.Time{}, bytes.NewReader([]byte("WEBVTT\n\n00:00.000 --> 00:01.000\nEquivalent\n")))
		default:
			http.NotFound(w, r)
		}
	}))
}

// newHLSTestServer wires a *Server against a real, migrated, on-disk SQLite
// store (so restart-persistence can be proven the same way HR4.1's own
// TestStoreHLSSessionSurvivesRestart proves it) and a real hlssession.Graph.
// Returns the server, the db path (for a real reopen-as-restart), and a
// teardown the caller must NOT call itself (t.Cleanup handles the first
// open; a restart test opens/closes its own second handle explicitly).
func newHLSTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hls-api-test.db")
	db, err := store.Open(context.Background(), path, 5*time.Second)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	st := store.New(db)

	graph, err := hlssession.New(st, hlssession.Options{})
	if err != nil {
		t.Fatalf("hlssession.New: %v", err)
	}

	s := httpTestServer("0123456789abcdef0123456789abcdef", true)
	s.store = st
	s.hlsSessions = graph
	s.sidecars, err = sidecar.New(st, sidecar.Options{})
	if err != nil {
		t.Fatalf("sidecar.New: %v", err)
	}
	s.httpHandlers = httpstream.NewRegistry()
	s.httpResolveCoord = newHTTPResolveCoordinator()
	s.playbackCoverage, err = playbackcoverage.New(st, playbackcoverage.Options{})
	if err != nil {
		t.Fatalf("playbackcoverage.New: %v", err)
	}
	return s, path
}

func hlsMux(s *Server) *http.ServeMux {
	mux := http.NewServeMux()
	s.registerHLS(mux)
	return mux
}

func createHLSTestItem(t *testing.T, s *Server, fileID, selector, masterURL string) *store.Item {
	t.Helper()
	key := `{"v":1,"backend_id":"b1","handler":"generic","kind":"movie","ids":{"test":"x"}}`
	files, err := httpstream.MarshalFiles([]httpstream.PersistedFile{{FileID: fileID, Selector: selector, Name: "master.m3u8"}})
	if err != nil {
		t.Fatalf("MarshalFiles: %v", err)
	}
	item := &store.Item{
		ID:            "hls-item-1",
		PublicID:      "hls-item-1",
		SourceType:    store.SourceTypeHTTP,
		ClientKind:    store.ClientKindQBit,
		Category:      "movies",
		State:         store.StateReady,
		SubmissionKey: "hls-item-1",
		DisplayName:   "Some.Show.S01E01.DH-HTTP",
		ResolveKey:    &key,
		FileList:      &files,
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}
	if err := s.store.CreateItem(context.Background(), item); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	s.httpHandlers.Register("b1", 1, &hlsTestHandler{url: masterURL, selector: selector})
	return item
}

func TestCreateHLSPlaybackSessionAndServeMasterThenMedia(t *testing.T) {
	origin := hlsFixtureOrigin()
	defer origin.Close()
	s, _ := newHLSTestServer(t)
	tracker, trackerErr := playbackcoverage.New(s.store, playbackcoverage.Options{ProposalEnabled: true, ProposalThreshold: 0.5})
	if trackerErr != nil {
		t.Fatal(trackerErr)
	}
	s.playbackCoverage = tracker
	s.httpClientOnce.Do(func() {
		s.httpProbeClient = origin.Client()
		s.httpRelayClient = origin.Client()
	})
	item := createHLSTestItem(t, s, "f1", "master-sel", origin.URL+"/master.m3u8")

	playURL, err := s.CreateHLSPlaybackSession(context.Background(), item.ID, "f1")
	if err != nil {
		t.Fatalf("CreateHLSPlaybackSession: %v", err)
	}
	if !strings.HasPrefix(playURL, "/hls/") {
		t.Fatalf("playURL = %q, want /hls/ prefix", playURL)
	}
	if !strings.HasSuffix(playURL, ".m3u8") {
		t.Fatalf("playURL = %q, want player-safe playlist suffix", playURL)
	}
	resourceID := strings.TrimPrefix(playURL, "/hls/")

	mux := hlsMux(s)

	// GET the master resource: expect the rewritten body to reference a
	// NEW opaque /hls/ URL for the variant, never the origin's own
	// relative "low/index.m3u8" path.
	req := httptest.NewRequest(http.MethodGet, playURL, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET master: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	masterBody := rec.Body.String()
	if strings.Contains(masterBody, "low/index.m3u8") {
		t.Fatalf("original variant URI leaked into rewritten master:\n%s", masterBody)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/vnd.apple.mpegurl" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}

	var mediaResourceID string
	for _, line := range strings.Split(masterBody, "\n") {
		if strings.HasPrefix(line, "/hls/") {
			mediaResourceID = strings.TrimPrefix(strings.TrimSpace(line), "/hls/")
		}
	}
	if mediaResourceID == "" {
		t.Fatalf("no rewritten variant URL found in master body:\n%s", masterBody)
	}
	if !strings.HasSuffix(mediaResourceID, ".m3u8") {
		t.Fatalf("media resource = %q, want player-safe playlist suffix", mediaResourceID)
	}

	// GET the media (variant) resource: expect the rewritten body to
	// reference a further opaque /hls/ URL for the segment, never the
	// origin's own "seg0.ts".
	req2 := httptest.NewRequest(http.MethodGet, "/hls/"+mediaResourceID, nil)
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET media: status = %d, body = %s", rec2.Code, rec2.Body.String())
	}
	mediaBody := rec2.Body.String()
	if strings.Contains(mediaBody, "seg0.ts") {
		t.Fatalf("original segment URI leaked into rewritten media playlist:\n%s", mediaBody)
	}
	if !strings.Contains(mediaBody, "#EXT-X-ENDLIST") {
		t.Errorf("unrelated media playlist tag lost:\n%s", mediaBody)
	}

	var segResourceID string
	for _, line := range strings.Split(mediaBody, "\n") {
		if strings.HasPrefix(line, "/hls/") {
			segResourceID = strings.TrimPrefix(strings.TrimSpace(line), "/hls/")
		}
	}
	if segResourceID == "" {
		t.Fatalf("no rewritten segment URL found in media body:\n%s", mediaBody)
	}
	if !strings.HasSuffix(segResourceID, ".ts") {
		t.Fatalf("segment resource = %q, want player-safe media suffix", segResourceID)
	}

	// HR4.3 proxies the leaf bytes without exposing the origin.
	req3 := httptest.NewRequest(http.MethodGet, "/hls/"+segResourceID, nil)
	rec3 := httptest.NewRecorder()
	mux.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK || rec3.Body.String() != "segment-zero" {
		t.Fatalf("GET segment: status = %d body=%q", rec3.Code, rec3.Body.String())
	}
	representationID := httpRepresentationID(item.ID, "f1")
	coverage, found, err := s.playbackCoverage.MediaSnapshot(context.Background(), representationID)
	if err != nil || !found || coverage.DeliveredNS != 6*int64(time.Second) || len(coverage.Intervals) != 1 {
		t.Fatalf("full segment media coverage=%+v found=%v err=%v", coverage, found, err)
	}
	proposal, found, err := s.store.GetPlaybackProposal(context.Background(), representationID)
	if err != nil || !found || proposal.ExtentKind != playbackcoverage.ExtentVODHLS ||
		proposal.Delivered != 6*int64(time.Second) || proposal.Total != 6*int64(time.Second) {
		t.Fatalf("VOD proposal=%+v found=%v err=%v", proposal, found, err)
	}
	req3Range := httptest.NewRequest(http.MethodGet, "/hls/"+segResourceID, nil)
	req3Range.Header.Set("Range", "bytes=1-3")
	rec3Range := httptest.NewRecorder()
	mux.ServeHTTP(rec3Range, req3Range)
	if rec3Range.Code != http.StatusPartialContent || rec3Range.Body.String() != "egm" ||
		rec3Range.Header().Get("Content-Range") != "bytes 1-3/12" {
		t.Fatalf("range segment: status=%d range=%q body=%q", rec3Range.Code, rec3Range.Header().Get("Content-Range"), rec3Range.Body.String())
	}
	for _, tc := range []struct {
		header string
		body   string
		span   string
	}{
		{header: "bytes=4-", body: "ent-zero", span: "bytes 4-11/12"},
		{header: "bytes=-4", body: "zero", span: "bytes 8-11/12"},
	} {
		req := httptest.NewRequest(http.MethodGet, "/hls/"+segResourceID, nil)
		req.Header.Set("Range", tc.header)
		out := httptest.NewRecorder()
		mux.ServeHTTP(out, req)
		if out.Code != http.StatusPartialContent || out.Body.String() != tc.body ||
			out.Header().Get("Content-Range") != tc.span {
			t.Fatalf("range %q: status=%d range=%q body=%q", tc.header, out.Code, out.Header().Get("Content-Range"), out.Body.String())
		}
	}
	coverage, found, err = s.playbackCoverage.MediaSnapshot(context.Background(), representationID)
	if err != nil || !found || coverage.DeliveredNS != 6*int64(time.Second) || len(coverage.Intervals) != 1 {
		t.Fatalf("client ranges changed media coverage=%+v found=%v err=%v", coverage, found, err)
	}

	// Idempotency: fetching the master a second time must mint the SAME
	// child resource ID, not a duplicate (bounded session state, DG-07).
	req4 := httptest.NewRequest(http.MethodGet, playURL, nil)
	rec4 := httptest.NewRecorder()
	mux.ServeHTTP(rec4, req4)
	if rec4.Code != http.StatusOK {
		t.Fatalf("second GET master: status = %d", rec4.Code)
	}
	if rec4.Body.String() != masterBody {
		t.Fatalf("second master fetch produced a different rewrite (non-idempotent minting):\nfirst:\n%s\nsecond:\n%s", masterBody, rec4.Body.String())
	}

	_ = resourceID
}

func TestHLSExpiryRefreshPreservesOpaqueResourcePaths(t *testing.T) {
	var expired atomic.Bool
	origin := expiringHLSOrigin(&expired)
	defer origin.Close()

	s, _ := newHLSTestServer(t)
	s.httpClientOnce.Do(func() {
		s.httpProbeClient = origin.Client()
		s.httpRelayClient = origin.Client()
	})
	item := createHLSTestItem(t, s, "f1", "master-sel", origin.URL+"/stale/master.m3u8")
	refreshContexts := make(chan string, 1)
	handler := &rotatingHLSHandler{
		selector:       "master-sel",
		staleURL:       origin.URL + "/stale/master.m3u8",
		freshURL:       origin.URL + "/fresh/master.m3u8",
		refreshContext: refreshContexts,
	}
	s.httpHandlers.Register("b1", 1, handler)

	playURL, err := s.CreateHLSPlaybackSession(context.Background(), item.ID, "f1")
	if err != nil {
		t.Fatal(err)
	}
	mux := hlsMux(s)
	get := func(resource string) *httptest.ResponseRecorder {
		t.Helper()
		out := httptest.NewRecorder()
		mux.ServeHTTP(out, httptest.NewRequest(http.MethodGet, resource, nil))
		if out.Code != http.StatusOK {
			t.Fatalf("GET opaque resource: status=%d body=%q", out.Code, out.Body.String())
		}
		return out
	}

	masterBefore := get(playURL).Body.String()
	masterEntries, err := hls.ParseMaster(masterBefore)
	if err != nil {
		t.Fatal(err)
	}
	var mediaURL, subtitleURL string
	for _, entry := range masterEntries {
		switch entry.Kind {
		case hlssession.KindMedia:
			mediaURL = entry.URI
		case hlssession.KindSubtitle:
			subtitleURL = entry.URI
		}
	}
	if mediaURL == "" || subtitleURL == "" {
		t.Fatalf("master entries missing: %v", masterEntries)
	}
	mediaBefore := get(mediaURL).Body.String()
	subtitleBefore := get(subtitleURL).Body.String()
	mediaEntries, err := hls.ParseMedia(mediaBefore)
	if err != nil || len(mediaEntries) != 3 {
		t.Fatalf("media entries=%v err=%v", mediaEntries, err)
	}
	subtitleEntries, err := hls.ParseMedia(subtitleBefore)
	if err != nil || len(subtitleEntries) != 1 {
		t.Fatalf("subtitle entries=%v err=%v", subtitleEntries, err)
	}

	wantBodies := map[string]string{
		mediaEntries[0].URI:    "0123456789abcdef",
		mediaEntries[1].URI:    "init-map",
		mediaEntries[2].URI:    "equivalent-segment",
		subtitleEntries[0].URI: "WEBVTT\n\n00:00.000 --> 00:01.000\nEquivalent\n",
	}
	for resource, want := range wantBodies {
		if got := get(resource).Body.String(); got != want {
			t.Fatalf("pre-expiry body=%q want=%q", got, want)
		}
	}

	expired.Store(true)
	if got := get(mediaEntries[2].URI).Body.String(); got != wantBodies[mediaEntries[2].URI] {
		t.Fatalf("post-refresh segment=%q", got)
	}
	if got := get(playURL).Body.String(); got != masterBefore {
		t.Fatal("master rewrite changed opaque paths after refresh")
	}
	if got := get(mediaURL).Body.String(); got != mediaBefore {
		t.Fatal("media rewrite changed opaque paths after refresh")
	}
	if got := get(subtitleURL).Body.String(); got != subtitleBefore {
		t.Fatal("subtitle rewrite changed opaque paths after refresh")
	}
	for resource, want := range wantBodies {
		if got := get(resource).Body.String(); got != want {
			t.Fatalf("post-refresh body=%q want=%q", got, want)
		}
	}
	if got := handler.normalCalls.Load(); got != 1 {
		t.Fatalf("normal resolves=%d want=1", got)
	}
	if got := handler.forcedCalls.Load(); got != 1 {
		t.Fatalf("forced resolves=%d want=1", got)
	}
	if got := <-refreshContexts; got != "opaque-stale-context" {
		t.Fatalf("forced refresh context=%q", got)
	}
	for _, body := range []string{masterBefore, mediaBefore, subtitleBefore} {
		if strings.Contains(body, "://") || strings.Contains(body, "stale") || strings.Contains(body, "fresh") {
			t.Fatal("origin identity leaked into rewritten playlist")
		}
	}
}

func TestHLSConcurrentExpiryRefreshCoalesces(t *testing.T) {
	var expired atomic.Bool
	origin := expiringHLSOrigin(&expired)
	defer origin.Close()

	s, _ := newHLSTestServer(t)
	s.httpClientOnce.Do(func() {
		s.httpProbeClient = origin.Client()
		s.httpRelayClient = origin.Client()
	})
	item := createHLSTestItem(t, s, "f1", "master-sel", origin.URL+"/stale/master.m3u8")
	forceBlock := make(chan struct{})
	forceStarted := make(chan struct{})
	handler := &rotatingHLSHandler{
		selector:     "master-sel",
		staleURL:     origin.URL + "/stale/master.m3u8",
		freshURL:     origin.URL + "/fresh/master.m3u8",
		forceBlock:   forceBlock,
		forceStarted: forceStarted,
	}
	s.httpHandlers.Register("b1", 1, handler)
	playURL, err := s.CreateHLSPlaybackSession(context.Background(), item.ID, "f1")
	if err != nil {
		t.Fatal(err)
	}
	mux := hlsMux(s)
	master := httptest.NewRecorder()
	mux.ServeHTTP(master, httptest.NewRequest(http.MethodGet, playURL, nil))
	masterEntries, _ := hls.ParseMaster(master.Body.String())
	mediaURL := masterEntries[len(masterEntries)-1].URI
	media := httptest.NewRecorder()
	mux.ServeHTTP(media, httptest.NewRequest(http.MethodGet, mediaURL, nil))
	mediaEntries, _ := hls.ParseMedia(media.Body.String())
	segmentURL := mediaEntries[len(mediaEntries)-1].URI

	expired.Store(true)
	const clients = 20
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, clients)
	var wg sync.WaitGroup
	for range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			out := httptest.NewRecorder()
			mux.ServeHTTP(out, httptest.NewRequest(http.MethodGet, segmentURL, nil))
			results <- out
		}()
	}
	close(start)
	select {
	case <-forceStarted:
	case <-time.After(time.Second):
		t.Fatal("forced refresh did not start")
	}
	time.Sleep(50 * time.Millisecond)
	close(forceBlock)
	wg.Wait()
	close(results)
	for out := range results {
		if out.Code != http.StatusOK || out.Body.String() != "equivalent-segment" {
			t.Fatalf("concurrent refresh status=%d body=%q", out.Code, out.Body.String())
		}
	}
	if got := handler.forcedCalls.Load(); got != 1 {
		t.Fatalf("%d concurrent clients caused %d forced resolves, want 1", clients, got)
	}
}

func TestHLSExpiryRefreshExhaustionIsBounded(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusTooManyRequests} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var expired atomic.Bool
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if expired.Load() {
					w.Header().Set("Retry-After", "999999")
					http.Error(w, "expired secret body", status)
					return
				}
				if r.URL.Path == "/media.m3u8" {
					_, _ = io.WriteString(w, "#EXTM3U\n#EXTINF:6,\nseg.ts\n#EXT-X-ENDLIST\n")
					return
				}
				_, _ = io.WriteString(w, "equivalent-segment")
			}))
			defer origin.Close()
			s, _ := newHLSTestServer(t)
			s.httpClientOnce.Do(func() {
				s.httpProbeClient = origin.Client()
				s.httpRelayClient = origin.Client()
			})
			item := createHLSTestItem(t, s, "f1", "media-sel", origin.URL+"/media.m3u8")
			handler := &rotatingHLSHandler{
				selector: "media-sel",
				staleURL: origin.URL + "/media.m3u8",
				freshURL: origin.URL + "/media.m3u8",
			}
			s.httpHandlers.Register("b1", 1, handler)
			playURL, err := s.CreateHLSPlaybackSession(context.Background(), item.ID, "f1")
			if err != nil {
				t.Fatal(err)
			}
			mux := hlsMux(s)
			media := httptest.NewRecorder()
			mux.ServeHTTP(media, httptest.NewRequest(http.MethodGet, playURL, nil))
			entries, _ := hls.ParseMedia(media.Body.String())
			expired.Store(true)

			leaf := httptest.NewRecorder()
			mux.ServeHTTP(leaf, httptest.NewRequest(http.MethodGet, entries[0].URI, nil))
			if leaf.Code != http.StatusServiceUnavailable || leaf.Header().Get("Retry-After") != "60" {
				t.Fatalf("status=%d retry-after=%q body=%q", leaf.Code, leaf.Header().Get("Retry-After"), leaf.Body.String())
			}
			if strings.Contains(leaf.Body.String(), "expired") || strings.Contains(leaf.Body.String(), "secret") {
				t.Fatal("upstream error body leaked")
			}
			if got := handler.forcedCalls.Load(); got != 1 {
				t.Fatalf("forced resolves=%d want=1", got)
			}
		})
	}
}

func TestHLSLeafHEADExpiryRefreshesOpenRange(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/stale/master.m3u8", "/fresh/master.m3u8":
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\nmedia.m3u8\n")
		case "/stale/media.m3u8", "/fresh/media.m3u8":
			_, _ = io.WriteString(w, "#EXTM3U\n#EXTINF:6,\nsegment.ts\n#EXT-X-ENDLIST\n")
		case "/stale/segment.ts":
			http.Error(w, "expired", http.StatusForbidden)
		case "/fresh/segment.ts":
			http.ServeContent(w, r, "segment.ts", time.Time{}, strings.NewReader("equivalent-segment"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer origin.Close()

	s, _ := newHLSTestServer(t)
	s.httpClientOnce.Do(func() {
		s.httpProbeClient = origin.Client()
		s.httpRelayClient = origin.Client()
	})
	item := createHLSTestItem(t, s, "f1", "master-sel", origin.URL+"/stale/master.m3u8")
	handler := &rotatingHLSHandler{
		selector: "master-sel",
		staleURL: origin.URL + "/stale/master.m3u8",
		freshURL: origin.URL + "/fresh/master.m3u8",
	}
	s.httpHandlers.Register("b1", 1, handler)
	playURL, err := s.CreateHLSPlaybackSession(context.Background(), item.ID, "f1")
	if err != nil {
		t.Fatal(err)
	}
	mux := hlsMux(s)
	get := func(path string) *httptest.ResponseRecorder {
		out := httptest.NewRecorder()
		mux.ServeHTTP(out, httptest.NewRequest(http.MethodGet, path, nil))
		if out.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%q", path, out.Code, out.Body.String())
		}
		return out
	}
	masterEntries, err := hls.ParseMaster(get(playURL).Body.String())
	if err != nil {
		t.Fatal(err)
	}
	mediaEntries, err := hls.ParseMedia(get(masterEntries[0].URI).Body.String())
	if err != nil {
		t.Fatal(err)
	}
	segmentURL := mediaEntries[0].URI

	req := httptest.NewRequest(http.MethodGet, segmentURL, nil)
	req.Header.Set("Range", "bytes=-7")
	out := httptest.NewRecorder()
	mux.ServeHTTP(out, req)
	if out.Code != http.StatusPartialContent || out.Body.String() != "segment" {
		t.Fatalf("refreshed suffix range status=%d body=%q", out.Code, out.Body.String())
	}
	if got := out.Header().Get("Content-Range"); got != "bytes 11-17/18" {
		t.Fatalf("content-range=%q", got)
	}
	if got := handler.forcedCalls.Load(); got != 1 {
		t.Fatalf("leaf HEAD expiry caused %d forced resolves, want 1", got)
	}
	if got := get(segmentURL).Body.String(); got != "equivalent-segment" {
		t.Fatalf("stable segment path after leaf refresh=%q", got)
	}
}

func TestHLSLeafFixedRangesMapsKeysAndMalformedUpstream(t *testing.T) {
	const media = "0123456789abcdef"
	var seenRanges []string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/media.m3u8":
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n#EXT-X-MAP:URI=\"blob.bin\",BYTERANGE=\"4@1\"\n#EXT-X-BYTERANGE:5@2\n#EXTINF:6,\nblob.bin\n#EXT-X-DISCONTINUITY\n#EXT-X-ENDLIST\n")
		case "/key.bin":
			_, _ = io.WriteString(w, "secret-key-bytes")
		case "/blob.bin":
			seenRanges = append(seenRanges, r.Header.Get("Range"))
			http.ServeContent(w, r, "blob.bin", time.Time{}, bytes.NewReader([]byte(media)))
		case "/bad.bin":
			w.Header().Set("Content-Range", "bytes 0-1/16")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "xx")
		default:
			http.NotFound(w, r)
		}
	}))
	defer origin.Close()

	s, _ := newHLSTestServer(t)
	s.httpClientOnce.Do(func() {
		s.httpProbeClient = origin.Client()
		s.httpRelayClient = origin.Client()
	})
	item := createHLSTestItem(t, s, "f1", "media-sel", origin.URL+"/media.m3u8")
	playURL, err := s.CreateHLSPlaybackSession(context.Background(), item.ID, "f1")
	if err != nil {
		t.Fatalf("CreateHLSPlaybackSession: %v", err)
	}
	mux := hlsMux(s)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, playURL, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("media status=%d body=%s", rec.Code, rec.Body.String())
	}
	entries, err := hls.ParseMedia(rec.Body.String())
	if err != nil || len(entries) != 3 {
		t.Fatalf("ParseMedia entries=%d err=%v body=%s", len(entries), err, rec.Body.String())
	}

	get := func(method, resource, byteRange string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, resource, nil)
		if byteRange != "" {
			req.Header.Set("Range", byteRange)
		}
		out := httptest.NewRecorder()
		mux.ServeHTTP(out, req)
		return out
	}
	key := get(http.MethodGet, entries[0].URI, "")
	if key.Code != http.StatusOK || key.Body.String() != "secret-key-bytes" ||
		key.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("key status=%d type=%q body=%q", key.Code, key.Header().Get("Content-Type"), key.Body.String())
	}
	keyFullRange := get(http.MethodGet, entries[0].URI, "bytes=0-")
	if keyFullRange.Code != http.StatusOK || keyFullRange.Body.String() != "secret-key-bytes" {
		t.Fatalf("whole-key range fallback status=%d body=%q", keyFullRange.Code, keyFullRange.Body.String())
	}
	keyPartial := get(http.MethodGet, entries[0].URI, "bytes=1-2")
	if keyPartial.Code != http.StatusBadGateway {
		t.Fatalf("partial key range-ignoring response status=%d, want 502", keyPartial.Code)
	}
	initMap := get(http.MethodGet, entries[1].URI, "")
	if initMap.Code != http.StatusOK || initMap.Body.String() != media[1:5] {
		t.Fatalf("map status=%d body=%q", initMap.Code, initMap.Body.String())
	}
	partial := get(http.MethodGet, entries[1].URI, "bytes=1-2")
	if partial.Code != http.StatusPartialContent || partial.Body.String() != media[2:4] ||
		partial.Header().Get("Content-Range") != "bytes 1-2/4" {
		t.Fatalf("partial map status=%d range=%q body=%q", partial.Code, partial.Header().Get("Content-Range"), partial.Body.String())
	}
	head := get(http.MethodHead, entries[2].URI, "")
	if head.Code != http.StatusOK || head.Header().Get("Content-Length") != "5" || head.Body.Len() != 0 {
		t.Fatalf("segment HEAD status=%d length=%q body=%q", head.Code, head.Header().Get("Content-Length"), head.Body.String())
	}
	unsatisfied := get(http.MethodGet, entries[2].URI, "bytes=9-10")
	if unsatisfied.Code != http.StatusRequestedRangeNotSatisfiable ||
		unsatisfied.Header().Get("Content-Range") != "bytes */5" {
		t.Fatalf("segment 416 status=%d range=%q", unsatisfied.Code, unsatisfied.Header().Get("Content-Range"))
	}
	if got := strings.Join(seenRanges, ","); got != "bytes=1-4,bytes=2-3" {
		t.Fatalf("upstream ranges=%q", got)
	}
}

func TestHLSSubtitlePlaylistAndPersistentPayload(t *testing.T) {
	origin := hlsFixtureOrigin()
	defer origin.Close()
	s, _ := newHLSTestServer(t)
	s.httpClientOnce.Do(func() {
		s.httpProbeClient = origin.Client()
		s.httpRelayClient = origin.Client()
	})
	item := createHLSTestItem(t, s, "f1", "master-sel", origin.URL+"/master.m3u8")
	playURL, err := s.CreateHLSPlaybackSession(context.Background(), item.ID, "f1")
	if err != nil {
		t.Fatal(err)
	}
	mux := hlsMux(s)
	master := httptest.NewRecorder()
	mux.ServeHTTP(master, httptest.NewRequest(http.MethodGet, playURL, nil))
	entries, err := hls.ParseMaster(master.Body.String())
	if err != nil || len(entries) < 2 || entries[0].Kind != hlssession.KindSubtitle || entries[1].Kind != hlssession.KindSubtitle {
		t.Fatalf("subtitle entry missing: entries=%v err=%v", entries, err)
	}
	subtitlePlaylist := httptest.NewRecorder()
	mux.ServeHTTP(subtitlePlaylist, httptest.NewRequest(http.MethodGet, entries[0].URI, nil))
	if subtitlePlaylist.Code != http.StatusOK || strings.Contains(subtitlePlaylist.Body.String(), "cue.vtt") {
		t.Fatalf("subtitle playlist status=%d body=%s", subtitlePlaylist.Code, subtitlePlaylist.Body.String())
	}
	subEntries, err := hls.ParseMedia(subtitlePlaylist.Body.String())
	if err != nil || len(subEntries) != 1 {
		t.Fatalf("subtitle media entries=%v err=%v", subEntries, err)
	}
	cue := httptest.NewRecorder()
	mux.ServeHTTP(cue, httptest.NewRequest(http.MethodGet, subEntries[0].URI, nil))
	if cue.Code != http.StatusOK || !strings.HasPrefix(cue.Body.String(), "WEBVTT") {
		t.Fatalf("cue status=%d body=%q", cue.Code, cue.Body.String())
	}
	if strings.Contains(master.Body.String()+subtitlePlaylist.Body.String(), origin.URL) {
		t.Fatal("origin URL leaked into rewritten subtitle graph")
	}

	direct := httptest.NewRecorder()
	mux.ServeHTTP(direct, httptest.NewRequest(http.MethodGet, entries[1].URI, nil))
	if direct.Code != http.StatusOK || !strings.Contains(direct.Body.String(), "Persisted") {
		t.Fatalf("direct subtitle status=%d body=%q", direct.Code, direct.Body.String())
	}
	origin.Close()
	replay := httptest.NewRecorder()
	mux.ServeHTTP(replay, httptest.NewRequest(http.MethodGet, entries[1].URI, nil))
	if replay.Code != http.StatusOK || replay.Body.String() != direct.Body.String() {
		t.Fatalf("persisted subtitle replay status=%d body=%q", replay.Code, replay.Body.String())
	}
}

func TestHLSLeafRejectsIncoherent206(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/media.m3u8" {
			_, _ = fmt.Fprint(w, "#EXTM3U\n#EXT-X-BYTERANGE:4@2\n#EXTINF:6,\nbad.bin\n#EXT-X-ENDLIST\n")
			return
		}
		w.Header().Set("Content-Range", "bytes 0-1/16")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "xx")
	}))
	defer origin.Close()
	s, _ := newHLSTestServer(t)
	s.httpClientOnce.Do(func() {
		s.httpProbeClient = origin.Client()
		s.httpRelayClient = origin.Client()
	})
	item := createHLSTestItem(t, s, "f1", "media-sel", origin.URL+"/media.m3u8")
	playURL, err := s.CreateHLSPlaybackSession(context.Background(), item.ID, "f1")
	if err != nil {
		t.Fatal(err)
	}
	mux := hlsMux(s)
	media := httptest.NewRecorder()
	mux.ServeHTTP(media, httptest.NewRequest(http.MethodGet, playURL, nil))
	entries, _ := hls.ParseMedia(media.Body.String())
	leaf := httptest.NewRecorder()
	mux.ServeHTTP(leaf, httptest.NewRequest(http.MethodGet, entries[0].URI, nil))
	if leaf.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%q", leaf.Code, leaf.Body.String())
	}
}

func TestHLSLeafCancellationStopsOrigin(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/media.m3u8" {
			_, _ = io.WriteString(w, "#EXTM3U\n#EXTINF:6,\nslow.ts\n#EXT-X-ENDLIST\n")
			return
		}
		close(started)
		<-r.Context().Done()
		close(stopped)
	}))
	defer origin.Close()
	s, _ := newHLSTestServer(t)
	s.httpClientOnce.Do(func() {
		s.httpProbeClient = origin.Client()
		s.httpRelayClient = origin.Client()
	})
	item := createHLSTestItem(t, s, "f1", "media-sel", origin.URL+"/media.m3u8")
	playURL, err := s.CreateHLSPlaybackSession(context.Background(), item.ID, "f1")
	if err != nil {
		t.Fatal(err)
	}
	mux := hlsMux(s)
	media := httptest.NewRecorder()
	mux.ServeHTTP(media, httptest.NewRequest(http.MethodGet, playURL, nil))
	entries, _ := hls.ParseMedia(media.Body.String())

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, entries[0].URI, nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		mux.ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("HLS handler leaked after cancellation")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("origin request was not cancelled")
	}
	if snapshot, found, err := s.playbackCoverage.MediaSnapshot(context.Background(), httpRepresentationID(item.ID, "f1")); err != nil || found {
		t.Fatalf("canceled leaf changed media coverage=%+v found=%v err=%v", snapshot, found, err)
	}
}

func TestHLSUsesTrustSourcePolicy(t *testing.T) {
	origin := hlsFixtureOrigin()
	defer origin.Close()
	s, _ := newHLSTestServer(t)
	item := createHLSTestItem(t, s, "f1", "master-sel", origin.URL+"/master.m3u8")
	_, err := s.CreateHLSPlaybackSession(context.Background(), item.ID, "f1")
	if err == nil {
		t.Fatal("loopback HLS origin bypassed TrustSource policy")
	}
	if strings.Contains(err.Error(), origin.URL) {
		t.Fatalf("origin URL leaked in error: %v", err)
	}
}

func TestHLSSealedHeadersDoNotCrossOriginRedirect(t *testing.T) {
	const header = "X-Fixture-Authorization"
	received := make(chan string, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get(header)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/leaf", http.StatusFound)
	}))
	defer origin.Close()
	source := hlsLiveSource{URL: origin.URL, Headers: map[string]string{header: "sealed-value"}}
	req, _ := http.NewRequest(http.MethodGet, source.URL, nil)
	req.Header.Set(header, source.Headers[header])
	resp, err := hlsHTTPClient(origin.Client(), source).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := <-received; got != "" {
		t.Fatal("sealed header crossed an origin redirect")
	}
}

func TestHTTPStreamDispatchesHLSFileToOpaqueSession(t *testing.T) {
	origin := hlsFixtureOrigin()
	defer origin.Close()
	s, _ := newHLSTestServer(t)
	s.httpClientOnce.Do(func() {
		s.httpProbeClient = origin.Client()
		s.httpRelayClient = origin.Client()
	})
	item := createHLSTestItem(t, s, "f1", "master-sel", origin.URL+"/master.m3u8")
	files, err := httpstream.ParseFiles(*item.FileList)
	if err != nil {
		t.Fatal(err)
	}
	files[0].ContentType = "application/vnd.apple.mpegurl"
	encoded, err := httpstream.MarshalFiles(files)
	if err != nil {
		t.Fatal(err)
	}
	item.FileList = &encoded
	if err := s.store.UpdateItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/stream/"+item.ID+"/f1", nil)
	req.SetPathValue("file_id", "f1")
	rec := httptest.NewRecorder()
	s.streamHTTPItem(rec, req, item)
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, "/hls/") || strings.Contains(location, origin.URL) {
		t.Fatalf("unsafe HLS redirect location %q", location)
	}
	root := httptest.NewRecorder()
	hlsMux(s).ServeHTTP(root, httptest.NewRequest(http.MethodGet, location, nil))
	if root.Code != http.StatusOK || strings.Contains(root.Body.String(), origin.URL) {
		t.Fatalf("root status=%d body=%q", root.Code, root.Body.String())
	}
}

// TestHLSResourceGraphSurvivesRestart proves the HR4.1 restart-resumable
// claim now has a real consumer: a resource ID minted before a restart
// resolves to byte-identical rewritten content after the process (a fresh
// *store.Store over the same on-disk db, exactly like a real restart)
// comes back up, since the graph never depended on anything held only in
// memory.
func TestHLSResourceGraphSurvivesRestart(t *testing.T) {
	origin := hlsFixtureOrigin()
	defer origin.Close()

	s1, path := newHLSTestServer(t)
	s1.httpClientOnce.Do(func() {
		s1.httpProbeClient = origin.Client()
		s1.httpRelayClient = origin.Client()
	})
	item := createHLSTestItem(t, s1, "f1", "master-sel", origin.URL+"/master.m3u8")
	playURL, err := s1.CreateHLSPlaybackSession(context.Background(), item.ID, "f1")
	if err != nil {
		t.Fatalf("CreateHLSPlaybackSession: %v", err)
	}
	mux1 := hlsMux(s1)
	req := httptest.NewRequest(http.MethodGet, playURL, nil)
	rec := httptest.NewRecorder()
	mux1.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pre-restart GET master: status = %d", rec.Code)
	}
	before := rec.Body.String()

	// Simulate a DarkHarrbor restart: open a fresh *sql.DB/*Store/*Server
	// over the SAME on-disk database file.
	db2, err := store.Open(context.Background(), path, 5*time.Second)
	if err != nil {
		t.Fatalf("store.Open (restart): %v", err)
	}
	defer db2.Close()
	st2 := store.New(db2)
	graph2, err := hlssession.New(st2, hlssession.Options{})
	if err != nil {
		t.Fatalf("hlssession.New (restart): %v", err)
	}
	s2 := httpTestServer("0123456789abcdef0123456789abcdef", true)
	s2.store = st2
	s2.hlsSessions = graph2
	s2.httpHandlers = httpstream.NewRegistry()
	s2.httpResolveCoord = newHTTPResolveCoordinator()
	s2.httpClientOnce.Do(func() {
		s2.httpProbeClient = origin.Client()
		s2.httpRelayClient = origin.Client()
	})
	// The handler registration is process-local configuration (re-supplied
	// at startup in real DarkHarrbor, exactly like every other backend);
	// only the graph/session/resource state must survive on disk.
	s2.httpHandlers.Register("b1", 1, &hlsTestHandler{url: origin.URL + "/master.m3u8", selector: "master-sel"})

	mux2 := hlsMux(s2)
	req2 := httptest.NewRequest(http.MethodGet, playURL, nil)
	rec2 := httptest.NewRecorder()
	mux2.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("post-restart GET master: status = %d, body = %s", rec2.Code, rec2.Body.String())
	}
	if rec2.Body.String() != before {
		t.Fatalf("post-restart rewrite differs from pre-restart:\nbefore:\n%s\nafter:\n%s", before, rec2.Body.String())
	}
}

func TestHLSSlidingLiveWindowStableIdentityAndBoundedState(t *testing.T) {
	var sequence atomic.Int64
	sequence.Store(40)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/live.m3u8" {
			first := sequence.Load()
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:%d\n", first)
			for n := first; n < first+3; n++ {
				_, _ = fmt.Fprintf(w, "#EXTINF:2,\nseg-%d.ts\n", n)
			}
			return
		}
		var n int64
		if _, err := fmt.Sscanf(r.URL.Path, "/seg-%d.ts", &n); err != nil {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "segment.ts", time.Time{}, strings.NewReader(fmt.Sprintf("segment-%d", n)))
	}))
	defer origin.Close()

	s, _ := newHLSTestServer(t)
	s.httpClientOnce.Do(func() {
		s.httpProbeClient = origin.Client()
		s.httpRelayClient = origin.Client()
	})
	item := createHLSTestItem(t, s, "f1", "live-sel", origin.URL+"/live.m3u8")
	playURL, err := s.CreateHLSPlaybackSession(context.Background(), item.ID, "f1")
	if err != nil {
		t.Fatalf("CreateHLSPlaybackSession: %v", err)
	}
	mux := hlsMux(s)
	fetchWindow := func() []hls.Entry {
		t.Helper()
		out := httptest.NewRecorder()
		mux.ServeHTTP(out, httptest.NewRequest(http.MethodGet, playURL, nil))
		if out.Code != http.StatusOK {
			t.Fatalf("live manifest status=%d body=%q", out.Code, out.Body.String())
		}
		if !strings.Contains(out.Body.String(), "#EXT-X-TARGETDURATION:2") ||
			!strings.Contains(out.Body.String(), "#EXT-X-MEDIA-SEQUENCE:") ||
			strings.Contains(out.Body.String(), "#EXT-X-ENDLIST") {
			t.Fatalf("live control tags changed: %q", out.Body.String())
		}
		entries, err := hls.ParseMedia(out.Body.String())
		if err != nil {
			t.Fatalf("ParseMedia rewritten live manifest: %v", err)
		}
		return entries
	}

	first := fetchWindow()
	sequence.Store(41)
	second := fetchWindow()
	if first[1].URI != second[0].URI || first[2].URI != second[1].URI {
		t.Fatalf("overlapping live identities changed: first=%v second=%v", first, second)
	}
	overlap := httptest.NewRecorder()
	mux.ServeHTTP(overlap, httptest.NewRequest(http.MethodGet, second[0].URI, nil))
	if overlap.Code != http.StatusOK || overlap.Body.String() != "segment-41" {
		t.Fatalf("overlap status=%d body=%q", overlap.Code, overlap.Body.String())
	}
	evicted := httptest.NewRecorder()
	mux.ServeHTTP(evicted, httptest.NewRequest(http.MethodGet, first[0].URI, nil))
	if evicted.Code != http.StatusNotFound {
		t.Fatalf("evicted segment status=%d body=%q, want safe 404", evicted.Code, evicted.Body.String())
	}

	for n := int64(42); n < 90; n++ {
		sequence.Store(n)
		_ = fetchWindow()
	}
	var wg sync.WaitGroup
	responses := make(chan *httptest.ResponseRecorder, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out := httptest.NewRecorder()
			mux.ServeHTTP(out, httptest.NewRequest(http.MethodGet, playURL, nil))
			responses <- out
		}()
	}
	wg.Wait()
	close(responses)
	var concurrentBody string
	for response := range responses {
		if response.Code != http.StatusOK {
			t.Fatalf("concurrent live refresh status=%d", response.Code)
		}
		if concurrentBody == "" {
			concurrentBody = response.Body.String()
		} else if response.Body.String() != concurrentBody {
			t.Fatal("concurrent live refreshes minted competing opaque identities")
		}
	}
	_ = fetchWindow() // recheck the settled graph after concurrent refreshes
	rootID := strings.TrimSuffix(strings.TrimPrefix(playURL, "/hls/"), ".m3u8")
	root, err := s.hlsSessions.Resolve(context.Background(), rootID)
	if err != nil {
		t.Fatalf("resolve live root: %v", err)
	}
	resources, err := s.hlsSessions.ListResources(context.Background(), root.SessionID)
	if err != nil {
		t.Fatalf("list live resources: %v", err)
	}
	if len(resources) > 7 {
		t.Fatalf("live graph retained %d resources, want at most root plus two 3-segment windows", len(resources))
	}
}

func TestHLSLowLatencyPassThroughBlockingDeltaHintAndRendition(t *testing.T) {
	var generation atomic.Int64
	blockSeen := make(chan string, 2)
	releaseBlock := make(chan struct{})
	cancelSeen := make(chan struct{}, 1)
	preloadSeen := make(chan struct{}, 1)
	releasePreload := make(chan struct{})
	render := func(w http.ResponseWriter, delta bool) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		if delta {
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:10\n#EXT-X-TARGETDURATION:4\n"+
				"#EXT-X-SERVER-CONTROL:CAN-BLOCK-RELOAD=YES,CAN-SKIP-UNTIL=24\n"+
				"#EXT-X-PART-INF:PART-TARGET=1\n#EXT-X-MEDIA-SEQUENCE:9\n"+
				"#EXT-X-SKIP:SKIPPED-SEGMENTS=2\n"+
				"#EXT-X-PART:DURATION=1,URI=\"part11.0.m4s\",INDEPENDENT=YES\n"+
				"#EXT-X-PRELOAD-HINT:TYPE=PART,URI=\"part11.1.m4s\"\n"+
				"#EXT-X-RENDITION-REPORT:URI=\"audio/live.m3u8\",LAST-MSN=11,LAST-PART=0\n")
			return
		}
		seq := int64(10)
		parts := 1
		if generation.Load() == 1 {
			parts = 2
		}
		if generation.Load() >= 2 {
			seq = 11
		}
		_, _ = fmt.Fprintf(w, "#EXTM3U\n#EXT-X-VERSION:10\n#EXT-X-TARGETDURATION:4\n"+
			"#EXT-X-SERVER-CONTROL:CAN-BLOCK-RELOAD=YES,CAN-SKIP-UNTIL=24\n"+
			"#EXT-X-PART-INF:PART-TARGET=1\n#EXT-X-MEDIA-SEQUENCE:%d\n", seq)
		for part := 0; part < parts; part++ {
			_, _ = fmt.Fprintf(w, "#EXT-X-PART:DURATION=1,URI=\"part%d.%d.m4s\"\n", seq, part)
		}
		_, _ = fmt.Fprintf(w, "#EXT-X-PRELOAD-HINT:TYPE=PART,URI=\"part%d.%d.m4s\"\n", seq, parts)
		_, _ = fmt.Fprintf(w, "#EXT-X-RENDITION-REPORT:URI=\"audio/live.m3u8\",LAST-MSN=%d,LAST-PART=%d\n", seq, parts-1)
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/live.m3u8":
			query := r.URL.Query()
			if query.Get("origin") != "kept" {
				http.Error(w, "missing origin query", http.StatusBadRequest)
				return
			}
			if query.Get("_HLS_msn") == "99" {
				cancelSeen <- struct{}{}
				<-r.Context().Done()
				return
			}
			if query.Get("_HLS_msn") != "" {
				blockSeen <- query.Encode()
				<-releaseBlock
				render(w, query.Get("_HLS_skip") != "")
				return
			}
			render(w, false)
		case "/audio/live.m3u8":
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:11\n#EXTINF:4,\naudio11.m4s\n")
		case "/part11.0.m4s":
			_, _ = io.WriteString(w, "part-11-0")
		case "/part10.1.m4s":
			preloadSeen <- struct{}{}
			<-releasePreload
			_, _ = io.WriteString(w, "preloaded-10-1")
		default:
			http.NotFound(w, r)
		}
	}))
	defer origin.Close()

	s, dbPath := newHLSTestServer(t)
	s.httpClientOnce.Do(func() {
		s.httpProbeClient = origin.Client()
		s.httpRelayClient = origin.Client()
	})
	item := createHLSTestItem(t, s, "f1", "ll-sel", origin.URL+"/live.m3u8?origin=kept")
	playURL, err := s.CreateHLSPlaybackSession(context.Background(), item.ID, "f1")
	if err != nil {
		t.Fatalf("CreateHLSPlaybackSession: %v", err)
	}
	mux := hlsMux(s)
	fetch := func(target string) string {
		t.Helper()
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%q", target, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	find := func(t *testing.T, body string, kind hlssession.ResourceKind, msn int64, part int, report bool) hls.Entry {
		t.Helper()
		entries, err := hls.ParseMedia(body)
		if err != nil {
			t.Fatalf("ParseMedia rewritten LL-HLS: %v", err)
		}
		for _, entry := range entries {
			if entry.Kind == kind && entry.HasRenditionIndex == report &&
				(report || (entry.HasMediaSequence && entry.MediaSequence == msn &&
					entry.HasPartIndex && entry.PartIndex == part)) {
				return entry
			}
		}
		t.Fatalf("entry not found kind=%s msn=%d part=%d report=%t: %+v", kind, msn, part, report, entries)
		return hls.Entry{}
	}

	first := fetch(playURL)
	hint := find(t, first, hlssession.KindSegment, 10, 1, false)
	report := find(t, first, hlssession.KindMedia, 0, 0, true)
	if strings.Contains(first, "part10.") || strings.Contains(first, "audio/live") || strings.Contains(first, origin.URL) {
		t.Fatalf("initial LL-HLS manifest leaked origin details:\n%s", first)
	}
	preloadDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, hint.URI, nil))
		preloadDone <- rec
	}()
	select {
	case <-preloadSeen:
	case <-time.After(time.Second):
		t.Fatal("preload hint request did not reach origin")
	}
	close(releasePreload)
	select {
	case rec := <-preloadDone:
		if rec.Code != http.StatusOK || rec.Body.String() != "preloaded-10-1" {
			t.Fatalf("preload hint status=%d body=%q", rec.Code, rec.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("preload hint request did not unblock")
	}

	generation.Store(1)
	second := fetch(playURL)
	published := find(t, second, hlssession.KindSegment, 10, 1, false)
	secondReport := find(t, second, hlssession.KindMedia, 0, 0, true)
	if hint.URI != published.URI {
		t.Fatalf("preload hint identity changed when published: hint=%q part=%q", hint.URI, published.URI)
	}
	if report.URI != secondReport.URI {
		t.Fatalf("rendition report identity changed as part count moved: first=%q second=%q", report.URI, secondReport.URI)
	}
	responses := make(chan *httptest.ResponseRecorder, 12)
	var refreshes sync.WaitGroup
	for i := 0; i < cap(responses); i++ {
		refreshes.Add(1)
		go func() {
			defer refreshes.Done()
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, playURL, nil))
			responses <- rec
		}()
	}
	refreshes.Wait()
	close(responses)
	for rec := range responses {
		if rec.Code != http.StatusOK || rec.Body.String() != second {
			t.Fatalf("concurrent LL-HLS refresh status=%d body differs=%t", rec.Code, rec.Body.String() != second)
		}
	}
	invalid := httptest.NewRecorder()
	mux.ServeHTTP(invalid, httptest.NewRequest(http.MethodGet, playURL+"?_HLS_part=1", nil))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid delivery directive status=%d, want 400", invalid.Code)
	}

	generation.Store(2)
	blockDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, playURL+"?_HLS_msn=11&_HLS_part=0&_HLS_skip=YES&discard=client", nil)
		mux.ServeHTTP(rec, req)
		blockDone <- rec
	}()
	var forwarded string
	select {
	case forwarded = <-blockSeen:
	case <-time.After(time.Second):
		t.Fatal("blocking reload did not reach origin")
	}
	if forwarded != "_HLS_msn=11&_HLS_part=0&_HLS_skip=YES&origin=kept" {
		t.Fatalf("forwarded query=%q", forwarded)
	}
	close(releaseBlock)
	var blocked *httptest.ResponseRecorder
	select {
	case blocked = <-blockDone:
	case <-time.After(time.Second):
		t.Fatal("blocking reload did not complete")
	}
	if blocked.Code != http.StatusOK || !strings.Contains(blocked.Body.String(), "#EXT-X-SKIP:SKIPPED-SEGMENTS=2") {
		t.Fatalf("blocking delta status=%d body=%q", blocked.Code, blocked.Body.String())
	}
	part := find(t, blocked.Body.String(), hlssession.KindSegment, 11, 0, false)
	if got := fetch(part.URI); got != "part-11-0" {
		t.Fatalf("partial segment body=%q", got)
	}
	if got := fetch(report.URI); !strings.Contains(got, "/hls/") || strings.Contains(got, "audio11.m4s") {
		t.Fatalf("rendition report target was not virtualized: %q", got)
	}

	cancelCtx, cancel := context.WithCancel(context.Background())
	cancelDone := make(chan struct{})
	go func() {
		defer close(cancelDone)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, playURL+"?_HLS_msn=99", nil).WithContext(cancelCtx)
		mux.ServeHTTP(rec, req)
	}()
	select {
	case <-cancelSeen:
	case <-time.After(time.Second):
		t.Fatal("cancelable blocking reload did not reach origin")
	}
	cancel()
	select {
	case <-cancelDone:
	case <-time.After(time.Second):
		t.Fatal("canceled blocking reload leaked a handler goroutine")
	}

	rootID := strings.TrimSuffix(strings.TrimPrefix(playURL, "/hls/"), ".m3u8")
	root, err := s.hlsSessions.Resolve(context.Background(), rootID)
	if err != nil {
		t.Fatalf("resolve LL-HLS root: %v", err)
	}
	resources, err := s.hlsSessions.ListResources(context.Background(), root.SessionID)
	if err != nil {
		t.Fatalf("list LL-HLS resources: %v", err)
	}
	if len(resources) > 12 {
		t.Fatalf("LL-HLS graph retained %d resources, want bounded current/prior parts", len(resources))
	}

	db2, err := store.Open(context.Background(), dbPath, 5*time.Second)
	if err != nil {
		t.Fatalf("open LL-HLS store after restart: %v", err)
	}
	defer db2.Close()
	var refs string
	if err := db2.QueryRowContext(context.Background(), `SELECT COALESCE(group_concat(ref_json, ''), '') FROM hls_resources WHERE session_id = ?`, root.SessionID).Scan(&refs); err != nil {
		t.Fatalf("scan durable LL-HLS references: %v", err)
	}
	if strings.Contains(refs, origin.URL) || strings.Contains(refs, "part11") || strings.Contains(refs, "origin=kept") {
		t.Fatalf("durable LL-HLS references leaked origin data: %q", refs)
	}
	st2 := store.New(db2)
	graph2, err := hlssession.New(st2, hlssession.Options{})
	if err != nil {
		t.Fatalf("hlssession.New after restart: %v", err)
	}
	s2 := httpTestServer("0123456789abcdef0123456789abcdef", true)
	s2.store = st2
	s2.hlsSessions = graph2
	s2.httpHandlers = httpstream.NewRegistry()
	s2.httpResolveCoord = newHTTPResolveCoordinator()
	s2.httpClientOnce.Do(func() {
		s2.httpProbeClient = origin.Client()
		s2.httpRelayClient = origin.Client()
	})
	s2.httpHandlers.Register("b1", 1, &hlsTestHandler{url: origin.URL + "/live.m3u8?origin=kept", selector: "ll-sel"})
	afterRestart := httptest.NewRecorder()
	hlsMux(s2).ServeHTTP(afterRestart, httptest.NewRequest(http.MethodGet, part.URI, nil))
	if afterRestart.Code != http.StatusOK || afterRestart.Body.String() != "part-11-0" {
		t.Fatalf("post-restart LL-HLS part status=%d body=%q", afterRestart.Code, afterRestart.Body.String())
	}
}

func TestHLSDeliveryDirectivesRejectMalformedAndUnknown(t *testing.T) {
	bad := []string{
		"_HLS_part=1",
		"_HLS_msn=-1",
		"_HLS_msn=1&_HLS_part=x",
		"_HLS_msn=1&_HLS_skip=no",
		"_HLS_msn=1&_HLS_other=2",
		"_HLS_msn=1&_HLS_msn=2",
		"_HLS_msn=9223372036854775808",
	}
	for _, raw := range bad {
		values, err := url.ParseQuery(raw)
		if err != nil {
			t.Fatalf("ParseQuery(%q): %v", raw, err)
		}
		if _, err := parseHLSDeliveryDirectives(values); err == nil {
			t.Fatalf("accepted malformed delivery directives %q", raw)
		}
	}
	values, err := url.ParseQuery("_HLS_msn=12&_HLS_part=3&_HLS_skip=v2&ignored=value")
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseHLSDeliveryDirectives(values)
	if err != nil {
		t.Fatalf("valid delivery directives: %v", err)
	}
	if got.Encode() != "_HLS_msn=12&_HLS_part=3&_HLS_skip=v2" {
		t.Fatalf("validated directives=%q", got.Encode())
	}
}

// TestHLSUnknownResourceIsNotFound covers the DG-05-style deterministic
// absent/removed-resource negative.
func TestHLSUnknownResourceIsNotFound(t *testing.T) {
	s, _ := newHLSTestServer(t)
	mux := hlsMux(s)
	req := httptest.NewRequest(http.MethodGet, "/hls/hz000000000000", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestHLSOversizedManifestIsRejected is the DG-03 negative: a "manifest"
// larger than hls.MaxPlaylistBytes must be rejected (502), never partially
// read into an unbounded buffer.
func TestHLSOversizedManifestIsRejected(t *testing.T) {
	origin := hlsFixtureOrigin()
	defer origin.Close()
	s, _ := newHLSTestServer(t)
	s.httpClientOnce.Do(func() {
		s.httpProbeClient = origin.Client()
		s.httpRelayClient = origin.Client()
	})
	item := createHLSTestItem(t, s, "f1", "master-sel", origin.URL+"/oversized.m3u8")

	_, err := s.CreateHLSPlaybackSession(context.Background(), item.ID, "f1")
	if err == nil {
		t.Fatal("expected CreateHLSPlaybackSession to fail on an oversized manifest")
	}
	if httpstream.ClassOf(err) != httpstream.ClassUpstreamMalformed {
		t.Fatalf("got error class %q, want upstream_malformed (err=%v)", httpstream.ClassOf(err), err)
	}
}

// TestHLSNonPlaylistResponseIsRejected covers a backend answering with
// something that is not recognizable HLS content at all.
func TestHLSNonPlaylistResponseIsRejected(t *testing.T) {
	origin := hlsFixtureOrigin()
	defer origin.Close()
	s, _ := newHLSTestServer(t)
	s.httpClientOnce.Do(func() {
		s.httpProbeClient = origin.Client()
		s.httpRelayClient = origin.Client()
	})
	item := createHLSTestItem(t, s, "f1", "master-sel", origin.URL+"/notaplaylist.txt")

	_, err := s.CreateHLSPlaybackSession(context.Background(), item.ID, "f1")
	if err == nil {
		t.Fatal("expected CreateHLSPlaybackSession to fail on non-playlist content")
	}
	if httpstream.ClassOf(err) != httpstream.ClassUpstreamMalformed {
		t.Fatalf("got error class %q, want upstream_malformed (err=%v)", httpstream.ClassOf(err), err)
	}
}

// TestHLSUnwiredGraphIsServiceUnavailable covers the nil-safety contract:
// an unwired hlsSessions graph must never panic.
func TestHLSUnwiredGraphIsServiceUnavailable(t *testing.T) {
	s := httpTestServer("0123456789abcdef0123456789abcdef", true)
	mux := hlsMux(s)
	req := httptest.NewRequest(http.MethodGet, "/hls/hz000000000000", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
