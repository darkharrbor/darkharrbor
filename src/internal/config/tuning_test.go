package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/util"
)

func TestTuningFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "darkharrbor.conf")
	values := map[string]string{
		"HARRBOR_BACKUP_ENABLED":                             "true",
		"HARRBOR_BACKUP_TARGET":                              "/backup",
		"HARRBOR_BACKUP_INTERVAL_HOURS":                      "24",
		"HARRBOR_BACKUP_KEEP":                                "7",
		"HARRBOR_DISK_CACHE_SIZE_MB":                         "51200",
		"HARRBOR_DISK_CACHE_TTL_MIN":                         "10080",
		"HARRBOR_FULL_EVICT_MODE":                            "ttl",
		"HARRBOR_FULL_EVICT_TTL_MIN":                         "10080",
		"HARRBOR_GOVERNOR_UNCACHED_BUDGET":                   "20",
		"HARRBOR_HTTP_ALLOW_PRIVATE_SOURCES":                 "true",
		"HARRBOR_HTTP_BACKENDS":                              "ia,aiostreams,cinepro",
		"HARRBOR_HTTP_STREAM_ENABLED":                        "true",
		"HARRBOR_HTTPBACKEND_AIOSTREAMS_TYPE":                "stremio",
		"HARRBOR_HTTPBACKEND_AIOSTREAMS_REDIRECT_ORIGINS":    "https://metadata.example",
		"HARRBOR_HTTPBACKEND_CINEPRO_TYPE":                   "omss",
		"HARRBOR_HTTPBACKEND_CINEPRO_URL":                    "http://cinepro-core:3000",
		"HARRBOR_HTTPBACKEND_IA_TYPE":                        "ia",
		"HARRBOR_HTTPBACKEND_IA_URL":                         "https://archive.org",
		"HARRBOR_STRM_INSTALLER_ENABLED":                     "true",
		"HARRBOR_LOG_LEVEL":                                  "INFO",
		"HARRBOR_NNTP_CACHE_MODE":                            "disk",
		"HARRBOR_NNTP_CONNECTIONS":                           "95",
		"HARRBOR_NNTP_CROSSLANE_SPLICE":                      "true",
		"HARRBOR_NNTP_PREWARM_BYTES":                         "16777216",
		"HARRBOR_PREFERENCE":                                 "torbox_torrent,nntp_nzb",
		"HARRBOR_PROVIDER_NEWSHOSTING_DEMAND_CONNECTIONS":    "56",
		"HARRBOR_PROVIDER_NEWSHOSTING_MAX_CONNS_PER_FILE":    "16",
		"HARRBOR_PROVIDER_NEWSHOSTING_READAHEAD_CONNECTIONS": "39",
		"HARRBOR_PROVIDER_NEWSHOSTING_TOTAL_CONNECTIONS":     "95",
		"HARRBOR_READAHEAD_MAX_SEGMENTS":                     "128",
		"HARRBOR_STREAM_CACHE_MODE":                          "disk",
		"HARRBOR_STREAM_CHUNK_SIZE_MB":                       "16",
		"HARRBOR_STREAM_MIN_BUFFER_SEGMENTS":                 "2",
		"HARRBOR_STREAM_READAHEAD_WORKERS":                   "1",
	}
	if err := WriteTuning(path, values); err != nil {
		t.Fatal(err)
	}
	got, err := ReadTuning(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, values) {
		t.Fatalf("round trip mismatch:\n got %#v\nwant %#v", got, values)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Fatalf("permissions = %o, want 644", perm)
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteTuning(path, values); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("second write changed canonical output:\n%s\n---\n%s", before, after)
	}

	bad := map[string]string{"HARRBOR_TORBOX_API_TOKEN": "do-not-leak-this"}
	if err := WriteTuning(path, bad); err == nil {
		t.Fatal("secret key accepted")
	} else if strings.Contains(err.Error(), "do-not-leak-this") {
		t.Fatalf("error leaked secret value: %v", err)
	}
	unchanged, _ := os.ReadFile(path)
	if string(unchanged) != string(before) {
		t.Fatal("failed validation changed existing file")
	}
}

func TestReadTuningWarnsAndIgnoresUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "darkharrbor.conf")
	body := "HARRBOR_LOG_LEVEL=INFO\nHARRBOR_TORBOX_API_TOKEN=secret-value\nHARRBOR_LOG_LEVL=WARN\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	values, warnings, err := ReadTuningWithWarnings(path)
	if err != nil {
		t.Fatal(err)
	}
	if values["HARRBOR_LOG_LEVEL"] != "INFO" || len(values) != 1 {
		t.Fatalf("unexpected accepted values: %#v", values)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings=%d, want 2", len(warnings))
	}
	for _, warning := range warnings {
		if strings.Contains(warning, "secret-value") {
			t.Fatalf("warning leaked value: %q", warning)
		}
	}
}

func TestPreviousSupportedReleaseTuningStartsCleanly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "darkharrbor.conf")
	previous := map[string]string{
		"HARRBOR_NNTP_CACHE_MODE":                            "disk",
		"HARRBOR_STREAM_CACHE_MODE":                          "disk",
		"HARRBOR_DISK_CACHE_SIZE_MB":                         "50000",
		"HARRBOR_DISK_CACHE_TTL_MIN":                         "10080",
		"HARRBOR_FULL_EVICT_MODE":                            "ttl",
		"HARRBOR_FULL_EVICT_TTL_MIN":                         "10080",
		"HARRBOR_READAHEAD_MAX_SEGMENTS":                     "128",
		"HARRBOR_STREAM_CHUNK_SIZE_MB":                       "16",
		"HARRBOR_STREAM_MIN_BUFFER_SEGMENTS":                 "2",
		"HARRBOR_STREAM_READAHEAD_WORKERS":                   "1",
		"HARRBOR_NNTP_CONNECTIONS":                           "95",
		"HARRBOR_PREFERENCE":                                 "torbox_torrent,nntp_nzb",
		"HARRBOR_GOVERNOR_UNCACHED_BUDGET":                   "20",
		"HARRBOR_LOG_LEVEL":                                  "INFO",
		"HARRBOR_PROVIDER_NEWSHOSTING_TOTAL_CONNECTIONS":     "95",
		"HARRBOR_PROVIDER_NEWSHOSTING_DEMAND_CONNECTIONS":    "56",
		"HARRBOR_PROVIDER_NEWSHOSTING_READAHEAD_CONNECTIONS": "39",
		"HARRBOR_PROVIDER_NEWSHOSTING_MAX_CONNS_PER_FILE":    "16",
		"HARRBOR_PROVIDER_NEWSHOSTING_CONNECTIONS":           "95",
	}
	if err := WriteTuning(path, previous); err != nil {
		t.Fatalf("write previous release tuning: %v", err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open previous release tuning: %v", err)
	}
	if _, err := file.WriteString("HARRBOR_RETIRED_SETTING=private-value\n"); err != nil {
		_ = file.Close()
		t.Fatalf("append retired tuning: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close previous release tuning: %v", err)
	}

	got, warnings, err := ReadTuningWithWarnings(path)
	if err != nil {
		t.Fatalf("read previous release tuning: %v", err)
	}
	if !reflect.DeepEqual(got, previous) {
		t.Fatalf("previous release tuning changed during read")
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "HARRBOR_RETIRED_SETTING") {
		t.Fatalf("retired setting warnings = %v, want key-only warning", warnings)
	}
	if strings.Contains(warnings[0], "private-value") {
		t.Fatal("retired setting warning leaked its value")
	}
}

func TestReadTuningRejectsDuplicateAllowedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "darkharrbor.conf")
	if err := os.WriteFile(path, []byte("HARRBOR_LOG_LEVEL=INFO\nHARRBOR_LOG_LEVEL=DEBUG\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTuning(path); err == nil {
		t.Fatal("duplicate tuning key accepted")
	}
}

