package api

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/availability"
	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/prowlarr"
)

// TestIsMultiSeasonTitle exercises the title classification logic that
// determines whether a release is treated as a multi-season pack. The key
// regression: plain "Sxx.COMPLETE" titles (a per-season completeness tag)
// must NOT be classified as multi-season packs, because doing so caused
// synthetic releases to be emitted for unrelated requested seasons.
func TestIsMultiSeasonTitle(t *testing.T) {
	tests := []struct {
		title string
		want  bool
	}{
		// Explicit season ranges — always multi-season.
		{"Show.S01-S05.1080p.BluRay", true},
		{"Show.S01 - S05.1080p", true},
		{"Show.s01-s05.720p", true},
		{"Show.S10-S12.1080p", true},

		// Complete Series / Full Series — genuine multi-season packs.
		{"Show.Complete.Series.BluRay.1080p", true},
		{"Show.COMPLETE.SERIES.720p", true},
		{"Show.Full.Series.1080p", true},
		{"Show.FULL.SERIES.BluRay", true},
		{"Show.Complete.Series.S01-S06.1080p", true},

		// Plain single-season "COMPLETE" tag — NOT multi-season.
		// These are per-season completeness markers ("all episodes of S03").
		{"The.Boys.S03.COMPLETE.720p.BluRay", false},
		{"The.Boys.S04.COMPLETE.720p.BluRay", false},
		{"Show.S01.COMPLETE.1080p", false},
		{"Show.S02.Complete.720p.WEB-DL", false},
		{"Show.S01.COMPLETE.720p.x264", false},

		// Single-season titles without "complete" — NOT multi-season.
		{"The.Boys.S03.720p.BluRay", false},
		{"Show.S01E01.720p", false},
		{"Show.S02.1080p.WEB-DL", false},

		// Edge: "Complete" as part of a word should not trigger.
		{"Show.Completed.S03.720p", false},

		// Edge: no season token at all.
		{"Show.1080p.BluRay", false},
		{"Movie.Name.2024.720p", false},
	}

	for _, tt := range tests {
		got := isMultiSeasonTitle(tt.title)
		if got != tt.want {
			t.Errorf("isMultiSeasonTitle(%q) = %v, want %v", tt.title, got, tt.want)
		}
	}
}

// TestSeasonCoversRequest verifies that synthetic-release emission only
// happens when the title genuinely covers the requested season. This is
// the guard that prevents a plain "S03.COMPLETE" title from generating a
// synthetic release when season 2 is requested.
func TestSeasonCoversRequest(t *testing.T) {
	tests := []struct {
		title     string
		reqSeason int
		want      bool
	}{
		// Explicit range covers the requested season.
		{"Show.S01-S05.1080p", 1, true},
		{"Show.S01-S05.1080p", 3, true},
		{"Show.S01-S05.1080p", 5, true},
		{"Show.S01-S05.1080p", 6, false},
		{"Show.S01-S05.1080p", 0, false},
		{"Show.S03-S06.1080p", 2, false},
		{"Show.S03-S06.1080p", 4, true},

		// Complete Series covers any requested season.
		{"Show.Complete.Series.BluRay", 1, true},
		{"Show.Complete.Series.BluRay", 5, true},
		{"Show.FULL.SERIES.1080p", 3, true},

		// Plain single-season titles: NOT multi-season, so no coverage.
		{"The.Boys.S03.COMPLETE.720p", 2, false},
		{"The.Boys.S03.COMPLETE.720p", 3, false},
		{"The.Boys.S04.COMPLETE.720p", 2, false},

		// Non-multi-season titles always return false.
		{"Show.S01.720p", 1, false},
		{"Show.S02E05.1080p", 2, false},
	}

	for _, tt := range tests {
		got := seasonCoversRequest(tt.title, tt.reqSeason)
		if got != tt.want {
			t.Errorf("seasonCoversRequest(%q, %d) = %v, want %v",
				tt.title, tt.reqSeason, got, tt.want)
		}
	}
}

