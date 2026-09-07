package omss

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/httpstream"
)

func newTestHandler(t *testing.T, mux http.Handler) (*Handler, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(mux)
	h := New("test", srv.URL, srv.Client(), nil)
	return h, srv
}

func movieQuery(tmdbID string) httpstream.StreamQuery {
	return httpstream.StreamQuery{Kind: "movie", TMDBID: tmdbID}
}

func TestSearchFiltersToSupportedStreamableSources(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/movies/155", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("platform") != "native" {
			t.Fatalf("expected platform=native, got %q", r.URL.Query().Get("platform"))
		}
		if r.URL.Query().Get("filter") != sourceFilter {
			t.Fatalf("expected filter pushdown, got %q", r.URL.Query().Get("filter"))
		}
		_, _ = w.Write([]byte(`{
			"id": "resp-1",
			"sources": [
				{"id":"src-hls","url":"https://u/1.m3u8","streamable":true,"type":"hls","quality":"4K","provider":{"id":"p","name":"P"}},
				{"id":"src-mp4-dl","url":"https://u/2.mp4","streamable":false,"type":"mp4","quality":"FHD","provider":{"id":"p","name":"P"}},
				{"id":"src-mkv-ok","url":"https://u/3.mkv","streamable":true,"type":"mkv","quality":"HD","provider":{"id":"p","name":"P"}},
				{"id":"src-dash","url":"https://u/4.mpd","streamable":true,"type":"dash","quality":"FHD","provider":{"id":"p","name":"P"}}
			],
			"subtitles": [],
			"diagnostics": []
		}`))
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()

	results, err := h.Search(context.Background(), movieQuery("155"))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 2 || results[0].Key.Selector != "src-hls" ||
		results[1].Key.Selector != "src-mkv-ok" {
		t.Fatalf("expected streamable HLS and MKV sources, got %+v", results)
	}
	if results[0].Key.Handler != "omss" || results[0].Key.IDs["tmdb"] != "155" {
		t.Fatalf("unexpected key: %+v", results[0].Key)
	}
}

func TestSearchFilterRejectedFallsBackClientSide(t *testing.T) {
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/movies/200", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("filter") != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"INVALID_PARAMETER","message":"bad filter"},"traceId":"t"}`))
			return
		}
		_, _ = w.Write([]byte(`{
			"id": "resp-2",
			"sources": [{"id":"src-1","url":"https://u/1.mp4","streamable":true,"type":"mp4","quality":"Auto","provider":{"id":"p","name":"P"}}],
			"subtitles": [], "diagnostics": []
		}`))
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()

	results, err := h.Search(context.Background(), movieQuery("200"))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected filtered attempt then unfiltered fallback, got %d calls", calls)
	}
	if len(results) != 1 || results[0].Quality != "" {
		// Quality "Auto" -> QualityToRes ok=false -> empty res, never guessed.
		t.Fatalf("unexpected results: %+v", results)
	}
}

func TestSearchNoSourcesAvailableIsAuthoritativeEmpty(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/movies/999999", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"NO_SOURCES_AVAILABLE","message":"none"},"traceId":"t"}`))
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()

	results, err := h.Search(context.Background(), movieQuery("999999"))
	if err != nil || results != nil {
		t.Fatalf("expected authoritative empty, got results=%v err=%v", results, err)
	}
}

func TestSearchRateLimitedIsTransientError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/movies/1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()

	_, err := h.Search(context.Background(), movieQuery("1"))
	if err == nil || !httpstream.ClassOf(err).Transient() {
		t.Fatalf("expected transient error, got %v", err)
	}
}

func TestFetchHealthDownStatusIsError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"spec":"omss","version":"1.1.0","status":"down","endpoints":{"movie":"/v1/movies/{id}","tv":"/v1/tv/{id}/seasons/{s}/episodes/{e}"},"media":{"movies":"*","tv":"*"}}`))
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()

	if _, err := h.fetchHealth(context.Background()); err == nil {
		t.Fatal("expected status=down to be reported as an error, never authoritative")
	}
}

func TestMediaMapExplicitListCheapNegative(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"spec":"omss","version":"1.1.0","status":"operational","endpoints":{"movie":"/v1/movies/{id}","tv":"/v1/tv/{id}/seasons/{s}/episodes/{e}"},"media":{"movies":[123,456],"tv":"*"}}`))
	})
	mux.HandleFunc("/v1/movies/999", func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("sources endpoint must not be called for an explicit-list negative")
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()

	results, err := h.Search(context.Background(), movieQuery("999"))
	if err != nil || results != nil {
		t.Fatalf("expected cheap-negative empty result, got results=%v err=%v", results, err)
	}
}

