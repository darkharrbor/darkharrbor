package stremio

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/httpstream"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newTestHandler(t *testing.T, mux http.Handler) (*Handler, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(mux)
	h := New("test", srv.URL, srv.Client(), nil)
	return h, srv
}

func movieQuery(imdb string) httpstream.StreamQuery {
	return httpstream.StreamQuery{Kind: "movie", IMDBID: imdb}
}

func episodeQuery(imdb string, season, ep int) httpstream.StreamQuery {
	return httpstream.StreamQuery{Kind: "episode", IMDBID: imdb, Season: season, Episode: ep}
}

func TestSearchFiltersToURLStreamsOnly(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/stream/movie/tt0111161.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"streams": [
				{"infoHash":"abc123","name":"torrent variant"},
				{"externalUrl":"https://example.com/watch","name":"external variant"},
				{"ytId":"dQw4w9WgXcQ","name":"youtube variant"},
				{"url":"https://cdn.example/movie.m3u8","name":"1080p hls (skip)"},
				{"url":"https://cdn.example/movie.mkv","name":"1080p","behaviorHints":{"filename":"Shawshank.1080p.mkv"}}
			]
		}`))
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()

	results, err := h.Search(context.Background(), movieQuery("tt0111161"))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected one torrent and one url result, got %+v", results)
	}
	if results[0].Protocol != "torrent" || results[0].InfoHash != "abc123" {
		t.Fatalf("unexpected torrent result: %+v", results[0])
	}
	if results[1].Key.Handler != "stremio" || results[1].Key.IDs["imdb"] != "tt0111161" {
		t.Fatalf("unexpected key: %+v", results[1].Key)
	}
	if results[1].Key.Selector != "file:Shawshank.1080p.mkv" {
		t.Fatalf("unexpected selector: %q", results[1].Key.Selector)
	}
	if results[1].Filename != "Shawshank.1080p.mkv" {
		t.Fatalf("unexpected transient filename: %q", results[1].Filename)
	}
	if results[1].Quality != "1080p" {
		t.Fatalf("expected quality parsed from name, got %q", results[1].Quality)
	}
}

func TestSearchNotWebReadyIsNotRejected(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/stream/movie/tt0111161.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"streams": [
				{"url":"https://cdn.example/movie.mp4","name":"720p","behaviorHints":{"notWebReady":true,"filename":"movie.mp4","proxyHeaders":{"request":{"X-Auth":"tok"}}}}
			]
		}`))
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()

	results, err := h.Search(context.Background(), movieQuery("tt0111161"))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("notWebReady must not reject an otherwise-valid url stream, got %+v", results)
	}
}

func TestSearchNoIMDBIDIsSilentSkipForMovies(t *testing.T) {
	h, srv := newTestHandler(t, http.NewServeMux())
	defer srv.Close()

	results, err := h.Search(context.Background(), httpstream.StreamQuery{Kind: "movie"})
	if err != nil {
		t.Fatalf("expected silent skip, got error: %v", err)
	}
	if results != nil {
		t.Fatalf("expected nil results, got %+v", results)
	}
}

func TestSearchEpisodeWithoutMapperIsSilentSkip(t *testing.T) {
	h, srv := newTestHandler(t, http.NewServeMux())
	defer srv.Close()

	results, err := h.Search(context.Background(), httpstream.StreamQuery{Kind: "episode", TVDBID: "12345", Season: 1, Episode: 1})
	if err != nil {
		t.Fatalf("expected silent skip (no mapper configured), got error: %v", err)
	}
	if results != nil {
		t.Fatalf("expected nil results, got %+v", results)
	}
}

func TestSearchSeasonOnlyIsSkipped(t *testing.T) {
	h, srv := newTestHandler(t, http.NewServeMux())
	defer srv.Close()

	results, err := h.Search(context.Background(), httpstream.StreamQuery{Kind: "episode", IMDBID: "tt0903747", Season: 1, Episode: 0})
	if err != nil {
		t.Fatalf("D12: season-only must be a silent skip, got error: %v", err)
	}
	if results != nil {
		t.Fatalf("D12: expected nil results for season-only query, got %+v", results)
	}
}

