package dockerctl

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestListContainers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1.51/containers/json" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Container{
			{ID: "abc123", Names: []string{"/sonarr-modern"}, Image: "lscr.io/linuxserver/sonarr:latest", State: "running"},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, nil)
	c.apiVersion = "v1.51"
	containers, err := c.ListContainers(context.Background())
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if len(containers) != 1 || containers[0].ID != "abc123" {
		t.Fatalf("unexpected result: %+v", containers)
	}
}

func TestNegotiatesCachesAndRetriesDockerAPIVersion(t *testing.T) {
	var versionCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			versionCalls++
			if versionCalls == 1 {
				http.Error(w, "temporary", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ApiVersion": "1.47", "Version": "redacted-by-proxy-in-real-use"})
		case "/v1.47/containers/json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[]"))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, nil)
	if _, err := c.ListContainers(context.Background()); err == nil {
		t.Fatal("expected first transient version handshake to fail")
	}
	if _, err := c.ListContainers(context.Background()); err != nil {
		t.Fatalf("retry after transient version failure: %v", err)
	}
	if _, err := c.ListContainers(context.Background()); err != nil {
		t.Fatalf("cached negotiated version: %v", err)
	}
	if versionCalls != 2 {
		t.Fatalf("version handshake calls=%d, want 2 (one failed, one cached success)", versionCalls)
	}
	if c.apiVersion != "v1.47" {
		t.Fatalf("cached API version=%q, want v1.47", c.apiVersion)
	}
}

func TestDoJSONNon2xxReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "403 Forbidden -- path not on harrbor-dockerproxy allowlist", http.StatusForbidden)
	}))
	defer srv.Close()

	c := New(srv.URL, nil)
	c.apiVersion = "v1.51"
	_, err := c.ListContainers(context.Background())
	if err == nil {
		t.Fatal("expected error on 403, got nil")
	}
}

// frame builds one Docker stream-protocol frame for the given stream type
// (1=stdout, 2=stderr) and payload.
func frame(streamType byte, payload []byte) []byte {
	header := make([]byte, 8)
	header[0] = streamType
	binary.BigEndian.PutUint32(header[4:8], uint32(len(payload)))
	return append(header, payload...)
}

func TestDemuxStream(t *testing.T) {
	var buf []byte
	buf = append(buf, frame(1, []byte("hello "))...)
	buf = append(buf, frame(2, []byte("stderr-bit "))...)
	buf = append(buf, frame(1, []byte("world\n"))...)
	// A stdin-typed frame (unexpected on a start response) must be skipped,
	// not folded into output or treated as an error.
	buf = append(buf, frame(0, []byte("ignored-stdin"))...)

	out, err := demuxStream(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("demuxStream: %v", err)
	}
	want := "hello stderr-bit world\n"
	if out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

func TestExecEndToEnd(t *testing.T) {
	execID := "exec-xyz"
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.51/containers/harrbor-target/exec", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		var req execCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode exec create body: %v", err)
		}
		if len(req.Cmd) == 0 || req.Cmd[0] != "id" {
			t.Fatalf("unexpected Cmd: %+v", req.Cmd)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(execCreateResponse{ID: execID})
	})
	mux.HandleFunc("/v1.51/exec/"+execID+"/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(frame(1, []byte("uid=0(root) gid=0(root)\n")))
	})
	mux.HandleFunc("/v1.51/exec/"+execID+"/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(execInspectResponse{ExitCode: 0, Running: false})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL, nil)
	c.apiVersion = "v1.51"
	result, err := c.Exec(context.Background(), "harrbor-target", "root", []string{"id"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("unexpected exit code: %d", result.ExitCode)
	}
	if result.Output != "uid=0(root) gid=0(root)\n" {
		t.Fatalf("unexpected output: %q", result.Output)
	}
}

func TestEventsStreamsBackToBackJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1.51/events" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("filters") == "" {
			t.Fatalf("expected a filters query param")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		enc := json.NewEncoder(w)
		_ = enc.Encode(map[string]any{
			"Type": "container", "Action": "create",
			"Actor": map[string]any{"ID": "abc", "Attributes": map[string]string{"image": "lscr.io/linuxserver/sonarr:latest", "name": "sonarr-modern"}},
			"time":  111,
		})
		if flusher != nil {
			flusher.Flush()
		}
		_ = enc.Encode(map[string]any{
			"Type": "container", "Action": "start",
			"Actor": map[string]any{"ID": "abc", "Attributes": map[string]string{"image": "lscr.io/linuxserver/sonarr:latest", "name": "sonarr-modern"}},
			"time":  112,
		})
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer srv.Close()

	c := New(srv.URL, nil)
	c.apiVersion = "v1.51"
	stream, err := c.Events(context.Background())
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	defer stream.Close()

	ev1, err := stream.Next()
	if err != nil {
		t.Fatalf("Next (1): %v", err)
	}
	if ev1.Action != "create" || ev1.Actor.Attributes["name"] != "sonarr-modern" {
		t.Fatalf("unexpected first event: %+v", ev1)
	}

	ev2, err := stream.Next()
	if err != nil {
		t.Fatalf("Next (2): %v", err)
	}
	if ev2.Action != "start" {
		t.Fatalf("unexpected second event: %+v", ev2)
	}
}

