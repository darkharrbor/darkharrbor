package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/sidecar"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func newSidecarTestServer(t *testing.T, dbPath string) (*Server, *store.Store, func()) {
	t.Helper()
	db, err := store.Open(context.Background(), dbPath, time.Second)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		_ = db.Close()
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	st := store.New(db)
	registry, err := sidecar.New(st, sidecar.Options{})
	if err != nil {
		_ = db.Close()
		t.Fatalf("sidecar.New: %v", err)
	}
	s := httpTestServer("0123456789abcdef0123456789abcdef", true)
	s.store = st
	s.sidecars = registry
	return s, st, func() { _ = db.Close() }
}

func createSidecarTestItem(t *testing.T, st *store.Store) {
	t.Helper()
	now := time.Now().UTC()
	item := &store.Item{
		ID: "sidecar-api-item", PublicID: "sidecar-api-item",
		SourceType: store.SourceTypeHTTP, ClientKind: store.ClientKindQBit,
		Category: "movies", State: store.StateReady,
		SubmissionKey: "sidecar-api-item", DisplayName: "Fixture",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := st.CreateItem(context.Background(), item); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
}

func sidecarMux(s *Server) http.Handler {
	mux := http.NewServeMux()
	s.registerSidecars(mux)
	return mux
}

func TestSidecarEndpointGetHeadRangeAndRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sidecar-api.db")
	s1, st1, close1 := newSidecarTestServer(t, dbPath)
	createSidecarTestItem(t, st1)
	payload := []byte("WEBVTT\n\n00:00.000 --> 00:01.000\nHello\n")
	resource, err := s1.CreateSidecarResource(context.Background(), sidecar.Input{
		ItemID: "sidecar-api-item", FileID: "file-1", Kind: sidecar.KindSubtitle,
		Filename: "episode.en.vtt", MediaType: "text/vtt", Language: "en", Bytes: payload,
	})
	if err != nil {
		t.Fatalf("CreateSidecarResource: %v", err)
	}
	ts1 := httptest.NewServer(sidecarMux(s1))

	resp, err := http.Get(ts1.URL + resource.Path())
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != string(payload) {
		t.Fatalf("GET status=%d body=%q", resp.StatusCode, body)
	}
	if resp.Header.Get("Content-Type") != "text/vtt" ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), "episode.en.vtt") ||
		resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("unsafe/incomplete headers: %v", resp.Header)
	}

	req, _ := http.NewRequest(http.MethodHead, ts1.URL+resource.Path(), nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength != int64(len(payload)) {
		t.Fatalf("HEAD status=%d length=%d", resp.StatusCode, resp.ContentLength)
	}

	req, _ = http.NewRequest(http.MethodGet, ts1.URL+resource.Path(), nil)
	req.Header.Set("Range", "bytes=0-5")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("range GET: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || string(body) != "WEBVTT" {
		t.Fatalf("range status=%d body=%q", resp.StatusCode, body)
	}
	ts1.Close()
	close1()

	s2, _, close2 := newSidecarTestServer(t, dbPath)
	defer close2()
	ts2 := httptest.NewServer(sidecarMux(s2))
	defer ts2.Close()
	resp, err = http.Get(ts2.URL + resource.Path())
	if err != nil {
		t.Fatalf("GET after restart: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != string(payload) {
		t.Fatalf("restart GET status=%d body=%q", resp.StatusCode, body)
	}
}

func TestSidecarEndpointNegatives(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sidecar-negative.db")
	s, st, closeDB := newSidecarTestServer(t, dbPath)
	defer closeDB()
	createSidecarTestItem(t, st)
	ts := httptest.NewServer(sidecarMux(s))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/sidecar/sc000000000000000000000000")
	if err != nil {
		t.Fatalf("GET missing: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing status=%d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/sidecar/sc000000000000000000000000", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST status=%d allow=%q", resp.StatusCode, resp.Header.Get("Allow"))
	}
}
