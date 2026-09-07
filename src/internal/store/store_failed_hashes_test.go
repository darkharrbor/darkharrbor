package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newTestStoreForFailedHashes(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "failed_hashes_test.db")
	db, err := Open(context.Background(), dbPath, 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	return New(db)
}

func TestRecordFailureThresholdMode(t *testing.T) {
	s := newTestStoreForFailedHashes(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 13, 6, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return now })

	for strike := 1; strike <= failHashThreshold; strike++ {
		result, err := s.RecordFailure(ctx, "threshold-key")
		if err != nil {
			t.Fatalf("RecordFailure strike %d: %v", strike, err)
		}
		if result.FailCount != strike {
			t.Fatalf("strike %d fail count = %d, want %d", strike, result.FailCount, strike)
		}
		wantBlacklisted := strike >= failHashThreshold
		if result.Blacklisted != wantBlacklisted {
			t.Fatalf("strike %d blacklisted = %v, want %v", strike, result.Blacklisted, wantBlacklisted)
		}
		if wantBlacklisted {
			wantUntil := now.Add(blacklistDuration)
			if result.BlacklistedUntil == nil || !result.BlacklistedUntil.Equal(wantUntil) {
				t.Fatalf("strike %d blacklist expiry = %v, want %v", strike, result.BlacklistedUntil, wantUntil)
			}
		} else if result.BlacklistedUntil != nil {
			t.Fatalf("strike %d blacklist expiry = %v, want nil", strike, result.BlacklistedUntil)
		}
	}
}

func TestBlacklistImmediatelyInsertUpdateAndExpiry(t *testing.T) {
	s := newTestStoreForFailedHashes(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 13, 7, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return now })

	first, err := s.BlacklistImmediately(ctx, "terminal-key")
	if err != nil {
		t.Fatalf("BlacklistImmediately insert: %v", err)
	}
	wantFirstExpiry := now.Add(blacklistDuration)
	if first.FailCount != 1 || !first.Blacklisted || first.BlacklistedUntil == nil || !first.BlacklistedUntil.Equal(wantFirstExpiry) {
		t.Fatalf("first result = %+v, want count=1 active until %v", first, wantFirstExpiry)
	}

	now = now.Add(2 * time.Hour)
	second, err := s.BlacklistImmediately(ctx, "terminal-key")
	if err != nil {
		t.Fatalf("BlacklistImmediately update: %v", err)
	}
	wantSecondExpiry := now.Add(blacklistDuration)
	if second.FailCount != 2 || !second.Blacklisted || second.BlacklistedUntil == nil || !second.BlacklistedUntil.Equal(wantSecondExpiry) {
		t.Fatalf("second result = %+v, want count=2 active until %v", second, wantSecondExpiry)
	}

	now = wantSecondExpiry
	active, err := s.IsBlacklisted(ctx, "terminal-key")
	if err != nil {
		t.Fatalf("IsBlacklisted at expiry: %v", err)
	}
	if active {
		t.Fatal("blacklist remained active at its expiry boundary")
	}

	expired, err := s.ExpireBlacklists(ctx)
	if err != nil {
		t.Fatalf("ExpireBlacklists: %v", err)
	}
	if expired != 1 {
		t.Fatalf("ExpireBlacklists affected %d rows, want 1", expired)
	}
}

func TestFailureMemoryPersistsAcrossStoreReopen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "failed_hashes_persistence.db")
	now := time.Date(2026, 7, 13, 8, 0, 0, 0, time.UTC)

	db, err := Open(ctx, dbPath, 5*time.Second)
	if err != nil {
		t.Fatalf("Open first: %v", err)
	}
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		_ = db.Close()
		t.Fatalf("RunMigrationsFS first: %v", err)
	}
	firstStore := New(db)
	firstStore.SetClock(func() time.Time { return now })
	written, err := firstStore.BlacklistImmediately(ctx, "persistent-key")
	if err != nil {
		_ = db.Close()
		t.Fatalf("BlacklistImmediately: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close first DB: %v", err)
	}

	db, err = Open(ctx, dbPath, 5*time.Second)
	if err != nil {
		t.Fatalf("Open second: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	secondStore := New(db)
	secondStore.SetClock(func() time.Time { return now })

	active, err := secondStore.IsBlacklisted(ctx, "persistent-key")
	if err != nil {
		t.Fatalf("IsBlacklisted after reopen: %v", err)
	}
	if !active {
		t.Fatal("persisted blacklist was not active after store reopen")
	}

	var failCount int
	var blacklistUntil string
	if err := db.QueryRowContext(ctx,
		`SELECT fail_count, blacklisted_until FROM failed_hashes WHERE hash=?`,
		"persistent-key",
	).Scan(&failCount, &blacklistUntil); err != nil {
		t.Fatalf("read persisted failure memory: %v", err)
	}
	if failCount != written.FailCount || blacklistUntil != formatTime(*written.BlacklistedUntil) {
		t.Fatalf("persisted row = count %d until %s, want count %d until %s", failCount, blacklistUntil, written.FailCount, formatTime(*written.BlacklistedUntil))
	}
}

func TestIncrementFailCountCompatibilityWrapper(t *testing.T) {
	s := newTestStoreForFailedHashes(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return now })

	for strike := 1; strike <= failHashThreshold; strike++ {
		if err := s.IncrementFailCount(ctx, "compatibility-key"); err != nil {
			t.Fatalf("IncrementFailCount strike %d: %v", strike, err)
		}
		active, err := s.IsBlacklisted(ctx, "compatibility-key")
		if err != nil {
			t.Fatalf("IsBlacklisted strike %d: %v", strike, err)
		}
		wantActive := strike >= failHashThreshold
		if active != wantActive {
			t.Fatalf("strike %d active = %v, want %v", strike, active, wantActive)
		}
	}
}
