package api

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/availability"
	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

const (
	searchAvailabilityMaxCandidates = 200
	searchAvailabilityMaxProviders  = 8
	searchAvailabilityLiveTop       = 8
	searchAvailabilityWorkers       = 4
	searchAvailabilityTimeout       = 5 * time.Second
)

type searchAvailabilityVerdict uint8

const (
	searchAvailabilityUnknown searchAvailabilityVerdict = iota
	searchAvailabilityUncached
	searchAvailabilityCached
)

type searchAvailabilityResult struct {
	hash     string
	provider string
	verdict  searchAvailabilityVerdict
}

// searchTorrentAvailability combines TS-5.1's own-traffic observations with
// a small live refresh for providers that expose a real cache oracle. A live
// answer replaces learned evidence for only that exact provider/hash pair;
// errors and cancellation leave the learned verdict in place.
func (s *Server) searchTorrentAvailability(r *http.Request, hashes []string) (map[string]bool, map[string]bool) {
	hashes = normalizedSearchHashes(hashes)
	providers := s.searchAvailabilityProviders()
	verdicts := make(map[string]map[string]searchAvailabilityVerdict, len(hashes))

	for _, hash := range hashes {
		perProvider := make(map[string]searchAvailabilityVerdict, len(providers))
		for _, p := range providers {
			if verdict := s.learnedSearchAvailability(r.Context(), p.Name(), hash); verdict != searchAvailabilityUnknown {
				perProvider[p.Name()] = verdict
			}
		}
		verdicts[hash] = perProvider
	}

	type task struct {
		hash     string
		provider provider.Provider
	}
	tasks := make([]task, 0, searchAvailabilityLiveTop*len(providers))
	for _, hash := range hashes[:min(len(hashes), searchAvailabilityLiveTop)] {
		for _, p := range providers {
			if providerCaps(p).CacheOracle {
				tasks = append(tasks, task{hash: hash, provider: p})
			}
		}
	}
	if len(tasks) > 0 {
		ctx, cancel := context.WithTimeout(r.Context(), searchAvailabilityTimeout)
		defer cancel()
		jobs := make(chan task, len(tasks))
		results := make(chan searchAvailabilityResult, len(tasks))
		for _, task := range tasks {
			jobs <- task
		}
		close(jobs)

		workers := min(searchAvailabilityWorkers, len(tasks))
		var wg sync.WaitGroup
		wg.Add(workers)
		for range workers {
			go func() {
				defer wg.Done()
				for task := range jobs {
					if ctx.Err() != nil {
						return
					}
					lease, err := s.acquireSearchAvailabilityLease(ctx, task.hash)
					if err != nil {
						return
					}
					hash := task.hash
					res, err := task.provider.CheckCached(ctx, &store.Item{
						SourceType: store.SourceTypeTorrent,
						InfoHash:   &hash,
					})
					if lease != nil {
						lease.Release()
					}
					if err != nil || res == nil {
						continue
					}
					verdict := searchAvailabilityUncached
					if res.Cached {
						verdict = searchAvailabilityCached
					}
					results <- searchAvailabilityResult{hash: hash, provider: task.provider.Name(), verdict: verdict}
				}
			}()
		}
		wg.Wait()
		close(results)
		for result := range results {
			verdicts[result.hash][result.provider] = result.verdict
			// Persist the live verdict so coverage compounds across
			// searches. Only genuine oracle answers reach here -- an error
			// or cancelled check never emits a result -- so this cannot
			// turn an unknown into a labelled verdict, which is the
			// invariant TestEligibleTorrentsDerankDoesNotLabelUnknownCache-
			// State defends. Without this, every search re-checked the same
			// searchAvailabilityLiveTop candidates and anything ranked
			// deeper stayed permanently unknown, so genuine releases buried
			// in a large indexer response could never become eligible.
			// Best-effort and non-blocking, exactly like the submit/stream
			// writers: a failure is logged and changes no search behavior.
			s.recordHashAvailability(r.Context(), result.provider, result.hash,
				result.verdict == searchAvailabilityCached, availability.SourceSearch)
		}
	}

	cached := make(map[string]bool, len(hashes))
	checked := make(map[string]bool, len(hashes))
	for hash, perProvider := range verdicts {
		for _, verdict := range perProvider {
			if verdict != searchAvailabilityUnknown {
				checked[hash] = true
			}
			if verdict == searchAvailabilityCached {
				cached[hash] = true
			}
		}
	}
	return cached, checked
}

func normalizedSearchHashes(hashes []string) []string {
	out := make([]string, 0, min(len(hashes), searchAvailabilityMaxCandidates))
	seen := make(map[string]struct{}, cap(out))
	for _, hash := range hashes {
		hash, ok := availability.NormalizeHash(hash)
		if !ok {
			continue
		}
		if _, ok := seen[hash]; ok {
			continue
		}
		seen[hash] = struct{}{}
		out = append(out, hash)
		if len(out) == searchAvailabilityMaxCandidates {
			break
		}
	}
	return out
}

func (s *Server) searchAvailabilityProviders() []provider.Provider {
	providers := make([]provider.Provider, 0, searchAvailabilityMaxProviders)
	seen := make(map[string]struct{}, searchAvailabilityMaxProviders)
	add := func(p provider.Provider) {
		if p == nil || len(providers) == searchAvailabilityMaxProviders {
			return
		}
		name, ok := availability.NormalizeProvider(p.Name())
		if !ok {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		providers = append(providers, p)
	}
	for _, name := range s.providerOrder {
		add(s.allProviders[name])
	}
	if len(providers) == 0 {
		add(s.prov)
	}
	if len(providers) == 0 && len(s.allProviders) > 0 {
		names := make([]string, 0, len(s.allProviders))
		for name := range s.allProviders {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			add(s.allProviders[name])
		}
	}
	return providers
}

// learnedSearchAvailability accepts one direct own-traffic observation as the
// confidence floor: every recognized source is an exact provider-or-byte
// outcome, not a heuristic. The configured TS-5.1 TTL is the age ceiling.
func (s *Server) learnedSearchAvailability(ctx context.Context, providerName, hash string) searchAvailabilityVerdict {
	providerName, ok := availability.NormalizeProvider(providerName)
	if !ok {
		return searchAvailabilityUnknown
	}
	hash, ok = availability.NormalizeHash(hash)
	if !ok {
		return searchAvailabilityUnknown
	}
	observation, found, err := s.store.GetHashAvailability(ctx, providerName, hash)
	if err != nil || !found || !availability.ValidSource(availability.Source(observation.Source)) {
		return searchAvailabilityUnknown
	}
	age := s.nowUTC().Sub(observation.ObservedAt)
	if age < 0 || age >= s.cfg.AvailabilityTTL() {
		return searchAvailabilityUnknown
	}
	if observation.Cached {
		return searchAvailabilityCached
	}
	return searchAvailabilityUncached
}

func (s *Server) acquireSearchAvailabilityLease(ctx context.Context, session string) (*accountgov.Lease, error) {
	if s.torrentGov == nil {
		return nil, nil
	}
	return s.torrentGov.Acquire(ctx, TorrentCDNGovOp, accountgov.PriorityGrab, session)
}
