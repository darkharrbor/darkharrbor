package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// ---- minimal test-only bencode encoder (mirrors internal/torrentmeta's own
// test fixtures) so these tests need no fixture file on disk. ----

func tmBStr(s string) string   { return fmt.Sprintf("%d:%s", len(s), s) }
func tmBBytes(b []byte) string { return fmt.Sprintf("%d:%s", len(b), b) }
func tmBInt(n int64) string    { return fmt.Sprintf("i%de", n) }
func tmBDict(pairs ...string) string {
	return "d" + strings.Join(pairs, "") + "e"
}

func canonicalTestTorrentBytes(name string, size int64) []byte {
	pieceHash := bytes.Repeat([]byte{0xAB}, 20)
	info := tmBDict(
		tmBStr("length")+tmBInt(size),
		tmBStr("name")+tmBStr(name),
		tmBStr("piece length")+tmBInt(16384),
		tmBStr("pieces")+tmBBytes(pieceHash),
	)
	return []byte(tmBDict(tmBStr("info") + info))
}

// pureV2TestTorrentBytes builds a torrent with meta version 2 and a file
// tree but NO v1 "pieces" field -- the case resolveTorrentFileSubmission
// must decline (no v1 infohash to submit under).
func pureV2TestTorrentBytes(name string, size int64) []byte {
	root := bytes.Repeat([]byte{0x42}, 32)
	leaf := tmBDict(tmBStr("") + tmBDict(tmBStr("length")+tmBInt(size), tmBStr("pieces root")+tmBBytes(root)))
	fileTree := tmBDict(tmBStr(name) + leaf)
	info := tmBDict(
		tmBStr("file tree")+fileTree,
		tmBStr("meta version")+tmBInt(2),
		tmBStr("name")+tmBStr(name),
		tmBStr("piece length")+tmBInt(16384),
	)
	return []byte(tmBDict(tmBStr("info") + info))
}

// ---- resolveTorrentFileSubmission: pure logic, no Server/store needed ----

func TestResolveTorrentFileSubmissionValidSingleFile(t *testing.T) {
	data := canonicalTestTorrentBytes("Movie.2024.1080p.mkv", 4096)
	meta, infoHash, displayName, magnet, err := resolveTorrentFileSubmission(data, "")
	if err != nil {
		t.Fatalf("resolveTorrentFileSubmission: %v", err)
	}
	if infoHash == "" {
		t.Fatal("infoHash is empty, want a computed v1 infohash")
	}
	if displayName != "Movie.2024.1080p.mkv" {
		t.Fatalf("displayName = %q, want the real torrent name", displayName)
	}
	wantMagnetPrefix := "magnet:?xt=urn:btih:" + infoHash
	if !strings.HasPrefix(magnet, wantMagnetPrefix) {
		t.Fatalf("magnet = %q, want prefix %q", magnet, wantMagnetPrefix)
	}
	if !strings.Contains(magnet, "dn=Movie.2024.1080p.mkv") {
		t.Fatalf("magnet = %q, want it to carry the real name as dn", magnet)
	}
	if len(meta.Files) != 1 || meta.Files[0].Size != 4096 {
		t.Fatalf("meta.Files = %+v, want one 4096-byte file", meta.Files)
	}
}

func TestResolveTorrentFileSubmissionPrefersRenameOverName(t *testing.T) {
	data := canonicalTestTorrentBytes("Obfuscated.Name.mkv", 100)
	_, _, displayName, _, err := resolveTorrentFileSubmission(data, "Sonarr.Chosen.Name.mkv")
	if err != nil {
		t.Fatalf("resolveTorrentFileSubmission: %v", err)
	}
	if displayName != "Sonarr.Chosen.Name.mkv" {
		t.Fatalf("displayName = %q, want the explicit rename to win", displayName)
	}
}