func TestSearchEpisodeURL(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/stream/series/tt0903747:1:1.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"streams":[{"url":"https://cdn.example/ep1.mp4","behaviorHints":{"filename":"Breaking.Bad.S01E01.mp4"}}]}`))
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()

	results, err := h.Search(context.Background(), episodeQuery("tt0903747", 1, 1))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 || results[0].Key.Season != 1 || results[0].Key.Episode != 1 {
		t.Fatalf("unexpected episode results: %+v", results)
	}
}

func TestSearch404IsNoResultNotError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/stream/movie/tt9999999.json", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()

	results, err := h.Search(context.Background(), movieQuery("tt9999999"))
	if err != nil {
		t.Fatalf("404 must be a no-result, not an error: %v", err)
	}
	if results != nil {
		t.Fatalf("expected nil results, got %+v", results)
	}
}

func TestSearchEmptyStreamsIsNoResult(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/stream/movie/tt0111161.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"streams":[]}`))
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()

	results, err := h.Search(context.Background(), movieQuery("tt0111161"))
	if err != nil {
		t.Fatalf("empty streams must be an authoritative no-result: %v", err)
	}
	if results != nil {
		t.Fatalf("expected nil results, got %+v", results)
	}
}

func TestSearch5xxIsTransient(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/stream/movie/tt0111161.json", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()

	_, err := h.Search(context.Background(), movieQuery("tt0111161"))
	if httpstream.ClassOf(err) != httpstream.ClassBackendUnavailable {
		t.Fatalf("expected ClassBackendUnavailable, got %v (%v)", httpstream.ClassOf(err), err)
	}
}

func TestSearchPreservesTransportCauseWithoutLeakingRequestURL(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})}
	h := New("test", "https://user:secret@example.invalid?token=leak", client, nil)
	_, err := h.Search(context.Background(), movieQuery("tt0111161"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("transport deadline was discarded: %v", err)
	}
	for _, leak := range []string{"secret", "token=leak", "example.invalid", "https://"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("transport error leaked %q: %v", leak, err)
		}
	}
}

func TestSearch429IsTransient(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/stream/movie/tt0111161.json", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()

	_, err := h.Search(context.Background(), movieQuery("tt0111161"))
	if httpstream.ClassOf(err) != httpstream.ClassRateLimited {
		t.Fatalf("expected ClassRateLimited, got %v (%v)", httpstream.ClassOf(err), err)
	}
}

func TestSearchMalformedJSONIsUpstreamMalformed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/stream/movie/tt0111161.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()

	_, err := h.Search(context.Background(), movieQuery("tt0111161"))
	if httpstream.ClassOf(err) != httpstream.ClassUpstreamMalformed {
		t.Fatalf("expected ClassUpstreamMalformed, got %v (%v)", httpstream.ClassOf(err), err)
	}
}

func TestStreamWithNoStableIdentityIsDropped(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/stream/movie/tt0111161.json", func(w http.ResponseWriter, r *http.Request) {
		// No filename, no name, no title, no description: nothing to key a
		// D10 selector on.
		_, _ = w.Write([]byte(`{"streams":[{"url":"https://cdn.example/x.mp4"}]}`))
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()

	results, err := h.Search(context.Background(), movieQuery("tt0111161"))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if results != nil {
		t.Fatalf("expected the identity-less stream to be dropped, got %+v", results)
	}
}

