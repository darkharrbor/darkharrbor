package api

import (
	"bytes"
	"context"
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

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

const c05ValidNZB = `<?xml version="1.0"?>
<nzb>
  <file poster="poster" date="1" subject="Example">
    <groups><group>alt.example</group></groups>
    <segments><segment bytes="4" number="1">safe-message-id</segment></segments>
  </file>
</nzb>`

func newC05TestServer(t *testing.T) (*Server, *bytes.Buffer, *store.Store) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "c0.5.db"), 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}

	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	var logs bytes.Buffer
	cfg := &config.Config{}
	cfg.Routing.Preference = []string{config.LaneNNTPNZB}
	s := &Server{
		cfg:              cfg,
		log:              slog.New(slog.NewTextHandler(&logs, nil)),
		store:            store.New(db),
		shutdownCtx:      shutdownCtx,
		shutdownCancel:   shutdownCancel,
		activeGoroutines: make(map[string]struct{}),
		janitorTimers:    make(map[string]context.CancelFunc),
		lastPlayedWrites: make(map[string]time.Time),
	}
	t.Cleanup(func() {
		shutdownCancel()
		s.wg.Wait()
	})
	return s, &logs, s.store
}

func c05Items(t *testing.T, st *store.Store) []*store.Item {
	t.Helper()
	items, err := st.ListVisibleClientItems(context.Background(), store.ClientKindSAB, "", 10)
	if err != nil {
		t.Fatalf("ListVisibleClientItems: %v", err)
	}
	return items
}

func TestHandleSABAddURLFetchesAndPersistsLiteralNZB(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, c05ValidNZB)
	}))
	defer origin.Close()

	s, logs, st := newC05TestServer(t)
	s.cfg.Server.BaseURL = origin.URL
	req := httptest.NewRequest(http.MethodPost, "/api?name="+origin.URL+"/getnzb/token&nzbname=Example.S01E01&cat=tv", nil)
	rec := httptest.NewRecorder()
	s.handleSABAddURL(rec, req)
	s.wg.Wait()

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	items := c05Items(t, st)
	if len(items) != 1 || items[0].SourceURI == nil {
		t.Fatalf("items=%d source_uri_present=%v, want one persisted source", len(items), len(items) == 1 && items[0].SourceURI != nil)
	}
	if got := *items[0].SourceURI; got != c05ValidNZB {
		t.Fatalf("persisted source is not the fetched NZB: len=%d", len(got))
	}
	if strings.Contains(*items[0].SourceURI, origin.URL) {
		t.Fatal("persisted source leaked the submitted URL")
	}
	for _, forbidden := range []string{origin.URL, "safe-message-id", c05ValidNZB} {
		if strings.Contains(logs.String(), forbidden) {
			t.Fatalf("logs contained prohibited fetch material %q", forbidden)
		}
	}
}

func TestHandleSABAddURLNormalizesAggregatorCategoryAndKeepsJobName(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, c05ValidNZB)
	}))
	defer origin.Close()

	s, _, st := newC05TestServer(t)
	s.cfg.Server.BaseURL = origin.URL
	req := httptest.NewRequest(http.MethodPost, "/api?name="+origin.URL+"/getnzb/item&nzbname=Client.Release.Name&cat=MoViEs", nil)
	rec := httptest.NewRecorder()
	s.handleSABAddURL(rec, req)
	s.wg.Wait()

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	items := c05Items(t, st)
	if len(items) != 1 {
		t.Fatalf("items=%d, want 1", len(items))
	}
	if items[0].Category != "movies" || items[0].DisplayName != "Client.Release.Name" {
		t.Fatalf("category=%q name=%q", items[0].Category, items[0].DisplayName)
	}
}

func TestHandleSABAddURLRejectsDisallowedTargetWithoutFetch(t *testing.T) {
	var calls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, c05ValidNZB)
	}))
	defer target.Close()

	s, _, st := newC05TestServer(t)
	s.cfg.Server.BaseURL = "http://allowed.invalid"
	s.cfg.Prowlarr.BaseURL = "https://prowlarr.invalid"
	req := httptest.NewRequest(http.MethodPost, "/api?name="+target.URL+"/private&nzbname=Example", nil)
	rec := httptest.NewRecorder()
	s.handleSABAddURL(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("disallowed target received %d requests", calls.Load())
	}
	if items := c05Items(t, st); len(items) != 0 {
		t.Fatalf("persisted %d items after SSRF refusal", len(items))
	}
}