func TestMediaMapWildcardNeverBlocksSearch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"spec":"omss","version":"1.1.0","status":"operational","endpoints":{"movie":"/v1/movies/{id}","tv":"/v1/tv/{id}/seasons/{s}/episodes/{e}"},"media":{"movies":"*","tv":"*"}}`))
	})
	called := false
	mux.HandleFunc("/v1/movies/155", func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{"id":"resp-x","sources":[],"subtitles":[],"diagnostics":[]}`))
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()

	if _, err := h.Search(context.Background(), movieQuery("155")); err != nil {
		t.Fatalf("search: %v", err)
	}
	if !called {
		t.Fatal("wildcard media map must not short-circuit the sources fetch")
	}
}

func TestResolveHonorsExpiresAtAndHeaders(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/movies/155", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"id": "resp-3",
			"expiresAt": "2026-07-13T23:00:00Z",
			"sources": [{"id":"src-1","url":"https://u/1.mp4","headers":{"Referer":"https://u/watch"},"streamable":true,"type":"mp4","quality":"FHD","provider":{"id":"p","name":"P"}}],
			"subtitles": [], "diagnostics": []
		}`))
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()

	key := httpstream.ResolveKey{
		Version: httpstream.ResolveKeyVersion, BackendID: "test", Handler: "omss",
		Kind: "movie", IDs: map[string]string{"tmdb": "155"}, Selector: "src-1",
	}
	files, err := h.Resolve(context.Background(), httpstream.ResolveRequest{Key: key, Operation: httpstream.ResolveNormal})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	f := files[0]
	if f.ExpiresAt.IsZero() {
		t.Fatal("expected expiresAt to be parsed")
	}
	if f.RequestHeaders["Referer"] != "https://u/watch" {
		t.Fatalf("expected native headers carried through, got %+v", f.RequestHeaders)
	}
	if f.RefreshContext != "resp-3" {
		t.Fatalf("expected refresh context = top-level response id, got %q", f.RefreshContext)
	}
}

func TestResolveCarriesHLSMetadata(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/movies/155", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"id":"resp-hls",
			"expiresAt":"2030-01-01T00:00:00Z",
			"sources":[{"id":"src-hls","url":"https://media.invalid/master.m3u8","headers":{"Referer":"https://media.invalid/watch"},"streamable":true,"type":"hls","quality":"FHD","audioTracks":["English"],"provider":{"id":"provider","name":"Provider"}}],
			"subtitles":[],"diagnostics":[]
		}`))
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()

	key := httpstream.ResolveKey{Version: httpstream.ResolveKeyVersion, BackendID: "test", Handler: "omss", Kind: "movie", IDs: map[string]string{"tmdb": "155"}, Selector: "src-hls"}
	files, err := h.Resolve(context.Background(), httpstream.ResolveRequest{Key: key})
	if err != nil || len(files) != 1 {
		t.Fatalf("resolve HLS: files=%v err=%v", files, err)
	}
	got := files[0]
	if got.Name != "omss.tmdb155.m3u8" || got.ContentType != "application/vnd.apple.mpegurl" ||
		got.RefreshContext != "resp-hls" || got.ExpiresAt.IsZero() || got.RequestHeaders["Referer"] == "" {
		t.Fatalf("HLS metadata not preserved: %+v", got)
	}
}

func TestSourceKindFailsClosed(t *testing.T) {
	truth := true
	falsehood := false
	cases := []struct {
		src  sourceObject
		want httpstream.StreamKind
	}{
		{sourceObject{Type: "hls", Streamable: &truth}, httpstream.KindHLS},
		{sourceObject{Type: "mkv", Streamable: &truth}, httpstream.KindProgressiveHTTP},
		{sourceObject{Type: "dash", Streamable: &truth}, httpstream.KindUnsupported},
		{sourceObject{Type: "hls", Streamable: &falsehood}, httpstream.KindUnsupported},
		{sourceObject{Type: ""}, httpstream.KindUnsupported},
	}
	for _, tc := range cases {
		if got := sourceKind(tc.src); got != tc.want {
			t.Fatalf("sourceKind(%q)=%q want %q", tc.src.Type, got, tc.want)
		}
	}
}

