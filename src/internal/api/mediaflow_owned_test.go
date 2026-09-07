package api

import (
	"bytes"
	"context"
	"crypto/sha1" // #nosec G505 -- test fixture uses BitTorrent v1 piece hashes.
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/contentproof"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
	"github.com/darkharrbor/darkharrbor/internal/store"
	"github.com/darkharrbor/darkharrbor/internal/torrentmeta"
)

func mediaFlowInlineDescriptor(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

func mediaFlowOwnedURL(base, descriptor string) string {
	return strings.TrimRight(base, "/") + mediaFlowAIOPlaybackPrefix + "store-auth/-/" + descriptor + "/tt0000001/movie.mkv"
}

// TestMediaFlowIdentityRepresentationIDIsStableAndOpaque is RX-9.1's
// regression. Before RD-30 D5 the MediaFlow representation ID was 24 random
// bytes, so two tickets for the SAME episode produced different
// representations and playback_coverage could never accumulate across
// sessions -- the threshold could not fire for a movie watched in two
// sittings. The ID must now be stable per identity, distinct across
// identities, opaque, and derivable for EVERY descriptor type.
func TestMediaFlowIdentityRepresentationIDIsStableAndOpaque(t *testing.T) {
	base := "https" + "://aio.example.test/config"
	backends := []config.HTTPBackend{{
		Name: "aio", URL: base, PublicURL: "https" + "://aio-public.example.test", Type: "stremio",
	}}
	descriptor := mediaFlowInlineDescriptor(t, map[string]any{
		"type": "torrent", "hash": "0123456789abcdef", "sources": []string{}, "title": "Exact.Release.2026",
	})
	prefix := strings.TrimRight("https"+"://aio.example.test", "/") + mediaFlowAIOPlaybackPrefix

	// Two separately issued routes for the same episode, differing in the
	// parts that must NOT affect identity: store auth and filename.
	first := prefix + "store-auth-A/-/" + descriptor + "/tt0070991:3:17/episode.mkv"
	second := prefix + "store-auth-B/-/" + descriptor + "/tt0070991:3:17/different.name.mkv"

	idA, err := mediaFlowStremioIdentityFromRoute(first, backends)
	if err != nil {
		t.Fatalf("first route rejected: %v", err)
	}
	idB, err := mediaFlowStremioIdentityFromRoute(second, backends)
	if err != nil {
		t.Fatalf("second route rejected: %v", err)
	}
	repA, repB := mediaFlowIdentityRepresentationID(idA), mediaFlowIdentityRepresentationID(idB)
	if repA != repB {
		t.Fatalf("same episode must share one representation: %q vs %q", repA, repB)
	}
	if err := playbackcoverage.ValidateRepresentationID(repA); err != nil {
		t.Fatalf("representation ID must satisfy coverage validation: %v", err)
	}
	// Opaque: no plaintext identity may appear anywhere in the ID.
	if strings.Contains(repA, "tt0070991") || strings.Contains(repA, ":3:") {
		t.Fatalf("representation ID leaks plaintext identity: %q", repA)
	}

	// A different episode, a different season, and the movie form must all
	// be distinct from each other and from the episode above.
	distinct := map[string]string{"episode": repA}
	for name, coordinate := range map[string]string{
		"other-episode": "tt0070991:3:18",
		"other-season":  "tt0070991:4:17",
		"movie":         "tt0070991",
	} {
		id, idErr := mediaFlowStremioIdentityFromRoute(prefix+"store-auth/-/"+descriptor+"/"+coordinate+"/x.mkv", backends)
		if idErr != nil {
			t.Fatalf("%s route rejected: %v", name, idErr)
		}
		rep := mediaFlowIdentityRepresentationID(id)
		for existing, value := range distinct {
			if value == rep {
				t.Fatalf("%s collided with %s: %q", name, existing, rep)
			}
		}
		distinct[name] = rep
	}

	// An off-origin route must not yield an identity: the origin check is
	// what stops a caller choosing another identity's representation.
	if _, err := mediaFlowStremioIdentityFromRoute(prefix2Foreign(descriptor), backends); err == nil {
		t.Fatal("off-origin route must not yield an identity")
	}
}

func prefix2Foreign(descriptor string) string {
	return "https" + "://attacker.example.test" + mediaFlowAIOPlaybackPrefix +
		"store-auth/-/" + descriptor + "/tt0070991:3:17/x.mkv"
}

func TestMediaFlowOwnedAIORouteCapabilityFailsClosed(t *testing.T) {
	base := "https" + "://aio.example.test/config"
	backends := []config.HTTPBackend{{
		Name: "aio", URL: base, PublicURL: "https" + "://aio-public.example.test", Type: "stremio",
	}}
	torrent := mediaFlowInlineDescriptor(t, map[string]any{
		"type": "torrent", "hash": "0123456789abcdef", "sources": []string{}, "title": "Exact.Release.2026",
	})
	usenet := mediaFlowInlineDescriptor(t, map[string]any{
		"type": "usenet", "hash": "abcdef0123456789", "nzb": "inline-document", "title": "Exact.Release.2026",
	})
	libraryUsenet := mediaFlowInlineDescriptor(t, map[string]any{
		"type": "usenet", "hash": "", "nzb": "", "serviceItemId": "library-item", "title": "Exact.Release.2026",
	})
	for _, raw := range []string{
		mediaFlowOwnedURL("https"+"://aio.example.test", torrent),
		mediaFlowOwnedURL("https"+"://aio.example.test:443", usenet),
		mediaFlowOwnedURL("https"+"://aio.example.test", libraryUsenet),
		mediaFlowOwnedURL("https"+"://aio-public.example.test", torrent),
	} {
		descriptor, err := mediaFlowOwnedAIORoute(raw, backends)
		if err != nil || descriptor.Title != "Exact.Release.2026" || (descriptor.Kind != "torrent" && descriptor.Kind != "usenet") {
			t.Fatalf("valid owned route rejected: kind=%q err=%v", descriptor.Kind, err)
		}
	}

	malformed := mediaFlowInlineDescriptor(t, map[string]any{"type": "torrent", "hash": "x"})
	for name, raw := range map[string]string{
		"cache key":    mediaFlowOwnedURL("https"+"://aio.example.test", strings.Repeat("a", 64)),
		"external":     "https" + "://cdn.example.test/movie.mkv",
		"cross origin": mediaFlowOwnedURL("https"+"://other.example.test", torrent),
		"query":        mediaFlowOwnedURL("https"+"://aio.example.test", torrent) + "?x=1",
		"fragment":     mediaFlowOwnedURL("https"+"://aio.example.test", torrent) + "#x",
		"extra segment": mediaFlowOwnedURL("https"+"://aio.example.test", torrent) +
			"/extra",
		"encoded slash": strings.Replace(mediaFlowOwnedURL("https"+"://aio.example.test", torrent), "/tt0000001/", "/tt0000001%2Fescape/", 1),
		"incomplete":    mediaFlowOwnedURL("https"+"://aio.example.test", malformed),
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := mediaFlowOwnedAIORoute(raw, backends); err == nil {
				t.Fatalf("unsafe route accepted: %+v", got)
			}
		})
	}
	if _, err := mediaFlowOwnedAIORoute(mediaFlowOwnedURL("https"+"://aio.example.test", torrent),
		[]config.HTTPBackend{{Name: "not-aio", URL: base, Type: "generic"}}); err == nil {
		t.Fatal("non-Stremio backend accepted as an owned origin")
	}
}

