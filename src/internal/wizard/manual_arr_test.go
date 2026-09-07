package wizard

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPromptManualArrInstancesAuthenticatesAndBuildsSealedReference(t *testing.T) {
	const apiKey = "test-api-key"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/system/status" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-Api-Key") != apiKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"appName":"Radarr"}`))
	}))
	defer server.Close()

	withResponses(t, []string{"radarr", "movies", server.URL + "/", apiKey, "done"})
	var out bytes.Buffer
	instances, err := promptManualArrInstances(context.Background(), &out, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 1 {
		t.Fatalf("instances=%v", instances)
	}
	got := instances[0]
	if got.Name != "movies" || got.AppType != "radarr" || got.URL != server.URL || got.APIKey != apiKey || got.APIKeyRef != "HARRBOR_ARR_MOVIES_APIKEY" {
		t.Fatalf("instance=%+v", got)
	}
	if strings.Contains(out.String(), apiKey) {
		t.Fatal("manual-entry output disclosed API key")
	}
}

func TestPromptManualArrInstancesRejectsMismatchedAppType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"appName":"Radarr"}`))
	}))
	defer server.Close()

	withResponses(t, []string{"sonarr", "tv", server.URL, "test-key"})
	_, err := promptManualArrInstances(context.Background(), &bytes.Buffer{}, false)
	if err == nil || !strings.Contains(err.Error(), "identifies as radarr") {
		t.Fatalf("expected app-type mismatch, got %v", err)
	}
}

func TestPromptManualArrInstancesRejectsFailedAuthentication(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()

	withResponses(t, []string{"sonarr", "tv", server.URL, "wrong-key"})
	_, err := promptManualArrInstances(context.Background(), &bytes.Buffer{}, false)
	if err == nil || !strings.Contains(err.Error(), "returned HTTP 401") {
		t.Fatalf("expected authenticated refusal, got %v", err)
	}
}

func TestPromptManualArrInstancesRejectsSecondProwlarrBeforeCredentialPrompt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/system/status" || r.Header.Get("X-Api-Key") != "first-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	withResponses(t, []string{"prowlarr", "search-main", server.URL, "first-key", "prowlarr", "search-alt"})
	var out bytes.Buffer
	_, err := promptManualArrInstances(context.Background(), &out, true)
	if err == nil || !strings.Contains(err.Error(), "only one Prowlarr") {
		t.Fatalf("expected one-Prowlarr boundary, got %v", err)
	}
	if strings.Contains(out.String(), "first-key") {
		t.Fatal("manual Prowlarr key was printed")
	}
}

func TestNormalizeArrBaseURL(t *testing.T) {
	got, err := normalizeArrBaseURL(" https://arr.example.test/sonarr/ ")
	if err != nil || got != "https://arr.example.test/sonarr" {
		t.Fatalf("got=%q err=%v", got, err)
	}
	for _, bad := range []string{"arr:8989", "ftp://arr.example", "https://user@arr.example", "https://arr.example?q=secret"} {
		if _, err := normalizeArrBaseURL(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestWithoutAppTypeKeepsArrs(t *testing.T) {
	instances := []discoveredArrInstance{{Name: "tv", AppType: "sonarr"}, {Name: "search", AppType: "prowlarr"}, {Name: "movies", AppType: "radarr"}}
	got := withoutAppType(instances, "prowlarr")
	if len(got) != 2 || got[0].Name != "tv" || got[1].Name != "movies" {
		t.Fatalf("got=%v", got)
	}
}
