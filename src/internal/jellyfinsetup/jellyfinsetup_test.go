package jellyfinsetup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/dockerctl"
)

func TestJellyfinImageRecognition(t *testing.T) {
	tests := []struct {
		image string
		want  bool
	}{
		{"jellyfin/jellyfin:latest", true},
		{"docker.io/jellyfin/jellyfin:10.10.7", true},
		{"jellyfin/jellyfin@sha256:abcdef", true},
		{"registry.example/jellyfin/jellyfin@sha256:abcdef", true},
		{"example/jellyfin:latest", false},
		{"jellyfin/jellyfin-helper:latest", false},
	}
	for _, tt := range tests {
		if got := IsSupportedImage(tt.image); got != tt.want {
			t.Fatalf("image %q match=%v, want %v", tt.image, got, tt.want)
		}
	}
}

func TestDiscoverJellyfinRequiresExactSelectionWhenMultiple(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/version" {
			_, _ = w.Write([]byte(`{"ApiVersion":"1.51"}`))
			return
		}
		_, _ = w.Write([]byte(`[
  {"Id":"1","Names":["/jellyfin-main"],"Image":"jellyfin/jellyfin:latest","State":"running"},
  {"Id":"2","Names":["/jellyfin-kids"],"Image":"jellyfin/jellyfin@sha256:abcdef","State":"running"}
]`))
	}))
	defer server.Close()
	client := dockerctl.New(server.URL, server.Client())
	if _, err := discoverJellyfin(context.Background(), client, ""); err == nil {
		t.Fatal("multiple Jellyfin containers were selected by list order")
	}
	target, err := discoverJellyfin(context.Background(), client, "jellyfin-kids")
	if err != nil {
		t.Fatal(err)
	}
	if target == nil || target.id != "2" || target.name != "jellyfin-kids" {
		t.Fatalf("target=%+v", target)
	}
	if _, err := discoverJellyfin(context.Background(), client, "missing"); err == nil {
		t.Fatal("missing selected Jellyfin was silently ignored")
	}
}
