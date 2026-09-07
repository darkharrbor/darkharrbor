package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/availability"
	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

type searchAvailabilityFakeProvider struct {
	*altFakeProvider

	mu        sync.Mutex
	results   map[string]bool
	err       error
	delay     time.Duration
	wait      <-chan struct{}
	calls     int
	active    int
	maxActive int
}

func newSearchAvailabilityFakeProvider(name string, oracle bool, results map[string]bool) *searchAvailabilityFakeProvider {
	return &searchAvailabilityFakeProvider{
		altFakeProvider: &altFakeProvider{name: name, caps: provider.Capabilities{CacheOracle: oracle}},
		results:         results,
	}
}

func (p *searchAvailabilityFakeProvider) CheckCached(ctx context.Context, item *store.Item) (*provider.CheckCachedResult, error) {
	p.mu.Lock()
	p.calls++
	p.active++
	if p.active > p.maxActive {
		p.maxActive = p.active
	}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.active--
		p.mu.Unlock()
	}()

	if p.delay > 0 {
		timer := time.NewTimer(p.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if p.wait != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-p.wait:
		}
	}
	if p.err != nil {
		return nil, p.err
	}
	if item == nil || item.InfoHash == nil {
		return nil, errors.New("missing hash")
	}
	return &provider.CheckCachedResult{Cached: p.results[*item.InfoHash]}, nil
}

func (p *searchAvailabilityFakeProvider) stats() (calls, active, maxActive int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, p.active, p.maxActive
}

func newSearchAvailabilityServer(t *testing.T, providers ...provider.Provider) (*Server, *store.Store, time.Time) {
	t.Helper()
	st := newRepairTestStore(t)
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	st.SetClock(func() time.Time { return now })
	cfg := &config.Config{}
	cfg.Availability.TTLHours = 72
	all := make(map[string]provider.Provider, len(providers))
	order := make([]string, 0, len(providers))
	for _, p := range providers {
		all[p.Name()] = p
		order = append(order, p.Name())
	}
	return &Server{
		cfg:           cfg,
		store:         st,
		allProviders:  all,
		providerOrder: order,
		nowFn:         func() time.Time { return now },
	}, st, now
}

func TestSearchTorrentAvailabilityUsesLearnedNoOracleEvidence(t *testing.T) {
	cachedHash := "1111111111111111111111111111111111111111"
	uncachedHash := "2222222222222222222222222222222222222222"
	noOracle := newSearchAvailabilityFakeProvider("realdebrid", false, nil)
	s, st, _ := newSearchAvailabilityServer(t, noOracle)
	ctx := context.Background()
	if err := st.RecordHashAvailability(ctx, "realdebrid", cachedHash, true, string(availability.SourceStream), s.cfg.AvailabilityTTL()); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordHashAvailability(ctx, "realdebrid", uncachedHash, false, string(availability.SourceStream), s.cfg.AvailabilityTTL()); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/torznab?t=search", nil)
	cached, checked := s.searchTorrentAvailability(req, []string{cachedHash, uncachedHash})
	if !cached[cachedHash] || !checked[cachedHash] {
		t.Fatalf("cached learned verdict missing: cached=%v checked=%v", cached, checked)
	}
	if cached[uncachedHash] || !checked[uncachedHash] {
		t.Fatalf("uncached learned verdict missing: cached=%v checked=%v", cached, checked)
	}
	if calls, _, _ := noOracle.stats(); calls != 0 {
		t.Fatalf("provider without cache oracle was called %d times", calls)
	}
}