// TestExtractSingleSeason verifies the helper that extracts the first Sxx
// token from a title.
func TestExtractSingleSeason(t *testing.T) {
	tests := []struct {
		title string
		want  int
	}{
		{"The.Boys.S03.COMPLETE.720p", 3},
		{"Show.S01-S05.1080p", 1}, // first Sxx token
		{"Show.S12.720p", 12},
		{"Show.s04.1080p", 4},
		{"No.Season.Here", 0},
		{"Show.S01E05.720p", 1},
		{"", 0},
	}

	for _, tt := range tests {
		got := extractSingleSeason(tt.title)
		if got != tt.want {
			t.Errorf("extractSingleSeason(%q) = %d, want %d", tt.title, got, tt.want)
		}
	}
}

func TestSeriesReleaseMatchesQuery(t *testing.T) {
	tests := []struct {
		name, query, title string
		want               bool
	}{
		{"different series", "Martin", "Martin.Mystery.S01.720p.WEB-DL", false},
		{"same series", "Martin", "Martin.S01.720p.WEB-DL", true},
		{"normalized separators", "Good Times", "good-times_s03e01.720p", true},
		{"normalized punctuation", "Good Times!", "GOOD.TIMES.S04.1080p", true},
		{"empty query abstains", "", "Other.Series.S01.720p", true},
		{"no season abstains", "Martin", "Unstructured release title", true},
		{"empty prefix abstains", "Martin", "S01.720p", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := seriesReleaseMatchesQuery(tt.query, tt.title); got != tt.want {
				t.Fatalf("seriesReleaseMatchesQuery(%q, %q) = %v, want %v", tt.query, tt.title, got, tt.want)
			}
		})
	}
}

func TestFilterSeriesTitleMatches(t *testing.T) {
	releases := []prowlarr.Release{
		{Title: "Martin.Mystery.S01.720p", GUID: "wrong"},
		{Title: "Martin.S01.720p", GUID: "right"},
		{Title: "opaque", GUID: "abstain"},
	}
	got := filterSeriesTitleMatches(releases, "Martin")
	if len(got) != 2 || got[0].GUID != "right" || got[1].GUID != "abstain" {
		t.Fatalf("filterSeriesTitleMatches returned unexpected releases: %#v", got)
	}
}

func FuzzSeriesReleaseMatchesQuery(f *testing.F) {
	f.Add("Martin", "Martin.Mystery.S01.720p")
	f.Add("Martin", "Martin.S01.720p")
	f.Add("Good Times", "Good.Times.S03E01.720p")
	f.Add("", "Other.Series.S01.720p")
	f.Add("Martin", "opaque")
	f.Fuzz(func(t *testing.T, query, title string) {
		got := seriesReleaseMatchesQuery(query, title)
		if normalizeSeriesSearchTitle(query) == "" && !got {
			t.Fatal("empty normalized query must abstain")
		}
		if singleSeasonRe.FindStringIndex(title) == nil && !got {
			t.Fatal("title without a season token must abstain")
		}
	})
}

func TestEligibleTorrentsUncachedPolicyModes(t *testing.T) {
	const cachedHash = "1111111111111111111111111111111111111111"
	const uncachedHash = "2222222222222222222222222222222222222222"
	releases := []prowlarr.Release{
		{Title: "Cached.Release.1080p", InfoHash: cachedHash, Protocol: "torrent"},
		{Title: "Uncached.Release.1080p", InfoHash: uncachedHash, Protocol: "torrent"},
	}
	tests := []struct {
		name       string
		preference []string
		wantTitles []string
		wantChecks int
	}{
		{
			name:       "deny",
			preference: []string{config.LaneTorBoxTorrent},
			wantTitles: []string{"Cached.Release.1080p"},
			wantChecks: 2,
		},
		{
			name:       "allow",
			preference: []string{config.LaneTorBoxTorrent, config.LaneUncachedTorrent},
			wantTitles: []string{"Cached.Release.1080p", "Uncached.Release.1080p"},
			wantChecks: 0,
		},
		{
			name:       "derank",
			preference: []string{config.LaneTorBoxTorrent, config.LaneUncachedTorrentDerank},
			wantTitles: []string{"Cached.Release.1080p", "Uncached.Release.1080p.DH-UNCACHED"},
			wantChecks: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oracle := newSearchAvailabilityFakeProvider("torbox", true, map[string]bool{
				cachedHash:   true,
				uncachedHash: false,
			})
			cfg := &config.Config{}
			cfg.Routing.Preference = tt.preference
			cfg.Availability.TTLHours = 72
			server := &Server{
				cfg:           cfg,
				log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
				store:         newTorrentBlacklistTestStore(t),
				prov:          oracle,
				allProviders:  map[string]provider.Provider{"torbox": oracle},
				providerOrder: []string{"torbox"},
			}
			req := httptest.NewRequest(http.MethodGet, "/api?t=search", nil)
			items := server.eligibleTorrents(req, releases, "2040", time.Now().UTC().Format(time.RFC1123Z), 0)
			titles := make([]string, 0, len(items))
			for _, item := range items {
				titles = append(titles, item.Title)
			}
			if strings.Join(titles, "|") != strings.Join(tt.wantTitles, "|") {
				t.Fatalf("titles = %v, want %v", titles, tt.wantTitles)
			}
			if checks, _, _ := oracle.stats(); checks != tt.wantChecks {
				t.Fatalf("checkcached calls = %d, want %d", checks, tt.wantChecks)
			}
		})
	}
}

