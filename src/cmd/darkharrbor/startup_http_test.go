package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestStartupSwitchHandler(t *testing.T) {
	handler := newStartupSwitchHandler()

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(method, "/api?tok=forbidden", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s status = %d, want 503", method, rec.Code)
		}
		if rec.Header().Get("Retry-After") != "5" || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s startup headers = %#v", method, rec.Header())
		}
		if rec.Body.String() != "" && rec.Body.String() != "Service Unavailable\n" {
			t.Fatalf("%s startup body = %q", method, rec.Body.String())
		}
	}

	ready := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api", nil))
			if rec.Code != http.StatusServiceUnavailable && rec.Code != http.StatusNoContent {
				t.Errorf("concurrent status = %d", rec.Code)
			}
		}()
	}
	handler.Ready(ready)
	wg.Wait()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("ready status = %d, want 204", rec.Code)
	}
}
