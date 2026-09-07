package shiminstall

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/arrdiscovery"
	"github.com/darkharrbor/darkharrbor/internal/dockerctl"
)

func frame(streamType byte, payload []byte) []byte {
	header := make([]byte, 8)
	header[0] = streamType
	binary.BigEndian.PutUint32(header[4:8], uint32(len(payload)))
	return append(header, payload...)
}

// fakeFS is a tiny in-memory stand-in for "files inside the container",
// exercised entirely through the mocked exec commands this package issues --
// it never parses shell, it pattern-matches the exact command shapes
// Install()'s helper functions produce.
type fakeFS struct {
	mu    sync.Mutex
	files map[string][]byte // path -> content
}

func newFakeFS() *fakeFS { return &fakeFS{files: map[string][]byte{}} }

// runCommand interprets one exec'd "sh -c <cmd>" or direct-argv command
// against the fake filesystem and returns (stdout, exitCode).
func (fs *fakeFS) runCommand(argv []string) (string, int) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	if len(argv) >= 2 && argv[0] == "sh" && argv[1] == "-c" {
		cmd := argv[2]
		switch {
		case strings.HasPrefix(cmd, "if head -c 8192"):
			return fs.handleProbe(cmd)
		case strings.HasPrefix(cmd, "test -e"):
			return fs.handleBackup(cmd)
		case strings.HasPrefix(cmd, "rm -f "):
			return fs.handleWrite(cmd)
		}
		return "unrecognized command: " + cmd, 1
	}

	// Direct-argv form: verify() calls Exec(ctx, id, "root", []string{path, "-version"}).
	if len(argv) == 2 && argv[1] == "-version" {
		path := argv[0]
		if _, ok := fs.files[path]; ok {
			return "ffprobe version 8.1.2\n", 0
		}
		return "not found", 127
	}

	return "unrecognized argv: " + strings.Join(argv, " "), 1
}

func (fs *fakeFS) handleProbe(cmd string) (string, int) {
	path := extractQuoted(cmd)
	content, ok := fs.files[path]
	wrapped := 0
	sha := ""
	if ok {
		if strings.Contains(string(content), wrapperMarker) {
			wrapped = 1
		}
		sum := sha256.Sum256(content)
		sha = hex.EncodeToString(sum[:])
	}
	return fmt.Sprintf("WRAPPED=%d SHA=%s", wrapped, sha), 0
}

func (fs *fakeFS) handleBackup(cmd string) (string, int) {
	// cmd shape: test -e "<path>.real" || cp -a "<path>" "<path>.real"
	quoted := extractAllQuoted(cmd)
	if len(quoted) < 3 {
		return "malformed backup cmd", 1
	}
	backupPath := quoted[0]
	srcPath := quoted[1]
	if _, exists := fs.files[backupPath]; !exists {
		src, ok := fs.files[srcPath]
		if !ok {
			return "source missing", 1
		}
		fs.files[backupPath] = append([]byte(nil), src...)
	}
	return "", 0
}

func (fs *fakeFS) handleWrite(cmd string) (string, int) {
	// cmd shape: rm -f "<tmp>" && printf %s <b64> | base64 -d > "<tmp>" ...
	const marker = "printf %s "
	start := strings.Index(cmd, marker)
	if start < 0 {
		return "malformed write cmd", 1
	}
	payload := cmd[start+len(marker):]
	end := strings.IndexByte(payload, ' ')
	if end < 1 {
		return "malformed write payload", 1
	}
	b64 := payload[:end]
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "bad base64: " + err.Error(), 1
	}
	quoted := extractAllQuoted(cmd)
	if len(quoted) < 2 {
		return "malformed write cmd (paths)", 1
	}
	finalPath := quoted[len(quoted)-1]
	fs.files[finalPath] = decoded
	return "", 0
}

// extractQuoted returns the first double-quoted substring in s (Go's %q
// output uses standard double quotes for these simple paths).
func extractQuoted(s string) string {
	all := extractAllQuoted(s)
	if len(all) == 0 {
		return ""
	}
	return all[0]
}

func extractAllQuoted(s string) []string {
	var out []string
	inQuote := false
	var cur strings.Builder
	for _, r := range s {
		if r == '"' {
			if inQuote {
				out = append(out, cur.String())
				cur.Reset()
			}
			inQuote = !inQuote
			continue
		}
		if inQuote {
			cur.WriteRune(r)
		}
	}
	return out
}