func FuzzMediaFlowOwnedAIORoute(f *testing.F) {
	valid := base64.RawURLEncoding.EncodeToString([]byte(`{"type":"torrent","hash":"0123456789abcdef","sources":[],"title":"release"}`))
	f.Add(mediaFlowOwnedURL("https"+"://aio.example.test", valid))
	f.Add(mediaFlowOwnedURL("https"+"://aio.example.test", strings.Repeat("a", 64)))
	f.Add("not-a-route")
	backends := []config.HTTPBackend{{Name: "aio", URL: "https" + "://aio.example.test/config", Type: "stremio"}}
	f.Fuzz(func(t *testing.T, raw string) {
		descriptor, err := mediaFlowOwnedAIORoute(raw, backends)
		if err == nil && descriptor.Kind != "torrent" && descriptor.Kind != "usenet" {
			t.Fatalf("accepted unsupported descriptor kind %q", descriptor.Kind)
		}
	})
}

type mediaFlowOwnedHTTPHandler struct {
	url      string
	selector string
	size     int64
	calls    atomic.Int32
}

func (h *mediaFlowOwnedHTTPHandler) Name() string { return "generic" }
func (h *mediaFlowOwnedHTTPHandler) Search(context.Context, httpstream.StreamQuery) ([]httpstream.SearchResult, error) {
	return nil, nil
}
func (h *mediaFlowOwnedHTTPHandler) Resolve(context.Context, httpstream.ResolveRequest) ([]httpstream.ResolvedFile, error) {
	h.calls.Add(1)
	return []httpstream.ResolvedFile{{
		Selector: h.selector, Name: "movie.mkv", Size: h.size, URL: h.url, SupportsRange: true,
	}}, nil
}

