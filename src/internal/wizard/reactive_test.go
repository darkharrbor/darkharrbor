package wizard

import (
	"bufio"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/store"
	"github.com/darkharrbor/darkharrbor/internal/topology"
)

type destinationChoice struct {
	name           string
	rootFolder     string
	qualityProfile int
}

type fakeArrDestinationStore struct {
	instances []store.ArrInstance
	set       []destinationChoice
}

func (f *fakeArrDestinationStore) ListArrInstances(context.Context) ([]store.ArrInstance, error) {
	return append([]store.ArrInstance(nil), f.instances...), nil
}

func (f *fakeArrDestinationStore) SetArrInstanceDestination(_ context.Context, name, rootFolder string, qualityProfile int) error {
	f.set = append(f.set, destinationChoice{name: name, rootFolder: rootFolder, qualityProfile: qualityProfile})
	return nil
}

func TestStagedArrDestinationStoreDefersWritesUntilApply(t *testing.T) {
	base := &fakeArrDestinationStore{}
	stage := newStagedArrDestinationStore(base)
	if err := stage.SetArrInstanceDestination(context.Background(), "radarr", "/movies", 14); err != nil {
		t.Fatal(err)
	}
	if len(base.set) != 0 {
		t.Fatalf("destination persisted before apply: %+v", base.set)
	}
	if err := stage.Apply(); err != nil {
		t.Fatal(err)
	}
	if len(base.set) != 1 || base.set[0] != (destinationChoice{name: "radarr", rootFolder: "/movies", qualityProfile: 14}) {
		t.Fatalf("applied destination=%+v", base.set)
	}
}

func reactiveTestConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Topology.Active = topology.T1
	cfg.Reactive.CommitMode = "off"
	cfg.Reactive.Threshold = 0.5
	cfg.Reactive.MonitorEpisodes = true
	cfg.Reactive.MonitorMovies = true
	cfg.Reactive.PromotionFreePercent = 10
	cfg.Arrs = []config.ArrTarget{{Name: "radarr"}}
	return cfg
}

