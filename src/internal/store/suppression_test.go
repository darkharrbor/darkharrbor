package store

import (
	"context"
	"testing"
	"time"
)

func TestSuppression_RecordAndIsSuppressed_RoundTrips(t *testing.T) {
	s, closeDB := newSegmentOffsetTestStore(t)
	defer closeDB()
	ctx := context.Background()

	fp := "fp-abc123"
	if err := s.RecordSuppression(ctx, fp, "nzb", "dead_post", time.Hour); err != nil {
		t.Fatalf("RecordSuppression: %v", err)
	}
	suppressed, err := s.IsSuppressed(ctx, fp)
	if err != nil {
		t.Fatalf("IsSuppressed: %v", err)
	}
	if !suppressed {
		t.Fatal("expected fingerprint to be suppressed immediately after RecordSuppression")
	}
}

func TestSuppression_UnknownFingerprint_NotSuppressed(t *testing.T) {
	s, closeDB := newSegmentOffsetTestStore(t)
	defer closeDB()
	ctx := context.Background()

	suppressed, err := s.IsSuppressed(ctx, "never-recorded")
	if err != nil {
		t.Fatalf("IsSuppressed: %v", err)
	}
	if suppressed {
		t.Fatal("expected a never-recorded fingerprint to not be suppressed")
	}
	// Empty fingerprint is a no-op, not an error, on both sides.
	if err := s.RecordSuppression(ctx, "", "nzb", "dead_post", time.Hour); err != nil {
		t.Fatalf("RecordSuppression(empty fingerprint): %v", err)
	}
	if suppressed, err := s.IsSuppressed(ctx, ""); err != nil || suppressed {
		t.Fatalf("IsSuppressed(empty fingerprint) = suppressed=%v err=%v, want false/nil", suppressed, err)
	}
}

// TestSuppression_ExpiresAfterTTL proves the suppression genuinely stops
// applying once its TTL elapses — SF-03 is a bounded, expiring effect, not
// a permanent blacklist. Uses the injectable clock per the project's
// standing time-dependent-logic convention.
func TestSuppression_ExpiresAfterTTL(t *testing.T) {
	s, closeDB := newSegmentOffsetTestStore(t)
	defer closeDB()
	ctx := context.Background()

	fixedNow := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return fixedNow }
	if err := s.RecordSuppression(ctx, "fp-ttl", "torrent", "unsupported_format", time.Hour); err != nil {
		t.Fatalf("RecordSuppression: %v", err)
	}
	if suppressed, err := s.IsSuppressed(ctx, "fp-ttl"); err != nil || !suppressed {
		t.Fatalf("expected suppressed before TTL elapses, got suppressed=%v err=%v", suppressed, err)
	}

	// Advance the clock just before expiry: still suppressed.
	s.now = func() time.Time { return fixedNow.Add(59 * time.Minute) }
	if suppressed, err := s.IsSuppressed(ctx, "fp-ttl"); err != nil || !suppressed {
		t.Fatalf("expected still suppressed at 59min of a 60min TTL, got suppressed=%v err=%v", suppressed, err)
	}

	// Advance past expiry: no longer suppressed.
	s.now = func() time.Time { return fixedNow.Add(61 * time.Minute) }
	if suppressed, err := s.IsSuppressed(ctx, "fp-ttl"); err != nil || suppressed {
		t.Fatalf("expected NOT suppressed at 61min of a 60min TTL, got suppressed=%v err=%v", suppressed, err)
	}
}

// TestSuppression_RepeatedRecordExtendsWindowAndIncrementsCount proves a
// second observation of the same fingerprint before expiry extends the
// suppression from the new observation time and accumulates count, rather
// than creating a second row or leaving the original (possibly sooner)
// expiry in place.
func TestSuppression_RepeatedRecordExtendsWindowAndIncrementsCount(t *testing.T) {
	s, closeDB := newSegmentOffsetTestStore(t)
	defer closeDB()
	ctx := context.Background()

	fixedNow := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return fixedNow }
	if err := s.RecordSuppression(ctx, "fp-repeat", "nzb", "dead_post", time.Hour); err != nil {
		t.Fatalf("first RecordSuppression: %v", err)
	}

	// Re-observe 30 minutes later (before the first would have expired).
	s.now = func() time.Time { return fixedNow.Add(30 * time.Minute) }
	if err := s.RecordSuppression(ctx, "fp-repeat", "nzb", "dead_post", time.Hour); err != nil {
		t.Fatalf("second RecordSuppression: %v", err)
	}

	var rows, count int
	if err := s.db.QueryRow(`SELECT COUNT(*), MAX(count) FROM release_suppressions WHERE fingerprint='fp-repeat'`).Scan(&rows, &count); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("row count = %d, want exactly 1 (upsert, not insert)", rows)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2 after two observations", count)
	}

	// The window should now extend to 30min(first offset)+90min(1h from the
	// second observation) = 90min from the original fixedNow, i.e. still
	// suppressed at 89min even though the FIRST TTL alone would have expired
	// at 60min.
	s.now = func() time.Time { return fixedNow.Add(89 * time.Minute) }
	if suppressed, err := s.IsSuppressed(ctx, "fp-repeat"); err != nil || !suppressed {
		t.Fatalf("expected the re-observation to extend the suppression window, got suppressed=%v err=%v", suppressed, err)
	}
}

