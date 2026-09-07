package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newTestStoreForFailedPrune(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "failed_prune_test.db")
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

func mustCreateItemForPrune(t *testing.T, s *Store, id string, state ItemState) *Item {
	t.Helper()
	now := time.Now().UTC()
	item := &Item{
		ID:            id,
		PublicID:      id,
		SourceType:    SourceTypeNZB,
		ClientKind:    ClientKindSAB,
		Category:      "darkharrbor",
		State:         state,
		SubmissionKey: id,
		DisplayName:   id,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.CreateItem(context.Background(), item); err != nil {
		t.Fatalf("CreateItem(%s): %v", id, err)
	}
	return item
}

// TestListFailedOlderThan_ScopedToFailedState covers the 2026-07-01 fix:
// StateFailed items previously had no age-out path at all (unlike
// StateRemoved), so DH's SAB-emulator history kept reporting a permanently
// failed NZB back to the arr forever, and Sonarr's queue rebuilt from that
// history on every poll — a failed item that should have aged out would
// keep resurrecting in the arr's queue.
func TestListFailedOlderThan_ScopedToFailedState(t *testing.T) {
	s := newTestStoreForFailedPrune(t)
	ctx := context.Background()

	mustCreateItemForPrune(t, s, "old-failed", StateFailed)
	mustCreateItemForPrune(t, s, "old-removed", StateRemoved) // must NOT be picked up by ListFailedOlderThan
	mustCreateItemForPrune(t, s, "old-ready", StateReady)     // must NOT be picked up

	// Backdate updated_at for all three so they're all "old" — only state
	// should determine which one ListFailedOlderThan returns.
	past := time.Now().UTC().Add(-48 * time.Hour)
	for _, id := range []string{"old-failed", "old-removed", "old-ready"} {
		if _, err := s.db.ExecContext(ctx, `UPDATE items SET updated_at=? WHERE id=?`, formatTime(past), id); err != nil {
			t.Fatalf("backdate %s: %v", id, err)
		}
	}

	// A recent failed item should NOT be picked up by a 24h cutoff.
	mustCreateItemForPrune(t, s, "recent-failed", StateFailed)

	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	got, err := s.ListFailedOlderThan(ctx, cutoff, 100)
	if err != nil {
		t.Fatalf("ListFailedOlderThan: %v", err)
	}
	if len(got) != 1 || got[0].ID != "old-failed" {
		ids := make([]string, 0, len(got))
		for _, it := range got {
			ids = append(ids, it.ID)
		}
		t.Fatalf("got %v, want exactly [old-failed]", ids)
	}
}

// TestDeleteRemovedItemsByIDs_ActuallyDeletes is a regression test for a
// pre-existing bug found while adding failed-item pruning: item_events has
// an enforced FK to items(id) (PRAGMA foreign_keys=ON), and every item has
// at least one item_events row (CreateItem's own AppendEvent call), so the
// original "DELETE FROM items WHERE state='removed' AND id IN (...)" ALWAYS
// violated the FK constraint and returned an error — silently, since the
// prune cycle only logs a non-fatal warning. The removed-item prune cycle
// had likely never deleted a single row in production.
func TestDeleteRemovedItemsByIDs_ActuallyDeletes(t *testing.T) {
	s := newTestStoreForFailedPrune(t)
	ctx := context.Background()

	mustCreateItemForPrune(t, s, "removed-1", StateRemoved)

	n, err := s.DeleteRemovedItemsByIDs(ctx, []string{"removed-1"})
	if err != nil {
		t.Fatalf("DeleteRemovedItemsByIDs: %v (this is the bug — it must not error)", err)
	}
	if n != 1 {
		t.Fatalf("deleted %d rows, want 1", n)
	}

	gone, err := s.GetItemByID(ctx, "removed-1")
	if err != nil {
		t.Fatalf("GetItemByID: %v", err)
	}
	if gone != nil {
		t.Error("removed-1 still present after DeleteRemovedItemsByIDs")
	}
}

// TestDeleteFailedItemsByIDs_ScopedToFailedState mirrors
// DeleteRemovedItemsByIDs's state guard: passing an ID for an item that is
// NOT in StateFailed must not delete it, even if the caller's ID list is
// stale or wrong.
func TestDeleteFailedItemsByIDs_ScopedToFailedState(t *testing.T) {
	s := newTestStoreForFailedPrune(t)
	ctx := context.Background()

	mustCreateItemForPrune(t, s, "failed-1", StateFailed)
	mustCreateItemForPrune(t, s, "ready-1", StateReady)

	n, err := s.DeleteFailedItemsByIDs(ctx, []string{"failed-1", "ready-1"})
	if err != nil {
		t.Fatalf("DeleteFailedItemsByIDs: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted %d rows, want 1 (only the StateFailed item)", n)
	}

	remaining, err := s.GetItemByID(ctx, "ready-1")
	if err != nil {
		t.Fatalf("GetItemByID(ready-1): %v", err)
	}
	if remaining == nil {
		t.Error("ready-1 was deleted, but it was StateReady and should have been protected")
	}

	gone, err := s.GetItemByID(ctx, "failed-1")
	if err != nil {
		t.Fatalf("GetItemByID(failed-1): %v", err)
	}
	if gone != nil {
		t.Error("failed-1 still present after DeleteFailedItemsByIDs")
	}
}

func TestDeleteFailedItemPreservesPersistentBlacklistAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "failed_prune_blacklist.db")

	db, err := Open(ctx, dbPath, 5*time.Second)
	if err != nil {
		t.Fatalf("Open first store: %v", err)
	}
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		_ = db.Close()
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	first := New(db)

	mustCreateItemForPrune(t, first, "failed-blacklisted", StateFailed)
	const blacklistKey = "nzb:persistent-after-failed-prune"
	if _, err := first.BlacklistImmediately(ctx, blacklistKey); err != nil {
		_ = db.Close()
		t.Fatalf("BlacklistImmediately: %v", err)
	}

	n, err := first.DeleteFailedItemsByIDs(ctx, []string{"failed-blacklisted"})
	if err != nil {
		_ = db.Close()
		t.Fatalf("DeleteFailedItemsByIDs: %v", err)
	}
	if n != 1 {
		_ = db.Close()
		t.Fatalf("deleted %d failed items, want 1", n)
	}
	item, err := first.GetItemByID(ctx, "failed-blacklisted")
	if err != nil {
		_ = db.Close()
		t.Fatalf("GetItemByID after prune: %v", err)
	}
	if item != nil {
		_ = db.Close()
		t.Fatal("failed item remained after prune deletion")
	}
	active, err := first.IsBlacklisted(ctx, blacklistKey)
	if err != nil {
		_ = db.Close()
		t.Fatalf("IsBlacklisted after prune: %v", err)
	}
	if !active {
		_ = db.Close()
		t.Fatal("failed-item deletion removed persistent blacklist state")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	reopenedDB, err := Open(ctx, dbPath, 5*time.Second)
	if err != nil {
		t.Fatalf("Open second store: %v", err)
	}
	t.Cleanup(func() { _ = reopenedDB.Close() })
	reopened := New(reopenedDB)
	active, err = reopened.IsBlacklisted(ctx, blacklistKey)
	if err != nil {
		t.Fatalf("IsBlacklisted after reopen: %v", err)
	}
	if !active {
		t.Fatal("blacklist did not persist after failed-item prune and database reopen")
	}
}

