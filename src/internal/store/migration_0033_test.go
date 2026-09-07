package store

import (
	"context"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

// TestMigration0033UpDownReUp is ID-03's DG-02-equivalent migration gate,
// mirroring the established migration_00NN_test.go convention (e.g.
// migration_0032_test.go).
func TestMigration0033UpDownReUp(t *testing.T) {
	db, err := Open(context.Background(), t.TempDir()+"/identity-audit-migration.db", time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	if _, err := db.Exec(`SELECT 1 FROM identity_audit LIMIT 1`); err != nil {
		t.Fatalf("identity_audit missing after up: %v", err)
	}
	if err := goose.DownTo(db, "migrations", 32); err != nil {
		t.Fatalf("DownTo 32: %v", err)
	}
	if _, err := db.Exec(`SELECT 1 FROM identity_audit LIMIT 1`); err == nil {
		t.Fatal("identity_audit still exists after down migration")
	}
	if err := goose.UpTo(db, "migrations", 33); err != nil {
		t.Fatalf("UpTo 33: %v", err)
	}
	if _, err := db.Exec(`SELECT 1 FROM identity_audit LIMIT 1`); err != nil {
		t.Fatalf("identity_audit missing after re-up: %v", err)
	}
}

// TestIdentityAudit_RestartPersistence is ID-03's restart-persistence
// proof: a recorded audit pass survives closing and reopening the same
// on-disk database (simulating a process restart), matching the
// established convention (e.g. TS-5.1's hash_availability restart test).
func TestIdentityAudit_RestartPersistence(t *testing.T) {
	path := t.TempDir() + "/identity-audit-restart.db"
	ctx := context.Background()

	db1, err := Open(ctx, path, time.Second)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	if err := RunMigrationsFS(db1, EmbeddedMigrations); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s1 := New(db1)
	now := time.Now().UTC()
	if err := s1.CreateItem(ctx, &Item{
		ID: "item-1", PublicID: "item-1", SourceType: SourceTypeTorrent, ClientKind: ClientKindQBit,
		Category: "movies", State: StateReady, SubmissionKey: "item-1", DisplayName: "item-1",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create item: %v", err)
	}
	if err := s1.UpsertIdentityAudit(ctx, "item-1", `[{"reason":"year_mismatch","detail":"x"}]`); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}

	db2, err := Open(ctx, path, time.Second)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	s2 := New(db2)
	a, err := s2.GetIdentityAudit(ctx, "item-1")
	if err != nil {
		t.Fatalf("get after restart: %v", err)
	}
	if a == nil {
		t.Fatal("expected persisted audit row after restart, got nil")
	}
	if a.VerdictsJSON != `[{"reason":"year_mismatch","detail":"x"}]` {
		t.Fatalf("unexpected verdicts json after restart: %q", a.VerdictsJSON)
	}
}
