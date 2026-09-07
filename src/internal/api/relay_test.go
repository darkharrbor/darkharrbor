package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
)

// ---- pure function tests ----

func TestValidateRelayURL(t *testing.T) {
	base := "http://darkharrbor:8381"
	cases := []struct {
		url     string
		wantErr bool
	}{
		{"http://darkharrbor:8381/stream/abc/0?tok=xyz", false},
		{"", true},
		{"not a url", true},
		{"http://evil.example.com/stream/abc/0", true},
		{"https://darkharrbor:8381/stream/abc/0", true}, // scheme mismatch
		{"http://darkharrbor:9999/stream/abc/0", true},  // port mismatch (different host:port)
		{"file:///etc/passwd", true},
	}
	for _, c := range cases {
		err := validateRelayURL(c.url, base)
		if (err != nil) != c.wantErr {
			t.Errorf("validateRelayURL(%q) error=%v, wantErr=%v", c.url, err, c.wantErr)
		}
	}
}

func TestValidateRelayArgs(t *testing.T) {
	cases := []struct {
		args    []string
		wantErr bool
	}{
		{nil, false},
		{[]string{"-v", "quiet", "-print_format", "json", "-show_streams", "-show_format"}, false},
		{[]string{"-i", "/etc/passwd"}, true},
		{[]string{"-protocol_whitelist", "file,http"}, true},
		{[]string{"-show_format", "-i"}, true},
	}
	for _, c := range cases {
		err := validateRelayArgs(c.args)
		if (err != nil) != c.wantErr {
			t.Errorf("validateRelayArgs(%v) error=%v, wantErr=%v", c.args, err, c.wantErr)
		}
	}
}

// ---- handler-level tests, using a fake ffprobe-full script ----

// writeFakeFFprobe writes a shell script standing in for ffprobe-full: it
// echoes fixedStdout to stdout and exits with exitCode, ignoring its argv
// entirely (the tests assert on argv indirectly via what the handler sends,
// not by inspecting what the script received).
func writeFakeFFprobe(t *testing.T, fixedStdout string, exitCode int, sleep time.Duration) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ffprobe-full")
	script := "#!/bin/sh\n"
	if sleep > 0 {
		script += fmt.Sprintf("sleep %f\n", sleep.Seconds())
	}
	script += "printf '%s' " + shellQuote(fixedStdout) + "\n"
	script += "exit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // test fixture, fixed 0755 mode is correct for an executable
		t.Fatalf("write fake ffprobe-full: %v", err)
	}
	return path
}

func shellQuote(s string) string {
	return "'" + s + "'" // test fixtures never contain a single quote
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func testRelayServer(t *testing.T, ffprobePath string, concurrency int) *Server {
	t.Helper()
	cfg := &config.Config{}
	cfg.Server.BaseURL = "http://darkharrbor:8381"
	cfg.Relay.Concurrency = concurrency
	cfg.Relay.Timeout = 5 * time.Second
	cfg.Relay.QueueTimeout = 500 * time.Millisecond
	cfg.Relay.MaxBodyBytes = 65536
	cfg.Relay.FFprobeFullPath = ffprobePath

	return &Server{
		cfg:      cfg,
		log:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		relaySem: make(chan struct{}, concurrency),
	}
}

func doProbeRequest(s *Server, body relayRequest) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/probe", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleProbeRelay(rec, req)
	return rec
}

