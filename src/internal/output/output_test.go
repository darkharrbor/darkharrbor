package output

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/sidecar"
	"github.com/darkharrbor/darkharrbor/internal/store"
	"github.com/darkharrbor/darkharrbor/internal/torrentmeta"
)

func newTestWriter(t *testing.T) (*StrmWriter, string) {
	t.Helper()
	root := t.TempDir()
	strmRoot := filepath.Join(root, "strm")
	stagingRoot := filepath.Join(root, "staging")
	return NewStrmWriter(strmRoot, stagingRoot), strmRoot
}

func seasonPackFiles() []provider.CachedFile {
	return []provider.CachedFile{
		{FileID: "1", Name: "S01E01.mkv", RelativePath: "S01E01.mkv", Size: 100},
		{FileID: "2", Name: "S01E02.mkv", RelativePath: "S01E02.mkv", Size: 100},
	}
}

func testItem() *store.Item {
	return &store.Item{
		ID:          "item123",
		Category:    "tv-modern",
		DisplayName: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
	}
}

// A season pack (>1 video file) publishes via a directory rename into a
// per-item named subfolder that must not pre-exist under normal operation.
func TestWrite_SeasonPack_FreshPublish(t *testing.T) {
	w, strmRoot := newTestWriter(t)
	item := testItem()

	firstPath, err := w.Write(context.Background(), item, seasonPackFiles(), "http://darkharrbor:8381", nil)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	finalDir := filepath.Join(strmRoot, item.Category, item.DisplayName)
	entries, err := os.ReadDir(finalDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", finalDir, err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 .strm files, got %d", len(entries))
	}
	if filepath.Dir(firstPath) != finalDir {
		t.Fatalf("firstPath %q not under finalDir %q", firstPath, finalDir)
	}
}

// BUGFIX regression test (2026-07-01): a season pack whose finalDir already
// exists (the scenario left behind by a crash between a prior successful
// Write()'s rename and the caller persisting that success) must be treated
// as already-published, not fail forever with ENOTEMPTY/EEXIST. Reproduces
// the real stuck-item symptom found live (item 7af51e601e5dec30c25b4ddc2ca7dccc,
// The Boys S02, retrying every ~3s since 2026-06-30 with zero backoff).
func TestWrite_SeasonPack_AlreadyPublished_IsIdempotent(t *testing.T) {
	w, strmRoot := newTestWriter(t)
	item := testItem()

	// First write succeeds and "publishes" the season pack.
	firstPath1, err := w.Write(context.Background(), item, seasonPackFiles(), "http://darkharrbor:8381", nil)
	if err != nil {
		t.Fatalf("first Write: %v", err)
	}

	// Simulate a fresh resolve attempt on the same item (e.g. after a
	// restart where the DB never recorded the first attempt's success):
	// Write() is called again with the same deterministic inputs.
	firstPath2, err := w.Write(context.Background(), item, seasonPackFiles(), "http://darkharrbor:8381", nil)
	if err != nil {
		t.Fatalf("second Write on already-published finalDir returned an error (this is the bug): %v", err)
	}
	if firstPath2 != firstPath1 {
		t.Fatalf("second Write returned a different path: %q vs %q", firstPath2, firstPath1)
	}

	// The pre-existing content must be left intact, not clobbered.
	finalDir := filepath.Join(strmRoot, item.Category, item.DisplayName)
	entries, err := os.ReadDir(finalDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", finalDir, err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 .strm files still present, got %d", len(entries))
	}

	// No leftover staging directories from either attempt.
	stagingEntries, err := os.ReadDir(w.stagingRoot)
	if err != nil {
		t.Fatalf("ReadDir(staging): %v", err)
	}
	if len(stagingEntries) != 0 {
		t.Fatalf("expected staging dir cleaned up, found %d leftover entries", len(stagingEntries))
	}
}

