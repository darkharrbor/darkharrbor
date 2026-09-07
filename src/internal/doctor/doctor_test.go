package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/arrsetup"
	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/publicip"
	"github.com/darkharrbor/darkharrbor/internal/store"
	"github.com/darkharrbor/darkharrbor/internal/topology"
)

type fakeErr struct{ msg string }

func (e *fakeErr) Error() string { return e.msg }

func TestErrClassRedactsURLs(t *testing.T) {
	err := &fakeErr{msg: `Get "http://sonarr-modern:8989/api/v3/system/status?apikey=SECRET123": dial tcp: connect refused`}
	got := errClass(err)
	if !strings.Contains(got, "<redacted-url>") || strings.Contains(got, "sonarr-modern") {
		t.Fatalf("errClass did not redact: got %q", got)
	}
	if strings.Contains(got, "SECRET123") {
		t.Fatalf("errClass leaked secret: %q", got)
	}
}

func TestErrClassPassesThroughPlainMessages(t *testing.T) {
	err := &fakeErr{msg: "HTTP 401"}
	if got := errClass(err); got != "HTTP 401" {
		t.Fatalf("got %q", got)
	}
}

func TestDesiredBaseArrTopologyHonorsAppliedPlan(t *testing.T) {
	db, err := store.Open(t.Context(), t.TempDir()+"/doctor.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO app_config(key,value,updated_at) VALUES('onboarding_plan','{"base_arr_topology":false}','now')`); err != nil {
		t.Fatal(err)
	}
	desired := desiredArrTopologyFromDB(db)
	if desired.Selected {
		t.Fatal("doctor ignored declined base topology")
	}
	report := &Report{}
	runDesiredArrTopology(t.Context(), report, arrsetup.ArrTarget{Name: "movies"}, desired)
	if len(report.Checks) != 1 || report.Checks[0].Status != StatusSkip || !strings.Contains(report.Checks[0].Detail, "declined") {
		t.Fatalf("checks=%+v", report.Checks)
	}
}

func TestDesiredSetupPlanRetainsJellyfinChoice(t *testing.T) {
	db, err := store.Open(t.Context(), t.TempDir()+"/doctor.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO app_config(key,value,updated_at) VALUES('onboarding_plan','{"topology":"t1","jellyfin":true}','now')`); err != nil {
		t.Fatal(err)
	}
	plan, ok := readDesiredSetupPlan(db)
	if !ok || !plan.Jellyfin {
		t.Fatalf("plan=%+v ok=%t", plan, ok)
	}
}

func TestProwlarrReachabilityUsesV1StatusAndRejectsBadKey(t *testing.T) {
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if r.Header.Get("X-Api-Key") != "prowlarr-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	report := &Report{}
	runArrReachability(t.Context(), report, arrsetup.ArrTarget{Name: "search", URL: server.URL, AppType: "prowlarr", APIKey: "prowlarr-key"}, func(*http.Response) {})
	if path != "/api/v1/system/status" || len(report.Checks) != 1 || report.Checks[0].Status != StatusOK {
		t.Fatalf("path=%q checks=%+v", path, report.Checks)
	}

	report = &Report{}
	runArrReachability(t.Context(), report, arrsetup.ArrTarget{Name: "search", URL: server.URL, AppType: "prowlarr", APIKey: "wrong"}, func(*http.Response) {})
	if len(report.Checks) != 1 || report.Checks[0].Status != StatusFail || !strings.Contains(report.Checks[0].Detail, "API key rejected") {
		t.Fatalf("checks=%+v", report.Checks)
	}
}