func mockShimProxy(t *testing.T, fs *fakeFS, containerID string) *httptest.Server {
	t.Helper()
	var lastArgv map[string][]string = map[string][]string{}
	var mu sync.Mutex

	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ApiVersion":"1.51"}`))
	})
	mux.HandleFunc("/v1.51/containers/"+containerID+"/exec", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Cmd []string `json:"Cmd"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode exec create: %v", err)
		}
		execID := fmt.Sprintf("exec-%d", len(lastArgv)+1)
		mu.Lock()
		lastArgv[execID] = req.Cmd
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"Id": execID})
	})

	execExit := map[string]int{}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/start"):
			execID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.51/exec/"), "/start")
			mu.Lock()
			argv := lastArgv[execID]
			mu.Unlock()
			stdout, exit := fs.runCommand(argv)
			mu.Lock()
			execExit[execID] = exit
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(frame(1, []byte(stdout)))
		case strings.HasSuffix(r.URL.Path, "/json") && strings.Contains(r.URL.Path, "/exec/"):
			execID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.51/exec/"), "/json")
			mu.Lock()
			exit := execExit[execID]
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]int{"ExitCode": exit})
		default:
			http.Error(w, "unexpected path in mock: "+r.URL.Path, http.StatusNotFound)
		}
	})
	return httptest.NewServer(mux)
}

func testTarget(containerID string) arrdiscovery.ArrContainer {
	return arrdiscovery.ArrContainer{
		ContainerID: containerID,
		Name:        "sonarr-test",
		Image:       "lscr.io/linuxserver/sonarr:latest",
		App:         arrdiscovery.AppSonarr,
		FFprobePath: "/app/sonarr/bin/ffprobe",
	}
}

func TestInstallFreshStockBinary(t *testing.T) {
	fs := newFakeFS()
	fs.files["/app/sonarr/bin/ffprobe"] = []byte("#!/bin/sh\necho stock ffprobe\n") // stock, unwrapped

	srv := mockShimProxy(t, fs, "c1")
	defer srv.Close()
	client := dockerctl.New(srv.URL, nil)

	result, err := Install(context.Background(), client, testTarget("c1"))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if result.Action != ActionInstalled {
		t.Fatalf("expected ActionInstalled, got %s", result.Action)
	}
	if !result.Verified {
		t.Fatalf("expected Verified=true")
	}
	if _, ok := fs.files["/app/sonarr/bin/ffprobe.real"]; !ok {
		t.Fatalf("expected original backed up to .real")
	}
	if !strings.Contains(string(fs.files["/app/sonarr/bin/ffprobe"]), wrapperMarker) {
		t.Fatalf("expected wrapper marker present after install")
	}
}

func TestInstallAlreadyCurrentNoOp(t *testing.T) {
	fs := newFakeFS()
	fs.files["/app/sonarr/bin/ffprobe"] = wrapperScript // exact current embedded content

	srv := mockShimProxy(t, fs, "c1")
	defer srv.Close()
	client := dockerctl.New(srv.URL, nil)

	result, err := Install(context.Background(), client, testTarget("c1"))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if result.Action != ActionAlreadyCurrent {
		t.Fatalf("expected ActionAlreadyCurrent, got %s", result.Action)
	}
	if _, backedUp := fs.files["/app/sonarr/bin/ffprobe.real"]; backedUp {
		t.Fatalf("no-op path must not create a backup")
	}
}

func TestInstallReinstallsOnDrift(t *testing.T) {
	fs := newFakeFS()
	// Wrapped, but an older/different version of the wrapper (has the marker,
	// different bytes -> different checksum).
	fs.files["/app/sonarr/bin/ffprobe"] = []byte("#!/bin/sh\n# " + wrapperMarker + " v0 (old)\necho old\n")
	// A backup already exists from the original first install -- must NOT be
	// overwritten by a drift-triggered reinstall.
	fs.files["/app/sonarr/bin/ffprobe.real"] = []byte("ORIGINAL-STOCK-BINARY")

	srv := mockShimProxy(t, fs, "c1")
	defer srv.Close()
	client := dockerctl.New(srv.URL, nil)

	result, err := Install(context.Background(), client, testTarget("c1"))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if result.Action != ActionReinstalled {
		t.Fatalf("expected ActionReinstalled, got %s", result.Action)
	}
	if string(fs.files["/app/sonarr/bin/ffprobe.real"]) != "ORIGINAL-STOCK-BINARY" {
		t.Fatalf("drift reinstall must not touch the existing backup")
	}
	if !strings.Contains(string(fs.files["/app/sonarr/bin/ffprobe"]), wrapperMarker) {
		t.Fatalf("expected current wrapper content after reinstall")
	}
}

func TestInstallMissingFFprobePathIsError(t *testing.T) {
	fs := newFakeFS()
	srv := mockShimProxy(t, fs, "c1")
	defer srv.Close()
	client := dockerctl.New(srv.URL, nil)

	target := testTarget("c1")
	target.FFprobePath = ""
	if _, err := Install(context.Background(), client, target); err == nil {
		t.Fatal("expected error for empty FFprobePath")
	}
}
