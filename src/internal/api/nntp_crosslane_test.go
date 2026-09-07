package api

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- test fixture exercises PAR2 IFSC MD5.
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	archiveparser "github.com/darkharrbor/darkharrbor/internal/archive"
	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/contentproof"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/nntp"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func TestResolveNNTPCrossLaneCandidates(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "crosslane.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	st := store.New(db)

	torrentFiles := `[{"file_id":"2","name":"video.mkv","size":1234}]`
	httpFiles, err := httpstream.MarshalFiles([]httpstream.PersistedFile{{FileID: "hf000000000001", Selector: "video", Name: "video.mp4", Size: 1234, RangeVerified: true}})
	if err != nil {
		t.Fatal(err)
	}
	wrongHTTPFiles, err := httpstream.MarshalFiles([]httpstream.PersistedFile{{FileID: "hf000000000002", Selector: "video", Name: "wrong.mp4", Size: 4321, RangeVerified: true}})
	if err != nil {
		t.Fatal(err)
	}
	unverifiedHTTPFiles, err := httpstream.MarshalFiles([]httpstream.PersistedFile{{FileID: "hf000000000003", Selector: "video", Name: "unverified.mp4", Size: 1234}})
	if err != nil {
		t.Fatal(err)
	}
	malformedFiles := "{"
	oversizedTorrentEntries := make([]fileListEntry, maxNNTPCrossLaneFilesPerItem+1)
	oversizedHTTPEntries := make([]httpstream.PersistedFile, maxNNTPCrossLaneFilesPerItem+1)
	for i := range oversizedTorrentEntries {
		size := int64(4321)
		if i == 0 {
			size = 1234
		}
		oversizedTorrentEntries[i] = fileListEntry{FileID: strconv.Itoa(i + 10), Name: "video.mkv", Size: size}
		oversizedHTTPEntries[i] = httpstream.PersistedFile{FileID: "hf" + strconv.Itoa(i+10), Name: "video.mp4", Size: size, RangeVerified: true}
	}
	oversizedTorrentJSON, err := json.Marshal(oversizedTorrentEntries)
	if err != nil {
		t.Fatal(err)
	}
	oversizedTorrentFiles := string(oversizedTorrentJSON)
	oversizedHTTPFiles, err := httpstream.MarshalFiles(oversizedHTTPEntries)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, item := range []*store.Item{
		{ID: "torrent-item", PublicID: "torrent-item", SourceType: store.SourceTypeTorrent, ClientKind: store.ClientKindQBit, Category: "movies", State: store.StateReady, SubmissionKey: "torrent-item", DisplayName: "Example.Release_2026", FileList: &torrentFiles, CreatedAt: now, UpdatedAt: now},
		{ID: "http-item", PublicID: "http-item", SourceType: store.SourceTypeHTTP, ClientKind: store.ClientKindQBit, Category: "movies", State: store.StateReady, SubmissionKey: "http-item", DisplayName: "example release 2026", FileList: &httpFiles, CreatedAt: now.Add(time.Second), UpdatedAt: now.Add(time.Second)},
		{ID: "wrong-item", PublicID: "wrong-item", SourceType: store.SourceTypeHTTP, ClientKind: store.ClientKindQBit, Category: "movies", State: store.StateReady, SubmissionKey: "wrong-item", DisplayName: "example release 2026", FileList: &wrongHTTPFiles, CreatedAt: now.Add(2 * time.Second), UpdatedAt: now.Add(2 * time.Second)},
		{ID: "unverified-item", PublicID: "unverified-item", SourceType: store.SourceTypeHTTP, ClientKind: store.ClientKindQBit, Category: "movies", State: store.StateReady, SubmissionKey: "unverified-item", DisplayName: "example release 2026", FileList: &unverifiedHTTPFiles, CreatedAt: now.Add(3 * time.Second), UpdatedAt: now.Add(3 * time.Second)},
		{ID: "malformed-item", PublicID: "malformed-item", SourceType: store.SourceTypeTorrent, ClientKind: store.ClientKindQBit, Category: "movies", State: store.StateReady, SubmissionKey: "malformed-item", DisplayName: "example release 2026", FileList: &malformedFiles, CreatedAt: now.Add(4 * time.Second), UpdatedAt: now.Add(4 * time.Second)},
		{ID: "oversized-torrent-item", PublicID: "oversized-torrent-item", SourceType: store.SourceTypeTorrent, ClientKind: store.ClientKindQBit, Category: "movies", State: store.StateReady, SubmissionKey: "oversized-torrent-item", DisplayName: "example release 2026", FileList: &oversizedTorrentFiles, CreatedAt: now.Add(5 * time.Second), UpdatedAt: now.Add(5 * time.Second)},
		{ID: "oversized-http-item", PublicID: "oversized-http-item", SourceType: store.SourceTypeHTTP, ClientKind: store.ClientKindQBit, Category: "movies", State: store.StateReady, SubmissionKey: "oversized-http-item", DisplayName: "example release 2026", FileList: &oversizedHTTPFiles, CreatedAt: now.Add(6 * time.Second), UpdatedAt: now.Add(6 * time.Second)},
	} {
		if err := st.CreateItem(ctx, item); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.Config{}
	srv := &Server{cfg: cfg, store: st}
	if got, err := srv.ResolveNNTPCrossLaneCandidates(ctx, "example-release.2026", 1234); err != nil || got != nil {
		t.Fatalf("disabled candidates=%v err=%v", got, err)
	}
	cfg.NNTP.CrossLaneSplice = true
	got, err := srv.ResolveNNTPCrossLaneCandidates(ctx, "example-release.2026", 1234)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Lane != contentproof.LaneTorrent || got[1].Lane != contentproof.LaneHTTP {
		t.Fatalf("candidates=%+v", got)
	}
	for _, candidate := range got {
		if candidate.ReleaseKey == "" || candidate.RepresentationID == "" || strings.Contains(candidate.ReleaseKey, "example") {
			t.Fatalf("candidate is not opaque: %+v", candidate)
		}
	}
}

func TestResolveNNTPCrossLaneCandidatesFailsClosed(t *testing.T) {
	cfg := &config.Config{}
	cfg.NNTP.CrossLaneSplice = true
	srv := &Server{cfg: cfg}
	if got, err := srv.ResolveNNTPCrossLaneCandidates(context.Background(), "release", 1); err != nil || got != nil {
		t.Fatalf("nil store candidates=%v err=%v", got, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	srv.store = &store.Store{}
	if _, err := srv.ResolveNNTPCrossLaneCandidates(ctx, "release", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
}

func TestRecoverNNTPCrossLaneBlockThroughXR(t *testing.T) {
	ctx := context.Background()
	data := bytes.Repeat([]byte("ifsc-controlled-block-"), 64)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spec := strings.TrimPrefix(r.Header.Get("Range"), "bytes=")
		parts := strings.SplitN(spec, "-", 2)
		if len(parts) != 2 {
			http.Error(w, "range required", http.StatusBadRequest)
			return
		}
		start, startErr := strconv.ParseInt(parts[0], 10, 64)
		end, endErr := strconv.ParseInt(parts[1], 10, 64)
		if startErr != nil || endErr != nil || start < 0 || end < start || end >= int64(len(data)) {
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
	defer origin.Close()

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "recover.db"), 5*time.Second)
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
	file := httpstream.PersistedFile{
		FileID: "hf000000000004", Selector: key.Selector, Name: "video.mkv",
		Size: int64(len(data)), RangeVerified: true,
	}
	files, err := httpstream.MarshalFiles([]httpstream.PersistedFile{file})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, item := range []*store.Item{
		{
			ID: "nntp-target", PublicID: "nntp-target", SourceType: store.SourceTypeNZB,
			ClientKind: store.ClientKindSAB, Category: "movies", State: store.StateReady,
			SubmissionKey: "nntp-target", DisplayName: "Controlled.Release.2026",
			CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "http-candidate", PublicID: "http-candidate", SourceType: store.SourceTypeHTTP,
			ClientKind: store.ClientKindQBit, Category: "movies", State: store.StateReady,
			SubmissionKey: "http-candidate", DisplayName: "controlled release 2026",
			FileList: &files, ResolveKey: &canonical, CreatedAt: now.Add(time.Second), UpdatedAt: now.Add(time.Second),
		},
	} {
		if err := st.CreateItem(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	graph, err := contentproof.New(st, contentproof.Options{})
	if err != nil {
		t.Fatal(err)
	}
	registry := httpstream.NewRegistry()
	registry.Register(key.BackendID, 0, &genericPrewarmHandler{url: origin.URL, size: file.Size})
	cfg := &config.Config{}
	cfg.NNTP.CrossLaneSplice = true
	cfg.HTTPStream.AllowPrivateSourceCIDRs = true
	cfg.Cache.StreamChunkSizeMB = 1
	srv := &Server{
		cfg: cfg, store: st, httpProofGraph: graph, httpHandlers: registry,
		httpResolveCoord: newHTTPResolveCoordinator(), httpRelayClient: origin.Client(),
	}
	srv.httpClientOnce.Do(func() {})
	sum := md5.Sum(data) // #nosec G401 -- PAR2 IFSC is defined as MD5.
	index := &archiveparser.PAR2ProofIndex{
		SliceSize: int64(len(data)),
		Files: []archiveparser.PAR2ProofFile{{
			NZBFileIndex: 0, Length: int64(len(data)), BlockMD5: sum[:],
		}},
	}
	domain, ok := nntp.NewPAR2ProofDomain(index, 0)
	if !ok {
		t.Fatal("proof domain unavailable")
	}
	proof, ok := domain.ProofAt(0)
	if !ok {
		t.Fatal("proof unavailable")
	}
	handles, err := srv.ResolveNNTPCrossLaneCandidates(ctx, "Controlled.Release.2026", int64(len(data)))
	if err != nil || len(handles) != 1 {
		t.Fatalf("candidate resolution count=%d err=%v", len(handles), err)
	}
	stale := handles[0]
	stale.ReleaseKey, _ = contentproof.ReleaseKey("different release", stale.Size)
	if _, err := srv.openNNTPCrossLaneSource(ctx, stale); err == nil {
		t.Fatal("stale release identity accepted")
	}
	src, err := srv.openNNTPCrossLaneSource(ctx, handles[0])
	if err != nil {
		t.Fatalf("candidate source: %v", err)
	}
	buf := make([]byte, len(data))
	if n, readErr := src.ReadAt(ctx, buf, 0); readErr != nil || n != len(buf) || !bytes.Equal(buf, data) {
		t.Fatalf("candidate read bytes=%d err=%v", n, readErr)
	}
	block, err := srv.RecoverNNTPCrossLaneBlock(ctx, "nntp-target", 0, int64(len(data)), proof)
	if err != nil {
		t.Fatal(err)
	}
	if block.Lane != contentproof.LaneHTTP || !bytes.Equal(block.Data, data) {
		t.Fatalf("unexpected recovered block lane=%s bytes=%d", block.Lane, len(block.Data))
	}

	const readers = 8
	errs := make(chan error, readers)
	var wg sync.WaitGroup
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, recoverErr := srv.RecoverNNTPCrossLaneBlock(ctx, "nntp-target", 0, int64(len(data)), proof)
			if recoverErr != nil {
				errs <- recoverErr
				return
			}
			if got.Lane != contentproof.LaneHTTP || !bytes.Equal(got.Data, data) {
				errs <- errors.New("concurrent recovered block disagrees")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	corrupt := sum
	corrupt[0] ^= 0xff
	conflictingIndex := &archiveparser.PAR2ProofIndex{
		SliceSize: int64(len(data)),
		Files: []archiveparser.PAR2ProofFile{{
			NZBFileIndex: 0, Length: int64(len(data)), BlockMD5: corrupt[:],
		}},
	}
	conflictingDomain, ok := nntp.NewPAR2ProofDomain(conflictingIndex, 0)
	if !ok {
		t.Fatal("conflicting proof domain unavailable")
	}
	conflictingProof, _ := conflictingDomain.ProofAt(0)
	if _, err := srv.RecoverNNTPCrossLaneBlock(ctx, "nntp-target", 0, int64(len(data)), conflictingProof); !errors.Is(err, nntp.ErrCrossLaneAbstain) {
		t.Fatalf("conflicting proof error=%v", err)
	}
	decision, err := graph.VerifyDigest(ctx, nntpCrossLaneRepresentationID("nntp-target", 0), contentproof.ScopeBlock, 0, int64(len(data)), contentproof.AlgorithmMD5, sum[:])
	if err != nil || decision.Relation != contentproof.RelationProven {
		t.Fatalf("authoritative proof was overwritten: decision=%+v err=%v", decision, err)
	}
}
