package wizard

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/dockerctl"
	"github.com/darkharrbor/darkharrbor/internal/torbox"
)

// capsCase is one row in the caps→derived-config table (W1 unit gate).
type capsCase struct {
	name          string
	info          *torbox.UserInfo
	wantPlanLabel string
	wantSlots     int
	wantMaxGB     int64 // expected maxBytes in GiB (for readability)
	wantUsenet    bool
	wantNews      bool
	wantFreeTier  bool
	wantUnknown   bool
	wantPrefLanes []string // expected preference for an Interactive S5 call with defaults accepted
}

var now = time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

var capsCases = []capsCase{
	{
		name:          "nil info → free fallback",
		info:          nil,
		wantPlanLabel: "Unknown (no account data)",
		wantSlots:     1,
		wantMaxGB:     10,
		wantUsenet:    false,
		wantNews:      false,
		wantFreeTier:  false,
		wantUnknown:   true,
		wantPrefLanes: []string{"torbox_torrent"},
	},
	{
		name: "unknown subscribed plan uses conservative caps without claiming Free",
		info: &torbox.UserInfo{
			Plan: 99, IsSubscribed: true,
			PremiumExpiresAt: now.Add(30 * 24 * time.Hour),
		},
		wantPlanLabel: "Unknown (plan=99)",
		wantSlots:     1,
		wantMaxGB:     10,
		wantUsenet:    false,
		wantNews:      false,
		wantFreeTier:  false,
		wantUnknown:   true,
		wantPrefLanes: []string{"torbox_torrent"},
	},
	{
		name: "free plan subscribed",
		info: &torbox.UserInfo{
			Plan: 0, IsSubscribed: true,
			PremiumExpiresAt: now.Add(30 * 24 * time.Hour),
		},
		wantPlanLabel: "Free",
		wantSlots:     1,
		wantMaxGB:     10,
		wantUsenet:    false,
		wantNews:      false,
		wantFreeTier:  true,
		wantPrefLanes: []string{"torbox_torrent"},
	},
	{
		name: "essential plan",
		info: &torbox.UserInfo{
			Plan: 1, IsSubscribed: true,
			PremiumExpiresAt: now.Add(30 * 24 * time.Hour),
		},
		wantPlanLabel: "Essential",
		wantSlots:     3,
		wantMaxGB:     200,
		wantUsenet:    false,
		wantNews:      false,
		wantFreeTier:  false,
		wantPrefLanes: []string{"torbox_torrent"},
	},
	{
		name: "standard plan",
		info: &torbox.UserInfo{
			Plan: 3, IsSubscribed: true,
			PremiumExpiresAt: now.Add(30 * 24 * time.Hour),
		},
		wantPlanLabel: "Standard",
		wantSlots:     5,
		wantMaxGB:     200,
		wantUsenet:    false,
		wantNews:      false,
		wantFreeTier:  false,
		wantPrefLanes: []string{"torbox_torrent"},
	},
	{
		name: "pro plan",
		info: &torbox.UserInfo{
			Plan: 2, IsSubscribed: true,
			PremiumExpiresAt: now.Add(30 * 24 * time.Hour),
		},
		wantPlanLabel: "Pro",
		wantSlots:     10,
		wantMaxGB:     1024,
		wantUsenet:    true,
		wantNews:      true,
		wantFreeTier:  false,
		wantPrefLanes: []string{"torbox_torrent", "torbox_nzb"},
	},
	{
		name: "pro with +2 additional slots",
		info: &torbox.UserInfo{
			Plan: 2, IsSubscribed: true, AdditionalSlots: 2,
			PremiumExpiresAt: now.Add(30 * 24 * time.Hour),
		},
		wantPlanLabel: "Pro",
		wantSlots:     12,
		wantMaxGB:     1024,
		wantUsenet:    true,
		wantNews:      true,
		wantFreeTier:  false,
		wantPrefLanes: []string{"torbox_torrent", "torbox_nzb"},
	},
	{
		name: "lapsed pro subscription → free fallback",
		info: &torbox.UserInfo{
			Plan: 2, IsSubscribed: true,
			PremiumExpiresAt: now.Add(-24 * time.Hour), // expired yesterday
		},
		wantPlanLabel: "Free (not subscribed)",
		wantSlots:     1,
		wantMaxGB:     10,
		wantUsenet:    false,
		wantNews:      false,
		wantFreeTier:  true,
		wantPrefLanes: []string{"torbox_torrent"},
	},
}