func TestEligibleTorrentsDerankDoesNotLabelUnknownCacheState(t *testing.T) {
	oracle := newSearchAvailabilityFakeProvider("torbox", true, nil)
	oracle.err = errors.New("cache oracle unavailable")
	cfg := &config.Config{}
	cfg.Routing.Preference = []string{config.LaneTorBoxTorrent, config.LaneUncachedTorrentDerank}
	cfg.Availability.TTLHours = 72
	server := &Server{
		cfg:           cfg,
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:         newTorrentBlacklistTestStore(t),
		prov:          oracle,
		allProviders:  map[string]provider.Provider{"torbox": oracle},
		providerOrder: []string{"torbox"},
	}
	req := httptest.NewRequest(http.MethodGet, "/api?t=search", nil)
	items := server.eligibleTorrents(req, []prowlarr.Release{{
		Title: "Unknown.Release", InfoHash: "3333333333333333333333333333333333333333",
	}}, "2040", time.Now().UTC().Format(time.RFC1123Z), 0)
	if len(items) != 0 {
		t.Fatalf("unknown cache state surfaced as truthful result: %+v", items)
	}
}

func TestTagUncachedTitleIsStable(t *testing.T) {
	if got := tagUncachedTitle("Release.Name"); got != "Release.Name.DH-UNCACHED" {
		t.Fatalf("tagUncachedTitle() = %q", got)
	}
	if got := tagUncachedTitle("Release.Name.DH-UNCACHED"); got != "Release.Name.DH-UNCACHED" {
		t.Fatalf("tagUncachedTitle() duplicated token: %q", got)
	}
}

// TestSyntheticEmissionGuard combines the two checks to verify the full
// guard condition used in eligibleTorrents. This is the end-to-end
// classification that prevents the bug reported in production.
func TestSyntheticEmissionGuard(t *testing.T) {
	tests := []struct {
		title     string
		reqSeason int
		// eligibleAsMulti: would eligibleTorrents emit a synthetic release?
		eligibleAsMulti bool
	}{
		// Bug scenario: plain S03.COMPLETE requested against season 2.
		// Must NOT be treated as multi-season eligible.
		{"The.Boys.S03.COMPLETE.720p.BluRay", 2, false},

		// Same title requested against season 3: isMultiSeasonTitle is false,
		// so no synthetic is emitted — it passes through as-is.
		{"The.Boys.S03.COMPLETE.720p.BluRay", 3, false},

		// True multi-season pack requested for a season in range.
		{"Show.S01-S05.1080p.BluRay", 3, true},
		{"Show.S01-S05.1080p.BluRay", 5, true},

		// True multi-season pack requested for a season OUT of range.
		{"Show.S01-S05.1080p.BluRay", 6, false},

		// Complete Series: always eligible for any season.
		{"Show.Complete.Series.BluRay.1080p", 2, true},
		{"Show.Complete.Series.BluRay.1080p", 7, true},

		// reqSeason 0 means no season filter — no synthetic emission.
		{"Show.S01-S05.1080p", 0, false},

		// Plain single-season title, matching season: passes through.
		{"Show.S03.720p", 3, false},

		// Full Series variant.
		{"Show.FULL.SERIES.1080p", 4, true},
	}

	for _, tt := range tests {
		eligible := false
		if tt.reqSeason > 0 && isMultiSeasonTitle(tt.title) && seasonCoversRequest(tt.title, tt.reqSeason) {
			eligible = true
		}
		if eligible != tt.eligibleAsMulti {
			t.Errorf("syntheticGuard(%q, %d) = %v, want %v",
				tt.title, tt.reqSeason, eligible, tt.eligibleAsMulti)
		}
	}
}

