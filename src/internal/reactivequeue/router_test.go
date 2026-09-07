package reactivequeue

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

type assignmentMemory struct {
	mu    sync.Mutex
	items map[string]store.ReactiveAssignment
}

func (m *assignmentMemory) GetReactiveAssignment(_ context.Context, kind, provider, value string) (store.ReactiveAssignment, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.items[kind+":"+provider+":"+value]
	return a, ok, nil
}

func (m *assignmentMemory) PutReactiveAssignment(ctx context.Context, a store.ReactiveAssignment, _ time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[a.Kind+":"+a.Provider+":"+a.Value] = a
	return nil
}

func TestRouterTiersAndStableAssignment(t *testing.T) {
	identity := &store.ProviderIdentity{Kind: "movie", IDs: store.ProviderIDs{TMDB: "42"}}
	tests := []struct {
		name       string
		responses  []string
		wantArr    string
		wantReason string
		wantTier   int
		wantOwned  bool
	}{
		{name: "one compatible", responses: []string{`[]`}, wantArr: "arr-0", wantTier: 0},
		{name: "one owner", responses: []string{`[]`, `[{"id":1}]`}, wantArr: "arr-1", wantTier: 1, wantOwned: true},
		{name: "no owner", responses: []string{`[]`, `[]`}, wantReason: "routing_unresolved", wantTier: 2},
		{name: "ambiguous owners", responses: []string{`[{"id":1}]`, `[{"id":2}]`}, wantReason: "routing_ambiguous", wantTier: 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := &assignmentMemory{items: make(map[string]store.ReactiveAssignment)}
			var targets []config.ArrTarget
			for i, body := range tc.responses {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/api/v3/movie" || r.URL.Query().Get("tmdbId") != "42" {
						http.NotFound(w, r)
						return
					}
					_, _ = w.Write([]byte(body))
				}))
				t.Cleanup(srv.Close)
				targets = append(targets, config.ArrTarget{Name: fmt.Sprintf("arr-%d", i), BaseURL: srv.URL, APIKey: "redacted"})
			}
			router, err := NewRouter(repo, targets, func() time.Time { return time.Unix(1, 0) })
			if err != nil {
				t.Fatal(err)
			}
			got, err := router.Resolve(t.Context(), identity)
			if err != nil || got.ArrName != tc.wantArr || got.Reason != tc.wantReason || got.Tier != tc.wantTier || got.Owned != tc.wantOwned {
				t.Fatalf("route=%+v err=%v", got, err)
			}
			if tc.wantTier == 1 {
				again, err := router.Resolve(t.Context(), identity)
				if err != nil || again.ArrName != tc.wantArr || again.Tier != 2 || !again.Owned {
					t.Fatalf("remembered route=%+v err=%v", again, err)
				}
			}
		})
	}
}

func TestRouterIMDBMalformedBoundedAndCancellation(t *testing.T) {
	repo := &assignmentMemory{items: make(map[string]store.ReactiveAssignment)}
	identity := &store.ProviderIdentity{Kind: "series", IDs: store.ProviderIDs{IMDB: "tt123"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/series" || r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`[{"id":1,"imdbId":"TT123"}]`))
	}))
	t.Cleanup(server.Close)
	router, err := NewRouter(repo, []config.ArrTarget{{Name: "sonarr", BaseURL: server.URL}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := router.Resolve(t.Context(), identity)
	if err != nil || got.ArrName != "sonarr" || got.Tier != 0 || !got.Owned {
		t.Fatalf("imdb route=%+v err=%v", got, err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{`)) }))
	t.Cleanup(bad.Close)
	router, _ = NewRouter(repo, []config.ArrTarget{{Name: "bad", BaseURL: bad.URL}}, nil)
	if _, err := router.Resolve(t.Context(), identity); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("malformed error=%v", err)
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := router.Resolve(canceled, identity); err == nil {
		t.Fatal("canceled route succeeded")
	}
	if _, err := decodeArrRows([]byte("[" + strings.Repeat(`{"id":1},`, maxArrRows) + `{"id":1}]`)); err == nil {
		t.Fatal("oversized row set accepted")
	}
}

func TestRouterFaultsFailClosed(t *testing.T) {
	repo := &assignmentMemory{items: map[string]store.ReactiveAssignment{
		"movie:tmdb:42": {Kind: "movie", Provider: "tmdb", Value: "42", ArrName: "removed-arr"},
	}}
	identity := &store.ProviderIdentity{Kind: "movie", IDs: store.ProviderIDs{TMDB: "42"}}
	router, err := NewRouter(repo, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := router.Resolve(t.Context(), identity)
	if err != nil || got.Reason != "routing_unresolved" || got.Tier != 2 {
		t.Fatalf("removed assignment route=%+v err=%v", got, err)
	}

	for _, status := range []int{http.StatusNotFound, http.StatusTooManyRequests} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		router, err := NewRouter(&assignmentMemory{items: make(map[string]store.ReactiveAssignment)}, []config.ArrTarget{{Name: "arr", BaseURL: server.URL}}, nil)
		if err != nil {
			server.Close()
			t.Fatal(err)
		}
		got, err := router.Resolve(t.Context(), identity)
		server.Close()
		if err != nil || got.Reason != "routing_unresolved" || got.Tier != 2 {
			t.Fatalf("status %d route=%+v err=%v", status, got, err)
		}
	}

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	baseURL := server.URL
	server.Close()
	router, _ = NewRouter(&assignmentMemory{items: make(map[string]store.ReactiveAssignment)}, []config.ArrTarget{{Name: "arr", BaseURL: baseURL}}, nil)
	if _, err := router.Resolve(t.Context(), identity); err == nil {
		t.Fatal("disconnected Arr accepted")
	}
}

func FuzzDecodeArrRows(f *testing.F) {
	f.Add([]byte(`[]`))
	f.Add([]byte(`[{"id":1,"tmdbId":42,"imdbId":"tt123"}]`))
	f.Add([]byte(`{`))
	f.Fuzz(func(t *testing.T, data []byte) {
		rows, err := decodeArrRows(data)
		if err == nil && len(rows) > maxArrRows {
			t.Fatalf("decoded %d rows", len(rows))
		}
	})
}
