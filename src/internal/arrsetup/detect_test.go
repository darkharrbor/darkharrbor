package arrsetup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDetectAppTypeUsesAuthenticatedStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/system/status" || r.Header.Get("X-Api-Key") != "opaque" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"appName":"Radarr"}`))
	}))
	defer server.Close()

	got, err := DetectAppType(context.Background(), server.URL, "opaque")
	if err != nil || got != "radarr" {
		t.Fatalf("got=%q err=%v", got, err)
	}
}

func TestDetectAppTypeRejectsUnknownOrUnauthorized(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"unauthorized", http.StatusUnauthorized, `{}`},
		{"unknown", http.StatusOK, `{"appName":"Prowlarr"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			_, err := DetectAppType(context.Background(), server.URL, "opaque")
			if err == nil || strings.Contains(err.Error(), server.URL) {
				t.Fatalf("expected redacted refusal, got %v", err)
			}
		})
	}
}
