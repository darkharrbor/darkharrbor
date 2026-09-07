package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// TestAttachGrabProviderIdentityWithFallbackPrefersCacheHit is the
// baseline: when the direct cache lookup already finds identity (the
// normal ID-02 path), the ID-04 fallback must never even attempt an Arr
// lookup, let alone overwrite what the cache already provided.
func TestAttachGrabProviderIdentityWithFallbackPrefersCacheHit(t *testing.T) {
	arrHit := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrHit = true
		_, _ = w.Write([]byte(`[]`))
	}))
	defer ts.Close()

	st := newC04TestStore(t)
	ctx := context.Background()
	cached := store.ProviderIdentity{Kind: "movie", IDs: store.ProviderIDs{TMDB: "939243"}}
	if err := st.UpsertGrabProviderIdentity(ctx, "nzb:cachehit", cached); err != nil {
		t.Fatal(err)
	}
	s := &Server{
		store: st, log: slog.Default(),
		cfg:               &config.Config{Arrs: []config.ArrTarget{{BaseURL: ts.URL, APIKey: "k"}}},
		arrMetadataClient: ts.Client(),
	}

	var metadata store.SubmissionMetadata
	s.attachGrabProviderIdentityWithFallback(ctx, "nzb:cachehit", "movies", "Sonic.the.Hedgehog.3.2024.WEB-DL", &metadata)

	if metadata.ProviderIdentity == nil || metadata.ProviderIdentity.IDs.TMDB != "939243" {
		t.Fatalf("expected the cached identity to be used, got %+v", metadata.ProviderIdentity)
	}
	if arrHit {
		t.Fatal("cache hit must short-circuit before ever calling the Arr")
	}
}

// TestAttachGrabProviderIdentityWithFallbackSeriesMatch is the ID-04
// regression: this is the exact real scenario found live this session --
// a Sonarr grab (category tv-*) whose cache lookup misses (the search-
// time-to-grab-time bridge was never exercised) must still resolve
// identity, by parsing the series title from the release filename and
// matching it against the configured Sonarr's own library.
func TestAttachGrabProviderIdentityWithFallbackSeriesMatch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":7,"title":"Roseanne","tvdbId":77068,"tmdbId":2706,"imdbId":"tt0094540"}]`))
	}))
	defer ts.Close()

	st := newC04TestStore(t)
	s := &Server{
		store: st, log: slog.Default(),
		cfg:               &config.Config{Arrs: []config.ArrTarget{{Name: "sonarr-classic", BaseURL: ts.URL, APIKey: "k"}}},
		arrMetadataClient: ts.Client(),
	}

	var metadata store.SubmissionMetadata
	s.attachGrabProviderIdentityWithFallback(
		context.Background(), "nzb:nevercached", "tv-classic",
		"Roseanne.S01E18.1080p.WEBRip.x265", &metadata,
	)

	if metadata.ProviderIdentity == nil {
		t.Fatal("expected the ID-04 fallback to resolve identity from the release title")
	}
	if metadata.ProviderIdentity.Kind != "series" || metadata.ProviderIdentity.IDs.TVDB != "77068" {
		t.Fatalf("got %+v, want series identity with tvdb 77068", metadata.ProviderIdentity)
	}
}

// TestAttachGrabProviderIdentityWithFallbackMovieMatch mirrors the series
// case for a movies-category grab.
func TestAttachGrabProviderIdentityWithFallbackMovieMatch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"title":"Forrest Gump","tmdbId":13,"imdbId":"tt0109830","year":1994}]`))
	}))
	defer ts.Close()

	st := newC04TestStore(t)
	s := &Server{
		store: st, log: slog.Default(),
		cfg:               &config.Config{Arrs: []config.ArrTarget{{Name: "radarr", BaseURL: ts.URL, APIKey: "k"}}},
		arrMetadataClient: ts.Client(),
	}

	var metadata store.SubmissionMetadata
	s.attachGrabProviderIdentityWithFallback(
		context.Background(), "nzb:nevercached", "movies",
		"Forrest.Gump.1994.REPACK.2160p.UHD.BluRay", &metadata,
	)

	if metadata.ProviderIdentity == nil || metadata.ProviderIdentity.Kind != "movie" || metadata.ProviderIdentity.IDs.TMDB != "13" {
		t.Fatalf("got %+v, want movie identity with tmdb 13", metadata.ProviderIdentity)
	}
}

// TestAttachGrabProviderIdentityWithFallbackAbstainsWithoutBoundaryToken
// confirms the abstain-not-guess rule: a release name with no exact
// season/episode or year token must never trigger an Arr lookup at all.
func TestAttachGrabProviderIdentityWithFallbackAbstainsWithoutBoundaryToken(t *testing.T) {
	arrHit := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrHit = true
		_, _ = w.Write([]byte(`[]`))
	}))
	defer ts.Close()

	st := newC04TestStore(t)
	s := &Server{
		store: st, log: slog.Default(),
		cfg:               &config.Config{Arrs: []config.ArrTarget{{BaseURL: ts.URL, APIKey: "k"}}},
		arrMetadataClient: ts.Client(),
	}

	var metadata store.SubmissionMetadata
	s.attachGrabProviderIdentityWithFallback(
		context.Background(), "nzb:nevercached", "tv-classic",
		"SomeAmbiguousRelease.WEB-DL.x264-GROUP", &metadata,
	)

	if metadata.ProviderIdentity != nil {
		t.Fatalf("expected abstain (no boundary token), got %+v", metadata.ProviderIdentity)
	}
	if arrHit {
		t.Fatal("must not even query the arr without a parseable title boundary")
	}
}

// TestAttachGrabProviderIdentityWithFallbackAbstainsOnAmbiguousArrMatch
// confirms an ambiguous Arr-side match (two series with the same title)
// still abstains rather than guessing, same as providerIdentityForQuery's
// own established rule.
func TestAttachGrabProviderIdentityWithFallbackAbstainsOnAmbiguousArrMatch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":1,"title":"Roseanne","tvdbId":100},{"id":2,"title":"Roseanne","tvdbId":200}]`))
	}))
	defer ts.Close()

	st := newC04TestStore(t)
	s := &Server{
		store: st, log: slog.Default(),
		cfg:               &config.Config{Arrs: []config.ArrTarget{{BaseURL: ts.URL, APIKey: "k"}}},
		arrMetadataClient: ts.Client(),
	}

	var metadata store.SubmissionMetadata
	s.attachGrabProviderIdentityWithFallback(
		context.Background(), "nzb:nevercached", "tv-classic",
		"Roseanne.S01E18.1080p.WEBRip.x265", &metadata,
	)

	if metadata.ProviderIdentity != nil {
		t.Fatalf("expected abstain on an ambiguous match, got %+v", metadata.ProviderIdentity)
	}
}

// TestAttachGrabProviderIdentityWithFallbackNilMetadataIsSafe confirms the
// nil-guard: a nil metadata pointer (defensive input) must never panic.
func TestAttachGrabProviderIdentityWithFallbackNilMetadataIsSafe(t *testing.T) {
	st := newC04TestStore(t)
	s := &Server{store: st, log: slog.Default(), cfg: &config.Config{}}
	s.attachGrabProviderIdentityWithFallback(context.Background(), "nzb:x", "movies", "Movie.2024.WEB-DL", nil)
}
