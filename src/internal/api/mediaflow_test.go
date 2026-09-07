package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
	"github.com/darkharrbor/darkharrbor/internal/publicip"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

const (
	mediaFlowTestSecret   = "0123456789abcdef0123456789abcdef"
	mediaFlowTestPassword = "test-mediaflow-password"
)

func newMediaFlowTestServer(client *http.Client) *Server {
	s := httpTestServer(mediaFlowTestSecret, true)
	s.cfg.MediaFlow.Password = mediaFlowTestPassword
	s.cfg.MediaFlow.PublicIP = "192.0.2.1"
	s.mediaFlowSem = make(chan struct{}, mediaFlowMaxConcurrent)
	s.httpGov = accountgov.New("mediaflow-test")
	s.httpGov.SetCapacity(HTTPSourceGovOp, 2)
	if client != nil {
		s.httpClientOnce.Do(func() {
			s.httpProbeClient = client
			s.httpRelayClient = client
		})
	}
	return s
}

func generateMediaFlowURL(t *testing.T, s *Server, candidate mediaFlowURLRequest) string {
	t.Helper()
	if candidate.Endpoint == "" {
		candidate.Endpoint = "/proxy/stream"
	}
	candidate.QueryParams = map[string]string{"api_password": mediaFlowTestPassword}
	body, err := json.Marshal(mediaFlowGenerateRequest{URLs: []mediaFlowURLRequest{candidate}})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handleMediaFlowGenerate(rec, httptest.NewRequest(http.MethodPost, "/generate_urls", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("generate status=%d", rec.Code)
	}
	var response mediaFlowGenerateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || len(response.URLs) != 1 {
		t.Fatalf("invalid generate response")
	}
	return response.URLs[0]
}

func TestMediaFlowGenerateAndRangePlayback(t *testing.T) {
	payload := []byte("0123456789")
	var calls atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "opaque credential" || r.Header.Get("Range") != "bytes=2-5" {
			t.Error("transient request headers were not applied safely")
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Range", "bytes 2-5/10")
		w.Header().Set("Content-Length", "4")
		w.Header().Set("Set-Cookie", "must-not-cross")
		w.Header().Set("Location", "must-not-cross")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[2:6])
	}))
	defer origin.Close()

	s := newMediaFlowTestServer(origin.Client())
	db, err := store.Open(context.Background(), t.TempDir()+"/coverage.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	tracker, _ := playbackcoverage.New(store.New(db), playbackcoverage.Options{})
	s.SetPlaybackCoverage(tracker)
	generated := generateMediaFlowURL(t, s, mediaFlowURLRequest{
		DestinationURL: origin.URL,
		Filename:       "../movie.mkv",
		RequestHeaders: map[string]string{
			"Authorization": "opaque credential",
		},
		ResponseHeaders: map[string]string{
			"Content-Type": "video/x-matroska",
			"Set-Cookie":   "must-be-ignored",
		},
	})
	if strings.Contains(generated, origin.URL) || strings.Contains(generated, mediaFlowTestPassword) || strings.Contains(generated, "opaque credential") {
		t.Fatal("generated URL exposed proxy input")
	}
	u, err := url.Parse(generated)
	if err != nil || u.Path != "/proxy/stream" || u.Query().Get("d") == "" {
		t.Fatal("generated URL is not an opaque DH proxy path")
	}
	req := httptest.NewRequest(http.MethodGet, u.RequestURI(), nil)
	req.Header.Set("Range", "bytes=2-5")
	rec := httptest.NewRecorder()
	s.handleMediaFlowStream(rec, req)
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "2345" || calls.Load() != 1 {
		t.Fatalf("playback status=%d body_bytes=%d calls=%d", rec.Code, rec.Body.Len(), calls.Load())
	}
	if rec.Header().Get("Content-Type") != "video/x-matroska" || rec.Header().Get("Set-Cookie") != "" || rec.Header().Get("Location") != "" {
		t.Fatal("unsafe or incorrect response headers")
	}
	ticket, err := openMediaFlowTicket(mediaFlowTestSecret, u.Query().Get("d"))
	if err != nil || ticket.RepresentationID == "" {
		t.Fatalf("generated representation missing: %v", err)
	}
	snapshot, found, err := tracker.Snapshot(context.Background(), ticket.RepresentationID)
	if err != nil || !found || snapshot.DeliveredBytes != 4 || len(snapshot.Intervals) != 1 || snapshot.Intervals[0] != (playbackcoverage.Interval{Start: 2, End: 6}) {
		t.Fatalf("coverage = %+v found=%v err=%v", snapshot, found, err)
	}
}