func TestResolveReturnsProxyHeadersTransiently(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/stream/movie/tt0111161.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"streams": [{
				"url":"https://cdn.example/movie.mkv",
				"behaviorHints":{
					"filename":"Shawshank.1080p.mkv",
					"proxyHeaders":{"request":{"X-Auth":"secret-token"},"response":{"Content-Disposition":"inline"}}
				}
			}]
		}`))
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()

	key := httpstream.ResolveKey{
		Version: httpstream.ResolveKeyVersion, BackendID: "test", Handler: "stremio",
		Kind: "movie", IDs: map[string]string{"imdb": "tt0111161"}, Selector: "file:Shawshank.1080p.mkv",
	}
	files, err := h.Resolve(context.Background(), httpstream.ResolveRequest{Key: key})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	matched, err := httpstream.MatchSelector(files, key.Selector)
	if err != nil {
		t.Fatalf("match selector: %v", err)
	}
	if matched.RequestHeaders["X-Auth"] != "secret-token" {
		t.Fatalf("expected proxyHeaders.request carried through, got %+v", matched.RequestHeaders)
	}
	if matched.ResponseHeaders["Content-Disposition"] != "inline" {
		t.Fatalf("expected proxyHeaders.response carried through, got %+v", matched.ResponseHeaders)
	}
	if matched.URL != "https://cdn.example/movie.mkv" {
		t.Fatalf("unexpected url: %q", matched.URL)
	}
}

func TestResolveRepresentationLostWhenSelectorMissing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/stream/movie/tt0111161.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"streams":[{"url":"https://cdn.example/other.mkv","behaviorHints":{"filename":"other.mkv"}}]}`))
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()

	key := httpstream.ResolveKey{
		Version: httpstream.ResolveKeyVersion, BackendID: "test", Handler: "stremio",
		Kind: "movie", IDs: map[string]string{"imdb": "tt0111161"}, Selector: "file:Shawshank.1080p.mkv",
	}
	files, err := h.Resolve(context.Background(), httpstream.ResolveRequest{Key: key})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := httpstream.MatchSelector(files, key.Selector); httpstream.ClassOf(err) != httpstream.ClassRepresentationLost {
		t.Fatalf("expected ClassRepresentationLost, got %v", err)
	}
}

func TestAcceptableStreamRejectsNonURLVariants(t *testing.T) {
	cases := []struct {
		name string
		s    streamObject
		want bool
	}{
		{"plain url", streamObject{URL: "https://cdn.example/a.mp4"}, true},
		{"infoHash", streamObject{URL: "https://cdn.example/a.mp4", InfoHash: "abc"}, false},
		{"externalUrl", streamObject{URL: "https://cdn.example/a.mp4", ExternalURL: "https://x/y"}, false},
		{"ytId", streamObject{URL: "https://cdn.example/a.mp4", YtID: "abc"}, false},
		{"hls path", streamObject{URL: "https://cdn.example/a.m3u8"}, false},
		{"hls query", streamObject{URL: "https://cdn.example/a?x=1&f=b.m3u8"}, false},
		{"no url", streamObject{}, false},
		{"userinfo url", streamObject{URL: "https://user:pass@cdn.example/a.mp4"}, false},
		{"non-http scheme", streamObject{URL: "ftp://cdn.example/a.mp4"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := acceptableStream(c.s); got != c.want {
				t.Errorf("acceptableStream(%+v) = %v, want %v", c.s, got, c.want)
			}
		})
	}
}

func TestQualityFromStreamExtractsToken(t *testing.T) {
	s := streamObject{Name: "🇺🇸 4K HDR", Title: "Some Release Group"}
	if got := qualityFromStream(s); got != "4K" {
		t.Fatalf("expected 4K token, got %q", got)
	}
	if got := qualityFromStream(streamObject{Name: "no quality info here"}); got != "" {
		t.Fatalf("expected no token, got %q", got)
	}
}

func TestStreamSelectorDeterministic(t *testing.T) {
	s := streamObject{
		Name:  "1080p",
		Title: "Release",
		BehaviorHints: behaviorHints{
			BingeGroup: "group-1",
			VideoSize:  jsonRaw(`1500000000`),
		},
	}
	a := streamSelector(s)
	b := streamSelector(s)
	if a != b || a == "" {
		t.Fatalf("expected deterministic non-empty selector, got %q and %q", a, b)
	}
}

func jsonRaw(s string) []byte { return []byte(s) }

