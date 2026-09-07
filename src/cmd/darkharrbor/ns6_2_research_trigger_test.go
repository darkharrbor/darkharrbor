package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/api"
	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/nntp"
	"github.com/darkharrbor/darkharrbor/internal/store"
	"github.com/darkharrbor/darkharrbor/internal/suppress"
)

// --- researchCooldown unit tests ---

func TestResearchCooldownTryClaimBasics(t *testing.T) {
	rc := newResearchCooldown()
	fixed := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	rc.SetClock(func() time.Time { return fixed })

	if !rc.tryClaim("item-1", time.Hour) {
		t.Fatal("first claim should succeed")
	}
	if rc.tryClaim("item-1", time.Hour) {
		t.Fatal("second claim within cooldown window should be refused")
	}
	if !rc.tryClaim("item-2", time.Hour) {
		t.Fatal("claim for a different item ID should succeed independently")
	}
}

func TestResearchCooldownExpiresAfterWindow(t *testing.T) {
	rc := newResearchCooldown()
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	rc.SetClock(func() time.Time { return now })

	if !rc.tryClaim("item-1", time.Hour) {
		t.Fatal("first claim should succeed")
	}
	now = now.Add(59 * time.Minute)
	if rc.tryClaim("item-1", time.Hour) {
		t.Fatal("claim just before cooldown expiry should still be refused")
	}
	now = now.Add(2 * time.Minute)
	if !rc.tryClaim("item-1", time.Hour) {
		t.Fatal("claim after cooldown window elapses should succeed")
	}
}

func TestResearchCooldownZeroDisablesGuard(t *testing.T) {
	rc := newResearchCooldown()
	if !rc.tryClaim("item-1", 0) {
		t.Fatal("first claim should succeed")
	}
	if !rc.tryClaim("item-1", 0) {
		t.Fatal("cooldown<=0 must never refuse a claim")
	}
}

// TestResearchCooldownConcurrentClaimsRaceSafe is this row's DG-07
// bounded-goroutine/race proof: many goroutines racing tryClaim for the
// SAME item ID at once must yield exactly one winner, with -race clean.
func TestResearchCooldownConcurrentClaimsRaceSafe(t *testing.T) {
	rc := newResearchCooldown()
	const n = 50
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rc.tryClaim("same-item", time.Hour) {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("concurrent claims for the same item: wins = %d, want exactly 1", wins)
	}
}

// --- shouldTriggerReSearch gate tests ---

func TestShouldTriggerReSearchRequiresTwoConsecutiveConfirmations(t *testing.T) {
	tests := []struct {
		name       string
		decayed    bool
		wasDecayed bool
		want       bool
	}{
		{"first-ever decayed pass (never audited before) does not trigger", true, false, false},
		{"healthy pass never triggers regardless of prior state", false, true, false},
		{"healthy-then-healthy never triggers", false, false, false},
		{"two consecutive decayed passes trigger", true, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldTriggerReSearch(tt.decayed, tt.wasDecayed); got != tt.want {
				t.Fatalf("shouldTriggerReSearch(decayed=%v, wasDecayed=%v) = %v, want %v", tt.decayed, tt.wasDecayed, got, tt.want)
			}
		})
	}
}

func TestShouldTriggerReSearchFiltersRealObservedFlipFlop(t *testing.T) {
	wasDecayed := false
	decayed := true
	if shouldTriggerReSearch(decayed, wasDecayed) {
		t.Fatal("pass 1 (first-ever decayed observation) must not trigger")
	}
	wasDecayed = decayed

	decayed = false
	if shouldTriggerReSearch(decayed, wasDecayed) {
		t.Fatal("pass 2 (recovered) must not trigger")
	}
	wasDecayed = decayed

	decayed = true
	if shouldTriggerReSearch(decayed, wasDecayed) {
		t.Fatal("pass 3 (first decayed reading after a recovery) must not trigger")
	}
	wasDecayed = decayed

	decayed = true
	if !shouldTriggerReSearch(decayed, wasDecayed) {
		t.Fatal("pass 4 (second consecutive decayed reading) must trigger")
	}
}

// --- resolveDecayedItemIdentity tests ---