func TestMediaFlowRoutesRegisteredWithExactMethods(t *testing.T) {
	handler := newMediaFlowTestServer(nil).Router()
	for _, test := range []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/proxy/ip?api_password=" + mediaFlowTestPassword, http.StatusOK},
		{http.MethodGet, "/generate_urls", http.StatusMethodNotAllowed},
		{http.MethodPost, "/proxy/stream", http.StatusMethodNotAllowed},
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(test.method, test.path, nil))
		if rec.Code != test.want {
			t.Fatalf("%s route status=%d want=%d", test.path, rec.Code, test.want)
		}
	}
}

func TestMediaFlowTicketTamperAndRestart(t *testing.T) {
	s1 := newMediaFlowTestServer(nil)
	generated := generateMediaFlowURL(t, s1, mediaFlowURLRequest{DestinationURL: "http" + "://media.invalid/movie.mkv"})
	u, err := url.Parse(generated)
	if err != nil {
		t.Fatal(err)
	}
	ticket := u.Query().Get("d")
	if _, err := openMediaFlowTicket(mediaFlowTestSecret, ticket); err != nil {
		t.Fatal("ticket did not survive same-secret restart")
	}
	replacement := byte('A')
	if ticket[0] == replacement {
		replacement = 'B'
	}
	tampered := string(replacement) + ticket[1:]
	if _, err := openMediaFlowTicket(mediaFlowTestSecret, tampered); err == nil {
		t.Fatal("tampered ticket accepted")
	}
	rec := httptest.NewRecorder()
	s2 := newMediaFlowTestServer(nil)
	s2.handleMediaFlowStream(rec, httptest.NewRequest(http.MethodGet, "/proxy/stream?d="+url.QueryEscape(tampered), nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("tampered ticket status=%d", rec.Code)
	}
}

func TestMediaFlowSelfOriginUnwrapsOnceWithoutCoverage(t *testing.T) {
	payload := []byte("0123456789")
	var calls atomic.Int32
	origin := mediaFlowRangeOrigin(payload, &calls)
	defer origin.Close()
	s := newMediaFlowTestServer(origin.Client())
	db, err := store.Open(context.Background(), t.TempDir()+"/coverage.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	tracker, _ := playbackcoverage.New(store.New(db), playbackcoverage.Options{})
	s.SetPlaybackCoverage(tracker)
	inner := generateMediaFlowURL(t, s, mediaFlowURLRequest{DestinationURL: origin.URL})
	outer := generateMediaFlowURL(t, s, mediaFlowURLRequest{DestinationURL: inner})
	u, err := url.Parse(outer)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	const nativeRepresentation = "http:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	req := httptest.NewRequest(http.MethodGet, u.RequestURI(), nil)
	req = req.WithContext(playbackcoverage.WithRepresentation(req.Context(), nativeRepresentation))
	s.handleMediaFlowStream(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != string(payload) || calls.Load() != 1 {
		t.Fatalf("playback status=%d body=%q calls=%d", rec.Code, rec.Body.String(), calls.Load())
	}
	for _, generated := range []string{inner, outer} {
		parsed, _ := url.Parse(generated)
		ticket, openErr := openMediaFlowTicket(mediaFlowTestSecret, parsed.Query().Get("d"))
		if openErr != nil {
			t.Fatal(openErr)
		}
		if _, found, snapshotErr := tracker.Snapshot(context.Background(), ticket.RepresentationID); snapshotErr != nil || found {
			t.Fatalf("self-origin coverage found=%v err=%v", found, snapshotErr)
		}
	}
	native, found, snapshotErr := tracker.Snapshot(context.Background(), nativeRepresentation)
	if snapshotErr != nil || !found || native.DeliveredBytes != int64(len(payload)) {
		t.Fatalf("native coverage = %+v found=%v err=%v", native, found, snapshotErr)
	}
}

func TestResolveHTTPPlaybackUnwrapsAggregatorSelfOrigin(t *testing.T) {
	s := newMediaFlowTestServer(nil)
	s.httpResolveCoord = newHTTPResolveCoordinator()
	inner := generateMediaFlowURL(t, s, mediaFlowURLRequest{
		DestinationURL: "http" + "://media.invalid/movie.mkv",
		RequestHeaders: map[string]string{"Authorization": "opaque credential"},
	})
	handler := &sequenceHandler{urls: []string{inner}}
	key := httpstream.ResolveKey{Version: httpstream.ResolveKeyVersion, BackendID: "aio", Handler: "stremio", Kind: "movie", IDs: map[string]string{"imdb": "tt0000001"}, Selector: "sel"}
	resolved, err := s.resolveHTTPPlayback(context.Background(), "item", "file", handler, key, "sel", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.URL != "http://media.invalid/movie.mkv" || resolved.RequestHeaders["Authorization"] != "opaque credential" {
		t.Fatal("self-origin resolved file was not unwrapped exactly once")
	}
}

func TestMediaFlowSelfOriginRejectsInvalidAndRecursiveTickets(t *testing.T) {
	s := newMediaFlowTestServer(nil)
	valid, err := sealMediaFlowTicket(mediaFlowTestSecret, mediaFlowTicket{Version: mediaFlowTicketVersion, DestinationURL: "http" + "://media.invalid/movie.mkv"})
	if err != nil {
		t.Fatal(err)
	}
	for _, destination := range []string{
		s.cfg.Server.BaseURL + "/wrong?d=" + url.QueryEscape(valid),
		s.cfg.Server.BaseURL + "/proxy/stream",
		s.cfg.Server.BaseURL + "/proxy/stream?d=bad",
	} {
		ticket := mediaFlowTicket{Version: mediaFlowTicketVersion, DestinationURL: destination}
		if _, err := s.unwrapMediaFlowSelfOrigin(&ticket); err == nil {
			t.Fatalf("accepted invalid self-origin destination")
		}
	}
	wrongVersion, err := sealMediaFlowTicket(mediaFlowTestSecret, mediaFlowTicket{Version: mediaFlowTicketVersion + 1, DestinationURL: "http" + "://media.invalid/movie.mkv"})
	if err != nil {
		t.Fatal(err)
	}
	ticket := mediaFlowTicket{Version: mediaFlowTicketVersion, DestinationURL: s.cfg.Server.BaseURL + "/proxy/stream?d=" + url.QueryEscape(wrongVersion)}
	if _, err := s.unwrapMediaFlowSelfOrigin(&ticket); err == nil {
		t.Fatal("accepted wrong-version self-origin ticket")
	}
	recursive, err := sealMediaFlowTicket(mediaFlowTestSecret, mediaFlowTicket{Version: mediaFlowTicketVersion, DestinationURL: s.cfg.Server.BaseURL + "/proxy/stream?d=" + url.QueryEscape(valid)})
	if err != nil {
		t.Fatal(err)
	}
	outer := mediaFlowTicket{Version: mediaFlowTicketVersion, DestinationURL: s.cfg.Server.BaseURL + "/proxy/stream?d=" + url.QueryEscape(recursive)}
	if _, err := s.unwrapMediaFlowSelfOrigin(&outer); err == nil {
		t.Fatal("accepted recursive self-origin ticket")
	}
}

func TestMediaFlowAuthBoundsAndPublicIP(t *testing.T) {
	s := newMediaFlowTestServer(nil)
	bad := mediaFlowGenerateRequest{APIPassword: "wrong", URLs: []mediaFlowURLRequest{{DestinationURL: "http" + "://media.invalid/movie.mkv"}}}
	body, _ := json.Marshal(bad)
	rec := httptest.NewRecorder()
	s.handleMediaFlowGenerate(rec, httptest.NewRequest(http.MethodPost, "/generate_urls", bytes.NewReader(body)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bad password status=%d", rec.Code)
	}
	valid := mediaFlowGenerateRequest{
		APIPassword: mediaFlowTestPassword,
		URLs:        []mediaFlowURLRequest{{Endpoint: "/proxy/stream", DestinationURL: "http" + "://media.invalid/movie.mkv"}},
	}
	body, _ = json.Marshal(valid)
	rec = httptest.NewRecorder()
	s.handleMediaFlowGenerate(rec, httptest.NewRequest(http.MethodPost, "/generate_urls", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("top-level password status=%d", rec.Code)
	}

	tooMany := mediaFlowGenerateRequest{APIPassword: mediaFlowTestPassword, URLs: make([]mediaFlowURLRequest, mediaFlowMaxURLs+1)}
	body, _ = json.Marshal(tooMany)
	rec = httptest.NewRecorder()
	s.handleMediaFlowGenerate(rec, httptest.NewRequest(http.MethodPost, "/generate_urls", bytes.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized list status=%d", rec.Code)
	}

	ip := httptest.NewRecorder()
	s.handleMediaFlowIP(ip, httptest.NewRequest(http.MethodGet, "/proxy/ip?api_password="+mediaFlowTestPassword, nil))
	if ip.Code != http.StatusOK || !strings.Contains(ip.Body.String(), "192.0.2.1") {
		t.Fatalf("public IP status=%d", ip.Code)
	}
	s.cfg.MediaFlow.PublicIP = "not-an-ip"
	ip = httptest.NewRecorder()
	s.handleMediaFlowIP(ip, httptest.NewRequest(http.MethodGet, "/proxy/ip?api_password="+mediaFlowTestPassword, nil))
	if ip.Code != http.StatusServiceUnavailable {
		t.Fatalf("invalid public IP status=%d", ip.Code)
	}
}

func TestMediaFlowPublicIPRuntimeLookupAndLiteralOverride(t *testing.T) {
	var calls atomic.Int64
	lookup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte("198.51.100.9"))
	}))
	defer lookup.Close()
	s := newMediaFlowTestServer(nil)
	s.publicIPResolver = publicip.New(t.Context(), publicip.Options{Endpoints: []string{lookup.URL}})
	defer s.publicIPResolver.Close()
	s.cfg.MediaFlow.PublicIP = ""

	rec := httptest.NewRecorder()
	s.handleMediaFlowIP(rec, httptest.NewRequest(http.MethodGet, "/proxy/ip?api_password="+mediaFlowTestPassword, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "198.51.100.9") {
		t.Fatalf("runtime public IP status=%d body=%q", rec.Code, rec.Body.String())
	}

	s.cfg.MediaFlow.PublicIP = "192.0.2.44"
	rec = httptest.NewRecorder()
	s.handleMediaFlowIP(rec, httptest.NewRequest(http.MethodGet, "/proxy/ip?api_password="+mediaFlowTestPassword, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "192.0.2.44") || calls.Load() != 1 {
		t.Fatalf("literal override status=%d body=%q lookup_calls=%d", rec.Code, rec.Body.String(), calls.Load())
	}
}

func TestMediaFlowPublicIPLookupFailureWithoutCacheReturns503(t *testing.T) {
	lookup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not-an-address"))
	}))
	defer lookup.Close()
	s := newMediaFlowTestServer(nil)
	s.publicIPResolver = publicip.New(t.Context(), publicip.Options{Endpoints: []string{lookup.URL}})
	defer s.publicIPResolver.Close()
	s.cfg.MediaFlow.PublicIP = ""
	rec := httptest.NewRecorder()
	s.handleMediaFlowIP(rec, httptest.NewRequest(http.MethodGet, "/proxy/ip?api_password="+mediaFlowTestPassword, nil))
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), lookup.URL) {
		t.Fatalf("lookup failure status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestMediaFlowRejectsUnsafeTargetsAndHeaders(t *testing.T) {
	for _, ticket := range []mediaFlowTicket{
		{Version: mediaFlowTicketVersion, DestinationURL: "file" + ":///tmp/media"},
		{Version: mediaFlowTicketVersion, DestinationURL: "http" + "://user:pass@media.invalid/movie.mkv"},
		{Version: mediaFlowTicketVersion, DestinationURL: "http" + "://media.invalid/movie.mkv", RequestHeaders: map[string]string{"Host": "other.invalid"}},
		{Version: mediaFlowTicketVersion, DestinationURL: "http" + "://media.invalid/movie.mkv", RequestHeaders: map[string]string{"X-Test": "bad\nvalue"}},
	} {
		if _, err := normalizeMediaFlowTicket(ticket); err == nil {
			t.Fatal("unsafe MediaFlow ticket accepted")
		}
	}
}

func TestMediaFlowCancellationAndSaturationReleaseBounds(t *testing.T) {
	started := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer origin.CloseClientConnections()
	defer origin.Close()

	s := newMediaFlowTestServer(origin.Client())
	generated := generateMediaFlowURL(t, s, mediaFlowURLRequest{DestinationURL: origin.URL})
	u, _ := url.Parse(generated)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodGet, u.RequestURI(), nil).WithContext(ctx)
		s.handleMediaFlowStream(httptest.NewRecorder(), req)
	}()
	<-started
	cancel()
	<-done
	if len(s.mediaFlowSem) != 0 || s.httpGov.InUse(HTTPSourceGovOp) != 0 || s.httpGov.Waiting(HTTPSourceGovOp) != 0 {
		t.Fatal("cancellation leaked concurrency state")
	}

	for i := 0; i < cap(s.mediaFlowSem); i++ {
		s.mediaFlowSem <- struct{}{}
	}
	rec := httptest.NewRecorder()
	s.handleMediaFlowStream(rec, httptest.NewRequest(http.MethodGet, u.RequestURI(), nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("saturated proxy status=%d", rec.Code)
	}
	for i := 0; i < cap(s.mediaFlowSem); i++ {
		<-s.mediaFlowSem
	}
}

func TestParseMediaFlowRangeAndContentRange(t *testing.T) {
	for _, raw := range []string{"bytes=0-0", "bytes=5-", "bytes=-10"} {
		if _, err := parseMediaFlowRange(raw); err != nil {
			t.Fatalf("valid range rejected: %q", raw)
		}
	}
	for _, raw := range []string{"", "items=0-1", "bytes=1-0", "bytes=0-1,2-3"} {
		if _, err := parseMediaFlowRange(raw); err == nil {
			t.Fatalf("invalid range accepted: %q", raw)
		}
	}
	want, _ := parseMediaFlowRange("bytes=2-5")
	if err := validateMediaFlowContentRange("bytes 2-5/10", "4", &want); err != nil {
		t.Fatal(err)
	}
	if err := validateMediaFlowContentRange("bytes 3-5/10", "3", &want); err == nil {
		t.Fatal("contradictory content range accepted")
	}
}

func FuzzMediaFlowTicketAndRange(f *testing.F) {
	ticket, err := sealMediaFlowTicket(mediaFlowTestSecret, mediaFlowTicket{
		Version:        mediaFlowTicketVersion,
		DestinationURL: "http" + "://media.invalid/movie.mkv",
	})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(ticket, "bytes=0-0")
	f.Add("garbage", "bytes=-1")
	f.Add("", "bytes=0-0")
	f.Fuzz(func(t *testing.T, encoded, rawRange string) {
		opened, err := openMediaFlowTicket(mediaFlowTestSecret, encoded)
		if err == nil {
			if _, validateErr := httpstream.ValidateURL(opened.DestinationURL); validateErr != nil {
				t.Fatal("ticket opened with invalid destination")
			}
		}
		s := newMediaFlowTestServer(nil)
		_, _ = s.unwrapMediaFlowSelfOrigin(&mediaFlowTicket{Version: mediaFlowTicketVersion, DestinationURL: s.cfg.Server.BaseURL + "/proxy/stream?d=" + url.QueryEscape(encoded)})
		_, _ = parseMediaFlowRange(rawRange)
	})
}

func FuzzMediaFlowContentRange(f *testing.F) {
	f.Add("bytes 0-0/1", "1", "bytes=0-0")
	f.Add("bytes 90-99/100", "10", "bytes=-10")
	f.Add("malformed", "", "bytes=0-")
	f.Fuzz(func(t *testing.T, contentRange, contentLength, requestRange string) {
		want, err := parseMediaFlowRange(requestRange)
		if err != nil {
			return
		}
		_ = validateMediaFlowContentRange(contentRange, contentLength, &want)
		_, _ = mediaFlowContentRangeStart(contentRange)
	})
}

func TestCopyPlaybackBodyCountsSuccessfulWritesAndAbstainsWithoutIdentity(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/copy.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	tracker, _ := playbackcoverage.New(store.New(db), playbackcoverage.Options{})
	s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil)), playbackCoverage: tracker}
	ctx = playbackcoverage.WithRepresentation(ctx, "torrent:passthrough")
	var dst bytes.Buffer
	n, err := s.copyPlaybackBody(ctx, &dst, strings.NewReader("0123456789"), make([]byte, 3), 20, "test", nil)
	if err != nil || n != 10 || dst.String() != "0123456789" {
		t.Fatalf("copy = %d, %q, %v", n, dst.String(), err)
	}
	snapshot, found, err := tracker.Snapshot(ctx, "torrent:passthrough")
	if err != nil || !found || snapshot.DeliveredBytes != 10 || len(snapshot.Intervals) != 1 || snapshot.Intervals[0] != (playbackcoverage.Interval{Start: 20, End: 30}) {
		t.Fatalf("coverage = %+v found=%v err=%v", snapshot, found, err)
	}
	if _, err := s.copyPlaybackBody(context.Background(), io.Discard, strings.NewReader("ignored"), make([]byte, 2), 0, "test", nil); err != nil {
		t.Fatal(err)
	}
	if _, found, err := tracker.Snapshot(ctx, "torrent:ignored"); err != nil || found {
		t.Fatalf("identity-free copy counted: found=%v err=%v", found, err)
	}
}