func TestSearchTorrentAvailabilityAbstainsOnStaleFutureAndUnknownEvidence(t *testing.T) {
	staleHash := "3333333333333333333333333333333333333333"
	futureHash := "4444444444444444444444444444444444444444"
	unknownHash := "5555555555555555555555555555555555555555"
	noOracle := newSearchAvailabilityFakeProvider("realdebrid", false, nil)
	s, st, now := newSearchAvailabilityServer(t, noOracle)
	ctx := context.Background()

	st.SetClock(func() time.Time { return now.Add(-73 * time.Hour) })
	if err := st.RecordHashAvailability(ctx, "realdebrid", staleHash, true, string(availability.SourceStream), s.cfg.AvailabilityTTL()); err != nil {
		t.Fatal(err)
	}
	st.SetClock(func() time.Time { return now.Add(time.Hour) })
	if err := st.RecordHashAvailability(ctx, "realdebrid", futureHash, true, string(availability.SourceStream), s.cfg.AvailabilityTTL()); err != nil {
		t.Fatal(err)
	}
	st.SetClock(func() time.Time { return now })
	if _, err := st.DB().ExecContext(ctx, `
		INSERT INTO hash_availability (provider, info_hash, cached, source, observed_at, expires_at, hits)
		VALUES (?, ?, 1, 'unknown', ?, ?, 0)`,
		"realdebrid", unknownHash, now.Format(time.RFC3339Nano), now.Add(time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/torznab?t=search", nil)
	cached, checked := s.searchTorrentAvailability(req, []string{staleHash, futureHash, unknownHash, "not-hex"})
	if len(cached) != 0 || len(checked) != 0 {
		t.Fatalf("unsafe evidence did not abstain: cached=%v checked=%v", cached, checked)
	}
}

func TestSearchTorrentAvailabilityFreshLiveAnswerOverridesLearned(t *testing.T) {
	hash := "6666666666666666666666666666666666666666"
	oracle := newSearchAvailabilityFakeProvider("torbox", true, map[string]bool{hash: false})
	s, st, _ := newSearchAvailabilityServer(t, oracle)
	if err := st.RecordHashAvailability(context.Background(), "torbox", hash, true, string(availability.SourceStream), s.cfg.AvailabilityTTL()); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/torznab?t=search", nil)
	cached, checked := s.searchTorrentAvailability(req, []string{hash})
	if cached[hash] || !checked[hash] {
		t.Fatalf("fresh uncached answer did not override learned cached verdict: cached=%v checked=%v", cached, checked)
	}
	if calls, _, _ := oracle.stats(); calls != 1 {
		t.Fatalf("live calls = %d, want 1", calls)
	}
}

func TestSearchTorrentAvailabilityBoundsLiveRefreshAndWorkers(t *testing.T) {
	results := make(map[string]bool)
	hashes := make([]string, 0, 12)
	for i := 1; i <= 12; i++ {
		hash := strings.Repeat(strconv.FormatInt(int64(i%10), 10), 40)
		hashes = append(hashes, hash)
		results[hash] = true
	}
	oracle := newSearchAvailabilityFakeProvider("torbox", true, results)
	oracle.delay = 20 * time.Millisecond
	s, _, _ := newSearchAvailabilityServer(t, oracle)

	req := httptest.NewRequest(http.MethodGet, "/torznab?t=search", nil)
	cached, _ := s.searchTorrentAvailability(req, hashes)
	calls, active, maxActive := oracle.stats()
	if calls != searchAvailabilityLiveTop {
		t.Fatalf("live calls = %d, want %d", calls, searchAvailabilityLiveTop)
	}
	if active != 0 || maxActive > searchAvailabilityWorkers {
		t.Fatalf("worker bound violated: active=%d max=%d bound=%d", active, maxActive, searchAvailabilityWorkers)
	}
	if len(cached) != searchAvailabilityLiveTop {
		t.Fatalf("cached results = %d, want %d", len(cached), searchAvailabilityLiveTop)
	}
}

func TestSearchTorrentAvailabilityCancellationReleasesGovernorLease(t *testing.T) {
	hash := "7777777777777777777777777777777777777777"
	never := make(chan struct{})
	oracle := newSearchAvailabilityFakeProvider("torbox", true, nil)
	oracle.wait = never
	s, _, _ := newSearchAvailabilityServer(t, oracle)
	gov := accountgov.New("test")
	gov.SetCapacity(TorrentCDNGovOp, 1)
	s.torrentGov = gov

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/torznab?t=search", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.searchTorrentAvailability(req, []string{hash})
	}()

	deadline := time.Now().Add(time.Second)
	for {
		_, active, _ := oracle.stats()
		if active == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("live check did not start")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled search did not return")
	}
	if got := gov.InUse(TorrentCDNGovOp); got != 0 {
		t.Fatalf("governor lease leaked: in_use=%d", got)
	}
	if got := gov.Waiting(TorrentCDNGovOp); got != 0 {
		t.Fatalf("governor waiter leaked: waiting=%d", got)
	}
	if _, active, _ := oracle.stats(); active != 0 {
		t.Fatalf("provider call still active after cancellation: %d", active)
	}
}
