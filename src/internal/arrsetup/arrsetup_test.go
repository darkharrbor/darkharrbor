package arrsetup

// HR0.3 deterministic coverage: the fake arr API below proves that Register
// reconciles the full 2+3 topology (qBit + SAB clients, torrent-only Torznab,
// usenet-only Newznab, http-stream Torznab) idempotently: everything is
// created on an empty arr, nothing is re-created on a populated arr, and a
// partially-wired arr (missing only the http-stream indexer — the live-fleet
// gap this item closes) gains exactly the missing registration.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeArr struct {
	t                  *testing.T
	clients            []downloadClientDef
	indexers           []indexerDef
	indexerCreates     int
	clientCreates      int
	clientUpdates      int
	sawForceSaveClient bool
	sawForceSaveIdx    bool
	customFormats      []customFormatDef
	formatCreates      int
	formatUpdates      int
	profiles           []map[string]json.RawMessage
	profileUpdates     int
	mediaManagement    map[string]any
	mediaUpdates       int
}

func (f *fakeArr) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/downloadclient/schema", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []downloadClientDef{
			{Implementation: "QBittorrent", ConfigContract: "QBittorrentSettings", Protocol: "torrent",
				Fields: []arrField{{Name: "host"}, {Name: "port"}, {Name: "useSsl"}, {Name: "username"}, {Name: "password"}, {Name: "tvCategory"}, {Name: "movieCategory"}}},
			{Implementation: "Sabnzbd", ConfigContract: "SabnzbdSettings", Protocol: "usenet",
				Fields: []arrField{{Name: "host"}, {Name: "port"}, {Name: "useSsl"}, {Name: "apiKey"}, {Name: "tvCategory"}, {Name: "movieCategory"}}},
		})
	})
	mux.HandleFunc("/api/v3/indexer/schema", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []indexerDef{
			{Priority: 25, Implementation: "Torznab", ConfigContract: "TorznabSettings", Protocol: "torrent",
				Fields: []arrField{{Name: "baseUrl"}, {Name: "apiPath"}, {Name: "apiKey"}, {Name: "categories"}}},
			{Priority: 25, Implementation: "Newznab", ConfigContract: "NewznabSettings", Protocol: "usenet",
				Fields: []arrField{{Name: "baseUrl"}, {Name: "apiPath"}, {Name: "apiKey"}, {Name: "categories"}}},
		})
	})
	mux.HandleFunc("/api/v3/config/mediamanagement", func(w http.ResponseWriter, r *http.Request) {
		if f.mediaManagement == nil {
			f.mediaManagement = map[string]any{
				"id": 1, "importExtraFiles": false, "extraFileExtensions": "srt",
				"untouched": "preserved",
			}
		}
		writeJSON(w, f.mediaManagement)
	})
	mux.HandleFunc("/api/v3/config/mediamanagement/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		mustDecode(f.t, r, &f.mediaManagement)
		f.mediaUpdates++
		writeJSON(w, f.mediaManagement)
	})
	mux.HandleFunc("/api/v3/downloadclient", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, f.clients)
		case http.MethodPost:
			if r.URL.Query().Get("forceSave") == "true" {
				f.sawForceSaveClient = true
			}
			var def downloadClientDef
			mustDecode(f.t, r, &def)
			def.ID = len(f.clients) + 1
			f.clients = append(f.clients, def)
			f.clientCreates++
			writeJSON(w, def)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/v3/downloadclient/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		f.clientUpdates++
		writeJSON(w, map[string]any{})
	})
	mux.HandleFunc("/api/v3/indexer", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, f.indexers)
		case http.MethodPost:
			if r.URL.Query().Get("forceSave") == "true" {
				f.sawForceSaveIdx = true
			}
			var def indexerDef
			mustDecode(f.t, r, &def)
			if def.Priority < 1 || def.Priority > 50 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			def.ID = len(f.indexers) + 1
			f.indexers = append(f.indexers, def)
			f.indexerCreates++
			writeJSON(w, def)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/v3/customformat/schema", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []customFormatSpecification{{
			Name:           "Release Title",
			Implementation: "ReleaseTitleSpecification",
			Fields:         []arrField{{Name: "value"}},
		}})
	})
	mux.HandleFunc("/api/v3/customformat", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, f.customFormats)
		case http.MethodPost:
			var def customFormatDef
			mustDecode(f.t, r, &def)
			def.ID = len(f.customFormats) + 1
			f.customFormats = append(f.customFormats, def)
			f.formatCreates++
			writeJSON(w, def)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/v3/customformat/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var def customFormatDef
		mustDecode(f.t, r, &def)
		for i := range f.customFormats {
			if f.customFormats[i].ID == def.ID {
				f.customFormats[i] = def
			}
		}
		f.formatUpdates++
		writeJSON(w, def)
	})
	mux.HandleFunc("/api/v3/qualityprofile", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, f.profiles)
	})
	mux.HandleFunc("/api/v3/qualityprofile/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var profile map[string]json.RawMessage
		mustDecode(f.t, r, &profile)
		var id int
		_ = json.Unmarshal(profile["id"], &id)
		for i := range f.profiles {
			var candidateID int
			_ = json.Unmarshal(f.profiles[i]["id"], &candidateID)
			if candidateID == id {
				f.profiles[i] = profile
			}
		}
		f.profileUpdates++
		writeJSON(w, profile)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func mustDecode(t *testing.T, r *http.Request, v any) {
	t.Helper()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
}

func runRegister(t *testing.T, f *fakeArr, appType string) Result {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	res := Register(context.Background(), []ArrTarget{{
		Name: "fake", URL: srv.URL, AppType: appType, APIKey: "k",
	}}, Creds{QBitPassword: "pw", SABAPIKey: "sabkey"})
	if len(res) != 1 {
		t.Fatalf("want 1 result, got %d", len(res))
	}
	if res[0].Err != nil {
		t.Fatalf("register error: %v", res[0].Err)
	}
	return res[0]
}

func indexerBaseURLs(f *fakeArr) map[string]string {
	out := map[string]string{}
	for _, idx := range f.indexers {
		out[fieldStr(idx.Fields, "baseUrl")] = idx.Implementation
	}
	return out
}

func TestRegisterEmptyArrCreatesFullTopology(t *testing.T) {
	f := &fakeArr{t: t}
	res := runRegister(t, f, "sonarr")

	if f.clientCreates != 2 {
		t.Fatalf("want 2 client creates, got %d", f.clientCreates)
	}
	if f.indexerCreates != 3 {
		t.Fatalf("want 3 indexer creates, got %d", f.indexerCreates)
	}
	if !f.sawForceSaveIdx {
		t.Fatal("indexer POST did not use forceSave=true")
	}
	if !f.sawForceSaveClient {
		t.Fatal("download-client POST did not use forceSave=true")
	}
	bases := indexerBaseURLs(f)
	if bases[torznabBase] != "Torznab" {
		t.Fatalf("torrent-only indexer missing/wrong impl: %v", bases)
	}
	if bases[newznabBase] != "Newznab" {
		t.Fatalf("usenet-only indexer missing/wrong impl: %v", bases)
	}
	if bases[httpStreamBase] != "Torznab" {
		t.Fatalf("http-stream indexer missing/wrong impl: %v", bases)
	}
	want := []string{"qbit:created", "sab:created", "torznab:created", "newznab:created", "http-stream:created",
		"nfo-import:updated",
		"uncached-format:created,profiles-updated=0,profile-floor-incompatible=0"}
	if strings.Join(res.Actions, ",") != strings.Join(want, ",") {
		t.Fatalf("actions = %v, want %v", res.Actions, want)
	}
}

func TestRegisterSelectedCreatesOnlyLaneScopedTopology(t *testing.T) {
	for _, tc := range []struct {
		name     string
		lanes    Lanes
		clients  int
		indexers int
		bases    []string
	}{
		{"torrent", Lanes{Torrent: true}, 1, 1, []string{torznabBase}},
		{"nntp", Lanes{NNTP: true}, 1, 1, []string{newznabBase}},
		{"http", Lanes{HTTP: true}, 1, 1, []string{httpStreamBase}},
		{"torrent-http-share-qbit", Lanes{Torrent: true, HTTP: true}, 1, 2, []string{torznabBase, httpStreamBase}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeArr{t: t}
			srv := httptest.NewServer(f.handler())
			defer srv.Close()
			results := RegisterSelected(context.Background(), []ArrTarget{{Name: "fake", URL: srv.URL, AppType: "radarr", APIKey: "k"}}, Creds{QBitPassword: "pw", SABAPIKey: "sab"}, tc.lanes)
			if len(results) != 1 || results[0].Err != nil {
				t.Fatalf("results=%+v", results)
			}
			if f.clientCreates != tc.clients || f.indexerCreates != tc.indexers {
				t.Fatalf("clients=%d indexers=%d", f.clientCreates, f.indexerCreates)
			}
			bases := indexerBaseURLs(f)
			for _, base := range tc.bases {
				if _, ok := bases[base]; !ok {
					t.Fatalf("missing %s in %v", base, bases)
				}
			}
		})
	}
}

