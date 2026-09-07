package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/cachegate"
	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/governor"
	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// trackingProvider is a provider.Provider stub that records whether
// CheckCached/Submit were ever invoked, so a test can assert TS-8.1's alias
// path never dials the provider a second time for a same-hash submission.
// Real network methods are not part of this row's scope and are left
// unimplemented (they panic loudly if ever reached).
type trackingProvider struct {
	name              string
	checkCachedCalled atomic.Bool
	submitCalled      atomic.Bool
}

func (p *trackingProvider) Name() string                        { return p.name }
func (p *trackingProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }

func (p *trackingProvider) CheckCached(ctx context.Context, item *store.Item) (*provider.CheckCachedResult, error) {
	p.checkCachedCalled.Store(true)
	return &provider.CheckCachedResult{Cached: true}, nil
}

func (p *trackingProvider) Submit(ctx context.Context, item *store.Item, opts provider.SubmitOptions) (*provider.CreateTaskResponse, error) {
	p.submitCalled.Store(true)
	return &provider.CreateTaskResponse{RemoteID: "unexpected-fresh-submit"}, nil
}

func (p *trackingProvider) Poll(ctx context.Context, item *store.Item) (*provider.TaskStatus, error) {
	return nil, errors.New("trackingProvider: Poll not implemented in test stub")
}

func (p *trackingProvider) RequestDownloadURL(ctx context.Context, item *store.Item, fileID string) (string, error) {
	return "", errors.New("trackingProvider: RequestDownloadURL not implemented in test stub")
}

func (p *trackingProvider) Remove(ctx context.Context, item *store.Item) error { return nil }

func strp(v string) *string { return &v }

func newAliasTestStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "alias_submission.db"), 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	return store.New(db)
}

func newAliasTestServer(st *store.Store, prov *trackingProvider) *Server {
	cfg := &config.Config{}
	cfg.Routing.Preference = []string{config.LaneUncachedTorrent, config.LaneTorBoxTorrent}
	cfg.Compatibility.DefaultCategory = "darkharrbor"
	gov := governor.New(st, 10, 1)
	return &Server{
		cfg:              cfg,
		log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:            st,
		prov:             prov,
		allProviders:     map[string]provider.Provider{prov.Name(): prov},
		providerOrder:    []string{prov.Name()},
		gate:             cachegate.New(prov, gov, false),
		shutdownCtx:      context.Background(),
		activeGoroutines: map[string]struct{}{},
	}
}

// TestEnqueueSubmissionAliasesLaterSameHashTorrent proves TS-8.1's core
// behavior: a torrent submission arriving after another item has already
// established a live provider binding for the same real info-hash reuses
// that binding directly -- no cachegate CheckCached dial, no provider Submit
// call -- while still getting its own independent item row.
func TestEnqueueSubmissionAliasesLaterSameHashTorrent(t *testing.T) {
	ctx := context.Background()
	st := newAliasTestStore(t)
	prov := &trackingProvider{name: "torbox"}
	server := newAliasTestServer(st, prov)

	const hash = "5555555555555555555555555555555555555555"
	remoteID := "remote-first"
	first := &store.Item{
		ID: "first", PublicID: "pfirst", SourceType: store.SourceTypeTorrent,
		ClientKind: store.ClientKindQBit, Category: "movies", State: store.StateReady,
		SubmissionKey: "first-key", DisplayName: "First.Release.2020",
		InfoHash: strp(hash), Provider: strp(prov.Name()), RemoteID: &remoteID, Cached: true,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateItem(ctx, first); err != nil {
		t.Fatalf("create first item: %v", err)
	}

	// A second, independent submission (different Arr instance, different
	// magnet/tracker query string -- hence a different submission key) for
	// the exact same real torrent.
	item, duplicate, err := server.enqueueSubmission(ctx, SubmissionRequest{
		SourceType:  store.SourceTypeTorrent,
		ClientKind:  store.ClientKindQBit,
		Category:    "movies",
		DisplayName: "Second.Release.2020",
		SourceURI:   "magnet:?xt=urn:btih:" + hash + "&tr=http://second-tracker.example/announce",
		InfoHash:    hash,
	})
	if err != nil {
		t.Fatalf("enqueueSubmission: %v", err)
	}
	if duplicate {
		t.Fatalf("enqueueSubmission reported duplicate=true, want a genuinely new item")
	}
	if item == nil || item.ID == first.ID {
		t.Fatalf("got item %+v, want a new independent item distinct from first", item)
	}

	server.wg.Wait() // let the alias-bind goroutine finish

	if prov.checkCachedCalled.Load() {
		t.Fatalf("CheckCached was called; TS-8.1 must skip the cachegate dial for an aliased submission")
	}
	if prov.submitCalled.Load() {
		t.Fatalf("Submit was called; TS-8.1 must skip provider Submit for an aliased submission")
	}

	got, err := st.GetItemByID(ctx, item.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if got.Provider == nil || *got.Provider != prov.Name() {
		t.Fatalf("aliased item provider = %v, want %q", got.Provider, prov.Name())
	}
	if got.RemoteID == nil || *got.RemoteID != remoteID {
		t.Fatalf("aliased item remote_id = %v, want %q (copied from the existing provider object)", got.RemoteID, remoteID)
	}
	if !got.Cached {
		t.Fatalf("aliased item Cached = false, want true (copied from alias source)")
	}
	if got.ID == first.ID {
		t.Fatalf("aliased item shares first item's row -- LC-01 requires independent .strm identities")
	}
}

// TestEnqueueSubmissionNoAliasWhenNoExistingBinding is the control case: a
// first-of-its-kind submission must still go through the normal
// cachegate+Submit path (no candidate exists yet to alias onto).
func TestEnqueueSubmissionNoAliasWhenNoExistingBinding(t *testing.T) {
	ctx := context.Background()
	st := newAliasTestStore(t)
	prov := &trackingProvider{name: "torbox"}
	server := newAliasTestServer(st, prov)

	const hash = "6666666666666666666666666666666666666666"
	item, duplicate, err := server.enqueueSubmission(ctx, SubmissionRequest{
		SourceType:  store.SourceTypeTorrent,
		ClientKind:  store.ClientKindQBit,
		Category:    "movies",
		DisplayName: "Only.Release.2020",
		SourceURI:   "magnet:?xt=urn:btih:" + hash,
		InfoHash:    hash,
	})
	if err != nil {
		t.Fatalf("enqueueSubmission: %v", err)
	}
	if duplicate || item == nil {
		t.Fatalf("got item=%v duplicate=%v, want a fresh non-duplicate item", item, duplicate)
	}

	server.wg.Wait()

	if !prov.checkCachedCalled.Load() {
		t.Fatalf("CheckCached was never called; a first-of-its-kind submission must still hit the cachegate")
	}
	if !prov.submitCalled.Load() {
		t.Fatalf("Submit was never called; a first-of-its-kind submission must still submit to the provider")
	}

	got, err := st.GetItemByID(ctx, item.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if got.RemoteID == nil || *got.RemoteID != "unexpected-fresh-submit" {
		t.Fatalf("item remote_id = %v, want the trackingProvider's real Submit response", got.RemoteID)
	}
}
