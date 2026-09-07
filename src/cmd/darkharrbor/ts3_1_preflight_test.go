package main

// ts3_1_preflight_test.go — TS-3.1: focused coverage for the grab-time
// cached preflight wired into resolveItem's main torrent path (distinct
// from resolveCDNRARItem's own pre-existing archive-branch preflight).
//
// Two real outcome shapes are exercised end to end against a real
// httptest.Server acting as the CDN origin, through the real resolveItem
// code path (no mock of internal/api's D9 probe machinery):
//
//   - capability probe succeeds (206 to bytes=0-0): item reaches
//     StateReady, the arr is notified (rn.ch receives), and a positive
//     "preflight" hash_availability observation is recorded.
//   - capability probe fails (500 to bytes=0-0, no reresolve status):
//     item still reaches StateReady (the .strm materialization itself
//     succeeded — TS-3.1's job is to withhold *completion*, not undo a
//     real write), the arr is NOT notified this cycle, triggerRepair
//     fires exactly once for the right item, and a negative "preflight"
//     hash_availability observation is recorded.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/availability"
	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/output"
	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

type ts31PreflightProvider struct {
	url    string
	fileID string
	name   string
	size   int64
}

func (*ts31PreflightProvider) Name() string                        { return "torbox" }
func (*ts31PreflightProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }
func (*ts31PreflightProvider) CheckCached(context.Context, *store.Item) (*provider.CheckCachedResult, error) {
	return &provider.CheckCachedResult{Cached: true}, nil
}
func (*ts31PreflightProvider) Submit(context.Context, *store.Item, provider.SubmitOptions) (*provider.CreateTaskResponse, error) {
	return nil, fmt.Errorf("unused")
}
func (p *ts31PreflightProvider) Poll(context.Context, *store.Item) (*provider.TaskStatus, error) {
	return &provider.TaskStatus{
		DownloadReady: true,
		Files:         []provider.RemoteFile{{FileID: p.fileID, Name: p.name, Size: p.size}},
	}, nil
}
func (p *ts31PreflightProvider) RequestDownloadURL(context.Context, *store.Item, string) (string, error) {
	return p.url, nil
}
func (*ts31PreflightProvider) Remove(context.Context, *store.Item) error { return nil }

func newTS31Store(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "ts3_1.db"), 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store.New(db)
}

