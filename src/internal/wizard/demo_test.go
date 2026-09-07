package wizard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/util"
)

// fakeRadarr is a minimal, fully controllable Radarr double covering exactly
// the endpoints demo.go calls: movie lookup by tmdbId, rootfolder,
// qualityprofile, add (POST), get-by-id, delete.
type fakeRadarr struct {
	mu sync.Mutex // guards movies/nextID/deletes -- the demo's own
	// goroutine and each test's background "grab completes"
	// goroutine both touch these concurrently.
	movies                 map[int]*radarrMovie
	nextID                 int
	deletes                []int
	addCalls               int32
	failAdd                bool
	malformed              bool                   // serve malformed JSON on movie list
	profiles               []radarrQualityProfile // if set, overrides the single-profile default
	lastAddedProfileID     int32
	lastGrabbedFromIndexer int32
	onGrab                 func(movieID int) // called after a simulated release grab
}

// withMovie runs fn under the fake's lock with the movie for id, if any.
// Test helper only -- keeps every background-goroutine mutation and every
// handler read on the same lock, avoiding the race the unguarded map
// produced under -race.
func (f *fakeRadarr) withMovie(id int, fn func(m *radarrMovie)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m, ok := f.movies[id]; ok {
		fn(m)
	}
}

// firstFileless returns the id of the first movie with HasFile==false, or 0.
func (f *fakeRadarr) firstFileless() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, m := range f.movies {
		if !m.HasFile {
			return id
		}
	}
	return 0
}

func (f *fakeRadarr) deleteCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.deletes)
}

func newFakeRadarr() *fakeRadarr {
	return &fakeRadarr{movies: map[int]*radarrMovie{}, nextID: 1000}
}

// seedMovie adds a movie directly under lock (test setup only).
func (f *fakeRadarr) seedMovie(m *radarrMovie) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.movies[m.ID] = m
}

func (f *fakeRadarr) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/movie", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if f.malformed {
				w.Write([]byte("{not json"))
				return
			}
			tmdbID := r.URL.Query().Get("tmdbId")
			f.mu.Lock()
			var out []radarrMovie
			for _, m := range f.movies {
				if tmdbID == "" || fmt.Sprintf("%d", m.TMDBID) == tmdbID {
					out = append(out, *m)
				}
			}
			f.mu.Unlock()
			writeJSONDemo(w, out)
		case http.MethodPost:
			atomic.AddInt32(&f.addCalls, 1)
			if f.failAdd {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`[{"errorMessage":"simulated failure"}]`))
				return
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if qp, ok := body["qualityProfileId"].(float64); ok {
				atomic.StoreInt32(&f.lastAddedProfileID, int32(qp))
			}
			f.mu.Lock()
			id := f.nextID
			f.nextID++
			m := &radarrMovie{ID: id, TMDBID: int(body["tmdbId"].(float64)), Title: body["title"].(string), Monitored: true}
			f.movies[id] = m
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			writeJSONDemo(w, m)
		}
	})
	mux.HandleFunc("/api/v3/rootfolder", func(w http.ResponseWriter, r *http.Request) {
		writeJSONDemo(w, []radarrRootFolder{{Path: "/movies"}})
	})
	mux.HandleFunc("/api/v3/qualityprofile", func(w http.ResponseWriter, r *http.Request) {
		if len(f.profiles) > 0 {
			writeJSONDemo(w, f.profiles)
			return
		}
		writeJSONDemo(w, []radarrQualityProfile{{ID: 12}})
	})
	mux.HandleFunc("/api/v3/indexer", func(w http.ResponseWriter, r *http.Request) {
		writeJSONDemo(w, []map[string]any{
			{
				"id": 24,
				"fields": []map[string]any{
					{"name": "baseUrl", "value": "http://darkharrbor:8381/torznab/http-stream"},
				},
			},
			{
				"id": 21,
				"fields": []map[string]any{
					{"name": "baseUrl", "value": "http://darkharrbor:8381/torznab/torrent-only"},
				},
			},
		})
	})
	mux.HandleFunc("/api/v3/release", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSONDemo(w, []map[string]any{
				{"indexerId": float64(21), "rejected": false, "title": "wrong-lane-release"},
				{"indexerId": float64(24), "rejected": true, "title": "rejected-http-stream-release"},
				{"indexerId": float64(24), "rejected": false, "title": "good-http-stream-release"},
			})
		case http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			atomic.StoreInt32(&f.lastGrabbedFromIndexer, int32(body["indexerId"].(float64)))
			// Simulate Radarr's own async import completing shortly after
			// grab -- the same "flip HasFile" contract the earlier tests'
			// background goroutines already model for the old
			// searchForMovie:true path, now triggered by this grab call.
			go func() {
				time.Sleep(10 * time.Millisecond)
				id := f.firstFileless()
				if id == 0 {
					return
				}
				if f.onGrab != nil {
					f.onGrab(id)
				}
			}()
			w.WriteHeader(http.StatusOK)
		}
	})
	mux.HandleFunc("/api/v3/movie/", func(w http.ResponseWriter, r *http.Request) {
		idStr := strings.TrimPrefix(r.URL.Path, "/api/v3/movie/")
		idStr = strings.Split(idStr, "?")[0]
		var id int
		fmt.Sscanf(idStr, "%d", &id)
		f.mu.Lock()
		m, ok := f.movies[id]
		var snapshot radarrMovie
		if ok {
			snapshot = *m
		}
		f.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			writeJSONDemo(w, &snapshot)
		case http.MethodDelete:
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			f.mu.Lock()
			delete(f.movies, id)
			f.deletes = append(f.deletes, id)
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}
	})
	return mux
}

