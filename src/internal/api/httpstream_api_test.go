package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
)

func httpTestServer(secret string, enabled bool) *Server {
	cfg := &config.Config{}
	cfg.Server.StreamSecret = secret
	cfg.Server.BaseURL = "http://dh.test:8381"
	cfg.HTTPStream.Enabled = enabled
	return &Server{
		cfg:          cfg,
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		torznabCache: make(map[string]torznabCacheEntry),
	}
}

func testEnvelope(t *testing.T, secret string) (httpstream.GrabEnvelope, string) {
	t.Helper()
	env := httpstream.GrabEnvelope{
		Key: httpstream.ResolveKey{
			Version:   httpstream.ResolveKeyVersion,
			BackendID: "b1",
			Handler:   "ia",
			Kind:      "movie",
			IDs:       map[string]string{"ia": "SomeMovie1949"},
			Selector:  "SomeMovie1949/file.mp4",
		},
		Title: "Some.Movie.1949.1080p.WEBDL.DH-HTTP",
	}
	tok, err := httpstream.EncodeGrabToken(secret, env)
	if err != nil {
		t.Fatalf("encode token: %v", err)
	}
	return env, tok
}

// HS-1.3/D1: /grab/http round trip — signed token → 302 synthetic magnet →
// qBit-add marker extraction recovers the identical token.
func TestGrabHTTPRedirectAndMarkerRoundTrip(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	s := httpTestServer(secret, true)
	env, tok := testEnvelope(t, secret)

	req := httptest.NewRequest(http.MethodGet, "/grab/http/"+tok, nil)
	rec := httptest.NewRecorder()
	s.handleGrabHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (body: %s)", rec.Code, rec.Body.String())
	}
	magnet := rec.Header().Get("Location")
	if !strings.HasPrefix(magnet, "magnet:?xt=urn:btih:"+env.Hash) {
		// env.Hash is filled during Encode; recompute for the assertion.
		digest, _ := env.Key.Digest()
		if !strings.HasPrefix(magnet, "magnet:?xt=urn:btih:"+digest) {
			t.Fatalf("magnet missing synthetic hash: %s", magnet)
		}
	}
	if !strings.Contains(magnet, "tr=") {
		t.Fatalf("magnet missing tracker (arr rejects tracker-less magnets): %s", magnet)
	}

	got := httpGrabTokenFromMagnet(magnet)
	if got != tok {
		t.Fatalf("marker extraction did not round-trip:\n got %q\nwant %q", got, tok)
	}
	// The recovered token must decode to the same key digest.
	dec, err := httpstream.DecodeGrabToken(secret, got)
	if err != nil {
		t.Fatalf("decode recovered token: %v", err)
	}
	digest, _ := env.Key.Digest()
	if dec.Hash != digest {
		t.Fatalf("digest mismatch after round trip")
	}

	// Non-marker magnets and garbage never match.
	if httpGrabTokenFromMagnet("magnet:?xt=urn:btih:aaaabbbbccccddddeeeeffff0000111122223333&dn=x") != "" {
		t.Fatal("plain magnet misdetected as http marker")
	}
	if httpGrabTokenFromMagnet("https://example.test/file.torrent") != "" {
		t.Fatal("http url misdetected as http marker")
	}
}

