package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
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