func writeJSONDemo(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// newFakeStreamServer serves a deterministic small "video" with real
// HEAD/Range/206 semantics, standing in for the real DH stream endpoint.
func newFakeStreamServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "fixture.mp4", time.Time{}, &sliceReadSeeker{data: body})
	}))
}

type sliceReadSeeker struct {
	data []byte
	pos  int64
}

func (s *sliceReadSeeker) Read(p []byte) (int, error) {
	if s.pos >= int64(len(s.data)) {
		return 0, fmt.Errorf("EOF")
	}
	n := copy(p, s.data[s.pos:])
	s.pos += int64(n)
	return n, nil
}

func (s *sliceReadSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case 0:
		s.pos = offset
	case 1:
		s.pos += offset
	case 2:
		s.pos = int64(len(s.data)) + offset
	}
	return s.pos, nil
}

// writeFixtureStrm writes a .strm file at dataRoot/movies/<rel> whose sole
// content is the given URL, mirroring StrmWriter's real output shape.
func writeFixtureStrm(t *testing.T, dataRoot, rel, url string) string {
	t.Helper()
	full := filepath.Join(dataRoot, "movies", rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(url), 0o644); err != nil {
		t.Fatal(err)
	}
	return full
}

func TestDemo_PreExistingWithFile_VerifiesOnlyNoCleanup(t *testing.T) {
	fake := newFakeRadarr()
	fake.seedMovie(&radarrMovie{
		ID: 118, TMDBID: curatedDemoItems[0].TMDBID, HasFile: true,
		MovieFile: &struct {
			Path string `json:"path"`
		}{Path: "/movies/fixture.mp4"},
	})
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	body := make([]byte, 200000)
	stream := newFakeStreamServer(t, body)
	defer stream.Close()

	dataRoot := t.TempDir()
	writeFixtureStrm(t, dataRoot, "fixture.mp4", stream.URL)

	cleanupAsked := false
	res := runZeroAccountDemo(context.Background(), &discardWriter{}, demoArrTarget{Name: "radarr", URL: srv.URL, APIKey: util.Redacted("k")}, dataRoot, time.Second, func(string) bool {
		cleanupAsked = true
		return true
	})
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if !res.Verified {
		t.Fatal("expected Verified=true")
	}
	if !res.PreExisting || res.Added {
		t.Fatalf("expected PreExisting=true Added=false, got %+v", res)
	}
	if cleanupAsked {
		t.Fatal("cleanup must never be offered for pre-existing library content")
	}
	if fake.addCalls != 0 {
		t.Fatal("must never add a movie that already exists")
	}
	if len(fake.deletes) != 0 {
		t.Fatal("must never delete pre-existing content")
	}
}

