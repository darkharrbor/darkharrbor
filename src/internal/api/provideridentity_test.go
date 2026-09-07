package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/nntp"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func TestProviderIdentityExactSeriesLookup(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
			{"id":7,"title":"Same Name","tvdbId":100,"tmdbId":200,"imdbId":"tt0000300"},
			{"id":8,"title":"Other","tvdbId":101}
		]`))
	})
	mux.HandleFunc("/api/v3/episode", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("seriesId") != "7" {
			http.Error(w, "wrong series", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`[{"seasonNumber":1,"episodeNumber":2,"tvdbId":400}]`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	s := &Server{
		cfg: &config.Config{Arrs: []config.ArrTarget{{Name: "sonarr", BaseURL: ts.URL, APIKey: "key"}}},
		log: slog.Default(), arrMetadataClient: ts.Client(),
	}
	got := s.providerIdentityForQuery(context.Background(), "series", "Same Name", "", "", "")
	if got == nil || got.IDs.TVDB != "100" || got.IDs.TMDB != "200" ||
		got.IDs.IMDB != "tt0000300" || len(got.Episodes) != 1 || got.Episodes[0].TVDB != "400" {
		t.Fatalf("providerIdentityForQuery = %+v", got)
	}
}

func TestProviderIdentityAmbiguousTitleRefusesGuess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/series") {
			_, _ = w.Write([]byte(`[
				{"id":1,"title":"Same Name","tvdbId":100},
				{"id":2,"title":"Same Name","tvdbId":200}
			]`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer ts.Close()
	s := &Server{
		cfg: &config.Config{Arrs: []config.ArrTarget{{BaseURL: ts.URL, APIKey: "key"}}},
		log: slog.Default(), arrMetadataClient: ts.Client(),
	}
	if got := s.providerIdentityForQuery(context.Background(), "series", "Same Name", "", "", ""); got != nil {
		t.Fatalf("ambiguous lookup guessed %+v", got)
	}
}

func TestProviderIdentityDirectIDNeedsNoArr(t *testing.T) {
	s := &Server{cfg: &config.Config{}}
	got := s.providerIdentityForQuery(context.Background(), "movie", "Movie", "", "42", "tt0111161")
	if got == nil || got.IDs.TMDB != "42" || got.IDs.IMDB != "tt0111161" {
		t.Fatalf("direct identity = %+v", got)
	}
}

func TestScopeProviderIdentityMatchesOnlyRequestedEpisodes(t *testing.T) {
	identity := &store.ProviderIdentity{
		Kind: "series", IDs: store.ProviderIDs{TVDB: "100"},
		Episodes: []store.EpisodeProviderIDs{
			{Season: 1, Episode: 1, TVDB: "101"},
			{Season: 1, Episode: 2, TVDB: "102"},
			{Season: 2, Episode: 1, TVDB: "201"},
		},
	}
	exact := scopeProviderIdentity(identity, 1, 2)
	if len(exact.Episodes) != 1 || exact.Episodes[0].TVDB != "102" {
		t.Fatalf("exact scope = %+v", exact.Episodes)
	}
	season := scopeProviderIdentity(identity, 1, 0)
	if len(season.Episodes) != 2 {
		t.Fatalf("season scope = %+v", season.Episodes)
	}
	series := scopeProviderIdentity(identity, 0, 0)
	if len(series.Episodes) != 0 || len(identity.Episodes) != 3 {
		t.Fatalf("series scope mutated source: scoped=%+v source=%+v", series.Episodes, identity.Episodes)
	}
}

func TestGrabProviderIdentityAttachesAcrossTorrentAndNZBKeys(t *testing.T) {
	st := newC04TestStore(t)
	s := &Server{store: st, log: slog.Default()}
	identity := store.ProviderIdentity{
		Kind: "series", Title: "Show", IDs: store.ProviderIDs{TVDB: "100"},
	}
	ctx := context.Background()
	if err := st.UpsertGrabProviderIdentity(ctx, "torrent:abc", identity); err != nil {
		t.Fatal(err)
	}
	nzbKey := nntp.ContentKey([]byte(`<nzb><file subject="x"><segments><segment bytes="1" number="1">id</segment></segments></file></nzb>`))
	if err := st.UpsertGrabProviderIdentity(ctx, nzbKey, identity); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"torrent:abc", nzbKey} {
		var metadata store.SubmissionMetadata
		s.attachGrabProviderIdentity(ctx, key, &metadata)
		if metadata.ProviderIdentity == nil || metadata.ProviderIdentity.IDs.TVDB != "100" {
			t.Fatalf("%s identity not attached: %+v", key, metadata.ProviderIdentity)
		}
	}
}

func TestTorrentProviderIdentityContextIsPerIdentity(t *testing.T) {
	first := &store.ProviderIdentity{Kind: "series", IDs: store.ProviderIDs{TVDB: "100"}}
	second := &store.ProviderIdentity{Kind: "series", IDs: store.ProviderIDs{TVDB: "200"}}
	firstID, secondID := providerIdentityContextID(first), providerIdentityContextID(second)
	if firstID == "" || secondID == "" || firstID == secondID {
		t.Fatalf("context IDs = %q, %q", firstID, secondID)
	}
	magnet := "magnet:?xt=urn:btih:abc&x.dh.id=" + firstID
	if got := magnetProviderIdentityContext(magnet); got != firstID {
		t.Fatalf("magnet context = %q, want %q", got, firstID)
	}
	if got := magnetProviderIdentityContext("magnet:?x.dh.id=not-hex"); got != "" {
		t.Fatalf("malformed context accepted: %q", got)
	}
}