func TestSetupPlanDriftNamesSelectedMissingPrerequisites(t *testing.T) {
	db, err := store.Open(t.Context(), t.TempDir()+"/doctor.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	plan := `{"topology":"t3","acquisition_lanes":["http"],"aggregator":true,"reactive_commit":true,"promotion":false,"wrapper_repair":true}`
	if _, err := db.Exec(`INSERT INTO app_config(key,value,updated_at) VALUES('onboarding_plan',?,'now')`, plan); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Topology.Active = topology.T3
	cfg.MediaFlow.Password = "configured"
	cfg.Reactive.Enabled = true
	cfg.Reactive.CommitMode = "supervised"
	report := &Report{}
	runSetupPlanDrift(report, db, cfg)
	if len(report.Checks) != 1 || report.Checks[0].Status != StatusFail || !strings.Contains(report.Checks[0].Detail, "HTTP backends") {
		t.Fatalf("checks=%+v", report.Checks)
	}
	cfg.HTTPStream.Enabled = true
	cfg.HTTPStream.Backends = []config.HTTPBackend{{Name: "archive", Type: "ia"}}
	report = &Report{}
	runSetupPlanDrift(report, db, cfg)
	if len(report.Checks) != 1 || report.Checks[0].Status != StatusFail || !strings.Contains(report.Checks[0].Detail, "wrapper repair choice") {
		t.Fatalf("checks=%+v", report.Checks)
	}
	cfg.StrmInstaller.Enabled = true
	report = &Report{}
	runSetupPlanDrift(report, db, cfg)
	if len(report.Checks) != 1 || report.Checks[0].Status != StatusOK {
		t.Fatalf("checks=%+v", report.Checks)
	}
}

func TestMediaFlowPublicIPDoctorReportsDegradedLastGood(t *testing.T) {
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("203.0.113.8"))
	}))
	defer srv.Close()
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	resolver := publicip.New(t.Context(), publicip.Options{
		Endpoints: []string{srv.URL}, RefreshInterval: time.Minute,
		StaleAfter: 2 * time.Minute, Now: func() time.Time { return now },
	})
	defer resolver.Close()
	if _, err := resolver.Resolve(t.Context()); err != nil {
		t.Fatal(err)
	}
	fail = true
	now = now.Add(3 * time.Minute)
	if _, err := resolver.Resolve(t.Context()); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline) && resolver.Status().Refreshing; {
		time.Sleep(time.Millisecond)
	}
	cfg := &config.Config{}
	cfg.MediaFlow.Password = "configured"
	r := &Report{}
	runMediaFlowPublicIP(t.Context(), r, cfg, resolver, true)
	if len(r.Checks) != 1 || r.Checks[0].Status != StatusWarn || !strings.Contains(r.Checks[0].Detail, "stale last known good") {
		t.Fatalf("checks = %+v", r.Checks)
	}
}

func TestMediaFlowPublicIPDoctorNamesEmptyCacheFailureWithoutEndpoint(t *testing.T) {
	lookup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream detail", http.StatusServiceUnavailable)
	}))
	defer lookup.Close()
	resolver := publicip.New(t.Context(), publicip.Options{Endpoints: []string{lookup.URL + "?token=secret-value"}})
	defer resolver.Close()
	cfg := &config.Config{}
	cfg.MediaFlow.Password = "configured"
	r := &Report{}
	runMediaFlowPublicIP(t.Context(), r, cfg, resolver, true)
	if len(r.Checks) != 1 || r.Checks[0].Status != StatusFail || !strings.Contains(r.Checks[0].Detail, "no last known good") {
		t.Fatalf("checks = %+v", r.Checks)
	}
	if strings.Contains(r.Checks[0].Detail, "secret-value") || strings.Contains(r.Checks[0].Detail, lookup.URL) {
		t.Fatalf("doctor detail leaked lookup endpoint: %q", r.Checks[0].Detail)
	}
}

func TestMediaFlowPasswordIsRequiredForSelectedAggregatorTopology(t *testing.T) {
	cfg := &config.Config{}
	cfg.Topology.Active = topology.T3
	report := &Report{}
	runMediaFlowPublicIP(t.Context(), report, cfg, nil, true)
	if len(report.Checks) != 1 || report.Checks[0].Status != StatusFail || !strings.Contains(report.Checks[0].Detail, "HARRBOR_MEDIAFLOW_PASSWORD") {
		t.Fatalf("checks=%+v", report.Checks)
	}
}

func TestMediaFlowPasswordSkippedWhenAggregatorDeclined(t *testing.T) {
	report := &Report{}
	runMediaFlowPublicIP(t.Context(), report, &config.Config{}, nil, false)
	if len(report.Checks) != 1 || report.Checks[0].Status != StatusSkip {
		t.Fatalf("checks=%+v", report.Checks)
	}
}