func TestApplyTuningOverridesOnlyAllowedSettings(t *testing.T) {
	cfg := defaultConfig()
	cfg.applySecrets(&SecretStore{values: map[string]util.Redacted{
		"HARRBOR_PREFERENCE":                             util.Redacted("torbox_torrent"),
		"HARRBOR_PROVIDER_NEWSHOSTING_TOTAL_CONNECTIONS": util.Redacted("8"),
		"HARRBOR_TORBOX_API_TOKEN":                       util.Redacted("sealed-token"),
	}})
	applyTuning(&cfg, map[string]string{
		"HARRBOR_PREFERENCE":                             "torbox_torrent,nntp_nzb",
		"HARRBOR_NNTP_CONNECTIONS":                       "95",
		"HARRBOR_GOVERNOR_UNCACHED_BUDGET":               "7",
		"HARRBOR_PROVIDER_NEWSHOSTING_TOTAL_CONNECTIONS": "95",
		"HARRBOR_HTTP_STREAM_ENABLED":                    "true",
		"HARRBOR_HTTP_ALLOW_PRIVATE_SOURCES":             "true",
		"HARRBOR_STRM_INSTALLER_ENABLED":                 "true",
	})

	if got := strings.Join(cfg.Routing.Preference, ","); got != "torbox_torrent,nntp_nzb" {
		t.Fatalf("preference = %q", got)
	}
	if cfg.NNTP.Connections != 95 || cfg.Governor.UncachedBudget != 7 {
		t.Fatalf("tuning fields not applied: connections=%d budget=%d", cfg.NNTP.Connections, cfg.Governor.UncachedBudget)
	}
	if got, _ := cfg.Lookup("HARRBOR_PROVIDER_NEWSHOSTING_TOTAL_CONNECTIONS"); got != "95" {
		t.Fatalf("dynamic tuning lookup = %q, want 95", got)
	}
	if got, _ := cfg.Lookup("HARRBOR_TORBOX_API_TOKEN"); got != "sealed-token" {
		t.Fatalf("sealed credential changed: %q", got)
	}
	if !cfg.HTTPStream.Enabled || !cfg.HTTPStream.AllowPrivateSourceCIDRs {
		t.Fatalf("HTTP tuning flags not applied: enabled=%t allow_private=%t", cfg.HTTPStream.Enabled, cfg.HTTPStream.AllowPrivateSourceCIDRs)
	}
	if !cfg.StrmInstaller.Enabled {
		t.Fatal("strm installer tuning flag not applied")
	}
}

func TestTuningHTTPBackendValidation(t *testing.T) {
	valid := map[string]string{
		"HARRBOR_HTTP_STREAM_ENABLED":                  "true",
		"HARRBOR_HTTP_ALLOW_PRIVATE_SOURCES":           "true",
		"HARRBOR_HTTP_BACKENDS":                        "ia,aiostreams,cinepro,fixture",
		"HARRBOR_HTTPBACKEND_IA_URL":                   "https://archive.org",
		"HARRBOR_HTTPBACKEND_IA_TYPE":                  "ia",
		"HARRBOR_HTTPBACKEND_AIOSTREAMS_TYPE":          "stremio",
		"HARRBOR_HTTPBACKEND_CINEPRO_URL":              "http://cinepro-core:3000",
		"HARRBOR_HTTPBACKEND_CINEPRO_TYPE":             "omss",
		"HARRBOR_HTTPBACKEND_FIXTURE_URL":              "https://example.invalid/media",
		"HARRBOR_HTTPBACKEND_FIXTURE_TYPE":             "generic",
		"HARRBOR_HTTPBACKEND_FIXTURE_DESCRIPTORS_FILE": "/data/descriptors.json",
	}
	if err := ValidateTuning(valid); err != nil {
		t.Fatalf("valid HTTP config rejected: %v", err)
	}

	for name, values := range map[string]map[string]string{
		"bad-bool": {"HARRBOR_HTTP_STREAM_ENABLED": "sometimes"},
		"bad-list": {"HARRBOR_HTTP_BACKENDS": "good,bad name"},
		"bad-url":  {"HARRBOR_HTTPBACKEND_IA_URL": "ftp://archive.org"},
		"userinfo": {"HARRBOR_HTTPBACKEND_IA_URL": "https://user:pass@archive.org"},
		"bad-type": {"HARRBOR_HTTPBACKEND_IA_TYPE": "scraper"},
		"relative": {"HARRBOR_HTTPBACKEND_FIXTURE_DESCRIPTORS_FILE": "descriptors.json"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateTuning(values); err == nil {
				t.Fatal("invalid HTTP tuning accepted")
			}
		})
	}
}

