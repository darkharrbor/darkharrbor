package eventwatcher

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/dockerctl"
)

// ---- a small fake Docker daemon, generic enough to drive the real
// arrdiscovery + shiminstall logic through dockerctl, the same way the real
// proxy would. Reused/extended from the per-package mocks in dockerctl,
// arrdiscovery, and shiminstall's own test files, but combined here since
// Gate 4 is specifically about those three working together correctly.

type fakeContainer struct {
	id      string
	name    string
	image   string
	running bool
	files   map[string][]byte
}

type fakeDockerd struct {
	mu         sync.Mutex
	containers map[string]*fakeContainer
	events     chan dockerctl.Event
	lastArgv   map[string][]string
	execExit   map[string]int
	execN      int
}

func newFakeDockerd() *fakeDockerd {
	return &fakeDockerd{
		containers: map[string]*fakeContainer{},
		events:     make(chan dockerctl.Event, 16),
		lastArgv:   map[string][]string{},
		execExit:   map[string]int{},
	}
}

func globMatch(pattern, path string) bool {
	re := "^" + regexp.QuoteMeta(pattern)
	re = strings.ReplaceAll(re, regexp.QuoteMeta("*"), "[^/]+") + "$"
	matched, _ := regexp.MatchString(re, path)
	return matched
}

func (d *fakeDockerd) runCommand(c *fakeContainer, argv []string) (string, int) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if len(argv) >= 2 && argv[0] == "sh" && argv[1] == "-c" {
		cmd := argv[2]
		switch {
		case strings.Contains(cmd, "ls -1 /app/*/bin/ffprobe"):
			var matches []string
			for path := range c.files {
				if globMatch("/app/*/bin/ffprobe", path) {
					matches = append(matches, path)
				}
			}
			return strings.Join(matches, "\n"), 0
		case strings.HasPrefix(cmd, "if head -c 8192"):
			path := extractQuoted(cmd)
			content, ok := c.files[path]
			wrapped := 0
			sha := ""
			if ok {
				if strings.Contains(string(content), "HARRBOR_STRM_WRAPPER") {
					wrapped = 1
				}
				sum := sha256.Sum256(content)
				sha = fmt.Sprintf("%x", sum)
			}
			return fmt.Sprintf("WRAPPED=%d SHA=%s", wrapped, sha), 0
		case strings.HasPrefix(cmd, "test -e"):
			quoted := extractAllQuoted(cmd)
			if len(quoted) < 2 {
				return "malformed", 1
			}
			backupPath, srcPath := quoted[0], quoted[1]
			if _, exists := c.files[backupPath]; !exists {
				src, ok := c.files[srcPath]
				if !ok {
					return "source missing", 1
				}
				c.files[backupPath] = append([]byte(nil), src...)
			}
			return "", 0
		case strings.HasPrefix(cmd, "rm -f "):
			const marker = "printf %s "
			start := strings.Index(cmd, marker)
			if start < 0 {
				return "malformed", 1
			}
			payload := cmd[start+len(marker):]
			end := strings.IndexByte(payload, ' ')
			if end < 1 {
				return "malformed payload", 1
			}
			decoded, err := base64.StdEncoding.DecodeString(payload[:end])
			if err != nil {
				return "bad base64", 1
			}
			quoted := extractAllQuoted(cmd)
			if len(quoted) < 2 {
				return "malformed paths", 1
			}
			c.files[quoted[len(quoted)-1]] = decoded
			return "", 0
		}
		return "unrecognized: " + cmd, 1
	}

	if len(argv) == 2 && argv[1] == "-version" {
		if _, ok := c.files[argv[0]]; ok {
			return "ffprobe version 8.1.2\n", 0
		}
		return "not found", 127
	}
	return "unrecognized argv", 1
}

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

func frame(streamType byte, payload []byte) []byte {
	header := make([]byte, 8)
	header[0] = streamType
	binary.BigEndian.PutUint32(header[4:8], uint32(len(payload)))
	return append(header, payload...)
}

