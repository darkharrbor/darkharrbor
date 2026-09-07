package config

import (
	"strings"
	"testing"
)

func setReactiveTestSecrets(t *testing.T) {
	t.Helper()
	t.Setenv("HARRBOR_TORBOX_API_TOKEN", "token")
	t.Setenv("HARRBOR_QBIT_PASSWORD", "not-the-default")
	t.Setenv("HARRBOR_SAB_API_KEY", "not-the-default")
	t.Setenv("HARRBOR_STREAM_SECRET", strings.Repeat("s", 32))
}

func TestReactiveProposalDefaultsAndOverrides(t *testing.T) {
	setReactiveTestSecrets(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Reactive.Enabled || cfg.Reactive.CommitMode != "off" || cfg.Reactive.Threshold != 0.5 || !cfg.Reactive.MonitorEpisodes || !cfg.Reactive.MonitorMovies || !cfg.Reactive.PromotionEnabled || cfg.Reactive.PromotionFreePercent != 10 {
		t.Fatalf("reactive defaults=%+v", cfg.Reactive)
	}
	t.Setenv("HARRBOR_REACTIVE_ENABLED", "true")
	t.Setenv("HARRBOR_REACTIVE_COMMIT_MODE", "auto")
	t.Setenv("HARRBOR_REACTIVE_THRESHOLD", "0.75")
	t.Setenv("HARRBOR_REACTIVE_MONITOR_EPISODES", "false")
	t.Setenv("HARRBOR_REACTIVE_MONITOR_MOVIES", "false")
	t.Setenv("HARRBOR_REACTIVE_PROMOTION_ENABLED", "false")
	t.Setenv("HARRBOR_REACTIVE_PROMOTION_FREE_PERCENT", "17")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Reactive.Enabled || cfg.Reactive.CommitMode != "auto" || cfg.Reactive.Threshold != 0.75 || cfg.Reactive.MonitorEpisodes || cfg.Reactive.MonitorMovies || cfg.Reactive.PromotionEnabled || cfg.Reactive.PromotionFreePercent != 17 {
		t.Fatalf("reactive overrides=%+v", cfg.Reactive)
	}
}

func TestReactiveCommitModeInvalidEnvironmentWarnsAndStarts(t *testing.T) {
	setReactiveTestSecrets(t)
	t.Setenv("HARRBOR_REACTIVE_COMMIT_MODE", "automatic")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Reactive.CommitMode != "off" || len(cfg.StartupWarnings) == 0 {
		t.Fatalf("mode=%q warnings=%v", cfg.Reactive.CommitMode, cfg.StartupWarnings)
	}
}

func TestReactivePromotionFloorInvalidEnvironmentWarnsAndStarts(t *testing.T) {
	setReactiveTestSecrets(t)
	for _, value := range []string{"0", "100", "invalid"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("HARRBOR_REACTIVE_PROMOTION_FREE_PERCENT", value)
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Reactive.PromotionFreePercent != 10 || len(cfg.StartupWarnings) == 0 {
				t.Fatalf("floor=%d warnings=%v", cfg.Reactive.PromotionFreePercent, cfg.StartupWarnings)
			}
		})
	}
}

func TestReactiveMonitoringInvalidEnvironmentWarnsAndStarts(t *testing.T) {
	setReactiveTestSecrets(t)
	t.Setenv("HARRBOR_REACTIVE_MONITOR_EPISODES", "invalid")
	t.Setenv("HARRBOR_REACTIVE_MONITOR_MOVIES", "invalid")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Reactive.MonitorEpisodes || !cfg.Reactive.MonitorMovies {
		t.Fatalf("monitor defaults weakened: %+v", cfg.Reactive)
	}
	if len(cfg.StartupWarnings) < 2 {
		t.Fatalf("warnings=%v", cfg.StartupWarnings)
	}
}

func TestReactiveProposalInvalidEnvironmentWarnsAndStarts(t *testing.T) {
	for _, threshold := range []string{"0", "1.1", "NaN", "+Inf", "invalid"} {
		t.Run(threshold, func(t *testing.T) {
			setReactiveTestSecrets(t)
			t.Setenv("HARRBOR_REACTIVE_ENABLED", "invalid")
			t.Setenv("HARRBOR_REACTIVE_THRESHOLD", threshold)
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Reactive.Enabled || cfg.Reactive.Threshold != 0.5 {
				t.Fatalf("invalid reactive config applied: %+v", cfg.Reactive)
			}
			if len(cfg.StartupWarnings) < 2 {
				t.Fatalf("warnings=%v, want reactive warnings", cfg.StartupWarnings)
			}
		})
	}
}

func TestReactiveProposalTuningValidationAndApply(t *testing.T) {
	values := map[string]string{
		"HARRBOR_REACTIVE_ENABLED":                "true",
		"HARRBOR_REACTIVE_COMMIT_MODE":            "supervised",
		"HARRBOR_REACTIVE_THRESHOLD":              "0.6",
		"HARRBOR_REACTIVE_MONITOR_EPISODES":       "false",
		"HARRBOR_REACTIVE_MONITOR_MOVIES":         "false",
		"HARRBOR_REACTIVE_PROMOTION_ENABLED":      "false",
		"HARRBOR_REACTIVE_PROMOTION_FREE_PERCENT": "15",
	}
	if err := ValidateTuning(values); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	applyTuning(&cfg, values)
	if !cfg.Reactive.Enabled || cfg.Reactive.CommitMode != "supervised" || cfg.Reactive.Threshold != 0.6 || cfg.Reactive.MonitorEpisodes || cfg.Reactive.MonitorMovies || cfg.Reactive.PromotionEnabled || cfg.Reactive.PromotionFreePercent != 15 {
		t.Fatalf("reactive tuning=%+v", cfg.Reactive)
	}
	for _, threshold := range []string{"0", "1.1", "NaN", "+Inf"} {
		if err := ValidateTuning(map[string]string{"HARRBOR_REACTIVE_THRESHOLD": threshold}); err == nil {
			t.Fatalf("accepted threshold %q", threshold)
		}
	}
	for _, floor := range []string{"0", "100", "bad"} {
		if err := ValidateTuning(map[string]string{"HARRBOR_REACTIVE_PROMOTION_FREE_PERCENT": floor}); err == nil {
			t.Fatalf("accepted floor %q", floor)
		}
	}
	if err := ValidateTuning(map[string]string{"HARRBOR_REACTIVE_COMMIT_MODE": "automatic"}); err == nil {
		t.Fatal("accepted unknown commit mode")
	}
}