// BUGFIX regression test (2026-07-01): Events() must not be killed by
// Client.HTTPClient's overall request Timeout -- only ctx cancellation
// should end the stream. Found live in production: eventwatcher's real
// event-watch loop (wired into cmd/darkharrbor/main.go for the first time
// this session) reconnected every ~18s against the real docker-proxy,
// tracing back to New()'s default 30s http.Client.Timeout silently killing
// the long-lived streaming GET. Every prior test/verification of this
// package used a context that canceled well under 30s, so this never
// surfaced. Reproduces it deterministically and fast (no real 30s wait) by
// using a client with an artificially tiny Timeout and asserting the stream
// survives well past it.
func TestEventsSurvivesClientTimeout(t *testing.T) {
	blockUntilClosed := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-blockUntilClosed // hang, simulating a live but idle events stream
	}))
	defer func() {
		close(blockUntilClosed)
		srv.Close()
	}()

	// A Timeout this small would kill the request almost immediately if
	// Events() used c.HTTPClient.Do directly instead of a Timeout-cleared
	// copy.
	tinyTimeoutClient := &http.Client{Timeout: 50 * time.Millisecond}
	c := New(srv.URL, tinyTimeoutClient)
	c.apiVersion = "v1.51"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := c.Events(ctx)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	defer stream.Close()

	done := make(chan error, 1)
	go func() {
		_, err := stream.Next()
		done <- err
	}()

	// Wait well past the client's 50ms Timeout, then confirm the stream is
	// still alive (Next() hasn't returned/errored on its own).
	select {
	case err := <-done:
		t.Fatalf("stream ended on its own after outliving the client Timeout (err=%v) -- Events() is not immune to Client.Timeout", err)
	case <-time.After(300 * time.Millisecond):
		// Good: still blocked in Next(), meaning the connection is still open.
	}

	// Now confirm ctx cancellation (the intended, only lifetime control)
	// still unblocks it cleanly.
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error from Next() after context cancel")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Next() did not unblock within 5s of context cancellation -- possible goroutine leak")
	}
}

func TestEventsContextCancelUnblocksNext(t *testing.T) {
	blockUntilClosed := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-blockUntilClosed // hang until the test closes this, simulating a live but idle events stream
	}))
	defer func() {
		close(blockUntilClosed)
		srv.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	c := New(srv.URL, nil)
	c.apiVersion = "v1.51"
	stream, err := c.Events(ctx)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	defer stream.Close()

	done := make(chan error, 1)
	go func() {
		_, err := stream.Next()
		done <- err
	}()

	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error from Next() after context cancel")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Next() did not unblock within 5s of context cancellation -- possible goroutine leak")
	}
}
