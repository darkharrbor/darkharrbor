package main

import (
	"context"
	"crypto/sha1" // #nosec G505 -- test fixture mirrors the BitTorrent v1 infohash definition; not a security primitive.
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
	"github.com/darkharrbor/darkharrbor/internal/torrentmeta"
)

// ---- TS-0.2 (T1) deterministic gates for the best-effort provider
// metainfo backfill. Every scenario here exercises only the store
// side-effects of backfillTorrentMeta; it must never panic or propagate a
// failure -- every negative case asserts "no row persisted", not an error
// return, because the function itself returns nothing.

func newTestStoreForBackfill(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "ts02-backfill.db"), 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	return store.New(db)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---- minimal test-only bencode encoder (mirrors internal/torrentmeta's own
// test-only encoder; kept self-contained rather than shared across packages).

func bStr(s string) string { return itoaLen(len(s)) + ":" + s }
func bInt(n int) string    { return "i" + itoaLen(n) + "e" }
func bDict(pairs ...string) string {
	out := "d"
	for _, p := range pairs {
		out += p
	}
	return out + "e"
}
func itoaLen(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

func sha1Hex(s string) string {
	sum := sha1.Sum([]byte(s)) // #nosec G401 -- reference computation mirroring the BitTorrent v1 infohash definition.
	return hex.EncodeToString(sum[:])
}

// validInfoDict is the raw bencoded "info" dict this fixture's torrent
// bytes carry. Its own sha1 IS the v1 infohash torrentmeta.Parse computes,
// so tests key items on sha1Hex(validInfoDict()) rather than a fabricated
// hash, exercising backfillTorrentMeta's real mismatch-guard path honestly.
func validInfoDict() string {
	return bDict(
		bStr("length")+bInt(1),
		bStr("name")+bStr("a.mkv"),
		bStr("piece length")+bInt(16384),
		bStr("pieces")+bStr("AAAAAAAAAAAAAAAAAAAA"),
	)
}

func validTorrentBytes() []byte {
	return []byte(bDict(bStr("info") + validInfoDict()))
}

// fakeMetainfoProvider implements provider.Provider + provider.MetainfoSource
// (the TorBox shape).
type fakeMetainfoProvider struct {
	fetchFn    func(ctx context.Context, remoteID string) ([]byte, error)
	fetchCalls int
}

func (f *fakeMetainfoProvider) Name() string                        { return "fake" }
func (f *fakeMetainfoProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }
func (f *fakeMetainfoProvider) CheckCached(ctx context.Context, item *store.Item) (*provider.CheckCachedResult, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeMetainfoProvider) Submit(ctx context.Context, item *store.Item, opts provider.SubmitOptions) (*provider.CreateTaskResponse, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeMetainfoProvider) Poll(ctx context.Context, item *store.Item) (*provider.TaskStatus, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeMetainfoProvider) RequestDownloadURL(ctx context.Context, item *store.Item, fileID string) (string, error) {
	return "", errors.New("not implemented")
}
func (f *fakeMetainfoProvider) Remove(ctx context.Context, item *store.Item) error { return nil }
func (f *fakeMetainfoProvider) FetchTorrentFile(ctx context.Context, remoteID string) ([]byte, error) {
	f.fetchCalls++
	return f.fetchFn(ctx, remoteID)
}

var _ provider.Provider = (*fakeMetainfoProvider)(nil)
var _ provider.MetainfoSource = (*fakeMetainfoProvider)(nil)

// noMetainfoProvider implements provider.Provider only -- the Real-Debrid
// shape: no MetainfoSource capability at all. backfillTorrentMeta's type
// assertion must fail closed against this, never panic.
type noMetainfoProvider struct{}

func (noMetainfoProvider) Name() string                        { return "no-metainfo" }
func (noMetainfoProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }
func (noMetainfoProvider) CheckCached(ctx context.Context, item *store.Item) (*provider.CheckCachedResult, error) {
	return nil, errors.New("not implemented")
}
func (noMetainfoProvider) Submit(ctx context.Context, item *store.Item, opts provider.SubmitOptions) (*provider.CreateTaskResponse, error) {
	return nil, errors.New("not implemented")
}
func (noMetainfoProvider) Poll(ctx context.Context, item *store.Item) (*provider.TaskStatus, error) {
	return nil, errors.New("not implemented")
}
func (noMetainfoProvider) RequestDownloadURL(ctx context.Context, item *store.Item, fileID string) (string, error) {
	return "", errors.New("not implemented")
}
func (noMetainfoProvider) Remove(ctx context.Context, item *store.Item) error { return nil }

var _ provider.Provider = noMetainfoProvider{}

func testTorrentItem(infoHash, remoteID string) *store.Item {
	return &store.Item{
		ID:         "item-1",
		SourceType: store.SourceTypeTorrent,
		InfoHash:   &infoHash,
		RemoteID:   &remoteID,
	}
}

func TestBackfillTorrentMetaProviderCapabilityAbsent(t *testing.T) {
	st := newTestStoreForBackfill(t)
	item := testTorrentItem("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", "123")

	backfillTorrentMeta(context.Background(), testLogger(), noMetainfoProvider{}, st, item)

	if _, found, err := st.GetTorrentMeta(context.Background(), *item.InfoHash); err != nil || found {
		t.Fatalf("expected no persisted row, found=%v err=%v", found, err)
	}
}

func TestBackfillTorrentMetaSkipsWhenAlreadyPersisted(t *testing.T) {
	st := newTestStoreForBackfill(t)
	hash := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	item := testTorrentItem(hash, "123")

	// Pre-seed a row under this exact hash directly, as if a prior
	// arr-uploaded .torrent (TS-0.1) or an earlier backfill already
	// populated it.
	seed := &torrentmeta.TorrentMeta{
		InfoHashV1:  hash,
		Name:        "already-here.mkv",
		PieceLength: 16384,
		MetaVersion: 1,
		Files:       []torrentmeta.FileEntry{{Path: "already-here.mkv", Size: 1}},
	}
	if err := st.UpsertTorrentMeta(context.Background(), seed); err != nil {
		t.Fatalf("UpsertTorrentMeta (seed): %v", err)
	}

	prov := &fakeMetainfoProvider{fetchFn: func(ctx context.Context, remoteID string) ([]byte, error) {
		t.Fatal("FetchTorrentFile must not be called when a row already exists")
		return nil, nil
	}}
	backfillTorrentMeta(context.Background(), testLogger(), prov, st, item)
	if prov.fetchCalls != 0 {
		t.Fatalf("fetchCalls = %d, want 0", prov.fetchCalls)
	}
}

func TestBackfillTorrentMetaSuccess(t *testing.T) {
	st := newTestStoreForBackfill(t)
	hash := sha1Hex(validInfoDict())
	item := testTorrentItem(hash, "123")

	prov := &fakeMetainfoProvider{fetchFn: func(ctx context.Context, remoteID string) ([]byte, error) {
		if remoteID != "123" {
			t.Fatalf("remoteID = %q, want 123", remoteID)
		}
		return validTorrentBytes(), nil
	}}

	backfillTorrentMeta(context.Background(), testLogger(), prov, st, item)

	if prov.fetchCalls != 1 {
		t.Fatalf("fetchCalls = %d, want 1", prov.fetchCalls)
	}
	meta, found, err := st.GetTorrentMeta(context.Background(), hash)
	if err != nil {
		t.Fatalf("GetTorrentMeta: %v", err)
	}
	if !found || meta == nil {
		t.Fatalf("expected a persisted torrent_meta row after successful backfill")
	}
	if meta.Name != "a.mkv" {
		t.Fatalf("Name = %q, want a.mkv", meta.Name)
	}
}

func TestBackfillTorrentMetaFetchErrorIsNonFatal(t *testing.T) {
	st := newTestStoreForBackfill(t)
	hash := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	item := testTorrentItem(hash, "123")

	prov := &fakeMetainfoProvider{fetchFn: func(ctx context.Context, remoteID string) ([]byte, error) {
		return nil, errors.New("export not available for cached downloads")
	}}

	backfillTorrentMeta(context.Background(), testLogger(), prov, st, item)

	if _, found, err := st.GetTorrentMeta(context.Background(), hash); err != nil || found {
		t.Fatalf("expected no persisted row on fetch error, found=%v err=%v", found, err)
	}
}

func TestBackfillTorrentMetaMalformedBytesIsNonFatal(t *testing.T) {
	st := newTestStoreForBackfill(t)
	hash := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	item := testTorrentItem(hash, "123")

	prov := &fakeMetainfoProvider{fetchFn: func(ctx context.Context, remoteID string) ([]byte, error) {
		return []byte("not bencode at all"), nil
	}}

	backfillTorrentMeta(context.Background(), testLogger(), prov, st, item)

	if _, found, err := st.GetTorrentMeta(context.Background(), hash); err != nil || found {
		t.Fatalf("expected no persisted row for malformed export, found=%v err=%v", found, err)
	}
}

func TestBackfillTorrentMetaInfoHashMismatchIsDiscarded(t *testing.T) {
	st := newTestStoreForBackfill(t)
	// Item tracks a hash that will NOT match what validTorrentBytes parses to.
	hash := "0000000000000000000000000000000000000000"
	item := testTorrentItem(hash, "123")

	prov := &fakeMetainfoProvider{fetchFn: func(ctx context.Context, remoteID string) ([]byte, error) {
		return validTorrentBytes(), nil
	}}

	backfillTorrentMeta(context.Background(), testLogger(), prov, st, item)

	if _, found, err := st.GetTorrentMeta(context.Background(), hash); err != nil || found {
		t.Fatalf("expected no persisted row on infohash mismatch, found=%v err=%v", found, err)
	}
}