func TestCapsTable(t *testing.T) {
	for _, tc := range capsCases {
		t.Run(tc.name, func(t *testing.T) {
			slots, maxBytes, usenet, conservativeFreePolicy := torbox.ResolveCaps(tc.info, now)
			news := torbox.NewsServerCapable(tc.info, now)
			planLabel, verifiedFreeTier, unknownFallback := torbox.AccountPlan(tc.info, now)

			if slots != tc.wantSlots {
				t.Errorf("slots: got %d, want %d", slots, tc.wantSlots)
			}
			wantMaxBytes := tc.wantMaxGB * (1 << 30)
			if maxBytes != wantMaxBytes {
				t.Errorf("maxBytes: got %d, want %d (%d GiB)", maxBytes, wantMaxBytes, tc.wantMaxGB)
			}
			if usenet != tc.wantUsenet {
				t.Errorf("usenetCapable: got %v, want %v", usenet, tc.wantUsenet)
			}
			if news != tc.wantNews {
				t.Errorf("newsServer: got %v, want %v", news, tc.wantNews)
			}
			if verifiedFreeTier != tc.wantFreeTier {
				t.Errorf("verifiedFreeTier: got %v, want %v", verifiedFreeTier, tc.wantFreeTier)
			}
			if unknownFallback != tc.wantUnknown {
				t.Errorf("unknownFallback: got %v, want %v", unknownFallback, tc.wantUnknown)
			}
			if unknownFallback && !conservativeFreePolicy {
				t.Error("unknown plan must retain conservative free-level runtime policy")
			}

			// Derive preference from caps (mirrors the S5 default logic).
			gotPref := DefaultPreference(usenet, verifiedFreeTier)
			if !stringSliceEq(gotPref, tc.wantPrefLanes) {
				t.Errorf("DefaultPreference: got %v, want %v", gotPref, tc.wantPrefLanes)
			}

			// Check planLabel helper.
			if !strings.EqualFold(planLabel, tc.wantPlanLabel) {
				t.Errorf("AccountPlan label: got %q, want %q", planLabel, tc.wantPlanLabel)
			}
		})
	}
}

func stringSliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestParsePreferenceRespectsNNTPAvailability(t *testing.T) {
	got := parsePreference("torbox_torrent,nntp_nzb,uncached_torrent", true, false)
	want := []string{config.LaneTorBoxTorrent, config.LaneUncachedTorrent}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsePreference() = %v, want %v", got, want)
	}

	got = parsePreference("torbox_torrent,nntp_nzb,uncached_torrent", true, true)
	want = []string{config.LaneTorBoxTorrent, config.LaneNNTPNZB, config.LaneUncachedTorrent}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsePreference() with NNTP = %v, want %v", got, want)
	}

	got = parsePreference("torbox_torrent,uncached_torrent_derank,uncached_torrent", true, true)
	want = []string{config.LaneTorBoxTorrent, config.LaneUncachedTorrentDerank}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsePreference() with mutually exclusive uncached modes = %v, want %v", got, want)
	}
}

func TestParseUsenetProviderSelectionIsExplicitOrderedAndBounded(t *testing.T) {
	got, err := parseUsenetProviderSelection("newshosting,custom-one,torbox", true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"newshosting", "custom-one", "torbox"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selection = %v, want %v", got, want)
	}
	if got, err := parseUsenetProviderSelection("", true); err != nil || got != nil {
		t.Fatalf("empty input must select none, got %v err=%v", got, err)
	}
	for _, raw := range []string{"torbox", "news,news", "custom-one,custom_one", "none,news", strings.Repeat("x", 65)} {
		if _, err := parseUsenetProviderSelection(raw, false); err == nil {
			t.Fatalf("unsafe or unavailable selection %q accepted", raw)
		}
	}
	tooMany := make([]string, maxUsenetProviders+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("news%d", i)
	}
	if _, err := parseUsenetProviderSelection(strings.Join(tooMany, ","), true); err == nil {
		t.Fatal("over-cap NNTP provider selection accepted")
	}
}

