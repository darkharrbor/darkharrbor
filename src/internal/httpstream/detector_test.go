package httpstream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDetectBackendStremioObjectResource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/manifest.json" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"id":"org.example","version":"1.2.3","resources":[{"name":"stream","types":["movie","series"],"idPrefixes":["tt"]}]}`))
	}))
	defer srv.Close()

	got, err := DetectBackend(context.Background(), srv.Client(), srv.URL)
	if err != nil || got != "stremio" {
		t.Fatalf("detect = %q, %v", got, err)
	}
}

func TestDetectBackendOMSS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/manifest.json" {
			// An unrelated successful JSON endpoint is not Stremio identity and
			// must not prevent the ordered OMSS probe.
			_, _ = w.Write([]byte(`{"service":"not-stremio"}`))
			return
		}
		_, _ = w.Write([]byte(`{"spec":"omss","version":"1.1.0","status":"operational","endpoints":{"movie":"/v1/movies/{id}"}}`))
	}))
	defer srv.Close()

	got, err := DetectBackend(context.Background(), srv.Client(), srv.URL)
	if err != nil || got != "omss" {
		t.Fatalf("detect = %q, %v", got, err)
	}
}

func TestDetectBackendRejectsIncompatibleStremioManifest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"org.example","version":"1.0.0","resources":[{"name":"catalog"}]}`))
	}))
	defer srv.Close()

	_, err := DetectBackend(context.Background(), srv.Client(), srv.URL)
	if !errors.Is(err, ErrDetectionSchemaMismatch) {
		t.Fatalf("expected schema mismatch, got %v", err)
	}
}

func TestDetectBackendRetriableStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	_, err := DetectBackend(context.Background(), srv.Client(), srv.URL)
	if err == nil || errors.Is(err, ErrDetectionSchemaMismatch) {
		t.Fatalf("expected transient detector error, got %v", err)
	}
}
