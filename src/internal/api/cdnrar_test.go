package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	archiveparser "github.com/darkharrbor/darkharrbor/internal/archive"
	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

type cdnRARProvider struct {
	urls  map[string]string
	calls atomic.Int32
}

func (*cdnRARProvider) Name() string                        { return "test" }
func (*cdnRARProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }
func (*cdnRARProvider) CheckCached(context.Context, *store.Item) (*provider.CheckCachedResult, error) {
	return &provider.CheckCachedResult{Cached: true}, nil
}
func (*cdnRARProvider) Submit(context.Context, *store.Item, provider.SubmitOptions) (*provider.CreateTaskResponse, error) {
	return &provider.CreateTaskResponse{RemoteID: "remote"}, nil
}
func (*cdnRARProvider) Poll(context.Context, *store.Item) (*provider.TaskStatus, error) {
	return nil, fmt.Errorf("not implemented")
}
func (p *cdnRARProvider) RequestDownloadURL(_ context.Context, _ *store.Item, fileID string) (string, error) {
	p.calls.Add(1)
	if url := p.urls[fileID]; url != "" {
		return url, nil
	}
	return "", fmt.Errorf("missing file")
}
func (*cdnRARProvider) Remove(context.Context, *store.Item) error { return nil }

func rangeOrigin(data []byte, ignoreRange bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ignoreRange {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}
		value := strings.TrimPrefix(r.Header.Get("Range"), "bytes=")
		parts := strings.SplitN(value, "-", 2)
		start, _ := strconv.ParseInt(parts[0], 10, 64)
		end, _ := strconv.ParseInt(parts[1], 10, 64)
		if start < 0 || start >= int64(len(data)) || end < start {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(data)))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if end >= int64(len(data)) {
			end = int64(len(data) - 1)
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
}

func newCDNRARTestServer(t *testing.T, prov provider.Provider, item *store.Item) (*Server, context.CancelFunc) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "cdnrar.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	st := store.New(db)
	if err := st.CreateItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	cache := rangecache.New(rangecache.Config{DiskCachePath: t.TempDir(), DiskCacheSizeMB: 8}, streamCDNTestLog)
	t.Cleanup(cache.Close)
	ctx, cancel := context.WithCancel(context.Background())
	cfg := &config.Config{}
	cfg.Cache.StreamMode = "disk"
	cfg.Cache.StreamChunkSizeMB = 1
	cfg.Cache.StreamMinBufferSegments = 1
	cfg.Lifecycle.CleanupHours = 1
	return &Server{
		cfg:              cfg,
		log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:            st,
		prov:             prov,
		allProviders:     map[string]provider.Provider{prov.Name(): prov},
		providerOrder:    []string{prov.Name()},
		streamCache:      cache,
		shutdownCtx:      ctx,
		shutdownCancel:   cancel,
		janitorTimers:    map[string]context.CancelFunc{},
		activeGoroutines: map[string]struct{}{},
	}, cancel
}

func TestTorrentRARFullRangeMatrixAndManifestPersistence(t *testing.T) {
	first := []byte("HEADfirst")
	second := []byte("HDRsecond!")
	one := rangeOrigin(first, false)
	t.Cleanup(one.Close)
	two := rangeOrigin(second, false)
	t.Cleanup(two.Close)
	prov := &cdnRARProvider{urls: map[string]string{
		"one": one.URL + "?token=must-not-persist",
		"two": two.URL + "?token=must-not-persist",
	}}
	manifest := &archiveparser.StoredRARManifest{
		MemberName: "movie.mkv",
		TotalSize:  12,
		Parts: []archiveparser.StoredRARPart{
			{FileID: "one", ArchiveSize: int64(len(first)), DataOffset: 4, DataSize: 5},
			{FileID: "two", ArchiveSize: int64(len(second)), DataOffset: 3, DataSize: 7},
		},
	}
	providerName := prov.Name()
	item := &store.Item{
		ID: "rar-item", PublicID: "rar-public", SourceType: store.SourceTypeTorrent,
		ClientKind: store.ClientKindQBit, Category: "movies", State: store.StateReady,
		SubmissionKey: "rar-key", DisplayName: "RAR Movie", Provider: &providerName,
		Metadata:  store.SubmissionMetadata{TorrentRARManifest: manifest},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	s, cancel := newCDNRARTestServer(t, prov, item)
	t.Cleanup(func() {
		cancel()
		s.wg.Wait()
	})

	head := httptest.NewRecorder()
	s.handleTorrentRAR(head, httptest.NewRequest(http.MethodHead, "/stream/rar-item/rar:0", nil), item, "rar:0")
	if head.Code != http.StatusOK || head.Header().Get("Content-Length") != "12" {
		t.Fatalf("HEAD = %d length=%q", head.Code, head.Header().Get("Content-Length"))
	}
	for _, test := range []struct {
		value string
		code  int
		body  string
	}{
		{value: "bytes=0-3", code: http.StatusPartialContent, body: "firs"},
		{value: "bytes=3-8", code: http.StatusPartialContent, body: "stseco"},
		{value: "bytes=9-", code: http.StatusPartialContent, body: "nd!"},
		{value: "bytes=-4", code: http.StatusPartialContent, body: "ond!"},
		{value: "bytes=12-13", code: http.StatusRequestedRangeNotSatisfiable},
	} {
		req := httptest.NewRequest(http.MethodGet, "/stream/rar-item/rar:0", nil)
		req.Header.Set("Range", test.value)
		rec := httptest.NewRecorder()
		s.handleTorrentRAR(rec, req, item, "rar:0")
		if rec.Code != test.code || rec.Body.String() != test.body {
			t.Fatalf("%s = %d/%q, want %d/%q", test.value, rec.Code, rec.Body.String(), test.code, test.body)
		}
	}
	if prov.calls.Load() != 2 {
		t.Fatalf("provider URL calls = %d, want one per cached volume", prov.calls.Load())
	}
	got, err := s.store.GetItemByID(context.Background(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf("%+v", got.Metadata.TorrentRARManifest)
	if got.Metadata.TorrentRARManifest == nil || strings.Contains(raw, "http://") || strings.Contains(raw, "token=") {
		t.Fatalf("persisted manifest leaked or disappeared: %s", raw)
	}
}

func TestTorrentRARRejectsRangeIgnoringVolume(t *testing.T) {
	origin := rangeOrigin([]byte("HDRpayload"), true)
	t.Cleanup(origin.Close)
	prov := &cdnRARProvider{urls: map[string]string{"one": origin.URL}}
	providerName := prov.Name()
	item := &store.Item{
		ID: "rar-ignore", PublicID: "rar-ignore-public", SourceType: store.SourceTypeTorrent,
		ClientKind: store.ClientKindQBit, Category: "movies", State: store.StateReady,
		SubmissionKey: "rar-ignore-key", DisplayName: "RAR Ignore", Provider: &providerName,
		Metadata: store.SubmissionMetadata{TorrentRARManifest: &archiveparser.StoredRARManifest{
			MemberName: "movie.mkv", TotalSize: 7,
			Parts: []archiveparser.StoredRARPart{{FileID: "one", ArchiveSize: 10, DataOffset: 3, DataSize: 7}},
		}},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	s, cancel := newCDNRARTestServer(t, prov, item)
	t.Cleanup(cancel)
	rec := httptest.NewRecorder()
	s.handleTorrentRAR(rec, httptest.NewRequest(http.MethodGet, "/stream/rar-ignore/rar:0", nil), item, "rar:0")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "does not support byte ranges") {
		t.Fatalf("range-ignoring response = %d/%q", rec.Code, rec.Body.String())
	}
}
