package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

type c02Provider struct{ downloadURL string }

func (p c02Provider) Name() string                        { return "c0.2-test" }
func (p c02Provider) Capabilities() provider.Capabilities { return provider.Capabilities{} }
func (p c02Provider) CheckCached(context.Context, *store.Item) (*provider.CheckCachedResult, error) {
	return &provider.CheckCachedResult{Cached: true}, nil
}
func (p c02Provider) Submit(context.Context, *store.Item, provider.SubmitOptions) (*provider.CreateTaskResponse, error) {
	return &provider.CreateTaskResponse{RemoteID: "remote"}, nil
}
func (p c02Provider) Poll(context.Context, *store.Item) (*provider.TaskStatus, error) {
	return &provider.TaskStatus{}, nil
}
func (p c02Provider) RequestDownloadURL(context.Context, *store.Item, string) (string, error) {
	return p.downloadURL, nil
}
func (p c02Provider) Remove(context.Context, *store.Item) error { return nil }

func TestC02ProbeStaysBehindProxy(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/x-matroska")
		_, _ = io.WriteString(w, "proxied")
	}))
	defer origin.Close()

	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "c0.2.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	st := store.New(db)
	now := time.Now().UTC()
	remoteID := "remote"
	item := &store.Item{
		ID: "c02-probe", PublicID: "c02-probe", SourceType: store.SourceTypeTorrent,
		ClientKind: store.ClientKindQBit, Category: "tv", State: store.StateReady,
		SubmissionKey: "c02-probe", DisplayName: "c02-probe", RemoteID: &remoteID,
		TotalSize: 7, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.CreateItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.TorBox.BaseURL = "https://provider.invalid"
	s := &Server{
		cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), store: st,
		prov: c02Provider{downloadURL: origin.URL},
	}
	req := httptest.NewRequest(http.MethodGet, "/stream/c02-probe/0?probe=1", nil)
	req.SetPathValue("item_id", "c02-probe")
	req.SetPathValue("file_id", "0")
	rec := httptest.NewRecorder()
	s.handleStreamProxy(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "proxied" {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if location := rec.Header().Get("Location"); location != "" {
		t.Fatalf("probe exposed upstream Location %q", location)
	}
}

// TestFileListEntryForID_OutOfOrderFileIDs is a regression test for a real
// bug found live 2026-07-27 during TS-0.3's CG-02 gate: a real TorBox item
// reported its files in the order [FileID "2", FileID "0", FileID "1"], and
// the HEAD/resolve-metadata call sites used to index file_list positionally
// (entries[Atoi(fileID)]) instead of matching each entry's own FileID. A
// request for FileID "2" landed on entries[2] -- FileID "1" -- silently
// returning the wrong file's size and content-type. GET/range playback was
// unaffected (RequestDownloadURL is keyed by FileID directly, not position);
// only this metadata hint was wrong.
func TestFileListEntryForID_OutOfOrderFileIDs(t *testing.T) {
	entries := []fileListEntry{
		{FileID: "2", Name: "movie.mp4", Size: 630299550},
		{FileID: "0", Name: "movie.srt", Size: 93759},
		{FileID: "1", Name: "cover.jpg", Size: 130677},
	}

	e, ok := fileListEntryForID(entries, "2")
	if !ok {
		t.Fatal("expected a match for FileID \"2\"")
	}
	if e.Name != "movie.mp4" || e.Size != 630299550 {
		t.Fatalf("FileID \"2\" resolved to wrong entry: %+v", e)
	}

	e, ok = fileListEntryForID(entries, "1")
	if !ok || e.Name != "cover.jpg" || e.Size != 130677 {
		t.Fatalf("FileID \"1\" resolved incorrectly: ok=%v entry=%+v", ok, e)
	}

	e, ok = fileListEntryForID(entries, "0")
	if !ok || e.Name != "movie.srt" || e.Size != 93759 {
		t.Fatalf("FileID \"0\" resolved incorrectly: ok=%v entry=%+v", ok, e)
	}
}

// TestFileListEntryForID_LegacyEmptyFileID preserves the pre-existing,
// intentional fallback for rows written before B-N3 started populating
// FileID at all (see fileListEntry's own doc comment): the positional index
// path param is the only identity available for those legacy rows.
func TestFileListEntryForID_LegacyEmptyFileID(t *testing.T) {
	entries := []fileListEntry{
		{Name: "S01E01.mkv", Size: 100},
		{Name: "S01E02.mkv", Size: 200},
	}

	e, ok := fileListEntryForID(entries, "1")
	if !ok || e.Name != "S01E02.mkv" || e.Size != 200 {
		t.Fatalf("legacy positional fallback failed: ok=%v entry=%+v", ok, e)
	}
}

// TestFileListEntryForID_SingleEntryFallback preserves the pre-existing
// single-file fallback: a single-video item resolves regardless of what
// fileID string is requested (matches the pre-fix behavior for this case).
func TestFileListEntryForID_SingleEntryFallback(t *testing.T) {
	entries := []fileListEntry{{FileID: "7", Name: "movie.mkv", Size: 42}}

	e, ok := fileListEntryForID(entries, "anything")
	if !ok || e.Name != "movie.mkv" || e.Size != 42 {
		t.Fatalf("single-entry fallback failed: ok=%v entry=%+v", ok, e)
	}
}

// TestFileListEntryForID_NoMatch confirms an empty/unmatched file_list
// correctly reports no match so callers fall back to item-level metadata.
func TestFileListEntryForID_NoMatch(t *testing.T) {
	entries := []fileListEntry{
		{FileID: "5", Name: "a.mkv", Size: 1},
		{FileID: "6", Name: "b.mkv", Size: 2},
	}
	if _, ok := fileListEntryForID(entries, "99"); ok {
		t.Fatal("expected no match for an unrelated FileID against a multi-entry list")
	}
	if _, ok := fileListEntryForID(nil, "0"); ok {
		t.Fatal("expected no match against an empty file_list")
	}
}

// TestHandleStreamProxy_HEAD_OutOfOrderFileIDs is an end-to-end regression
// proof at the actual HTTP handler: HEAD for the real out-of-order-FileID
// shape now returns the correct file's Content-Length/Content-Type.
func TestHandleStreamProxy_HEAD_OutOfOrderFileIDs(t *testing.T) {
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "head-fix.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	st := store.New(db)
	now := time.Now().UTC()
	remoteID := "remote"
	fileList := `[{"file_id":"2","name":"movie.mp4","size":630299550},{"file_id":"0","name":"movie.srt","size":93759},{"file_id":"1","name":"cover.jpg","size":130677}]`
	item := &store.Item{
		ID: "head-fix", PublicID: "head-fix", SourceType: store.SourceTypeTorrent,
		ClientKind: store.ClientKindQBit, Category: "movies", State: store.StateReady,
		SubmissionKey: "head-fix", DisplayName: "head-fix", RemoteID: &remoteID,
		TotalSize: 630523986, FileList: &fileList, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.CreateItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.TorBox.BaseURL = "https://provider.invalid"
	s := &Server{
		cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), store: st,
	}
	req := httptest.NewRequest(http.MethodHead, "/stream/head-fix/2", nil)
	req.SetPathValue("item_id", "head-fix")
	req.SetPathValue("file_id", "2")
	rec := httptest.NewRecorder()
	s.handleStreamProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if got := rec.Header().Get("Content-Length"); got != "630299550" {
		t.Fatalf("Content-Length = %q, want 630299550 (the real video, not the jpg)", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "video/mp4" {
		t.Fatalf("Content-Type = %q, want video/mp4", got)
	}
}
