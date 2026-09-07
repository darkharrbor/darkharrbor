package omss

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/httpstream"
)

// nonConformingBody mirrors the response shape emitted by a real backend that
// advertises `spec:"omss"` but diverges from v1.1 §6.1/§6.2 in four ways:
// the top-level id is spelled `responseId`, `streamable` is absent, source
// objects carry no `id`, and `audioTracks` is an object array rather than a
// string array. URLs/headers here are synthetic fixtures only.
const nonConformingBody = `{
	"responseId": "9f1c-nonconf",
	"expiresAt": "2026-07-25T00:21:18Z",
	"sources": [
		{"url":"https://backend.invalid/proxy?data=aaa","type":"mp4","quality":"1080p",
		 "audioTracks":[{"language":"org","label":"Original"}],
		 "headers":{"Referer":"https://backend.invalid"},
		 "provider":{"id":"fsharetv","name":"FshareTV"}},
		{"url":"https://backend.invalid/proxy?data=bbb","type":"mp4","quality":"720p",
		 "audioTracks":[{"language":"org","label":"Original"}],
		 "provider":{"id":"fsharetv","name":"FshareTV"}}
	]
}`

func nonConformingMux(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/movies/550", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(nonConformingBody))
	})
	return mux
}

// TestNonConformingBackendSearchYieldsSources is the core HR0.1 regression:
// before the tolerance work every source in this payload was dropped.
func TestNonConformingBackendSearchYieldsSources(t *testing.T) {
	h, srv := newTestHandler(t, nonConformingMux(t))
	defer srv.Close()

	results, err := h.Search(context.Background(), movieQuery("550"))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results from non-conforming backend, got %d", len(results))
	}
	for _, r := range results {
		if strings.TrimSpace(r.Key.Selector) == "" {
			t.Fatal("expected a synthesized selector, got empty")
		}
		if err := r.Key.Validate(); err != nil {
			t.Fatalf("synthesized key must validate: %v", err)
		}
	}
	if results[0].Key.Selector == results[1].Key.Selector {
		t.Fatalf("selectors must be distinct, both were %q", results[0].Key.Selector)
	}
}

// TestNonConformingSelectorsAgreeAcrossSearchAndResolve guards the failure
// mode that per-function tests would miss: a grab persists the Search
// selector, and playback matches it in Resolve. If the two derivations ever
// diverge, every non-conforming-backend item becomes unplayable.
func TestNonConformingSelectorsAgreeAcrossSearchAndResolve(t *testing.T) {
	h, srv := newTestHandler(t, nonConformingMux(t))
	defer srv.Close()

	results, err := h.Search(context.Background(), movieQuery("550"))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	files, err := h.Resolve(context.Background(), httpstream.ResolveRequest{
		Key:       results[0].Key,
		Operation: httpstream.ResolveNormal,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got := map[string]bool{}
	for _, f := range files {
		got[f.Selector] = true
	}
	for _, r := range results {
		if !got[r.Key.Selector] {
			t.Fatalf("search selector %q absent from resolve output %v", r.Key.Selector, got)
		}
	}
	if files[0].RefreshContext != "9f1c-nonconf" {
		t.Fatalf("expected responseId fallback as refresh context, got %q", files[0].RefreshContext)
	}
	if files[0].ExpiresAt.IsZero() {
		t.Fatal("expected expiresAt to survive the tolerant parse")
	}
}

// TestExplicitStreamableFalseStillRejected proves the tolerance is confined to
// an ABSENT field. An explicit false remains authoritative per §6.2.
func TestExplicitStreamableFalseStillRejected(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/movies/550", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"responseId":"r1","sources":[
			{"url":"https://backend.invalid/a.mp4","type":"mp4","quality":"1080p","streamable":false,
			 "provider":{"id":"p","name":"P"}}]}`))
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()

	results, err := h.Search(context.Background(), movieQuery("550"))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("explicit streamable:false must stay rejected, got %d results", len(results))
	}
}

// TestConformingIDWinsOverResponseID pins precedence: v1.1 `id` is preferred
// whenever both spellings are present.
func TestConformingIDWinsOverResponseID(t *testing.T) {
	var r sourcesResponse
	if err := json.Unmarshal([]byte(`{"id":"canonical","responseId":"legacy"}`), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.responseID() != "canonical" {
		t.Fatalf("expected conforming id to win, got %q", r.responseID())
	}
}

func TestResponseMissingBothIDSpellingsIsMalformed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/movies/550", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"sources":[]}`))
	})
	h, srv := newTestHandler(t, mux)
	defer srv.Close()

	if _, err := h.Search(context.Background(), movieQuery("550")); err == nil {
		t.Fatal("expected a malformed-response error when both id spellings are absent")
	}
}