func TestCopyPlaybackBodyRecoversCoverageAfterTransientObserveFailure(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	var ticks int64
	s := &Server{
		log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		playbackCoverage: &playbackcoverage.Tracker{},
		nowFn: func() time.Time {
			ticks++
			return base.Add(time.Duration(ticks) * time.Second)
		},
	}
	type span struct{ offset, length int64 }
	var calls int
	var spans []span
	s.playbackObserve = func(_ context.Context, representationID string, offset, length int64) (playbackcoverage.Snapshot, error) {
		calls++
		if representationID != "mediaflow:test-retry" {
			t.Fatalf("representation=%q", representationID)
		}
		if calls == 1 {
			return playbackcoverage.Snapshot{}, errors.New("transient coverage write failure")
		}
		spans = append(spans, span{offset: offset, length: length})
		return playbackcoverage.Snapshot{RepresentationID: representationID, DeliveredBytes: offset + length}, nil
	}
	ctx := playbackcoverage.WithRepresentation(context.Background(), "mediaflow:test-retry")
	var dst bytes.Buffer
	n, err := s.copyPlaybackBody(ctx, &dst, strings.NewReader("0123456789"), make([]byte, 2), 0, "test", nil)
	if err != nil || n != 10 || dst.String() != "0123456789" {
		t.Fatalf("copy = %d, %q, %v", n, dst.String(), err)
	}
	if calls < 2 || len(spans) == 0 {
		t.Fatalf("tracking did not recover: calls=%d spans=%+v", calls, spans)
	}
	if spans[0] != (span{offset: 0, length: 4}) {
		t.Fatalf("first recovered span=%+v want offset=0 length=4", spans[0])
	}
}

