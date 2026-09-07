package wizard

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/config"
)

func TestRunConfigureRoundTripPreservesLiveSplit(t *testing.T) {
	t.Setenv("HARRBOR_LOG_LEVEL", "INFO")
	t.Setenv("HARRBOR_PROVIDER_NEWSHOSTING_TOTAL_CONNECTIONS", "95")
	t.Setenv("HARRBOR_PROVIDER_NEWSHOSTING_DEMAND_CONNECTIONS", "56")
	t.Setenv("HARRBOR_PROVIDER_NEWSHOSTING_READAHEAD_CONNECTIONS", "39")
	t.Setenv("HARRBOR_PROVIDER_NEWSHOSTING_MAX_CONNS_PER_FILE", "16")
	t.Setenv("HARRBOR_PROVIDER_NEWSHOSTING_READAHEAD_MAX_SEGMENTS", "128")

	var current config.Config
	current.Cache.Mode = "readahead"
	current.Cache.NNTPMode = "disk"
	current.Cache.StreamMode = "disk"
	current.Cache.DiskCacheSizeMB = 51200
	current.Cache.DiskCacheTTLMin = 60
	current.Cache.FullEvictMode = "ttl"
	current.Cache.FullEvictTTLMin = 10080
	current.Cache.TotalConnections = 16
	current.Cache.ReadaheadMaxSegments = 16
	current.Cache.StreamChunkSizeMB = 16
	current.Cache.StreamMinBufferSegments = 2
	current.Cache.StreamReadaheadWorkers = 1
	current.NNTP.Connections = 95
	current.Governor.UncachedBudget = 20
	current.Governor.StallTimeoutMin = 30
	current.Governor.DeadSentinelGraceMin = 5
	current.Backup.Enabled = true
	current.Backup.Target = "/backup"
	current.Backup.IntervalHours = 24
	current.Backup.Keep = 7
	current.Routing.Preference = []string{"torbox_torrent", "torbox_nzb", "nntp_nzb", "uncached_torrent"}
	current.Providers.UsenetProviders = []string{"newshosting", "torbox"}

	path := filepath.Join(t.TempDir(), "darkharrbor.conf")
	var out bytes.Buffer
	saved, err := RunConfigure(ConfigureOptions{
		Path:    path,
		Stdin:   strings.NewReader(strings.Repeat("\n", 22) + "y\n\n\n"),
		Stdout:  &out,
		Current: &current,
	})
	if err != nil {
		t.Fatalf("RunConfigure: %v\n%s", err, out.String())
	}
	if !saved {
		t.Fatal("RunConfigure did not save")
	}
	values, err := config.ReadTuning(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"HARRBOR_BACKUP_ENABLED":                             "true",
		"HARRBOR_BACKUP_TARGET":                              "/backup",
		"HARRBOR_BACKUP_INTERVAL_HOURS":                      "24",
		"HARRBOR_BACKUP_KEEP":                                "7",
		"HARRBOR_NNTP_CACHE_MODE":                            "disk",
		"HARRBOR_STREAM_CACHE_MODE":                          "disk",
		"HARRBOR_DISK_CACHE_SIZE_MB":                         "51200",
		"HARRBOR_DISK_CACHE_TTL_MIN":                         "10080",
		"HARRBOR_FULL_EVICT_MODE":                            "ttl",
		"HARRBOR_FULL_EVICT_TTL_MIN":                         "10080",
		"HARRBOR_PREFERENCE":                                 "torbox_torrent,torbox_nzb,nntp_nzb,uncached_torrent",
		"HARRBOR_GOVERNOR_UNCACHED_BUDGET":                   "20",
		"HARRBOR_GOVERNOR_STALL_TIMEOUT_MIN":                 "30",
		"HARRBOR_GOVERNOR_DEAD_SENTINEL_GRACE_MIN":           "5",
		"HARRBOR_LOG_LEVEL":                                  "INFO",
		"HARRBOR_NNTP_CONNECTIONS":                           "95",
		"HARRBOR_NNTP_CROSSLANE_SPLICE":                      "true",
		"HARRBOR_PROVIDER_NEWSHOSTING_TOTAL_CONNECTIONS":     "95",
		"HARRBOR_PROVIDER_NEWSHOSTING_DEMAND_CONNECTIONS":    "56",
		"HARRBOR_PROVIDER_NEWSHOSTING_READAHEAD_CONNECTIONS": "39",
		"HARRBOR_PROVIDER_NEWSHOSTING_MAX_CONNS_PER_FILE":    "16",
		"HARRBOR_REACTIVE_COMMIT_MODE":                       "off",
		"HARRBOR_REACTIVE_ENABLED":                           "false",
		"HARRBOR_READAHEAD_MAX_SEGMENTS":                     "128",
		"HARRBOR_STREAM_CHUNK_SIZE_MB":                       "16",
		"HARRBOR_STREAM_MIN_BUFFER_SEGMENTS":                 "2",
		"HARRBOR_STREAM_READAHEAD_WORKERS":                   "1",
		"HARRBOR_STREMIO_EDGE_MODE":                          "internal",
	}
	for key, expected := range want {
		if got := values[key]; got != expected {
			t.Errorf("%s = %q, want %q", key, got, expected)
		}
	}
	if len(values) != len(want) {
		t.Fatalf("wrote %d settings, want %d: %#v", len(values), len(want), values)
	}
}