func TestGovernorBudgetEnvironmentIsApplied(t *testing.T) {
	t.Setenv("HARRBOR_GOVERNOR_UNCACHED_BUDGET", "31")
	cfg := defaultConfig()
	applyEnv(&cfg)
	if cfg.Governor.UncachedBudget != 31 {
		t.Fatalf("budget = %d, want 31", cfg.Governor.UncachedBudget)
	}
}

func TestNNTPPrewarmBytesDefaultEnvironmentAndTuning(t *testing.T) {
	cfg := defaultConfig()
	if cfg.Prewarm.NNTPBytes != 16*1024*1024 {
		t.Fatalf("default NNTP prewarm bytes = %d", cfg.Prewarm.NNTPBytes)
	}

	t.Setenv("HARRBOR_NNTP_PREWARM_BYTES", "0")
	applyEnv(&cfg)
	if cfg.Prewarm.NNTPBytes != 0 {
		t.Fatalf("environment did not disable NNTP prewarm: %d", cfg.Prewarm.NNTPBytes)
	}

	applyTuning(&cfg, map[string]string{"HARRBOR_NNTP_PREWARM_BYTES": "4096"})
	if cfg.Prewarm.NNTPBytes != 4096 {
		t.Fatalf("tuning NNTP prewarm bytes = %d, want 4096", cfg.Prewarm.NNTPBytes)
	}
	if err := ValidateTuning(map[string]string{"HARRBOR_NNTP_PREWARM_BYTES": "0"}); err != nil {
		t.Fatalf("zero disable rejected: %v", err)
	}
}

func TestNNTPStripeEnvironmentAndTuning(t *testing.T) {
	cfg := defaultConfig()
	if cfg.NNTP.Stripe {
		t.Fatal("NNTP striping must default off")
	}

	t.Setenv("HARRBOR_NNTP_STRIPE", "true")
	applyEnv(&cfg)
	if !cfg.NNTP.Stripe {
		t.Fatal("environment did not enable NNTP striping")
	}

	applyTuning(&cfg, map[string]string{"HARRBOR_NNTP_STRIPE": "false"})
	if cfg.NNTP.Stripe {
		t.Fatal("tuning did not override NNTP striping")
	}
	if err := ValidateTuning(map[string]string{
		"HARRBOR_NNTP_STRIPE":                         "true",
		"HARRBOR_PROVIDER_NEWSHOSTING_RETENTION_DAYS": "6000",
	}); err != nil {
		t.Fatalf("valid NS-8.4 tuning rejected: %v", err)
	}
}

func TestNNTPCrossLaneSpliceManagedTuning(t *testing.T) {
	cfg := defaultConfig()
	if cfg.NNTP.CrossLaneSplice {
		t.Fatal("cross-lane splice must default off")
	}

	applyTuning(&cfg, map[string]string{"HARRBOR_NNTP_CROSSLANE_SPLICE": "true"})
	if !cfg.NNTP.CrossLaneSplice {
		t.Fatal("managed tuning did not enable cross-lane splice")
	}
	if err := ValidateTuning(map[string]string{"HARRBOR_NNTP_CROSSLANE_SPLICE": "ambiguous"}); err == nil {
		t.Fatal("invalid cross-lane splice boolean accepted")
	}
}

func TestNNTPRetentionTuningRejectsOutOfBoundsValues(t *testing.T) {
	for _, value := range []string{"not-a-number", "0", "-1", "36501"} {
		err := ValidateTuning(map[string]string{
			"HARRBOR_PROVIDER_NEWSHOSTING_RETENTION_DAYS": value,
		})
		if err == nil {
			t.Fatalf("retention value %q accepted", value)
		}
		if value == "not-a-number" && strings.Contains(err.Error(), value) {
			t.Fatalf("validation error exposed raw value %q: %v", value, err)
		}
	}
	if err := ValidateTuning(map[string]string{"HARRBOR_NNTP_STRIPE": "sometimes"}); err == nil {
		t.Fatal("invalid stripe boolean accepted")
	}
}

