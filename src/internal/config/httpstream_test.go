package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/util"
)

func backendCfg(t *testing.T, env map[string]string) (*Config, error) {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
	cfg := &Config{}
	return cfg, applyHTTPBackends(cfg)
}

func TestHTTPBackendsParsing(t *testing.T) {
	cfg, err := backendCfg(t, map[string]string{
		"HARRBOR_HTTP_BACKENDS":                            "Archive-One, second_ia",
		"HARRBOR_HTTPBACKEND_ARCHIVE_ONE_URL":              "https://archive.example/",
		"HARRBOR_HTTPBACKEND_ARCHIVE_ONE_PUBLIC_URL":       "https://client.example/",
		"HARRBOR_HTTPBACKEND_ARCHIVE_ONE_REDIRECT_ORIGINS": "https://Metadata.Example/, https://cdn.example,https://metadata.example",
		"HARRBOR_HTTPBACKEND_ARCHIVE_ONE_TYPE":             "ia",
		"HARRBOR_HTTPBACKEND_SECOND_IA_URL":                "http://10.0.0.9:8080",
		"HARRBOR_HTTPBACKEND_SECOND_IA_TYPE":               "IA",
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	b := cfg.HTTPStream.Backends
	if len(b) != 2 {
		t.Fatalf("backends = %d, want 2", len(b))
	}
	if b[0].Name != "archive-one" || b[0].URL != "https://archive.example" || b[0].PublicURL != "https://client.example" || b[0].Type != "ia" ||
		len(b[0].RedirectOrigins) != 2 || b[0].RedirectOrigins[0] != "https://metadata.example" || b[0].RedirectOrigins[1] != "https://cdn.example" {
		t.Fatalf("backend[0] wrong: %+v", b[0])
	}
	if b[1].Name != "second_ia" || b[1].Type != "ia" {
		t.Fatalf("backend[1] wrong: %+v", b[1])
	}
}

func TestHTTPBackendsRejections(t *testing.T) {
	cases := []map[string]string{
		{ // missing URL
			"HARRBOR_HTTP_BACKENDS": "b1", "HARRBOR_HTTPBACKEND_B1_TYPE": "ia",
		},
		{ // unknown type
			"HARRBOR_HTTP_BACKENDS": "b1", "HARRBOR_HTTPBACKEND_B1_URL": "https://x.example",
			"HARRBOR_HTTPBACKEND_B1_TYPE": "scraper",
		},
		{ // dupe after normalization
			"HARRBOR_HTTP_BACKENDS": "B1,b1", "HARRBOR_HTTPBACKEND_B1_URL": "https://x.example",
			"HARRBOR_HTTPBACKEND_B1_TYPE": "ia",
		},
		{ // non-http scheme
			"HARRBOR_HTTP_BACKENDS": "b1", "HARRBOR_HTTPBACKEND_B1_URL": "ftp://x.example",
			"HARRBOR_HTTPBACKEND_B1_TYPE": "ia",
		},
		{ // URL userinfo (D11)
			"HARRBOR_HTTP_BACKENDS": "b1", "HARRBOR_HTTPBACKEND_B1_URL": "https://user:pass@x.example",
			"HARRBOR_HTTPBACKEND_B1_TYPE": "ia",
		},
		{ // public URL must be an origin
			"HARRBOR_HTTP_BACKENDS": "b1", "HARRBOR_HTTPBACKEND_B1_URL": "https://x.example",
			"HARRBOR_HTTPBACKEND_B1_PUBLIC_URL": "https://client.example/path",
			"HARRBOR_HTTPBACKEND_B1_TYPE":       "stremio",
		},
		{ // redirect origins are exact origins, never paths
			"HARRBOR_HTTP_BACKENDS": "b1", "HARRBOR_HTTPBACKEND_B1_URL": "https://x.example",
			"HARRBOR_HTTPBACKEND_B1_REDIRECT_ORIGINS": "https://redirect.example/path",
			"HARRBOR_HTTPBACKEND_B1_TYPE":             "stremio",
		},
		{ // invalid name chars
			"HARRBOR_HTTP_BACKENDS": "b one", "HARRBOR_HTTPBACKEND_B_ONE_URL": "https://x.example",
			"HARRBOR_HTTPBACKEND_B_ONE_TYPE": "ia",
		},
	}
	for i, env := range cases {
		env := env
		t.Run(strings.TrimSpace(env["HARRBOR_HTTP_BACKENDS"]), func(t *testing.T) {
			// t.Setenv scoping isolates each case's env inside its subtest.
			for k, v := range env {
				t.Setenv(k, v)
			}
			cfg := &Config{}
			if err := applyHTTPBackends(cfg); err == nil {
				t.Fatalf("case %d: expected rejection", i)
			}
		})
	}
}

func TestHTTPBackendsUnsetIsEmpty(t *testing.T) {
	cfg := &Config{}
	if err := applyHTTPBackends(cfg); err != nil {
		t.Fatalf("unset must not error: %v", err)
	}
	if len(cfg.HTTPStream.Backends) != 0 {
		t.Fatal("unset must produce zero backends")
	}
	if !strings.Contains(canonicalBackendEnv("archive-one"), "ARCHIVE_ONE") {
		t.Fatal("canonical env mapping wrong")
	}
}

func TestHTTPBackendRedirectOriginCap(t *testing.T) {
	var origins []string
	for i := 0; i < maxHTTPBackendRedirectOrigins+1; i++ {
		origins = append(origins, fmt.Sprintf("https://metadata-%d.example", i))
	}
	if _, err := ParseHTTPBackendRedirectOrigins(strings.Join(origins, ",")); err == nil {
		t.Fatal("over-cap redirect origin list accepted")
	}
}

func TestHTTPBackendsMixTuningAndSealedURL(t *testing.T) {
	cfg := defaultConfig()
	cfg.applySecrets(&SecretStore{values: map[string]util.Redacted{
		"HARRBOR_HTTPBACKEND_AIOSTREAMS_URL": util.Redacted("https://aiostreams.example/config-token"),
	}})
	applyTuning(&cfg, map[string]string{
		"HARRBOR_HTTP_STREAM_ENABLED":         "true",
		"HARRBOR_HTTP_BACKENDS":               "ia,aiostreams,cinepro",
		"HARRBOR_HTTPBACKEND_IA_URL":          "https://archive.org",
		"HARRBOR_HTTPBACKEND_IA_TYPE":         "ia",
		"HARRBOR_HTTPBACKEND_AIOSTREAMS_TYPE": "stremio",
		"HARRBOR_HTTPBACKEND_CINEPRO_URL":     "http://cinepro-core:3000",
		"HARRBOR_HTTPBACKEND_CINEPRO_TYPE":    "omss",
	})
	if err := applyHTTPBackends(&cfg); err != nil {
		t.Fatalf("applyHTTPBackends: %v", err)
	}
	if !cfg.HTTPStream.Enabled {
		t.Fatal("HTTP stream tuning flag not applied")
	}
	if len(cfg.HTTPStream.Backends) != 3 {
		t.Fatalf("backends = %d, want 3", len(cfg.HTTPStream.Backends))
	}
	if cfg.HTTPStream.Backends[1].Name != "aiostreams" || cfg.HTTPStream.Backends[1].Type != "stremio" {
		t.Fatalf("aiostreams backend = %+v", cfg.HTTPStream.Backends[1])
	}
}

func TestHTTPBackendGenericType(t *testing.T) {
	cfg, err := backendCfg(t, map[string]string{
		"HARRBOR_HTTP_BACKENDS":                   "g1",
		"HARRBOR_HTTPBACKEND_G1_TYPE":             "generic",
		"HARRBOR_HTTPBACKEND_G1_DESCRIPTORS_FILE": "/srv/docker/homelab/darkharrbor/generic-descriptors.json",
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfg.HTTPStream.Backends) != 1 {
		t.Fatalf("backends = %d, want 1", len(cfg.HTTPStream.Backends))
	}
	b := cfg.HTTPStream.Backends[0]
	if b.Type != "generic" || b.URL != "" || b.DescriptorsFile != "/srv/docker/homelab/darkharrbor/generic-descriptors.json" {
		t.Fatalf("generic backend wrong: %+v", b)
	}
}

func TestHTTPBackendGenericRequiresDescriptorsFile(t *testing.T) {
	_, err := backendCfg(t, map[string]string{
		"HARRBOR_HTTP_BACKENDS":       "g1",
		"HARRBOR_HTTPBACKEND_G1_TYPE": "generic",
	})
	if err == nil {
		t.Fatal("expected error when generic backend has no descriptors file")
	}
}

func TestHTTPBackendGenericRejectsRelativeDescriptorsFile(t *testing.T) {
	_, err := backendCfg(t, map[string]string{
		"HARRBOR_HTTP_BACKENDS":                   "g1",
		"HARRBOR_HTTPBACKEND_G1_TYPE":             "generic",
		"HARRBOR_HTTPBACKEND_G1_DESCRIPTORS_FILE": "relative/path.json",
	})
	if err == nil {
		t.Fatal("expected error for relative descriptors file path")
	}
}

func TestHTTPBackendEmptyTypeMeansAutodetect(t *testing.T) {
	cfg, err := backendCfg(t, map[string]string{
		"HARRBOR_HTTP_BACKENDS":      "b1",
		"HARRBOR_HTTPBACKEND_B1_URL": "https://x.example/",
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfg.HTTPStream.Backends) != 1 || cfg.HTTPStream.Backends[0].Type != "" {
		t.Fatalf("autodetect backend parsed incorrectly: %+v", cfg.HTTPStream.Backends)
	}
}

// TestHTTPAllowPrivateSourcesDefaultsFalse covers the HR0.2 live-gate
// SSRF-policy finding (2026-07-18): AllowPrivateSourceCIDRs stays off
// unless the operator explicitly opts in via
// HARRBOR_HTTP_ALLOW_PRIVATE_SOURCES — never a hardcoded, unconditional
// value with no config surface at all.
func TestHTTPAllowPrivateSourcesDefaultsFalse(t *testing.T) {
	t.Setenv("HARRBOR_TORBOX_API_TOKEN", "token")
	t.Setenv("HARRBOR_QBIT_PASSWORD", "not-the-default")
	t.Setenv("HARRBOR_SAB_API_KEY", "not-the-default")
	t.Setenv("HARRBOR_STREAM_SECRET", strings.Repeat("s", 32))

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPStream.AllowPrivateSourceCIDRs {
		t.Fatal("AllowPrivateSourceCIDRs must default to false")
	}
}

func TestHTTPAllowPrivateSourcesOptIn(t *testing.T) {
	t.Setenv("HARRBOR_TORBOX_API_TOKEN", "token")
	t.Setenv("HARRBOR_QBIT_PASSWORD", "not-the-default")
	t.Setenv("HARRBOR_SAB_API_KEY", "not-the-default")
	t.Setenv("HARRBOR_STREAM_SECRET", strings.Repeat("s", 32))
	t.Setenv("HARRBOR_HTTP_ALLOW_PRIVATE_SOURCES", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HTTPStream.AllowPrivateSourceCIDRs {
		t.Fatal("AllowPrivateSourceCIDRs should be true when explicitly opted in")
	}
}

func TestHTTPStreamIAFilePreference(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv("HARRBOR_HTTP_IA_FILE_PREFERENCE", "")
		cfg := defaultConfig()
		applyEnv(&cfg)
		if cfg.HTTPStream.IAFilePreference != "derivative" {
			t.Fatalf("default preference = %q", cfg.HTTPStream.IAFilePreference)
		}
	})
	t.Run("original", func(t *testing.T) {
		t.Setenv("HARRBOR_HTTP_IA_FILE_PREFERENCE", "original")
		cfg := defaultConfig()
		applyEnv(&cfg)
		if cfg.HTTPStream.IAFilePreference != "original" || len(cfg.StartupWarnings) != 0 {
			t.Fatalf("original preference = %q warnings=%v", cfg.HTTPStream.IAFilePreference, cfg.StartupWarnings)
		}
	})
	t.Run("unknown warns", func(t *testing.T) {
		t.Setenv("HARRBOR_HTTP_IA_FILE_PREFERENCE", "future-mode")
		cfg := defaultConfig()
		applyEnv(&cfg)
		if cfg.HTTPStream.IAFilePreference != "derivative" || len(cfg.StartupWarnings) != 1 {
			t.Fatalf("unknown preference = %q warnings=%v", cfg.HTTPStream.IAFilePreference, cfg.StartupWarnings)
		}
	})
}

// TestGovernorDeadSentinelGraceMinDefaultAndOverride covers the 2026-07-18
// soak finding: TorBox's own checking+zero-seed+100-day-sentinel-ETA signal
// gets its own short, dedicated grace period (default 5 min), separate from
// both the instant explicit-failure path and the longer generic stall
// timeout (StallTimeoutMin, default 30 min).
func TestGovernorDeadSentinelGraceMinDefaultAndOverride(t *testing.T) {
	t.Setenv("HARRBOR_TORBOX_API_TOKEN", "token")
	t.Setenv("HARRBOR_QBIT_PASSWORD", "not-the-default")
	t.Setenv("HARRBOR_SAB_API_KEY", "not-the-default")
	t.Setenv("HARRBOR_STREAM_SECRET", strings.Repeat("s", 32))

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Governor.DeadSentinelGraceMin != 5 {
		t.Fatalf("DeadSentinelGraceMin default = %d, want 5", cfg.Governor.DeadSentinelGraceMin)
	}

	t.Setenv("HARRBOR_GOVERNOR_DEAD_SENTINEL_GRACE_MIN", "10")
	cfg2, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.Governor.DeadSentinelGraceMin != 10 {
		t.Fatalf("DeadSentinelGraceMin override = %d, want 10", cfg2.Governor.DeadSentinelGraceMin)
	}
}
