package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/output"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

var testStremioInstallToken = strings.Repeat("i", 32)

func stremioAddonTestServer(t *testing.T, committed bool, streamURL string) *Server {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "stremio.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	st := store.New(db)
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	item := &store.Item{
		ID: "item", PublicID: "item", SourceType: store.SourceTypeHTTP, ClientKind: store.ClientKindQBit,
		Category: "movies", State: store.StateReady, SubmissionKey: "item", DisplayName: "Movie",
		Metadata: store.SubmissionMetadata{ProviderIdentity: &store.ProviderIdentity{
			Kind: "movie", Title: "Movie", IDs: store.ProviderIDs{IMDB: "tt0111161"},
		}}, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.CreateItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertStrmBlobAt(ctx, item.ID, 0, "item/movie.strm", streamURL); err != nil {
		t.Fatal(err)
	}
	if committed {
		record, _, err := st.BeginReactiveCommit(ctx, "rep", item.ID, "0", "supervised", now)
		if err != nil {
			t.Fatal(err)
		}
		record.ArrName, record.ArrKind, record.ArrItemID, record.ArrFileIDs = "radarr", "movie", 1, []int{1}
		if err := st.CompleteReactiveCommit(ctx, record, now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{}
	cfg.Server.BaseURL = "http://dh.invalid"
	cfg.Stremio.EdgeMode = config.StremioEdgeCustom
	cfg.Stremio.ClientAddress = ":8382"
	cfg.Stremio.ClientBaseURL = "https://client.invalid"
	cfg.Stremio.InstallToken = testStremioInstallToken
	return &Server{cfg: cfg, store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestStremioManifestAndCommittedStream(t *testing.T) {
	s := stremioAddonTestServer(t, true, "http://dh.invalid/stream/item/0")
	manifest := httptest.NewRecorder()
	s.ClientRouter().ServeHTTP(manifest, httptest.NewRequest(http.MethodGet, "/stremio/"+testStremioInstallToken+"/manifest.json", nil))
	if manifest.Code != http.StatusOK || manifest.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("manifest status=%d type=%q", manifest.Code, manifest.Header().Get("Content-Type"))
	}
	if manifest.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("manifest CORS = %q", manifest.Header().Get("Access-Control-Allow-Origin"))
	}
	var parsed stremioManifest
	if err := json.Unmarshal(manifest.Body.Bytes(), &parsed); err != nil || len(parsed.Resources) != 1 || len(parsed.Catalogs) != 0 {
		t.Fatalf("manifest=%+v err=%v", parsed, err)
	}
	if parsed.Version != "1.2.4" || strings.Contains(strings.ToLower(parsed.Description), "committed") {
		t.Fatalf("stale manifest=%+v", parsed)
	}

	response := httptest.NewRecorder()
	s.ClientRouter().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/stremio/"+testStremioInstallToken+"/stream/movie/tt0111161.json", nil))
	var streams stremioStreamResponse
	if err := json.Unmarshal(response.Body.Bytes(), &streams); err != nil || response.Code != http.StatusOK || len(streams.Streams) != 1 {
		t.Fatalf("status=%d streams=%+v err=%v", response.Code, streams, err)
	}
	if streams.Streams[0].URL != "https://client.invalid/stream/item/0" || streams.Streams[0].Title != "Movie" {
		t.Fatalf("unexpected stream: %+v", streams.Streams[0])
	}
}

func TestStremioReadyUncommittedStream(t *testing.T) {
	s := stremioAddonTestServer(t, false, "http://dh.invalid/stream/item/0")
	rec := httptest.NewRecorder()
	s.ClientRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stremio/"+testStremioInstallToken+"/stream/movie/tt0111161.json", nil))
	var response stremioStreamResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || rec.Code != http.StatusOK || len(response.Streams) != 1 {
		t.Fatalf("status=%d response=%+v err=%v", rec.Code, response, err)
	}
}

func TestStremioStreamFailsClosed(t *testing.T) {
	tests := []struct {
		name      string
		committed bool
		url       string
		path      string
	}{
		{"wrong-origin", true, "https://upstream.invalid/video", "/stream/movie/tt0111161.json"},
		{"wrong-route", true, "http://dh.invalid/proxy/stream", "/stream/movie/tt0111161.json"},
		{"wrong-item", true, "http://dh.invalid/stream/another/0", "/stream/movie/tt0111161.json"},
		{"malformed", true, "http://dh.invalid/stream/item/0", "/stream/series/not-an-id.json"},
		{"unsupported", true, "http://dh.invalid/stream/item/0", "/stream/channel/tt0111161.json"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := stremioAddonTestServer(t, tc.committed, tc.url)
			rec := httptest.NewRecorder()
			path := "/stremio/" + testStremioInstallToken + tc.path
			s.ClientRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			var response stremioStreamResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || rec.Code != http.StatusOK || len(response.Streams) != 0 {
				t.Fatalf("status=%d response=%+v err=%v", rec.Code, response, err)
			}
		})
	}
}

func TestStremioLibraryEpisodeMatchingFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		row  store.StremioLibraryStream
		want bool
	}{
		{"one-to-one", store.StremioLibraryStream{RelPath: "show/video.strm", BlobCount: 1, EpisodeCount: 1}, true},
		{"multi-exact", store.StremioLibraryStream{RelPath: "show/Show.S05E14.strm", BlobCount: 2, EpisodeCount: 2}, true},
		{"multi-wrong-episode", store.StremioLibraryStream{RelPath: "show/Show.S05E15.strm", BlobCount: 2, EpisodeCount: 2}, false},
		{"multi-ambiguous-name", store.StremioLibraryStream{RelPath: "show/video.strm", BlobCount: 2, EpisodeCount: 2}, false},
		{"missing-cardinality", store.StremioLibraryStream{RelPath: "show/Show.S05E14.strm"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := stremioLibraryEpisodeMatches(tc.row, 5, 14); got != tc.want {
				t.Fatalf("match=%v want=%v row=%+v", got, tc.want, tc.row)
			}
		})
	}
}