// TestNZBExceedsDeepestRetention exercises NS-9.1's search-time retention
// annotation decision in isolation: it must derank only when a horizon is
// actually configured and the NZB is genuinely older than the deepest one,
// and must abstain (never guess) on every other input shape.
func TestNZBExceedsDeepestRetention(t *testing.T) {
	fixedNow := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC)
	freshDate := fixedNow.AddDate(0, 0, -10).Format(time.RFC3339)   // 10 days old
	staleDate := fixedNow.AddDate(0, 0, -4000).Format(time.RFC3339) // ~11y old

	tests := []struct {
		name        string
		retention   map[string]string // provider name -> env RETENTION_DAYS value
		publishDate string
		want        bool
	}{
		{
			name:        "no horizon configured abstains even for an ancient post",
			retention:   nil,
			publishDate: staleDate,
			want:        false,
		},
		{
			name:        "within the deepest configured horizon",
			retention:   map[string]string{"NEWSHOSTING": "4000"},
			publishDate: freshDate,
			want:        false,
		},
		{
			name:        "older than the deepest configured horizon",
			retention:   map[string]string{"NEWSHOSTING": "3000"},
			publishDate: staleDate,
			want:        true,
		},
		{
			name:        "deepest of multiple providers governs, not the shallowest",
			retention:   map[string]string{"NEWSHOSTING": "3000", "BACKUP": "3999"},
			publishDate: staleDate,
			want:        true, // age is ~4000d; max(3000,3999)=3999, 4000 > 3999
		},
		{
			name:        "empty publish date abstains",
			retention:   map[string]string{"NEWSHOSTING": "10"},
			publishDate: "",
			want:        false,
		},
		{
			name:        "unparseable publish date abstains",
			retention:   map[string]string{"NEWSHOSTING": "10"},
			publishDate: "not-a-date",
			want:        false,
		},
		{
			name:        "invalid configured horizon (out of bounds) abstains",
			retention:   map[string]string{"NEWSHOSTING": "0"},
			publishDate: staleDate,
			want:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for name, days := range tt.retention {
				t.Setenv("HARRBOR_PROVIDER_"+name+"_RETENTION_DAYS", days)
			}
			server := &Server{
				cfg:         &config.Config{},
				log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
				usenetOrder: []string{"newshosting", "backup"},
				nowFn:       func() time.Time { return fixedNow },
			}
			got := server.nzbExceedsDeepestRetention(tt.publishDate)
			if got != tt.want {
				t.Fatalf("nzbExceedsDeepestRetention(%q) = %v, want %v", tt.publishDate, got, tt.want)
			}
		})
	}
}

// TestNZBItemAppliesRetentionToken verifies nzbItem (the actual search-result
// construction seam used by eligibleNZBs) tags the title with the same
// DH-UNCACHED token TS-0.4's Arr custom format already deranks, and leaves an
// in-horizon release's title untouched.
func TestNZBItemAppliesRetentionToken(t *testing.T) {
	fixedNow := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC)
	t.Setenv("HARRBOR_PROVIDER_NEWSHOSTING_RETENTION_DAYS", "3000")
	server := &Server{
		cfg:         &config.Config{},
		log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		usenetOrder: []string{"newshosting"},
		nowFn:       func() time.Time { return fixedNow },
	}

	staleRel := prowlarr.Release{
		Title: "Old.Release.720p", GUID: "guid-1", DownloadURL: "https://prowlarr.example/dl/1",
		PublishDate: fixedNow.AddDate(0, 0, -4000).Format(time.RFC3339),
	}
	freshRel := prowlarr.Release{
		Title: "New.Release.720p", GUID: "guid-2", DownloadURL: "https://prowlarr.example/dl/2",
		PublishDate: fixedNow.AddDate(0, 0, -10).Format(time.RFC3339),
	}

	staleItem := server.nzbItem(staleRel, "5040", fixedNow.Format(time.RFC1123Z))
	if staleItem.Title != "Old.Release.720p.DH-UNCACHED" {
		t.Fatalf("stale nzb title = %q, want DH-UNCACHED-tagged", staleItem.Title)
	}

	freshItem := server.nzbItem(freshRel, "5040", fixedNow.Format(time.RFC1123Z))
	if freshItem.Title != "New.Release.720p" {
		t.Fatalf("fresh nzb title = %q, want unchanged", freshItem.Title)
	}
}