func TestNNTPVerifyCRCEnvironment(t *testing.T) {
	cfg := defaultConfig()
	if !cfg.NNTP.VerifyCRC {
		t.Fatal("NNTP CRC verification must default on")
	}

	t.Setenv("HARRBOR_NNTP_VERIFY_CRC", "false")
	applyEnv(&cfg)
	if cfg.NNTP.VerifyCRC {
		t.Fatal("NNTP CRC verification remained enabled")
	}
}

func TestRemovedDirectStreamWarnsAndIsIgnored(t *testing.T) {
	t.Setenv("HARRBOR_DIRECT_STREAM", "true")
	cfg := defaultConfig()
	applyEnv(&cfg)

	if len(cfg.StartupWarnings) != 1 || !strings.Contains(cfg.StartupWarnings[0], "HARRBOR_DIRECT_STREAM") {
		t.Fatalf("startup warnings = %v, want removed-key warning", cfg.StartupWarnings)
	}
}

func TestLoadUsesTuningAfterLegacySources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "darkharrbor.conf")
	if err := WriteTuning(path, map[string]string{
		"HARRBOR_PREFERENCE":                             "torbox_torrent,nntp_nzb",
		"HARRBOR_GOVERNOR_UNCACHED_BUDGET":               "31",
		"HARRBOR_PROVIDER_NEWSHOSTING_TOTAL_CONNECTIONS": "95",
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARRBOR_TUNING_FILE", path)
	t.Setenv("HARRBOR_SECRETS_FILE", "")
	t.Setenv("HARRBOR_PREFERENCE", "torbox_torrent")
	t.Setenv("HARRBOR_PROVIDER_NEWSHOSTING_TOTAL_CONNECTIONS", "8")
	t.Setenv("HARRBOR_TORBOX_API_TOKEN", "token")
	t.Setenv("HARRBOR_QBIT_PASSWORD", "not-the-default")
	t.Setenv("HARRBOR_SAB_API_KEY", "not-the-default")
	t.Setenv("HARRBOR_STREAM_SECRET", strings.Repeat("s", 32))

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Routing.Preference, ","); got != "torbox_torrent,nntp_nzb" {
		t.Fatalf("preference = %q", got)
	}
	if cfg.Governor.UncachedBudget != 31 {
		t.Fatalf("budget = %d, want 31", cfg.Governor.UncachedBudget)
	}
	if got, _ := cfg.Lookup("HARRBOR_PROVIDER_NEWSHOSTING_TOTAL_CONNECTIONS"); got != "95" {
		t.Fatalf("provider total = %q, want 95", got)
	}
}

func TestHealthAuditDefaultsAndEnvironment(t *testing.T) {
	cfg := defaultConfig()
	if !cfg.HealthAudit.Enabled {
		t.Fatal("health audit must default enabled")
	}
	if cfg.HealthAudit.IntervalSec != 60 {
		t.Fatalf("default interval_sec = %d, want 60", cfg.HealthAudit.IntervalSec)
	}
	if cfg.HealthAudit.SampleSize != 8 {
		t.Fatalf("default sample_size = %d, want 8", cfg.HealthAudit.SampleSize)
	}
	if cfg.HealthAudit.CompletenessThreshold != 0.95 {
		t.Fatalf("default completeness_threshold = %v, want 0.95", cfg.HealthAudit.CompletenessThreshold)
	}
	if cfg.HealthAudit.MaxDeadRegions != 64 {
		t.Fatalf("default max_dead_regions = %d, want 64", cfg.HealthAudit.MaxDeadRegions)
	}

	t.Setenv("HARRBOR_HEALTHAUDIT_ENABLED", "false")
	t.Setenv("HARRBOR_HEALTHAUDIT_INTERVAL_SEC", "120")
	t.Setenv("HARRBOR_HEALTHAUDIT_SAMPLE_SIZE", "16")
	t.Setenv("HARRBOR_HEALTHAUDIT_COMPLETENESS_THRESHOLD", "0.9")
	t.Setenv("HARRBOR_HEALTHAUDIT_MAX_DEAD_REGIONS", "32")
	applyEnv(&cfg)
	if cfg.HealthAudit.Enabled {
		t.Fatal("environment did not disable health audit")
	}
	if cfg.HealthAudit.IntervalSec != 120 {
		t.Fatalf("interval_sec = %d, want 120", cfg.HealthAudit.IntervalSec)
	}
	if cfg.HealthAudit.SampleSize != 16 {
		t.Fatalf("sample_size = %d, want 16", cfg.HealthAudit.SampleSize)
	}
	if cfg.HealthAudit.CompletenessThreshold != 0.9 {
		t.Fatalf("completeness_threshold = %v, want 0.9", cfg.HealthAudit.CompletenessThreshold)
	}
	if cfg.HealthAudit.MaxDeadRegions != 32 {
		t.Fatalf("max_dead_regions = %d, want 32", cfg.HealthAudit.MaxDeadRegions)
	}
}

