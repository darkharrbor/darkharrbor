package api

import (
	"bytes"
	"context"
	"crypto/sha1" // #nosec G505 -- BitTorrent v1 fixture.
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/bytesource"
	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/contentproof"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
	"github.com/darkharrbor/darkharrbor/internal/store"
	"github.com/darkharrbor/darkharrbor/internal/torrentmeta"
)

func TestRecoverTorrentCrossLaneWindowThroughHTTPAndRejectsCorruption(t *testing.T) {
	ctx := context.Background()
	good := bytes.Repeat([]byte("verified-piece-"), 8)
	corrupt := append([]byte(nil), good...)
	corrupt[0] ^= 0xff
	var serveCorrupt atomic.Bool
	serveCorrupt.Store(true)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := good
		if serveCorrupt.Load() {
			body = corrupt
		}
		var start, end int
		if _, err := fmt.Sscanf(strings.TrimPrefix(r.Header.Get("Range"), "bytes="), "%d-%d", &start, &end); err != nil || start < 0 || end < start || end >= len(body) {
			http.Error(w, "range required", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body[start : end+1])
	}))
	defer origin.Close()

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "torrent-crosslane.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	st := store.New(db)
	key := httpstream.ResolveKey{
		Version: httpstream.ResolveKeyVersion, BackendID: "b", Handler: "generic",
		Kind: "movie", IDs: map[string]string{"x": "1"}, Selector: "next",
	}
	canonical, err := key.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	httpFile := httpstream.PersistedFile{
		FileID: "hf000000000072", Selector: key.Selector, Name: "video.mkv",
		Size: int64(len(good)), RangeVerified: true,
	}
	fileList, err := httpstream.MarshalFiles([]httpstream.PersistedFile{httpFile})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(72, 0).UTC()
	target := &store.Item{
		ID: "torrent-target", PublicID: "torrent-target", SourceType: store.SourceTypeTorrent,
		ClientKind: store.ClientKindQBit, Category: "movies", State: store.StateReady,
		SubmissionKey: "torrent-target", DisplayName: "Controlled.Release.2026",
		CreatedAt: now, UpdatedAt: now,
	}
	alternate := &store.Item{
		ID: "http-alternate", PublicID: "http-alternate", SourceType: store.SourceTypeHTTP,
		ClientKind: store.ClientKindQBit, Category: "movies", State: store.StateReady,
		SubmissionKey: "http-alternate", DisplayName: "controlled release 2026",
		FileList: &fileList, ResolveKey: &canonical, CreatedAt: now.Add(time.Second), UpdatedAt: now.Add(time.Second),
	}
	for _, item := range []*store.Item{target, alternate} {
		if err := st.CreateItem(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	graph, err := contentproof.New(st, contentproof.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	registry := httpstream.NewRegistry()
	registry.Register(key.BackendID, 0, &genericPrewarmHandler{url: origin.URL, size: int64(len(good))})
	cfg := &config.Config{}
	cfg.NNTP.CrossLaneSplice = true
	cfg.HTTPStream.AllowPrivateSourceCIDRs = true
	srv := &Server{
		cfg: cfg, store: st, httpProofGraph: graph, httpHandlers: registry,
		httpResolveCoord: newHTTPResolveCoordinator(), httpRelayClient: origin.Client(),
	}
	srv.httpClientOnce.Do(func() {})

	const pieceLength = 32
	hashes := make([]byte, 0, ((len(good)+pieceLength-1)/pieceLength)*sha1.Size)
	for off := 0; off < len(good); off += pieceLength {
		end := off + pieceLength
		if end > len(good) {
			end = len(good)
		}
		sum := sha1.Sum(good[off:end]) // #nosec G401 -- BitTorrent v1 definition.
		hashes = append(hashes, sum[:]...)
	}
	meta := &torrentmeta.TorrentMeta{
		PieceLength:   pieceLength,
		Files:         []torrentmeta.FileEntry{{Path: "video.mkv", Size: int64(len(good)), EndOffset: int64(len(good))}},
		PieceHashesV1: hashes,
	}
	file := &meta.Files[0]

	if got, err := srv.recoverTorrentCrossLaneWindow(ctx, target, "0", meta, file, 7, int64(len(good)-8)); err == nil || got.data != nil {
		t.Fatalf("corrupt candidate accepted: bytes=%d err=%v", len(got.data), err)
	}
	serveCorrupt.Store(false)
	got, err := srv.recoverTorrentCrossLaneWindow(ctx, target, "0", meta, file, 7, int64(len(good)-8))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.data, good[7:len(good)-7]) || got.blocks < 2 || got.httpBlocks != got.blocks || got.nntpBlocks != 0 {
		t.Fatalf("recovery summary bytes=%d blocks=%d http=%d nntp=%d", len(got.data), got.blocks, got.httpBlocks, got.nntpBlocks)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			concurrent, recoverErr := srv.recoverTorrentCrossLaneWindow(ctx, target, "0", meta, file, 7, int64(len(good)-8))
			if recoverErr != nil || !bytes.Equal(concurrent.data, good[7:len(good)-7]) {
				errs <- recoverErr
			}
		}()
	}
	wg.Wait()
	close(errs)
	for recoverErr := range errs {
		t.Fatalf("concurrent recovery failed: %v", recoverErr)
	}

	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	if got, err := srv.recoverTorrentCrossLaneWindow(cancelCtx, target, "0", meta, file, 0, 1); !errors.Is(err, context.Canceled) || got.data != nil {
		t.Fatalf("cancel result bytes=%d err=%v", len(got.data), err)
	}
	cfg.NNTP.CrossLaneSplice = false
	if got, err := srv.ResolveTorrentCrossLaneCandidates(ctx, target.DisplayName, int64(len(good))); err != nil || got != nil {
		t.Fatalf("disabled candidates=%v err=%v", got, err)
	}
}

