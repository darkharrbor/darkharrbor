package store

import (
	"context"
	"testing"
	"time"
)

func newTestStoreSF05(t *testing.T) *Store {
	t.Helper()
	db, err := Open(context.Background(), t.TempDir()+"/test.db", time.Second)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	return New(db)
}

func TestFileProbeFailure_RoundTripAndHits(t *testing.T) {
	s := newTestStoreSF05(t)
	ctx := context.Background()
	if err := s.SetFileProbeFailure(ctx, "it", "dav:f.mkv", "k", 1, "out", time.Hour); err != nil {
		t.Fatalf("set: %v", err)
	}
	for want := int64(1); want <= 3; want++ {
		stdout, code, hits, found, err := s.GetFileProbeFailure(ctx, "it", "dav:f.mkv", "k")
		if err != nil || !found {
			t.Fatalf("get #%d: found=%v err=%v", want, found, err)
		}
		if stdout != "out" || code != 1 || hits != want {
			t.Fatalf("get #%d: stdout=%q code=%d hits=%d", want, stdout, code, hits)
		}
	}
	// Different key misses.
	if _, _, _, found, _ := s.GetFileProbeFailure(ctx, "it", "dav:f.mkv", "other"); found {
		t.Fatalf("different args key must miss")
	}
}

func TestFileProbeFailure_ExpiryDeletes(t *testing.T) {
	s := newTestStoreSF05(t)
	ctx := context.Background()
	if err := s.SetFileProbeFailure(ctx, "it", "0", "k", 1, "x", time.Millisecond); err != nil {
		t.Fatalf("set: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, _, _, found, err := s.GetFileProbeFailure(ctx, "it", "0", "k"); err != nil || found {
		t.Fatalf("expired entry must be not-found (found=%v err=%v)", found, err)
	}
	// Row physically deleted.
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM item_file_probe_failures").Scan(&n); err != nil || n != 0 {
		t.Fatalf("expired row not deleted: n=%d err=%v", n, err)
	}
}

func TestFileProbeFailure_UpsertRefreshes(t *testing.T) {
	s := newTestStoreSF05(t)
	ctx := context.Background()
	_ = s.SetFileProbeFailure(ctx, "it", "0", "k", 1, "a", time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	if err := s.SetFileProbeFailure(ctx, "it", "0", "k", 2, "b", time.Hour); err != nil {
		t.Fatalf("re-set: %v", err)
	}
	stdout, code, _, found, err := s.GetFileProbeFailure(ctx, "it", "0", "k")
	if err != nil || !found || stdout != "b" || code != 2 {
		t.Fatalf("upsert not refreshed: found=%v stdout=%q code=%d err=%v", found, stdout, code, err)
	}
}
