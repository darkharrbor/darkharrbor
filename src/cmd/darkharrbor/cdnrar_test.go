package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/output"
	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
	"github.com/darkharrbor/darkharrbor/internal/torbox"
)

func TestRARVolumeOrder(t *testing.T) {
	names := []string{"movie.part03.rar", "movie.part01.rar", "movie.part02.rar"}
	sort.SliceStable(names, func(i, j int) bool { return rarVolumeOrder(names[i]) < rarVolumeOrder(names[j]) })
	want := []string{"movie.part01.rar", "movie.part02.rar", "movie.part03.rar"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("order = %v, want %v", names, want)
		}
	}
	names = []string{"movie.r01", "movie.rar", "movie.r00"}
	sort.SliceStable(names, func(i, j int) bool { return rarVolumeOrder(names[i]) < rarVolumeOrder(names[j]) })
	want = []string{"movie.rar", "movie.r00", "movie.r01"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("legacy order = %v, want %v", names, want)
		}
	}
}

type resolverRARProvider struct{ url string }

func (*resolverRARProvider) Name() string                        { return "test" }
func (*resolverRARProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }
func (*resolverRARProvider) CheckCached(context.Context, *store.Item) (*provider.CheckCachedResult, error) {
	return &provider.CheckCachedResult{Cached: true}, nil
}
func (*resolverRARProvider) Submit(context.Context, *store.Item, provider.SubmitOptions) (*provider.CreateTaskResponse, error) {
	return nil, fmt.Errorf("unused")
}
func (*resolverRARProvider) Poll(context.Context, *store.Item) (*provider.TaskStatus, error) {
	return nil, fmt.Errorf("unused")
}
func (p *resolverRARProvider) RequestDownloadURL(context.Context, *store.Item, string) (string, error) {
	return p.url, nil
}
func (*resolverRARProvider) Remove(context.Context, *store.Item) error { return nil }

func storedRAR3(payload []byte) []byte {
	const name = "movie.mkv"
	const signatureLen = 7
	headSize := 32 + len(name)
	data := make([]byte, signatureLen+headSize)
	copy(data, []byte{0x52, 0x61, 0x72, 0x21, 0x1a, 0x07, 0x00})
	block := signatureLen
	data[block+2] = 0x74
	binary.LittleEndian.PutUint16(data[block+5:], uint16(headSize))
	binary.LittleEndian.PutUint32(data[block+7:], uint32(len(payload)))
	binary.LittleEndian.PutUint32(data[block+11:], uint32(len(payload)))
	data[block+25] = 0x30
	binary.LittleEndian.PutUint16(data[block+26:], uint16(len(name)))
	copy(data[block+32:], name)
	return append(data, payload...)
}

func TestResolveCDNRARItemPersistsReadyVirtualFile(t *testing.T) {
	archive := storedRAR3([]byte("video-payload"))
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value := strings.TrimPrefix(r.Header.Get("Range"), "bytes=")
		parts := strings.SplitN(value, "-", 2)
		start, _ := strconv.ParseInt(parts[0], 10, 64)
		end, _ := strconv.ParseInt(parts[1], 10, 64)
		if end >= int64(len(archive)) {
			end = int64(len(archive) - 1)
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(archive)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(archive[start : end+1])
	}))
	t.Cleanup(origin.Close)
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "resolver-rar.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	st := store.New(db)
	root := t.TempDir()
	writer := output.NewStrmWriter(filepath.Join(root, "strm"), filepath.Join(root, "staging"))
	writer.SetBlobSink(st, root)
	writer.SetStreamSecret(strings.Repeat("s", 32))
	item := &store.Item{
		ID: "resolver-rar", PublicID: "resolver-rar-public", SourceType: store.SourceTypeTorrent,
		ClientKind: store.ClientKindQBit, Category: "movies", State: store.StateResolving,
		SubmissionKey: "resolver-rar-key", DisplayName: "Movie.2026",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Cache.StreamChunkSizeMB = 1
	prov := &resolverRARProvider{url: origin.URL + "?token=must-not-persist"}
	resolveCDNRARItem(
		ctx,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		st,
		writer,
		"http://darkharrbor:8381",
		item,
		[]torbox.CachedFile{{FileID: "7", Name: "movie.rar", Size: int64(len(archive))}},
		[]torbox.CachedFile{{FileID: "7", Name: "movie.rar", Size: int64(len(archive))}},
		nil,
		cfg,
		prov,
		nil,
		strings.Repeat("s", 32),
		&readyNotifier{},
	)
	got, err := st.GetItemByID(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.StateReady || got.Metadata.TorrentRARManifest == nil ||
		got.Metadata.TorrentRARManifest.TotalSize != int64(len("video-payload")) ||
		got.FileList == nil || !strings.Contains(*got.FileList, `"file_id":"rar:0"`) {
		t.Fatalf("resolved item = %+v", got)
	}
	body, err := os.ReadFile(*got.StrmPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "/stream/resolver-rar/rar:0") ||
		strings.Contains(string(body), origin.URL) || strings.Contains(*got.FileList, origin.URL) {
		t.Fatalf("unsafe or incorrect strm/file list: strm=%q files=%q", body, *got.FileList)
	}
	var blobs int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM strm_blobs WHERE item_id=?`, item.ID).Scan(&blobs); err != nil || blobs != 1 {
		t.Fatalf("strm blob count = %d, err=%v", blobs, err)
	}
}
