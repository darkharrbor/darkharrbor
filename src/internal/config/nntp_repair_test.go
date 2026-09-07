package config

import (
	"strings"
	"testing"
)

func TestNNTPRepairBudgetConfig(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		t.Setenv("HARRBOR_NNTP_REPAIR_BUDGET_MB", "")
		t.Setenv("HARRBOR_NNTP_REPAIR_BUDGET_SECONDS", "")
		cfg := defaultConfig()
		applyEnv(&cfg)
		if cfg.NNTP.RepairBudgetMB != 256 || cfg.NNTP.RepairBudgetSeconds != 45 {
			t.Fatalf("budget=%dMB/%ds", cfg.NNTP.RepairBudgetMB, cfg.NNTP.RepairBudgetSeconds)
		}
	})
	t.Run("zero disables", func(t *testing.T) {
		t.Setenv("HARRBOR_NNTP_REPAIR_BUDGET_MB", "0")
		t.Setenv("HARRBOR_NNTP_REPAIR_BUDGET_SECONDS", "0")
		cfg := defaultConfig()
		applyEnv(&cfg)
		if cfg.NNTP.RepairBudgetMB != 0 || cfg.NNTP.RepairBudgetSeconds != 0 {
			t.Fatalf("budget=%dMB/%ds", cfg.NNTP.RepairBudgetMB, cfg.NNTP.RepairBudgetSeconds)
		}
	})
	t.Run("valid", func(t *testing.T) {
		t.Setenv("HARRBOR_NNTP_REPAIR_BUDGET_MB", "512")
		t.Setenv("HARRBOR_NNTP_REPAIR_BUDGET_SECONDS", "90")
		cfg := defaultConfig()
		applyEnv(&cfg)
		if cfg.NNTP.RepairBudgetMB != 512 || cfg.NNTP.RepairBudgetSeconds != 90 {
			t.Fatalf("budget=%dMB/%ds", cfg.NNTP.RepairBudgetMB, cfg.NNTP.RepairBudgetSeconds)
		}
	})
	for _, tc := range []struct {
		name, key, value string
	}{
		{"negative bytes", "HARRBOR_NNTP_REPAIR_BUDGET_MB", "-1"},
		{"overflow bytes", "HARRBOR_NNTP_REPAIR_BUDGET_MB", "8796093022208"},
		{"negative seconds", "HARRBOR_NNTP_REPAIR_BUDGET_SECONDS", "-1"},
		{"overflow seconds", "HARRBOR_NNTP_REPAIR_BUDGET_SECONDS", "9223372037"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HARRBOR_NNTP_REPAIR_BUDGET_MB", "")
			t.Setenv("HARRBOR_NNTP_REPAIR_BUDGET_SECONDS", "")
			t.Setenv(tc.key, tc.value)
			cfg := defaultConfig()
			applyEnv(&cfg)
			if cfg.NNTP.RepairBudgetMB != 256 || cfg.NNTP.RepairBudgetSeconds != 45 {
				t.Fatalf("invalid input changed budget to %dMB/%ds", cfg.NNTP.RepairBudgetMB, cfg.NNTP.RepairBudgetSeconds)
			}
			if len(cfg.StartupWarnings) != 1 || !strings.Contains(cfg.StartupWarnings[0], tc.key) {
				t.Fatalf("warnings=%v", cfg.StartupWarnings)
			}
		})
	}
}