// Feature-flag and integrity gates on the grab surface.
func TestGrabHTTPRejections(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"

	// Disabled feature → 404 regardless of token validity.
	s := httpTestServer(secret, false)
	_, tok := testEnvelope(t, secret)
	rec := httptest.NewRecorder()
	s.handleGrabHTTP(rec, httptest.NewRequest(http.MethodGet, "/grab/http/"+tok, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled: status = %d, want 404", rec.Code)
	}

	// Enabled but tampered token → 400.
	s = httpTestServer(secret, true)
	bad := tok[:len(tok)-2] + "zz"
	rec = httptest.NewRecorder()
	s.handleGrabHTTP(rec, httptest.NewRequest(http.MethodGet, "/grab/http/"+bad, nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("tampered: status = %d, want 400", rec.Code)
	}

	// Wrong method → 405.
	rec = httptest.NewRecorder()
	s.handleGrabHTTP(rec, httptest.NewRequest(http.MethodPost, "/grab/http/"+tok, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post: status = %d, want 405", rec.Code)
	}
}

// HS-1.6: the http-stream torznab route returns an authoritative empty feed
// when disabled or when no handlers are registered — and never touches
// magnetCache.
func TestTorznabHTTPStreamEmptyWhenDisabledOrNoHandlers(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		s := httpTestServer("0123456789abcdef0123456789abcdef", enabled)
		s.httpHandlers = httpstream.NewRegistry() // empty (H1 state)
		req := httptest.NewRequest(http.MethodGet,
			"/torznab/http-stream?t=tvsearch&tvdbid=1234&season=1&ep=2", nil)
		rec := httptest.NewRecorder()
		s.handleTorznabHTTPStream(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("enabled=%v: status %d", enabled, rec.Code)
		}
		body := rec.Body.String()
		if strings.Contains(body, "<item>") {
			t.Fatalf("enabled=%v: expected empty feed, got items: %s", enabled, body)
		}
		if len(s.magnetCache) != 0 {
			t.Fatalf("enabled=%v: magnetCache touched by http-stream route", enabled)
		}
	}
}

// idNativeStub is a minimal ID-native handler for caps-shape tests.
type idNativeStub struct{}

func (idNativeStub) Name() string { return "omss" }
func (idNativeStub) Search(context.Context, httpstream.StreamQuery) ([]httpstream.SearchResult, error) {
	return nil, nil
}
func (idNativeStub) Resolve(context.Context, httpstream.ResolveRequest) ([]httpstream.ResolvedFile, error) {
	return nil, nil
}
func (idNativeStub) IDNative() bool { return true }

// Caps shape follows the registered handler set (A16): text-only until an
// ID-native handler exists — advertising IDs with only ia registered would
// make the arrs send titleless queries ia must skip (HS-2.1).
func TestTorznabHTTPStreamCapsFollowRegistry(t *testing.T) {
	s := httpTestServer("0123456789abcdef0123456789abcdef", true)
	s.httpHandlers = httpstream.NewRegistry() // empty / ia-only equivalence: no ID-native handler
	rec := httptest.NewRecorder()
	s.handleTorznabHTTPStream(rec, httptest.NewRequest(http.MethodGet, "/torznab/http-stream?t=caps", nil))
	body := rec.Body.String()
	if strings.Contains(body, "tvdbid") || strings.Contains(body, "tmdbid") {
		t.Fatalf("ID caps advertised without an ID-native handler (A16): %s", body)
	}
	for _, want := range []string{"q,season,ep", "HTTP Stream"} {
		if !strings.Contains(body, want) {
			t.Fatalf("caps missing %q: %s", want, body)
		}
	}

	s.httpHandlers.Register("id-native", 0, idNativeStub{})
	rec = httptest.NewRecorder()
	s.handleTorznabHTTPStream(rec, httptest.NewRequest(http.MethodGet, "/torznab/http-stream?t=caps", nil))
	body = rec.Body.String()
	for _, want := range []string{"tvdbid", "tmdbid", "imdbid"} {
		if !strings.Contains(body, want) {
			t.Fatalf("ID-native caps missing %q: %s", want, body)
		}
	}
}

// The synthetic magnet must be a valid URL whose x.dh param survives
// url.Parse round-tripping (the arr treats it as opaque text).
func TestSyntheticMagnetParseable(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	s := httpTestServer(secret, true)
	_, tok := testEnvelope(t, secret)
	rec := httptest.NewRecorder()
	s.handleGrabHTTP(rec, httptest.NewRequest(http.MethodGet, "/grab/http/"+tok, nil))
	magnet := rec.Header().Get("Location")
	u, err := url.Parse(magnet)
	if err != nil {
		t.Fatalf("magnet unparseable: %v", err)
	}
	if u.Query().Get("x.dh") != tok {
		t.Fatal("x.dh param corrupted by encoding")
	}
	if !strings.Contains(u.Query().Get("dn"), "DH-HTTP") {
		t.Fatal("dn missing tagged title")
	}
}

// D9 preflight: coherent 206 with span 0-0 and parseable total is the only
// acceptance; declared sizes must match, while non-seekable 200, wrong spans,
// and manifest/text bodies reject.
func TestPreflightHTTPSource(t *testing.T) {
	mode := "ok"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "ok":
			if r.Header.Get("Range") != "bytes=0-0" {
				t.Errorf("preflight sent Range %q, want bytes=0-0", r.Header.Get("Range"))
			}
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Content-Range", "bytes 0-0/734003200")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte{0})
		case "no-range":
			w.Header().Set("Content-Type", "video/mp4")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("full body"))
		case "bad-span":
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Content-Range", "bytes 0-1023/734003200")
			w.WriteHeader(http.StatusPartialContent)
		case "hls":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Header().Set("Content-Range", "bytes 0-0/1000")
			w.WriteHeader(http.StatusPartialContent)
		case "forbidden":
			http.Error(w, "no", http.StatusForbidden)
		}
	}))
	defer ts.Close()

	s := httpTestServer("0123456789abcdef0123456789abcdef", true)
	// The production source client intentionally rejects private/loopback
	// destinations. This fixture injects httptest's loopback-aware client so
	// the test remains focused on D9 response semantics.
	s.httpClientOnce.Do(func() {
		s.httpProbeClient = ts.Client()
		s.httpRelayClient = ts.Client()
	})
	rf := httpstream.ResolvedFile{URL: ts.URL + "/file.mp4"}

	size, ct, err := s.preflightHTTPSource(context.Background(), rf, "test-item", "test-representation", "test-backend", "test-handler")
	if err != nil || size != 734003200 || ct != "video/mp4" {
		t.Fatalf("good 206 rejected: size=%d ct=%q err=%v", size, ct, err)
	}

	rf.Size = size
	if _, _, err := s.preflightHTTPSource(context.Background(), rf, "test-item", "test-representation", "test-backend", "test-handler"); err != nil {
		t.Fatalf("matching declared size rejected: %v", err)
	}
	rf.Size++
	if _, _, err := s.preflightHTTPSource(context.Background(), rf, "test-item", "test-representation", "test-backend", "test-handler"); httpstream.ClassOf(err) != httpstream.ClassRepresentationLost {
		t.Fatalf("mismatched declared size class = %q, want %q", httpstream.ClassOf(err), httpstream.ClassRepresentationLost)
	} else if strings.Contains(err.Error(), ts.URL) {
		t.Fatalf("mismatched declared size leaked URL: %v", err)
	}
	rf.Size = 0

	for _, m := range []string{"no-range", "bad-span", "hls", "forbidden"} {
		mode = m
		if _, _, err := s.preflightHTTPSource(context.Background(), rf, "test-item", "test-representation", "test-backend", "test-handler"); err == nil {
			t.Fatalf("mode %q: expected rejection", m)
		} else if strings.Contains(err.Error(), ts.URL) {
			t.Fatalf("mode %q: URL leaked into error: %v", m, err)
		}
	}
}

