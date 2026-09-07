package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/securefile"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func TestCreateBackupSnapshot(t *testing.T) {
	root := t.TempDir()
	private := filepath.Join(root, "config")
	if err := securefile.PrepareDir(private); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(private, "darkharrbor.db")
	db, err := store.Open(context.Background(), databasePath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE durable (value TEXT NOT NULL); INSERT INTO durable VALUES ('present')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	secretsPath := filepath.Join(private, "secrets.sealed")
	if err := securefile.AtomicWrite(secretsPath, []byte("sealed-test-data")); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Database.Path = databasePath
	cfg.Database.BusyTimeout = time.Second
	cfg.Backup.Target = filepath.Join(root, "backup")
	cfg.Backup.SecretsPath = secretsPath
	cfg.Backup.Keep = 2
	name, err := createBackupSnapshot(context.Background(), cfg)
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	for _, filename := range []string{"darkharrbor.db", "secrets.sealed"} {
		info, err := os.Stat(filepath.Join(cfg.Backup.Target, name, filename))
		if err != nil {
			t.Fatalf("stat %s: %v", filename, err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Fatalf("%s mode = %o, want 600", filename, mode)
		}
	}
}

func TestCreateBackupSnapshotRequiresExistingDatabase(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{}
	cfg.Database.Path = filepath.Join(root, "missing.db")
	cfg.Database.BusyTimeout = time.Second
	cfg.Backup.Target = filepath.Join(root, "backup")
	cfg.Backup.SecretsPath = filepath.Join(root, "missing.sealed")
	cfg.Backup.Keep = 1
	if _, err := createBackupSnapshot(context.Background(), cfg); err == nil {
		t.Fatal("missing database unexpectedly produced a snapshot")
	}
	if _, err := os.Stat(cfg.Backup.Target); !os.IsNotExist(err) {
		t.Fatalf("backup target was mutated on failure: %v", err)
	}
}
