package config

import (
	"strings"
	"testing"
)

// TestGovernorSubmitBudgetMinDefaultAndOverride covers TS-3.2 (T4): the
// uncached torrent submit-progress budget, default 15 min, distinct from and
// stricter than the longer generic stall timeout (StallTimeoutMin, default
// 30 min, which only engages once the provider explicitly reports a stalled
// state and never blacklists).
func TestGovernorSubmitBudgetMinDefaultAndOverride(t *testing.T) {
	t.Setenv("HARRBOR_TORBOX_API_TOKEN", "token")
	t.Setenv("HARRBOR_QBIT_PASSWORD", "not-the-default")
	t.Setenv("HARRBOR_SAB_API_KEY", "not-the-default")
	t.Setenv("HARRBOR_STREAM_SECRET", strings.Repeat("s", 32))

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Governor.SubmitBudgetMin != 15 {
		t.Fatalf("SubmitBudgetMin default = %d, want 15", cfg.Governor.SubmitBudgetMin)
	}

	t.Setenv("HARRBOR_TORRENT_SUBMIT_BUDGET_MIN", "7")
	cfg2, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.Governor.SubmitBudgetMin != 7 {
		t.Fatalf("SubmitBudgetMin override = %d, want 7", cfg2.Governor.SubmitBudgetMin)
	}
}

func TestGovernorSubmitBudgetMinTuningReload(t *testing.T) {
	cfg := defaultConfig()
	applyTuning(&cfg, map[string]string{
		"HARRBOR_TORRENT_SUBMIT_BUDGET_MIN": "22",
	})
	if cfg.Governor.SubmitBudgetMin != 22 {
		t.Fatalf("SubmitBudgetMin tuning = %d, want 22", cfg.Governor.SubmitBudgetMin)
	}
}

func TestGovernorSubmitBudgetMinRejectsBelowOne(t *testing.T) {
	t.Setenv("HARRBOR_TORBOX_API_TOKEN", "token")
	t.Setenv("HARRBOR_QBIT_PASSWORD", "not-the-default")
	t.Setenv("HARRBOR_SAB_API_KEY", "not-the-default")
	t.Setenv("HARRBOR_STREAM_SECRET", strings.Repeat("s", 32))
	t.Setenv("HARRBOR_TORRENT_SUBMIT_BUDGET_MIN", "0")

	if _, err := Load(); err == nil {
		t.Fatal("expected Load to reject governor.submit_budget_min < 1")
	}
}
