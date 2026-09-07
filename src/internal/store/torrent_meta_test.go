package store

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/torrentmeta"
)

func openTorrentMetaTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := Open(context.Background(), t.TempDir()+"/torrent-meta.db", time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	return New(db)
}

func TestTorrentMetaRoundTrip(t *testing.T) {
	s := openTorrentMetaTestStore(t)
	ctx := context.Background()

	meta := &torrentmeta.TorrentMeta{
		Name:          "Show.S01.Pack",
		InfoHashV1:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		InfoHashV2:    "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		MetaVersion:   2,
		PieceLength:   16384,
		PieceHashesV1: []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14},
		Files: []torrentmeta.FileEntry{
			{Path: "Show.S01.Pack/S01E01.mkv", Size: 1000, StartOffset: 0, EndOffset: 1000},
			{Path: "Show.S01.Pack/S01E02.mkv", Size: 2000, StartOffset: 1000, EndOffset: 3000},
		},
		V2MerkleRoots: map[string]string{
			"Show.S01.Pack/S01E01.mkv": "root1",
			"Show.S01.Pack/S01E02.mkv": "root2",
		},
	}

	if err := s.UpsertTorrentMeta(ctx, meta); err != nil {
		t.Fatalf("UpsertTorrentMeta: %v", err)
	}

	got, ok, err := s.GetTorrentMeta(ctx, meta.InfoHashV1)
	if err != nil {
		t.Fatalf("GetTorrentMeta: %v", err)
	}
	if !ok {
		t.Fatal("GetTorrentMeta: not found")
	}
	if got.Name != meta.Name {
		t.Fatalf("Name = %q, want %q", got.Name, meta.Name)
	}
	if got.InfoHashV2 != meta.InfoHashV2 {
		t.Fatalf("InfoHashV2 = %q, want %q", got.InfoHashV2, meta.InfoHashV2)
	}
	if got.MetaVersion != meta.MetaVersion {
		t.Fatalf("MetaVersion = %d, want %d", got.MetaVersion, meta.MetaVersion)
	}
	if got.PieceLength != meta.PieceLength {
		t.Fatalf("PieceLength = %d, want %d", got.PieceLength, meta.PieceLength)
	}
	if len(got.Files) != len(meta.Files) {
		t.Fatalf("Files len = %d, want %d", len(got.Files), len(meta.Files))
	}
	for i := range meta.Files {
		if got.Files[i] != meta.Files[i] {
			t.Fatalf("Files[%d] = %+v, want %+v", i, got.Files[i], meta.Files[i])
		}
	}
	for k, v := range meta.V2MerkleRoots {
		if got.V2MerkleRoots[k] != v {
			t.Fatalf("V2MerkleRoots[%q] = %q, want %q", k, got.V2MerkleRoots[k], v)
		}
	}
	if len(got.PieceHashesV1) != len(meta.PieceHashesV1) {
		t.Fatalf("PieceHashesV1 len = %d, want %d", len(got.PieceHashesV1), len(meta.PieceHashesV1))
	}

	// Restart persistence (DG-02, LG-12): a fresh Store handle over the same
	// underlying DB file must see the same row.
	s2 := New(s.DB())
	got2, ok2, err := s2.GetTorrentMeta(ctx, meta.InfoHashV1)
	if err != nil || !ok2 {
		t.Fatalf("GetTorrentMeta after reopen: ok=%v err=%v", ok2, err)
	}
	if got2.Name != meta.Name {
		t.Fatalf("after reopen: Name = %q, want %q", got2.Name, meta.Name)
	}
}

func TestTorrentMetaUpsertUpdatesExistingRow(t *testing.T) {
	s := openTorrentMetaTestStore(t)
	ctx := context.Background()

	first := &torrentmeta.TorrentMeta{
		Name:        "Old.Name",
		InfoHashV1:  "cccccccccccccccccccccccccccccccccccccccc",
		MetaVersion: 1,
		PieceLength: 16384,
		Files:       []torrentmeta.FileEntry{{Path: "Old.Name", Size: 1, StartOffset: 0, EndOffset: 1}},
	}
	if err := s.UpsertTorrentMeta(ctx, first); err != nil {
		t.Fatalf("UpsertTorrentMeta (first): %v", err)
	}

	second := &torrentmeta.TorrentMeta{
		Name:        "New.Name",
		InfoHashV1:  first.InfoHashV1,
		MetaVersion: 1,
		PieceLength: 32768,
		Files:       []torrentmeta.FileEntry{{Path: "New.Name", Size: 2, StartOffset: 0, EndOffset: 2}},
	}
	if err := s.UpsertTorrentMeta(ctx, second); err != nil {
		t.Fatalf("UpsertTorrentMeta (second): %v", err)
	}

	got, ok, err := s.GetTorrentMeta(ctx, first.InfoHashV1)
	if err != nil || !ok {
		t.Fatalf("GetTorrentMeta: ok=%v err=%v", ok, err)
	}
	if got.Name != "New.Name" || got.PieceLength != 32768 {
		t.Fatalf("row was not updated: %+v", got)
	}
}

