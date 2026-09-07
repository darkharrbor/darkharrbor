package config

import (
	"os"
	"testing"
)

func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		old, existed := os.LookupEnv(k)
		if err := os.Setenv(k, v); err != nil {
			t.Fatalf("setenv %s: %v", k, err)
		}
		t.Cleanup(func() {
			if existed {
				_ = os.Setenv(k, old)
			} else {
				_ = os.Unsetenv(k)
			}
		})
	}
}

func TestApplyArrTargets_Unset(t *testing.T) {
	os.Unsetenv("HARRBOR_ARR_NAMES")
	cfg := &Config{}
	applyArrTargets(cfg)
	if len(cfg.Arrs) != 0 {
		t.Errorf("Arrs = %v, want empty when HARRBOR_ARR_NAMES unset", cfg.Arrs)
	}
}

func TestApplyArrTargets_ParsesMultipleInstances(t *testing.T) {
	withEnv(t, map[string]string{
		"HARRBOR_ARR_NAMES":                "sonarr-modern,radarr",
		"HARRBOR_ARR_SONARR_MODERN_URL":    "http://sonarr-modern:8989/",
		"HARRBOR_ARR_SONARR_MODERN_APIKEY": "key-sm",
		"HARRBOR_ARR_RADARR_URL":           "http://radarr:7878",
		"HARRBOR_ARR_RADARR_APIKEY":        "key-radarr",
	})
	cfg := &Config{}
	applyArrTargets(cfg)

	if len(cfg.Arrs) != 2 {
		t.Fatalf("Arrs = %v, want 2 entries", cfg.Arrs)
	}
	sm := cfg.Arrs[0]
	if sm.Name != "sonarr-modern" || sm.BaseURL != "http://sonarr-modern:8989" || sm.APIKey != "key-sm" {
		t.Errorf("Arrs[0] = %+v, want trimmed-slash sonarr-modern entry", sm)
	}
	rd := cfg.Arrs[1]
	if rd.Name != "radarr" || rd.BaseURL != "http://radarr:7878" || rd.APIKey != "key-radarr" {
		t.Errorf("Arrs[1] = %+v, want radarr entry", rd)
	}
}

func TestApplyArrTargets_SkipsNameWithoutURL(t *testing.T) {
	withEnv(t, map[string]string{
		"HARRBOR_ARR_NAMES": "sonarr-cartoons,radarr",
		// sonarr-cartoons intentionally has no URL configured.
		"HARRBOR_ARR_RADARR_URL":    "http://radarr:7878",
		"HARRBOR_ARR_RADARR_APIKEY": "key-radarr",
	})
	cfg := &Config{}
	applyArrTargets(cfg)

	if len(cfg.Arrs) != 1 {
		t.Fatalf("Arrs = %v, want exactly 1 (radarr only, sonarr-cartoons skipped)", cfg.Arrs)
	}
	if cfg.Arrs[0].Name != "radarr" {
		t.Errorf("Arrs[0].Name = %q, want radarr", cfg.Arrs[0].Name)
	}
}