func TestRegisterIsIdempotentOnPopulatedArr(t *testing.T) {
	f := &fakeArr{t: t}
	runRegister(t, f, "sonarr")
	creates := f.indexerCreates

	res := runRegister(t, f, "sonarr")
	if f.indexerCreates != creates {
		t.Fatalf("second run created indexers: %d -> %d", creates, f.indexerCreates)
	}
	for _, a := range res.Actions {
		if strings.HasSuffix(a, ":created") && !strings.HasPrefix(a, "qbit") && !strings.HasPrefix(a, "sab") {
			t.Fatalf("second run reports creation: %v", res.Actions)
		}
	}
	// Download clients refresh rotated credentials in place, never duplicate.
	if f.clientCreates != 2 {
		t.Fatalf("client duplicated on rerun: creates=%d", f.clientCreates)
	}
	if f.clientUpdates == 0 {
		t.Fatal("rerun did not refresh client credentials")
	}
	if f.formatCreates != 1 || f.formatUpdates != 0 || f.profileUpdates != 0 {
		t.Fatalf("custom format was not idempotent: creates=%d updates=%d profile_updates=%d",
			f.formatCreates, f.formatUpdates, f.profileUpdates)
	}
	if f.mediaUpdates != 1 || f.mediaManagement["untouched"] != "preserved" ||
		f.mediaManagement["importExtraFiles"] != true ||
		!strings.Contains(f.mediaManagement["extraFileExtensions"].(string), "nfo") {
		t.Fatalf("NFO import was not idempotent/preserving: updates=%d config=%v", f.mediaUpdates, f.mediaManagement)
	}
}

