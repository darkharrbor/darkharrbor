package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestGrabProviderIdentityRestartAndExpiry(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "provider-identity.db")
	db, err := Open(ctx, path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	first := New(db)
	first.SetClock(func() time.Time { return now })
	identity := ProviderIdentity{
		Kind: "series", Title: "Show", IDs: ProviderIDs{TVDB: "123"},
		Episodes: []EpisodeProviderIDs{{Season: 1, Episode: 1, TVDB: "456"}},
	}
	if err := first.UpsertGrabProviderIdentity(ctx, "torrent:abc", identity); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(ctx, path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	second := New(db)
	second.SetClock(func() time.Time { return now.Add(time.Hour) })
	got, ok, err := second.GetGrabProviderIdentity(ctx, "torrent:abc")
	if err != nil || !ok || got.IDs.TVDB != "123" || len(got.Episodes) != 1 {
		t.Fatalf("post-restart Get = (%+v, %v, %v)", got, ok, err)
	}
	second.SetClock(func() time.Time { return now.Add(grabProviderIdentityTTL + time.Second) })
	if _, ok, err := second.GetGrabProviderIdentity(ctx, "torrent:abc"); err != nil || ok {
		t.Fatalf("expired Get = (ok=%v, err=%v)", ok, err)
	}
}

func TestGrabProviderIdentityRejectsMalformed(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "malformed.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	st := New(db)
	err = st.UpsertGrabProviderIdentity(ctx, "torrent:abc", ProviderIdentity{
		Kind: "series", IDs: ProviderIDs{TVDB: "https://example.invalid"},
	})
	if err == nil {
		t.Fatal("malformed provider ID accepted")
	}
}