func TestCopyPlaybackBodyFlushesPendingCoverageAtEOF(t *testing.T) {
	fixed := time.Unix(1_700_000_000, 0)
	s := &Server{
		log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		playbackCoverage: &playbackcoverage.Tracker{},
		nowFn:            func() time.Time { return fixed },
	}
	type span struct{ offset, length int64 }
	var calls int
	var recovered span
	s.playbackObserve = func(_ context.Context, representationID string, offset, length int64) (playbackcoverage.Snapshot, error) {
		calls++
		if calls == 1 {
			return playbackcoverage.Snapshot{}, errors.New("transient coverage write failure")
		}
		recovered = span{offset: offset, length: length}
		return playbackcoverage.Snapshot{RepresentationID: representationID, DeliveredBytes: offset + length}, nil
	}
	ctx := playbackcoverage.WithRepresentation(context.Background(), "mediaflow:test-eof")
	var dst bytes.Buffer
	n, err := s.copyPlaybackBody(ctx, &dst, strings.NewReader("0123456789"), make([]byte, 2), 0, "test", nil)
	if err != nil || n != 10 || dst.String() != "0123456789" {
		t.Fatalf("copy = %d, %q, %v", n, dst.String(), err)
	}
	if calls != 2 {
		t.Fatalf("observe calls=%d want 2 (initial failure + EOF recovery)", calls)
	}
	if recovered != (span{offset: 0, length: 10}) {
		t.Fatalf("EOF recovered span=%+v want offset=0 length=10", recovered)
	}
}

