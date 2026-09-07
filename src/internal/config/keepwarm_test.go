package config

import (
	"strings"
	"testing"
)

// TestGovernorTorrentKeepWarmDaysDefaultAndOverride covers TS-4.2 (T8a):
// the keep-warm "recent play" window, default 20 days, 0 disables.
func TestGovernorTorrentKeepWarmDaysDefaultAndOverride(t *testing.T) {
	t.Setenv("HARRBOR_TORBOX_API_TOKEN", "token")
	t.Setenv("HARRBOR_QBIT_PASSWORD", "not-the-default")
	t.Setenv("HARRBOR_SAB_API_KEY", "not-the-default")
	t.Setenv("HARRBOR_STREAM_SECRET", strings.Repeat("s", 32))

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Governor.TorrentKeepWarmDays != 20 {
		t.Fatalf("TorrentKeepWarmDays default = %d, want 20", cfg.Governor.TorrentKeepWarmDays)
	}

	t.Setenv("HARRBOR_TORRENT_KEEPWARM_DAYS", "10")
	cfg2, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.Governor.TorrentKeepWarmDays != 10 {
		t.Fatalf("TorrentKeepWarmDays override = %d, want 10", cfg2.Governor.TorrentKeepWarmDays)
	}
}

// TestGovernorTorrentKeepWarmDaysZeroDisables proves 0 is a valid,
// accepted "disabled" value (distinct from SubmitBudgetMin's >= 1 floor).
func TestGovernorTorrentKeepWarmDaysZeroDisables(t *testing.T) {
	t.Setenv("HARRBOR_TORBOX_API_TOKEN", "token")
	t.Setenv("HARRBOR_QBIT_PASSWORD", "not-the-default")
	t.Setenv("HARRBOR_SAB_API_KEY", "not-the-default")
	t.Setenv("HARRBOR_STREAM_SECRET", strings.Repeat("s", 32))
	t.Setenv("HARRBOR_TORRENT_KEEPWARM_DAYS", "0")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Governor.TorrentKeepWarmDays != 0 {
		t.Fatalf("TorrentKeepWarmDays = %d, want 0 (disabled)", cfg.Governor.TorrentKeepWarmDays)
	}
}

func TestGovernorTorrentKeepWarmDaysRejectsNegative(t *testing.T) {
	t.Setenv("HARRBOR_TORBOX_API_TOKEN", "token")
	t.Setenv("HARRBOR_QBIT_PASSWORD", "not-the-default")
	t.Setenv("HARRBOR_SAB_API_KEY", "not-the-default")
	t.Setenv("HARRBOR_STREAM_SECRET", strings.Repeat("s", 32))
	t.Setenv("HARRBOR_TORRENT_KEEPWARM_DAYS", "-1")

	cfg, err := Load()
	// -1 fails strconv-negative-guard in applyEnv (n >= 0 check), so the
	// env var is silently ignored and the default (20) remains -- never a
	// hard startup failure from a malformed env value alone.
	if err != nil {
		t.Fatalf("Load should not fail on an ignored malformed env value: %v", err)
	}
	if cfg.Governor.TorrentKeepWarmDays != 20 {
		t.Fatalf("TorrentKeepWarmDays = %d, want default 20 (negative env value ignored)", cfg.Governor.TorrentKeepWarmDays)
	}
}

func TestGovernorTorrentKeepWarmDaysTuningReload(t *testing.T) {
	cfg := defaultConfig()
	applyTuning(&cfg, map[string]string{
		"HARRBOR_TORRENT_KEEPWARM_DAYS": "15",
	})
	if cfg.Governor.TorrentKeepWarmDays != 15 {
		t.Fatalf("TorrentKeepWarmDays tuning = %d, want 15", cfg.Governor.TorrentKeepWarmDays)
	}
}

func TestGovernorTorrentKeepWarmDaysTuningAllowsZero(t *testing.T) {
	if err := ValidateTuning(map[string]string{"HARRBOR_TORRENT_KEEPWARM_DAYS": "0"}); err != nil {
		t.Fatalf("ValidateTuning should accept 0 (disabled): %v", err)
	}
}
