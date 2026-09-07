package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/prowlarr"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func newTorrentBlacklistTestStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "torrent_blacklist.db"), 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	return store.New(db)
}

func newTorrentBlacklistTestServer(st *store.Store) *Server {
	cfg := &config.Config{}
	cfg.Routing.Preference = []string{config.LaneUncachedTorrent}
	cfg.Compatibility.DefaultCategory = "darkharrbor"
	return &Server{
		cfg:   cfg,
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		store: st,
	}
}

func TestEligibleTorrentsExcludesBlacklistedCanonicalHash(t *testing.T) {
	ctx := context.Background()
	st := newTorrentBlacklistTestStore(t)
	const hash = "1111111111111111111111111111111111111111"
	if _, err := st.BlacklistImmediately(ctx, hash); err != nil {
		t.Fatalf("BlacklistImmediately: %v", err)
	}

	server := newTorrentBlacklistTestServer(st)
	req := httptest.NewRequest("GET", "/api?t=search", nil)
	items := server.eligibleTorrents(req, []prowlarr.Release{{
		Title:    "Dead.Release.S01",
		InfoHash: hash,
		Protocol: "torrent",
	}}, "5040", time.Now().UTC().Format(time.RFC1123Z), 1)
	if len(items) != 0 {
		t.Fatalf("eligibleTorrents returned %d blacklisted items, want 0", len(items))
	}
}

func TestEnqueueSubmissionRejectsSyntheticWhenCanonicalHashBlacklisted(t *testing.T) {
	ctx := context.Background()
	st := newTorrentBlacklistTestStore(t)
	const realHash = "2222222222222222222222222222222222222222"
	const synthHash = "3333333333333333333333333333333333333333"
	if err := st.UpsertSyntheticRelease(ctx, store.SyntheticRelease{
		SynthHash: synthHash, RealInfohash: realHash,
		Magnet: "magnet:?xt=urn:btih:" + realHash, Season: 2,
	}); err != nil {
		t.Fatalf("UpsertSyntheticRelease: %v", err)
	}
	if _, err := st.BlacklistImmediately(ctx, realHash); err != nil {
		t.Fatalf("BlacklistImmediately: %v", err)
	}

	server := newTorrentBlacklistTestServer(st)
	item, duplicate, err := server.enqueueSubmission(ctx, SubmissionRequest{
		SourceType:  store.SourceTypeTorrent,
		ClientKind:  store.ClientKindQBit,
		Category:    "tv",
		DisplayName: "Synthetic.Release.S02",
		SourceURI:   "magnet:?xt=urn:btih:" + synthHash,
		InfoHash:    synthHash,
	})
	if err == nil || !strings.Contains(err.Error(), "temporarily blacklisted") {
		t.Fatalf("enqueue error = %v, want temporary-blacklist rejection", err)
	}
	if item != nil || duplicate {
		t.Fatalf("rejected enqueue returned item=%v duplicate=%v", item, duplicate)
	}
	items, err := st.ListVisibleClientItems(ctx, store.ClientKindQBit, "", 100)
	if err != nil {
		t.Fatalf("ListVisibleClientItems: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("blacklist rejection created %d qBit-visible items, want 0", len(items))
	}
}

func TestTorrentBlacklistExpiryRestoresEligibility(t *testing.T) {
	ctx := context.Background()
	st := newTorrentBlacklistTestStore(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	st.SetClock(func() time.Time { return now })
	const hash = "4444444444444444444444444444444444444444"
	if _, err := st.BlacklistImmediately(ctx, hash); err != nil {
		t.Fatalf("BlacklistImmediately: %v", err)
	}
	now = now.Add(8 * 24 * time.Hour)

	server := newTorrentBlacklistTestServer(st)
	req := httptest.NewRequest("GET", "/api?t=search", nil)
	items := server.eligibleTorrents(req, []prowlarr.Release{{
		Title:    "Recovered.Release.S01",
		InfoHash: hash,
		Protocol: "torrent",
	}}, "5040", now.Format(time.RFC1123Z), 1)
	if len(items) != 1 || items[0].InfoHash != hash {
		t.Fatalf("eligibleTorrents after expiry = %+v, want hash %s", items, hash)
	}

	const sourceURI = "magnet:?xt=urn:btih:" + hash
	const category = "tv"
	submissionKey := submissionFingerprint(store.SourceTypeTorrent, store.ClientKindQBit, category, sourceURI, hash)
	existing := &store.Item{
		ID:            "expired-blacklist-eligible",
		PublicID:      "expired-blacklist-public",
		SourceType:    store.SourceTypeTorrent,
		ClientKind:    store.ClientKindQBit,
		Category:      category,
		State:         store.StateResolving,
		SubmissionKey: submissionKey,
		DisplayName:   "Recovered.Release.S01",
		InfoHash:      stringPtr(hash),
		SourceURI:     stringPtr(sourceURI),
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := st.CreateItem(ctx, existing); err != nil {
		t.Fatalf("CreateItem after expiry: %v", err)
	}
	item, duplicate, err := server.enqueueSubmission(ctx, SubmissionRequest{
		SourceType:  store.SourceTypeTorrent,
		ClientKind:  store.ClientKindQBit,
		Category:    category,
		DisplayName: "Recovered.Release.S01",
		SourceURI:   sourceURI,
		InfoHash:    hash,
	})
	if err != nil || !duplicate || item == nil || item.ID != existing.ID {
		t.Fatalf("enqueue after expiry item=%+v duplicate=%v err=%v, want existing duplicate", item, duplicate, err)
	}
}

func TestEnqueueSubmissionPreservesNonBlacklistedDuplicateBehavior(t *testing.T) {
	ctx := context.Background()
	st := newTorrentBlacklistTestStore(t)
	server := newTorrentBlacklistTestServer(st)
	const hash = "5555555555555555555555555555555555555555"
	const sourceURI = "magnet:?xt=urn:btih:" + hash
	const category = "tv"
	submissionKey := submissionFingerprint(store.SourceTypeTorrent, store.ClientKindQBit, category, sourceURI, hash)
	now := time.Now().UTC()
	existing := &store.Item{
		ID:            "existing-torrent",
		PublicID:      "existing-public-id",
		SourceType:    store.SourceTypeTorrent,
		ClientKind:    store.ClientKindQBit,
		Category:      category,
		State:         store.StateResolving,
		SubmissionKey: submissionKey,
		DisplayName:   "Existing.Release.S01",
		InfoHash:      stringPtr(hash),
		SourceURI:     stringPtr(sourceURI),
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := st.CreateItem(ctx, existing); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	item, duplicate, err := server.enqueueSubmission(ctx, SubmissionRequest{
		SourceType:  store.SourceTypeTorrent,
		ClientKind:  store.ClientKindQBit,
		Category:    category,
		DisplayName: "Existing.Release.S01",
		SourceURI:   sourceURI,
		InfoHash:    hash,
	})
	if err != nil {
		t.Fatalf("enqueueSubmission: %v", err)
	}
	if !duplicate || item == nil || item.ID != existing.ID {
		t.Fatalf("duplicate result item=%+v duplicate=%v, want existing item", item, duplicate)
	}
}

func stringPtr(v string) *string { return &v }