// RD-28 / RX-0.2 class: MediaFlow playback URLs are fetched by viewing devices,
// so the emitted origin must be overridable independently of the private
// Server.BaseURL. Empty must preserve prior behavior byte-for-byte.
func TestMediaFlowClientBaseFallsBackToServerBaseURL(t *testing.T) {
	s := &Server{cfg: &config.Config{}}
	s.cfg.Server.BaseURL = "http://darkharrbor:8381"
	if got := s.mediaFlowClientBase(); got != "http://darkharrbor:8381" {
		t.Fatalf("unset MediaFlow.BaseURL must fall back to Server.BaseURL, got %q", got)
	}
}

func TestMediaFlowClientBaseOverridesServerBaseURL(t *testing.T) {
	s := &Server{cfg: &config.Config{}}
	s.cfg.Server.BaseURL = "http://darkharrbor:8381"
	s.cfg.MediaFlow.BaseURL = "http://100.64.0.1:8381"
	if got := s.mediaFlowClientBase(); got != "http://100.64.0.1:8381" {
		t.Fatalf("MediaFlow.BaseURL must win, got %q", got)
	}
}

func TestMediaFlowClientBaseIgnoresWhitespaceOnlyOverride(t *testing.T) {
	s := &Server{cfg: &config.Config{}}
	s.cfg.Server.BaseURL = "http://darkharrbor:8381"
	s.cfg.MediaFlow.BaseURL = "   "
	if got := s.mediaFlowClientBase(); got != "http://darkharrbor:8381" {
		t.Fatalf("whitespace override must fail closed to Server.BaseURL, got %q", got)
	}
}

// The client listener must serve playback but must NOT expose the
// aggregator-facing MediaFlow endpoints, which live on the primary API
// alongside qBit/SAB/WebDAV/metrics/debug/administration routes.
func TestClientRouterServesPlaybackButNotAggregatorEndpoints(t *testing.T) {
	mux := http.NewServeMux()
	s := &Server{cfg: &config.Config{}}
	s.registerMediaFlowPlayback(mux)

	if _, pattern := mux.Handler(httptest.NewRequest(http.MethodGet, "/proxy/stream", nil)); pattern == "" {
		t.Fatal("client listener must serve /proxy/stream")
	}
	for _, path := range []string{"/generate_urls", "/proxy/ip"} {
		if _, pattern := mux.Handler(httptest.NewRequest(http.MethodGet, path, nil)); pattern != "" {
			t.Fatalf("client listener must NOT expose %s, got pattern %q", path, pattern)
		}
	}
}
