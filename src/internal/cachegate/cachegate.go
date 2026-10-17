package cachegate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/governor"
	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// Result is the outcome of a cache gate check.
type Result struct {
	Cached        bool
	Files         []provider.CachedFile
	AllowUncached bool
}

// Gate checks whether a submission should proceed and whether it is cached.
type Gate struct {
	client        provider.Provider
	governor      *governor.Governor
	requireCached bool
}

func New(client provider.Provider, gov *governor.Governor, requireCached bool) *Gate {
	return &Gate{client: client, governor: gov, requireCached: requireCached}
}

// CheckTimeout bounds one provider cache check.
var CheckTimeout = 15 * time.Second

// Check runs the cache check for an item.
// If requireCached is true (default), uncached submissions are rejected
// immediately with ErrNotCached regardless of the governor budget. This
// prevents uncached TorBox transfers when the item came from a non-Dark Harrbor
// indexer that bypassed the Torznab cache-first search.
func (g *Gate) Check(ctx context.Context, item *store.Item) (*Result, error) {
	// Bounded: after a provider 429 the client waits out Retry-After before
	// the call, and this runs inside an arr's qBit add request and the
	// one-at-a-time submit paths. A check that runs out its own deadline is
	// transient (retryable), so callers defer rather than fail the item.
	checkCtx, cancel := context.WithTimeout(ctx, CheckTimeout)
	checkResult, err := g.client.CheckCached(checkCtx, item)
	cancel()

	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			err = provider.MarkRetryable(err)
		}
		return nil, fmt.Errorf("cachegate: %w", err)
	}

	if checkResult.Cached {
		return &Result{Cached: true, Files: checkResult.Files}, nil
	}

	// Not cached. If RequireCached is set, reject immediately — do not
	// consume governor budget or submit to TorBox.
	if g.requireCached {
		return nil, fmt.Errorf("cachegate: not in TorBox cache (HARRBOR_REQUIRE_CACHED=true)")
	}

	// Provider slot governors apply only to uncached torrent acquisition.
	// NZB/NNTP/HTTP work has independent provider/account limits.
	if item.SourceType == store.SourceTypeTorrent {
		if err := g.governor.Allow(ctx); err != nil {
			return nil, fmt.Errorf("cachegate: %w", err)
		}
	}

	return &Result{AllowUncached: true}, nil
}

// RecordUncached records a successful uncached torrent submission against the
// governor log. Check may run before the item exists durably (the inline hot
// path probes cacheability before CreateItem), so recording is a separate step
// after the provider create succeeds and the item ID is known.
func (g *Gate) RecordUncached(ctx context.Context, item *store.Item) error {
	if g == nil || g.governor == nil || item == nil || item.SourceType != store.SourceTypeTorrent {
		return nil
	}
	return g.governor.Record(ctx, item.ID)
}
