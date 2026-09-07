package store

import (
	"context"
	"testing"
	"time"
)

func newTestStoreForRepresentationMetrics(t *testing.T) (*Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir()+"/rx82.db", time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	return New(db), ctx
}

func insertCoverage(t *testing.T, s *Store, ctx context.Context, representationID string, at time.Time) {
	t.Helper()
	if _, err := s.db.ExecContext(ctx, `
        INSERT INTO playback_coverage (representation_id, byte_start, byte_end, updated_at)
        VALUES (?, 0, 1024, ?)`, representationID, at.UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert coverage %s: %v", representationID, err)
	}
}

// The ABSENCE case must be visible without database access: a deployment that
// has never served aggregator-proxied playback reports known=false rather than
// an indistinguishable zero. RX-8.1 alone cannot make that distinction.
func TestPlaybackRepresentationMetricsAbsenceIsVisible(t *testing.T) {
	s, ctx := newTestStoreForRepresentationMetrics(t)
	got, err := s.CurrentPlaybackRepresentationMetrics(ctx, time.Now())
	if err != nil {
		t.Fatalf("CurrentPlaybackRepresentationMetrics: %v", err)
	}
	if got.AggregatorKnown {
		t.Fatal("no delivery has occurred, so AggregatorKnown must be false")
	}
	if n, ok := got.ByClass["mediaflow"]; !ok || n != 0 {
		t.Fatalf("mediaflow class must be present and zero, got %d (present=%v)", n, ok)
	}
}

// A genuine aggregator-proxied delivery moves the counter and advances recency.
func TestPlaybackRepresentationMetricsCountsAndRecency(t *testing.T) {
	s, ctx := newTestStoreForRepresentationMetrics(t)
	now := time.Now().UTC()
	insertCoverage(t, s, ctx, "mediaflow:aaaa", now.Add(-90*time.Second))
	insertCoverage(t, s, ctx, "mediaflow:bbbb", now.Add(-30*time.Second))
	insertCoverage(t, s, ctx, "torrent:cccc", now.Add(-10*time.Minute))

	got, err := s.CurrentPlaybackRepresentationMetrics(ctx, now)
	if err != nil {
		t.Fatalf("CurrentPlaybackRepresentationMetrics: %v", err)
	}
	if got.ByClass["mediaflow"] != 2 {
		t.Fatalf("mediaflow count = %d, want 2", got.ByClass["mediaflow"])
	}
	if got.ByClass["torrent"] != 1 {
		t.Fatalf("torrent count = %d, want 1", got.ByClass["torrent"])
	}
	if !got.AggregatorKnown {
		t.Fatal("AggregatorKnown must be true after a delivery")
	}
	if got.AggregatorAge > 60*time.Second {
		t.Fatalf("recency must reflect the NEWEST delivery, got %s", got.AggregatorAge)
	}
}

// DG-04 / fixed cardinality: an unrecognized prefix must fold into "other"
// rather than introducing a new label, and no representation identity may leak
// into any key.
func TestPlaybackRepresentationMetricsFixedCardinality(t *testing.T) {
	s, ctx := newTestStoreForRepresentationMetrics(t)
	now := time.Now().UTC()
	insertCoverage(t, s, ctx, "someFutureLane:zzzz", now)
	insertCoverage(t, s, ctx, "https://user:secret@host/path", now)
	insertCoverage(t, s, ctx, "noseparator", now)

	got, err := s.CurrentPlaybackRepresentationMetrics(ctx, now)
	if err != nil {
		t.Fatalf("CurrentPlaybackRepresentationMetrics: %v", err)
	}
	if got.ByClass["other"] != 3 {
		t.Fatalf("unrecognized prefixes must fold into other, got %d", got.ByClass["other"])
	}
	want := map[string]bool{"mediaflow": true, "torrent": true, "http": true, "nntp": true, "archive": true, "other": true}
	for key := range got.ByClass {
		if !want[key] {
			t.Fatalf("unexpected label %q: cardinality is not fixed", key)
		}
	}
}
