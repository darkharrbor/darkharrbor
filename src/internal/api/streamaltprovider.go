package api

// streamaltprovider.go — TS-2.2: bounded alternate-provider same-hash
// midstream failover (T3 chunk-acquisition-ladder rung 4).
//
// Scope: this is a per-fetch, ephemeral byte-source failover, not TS-4.1's
// permanent repair re-homing. Confirming/submitting against an alternate
// provider here NEVER mutates or persists the real *store.Item: every call
// into provider.Provider uses a shallow, unpersisted copy so item.Provider
// (the primary binding TS-4.1 owns) and item.RemoteID/QueuedID are
// untouched. A successful rung 4 only changes the in-memory cdnChunkSource
// for the remainder of the current stream (see streamcdn.go Fetch).
import (
	"context"
	"fmt"
	"path"

	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// altProviderCandidate is one alternate provider's bounded confirm+resolve
// closure for rung 4. confirm performs at most one real bounded cached
// check (or add-then-verify where no cache oracle exists) and, on success,
// returns a cdnReResolver-compatible function that yields that provider's
// CDN URL for the same logical file. Never retried internally -- exactly
// the frozen T3 spec's "one bounded confirm".
type altProviderCandidate struct {
	name    string
	confirm func(ctx context.Context) (cdnReResolver, error)
}

// buildAltProviderCandidates returns TS-2.2 rung 4 candidates: every
// configured debrid provider other than primary, in providerOrder (plan
// order, deterministic), for a torrent item with a known infohash. Returns
// nil (rung 4 becomes a structural no-op) for NZB/HTTP items, items with no
// infohash, or when fewer than two providers are configured -- exactly the
// cases where "another configured provider" cannot exist.
func buildAltProviderCandidates(item *store.Item, targetName string, targetSize int64, primary provider.Provider, allProviders map[string]provider.Provider, providerOrder []string, observe func(context.Context, string, string)) []altProviderCandidate {
	if item == nil || item.SourceType != store.SourceTypeTorrent {
		return nil
	}
	if item.InfoHash == nil || *item.InfoHash == "" {
		return nil
	}
	if targetSize <= 0 {
		// No known target size means file matching (matchAltFile) cannot
		// be trusted; refuse rung 4 rather than risk streaming the wrong
		// file from an alternate provider.
		return nil
	}
	primaryName := ""
	if primary != nil {
		primaryName = primary.Name()
	}

	var out []altProviderCandidate
	for _, name := range providerOrder {
		if name == primaryName {
			continue
		}
		p, ok := allProviders[name]
		if !ok || p == nil {
			continue
		}
		out = append(out, altProviderCandidate{
			name: name,
			confirm: func(ctx context.Context) (cdnReResolver, error) {
				resolve, err := confirmAltProvider(ctx, p, item, targetName, targetSize)
				if err == nil && ctx.Err() == nil && observe != nil {
					observe(ctx, p.Name(), *item.InfoHash)
				}
				return resolve, err
			},
		})
	}
	return out
}

// confirmAltProvider performs one bounded confirm against provider p for
// item's infohash and returns a resolver for the matching file's CDN URL.
//
// A provider exposing a cache oracle (Capabilities().CacheOracle, e.g.
// TorBox) is checked with one CheckCached call. A provider without one
// (e.g. Real-Debrid -- instantAvailability disabled, error_code 37) uses
// the T4/T6-established add-then-verify pattern: one Submit
// (AddOnlyIfCached: true) + one Poll, with best-effort Remove cleanup if
// the add-then-verify does not come back ready, so a failed confirm never
// leaves debris on the alternate account.
//
// item is never mutated: a shallow copy carries the confirm/submit
// identity, and its own RemoteID (once known) stays local to the returned
// resolver closure -- never written back to the real item or the DB.
func confirmAltProvider(ctx context.Context, p provider.Provider, item *store.Item, targetName string, targetSize int64) (cdnReResolver, error) {
	altItem := *item
	altItem.RemoteID = nil
	altItem.QueuedID = nil

	if p.Capabilities().CacheOracle {
		res, err := p.CheckCached(ctx, &altItem)
		if err != nil {
			return nil, fmt.Errorf("checkcached: %w", err)
		}
		if !res.Cached {
			return nil, fmt.Errorf("not cached on %s", p.Name())
		}
	}

	resp, err := p.Submit(ctx, &altItem, provider.SubmitOptions{AddOnlyIfCached: true})
	if err != nil {
		return nil, fmt.Errorf("submit: %w", err)
	}
	remoteID := resp.RemoteID
	altItem.RemoteID = &remoteID

	status, err := p.Poll(ctx, &altItem)
	if err != nil {
		return nil, fmt.Errorf("poll: %w", err)
	}
	if !status.DownloadReady && !status.DownloadPresent {
		// Best-effort cleanup: never leave a non-cached add-then-verify
		// attempt sitting on the alternate account. Uses a background
		// context deliberately -- cleanup must not be cut short by the
		// caller's own (likely already-cancelled) fetch context.
		_ = p.Remove(context.Background(), &altItem)
		return nil, fmt.Errorf("not ready on %s after add-then-verify", p.Name())
	}

	altFileID, ok := matchAltFile(status.Files, status.CachedFiles, targetName, targetSize)
	if !ok {
		return nil, fmt.Errorf("no matching file on %s (name=%q size=%d)", p.Name(), targetName, targetSize)
	}

	resolvedItem := altItem
	resolvedFileID := altFileID
	return func(rctx context.Context) (string, error) {
		return p.RequestDownloadURL(rctx, &resolvedItem, resolvedFileID)
	}, nil
}

// matchAltFile locates the alternate provider's file matching the primary
// provider's target file, since provider-issued file IDs are not portable
// across providers. Preference order: exact name match, then basename
// match (paths may differ), both requiring an exact size match too; a
// same-size-only match is accepted only when it is unique, since a
// collision there means the alternate release layout cannot be trusted to
// mean the same file -- refusing (rather than guessing) prevents ever
// streaming the wrong file's bytes under the original file's identity.
func matchAltFile(files []provider.RemoteFile, cachedFiles []provider.CachedFile, targetName string, targetSize int64) (string, bool) {
	base := path.Base(targetName)

	for _, f := range files {
		if f.Size == targetSize && (f.Name == targetName || path.Base(f.Name) == base || path.Base(f.RelativePath) == base) {
			return f.FileID, true
		}
	}
	for _, f := range cachedFiles {
		if f.Size == targetSize && (f.Name == targetName || path.Base(f.Name) == base || path.Base(f.RelativePath) == base) {
			return f.FileID, true
		}
	}

	var sizeMatchID string
	count := 0
	for _, f := range files {
		if f.Size == targetSize {
			sizeMatchID = f.FileID
			count++
		}
	}
	for _, f := range cachedFiles {
		if f.Size == targetSize {
			sizeMatchID = f.FileID
			count++
		}
	}
	if count == 1 {
		return sizeMatchID, true
	}
	return "", false
}