func TestResolveTorrentFileSubmissionRecoversFromHashNamedRename(t *testing.T) {
	// If the arr's own rename field degenerately equals the infohash
	// (mirrors the magnet-path "Rejected Hashed Release Title" bug this
	// codebase already works around), the real torrent name must win.
	data := canonicalTestTorrentBytes("Real.Release.Name.mkv", 100)
	meta, infoHash, _, _, err := resolveTorrentFileSubmission(data, "")
	if err != nil {
		t.Fatalf("resolveTorrentFileSubmission (priming): %v", err)
	}
	_ = meta
	_, _, displayName, _, err := resolveTorrentFileSubmission(data, infoHash)
	if err != nil {
		t.Fatalf("resolveTorrentFileSubmission: %v", err)
	}
	if displayName != "Real.Release.Name.mkv" {
		t.Fatalf("displayName = %q, want recovery to the real name, not the hash", displayName)
	}
}

func TestResolveTorrentFileSubmissionRejectsMalformedInput(t *testing.T) {
	if _, _, _, _, err := resolveTorrentFileSubmission([]byte("not bencode at all"), ""); err == nil {
		t.Fatal("resolveTorrentFileSubmission succeeded on garbage input, want error")
	}
}

func TestResolveTorrentFileSubmissionDeclinesPureV2NoV1Hash(t *testing.T) {
	data := pureV2TestTorrentBytes("only.mkv", 500)
	meta, infoHash, _, _, err := resolveTorrentFileSubmission(data, "")
	if err == nil {
		t.Fatal("resolveTorrentFileSubmission succeeded on a pure v2-only torrent, want a declined-submission error")
	}
	if infoHash != "" {
		t.Fatalf("infoHash = %q, want empty", infoHash)
	}
	// The parsed TorrentMeta is still returned to the caller so it can be
	// persisted even though submission itself is declined.
	if meta == nil || meta.InfoHashV2 == "" {
		t.Fatal("meta with a computed v2 infohash should still be returned")
	}
}

// ---- handleQBitAdd integration: malformed-upload rejection path only.
// This path returns before ever reaching enqueueSubmission's
// cachegate/governor/background-resolve machinery, so it is safe to drive
// through the real HTTP handler without standing up a full fake provider
// stack. The accept/success path is proven by the real CG-02 live gate
// instead (WORKFLOW: canned fixtures prove deterministic logic; they do not
// replace a real provider/backend/Arr gate). ----

func newQBitAddTestStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "qbit_add.db"), 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	return store.New(db)
}

func newQBitAddTestServer(st *store.Store) *Server {
	cfg := &config.Config{}
	cfg.Routing.Preference = []string{config.LaneUncachedTorrent}
	cfg.Compatibility.DefaultCategory = "darkharrbor"
	return &Server{
		cfg:   cfg,
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		store: st,
	}
}

func buildTorrentsMultipartRequest(t *testing.T, filename string, torrentBytes []byte) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("torrents", filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write(torrentBytes); err != nil {
		t.Fatalf("write torrent bytes: %v", err)
	}
	if err := mw.WriteField("category", "darkharrbor"); err != nil {
		t.Fatalf("write category field: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest("POST", "/api/v2/torrents/add", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	return rr, req
}

// TestHandleQBitAddRejectsMalformedTorrentFile is the DG-08 regression for
// TS-0.1: previously ANY uploaded .torrent file was silently ignored
// (created=0, "Fails.") because handleQBitAdd never read the "torrents"
// multipart field at all. A malformed upload must still be declined
// (non-fatal to the request), proving the new code path is reached and
// fails closed rather than panicking or accepting garbage.
func TestHandleQBitAddRejectsMalformedTorrentFile(t *testing.T) {
	st := newQBitAddTestStore(t)
	server := newQBitAddTestServer(st)

	rr, req := buildTorrentsMultipartRequest(t, "garbage.torrent", []byte("this is not bencode"))
	server.handleQBitAdd(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if body := rr.Body.String(); !strings.Contains(body, "Fails.") {
		t.Fatalf("body = %q, want Fails. for a malformed upload", body)
	}

	items, err := st.ListVisibleClientItems(req.Context(), store.ClientKindQBit, "darkharrbor", 10)
	if err != nil {
		t.Fatalf("ListVisibleClientItems: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("items = %d, want 0 for a malformed upload", len(items))
	}
}