func TestHostOnly(t *testing.T) {
	cases := map[string]string{
		"http://sonarr-modern:8989": "sonarr-modern",
		"https://radarr:7878/":      "radarr",
		"http://prowlarr:9696/base": "prowlarr",
		"not-a-url":                 "not-a-url",
	}
	for in, want := range cases {
		if got := hostOnly(in); got != want {
			t.Errorf("hostOnly(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	if got := humanBytes(500); got != "500 B" {
		t.Errorf("got %q", got)
	}
	if got := humanBytes(2048); got != "2.0 KiB" {
		t.Errorf("got %q", got)
	}
}

func TestReportOK(t *testing.T) {
	r := &Report{}
	r.add(Check{Status: StatusOK})
	r.add(Check{Status: StatusSkip})
	r.add(Check{Status: StatusWarn})
	if !r.OK() {
		t.Fatalf("expected OK with only ok/skip/warn checks")
	}
	r.add(Check{Status: StatusFail})
	if r.OK() {
		t.Fatalf("expected not-OK once a fail check is present")
	}
}

type fakePlaybackCoverage struct {
	at    time.Time
	found bool
	err   error
}

func (f fakePlaybackCoverage) LatestPlaybackCoverage(context.Context, string) (time.Time, bool, error) {
	return f.at, f.found, f.err
}

func TestRunDeploymentTopology(t *testing.T) {
	now := time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)
	aggregator := []config.HTTPBackend{{Name: "aggregator", Type: "stremio"}}
	t.Run("T1 is available", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Topology.Active = topology.T1
		r := &Report{}
		runDeploymentTopology(context.Background(), r, cfg, fakePlaybackCoverage{err: errors.New("must not read")}, now)
		if len(r.Checks) != 1 || r.Checks[0].Status != StatusOK || r.Checks[0].Name != "t1" {
			t.Fatalf("checks=%+v", r.Checks)
		}
	})

	t.Run("T2 proxy is available", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Topology.Active = topology.T2
		r := &Report{}
		runDeploymentTopology(context.Background(), r, cfg, nil, now)
		if len(r.Checks) != 1 || r.Checks[0].Status != StatusWarn || !r.OK() {
			t.Fatalf("checks=%+v ok=%v", r.Checks, r.OK())
		}
	})

	t.Run("T3 reactive and promotion are available", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Topology.Active = topology.T3
		cfg.Reactive.Enabled = true
		cfg.HTTPStream.Backends = aggregator
		r := &Report{}
		runDeploymentTopology(context.Background(), r, cfg, fakePlaybackCoverage{}, now)
		if len(r.Checks) != 1 || r.Checks[0].Status != StatusWarn || !strings.Contains(r.Checks[0].Detail, "no aggregator-proxied") {
			t.Fatalf("checks=%+v ok=%v", r.Checks, r.OK())
		}
	})
	t.Run("fresh aggregator delivery substantiates T3", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Topology.Active, cfg.Reactive.Enabled, cfg.HTTPStream.Backends = topology.T3, true, aggregator
		r := &Report{}
		runDeploymentTopology(context.Background(), r, cfg, fakePlaybackCoverage{at: now.Add(-time.Hour), found: true}, now)
		if len(r.Checks) != 1 || r.Checks[0].Status != StatusOK {
			t.Fatalf("checks=%+v", r.Checks)
		}
	})
	t.Run("stale and unreadable coverage warns", func(t *testing.T) {
		for _, coverage := range []fakePlaybackCoverage{{at: now.Add(-8 * 24 * time.Hour), found: true}, {err: errors.New("unreadable")}} {
			cfg := &config.Config{}
			cfg.Topology.Active, cfg.Reactive.Enabled, cfg.HTTPStream.Backends = topology.T3, true, aggregator
			r := &Report{}
			runDeploymentTopology(context.Background(), r, cfg, coverage, now)
			if len(r.Checks) != 1 || r.Checks[0].Status != StatusWarn {
				t.Fatalf("checks=%+v", r.Checks)
			}
		}
	})

	t.Run("unknown warns", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Topology.Active = topology.Name("t4")
		r := &Report{}
		runDeploymentTopology(context.Background(), r, cfg, nil, now)
		if len(r.Checks) != 1 || r.Checks[0].Status != StatusWarn || !r.OK() {
			t.Fatalf("checks=%+v ok=%v", r.Checks, r.OK())
		}
	})
}