func mediaFlowRangeOrigin(body []byte, calls *atomic.Int32) *httptest.Server {
	return mediaFlowRangeOriginFunc(func() []byte { return body }, calls)
}

func mediaFlowRangeOriginFunc(body func() []byte, calls *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body := body()
		if r.Header.Get("Range") == "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				_, _ = w.Write(body)
			}
			return
		}
		requested, err := parseMediaFlowRange(r.Header.Get("Range"))
		if err != nil {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		start, length, status, ok := mediaFlowRangeBounds(&requested, int64(len(body)))
		if !ok || status == http.StatusRequestedRangeNotSatisfiable {
			w.Header().Set("Content-Range", "bytes */"+strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+length-1, len(body)))
		w.WriteHeader(http.StatusPartialContent)
		if r.Method != http.MethodHead {
			_, _ = w.Write(body[start : start+length])
		}
	}))
}

type mediaFlowOwnedFixture struct {
	server        *Server
	store         *store.Store
	graph         *contentproof.Graph
	converger     *contentproof.Converger
	registry      *httpstream.Registry
	nativeOrigin  *httptest.Server
	nativeCalls   atomic.Int32
	nativeCorrupt atomic.Bool
	nativeRoute   contentproof.LaneCandidate
	ownedFilename string
	now           time.Time
}

