package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestUpsertArrInstance(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "wizard.db"), 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}

	st := New(db)
	fixed := time.Date(2026, 7, 4, 19, 15, 0, 0, time.UTC)
	st.SetClock(func() time.Time { return fixed })

	inst := ArrInstance{
		Name:      "sonarr-modern",
		URL:       "http://sonarr-modern:8989",
		AppType:   "sonarr",
		APIKeyRef: "HARRBOR_ARR_SONARR_MODERN_APIKEY",
	}
	if err := st.UpsertArrInstance(ctx, inst); err != nil {
		t.Fatalf("UpsertArrInstance: %v", err)
	}

	inst.URL = "http://sonarr-modern:9898"
	if err := st.UpsertArrInstance(ctx, inst); err != nil {
		t.Fatalf("UpsertArrInstance update: %v", err)
	}

	got, err := st.ListArrInstances(ctx)
	if err != nil {
		t.Fatalf("ListArrInstances: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListArrInstances len=%d, want 1", len(got))
	}
	if got[0].URL != "http://sonarr-modern:9898" {
		t.Fatalf("URL=%q, want updated value", got[0].URL)
	}
	if got[0].Discovered != "2026-07-04T19:15:00Z" {
		t.Fatalf("Discovered=%q, want fixed timestamp", got[0].Discovered)
	}
}