func TestHandleSABAddURLRejectsMalformedAndOversizedBodies(t *testing.T) {
	tests := []struct {
		name    string
		handler http.Handler
		want    int
	}{
		{
			name: "malformed",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "<html>not an nzb</html>")
			}),
			want: http.StatusBadRequest,
		},
		{
			name: "oversized",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", strconv.Itoa(maxNZBFetchBytes+1))
				w.WriteHeader(http.StatusOK)
			}),
			want: http.StatusInternalServerError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origin := httptest.NewServer(tt.handler)
			defer origin.Close()
			s, logs, st := newC05TestServer(t)
			s.cfg.Server.BaseURL = origin.URL

			req := httptest.NewRequest(http.MethodPost, "/api?name="+origin.URL+"/item&nzbname=SensitiveTitle", nil)
			rec := httptest.NewRecorder()
			s.handleSABAddURL(rec, req)

			if rec.Code != tt.want {
				t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
			}
			if items := c05Items(t, st); len(items) != 0 {
				t.Fatalf("persisted %d items after %s input", len(items), tt.name)
			}
			if strings.Contains(logs.String(), origin.URL) || strings.Contains(rec.Body.String(), origin.URL) {
				t.Fatal("failure surfaces leaked the upstream URL")
			}
		})
	}
}

func TestHandleSABAddURLCancellationStopsFetchAndPersistsNothing(t *testing.T) {
	started := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer origin.Close()

	s, logs, st := newC05TestServer(t)
	s.cfg.Server.BaseURL = origin.URL
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/api?name="+origin.URL+"/item&nzbname=Example", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		s.handleSABAddURL(rec, req)
		close(done)
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after request cancellation")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if items := c05Items(t, st); len(items) != 0 {
		t.Fatalf("persisted %d items after cancellation", len(items))
	}
	if strings.Contains(logs.String(), origin.URL) || strings.Contains(rec.Body.String(), origin.URL) {
		t.Fatal("cancellation surfaces leaked the upstream URL")
	}
}

func TestFetchNZBBytesStripsProwlarrKeyAcrossHostRedirect(t *testing.T) {
	const apiKey = "test-prowlarr-key"
	var redirectedKey atomic.Value
	redirected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedKey.Store(r.Header.Get("X-Api-Key"))
		_, _ = io.WriteString(w, c05ValidNZB)
	}))
	defer redirected.Close()

	var initialKey atomic.Value
	prowlarr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		initialKey.Store(r.Header.Get("X-Api-Key"))
		http.Redirect(w, r, redirected.URL+"/nzb", http.StatusFound)
	}))
	defer prowlarr.Close()

	s, _, _ := newC05TestServer(t)
	s.cfg.Prowlarr.BaseURL = prowlarr.URL
	s.cfg.Prowlarr.APIKey = apiKey
	body, err := s.fetchNZBBytes(context.Background(), prowlarr.URL+"/download")
	if err != nil {
		t.Fatalf("fetchNZBBytes: %v", err)
	}
	if string(body) != c05ValidNZB {
		t.Fatal("redirected fetch returned unexpected content")
	}
	if got, _ := initialKey.Load().(string); got != apiKey {
		t.Fatalf("initial configured-host key=%q, want configured key", got)
	}
	if got, _ := redirectedKey.Load().(string); got != "" {
		t.Fatalf("cross-host redirect received Prowlarr key %q", got)
	}
}

func TestValidateFetchedNZBFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "empty"},
		{name: "malformed", data: "<nzb>"},
		{name: "no files", data: "<nzb></nzb>"},
		{name: "no segments", data: "<nzb><file></file></nzb>"},
		{name: "short segment", data: "<nzb><file><segments><segment bytes=\"0\" number=\"1\"></segment></segments></file></nzb>"},
		{name: "duplicate segment", data: "<nzb><file><segments><segment bytes=\"1\" number=\"1\">a</segment><segment bytes=\"1\" number=\"1\">b</segment></segments></file></nzb>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateFetchedNZB([]byte(tt.data)); err == nil {
				t.Fatal("expected invalid NZB to fail closed")
			}
		})
	}
	if err := validateFetchedNZB([]byte(c05ValidNZB)); err != nil {
		t.Fatalf("valid NZB rejected: %v", err)
	}
}
