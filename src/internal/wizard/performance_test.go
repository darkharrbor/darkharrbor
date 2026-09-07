package wizard

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/config"
)

func TestPerformanceProfilesUseValidatedExistingSettings(t *testing.T) {
	for name := range performanceProfiles {
		t.Run(name, func(t *testing.T) {
			values := map[string]string{"HARRBOR_BACKUP_ENABLED": "true"}
			if err := applyPerformanceProfile(values, name); err != nil {
				t.Fatal(err)
			}
			if values["HARRBOR_BACKUP_ENABLED"] != "true" {
				t.Fatal("profile did not preserve unrelated tuning")
			}
			if values["HARRBOR_STREAM_READAHEAD_WORKERS"] != "1" {
				t.Fatal("profile bypassed provider-safe worker limit")
			}
		})
	}
}

func TestRunPerformanceSetupMergesExistingTuning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, filepath.Base(config.DefaultTuningPath))
	if err := config.WriteTuning(path, map[string]string{"HARRBOR_BACKUP_ENABLED": "false"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runPerformanceSetup(&out, dir, "low-memory"); err != nil {
		t.Fatal(err)
	}
	values, err := config.ReadTuning(path)
	if err != nil {
		t.Fatal(err)
	}
	if values["HARRBOR_BACKUP_ENABLED"] != "false" || values["HARRBOR_READAHEAD_MAX_SEGMENTS"] != "8" {
		t.Fatalf("merged values=%v", values)
	}
}

func TestPersistPlanSwitchesEnablesHTTPAndDisablesDeclinedReactive(t *testing.T) {
	dir := t.TempDir()
	if err := persistPlanSwitches(dir, true, false); err != nil {
		t.Fatalf("persistPlanSwitches: %v", err)
	}
	values, err := config.ReadTuning(filepath.Join(dir, filepath.Base(config.DefaultTuningPath)))
	if err != nil {
		t.Fatalf("ReadTuning: %v", err)
	}
	for key, want := range map[string]string{
		"HARRBOR_HTTP_STREAM_ENABLED":        "true",
		"HARRBOR_REACTIVE_ENABLED":           "false",
		"HARRBOR_REACTIVE_COMMIT_MODE":       "off",
		"HARRBOR_REACTIVE_PROMOTION_ENABLED": "false",
	} {
		if values[key] != want {
			t.Fatalf("%s = %q, want %q", key, values[key], want)
		}
	}
}

func TestRunWrapperRepairSetupPersistsChoiceAndPreservesTuning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, filepath.Base(config.DefaultTuningPath))
	if err := config.WriteTuning(path, map[string]string{"HARRBOR_LOG_LEVEL": "WARN", "HARRBOR_STRM_INSTALLER_ENABLED": "false"}); err != nil {
		t.Fatal(err)
	}
	if err := runWrapperRepairSetup(&bytes.Buffer{}, dir, true); err != nil {
		t.Fatal(err)
	}
	values, err := config.ReadTuning(path)
	if err != nil {
		t.Fatal(err)
	}
	if values["HARRBOR_STRM_INSTALLER_ENABLED"] != "true" || values["HARRBOR_LOG_LEVEL"] != "WARN" {
		t.Fatalf("values=%v", values)
	}
}

func TestApplyPerformanceProfileRejectsUnknownName(t *testing.T) {
	if err := applyPerformanceProfile(map[string]string{}, "fastest-ever"); err == nil {
		t.Fatal("unknown profile was accepted")
	}
}

func TestRecommendPerformanceIsConservative(t *testing.T) {
	tests := []struct {
		name               string
		memory, free       int64
		diskKnown          bool
		wantDefault        string
		wantHighThroughput bool
	}{
		{"unknown resources", 0, 0, false, "recommended", true},
		{"limited memory", 1 << 30, 20 << 30, true, "low-memory", true},
		{"low disk", 8 << 30, 9 << 30, true, "recommended", false},
		{"ample is not auto-fast", 16 << 30, 100 << 30, true, "recommended", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := recommendPerformance(tt.memory, tt.free, tt.diskKnown)
			if got.Default != tt.wantDefault || got.HighThroughputAvailable != tt.wantHighThroughput {
				t.Fatalf("recommendation=%+v", got)
			}
		})
	}
}

func TestPerformancePromptRejectsHighThroughputWithoutDiskHeadroom(t *testing.T) {
	withResponses(t, []string{"high-throughput", "recommended"})
	var out bytes.Buffer
	got := promptPerformanceProfile(&out, performanceRecommendation{Default: "recommended", HighThroughputAvailable: false})
	if got != "recommended" {
		t.Fatalf("profile=%s", got)
	}
}