func TestTimeoutFromEnv(t *testing.T) {
	t.Run("absent uses default", func(t *testing.T) {
		os.Unsetenv(defaultTimeoutEnv)
		if got := TimeoutFromEnv(nil); got != defaultTimeout {
			t.Fatalf("got %v want %v", got, defaultTimeout)
		}
	})
	t.Run("valid overrides", func(t *testing.T) {
		os.Setenv(defaultTimeoutEnv, "5")
		defer os.Unsetenv(defaultTimeoutEnv)
		if got := TimeoutFromEnv(nil); got != 5*time.Second {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("invalid warns and falls back, never fails", func(t *testing.T) {
		os.Setenv(defaultTimeoutEnv, "not-a-number")
		defer os.Unsetenv(defaultTimeoutEnv)
		var warned string
		got := TimeoutFromEnv(func(msg string) { warned = msg })
		if got != defaultTimeout {
			t.Fatalf("got %v want default", got)
		}
		if warned == "" {
			t.Fatalf("expected a warning for invalid value")
		}
	})
	t.Run("zero/negative treated as invalid", func(t *testing.T) {
		os.Setenv(defaultTimeoutEnv, "0")
		defer os.Unsetenv(defaultTimeoutEnv)
		if got := TimeoutFromEnv(nil); got != defaultTimeout {
			t.Fatalf("got %v want default", got)
		}
	})
}

// fakeArr is a minimal Sonarr/Radarr double covering exactly the endpoints
// doctor calls: system/status, indexer list, downloadclient list, and
// filesystem browse.
type fakeArr struct {
	apiKey        string
	statusCode    int
	indexers      []map[string]any
	downloadCli   []map[string]any
	filesystemDir map[string][]string // dir -> file names present
}

func newFakeArr() *fakeArr {
	return &fakeArr{statusCode: http.StatusOK, filesystemDir: map[string][]string{}}
}

func (f *fakeArr) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/system/status", func(w http.ResponseWriter, r *http.Request) {
		if f.apiKey != "" && r.Header.Get("X-Api-Key") != f.apiKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(f.statusCode)
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/api/v3/indexer", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(f.indexers)
	})
	mux.HandleFunc("/api/v3/downloadclient", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(f.downloadCli)
	})
	mux.HandleFunc("/api/v3/filesystem", func(w http.ResponseWriter, r *http.Request) {
		dir := r.URL.Query().Get("path")
		names := f.filesystemDir[dir]
		files := make([]map[string]string, 0, len(names))
		for _, n := range names {
			files = append(files, map[string]string{"name": n})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"files": files})
	})
	return httptest.NewServer(mux)
}

func indexerField(base, impl string) map[string]any {
	return map[string]any{
		"implementation": impl,
		"fields":         []map[string]any{{"name": "baseUrl", "value": base}},
	}
}

func clientField(impl, host string, port int) map[string]any {
	return map[string]any{
		"implementation": impl,
		"fields": []map[string]any{
			{"name": "host", "value": host},
			{"name": "port", "value": float64(port)},
		},
	}
}

func TestRunArrReachability(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		fa := newFakeArr()
		fa.apiKey = "good-key"
		srv := fa.server()
		defer srv.Close()
		r := &Report{}
		runArrReachability(t.Context(), r, arrsetup.ArrTarget{Name: "sonarr", URL: srv.URL, APIKey: "good-key"}, func(*http.Response) {})
		if len(r.Checks) != 1 || r.Checks[0].Status != StatusOK {
			t.Fatalf("got %+v", r.Checks)
		}
	})
	t.Run("bad api key", func(t *testing.T) {
		fa := newFakeArr()
		fa.apiKey = "good-key"
		srv := fa.server()
		defer srv.Close()
		r := &Report{}
		runArrReachability(t.Context(), r, arrsetup.ArrTarget{Name: "sonarr", URL: srv.URL, APIKey: "wrong"}, func(*http.Response) {})
		if len(r.Checks) != 1 || r.Checks[0].Status != StatusFail || r.Checks[0].Name != "sonarr: api-key" {
			t.Fatalf("got %+v", r.Checks)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		r := &Report{}
		runArrReachability(t.Context(), r, arrsetup.ArrTarget{Name: "sonarr", URL: "http://127.0.0.1:1"}, func(*http.Response) {})
		if len(r.Checks) != 1 || r.Checks[0].Status != StatusFail {
			t.Fatalf("got %+v", r.Checks)
		}
	})
}

