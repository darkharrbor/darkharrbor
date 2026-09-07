package httpstream

import (
	"testing"
	"time"
)

func TestValidateResolvedFile(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	in := ResolvedFile{
		Selector:        " stable/file ",
		URL:             "https://cdn.example/file.mp4",
		Size:            42,
		RequestHeaders:  map[string]string{"authorization": "Bearer source-token"},
		ResponseHeaders: map[string]string{"content-disposition": "inline"},
		ExpiresAt:       now.Add(time.Minute),
		RefreshContext:  "opaque-response-id",
	}
	got, err := ValidateResolvedFile(in, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Selector != "stable/file" {
		t.Fatalf("selector = %q", got.Selector)
	}
	if got.RequestHeaders["Authorization"] != "Bearer source-token" {
		t.Fatalf("request headers = %#v", got.RequestHeaders)
	}
	if got.ResponseHeaders["Content-Disposition"] != "inline" {
		t.Fatalf("response headers = %#v", got.ResponseHeaders)
	}
	if got.RefreshContext != "opaque-response-id" {
		t.Fatalf("refresh context = %q", got.RefreshContext)
	}
}

func TestValidateResolvedFileRejectsUnsafeTransientData(t *testing.T) {
	now := time.Now()
	tests := []ResolvedFile{
		{URL: "https://cdn.example/file.mp4"},
		{Selector: "x", URL: "file:///tmp/movie"},
		{Selector: "x", URL: "https://cdn.example/file", ExpiresAt: now.Add(-time.Second)},
		{Selector: "x", URL: "https://cdn.example/file", RequestHeaders: map[string]string{"Range": "bytes=0-1"}},
		{Selector: "x", URL: "https://cdn.example/file", ResponseHeaders: map[string]string{"X-Test": "bad\r\nvalue"}},
	}
	for i, tc := range tests {
		if _, err := ValidateResolvedFile(tc, now); err == nil {
			t.Fatalf("case %d: expected error", i)
		}
	}
}