func TestUnselectedUsenetProvidersConsumeNoCredentialInput(t *testing.T) {
	withResponses(t, []string{"must-not-be-consumed"})
	var out strings.Builder
	lines, available, err := configureUsenetProviders(t.Context(), &out, nil, nil, nil, true)
	if err != nil || available || len(lines) != 0 {
		t.Fatalf("lines=%v available=%t err=%v", lines, available, err)
	}
	remaining, _ := stdinReader.ReadString('\n')
	if remaining != "must-not-be-consumed\n" {
		t.Fatalf("unselected provider consumed credential input: %q", remaining)
	}
}

func TestSelectedUsenetCannotSilentlyLoseItsOnlyCapability(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		torbox, cached, direct bool
		want                   bool
	}{
		{name: "none", want: false},
		{name: "unconfigured TorBox does not count", cached: true, want: false},
		{name: "cached NZB", torbox: true, cached: true, want: true},
		{name: "direct NNTP", direct: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := usableUsenetSource(tc.torbox, tc.cached, tc.direct); got != tc.want {
				t.Fatalf("usableUsenetSource()=%t want=%t", got, tc.want)
			}
		})
	}
}

func TestSelectedTorBoxNewsServerRequiresVerifiedCapability(t *testing.T) {
	var out strings.Builder
	_, _, err := configureUsenetProviders(t.Context(), &out, nil, nil, []string{"torbox"}, false)
	if err == nil || !strings.Contains(err.Error(), "verified plan does not provide it") {
		t.Fatalf("expected capability failure, got %v", err)
	}
}

func TestValidProviderName(t *testing.T) {
	for _, name := range []string{"torbox", "news-host", "astraweb_1"} {
		if !validProviderName(name) {
			t.Fatalf("validProviderName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "bad name", "Upper", "oops!"} {
		if validProviderName(name) {
			t.Fatalf("validProviderName(%q) = true, want false", name)
		}
	}
}

func execFrame(streamType byte, payload []byte) []byte {
	header := make([]byte, 8)
	header[0] = streamType
	binary.BigEndian.PutUint32(header[4:8], uint32(len(payload)))
	return append(header, payload...)
}

func mockWizardProxy(t *testing.T, containers []dockerctl.Container, configXMLByID map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ApiVersion":"1.51"}`))
	})
	mux.HandleFunc("/v1.51/containers/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(containers)
	})

	execIDToContainer := map[string]string{}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exec"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.51/containers/"), "/exec")
			execID := "exec-" + id
			execIDToContainer[execID] = id
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": execID})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
			execID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.51/exec/"), "/start")
			cid := execIDToContainer[execID]
			body := configXMLByID[cid]
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(execFrame(1, []byte(body)))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]int{"ExitCode": 0})
		default:
			http.Error(w, "unexpected path in mock", http.StatusNotFound)
		}
	})
	return httptest.NewServer(mux)
}