func TestRunArrTopologyComplete(t *testing.T) {
	fa := newFakeArr()
	fa.downloadCli = []map[string]any{
		clientField("QBittorrent", "darkharrbor", 8381),
		clientField("Sabnzbd", "darkharrbor", 8381),
	}
	fa.indexers = []map[string]any{
		indexerField("http://darkharrbor:8381/torznab/torrent-only", "Torznab"),
		indexerField("http://darkharrbor:8381/torznab/usenet-only", "Newznab"),
		indexerField("http://darkharrbor:8381/torznab/http-stream", "Torznab"),
	}
	srv := fa.server()
	defer srv.Close()
	r := &Report{}
	runArrTopology(t.Context(), r, arrsetup.ArrTarget{Name: "sonarr", URL: srv.URL})
	if len(r.Checks) != 1 || r.Checks[0].Status != StatusOK {
		t.Fatalf("got %+v", r.Checks)
	}
}

func TestRunArrTopologyMissingEntries(t *testing.T) {
	fa := newFakeArr()
	// Only qbit + torznab present; sab/newznab/http-stream missing.
	fa.downloadCli = []map[string]any{clientField("QBittorrent", "darkharrbor", 8381)}
	fa.indexers = []map[string]any{indexerField("http://darkharrbor:8381/torznab/torrent-only", "Torznab")}
	srv := fa.server()
	defer srv.Close()
	r := &Report{}
	runArrTopology(t.Context(), r, arrsetup.ArrTarget{Name: "sonarr", URL: srv.URL})
	if len(r.Checks) != 1 || r.Checks[0].Status != StatusFail {
		t.Fatalf("got %+v", r.Checks)
	}
	got := r.Checks[0].Detail
	for _, want := range []string{"sab-client", "newznab-indexer", "http-stream-indexer"} {
		if !strings.Contains(got, want) {
			t.Fatalf("detail missing %q: %q", want, got)
		}
	}
}

func TestRunPathMappingProbeVisible(t *testing.T) {
	dataRoot := t.TempDir()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/filesystem", func(w http.ResponseWriter, r *http.Request) {
		dir := r.URL.Query().Get("path")
		entries, _ := os.ReadDir(dir)
		files := make([]map[string]string, 0, len(entries))
		for _, e := range entries {
			files = append(files, map[string]string{"name": e.Name()})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"files": files})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := &config.Config{}
	cfg.Data.Strm = dataRoot
	cfg.Data.ReportedPathPrefix = dataRoot

	r := &Report{}
	runPathMappingProbe(t.Context(), r, cfg, []arrsetup.ArrTarget{{Name: "sonarr", URL: srv.URL}})
	if len(r.Checks) != 1 || r.Checks[0].Status != StatusOK {
		t.Fatalf("got %+v", r.Checks)
	}
	entries, _ := os.ReadDir(dataRoot)
	if len(entries) != 0 {
		t.Fatalf("marker file was not cleaned up: %+v", entries)
	}
}

func TestRunPathMappingProbeNotVisible(t *testing.T) {
	dataRoot := t.TempDir()
	fa := newFakeArr() // empty filesystemDir -> arr sees nothing
	srv := fa.server()
	defer srv.Close()

	cfg := &config.Config{}
	cfg.Data.Strm = dataRoot
	cfg.Data.ReportedPathPrefix = "/mnt/different-mapping"

	r := &Report{}
	runPathMappingProbe(t.Context(), r, cfg, []arrsetup.ArrTarget{{Name: "sonarr", URL: srv.URL}})
	if len(r.Checks) != 1 || r.Checks[0].Status != StatusFail {
		t.Fatalf("got %+v", r.Checks)
	}
}