// TestClassifyStreamDispatchKinds is the HR2.1 acceptance table: every
// documented Stremio stream-object variant named by HR-D2 classifies to its
// own distinct httpstream.StreamKind, never silently merged into a single
// drop bucket.
func TestClassifyStreamDispatchKinds(t *testing.T) {
	cases := []struct {
		name string
		s    streamObject
		want httpstream.StreamKind
	}{
		{"progressive http", streamObject{URL: "https://cdn.example/a.mp4"}, httpstream.KindProgressiveHTTP},
		{"progressive https", streamObject{URL: "https://cdn.example/a.mkv"}, httpstream.KindProgressiveHTTP},
		{"hls path", streamObject{URL: "https://cdn.example/a.m3u8"}, httpstream.KindHLS},
		{"hls query", streamObject{URL: "https://cdn.example/a?x=1&f=b.m3u8"}, httpstream.KindHLS},
		{"infoHash", streamObject{InfoHash: "abc123"}, httpstream.KindInfoHash},
		{"infoHash wins over url", streamObject{URL: "https://cdn.example/a.mp4", InfoHash: "abc123"}, httpstream.KindInfoHash},
		{"nzbUrl", streamObject{NZBURL: "https://indexer.example/a.nzb"}, httpstream.KindNZB},
		{"rarUrls", streamObject{RarURLs: []string{"https://cdn.example/a.rar"}}, httpstream.KindArchive},
		{"zipUrls", streamObject{ZipURLs: []string{"https://cdn.example/a.zip"}}, httpstream.KindArchive},
		{"7zipUrls", streamObject{SevenZipURLs: []string{"https://cdn.example/a.7z"}}, httpstream.KindArchive},
		{"tgzUrls", streamObject{TgzURLs: []string{"https://cdn.example/a.tgz"}}, httpstream.KindArchive},
		{"tarUrls", streamObject{TarURLs: []string{"https://cdn.example/a.tar"}}, httpstream.KindArchive},
		{"externalUrl", streamObject{ExternalURL: "https://example.com/watch"}, httpstream.KindUnsupported},
		{"externalUrl wins over url", streamObject{URL: "https://cdn.example/a.mp4", ExternalURL: "https://x/y"}, httpstream.KindUnsupported},
		{"ytId", streamObject{YtID: "dQw4w9WgXcQ"}, httpstream.KindUnsupported},
		{"empty", streamObject{}, httpstream.KindUnsupported},
		{"userinfo url", streamObject{URL: "https://user:pass@cdn.example/a.mp4"}, httpstream.KindUnsupported},
		{"non-http scheme", streamObject{URL: "ftp://cdn.example/a.mp4"}, httpstream.KindUnsupported},
		{"subtitles hint does not change classification", streamObject{
			URL:       "https://cdn.example/a.mp4",
			Subtitles: []subtitleRef{{URL: "https://cdn.example/a.srt", Lang: "eng"}},
		}, httpstream.KindProgressiveHTTP},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyStream(c.s); got != c.want {
				t.Errorf("classifyStream(%+v) = %v, want %v", c.s, got, c.want)
			}
		})
	}
}

// TestSearchDispatchesOwnedKinds proves the existing classifier routes its
// infoHash and archive arms while unowned NZB/HLS kinds remain abstentions.
func TestSearchDispatchesOwnedKinds(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/stream/movie/tt0111161.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"streams": [
				{"infoHash":"abc123","name":"torrent variant"},
				{"nzbUrl":"https://indexer.example/a.nzb","name":"nzb variant"},
				{"rarUrls":["https://cdn.example/a.rar"],"name":"archive variant"},
				{"url":"https://cdn.example/movie.m3u8","name":"hls variant"},
				{"url":"https://cdn.example/movie.mkv","name":"progressive","behaviorHints":{"filename":"Shawshank.1080p.mkv"}}
			]
		}`))
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()

	results, err := h.Search(context.Background(), movieQuery("tt0111161"))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected torrent, archive, and progressive results, got %+v", results)
	}
	if results[0].Protocol != "torrent" || results[0].InfoHash != "abc123" {
		t.Fatalf("unexpected torrent result: %+v", results[0])
	}
	if !httpstream.IsRemoteArchiveSelector(results[1].Key.Selector) {
		t.Fatalf("unexpected archive selector: %q", results[1].Key.Selector)
	}
	if results[2].Key.Selector != "file:Shawshank.1080p.mkv" {
		t.Fatalf("unexpected progressive selector: %q", results[2].Key.Selector)
	}
}

// TestSearchCachesBackendResultsBriefly proves the HR2.1 polite-
// revalidation cache: two searches for the exact same media identity
// within the TTL window share one backend fetch; a search after the TTL
// (simulated via the injectable clock) re-fetches.
func TestSearchCachesBackendResultsBriefly(t *testing.T) {
	var calls int
	mux := http.NewServeMux()
	mux.HandleFunc("/stream/movie/tt0111161.json", func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"streams":[{"url":"https://cdn.example/a.mkv","behaviorHints":{"filename":"a.mkv"}}]}`))
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()

	fakeNow := time.Now()
	h.now = func() time.Time { return fakeNow }

	if _, err := h.Search(context.Background(), movieQuery("tt0111161")); err != nil {
		t.Fatalf("first search: %v", err)
	}
	if _, err := h.Search(context.Background(), movieQuery("tt0111161")); err != nil {
		t.Fatalf("second search: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected the second search within TTL to be a cache hit (1 backend call), got %d", calls)
	}

	fakeNow = fakeNow.Add(searchResultCacheTTL + time.Second)
	if _, err := h.Search(context.Background(), movieQuery("tt0111161")); err != nil {
		t.Fatalf("third search: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected a fresh backend call after TTL expiry, got %d calls", calls)
	}
}

