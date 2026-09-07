package arrdiscovery

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/dockerctl"
)

// frame builds one Docker stream-protocol frame (stdout=1) for exec output.
func frame(streamType byte, payload []byte) []byte {
	header := make([]byte, 8)
	header[0] = streamType
	binary.BigEndian.PutUint32(header[4:8], uint32(len(payload)))
	return append(header, payload...)
}

// mockProxy stands in for harrbor-dockerproxy: serves a fixed container list
// and, for exec calls, returns the ffprobe path configured per container ID
// in ffprobeByID (empty string => no match found, simulating a missing
// glob result).
func mockProxy(t *testing.T, containers []dockerctl.Container, ffprobeByID map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ApiVersion":"1.51"}`))
	})
	mux.HandleFunc("/v1.51/containers/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(containers)
	})

	execIDToContainer := map[string]string{}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// crude router for the two remaining exec endpoints, keyed by path shape
		switch {
		case r.Method == http.MethodPost && len(r.URL.Path) > len("/v1.51/containers/") && r.URL.Path[len(r.URL.Path)-5:] == "/exec":
			id := r.URL.Path[len("/v1.51/containers/") : len(r.URL.Path)-len("/exec")]
			execID := "exec-" + id
			execIDToContainer[execID] = id
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": execID})
		case r.Method == http.MethodPost && len(r.URL.Path) > len("/v1.51/exec/") && r.URL.Path[len(r.URL.Path)-6:] == "/start":
			execID := r.URL.Path[len("/v1.51/exec/") : len(r.URL.Path)-len("/start")]
			cid := execIDToContainer[execID]
			path := ffprobeByID[cid]
			w.WriteHeader(http.StatusOK)
			if path == "" {
				_, _ = w.Write(frame(1, []byte(""))) // simulate empty glob result
			} else {
				_, _ = w.Write(frame(1, []byte(path+"\n")))
			}
		case r.Method == http.MethodGet && len(r.URL.Path) > len("/v1.51/exec/") && r.URL.Path[len(r.URL.Path)-5:] == "/json":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]int{"ExitCode": 0})
		default:
			http.Error(w, "unexpected path in mock", http.StatusNotFound)
		}
	})
	return httptest.NewServer(mux)
}

func TestDiscoverMatchesOnlySonarrRadarr(t *testing.T) {
	containers := []dockerctl.Container{
		{ID: "sonarr1", Names: []string{"/sonarr-modern"}, Image: "lscr.io/linuxserver/sonarr:latest", State: "running"},
		{ID: "radarr1", Names: []string{"/radarr"}, Image: "lscr.io/linuxserver/radarr:latest", State: "running"},
		{ID: "prowlarr1", Names: []string{"/prowlarr"}, Image: "lscr.io/linuxserver/prowlarr:latest", State: "running"},
		{ID: "qbit1", Names: []string{"/qbittorrent"}, Image: "lscr.io/linuxserver/qbittorrent:latest", State: "running"},
		{ID: "dh1", Names: []string{"/darkharrbor"}, Image: "darkharrbor/darkharrbor:latest", State: "running"},
		{ID: "jelly1", Names: []string{"/jellyfin"}, Image: "jellyfin/jellyfin:latest", State: "running"},
	}
	ffprobe := map[string]string{
		"sonarr1": "/app/sonarr/bin/ffprobe",
		"radarr1": "/app/radarr/bin/ffprobe",
	}

	srv := mockProxy(t, containers, ffprobe)
	defer srv.Close()

	client := dockerctl.New(srv.URL, nil)
	got, err := Discover(context.Background(), client)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 matches (sonarr+radarr), got %d: %+v", len(got), got)
	}
	byApp := map[App]ArrContainer{}
	for _, m := range got {
		byApp[m.App] = m
	}
	if byApp[AppSonarr].FFprobePath != "/app/sonarr/bin/ffprobe" {
		t.Fatalf("unexpected sonarr ffprobe path: %+v", byApp[AppSonarr])
	}
	if byApp[AppRadarr].FFprobePath != "/app/radarr/bin/ffprobe" {
		t.Fatalf("unexpected radarr ffprobe path: %+v", byApp[AppRadarr])
	}
	// Explicit zero-non-arr-containers check (Gate 2 AC).
	for _, m := range got {
		if m.App != AppSonarr && m.App != AppRadarr {
			t.Fatalf("non-arr container leaked into result: %+v", m)
		}
	}
}

func TestDiscoverSkipsStoppedContainer(t *testing.T) {
	containers := []dockerctl.Container{
		{ID: "sonarr1", Names: []string{"/sonarr-stopped"}, Image: "lscr.io/linuxserver/sonarr:latest", State: "exited"},
	}
	srv := mockProxy(t, containers, nil)
	defer srv.Close()

	client := dockerctl.New(srv.URL, nil)
	got, err := Discover(context.Background(), client)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 matches for a stopped container, got %d: %+v", len(got), got)
	}
}

func TestDiscoverSkipsUnresolvableFFprobe(t *testing.T) {
	containers := []dockerctl.Container{
		{ID: "sonarr1", Names: []string{"/sonarr-weird"}, Image: "lscr.io/linuxserver/sonarr:latest", State: "running"},
	}
	// No entry in ffprobeByID for "sonarr1" -> empty glob result -> unresolvable.
	srv := mockProxy(t, containers, map[string]string{})
	defer srv.Close()

	client := dockerctl.New(srv.URL, nil)
	got, err := Discover(context.Background(), client)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 matches when ffprobe path can't be resolved, got %d: %+v", len(got), got)
	}
}

func TestMatchAppExactness(t *testing.T) {
	cases := map[string]App{
		"lscr.io/linuxserver/sonarr:latest":      AppSonarr,
		"lscr.io/linuxserver/radarr:4.0.1":       AppRadarr,
		"lscr.io/linuxserver/prowlarr:latest":    "",
		"lscr.io/linuxserver/qbittorrent:latest": "",
		"darkharrbor/darkharrbor:latest":         "",
		"someregistry/mysonarrclone:latest":      "", // must not substring-match
		"lscr.io/linuxserver/sonarr":             AppSonarr,
	}
	for image, want := range cases {
		if got := MatchApp(image); got != want {
			t.Errorf("matchApp(%q) = %q, want %q", image, got, want)
		}
	}
}
