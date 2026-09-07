// Package searchbudget bounds repeated successful searches that return no
// releases for one stable media identity.
package searchbudget

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/mediaidentity"
)

type Result string

const (
	ResultFound     Result = "found"
	ResultNoResult  Result = "no_result"
	ResultFiltered  Result = "filtered"
	ResultTransient Result = "transient"
	ResultAccount   Result = "account"
)

type State struct {
	ConsecutiveNoResults int
	SuppressedUntil      time.Time
}

type Repository interface {
	GetSearchBudget(context.Context, string) (State, bool, error)
	RecordSearchNoResult(context.Context, string, int, time.Duration, time.Time) (State, error)
	ResetSearchBudget(context.Context, string) error
}

type Options struct {
	Cap      int
	Suppress time.Duration
	Now      func() time.Time
}

type Service struct {
	repo     Repository
	cap      int
	suppress time.Duration
	now      func() time.Time
	mu       sync.Mutex
	active   map[string]*admission
}

type admission struct {
	gate chan struct{}
	refs int
}

const maxActiveIdentities = 1024

func New(repo Repository, opts Options) (*Service, error) {
	if repo == nil {
		return nil, fmt.Errorf("searchbudget: nil repository")
	}
	if opts.Cap < 1 || opts.Suppress <= 0 {
		return nil, fmt.Errorf("searchbudget: invalid limits")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{
		repo: repo, cap: opts.Cap, suppress: opts.Suppress, now: opts.Now,
		active: make(map[string]*admission),
	}, nil
}

// Begin serializes the check/search/record cycle for one identity. The caller
// must invoke release after Record (or after an aborted search). Waiters are
// cancellable and the keyed state is removed as soon as its last user leaves.
func (s *Service) Begin(ctx context.Context, key string) (allowed bool, release func(), err error) {
	s.mu.Lock()
	a := s.active[key]
	if a == nil {
		if len(s.active) >= maxActiveIdentities {
			s.mu.Unlock()
			return false, func() {}, fmt.Errorf("searchbudget: admission capacity reached")
		}
		a = &admission{gate: make(chan struct{}, 1)}
		a.gate <- struct{}{}
		s.active[key] = a
	}
	a.refs++
	s.mu.Unlock()

	select {
	case <-ctx.Done():
		s.dropAdmission(key, a)
		return false, func() {}, ctx.Err()
	case <-a.gate:
	}
	released := false
	release = func() {
		s.mu.Lock()
		if !released {
			released = true
			a.gate <- struct{}{}
			s.dropAdmissionLocked(key, a)
		}
		s.mu.Unlock()
	}
	allowed, err = s.Allowed(ctx, key)
	if err != nil {
		release()
		return false, func() {}, err
	}
	return allowed, release, nil
}

func (s *Service) dropAdmission(key string, a *admission) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropAdmissionLocked(key, a)
}

func (s *Service) dropAdmissionLocked(key string, a *admission) {
	a.refs--
	if a.refs == 0 {
		delete(s.active, key)
	}
}

// Allowed reports whether the identity may consume an upstream search now.
func (s *Service) Allowed(ctx context.Context, key string) (bool, error) {
	state, found, err := s.repo.GetSearchBudget(ctx, key)
	if err != nil || !found {
		return !found, err
	}
	return !state.SuppressedUntil.After(s.now().UTC()), nil
}

// Record applies only the two state-changing results. Local filtering and
// provider/account failures deliberately leave the durable budget untouched.
func (s *Service) Record(ctx context.Context, key string, result Result) error {
	switch result {
	case ResultFound:
		return s.repo.ResetSearchBudget(ctx, key)
	case ResultNoResult:
		_, err := s.repo.RecordSearchNoResult(ctx, key, s.cap, s.suppress, s.now().UTC())
		return err
	case ResultFiltered, ResultTransient, ResultAccount:
		return nil
	default:
		return fmt.Errorf("searchbudget: unknown result %q", result)
	}
}

// CanonicalKey returns a stable, URL-free key. An identity without a stable
// external catalog ID abstains instead of falling back to mutable title text.
func CanonicalKey(identity *mediaidentity.ProviderIdentity, season, episode int) (string, bool) {
	if identity == nil || (identity.Kind != "series" && identity.Kind != "movie") {
		return "", false
	}
	if season < 0 || episode < 0 || episode > 0 && season < 1 {
		return "", false
	}
	idType, id := firstID(identity)
	if id == "" {
		return "", false
	}
	parts := []string{identity.Kind, idType, id}
	if identity.Kind == "series" {
		parts = append(parts, strconv.Itoa(season), strconv.Itoa(episode))
	}
	return strings.Join(parts, ":"), true
}

func firstID(identity *mediaidentity.ProviderIdentity) (string, string) {
	if id := numericID(identity.IDs.TVDB); id != "" {
		return "tvdb", id
	}
	if id := numericID(identity.IDs.TMDB); id != "" {
		return "tmdb", id
	}
	if id := imdbID(identity.IDs.IMDB); id != "" {
		return "imdb", id
	}
	return "", ""
}

func numericID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 20 {
		return ""
	}
	for _, r := range raw {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return strings.TrimLeft(raw, "0")
}

func imdbID(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if len(raw) < 3 || len(raw) > 20 || !strings.HasPrefix(raw, "tt") {
		return ""
	}
	if numericID(raw[2:]) == "" {
		return ""
	}
	return "tt" + numericID(raw[2:])
}
