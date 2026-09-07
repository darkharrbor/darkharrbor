package torbox

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/provider"
)

func TestParseCheckcachedResponsePresenceMeansCached(t *testing.T) {
	hash := "0123456789abcdef0123456789abcdef01234567"
	got, err := parseCheckcachedResponse(&apiEnvelope{Data: json.RawMessage(`{"0123456789abcdef0123456789abcdef01234567":{"name":"release"}}`)}, hash)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Cached || len(got.Files) != 0 {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestParseCheckcachedResponseNullMeansUncached(t *testing.T) {
	got, err := parseCheckcachedResponse(&apiEnvelope{Data: json.RawMessage(`null`)}, "0123456789abcdef0123456789abcdef01234567")
	if err != nil {
		t.Fatal(err)
	}
	if got.Cached {
		t.Fatalf("unexpected cached result: %+v", got)
	}
}

func TestParseCheckcachedResponseFiles(t *testing.T) {
	hash := "0123456789abcdef0123456789abcdef01234567"
	got, err := parseCheckcachedResponse(&apiEnvelope{Data: json.RawMessage(`{"0123456789abcdef0123456789abcdef01234567":{"files":[{"id":7,"name":"release.part01.rar","short_name":"release.part01.rar","size":123}]}}`)}, hash)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Cached || len(got.Files) != 1 || got.Files[0].Name != "release.part01.rar" || got.Files[0].Size != 123 {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestIsSlotActiveState(t *testing.T) {
	cases := []struct {
		state string
		want  bool
	}{
		{"downloading", true},
		{"seeding", false},
		{"uploading", false},
		{"stalled (no seeds)", true},
		{"metaDL", true},
		{"checking resume data", true},
		{"completed", false},
		{"cached", false},
		{"paused", false},
		{"queued", false},
		{"failed", false},
		{"error", false},
		{"", false},
		{"DOWNLOADING", true},
	}
	for _, c := range cases {
		if got := isSlotActiveState(c.state); got != c.want {
			t.Errorf("isSlotActiveState(%q) = %v, want %v", c.state, got, c.want)
		}
	}
}

func TestOccupiesUncachedTorrentSlot(t *testing.T) {
	cases := []struct {
		name string
		item map[string]any
		want bool
	}{
		{"downloading", map[string]any{"download_state": "downloading"}, true},
		{"checking", map[string]any{"download_state": "checking"}, true},
		{"stalled", map[string]any{"download_state": "stalled (no seeds)"}, true},
		{"metadata", map[string]any{"download_state": "metaDL"}, true},
		{"ready overrides stale state", map[string]any{"download_state": "downloading", "download_present": true}, false},
		{"ready field", map[string]any{"download_state": "downloading", "download_ready": true}, false},
		{"cached label", map[string]any{"download_state": "downloading", "download_label": "cached"}, false},
		{"seeding", map[string]any{"download_state": "seeding"}, false},
		{"unknown", map[string]any{"download_state": "mystery"}, false},
		{"empty", map[string]any{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := occupiesUncachedTorrentSlot(tc.item); got != tc.want {
				t.Fatalf("occupiesUncachedTorrentSlot() = %v, want %v", got, tc.want)
			}
		})
	}
}

func FuzzOccupiesUncachedTorrentSlot(f *testing.F) {
	f.Add("downloading", "", false, false)
	f.Add("seeding", "cached", true, true)
	f.Add("", "", false, false)
	f.Fuzz(func(t *testing.T, state, label string, present, ready bool) {
		item := map[string]any{
			"download_state":   state,
			"download_label":   label,
			"download_present": present,
			"download_ready":   ready,
		}
		got := occupiesUncachedTorrentSlot(item)
		if present || ready || strings.EqualFold(strings.TrimSpace(label), "cached") ||
			strings.EqualFold(strings.TrimSpace(label), "download ready") {
			if got {
				t.Fatal("ready/cached torrent consumed an uncached slot")
			}
		}
	})
}

func TestParseTaskStatusNormalizedOutcomes(t *testing.T) {
	tests := []struct {
		name    string
		item    map[string]any
		outcome provider.TorrentOutcome
		code    provider.TorrentFailureCode
	}{
		{"ready", map[string]any{"download_present": true}, provider.TorrentOutcomeReady, provider.TorrentFailureNone},
		{"dead sentinel", map[string]any{"download_state": "checking", "seeds": 0, "eta": 8640000}, provider.TorrentOutcomeDeadSentinel, provider.TorrentFailureTerminalDeadSource},
		// Regression: 2026-07-18 soak finding. This ambiguous TorBox signal
		// (checking, zero seeders, TorBox's own 100-day sentinel) must NOT
		// map to the instant-fail TerminalDeadSource outcome -- it was
		// observed live to appear within seconds of a submission that later
		// resolved successfully. Only an explicit provider failure state
		// (below) gets the instant-fail outcome.
		{"incomplete", map[string]any{"download_state": "incomplete"}, provider.TorrentOutcomeTerminalDeadSource, provider.TorrentFailureTerminalDeadSource},
		{"cancelled", map[string]any{"download_state": "cancelled"}, provider.TorrentOutcomeCancelled, provider.TorrentFailureCancelled},
		{"stalled", map[string]any{"download_state": "stalled (no seeds)"}, provider.TorrentOutcomeStalled, provider.TorrentFailureTransientStall},
		{"generic error", map[string]any{"download_state": "error"}, provider.TorrentOutcomeTransientError, provider.TorrentFailureProviderUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseTaskStatus("torrent", tt.item)
			if got.Outcome != tt.outcome {
				t.Fatalf("Outcome=%q want %q", got.Outcome, tt.outcome)
			}
			if got.FailureCode != tt.code {
				t.Fatalf("FailureCode=%q want %q", got.FailureCode, tt.code)
			}
		})
	}
}
