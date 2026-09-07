package metrics

import (
	"strings"
	"testing"
)

func TestRenderBasic(t *testing.T) {
	var b strings.Builder
	snap := Snapshot{
		UptimeSeconds:            42,
		ItemsByState:             map[string]int64{"ready": 3, "failed": 1},
		NNTPZeroFillTotal:        7,
		ErrorJournalEntriesTotal: 5,
		ErrorJournalByClass:      map[string]int64{"transient_here": 4, "permanent_here": 1},
		RangeCacheEntries:        map[string]int64{"disk_entries": 10},
		HTTPOriginScoreEntries:   2,
		TorrentCDNScoreEntries:   1,
		ReactiveCommits: map[string]map[string]int64{
			"24h": {"auto": 2, "supervised": 1},
		},
		ReactivePending:        map[string]int64{"positive_mismatch": 3},
		ReactivePendingAge:     map[string]int64{"positive_mismatch": 90},
		ReactiveIdentity:       map[string]int64{"strong_evidence": 2},
		ReactiveUndoTotal:      1,
		ReactivePromotionBytes: 4096,
		ReactiveHeadroomBytes:  8192,
		ReactiveHeadroomKnown:  true,
	}
	if err := Render(&b, snap); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := b.String()

	for _, want := range []string{
		"darkharrbor_uptime_seconds 42",
		`darkharrbor_items_total{state="ready"} 3`,
		`darkharrbor_items_total{state="failed"} 1`,
		"darkharrbor_nntp_zero_fill_events_total 7",
		"darkharrbor_error_journal_entries_total 5",
		`darkharrbor_error_journal_entries_by_class_total{class="transient_here"} 4`,
		`darkharrbor_rangecache{field="disk_entries"} 10`,
		"darkharrbor_http_origin_score_entries 2",
		"darkharrbor_torrent_cdn_score_entries 1",
		`darkharrbor_reactive_commits{mode="auto",period="24h"} 2`,
		`darkharrbor_reactive_pending_depth{reason="positive_mismatch"} 3`,
		`darkharrbor_reactive_pending_oldest_age_seconds{reason="positive_mismatch"} 90`,
		`darkharrbor_reactive_identity_outcomes_total{outcome="strong_evidence"} 2`,
		"# TYPE darkharrbor_reactive_undo_events_total counter",
		"darkharrbor_reactive_undo_events_total 1",
		"darkharrbor_reactive_promotion_bytes_total 4096",
		"darkharrbor_reactive_promotion_headroom_available 1",
		"darkharrbor_reactive_promotion_headroom_bytes 8192",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\nfull output:\n%s", want, out)
		}
	}
}

// TestRenderEmpty proves a scrape never fails/panics on a zero-value
// Snapshot (DG-05: absent/empty input abstains cleanly rather than erroring).
func TestRenderEmpty(t *testing.T) {
	var b strings.Builder
	if err := Render(&b, Snapshot{}); err != nil {
		t.Fatalf("Render(empty): %v", err)
	}
	if !strings.Contains(b.String(), "darkharrbor_uptime_seconds 0") {
		t.Errorf("expected zero-value uptime series, got:\n%s", b.String())
	}
	if !strings.Contains(b.String(), "darkharrbor_reactive_promotion_headroom_available 0") {
		t.Errorf("expected unavailable reactive headroom, got:\n%s", b.String())
	}
}

// TestFormatLabelsEscaping proves label-value escaping is applied so a
// pathological value (should never occur given this package's closed label
// vocabulary, but defense in depth) cannot break exposition-format parsing.
func TestFormatLabelsEscaping(t *testing.T) {
	got := formatLabels(map[string]string{"class": `weird"value` + "\nwith\\backslash"})
	want := `{class="weird\"value\nwith\\backslash"}`
	if got != want {
		t.Errorf("formatLabels: got %q, want %q", got, want)
	}
}