func (d *fakeDockerd) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ApiVersion":"1.51"}`))
	})

	mux.HandleFunc("/v1.51/containers/json", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		var out []dockerctl.Container
		for _, c := range d.containers {
			state := "exited"
			if c.running {
				state = "running"
			}
			out = append(out, dockerctl.Container{ID: c.id, Names: []string{"/" + c.name}, Image: c.image, State: state})
		}
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})

	mux.HandleFunc("/v1.51/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		enc := json.NewEncoder(w)
		for {
			select {
			case ev := <-d.events:
				_ = enc.Encode(ev)
				if flusher != nil {
					flusher.Flush()
				}
			case <-r.Context().Done():
				return
			}
		}
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/exec") && strings.HasSuffix(r.URL.Path, "/exec"):
			// /v1.51/containers/{id}/exec
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.51/containers/"), "/exec")
			var req struct {
				Cmd []string `json:"Cmd"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			d.mu.Lock()
			d.execN++
			execID := fmt.Sprintf("exec-%d", d.execN)
			d.lastArgv[execID] = req.Cmd
			d.containersLocked(id) // no-op, just documents id lookup happens at start time
			d.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": execID, "cid": id})
			// stash the container id alongside the exec id
			d.mu.Lock()
			d.lastArgv["cid:"+execID] = []string{id}
			d.mu.Unlock()
		case strings.HasSuffix(r.URL.Path, "/start"):
			execID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.51/exec/"), "/start")
			d.mu.Lock()
			argv := d.lastArgv[execID]
			cidArgv := d.lastArgv["cid:"+execID]
			d.mu.Unlock()
			var cid string
			if len(cidArgv) == 1 {
				cid = cidArgv[0]
			}
			d.mu.Lock()
			c := d.containers[cid]
			d.mu.Unlock()
			if c == nil {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(frame(1, []byte("no such container")))
				return
			}
			stdout, exit := d.runCommand(c, argv)
			d.mu.Lock()
			d.execExit[execID] = exit
			d.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(frame(1, []byte(stdout)))
		case strings.HasSuffix(r.URL.Path, "/json") && strings.Contains(r.URL.Path, "/exec/"):
			execID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.51/exec/"), "/json")
			d.mu.Lock()
			exit := d.execExit[execID]
			d.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]int{"ExitCode": exit})
		default:
			http.Error(w, "unexpected path in fakeDockerd: "+r.URL.Path, http.StatusNotFound)
		}
	})

	return httptest.NewServer(mux)
}

func (d *fakeDockerd) containersLocked(id string) *fakeContainer { return d.containers[id] }

// testEvent builds a dockerctl.Event without repeating its Actor's anonymous
// struct type (which must match exactly, tags included, at each call site).
func testEvent(typ, action, id, image, name string) dockerctl.Event {
	var ev dockerctl.Event
	ev.Type = typ
	ev.Action = action
	ev.Actor.ID = id
	ev.Actor.Attributes = map[string]string{"image": image, "name": name}
	return ev
}

// ---- tests ----

func TestReconcileInstallsIntoDiscoveredContainer(t *testing.T) {
	d := newFakeDockerd()
	d.containers["c1"] = &fakeContainer{
		id: "c1", name: "sonarr-modern", image: "lscr.io/linuxserver/sonarr:latest", running: true,
		files: map[string][]byte{"/app/sonarr/bin/ffprobe": []byte("#!/bin/sh\necho stock\n")},
	}
	srv := d.server(t)
	defer srv.Close()
	client := dockerctl.New(srv.URL, nil)

	reconcile(context.Background(), client, nil)

	d.mu.Lock()
	defer d.mu.Unlock()
	content := d.containers["c1"].files["/app/sonarr/bin/ffprobe"]
	if !strings.Contains(string(content), "HARRBOR_STRM_WRAPPER") {
		t.Fatalf("expected wrapper installed after reconcile, got: %s", content)
	}
	if _, ok := d.containers["c1"].files["/app/sonarr/bin/ffprobe.real"]; !ok {
		t.Fatalf("expected original backed up")
	}
}

func TestHandleEventIgnoresNonContainerAndNonArrImages(t *testing.T) {
	d := newFakeDockerd()
	d.containers["c1"] = &fakeContainer{id: "c1", name: "jellyfin", image: "jellyfin/jellyfin:latest", running: true, files: map[string][]byte{}}
	srv := d.server(t)
	defer srv.Close()
	client := dockerctl.New(srv.URL, nil)

	before := d.execN
	handleEvent(context.Background(), client, testEvent("container", "start", "c1", "jellyfin/jellyfin:latest", "jellyfin"), nil)
	d.mu.Lock()
	after := d.execN
	d.mu.Unlock()
	if after != before {
		t.Fatalf("expected zero exec calls for a non-arr image start event, got %d", after-before)
	}
}

func TestHandleEventIgnoresCreateAction(t *testing.T) {
	d := newFakeDockerd()
	d.containers["c1"] = &fakeContainer{
		id: "c1", name: "sonarr-modern", image: "lscr.io/linuxserver/sonarr:latest", running: false,
		files: map[string][]byte{"/app/sonarr/bin/ffprobe": []byte("#!/bin/sh\necho stock\n")},
	}
	srv := d.server(t)
	defer srv.Close()
	client := dockerctl.New(srv.URL, nil)

	before := d.execN
	handleEvent(context.Background(), client, testEvent("container", "create", "c1", "lscr.io/linuxserver/sonarr:latest", "sonarr-modern"), nil)
	d.mu.Lock()
	after := d.execN
	d.mu.Unlock()
	if after != before {
		t.Fatalf("expected zero exec calls for a create event, got %d", after-before)
	}
}

func TestHandleEventInstallsOnStart(t *testing.T) {
	d := newFakeDockerd()
	d.containers["c1"] = &fakeContainer{
		id: "c1", name: "radarr", image: "lscr.io/linuxserver/radarr:latest", running: true,
		files: map[string][]byte{"/app/radarr/bin/ffprobe": []byte("#!/bin/sh\necho stock\n")},
	}
	srv := d.server(t)
	defer srv.Close()
	client := dockerctl.New(srv.URL, nil)

	handleEvent(context.Background(), client, testEvent("container", "start", "c1", "lscr.io/linuxserver/radarr:latest", "radarr"), nil)

	d.mu.Lock()
	content := d.containers["c1"].files["/app/radarr/bin/ffprobe"]
	d.mu.Unlock()
	if !strings.Contains(string(content), "HARRBOR_STRM_WRAPPER") {
		t.Fatalf("expected wrapper installed after start event, got: %s", content)
	}
}

func TestRunStopsCleanlyOnContextCancel(t *testing.T) {
	d := newFakeDockerd()
	srv := d.server(t)
	defer srv.Close()
	client := dockerctl.New(srv.URL, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, client, nil)
	}()

	time.Sleep(50 * time.Millisecond) // let Run get into its watch loop
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected Run to return a context error, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of context cancellation -- possible goroutine leak")
	}
}

func TestReconcileAndEventsRespectConfiguredArrNames(t *testing.T) {
	d := newFakeDockerd()
	d.containers["c1"] = &fakeContainer{
		id: "c1", name: "radarr-main", image: "lscr.io/linuxserver/radarr:latest", running: true,
		files: map[string][]byte{"/app/radarr/bin/ffprobe": []byte("#!/bin/sh\necho stock\n")},
	}
	d.containers["c2"] = &fakeContainer{
		id: "c2", name: "radarr-kids", image: "lscr.io/linuxserver/radarr:latest", running: true,
		files: map[string][]byte{"/app/radarr/bin/ffprobe": []byte("#!/bin/sh\necho stock\n")},
	}
	srv := d.server(t)
	defer srv.Close()
	client := dockerctl.New(srv.URL, nil)
	allowed := nameAllowlist([]string{"radarr-main"})

	reconcile(context.Background(), client, allowed)
	handleEvent(context.Background(), client, testEvent("container", "start", "c2", "lscr.io/linuxserver/radarr:latest", "radarr-kids"), allowed)

	d.mu.Lock()
	mainContent := d.containers["c1"].files["/app/radarr/bin/ffprobe"]
	kidsContent := d.containers["c2"].files["/app/radarr/bin/ffprobe"]
	d.mu.Unlock()
	if !strings.Contains(string(mainContent), "HARRBOR_STRM_WRAPPER") {
		t.Fatal("selected Arr did not receive wrapper")
	}
	if strings.Contains(string(kidsContent), "HARRBOR_STRM_WRAPPER") {
		t.Fatal("unselected Arr received wrapper")
	}
}

func TestExplicitEmptyAllowlistDeniesAllArrs(t *testing.T) {
	if nameAllowed(nameAllowlist([]string{}), "radarr") {
		t.Fatal("explicit empty configured-name list allowed an Arr")
	}
	if !nameAllowed(nameAllowlist(nil), "radarr") {
		t.Fatal("nil legacy allowlist did not preserve compatibility")
	}
}