func TestPreflightHLSSource(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Manifest-Proof") != "present" {
			t.Error("OMSS source header was not forwarded")
		}
		switch r.URL.Path {
		case "/valid":
			_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:1,\nsegment.ts\n#EXT-X-ENDLIST\n"))
		case "/malformed":
			_, _ = w.Write([]byte("not a playlist"))
		default:
			<-r.Context().Done()
		}
	}))
	defer ts.Close()

	s := httpTestServer("0123456789abcdef0123456789abcdef", true)
	s.httpClientOnce.Do(func() {
		s.httpProbeClient = ts.Client()
		s.httpRelayClient = ts.Client()
	})
	rf := httpstream.ResolvedFile{URL: ts.URL + "/valid", Name: "master.m3u8", RequestHeaders: map[string]string{"X-Manifest-Proof": "present"}}
	if err := s.preflightHLSSource(context.Background(), rf); err != nil {
		t.Fatalf("valid HLS rejected: %v", err)
	}

	rf.URL = ts.URL + "/malformed"
	if err := s.preflightHLSSource(context.Background(), rf); err == nil ||
		httpstream.ClassOf(err) != httpstream.ClassUpstreamMalformed {
		t.Fatalf("malformed HLS did not fail closed: %v", err)
	}

	rf.URL = ts.URL + "/cancel"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.preflightHLSSource(ctx, rf); err == nil {
		t.Fatal("cancelled HLS preflight succeeded")
	}
}

func TestSplitTrailingYear(t *testing.T) {
	cases := []struct {
		in    string
		title string
		year  int
	}{
		{"Night of the Living Dead 1968", "Night of the Living Dead", 1968},
		{"Night of the Living Dead", "Night of the Living Dead", 0},
		{"2001 A Space Odyssey 1968", "2001 A Space Odyssey", 1968},
		{"1984", "1984", 0}, // bare year-title stays a title
		{"Blade Runner 2049", "Blade Runner", 2049},
	}
	for _, c := range cases {
		title, year := splitTrailingYear(c.in)
		if title != c.title || year != c.year {
			t.Fatalf("%q → %q/%d, want %q/%d", c.in, title, year, c.title, c.year)
		}
	}
}

// idAttrStub is a minimal stremio-shaped handler that returns one fixed
// result, for exercising the response-side tvdbid/imdbid/tmdbid attr
// emission (HR0.2 live-gate finding, 2026-07-18).
type idAttrStub struct{}