func TestResolveForceRefreshIsBestEffortAndNonFatal(t *testing.T) {
	var refreshPath string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/refresh/", func(w http.ResponseWriter, r *http.Request) {
		refreshPath = r.URL.Path
		w.WriteHeader(http.StatusNotFound) // RESPONSE_ID_NOT_FOUND — must not fail Resolve.
		_, _ = w.Write([]byte(`{"error":{"code":"RESPONSE_ID_NOT_FOUND","message":"unknown"},"traceId":"t"}`))
	})
	mux.HandleFunc("/v1/movies/155", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"id": "resp-4",
			"sources": [{"id":"src-1","url":"https://u/1.mp4","streamable":true,"type":"mp4","quality":"FHD","provider":{"id":"p","name":"P"}}],
			"subtitles": [], "diagnostics": []
		}`))
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()

	key := httpstream.ResolveKey{
		Version: httpstream.ResolveKeyVersion, BackendID: "test", Handler: "omss",
		Kind: "movie", IDs: map[string]string{"tmdb": "155"}, Selector: "src-1",
	}
	files, err := h.Resolve(context.Background(), httpstream.ResolveRequest{
		Key: key, Operation: httpstream.ResolveForceRefresh, RefreshContext: "stale-resp-id",
	})
	if err != nil {
		t.Fatalf("forced refresh must never fail resolve on a rejected refresh: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected the plain re-GET to still succeed, got %d files", len(files))
	}
	if refreshPath != "/v1/refresh/stale-resp-id" {
		t.Fatalf("unexpected refresh path %q", refreshPath)
	}
}

func TestSearchMovieWithoutTMDBIDSkipsHandler(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("no backend call expected when the query carries no TMDB id")
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()

	results, err := h.Search(context.Background(), httpstream.StreamQuery{Kind: "movie"})
	if err != nil || results != nil {
		t.Fatalf("expected a silent skip, got results=%v err=%v", results, err)
	}
}

func TestSearchEpisodeWithoutMapperSkipsHandler(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("no backend call expected when no IDMapper is configured")
	})
	h, srv := newTestHandler(t, mux) // mapper is nil
	defer srv.Close()

	results, err := h.Search(context.Background(), httpstream.StreamQuery{
		Kind: "episode", TVDBID: "777", Season: 1, Episode: 1,
	})
	if err != nil || results != nil {
		t.Fatalf("expected a silent skip, got results=%v err=%v", results, err)
	}
}

func TestSearchEpisodeMissingEpisodeNumberSkips(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("season-only queries must never reach the backend (D12)")
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()

	results, err := h.Search(context.Background(), httpstream.StreamQuery{
		Kind: "episode", TMDBID: "1396", Season: 1, Episode: 0,
	})
	if err != nil || results != nil {
		t.Fatalf("expected a silent skip, got results=%v err=%v", results, err)
	}
}

func TestReleaseNameFallsBackToTMDBStubWhenTitleAbsent(t *testing.T) {
	name := releaseName(httpstream.StreamQuery{Season: 1, Episode: 2}, "1396", true, "1080p")
	if name != "TMDB.1396.S01E02.1080p.WEBDL" {
		t.Fatalf("unexpected release name: %q", name)
	}
}

func TestFileNameHasProtocolCorrectExtension(t *testing.T) {
	if got := fileName("155", false, 0, 0, "mkv"); got != "omss.tmdb155.mkv" {
		t.Fatalf("unexpected file name: %q", got)
	}
	if got := fileName("1396", true, 1, 2, "mp4"); got != "omss.tmdb1396.s01e02.mp4" {
		t.Fatalf("unexpected file name: %q", got)
	}
	if got := fileName("155", false, 0, 0, "hls"); got != "omss.tmdb155.m3u8" {
		t.Fatalf("unexpected HLS file name: %q", got)
	}
}