func ts31Item(t *testing.T, st *store.Store, id string) *store.Item {
	t.Helper()
	remoteID := "remote-" + id
	provName := "torbox"
	hash := "deadbeef00deadbeef00deadbeef00deadbeef0"
	item := &store.Item{
		ID: id, PublicID: id + "-pub", SourceType: store.SourceTypeTorrent,
		ClientKind: store.ClientKindQBit, Category: "movies", State: store.StateResolving,
		SubmissionKey: id + "-key", DisplayName: "Movie.2026",
		RemoteID: &remoteID, Provider: &provName, InfoHash: &hash,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateItem(context.Background(), item); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	return item
}

func ts31Config() *config.Config {
	cfg := &config.Config{}
	cfg.Cache.StreamChunkSizeMB = 1
	cfg.Prewarm.TimeoutSec = 5
	cfg.Availability.TTLHours = 1
	return cfg
}

// TestResolveItemGrabPreflightSuccessNotifiesAndRecordsPositive covers the
// healthy path: a real 206 to the bytes=0-0 probe means the grab is
// verified before the arr is told, rn.Notify() fires, and TS-5.1's
// "preflight" source records cached=true.
func TestResolveItemGrabPreflightSuccessNotifiesAndRecordsPositive(t *testing.T) {
	payload := []byte("video-payload-bytes")
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=0-0" {
			t.Errorf("probe sent Range %q, want bytes=0-0", r.Header.Get("Range"))
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[:1])
	}))
	t.Cleanup(origin.Close)

	ctx := context.Background()
	st := newTS31Store(t)
	root := t.TempDir()
	writer := output.NewStrmWriter(filepath.Join(root, "strm"), filepath.Join(root, "staging"))
	writer.SetBlobSink(st, root)
	writer.SetStreamSecret(strings.Repeat("s", 32))

	item := ts31Item(t, st, "ts31-ok")
	cfg := ts31Config()
	prov := &ts31PreflightProvider{url: origin.URL + "?token=must-not-persist", fileID: "f1", name: "movie.mkv", size: int64(len(payload))}

	rn := &readyNotifier{ch: make(chan struct{}, 1), arrs: []config.ArrTarget{{}}}
	var repairCalls []string
	triggerRepair := func(it *store.Item) { repairCalls = append(repairCalls, it.ID) }

	resolveItem(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), prov, st, writer, nil, 0,
		"http://darkharrbor:8381", nil, nil, item, strings.Repeat("s", 32), root, "",
		rn, cfg, nil, nil, nil, nil, triggerRepair, nil)

	got, err := st.GetItemByID(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.StateReady {
		t.Fatalf("state = %s, want ready", got.State)
	}
	if len(rn.ch) != 1 {
		t.Fatalf("rn.Notify() was not called on preflight success")
	}
	if len(repairCalls) != 0 {
		t.Fatalf("triggerRepair called on preflight success: %v", repairCalls)
	}
	ha, found, err := st.GetHashAvailability(ctx, "torbox", *item.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if !found || !ha.Cached || ha.Source != string(availability.SourcePreflight) {
		t.Fatalf("hash availability = %+v, found=%v, want cached=true source=preflight", ha, found)
	}
}

// TestResolveItemGrabPreflightFailureWithholdsCompletionAndRepairs covers
// the broken-cached-grab path: a real non-206, non-refresh-status response
// to the bytes=0-0 probe means the claimed-cached grab is not actually
// fetchable. The item still reaches StateReady (the .strm write already
// genuinely succeeded), but rn.Notify() must NOT fire this cycle, and
// TS-4.1's repair chain must be triggered instead — the master-plan
// contract this row exists to satisfy ("broken-cached grab never reaches
// import"). TS-5.1's "preflight" source must record cached=false.
func TestResolveItemGrabPreflightFailureWithholdsCompletionAndRepairs(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(origin.Close)

	ctx := context.Background()
	st := newTS31Store(t)
	root := t.TempDir()
	writer := output.NewStrmWriter(filepath.Join(root, "strm"), filepath.Join(root, "staging"))
	writer.SetBlobSink(st, root)
	writer.SetStreamSecret(strings.Repeat("s", 32))

	item := ts31Item(t, st, "ts31-broken")
	cfg := ts31Config()
	prov := &ts31PreflightProvider{url: origin.URL, fileID: "f1", name: "movie.mkv", size: 12345}

	rn := &readyNotifier{ch: make(chan struct{}, 1)}
	var repairCalls []string
	triggerRepair := func(it *store.Item) { repairCalls = append(repairCalls, it.ID) }

	resolveItem(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), prov, st, writer, nil, 0,
		"http://darkharrbor:8381", nil, nil, item, strings.Repeat("s", 32), root, "",
		rn, cfg, nil, nil, nil, nil, triggerRepair, nil)

	got, err := st.GetItemByID(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.StateReady {
		t.Fatalf("state = %s, want ready (write itself succeeded; only completion-reporting withheld)", got.State)
	}
	if len(rn.ch) != 0 {
		t.Fatalf("rn.Notify() fired on a broken-cached grab (want withheld)")
	}
	if len(repairCalls) != 1 || repairCalls[0] != item.ID {
		t.Fatalf("triggerRepair calls = %v, want exactly [%s]", repairCalls, item.ID)
	}
	ha, found, err := st.GetHashAvailability(ctx, "torbox", *item.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if !found || ha.Cached || ha.Source != string(availability.SourcePreflight) {
		t.Fatalf("hash availability = %+v, found=%v, want cached=false source=preflight", ha, found)
	}
}
