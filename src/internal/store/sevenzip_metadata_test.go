package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestSevenZipManifestMetadataPersistsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "sevenzip.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	raw := `{"volumes":[{"number":1}],"entries":[{"entry_idx":0}]}`
	at := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	item := &Item{
		ID: "sevenzip", PublicID: "public-sevenzip", SourceType: SourceTypeNZB, ClientKind: ClientKindSAB,
		Category: "tv", State: StateReady, SubmissionKey: "sevenzip-submission", DisplayName: "episode",
		CreatedAt: at, UpdatedAt: at, Metadata: SubmissionMetadata{SevenZipManifest: &raw},
	}
	if err := New(db).CreateItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	got, err := New(db).GetItemByID(ctx, item.ID)
	if err != nil || got.Metadata.SevenZipManifest == nil || *got.Metadata.SevenZipManifest != raw {
		t.Fatalf("restart read = %#v, %v", got, err)
	}
}
