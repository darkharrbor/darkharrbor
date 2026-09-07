package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/output"
	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

type rx93TestHandler struct {
	results      []httpstream.SearchResult
	searchFn     func() []httpstream.SearchResult
	resolved     []httpstream.ResolvedFile
	searchCalls  atomic.Int32
	resolveCalls atomic.Int32
}

func (*rx93TestHandler) Name() string { return "stremio" }

func (h *rx93TestHandler) Search(context.Context, httpstream.StreamQuery) ([]httpstream.SearchResult, error) {
	h.searchCalls.Add(1)
	if h.searchFn != nil {
		return append([]httpstream.SearchResult(nil), h.searchFn()...), nil
	}
	return append([]httpstream.SearchResult(nil), h.results...), nil
}

func (h *rx93TestHandler) Resolve(context.Context, httpstream.ResolveRequest) ([]httpstream.ResolvedFile, error) {
	h.resolveCalls.Add(1)
	return append([]httpstream.ResolvedFile(nil), h.resolved...), nil
}

type rx93Fixture struct {
	server  *Server
	store   *store.Store
	handler *rx93TestHandler
	origin  *httptest.Server
	payload []byte
	key     httpstream.ResolveKey
}

func newRX93Fixture(t *testing.T, results func(httpstream.ResolveKey, string, int64) []httpstream.SearchResult) *rx93Fixture {
	t.Helper()
	payload := bytes.Repeat([]byte("rx93-media-byte"), 4096)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/x-matroska")
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Header.Get("Range") == "bytes=0-0" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(payload)))
			w.Header().Set("Content-Length", "1")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(payload[:1])
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		_, _ = w.Write(payload)
	}))
	t.Cleanup(origin.Close)

	ctx, cancel := context.WithCancel(context.Background())
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "rx93.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	st := store.New(db)
	t.Cleanup(func() { _ = db.Close() })

	dataRoot := t.TempDir()
	writer := output.NewStrmWriter(filepath.Join(dataRoot, "strm"), filepath.Join(dataRoot, ".staging"))
	writer.SetStreamSecret(mediaFlowTestSecret)
	writer.SetBlobSink(st, dataRoot)

	const filename = "First.Seen.S01E02.2026.1080p.mkv"
	key := httpstream.ResolveKey{
		Version: httpstream.ResolveKeyVersion, BackendID: "aio", Handler: "stremio",
		Kind: "episode", IDs: map[string]string{"imdb": "tt1234567"}, Selector: "file:" + filename + "#sz:" + fmt.Sprint(len(payload)),
		Season: 1, Episode: 2,
	}
	handler := &rx93TestHandler{results: results(key, filename, int64(len(payload)))}
	registry := httpstream.NewRegistry()
	registry.Register("aio", 0, handler)

	s := newMediaFlowTestServer(origin.Client())
	s.rx95 = newRX95Store()
	handler.resolved = []httpstream.ResolvedFile{{
		Selector: key.Selector, Name: filename, Size: int64(len(payload)),
		URL: generateMediaFlowURL(t, s, mediaFlowURLRequest{
			DestinationURL: origin.URL,
			Filename:       filename,
		}),
		SupportsRange: true,
	}}
	s.cfg.Data.Root = dataRoot
	s.cfg.HTTPStream.Enabled = true
	s.cfg.Reactive.Enabled = true
	s.cfg.Reactive.Threshold = 0.5
	s.store = st
	s.writer = writer
	s.httpHandlers = registry
	s.activeGoroutines = make(map[string]struct{})
	s.shutdownCtx, s.shutdownCancel = ctx, cancel
	s.rx93CaptureSem = make(chan struct{}, rx93MaxConcurrent)
	s.rx93Selectors = newRX93SelectorStore()
	tracker, err := playbackcoverage.New(st, playbackcoverage.Options{
		ProposalEnabled: true, ProposalThreshold: s.cfg.Reactive.Threshold,
	})
	if err != nil {
		t.Fatal(err)
	}
	s.playbackCoverage = tracker
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer shutdownCancel()
		if err := s.Shutdown(shutdownCtx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	return &rx93Fixture{server: s, store: st, handler: handler, origin: origin, payload: payload, key: key}
}