func newMediaFlowOwnedFixture(t *testing.T, client *http.Client, body []byte) *mediaFlowOwnedFixture {
	t.Helper()
	ctx := context.Background()
	now := time.Unix(2300, 0).UTC()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "mediaflow-owned.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	st := store.New(db)
	st.SetClock(func() time.Time { return now })
	graph, err := contentproof.New(st, contentproof.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	converger, err := contentproof.NewConverger(st, contentproof.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	fixture := &mediaFlowOwnedFixture{store: st, graph: graph, converger: converger, now: now, ownedFilename: "Exact.Release.2026"}
	corruptBody := append([]byte(nil), body...)
	corruptBody[0] ^= 0xff
	fixture.nativeOrigin = mediaFlowRangeOriginFunc(func() []byte {
		if fixture.nativeCorrupt.Load() {
			return corruptBody
		}
		return body
	}, &fixture.nativeCalls)
	t.Cleanup(fixture.nativeOrigin.Close)
	key := httpstream.ResolveKey{
		Version: httpstream.ResolveKeyVersion, BackendID: "native", Handler: "generic",
		Kind: "movie", IDs: map[string]string{"test": "one"}, Selector: "movie",
	}
	canonicalKey, err := key.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	file := httpstream.PersistedFile{FileID: "hf000000000023", Selector: key.Selector, Name: "movie.mkv", Size: int64(len(body)), RangeVerified: true}
	files, err := httpstream.MarshalFiles([]httpstream.PersistedFile{file})
	if err != nil {
		t.Fatal(err)
	}
	item := &store.Item{
		ID: "native-http", PublicID: "native-http", SourceType: store.SourceTypeHTTP, ClientKind: store.ClientKindQBit,
		Category: "movies", State: store.StateReady, SubmissionKey: "native-http", DisplayName: fixture.ownedFilename,
		FileList: &files, ResolveKey: &canonicalKey, TotalSize: int64(len(body)), CreatedAt: now, UpdatedAt: now,
		Metadata: store.SubmissionMetadata{ProviderIdentity: &store.ProviderIdentity{Kind: "movie", IDs: store.ProviderIDs{IMDB: "tt0000001"}}},
	}
	if err := st.CreateItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	fixture.nativeRoute, err = contentproof.NewLaneCandidate(contentproof.LaneHTTP, item.ID, file.FileID,
		httpRepresentationID(item.ID, file.FileID), item.DisplayName, file.Size)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	if err := graph.Record(ctx, contentproof.Evidence{
		RepresentationID: fixture.nativeRoute.RepresentationID, Scope: contentproof.ScopeWhole, Offset: 0, Length: int64(len(body)),
		Kind: contentproof.KindAuthoritative, Algorithm: contentproof.AlgorithmSHA256, Digest: digest[:],
		Provenance: contentproof.ProvenanceHTTPDigest, ObservedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	registry := httpstream.NewRegistry()
	registry.Register(key.BackendID, 0, &mediaFlowOwnedHTTPHandler{url: fixture.nativeOrigin.URL, selector: key.Selector, size: int64(len(body))})
	fixture.registry = registry

	srv := newMediaFlowTestServer(client)
	srv.store = st
	srv.httpProofGraph = graph
	srv.representationConvergence = converger
	srv.httpHandlers = registry
	srv.httpResolveCoord = newHTTPResolveCoordinator()
	srv.cfg.NNTP.CrossLaneSplice = true
	srv.nowFn = func() time.Time { return now }
	fixture.server = srv
	return fixture
}

func (f *mediaFlowOwnedFixture) attach(server *Server, backendURL string) {
	server.store = f.store
	server.httpProofGraph = f.graph
	server.representationConvergence = f.converger
	server.httpHandlers = f.registry
	server.httpResolveCoord = newHTTPResolveCoordinator()
	server.cfg.NNTP.CrossLaneSplice = true
	server.cfg.HTTPStream.Backends = []config.HTTPBackend{{Name: "aio", URL: backendURL, Type: "stremio"}}
	server.nowFn = func() time.Time { return f.now }
}

func requestMediaFlowRange(generated string, server *Server, end int) (*httptest.ResponseRecorder, error) {
	return requestMediaFlowByteRange(generated, server, 0, end)
}

func requestMediaFlowByteRange(generated string, server *Server, start, end int) (*httptest.ResponseRecorder, error) {
	u, err := url.Parse(generated)
	if err != nil {
		return nil, err
	}
	req := httptest.NewRequest(http.MethodGet, u.RequestURI(), nil)
	req.Header.Set("Range", "bytes="+strconv.Itoa(start)+"-"+strconv.Itoa(end))
	rec := httptest.NewRecorder()
	server.handleMediaFlowStream(rec, req)
	return rec, nil
}

func ticketFromGenerated(generated string) (mediaFlowTicket, error) {
	u, err := url.Parse(generated)
	if err != nil {
		return mediaFlowTicket{}, err
	}
	return openMediaFlowTicket(mediaFlowTestSecret, u.Query().Get("d"))
}

func TestMediaFlowOwnedRouteReresolvesAdmitsRestartsAndFallsBack(t *testing.T) {
	body := bytes.Repeat([]byte("proof-bytes-"), 128)
	var firstCalls, secondCalls, aioCalls atomic.Int32
	first := mediaFlowRangeOrigin(body, &firstCalls)
	defer first.Close()
	second := mediaFlowRangeOrigin(body, &secondCalls)
	defer second.Close()
	var broken atomic.Bool
	aio := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := aioCalls.Add(1)
		if broken.Load() {
			http.Error(w, "unavailable", http.StatusBadGateway)
			return
		}
		destination := first.URL
		if call%2 == 0 {
			destination = second.URL
		}
		http.Redirect(w, r, destination, http.StatusFound)
	}))
	defer aio.Close()

	fixture := newMediaFlowOwnedFixture(t, aio.Client(), body)
	fixture.attach(fixture.server, aio.URL)
	inline := mediaFlowInlineDescriptor(t, map[string]any{
		"type": "torrent", "hash": "0123456789abcdef", "sources": []string{}, "title": fixture.ownedFilename,
	})
	generated := generateMediaFlowURL(t, fixture.server, mediaFlowURLRequest{
		DestinationURL: mediaFlowOwnedURL(aio.URL, inline), Filename: "movie.mkv",
	})
	ticket, err := ticketFromGenerated(generated)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mediaFlowOwnedAIORoute(ticket.DestinationURL, fixture.server.cfg.HTTPStream.Backends); err != nil {
		t.Fatalf("generated owned route was not recognized: %v", err)
	}
	handles, err := fixture.server.resolveMediaFlowProofCandidates(context.Background(), fixture.ownedFilename, int64(len(body)))
	if err != nil || len(handles) != 1 {
		t.Fatalf("native proof candidates=%d err=%v", len(handles), err)
	}
	availableProofs, err := fixture.store.ListContentProofsForWindow(context.Background(), []string{handles[0].RepresentationID}, fixture.now, 0, int64(len(body)), mediaFlowMaxProofsPerRequest)
	if err != nil || len(availableProofs) != 1 {
		t.Fatalf("native window proofs=%d err=%v", len(availableProofs), err)
	}

	firstResponse, err := requestMediaFlowRange(generated, fixture.server, len(body)-1)
	if err != nil || firstResponse.Code != http.StatusPartialContent || !bytes.Equal(firstResponse.Body.Bytes(), body) {
		t.Fatalf("first owned playback status=%d bytes=%d err=%v", firstResponse.Code, firstResponse.Body.Len(), err)
	}
	canonical, size, found, err := fixture.server.mediaFlowCanonical(context.Background(), ticket.RepresentationID)
	if err != nil || !found || canonical == "" || size != int64(len(body)) {
		aggregateProofs, proofErr := fixture.store.ListContentProofs(context.Background(), []string{ticket.RepresentationID}, fixture.now)
		t.Fatalf("canonical admission = %q size=%d found=%v err=%v aggregate_proofs=%d proof_err=%v", canonical, size, found, err, len(aggregateProofs), proofErr)
	}
	routes, err := fixture.converger.Routes(context.Background(), ticket.RepresentationID)
	if err != nil || len(routes) != 2 {
		t.Fatalf("canonical routes=%d err=%v", len(routes), err)
	}
	for _, route := range routes {
		encoded, _ := json.Marshal(route)
		if bytes.Contains(encoded, []byte("://")) || route.Size != int64(len(body)) {
			t.Fatal("durable route is not bounded and URL-free")
		}
	}

	secondResponse, err := requestMediaFlowRange(generated, fixture.server, len(body)-1)
	if err != nil || secondResponse.Code != http.StatusPartialContent || !bytes.Equal(secondResponse.Body.Bytes(), body) || aioCalls.Load() != 2 || firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Fatalf("same ticket did not re-enter owned resolver: status=%d aio=%d first=%d second=%d err=%v",
			secondResponse.Code, aioCalls.Load(), firstCalls.Load(), secondCalls.Load(), err)
	}

	graphAfterRestart, err := contentproof.New(fixture.store, contentproof.Options{Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatal(err)
	}
	convergerAfterRestart, err := contentproof.NewConverger(fixture.store, contentproof.Options{Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatal(err)
	}
	fixture.graph = graphAfterRestart
	fixture.converger = convergerAfterRestart
	restarted := newMediaFlowTestServer(aio.Client())
	fixture.attach(restarted, aio.URL)
	restartResponse, err := requestMediaFlowRange(generated, restarted, len(body)-1)
	if err != nil || restartResponse.Code != http.StatusPartialContent || !bytes.Equal(restartResponse.Body.Bytes(), body) || aioCalls.Load() != 3 {
		t.Fatalf("restart playback status=%d bytes=%d aio=%d err=%v", restartResponse.Code, restartResponse.Body.Len(), aioCalls.Load(), err)
	}

	broken.Store(true)
	const fallbackStart, fallbackEnd = 37, 413
	fallbackResponse, err := requestMediaFlowByteRange(generated, restarted, fallbackStart, fallbackEnd)
	if err != nil || fallbackResponse.Code != http.StatusPartialContent || !bytes.Equal(fallbackResponse.Body.Bytes(), body[fallbackStart:fallbackEnd+1]) || aioCalls.Load() != 4 || fixture.nativeCalls.Load() == 0 {
		t.Fatalf("canonical fallback status=%d bytes=%d aio=%d native=%d err=%v",
			fallbackResponse.Code, fallbackResponse.Body.Len(), aioCalls.Load(), fixture.nativeCalls.Load(), err)
	}
	fixture.nativeCorrupt.Store(true)
	corruptResponse, err := requestMediaFlowByteRange(generated, restarted, fallbackStart, fallbackEnd)
	if err != nil || corruptResponse.Code != http.StatusBadGateway || bytes.Equal(corruptResponse.Body.Bytes(), body) || aioCalls.Load() != 5 {
		t.Fatalf("corrupt canonical peer did not fail closed: status=%d bytes=%d aio=%d err=%v",
			corruptResponse.Code, corruptResponse.Body.Len(), aioCalls.Load(), err)
	}
}

func TestMediaFlowOwnedAdmissionDerivesStoredTorrentProofsAndRejectsPartialFileDomain(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(2400, 0).UTC()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "mediaflow-torrent-proof.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	st := store.New(db)
	st.SetClock(func() time.Time { return now })
	graph, err := contentproof.New(st, contentproof.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	converger, err := contentproof.NewConverger(st, contentproof.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	srv := newMediaFlowTestServer(http.DefaultClient)
	srv.store = st
	srv.httpProofGraph = graph
	srv.representationConvergence = converger
	srv.cfg.NNTP.CrossLaneSplice = true
	srv.nowFn = func() time.Time { return now }

	createTorrent := func(id, hash, release string, body, torrentBody []byte, file torrentmeta.FileEntry) contentproof.LaneCandidate {
		t.Helper()
		files := fmt.Sprintf(`[{"file_id":"0","name":"movie.mkv","size":%d}]`, len(body))
		item := &store.Item{
			ID: id, PublicID: id, SourceType: store.SourceTypeTorrent, ClientKind: store.ClientKindQBit,
			Category: "movies", State: store.StateReady, SubmissionKey: id, DisplayName: release,
			InfoHash: &hash, FileList: &files, TotalSize: int64(len(torrentBody)), CreatedAt: now, UpdatedAt: now,
		}
		if err := st.CreateItem(ctx, item); err != nil {
			t.Fatal(err)
		}
		pieceLength := int64(4)
		var hashes []byte
		for offset := 0; offset < len(torrentBody); offset += int(pieceLength) {
			end := offset + int(pieceLength)
			if end > len(torrentBody) {
				end = len(torrentBody)
			}
			digest := sha1.Sum(torrentBody[offset:end])
			hashes = append(hashes, digest[:]...)
		}
		meta := &torrentmeta.TorrentMeta{
			Name: release, InfoHashV1: hash, MetaVersion: 1, PieceLength: pieceLength,
			Files: []torrentmeta.FileEntry{file}, PieceHashesV1: hashes,
		}
		if len(torrentBody) != len(body) {
			meta.Files = []torrentmeta.FileEntry{
				{Path: "prefix.bin", Size: file.StartOffset, StartOffset: 0, EndOffset: file.StartOffset},
				file,
			}
		}
		if err := st.UpsertTorrentMeta(ctx, meta); err != nil {
			t.Fatal(err)
		}
		handle, err := contentproof.NewLaneCandidate(contentproof.LaneTorrent, item.ID, "0",
			torrentCrossLaneRepresentationID(item.ID, "0"), item.DisplayName, int64(len(body)))
		if err != nil {
			t.Fatal(err)
		}
		return handle
	}

	full := []byte("abcdefgh")
	fullHash := strings.Repeat("1", 40)
	fullHandle := createTorrent("torrent-full", fullHash, "Full.Release", full, full,
		torrentmeta.FileEntry{Path: "movie.mkv", Size: 8, StartOffset: 0, EndOffset: 8})
	fullTicket := mediaFlowTicket{RepresentationID: "mediaflow:full", Filename: "Full.Release"}
	admitted, err := srv.admitMediaFlowOwnedWindow(ctx, fullTicket,
		mediaFlowOwnedDescriptor{Kind: "torrent", Title: "Full.Release", Hash: fullHash},
		mediaFlowGeometry{Length: 8, Total: 8}, full)
	if err != nil || !admitted {
		t.Fatalf("stored torrent proof admission=%v err=%v", admitted, err)
	}
	proofs, err := st.ListContentProofs(ctx, []string{fullHandle.RepresentationID}, now)
	if err != nil || len(proofs) != 2 {
		t.Fatalf("derived native proofs=%d err=%v", len(proofs), err)
	}
	routes, err := converger.Routes(ctx, fullTicket.RepresentationID)
	if err != nil || len(routes) != 2 {
		t.Fatalf("derived proof routes=%d err=%v", len(routes), err)
	}

	partial := []byte("abcdefgh")
	partialTorrent := append([]byte("x"), partial...)
	partialHash := strings.Repeat("2", 40)
	partialHandle := createTorrent("torrent-partial", partialHash, "Partial.Release", partial, partialTorrent,
		torrentmeta.FileEntry{Path: "movie.mkv", Size: 8, StartOffset: 1, EndOffset: 9})
	partialTicket := mediaFlowTicket{RepresentationID: "mediaflow:partial", Filename: "Partial.Release"}
	admitted, err = srv.admitMediaFlowOwnedWindow(ctx, partialTicket,
		mediaFlowOwnedDescriptor{Kind: "torrent", Title: "Partial.Release", Hash: partialHash},
		mediaFlowGeometry{Length: 8, Total: 8}, partial)
	if err != nil || admitted {
		t.Fatalf("partial-file proof admission=%v err=%v", admitted, err)
	}
	proofs, err = st.ListContentProofs(ctx, []string{partialHandle.RepresentationID}, now)
	if err != nil || len(proofs) != 0 {
		t.Fatalf("partial-file domain gained proofs=%d err=%v", len(proofs), err)
	}
	if _, found, err := converger.CanonicalRepresentationID(ctx, partialTicket.RepresentationID); err != nil || found {
		t.Fatalf("partial-file domain canonical=%v err=%v", found, err)
	}
}

func TestMediaFlowCacheExternalMalformedAndCanceledNeverAdmit(t *testing.T) {
	body := bytes.Repeat([]byte("bounded"), 32)
	var originCalls atomic.Int32
	origin := mediaFlowRangeOrigin(body, &originCalls)
	defer origin.Close()
	aio := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, origin.URL, http.StatusFound)
	}))
	defer aio.Close()
	fixture := newMediaFlowOwnedFixture(t, aio.Client(), body)
	fixture.attach(fixture.server, aio.URL)
	validInline := mediaFlowInlineDescriptor(t, map[string]any{
		"type": "torrent", "hash": "0123456789abcdef", "sources": []string{}, "title": fixture.ownedFilename,
	})
	malformedInline := mediaFlowInlineDescriptor(t, map[string]any{"type": "torrent", "hash": "x"})
	for name, destination := range map[string]string{
		"cache":        mediaFlowOwnedURL(aio.URL, strings.Repeat("a", 64)),
		"external":     origin.URL,
		"malformed":    mediaFlowOwnedURL(aio.URL, malformedInline),
		"cross-origin": mediaFlowOwnedURL(origin.URL, validInline),
	} {
		t.Run(name, func(t *testing.T) {
			generated := generateMediaFlowURL(t, fixture.server, mediaFlowURLRequest{DestinationURL: destination, Filename: "movie.mkv"})
			ticket, err := ticketFromGenerated(generated)
			if err != nil {
				t.Fatal(err)
			}
			response, err := requestMediaFlowRange(generated, fixture.server, len(body)-1)
			if err != nil || response.Code != http.StatusPartialContent || !bytes.Equal(response.Body.Bytes(), body) {
				t.Fatalf("ephemeral playback status=%d bytes=%d err=%v", response.Code, response.Body.Len(), err)
			}
			if _, found, err := fixture.converger.CanonicalRepresentationID(context.Background(), ticket.RepresentationID); err != nil || found {
				t.Fatalf("ephemeral route gained canonical status: found=%v err=%v", found, err)
			}
			proofs, err := fixture.store.ListContentProofs(context.Background(), []string{ticket.RepresentationID}, fixture.now)
			if err != nil || len(proofs) != 0 {
				t.Fatalf("ephemeral route gained proofs: count=%d err=%v", len(proofs), err)
			}
		})
	}

	canceledTicket := mediaFlowTicket{RepresentationID: "mediaflow:canceled", Filename: fixture.ownedFilename}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := fixture.server.admitMediaFlowOwnedWindow(ctx, canceledTicket,
		mediaFlowOwnedDescriptor{Kind: "torrent", Title: fixture.ownedFilename},
		mediaFlowGeometry{Length: int64(len(body)), Total: int64(len(body))}, body)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled admission error=%v", err)
	}
	proofs, err := fixture.store.ListContentProofs(context.Background(), []string{canceledTicket.RepresentationID}, fixture.now)
	if err != nil || len(proofs) != 0 {
		t.Fatalf("canceled route gained proofs: count=%d err=%v", len(proofs), err)
	}
}