func (idAttrStub) Name() string { return "stremio" }
func (idAttrStub) Search(_ context.Context, q httpstream.StreamQuery) ([]httpstream.SearchResult, error) {
	kind := "movie"
	if (q.Kind == "episode" || q.Kind == "tv") && q.Season >= 1 && q.Episode >= 1 {
		kind = "episode"
	}
	return []httpstream.SearchResult{{
		Title: "Placeholder.Title",
		Key: httpstream.ResolveKey{
			Version:   httpstream.ResolveKeyVersion,
			BackendID: "aiostreams",
			Handler:   "stremio",
			Kind:      kind,
			IDs:       map[string]string{"imdb": "tt4236770"},
			Selector:  "file:placeholder.mkv",
			Season:    q.Season,
			Episode:   q.Episode,
		},
		Size: 1024,
	}}, nil
}
func (idAttrStub) Resolve(context.Context, httpstream.ResolveRequest) ([]httpstream.ResolvedFile, error) {
	return nil, nil
}
func (idAttrStub) IDNative() bool { return true }

// TestHTTPStreamFeedEchoesRequestIDsAsAttrs is the direct regression for the
// HR0.2 live-gate finding: an ID-only tv-search (tvdbid+season+ep, no q=,
// exactly what Sonarr's interactive/automatic search sends) must echo the
// arr's own tvdbid back as a torznab:attr so the arr can match the release
// to its library series by ID, since the rendered title text alone cannot
// be relied on to carry a matchable series name.
func TestHTTPStreamFeedEchoesRequestIDsAsAttrs(t *testing.T) {
	s := httpTestServer("0123456789abcdef0123456789abcdef", true)
	s.httpHandlers = httpstream.NewRegistry()
	s.httpHandlers.Register("aiostreams", 0, idAttrStub{})
	s.httpHandlers.Start(context.Background(), nil)

	req := httptest.NewRequest(http.MethodGet,
		"/torznab/http-stream?t=tvsearch&tvdbid=341164&season=5&ep=9", nil)
	rec := httptest.NewRecorder()
	s.handleTorznabHTTPStream(rec, req)
	body := rec.Body.String()

	if !strings.Contains(body, `<torznab:attr name="tvdbid" value="341164">`) {
		t.Fatalf("expected tvdbid attr echoed from request, got: %s", body)
	}
	if strings.Contains(body, `name="imdbid"`) || strings.Contains(body, `name="tmdbid"`) {
		t.Fatalf("did not expect imdbid/tmdbid attrs when request carried neither: %s", body)
	}
}

// TestHTTPStreamFeedEchoesIMDBIDForMovieSearch covers the Radarr-style
// direct-imdbid path: no tvdbid attr should appear when the request didn't
// carry one, only imdbid.
func TestHTTPStreamFeedEchoesIMDBIDForMovieSearch(t *testing.T) {
	s := httpTestServer("0123456789abcdef0123456789abcdef", true)
	s.httpHandlers = httpstream.NewRegistry()
	s.httpHandlers.Register("aiostreams", 0, idAttrStub{})
	s.httpHandlers.Start(context.Background(), nil)

	req := httptest.NewRequest(http.MethodGet,
		"/torznab/http-stream?t=movie&imdbid=tt0110912", nil)
	rec := httptest.NewRecorder()
	s.handleTorznabHTTPStream(rec, req)
	body := rec.Body.String()

	if !strings.Contains(body, `<torznab:attr name="imdbid" value="tt0110912">`) {
		t.Fatalf("expected imdbid attr echoed from request, got: %s", body)
	}
	if strings.Contains(body, `name="tvdbid"`) || strings.Contains(body, `name="tmdbid"`) {
		t.Fatalf("did not expect tvdbid/tmdbid attrs when request carried neither: %s", body)
	}
}

// TestHTTPStreamFeedNoIDAttrsForPlainTextSearch: a plain q= text search
// with no IDs at all must render exactly the pre-existing attr set — no
// regression for that shape.
func TestHTTPStreamFeedNoIDAttrsForPlainTextSearch(t *testing.T) {
	s := httpTestServer("0123456789abcdef0123456789abcdef", true)
	s.httpHandlers = httpstream.NewRegistry()
	s.httpHandlers.Register("aiostreams", 0, idAttrStub{})
	s.httpHandlers.Start(context.Background(), nil)

	req := httptest.NewRequest(http.MethodGet, "/torznab/http-stream?t=search&q=Something", nil)
	rec := httptest.NewRecorder()
	s.handleTorznabHTTPStream(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, "tvdbid") || strings.Contains(body, "imdbid") || strings.Contains(body, "tmdbid") {
		t.Fatalf("did not expect any id attrs for a plain text search: %s", body)
	}
	for _, want := range []string{"category", "infohash", "seeders", "peers"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing pre-existing attr %q: %s", want, body)
		}
	}
}