func TestDeleteFailedTorrentPreservesCanonicalAndAliasBlacklistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "failed_torrent_alias_prune.db")
	db, err := Open(ctx, dbPath, 5*time.Second)
	if err != nil {
		t.Fatalf("Open first store: %v", err)
	}
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		_ = db.Close()
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	first := New(db)
	mustCreateItemForPrune(t, first, "failed-torrent", StateFailed)
	const canonicalHash = "6666666666666666666666666666666666666666"
	const syntheticAlias = "7777777777777777777777777777777777777777"
	for _, identity := range []string{canonicalHash, syntheticAlias} {
		if _, err := first.BlacklistImmediately(ctx, identity); err != nil {
			_ = db.Close()
			t.Fatalf("BlacklistImmediately(%s): %v", identity, err)
		}
	}
	if n, err := first.DeleteFailedItemsByIDs(ctx, []string{"failed-torrent"}); err != nil || n != 1 {
		_ = db.Close()
		t.Fatalf("DeleteFailedItemsByIDs = %d, %v; want 1, nil", n, err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	reopenedDB, err := Open(ctx, dbPath, 5*time.Second)
	if err != nil {
		t.Fatalf("Open second store: %v", err)
	}
	t.Cleanup(func() { _ = reopenedDB.Close() })
	reopened := New(reopenedDB)
	for _, identity := range []string{canonicalHash, syntheticAlias} {
		active, err := reopened.IsBlacklisted(ctx, identity)
		if err != nil {
			t.Fatalf("IsBlacklisted(%s): %v", identity, err)
		}
		if !active {
			t.Fatalf("blacklist identity %s did not survive prune and reopen", identity)
		}
	}
}