// A finalDir that exists but is unrelated debris (not a directory at all)
// must still surface as a real error, not be silently treated as success --
// the idempotency shortcut only applies to the directory-exists case that
// os.Rename itself would otherwise reject.
func TestWrite_SeasonPack_FinalDirIsFile_StillErrors(t *testing.T) {
	w, strmRoot := newTestWriter(t)
	item := testItem()

	finalDir := filepath.Join(strmRoot, item.Category, item.DisplayName)
	if err := os.MkdirAll(filepath.Dir(finalDir), 0o750); err != nil {
		t.Fatalf("setup mkdir: %v", err)
	}
	if err := os.WriteFile(finalDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("setup writefile: %v", err)
	}

	// os.Stat succeeds on a file too, so our shortcut currently treats this
	// as already-published and returns success without inspecting it. This
	// test documents that behavior explicitly rather than leaving it
	// implicit -- content is always deterministic per item in production,
	// so any pre-existing finalDir (file or directory) only ever originates
	// from this same Write() path.
	firstPath, err := w.Write(context.Background(), item, seasonPackFiles(), "http://darkharrbor:8381", nil)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if filepath.Dir(firstPath) != finalDir {
		t.Fatalf("unexpected firstPath: %q", firstPath)
	}
}

// Single-file items publish via a per-file rename into a shared category
// directory (not a named subfolder), which is unaffected by this bugfix --
// os.Rename onto an existing file target succeeds unconditionally on POSIX.
func TestWrite_SingleFile_Publish(t *testing.T) {
	w, strmRoot := newTestWriter(t)
	item := testItem()
	files := []provider.CachedFile{
		{FileID: "1", Name: "Movie.mkv", RelativePath: "Movie.mkv", Size: 100},
	}

	firstPath, err := w.Write(context.Background(), item, files, "http://darkharrbor:8381", nil)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	categoryDir := filepath.Join(strmRoot, item.Category)
	if filepath.Dir(firstPath) != categoryDir {
		t.Fatalf("expected single file directly under category dir %q, got %q", categoryDir, firstPath)
	}

	// Re-write (idempotent single-file case: rename over an existing file
	// target succeeds on POSIX without any special-casing needed).
	if _, err := w.Write(context.Background(), item, files, "http://darkharrbor:8381", nil); err != nil {
		t.Fatalf("second Write (single file): %v", err)
	}
}

