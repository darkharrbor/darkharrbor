package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/config"
)

func TestNNTPProviderRetentionDaysBoundsAndAbstains(t *testing.T) {
	t.Setenv("HARRBOR_PROVIDER_FIRST_RETENTION_DAYS", "6000")
	t.Setenv("HARRBOR_PROVIDER_BAD_RETENTION_DAYS", "not-a-number")
	t.Setenv("HARRBOR_PROVIDER_ZERO_RETENTION_DAYS", "0")
	t.Setenv("HARRBOR_PROVIDER_HUGE_RETENTION_DAYS", "36501")
	t.Setenv("HARRBOR_PROVIDER_UNCONFIGURED_RETENTION_DAYS", "9000")

	cfg := &config.Config{}
	got := nntpProviderRetentionDays(
		cfg,
		[]string{"first", "bad", "zero", "huge", "missing"},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if len(got) != 1 || got["first"] != 6000 {
		t.Fatalf("retention map = %v, want only first=6000", got)
	}
}