func TestHR63HTTPRecoveryUsesHTTPGovernorAndCancelsCleanly(t *testing.T) {
	data := []byte("verified-http-recovery")
	gov := accountgov.New("http-recovery-test")
	gov.SetCapacity(HTTPSourceGovOp, 1)
	source := &governedHTTPRecoverySource{
		ByteSource: bytesource.NewMemSource("http-candidate", data),
		gov:        gov,
		session:    "target",
	}

	held, err := gov.Acquire(context.Background(), HTTPSourceGovOp, accountgov.PriorityPlayback, "playback")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, len(data))
		_, readErr := source.ReadAt(ctx, buf, 0)
		done <- readErr
	}()
	waitForHTTPGovQueue(t, gov, 1)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled recovery error = %v", err)
	}
	held.Release()
	if gov.InUse(HTTPSourceGovOp) != 0 || gov.Waiting(HTTPSourceGovOp) != 0 {
		t.Fatalf("HTTP recovery governor leaked: in_use=%d waiting=%d", gov.InUse(HTTPSourceGovOp), gov.Waiting(HTTPSourceGovOp))
	}

	buf := make([]byte, len(data))
	n, err := source.ReadAt(context.Background(), buf, 0)
	if err != nil || n != len(data) || !bytes.Equal(buf, data) {
		t.Fatalf("governed HTTP recovery = %q, %v", buf[:n], err)
	}
	if gov.InUse(HTTPSourceGovOp) != 0 || gov.Waiting(HTTPSourceGovOp) != 0 {
		t.Fatalf("successful HTTP recovery governor leaked: in_use=%d waiting=%d", gov.InUse(HTTPSourceGovOp), gov.Waiting(HTTPSourceGovOp))
	}
}

func TestCDNChunkSourceUsesOnlyExactCrossLaneWindow(t *testing.T) {
	want := []byte("verified recovery")
	source := &cdnChunkSource{
		url: "://invalid", windowBytes: int64(len(want)), total: int64(len(want)),
		crossLaneRecover: func(context.Context, int64, int64) (torrentCrossLaneResult, error) {
			return torrentCrossLaneResult{data: append([]byte(nil), want...), blocks: 1, httpBlocks: 1, bytes: int64(len(want))}, nil
		},
	}
	ref := rangecache.ChunkRef{Index: 0, Key: "w000000000000", Size: int64(len(want))}
	got, err := source.Fetch(context.Background(), ref, rangecache.FetchDemand)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("recovery bytes=%q err=%v", got, err)
	}
	source.crossLaneRecover = func(context.Context, int64, int64) (torrentCrossLaneResult, error) {
		return torrentCrossLaneResult{data: want[:len(want)-1]}, nil
	}
	if got, err := source.Fetch(context.Background(), ref, rangecache.FetchDemand); err == nil || got != nil {
		t.Fatalf("short recovery accepted: bytes=%d err=%v", len(got), err)
	}
}