func TestResolveDecayedItemIdentityPrefersDurableMetadata(t *testing.T) {
	item := &store.Item{
		Metadata: store.SubmissionMetadata{
			ProviderIdentity: &store.ProviderIdentity{Kind: "movie", IDs: store.ProviderIDs{TMDB: "939243"}},
		},
	}
	fakeStore := &fakeTerminalFailureStore{}
	got := resolveDecayedItemIdentity(context.Background(), terminalFailureTestLogger(), terminalFailureStoreAdapter{fakeStore}, item)
	if got == nil || got.IDs.TMDB != "939243" {
		t.Fatalf("got %+v, want the durable metadata identity used directly (no fallback lookup)", got)
	}
	if fakeStore.grabIdentityCalls != 0 {
		t.Fatalf("grab-cache fallback should not even be attempted when durable metadata is present, got %d calls", fakeStore.grabIdentityCalls)
	}
}

func TestResolveDecayedItemIdentityFallsBackToGrabCache(t *testing.T) {
	source := "<nzb>real-nzb-bytes</nzb>"
	key := nntp.ContentKey([]byte(source))
	fakeStore := &fakeTerminalFailureStore{
		grabIdentity: map[string]*store.ProviderIdentity{
			key: {Kind: "series", IDs: store.ProviderIDs{TVDB: "77992"}},
		},
	}
	item := &store.Item{SourceURI: &source}
	got := resolveDecayedItemIdentity(context.Background(), terminalFailureTestLogger(), terminalFailureStoreAdapter{fakeStore}, item)
	if got == nil || got.IDs.TVDB != "77992" {
		t.Fatalf("got %+v, want the grab-cache fallback identity, keyed by nntp.ContentKey(SourceURI)", got)
	}
	if fakeStore.grabIdentityCalls != 1 {
		t.Fatalf("grab-cache calls = %d, want 1", fakeStore.grabIdentityCalls)
	}
}

func TestResolveDecayedItemIdentityAbstainsWhenNeitherSourceHasUsableIdentity(t *testing.T) {
	tests := []struct {
		name string
		item *store.Item
	}{
		{"no metadata, no source URI", &store.Item{}},
		{"metadata present but unknown kind", &store.Item{Metadata: store.SubmissionMetadata{ProviderIdentity: &store.ProviderIdentity{Kind: "unknown"}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeStore := &fakeTerminalFailureStore{}
			got := resolveDecayedItemIdentity(context.Background(), terminalFailureTestLogger(), terminalFailureStoreAdapter{fakeStore}, tt.item)
			if got != nil {
				t.Fatalf("got %+v, want nil (abstain, never guess)", got)
			}
		})
	}
}

func TestResolveDecayedItemIdentityAbstainsOnGrabCacheError(t *testing.T) {
	source := "<nzb>real-nzb-bytes</nzb>"
	fakeStore := &fakeTerminalFailureStore{grabIdentityErr: errors.New("cache read failed")}
	item := &store.Item{SourceURI: &source}
	got := resolveDecayedItemIdentity(context.Background(), terminalFailureTestLogger(), terminalFailureStoreAdapter{fakeStore}, item)
	if got != nil {
		t.Fatalf("got %+v, want nil on a fallback lookup error (non-fatal abstain)", got)
	}
}

// --- resolveArrSearchTarget tests ---

func seriesLookupServer(t *testing.T, seriesID int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seriesID <= 0 {
			w.Write([]byte(`[]`))
			return
		}
		w.Write([]byte(`[{"id":` + strconv.Itoa(seriesID) + `}]`))
	}))
}

func movieLookupServer(t *testing.T, movieID int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if movieID <= 0 {
			w.Write([]byte(`[]`))
			return
		}
		w.Write([]byte(`[{"id":` + strconv.Itoa(movieID) + `}]`))
	}))
}