func TestReconcilePreservesExistingClients(t *testing.T) {
	f := &fakeArr{t: t}
	runRegister(t, f, "sonarr")
	updates := f.clientUpdates

	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	results := Reconcile(context.Background(), []ArrTarget{{
		Name: "fake", URL: srv.URL, AppType: "sonarr", APIKey: "k",
	}}, Creds{QBitPassword: "rotated", SABAPIKey: "rotated"})
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("reconcile = %+v", results)
	}
	if f.clientUpdates != updates || f.clientCreates != 2 || f.indexerCreates != 3 {
		t.Fatalf("existing topology drifted: updates=%d creates=%d indexers=%d", f.clientUpdates, f.clientCreates, f.indexerCreates)
	}
	joined := strings.Join(results[0].Actions, ",")
	if !strings.Contains(joined, "qbit:already-present") || !strings.Contains(joined, "sab:already-present") {
		t.Fatalf("actions = %v", results[0].Actions)
	}
}

func TestRegisterReconcilesUncachedFormatWithoutChangingProfileFloor(t *testing.T) {
	profile := map[string]json.RawMessage{}
	for key, value := range map[string]any{
		"id":             7,
		"name":           "Any",
		"minFormatScore": 0,
		"formatItems": []qualityFormatItem{{
			Format: 99,
			Name:   "Existing",
			Score:  500,
		}},
		"qualityCutoffNotMetSearchDelay": 12,
	} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		profile[key] = raw
	}
	f := &fakeArr{t: t, profiles: []map[string]json.RawMessage{profile}}
	res := runRegister(t, f, "sonarr")
	if f.formatCreates != 1 || f.profileUpdates != 1 {
		t.Fatalf("creates=%d profile updates=%d, want 1/1", f.formatCreates, f.profileUpdates)
	}
	if !strings.Contains(strings.Join(res.Actions, ","), "profile-floor-incompatible=1") {
		t.Fatalf("actions did not report incompatible floor: %v", res.Actions)
	}

	var minimum int
	if err := json.Unmarshal(f.profiles[0]["minFormatScore"], &minimum); err != nil {
		t.Fatal(err)
	}
	if minimum != 0 {
		t.Fatalf("profile floor changed to %d", minimum)
	}
	var untouched int
	if err := json.Unmarshal(f.profiles[0]["qualityCutoffNotMetSearchDelay"], &untouched); err != nil {
		t.Fatal(err)
	}
	if untouched != 12 {
		t.Fatalf("unrelated profile field changed to %d", untouched)
	}
	var items []qualityFormatItem
	if err := json.Unmarshal(f.profiles[0]["formatItems"], &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Format != 99 || items[0].Score != 500 ||
		items[1].Format != f.customFormats[0].ID || items[1].Score != uncachedCustomFormatScore {
		t.Fatalf("profile format items = %+v", items)
	}

	runRegister(t, f, "sonarr")
	if f.formatCreates != 1 || f.profileUpdates != 1 {
		t.Fatalf("rerun was not idempotent: creates=%d profile updates=%d", f.formatCreates, f.profileUpdates)
	}
}

