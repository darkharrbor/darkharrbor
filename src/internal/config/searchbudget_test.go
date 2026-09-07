package config

import "testing"

func TestSearchBudgetDefaultsAndInvalidValuesWarn(t *testing.T) {
	cfg := defaultConfig()
	if cfg.SearchBudget.Cap != 5 || cfg.SearchBudget.SuppressDays != 7 {
		t.Fatalf("defaults = %d/%d", cfg.SearchBudget.Cap, cfg.SearchBudget.SuppressDays)
	}
	t.Setenv("HARRBOR_SEARCH_BUDGET_CAP", "0")
	t.Setenv("HARRBOR_SEARCH_BUDGET_SUPPRESS_DAYS", "not-a-number")
	applyEnv(&cfg)
	if cfg.SearchBudget.Cap != 5 || cfg.SearchBudget.SuppressDays != 7 {
		t.Fatalf("invalid values changed defaults to %d/%d", cfg.SearchBudget.Cap, cfg.SearchBudget.SuppressDays)
	}
	if len(cfg.StartupWarnings) != 2 {
		t.Fatalf("warnings = %d, want 2", len(cfg.StartupWarnings))
	}
}

func TestSearchBudgetValidOverrides(t *testing.T) {
	t.Setenv("HARRBOR_SEARCH_BUDGET_CAP", "9")
	t.Setenv("HARRBOR_SEARCH_BUDGET_SUPPRESS_DAYS", "14")
	cfg := defaultConfig()
	applyEnv(&cfg)
	if cfg.SearchBudget.Cap != 9 || cfg.SearchBudget.SuppressDays != 14 {
		t.Fatalf("overrides = %d/%d", cfg.SearchBudget.Cap, cfg.SearchBudget.SuppressDays)
	}
}

func TestSearchBudgetTuningOverrides(t *testing.T) {
	values := map[string]string{
		"HARRBOR_SEARCH_BUDGET_CAP":           "8",
		"HARRBOR_SEARCH_BUDGET_SUPPRESS_DAYS": "21",
	}
	if err := ValidateTuning(values); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	applyTuning(&cfg, values)
	if cfg.SearchBudget.Cap != 8 || cfg.SearchBudget.SuppressDays != 21 {
		t.Fatalf("tuning = %d/%d", cfg.SearchBudget.Cap, cfg.SearchBudget.SuppressDays)
	}
	values["HARRBOR_SEARCH_BUDGET_CAP"] = "1001"
	if err := ValidateTuning(values); err == nil {
		t.Fatal("oversized cap accepted")
	}
}