func TestMediaFlowOwnedAdmissionIsConcurrentAndProofBounded(t *testing.T) {
	body := bytes.Repeat([]byte("concurrent-proof"), 64)
	var originCalls atomic.Int32
	origin := mediaFlowRangeOrigin(body, &originCalls)
	defer origin.Close()
	aio := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, origin.URL, http.StatusFound)
	}))
	defer aio.Close()
	fixture := newMediaFlowOwnedFixture(t, aio.Client(), body)
	fixture.attach(fixture.server, aio.URL)
	inline := mediaFlowInlineDescriptor(t, map[string]any{
		"type": "torrent", "hash": "0123456789abcdef", "sources": []string{}, "title": fixture.ownedFilename,
	})
	generated := generateMediaFlowURL(t, fixture.server, mediaFlowURLRequest{DestinationURL: mediaFlowOwnedURL(aio.URL, inline), Filename: "movie.mkv"})
	ticket, err := ticketFromGenerated(generated)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response, requestErr := requestMediaFlowRange(generated, fixture.server, len(body)-1)
			if requestErr != nil {
				errs <- requestErr
				return
			}
			if response.Code != http.StatusPartialContent || !bytes.Equal(response.Body.Bytes(), body) {
				errs <- fmt.Errorf("status=%d bytes=%d", response.Code, response.Body.Len())
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if _, _, found, err := fixture.server.mediaFlowCanonical(context.Background(), ticket.RepresentationID); err != nil || !found {
		t.Fatalf("concurrent admission found=%v err=%v", found, err)
	}

	for i := int64(0); i < mediaFlowMaxProofsPerRequest+1; i++ {
		digest := sha256.Sum256([]byte{byte(i)})
		if err := fixture.graph.Record(context.Background(), contentproof.Evidence{
			RepresentationID: "http:proof-limit", Scope: contentproof.ScopeBlock, Offset: i, Length: 1,
			Kind: contentproof.KindAuthoritative, Algorithm: contentproof.AlgorithmSHA256, Digest: digest[:],
			Provenance: contentproof.ProvenanceHTTPDigest, ObservedAt: fixture.now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fixture.store.ListContentProofsForWindow(context.Background(), []string{"http:proof-limit"}, fixture.now,
		0, mediaFlowMaxProofsPerRequest+1, mediaFlowMaxProofsPerRequest); err == nil {
		t.Fatal("proof window limit was not enforced")
	}
}

func TestMediaFlowCanonicalTargetUsesExactStremioIdentity(t *testing.T) {
	body := bytes.Repeat([]byte("identity-proof"), 128)
	origin := mediaFlowRangeOrigin(body, new(atomic.Int32))
	defer origin.Close()
	aio := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, origin.URL, http.StatusFound)
	}))
	defer aio.Close()
	fixture := newMediaFlowOwnedFixture(t, aio.Client(), body)
	fixture.attach(fixture.server, aio.URL)
	tracker, err := playbackcoverage.New(fixture.store, playbackcoverage.Options{
		Now: func() time.Time { return fixture.now }, ProposalEnabled: true, ProposalThreshold: 0.5,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.server.playbackCoverage = tracker
	inline := mediaFlowInlineDescriptor(t, map[string]any{
		"type": "torrent", "hash": "0123456789abcdef", "sources": []string{}, "title": fixture.ownedFilename,
	})
	generated := generateMediaFlowURL(t, fixture.server, mediaFlowURLRequest{
		DestinationURL: mediaFlowOwnedURL(aio.URL, inline), Filename: "movie.mkv",
	})
	ticket, err := ticketFromGenerated(generated)
	if err != nil {
		t.Fatal(err)
	}
	if response, err := requestMediaFlowRange(generated, fixture.server, len(body)-1); err != nil || response.Code != http.StatusPartialContent {
		t.Fatalf("proof playback status=%d err=%v", response.Code, err)
	}
	canonical, _, found, err := fixture.server.mediaFlowCanonical(context.Background(), ticket.RepresentationID)
	if err != nil || !found {
		t.Fatalf("canonical route found=%v err=%v", found, err)
	}
	descriptor, err := mediaFlowOwnedAIORoute(mediaFlowOwnedURL(aio.URL, inline), fixture.server.cfg.HTTPStream.Backends)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := fixture.store.ListRepresentationRoutes(context.Background(), ticket.RepresentationID, contentproof.DefaultMaxRoutesPerRepresentation)
	if err != nil {
		t.Fatal(err)
	}
	if target, ok, err := fixture.server.mediaFlowOwnedProposalTarget(context.Background(), ticket.RepresentationID, descriptor); err != nil || !ok {
		t.Fatalf("canonical target=%+v ok=%v err=%v routes=%+v descriptor=%+v", target, ok, err, routes, descriptor)
	}
	if response, err := requestMediaFlowRange(generated, fixture.server, len(body)-1); err != nil || response.Code != http.StatusPartialContent {
		t.Fatalf("target playback status=%d err=%v", response.Code, err)
	}
	proposal, found, err := fixture.store.GetPlaybackProposal(context.Background(), canonical)
	if err != nil || !found || proposal.ItemID != "native-http" || proposal.FileID != fixture.nativeRoute.FileID {
		t.Fatalf("proposal=%+v found=%v err=%v", proposal, found, err)
	}
	wrong, err := parseMediaFlowStremioIdentity("tt0000002")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := fixture.server.mediaFlowOwnedProposalTarget(context.Background(), ticket.RepresentationID, mediaFlowOwnedDescriptor{Identity: wrong}); err != nil || ok {
		t.Fatalf("wrong IMDb target ok=%v err=%v", ok, err)
	}
	for _, raw := range []string{"tt0000001:1:2", "tt0000001:0:1"} {
		identity, parseErr := parseMediaFlowStremioIdentity(raw)
		if parseErr != nil || !identity.Series {
			t.Fatalf("series identity %q = %+v, %v", raw, identity, parseErr)
		}
	}
	for _, raw := range []string{"Exact.Release", "tt0000001:1", "tt0000001:1:0", "tt0000001:1:2:3"} {
		if _, err := parseMediaFlowStremioIdentity(raw); err == nil {
			t.Fatalf("unsafe identity accepted: %q", raw)
		}
	}
}