func TestResolveArrSearchTargetSeriesSingleMatch(t *testing.T) {
	sonarr := seriesLookupServer(t, 20)
	defer sonarr.Close()
	radarr := seriesLookupServer(t, 0) // does not own this series
	defer radarr.Close()

	arrs := []config.ArrTarget{
		{Name: "radarr", BaseURL: radarr.URL, APIKey: "k1"},
		{Name: "sonarr-cartoons", BaseURL: sonarr.URL, APIKey: "k2"},
	}
	identity := &store.ProviderIdentity{Kind: "series", IDs: store.ProviderIDs{TVDB: "77992"}}

	target, ok := resolveArrSearchTarget(context.Background(), terminalFailureTestLogger(), arrs, identity)
	if !ok {
		t.Fatal("expected exactly one match to resolve")
	}
	if target.arr.Name != "sonarr-cartoons" || target.internalID != 20 || target.kind != "series" {
		t.Fatalf("got %+v, want sonarr-cartoons/20/series", target)
	}
}

func TestResolveArrSearchTargetMovieSingleMatch(t *testing.T) {
	radarr := movieLookupServer(t, 116)
	defer radarr.Close()

	arrs := []config.ArrTarget{{Name: "radarr", BaseURL: radarr.URL, APIKey: "k1"}}
	identity := &store.ProviderIdentity{Kind: "movie", IDs: store.ProviderIDs{TMDB: "939243"}}

	target, ok := resolveArrSearchTarget(context.Background(), terminalFailureTestLogger(), arrs, identity)
	if !ok {
		t.Fatal("expected exactly one match to resolve")
	}
	if target.arr.Name != "radarr" || target.internalID != 116 || target.kind != "movie" {
		t.Fatalf("got %+v, want radarr/116/movie", target)
	}
}

func TestResolveArrSearchTargetAbstainsOnNoMatch(t *testing.T) {
	sonarr := seriesLookupServer(t, 0)
	defer sonarr.Close()
	arrs := []config.ArrTarget{{Name: "sonarr-cartoons", BaseURL: sonarr.URL, APIKey: "k"}}
	identity := &store.ProviderIdentity{Kind: "series", IDs: store.ProviderIDs{TVDB: "999999"}}

	if _, ok := resolveArrSearchTarget(context.Background(), terminalFailureTestLogger(), arrs, identity); ok {
		t.Fatal("expected no match to abstain")
	}
}

func TestResolveArrSearchTargetAbstainsOnAmbiguousMatch(t *testing.T) {
	sonarrA := seriesLookupServer(t, 20)
	defer sonarrA.Close()
	sonarrB := seriesLookupServer(t, 21)
	defer sonarrB.Close()
	arrs := []config.ArrTarget{
		{Name: "sonarr-a", BaseURL: sonarrA.URL, APIKey: "k1"},
		{Name: "sonarr-b", BaseURL: sonarrB.URL, APIKey: "k2"},
	}
	identity := &store.ProviderIdentity{Kind: "series", IDs: store.ProviderIDs{TVDB: "77992"}}

	if _, ok := resolveArrSearchTarget(context.Background(), terminalFailureTestLogger(), arrs, identity); ok {
		t.Fatal("expected an ambiguous (two-instance) match to abstain, not guess")
	}
}

func TestResolveArrSearchTargetAbstainsWithoutUsableID(t *testing.T) {
	tests := []struct {
		name     string
		identity *store.ProviderIdentity
	}{
		{"nil identity", nil},
		{"unknown kind", &store.ProviderIdentity{Kind: "unknown"}},
		{"series with no tvdb id", &store.ProviderIdentity{Kind: "series"}},
		{"movie with no tmdb id", &store.ProviderIdentity{Kind: "movie"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := resolveArrSearchTarget(context.Background(), terminalFailureTestLogger(), nil, tt.identity); ok {
				t.Fatal("expected abstain without a usable id")
			}
		})
	}
}

// --- fireArrSearchCommand tests ---