func TestAudioTrackListAcceptsBothEncodings(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"v1.1 string array", `["en","fr"]`, []string{"en", "fr"}},
		{"object array language", `[{"language":"org","label":"Original"}]`, []string{"org"}},
		{"object array label fallback", `[{"label":"English"}]`, []string{"English"}},
		{"null", `null`, nil},
		{"unknown shape is advisory-empty", `{"weird":true}`, nil},
		{"number array is advisory-empty", `[1,2]`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got audioTrackList
			if err := json.Unmarshal([]byte(tc.in), &got); err != nil {
				t.Fatalf("audioTracks must never fail the response: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestAssignSelectorsCollisionOrdinal(t *testing.T) {
	resp := &sourcesResponse{Sources: []sourceObject{
		{Type: "mp4", Quality: "1080p", Provider: providerRef{ID: "p"}},
		{Type: "mp4", Quality: "1080p", Provider: providerRef{ID: "p"}},
		{ID: "explicit", Type: "mkv", Quality: "720p"},
	}}
	got := assignSelectors(resp)
	if got[0] == got[1] {
		t.Fatalf("collision must be disambiguated, both %q", got[0])
	}
	if got[2] != "explicit" {
		t.Fatalf("conforming source id must be used verbatim, got %q", got[2])
	}
	if !strings.HasPrefix(got[0], "syn.") {
		t.Fatalf("synthesized selectors must be namespaced, got %q", got[0])
	}
}

// TestAssignSelectorsStableAcrossIdenticalResponses proves a synthesized
// selector survives re-resolve, which is what keeps a grabbed item playable.
func TestAssignSelectorsStableAcrossIdenticalResponses(t *testing.T) {
	build := func() *sourcesResponse {
		return &sourcesResponse{Sources: []sourceObject{
			{Type: "mp4", Quality: "1080p", Provider: providerRef{ID: "fsharetv"}},
			{Type: "mp4", Quality: "720p", Provider: providerRef{ID: "fsharetv"}},
		}}
	}
	a, b := assignSelectors(build()), assignSelectors(build())
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("selector %d unstable: %q vs %q", i, a[i], b[i])
		}
	}
}

// FuzzSourcesResponseParse is the §5.0 fuzz target required of any new parser.
// The contract under fuzz is that a tolerant parse never panics and never
// yields a source that both survives filtering and carries an empty selector.
func FuzzSourcesResponseParse(f *testing.F) {
	f.Add(nonConformingBody)
	f.Add(`{"id":"a","sources":[{"id":"s","url":"u","streamable":true,"type":"mp4"}]}`)
	f.Add(`{"responseId":"a","sources":[{"audioTracks":[{"language":"en"}]}]}`)
	f.Add(`{"sources":null}`)
	f.Add(`{}`)

	f.Fuzz(func(t *testing.T, body string) {
		var parsed sourcesResponse
		if err := json.Unmarshal([]byte(body), &parsed); err != nil {
			return
		}
		selectors := assignSelectors(&parsed)
		if len(selectors) != len(parsed.Sources) {
			t.Fatalf("selector count %d != source count %d", len(selectors), len(parsed.Sources))
		}
		for i, src := range parsed.Sources {
			if sourceKind(src) != httpstream.KindUnsupported && strings.TrimSpace(selectors[i]) == "" {
				t.Fatalf("playable source %d has empty selector", i)
			}
		}
	})
}
