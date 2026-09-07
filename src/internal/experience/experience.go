// Package experience implements TS-1.4 (torrent-plan T14): the shared owner
// of prewarm, seek-target prefetch, next-episode prewarm, and adaptive-
// readahead recommendation over the TS-1.1 bytesource/rangecache substrate.
//
// It is lane-agnostic: every method takes a rangecache.ChunkSource, so any
// lane's adapter (torrent/debrid-CDN, and in future rows NNTP and HTTP) can
// be prewarmed identically. This row wires only the torrent/debrid-CDN lane
// (internal/api, cmd/darkharrbor/main.go); NS-5.2-NS-5.5 and HR5.3/HR5.4
// wire NNTP/HTTP onto this same Service in their own future rows, per the
// master plan's "Prewarm and readahead" shared-subsystem ownership table.
//
// Bounded by construction (LC-01, C0.3, DG-09): every operation is wrapped
// in Config.Timeout regardless of the caller's own context, acquires an
// accountgov.PriorityPrewarm lease when a Governor is configured (so live
// playback demand is never starved — LC-06), and a merely-unsupported or
// truncated container is a legitimate bounded outcome, never an error.
//
// Secret discipline (DG-04/LC-02): this package never logs or persists an
// upstream URL, token, or credential — it operates purely on ChunkRef/Key
// identities and mediatruth's own secret-free Facts/Index types.
package experience

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/bytesource"
	"github.com/darkharrbor/darkharrbor/internal/mediatruth"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
)

// Config bounds one Service.
type Config struct {
	// Enabled gates every operation in this package to a no-op when false,
	// so a deployment can disable prewarm/adaptive-readahead entirely
	// without removing the wiring.
	Enabled bool
	// HotHeadBytes is how many leading bytes PrewarmHotHead pins.
	HotHeadBytes int64
	// Timeout bounds one PrewarmHotHead/SeekTargetPrefetch call — the
	// DG-09 "prewarm budget" — so a slow or stalled origin cannot hold a
	// prewarm lease indefinitely, regardless of the caller's own context.
	Timeout time.Duration
	// NextEpisodeThreshold is the playback-fraction (0.0-1.0) gate for
	// NextEpisodePrewarm.
	NextEpisodeThreshold float64
	// MinReadaheadWorkers / MaxReadaheadWorkers bound AdaptiveReadahead's
	// recommendation.
	MinReadaheadWorkers int
	MaxReadaheadWorkers int
}

// sourceStats is one source's last-measured throughput/bitrate, used by
// Workers to recommend a bounded readahead worker count.
type sourceStats struct {
	bitrateBps   int64
	perWorkerBps int64
	at           time.Time
}

// maxTrackedSources bounds the stats map so a long-running deployment
// touching many distinct items never grows this state unboundedly (DG-07).
const maxTrackedSources = 512

// Service is the shared TS-1.4 owner. gov may be nil (every operation then
// runs unleased — no priority arbitration, matching an unconfigured
// deployment's existing behavior exactly).
type Service struct {
	cache *rangecache.Cache
	gov   *accountgov.Governor
	govOp string
	log   *slog.Logger
	clock func() time.Time
	cfg   Config

	readahead AdaptiveReadahead

	statsMu sync.Mutex
	stats   map[string]sourceStats
}

// New constructs a Service. clock may be nil (defaults to time.Now, the
// program's standing injectable-clock convention, master §5.0).
func New(cache *rangecache.Cache, gov *accountgov.Governor, govOp string, cfg Config, log *slog.Logger, clock func() time.Time) *Service {
	if clock == nil {
		clock = time.Now
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 45 * time.Second
	}
	if cfg.MinReadaheadWorkers < 1 {
		cfg.MinReadaheadWorkers = 1
	}
	if cfg.MaxReadaheadWorkers < cfg.MinReadaheadWorkers {
		cfg.MaxReadaheadWorkers = cfg.MinReadaheadWorkers
	}
	return &Service{
		cache: cache,
		gov:   gov,
		govOp: govOp,
		log:   log,
		clock: clock,
		cfg:   cfg,
		readahead: AdaptiveReadahead{
			MinWorkers:     cfg.MinReadaheadWorkers,
			MaxWorkers:     cfg.MaxReadaheadWorkers,
			SafetyMultiple: 2.0,
		},
		stats: make(map[string]sourceStats),
	}
}