func TestRegisterFillsOnlyMissingHTTPStreamIndexer(t *testing.T) {
	// Live-fleet shape before HR0.3: 2+2 already wired, http-stream absent.
	f := &fakeArr{t: t}
	runRegister(t, f, "radarr")
	// Remove only the http-stream registration.
	kept := f.indexers[:0]
	for _, idx := range f.indexers {
		if fieldStr(idx.Fields, "baseUrl") != httpStreamBase {
			kept = append(kept, idx)
		}
	}
	f.indexers = kept
	f.indexerCreates = 0

	res := runRegister(t, f, "radarr")
	if f.indexerCreates != 1 {
		t.Fatalf("want exactly 1 indexer create, got %d", f.indexerCreates)
	}
	bases := indexerBaseURLs(f)
	if bases[httpStreamBase] != "Torznab" {
		t.Fatalf("http-stream indexer not restored: %v", bases)
	}
	joined := strings.Join(res.Actions, ",")
	if !strings.Contains(joined, "http-stream:created") ||
		!strings.Contains(joined, "torznab:already-present") ||
		!strings.Contains(joined, "newznab:already-present") {
		t.Fatalf("unexpected actions: %v", res.Actions)
	}
	// Radarr registrations must carry movie categories.
	for _, idx := range f.indexers {
		if fieldStr(idx.Fields, "baseUrl") == httpStreamBase {
			raw, _ := json.Marshal(idx.Fields)
			if !strings.Contains(string(raw), "2000") {
				t.Fatalf("radarr http-stream indexer missing movie categories: %s", raw)
			}
		}
	}
}
