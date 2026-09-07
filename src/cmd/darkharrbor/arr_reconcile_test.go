package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/arrsetup"
	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func TestResolveArrReconcileTargetsExcludesProwlarrAndUnknown(t *testing.T) {
	instances := []store.ArrInstance{
		{Name: "tv", URL: "http://sonarr:8989", AppType: "Sonarr", APIKeyRef: "TV_KEY"},
		{Name: "meta", URL: "http://prowlarr:9696", AppType: "prowlarr", APIKeyRef: "META_KEY"},
		{Name: "movies", URL: "http://radarr:7878/", AppType: "radarr", APIKeyRef: "MOVIE_KEY"},
		{Name: "future", URL: "http://example.invalid", AppType: "future", APIKeyRef: "FUTURE_KEY"},
	}
	keys := map[string]string{"TV_KEY": "tv-secret", "MOVIE_KEY": "movie-secret"}
	targets, err := resolveArrReconcileTargets(instances, func(key string) (string, bool) {
		value, ok := keys[key]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 || targets[0].AppType != "sonarr" || targets[1].AppType != "radarr" {
		t.Fatalf("targets = %+v", targets)
	}
}

func TestResolveEffectiveArrReconcileTargetsFallsBackToEnvironment(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"appName":"Sonarr"}`))
	}))
	defer server.Close()
	targets, err := resolveEffectiveArrReconcileTargets(context.Background(), nil, []config.ArrTarget{{
		Name: "tv", BaseURL: server.URL, APIKey: "secret",
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].Name != "tv" || targets[0].AppType != "sonarr" {
		t.Fatalf("targets=%+v", targets)
	}
}

func TestResolveArrReconcileTargetsFailsClosed(t *testing.T) {
	lookup := func(string) (string, bool) { return "", false }
	for name, instances := range map[string][]store.ArrInstance{
		"missing-key": {{URL: "http://sonarr:8989", AppType: "sonarr", APIKeyRef: "MISSING"}},
		"userinfo":    {{URL: "http://user:pass@sonarr:8989", AppType: "sonarr", APIKeyRef: "KEY"}},
		"no-targets":  {{URL: "http://prowlarr:9696", AppType: "prowlarr", APIKeyRef: "KEY"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := resolveArrReconcileTargets(instances, lookup); err == nil {
				t.Fatal("expected failure")
			}
		})
	}
}

func TestResolveArrReconcileTargetsIsBounded(t *testing.T) {
	instances := make([]store.ArrInstance, maxArrReconcileTargets+1)
	for i := range instances {
		instances[i] = store.ArrInstance{URL: "http://sonarr:8989", AppType: "sonarr", APIKeyRef: "KEY"}
	}
	if _, err := resolveArrReconcileTargets(instances, func(string) (string, bool) { return "secret", true }); err == nil {
		t.Fatal("expected target limit failure")
	}
}

func TestSafeArrReconcileErrorDoesNotExposeResponse(t *testing.T) {
	secretBearing := "qbit: GET /api/v3/downloadclient: status 500: tok=do-not-print https://private.invalid/path"
	if got := safeArrReconcileError(context.Canceled); got != "canceled" {
		t.Fatalf("cancel = %q", got)
	}
	if got := safeArrReconcileError(assertError(secretBearing)); got != "qbit failed" ||
		strings.Contains(got, "tok=") || strings.Contains(got, "://") {
		t.Fatalf("safe error = %q", got)
	}
}

type assertError string

func (e assertError) Error() string { return string(e) }

func TestReconcileStoredArrsHonorsCancellationBeforeMutation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	t.Setenv("ARR_KEY", "secret")
	cfg := &config.Config{}
	instances := []store.ArrInstance{{URL: "http://sonarr:8989", AppType: "sonarr", APIKeyRef: "ARR_KEY"}}
	err := reconcileStoredArrs(ctx, io.Discard, instances, cfg, arrsetup.Lanes{Torrent: true})
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("error = %v, want cancellation", err)
	}
}