func (f *rx93Fixture) play(t *testing.T, bind bool) mediaFlowTicket {
	t.Helper()
	if bind {
		f.server.rx95.Observe(rx95Identity{Kind: "series", IMDb: "tt1234567", Season: 1, Episode: 2}, f.server.nowUTC())
	}
	generated := generateMediaFlowURL(t, f.server, mediaFlowURLRequest{
		DestinationURL: f.origin.URL,
		Filename:       "First.Seen.S01E02.2026.1080p.mkv",
	})
	u, err := http.NewRequest(http.MethodGet, generated, nil)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := openMediaFlowTicket(mediaFlowTestSecret, u.URL.Query().Get("d"))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	f.server.handleMediaFlowStream(rec, httptest.NewRequest(http.MethodGet, u.URL.RequestURI(), nil))
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), f.payload) {
		t.Fatalf("playback status=%d bytes=%d", rec.Code, rec.Body.Len())
	}
	return ticket
}

func waitRX93Proposal(t *testing.T, f *rx93Fixture, representationID string) playbackcoverage.Proposal {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		proposal, found, err := f.store.GetPlaybackProposal(context.Background(), representationID)
		if err != nil {
			t.Fatal(err)
		}
		if found {
			return proposal
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("RX-9.3 proposal did not appear")
	return playbackcoverage.Proposal{}
}

func waitRX93Idle(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(s.rx93CaptureSem) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("RX-9.3 capture did not become idle")
}

func TestRX93FirstSeenAggregatorPlaybackCapturesReadyItemAndProposalOnce(t *testing.T) {
	f := newRX93Fixture(t, func(key httpstream.ResolveKey, filename string, size int64) []httpstream.SearchResult {
		return []httpstream.SearchResult{{Title: "First.Seen.S01E02.2026.1080p", Filename: filename, Size: size, Key: key}}
	})
	ticket := f.play(t, true)
	if ticket.Identity == nil || ticket.RepresentationID != rx95RepresentationID(*ticket.Identity) {
		t.Fatalf("bound provider identity missing=%v representation_match=%v", ticket.Identity == nil,
			ticket.Identity != nil && ticket.RepresentationID == rx95RepresentationID(*ticket.Identity))
	}
	proposal := waitRX93Proposal(t, f, ticket.RepresentationID)
	item, err := f.store.GetItemByID(context.Background(), proposal.ItemID)
	if err != nil || item == nil || item.State != store.StateReady || item.SourceType != store.SourceTypeHTTP {
		t.Fatalf("captured item ready=%v err=%v", item != nil && item.State == store.StateReady, err)
	}
	if item.Metadata.ProviderIdentity == nil || item.Metadata.ProviderIdentity.Kind != "series" ||
		item.Metadata.ProviderIdentity.IDs.IMDB != "tt1234567" || len(item.Metadata.ProviderIdentity.Episodes) != 1 ||
		item.Metadata.ProviderIdentity.Episodes[0].Season != 1 || item.Metadata.ProviderIdentity.Episodes[0].Episode != 2 {
		t.Fatalf("captured provider identity = %+v", item.Metadata.ProviderIdentity)
	}
	canonical, _ := f.key.Canonical()
	if item.ResolveKey == nil || *item.ResolveKey != canonical {
		t.Fatal("Search ResolveKey was not persisted verbatim")
	}
	blobs, err := f.store.GetStrmBlobs(context.Background(), item.ID)
	if err != nil || len(blobs) != 1 || strings.Contains(blobs[0].URL, f.origin.URL) || !strings.Contains(blobs[0].URL, "/stream/") {
		t.Fatalf("captured blobs=%d err=%v", len(blobs), err)
	}
	if proposal.Total != int64(len(f.payload)) || proposal.ExtentKind != playbackcoverage.ExtentDeclaredBytes {
		t.Fatalf("proposal = %+v", proposal)
	}

	f.play(t, true)
	waitRX93Idle(t, f.server)
	items, err := f.store.ListReadyHTTPItems(context.Background(), 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("ready HTTP items=%d err=%v", len(items), err)
	}
	if f.handler.searchCalls.Load() != 1 || f.handler.resolveCalls.Load() != 1 {
		t.Fatalf("Search/Resolve calls=%d/%d", f.handler.searchCalls.Load(), f.handler.resolveCalls.Load())
	}
}

func TestRX93RetainsExactSelectorBeforeThresholdWhenLaterSearchDisappears(t *testing.T) {
	f := newRX93Fixture(t, func(key httpstream.ResolveKey, filename string, size int64) []httpstream.SearchResult {
		return []httpstream.SearchResult{{Title: "First.Seen.S01E02.2026.1080p", Filename: filename, Size: size, Key: key}}
	})
	representationID := rx95RepresentationID(rx95Identity{Kind: "series", IMDb: "tt1234567", Season: 1, Episode: 2})
	match := append([]httpstream.SearchResult(nil), f.handler.results...)
	f.handler.searchFn = func() []httpstream.SearchResult {
		snapshot, found, err := f.store.GetPlaybackCoverage(context.Background(), representationID, 64)
		if err != nil {
			t.Fatalf("GetPlaybackCoverage: %v", err)
		}
		if found && snapshot.DeliveredBytes > 0 {
			return nil
		}
		return match
	}

	ticket := f.play(t, true)
	proposal := waitRX93Proposal(t, f, ticket.RepresentationID)
	if proposal.Total != int64(len(f.payload)) {
		t.Fatalf("proposal total=%d want=%d", proposal.Total, len(f.payload))
	}
	if f.handler.searchCalls.Load() != 1 {
		t.Fatalf("Search calls=%d want=1 retained pre-threshold selector", f.handler.searchCalls.Load())
	}
}

func TestRX93AbsentIdentityAndSearchMismatchAbstain(t *testing.T) {
	t.Run("absent identity", func(t *testing.T) {
		f := newRX93Fixture(t, func(key httpstream.ResolveKey, filename string, size int64) []httpstream.SearchResult {
			return []httpstream.SearchResult{{Title: "match", Filename: filename, Size: size, Key: key}}
		})
		ticket := f.play(t, false)
		if ticket.Identity != nil {
			t.Fatal("unbound batch carried an identity")
		}
		if f.handler.searchCalls.Load() != 0 {
			t.Fatal("identity-less playback searched a backend")
		}
	})

	for _, test := range []struct {
		name    string
		results func(httpstream.ResolveKey, string, int64) []httpstream.SearchResult
	}{
		{"no filename-size match", func(key httpstream.ResolveKey, filename string, size int64) []httpstream.SearchResult {
			return []httpstream.SearchResult{{Title: "wrong", Filename: filename + ".other", Size: size, Key: key}}
		}},
		{"ambiguous filename-size match", func(key httpstream.ResolveKey, filename string, size int64) []httpstream.SearchResult {
			other := key
			other.Selector = key.Selector + "#other"
			return []httpstream.SearchResult{
				{Title: "one", Filename: filename, Size: size, Key: key},
				{Title: "two", Filename: filename, Size: size, Key: other},
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRX93Fixture(t, test.results)
			f.play(t, true)
			waitRX93Idle(t, f.server)
			items, err := f.store.ListReadyHTTPItems(context.Background(), 10)
			if err != nil || len(items) != 0 {
				t.Fatalf("capture did not abstain: items=%d err=%v", len(items), err)
			}
		})
	}

	identity := rx95Identity{Kind: "movie", IMDb: "not-an-imdb-id"}
	if _, err := normalizeMediaFlowTicket(mediaFlowTicket{
		Version: mediaFlowTicketVersion, RepresentationID: "mediaflow:valid", Identity: &identity,
		DestinationURL: "https://media.invalid/file.mkv",
	}); err == nil {
		t.Fatal("malformed provider identity entered a ticket")
	}
}