// PrewarmHotHead pins src's leading HotHeadBytes plus whatever additional
// bytes mediatruth.Analyze reads while locating the container's seek index
// (tail-located Cues/moov, or a targeted index read), all under one
// PriorityPrewarm governor lease so live playback demand is never starved
// (LC-06, DG-07). It is bounded and cancellable (DG-09): the whole call is
// wrapped in Config.Timeout regardless of the caller's own context.
//
// It never returns an error for a merely-unsupported or truncated
// container — that is a legitimate bounded outcome (e.g. an archive-wrapped
// item mediatruth cannot parse), not a prewarm failure, and the hot-head
// bytes already pinned before Analyze ran remain useful. Errors are
// returned only for a lease/context failure, or a pin failure on the very
// first chunk (an origin that cannot be reached at all).
func (s *Service) PrewarmHotHead(ctx context.Context, src rangecache.ChunkSource) (*mediatruth.Result, error) {
	return s.prewarm(ctx, src, true)
}

// PrewarmLeading pins only src's leading HotHeadBytes. NNTP uses this
// post-manifest variant because archive-aware index discovery belongs to
// NS-5.3; the bounded fetch, timeout, cache tier, and PriorityPrewarm lease
// remain owned by this shared service.
func (s *Service) PrewarmLeading(ctx context.Context, src rangecache.ChunkSource) error {
	_, err := s.prewarm(ctx, src, false)
	return err
}

