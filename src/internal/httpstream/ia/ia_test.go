package ia

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/httpstream"
)

// Canned IA API shapes (parser tolerance — DH-internal determinism, not a
// fake service harness; the live H2 gate runs against real IA).
const searchJSON = `{"response":{"docs":[
 {"identifier":"NightOfTheLivingDead1968","title":"Night of the Living Dead","year":"1968","downloads":500000},
 {"identifier":"night_of_the_living_dead_colorized","title":["Night of the Living Dead"],"year":1968,"downloads":1000},
 {"identifier":"unrelated_thing","title":"Some Totally Different Film","year":"1968","downloads":99999}
]}}`

const metadataJSON = `{"files":[
 {"name":"NightOfTheLivingDead.mp4","source":"derivative","format":"h.264","size":"734003200","height":"480"},
 {"name":"NightOfTheLivingDead_512kb.mp4","source":"derivative","format":"512Kb MPEG4","size":"200000000","height":"240"},
 {"name":"NightOfTheLivingDead.mkv","source":"original","format":"Matroska","size":900000000,"height":1080,"md5":"d41d8cd98f00b204e9800998ecf8427e","sha1":"da39a3ee5e6b4b0d3255bfef95601890afd80709"},
 {"name":"NightOfTheLivingDead.thumbs.jpg","source":"derivative","format":"Thumbnail","size":"1000"},
 {"name":"cover.pdf","source":"derivative","format":"Text PDF","size":"5000"}
]}`

const episodeMetadataJSON = `{"files":[
 {"name":"Show.S02E07.mp4","source":"derivative","format":"h.264","size":"100","height":"720"},
 {"name":"Show.S02E08.mp4","source":"derivative","format":"h.264","size":"100","height":"720"},
 {"name":"Show.s2e7.alt.mp4","source":"original","format":"MPEG4","size":"200","height":"480"}
]}`

func fixtureServer(t *testing.T) (*httptest.Server, *Handler) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/advancedsearch.php", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(searchJSON))
	})
	mux.HandleFunc("/metadata/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "colorized") {
			_, _ = w.Write([]byte(`{"files":[]}`))
			return
		}
		_, _ = w.Write([]byte(metadataJSON))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, New("iabackend", ts.URL, ts.Client())
}

