package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	archiveparser "github.com/darkharrbor/darkharrbor/internal/archive"
)

func TestPAR2ProofIndexPersistsInItemMetadata(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "par2-proof.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	st := New(db)
	at := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	item := &Item{
		ID: "item", PublicID: "public", SourceType: SourceTypeNZB, ClientKind: ClientKindSAB,
		Category: "tv", State: StateResolving, SubmissionKey: "submission", DisplayName: "episode",
		CreatedAt: at, UpdatedAt: at,
	}
	if err := st.CreateItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	item.Metadata.NNTPPAR2ProofIndex = &archiveparser.PAR2ProofIndex{
		SliceSize: 4,
		Files: []archiveparser.PAR2ProofFile{{
			NZBFileIndex: 2,
			Length:       4,
			BlockMD5:     []byte("0123456789abcdef"),
		}},
	}
	if err := st.UpdateItemState(ctx, item, StateReady, ""); err != nil {
		t.Fatal(err)
	}
	got, err := New(db).GetItemByID(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	index := got.Metadata.NNTPPAR2ProofIndex
	if index == nil || index.SliceSize != 4 || len(index.Files) != 1 ||
		index.Files[0].NZBFileIndex != 2 || string(index.Files[0].BlockMD5) != "0123456789abcdef" {
		t.Fatalf("persisted PAR2 proof index = %#v", index)
	}
}