func TestSuppression_ZeroTTLDefaults(t *testing.T) {
	s, closeDB := newSegmentOffsetTestStore(t)
	defer closeDB()
	ctx := context.Background()

	if err := s.RecordSuppression(ctx, "fp-defaultttl", "http", "unsupported_format", 0); err != nil {
		t.Fatalf("RecordSuppression(ttl=0): %v", err)
	}
	if suppressed, err := s.IsSuppressed(ctx, "fp-defaultttl"); err != nil || !suppressed {
		t.Fatalf("expected a default TTL to still suppress immediately, got suppressed=%v err=%v", suppressed, err)
	}
}

// TestSuppression_NegativeTTLExpiresImmediately proves a negative TTL is
// NOT folded into the same default as zero -- it deliberately produces an
// already-past expiry (used elsewhere in this file to construct
// already-expired fixture rows), rather than a silent 72h fallback.
func TestSuppression_NegativeTTLExpiresImmediately(t *testing.T) {
	s, closeDB := newSegmentOffsetTestStore(t)
	defer closeDB()
	ctx := context.Background()

	if err := s.RecordSuppression(ctx, "fp-negativettl", "http", "unsupported_format", -time.Minute); err != nil {
		t.Fatalf("RecordSuppression(ttl<0): %v", err)
	}
	if suppressed, err := s.IsSuppressed(ctx, "fp-negativettl"); err != nil || suppressed {
		t.Fatalf("expected a negative TTL to be already-expired, not suppressed, got suppressed=%v err=%v", suppressed, err)
	}
}

// TestSuppression_SurvivesAcrossStoreInstances is the restart-persistence
// proof (DG-02): an in-flight suppression must be readable by a freshly
// opened *Store against the same database file, simulating a DarkHarrbor
// restart.
func TestSuppression_SurvivesAcrossStoreInstances(t *testing.T) {
	dbPath := t.TempDir() + "/restart.db"
	ctx := context.Background()

	db1, err := Open(ctx, dbPath, 5*time.Second)
	if err != nil {
		t.Fatalf("Open (first): %v", err)
	}
	if err := RunMigrationsFS(db1, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS (first): %v", err)
	}
	s1 := New(db1)
	if err := s1.RecordSuppression(ctx, "fp-restart", "nzb", "dead_post", 24*time.Hour); err != nil {
		t.Fatalf("RecordSuppression: %v", err)
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("close first db: %v", err)
	}

	db2, err := Open(ctx, dbPath, 5*time.Second)
	if err != nil {
		t.Fatalf("Open (second): %v", err)
	}
	defer func() { _ = db2.Close() }()
	s2 := New(db2)
	suppressed, err := s2.IsSuppressed(ctx, "fp-restart")
	if err != nil {
		t.Fatalf("IsSuppressed (post-restart): %v", err)
	}
	if !suppressed {
		t.Fatal("expected the suppression recorded before the simulated restart to still apply")
	}
}

func TestSuppression_CountersBreakOutByReasonAndExcludeExpired(t *testing.T) {
	s, closeDB := newSegmentOffsetTestStore(t)
	defer closeDB()
	ctx := context.Background()

	fixedNow := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return fixedNow }
	if err := s.RecordSuppression(ctx, "fp-1", "nzb", "dead_post", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSuppression(ctx, "fp-2", "nzb", "dead_post", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSuppression(ctx, "fp-3", "torrent", "unsupported_format", time.Hour); err != nil {
		t.Fatal(err)
	}
	// Already-expired by the time we count: must not appear.
	if err := s.RecordSuppression(ctx, "fp-expired", "http", "unsupported_format", -time.Minute); err != nil {
		t.Fatal(err)
	}

	counts, err := s.SuppressionCounters(ctx)
	if err != nil {
		t.Fatalf("SuppressionCounters: %v", err)
	}
	if counts.Total != 3 {
		t.Fatalf("Total = %d, want 3 (excluding the already-expired row)", counts.Total)
	}
	if counts.ByReason["dead_post"] != 2 {
		t.Fatalf("ByReason[dead_post] = %d, want 2", counts.ByReason["dead_post"])
	}
	if counts.ByReason["unsupported_format"] != 1 {
		t.Fatalf("ByReason[unsupported_format] = %d, want 1 (excluding the expired one)", counts.ByReason["unsupported_format"])
	}
}

func TestSuppression_PruneExpiredRemovesOnlyExpiredRows(t *testing.T) {
	s, closeDB := newSegmentOffsetTestStore(t)
	defer closeDB()
	ctx := context.Background()

	fixedNow := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return fixedNow }
	if err := s.RecordSuppression(ctx, "fp-keep", "nzb", "dead_post", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSuppression(ctx, "fp-gone", "nzb", "dead_post", -time.Minute); err != nil {
		t.Fatal(err)
	}

	n, err := s.PruneExpiredSuppressions(ctx)
	if err != nil {
		t.Fatalf("PruneExpiredSuppressions: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned %d rows, want exactly 1", n)
	}
	if suppressed, err := s.IsSuppressed(ctx, "fp-keep"); err != nil || !suppressed {
		t.Fatalf("expected fp-keep to survive pruning, got suppressed=%v err=%v", suppressed, err)
	}
	if suppressed, err := s.IsSuppressed(ctx, "fp-gone"); err != nil || suppressed {
		t.Fatalf("expected fp-gone to be pruned/not suppressed, got suppressed=%v err=%v", suppressed, err)
	}
}