func TestSearchMovieConservativeMatchAndRanking(t *testing.T) {
	_, h := fixtureServer(t)
	res, err := h.Search(context.Background(), httpstream.StreamQuery{
		Kind: "movie", Title: "Night of the Living Dead", Year: 1968,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("results = %d, want 1 (unrelated title must not match; empty-file item dropped): %+v", len(res), res)
	}
	r := res[0]
	// h.264 (rank 3) beats Matroska (2) and 512Kb (1).
	if r.Key.Selector != "NightOfTheLivingDead1968/NightOfTheLivingDead.mp4" {
		t.Fatalf("selector = %q (ranking wrong)", r.Key.Selector)
	}
	if r.Size != 734003200 {
		t.Fatalf("size = %d (string size not decoded)", r.Size)
	}
	if r.Quality != "480p" {
		t.Fatalf("quality = %q", r.Quality)
	}
	if !strings.Contains(r.Title, "Night.of.the.Living.Dead.1968") || !strings.HasSuffix(r.Title, "WEBDL") {
		t.Fatalf("release name synthesis wrong: %q", r.Title)
	}
	if r.Key.BackendID != "iabackend" || r.Key.Handler != "ia" || r.Key.IDs["ia"] != "NightOfTheLivingDead1968" {
		t.Fatalf("key wrong: %+v", r.Key)
	}
	if _, err := r.Key.Canonical(); err != nil {
		t.Fatalf("key not canonicalizable: %v", err)
	}
}

func TestSearchSkipsIDOnlyAndSeasonOnly(t *testing.T) {
	_, h := fixtureServer(t)
	// ID-only: no title → skip (HS-2.1). Must not even hit the network,
	// but fixture server tolerates it either way.
	res, err := h.Search(context.Background(), httpstream.StreamQuery{Kind: "movie", TMDBID: "123"})
	if err != nil || len(res) != 0 {
		t.Fatalf("ID-only query not skipped: %v %+v", err, res)
	}
	res, err = h.Search(context.Background(), httpstream.StreamQuery{Kind: "episode", Title: "Show", Season: 2})
	if err != nil || len(res) != 0 {
		t.Fatalf("season-only query not skipped: %v %+v", err, res)
	}
}

func TestEpisodeEvidenceFiltering(t *testing.T) {
	files := []metadataFile{}
	if err := json.Unmarshal([]byte(episodeMetadataJSON), &struct {
		Files *[]metadataFile `json:"files"`
	}{&files}); err != nil {
		t.Fatal(err)
	}
	vids := selectVideoFiles(files)
	got := filterEpisodeFiles(vids, 2, 7)
	if len(got) != 2 {
		t.Fatalf("SxxExx filter = %d files, want 2 (S02E07 + s2e7): %+v", len(got), got)
	}
	// E07 must not match E70/E71...
	if episodeRe(2, 7).MatchString("Show.S02E70.mp4") {
		t.Fatal("S02E07 regexp matched E70")
	}
	best, ok := bestRanked(got)
	if !ok || best.Name != "Show.S02E07.mp4" {
		t.Fatalf("episode ranking wrong: %+v", best)
	}
}

func TestTolerantEpisodeEvidenceAndAmbiguity(t *testing.T) {
	files := []metadataFile{
		{Name: "Show 1x02 The Beastly Shadow.mp4", Source: "derivative", Format: "h.264"},
		{Name: "Show 03 Disappearing Act.mp4", Source: "derivative", Format: "h.264"},
		{Name: "Show Pilot.mp4", Source: "derivative", Format: "h.264"},
	}
	q := httpstream.StreamQuery{
		Kind: "episode", Season: 1, Episode: 2,
		Episodes: []httpstream.EpisodeIdentity{
			{Season: 1, Episode: 2, Absolute: 2, Title: "The Beastly Shadow"},
			{Season: 1, Episode: 3, Absolute: 3, Title: "Disappearing Act"},
			{Season: 2, Episode: 2, Title: "Pilot"},
		},
	}
	got, ambiguous := filterEpisodeFilesTolerant(files, q)
	if len(got) != 1 || got[0].Name != files[0].Name || ambiguous[got[0].Name] {
		t.Fatalf("tolerant filter = %+v ambiguous=%v", got, ambiguous)
	}

	q.Episodes[0].Title = "Pilot"
	got, ambiguous = filterEpisodeFilesTolerant(files[2:], q)
	if len(got) != 1 || !ambiguous[got[0].Name] {
		t.Fatalf("ambiguous filter = %+v ambiguous=%v", got, ambiguous)
	}
}

func TestFilePreference(t *testing.T) {
	files := []metadataFile{
		{Name: "original.mkv", Source: "original", Format: "Matroska", Size: json.RawMessage(`900`)},
		{Name: "derivative.mp4", Source: "derivative", Format: "h.264", Size: json.RawMessage(`700`)},
	}
	if got, _ := bestRankedWithPreference(files, PreferDerivative); got.Name != "derivative.mp4" {
		t.Fatalf("derivative preference chose %q", got.Name)
	}
	if got, _ := bestRankedWithPreference(files, PreferOriginal); got.Name != "original.mkv" {
		t.Fatalf("original preference chose %q", got.Name)
	}
}

func TestResolveBuildsTransientURLsFromBackendBase(t *testing.T) {
	ts, h := fixtureServer(t)
	key := httpstream.ResolveKey{
		Version: httpstream.ResolveKeyVersion, BackendID: "iabackend", Handler: "ia",
		Kind: "movie", IDs: map[string]string{"ia": "NightOfTheLivingDead1968"},
		Selector: "NightOfTheLivingDead1968/NightOfTheLivingDead.mkv",
	}
	files, err := h.Resolve(context.Background(), httpstream.ResolveRequest{Key: key})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("files = %d, want 3 video files", len(files))
	}
	rf, err := httpstream.MatchSelector(files, key.Selector)
	if err != nil {
		t.Fatalf("selector match: %v", err)
	}
	if !strings.HasPrefix(rf.URL, ts.URL+"/download/NightOfTheLivingDead1968/") {
		t.Fatalf("URL not derived from backend base: %s", rf.URL)
	}
	if rf.Size != 900000000 || rf.ContentType != "video/x-matroska" {
		t.Fatalf("mkv file metadata wrong: %+v", rf)
	}
	if rf.SupportsRange {
		t.Fatal("handler must not claim range support — D9 preflight earns it")
	}
}