// TestArrSeesPathRequiresIncludeFilesAndTrailingSlash is a regression test
// for a real defect this row's own live gate caught: Sonarr/Radarr's real
// GET /api/v3/filesystem omits the "files" array entirely unless
// includeFiles=true is set, and returns the PARENT directory's listing
// when path has no trailing slash. This fake reproduces exactly that real
// shape (unlike the other path-mapping tests' more permissive fakes) so a
// regression to the pre-fix query would fail this test.
func TestArrSeesPathRequiresIncludeFilesAndTrailingSlash(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/filesystem", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		path := q.Get("path")
		includeFiles := q.Get("includeFiles") == "true"
		hasTrailingSlash := strings.HasSuffix(path, "/")
		var files []map[string]string
		if includeFiles && hasTrailingSlash && path == "/mnt/darkharrbor/" {
			files = []map[string]string{{"name": "marker.txt"}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"files": files})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	visible, err := arrSeesPath(t.Context(), arrsetup.ArrTarget{Name: "sonarr", URL: srv.URL}, "/mnt/darkharrbor/marker.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !visible {
		t.Fatalf("expected marker to be visible with a real Sonarr/Radarr-shaped filesystem response")
	}
}

func TestRunPathMappingProbeNoDataRoot(t *testing.T) {
	cfg := &config.Config{}
	r := &Report{}
	runPathMappingProbe(t.Context(), r, cfg, []arrsetup.ArrTarget{{Name: "sonarr", URL: "http://example.invalid"}})
	if len(r.Checks) != 1 || r.Checks[0].Status != StatusSkip {
		t.Fatalf("got %+v", r.Checks)
	}
}

func TestCheckDiskMissingPathAbstains(t *testing.T) {
	r := &Report{}
	checkDisk(r, "data-root", "/this/path/does/not/exist/at/all")
	if len(r.Checks) != 1 || r.Checks[0].Status != StatusFail {
		t.Fatalf("got %+v", r.Checks)
	}
}

func TestCheckDiskRealPathOK(t *testing.T) {
	r := &Report{}
	checkDisk(r, "data-root", t.TempDir())
	if len(r.Checks) != 1 {
		t.Fatalf("got %+v", r.Checks)
	}
	switch r.Checks[0].Status {
	case StatusOK, StatusWarn, StatusFail:
	default:
		t.Fatalf("unexpected status %+v", r.Checks[0])
	}
}

func TestRunReturnsErrorOnNilDeps(t *testing.T) {
	if _, err := Run(t.Context(), Options{}, time.Second); err == nil {
		t.Fatalf("expected error for nil DB/Cfg")
	}
}

func aggregatorCfg(active topology.Name, withBackend bool) *config.Config {
	cfg := &config.Config{}
	cfg.Topology.Active = active
	if withBackend {
		cfg.HTTPStream.Backends = []config.HTTPBackend{{Name: "aiostreams", Type: "stremio"}}
	}
	return cfg
}

func aggregatorCheck(t *testing.T, r *Report) (Check, bool) {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == "aggregator resolution" {
			return c, true
		}
	}
	return Check{}, false
}

func TestAggregatorResolutionSkipsInvasiveCatalogLookup(t *testing.T) {
	r := &Report{}
	runAggregatorResolution(r, aggregatorCfg(topology.T3, true))
	c, ok := aggregatorCheck(t, r)
	if !ok || c.Status != StatusSkip {
		t.Fatalf("expected a SKIP check, got %+v (found=%v)", c, ok)
	}
	for _, want := range []string{"deliberately excluded", "Library addon", "reactive identity", "egress"} {
		if !strings.Contains(c.Detail, want) {
			t.Fatalf("skip detail %q missing %q", c.Detail, want)
		}
	}
}

// T1 has no aggregator, and a T3 with no configured backend has nothing to
// diagnose: neither may emit this check at all.
func TestAggregatorResolutionSilentWithoutAggregator(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *config.Config
	}{
		{"t1", aggregatorCfg(topology.T1, false)},
		{"t3 without backend", aggregatorCfg(topology.T3, false)},
	} {
		r := &Report{}
		runAggregatorResolution(r, tc.cfg)
		if _, ok := aggregatorCheck(t, r); ok {
			t.Fatalf("%s: must emit no aggregator-resolution check", tc.name)
		}
	}
}

type stubDeliveryReader struct {
	counts store.PlaybackRepresentationCounts
	err    error
}