func TestStremioStreamConcurrentReads(t *testing.T) {
	s := stremioAddonTestServer(t, true, "http://dh.invalid/stream/item/0")
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			s.ClientRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stremio/"+testStremioInstallToken+"/stream/movie/tt0111161.json", nil))
			if rec.Code != http.StatusOK {
				t.Errorf("status=%d", rec.Code)
			}
		}()
	}
	wg.Wait()
}

func TestStremioRoutesRejectWritesAndMalformedIDs(t *testing.T) {
	s := stremioAddonTestServer(t, true, "http://dh.invalid/stream/item/0")
	post := httptest.NewRecorder()
	s.ClientRouter().ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/stremio/"+testStremioInstallToken+"/manifest.json", nil))
	if post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST manifest status=%d", post.Code)
	}
	for _, path := range []string{
		"/stremio/" + testStremioInstallToken + "/stream/movie/tt0111161",
		"/stremio/" + testStremioInstallToken + "/stream/movie/tt0111161:1:1.json",
		"/stremio/" + testStremioInstallToken + "/stream/series/tt0903747:-1:1.json",
		"/stremio/" + testStremioInstallToken + "/stream/series/tt0903747:1:0.json",
		"/stremio/" + testStremioInstallToken + "/stream/series/tt0903747:1:1.json/extra",
	} {
		rec := httptest.NewRecorder()
		s.ClientRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK && rec.Code != http.StatusNotFound {
			t.Fatalf("path=%q status=%d", path, rec.Code)
		}
		if rec.Code == http.StatusOK {
			var response stremioStreamResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || len(response.Streams) != 0 {
				t.Fatalf("path=%q response=%+v err=%v", path, response, err)
			}
		}
	}
}

func TestStremioClientRouterExcludesPrimaryAPIsAndRejectsWrongToken(t *testing.T) {
	s := stremioAddonTestServer(t, true, "http://dh.invalid/stream/item/0")
	for _, path := range []string{"/metrics", "/api/v2/auth/login", "/sabnzbd/api", "/dav/", "/stremio/wrong-token/manifest.json"} {
		rec := httptest.NewRecorder()
		s.ClientRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("path=%q status=%d cors=%q", path, rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
		}
	}
	primary := httptest.NewRecorder()
	s.Router().ServeHTTP(primary, httptest.NewRequest(http.MethodGet, "/stremio/"+testStremioInstallToken+"/manifest.json", nil))
	if primary.Code != http.StatusNotFound {
		t.Fatalf("primary router exposed Stremio addon: status=%d", primary.Code)
	}
}

func TestStremioClientRewritePreservesVerifiedSignedPath(t *testing.T) {
	secret := strings.Repeat("s", 32)
	token := output.GenerateStreamToken(secret, "item", "0")
	internal := fmt.Sprintf("http://dh.invalid/stream/item/0?tok=%s", token)
	if !validStremioLibraryURL(internal, "http://dh.invalid", secret, "item", "0") {
		t.Fatal("signed internal stream did not validate")
	}
	got, ok := rewriteStremioClientURL(internal, "https://client.invalid")
	if !ok {
		t.Fatal("verified stream did not rewrite")
	}
	u, err := url.Parse(got)
	if err != nil || u.Scheme != "https" || u.Host != "client.invalid" || u.Path != "/stream/item/0" || u.Query().Get("tok") != token {
		t.Fatal("rewrite changed more than the origin")
	}
}

func TestStremioClientConcurrencyFailsFast(t *testing.T) {
	const limit = 4
	started := make(chan struct{}, limit)
	release := make(chan struct{})
	h := limitStremioClientConcurrency(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
		w.WriteHeader(http.StatusNoContent)
	}), limit)
	var wg sync.WaitGroup
	for range limit {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		}()
	}
	for range limit {
		<-started
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("overflow status=%d retry=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
	close(release)
	wg.Wait()
}

func FuzzParseStremioVideoID(f *testing.F) {
	f.Add("movie", "tt0111161.json")
	f.Add("series", "tt0903747:5:14.json")
	f.Add("series", "tt1:0:1.json")
	f.Fuzz(func(t *testing.T, kind, id string) {
		identity, err := parseStremioVideoID(kind, id)
		if err == nil {
			if identity.kind != kind || identity.imdbID == "" {
				t.Fatal("successful parse returned incomplete identity")
			}
		}
	})
}

func FuzzRewriteStremioClientURL(f *testing.F) {
	f.Add("http://dh.invalid/stream/item/0", "https://client.invalid")
	f.Add("https://upstream.invalid/video", "https://client.invalid/path")
	f.Fuzz(func(t *testing.T, streamURL, clientBase string) {
		got, ok := rewriteStremioClientURL(streamURL, clientBase)
		if ok {
			u, err := url.Parse(got)
			base, baseErr := config.ParseStremioClientBaseURL(clientBase)
			if err != nil || baseErr != nil || u.Scheme != base.Scheme || u.Host != base.Host || u.User != nil || u.Fragment != "" {
				t.Fatalf("unsafe rewrite accepted")
			}
		}
	})
}
