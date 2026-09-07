package provider

import (
	"context"
	"net/http"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/store"
)

// SubmitOptions carries per-submission flags shared across debrid-style
// providers.
type SubmitOptions struct {
	// AddOnlyIfCached asks the provider to reject the submission if the
	// payload is not already in its global cache (redundant safety net
	// behind the cachegate; preserved verbatim from the pre-inversion
	// submit path).
	AddOnlyIfCached bool
}

// Provider is the neutral interface all debrid-style source plugins
// implement. Core depends only on this interface and the neutral types in
// plan §3.1.2).
type Provider interface {
	// Name returns the registry name of this provider instance.
	Name() string

	// Capabilities returns optional provider capabilities used by core routing.
	Capabilities() Capabilities

	// CheckCached checks whether the given submission is already cached
	// in the provider's global cache.
	CheckCached(ctx context.Context, item *store.Item) (*CheckCachedResult, error)

	// Submit submits the item to the provider for resolution.
	// Called only after CheckCached confirms the item is cached,
	// or the governor explicitly allows an uncached submission.
	Submit(ctx context.Context, item *store.Item, opts SubmitOptions) (*CreateTaskResponse, error)

	// Poll returns the current status of a submitted item.
	Poll(ctx context.Context, item *store.Item) (*TaskStatus, error)

	// RequestDownloadURL returns a time-limited direct download URL for
	// a specific file within a resolved item.
	RequestDownloadURL(ctx context.Context, item *store.Item, fileID string) (string, error)

	// Remove cancels and removes an item from the provider.
	Remove(ctx context.Context, item *store.Item) error
}

// SlotStatus describes a provider's live capacity for uncached submissions
// (R2 governor redesign). Providers without a meaningful slot concept (e.g.
// NNTP) simply do not implement SlotSource — callers must type-assert.
type SlotStatus struct {
	// AllowedActiveSlots is the provider-defined concurrent uncached-transfer
	// ceiling. Always >= 1.
	AllowedActiveSlots int
	// ActiveCount is the live count of uncached transfers occupying that ceiling.
	ActiveCount int
	// CooldownUntil is non-zero when the provider's abuse/rate cooldown is
	// in effect; submissions should be held until this time passes.
	CooldownUntil time.Time
	// FreeTier reports whether the account is on the provider's free plan,
	// which may carry additional time-windowed budgets (e.g. TorBox Free:
	// 1/24h + 10/month) on top of the slot ceiling.
	FreeTier bool
}

// SlotSource is implemented by providers that support slot-based governance
// for uncached submissions. Optional capability: checked via type assertion
// since not every provider has an active-slot concept.
type SlotSource interface {
	SlotStatus(ctx context.Context) (*SlotStatus, error)
}

// MetainfoSource is implemented by providers that can hand back the raw
// bencoded .torrent file they already hold for an item previously submitted
// to that provider account (TS-0.2/T1: "for magnet-only adds, fetch the
// metainfo from the provider after submit -- providers hold the .torrent
// once added"). This is deliberately NOT a DHT/peer/tracker lookup by
// infohash alone -- it only returns metainfo for an item this account
// already added, so it does not reintroduce the "no DHT/peer fetching"
// constraint (Sec 0) that rules out endpoints which query the BitTorrent
// network for an arbitrary hash.
//
// Optional capability: checked via type assertion. A provider that cannot
// supply this (Real-Debrid has no equivalent endpoint) simply does not
// implement it; absence is always non-fatal per T1.
type MetainfoSource interface {
	// FetchTorrentFile returns the raw .torrent bytes the provider holds
	// for remoteID, if any. A provider is free to fail this (e.g. TorBox's
	// exportdata is documented as incompatible with already-cached
	// downloads) -- callers must treat any error as best-effort absence,
	// never as a fatal condition.
	FetchTorrentFile(ctx context.Context, remoteID string) ([]byte, error)
}