func TestDemo_AddGrabVerifyCleanup_FullCycle(t *testing.T) {
	origInterval := demoPollInterval
	demoPollInterval = 20 * time.Millisecond
	defer func() { demoPollInterval = origInterval }()

	fake := newFakeRadarr()
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	body := make([]byte, 200000)
	stream := newFakeStreamServer(t, body)
	defer stream.Close()

	dataRoot := t.TempDir()

	// Simulate Radarr's own async import completing shortly after add by
	// pre-seeding the .strm the moment the movie is created: the test
	// polls the fake's movies map and flips HasFile after a short delay
	// on a background goroutine, exactly like a real grab completing.
	go func() {
		for i := 0; i < 50; i++ {
			time.Sleep(20 * time.Millisecond)
			id := fake.firstFileless()
			if id == 0 {
				continue
			}
			rel := "fixture.mp4"
			writeFixtureStrm(t, dataRoot, rel, stream.URL)
			fake.withMovie(id, func(m *radarrMovie) {
				m.HasFile = true
				m.MovieFile = &struct {
					Path string `json:"path"`
				}{Path: "/movies/" + rel}
			})
			return
		}
	}()

	cleanupAsked := false
	res := runZeroAccountDemo(context.Background(), &discardWriter{}, demoArrTarget{Name: "radarr", URL: srv.URL, APIKey: util.Redacted("k")}, dataRoot, 5*time.Second, func(string) bool {
		cleanupAsked = true
		return true
	})
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if !res.Verified || !res.Added || res.PreExisting {
		t.Fatalf("expected Added+Verified, not PreExisting: %+v", res)
	}
	if !cleanupAsked {
		t.Fatal("cleanup must be offered for a demo-added item")
	}
	if !res.CleanedUp {
		t.Fatal("expected CleanedUp=true after accepting cleanup")
	}
	if fake.deleteCount() != 1 {
		t.Fatalf("expected exactly one delete, got %d", fake.deleteCount())
	}
}

func TestDemo_TimeoutOffersCleanupForPartialAdd(t *testing.T) {
	fake := newFakeRadarr()
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	dataRoot := t.TempDir()
	cleanupAsked := false
	res := runZeroAccountDemo(context.Background(), &discardWriter{}, demoArrTarget{Name: "radarr", URL: srv.URL, APIKey: util.Redacted("k")}, dataRoot, 60*time.Millisecond, func(string) bool {
		cleanupAsked = true
		return true
	})
	if res.Err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	if !cleanupAsked {
		t.Fatal("a partially-added item (never imported) must still be offered for cleanup")
	}
	if fake.deleteCount() == 0 {
		t.Fatal("expected the partial add to be cleaned up")
	}
}

func TestDemo_MalformedResponseAbstainsAndTriesNextCandidate(t *testing.T) {
	fake := newFakeRadarr()
	fake.malformed = true
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	dataRoot := t.TempDir()
	res := runZeroAccountDemo(context.Background(), &discardWriter{}, demoArrTarget{Name: "radarr", URL: srv.URL, APIKey: util.Redacted("k")}, dataRoot, time.Second, nil)
	if res.Err == nil {
		t.Fatal("expected an abstain error when every candidate's lookup is malformed")
	}
	if !strings.Contains(res.Err.Error(), "abstaining") {
		t.Fatalf("expected an explicit abstain message, got: %v", res.Err)
	}
}

