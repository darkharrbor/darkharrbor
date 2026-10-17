package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Regression: Torrentio's /resolve/torbox answers 400 on first touch while
// TorBox adds the torrent, then succeeds. DH must retry once, not relay 400.
func TestMediaFlowRetriesTransientUpstreamStatusOnce(t *testing.T) {
	prev := mediaFlowTransientRetryDelay
	mediaFlowTransientRetryDelay = 10 * time.Millisecond
	t.Cleanup(func() { mediaFlowTransientRetryDelay = prev })

	for _, tc := range []struct {
		name      string
		statuses  []int
		wantCode  int
		wantCalls int32
	}{
		{"transient then ok", []int{http.StatusBadRequest, http.StatusOK}, http.StatusOK, 2},
		{"persistent transient relayed after one retry", []int{http.StatusBadGateway, http.StatusBadGateway, http.StatusOK}, http.StatusBadGateway, 2},
		{"non-transient not retried", []int{http.StatusForbidden, http.StatusOK}, http.StatusForbidden, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				w.WriteHeader(tc.statuses[n-1])
				_, _ = w.Write([]byte("x"))
			}))
			defer origin.Close()

			s := newMediaFlowTestServer(origin.Client())
			generated := generateMediaFlowURL(t, s, mediaFlowURLRequest{DestinationURL: origin.URL})
			u, err := url.Parse(generated)
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			s.handleMediaFlowStream(rec, httptest.NewRequest(http.MethodGet, u.RequestURI(), nil))
			if rec.Code != tc.wantCode || calls.Load() != tc.wantCalls {
				t.Fatalf("status=%d calls=%d, want %d/%d", rec.Code, calls.Load(), tc.wantCode, tc.wantCalls)
			}
		})
	}
}

// Regression: one invalid candidate used to 400 the whole /generate_urls
// batch, and AIOStreams then dropped every stream for the title.
func TestMediaFlowGenerateKeepsBatchWhenOneCandidateInvalid(t *testing.T) {
	s := newMediaFlowTestServer(nil)
	long := strings.Repeat("x", 4096)
	body := `{"urls":[` +
		`{"endpoint":"/proxy/stream","destination_url":"https://cdn.example/a.mkv","query_params":{"api_password":"` + mediaFlowTestPassword + `"}},` +
		`{"endpoint":"/proxy/stream","destination_url":"https://cdn.example/b.mkv","filename":"` + long + `","query_params":{"api_password":"` + mediaFlowTestPassword + `"}},` +
		`{"endpoint":"/proxy/stream","destination_url":"ftp://cdn.example/c.mkv","query_params":{"api_password":"` + mediaFlowTestPassword + `"}}` +
		`]}`
	rec := httptest.NewRecorder()
	s.handleMediaFlowGenerate(rec, httptest.NewRequest(http.MethodPost, "/generate_urls", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp mediaFlowGenerateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp.URLs) != 3 {
		t.Fatalf("want 3 positional urls, got %v (%v)", resp.URLs, err)
	}
	if !strings.Contains(resp.URLs[0], "/proxy/stream?d=") || !strings.Contains(resp.URLs[1], "/proxy/stream?d=") {
		t.Fatalf("valid candidates (incl. over-long filename) must be proxied: %v", resp.URLs[:2])
	}
	if resp.URLs[2] != "ftp://cdn.example/c.mkv" {
		t.Fatalf("unproxyable candidate must pass through, got %q", resp.URLs[2])
	}
}

// Regression: a realistic 1,012-entry AIOStreams batch (~1 KiB/entry) overran
// the old 1 MiB body cap and the whole title's streams were dropped.
func TestMediaFlowGenerateAcceptsLargeRealisticBatch(t *testing.T) {
	s := newMediaFlowTestServer(nil)
	hdr := strings.Repeat("h", 900)
	var b strings.Builder
	b.WriteString(`{"urls":[`)
	const n = 1100
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"endpoint":"/proxy/stream","destination_url":"https://cdn.example/f/` + strconv.Itoa(i) + `.mkv","request_headers":{"X-Pad":"` + hdr + `"},"query_params":{"api_password":"` + mediaFlowTestPassword + `"}}`)
	}
	b.WriteString(`]}`)
	if b.Len() <= 1<<20 {
		t.Fatalf("fixture too small to exercise the old cap: %d", b.Len())
	}
	rec := httptest.NewRecorder()
	s.handleMediaFlowGenerate(rec, httptest.NewRequest(http.MethodPost, "/generate_urls", strings.NewReader(b.String())))
	var resp mediaFlowGenerateResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &resp) != nil || len(resp.URLs) != n {
		t.Fatalf("status=%d urls=%d", rec.Code, len(resp.URLs))
	}
}
