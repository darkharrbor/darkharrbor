package searchbudget

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/mediaidentity"
)

type memoryRepo struct{ states map[string]State }

func (m *memoryRepo) GetSearchBudget(_ context.Context, key string) (State, bool, error) {
	s, ok := m.states[key]
	return s, ok, nil
}
func (m *memoryRepo) RecordSearchNoResult(_ context.Context, key string, cap int, suppress time.Duration, now time.Time) (State, error) {
	s := m.states[key]
	if s.ConsecutiveNoResults < cap {
		s.ConsecutiveNoResults++
	}
	if s.ConsecutiveNoResults >= cap {
		s.SuppressedUntil = now.Add(suppress)
	}
	m.states[key] = s
	return s, nil
}
func (m *memoryRepo) ResetSearchBudget(_ context.Context, key string) error {
	delete(m.states, key)
	return nil
}

func TestBudgetSuppressesThenPermitsOneProbe(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	repo := &memoryRepo{states: map[string]State{}}
	svc, err := New(repo, Options{Cap: 2, Suppress: 7 * 24 * time.Hour, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if allowed, err := svc.Allowed(context.Background(), "series:tvdb:1:1:1"); err != nil || !allowed {
			t.Fatalf("attempt %d unexpectedly blocked: allowed=%v err=%v", i+1, allowed, err)
		}
		if err := svc.Record(context.Background(), "series:tvdb:1:1:1", ResultNoResult); err != nil {
			t.Fatal(err)
		}
	}
	if allowed, _ := svc.Allowed(context.Background(), "series:tvdb:1:1:1"); allowed {
		t.Fatal("budget did not suppress at cap")
	}
	now = now.Add(7 * 24 * time.Hour)
	if allowed, _ := svc.Allowed(context.Background(), "series:tvdb:1:1:1"); !allowed {
		t.Fatal("expired suppression did not permit probe")
	}
	if err := svc.Record(context.Background(), "series:tvdb:1:1:1", ResultNoResult); err != nil {
		t.Fatal(err)
	}
	if allowed, _ := svc.Allowed(context.Background(), "series:tvdb:1:1:1"); allowed {
		t.Fatal("failed probe did not extend suppression")
	}
}

func TestNonCountableResultsAndFound(t *testing.T) {
	repo := &memoryRepo{states: map[string]State{"movie:tmdb:1": {ConsecutiveNoResults: 1}}}
	svc, _ := New(repo, Options{Cap: 2, Suppress: time.Hour})
	for _, result := range []Result{ResultFiltered, ResultTransient, ResultAccount} {
		if err := svc.Record(context.Background(), "movie:tmdb:1", result); err != nil {
			t.Fatal(err)
		}
	}
	if got := repo.states["movie:tmdb:1"].ConsecutiveNoResults; got != 1 {
		t.Fatalf("non-countable results changed count to %d", got)
	}
	if err := svc.Record(context.Background(), "movie:tmdb:1", ResultFound); err != nil {
		t.Fatal(err)
	}
	if _, ok := repo.states["movie:tmdb:1"]; ok {
		t.Fatal("found result did not reset budget")
	}
}

func TestBeginSerializesIdentityAndCancellationDoesNotLeak(t *testing.T) {
	repo := &memoryRepo{states: map[string]State{}}
	svc, _ := New(repo, Options{Cap: 2, Suppress: time.Hour})
	allowed, release, err := svc.Begin(context.Background(), "movie:tmdb:1")
	if err != nil || !allowed {
		t.Fatalf("first begin: allowed=%v err=%v", allowed, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	result := make(chan error, 1)
	go func() {
		defer wg.Done()
		_, _, err := svc.Begin(ctx, "movie:tmdb:1")
		result <- err
	}()
	cancel()
	if err := <-result; err == nil {
		t.Fatal("cancelled waiter returned nil error")
	}
	release()
	wg.Wait()
	svc.mu.Lock()
	defer svc.mu.Unlock()
	if len(svc.active) != 0 {
		t.Fatalf("active admissions leaked: %d", len(svc.active))
	}
}

func TestCanonicalKeyRequiresStableIdentity(t *testing.T) {
	id := &mediaidentity.ProviderIdentity{Kind: "series", IDs: mediaidentity.ProviderIDs{TVDB: "00123"}}
	if got, ok := CanonicalKey(id, 2, 3); !ok || got != "series:tvdb:123:2:3" {
		t.Fatalf("CanonicalKey = %q, %v", got, ok)
	}
	if _, ok := CanonicalKey(&mediaidentity.ProviderIdentity{Kind: "movie", Title: "mutable"}, 0, 0); ok {
		t.Fatal("title-only identity must abstain")
	}
	if _, ok := CanonicalKey(id, 0, 1); ok {
		t.Fatal("episode without season must abstain")
	}
}

func FuzzCanonicalKey(f *testing.F) {
	f.Add("series", "123", "", "", 1, 2)
	f.Add("movie", "", "456", "", 0, 0)
	f.Add("movie", "", "", "tt0000001", 0, 0)
	f.Fuzz(func(t *testing.T, kind, tvdb, tmdb, imdb string, season, episode int) {
		key, ok := CanonicalKey(&mediaidentity.ProviderIdentity{
			Kind: kind, IDs: mediaidentity.ProviderIDs{TVDB: tvdb, TMDB: tmdb, IMDB: imdb},
		}, season, episode)
		if ok && (key == "" || len(key) > 80) {
			t.Fatalf("invalid accepted key length %d", len(key))
		}
	})
}
