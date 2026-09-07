package reactivecommit

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

type addOwnerCapture struct {
	lookupPath string
	addBody    map[string]any
}

// rx94AddOwnerServer answers the Arr lookup with exactly one candidate and
// records what addOwner asked for and what it posted.
func rx94AddOwnerServer(t *testing.T, kind string, capture *addOwnerCapture) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "lookup"):
			capture.lookupPath = r.URL.Path + "?" + r.URL.RawQuery
			if kind == "series" {
				_, _ = w.Write([]byte(`[{"title":"Show","tvdbId":121361}]`))
				return
			}
			_, _ = w.Write([]byte(`{"title":"Film","tmdbId":653}`))
		case r.Method == http.MethodPost:
			data, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			_ = json.Unmarshal(data, &capture.addBody)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":77,"monitored":false}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// RD-34 D4: the Stremio coordinate is IMDb-only, so without an IMDb lookup
// addOwner fails on every captured identity the moment RX-9.4 reaches it.
func TestAddOwnerLooksUpByIMDbWhenThatIsAllTheIdentityHas(t *testing.T) {
	for _, tc := range []struct {
		kind       string
		identity   *store.ProviderIdentity
		wantSubstr string
	}{
		{
			kind:       "series",
			identity:   &store.ProviderIdentity{Kind: "series", IDs: store.ProviderIDs{IMDB: "tt0944947"}},
			wantSubstr: "term=imdb:tt0944947",
		},
		{
			kind:       "movie",
			identity:   &store.ProviderIdentity{Kind: "movie", IDs: store.ProviderIDs{IMDB: "tt0013442"}},
			wantSubstr: "imdbId=tt0013442",
		},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			capture := &addOwnerCapture{}
			srv := rx94AddOwnerServer(t, tc.kind, capture)
			client := NewArrClient([]config.ArrTarget{{Name: "arr", BaseURL: srv.URL, APIKey: "secret"}})

			owner, err := client.addOwner(t.Context(), CommitRequest{
				Identity: tc.identity, TargetName: "arr",
				RootFolder: "/data", QualityProfile: 3,
			})
			if err != nil {
				t.Fatalf("addOwner: %v", err)
			}
			if owner.id != 77 {
				t.Fatalf("owner id = %d", owner.id)
			}
			if !strings.Contains(capture.lookupPath, tc.wantSubstr) {
				t.Fatalf("lookup %q does not use the IMDb coordinate %q", capture.lookupPath, tc.wantSubstr)
			}
			if capture.addBody["rootFolderPath"] != "/data" {
				t.Fatalf("root folder not applied: %+v", capture.addBody)
			}
			// RD-30's monitoring contract is UNCHANGED by RX-9.4.
			if tc.kind == "series" {
				if capture.addBody["monitored"] != false {
					t.Fatal("a series must be added UNMONITORED")
				}
				opts, ok := capture.addBody["addOptions"].(map[string]any)
				if !ok || opts["monitor"] != "none" || opts["searchForMissingEpisodes"] != false {
					t.Fatalf("series addOptions changed: %+v", capture.addBody["addOptions"])
				}
			}
		})
	}
}

// A TVDB/TMDB value still wins where present; IMDb is only the fallback.
func TestAddOwnerPrefersNativeIDOverIMDb(t *testing.T) {
	capture := &addOwnerCapture{}
	srv := rx94AddOwnerServer(t, "series", capture)
	client := NewArrClient([]config.ArrTarget{{Name: "arr", BaseURL: srv.URL, APIKey: "secret"}})

	if _, err := client.addOwner(t.Context(), CommitRequest{
		Identity:   &store.ProviderIdentity{Kind: "series", IDs: store.ProviderIDs{TVDB: "121361", IMDB: "tt0944947"}},
		TargetName: "arr", RootFolder: "/data", QualityProfile: 3,
	}); err != nil {
		t.Fatalf("addOwner: %v", err)
	}
	if !strings.Contains(capture.lookupPath, "term=tvdb:121361") {
		t.Fatalf("native TVDB id must win, got %q", capture.lookupPath)
	}
}

// An identity carrying NO usable lookup key must refuse rather than guess.
func TestAddOwnerRefusesIdentityWithNoLookupKey(t *testing.T) {
	capture := &addOwnerCapture{}
	srv := rx94AddOwnerServer(t, "movie", capture)
	client := NewArrClient([]config.ArrTarget{{Name: "arr", BaseURL: srv.URL, APIKey: "secret"}})

	if _, err := client.addOwner(t.Context(), CommitRequest{
		Identity: &store.ProviderIdentity{Kind: "movie"}, TargetName: "arr",
		RootFolder: "/data", QualityProfile: 3,
	}); err == nil {
		t.Fatal("want refusal for an identity with no lookup key")
	}
	if capture.addBody != nil {
		t.Fatalf("nothing may be added: %+v", capture.addBody)
	}
}

// Unchanged pre-RX-9.4 refusal: no root folder or profile means no add.
func TestAddOwnerStillRequiresRootFolderAndProfile(t *testing.T) {
	client := NewArrClient([]config.ArrTarget{{Name: "arr", BaseURL: "http://127.0.0.1:1", APIKey: "secret"}})
	if _, err := client.addOwner(t.Context(), CommitRequest{
		Identity:   &store.ProviderIdentity{Kind: "movie", IDs: store.ProviderIDs{IMDB: "tt0013442"}},
		TargetName: "arr",
	}); err == nil {
		t.Fatal("want refusal without root folder and quality profile")
	}
}

// RD-34 D6: findOwner runs BEFORE addOwner and used to hard-fail on an
// identity carrying no TVDB/TMDB, so a captured Stremio identity could never
// reach the add path at all. It must now match on imdbId, and must filter the
// listed resource down to the exact coordinate rather than trusting every row.
func TestFindOwnerMatchesByIMDbAndFiltersOtherRows(t *testing.T) {
	for _, tc := range []struct {
		kind      string
		resource  string
		body      string
		wantFound bool
	}{
		{"series", "/api/v3/series", `[{"id":5,"imdbId":"tt9999999"},{"id":9,"imdbId":"tt0944947"}]`, true},
		{"movie", "/api/v3/movie", `[{"id":3,"imdbId":"tt0013442"}]`, true},
		{"series", "/api/v3/series", `[{"id":5,"imdbId":"tt9999999"}]`, false},
	} {
		t.Run(tc.kind+"/"+tc.body[:12], func(t *testing.T) {
			var listed string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				listed = r.URL.Path + "?" + r.URL.RawQuery
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			client := NewArrClient([]config.ArrTarget{{Name: "arr", BaseURL: srv.URL, APIKey: "secret"}})

			imdb := "tt0944947"
			if tc.kind == "movie" {
				imdb = "tt0013442"
			}
			owner, found, err := client.findOwner(t.Context(), &store.ProviderIdentity{
				Kind: tc.kind, IDs: store.ProviderIDs{IMDB: imdb},
			})
			if err != nil {
				t.Fatalf("findOwner must not fail on an IMDb-only identity: %v", err)
			}
			if found != tc.wantFound {
				t.Fatalf("found = %t, want %t (owner %+v)", found, tc.wantFound, owner)
			}
			if !strings.Contains(listed, tc.resource) || strings.Contains(listed, "tvdbId=") || strings.Contains(listed, "tmdbId=") {
				t.Fatalf("must list the resource without a native-ID query, got %q", listed)
			}
			if tc.wantFound && owner.id == 5 {
				t.Fatal("a non-matching row must never be selected as the owner")
			}
		})
	}
}