// TestSearchCacheIsKeyedPerMediaIdentity proves a different episode key on
// the same backend never returns another key's cached streams.
func TestSearchCacheIsKeyedPerMediaIdentity(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/stream/series/tt0903747:1:1.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"streams":[{"url":"https://cdn.example/ep1.mp4","behaviorHints":{"filename":"ep1.mp4"}}]}`))
	})
	mux.HandleFunc("/stream/series/tt0903747:1:2.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"streams":[{"url":"https://cdn.example/ep2.mp4","behaviorHints":{"filename":"ep2.mp4"}}]}`))
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()

	r1, err := h.Search(context.Background(), episodeQuery("tt0903747", 1, 1))
	if err != nil {
		t.Fatalf("ep1 search: %v", err)
	}
	r2, err := h.Search(context.Background(), episodeQuery("tt0903747", 1, 2))
	if err != nil {
		t.Fatalf("ep2 search: %v", err)
	}
	if r1[0].Key.Selector == r2[0].Key.Selector {
		t.Fatalf("expected distinct selectors for distinct episodes, got %q for both", r1[0].Key.Selector)
	}
}

// TestSearchCacheDoesNotCacheBackendErrors proves a transient backend
// failure is never cached over — a subsequent search must retry the
// backend, not replay the failure or an empty result.
func TestSearchCacheDoesNotCacheBackendErrors(t *testing.T) {
	var calls int
	mux := http.NewServeMux()
	mux.HandleFunc("/stream/movie/tt0111161.json", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"streams":[{"url":"https://cdn.example/a.mkv","behaviorHints":{"filename":"a.mkv"}}]}`))
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()

	if _, err := h.Search(context.Background(), movieQuery("tt0111161")); httpstream.ClassOf(err) != httpstream.ClassBackendUnavailable {
		t.Fatalf("expected first search to surface the transient failure, got %v", err)
	}
	results, err := h.Search(context.Background(), movieQuery("tt0111161"))
	if err != nil {
		t.Fatalf("second search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected the retried search to reach the backend and succeed, got %+v (calls=%d)", results, calls)
	}
	if calls != 2 {
		t.Fatalf("expected 2 backend calls (error never cached), got %d", calls)
	}
}

// TestStreamNameHintPrefersFilename covers the HR0.2 live-gate finding
// (2026-07-18): a real stream's own filename, extension stripped, is used
// as human-readable release text rather than the bare IMDB.<id> placeholder.
func TestStreamNameHintPrefersFilename(t *testing.T) {
	s := streamObject{
		Name:  "AIOStreams 2160p",
		Title: "ignored when filename present",
		BehaviorHints: behaviorHints{
			Filename: "Yellowstone.2018.S05E09.MULTI.SDR.2160p.WEB.H265-FW.mkv",
		},
	}
	got := streamNameHint(s)
	want := "Yellowstone.2018.S05E09.MULTI.SDR.2160p.WEB.H265.FW"
	if got != want {
		t.Fatalf("streamNameHint = %q, want %q", got, want)
	}
}

func TestStreamNameHintFallsBackToNameTitle(t *testing.T) {
	s := streamObject{Name: "AIOStreams", Title: "Some Release Text"}
	got := streamNameHint(s)
	if got == "" || strings.Contains(got, "..") {
		t.Fatalf("expected non-empty path-safe hint from name+title, got %q", got)
	}
}

func TestStreamNameHintEmptyWhenNoText(t *testing.T) {
	if got := streamNameHint(streamObject{}); got != "" {
		t.Fatalf("expected empty hint with no filename/name/title, got %q", got)
	}
}

// TestReleaseNamePrefersQueryTitle preserves the pre-existing behavior:
// when the arr's own free-text q= is present, it wins over any stream hint,
// and still gets the synthesized SxxEyy/res/WEBDL suffix.
func TestReleaseNamePrefersQueryTitle(t *testing.T) {
	q := httpstream.StreamQuery{Title: "Breaking Bad", Season: 1, Episode: 1}
	got := releaseName(q, "tt0903747", true, "1080p", "Some.Real.Filename")
	want := "Breaking.Bad.S01E01.1080p.WEBDL"
	if got != want {
		t.Fatalf("releaseName = %q, want %q", got, want)
	}
}

// TestReleaseNameUsesStreamHintWhenQueryTitleAbsent is the direct regression
// for the HR0.2 finding: an ID-only search (tvdbid+season+ep, no q=, exactly
// what Sonarr's interactive/automatic search sends) must not fall back to
// the unmatchable IMDB.<id> placeholder when the stream itself carries real
// text, and must not double-append a synthesized suffix on top of it.
func TestReleaseNameUsesStreamHintWhenQueryTitleAbsent(t *testing.T) {
	q := httpstream.StreamQuery{Season: 5, Episode: 9}
	got := releaseName(q, "tt4236770", true, "2160p", "Yellowstone.2018.S05E09.MULTI.SDR.2160p.WEB.H265-FW")
	want := "Yellowstone.2018.S05E09.MULTI.SDR.2160p.WEB.H265-FW"
	if got != want {
		t.Fatalf("releaseName = %q, want %q", got, want)
	}
}

// TestReleaseNameFallsBackToIMDBPlaceholder is the last-resort case: neither
// the arr's q= nor any stream text is available. The old placeholder
// behavior (and its synthesized suffix) is preserved exactly.
func TestReleaseNameFallsBackToIMDBPlaceholder(t *testing.T) {
	q := httpstream.StreamQuery{Season: 5, Episode: 9}
	got := releaseName(q, "tt4236770", true, "2160p", "")
	want := "IMDB.4236770.S05E09.2160p.WEBDL"
	if got != want {
		t.Fatalf("releaseName = %q, want %q", got, want)
	}
}

// TestSearchTitleUsesStreamFilenameForIDOnlyEpisodeQuery is the end-to-end
// regression through the public Search API: an ID-only episode query (no
// q=, tvdbid resolved upstream to imdbID already) must surface the stream's
// own filename as the release title instead of the unmatchable IMDB.<id>
// placeholder (HR0.2 live-gate finding, 2026-07-18).
func TestSearchTitleUsesStreamFilenameForIDOnlyEpisodeQuery(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/stream/series/tt4236770:5:9.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"streams": [
				{"url":"https://cdn.example/a.mkv","behaviorHints":{"filename":"Yellowstone.2018.S05E09.2160p.WEB.H265-FW.mkv"}}
			]
		}`))
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()

	// No Title set — mirrors Sonarr's real ID-only interactive/automatic
	// search request shape.
	q := httpstream.StreamQuery{Kind: "episode", IMDBID: "tt4236770", Season: 5, Episode: 9}
	results, err := h.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected one result, got %+v", results)
	}
	if strings.HasPrefix(results[0].Title, "IMDB.") {
		t.Fatalf("expected real filename-derived title, got unmatchable placeholder %q", results[0].Title)
	}
	want := "Yellowstone.2018.S05E09.2160p.WEB.H265.FW"
	if results[0].Title != want {
		t.Fatalf("title = %q, want %q", results[0].Title, want)
	}
}