func (s *Service) prewarm(ctx context.Context, src rangecache.ChunkSource, analyze bool) (*mediatruth.Result, error) {
	if !s.cfg.Enabled || s.cache == nil || src == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	if s.gov != nil {
		lease, err := s.gov.Acquire(ctx, s.govOp, accountgov.PriorityPrewarm, src.Key())
		if err != nil {
			return nil, fmt.Errorf("experience: prewarm lease: %w", err)
		}
		defer lease.Release()
	}

	refs := src.Chunks()
	if len(refs) == 0 {
		return nil, nil
	}

	start := s.clock()
	var pinnedBytes int64
	for i, ref := range refs {
		if pinnedBytes >= s.cfg.HotHeadBytes {
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := s.cache.GetChunkPinned(ctx, src, ref); err != nil {
			if i == 0 {
				return nil, fmt.Errorf("experience: hot-head pin: %w", err)
			}
			// A later chunk failing is logged and stops the pin loop, but
			// the leading chunks already pinned remain useful — never
			// fatal for a merely partial hot-head.
			if s.log != nil {
				s.log.Warn("experience: hot-head pin stopped early",
					"source", src.Key(), "chunk", ref.Index, "err", err)
			}
			break
		}
		pinnedBytes += ref.Size
	}
	if elapsed := s.clock().Sub(start); elapsed > 0 && pinnedBytes > 0 {
		measuredBps := int64(float64(pinnedBytes*8) / elapsed.Seconds())
		s.recordThroughput(src.Key(), measuredBps)
	}
	if !analyze {
		return nil, nil
	}

	bs, err := s.pinnedByteSource(src)
	if err != nil {
		return nil, fmt.Errorf("experience: byte source: %w", err)
	}
	res, aerr := mediatruth.Analyze(ctx, bs, mediatruth.Options{Now: s.clock})
	if aerr != nil {
		// Unsupported/truncated container is a legitimate bounded outcome,
		// not a prewarm failure: the hot-head bytes above are already
		// pinned and useful regardless.
		if s.log != nil {
			s.log.Info("experience: media truth unavailable during prewarm",
				"source", src.Key(), "reason", aerr.Error())
		}
		return nil, nil
	}
	if res.Facts.BitRate > 0 {
		s.recordBitrate(src.Key(), res.Facts.BitRate)
	}
	return res, nil
}

// SeekTargetPrefetch pins the chunk covering a known future seek target
// (resolved from a previously computed mediatruth.Index), letting a caller
// jump-start a predictable seek — e.g. a cold Range request past the
// pinned hot-head — rather than waiting for the passive reactive readahead
// a normal request would otherwise trigger cold.
func (s *Service) SeekTargetPrefetch(ctx context.Context, src rangecache.ChunkSource, index mediatruth.Index, targetMS int64) error {
	if !s.cfg.Enabled || s.cache == nil || src == nil {
		return nil
	}
	entry, ok := index.SeekPoint(targetMS)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	if s.gov != nil {
		lease, err := s.gov.Acquire(ctx, s.govOp, accountgov.PriorityPrewarm, src.Key())
		if err != nil {
			return fmt.Errorf("experience: seek-target lease: %w", err)
		}
		defer lease.Release()
	}
	var offset int64
	for _, ref := range src.Chunks() {
		end := offset + ref.Size
		if entry.Offset >= offset && entry.Offset < end {
			if _, err := s.cache.GetChunkPinned(ctx, src, ref); err != nil {
				return fmt.Errorf("experience: seek-target prefetch: %w", err)
			}
			return nil
		}
		offset = end
	}
	return nil
}

// NextEpisodePrewarm triggers PrewarmHotHead for the next item only once
// playback of the current one has crossed Config.NextEpisodeThreshold, so a
// speculative prewarm never starts the moment an episode begins.
//
// No current lane reaches a live playback-position signal into this row's
// call sites (torrent-lane season packs are not yet per-episode-mapped
// until TS-0.1 lands), so this method is proven only by deterministic
// test this row — mirroring how HR1.4's generic Governor shipped with no live
// call site until its first consuming row wired one.
func (s *Service) NextEpisodePrewarm(ctx context.Context, next rangecache.ChunkSource, playbackFraction float64) (*mediatruth.Result, error) {
	if !s.cfg.Enabled || playbackFraction < s.cfg.NextEpisodeThreshold {
		return nil, nil
	}
	return s.PrewarmHotHead(ctx, next)
}

// Workers recommends a bounded CDN readahead worker count for src, based on
// throughput/bitrate measured the last time PrewarmHotHead ran against it
// and current governor headroom. fallback is returned unchanged when no
// measurement exists yet or the service is disabled, so a cold or
// never-prewarmed source degrades to the caller's own static default —
// zero behavior change for a deployment that never prewarms.
func (s *Service) Workers(src rangecache.ChunkSource, fallback int) int {
	if !s.cfg.Enabled || src == nil {
		return fallback
	}
	stats, ok := s.statsFor(src.Key())
	if !ok || stats.perWorkerBps <= 0 {
		return fallback
	}
	headroom := -1
	if s.gov != nil {
		if capacity := s.gov.Capacity(s.govOp); capacity > 0 {
			headroom = capacity - s.gov.InUse(s.govOp)
			if headroom < 0 {
				headroom = 0
			}
		}
	}
	return s.readahead.Recommend(stats.bitrateBps, stats.perWorkerBps, headroom)
}

// pinnedByteSource adapts src into a bytesource.ByteSource whose every read
// is pinned into the shared hot-head tier as a side effect, so running
// mediatruth.Analyze through it warms exactly the bytes mediatruth itself
// determined the container needs (its own head/tail/index reads) — no
// separate bookkeeping is needed to know which extra bytes to pin.
func (s *Service) pinnedByteSource(src rangecache.ChunkSource) (bytesource.ByteSource, error) {
	refs := src.Chunks()
	chunks := make([]bytesource.Chunk, len(refs))
	starts := make([]int64, len(refs)+1)
	var acc int64
	for i, r := range refs {
		chunks[i] = bytesource.Chunk{Index: r.Index, Key: r.Key, DeclaredSize: r.Size}
		starts[i] = acc
		acc += r.Size
	}
	starts[len(refs)] = acc
	fetch := func(fctx context.Context, ch bytesource.Chunk) ([]byte, error) {
		return s.cache.GetChunkPinned(fctx, src, rangecache.ChunkRef{Index: ch.Index, Key: ch.Key, Size: ch.DeclaredSize})
	}
	var alignment int64
	if len(refs) > 0 {
		alignment = refs[0].Size
	}
	caps := bytesource.Capabilities{
		RangeSupport: true,
		ExactSize:    src.ExactSizes(),
		TailCost:     bytesource.TailCostCheap,
		Alignment:    alignment,
	}
	return bytesource.NewChunked(src.Key(), chunks, starts, acc, caps, fetch)
}

func (s *Service) statsFor(key string) (sourceStats, bool) {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	st, ok := s.stats[key]
	return st, ok
}

func (s *Service) recordThroughput(key string, bps int64) {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	s.pruneLocked()
	st := s.stats[key]
	st.perWorkerBps = bps
	st.at = s.clock()
	s.stats[key] = st
}

func (s *Service) recordBitrate(key string, bps int64) {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	s.pruneLocked()
	st := s.stats[key]
	st.bitrateBps = bps
	st.at = s.clock()
	s.stats[key] = st
}

// pruneLocked bounds the stats map to maxTrackedSources, dropping the
// oldest entry first, so a long-running deployment touching many distinct
// items never grows this state unboundedly (DG-07). Must be called with
// statsMu held.
func (s *Service) pruneLocked() {
	if len(s.stats) < maxTrackedSources {
		return
	}
	oldestKey := ""
	var oldestAt time.Time
	first := true
	for k, v := range s.stats {
		if first || v.at.Before(oldestAt) {
			oldestKey, oldestAt = k, v.at
			first = false
		}
	}
	if oldestKey != "" {
		delete(s.stats, oldestKey)
	}
}
