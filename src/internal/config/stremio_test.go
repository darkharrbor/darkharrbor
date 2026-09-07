package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStremioConfigValidation(t *testing.T) {
	secret := strings.Repeat("s", 32)
	for _, tc := range []struct {
		name    string
		cfg     StremioConfig
		wantErr bool
	}{
		{"internal-default", StremioConfig{EdgeMode: StremioEdgeInternal}, false},
		{"tailscale-https", StremioConfig{EdgeMode: StremioEdgeTailscale, ClientAddress: ":8382", ClientBaseURL: "https://dh.example.test"}, false},
		{"localhost-http", StremioConfig{EdgeMode: StremioEdgeLAN, ClientAddress: ":8382", ClientBaseURL: "http://127.0.0.1:8382"}, false},
		{"remote-http", StremioConfig{EdgeMode: StremioEdgeLAN, ClientAddress: ":8382", ClientBaseURL: "http://192.0.2.1:8382"}, true},
		{"origin-path", StremioConfig{EdgeMode: StremioEdgeCustom, ClientAddress: ":8382", ClientBaseURL: "https://dh.example.test/path"}, true},
		{"unknown-mode", StremioConfig{EdgeMode: "public", ClientAddress: ":8382", ClientBaseURL: "https://dh.example.test"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate(secret)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestUnknownStremioModeWarnsAndFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "darkharrbor.conf")
	if err := os.WriteFile(path, []byte("HARRBOR_STREMIO_EDGE_MODE=unknown\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	values, warnings, err := ReadTuningWithWarnings(path)
	if err != nil || len(warnings) != 1 || values["HARRBOR_STREMIO_EDGE_MODE"] != "" {
		t.Fatalf("values=%v warnings=%v err=%v", values, warnings, err)
	}
}

func TestUnknownStremioEnvironmentModeWarnsAndFailsClosed(t *testing.T) {
	t.Setenv("HARRBOR_STREMIO_EDGE_MODE", "unknown")
	cfg := defaultConfig()
	applyEnv(&cfg)
	if cfg.Stremio.EdgeMode != StremioEdgeInternal || len(cfg.StartupWarnings) != 1 {
		t.Fatalf("mode=%q warnings=%v", cfg.Stremio.EdgeMode, cfg.StartupWarnings)
	}
}

func TestStremioInstallTokenIsStableAndSeparate(t *testing.T) {
	cfg := StremioConfig{}
	a := cfg.EffectiveInstallToken(strings.Repeat("a", 32))
	b := cfg.EffectiveInstallToken(strings.Repeat("b", 32))
	if a == b || len(a) != 64 || strings.Contains(a, strings.Repeat("a", 8)) {
		t.Fatalf("derived install token is not stable opaque material")
	}
	if got := cfg.EffectiveInstallToken(strings.Repeat("a", 32)); got != a {
		t.Fatal("derived install token changed for identical secret")
	}
}

func FuzzParseStremioClientBaseURL(f *testing.F) {
	f.Add("https://dh.example.test")
	f.Add("http://127.0.0.1:8382")
	f.Add("https://user@example.test/path?query=value")
	f.Fuzz(func(t *testing.T, raw string) {
		u, err := ParseStremioClientBaseURL(raw)
		if err == nil && (u == nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "") {
			t.Fatalf("accepted non-origin URL")
		}
	})
}
