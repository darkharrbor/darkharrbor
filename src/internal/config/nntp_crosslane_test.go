package config

import (
	"strings"
	"testing"
)

func TestNNTPCrossLaneSpliceConfig(t *testing.T) {
	t.Run("default off", func(t *testing.T) {
		t.Setenv("HARRBOR_NNTP_CROSSLANE_SPLICE", "")
		cfg := defaultConfig()
		applyEnv(&cfg)
		if cfg.NNTP.CrossLaneSplice || len(cfg.StartupWarnings) != 0 {
			t.Fatalf("enabled=%v warnings=%v", cfg.NNTP.CrossLaneSplice, cfg.StartupWarnings)
		}
	})
	t.Run("explicit on", func(t *testing.T) {
		t.Setenv("HARRBOR_NNTP_CROSSLANE_SPLICE", "true")
		cfg := defaultConfig()
		applyEnv(&cfg)
		if !cfg.NNTP.CrossLaneSplice || len(cfg.StartupWarnings) != 0 {
			t.Fatalf("enabled=%v warnings=%v", cfg.NNTP.CrossLaneSplice, cfg.StartupWarnings)
		}
	})
	t.Run("invalid warns and stays off", func(t *testing.T) {
		t.Setenv("HARRBOR_NNTP_CROSSLANE_SPLICE", "ambiguous")
		cfg := defaultConfig()
		applyEnv(&cfg)
		if cfg.NNTP.CrossLaneSplice || len(cfg.StartupWarnings) != 1 ||
			!strings.Contains(cfg.StartupWarnings[0], "HARRBOR_NNTP_CROSSLANE_SPLICE") {
			t.Fatalf("enabled=%v warnings=%v", cfg.NNTP.CrossLaneSplice, cfg.StartupWarnings)
		}
	})
}
