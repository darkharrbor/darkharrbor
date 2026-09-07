package main

import (
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/config"
)

func TestDemoAcquisitionConfigured(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.Config
		want bool
	}{
		{name: "none"},
		{name: "routing preference", cfg: configWithDemoPreference(), want: true},
		{name: "disabled HTTP backend", cfg: configWithDemoHTTP(false)},
		{name: "enabled HTTP backend", cfg: configWithDemoHTTP(true), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := demoAcquisitionConfigured(&tt.cfg); got != tt.want {
				t.Fatalf("demoAcquisitionConfigured() = %v, want %v", got, tt.want)
			}
		})
	}
}

func configWithDemoPreference() config.Config {
	var cfg config.Config
	cfg.Routing.Preference = []string{"cached-torrent"}
	return cfg
}

func configWithDemoHTTP(enabled bool) config.Config {
	var cfg config.Config
	cfg.HTTPStream.Enabled = enabled
	cfg.HTTPStream.Backends = []config.HTTPBackend{{Name: "ia", Type: "ia"}}
	return cfg
}