func TestDemo_ContextCancellationExitsCleanly(t *testing.T) {
	fake := newFakeRadarr()
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	dataRoot := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the demo even starts

	done := make(chan struct{})
	go func() {
		runZeroAccountDemo(ctx, &discardWriter{}, demoArrTarget{Name: "radarr", URL: srv.URL, APIKey: util.Redacted("k")}, dataRoot, time.Second, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runZeroAccountDemo did not respect an already-cancelled context")
	}
}

func TestDemo_NeverPrintsStrmURLOrToken(t *testing.T) {
	origInterval := demoPollInterval
	demoPollInterval = 20 * time.Millisecond
	defer func() { demoPollInterval = origInterval }()

	fake := newFakeRadarr()
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	body := make([]byte, 200000)
	stream := newFakeStreamServer(t, body)
	defer stream.Close()
	secretURL := stream.URL + "/stream/abc/def?tok=SUPERSECRETTOKEN"

	dataRoot := t.TempDir()
	go func() {
		for i := 0; i < 50; i++ {
			time.Sleep(20 * time.Millisecond)
			id := fake.firstFileless()
			if id == 0 {
				continue
			}
			writeFixtureStrm(t, dataRoot, "fixture.mp4", secretURL)
			fake.withMovie(id, func(m *radarrMovie) {
				m.HasFile = true
				m.MovieFile = &struct {
					Path string `json:"path"`
				}{Path: "/movies/fixture.mp4"}
			})
			return
		}
	}()

	var buf capturingWriter
	res := runZeroAccountDemo(context.Background(), &buf, demoArrTarget{Name: "radarr", URL: srv.URL, APIKey: util.Redacted("k")}, dataRoot, 5*time.Second, func(string) bool { return true })
	// This candidate's HTTP verification will fail (secretURL points at a
	// real stream server but with an unmatched path prefix is fine — the
	// fake stream server ignores path); either way, assert the captured
	// output never contains the token or "tok=".
	_ = res
	if strings.Contains(buf.String(), "SUPERSECRETTOKEN") || strings.Contains(buf.String(), "tok=") {
		t.Fatalf("output leaked the stream token: %q", buf.String())
	}
}

// discardWriter and capturingWriter avoid importing io/ioutil-style helpers.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

type capturingWriter struct{ buf []byte }

func (c *capturingWriter) Write(p []byte) (int, error) {
	c.buf = append(c.buf, p...)
	return len(p), nil
}
func (c *capturingWriter) String() string { return string(c.buf) }

func TestDemo_PicksMostPermissiveQualityProfile(t *testing.T) {
	// Regression test for the live finding (2026-07-31): picking an
	// arbitrary/first-returned profile silently zeroed every automatic
	// Radarr search when that profile happened to be a narrow, real
	// acquisition profile (e.g. 1080p-only) that allows none of a
	// public-domain fixture's actual available qualities. The demo must
	// pick the profile with the most allowed qualities.
	origInterval := demoPollInterval
	demoPollInterval = 20 * time.Millisecond
	defer func() { demoPollInterval = origInterval }()

	fake := newFakeRadarr()
	fake.profiles = []radarrQualityProfile{
		{ID: 12, Name: "HD MAX (narrow)", Items: []struct {
			Allowed bool `json:"allowed"`
		}{{Allowed: true}, {Allowed: true}}}, // 2 allowed
		{ID: 14, Name: "Best Available (broad)", Items: []struct {
			Allowed bool `json:"allowed"`
		}{{Allowed: true}, {Allowed: true}, {Allowed: true}, {Allowed: true}, {Allowed: true}}}, // 5 allowed
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	body := make([]byte, 200000)
	stream := newFakeStreamServer(t, body)
	defer stream.Close()

	dataRoot := t.TempDir()
	go func() {
		for i := 0; i < 50; i++ {
			time.Sleep(20 * time.Millisecond)
			id := fake.firstFileless()
			if id == 0 {
				continue
			}
			writeFixtureStrm(t, dataRoot, "fixture.mp4", stream.URL)
			fake.withMovie(id, func(m *radarrMovie) {
				m.HasFile = true
				m.MovieFile = &struct {
					Path string `json:"path"`
				}{Path: "/movies/fixture.mp4"}
			})
			return
		}
	}()

	res := runZeroAccountDemo(context.Background(), &discardWriter{}, demoArrTarget{Name: "radarr", URL: srv.URL, APIKey: util.Redacted("k")}, dataRoot, 5*time.Second, func(string) bool { return true })
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if got := atomic.LoadInt32(&fake.lastAddedProfileID); got != 14 {
		t.Fatalf("expected the broader profile (id=14) to be selected, got id=%d", got)
	}
}

func TestDemo_GrabsOnlyFromHTTPStreamIndexer(t *testing.T) {
	// Regression test for the live finding (2026-07-31): Radarr's own
	// automatic search scores releases across every enabled indexer and
	// picked a real Newshosting/NNTP release over the intended
	// HTTP-stream/Internet-Archive one. The demo must resolve the
	// http-stream indexer by its configured baseUrl and grab specifically
	// from it, never from another indexer even if Radarr would otherwise
	// prefer it.
	origInterval := demoPollInterval
	demoPollInterval = 20 * time.Millisecond
	defer func() { demoPollInterval = origInterval }()

	fake := newFakeRadarr()
	fake.onGrab = func(movieID int) {
		fake.withMovie(movieID, func(m *radarrMovie) {
			m.HasFile = true
			m.MovieFile = &struct {
				Path string `json:"path"`
			}{Path: "/movies/fixture.mp4"}
		})
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	body := make([]byte, 200000)
	stream := newFakeStreamServer(t, body)
	defer stream.Close()

	dataRoot := t.TempDir()
	writeFixtureStrm(t, dataRoot, "fixture.mp4", stream.URL)

	res := runZeroAccountDemo(context.Background(), &discardWriter{}, demoArrTarget{Name: "radarr", URL: srv.URL, APIKey: util.Redacted("k")}, dataRoot, 5*time.Second, func(string) bool { return true })
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if got := atomic.LoadInt32(&fake.lastGrabbedFromIndexer); got != 24 {
		t.Fatalf("expected grab from the http-stream indexer (id=24), got id=%d", got)
	}
}

func TestDemo_FallsBackToRecentStrmWhenArrPathStale(t *testing.T) {
	// Regression test for the live finding (2026-07-31): Radarr's recorded
	// movieFile.path reflects its own post-import renamed convention, but
	// the .strm DarkHarrbor actually wrote sits at the original
	// release-title-derived ingest path and is never physically moved.
	// For content the demo itself just added, it must fall back to a
	// bounded, recency-scoped scan rather than failing outright.
	origInterval := demoPollInterval
	demoPollInterval = 20 * time.Millisecond
	defer func() { demoPollInterval = origInterval }()

	fake := newFakeRadarr()
	body := make([]byte, 200000)
	stream := newFakeStreamServer(t, body)
	defer stream.Close()

	dataRoot := t.TempDir()
	fake.onGrab = func(movieID int) {
		fake.withMovie(movieID, func(m *radarrMovie) {
			m.HasFile = true
			// Deliberately a path that will NOT exist on disk -- Radarr's
			// "renamed" convention, per the live finding.
			m.MovieFile = &struct {
				Path string `json:"path"`
			}{Path: "/movies/Detour (1945)/Detour (1945) [WEBDL-480p].strm"}
		})
		// The real file DarkHarrbor actually wrote, at its own
		// release-title-derived ingest path -- matches the token/year
		// scan, not the Radarr-reported path.
		writeFixtureStrm(t, dataRoot, "Detour.1945.480p.WEBDL.DH-HTTP/Detour.strm", stream.URL)
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	res := runZeroAccountDemo(context.Background(), &discardWriter{}, demoArrTarget{Name: "radarr", URL: srv.URL, APIKey: util.Redacted("k")}, dataRoot, 5*time.Second, func(string) bool { return true })
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if !res.Verified {
		t.Fatalf("expected the fallback scan to find the real file and verify: %+v", res)
	}
}

func TestDemo_PreExistingNeverUsesFallbackScan(t *testing.T) {
	// The fuzzy recency/token fallback must never apply to pre-existing
	// library content -- too ambiguous against a real, possibly years-old
	// library. A stale reported path there must still abstain outright.
	fake := newFakeRadarr()
	fake.seedMovie(&radarrMovie{
		ID: 118, TMDBID: curatedDemoItems[0].TMDBID, HasFile: true,
		MovieFile: &struct {
			Path string `json:"path"`
		}{Path: "/movies/Detour (1945)/Detour (1945) [WEBDL-480p].strm"}, // stale, won't exist
	})
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	dataRoot := t.TempDir()
	// A file that WOULD match the fallback scan's token/year heuristic,
	// proving it is genuinely never consulted for a pre-existing item.
	writeFixtureStrm(t, dataRoot, "Detour.1945.480p.WEBDL.DH-HTTP/Detour.strm", "http://example.invalid/should-not-be-used")

	res := runZeroAccountDemo(context.Background(), &discardWriter{}, demoArrTarget{Name: "radarr", URL: srv.URL, APIKey: util.Redacted("k")}, dataRoot, time.Second, nil)
	if res.Err == nil {
		t.Fatal("expected abstention for a pre-existing item with a stale reported path")
	}
	if res.Verified {
		t.Fatal("must never verify a pre-existing item via the fuzzy fallback scan")
	}
}

func TestDemo_CleanupRemovesOrphanedStrmWhenArrDeleteFilesNoOps(t *testing.T) {
	// Regression test for the live finding (2026-07-31): Radarr's own
	// deleteFiles=true silently no-ops when its recorded movieFile.path
	// doesn't match the real file on disk (the same rename mismatch that
	// motivates the verification fallback), leaving the .strm orphaned
	// after Radarr reports the movie gone. Cleanup must remove the real
	// file too, using the same bounded, recency-scoped, added-content-only
	// heuristic.
	origInterval := demoPollInterval
	demoPollInterval = 20 * time.Millisecond
	defer func() { demoPollInterval = origInterval }()

	fake := newFakeRadarr()
	body := make([]byte, 200000)
	stream := newFakeStreamServer(t, body)
	defer stream.Close()

	dataRoot := t.TempDir()
	realStrm := writeFixtureStrm(t, dataRoot, "Detour.1945.480p.WEBDL.DH-HTTP/Detour.strm", stream.URL)
	fake.onGrab = func(movieID int) {
		fake.withMovie(movieID, func(m *radarrMovie) {
			m.HasFile = true
			// Stale, Radarr-renamed path -- never matches anything on
			// disk, exactly like the live finding. deleteMovie in the
			// fake still "succeeds" (200), mirroring Radarr's own
			// behavior of reporting success while not actually removing
			// a file it can't find at its recorded path.
			m.MovieFile = &struct {
				Path string `json:"path"`
			}{Path: "/movies/Detour (1945)/Detour (1945) [WEBDL-480p].strm"}
		})
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	res := runZeroAccountDemo(context.Background(), &discardWriter{}, demoArrTarget{Name: "radarr", URL: srv.URL, APIKey: util.Redacted("k")}, dataRoot, 5*time.Second, func(string) bool { return true })
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if !res.CleanedUp {
		t.Fatalf("expected CleanedUp=true: %+v", res)
	}
	if _, statErr := os.Stat(realStrm); statErr == nil {
		t.Fatalf("expected the orphaned .strm to be removed by supplementary cleanup, but it still exists at %s", realStrm)
	}
	if _, statErr := os.Stat(filepath.Dir(realStrm)); statErr == nil {
		t.Fatal("expected the now-empty parent directory to be pruned too")
	}
}
