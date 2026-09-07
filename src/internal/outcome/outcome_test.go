package outcome

import (
	"errors"
	"fmt"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/provider"
)

func TestClassifyNil(t *testing.T) {
	if got := Classify(nil); got != "" {
		t.Fatalf("Classify(nil) = %q, want empty", got)
	}
}

func TestClassifyWrappedBuckets(t *testing.T) {
	base := errors.New("boom")
	cases := []struct {
		name string
		err  error
		want Class
	}{
		{"permanent", Permanent(base), ClassPermanentHere},
		{"transient", Transient(base), ClassTransientHere},
		{"account", AccountLevel(base), ClassAccountLevel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.err); got != tc.want {
				t.Fatalf("Classify(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestClassifyPreservesErrorsIsIdentity(t *testing.T) {
	sentinel := errors.New("dead post")
	wrapped := fmt.Errorf("nntp: probe: consecutive failures: %w", Permanent(sentinel))
	if !errors.Is(wrapped, sentinel) {
		t.Fatalf("errors.Is lost identity through outcome.Permanent wrap")
	}
	if got := Classify(wrapped); got != ClassPermanentHere {
		t.Fatalf("Classify(wrapped) = %q, want %q", got, ClassPermanentHere)
	}
}

func TestClassifyRetryableFallsBackToTransient(t *testing.T) {
	err := provider.MarkRetryable(errors.New("temporary"))
	if got := Classify(err); got != ClassTransientHere {
		t.Fatalf("Classify(retryable) = %q, want %q", got, ClassTransientHere)
	}
}

func TestClassifyUnknownFallback(t *testing.T) {
	err := errors.New("some unclassified failure")
	if got := Classify(err); got != ClassUnknown {
		t.Fatalf("Classify(plain error) = %q, want %q", got, ClassUnknown)
	}
}

func TestClassifyWrappedNilIsNil(t *testing.T) {
	if Permanent(nil) != nil {
		t.Fatalf("Permanent(nil) should return nil")
	}
	if Transient(nil) != nil {
		t.Fatalf("Transient(nil) should return nil")
	}
	if AccountLevel(nil) != nil {
		t.Fatalf("AccountLevel(nil) should return nil")
	}
}

func TestClassifyTorrentOutcome(t *testing.T) {
	cases := []struct {
		outcome provider.TorrentOutcome
		want    Class
	}{
		{provider.TorrentOutcomeReady, ""},
		{"", ""},
		{provider.TorrentOutcomeTerminalDeadSource, ClassPermanentHere},
		{provider.TorrentOutcomeRemoteRemoved, ClassPermanentHere},
		{provider.TorrentOutcomeCancelled, ClassPermanentHere},
		{provider.TorrentOutcomeTransientError, ClassTransientHere},
		{provider.TorrentOutcomeStalled, ClassTransientHere},
		{provider.TorrentOutcomeDeadSentinel, ClassTransientHere},
		{provider.TorrentOutcomeCapacityLimited, ClassAccountLevel},
		{provider.TorrentOutcomeUnknown, ClassUnknown},
		{provider.TorrentOutcome("something_new"), ClassUnknown},
	}
	for _, tc := range cases {
		if got := ClassifyTorrentOutcome(tc.outcome); got != tc.want {
			t.Errorf("ClassifyTorrentOutcome(%q) = %q, want %q", tc.outcome, got, tc.want)
		}
	}
}