func TestSplitConnections(t *testing.T) {
	tests := []struct {
		total         int
		wantDemand    int
		wantReadahead int
	}{
		{3, 1, 2},
		{8, 5, 3},
		{95, 57, 38},
		{100, 60, 40},
	}
	for _, tt := range tests {
		demand, readahead := splitConnections(tt.total)
		if demand != tt.wantDemand || readahead != tt.wantReadahead {
			t.Errorf("splitConnections(%d) = %d/%d, want %d/%d", tt.total, demand, readahead, tt.wantDemand, tt.wantReadahead)
		}
	}
}

func TestRunConfigureClosedInputDoesNotSave(t *testing.T) {
	var current config.Config
	current.Cache.Mode = "readahead"
	path := filepath.Join(t.TempDir(), "darkharrbor.conf")
	saved, err := RunConfigure(ConfigureOptions{
		Path:    path,
		Stdin:   strings.NewReader(""),
		Stdout:  &bytes.Buffer{},
		Current: &current,
	})
	if err == nil || saved {
		t.Fatalf("saved=%v err=%v, want closed-input error", saved, err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("tuning file created on closed input: %v", statErr)
	}
}

func TestApplyConfigureFilesRollsBackTuningWhenSealedUpdateFails(t *testing.T) {
	dir := t.TempDir()
	tuningPath := filepath.Join(dir, "darkharrbor.conf")
	if err := config.WriteTuning(tuningPath, map[string]string{"HARRBOR_LOG_LEVEL": "INFO"}); err != nil {
		t.Fatal(err)
	}
	err := applyConfigureFiles(tuningPath, filepath.Join(dir, "missing.sealed"), nil,
		map[string]string{"HARRBOR_LOG_LEVEL": "DEBUG"},
		map[string]string{"HARRBOR_LOG_LEVEL": "INFO"},
		map[string]string{"HARRBOR_HTTPBACKEND_AIOSTREAMS_URL": "https://opaque.invalid/path"})
	if err == nil {
		t.Fatal("expected sealed update failure")
	}
	values, readErr := config.ReadTuning(tuningPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if values["HARRBOR_LOG_LEVEL"] != "INFO" {
		t.Fatalf("tuning was not rolled back: %v", values)
	}
}

func TestRunBackupSetupWritesDefaultsAndPreservesTuning(t *testing.T) {
	cfgDir := t.TempDir()
	path := filepath.Join(cfgDir, filepath.Base(config.DefaultTuningPath))
	if err := config.WriteTuning(path, map[string]string{"HARRBOR_LOG_LEVEL": "WARN"}); err != nil {
		t.Fatal(err)
	}

	oldReader := stdinReader
	stdinReader = bufio.NewReader(strings.NewReader("\n\n\n\n"))
	defer func() { stdinReader = oldReader }()

	var out bytes.Buffer
	if err := runBackupSetup(&out, cfgDir, true); err != nil {
		t.Fatal(err)
	}
	values, err := config.ReadTuning(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"HARRBOR_BACKUP_ENABLED":        "true",
		"HARRBOR_BACKUP_TARGET":         "/backup",
		"HARRBOR_BACKUP_INTERVAL_HOURS": "24",
		"HARRBOR_BACKUP_KEEP":           "7",
		"HARRBOR_LOG_LEVEL":             "WARN",
	}
	for key, expected := range want {
		if values[key] != expected {
			t.Errorf("%s = %q, want %q", key, values[key], expected)
		}
	}
}

func TestRunBackupSetupDisabledAsksNoChildQuestions(t *testing.T) {
	cfgDir := t.TempDir()
	path := filepath.Join(cfgDir, filepath.Base(config.DefaultTuningPath))
	if err := config.WriteTuning(path, map[string]string{"HARRBOR_BACKUP_TARGET": "/keep"}); err != nil {
		t.Fatal(err)
	}
	oldReader := stdinReader
	stdinReader = bufio.NewReader(strings.NewReader("must-not-be-consumed\n"))
	defer func() { stdinReader = oldReader }()

	var out bytes.Buffer
	if err := runBackupSetup(&out, cfgDir, false); err != nil {
		t.Fatal(err)
	}
	line, _ := stdinReader.ReadString('\n')
	if line != "must-not-be-consumed\n" {
		t.Fatalf("disabled backup consumed a child response: %q", line)
	}
	values, err := config.ReadTuning(path)
	if err != nil {
		t.Fatal(err)
	}
	if values["HARRBOR_BACKUP_ENABLED"] != "false" || values["HARRBOR_BACKUP_TARGET"] != "/keep" {
		t.Fatalf("values=%v", values)
	}
}