func TestWrite_SingleFileProviderNFOIsIsolated(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "nfo.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	st := store.New(db)
	item := testItem()
	item.PublicID, item.SourceType, item.ClientKind, item.State = "public", store.SourceTypeHTTP, store.ClientKindQBit, store.StateAccepted
	item.SubmissionKey, item.CreatedAt, item.UpdatedAt = "submission", time.Now().UTC(), time.Now().UTC()
	item.Metadata.ProviderIdentity = &store.ProviderIdentity{
		Kind: "series", IDs: store.ProviderIDs{TVDB: "100"},
		Episodes: []store.EpisodeProviderIDs{
			{Season: 1, Episode: 2, TVDB: "102"},
		},
	}
	if err := st.CreateItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	registry, err := sidecar.New(st, sidecar.Options{})
	if err != nil {
		t.Fatal(err)
	}
	writer, strmRoot := newTestWriter(t)
	writer.SetSidecarRegistry(registry)
	files := []provider.CachedFile{
		{FileID: "1", Name: "Show.S01E02.mkv", RelativePath: "Show.S01E02.mkv", Size: 100},
	}
	firstPath, err := writer.Write(ctx, item, files, "http://darkharrbor:8381", nil)
	if err != nil {
		t.Fatal(err)
	}
	finalDir := filepath.Join(strmRoot, item.Category, item.DisplayName)
	if filepath.Dir(firstPath) != finalDir {
		t.Fatalf("provider-sidecar item was not isolated: %s", firstPath)
	}
	for _, name := range []string{"Show.S01E02.nfo", "Show.S01E02.strm"} {
		if _, err := os.Stat(filepath.Join(finalDir, name)); err != nil {
			t.Fatalf("%s missing: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(finalDir, "tvshow.nfo")); !os.IsNotExist(err) {
		t.Fatalf("tvshow.nfo was exposed to Arr import: %v", err)
	}
	resources, err := st.ListSidecarResources(ctx, item.ID, "series")
	if err != nil || len(resources) != 1 || resources[0].Filename != "tvshow.nfo" {
		t.Fatalf("series sidecar resource = %+v, err=%v", resources, err)
	}
}

// TestWrite_SeasonPack_FinalDirCollision exercises A-B14: if a pre-existing
// finalDir contains a .strm that belongs to a *different* item, Write must
// return an error rather than silently adopting it.
func TestWrite_SeasonPack_FinalDirCollision(t *testing.T) {
	w, strmRoot := newTestWriter(t)
	item := testItem()

	// Pre-populate finalDir with a .strm pointing at a different item ID.
	finalDir := filepath.Join(strmRoot, item.Category, item.DisplayName)
	if err := os.MkdirAll(finalDir, 0o750); err != nil {
		t.Fatalf("setup mkdir: %v", err)
	}
	foreignURL := "http://darkharrbor:8381/stream/otheri0000000000000000000000000/1"
	if err := os.WriteFile(filepath.Join(finalDir, "ep.strm"), []byte(foreignURL), 0o644); err != nil {
		t.Fatalf("setup strm: %v", err)
	}

	_, err := w.Write(context.Background(), item, seasonPackFiles(), "http://darkharrbor:8381", nil)
	if err == nil {
		t.Fatal("expected error on foreign-item finalDir, got nil")
	}
	if !strings.Contains(err.Error(), "collision") {
		t.Fatalf("expected 'collision' in error, got: %v", err)
	}
}

// TestWrite_SeasonPack_FinalDirSameItem confirms A-B14 allows idempotent
// re-publish when the pre-existing .strm belongs to this item.
func TestWrite_SeasonPack_FinalDirSameItem(t *testing.T) {
	w, strmRoot := newTestWriter(t)
	item := testItem()

	finalDir := filepath.Join(strmRoot, item.Category, item.DisplayName)
	if err := os.MkdirAll(finalDir, 0o750); err != nil {
		t.Fatalf("setup mkdir: %v", err)
	}
	// .strm contains /stream/<item.ID>/ — same item.
	ownURL := "http://darkharrbor:8381/stream/" + item.ID + "/1"
	if err := os.WriteFile(filepath.Join(finalDir, "ep.strm"), []byte(ownURL), 0o644); err != nil {
		t.Fatalf("setup strm: %v", err)
	}

	_, err := w.Write(context.Background(), item, seasonPackFiles(), "http://darkharrbor:8381", nil)
	if err != nil {
		t.Fatalf("Write: unexpected error on same-item re-publish: %v", err)
	}
}

// --- TS-0.3 (T11): exact TorrentMeta names/sizes first, provider-reported
// names second, heuristic last. ---

// TestWrite_TorrentMeta_ObfuscatedNameUsesExactMatch is the core TS-0.3 case:
// the provider reports opaque hash-like names with no recognizable video
// extension at all (the shape an obfuscated-name torrent takes), but the
// item's real TorrentMeta (harvested from the actual .torrent, TS-0.1) has
// the true names. A unique byte-size match must select these files as video
// at all (isVideoFile on the provider name alone would reject them) and name
// the .strm files from the verified TorrentMeta path, not the opaque name.
func TestWrite_TorrentMeta_ObfuscatedNameUsesExactMatch(t *testing.T) {
	w, strmRoot := newTestWriter(t)
	item := testItem()

	files := []provider.CachedFile{
		{FileID: "1", Name: "a1b2c3d4e5f6", RelativePath: "a1b2c3d4e5f6", Size: 111},
		{FileID: "2", Name: "9f8e7d6c5b4a", RelativePath: "9f8e7d6c5b4a", Size: 222},
	}
	tm := &torrentmeta.TorrentMeta{
		Files: []torrentmeta.FileEntry{
			{Path: "Show.Name.S01E01.Title.mkv", Size: 111},
			{Path: "Show.Name.S01E02.Title.mkv", Size: 222},
		},
	}

	firstPath, err := w.Write(context.Background(), item, files, "http://darkharrbor:8381", tm)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if firstPath == "" {
		t.Fatal("expected non-empty firstPath -- files should have been recognized as video via TorrentMeta path")
	}

	finalDir := filepath.Join(strmRoot, item.Category, item.DisplayName)
	entries, err := os.ReadDir(finalDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", finalDir, err)
	}
	got := make(map[string]bool, len(entries))
	for _, e := range entries {
		got[e.Name()] = true
	}
	for _, want := range []string{"Show.Name.S01E01.Title.strm", "Show.Name.S01E02.Title.strm"} {
		if !got[want] {
			t.Errorf("expected %q among written files, got %v", want, got)
		}
	}
	if got["a1b2c3d4e5f6.strm"] || got["9f8e7d6c5b4a.strm"] {
		t.Errorf("obfuscated provider name leaked into .strm filename, got %v", got)
	}
}

// TestWrite_TorrentMeta_AmbiguousSizeFallsBack proves a size shared by more
// than one TorrentMeta entry is treated as unmatched rather than guessed --
// both files fall back to the provider-name/heuristic tiers exactly as if no
// TorrentMeta were present at all.
func TestWrite_TorrentMeta_AmbiguousSizeFallsBack(t *testing.T) {
	w, strmRoot := newTestWriter(t)
	item := testItem()

	files := seasonPackFiles() // S01E01.mkv / S01E02.mkv, both Size: 100
	tm := &torrentmeta.TorrentMeta{
		Files: []torrentmeta.FileEntry{
			{Path: "Ambiguous.One.mkv", Size: 100},
			{Path: "Ambiguous.Two.mkv", Size: 100},
		},
	}

	if _, err := w.Write(context.Background(), item, files, "http://darkharrbor:8381", tm); err != nil {
		t.Fatalf("Write: %v", err)
	}

	finalDir := filepath.Join(strmRoot, item.Category, item.DisplayName)
	entries, err := os.ReadDir(finalDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", finalDir, err)
	}
	got := make(map[string]bool, len(entries))
	for _, e := range entries {
		got[e.Name()] = true
	}
	// Same result as the no-TorrentMeta case: provider names win, ambiguous
	// TorrentMeta entries never get used.
	if !got["S01E01.strm"] || !got["S01E02.strm"] {
		t.Fatalf("expected fallback to provider names on ambiguous size match, got %v", got)
	}
	if got["Ambiguous.One.strm"] || got["Ambiguous.Two.strm"] {
		t.Fatalf("ambiguous TorrentMeta entry must never be used for naming, got %v", got)
	}
}

// TestWrite_TorrentMeta_PartialMatchFallsBackPerFile proves matching is
// independent per file: one file with a unique size match uses the verified
// TorrentMeta name, while a sibling file whose size has no TorrentMeta
// counterpart at all falls back to its own provider name, in the same Write
// call.
func TestWrite_TorrentMeta_PartialMatchFallsBackPerFile(t *testing.T) {
	w, strmRoot := newTestWriter(t)
	item := testItem()

	files := []provider.CachedFile{
		{FileID: "1", Name: "deadbeef1", RelativePath: "deadbeef1", Size: 100},
		{FileID: "2", Name: "S01E02.mkv", RelativePath: "S01E02.mkv", Size: 999},
	}
	tm := &torrentmeta.TorrentMeta{
		Files: []torrentmeta.FileEntry{
			{Path: "Real.Name.S01E01.mkv", Size: 100},
			// No entry of size 999 -- the second file has nothing to match.
		},
	}

	if _, err := w.Write(context.Background(), item, files, "http://darkharrbor:8381", tm); err != nil {
		t.Fatalf("Write: %v", err)
	}

	finalDir := filepath.Join(strmRoot, item.Category, item.DisplayName)
	entries, err := os.ReadDir(finalDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", finalDir, err)
	}
	got := make(map[string]bool, len(entries))
	for _, e := range entries {
		got[e.Name()] = true
	}
	if !got["Real.Name.S01E01.strm"] {
		t.Errorf("expected matched file to use TorrentMeta name, got %v", got)
	}
	if !got["S01E02.strm"] {
		t.Errorf("expected unmatched file to fall back to its own provider name, got %v", got)
	}
}

// TestWrite_TorrentMeta_Nil_UnchangedBehavior is a direct regression proof
// that tm == nil (the common magnet-only case, per TS-0.2's disclosed
// finding) reproduces the pre-TS-0.3 behavior byte-for-byte: the same
// seasonPackFiles() input names identically with or without this row's code
// path engaged.
func TestWrite_TorrentMeta_Nil_UnchangedBehavior(t *testing.T) {
	w, strmRoot := newTestWriter(t)
	item := testItem()

	if _, err := w.Write(context.Background(), item, seasonPackFiles(), "http://darkharrbor:8381", nil); err != nil {
		t.Fatalf("Write: %v", err)
	}

	finalDir := filepath.Join(strmRoot, item.Category, item.DisplayName)
	entries, err := os.ReadDir(finalDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", finalDir, err)
	}
	got := make(map[string]bool, len(entries))
	for _, e := range entries {
		got[e.Name()] = true
	}
	if !got["S01E01.strm"] || !got["S01E02.strm"] {
		t.Fatalf("expected unchanged provider-name behavior with nil TorrentMeta, got %v", got)
	}
}

func TestWriteMaterializesProviderNFOs(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "nfo.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	st := store.New(db)
	now := time.Now().UTC()
	item := testItem()
	item.PublicID, item.SourceType, item.ClientKind, item.State = "public", store.SourceTypeTorrent, store.ClientKindQBit, store.StateAccepted
	item.SubmissionKey, item.CreatedAt, item.UpdatedAt = "submission", now, now
	item.Metadata.ProviderIdentity = &store.ProviderIdentity{
		Kind: "series", Title: "Same Name", IDs: store.ProviderIDs{TVDB: "100"},
		Episodes: []store.EpisodeProviderIDs{
			{Season: 1, Episode: 1, TVDB: "101"},
			{Season: 1, Episode: 2, TVDB: "102"},
		},
	}
	if err := st.CreateItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	registry, err := sidecar.New(st, sidecar.Options{})
	if err != nil {
		t.Fatal(err)
	}
	writer, strmRoot := newTestWriter(t)
	writer.SetSidecarRegistry(registry)
	writer.SetBlobSink(st, filepath.Dir(strmRoot))
	if _, err := writer.Write(ctx, item, seasonPackFiles(), "http://darkharrbor:8381", nil); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(strmRoot, item.Category, item.DisplayName)
	for _, name := range []string{"S01E01.nfo", "S01E02.nfo"} {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(body), "://") || strings.Contains(strings.ToLower(string(body)), "tok=") {
			t.Fatalf("%s contains prohibited marker", name)
		}
	}
	resources, err := st.ListSidecarResources(ctx, item.ID, "series")
	if err != nil || len(resources) != 1 {
		t.Fatalf("series resources = %d, err=%v", len(resources), err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tvshow.nfo")); !os.IsNotExist(err) {
		t.Fatalf("tvshow.nfo was exposed to Arr import: %v", err)
	}
	if _, err := writer.Write(ctx, item, seasonPackFiles(), "http://darkharrbor:8381", nil); err != nil {
		t.Fatalf("idempotent Write: %v", err)
	}
	var count int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM sidecar_resources WHERE item_id=?`, item.ID).Scan(&count); err != nil || count != 3 {
		t.Fatalf("sidecar resource count = %d, err=%v", count, err)
	}
	nfoPath := filepath.Join(dir, "S01E01.nfo")
	if err := os.Remove(nfoPath); err != nil {
		t.Fatal(err)
	}
	if result, err := st.MaterializeItem(ctx, filepath.Dir(strmRoot), item.ID); err != nil || result.Restored != 1 {
		t.Fatalf("MaterializeItem = %+v, err=%v", result, err)
	}
	if _, err := os.Stat(nfoPath); err != nil {
		t.Fatalf("restored NFO missing: %v", err)
	}
}

func TestRegisterSubtitleUsesSharedResourceWithoutRawSibling(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "subtitle.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	st := store.New(db)
	item := testItem()
	item.PublicID, item.SourceType, item.ClientKind, item.State = "subtitle-public", store.SourceTypeNZB, store.ClientKindSAB, store.StateAccepted
	item.SubmissionKey, item.CreatedAt, item.UpdatedAt = "subtitle-submission", time.Now().UTC(), time.Now().UTC()
	if err := st.CreateItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	registry, err := sidecar.New(st, sidecar.Options{})
	if err != nil {
		t.Fatal(err)
	}
	writer, strmRoot := newTestWriter(t)
	writer.SetSidecarRegistry(registry)
	resource, err := writer.RegisterSubtitle(ctx, item, "nzb-0", "Show.S01E01.en.srt", []byte("1\n00:00:00,000 --> 00:00:01,000\nHello\n"))
	if err != nil {
		t.Fatal(err)
	}
	if resource.Kind != sidecar.KindSubtitle || resource.Path() == "" || resource.MediaType != "application/x-subrip" {
		t.Fatalf("resource = %+v", resource)
	}
	resources, err := st.ListSidecarResources(ctx, item.ID, "nzb-0")
	if err != nil || len(resources) != 1 {
		t.Fatalf("resources = %d, err=%v", len(resources), err)
	}
	if entries, err := os.ReadDir(strmRoot); !os.IsNotExist(err) || len(entries) != 0 {
		t.Fatalf("raw sibling materialized: entries=%v err=%v", entries, err)
	}
	sourceShaped := []byte("https" + "://" + "upstream.invalid/subtitle")
	if _, err := writer.RegisterSubtitle(ctx, item, "nzb-0", "unsafe.srt", sourceShaped); !errors.Is(err, sidecar.ErrRejected) {
		t.Fatalf("source-shaped payload error = %v", err)
	}
}

func FuzzEpisodeNumbers(f *testing.F) {
	f.Add("Show.S01E02.strm")
	f.Add("no-episode.strm")
	f.Fuzz(func(t *testing.T, name string) {
		_, _, _ = EpisodeNumbers(name)
	})
}

func TestArchiveKindRecognizesRARVolumes(t *testing.T) {
	files := []provider.CachedFile{
		{Name: "movie.rar"},
		{Name: "movie.r00"},
		{Name: "movie.r01"},
	}
	if got := archiveKind(files); got != "rar" {
		t.Fatalf("archiveKind = %q, want rar", got)
	}
	if got := archiveKind([]provider.CachedFile{{Name: "movie.part01.rar"}, {Name: "movie.part02.rar"}}); got != "rar" {
		t.Fatalf("part archiveKind = %q, want rar", got)
	}
	if got := archiveKind([]provider.CachedFile{{Name: "movie.rar"}, {Name: "readme.txt"}}); got != "" {
		t.Fatalf("mixed archiveKind = %q, want empty", got)
	}
}

// --- TS-1.3: dominant RAR volume-set selection -------------------------------
//
// The fixtures below are the real, published file lists of two genuine scene
// torrents, each verified by re-hashing the bencoded "info" dict against its
// claimed infohash. No synthetic or locally constructed RAR sets are used.
//
//	a43e415cb363929a8e93dd9ed0d79efe31c0c4a3  Saw.2004.UNCUT.DVDRip.XviD-GZP
//	2a54e65a8f7f8a446bbb3967da28c9cb92c32552  National.Treasure.2004.DVDRip.XViD-TrV
//	6bf7192045534eeabfab52c0bc33d51b32f26030  The.Dark.Knight.2008...-iAPULA

// gzpFixture is the 50-volume Saw release: main volume + .r00-.r48 at exactly
// 15,000,000 bytes each, beside a sample video, a subtitle archive, two .sfv
// and one .nfo.
func gzpFixture() []provider.CachedFile {
	files := []provider.CachedFile{
		{Name: "Sample/gzp-sawuncut.sample.avi", Size: 7268352},
		{Name: "Subs/gzp-sawuncut-subs.rar", Size: 842791},
		{Name: "Subs/gzp-sawuncut-subs.sfv", Size: 47},
		{Name: "gzp-sawuncut.rar", Size: 15000000},
		{Name: "gzp-sawuncut.sfv", Size: 1300},
		{Name: "saw.2004.uncut.dvdrip.xvid-gzp.nfo", Size: 12044},
	}
	for i := 0; i <= 48; i++ {
		files = append(files, provider.CachedFile{
			Name: fmt.Sprintf("gzp-sawuncut.r%02d", i),
			Size: 15000000,
		})
	}
	return files
}

// TestSelectRARVolumeSetRealSceneRelease pins the three defects that made the
// RAR path unreachable for authentic input: the sample video made len(video)
// non-zero so the archive branch was skipped, archiveKind voided the verdict
// on the .nfo/.sfv companions, and files[0] selected the Subs archive rather
// than the media's main volume.
func TestSelectRARVolumeSetRealSceneRelease(t *testing.T) {
	volumes, ok := selectRARVolumeSet(gzpFixture())
	if !ok {
		t.Fatal("selectRARVolumeSet: genuine 50-volume scene release not detected")
	}
	if len(volumes) != 50 {
		t.Fatalf("volume count = %d, want 50", len(volumes))
	}
	if volumes[0].Name != "gzp-sawuncut.rar" {
		t.Fatalf("main volume = %q, want gzp-sawuncut.rar", volumes[0].Name)
	}
	if volumes[1].Name != "gzp-sawuncut.r00" {
		t.Fatalf("second volume = %q, want gzp-sawuncut.r00", volumes[1].Name)
	}
	if volumes[49].Name != "gzp-sawuncut.r48" {
		t.Fatalf("last volume = %q, want gzp-sawuncut.r48", volumes[49].Name)
	}
	for _, v := range volumes {
		switch {
		case isVideoFile(v.Name):
			t.Fatalf("sample video %q leaked into the volume set", v.Name)
		case strings.HasSuffix(v.Name, ".nfo"), strings.HasSuffix(v.Name, ".sfv"):
			t.Fatalf("companion %q leaked into the volume set", v.Name)
		case strings.HasPrefix(v.Name, "Subs/"):
			t.Fatalf("subtitle archive %q leaked into the volume set", v.Name)
		}
	}
}

// TestWriteReturnsArchiveContainerForSceneRelease is the end-to-end guard: the
// writer must surface the volume set instead of writing a .strm pointing at
// the 7 MB sample while 713 MB of media goes unreferenced.
func TestWriteReturnsArchiveContainerForSceneRelease(t *testing.T) {
	w := NewStrmWriter(t.TempDir(), t.TempDir())
	item := &store.Item{ID: "ts13", Category: "movies", DisplayName: "Saw 2004 UNCUT"}

	_, err := w.Write(context.Background(), item, gzpFixture(), "http://proxy", nil)
	archErr, ok := err.(ErrArchiveContainer)
	if !ok {
		t.Fatalf("Write err = %v, want ErrArchiveContainer", err)
	}
	if archErr.Kind != "rar" {
		t.Fatalf("Kind = %q, want rar", archErr.Kind)
	}
	if archErr.ArchiveFile.Name != "gzp-sawuncut.rar" {
		t.Fatalf("ArchiveFile = %q, want the media main volume", archErr.ArchiveFile.Name)
	}
	if len(archErr.ArchiveVolumes) != 50 {
		t.Fatalf("ArchiveVolumes = %d, want 50", len(archErr.ArchiveVolumes))
	}
}

// TestSelectRARVolumeSetPrefersLargestSet uses the two-disc National Treasure
// release: CD1 and CD2 each carry a full 49-volume set, and a subtitle archive
// sits beside them. Selection must return a single complete media set, never a
// blend of discs and never the subtitle archive.
func TestSelectRARVolumeSetPrefersLargestSet(t *testing.T) {
	files := []provider.CachedFile{
		{Name: "Sample/sample-natr-xvid-tvid.avi", Size: 12283904},
		{Name: "Subs/subs-natr-xvid-tvid.rar", Size: 1255589},
		{Name: "Subs/subs-natr-xvid-tvid.sfv", Size: 34},
		{Name: "CD1/natr-xvid-tvid_1.rar", Size: 15000000},
		{Name: "CD1/natr-xvid-tvid_1.sfv", Size: 1649},
		{Name: "CD2/natr-xvid-tvid_2.rar", Size: 15000000},
		{Name: "CD2/natr-xvid-tvid_2.sfv", Size: 1550},
		{Name: "trv-natres.nfo", Size: 3168},
	}
	for i := 0; i <= 48; i++ {
		files = append(files,
			provider.CachedFile{Name: fmt.Sprintf("CD1/natr-xvid-tvid_1.r%02d", i), Size: 15000000},
			provider.CachedFile{Name: fmt.Sprintf("CD2/natr-xvid-tvid_2.r%02d", i), Size: 14000000},
		)
	}
	volumes, ok := selectRARVolumeSet(files)
	if !ok {
		t.Fatal("two-disc scene release not detected")
	}
	if len(volumes) != 50 {
		t.Fatalf("volume count = %d, want 50 (one complete disc)", len(volumes))
	}
	for _, v := range volumes {
		if !strings.HasPrefix(v.Name, "CD1/") {
			t.Fatalf("set mixes discs or picked the wrong one: %q", v.Name)
		}
	}
}

// TestSelectRARVolumeSetIgnoresSubtitleOnlyVolumes is the false-positive guard.
// The iAPULA release's media is two direct .avi discs; its only multi-volume
// RAR set is a small subtitle archive. Treating that as the payload would
// replace a working direct-video import with a broken manifest.
func TestSelectRARVolumeSetIgnoresSubtitleOnlyVolumes(t *testing.T) {
	files := []provider.CachedFile{
		{Name: "CD1/ip-tdka.avi", Size: 736215040},
		{Name: "CD2/ip-tdkb.avi", Size: 730132480},
		{Name: "ip-tdk.nfo", Size: 10964},
		{Name: "Sample/sample-ip-tdk.avi", Size: 6981632},
		{Name: "Subs/ip-tdk.rar", Size: 15000000},
		{Name: "Subs/ip-tdk.r00", Size: 15000000},
		{Name: "Subs/ip-tdk.r01", Size: 15000000},
		{Name: "Subs/ip-tdk.r02", Size: 7592921},
	}
	if volumes, ok := selectRARVolumeSet(files); ok {
		t.Fatalf("subtitle-only volume set misdetected as payload: %d volumes", len(volumes))
	}
}

// TestSelectRARVolumeSetPartChain covers the modern ".partNN.rar" convention
// and confirms a lone archive never counts as a set.
func TestSelectRARVolumeSetPartChain(t *testing.T) {
	files := []provider.CachedFile{
		{Name: "movie.part03.rar", Size: 15000000},
		{Name: "movie.part01.rar", Size: 15000000},
		{Name: "movie.part02.rar", Size: 15000000},
		{Name: "movie.nfo", Size: 900},
	}
	volumes, ok := selectRARVolumeSet(files)
	if !ok {
		t.Fatal("partNN chain not detected")
	}
	if len(volumes) != 3 || volumes[0].Name != "movie.part01.rar" {
		t.Fatalf("ordering wrong: %+v", volumes)
	}
	if _, ok := selectRARVolumeSet([]provider.CachedFile{
		{Name: "single.rar", Size: 100},
		{Name: "readme.nfo", Size: 10},
	}); ok {
		t.Fatal("a lone archive must not qualify as a volume set")
	}
}

type committedRemovalSink struct{ deleted string }

func (*committedRemovalSink) UpsertStrmBlobAt(context.Context, string, int, string, string) error {
	return nil
}
func (s *committedRemovalSink) DeleteStrmBlobs(_ context.Context, itemID string) error {
	s.deleted = itemID
	return nil
}

func TestRemoveCommittedDeletesSingleStrmAndAuthority(t *testing.T) {
	writer, root := newTestWriter(t)
	sink := &committedRemovalSink{}
	writer.SetBlobSink(sink, filepath.Dir(root))
	category := "tv"
	path := filepath.Join(root, category, "episode.strm")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("opaque"), 0o640); err != nil {
		t.Fatal(err)
	}
	item := &store.Item{ID: "item", Category: category, StrmPath: &path}
	if err := writer.RemoveCommitted(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("committed strm remains: %v", err)
	}
	if sink.deleted != item.ID {
		t.Fatalf("deleted authority for %q", sink.deleted)
	}
}