func TestFireArrSearchCommandSeries(t *testing.T) {
	var gotBody string
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		buf := make([]byte, 256)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		if r.Header.Get("X-Api-Key") != "secret-key" {
			t.Errorf("X-Api-Key header = %q, want secret-key", r.Header.Get("X-Api-Key"))
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	fireArrSearchCommand(context.Background(), terminalFailureTestLogger(), arrSearchTarget{
		arr:        config.ArrTarget{Name: "sonarr-cartoons", BaseURL: srv.URL, APIKey: "secret-key"},
		internalID: 20,
		kind:       "series",
	})

	if gotPath != "/api/v3/command" {
		t.Fatalf("path = %q, want /api/v3/command", gotPath)
	}
	if !strings.Contains(gotBody, `"name":"SeriesSearch"`) || !strings.Contains(gotBody, `"seriesId":20`) {
		t.Fatalf("body = %q, want SeriesSearch command with seriesId 20", gotBody)
	}
}

func TestFireArrSearchCommandMovie(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 256)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	fireArrSearchCommand(context.Background(), terminalFailureTestLogger(), arrSearchTarget{
		arr:        config.ArrTarget{Name: "radarr", BaseURL: srv.URL, APIKey: "k"},
		internalID: 116,
		kind:       "movie",
	})

	if !strings.Contains(gotBody, `"name":"MoviesSearch"`) || !strings.Contains(gotBody, `"movieIds":[116]`) {
		t.Fatalf("body = %q, want MoviesSearch command with movieIds [116]", gotBody)
	}
}

func TestFireArrSearchCommandUnreachableArrIsNonFatal(t *testing.T) {
	// Points at a closed connection; must not panic and must simply log.
	fireArrSearchCommand(context.Background(), terminalFailureTestLogger(), arrSearchTarget{
		arr:        config.ArrTarget{Name: "unreachable", BaseURL: "http://127.0.0.1:1", APIKey: "k"},
		internalID: 1,
		kind:       "movie",
	})
}

// --- triggerReSearchOnDecay integration tests (blacklist/suppress + targeted search, .strm untouched) ---

// movieArrServer builds a mock Radarr standing in for all three real calls
// triggerReSearchOnDecay's movie path makes: GET /api/v3/movie?tmdbId= for
// resolveArrSearchTarget, GET /api/v3/movie/{id} for arrFileIDForTarget, and
// DELETE /api/v3/moviefile/{id} for markArrFileMissing, plus POST
// /api/v3/command for fireArrSearchCommand -- routed by method+path, exactly
// matching the four real distinct calls verified live against a real Radarr
// this session.
func movieArrServer(t *testing.T, movieID, movieFileID int, hasFile bool) (*httptest.Server, *bool, *string) {
	t.Helper()
	searchFired := new(bool)
	searchBody := new(string)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/movie":
			fmt.Fprintf(w, `[{"id":%d}]`, movieID)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v3/movie/"):
			fmt.Fprintf(w, `{"hasFile":%v,"movieFileId":%d}`, hasFile, movieFileID)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v3/moviefile/"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/command":
			*searchFired = true
			buf := make([]byte, 256)
			n, _ := r.Body.Read(buf)
			*searchBody = string(buf[:n])
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	return srv, searchFired, searchBody
}

func TestTriggerReSearchOnDecayBlacklistsSuppressesMarksMissingAndFiresTargetedSearch(t *testing.T) {
	srv, searchFired, searchBody := movieArrServer(t, 116, 190, true)
	defer srv.Close()

	source := "<nzb>decayed-sample</nzb>"
	item := &store.Item{
		ID:          "item-decayed-1",
		DisplayName: "Some.Movie.2024",
		TotalSize:   12345,
		SourceURI:   &source,
		Metadata: store.SubmissionMetadata{
			ProviderIdentity: &store.ProviderIdentity{Kind: "movie", IDs: store.ProviderIDs{TMDB: "939243"}},
		},
	}
	fakeStore := &fakeTerminalFailureStore{}
	rc := newResearchCooldown()
	arrs := []config.ArrTarget{{Name: "radarr", BaseURL: srv.URL, APIKey: "k"}}

	triggerReSearchOnDecay(
		context.Background(),
		terminalFailureTestLogger(),
		terminalFailureStoreAdapter{fakeStore},
		item,
		arrs,
		rc,
		time.Hour,
		2*time.Hour,
	)

	if fakeStore.blacklistCalls != 1 {
		t.Fatalf("blacklist calls = %d, want 1", fakeStore.blacklistCalls)
	}
	if want := api.NZBBlacklistKey(source); fakeStore.blacklistKey != want {
		t.Fatalf("blacklist key = %q, want %q", fakeStore.blacklistKey, want)
	}
	if fakeStore.suppressCalls != 1 {
		t.Fatalf("suppression calls = %d, want 1", fakeStore.suppressCalls)
	}
	if want := suppress.Fingerprint(item.DisplayName, item.TotalSize, suppress.LaneNZB); fakeStore.suppressFP != want {
		t.Fatalf("suppression fingerprint = %q, want %q", fakeStore.suppressFP, want)
	}
	if fakeStore.suppressRsn != string(suppress.ReasonDeadPost) {
		t.Fatalf("suppression reason = %q, want %q (reused, not a new vocabulary entry)", fakeStore.suppressRsn, suppress.ReasonDeadPost)
	}
	if fakeStore.suppressTTL != 2*time.Hour {
		t.Fatalf("suppression ttl = %v, want %v", fakeStore.suppressTTL, 2*time.Hour)
	}
	// The locked constraint under test: NEVER StateFailed, NEVER touch the
	// .strm. This function must not even attempt UpdateItemState.
	if fakeStore.updateCalls != 0 {
		t.Fatalf("state update calls = %d, want 0 -- the item must stay Ready and its .strm must never be touched", fakeStore.updateCalls)
	}
	if !*searchFired {
		t.Fatal("expected a targeted Arr search command to fire")
	}
	if !strings.Contains(*searchBody, `"name":"MoviesSearch"`) || !strings.Contains(*searchBody, `"movieIds":[116]`) {
		t.Fatalf("search body = %q, want a MoviesSearch command scoped to movieIds:[116]", *searchBody)
	}
}

