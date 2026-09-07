package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenReadOnlyReadsButCannotMutateOrCreate(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(ctx, path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE durable (value TEXT); INSERT INTO durable VALUES ('kept')"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	readOnly, err := OpenReadOnly(ctx, path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	var value string
	if err := readOnly.QueryRow("SELECT value FROM durable").Scan(&value); err != nil || value != "kept" {
		t.Fatalf("read value=%q err=%v", value, err)
	}
	if _, err := readOnly.Exec("INSERT INTO durable VALUES ('changed')"); err == nil {
		t.Fatal("read-only database accepted a write")
	}

	missing := filepath.Join(t.TempDir(), "absent", "state.db")
	if db, err := OpenReadOnly(ctx, missing, time.Second); err == nil {
		_ = db.Close()
		t.Fatal("read-only open created a missing database")
	}
	if _, err := os.Stat(filepath.Dir(missing)); !os.IsNotExist(err) {
		t.Fatalf("read-only open created a parent directory: %v", err)
	}
}