// HR2.7: IA's own metadata md5/sha1 fields become authoritative whole-file
// SourceDigest evidence on the resolved representation.
func TestResolveExtractsAuthoritativeDigests(t *testing.T) {
	_, h := fixtureServer(t)
	key := httpstream.ResolveKey{
		Version: httpstream.ResolveKeyVersion, BackendID: "iabackend", Handler: "ia",
		Kind: "movie", IDs: map[string]string{"ia": "NightOfTheLivingDead1968"},
		Selector: "NightOfTheLivingDead1968/NightOfTheLivingDead.mkv",
	}
	files, err := h.Resolve(context.Background(), httpstream.ResolveRequest{Key: key})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	rf, err := httpstream.MatchSelector(files, key.Selector)
	if err != nil {
		t.Fatalf("selector match: %v", err)
	}
	if len(rf.Digests) != 2 {
		t.Fatalf("digests = %+v, want 2 (md5+sha1)", rf.Digests)
	}
	seen := map[string]string{}
	for _, d := range rf.Digests {
		if !httpstream.ValidSourceDigest(d) {
			t.Fatalf("invalid digest returned: %+v", d)
		}
		seen[d.Algorithm] = d.Hex
	}
	if seen["md5"] != "d41d8cd98f00b204e9800998ecf8427e" {
		t.Fatalf("md5 = %q", seen["md5"])
	}
	if seen["sha1"] != "da39a3ee5e6b4b0d3255bfef95601890afd80709" {
		t.Fatalf("sha1 = %q", seen["sha1"])
	}

	// A file with no md5/sha1 in the fixture must yield zero digests, never
	// a fabricated one.
	otherKey := key
	otherKey.Selector = "NightOfTheLivingDead1968/NightOfTheLivingDead.mp4"
	otherRF, err := httpstream.MatchSelector(files, otherKey.Selector)
	if err != nil {
		t.Fatalf("selector match: %v", err)
	}
	if len(otherRF.Digests) != 0 {
		t.Fatalf("expected zero digests for a file with no md5/sha1, got %+v", otherRF.Digests)
	}
}

func TestBackendErrorClassification(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metadata/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusServiceUnavailable)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	h := New("b", ts.URL, ts.Client())
	_, err := h.Resolve(context.Background(), httpstream.ResolveRequest{Key: httpstream.ResolveKey{
		Version: 1, BackendID: "b", Handler: "ia", Kind: "movie",
		IDs: map[string]string{"ia": "x"},
	}})
	if httpstream.ClassOf(err) != httpstream.ClassBackendUnavailable {
		t.Fatalf("5xx not classified transient: %v", err)
	}
	if strings.Contains(err.Error(), ts.URL) {
		t.Fatalf("backend URL leaked into error: %v", err)
	}
}

func TestTitleNormalizationRules(t *testing.T) {
	// Movies: strict normalized equality only — IA containment matches are
	// trailers/featurettes/clips (live finding 2026-07-13).
	if !titleMatches("Night of the Living Dead", "Night Of The Living Dead!", false) {
		t.Fatal("normalized equality should match for movies")
	}
	if titleMatches("Avengers Endgame", "AVENGERS: ENDGAME | Cap Vs Cap VFX Breakdown", false) {
		t.Fatal("movie containment must be rejected (trailer/clip junk)")
	}
	// Episodes: word-bounded containment allowed (SxxExx evidence follows).
	if !titleMatches("Night Gallery", "Night Gallery - Complete Series", true) {
		t.Fatal("word-bounded containment should match for tv")
	}
	if titleMatches("Dead", "Deadwood", true) {
		t.Fatal("substring without word boundary must not match")
	}
	if titleMatches("Night of the Living Dead", "Night of the Living", true) {
		t.Fatal("query longer than candidate must not match")
	}
}