// TestSearchAvailabilityPersistsLiveVerdicts covers the coverage defect that
// kept genuine releases ranked below searchAvailabilityLiveTop permanently
// unknown. A live verdict is a direct provider oracle answer, so persisting it
// lets a later search resolve that hash from learned evidence even when the
// hash falls outside the live-refresh budget. Without persistence every search
// re-checked the same handful of leading candidates and nothing deeper could
// ever become eligible.
func TestSearchAvailabilityPersistsLiveVerdicts(t *testing.T) {
	oracle := newSearchAvailabilityFakeProvider("torbox", true, map[string]bool{
		"4444444444444444444444444444444444444444": true,
	})
	cfg := &config.Config{}
	cfg.Routing.Preference = []string{config.LaneTorBoxTorrent, config.LaneUncachedTorrentDerank}
	cfg.Availability.TTLHours = 72
	server := &Server{
		cfg:           cfg,
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:         newTorrentBlacklistTestStore(t),
		prov:          oracle,
		allProviders:  map[string]provider.Provider{"torbox": oracle},
		providerOrder: []string{"torbox"},
	}
	req := httptest.NewRequest(http.MethodGet, "/api?t=search", nil)
	hash := "4444444444444444444444444444444444444444"

	if _, checked := server.searchTorrentAvailability(req, []string{hash}); !checked[hash] {
		t.Fatal("live refresh did not resolve the hash")
	}

	observation, found, err := server.store.GetHashAvailability(req.Context(), "torbox", hash)
	if err != nil {
		t.Fatalf("GetHashAvailability: %v", err)
	}
	if !found {
		t.Fatal("live search verdict was not persisted; coverage cannot compound across searches")
	}
	if !observation.Cached {
		t.Fatal("persisted verdict lost the cached result")
	}
	if observation.Source != string(availability.SourceSearch) {
		t.Fatalf("Source = %q, want %q", observation.Source, availability.SourceSearch)
	}

	// The learned path must now resolve it without another oracle call, which
	// is what lets a deeper-ranked hash be answered on a later search.
	before, _, _ := oracle.stats()
	if verdict := server.learnedSearchAvailability(req.Context(), "torbox", hash); verdict != searchAvailabilityCached {
		t.Fatalf("learned verdict = %v, want cached", verdict)
	}
	if after, _, _ := oracle.stats(); after != before {
		t.Fatalf("learned lookup issued %d extra oracle calls", after-before)
	}
}

// TestSearchAvailabilityDoesNotPersistOracleErrors is the companion guard: a
// failed oracle call must leave no row behind, so an unknown can never be
// resurrected later as a labelled verdict.
func TestSearchAvailabilityDoesNotPersistOracleErrors(t *testing.T) {
	oracle := newSearchAvailabilityFakeProvider("torbox", true, nil)
	oracle.err = errors.New("cache oracle unavailable")
	cfg := &config.Config{}
	cfg.Routing.Preference = []string{config.LaneTorBoxTorrent, config.LaneUncachedTorrentDerank}
	cfg.Availability.TTLHours = 72
	server := &Server{
		cfg:           cfg,
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:         newTorrentBlacklistTestStore(t),
		prov:          oracle,
		allProviders:  map[string]provider.Provider{"torbox": oracle},
		providerOrder: []string{"torbox"},
	}
	req := httptest.NewRequest(http.MethodGet, "/api?t=search", nil)
	hash := "5555555555555555555555555555555555555555"

	server.searchTorrentAvailability(req, []string{hash})

	if _, found, err := server.store.GetHashAvailability(req.Context(), "torbox", hash); err != nil {
		t.Fatalf("GetHashAvailability: %v", err)
	} else if found {
		t.Fatal("an oracle error was persisted as an observation")
	}
}