func (s stubDeliveryReader) CurrentPlaybackRepresentationMetrics(context.Context, time.Time) (store.PlaybackRepresentationCounts, error) {
	return s.counts, s.err
}

func deliveryCheck(t *testing.T, r *Report) (Check, bool) {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == "aggregator delivery" {
			return c, true
		}
	}
	return Check{}, false
}

// RD-28 RX-0.3: an aggregator-side failure must be NAMED. "Never delivered"
// and "delivered, then stopped" are diagnostically different and must not be
// collapsed -- the first is a posture that never worked, the second is the
// shape of the 2026-08-16 shared-tier outage.
func TestAggregatorDeliveryNeverObservedIsNamed(t *testing.T) {
	r := &Report{}
	runAggregatorDelivery(context.Background(), r, aggregatorCfg(topology.T3, true),
		stubDeliveryReader{counts: store.PlaybackRepresentationCounts{AggregatorKnown: false}}, time.Now(), 48*time.Hour)
	c, ok := deliveryCheck(t, r)
	if !ok || c.Status != StatusFail {
		t.Fatalf("expected FAIL, got %+v (found=%v)", c, ok)
	}
	if !strings.Contains(c.Detail, "EVER") || !strings.Contains(c.Detail, "CLIENT DEVICES") {
		t.Fatalf("detail must name the never-delivered case and the client-reachability cause, got %q", c.Detail)
	}
}

func TestAggregatorDeliveryStoppedIsNamedAsDownstreamOfDiscovery(t *testing.T) {
	r := &Report{}
	runAggregatorDelivery(context.Background(), r, aggregatorCfg(topology.T3, true),
		stubDeliveryReader{counts: store.PlaybackRepresentationCounts{AggregatorKnown: true, AggregatorAge: 72 * time.Hour}},
		time.Now(), 48*time.Hour)
	c, ok := deliveryCheck(t, r)
	if !ok || c.Status != StatusFail {
		t.Fatalf("expected FAIL, got %+v (found=%v)", c, ok)
	}
	if !strings.Contains(c.Detail, "DOWNSTREAM of discovery") {
		t.Fatalf("detail must distinguish a downstream failure, got %q", c.Detail)
	}
	if !strings.Contains(c.Detail, "not a content-source outage") {
		t.Fatalf("detail must exclude the isolated content-source case, got %q", c.Detail)
	}
}

func TestAggregatorDeliveryRecentIsOK(t *testing.T) {
	r := &Report{}
	runAggregatorDelivery(context.Background(), r, aggregatorCfg(topology.T3, true),
		stubDeliveryReader{counts: store.PlaybackRepresentationCounts{AggregatorKnown: true, AggregatorAge: 30 * time.Second}},
		time.Now(), 48*time.Hour)
	c, ok := deliveryCheck(t, r)
	if !ok || c.Status != StatusOK {
		t.Fatalf("expected OK, got %+v (found=%v)", c, ok)
	}
}

// T1, and a T3 with no configured aggregator, have nothing to diagnose.
func TestAggregatorDeliverySilentWithoutAggregator(t *testing.T) {
	for _, cfg := range []*config.Config{aggregatorCfg(topology.T1, false), aggregatorCfg(topology.T3, false)} {
		r := &Report{}
		runAggregatorDelivery(context.Background(), r, cfg,
			stubDeliveryReader{counts: store.PlaybackRepresentationCounts{AggregatorKnown: false}}, time.Now(), 48*time.Hour)
		if _, ok := deliveryCheck(t, r); ok {
			t.Fatal("must emit no aggregator-delivery check without a configured aggregator")
		}
	}
}

// DG-04: a reader error must not leak connection detail into the report.
func TestAggregatorDeliveryReaderErrorIsRedacted(t *testing.T) {
	r := &Report{}
	runAggregatorDelivery(context.Background(), r, aggregatorCfg(topology.T3, true),
		stubDeliveryReader{err: errors.New(`query "https://user:secret@db/path?token=abc" failed`)}, time.Now(), 48*time.Hour)
	c, _ := deliveryCheck(t, r)
	for _, bad := range []string{"secret", "token=abc", "https://"} {
		if strings.Contains(c.Detail, bad) {
			t.Fatalf("detail leaked %q: %s", bad, c.Detail)
		}
	}
}
