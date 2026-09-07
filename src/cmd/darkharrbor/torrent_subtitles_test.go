package main

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/output"
	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/sidecar"
	"github.com/darkharrbor/darkharrbor/internal/store"
	"github.com/darkharrbor/darkharrbor/internal/torrentmeta"
)

type subtitleURLProvider map[string]string

func (p subtitleURLProvider) RequestDownloadURL(_ context.Context, _ *store.Item, fileID string) (string, error) {
	return p[fileID], nil
}

func TestTS61TorrentDirectAndArchiveSubtitles(t *testing.T) {
	direct := []byte("1\n00:00:00,000 --> 00:00:01,000\nDirect\n")
	archive := subtitleZIP(t, map[string]string{
		"Subs/Show.S01E01.ass": "Archive",
		"Subs/unrelated.srt":   "Ignored",
	})
	directServer := rangeFixtureServer(t, direct)
	archiveServer := rangeFixtureServer(t, archive)
	files := []provider.CachedFile{
		{FileID: "video-1", Name: "opaque-video", Size: 100},
		{FileID: "video-2", Name: "opaque-video-2", Size: 101},
		{FileID: "subtitle-1", Name: "opaque-direct", Size: int64(len(direct)), RequestDLURL: directServer.URL},
		{FileID: "archive-1", Name: "opaque-archive", Size: int64(len(archive)), RequestDLURL: archiveServer.URL},
	}
	meta := &torrentmeta.TorrentMeta{Files: []torrentmeta.FileEntry{
		{Path: "Show.S01E01.mkv", Size: 100},
		{Path: "Show.S01E02.mkv", Size: 101},
		{Path: "Subs/Show.S01E01.en.srt", Size: int64(len(direct))},
		{Path: "Subs/Subtitles.zip", Size: int64(len(archive))},
	}}
	targets := torrentSubtitleTargets(files, meta)
	if len(targets) != 2 || targets[0].FileID != "video-1" || targets[1].FileID != "video-2" {
		t.Fatalf("targets=%+v", targets)
	}
	item := &store.Item{ID: "ts61-item"}
	urls := subtitleURLProvider{"subtitle-1": directServer.URL, "archive-1": archiveServer.URL}
	directPayloads, err := readTorrentDirectSubtitles(context.Background(), item, files, meta, targets, urls, nil, 1<<20, 1<<20, 8, 64)
	if err != nil || len(directPayloads) != 1 || directPayloads[0].Filename != "Show.S01E01.en.srt" ||
		directPayloads[0].FileID != "video-1" || !bytes.Equal(directPayloads[0].Bytes, direct) {
		t.Fatalf("direct=%+v err=%v", directPayloads, err)
	}
	if got, err := readTorrentDirectSubtitles(context.Background(), item, files, meta, targets, urls, nil,
		1<<20, 1<<20, 8, int64(len(direct)-1)); err != nil || len(got) != 0 {
		t.Fatalf("total-byte limit payloads=%+v err=%v", got, err)
	}
	contradictory := append([]provider.CachedFile(nil), files...)
	contradictory[2].Size++
	if got, err := readTorrentDirectSubtitles(context.Background(), item, contradictory, meta, targets, urls, nil,
		1<<20, 1<<20, 8, 64); err != nil || len(got) != 0 {
		t.Fatalf("contradictory-size payloads=%+v err=%v", got, err)
	}
	nonRange := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(direct) }))
	t.Cleanup(nonRange.Close)
	nonRanged := append([]provider.CachedFile(nil), files...)
	nonRanged[2].RequestDLURL = nonRange.URL
	if got, err := readTorrentDirectSubtitles(context.Background(), item, nonRanged, meta, targets,
		subtitleURLProvider{"subtitle-1": nonRange.URL}, nil, 1<<20, 1<<20, 8, 64); err != nil || len(got) != 0 {
		t.Fatalf("range-incapable payloads=%+v err=%v", got, err)
	}
	archived, err := readTorrentZIPSubtitles(context.Background(), item, files, meta, targets, urls, nil, 1<<20, 1<<20, 8, 64)
	if err != nil || len(archived) != 1 || archived[0].Filename != "Show.S01E01.ass" ||
		archived[0].FileID != "video-1" || string(archived[0].Bytes) != "Archive" {
		t.Fatalf("archived=%+v err=%v", archived, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readTorrentDirectSubtitles(ctx, item, files, meta, targets, urls, nil, 1<<20, 1<<20, 8, 64); err != context.Canceled {
		t.Fatalf("cancel error=%v", err)
	}
}

func TestTS61RegistrationUsesOnlySharedResource(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(root, "ts61.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	st := store.New(db)
	item := &store.Item{
		ID: "ts61-register", PublicID: "ts61-public", SourceType: store.SourceTypeTorrent,
		ClientKind: store.ClientKindQBit, Category: "tv", State: store.StateResolving,
		SubmissionKey: "ts61-register", DisplayName: "Show.S01E01",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	registry, err := sidecar.New(st, sidecar.Options{})
	if err != nil {
		t.Fatal(err)
	}
	mediaRoot := filepath.Join(root, "media")
	writer := output.NewStrmWriter(mediaRoot, filepath.Join(root, "staging"))
	writer.SetSidecarRegistry(registry)
	payloads := []sidecar.SubtitlePayload{{
		Filename: "Show.S01E01.en.srt", FileID: "video-1",
		Bytes: []byte("1\n00:00:00,000 --> 00:00:01,000\nHello\n"),
	}}
	if err := registerTorrentSubtitles(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), writer, item, payloads); err != nil {
		t.Fatal(err)
	}
	resources, err := st.ListSidecarResources(ctx, item.ID, "video-1")
	if err != nil || len(resources) != 1 || resources[0].Path() == "" {
		t.Fatalf("resources=%+v err=%v", resources, err)
	}
	if _, err := os.Stat(mediaRoot); !os.IsNotExist(err) {
		t.Fatalf("raw subtitle root exists: %v", err)
	}
}

func subtitleZIP(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var raw bytes.Buffer
	zw := zip.NewWriter(&raw)
	for name, data := range files {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		writer, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

func rangeFixtureServer(t *testing.T, data []byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spec := strings.TrimPrefix(r.Header.Get("Range"), "bytes=")
		parts := strings.SplitN(spec, "-", 2)
		if len(parts) != 2 {
			http.Error(w, "range required", http.StatusBadRequest)
			return
		}
		start, startErr := strconv.ParseInt(parts[0], 10, 64)
		end, endErr := strconv.ParseInt(parts[1], 10, 64)
		if startErr != nil || endErr != nil || start < 0 || end < start || end >= int64(len(data)) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
	t.Cleanup(server.Close)
	return server
}