func TestHealthAuditValidateRejectsOutOfRangeValues(t *testing.T) {
	base := func() Config {
		cfg := defaultConfig()
		cfg.Server.StreamSecret = strings.Repeat("s", 32)
		cfg.Auth.QBitPassword = "not-the-default"
		cfg.Auth.SABAPIKey = "not-the-default"
		return cfg
	}

	cfg := base()
	cfg.HealthAudit.IntervalSec = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for interval_sec = 0")
	}

	cfg = base()
	cfg.HealthAudit.SampleSize = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for sample_size = 0")
	}

	cfg = base()
	cfg.HealthAudit.CompletenessThreshold = 1.5
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for completeness_threshold > 1")
	}

	cfg = base()
	cfg.HealthAudit.MaxDeadRegions = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for negative max_dead_regions")
	}
}

func TestBackupDefaultsEnvironmentAndTuning(t *testing.T) {
	cfg := defaultConfig()
	if !cfg.Backup.Enabled || cfg.Backup.Target != "/backup" || cfg.Backup.IntervalHours != 24 || cfg.Backup.Keep != 7 {
		t.Fatalf("backup defaults = %+v", cfg.Backup)
	}

	t.Setenv("HARRBOR_BACKUP_ENABLED", "false")
	t.Setenv("HARRBOR_BACKUP_TARGET", "/mnt/backups")
	t.Setenv("HARRBOR_BACKUP_INTERVAL_HOURS", "12")
	t.Setenv("HARRBOR_BACKUP_KEEP", "3")
	applyEnv(&cfg)
	if cfg.Backup.Enabled || cfg.Backup.Target != "/mnt/backups" || cfg.Backup.IntervalHours != 12 || cfg.Backup.Keep != 3 {
		t.Fatalf("backup environment = %+v", cfg.Backup)
	}

	applyTuning(&cfg, map[string]string{
		"HARRBOR_BACKUP_ENABLED":        "true",
		"HARRBOR_BACKUP_TARGET":         "/backup",
		"HARRBOR_BACKUP_INTERVAL_HOURS": "24",
		"HARRBOR_BACKUP_KEEP":           "7",
	})
	if !cfg.Backup.Enabled || cfg.Backup.Target != "/backup" || cfg.Backup.IntervalHours != 24 || cfg.Backup.Keep != 7 {
		t.Fatalf("backup tuning = %+v", cfg.Backup)
	}

	for _, values := range []map[string]string{
		{"HARRBOR_BACKUP_ENABLED": "maybe"},
		{"HARRBOR_BACKUP_TARGET": "relative"},
		{"HARRBOR_BACKUP_TARGET": "/"},
		{"HARRBOR_BACKUP_TARGET": "https://example.invalid/backup"},
		{"HARRBOR_BACKUP_INTERVAL_HOURS": "0"},
		{"HARRBOR_BACKUP_KEEP": "0"},
	} {
		if err := ValidateTuning(values); err == nil {
			t.Fatalf("invalid backup tuning accepted: %v", values)
		}
	}
}
