package config

import "testing"

func TestIdentityStrictnessDefaultAndOverride(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv("HARRBOR_IDENTITY_STRICTNESS", "")
		cfg := defaultConfig()
		applyEnv(&cfg)
		if cfg.Identity.Strictness != "warn" {
			t.Fatalf("default strictness = %q, want warn", cfg.Identity.Strictness)
		}
	})
	t.Run("off", func(t *testing.T) {
		t.Setenv("HARRBOR_IDENTITY_STRICTNESS", "off")
		cfg := defaultConfig()
		applyEnv(&cfg)
		if cfg.Identity.Strictness != "off" || len(cfg.StartupWarnings) != 0 {
			t.Fatalf("strictness = %q warnings=%v", cfg.Identity.Strictness, cfg.StartupWarnings)
		}
	})
	t.Run("enforce", func(t *testing.T) {
		t.Setenv("HARRBOR_IDENTITY_STRICTNESS", "ENFORCE")
		cfg := defaultConfig()
		applyEnv(&cfg)
		if cfg.Identity.Strictness != "enforce" || len(cfg.StartupWarnings) != 0 {
			t.Fatalf("strictness = %q warnings=%v", cfg.Identity.Strictness, cfg.StartupWarnings)
		}
	})
	t.Run("unknown warns and falls back to warn", func(t *testing.T) {
		t.Setenv("HARRBOR_IDENTITY_STRICTNESS", "paranoid")
		cfg := defaultConfig()
		applyEnv(&cfg)
		if cfg.Identity.Strictness != "warn" || len(cfg.StartupWarnings) != 1 {
			t.Fatalf("strictness = %q warnings=%v", cfg.Identity.Strictness, cfg.StartupWarnings)
		}
	})
}
