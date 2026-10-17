package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/provider"
)

func TestInitCDNSourceMarksTransientProbeFailures(t *testing.T) {
	for _, tt := range []struct {
		name      string
		status    int
		retryable bool
	}{
		{"429 twice is transient", http.StatusTooManyRequests, true},
		{"500 is evidence, not transient (TS-3.1)", http.StatusInternalServerError, false},
		{"416 is not", http.StatusRequestedRangeNotSatisfiable, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()
			reresolve := cdnReResolver(func(context.Context) (string, error) { return srv.URL + "/fresh", nil })
			_, _, _, _, err := initCDNSource(context.Background(), srv.URL, 1<<20, reresolve)
			if err == nil {
				t.Fatal("want error")
			}
			if got := provider.IsRetryable(err); got != tt.retryable {
				t.Fatalf("retryable=%v want %v (err=%v)", got, tt.retryable, err)
			}
		})
	}
}

func TestInitCDNSourceTransportErrorIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // connection refused
	_, _, _, _, err := initCDNSource(context.Background(), url, 1<<20, nil)
	if err == nil || !provider.IsRetryable(err) {
		t.Fatalf("err=%v, want retryable transport error", err)
	}
}
