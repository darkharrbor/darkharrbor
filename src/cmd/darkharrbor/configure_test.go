package main

import (
	"errors"
	"log/slog"
	"syscall"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/config"
)

func TestConfiguredLogLevel(t *testing.T) {
	tests := map[string]slog.Level{
		"DEBUG": slog.LevelDebug,
		"INFO":  slog.LevelInfo,
		"WARN":  slog.LevelWarn,
		"ERROR": slog.LevelError,
		"bad":   slog.LevelInfo,
	}
	for raw, want := range tests {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("HARRBOR_LOG_LEVEL", raw)
			if got := configuredLogLevel(&config.Config{}); got != want {
				t.Fatalf("configuredLogLevel(%q) = %v, want %v", raw, got, want)
			}
		})
	}
}

func TestRestartServerPID1Guard(t *testing.T) {
	var gotPID int
	var gotSignal syscall.Signal
	restarted, err := restartServerPID1(
		42,
		func(string) (string, error) { return "/usr/local/bin/darkharrbor", nil },
		func(pid int, signal syscall.Signal) error {
			gotPID, gotSignal = pid, signal
			return nil
		},
	)
	if err != nil || !restarted {
		t.Fatalf("restart = %v, %v", restarted, err)
	}
	if gotPID != 1 || gotSignal != syscall.SIGTERM {
		t.Fatalf("kill = (%d, %v), want (1, SIGTERM)", gotPID, gotSignal)
	}

	for name, test := range map[string]struct {
		pid int
		exe string
	}{
		"configure-is-pid1": {1, "/usr/local/bin/darkharrbor"},
		"wrong-pid1":        {42, "/sbin/init"},
	} {
		t.Run(name, func(t *testing.T) {
			killed := false
			restarted, err := restartServerPID1(
				test.pid,
				func(string) (string, error) { return test.exe, nil },
				func(int, syscall.Signal) error { killed = true; return nil },
			)
			if err != nil || restarted || killed {
				t.Fatalf("restart = %v, killed=%v, err=%v", restarted, killed, err)
			}
		})
	}

	wantErr := errors.New("kill failed")
	if restarted, err := restartServerPID1(
		42,
		func(string) (string, error) { return "/usr/local/bin/darkharrbor", nil },
		func(int, syscall.Signal) error { return wantErr },
	); restarted || !errors.Is(err, wantErr) {
		t.Fatalf("restart error = %v, %v", restarted, err)
	}
}

func TestRangeCacheWorkersUseLargestProviderPool(t *testing.T) {
	t.Setenv("HARRBOR_PROVIDER_OTHER_READAHEAD_CONNECTIONS", "27")
	var cfg config.Config
	cfg.Cache.ReadaheadConnections = 8
	cfg.Providers.UsenetProviders = []string{"other"}
	if got := rangecacheConfigFromCfg(&cfg).ReadaheadWorkers; got != 27 {
		t.Fatalf("readahead workers = %d, want 27", got)
	}
}
