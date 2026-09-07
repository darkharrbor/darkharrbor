package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func TestAdvertisedHTTPContentType(t *testing.T) {
	tests := []struct {
		origin string
		name   string
		want   string
	}{
		{"", "movie.mp4", "video/mp4"},
		{"application/octet-stream", "movie.mkv", "video/x-matroska"},
		{"Application/Octet-Stream; name=opaque", "movie.avi", "video/x-msvideo"},
		{"not a media type", "movie.webm", "video/webm"},
		{"video/mp4; profile=main", "movie.mkv", "video/mp4; profile=main"},
	}
	for _, tc := range tests {
		if got := advertisedHTTPContentType(tc.origin, tc.name); got != tc.want {
			t.Errorf("advertisedHTTPContentType(%q, %q) = %q, want %q", tc.origin, tc.name, got, tc.want)
		}
	}
}

func TestHR16PreservesNonMediaRejection(t *testing.T) {
	for _, value := range []string{"text/html", "application/json", "application/vnd.apple.mpegurl", "application/dash+xml", "application/xml"} {
		if !badPreflightContentType(value) {
			t.Errorf("badPreflightContentType(%q) = false, want true", value)
		}
	}
	for _, value := range []string{"", "application/octet-stream", "not a media type"} {
		if badPreflightContentType(value) {
			t.Errorf("badPreflightContentType(%q) = true, want filename fallback", value)
		}
	}
}

func TestPreflightHTTPSourceDerivesUselessOriginType(t *testing.T) {
	originType := "application/octet-stream"
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if originType != "" {
			w.Header().Set("Content-Type", originType)
		}
		w.Header().Set("Content-Range", "bytes 0-0/1024")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte{0})
	}))
	defer origin.Close()

	s := httpTestServer("0123456789abcdef0123456789abcdef", true)
	s.httpClientOnce.Do(func() {
		s.httpProbeClient = origin.Client()
		s.httpRelayClient = origin.Client()
	})
	rf := httpstream.ResolvedFile{Name: "movie.mkv", URL: origin.URL}
	for _, value := range []string{"application/octet-stream", "", "not a media type"} {
		originType = value
		_, got, err := s.preflightHTTPSource(context.Background(), rf, "item", "representation", "backend", "handler")
		if err != nil {
			t.Fatalf("origin type %q rejected: %v", value, err)
		}
		if got != "video/x-matroska" {
			t.Fatalf("origin type %q produced %q, want video/x-matroska", value, got)
		}
	}
}

func TestHTTPHeadAndRangeDeriveUselessPersistedType(t *testing.T) {
	files, err := httpstream.MarshalFiles([]httpstream.PersistedFile{{
		FileID: "file", Name: "movie.mkv", Size: 4,
		ContentType: "application/octet-stream", RangeVerified: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	item := &store.Item{ID: "item", FileList: &files}
	s := httpTestServer("0123456789abcdef0123456789abcdef", true)

	head := httptest.NewRecorder()
	headRequest := httptest.NewRequest(http.MethodHead, "/stream/item/file", nil)
	headRequest.SetPathValue("file_id", "file")
	s.streamHTTPItem(head, headRequest, item)
	if got := head.Header().Get("Content-Type"); got != "video/x-matroska" {
		t.Fatalf("HEAD Content-Type = %q, want video/x-matroska", got)
	}

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Range", "bytes 0-0/4")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("x"))
	}))
	defer origin.Close()
	s.httpClientOnce.Do(func() {
		s.httpProbeClient = origin.Client()
		s.httpRelayClient = origin.Client()
	})
	ranged := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stream/item/file", nil)
	rng := &httpstream.ByteRange{Start: 0, End: 0}
	s.attemptHTTPRelay(ranged, req, item, httpstream.PersistedFile{
		FileID: "file", Name: "movie.mkv", Size: 4,
		ContentType: "application/octet-stream", RangeVerified: true,
	}, httpstream.ResolvedFile{URL: origin.URL}, rng)
	if ranged.Code != http.StatusPartialContent {
		t.Fatalf("range status = %d, want 206", ranged.Code)
	}
	if got := ranged.Header().Get("Content-Type"); got != "video/x-matroska" {
		t.Fatalf("range Content-Type = %q, want video/x-matroska", got)
	}
}

func FuzzAdvertisedHTTPContentType(f *testing.F) {
	f.Add("application/octet-stream", "movie.mkv")
	f.Add("video/mp4; profile=main", "movie.mp4")
	f.Add("not a media type", "opaque")
	f.Fuzz(func(t *testing.T, originType, name string) {
		got := advertisedHTTPContentType(originType, name)
		if got == "" {
			t.Fatal("empty advertised content type")
		}
		if originType == "" && got != fileExtContentType(name) {
			t.Fatal("empty origin type did not derive from filename")
		}
	})
}