func destinationTestConfig(t *testing.T) (*config.Config, *fakeArrDestinationStore, string) {
	t.Helper()
	secret := "opaque-test-key"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != secret {
			t.Error("missing or incorrect API key header")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/rootfolder":
			_, _ = w.Write([]byte(`[{"id":7,"path":"/movies"}]`))
		case "/api/v3/qualityprofile":
			_, _ = w.Write([]byte(`[{"id":1,"name":"Any"},{"id":14,"name":"Best Available"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	cfg := reactiveTestConfig()
	cfg.Arrs = []config.ArrTarget{{Name: "radarr", BaseURL: server.URL, APIKey: secret}}
	destinations := &fakeArrDestinationStore{instances: []store.ArrInstance{{Name: "radarr", AppType: "radarr", RootFolder: "/movies", QualityProfile: 14}}}
	return cfg, destinations, secret
}

func TestConfigureReactiveWholeFeatureAndSafeDefault(t *testing.T) {
	t.Run("explicit auto", func(t *testing.T) {
		cfg, destinations, secret := destinationTestConfig(t)
		values := map[string]string{}
		var out bytes.Buffer
		// Root auto-selects because Radarr exposes exactly one. Blank keeps the
		// stored quality profile (14), and 1 selects radarr as movie default.
		p := configurePrompter{reader: bufio.NewReader(strings.NewReader("t3\nauto\n0.6\nn\ny\n15\n\n1\n")), out: &out}
		if err := configureReactive(&p, cfg, values, func(context.Context, config.ArrTarget) bool { return true }, destinations, true, false); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{
			"HARRBOR_TOPOLOGY":                        "t3",
			"HARRBOR_REACTIVE_ENABLED":                "true",
			"HARRBOR_REACTIVE_COMMIT_MODE":            "auto",
			"HARRBOR_REACTIVE_THRESHOLD":              "0.6",
			"HARRBOR_REACTIVE_MONITOR_EPISODES":       "false",
			"HARRBOR_REACTIVE_MONITOR_MOVIES":         "true",
			"HARRBOR_REACTIVE_PROMOTION_ENABLED":      "true",
			"HARRBOR_REACTIVE_PROMOTION_FREE_PERCENT": "15",
			"HARRBOR_REACTIVE_DEFAULT_MOVIE_ARR":      "radarr",
		}
		for key, expected := range want {
			if values[key] != expected {
				t.Errorf("%s=%q want %q", key, values[key], expected)
			}
		}
		if len(destinations.set) != 1 || destinations.set[0] != (destinationChoice{name: "radarr", rootFolder: "/movies", qualityProfile: 14}) {
			t.Fatalf("destination=%+v", destinations.set)
		}
		if !strings.Contains(out.String(), "/movies (only available root; selected automatically)") || !strings.Contains(out.String(), "14 (Best Available)") {
			t.Fatalf("destination choices not surfaced:\n%s", out.String())
		}
		if strings.Contains(out.String(), secret) {
			t.Fatal("credential leaked to wizard output")
		}
	})

	t.Run("enter stays off", func(t *testing.T) {
		values := map[string]string{}
		p := configurePrompter{reader: bufio.NewReader(strings.NewReader("\n")), out: &bytes.Buffer{}}
		if err := configureReactive(&p, reactiveTestConfig(), values, func(context.Context, config.ArrTarget) bool { return true }, nil, false, false); err != nil {
			t.Fatal(err)
		}
		if values["HARRBOR_TOPOLOGY"] != "t1" || values["HARRBOR_REACTIVE_ENABLED"] != "false" || values["HARRBOR_REACTIVE_COMMIT_MODE"] != "off" {
			t.Fatalf("values=%v", values)
		}
	})

	t.Run("declined promotion stays off", func(t *testing.T) {
		cfg, destinations, _ := destinationTestConfig(t)
		values := map[string]string{}
		var out bytes.Buffer
		p := configurePrompter{reader: bufio.NewReader(strings.NewReader("t3\nauto\n0.6\nn\ny\n\n1\n")), out: &out}
		if err := configureReactive(&p, cfg, values, func(context.Context, config.ArrTarget) bool { return true }, destinations, false, false); err != nil {
			t.Fatal(err)
		}
		if values["HARRBOR_REACTIVE_PROMOTION_ENABLED"] != "false" {
			t.Fatalf("promotion opt-out was not preserved: %v", values)
		}
		if !strings.Contains(out.String(), "Promotion settings skipped") {
			t.Fatalf("promotion skip was not explained:\n%s", out.String())
		}
	})

	t.Run("stale t3 off still requires typed consent", func(t *testing.T) {
		cfg := reactiveTestConfig()
		cfg.Topology.Active = topology.T3
		values := map[string]string{}
		p := configurePrompter{reader: bufio.NewReader(strings.NewReader("\n")), out: &bytes.Buffer{}}
		if err := configureReactive(&p, cfg, values, func(context.Context, config.ArrTarget) bool { return true }, nil, false, false); err != nil {
			t.Fatal(err)
		}
		if values["HARRBOR_TOPOLOGY"] != "t1" || values["HARRBOR_REACTIVE_COMMIT_MODE"] != "off" {
			t.Fatalf("values=%v", values)
		}
	})
}

func TestConfigureReactiveUnreachableAndClosedInputFailClosed(t *testing.T) {
	values := map[string]string{"KEEP": "same"}
	p := configurePrompter{reader: bufio.NewReader(strings.NewReader("t3\n")), out: &bytes.Buffer{}}
	if err := configureReactive(&p, reactiveTestConfig(), values, func(context.Context, config.ArrTarget) bool { return false }, nil, false, false); err != nil {
		t.Fatal(err)
	}
	if values["HARRBOR_REACTIVE_ENABLED"] != "false" || values["HARRBOR_REACTIVE_COMMIT_MODE"] != "off" || values["KEEP"] != "same" {
		t.Fatalf("values=%v", values)
	}

	closed := map[string]string{"KEEP": "same"}
	p = configurePrompter{reader: bufio.NewReader(strings.NewReader("")), out: &bytes.Buffer{}}
	if err := configureReactive(&p, reactiveTestConfig(), closed, func(context.Context, config.ArrTarget) bool { return true }, nil, false, false); err == nil {
		t.Fatal("closed input succeeded")
	}
	if len(closed) != 1 || closed["KEEP"] != "same" {
		t.Fatalf("closed input mutated values=%v", closed)
	}
}

func TestProbeArrAndSetupPersistence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "opaque" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/system/status":
			w.WriteHeader(http.StatusOK)
		case "/api/v3/rootfolder":
			_, _ = w.Write([]byte(`[{"id":7,"path":"/movies"}]`))
		case "/api/v3/qualityprofile":
			_, _ = w.Write([]byte(`[{"id":1,"name":"Any"},{"id":14,"name":"Best Available"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	if !anyReachableArr([]config.ArrTarget{{BaseURL: server.URL, APIKey: "opaque"}}, nil) {
		t.Fatal("reachable Arr rejected")
	}

	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "wizard.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	repo := store.New(db)
	if err := repo.UpsertArrInstance(context.Background(), store.ArrInstance{Name: "radarr", AppType: "radarr", URL: server.URL, APIKeyRef: "RADARR_API_KEY"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetArrInstanceDestination(context.Background(), "radarr", "/movies", 14); err != nil {
		t.Fatal(err)
	}

	oldReader := stdinReader
	stdinReader = bufio.NewReader(strings.NewReader("supervised\n0.5\ny\ny\n10\n\n1\n"))
	t.Cleanup(func() { stdinReader = oldReader })
	dir := t.TempDir()
	if err := runReactiveSetup(&bytes.Buffer{}, dir, db, []discoveredArrInstance{{Name: "radarr", URL: server.URL, AppType: "radarr", APIKey: "opaque", APIKeyRef: "RADARR_API_KEY"}}, onboardingPlan{Topology: "t3", ReactiveCommit: true, Promotion: true}); err != nil {
		t.Fatal(err)
	}
	values, err := config.ReadTuning(filepath.Join(dir, filepath.Base(config.DefaultTuningPath)))
	if err != nil {
		t.Fatal(err)
	}
	if values["HARRBOR_TOPOLOGY"] != "t3" || values["HARRBOR_REACTIVE_ENABLED"] != "true" || values["HARRBOR_REACTIVE_COMMIT_MODE"] != "supervised" || values["HARRBOR_REACTIVE_DEFAULT_MOVIE_ARR"] != "radarr" {
		t.Fatalf("values=%v", values)
	}
	instances, err := repo.ListArrInstances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 1 || instances[0].RootFolder != "/movies" || instances[0].QualityProfile != 14 {
		t.Fatalf("instances=%+v", instances)
	}
}