// Capabilities describes provider abilities beyond the base Provider methods.
// Core reads these to route fulfillment without knowing which provider is
// active. Provider-specific knowledge stays in provider packages.
type Capabilities struct {
	// UsenetIngest reports whether this provider can accept NZB submissions and
	// fulfill them through its own CDN instead of raw NNTP streaming.
	UsenetIngest bool

	// C-1: Capability matrix extension.

	// CacheOracle reports whether this provider exposes a pre-submit
	// instant-availability check (TorBox: checkcached; Real-Debrid removed
	// theirs Nov 2023 — assume false for RD/AD until live-probed). When
	// false, the cached-first selection design must use add-then-verify
	// (submit + one poll + delete if not instant) or uncached-only lanes.
	CacheOracle bool

	// SlotModel reports whether this provider has a meaningful
	// Allowed Active Slots concept that the governor should gate on.
	// False means the governor falls back to rolling-window budget alone.
	SlotModel bool
}

// UsenetProvider is the parallel interface for usenet streaming providers
// (plan §3.1.4). Probe and Stream are lifted verbatim from the concrete
// NNTPProvider signatures; Newshosting/NNTP is implementation #1 with zero
//
// RAR streaming is exposed through the JSON manifest form
// (StreamRARManifest) rather than the concrete []nntp.RARPart signature:
// the manifest's storage representation is already the JSON string carried
// on store.Item.RarManifest, and lifting []RARPart verbatim would drag
// nntp.NZBSegment into this package or force a provider->nntp import —
// inverting the dependency direction. The concrete StreamRAR([]RARPart, ...)
// remains unchanged on NNTPProvider; the interface method is a thin
// unmarshal-and-delegate wrapper (documented deviation, gate artifact §F-A).
type UsenetProvider interface {
	// Name returns the registry name of this provider instance.
	Name() string

	// Probe fetches the first probeBytes bytes of the main file in the NZB
	// and writes them to a temp file for ffprobe. Caller removes the file.
	Probe(ctx context.Context, nzbData []byte, probeBytes int64) (tmpPath string, err error)

	// Stream streams one file of the NZB to w, honoring rangeHeader.
	Stream(ctx context.Context, itemID string, nzbData []byte, fileIndex int, w http.ResponseWriter, rangeHeader string) error

	// CorrectedTotal returns the actual decoded byte total for one NZB file
	// from the segment offset cache. Returns 0 when cache is cold (first play).
	CorrectedTotal(ctx context.Context, itemID string, nzbData []byte, fileIndex int) int64

	// StreamRARManifest streams RAR-packed video bytes described by the
	// JSON-encoded manifest (the store.Item.RarManifest representation),
	// honoring rangeHeader.
	StreamRARManifest(ctx context.Context, manifestJSON string, totalVideoBytes int64, w http.ResponseWriter, rangeHeader, itemID, contentKey string) error

	// StreamZIPEntry streams one video entry from a ZIP archive described by
	// the JSON-encoded manifest (store.Item.ZIPManifest). entryIdx selects
	// which ZIPEntry to serve (same slot as NZB fileIndex). Supports
	// byte-range requests for stored entries; deflate entries seek by discard.
	StreamZIPEntry(ctx context.Context, manifestJSON string, entryIdx int, w http.ResponseWriter, rangeHeader, itemID, contentKey string) error

	// GrabTimeHealthCheck (NS-6.3) bounded-STAT-samples nzbData -- up to
	// sampleSize segments spread across the NZB, reusing the exact NS-6.1
	// sampling/audit primitives (SelectSampleSegments/RunHealthAudit) -- and
	// reports whether every conclusively-answered sample came back missing
	// (a "dead post" signature: sampled > 0 && every one of them missing).
	// sampled == 0 means inconclusive (parse failure, no segments, or every
	// candidate provider failed transiently/timed out) and allDead is
	// always false in that case: an inconclusive grab-time check must never
	// reject a plausibly-healthy grab, matching NS-6.1's own established
	// inconclusive-never-decay precedent. Only primitive/stdlib types cross
	// this interface boundary (no nntp.* types) so internal/provider never
	// imports internal/nntp -- internal/nntp already imports internal/provider
	// for interface conformance, and the reverse would cycle.
	GrabTimeHealthCheck(ctx context.Context, nzbData []byte, sampleSize int) (allDead bool, sampled int, err error)
}
