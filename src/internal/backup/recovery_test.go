package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/securefile"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func TestRestorePreservesStablePointersAfterConfigLoss(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(root, "config")
	if err := securefile.PrepareDir(configDir); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(configDir, databaseName)
	db, err := store.Open(ctx, databasePath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	st := store.New(db)
	now := time.Now().UTC()
	const token = "0123456789abcdef0123456789abcdef"
	urls := map[string]string{
		"pending":  "http://darkharrbor:8381/stream/pending/0?tok=" + token,
		"consumed": "http://darkharrbor:8381/stream/consumed/0?tok=" + token,
	}
	for _, id := range []string{"pending", "consumed"} {
		item := &store.Item{
			ID: id, PublicID: id, SourceType: store.SourceTypeNZB,
			ClientKind: store.ClientKindSAB, Category: "movies", State: store.StateReady,
			SubmissionKey: id, DisplayName: id, CreatedAt: now, UpdatedAt: now,
			DownloadConsumed: id == "consumed",
		}
		if err := st.CreateItem(ctx, item); err != nil {
			t.Fatal(err)
		}
		if err := st.UpsertStrmBlob(ctx, store.StrmBlob{
			ItemID: id, FileIndex: 0, RelPath: filepath.Join("movies", id+".strm"), URL: urls[id],
		}); err != nil {
			t.Fatal(err)
		}
	}
	secretsPath := filepath.Join(configDir, secretsName)
	const sealed = "sealed-recovery-test"
	if err := securefile.AtomicWrite(secretsPath, []byte(sealed)); err != nil {
		t.Fatal(err)
	}
	backupRoot := filepath.Join(root, "backups")
	svc := New(db, Config{Target: backupRoot, SecretsPath: secretsPath, Interval: time.Hour, Keep: 1}, nil)
	if _, err := svc.Create(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(configDir); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(ctx, backupRoot, configDir); err != nil {
		t.Fatal(err)
	}
	restoredSecret, err := os.ReadFile(filepath.Join(configDir, secretsName))
	if err != nil || string(restoredSecret) != sealed {
		t.Fatalf("restored sealed secrets = %q, err=%v", restoredSecret, err)
	}
	restoredDB, err := store.Open(ctx, filepath.Join(configDir, databaseName), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer restoredDB.Close()
	restored := store.New(restoredDB)
	for id, wantURL := range urls {
		blobs, err := restored.GetStrmBlobs(ctx, id)
		if err != nil || len(blobs) != 1 || blobs[0].URL != wantURL {
			t.Fatalf("restored %s blobs=%+v err=%v", id, blobs, err)
		}
	}

	dataRoot := filepath.Join(root, "data")
	result, err := restored.ReconcileAll(ctx, dataRoot)
	if err != nil || result.Restored != 1 || result.Errors != 0 {
		t.Fatalf("reconcile result=%+v err=%v", result, err)
	}
	pending, err := os.ReadFile(filepath.Join(dataRoot, "movies", "pending.strm"))
	if err != nil || string(pending) != urls["pending"] {
		t.Fatalf("rematerialized pending pointer=%q err=%v", pending, err)
	}
	if _, err := os.Stat(filepath.Join(dataRoot, "movies", "consumed.strm")); !os.IsNotExist(err) {
		t.Fatalf("consumed download pointer was fabricated: %v", err)
	}

	library := filepath.Join(root, "library")
	if err := os.Mkdir(library, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(library, "consumed.strm"), []byte(urls["consumed"]), 0o600); err != nil {
		t.Fatal(err)
	}
	audit, err := restored.ReAdoptionScan(ctx, library, 10)
	if err != nil || audit.Verified != 1 || audit.Orphans != 0 || audit.Malformed != 0 {
		t.Fatalf("library audit=%+v err=%v", audit, err)
	}
}
