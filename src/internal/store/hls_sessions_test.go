package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/hlssession"
)

func newTestStoreForHLS(t *testing.T) *Store {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "hls-session-store.db"), 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	return New(db)
}

func TestStoreHLSSessionInsertGetTouch(t *testing.T) {
	st := newTestStoreForHLS(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)

	sess := hlssession.Session{
		SessionID: "hs000000000001", ItemID: "item-1", FileID: "file-1",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := st.InsertSession(ctx, sess); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	got, err := st.GetSession(ctx, sess.SessionID, now)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got == nil || got.ItemID != "item-1" || got.FileID != "file-1" {
		t.Fatalf("unexpected session: %+v", got)
	}

	// Past expiry, GetSession must report absent.
	if got, err := st.GetSession(ctx, sess.SessionID, now.Add(2*time.Hour)); err != nil || got != nil {
		t.Fatalf("expected nil session past expiry, got %+v err %v", got, err)
	}

	touched, err := st.TouchSession(ctx, sess.SessionID, now.Add(6*time.Hour))
	if err != nil || !touched {
		t.Fatalf("TouchSession: touched=%v err=%v", touched, err)
	}
	if got, err := st.GetSession(ctx, sess.SessionID, now.Add(2*time.Hour)); err != nil || got == nil {
		t.Fatalf("expected session alive after touch extended expiry, got %+v err %v", got, err)
	}

	touched, err = st.TouchSession(ctx, "hs-does-not-exist", now)
	if err != nil {
		t.Fatalf("TouchSession on missing id: %v", err)
	}
	if touched {
		t.Fatal("expected TouchSession on missing id to report false")
	}
}

func TestStoreHLSResourceInsertGetListCapacity(t *testing.T) {
	st := newTestStoreForHLS(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)

	sess := hlssession.Session{
		SessionID: "hs000000000002", ItemID: "item-1", FileID: "file-1",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := st.InsertSession(ctx, sess); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	bs, be := hlssession.NewWholeReference()
	res := hlssession.Resource{
		ResourceID: "hz000000000001", SessionID: sess.SessionID, Kind: hlssession.KindMaster,
		Reference: hlssession.Reference{Handler: "ia", Selector: "sel-1", ByteStart: bs, ByteEnd: be},
		CreatedAt: now,
	}
	if err := st.InsertResource(ctx, res, 2, now); err != nil {
		t.Fatalf("InsertResource: %v", err)
	}

	gotRes, gotSess, err := st.GetResource(ctx, res.ResourceID, now)
	if err != nil {
		t.Fatalf("GetResource: %v", err)
	}
	if gotRes == nil || gotSess == nil {
		t.Fatal("expected resource and session both present")
	}
	if gotRes.Reference.Handler != "ia" || gotRes.Reference.Selector != "sel-1" {
		t.Fatalf("unexpected reference round-trip: %+v", gotRes.Reference)
	}

	// A second insert within the cap succeeds; a third at cap=2 fails.
	res2 := res
	res2.ResourceID = "hz000000000002"
	if err := st.InsertResource(ctx, res2, 2, now); err != nil {
		t.Fatalf("InsertResource (2nd): %v", err)
	}
	res3 := res
	res3.ResourceID = "hz000000000003"
	err = st.InsertResource(ctx, res3, 2, now)
	if !errors.Is(err, hlssession.ErrSessionResourceCapacity) {
		t.Fatalf("expected ErrSessionResourceCapacity, got %v", err)
	}

	list, err := st.ListResources(ctx, sess.SessionID, now)
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 resources, got %d", len(list))
	}
	deleted, err := st.DeleteResources(ctx, sess.SessionID, []string{res.ResourceID, "hz000000000099"})
	if err != nil || deleted != 1 {
		t.Fatalf("DeleteResources deleted=%d err=%v", deleted, err)
	}
	if got, _, err := st.GetResource(ctx, res.ResourceID, now); err != nil || got != nil {
		t.Fatalf("deleted resource still present: %+v err=%v", got, err)
	}

	// Insert against an expired/absent session must fail distinctly.
	res4 := res
	res4.ResourceID = "hz000000000004"
	res4.SessionID = "hs-does-not-exist"
	if err := st.InsertResource(ctx, res4, 2, now); !errors.Is(err, hlssession.ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestStoreHLSPruneExpiredCascades(t *testing.T) {
	st := newTestStoreForHLS(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)

	live := hlssession.Session{SessionID: "hs-live-0000000", ItemID: "item-1", FileID: "file-1", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	dead := hlssession.Session{SessionID: "hs-dead-0000000", ItemID: "item-2", FileID: "file-2", CreatedAt: now, ExpiresAt: now.Add(-time.Minute)}
	if err := st.InsertSession(ctx, live); err != nil {
		t.Fatalf("InsertSession live: %v", err)
	}
	if err := st.InsertSession(ctx, dead); err != nil {
		t.Fatalf("InsertSession dead: %v", err)
	}

	n, err := st.PruneExpired(ctx, now)
	if err != nil {
		t.Fatalf("PruneExpired: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 pruned session, got %d", n)
	}
	if got, err := st.GetSession(ctx, live.SessionID, now); err != nil || got == nil {
		t.Fatalf("expected live session to survive prune, got %+v err %v", got, err)
	}
	if got, err := st.GetSession(ctx, dead.SessionID, now); err != nil || got != nil {
		t.Fatalf("expected dead session to be pruned, got %+v err %v", got, err)
	}
}

// TestStoreHLSSessionSurvivesRestart proves the HR4.1 "restart-resumable
// without full re-resolve" claim at the persistence layer: a resource
// inserted through one *Store instance/db connection is directly readable
// through a brand-new *Store instance opened over the same on-disk file —
// exactly what happens across a real DarkHarrbor process restart.
func TestStoreHLSSessionSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hls-session-restart.db")
	now := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)

	db1, err := Open(context.Background(), path, 5*time.Second)
	if err != nil {
		t.Fatalf("Open (1st): %v", err)
	}
	if err := RunMigrationsFS(db1, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	st1 := New(db1)

	sess := hlssession.Session{SessionID: "hs-restart-0001", ItemID: "item-1", FileID: "file-1", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := st1.InsertSession(context.Background(), sess); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	bs, be := hlssession.NewWholeReference()
	res := hlssession.Resource{
		ResourceID: "hz-restart-0001", SessionID: sess.SessionID, Kind: hlssession.KindSegment,
		Reference: hlssession.Reference{
			Handler: "ia", Ordinal: 3, ByteStart: bs, ByteEnd: be,
			MediaSequence: 57, HasMediaSequence: true, PartIndex: 2, HasPartIndex: true,
		},
		CreatedAt: now,
	}
	if err := st1.InsertResource(context.Background(), res, 100, now); err != nil {
		t.Fatalf("InsertResource: %v", err)
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("Close (1st): %v", err)
	}

	// Simulate a DarkHarrbor restart: open a fresh *sql.DB/*Store pair over
	// the same on-disk database file, exactly as cmd/darkharrbor/main.go
	// would after a process restart.
	db2, err := Open(context.Background(), path, 5*time.Second)
	if err != nil {
		t.Fatalf("Open (2nd, post-restart): %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	st2 := New(db2)

	gotRes, gotSess, err := st2.GetResource(context.Background(), res.ResourceID, now)
	if err != nil {
		t.Fatalf("GetResource post-restart: %v", err)
	}
	if gotRes == nil || gotSess == nil {
		t.Fatal("expected resource to resolve directly from the database after a restart, without any re-resolve")
	}
	if gotRes.Reference.Handler != "ia" || gotRes.Reference.Ordinal != 3 ||
		!gotRes.Reference.HasMediaSequence || gotRes.Reference.MediaSequence != 57 ||
		!gotRes.Reference.HasPartIndex || gotRes.Reference.PartIndex != 2 {
		t.Fatalf("unexpected post-restart reference: %+v", gotRes.Reference)
	}
	if gotSess.ItemID != "item-1" || gotSess.FileID != "file-1" {
		t.Fatalf("unexpected post-restart session: %+v", gotSess)
	}
}