func TestDiscoverArrInstancesIncludesProwlarr(t *testing.T) {
	containers := []dockerctl.Container{
		{ID: "sonarr1", Names: []string{"/sonarr-modern"}, Image: "lscr.io/linuxserver/sonarr:latest", State: "running"},
		{ID: "sonarr2", Names: []string{"/sonarr-classic"}, Image: "lscr.io/linuxserver/sonarr:latest", State: "running"},
		{ID: "sonarr3", Names: []string{"/sonarr-cartoons"}, Image: "lscr.io/linuxserver/sonarr:latest", State: "running"},
		{ID: "radarr1", Names: []string{"/radarr"}, Image: "lscr.io/linuxserver/radarr:latest", State: "running"},
		{ID: "prowlarr1", Names: []string{"/prowlarr"}, Image: "lscr.io/linuxserver/prowlarr:latest", State: "running"},
		{ID: "qb1", Names: []string{"/qbittorrent"}, Image: "lscr.io/linuxserver/qbittorrent:latest", State: "running"},
	}
	configXML := map[string]string{
		"sonarr1":   `<Config><Port>8989</Port><ApiKey>key-modern</ApiKey></Config>`,
		"sonarr2":   `<Config><Port>8990</Port><ApiKey>key-classic</ApiKey></Config>`,
		"sonarr3":   `<Config><Port>8991</Port><ApiKey>key-cartoons</ApiKey></Config>`,
		"radarr1":   `<Config><Port>7878</Port><ApiKey>key-radarr</ApiKey></Config>`,
		"prowlarr1": `<Config><Port>9696</Port><ApiKey>key-prowlarr</ApiKey></Config>`,
	}
	srv := mockWizardProxy(t, containers, configXML)
	defer srv.Close()

	client := dockerctl.New(srv.URL, nil)
	var verified []string
	got, err := discoverArrInstances(context.Background(), client, func(_ context.Context, inst discoveredArrInstance) error {
		verified = append(verified, inst.Name)
		return nil
	})
	if err != nil {
		t.Fatalf("discoverArrInstances: %v", err)
	}

	wantNames := []string{"prowlarr", "radarr", "sonarr-cartoons", "sonarr-classic", "sonarr-modern"}
	if len(got) != len(wantNames) {
		t.Fatalf("len(discovered)=%d, want %d", len(got), len(wantNames))
	}
	var names []string
	for _, inst := range got {
		names = append(names, inst.Name)
	}
	if !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("names=%v, want %v", names, wantNames)
	}
	sortedVerified := append([]string(nil), verified...)
	sort.Strings(sortedVerified)
	wantVerified := append([]string(nil), wantNames...)
	sort.Strings(wantVerified)
	if !reflect.DeepEqual(sortedVerified, wantVerified) {
		t.Fatalf("verified=%v, want %v", verified, wantNames)
	}
	if got[0].APIKeyRef != "HARRBOR_ARR_PROWLARR_APIKEY" {
		t.Fatalf("prowlarr APIKeyRef=%q", got[0].APIKeyRef)
	}
}

func TestDiscoverChosenArrInstancesDoesNotExtractUnselectedCredentials(t *testing.T) {
	containers := []dockerctl.Container{
		{ID: "sonarr1", Names: []string{"/sonarr-private"}, Image: "lscr.io/linuxserver/sonarr:latest", State: "running"},
		{ID: "radarr1", Names: []string{"/radarr-main"}, Image: "lscr.io/linuxserver/radarr:latest", State: "running"},
	}
	configXML := map[string]string{
		"sonarr1": `<Config><Port>8989</Port></Config>`,
		"radarr1": `<Config><Port>7878</Port><ApiKey>selected-key</ApiKey></Config>`,
	}
	srv := mockWizardProxy(t, containers, configXML)
	defer srv.Close()

	client := dockerctl.New(srv.URL, nil)
	got, err := discoverChosenArrInstances(context.Background(), client, nil, false, []string{"radarr-main"})
	if err != nil {
		t.Fatalf("discover selected subset: %v", err)
	}
	if len(got) != 1 || got[0].Name != "radarr-main" || got[0].APIKey != "selected-key" {
		t.Fatalf("instances=%+v", got)
	}
}

func TestDiscoverChosenArrInstancesExtractsOnlySelectedProwlarrCredential(t *testing.T) {
	containers := []dockerctl.Container{
		{ID: "radarr1", Names: []string{"/radarr-main"}, Image: "lscr.io/linuxserver/radarr:latest", State: "running"},
		{ID: "prowlarr1", Names: []string{"/prowlarr-main"}, Image: "lscr.io/linuxserver/prowlarr:latest", State: "running"},
		{ID: "prowlarr2", Names: []string{"/prowlarr-private"}, Image: "lscr.io/linuxserver/prowlarr:latest", State: "running"},
	}
	configXML := map[string]string{
		"radarr1":   `<Config><Port>7878</Port><ApiKey>radarr-key</ApiKey></Config>`,
		"prowlarr1": `<Config><Port>9696</Port><ApiKey>selected-key</ApiKey></Config>`,
		"prowlarr2": `<Config><Port>9697</Port></Config>`,
	}
	srv := mockWizardProxy(t, containers, configXML)
	defer srv.Close()

	client := dockerctl.New(srv.URL, nil)
	got, err := discoverChosenArrInstances(context.Background(), client, nil, true, []string{"radarr-main", "prowlarr-main"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("instances=%+v", got)
	}
	for _, instance := range got {
		if instance.Name == "prowlarr-private" {
			t.Fatal("unselected Prowlarr credential was extracted")
		}
	}
}