// TestTriggerReSearchOnDecaySkipsSearchWhenMarkMissingFails is the direct
// regression for this session's live finding: firing a search WITHOUT first
// telling the Arr the file is gone is a no-op for content the Arr still
// believes it has, so a failed mark-missing must skip the search entirely
// rather than fire it anyway.
func TestTriggerReSearchOnDecaySkipsSearchWhenMarkMissingFails(t *testing.T) {
	var searchFired bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/movie":
			w.Write([]byte(`[{"id":116}]`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v3/movie/"):
			w.Write([]byte(`{"hasFile":true,"movieFileId":190}`))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusInternalServerError) // the delete itself fails
		case r.Method == http.MethodPost:
			searchFired = true
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer srv.Close()

	source := "<nzb>decayed-sample</nzb>"
	item := &store.Item{
		ID:        "item-decayed-markfail",
		SourceURI: &source,
		Metadata:  store.SubmissionMetadata{ProviderIdentity: &store.ProviderIdentity{Kind: "movie", IDs: store.ProviderIDs{TMDB: "939243"}}},
	}
	fakeStore := &fakeTerminalFailureStore{}
	rc := newResearchCooldown()
	arrs := []config.ArrTarget{{Name: "radarr", BaseURL: srv.URL, APIKey: "k"}}

	triggerReSearchOnDecay(context.Background(), terminalFailureTestLogger(), terminalFailureStoreAdapter{fakeStore}, item, arrs, rc, time.Hour, time.Hour)

	if searchFired {
		t.Fatal("search must not fire when marking the arr's file missing failed -- it would be a no-op")
	}
	// Blacklist/suppress happen before the arr calls and must still occur.
	if fakeStore.blacklistCalls != 1 || fakeStore.suppressCalls != 1 {
		t.Fatalf("blacklist/suppress should still happen: blacklist=%d suppress=%d", fakeStore.blacklistCalls, fakeStore.suppressCalls)
	}
}

// TestTriggerReSearchOnDecayFiresSearchWhenAlreadyMissing confirms the
// already-missing shortcut: if the Arr's own bookkeeping already shows
// hasFile=false, there is nothing to delete, but the search is still
// worthwhile and must still fire.
func TestTriggerReSearchOnDecayFiresSearchWhenAlreadyMissing(t *testing.T) {
	var deleteAttempted, searchFired bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/movie":
			w.Write([]byte(`[{"id":116}]`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v3/movie/"):
			w.Write([]byte(`{"hasFile":false,"movieFileId":0}`))
		case r.Method == http.MethodDelete:
			deleteAttempted = true
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost:
			searchFired = true
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer srv.Close()

	source := "<nzb>decayed-sample</nzb>"
	item := &store.Item{
		ID:        "item-decayed-alreadymissing",
		SourceURI: &source,
		Metadata:  store.SubmissionMetadata{ProviderIdentity: &store.ProviderIdentity{Kind: "movie", IDs: store.ProviderIDs{TMDB: "939243"}}},
	}
	fakeStore := &fakeTerminalFailureStore{}
	rc := newResearchCooldown()
	arrs := []config.ArrTarget{{Name: "radarr", BaseURL: srv.URL, APIKey: "k"}}

	triggerReSearchOnDecay(context.Background(), terminalFailureTestLogger(), terminalFailureStoreAdapter{fakeStore}, item, arrs, rc, time.Hour, time.Hour)

	if deleteAttempted {
		t.Fatal("must not attempt to delete a file record that already doesn't exist")
	}
	if !searchFired {
		t.Fatal("search must still fire when the arr already considers the file missing")
	}
}

func TestTriggerReSearchOnDecayCooldownPreventsSecondTrigger(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/movie":
			w.Write([]byte(`[{"id":116}]`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v3/movie/"):
			w.Write([]byte(`{"hasFile":true,"movieFileId":190}`))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer srv.Close()

	source := "<nzb>decayed-sample</nzb>"
	item := &store.Item{
		ID:        "item-decayed-2",
		SourceURI: &source,
		Metadata:  store.SubmissionMetadata{ProviderIdentity: &store.ProviderIdentity{Kind: "movie", IDs: store.ProviderIDs{TMDB: "939243"}}},
	}
	fakeStore := &fakeTerminalFailureStore{}
	rc := newResearchCooldown()
	arrs := []config.ArrTarget{{Name: "radarr", BaseURL: srv.URL, APIKey: "k"}}

	for i := 0; i < 2; i++ {
		triggerReSearchOnDecay(context.Background(), terminalFailureTestLogger(), terminalFailureStoreAdapter{fakeStore}, item, arrs, rc, time.Hour, time.Hour)
	}

	if fakeStore.blacklistCalls != 1 || fakeStore.suppressCalls != 1 {
		t.Fatalf("second call within cooldown must be a full no-op: blacklist=%d suppress=%d", fakeStore.blacklistCalls, fakeStore.suppressCalls)
	}
	// One GET (target lookup) + one GET (file-id lookup) + one DELETE +
	// one POST (command) for the FIRST call only.
	if calls != 4 {
		t.Fatalf("arr HTTP calls = %d, want 4 (lookup+file-lookup+delete+command, only on the first trigger)", calls)
	}
}

func TestTriggerReSearchOnDecayAbstainsFromSearchWithoutIdentityButStillSuppresses(t *testing.T) {
	source := "<nzb>decayed-sample</nzb>"
	item := &store.Item{ID: "item-decayed-3", SourceURI: &source} // no identity anywhere
	fakeStore := &fakeTerminalFailureStore{}
	rc := newResearchCooldown()

	triggerReSearchOnDecay(context.Background(), terminalFailureTestLogger(), terminalFailureStoreAdapter{fakeStore}, item, nil, rc, time.Hour, time.Hour)

	if fakeStore.blacklistCalls != 1 || fakeStore.suppressCalls != 1 {
		t.Fatalf("blacklist/suppress must still happen without identity: blacklist=%d suppress=%d", fakeStore.blacklistCalls, fakeStore.suppressCalls)
	}
	if fakeStore.updateCalls != 0 {
		t.Fatalf("state update calls = %d, want 0", fakeStore.updateCalls)
	}
}

func TestTriggerReSearchOnDecayNoSourceURIStillAbstainsFromBlacklist(t *testing.T) {
	item := &store.Item{ID: "item-decayed-4"} // no SourceURI: malformed/absent input
	fakeStore := &fakeTerminalFailureStore{}
	rc := newResearchCooldown()

	triggerReSearchOnDecay(context.Background(), terminalFailureTestLogger(), terminalFailureStoreAdapter{fakeStore}, item, nil, rc, time.Hour, time.Hour)

	if fakeStore.blacklistCalls != 0 || fakeStore.suppressCalls != 0 {
		t.Fatalf("blacklist/suppression calls = %d/%d, want 0/0 (no SourceURI to key on -- abstain, not guess)", fakeStore.blacklistCalls, fakeStore.suppressCalls)
	}
	if fakeStore.updateCalls != 0 {
		t.Fatalf("state update calls = %d, want 0 -- this function must never fail the item", fakeStore.updateCalls)
	}
}

func TestTriggerReSearchOnDecayNilItemIsNoOp(t *testing.T) {
	fakeStore := &fakeTerminalFailureStore{}
	rc := newResearchCooldown()

	triggerReSearchOnDecay(context.Background(), terminalFailureTestLogger(), terminalFailureStoreAdapter{fakeStore}, nil, nil, rc, time.Hour, time.Hour)

	if fakeStore.blacklistCalls != 0 || fakeStore.updateCalls != 0 {
		t.Fatal("nil item must produce zero side effects")
	}
}

func TestTriggerReSearchOnDecayStillAttemptsSearchWhenBlacklistOrSuppressionFail(t *testing.T) {
	var searchFired bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/movie":
			w.Write([]byte(`[{"id":116}]`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v3/movie/"):
			w.Write([]byte(`{"hasFile":true,"movieFileId":190}`))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost:
			searchFired = true
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer srv.Close()

	source := "<nzb>decayed-sample</nzb>"
	fakeStore := &fakeTerminalFailureStore{blacklistErr: errors.New("blacklist failed"), suppressErr: errors.New("suppress failed")}
	rc := newResearchCooldown()
	arrs := []config.ArrTarget{{Name: "radarr", BaseURL: srv.URL, APIKey: "k"}}
	item := &store.Item{
		ID:        "item-decayed-5",
		SourceURI: &source,
		Metadata:  store.SubmissionMetadata{ProviderIdentity: &store.ProviderIdentity{Kind: "movie", IDs: store.ProviderIDs{TMDB: "939243"}}},
	}

	triggerReSearchOnDecay(context.Background(), terminalFailureTestLogger(), terminalFailureStoreAdapter{fakeStore}, item, arrs, rc, time.Hour, time.Hour)

	if fakeStore.blacklistCalls != 1 || fakeStore.suppressCalls != 1 {
		t.Fatalf("blacklist/suppression should still be attempted: blacklist=%d suppress=%d", fakeStore.blacklistCalls, fakeStore.suppressCalls)
	}
	if !searchFired {
		t.Fatal("a blacklist/suppression persistence failure must not skip the targeted Arr search")
	}
}

// --- arrFileIDForTarget / markArrFileMissing direct unit tests ---

func TestArrFileIDForTargetSeriesMatchesExactEpisode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("seasonNumber") != "1" {
			t.Errorf("seasonNumber = %q, want 1", r.URL.Query().Get("seasonNumber"))
		}
		w.Write([]byte(`[
			{"episodeNumber":12,"hasFile":true,"episodeFileId":100},
			{"episodeNumber":13,"hasFile":true,"episodeFileId":16504},
			{"episodeNumber":14,"hasFile":false,"episodeFileId":0}
		]`))
	}))
	defer srv.Close()

	target := arrSearchTarget{arr: config.ArrTarget{BaseURL: srv.URL, APIKey: "k"}, internalID: 7, kind: "series"}
	fileID, hasFile, ok := arrFileIDForTarget(context.Background(), &http.Client{}, terminalFailureTestLogger(), target, 1, 13)
	if !ok || !hasFile || fileID != 16504 {
		t.Fatalf("got fileID=%d hasFile=%v ok=%v, want 16504/true/true", fileID, hasFile, ok)
	}
}

func TestArrFileIDForTargetSeriesAbstainsOnAmbiguousSeasonEpisode(t *testing.T) {
	target := arrSearchTarget{arr: config.ArrTarget{BaseURL: "http://unused", APIKey: "k"}, internalID: 7, kind: "series"}
	if _, _, ok := arrFileIDForTarget(context.Background(), &http.Client{}, terminalFailureTestLogger(), target, 0, 13); ok {
		t.Fatal("season 0 must abstain without ever making a request")
	}
	if _, _, ok := arrFileIDForTarget(context.Background(), &http.Client{}, terminalFailureTestLogger(), target, 1, 0); ok {
		t.Fatal("episode 0 must abstain without ever making a request")
	}
}

func TestArrFileIDForTargetSeriesAbstainsWhenEpisodeNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"episodeNumber":1,"hasFile":true,"episodeFileId":5}]`))
	}))
	defer srv.Close()
	target := arrSearchTarget{arr: config.ArrTarget{BaseURL: srv.URL, APIKey: "k"}, internalID: 7, kind: "series"}
	if _, _, ok := arrFileIDForTarget(context.Background(), &http.Client{}, terminalFailureTestLogger(), target, 1, 99); ok {
		t.Fatal("an episode number absent from the arr's own list must abstain, never guess")
	}
}

func TestArrFileIDForTargetMovie(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/movie/116" {
			t.Errorf("path = %q, want /api/v3/movie/116", r.URL.Path)
		}
		w.Write([]byte(`{"hasFile":true,"movieFileId":190}`))
	}))
	defer srv.Close()
	target := arrSearchTarget{arr: config.ArrTarget{BaseURL: srv.URL, APIKey: "k"}, internalID: 116, kind: "movie"}
	fileID, hasFile, ok := arrFileIDForTarget(context.Background(), &http.Client{}, terminalFailureTestLogger(), target, 0, 0)
	if !ok || !hasFile || fileID != 190 {
		t.Fatalf("got fileID=%d hasFile=%v ok=%v, want 190/true/true", fileID, hasFile, ok)
	}
}

func TestMarkArrFileMissingSeriesDeletesCorrectID(t *testing.T) {
	var deletedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			w.Write([]byte(`[{"episodeNumber":18,"hasFile":true,"episodeFileId":16509}]`))
		case r.Method == http.MethodDelete:
			deletedPath = r.URL.Path
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	target := arrSearchTarget{arr: config.ArrTarget{BaseURL: srv.URL, APIKey: "k"}, internalID: 7, kind: "series"}
	if !markArrFileMissing(context.Background(), terminalFailureTestLogger(), target, 1, 18) {
		t.Fatal("expected markArrFileMissing to succeed")
	}
	if deletedPath != "/api/v3/episodefile/16509" {
		t.Fatalf("deleted path = %q, want /api/v3/episodefile/16509", deletedPath)
	}
}

func TestMarkArrFileMissingMovieDeletesCorrectID(t *testing.T) {
	var deletedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			w.Write([]byte(`{"hasFile":true,"movieFileId":190}`))
		case r.Method == http.MethodDelete:
			deletedPath = r.URL.Path
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	target := arrSearchTarget{arr: config.ArrTarget{BaseURL: srv.URL, APIKey: "k"}, internalID: 116, kind: "movie"}
	if !markArrFileMissing(context.Background(), terminalFailureTestLogger(), target, 0, 0) {
		t.Fatal("expected markArrFileMissing to succeed")
	}
	if deletedPath != "/api/v3/moviefile/190" {
		t.Fatalf("deleted path = %q, want /api/v3/moviefile/190", deletedPath)
	}
}

func TestMarkArrFileMissingReturnsTrueWhenAlreadyMissingWithoutDeleting(t *testing.T) {
	deleteAttempted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			w.Write([]byte(`{"hasFile":false,"movieFileId":0}`))
		case r.Method == http.MethodDelete:
			deleteAttempted = true
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	target := arrSearchTarget{arr: config.ArrTarget{BaseURL: srv.URL, APIKey: "k"}, internalID: 116, kind: "movie"}
	if !markArrFileMissing(context.Background(), terminalFailureTestLogger(), target, 0, 0) {
		t.Fatal("expected true (nothing to delete, but search still worthwhile)")
	}
	if deleteAttempted {
		t.Fatal("must not attempt a delete when the arr already has no file")
	}
}

func TestMarkArrFileMissingReturnsFalseOnDeleteFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			w.Write([]byte(`{"hasFile":true,"movieFileId":190}`))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	target := arrSearchTarget{arr: config.ArrTarget{BaseURL: srv.URL, APIKey: "k"}, internalID: 116, kind: "movie"}
	if markArrFileMissing(context.Background(), terminalFailureTestLogger(), target, 0, 0) {
		t.Fatal("expected false when the arr's delete call fails")
	}
}
