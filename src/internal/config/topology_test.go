package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/topology"
)

func TestTopologyEnvironment(t *testing.T) {
	t.Run("default T1", func(t *testing.T) {
		t.Setenv("HARRBOR_TOPOLOGY", "")
		cfg := defaultConfig()
		applyEnv(&cfg)
		if cfg.Topology.Active != topology.T1 || cfg.Topology.AchievedPostureStalenessHours != 48 || len(cfg.StartupWarnings) != 0 {
			t.Fatalf("active=%q warnings=%v", cfg.Topology.Active, cfg.StartupWarnings)
		}
	})

	t.Run("achieved posture window is bounded and optional", func(t *testing.T) {
		t.Setenv("HARRBOR_TOPOLOGY_ACHIEVED_STALENESS_HOURS", "24")
		cfg := defaultConfig()
		applyEnv(&cfg)
		if cfg.Topology.AchievedPostureStalenessHours != 24 {
			t.Fatalf("window=%d", cfg.Topology.AchievedPostureStalenessHours)
		}
		t.Setenv("HARRBOR_TOPOLOGY_ACHIEVED_STALENESS_HOURS", "0")
		cfg = defaultConfig()
		applyEnv(&cfg)
		if cfg.Topology.AchievedPostureStalenessHours != 48 || len(cfg.StartupWarnings) != 1 {
			t.Fatalf("window=%d warnings=%v", cfg.Topology.AchievedPostureStalenessHours, cfg.StartupWarnings)
		}
	})

	t.Run("canonicalizes supported value", func(t *testing.T) {
		t.Setenv("HARRBOR_TOPOLOGY", " T2 ")
		cfg := defaultConfig()
		applyEnv(&cfg)
		if cfg.Topology.Active != topology.T2 || len(cfg.StartupWarnings) != 0 {
			t.Fatalf("active=%q warnings=%v", cfg.Topology.Active, cfg.StartupWarnings)
		}
	})

	t.Run("unknown warns and falls back", func(t *testing.T) {
		t.Setenv("HARRBOR_TOPOLOGY", "experimental")
		cfg := defaultConfig()
		applyEnv(&cfg)
		if cfg.Topology.Active != topology.T1 || len(cfg.StartupWarnings) != 1 ||
			!strings.Contains(cfg.StartupWarnings[0], "HARRBOR_TOPOLOGY") {
			t.Fatalf("active=%q warnings=%v", cfg.Topology.Active, cfg.StartupWarnings)
		}
	})
}

func TestTopologyTuning(t *testing.T) {
	values := map[string]string{"HARRBOR_TOPOLOGY": "t3"}
	if err := ValidateTuning(values); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	applyTuning(&cfg, values)
	if cfg.Topology.Active != topology.T3 {
		t.Fatalf("active=%q", cfg.Topology.Active)
	}
	if err := ValidateTuning(map[string]string{"HARRBOR_TOPOLOGY": "t4"}); err == nil {
		t.Fatal("unsupported topology accepted")
	}
}

func TestUnknownTopologyTuningWarnsAndStarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "darkharrbor.conf")
	if err := os.WriteFile(path, []byte("HARRBOR_TOPOLOGY=t4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	values, warnings, err := ReadTuningWithWarnings(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 0 || len(warnings) != 1 || !strings.Contains(warnings[0], "HARRBOR_TOPOLOGY") {
		t.Fatalf("values=%v warnings=%v", values, warnings)
	}
}