func TestHandleProbeRelaySuccess(t *testing.T) {
	fake := writeFakeFFprobe(t, `{"streams":[]}`, 0, 0)
	s := testRelayServer(t, fake, 4)

	rec := doProbeRequest(s, relayRequest{
		URL:  "http://darkharrbor:8381/stream/abc/0?tok=xyz",
		Args: []string{"-v", "quiet", "-print_format", "json"},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp relayResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit_code = %d, want 0", resp.ExitCode)
	}
	if resp.Stdout != `{"streams":[]}` {
		t.Fatalf("stdout = %q", resp.Stdout)
	}
}

func TestProbeRelayLogsDoNotLeakURLTokenArgsOrSource(t *testing.T) {
	fake := writeFakeFFprobe(t, `{"streams":[]}`, 0, 0)
	s := testRelayServer(t, fake, 4)
	var logs bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	secret := "must-not-appear"
	rec := doProbeRequest(s, relayRequest{
		URL:    "http://darkharrbor:8381/stream/abc/0?tok=" + secret,
		Args:   []string{"-v", secret},
		Source: secret,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := logs.String(); strings.Contains(got, secret) || strings.Contains(got, "http://") || strings.Contains(got, "tok=") {
		t.Fatalf("relay log leaked URL or secret: %s", got)
	}
}

func TestHandleProbeRelayRelaysNonZeroExit(t *testing.T) {
	fake := writeFakeFFprobe(t, "", 1, 0)
	s := testRelayServer(t, fake, 4)

	rec := doProbeRequest(s, relayRequest{URL: "http://darkharrbor:8381/stream/abc/0"})

	// A real ffprobe failure is still a 200 at the HTTP level -- the exit
	// code IS the payload, not an HTTP-level error.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (exit code relayed in body)", rec.Code)
	}
	var resp relayResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.ExitCode != 1 {
		t.Fatalf("exit_code = %d, want 1", resp.ExitCode)
	}
}

func TestHandleProbeRelayRejectsBadURL(t *testing.T) {
	fake := writeFakeFFprobe(t, "{}", 0, 0)
	s := testRelayServer(t, fake, 4)

	rec := doProbeRequest(s, relayRequest{URL: "http://evil.example.com/x"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleProbeRelayRejectsMalformedBody(t *testing.T) {
	fake := writeFakeFFprobe(t, "{}", 0, 0)
	s := testRelayServer(t, fake, 4)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/probe", bytes.NewReader([]byte("{not json")))
	rec := httptest.NewRecorder()
	s.handleProbeRelay(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleProbeRelayRejectsOversizedBody(t *testing.T) {
	fake := writeFakeFFprobe(t, "{}", 0, 0)
	s := testRelayServer(t, fake, 4)
	s.cfg.Relay.MaxBodyBytes = 16 // tiny limit

	rec := doProbeRequest(s, relayRequest{URL: "http://darkharrbor:8381/stream/abc/0?tok=some-long-token-value"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for oversized body", rec.Code)
	}
}

// BUGFIX regression test (2026-07-01): found live during a real bulk
// multi-season grab (~98 episodes at once) -- a relay-level exec timeout
// was being misclassified as a successful ffprobe run with exit_code=-1
// instead of a 502. exec.CommandContext SIGKILLs the process on timeout;
// on Linux the resulting error still satisfies errors.As(*exec.ExitError),
// and ExitError.ExitCode() returns -1 for a signal-terminated process, so
// checking that branch before ctx.Err() silently turned every timeout into
// an HTTP 200 with garbage (exit_code=-1, truncated/empty stdout) that the
// arr-side shim would relay straight to Sonarr as if it were real ffprobe
// output. Sets Relay.Timeout shorter than the fake script's sleep so the
// real exec.CommandContext kill path fires, exactly reproducing the live
// failure mode instead of asserting against a description of it.
func TestHandleProbeRelayExecTimeoutReturns502NotFalseSuccess(t *testing.T) {
	fake := writeFakeFFprobe(t, `{"streams":[]}`, 0, 2*time.Second)
	s := testRelayServer(t, fake, 4)
	s.cfg.Relay.Timeout = 100 * time.Millisecond // shorter than the fake's 2s sleep

	rec := doProbeRequest(s, relayRequest{URL: "http://darkharrbor:8381/stream/abc/0"})

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s -- want 502 (relay-level timeout), not a false-success 200 with a garbage exit code",
			rec.Code, rec.Body.String())
	}
}

func TestHandleProbeRelayQueueTimeoutUnderSaturation(t *testing.T) {
	// A slow fake ffprobe-full + concurrency=1 means a second concurrent
	// request must queue and then hit the queue timeout, never running
	// unboundedly in parallel.
	fake := writeFakeFFprobe(t, "{}", 0, 300*time.Millisecond)
	s := testRelayServer(t, fake, 1)
	s.cfg.Relay.QueueTimeout = 100 * time.Millisecond

	done := make(chan *httptest.ResponseRecorder, 2)
	go func() {
		done <- doProbeRequest(s, relayRequest{URL: "http://darkharrbor:8381/stream/a/0"})
	}()
	time.Sleep(20 * time.Millisecond) // let the first request acquire the slot
	go func() {
		done <- doProbeRequest(s, relayRequest{URL: "http://darkharrbor:8381/stream/b/0"})
	}()

	first := <-done
	second := <-done
	codes := map[int]bool{first.Code: true, second.Code: true}
	if !codes[http.StatusOK] {
		t.Errorf("expected one request to succeed (200), got codes %v", codes)
	}
	if !codes[http.StatusServiceUnavailable] {
		t.Errorf("expected one request to hit queue timeout (503), got codes %v", codes)
	}
}

func TestHandleProbeRelayContextCancelDuringQueue(t *testing.T) {
	fake := writeFakeFFprobe(t, "{}", 0, 500*time.Millisecond)
	s := testRelayServer(t, fake, 1)
	s.cfg.Relay.QueueTimeout = 5 * time.Second // longer than the client cancels at

	go doProbeRequest(s, relayRequest{URL: "http://darkharrbor:8381/stream/a/0"})
	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	b, _ := json.Marshal(relayRequest{URL: "http://darkharrbor:8381/stream/b/0"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/probe", bytes.NewReader(b)).WithContext(ctx)
	rec := httptest.NewRecorder()

	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	s.handleProbeRelay(rec, req)
	elapsed := time.Since(start)

	if elapsed > 1*time.Second {
		t.Fatalf("handler took %s to return after context cancel -- expected prompt return, not the full queue timeout", elapsed)
	}
}

func TestCapProbeArgs(t *testing.T) {
	in := []string{"-loglevel", "error", "-probesize", "50000000", "-analyzeduration", "50000000", "-show_streams"}
	out := capProbeArgs(in)
	if out[3] != "10000000" || out[5] != "10000000" {
		t.Fatalf("cap not applied: %v", out)
	}
	if in[3] != "50000000" {
		t.Fatalf("caller slice mutated")
	}
	// under-cap and non-numeric values untouched
	in2 := []string{"-probesize", "5M", "-analyzeduration", "1000"}
	out2 := capProbeArgs(in2)
	if out2[1] != "5M" || out2[3] != "1000" {
		t.Fatalf("under-cap values must pass through: %v", out2)
	}
}

func TestCapProbeArgsTo(t *testing.T) {
	in := []string{"-probesize", "50000000", "-analyzeduration", "50000000"}
	out := capProbeArgsTo(in, relayEscalatedProbeBytes)
	if out[1] != "50000000" || out[3] != "50000000" {
		t.Fatalf("value under the escalated limit must pass through unchanged: %v", out)
	}
	out2 := capProbeArgsTo(in, relayMaxProbeBytes)
	if out2[1] != "10000000" || out2[3] != "10000000" {
		t.Fatalf("value over the default limit must still be capped: %v", out2)
	}
}

func TestProbeResultMissingAudio(t *testing.T) {
	cases := []struct {
		name          string
		stdout        string
		wantMissing   bool
		wantParseable bool
	}{
		{"has audio", `{"streams":[{"codec_type":"video"},{"codec_type":"audio"}]}`, false, true},
		{"video only", `{"streams":[{"codec_type":"video"}]}`, true, true},
		{"empty streams", `{"streams":[]}`, true, true},
		{"malformed json", `not json`, false, false},
		{"no streams field at all", `{}`, false, false},
	}
	for _, c := range cases {
		missing, parseable := probeResultMissingAudio(c.stdout)
		if missing != c.wantMissing || parseable != c.wantParseable {
			t.Errorf("%s: probeResultMissingAudio(%q) = (%v, %v), want (%v, %v)",
				c.name, c.stdout, missing, parseable, c.wantMissing, c.wantParseable)
		}
	}
}

// writeArgAwareFakeFFprobe writes a shell script that inspects its own
// -probesize value and reports audio present only once that value exceeds
// threshold -- standing in for a real container whose audio track index sits
// past a small probe window, to test the escalation retry in
// handleProbeRelay deterministically without a real media file.
func writeArgAwareFakeFFprobe(t *testing.T, threshold int64) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ffprobe-full")
	script := `#!/bin/sh
probesize=0
prev=""
for arg in "$@"; do
  if [ "$prev" = "-probesize" ]; then
    probesize="$arg"
  fi
  prev="$arg"
done
if [ "$probesize" -gt ` + fmt.Sprintf("%d", threshold) + ` ] 2>/dev/null; then
  printf '%s' '{"streams":[{"codec_type":"video"},{"codec_type":"audio"}]}'
else
  printf '%s' '{"streams":[{"codec_type":"video"}]}'
fi
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // test fixture, fixed 0755 mode is correct for an executable
		t.Fatalf("write arg-aware fake ffprobe-full: %v", err)
	}
	return path
}

// writeArgAwareFakeFFprobeDelayAboveThreshold is like writeArgAwareFakeFFprobe
// but always reports no audio, sleeping first when -probesize exceeds
// threshold -- used to force the escalated retry specifically (not the
// capped first attempt) into a relay-level timeout, proving the handler
// falls back to the capped result rather than failing the whole request.
func writeArgAwareFakeFFprobeDelayAboveThreshold(t *testing.T, threshold int64, delay time.Duration) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ffprobe-full")
	script := `#!/bin/sh
probesize=0
prev=""
for arg in "$@"; do
  if [ "$prev" = "-probesize" ]; then
    probesize="$arg"
  fi
  prev="$arg"
done
if [ "$probesize" -gt ` + fmt.Sprintf("%d", threshold) + ` ] 2>/dev/null; then
  sleep ` + fmt.Sprintf("%f", delay.Seconds()) + `
fi
printf '%s' '{"streams":[{"codec_type":"video"}]}'
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // test fixture, fixed 0755 mode is correct for an executable
		t.Fatalf("write delayed arg-aware fake ffprobe-full: %v", err)
	}
	return path
}

func TestHandleProbeRelayEscalatesWhenCappedProbeFindsNoAudio(t *testing.T) {
	fake := writeArgAwareFakeFFprobe(t, relayMaxProbeBytes)
	s := testRelayServer(t, fake, 4)

	rec := doProbeRequest(s, relayRequest{
		URL:  "http://darkharrbor:8381/stream/abc/0",
		Args: []string{"-print_format", "json", "-show_streams", "-probesize", "50000000"},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp relayResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit_code = %d, want 0", resp.ExitCode)
	}
	if missing, parseable := probeResultMissingAudio(resp.Stdout); !parseable || missing {
		t.Fatalf("final result still missing audio after escalation: stdout=%q", resp.Stdout)
	}
}

func TestHandleProbeRelayEscalationFailureFallsBackToCappedResult(t *testing.T) {
	fake := writeArgAwareFakeFFprobeDelayAboveThreshold(t, relayMaxProbeBytes, 2*time.Second)
	s := testRelayServer(t, fake, 4)
	s.cfg.Relay.Timeout = 200 * time.Millisecond // shorter than the escalated call's sleep, longer than the capped call's

	rec := doProbeRequest(s, relayRequest{
		URL:  "http://darkharrbor:8381/stream/abc/0",
		Args: []string{"-print_format", "json", "-show_streams", "-probesize", "50000000"},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s -- escalation failure must not fail the whole request", rec.Code, rec.Body.String())
	}
	var resp relayResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit_code = %d, want 0 (capped result, which ran to completion)", resp.ExitCode)
	}
	if resp.Stdout != `{"streams":[{"codec_type":"video"}]}` {
		t.Fatalf("stdout = %q, want the capped (pre-escalation) result", resp.Stdout)
	}
}

func TestMarkProbeURL(t *testing.T) {
	got := markProbeURL("http://darkharrbor:8381/stream/abc/3?tok=xyz")
	if !strings.Contains(got, "probe=1") || !strings.Contains(got, "tok=xyz") {
		t.Fatalf("probe marker missing or token dropped: %q", got)
	}
}

func TestMagnetDisplayNameAndDerive(t *testing.T) {
	m := "magnet:?xt=urn:btih:37e4e0f487d7fa4d8d2ebc5efc7e6ec175d92af2&dn=All.in.the.Family.S05.DVDRip.x264-NOGRP%5Brartv%5D&xl=1"
	if got := magnetDisplayName(m); got != "All.in.the.Family.S05.DVDRip.x264-NOGRP[rartv]" {
		t.Fatalf("dn parse: %q", got)
	}
	if magnetDisplayName("http://x/y.torrent") != "" || magnetDisplayName("magnet:?xt=urn:btih:abc") != "" {
		t.Fatal("non-dn cases must be empty")
	}
	// degenerate hash display name recovers dn from source
	hash := "37e4e0f487d7fa4d8d2ebc5efc7e6ec175d92af2"
	if got := deriveDisplayName(hash, m, hash, "fb"); got != "All.in.the.Family.S05.DVDRip.x264-NOGRP[rartv]" {
		t.Fatalf("derive should recover dn, got %q", got)
	}
	// real names pass through untouched
	if got := deriveDisplayName("Real.Name.S01", m, hash, "fb"); got != "Real.Name.S01" {
		t.Fatalf("real name clobbered: %q", got)
	}
}

func TestMagnetCacheTitleFallback(t *testing.T) {
	s := &Server{magnetCache: make(map[string]magnetEntry)}
	ctx := context.Background()
	s.magnetCacheSet(ctx, "abc123", "magnet:?xt=urn:btih:abc123&tr=udp%3A%2F%2Fx", "Show.S05.DVDRip.x264-GRP")
	if got := s.magnetCacheTitle(ctx, "ABC123"); got != "Show.S05.DVDRip.x264-GRP" {
		t.Fatalf("title lookup: %q", got)
	}
	if m, ok := s.magnetCacheGet(ctx, "abc123"); !ok || m == "" {
		t.Fatal("magnet lookup broken")
	}
	if s.magnetCacheTitle(ctx, "missing") != "" {
		t.Fatal("missing hash must be empty")
	}
}
