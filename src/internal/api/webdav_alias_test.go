package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/auth"
)

func TestParseWebDAVContentAlias(t *testing.T) {
	tests := []struct {
		path    string
		parts   string
		visible string
		alias   bool
		ok      bool
	}{
		{path: "/dav/", ok: true},
		{path: "/dav/tv/release/file.mkv", parts: "tv/release/file.mkv", ok: true},
		{path: "/dav/content/", alias: true, ok: true},
		{path: "/dav/content/mOvIeS/release/file.mkv", parts: "movies/release/file.mkv", visible: "Movies", alias: true, ok: true},
		{path: "/dav/content/tV/release/file.mkv", parts: "tv/release/file.mkv", visible: "TV", alias: true, ok: true},
		{path: "/dav/content/audio/release/file.mkv"},
		{path: "/dav/content/TV/../file.mkv"},
		{path: "/dav/content/TV/release/file.mkv/extra"},
		{path: "/other/content/TV/release/file.mkv"},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			got, ok := parseWebDAVPath(test.path)
			if ok != test.ok || got.alias != test.alias || strings.Join(got.parts, "/") != test.parts || got.visible != test.visible {
				t.Fatalf("parseWebDAVPath() = %#v, %v", got, ok)
			}
		})
	}
}

func TestWebDAVContentAliasResolvesWithoutChangingLegacyPath(t *testing.T) {
	srv, stem := newWebDAVExactIndexServer(t, "http://darkharrbor:8381/dav/tv/release/Recovered.Show.S01E01.mkv")
	for _, path := range []string{
		"/dav/tv/release/" + stem + ".mkv",
		"/dav/content/TV/release/" + stem + ".mkv",
		"/dav/content/tv/release/" + stem + ".mkv",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		item, index, err := srv.webdavResolveItem(req)
		if err != nil || item == nil || index != 0 {
			t.Fatalf("path=%q item=%v index=%d error=%v", path, item, index, err)
		}
	}
}

func TestWebDAVContentAliasPropfindUsesAggregatorNames(t *testing.T) {
	srv, _ := newWebDAVExactIndexServer(t, "http://darkharrbor:8381/dav/tv/release/Recovered.Show.S01E01.mkv")
	for _, test := range []struct {
		path string
		want []string
	}{
		{path: "/dav/content/", want: []string{"/dav/content/Movies/", "/dav/content/TV/"}},
		{path: "/dav/content/tv/", want: []string{"/dav/content/TV/release/"}},
		{path: "/dav/tv/", want: []string{"/dav/tv/release/"}},
	} {
		req := httptest.NewRequest("PROPFIND", test.path, nil)
		rec := httptest.NewRecorder()
		srv.webdavPropfind(rec, req)
		if rec.Code != 207 {
			t.Fatalf("path=%q status=%d body=%q", test.path, rec.Code, rec.Body.String())
		}
		for _, want := range test.want {
			if !strings.Contains(rec.Body.String(), want) {
				t.Fatalf("path=%q response missing %q", test.path, want)
			}
		}
	}
}

func TestNormalizeAggregatorCategory(t *testing.T) {
	for input, want := range map[string]string{
		"Movies": "movies", "mOvIeS": "movies", " TV ": "tv", "tv-classic": "tv-classic", "audio": "audio",
	} {
		if got := normalizeAggregatorCategory(input); got != want {
			t.Fatalf("normalizeAggregatorCategory(%q)=%q, want %q", input, got, want)
		}
	}
}

func TestSABRequestCategoryAcceptsAggregatorHistoryParameter(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/dav/api?category=tV", nil)
	if got := sabRequestCategory(req); got != "tv" {
		t.Fatalf("sabRequestCategory()=%q, want tv", got)
	}
}

func TestWebDAVRegistersAggregatorSABAlias(t *testing.T) {
	srv, _ := newWebDAVExactIndexServer(t, "http://darkharrbor:8381/dav/tv/release/Recovered.Show.S01E01.mkv")
	srv.sabAuth = auth.NewSABAuth("test-key", "test-nzb-key")
	srv.cfg.Compatibility.SABVersion = "4.5.1"
	mux := http.NewServeMux()
	srv.registerWebDAV(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dav/api?mode=version&apikey=test-key", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "4.5.1") {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func FuzzParseWebDAVPath(f *testing.F) {
	for _, seed := range []string{"/dav/", "/dav/content/Movies/release/file.mkv", "/dav/content/TV/../file.mkv"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, path string) {
		parsed, ok := parseWebDAVPath(path)
		if !ok {
			return
		}
		if len(parsed.parts) > 3 || !validateWebDAVPathParts(parsed.parts) {
			t.Fatalf("accepted unsafe path %q: %#v", path, parsed)
		}
		if parsed.alias && len(parsed.parts) > 0 && parsed.parts[0] != "movies" && parsed.parts[0] != "tv" {
			t.Fatalf("accepted unsupported alias category %q: %#v", path, parsed)
		}
	})
}
