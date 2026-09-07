package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

const previousSupportedSchema int64 = 14

func TestPreviousSupportedReleaseMigratesWithoutDataLoss(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "release-migration.db")
	db, err := Open(ctx, path, time.Second)
	if err != nil {
		t.Fatalf("Open previous release database: %v", err)
	}

	gooseMu.Lock()
	goose.SetBaseFS(EmbeddedMigrations)
	if err = goose.SetDialect("sqlite3"); err == nil {
		err = goose.UpTo(db, "migrations", previousSupportedSchema)
	}
	goose.SetBaseFS(nil)
	gooseMu.Unlock()
	if err != nil {
		_ = db.Close()
		t.Fatalf("create schema %d database: %v", previousSupportedSchema, err)
	}

	const created = "2026-01-02T03:04:05Z"
	if _, err := db.ExecContext(ctx, `
		INSERT INTO items (
			id, public_id, source_type, client_kind, category, state,
			submission_key, display_name, cached, metadata_json,
			created_at, updated_at, total_size, provider
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"release-item", "release-public", "torrent", "qbit", "tv", "ready",
		"release-key", "Release Item", 1, "{}", created, created, 734003200, "torbox"); err != nil {
		_ = db.Close()
		t.Fatalf("insert previous release item: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO strm_blobs (item_id, file_index, rel_path, sha256, url, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		"release-item", 0, "release/video.strm", "digest", "http://darkharrbor:8381/dav/release-public/0", created); err != nil {
		_ = db.Close()
		t.Fatalf("insert previous release strm authority: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO app_config (key, value, updated_at) VALUES (?, ?, ?)`,
		"wizard.complete", "true", created); err != nil {
		_ = db.Close()
		t.Fatalf("insert previous release config: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO arr_instances (name, url, app_type, api_key_ref, discovered_at)
		VALUES (?, ?, ?, ?, ?)`,
		"sonarr-classic", "http://sonarr-classic:8989", "sonarr", "HARRBOR_ARR_SONARR_CLASSIC_APIKEY", created); err != nil {
		_ = db.Close()
		t.Fatalf("insert previous release Arr instance: %v", err)
	}

	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		_ = db.Close()
		t.Fatalf("migrate previous release: %v", err)
	}
	assertCurrentReleaseData(t, ctx, db)
	if err := db.Close(); err != nil {
		t.Fatalf("close migrated database: %v", err)
	}

	reopened, err := Open(ctx, path, time.Second)
	if err != nil {
		t.Fatalf("reopen migrated database: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := RunMigrationsFS(reopened, EmbeddedMigrations); err != nil {
		t.Fatalf("repeat migrations after restart: %v", err)
	}
	assertCurrentReleaseData(t, ctx, reopened)
}

func assertCurrentReleaseData(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	version, err := goose.GetDBVersion(db)
	if err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	// Bumped to 45 by RX-9.4 (RD-34 D8), which adds
	// 0045_arr_instance_destinations.sql so the reactive router can source
	// wizard-discovered instances instead of environment-declared arrs.
	// Previously bumped to 44 by RX-9.6 (RD-32), which adds
	// 0044_playback_coverage_declared_size.sql so coverage can be discarded
	// when an identity's declared release size changes.
	if version != 45 {
		t.Fatalf("schema version = %d, want 45", version)
	}

	item, err := New(db).GetItemByID(ctx, "release-item")
	if err != nil {
		t.Fatalf("read migrated item: %v", err)
	}
	if item == nil || item.PublicID != "release-public" || !item.Cached || item.TotalSize != 734003200 {
		t.Fatalf("migrated item was not preserved")
	}

	var blobCount, configCount int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM strm_blobs
		WHERE item_id=? AND rel_path=?`, "release-item", "release/video.strm").Scan(&blobCount); err != nil {
		t.Fatalf("read migrated strm authority: %v", err)
	}
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM app_config
		WHERE key=? AND value=?`, "wizard.complete", "true").Scan(&configCount); err != nil {
		t.Fatalf("read migrated app config: %v", err)
	}
	instances, err := New(db).ListArrInstances(ctx)
	if err != nil {
		t.Fatalf("read migrated Arr instances: %v", err)
	}
	if blobCount != 1 || configCount != 1 || len(instances) != 1 || instances[0].Name != "sonarr-classic" {
		t.Fatalf("migrated durable state was not preserved")
	}
}
