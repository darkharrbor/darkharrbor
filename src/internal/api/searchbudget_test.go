package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/prowlarr"
	"github.com/darkharrbor/darkharrbor/internal/searchbudget"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func TestSelectionSearchBudgetCountsOnlyGenuineNoResult(t *testing.T) {
	var status atomic.Int64
	status.Store(http.StatusOK)
	var searches atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/search" {
			http.NotFound(w, r)
			return
		}
		searches.Add(1)
		w.WriteHeader(int(status.Load()))
		_, _ = io.WriteString(w, `[]`)
	}))
	t.Cleanup(origin.Close)

	db, err := store.Open(context.Background(), t.TempDir()+"/budget.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	st := store.New(db)
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	budget, err := searchbudget.New(st, searchbudget.Options{
		Cap: 2, Suppress: 7 * 24 * time.Hour, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Server.BaseURL = "http://darkharrbor.invalid"
	cfg.Prowlarr.IndexerIDs = []int{1}
	server := &Server{
		cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), store: st,
		prowlarr:     prowlarr.New(origin.URL, "test-key", "test", time.Second, nil),
		searchBudget: budget,
	}
	request := func(id string) selectionSearchResult {
		r := httptest.NewRequest(http.MethodGet,
			"http://darkharrbor.invalid/api?t=tvsearch&q=Missing&tvdbid="+id+"&season=1&ep=1", nil)
		return server.selectionSearch(r, r.URL.Query(), "tv")
	}

	for i := 0; i < 2; i++ {
		if got := request("123").outcome; got != searchbudget.ResultNoResult {
			t.Fatalf("attempt %d outcome = %q", i+1, got)
		}
	}
	if got := request("123").outcome; got != searchbudget.ResultFiltered {
		t.Fatalf("suppressed outcome = %q", got)
	}
	if got := searches.Load(); got != 2 {
		t.Fatalf("upstream searches = %d, want 2", got)
	}

	status.Store(http.StatusTooManyRequests)
	if got := request("456").outcome; got != searchbudget.ResultAccount {
		t.Fatalf("429 outcome = %q", got)
	}
	state, found, err := st.GetSearchBudget(context.Background(), "series:tvdb:456:1:1")
	if err != nil || found || state.ConsecutiveNoResults != 0 {
		t.Fatalf("account failure changed budget: state=%+v found=%v err=%v", state, found, err)
	}
}

func TestSelectionSearchBudgetPreservesFindableResultAndResets(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[{
            "title":"Findable.S01E01.1080p","guid":"safe-guid",
            "downloadUrl":"https://example.invalid/safe.nzb","protocol":"usenet","size":1024
        }]`)
	}))
	t.Cleanup(origin.Close)
	db, err := store.Open(context.Background(), t.TempDir()+"/found.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	st := store.New(db)
	budget, _ := searchbudget.New(st, searchbudget.Options{Cap: 5, Suppress: 7 * 24 * time.Hour})
	if _, err := st.RecordSearchNoResult(context.Background(), "series:tvdb:789:1:1", 5, 7*24*time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Server.BaseURL = "http://darkharrbor.invalid"
	cfg.Prowlarr.IndexerIDs = []int{1}
	cfg.Routing.Preference = []string{config.LaneNNTPNZB}
	server := &Server{
		cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), store: st,
		prowlarr:     prowlarr.New(origin.URL, "test-key", "test", time.Second, nil),
		searchBudget: budget,
	}
	r := httptest.NewRequest(http.MethodGet,
		"http://darkharrbor.invalid/api?t=tvsearch&q=Findable&tvdbid=789&season=1&ep=1", nil)
	result := server.selectionSearch(r, r.URL.Query(), "tv")
	if result.outcome != searchbudget.ResultFound || len(result.items) != 1 {
		t.Fatalf("findable result = outcome %q, items %d", result.outcome, len(result.items))
	}
	if _, found, err := st.GetSearchBudget(context.Background(), "series:tvdb:789:1:1"); err != nil || found {
		t.Fatalf("found result did not reset budget: found=%v err=%v", found, err)
	}
}

func TestSelectionSearchBudgetCancellationDoesNotDecrement(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(origin.Close)
	db, err := store.Open(context.Background(), t.TempDir()+"/cancel.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	st := store.New(db)
	budget, _ := searchbudget.New(st, searchbudget.Options{Cap: 5, Suppress: 7 * 24 * time.Hour})
	cfg := &config.Config{}
	cfg.Server.BaseURL = "http://darkharrbor.invalid"
	cfg.Prowlarr.IndexerIDs = []int{1}
	server := &Server{
		cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), store: st,
		prowlarr:     prowlarr.New(origin.URL, "test-key", "test", time.Second, nil),
		searchBudget: budget,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest(http.MethodGet,
		"http://darkharrbor.invalid/api?t=tvsearch&q=Cancelled&tvdbid=999&season=1&ep=1", nil).WithContext(ctx)
	if got := server.selectionSearch(r, r.URL.Query(), "tv").outcome; got != searchbudget.ResultTransient {
		t.Fatalf("cancelled outcome = %q", got)
	}
	if _, found, err := st.GetSearchBudget(context.Background(), "series:tvdb:999:1:1"); err != nil || found {
		t.Fatalf("cancellation changed budget: found=%v err=%v", found, err)
	}
}
