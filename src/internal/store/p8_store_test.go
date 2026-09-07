package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestUpdateLastPlayedAtIsTargetedAndStateGuarded(t *testing.T) {
	s := newTestStoreForCleanup(t)
	ctx := context.Background()
	remoteID := "remote-1"
	item := mustCreateItem(t, s, "last-played", StateReady, &remoteID, nil)
	at := time.Date(2026, 7, 9, 20, 0, 0, 0, time.UTC)

	if err := s.UpdateLastPlayedAt(ctx, item.ID, StateReady, at); err != nil {
		t.Fatalf("UpdateLastPlayedAt: %v", err)
	}
	got, err := s.GetItemByID(ctx, item.ID)
	if err != nil {
		t.Fatalf("GetItemByID: %v", err)
	}
	if got.LastPlayedAt == nil || !got.LastPlayedAt.Equal(at) {
		t.Fatalf("LastPlayedAt = %v, want %v", got.LastPlayedAt, at)
	}
	if !got.UpdatedAt.Equal(at) {
		t.Fatalf("UpdatedAt = %v, want %v", got.UpdatedAt, at)
	}
	if got.State != StateReady || got.RemoteID == nil || *got.RemoteID != remoteID || got.DisplayName != item.DisplayName {
		t.Fatalf("targeted update changed other fields: %#v", got)
	}
	if err := s.UpdateLastPlayedAt(ctx, item.ID, StateResolving, at.Add(time.Minute)); err == nil {
		t.Fatal("UpdateLastPlayedAt with stale state succeeded")
	}
}

func TestRekeyFileProbesChecksDeleteAndIsIdempotent(t *testing.T) {
	s := newTestStoreForCleanup(t)
	ctx := context.Background()

	createProbe := func(itemID, realFileID string) {
		t.Helper()
		item := mustCreateItem(t, s, itemID, StateReady, nil, nil)
		fileList := `[{"file_id":"` + realFileID + `"}]`
		item.FileList = &fileList
		item.UpdatedAt = time.Now().UTC()
		if err := s.UpdateItem(ctx, item); err != nil {
			t.Fatalf("UpdateItem(%s): %v", itemID, err)
		}
		if err := s.SetFileProbe(ctx, itemID, "0", "args", `{"streams":[]}`); err != nil {
			t.Fatalf("SetFileProbe(%s): %v", itemID, err)
		}
	}

	createProbe("rekey-ok", "provider-7")
	n, err := s.RekeyFileProbesByFileID(ctx)
	if err != nil || n != 1 {
		t.Fatalf("first rekey: n=%d err=%v", n, err)
	}
	if _, found, err := s.GetFileProbe(ctx, "rekey-ok", "0", "args"); err != nil || found {
		t.Fatalf("old probe key: found=%v err=%v", found, err)
	}
	if got, found, err := s.GetFileProbe(ctx, "rekey-ok", "provider-7", "args"); err != nil || !found || got != `{"streams":[]}` {
		t.Fatalf("new probe key: got=%q found=%v err=%v", got, found, err)
	}
	if n, err := s.RekeyFileProbesByFileID(ctx); err != nil || n != 0 {
		t.Fatalf("idempotent rekey: n=%d err=%v", n, err)
	}

	createProbe("rekey-fail", "provider-8")
	if _, err := s.db.ExecContext(ctx, `
		CREATE TRIGGER fail_probe_rekey_delete
		BEFORE DELETE ON item_file_probes
		WHEN OLD.item_id = 'rekey-fail'
		BEGIN SELECT RAISE(FAIL, 'delete blocked'); END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}
	if _, err := s.RekeyFileProbesByFileID(ctx); err == nil || !strings.Contains(err.Error(), "delete old key") {
		t.Fatalf("delete failure not surfaced: %v", err)
	}
	if _, found, err := s.GetFileProbe(ctx, "rekey-fail", "0", "args"); err != nil || !found {
		t.Fatalf("old key after failed delete: found=%v err=%v", found, err)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TRIGGER fail_probe_rekey_delete`); err != nil {
		t.Fatalf("drop failure trigger: %v", err)
	}
	if n, err := s.RekeyFileProbesByFileID(ctx); err != nil || n != 1 {
		t.Fatalf("retry rekey: n=%d err=%v", n, err)
	}
}

func TestDeleteRemovedItemsDeletesSegmentOffsets(t *testing.T) {
	s := newTestStoreForCleanup(t)
	ctx := context.Background()
	item := mustCreateItem(t, s, "removed-with-offset", StateRemoved, nil, nil)
	if _, err := s.execWrite(ctx, `INSERT INTO segment_offsets
		(item_id, file_index, seg_index, decoded_bytes) VALUES (?, ?, ?, ?)`,
		item.ID, 0, 0, 1234); err != nil {
		t.Fatalf("record segment: %v", err)
	}
	if n, err := s.DeleteRemovedItemsByIDs(ctx, []string{item.ID}); err != nil || n != 1 {
		t.Fatalf("DeleteRemovedItemsByIDs: n=%d err=%v", n, err)
	}
	var offsets int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM segment_offsets WHERE item_id=?`, item.ID).Scan(&offsets); err != nil {
		t.Fatalf("count segment offsets: %v", err)
	}
	if offsets != 0 {
		t.Fatalf("segment offset rows after item delete = %d, want 0", offsets)
	}
}
