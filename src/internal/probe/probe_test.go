package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRangeProbeRejectsRangeIgnoringOversizeResponseAndCleansTemp(t *testing.T) {
	const limit = int64(32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "bytes=0-31" {
			t.Errorf("Range = %q, want bytes=0-31", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("x", int(limit+1))))
	}))
	defer server.Close()

	before, err := filepath.Glob(filepath.Join(os.TempDir(), "darkharrbor-probe-*"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(nil, time.Second).RangeProbe(context.Background(), server.URL, limit)
	if err == nil || !strings.Contains(err.Error(), "response exceeded 32-byte limit") {
		t.Fatalf("RangeProbe error = %v, want bounded-response error", err)
	}
	after, err := filepath.Glob(filepath.Join(os.TempDir(), "darkharrbor-probe-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("temporary probe files before=%d after=%d", len(before), len(after))
	}
}

func TestRangeProbeAcceptsExactLimit(t *testing.T) {
	binDir := t.TempDir()
	ffprobe := filepath.Join(binDir, "ffprobe")
	if err := os.WriteFile(ffprobe, []byte("#!/bin/sh\nprintf '%s' '{\"streams\":[]}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte(strings.Repeat("x", 32)))
	}))
	defer server.Close()

	result, err := New(nil, time.Second).RangeProbe(context.Background(), server.URL, 32)
	if err != nil {
		t.Fatal(err)
	}
	if result.JSON != `{"streams":[]}` {
		t.Fatalf("probe JSON = %q", result.JSON)
	}
}

func TestRangeProbeCancellationCleansTemp(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()

	before, err := filepath.Glob(filepath.Join(os.TempDir(), "darkharrbor-probe-*"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := New(nil, time.Second).RangeProbe(ctx, server.URL, 32)
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("RangeProbe error = %v, want context cancellation", err)
	}
	after, err := filepath.Glob(filepath.Join(os.TempDir(), "darkharrbor-probe-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("temporary probe files before=%d after=%d", len(before), len(after))
	}
}

func TestRangeProbeRejectsInvalidByteLimitBeforeRequest(t *testing.T) {
	requested := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requested = true
	}))
	defer server.Close()

	_, err := New(nil, time.Second).RangeProbe(context.Background(), server.URL, 0)
	if err == nil || !strings.Contains(err.Error(), "probe bytes must be positive") {
		t.Fatalf("RangeProbe error = %v, want invalid-limit error", err)
	}
	if requested {
		t.Fatal("invalid probe limit reached origin")
	}
}

func TestRangeProbeErrorsDoNotExposeURL(t *testing.T) {
	const secret = "signed-secret"
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := server.URL
	server.Close()

	for _, rawURL := range []string{
		"http://%zz?tok=" + secret,
		closedURL + "?tok=" + secret,
	} {
		_, err := New(nil, 100*time.Millisecond).RangeProbe(context.Background(), rawURL, 32)
		if err == nil {
			t.Fatalf("RangeProbe(%q) unexpectedly succeeded", rawURL)
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), rawURL) {
			t.Fatalf("RangeProbe error exposed URL: %q", err)
		}
	}
}