func TestTorrentMetaGetMissReturnsNotFoundNotError(t *testing.T) {
	s := openTorrentMetaTestStore(t)
	ctx := context.Background()

	got, ok, err := s.GetTorrentMeta(ctx, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	if err != nil {
		t.Fatalf("GetTorrentMeta: unexpected error %v", err)
	}
	if ok || got != nil {
		t.Fatalf("expected miss, got ok=%v meta=%+v", ok, got)
	}
}

func TestUpsertTorrentMetaSkipsV2OnlyTorrent(t *testing.T) {
	// A pure-v2 torrent (no v1 infohash) is not persisted here -- absence
	// is non-fatal per T1, not an error.
	s := openTorrentMetaTestStore(t)
	ctx := context.Background()

	meta := &torrentmeta.TorrentMeta{
		Name:        "PureV2Only",
		InfoHashV2:  "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		MetaVersion: 2,
		PieceLength: 16384,
		Files:       []torrentmeta.FileEntry{{Path: "PureV2Only", Size: 1}},
	}
	if err := s.UpsertTorrentMeta(ctx, meta); err != nil {
		t.Fatalf("UpsertTorrentMeta: %v", err)
	}
	var count int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM torrent_meta`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no rows for a v2-only torrent, got %d", count)
	}
}

// TestTorrentMetaPieceLayersRoundTrip covers TS-3.4 (T12): PieceLayers must
// survive persistence and a restart (fresh Store handle over the same DB)
// exactly like every other TorrentMeta field, and a torrent with no v2
// piece-level material must round-trip with a nil map, not an error.
func TestTorrentMetaPieceLayersRoundTrip(t *testing.T) {
	s := openTorrentMetaTestStore(t)
	ctx := context.Background()

	layer0 := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20}
	root := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	meta := &torrentmeta.TorrentMeta{
		Name:        "PieceLayerTest",
		InfoHashV1:  "ffffffffffffffffffffffffffffffffffffffff",
		MetaVersion: 2,
		PieceLength: 32768,
		Files:       []torrentmeta.FileEntry{{Path: "PieceLayerTest/movie.mkv", Size: 32768, StartOffset: 0, EndOffset: 32768}},
		V2MerkleRoots: map[string]string{
			"PieceLayerTest/movie.mkv": root,
		},
		PieceLayers: map[string][]byte{
			root: layer0,
		},
	}

	if err := s.UpsertTorrentMeta(ctx, meta); err != nil {
		t.Fatalf("UpsertTorrentMeta: %v", err)
	}

	got, ok, err := s.GetTorrentMeta(ctx, meta.InfoHashV1)
	if err != nil || !ok {
		t.Fatalf("GetTorrentMeta: ok=%v err=%v", ok, err)
	}
	gotLayer, present := got.PieceLayers[root]
	if !present {
		t.Fatalf("PieceLayers[%s] missing after round trip", root)
	}
	if len(gotLayer) != len(layer0) {
		t.Fatalf("PieceLayers[%s] len = %d, want %d", root, len(gotLayer), len(layer0))
	}
	for i := range layer0 {
		if gotLayer[i] != layer0[i] {
			t.Fatalf("PieceLayers[%s][%d] = %#x, want %#x", root, i, gotLayer[i], layer0[i])
		}
	}

	// Restart persistence (DG-02): a fresh Store handle over the same
	// underlying DB file must see the identical piece layers.
	s2 := New(s.DB())
	got2, ok2, err := s2.GetTorrentMeta(ctx, meta.InfoHashV1)
	if err != nil || !ok2 {
		t.Fatalf("GetTorrentMeta after reopen: ok=%v err=%v", ok2, err)
	}
	if len(got2.PieceLayers[root]) != len(layer0) {
		t.Fatalf("after reopen: PieceLayers[%s] len = %d, want %d", root, len(got2.PieceLayers[root]), len(layer0))
	}
}

// TestTorrentMetaNoPieceLayersRoundTripsNil covers the common case (v1-only
// torrent, or a v2 torrent with no file over one piece long): the column is
// NULL, and PieceLayers must come back nil, never an error or an empty
// non-nil map that a caller might mistake for "verified empty".
func TestTorrentMetaNoPieceLayersRoundTripsNil(t *testing.T) {
	s := openTorrentMetaTestStore(t)
	ctx := context.Background()

	meta := &torrentmeta.TorrentMeta{
		Name:          "NoLayers",
		InfoHashV1:    "1111111111111111111111111111111111111a",
		MetaVersion:   1,
		PieceLength:   16384,
		PieceHashesV1: bytes.Repeat([]byte{0xAA}, 20),
		Files:         []torrentmeta.FileEntry{{Path: "NoLayers", Size: 16384, StartOffset: 0, EndOffset: 16384}},
	}
	if err := s.UpsertTorrentMeta(ctx, meta); err != nil {
		t.Fatalf("UpsertTorrentMeta: %v", err)
	}
	got, ok, err := s.GetTorrentMeta(ctx, meta.InfoHashV1)
	if err != nil || !ok {
		t.Fatalf("GetTorrentMeta: ok=%v err=%v", ok, err)
	}
	if got.PieceLayers != nil {
		t.Fatalf("PieceLayers = %v, want nil", got.PieceLayers)
	}
}
