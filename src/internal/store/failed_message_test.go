package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestUpdateItemStateFailedRecordsMessage(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "msg.db"), 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st := New(db)
	now := time.Now().UTC()
	mk := func(id string, msg *string) *Item {
		it := &Item{ID: id, PublicID: "p" + id, SourceType: SourceTypeNZB, ClientKind: "sab", Category: "movies",
			State: StateAccepted, SubmissionKey: "k" + id, DisplayName: id, CreatedAt: now, UpdatedAt: now, ErrorMessage: msg}
		if err := st.CreateItem(ctx, it); err != nil {
			t.Fatalf("CreateItem: %v", err)
		}
		return it
	}
	read := func(id string) string {
		it, err := st.GetItemByID(ctx, id)
		if err != nil || it == nil || it.ErrorMessage == nil {
			return ""
		}
		return *it.ErrorMessage
	}
	waiting, explicit := "waiting: slots full", "explicit reason"
	a, b, c := mk("a", nil), mk("b", &waiting), mk("c", &explicit)
	for _, it := range []*Item{a, b, c} {
		if err := st.UpdateItemState(ctx, it, StateFailed, "unrecognised archive format"); err != nil {
			t.Fatalf("UpdateItemState: %v", err)
		}
	}
	if got := read("a"); got != "unrecognised archive format" {
		t.Fatalf("no prior message: got %q", got)
	}
	if got := read("b"); got != "unrecognised archive format" {
		t.Fatalf("waiting note: got %q", got)
	}
	if got := read("c"); got != "explicit reason" {
		t.Fatalf("explicit message overwritten: got %q", got)
	}
}
