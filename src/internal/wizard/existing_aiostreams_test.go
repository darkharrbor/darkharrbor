package wizard

import (
	"strings"
	"testing"
)

func TestExistingAIOStreamsBackendPreservesSavedUserPath(t *testing.T) {
	backend, err := existingAIOStreamsBackend(
		"http://aiostreams:3000",
		"https://watch.example/stremio/user-id/opaque%2Fconfig/manifest.json",
	)
	if err != nil {
		t.Fatal(err)
	}
	if backend.URL != "http://aiostreams:3000/stremio/user-id/opaque%2Fconfig" {
		t.Fatalf("backend URL = %q", backend.URL)
	}
	if backend.PublicURL != "https://watch.example" || backend.Type != "stremio" {
		t.Fatalf("backend = %+v", backend)
	}
}

func TestExistingAIOStreamsBackendRejectsAmbiguousInputs(t *testing.T) {
	cases := [][2]string{
		{"http://aiostreams:3000/prefix", "https://watch.example/stremio/u/c/manifest.json"},
		{"http://aiostreams:3000", "https://watch.example/not-stremio/manifest.json"},
		{"http://aiostreams:3000", "https://watch.example/stremio/u/c/manifest.json?token=leak"},
	}
	for _, pair := range cases {
		if _, err := existingAIOStreamsBackend(pair[0], pair[1]); err == nil {
			t.Fatalf("accepted internal=%q manifest=%q", pair[0], pair[1])
		} else if strings.Contains(err.Error(), "token=leak") {
			t.Fatal("validation error leaked manifest value")
		}
	}
}
