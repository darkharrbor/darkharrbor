package publicip

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestResolverReflectsChangeAndServesLastGoodDuringRefresh(t *testing.T) {
	var mu sync.Mutex
	address := "192.0.2.1"
	release := make(chan struct{})
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		requests++
		n := requests
		value := address
		mu.Unlock()
		if n == 2 {
			<-release
		}
		_, _ = w.Write([]byte(value))
	}))
	defer srv.Close()

	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	r := New(t.Context(), Options{Endpoints: []string{srv.URL}, RefreshInterval: time.Minute, Now: func() time.Time { return now }})
	defer r.Close()

	got, err := r.Resolve(t.Context())
	if err != nil || got != "192.0.2.1" {
		t.Fatalf("initial resolve = %q, %v", got, err)
	}
	mu.Lock()
	address = "198.51.100.2"
	mu.Unlock()
	now = now.Add(2 * time.Minute)
	got, err = r.Resolve(t.Context())
	if err != nil || got != "192.0.2.1" {
		t.Fatalf("last good during refresh = %q, %v", got, err)
	}
	close(release)
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		if !r.Status().Refreshing {
			break
		}
		time.Sleep(time.Millisecond)
	}
	got, err = r.Resolve(t.Context())
	if err != nil || got != "198.51.100.2" {
		t.Fatalf("refreshed resolve = %q, %v", got, err)
	}
}

func TestResolverCancellationAndMalformedResponse(t *testing.T) {
	requestStarted := make(chan struct{})
	requestDone := make(chan struct{})
	blocked := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		<-r.Context().Done()
		close(requestDone)
	}))
	defer blocked.Close()
	r := New(t.Context(), Options{Endpoints: []string{blocked.URL}, Timeout: time.Minute})
	ctx, cancel := context.WithCancel(t.Context())
	resolveDone := make(chan error, 1)
	go func() {
		_, err := r.Resolve(ctx)
		resolveDone <- err
	}()
	<-requestStarted
	cancel()
	if err := <-resolveDone; err == nil {
		t.Fatal("canceled lookup succeeded")
	}
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("canceled lookup left request running")
	}
	r.Close()

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not-an-address"))
	}))
	defer bad.Close()
	r = New(t.Context(), Options{Endpoints: []string{bad.URL}})
	defer r.Close()
	if _, err := r.Resolve(t.Context()); err == nil || r.Status().Address != "" {
		t.Fatal("malformed response was cached")
	}
}

func TestResolverKeepsLastGoodAfterFailedRefresh(t *testing.T) {
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("203.0.113.7"))
	}))
	defer srv.Close()
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	r := New(t.Context(), Options{
		Endpoints: []string{srv.URL}, RefreshInterval: time.Minute,
		StaleAfter: 2 * time.Minute, Now: func() time.Time { return now },
	})
	defer r.Close()
	if _, err := r.Resolve(t.Context()); err != nil {
		t.Fatal(err)
	}
	fail = true
	now = now.Add(3 * time.Minute)
	got, err := r.Resolve(t.Context())
	if err != nil || got != "203.0.113.7" {
		t.Fatalf("last good = %q, %v", got, err)
	}
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline) && r.Status().Refreshing; {
		time.Sleep(time.Millisecond)
	}
	status := r.Status()
	if !status.LastAttemptFail || !status.Stale || status.Address != "203.0.113.7" {
		t.Fatalf("status = %+v", status)
	}
}

func TestResolverFallsBackAcrossServices(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("2001:db8::7"))
	}))
	defer good.Close()
	r := New(t.Context(), Options{Endpoints: []string{bad.URL, good.URL}})
	defer r.Close()
	got, err := r.Resolve(t.Context())
	if err != nil || got != "2001:db8::7" {
		t.Fatalf("fallback resolve = %q, %v", got, err)
	}
}
