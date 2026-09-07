package nntp

// cache.go — NNTP adapter onto the shared internal/rangecache byte-range
//
// The former SegmentCache implementation (modes, sessions, LRU/TTL/tail
// eviction, atomic disk writes) moved verbatim-in-semantics to
// internal/rangecache. This file is the NNTP-specific remainder:
//   - the NZB-segment ChunkSource (message-id keyed, ExactSizes()==false so
//     apply in the shared write loop),
//
// SegmentCache's public behavior is reproduced, not redesigned: SetCache,
// StreamWithCache, GetSegment (RAR demand fetches), and Close keep their
// contracts for provider.go and rarstream.go.

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/experience"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
)

// CacheMode defines the caching behavior for NNTP segment retrieval.
// Values map 1:1 onto rangecache.Mode.
type CacheMode string

const (
	defaultTotalConnections     = 16
	defaultDemandConnections    = 8
	defaultReadaheadConnections = 8
	rarLayoutTTL                = 30 * time.Second

	// CacheModeNone disables all caching. Every segment is fetched live.
	CacheModeNone CacheMode = "none"
	// CacheModeReadahead enables read-ahead prefetching but no disk caching.
	CacheModeReadahead CacheMode = "readahead"
	// CacheModeDisk enables disk caching of segments with LRU/TTL eviction.
	CacheModeDisk CacheMode = "disk"
	// HARRBOR_READAHEAD_ENABLED is ignored in this mode — always true.
	CacheModeFull CacheMode = "full"
)

// CacheConfig holds configuration for the NNTP segment cache.
// Fields map directly to environment variables as documented.
// Boolean fields default to false (Go zero value); callers must set true
// where defaults are true (e.g. ReadaheadEnabled, TailEvict).
// DiskCachePath must be resolved to an absolute path by the caller before
// NewSegmentCache is called (default: HARRBOR_DATA_ROOT/.cache).
//
// Shared rangecache knobs (Readahead*, MinBuffer, TailEvict/Lookback,
// DiskCache*, FullEvict*) configure the SHARED cache instance and therefore
// also govern the /stream byte-proxy path; connection counts remain
// NNTP-only (plan §4.1.4).
type CacheConfig struct {
	Mode                 CacheMode
	TotalConnections     int
	DemandConnections    int
	ReadaheadConnections int
	ReadaheadEnabled     bool
	ReadaheadMaxSegments int
	// MaxDemandConnsPerFile limits simultaneous demand fetches for one stream.
	// Prevents one file from monopolizing the demand pool. 0 = unlimited.
	MaxDemandConnsPerFile int
	MinBufferSegments     int
	TailEvict             bool
	TailLookback          int
	DiskCacheSizeMB       int64
	DiskCacheTTLMin       int
	DiskCachePath         string
	FullEvictMode         string
	FullEvictTTLMin       int
}

// CacheConnectionOverrides reports whether a cache-connection knob came from
// explicit operator config (env or sealed secret) instead of package defaults.
type CacheConnectionOverrides struct {
	TotalExplicit     bool
	DemandExplicit    bool
	ReadaheadExplicit bool
}

// SegmentOffsetStore is the subset of store.Store used for decoded-size tracking.
// Defined here to avoid an import cycle (nntp → store → nntp).
type SegmentOffsetStore interface {
	RecordSegmentSize(ctx context.Context, itemID, contentKey string, fileIndex, segIndex int, decodedBytes int) error
	RecordFileTotal(ctx context.Context, itemID, contentKey string, fileIndex int, decodedBytes int64) error
	GetSegmentOffsets(ctx context.Context, itemID, contentKey string, fileIndex, segCount int, declaredSizes []int64) ([]int64, map[int]struct{}, error)
	GetFileTotal(ctx context.Context, itemID, contentKey string, fileIndex int) (int64, error)
}

type segmentOffsetFile struct {
	itemID    string
	fileIndex int
}

type rarLayoutCacheEntry struct {
	meta             []rarSegMeta
	refs             []rangecache.ChunkRef
	correctedOffsets []int64
	fileKey          string
	expires          time.Time
}

// SegmentCache manages NNTP segment retrieval through the shared rangecache:
// it owns the demand/readahead connection pools and adapts NZB segments to
// rangecache chunks.
type SegmentCache struct {
	cfg        CacheConfig
	mode       rangecache.Mode
	rc         *rangecache.Cache
	demandPool *Pool
	raPool     *Pool
	log        *slog.Logger
	host       string
	skipCRC    bool
	demandSeen sync.Once
	raSeen     sync.Once
	offsets    SegmentOffsetStore // nil when not wired
	offsetMu   sync.Mutex
	recorded   map[segmentOffsetFile]map[int]struct{}
	rarMu      sync.Mutex
	rarLayouts map[string]rarLayoutCacheEntry
	// expSvc (NS-5.5) is the shared TS-1.4 experience.Service, consulted for
	// an adaptive steady readahead-worker recommendation before each stream.
	// nil is safe throughout: steadyWorkers() below degrades to the
	// existing ReadaheadConnections default, unchanged from pre-NS-5.5.
	expSvc *experience.Service
}

// SetOffsetStore wires a SegmentOffsetStore for decoded-size recording and
// corrected seek offset lookup. Must be called before StreamWithCache. Safe
// to omit: without it seeking falls back to declared NZB sizes.
func (sc *SegmentCache) SetOffsetStore(s SegmentOffsetStore) { sc.offsets = s }

// SetExperience wires the shared experience.Service (NS-5.5) used to compute
// each stream's adaptive steady readahead-worker recommendation. Safe to
// omit: without it every stream uses the existing static
// ReadaheadConnections default, unchanged from pre-NS-5.5 behavior.
func (sc *SegmentCache) SetExperience(svc *experience.Service) { sc.expSvc = svc }

// steadyWorkers (NS-5.5) returns sc.expSvc's recommendation for src, capped
// by fallback (the existing static ReadaheadConnections ceiling) and never
// exceeding it -- this is the RESTING width, not a hard cap; rangecache's
// own ceiling sizing is unchanged. Returns 0 (no recommendation) when expSvc
// is nil or src has no prior measurement, exactly preserving pre-NS-5.5
// behavior for a deployment that never prewarms.
func (sc *SegmentCache) steadyWorkers(src rangecache.ChunkSource, fallback int) int {
	if sc.expSvc == nil {
		return 0
	}
	want := sc.expSvc.Workers(src, fallback)
	if want <= 0 || want >= fallback {
		return 0
	}
	return want
}

func (sc *SegmentCache) seedRecorded(itemID string, fileIndex int, indexes map[int]struct{}) {
	if len(indexes) == 0 {
		return
	}
	key := segmentOffsetFile{itemID: itemID, fileIndex: fileIndex}
	sc.offsetMu.Lock()
	defer sc.offsetMu.Unlock()
	if sc.recorded == nil {
		sc.recorded = make(map[segmentOffsetFile]map[int]struct{})
	}
	set := sc.recorded[key]
	if set == nil {
		set = make(map[int]struct{}, len(indexes))
		sc.recorded[key] = set
	}
	for index := range indexes {
		set[index] = struct{}{}
	}
}

// recordSegmentSize deduplicates both persisted and in-flight segment indexes.
// SegmentOffsetStore.RecordSegmentSize is a bounded, non-blocking enqueue.
func (sc *SegmentCache) recordSegmentSize(itemID, contentKey string, fileIndex, segIndex, decodedBytes int) {
	if sc.offsets == nil || itemID == "" {
		return
	}
	key := segmentOffsetFile{itemID: itemID, fileIndex: fileIndex}
	sc.offsetMu.Lock()
	defer sc.offsetMu.Unlock()
	if sc.recorded == nil {
		sc.recorded = make(map[segmentOffsetFile]map[int]struct{})
	}
	set := sc.recorded[key]
	if set == nil {
		set = make(map[int]struct{})
		sc.recorded[key] = set
	}
	if _, exists := set[segIndex]; exists {
		return
	}
	if err := sc.offsets.RecordSegmentSize(context.Background(), itemID, contentKey, fileIndex, segIndex, decodedBytes); err != nil {
		if sc.log != nil {
			sc.log.Debug("nntp: segment offset enqueue failed", "item", itemID, "file", fileIndex, "segment", segIndex, "err", err)
		}
		return
	}
	set[segIndex] = struct{}{}
}

func (sc *SegmentCache) recordFileTotal(itemID, contentKey string, fileIndex int, decodedBytes int64) {
	if sc.offsets == nil || itemID == "" || contentKey == "" || decodedBytes <= 0 {
		return
	}
	key := segmentOffsetFile{itemID: itemID, fileIndex: fileIndex}
	sc.offsetMu.Lock()
	defer sc.offsetMu.Unlock()
	if sc.recorded == nil {
		sc.recorded = make(map[segmentOffsetFile]map[int]struct{})
	}
	set := sc.recorded[key]
	if set == nil {
		set = make(map[int]struct{})
		sc.recorded[key] = set
	}
	if _, exists := set[-1]; exists {
		return
	}
	if err := sc.offsets.RecordFileTotal(context.Background(), itemID, contentKey, fileIndex, decodedBytes); err != nil {
		if sc.log != nil {
			sc.log.Debug("nntp: file total enqueue failed", "item", itemID, "file", fileIndex, "err", err)
		}
		return
	}
	set[-1] = struct{}{}
}

// NewSegmentCache creates a SegmentCache bound to the shared rangecache
// instance rc. Callers are responsible for applying env-var defaults before
// calling (booleans, DiskCachePath resolution). Numeric defaults and the
func NewSegmentCache(cfg CacheConfig, nntpCfg Config, rc *rangecache.Cache, log *slog.Logger) *SegmentCache {
	if cfg.TotalConnections == 0 && cfg.DemandConnections == 0 && cfg.ReadaheadConnections == 0 {
		cfg.TotalConnections = defaultTotalConnections
		cfg.DemandConnections = defaultDemandConnections
		cfg.ReadaheadConnections = defaultReadaheadConnections
	} else {
		if cfg.TotalConnections == 0 {
			cfg.TotalConnections = defaultTotalConnections
		}
		if cfg.DemandConnections < 0 {
			cfg.DemandConnections = 0
		}
		if cfg.ReadaheadConnections < 0 {
			cfg.ReadaheadConnections = 0
		}
	}
	if cfg.Mode == "" {
		cfg.Mode = CacheModeReadahead
	}

	if cfg.DemandConnections+cfg.ReadaheadConnections > cfg.TotalConnections {
		log.Warn("nntp: cache: connection guard violated, clamping readahead",
			"demand", cfg.DemandConnections,
			"readahead", cfg.ReadaheadConnections,
			"total", cfg.TotalConnections)
		cfg.ReadaheadConnections = cfg.TotalConnections - cfg.DemandConnections
		if cfg.ReadaheadConnections < 0 {
			cfg.ReadaheadConnections = 0
		}
	}

	if cfg.Mode == CacheModeFull {
		cfg.ReadaheadEnabled = true
	}

	demandPool := NewPool(PoolConfig{
		MaxConns:    cfg.DemandConnections,
		Host:        nntpCfg.Host,
		Port:        nntpCfg.Port,
		TLS:         nntpCfg.TLS,
		Username:    nntpCfg.Username,
		Password:    nntpCfg.Password,
		DialTimeout: 30 * time.Second,
		IdleTimeout: 5 * time.Minute,
		// NS-5.1: only the demand pool is warmed -- it is the one real
		// playback Acquire calls at play-start actually draw from under
		// the shipped disk-cache default. The readahead pool (below) and
		// the provider's own direct pool (grab-time Probe/manifest/STAT
		// audit) are deliberately out of this row's scope; see WORKLOG.
		WarmConns:     nntpCfg.WarmConns,
		PipelineDepth: nntpCfg.PipelineDepth,
	}, log)
	// HR1.4: the operator-configured demand pool size is the AIMD ceiling's
	// starting point and upper bound. Absent a real conn-cap rejection, the
	// ceiling never shrinks and the pool behaves exactly as before this row.
	demandPool.SetCeiling(accountgov.NewConnCeiling(demandPool.MaxConns()))

	var raPool *Pool
	if cfg.ReadaheadEnabled {
		raPool = NewPool(PoolConfig{
			MaxConns:      cfg.ReadaheadConnections,
			Host:          nntpCfg.Host,
			Port:          nntpCfg.Port,
			TLS:           nntpCfg.TLS,
			Username:      nntpCfg.Username,
			Password:      nntpCfg.Password,
			DialTimeout:   30 * time.Second,
			IdleTimeout:   5 * time.Minute,
			PipelineDepth: nntpCfg.PipelineDepth,
		}, log)
		raPool.SetCeiling(accountgov.NewConnCeiling(raPool.MaxConns()))
	}

	return &SegmentCache{
		cfg:        cfg,
		mode:       rangecache.Mode(cfg.Mode),
		rc:         rc,
		demandPool: demandPool,
		raPool:     raPool,
		log:        log,
		host:       nntpCfg.Host,
		skipCRC:    nntpCfg.SkipCRC,
		recorded:   make(map[segmentOffsetFile]map[int]struct{}),
		rarLayouts: make(map[string]rarLayoutCacheEntry),
	}
}

// useNegativeCache adopts the owning provider's shared NS-1.1
// missing-article cache for both of this cache's pools. Called by
// NNTPProvider.SetCache so the provider's direct pool, the demand pool,
// and the readahead pool all consult and populate one instance: they are
// the same account talking to the same host, so a 430 seen by any of them
// is true for all of them. A nil cache leaves every pool disabled, which
// is exactly pre-NS-1.1 behavior.
func (sc *SegmentCache) useNegativeCache(nc *negativeCache) {
	if sc == nil {
		return
	}
	if sc.demandPool != nil {
		sc.demandPool.SetNegativeCache(nc)
	}
	if sc.raPool != nil {
		sc.raPool.SetNegativeCache(nc)
	}
}

// useFinalRungAccounting gives both cache-owned pools the same provider name
// and NS-1.3 reporter as the provider's direct pool.
func (sc *SegmentCache) usePipelineState(state *pipelineState) {
	if sc == nil {
		return
	}
	sc.demandPool.setPipelineState(state)
	sc.raPool.setPipelineState(state)
}

func (sc *SegmentCache) useFinalRungAccounting(providerName string, reporter FinalRungReporter) {
	if sc == nil {
		return
	}
	if sc.demandPool != nil {
		sc.demandPool.setFinalRungAccounting(providerName, reporter)
	}
	if sc.raPool != nil {
		sc.raPool.setFinalRungAccounting(providerName, reporter)
	}
}

func (sc *SegmentCache) logPoolUse(class rangecache.FetchClass) {
	pool := "demand"
	once := &sc.demandSeen
	if class == rangecache.FetchReadahead {
		pool = "readahead"
		once = &sc.raSeen
	}
	once.Do(func() {
		sc.log.Info("nntp: cache pool selected", "host", sc.host, "class", pool, "pool", pool, "mode", sc.mode)
	})
}

// NormalizeCacheConnections applies provider-ceiling enforcement and default
// split heuristics for the NNTP cache pools. Explicit operator overrides are
// preserved unless they exceed the provider's declared connection ceiling.
func NormalizeCacheConnections(cfg CacheConfig, providerConnections int, overrides CacheConnectionOverrides, log *slog.Logger) CacheConfig {
	total := cfg.TotalConnections
	if total <= 0 {
		if providerConnections > 0 {
			total = providerConnections
		} else {
			total = defaultTotalConnections
		}
	} else if providerConnections > 0 && total < providerConnections {
		// TotalConnections was set (e.g. default 16) but provider supports more.
		// Silently raise total to the provider ceiling so HARRBOR_NNTP_CONNECTIONS
		// is the single knob operators need to set — they should not also need to
		// set HARRBOR_NNTP_TOTAL_CONNECTIONS separately.
		total = providerConnections
	}
	if providerConnections > 0 && total > providerConnections {
		if log != nil {
			log.Warn("nntp: cache: total connections exceed provider ceiling; clamping",
				"configured_total", total,
				"provider_connections", providerConnections)
		}
		total = providerConnections
	}

	readaheadEnabled := cfg.ReadaheadEnabled
	demand := cfg.DemandConnections
	readahead := cfg.ReadaheadConnections

	switch {
	case !overrides.DemandExplicit && !overrides.ReadaheadExplicit:
		if readaheadEnabled {
			demand = minInt(defaultDemandConnections, total)
			readahead = total - demand
		} else {
			demand = total
			readahead = 0
		}
	case overrides.DemandExplicit && !overrides.ReadaheadExplicit:
		if demand <= 0 {
			demand = defaultDemandConnections
		}
		if demand > total {
			if log != nil {
				log.Warn("nntp: cache: demand connections exceed provider ceiling; clamping",
					"configured_demand", demand,
					"effective_total", total)
			}
			demand = total
		}
		if readaheadEnabled {
			readahead = total - demand
		} else {
			readahead = 0
		}
	case !overrides.DemandExplicit && overrides.ReadaheadExplicit:
		if readaheadEnabled {
			if readahead <= 0 {
				readahead = defaultReadaheadConnections
			}
			if readahead > total {
				if log != nil {
					log.Warn("nntp: cache: readahead connections exceed provider ceiling; clamping",
						"configured_readahead", readahead,
						"effective_total", total)
				}
				readahead = total
			}
			demand = total - readahead
		} else {
			demand = total
			readahead = 0
		}
	default:
		if demand <= 0 {
			demand = defaultDemandConnections
		}
		if demand > total {
			if log != nil {
				log.Warn("nntp: cache: demand connections exceed provider ceiling; clamping",
					"configured_demand", demand,
					"effective_total", total)
			}
			demand = total
		}
		if readaheadEnabled {
			if readahead <= 0 {
				readahead = defaultReadaheadConnections
			}
			if demand+readahead > total {
				if log != nil {
					log.Warn("nntp: cache: demand+readahead exceed provider ceiling; clamping readahead",
						"configured_demand", demand,
						"configured_readahead", readahead,
						"effective_total", total)
				}
				readahead = total - demand
			}
		} else {
			readahead = 0
		}
	}

	if readahead < 0 {
		readahead = 0
	}
	if demand < 0 {
		demand = 0
	}

	cfg.TotalConnections = total
	cfg.DemandConnections = demand
	cfg.ReadaheadConnections = readahead
	return cfg
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// nntpChunkSource adapts one NZB file's segments to a rangecache.ChunkSource.
// ExactSizes()==false: seg.Bytes is NZB-declared and diverges from decoded
// yEnc payload length, so the shared write loop applies decoded-offset
type nntpChunkSource struct {
	sc               *SegmentCache
	segments         []NZBSegment
	itemID           string // for offset recording; empty disables recording
	contentKey       string // canonical NZB identity for shared offset maps
	fileIndex        int    // NZB file index within item
	captureFileTotal bool   // raw direct logical media only; never archive backing files
	// fileKey is a stable per-file identifier for session keying in rangecache.
	// Must be the same for all range requests against the same file so that
	// seek and reconnect reuse the existing readahead session and buffer instead
	// of creating a new cold session on every Jellyfin range probe.
	fileKey string
	// op selects the ladder priority/accounting label. Empty preserves the
	// playback-demand "cache" behavior.
	op string
	// start limits Chunks to a suffix while preserving original segment
	// indexes. ZIP prewarm uses it to begin at the selected entry's data.
	start int
	// sem is a per-file demand concurrency cap (buffered chan). nil = unlimited.
	sem chan struct{}
	// repair is NS-4.2's rung 4. Non-nil only for live playback of a file
	// with persisted IFSC proof material and an enabled repair budget;
	// prewarm, archive, and probe sources deliberately leave it nil, which
	// makes rung 4 a structural no-op on those paths.
	repair func(context.Context, int) ([]byte, error)
	// steadyWorkers (NS-5.5) is this source's adaptive-readahead resting
	// width, computed once by the caller (StreamWithCache et al.) via the
	// shared experience.Service.Workers recommendation. 0 means "no
	// recommendation yet" -- rangecache.SteadyWorkerRecommender then falls
	// back to the session's ceiling, a complete no-op exactly like a
	// deployment that never prewarms.
	steadyWorkers int
}

func (s *nntpChunkSource) Key() string      { return s.fileKey }
func (s *nntpChunkSource) ExactSizes() bool { return false }
func (s *nntpChunkSource) PayloadKey(ref rangecache.ChunkRef) string {
	return "nntp|" + ref.Key
}

// SteadyWorkers implements rangecache.SteadyWorkerRecommender (NS-5.5).
func (s *nntpChunkSource) SteadyWorkers() int { return s.steadyWorkers }
func (s *nntpChunkSource) ObserveChunk(ref rangecache.ChunkRef, data []byte) {
	if data != nil {
		s.sc.recordSegmentSize(s.itemID, s.contentKey, s.fileIndex, ref.Index, len(data))
	}
}

func (s *nntpChunkSource) Chunks() []rangecache.ChunkRef {
	start := s.start
	if start < 0 || start > len(s.segments) {
		start = len(s.segments)
	}
	refs := make([]rangecache.ChunkRef, 0, len(s.segments)-start)
	for i := start; i < len(s.segments); i++ {
		seg := s.segments[i]
		refs = append(refs, rangecache.ChunkRef{Index: i, Key: seg.MessageID, Size: seg.Bytes})
	}
	return refs
}

// Fetch retrieves and yEnc-decodes one segment. Demand-class fetches use the
// demand pool; readahead-class fetches use the read-ahead pool, returning
// (nil, nil) when no read-ahead pool exists (readahead unavailable) —
// unchanged from the former PrefetchSegment contract.
func (s *nntpChunkSource) Fetch(ctx context.Context, ref rangecache.ChunkRef, class rangecache.FetchClass) ([]byte, error) {
	pool := s.sc.demandPool
	poolClass := class
	if s.op == "prewarm" {
		poolClass = rangecache.FetchReadahead
	}
	if poolClass == rangecache.FetchReadahead {
		if s.sc.raPool == nil {
			return nil, nil
		}
		pool = s.sc.raPool
	}
	s.sc.logPoolUse(poolClass)
	// Per-file demand cap: acquired before pool Get, released after.
	// Skipped for readahead — raPool already bounds that concurrency.
	if poolClass == rangecache.FetchDemand && s.sem != nil {
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	var data []byte
	op := s.op
	if op == "" {
		op = "cache"
	}
	postedAt := int64(0)
	if ref.Index >= 0 && ref.Index < len(s.segments) {
		postedAt = s.segments[ref.Index].PostedAt
	} else if len(s.segments) == 1 {
		postedAt = s.segments[0].PostedAt
	}
	if err := fetchArticleWithRetryAtCRCTotal(ctx, pool, s.sc.log, op, ref.Index, ref.Key, postedAt, !s.sc.skipCRC, func(decoded []byte, fileTotal int64) error {
		data = decoded
		if s.captureFileTotal && fileTotal > 0 {
			s.sc.recordFileTotal(s.itemID, s.contentKey, s.fileIndex, fileTotal)
		}
		return nil
	}); err != nil {
		// LADDER RUNG 4 (NS-4.2). Rungs 1-3 (primary, failover, retry) have
		// all been walked by fetchArticleWithRetryAtCRCTotal above and come
		// back with no bytes. Bounded PAR2 reconstruction is attempted only
		// for genuine article-unavailability verdicts on demand-class
		// fetches; anything else -- cancellation, account conditions,
		// readahead, or a source without a repair session -- surfaces the
		// original error completely unchanged, as it did before this row.
		if repaired, ok := s.repairSegment(ctx, ref, poolClass, err); ok {
			return repaired, nil
		}
		return nil, fmt.Errorf("nntp: cache: body fetch: %w", err)
	}
	return data, nil
}

// repairSegment is the rung-4 attempt. It never mutates or masks the incoming
// provider error: a false return means the caller proceeds exactly as it would
// have without this row.
func (s *nntpChunkSource) repairSegment(
	ctx context.Context,
	ref rangecache.ChunkRef,
	class rangecache.FetchClass,
	fetchErr error,
) ([]byte, bool) {
	if s.repair == nil || class != rangecache.FetchDemand {
		return nil, false
	}
	if ref.Index < 0 || ref.Index >= len(s.segments) {
		return nil, false
	}
	if !eligibleForRepair(fetchErr) {
		return nil, false
	}
	data, err := s.repair(ctx, ref.Index)
	if err != nil || len(data) == 0 {
		return nil, false
	}
	return data, true
}

// CorrectedTotal returns the exact decoded logical-file total captured from
// yEnc's =ybegin size= field. It deliberately does not derive a total from a
// partially learned segment-offset map because that makes the HTTP byte
// domain shrink as playback progresses.
func (sc *SegmentCache) CorrectedTotal(ctx context.Context, itemID, contentKey string, fileIndex, segCount int, declaredSizes []int64) int64 {
	if sc.offsets == nil || itemID == "" {
		return 0
	}
	total, err := sc.offsets.GetFileTotal(ctx, itemID, contentKey, fileIndex)
	if err != nil || total <= 0 {
		return 0
	}
	sc.seedRecorded(itemID, fileIndex, map[int]struct{}{-1: {}})
	return total
}

// StreamWithCache is the cache-aware streaming entry point. Replaces
// NNTPProvider.Stream's demand path for all NNTP playback when a
// SegmentCache is attached. The caller sets Content-Type beforehand; the
// shared rangecache write loop owns range math, headers, and read-ahead.
func (sc *SegmentCache) StreamWithCache(
	ctx context.Context,
	itemID, contentKey string,
	fileIndex int,
	segments []NZBSegment,
	totalBytes int64,
	w http.ResponseWriter,
	rangeHeader string,
	repair *par2RepairSession,
) error {
	contentKey = SegmentOffsetKey(contentKey, segments)
	declaredSizes := make([]int64, len(segments))
	for i, seg := range segments {
		declaredSizes[i] = seg.Bytes
	}
	if exact := sc.CorrectedTotal(ctx, itemID, contentKey, fileIndex, len(segments), declaredSizes); exact > 0 {
		totalBytes = exact
	}
	var sem chan struct{}
	if sc.cfg.MaxDemandConnsPerFile > 0 {
		sem = make(chan struct{}, sc.cfg.MaxDemandConnsPerFile)
	}
	// fileKey is item/file scoped so readahead accounting never crosses items.
	// Payload identity is independently message-ID keyed by PayloadKey above.
	fileKey := "nntp|" + itemID
	if len(segments) > 0 {
		fileKey = fmt.Sprintf("nntp|%s|%d|%d", itemID, fileIndex, len(segments))
	}
	var repairFn func(context.Context, int) ([]byte, error)
	if repair != nil {
		repairFn = repair.RecoverSegment
	}
	src := &nntpChunkSource{sc: sc, segments: segments, fileKey: fileKey, sem: sem, itemID: itemID, contentKey: contentKey, fileIndex: fileIndex, captureFileTotal: true, repair: repairFn}
	// NS-5.5: adaptive steady readahead width. fallback is the existing
	// static ceiling (unchanged pre-NS-5.5 default); steadyWorkers returns 0
	// (no recommendation, ceiling stays the resting width) when expSvc is
	// nil or src has no prior measurement -- zero behavior change for a
	// deployment that never prewarms this item.
	src.steadyWorkers = sc.steadyWorkers(src, sc.cfg.ReadaheadConnections)

	// Corrected seek offsets: load decoded-size cumulative array from DB.
	// Populated passively during first play; each segment's actual decoded
	// byte length is recorded after yEnc decode (see Fetch above). On seek,
	// rangecache uses the corrected array to map byte offsets to the right
	// segment index, fixing seek accuracy on raw NNTP streams.
	var correctedOffsets []int64
	if sc.offsets != nil && itemID != "" && len(segments) > 0 {
		declSizes := make([]int64, len(segments))
		for i, seg := range segments {
			declSizes[i] = seg.Bytes
		}
		if off, recorded, err := sc.offsets.GetSegmentOffsets(ctx, itemID, contentKey, fileIndex, len(segments), declSizes); err != nil {
			sc.log.Debug("nntp: get segment offsets failed", "err", err)
		} else if len(off) > 0 {
			sc.seedRecorded(itemID, fileIndex, recorded)
			correctedOffsets = off
			sc.log.Debug("nntp: corrected offsets loaded", "item", itemID, "segments_recorded", len(recorded), "total_decoded", off[len(off)-1])
		}
	}
	// minBuffer=1: serve first byte immediately — default 4 causes Jellyfin probe timeout loop
	return sc.rc.StreamWithOffsets(ctx, src, sc.mode, totalBytes, w, rangeHeader, 1, correctedOffsets)
}

// PrewarmFileSource returns the raw-file source used by live playback, with
// Fetch routed to the reserved readahead pool. A nil source means prewarm is
// unavailable and must remain a non-fatal no-op.
func (p *NNTPProvider) PrewarmFileSource(itemID, contentKey string, fileIndex int, segments []NZBSegment) rangecache.ChunkSource {
	if p == nil || p.cache == nil || p.cache.raPool == nil || len(segments) == 0 {
		return nil
	}
	contentKey = SegmentOffsetKey(contentKey, segments)
	fileKey := fmt.Sprintf("nntp|%s|%d|%d", itemID, fileIndex, len(segments))
	return &nntpChunkSource{
		sc:               p.cache,
		segments:         segments,
		itemID:           itemID,
		contentKey:       contentKey,
		fileIndex:        fileIndex,
		fileKey:          fileKey,
		op:               "prewarm",
		captureFileTotal: true,
	}
}

// SeekFileSource reconstructs the same logical raw-video source selected by
// Stream from persisted NZB content. It is used only for bounded media-index
// analysis and seek-target prefetch.
func (p *NNTPProvider) SeekFileSource(itemID string, nzbData []byte, fileIndex int) rangecache.ChunkSource {
	parsed, err := ParseNZB(nzbData)
	if err != nil {
		return nil
	}
	files := filterNZBVideoFiles(parsed.Files)
	if fileIndex < 0 || fileIndex >= len(files) {
		return nil
	}
	return p.PrewarmFileSource(itemID, ContentKey(nzbData), fileIndex, files[fileIndex].Segments)
}

// PrewarmRARSource returns the same manifest-derived source and header
// transform used by StreamRARWithCache, routed to the readahead pool.
func (p *NNTPProvider) PrewarmRARSource(ctx context.Context, itemID, contentKey string, manifest []RARPart) rangecache.ChunkSource {
	if p == nil || p.cache == nil || p.cache.raPool == nil || len(manifest) == 0 || manifest[0].Encryption != nil {
		return nil
	}
	layout := p.cache.loadRARLayout(ctx, itemID, contentKey, manifest)
	return &rarSegSource{
		sc:         p.cache,
		itemID:     itemID,
		contentKey: contentKey,
		fileKey:    fmt.Sprintf("rar|%s|%d", itemID, len(layout.refs)),
		meta:       layout.meta,
		refs:       layout.refs,
		op:         "prewarm",
	}
}

// SeekRARSource reconstructs the same manifest-derived logical media source
// from the persisted representation used by StreamRARManifest.
func (p *NNTPProvider) SeekRARSource(ctx context.Context, itemID, contentKey, manifestJSON string) rangecache.ChunkSource {
	manifest, err := UnmarshalRARManifest(manifestJSON)
	if err != nil {
		return nil
	}
	return p.PrewarmRARSource(ctx, itemID, contentKey, manifest)
}

// PrewarmZIPSource starts at the segment containing the selected entry's
// compressed data while retaining the message-ID payload identity consumed
// by both stored and deflated ZIP playback.
func (p *NNTPProvider) PrewarmZIPSource(ctx context.Context, itemID, contentKey string, entry ZIPEntry) rangecache.ChunkSource {
	if p == nil || p.cache == nil || p.cache.raPool == nil || len(entry.Segments) == 0 {
		return nil
	}
	offsetKey := SegmentOffsetKey(contentKey, entry.Segments)
	declared := make([]int64, len(entry.Segments))
	for i, segment := range entry.Segments {
		declared[i] = segment.Bytes
	}
	starts := p.cache.segmentStarts(ctx, itemID, offsetKey, entry.NZBFileIdx, declared)
	start := 0
	for start+1 < len(starts) && starts[start+1] <= entry.DataOff {
		start++
	}
	return &nntpChunkSource{
		sc:         p.cache,
		segments:   entry.Segments,
		itemID:     itemID,
		contentKey: offsetKey,
		fileIndex:  entry.NZBFileIdx,
		fileKey:    fmt.Sprintf("zip|%s|%d", itemID, entry.EntryIdx),
		op:         "prewarm",
		start:      start,
	}
}

type zipEntryChunk struct {
	segmentIdx int
	segment    NZBSegment
	trimStart  int64
	trimEnd    int64
}

type zipEntrySource struct {
	base   *nntpChunkSource
	key    string
	chunks []zipEntryChunk
	refs   []rangecache.ChunkRef
}

func (s *zipEntrySource) Key() string      { return s.key }
func (s *zipEntrySource) ExactSizes() bool { return false }
func (s *zipEntrySource) PayloadKey(ref rangecache.ChunkRef) string {
	return "nntp|" + ref.Key
}
func (s *zipEntrySource) Chunks() []rangecache.ChunkRef { return s.refs }
func (s *zipEntrySource) Fetch(ctx context.Context, ref rangecache.ChunkRef, class rangecache.FetchClass) ([]byte, error) {
	if ref.Index < 0 || ref.Index >= len(s.chunks) {
		return nil, fmt.Errorf("zip: seek chunk out of range")
	}
	chunk := s.chunks[ref.Index]
	return s.base.Fetch(ctx, rangecache.ChunkRef{
		Index: chunk.segmentIdx,
		Key:   chunk.segment.MessageID,
		Size:  chunk.segment.Bytes,
	}, class)
}
func (s *zipEntrySource) TransformChunk(ref rangecache.ChunkRef, data []byte) []byte {
	if ref.Index < 0 || ref.Index >= len(s.chunks) {
		return nil
	}
	chunk := s.chunks[ref.Index]
	start := min(chunk.trimStart, int64(len(data)))
	end := int64(len(data)) - min(chunk.trimEnd, int64(len(data))-start)
	return data[start:end]
}

// SeekZIPSource exposes a stored ZIP entry as its logical media bytes. Deflate
// entries retain their shipped discard-seek path and deliberately return nil:
// compressed bytes cannot provide a precise random-access container index.
func (p *NNTPProvider) SeekZIPSource(ctx context.Context, itemID, contentKey, manifestJSON string, entryIdx int) rangecache.ChunkSource {
	if p == nil || p.cache == nil || p.cache.raPool == nil {
		return nil
	}
	manifest, err := UnmarshalZIPManifest(manifestJSON)
	if err != nil || entryIdx < 0 || entryIdx >= len(manifest) {
		return nil
	}
	entry := manifest[entryIdx]
	if entry.Method != 0 || len(entry.Segments) == 0 || entry.CompressedSize <= 0 {
		return nil
	}
	offsetKey := SegmentOffsetKey(contentKey, entry.Segments)
	declared := make([]int64, len(entry.Segments))
	for i, segment := range entry.Segments {
		declared[i] = segment.Bytes
	}
	starts := p.cache.segmentStarts(ctx, itemID, offsetKey, entry.NZBFileIdx, declared)
	entryEnd := entry.DataOff + entry.CompressedSize
	if entryEnd < entry.DataOff {
		return nil
	}
	source := &zipEntrySource{
		key: fmt.Sprintf("zip|%s|%d", itemID, entry.EntryIdx),
		base: &nntpChunkSource{
			sc:         p.cache,
			segments:   entry.Segments,
			itemID:     itemID,
			contentKey: offsetKey,
			fileIndex:  entry.NZBFileIdx,
			op:         "prewarm",
		},
	}
	for i, segment := range entry.Segments {
		segmentStart, segmentEnd := starts[i], starts[i+1]
		if segmentEnd <= entry.DataOff || segmentStart >= entryEnd {
			continue
		}
		meta := zipEntryChunk{segmentIdx: i, segment: segment}
		if entry.DataOff > segmentStart {
			meta.trimStart = entry.DataOff - segmentStart
		}
		if segmentEnd > entryEnd {
			meta.trimEnd = segmentEnd - entryEnd
		}
		size := segmentEnd - segmentStart - meta.trimStart - meta.trimEnd
		if size <= 0 {
			continue
		}
		source.chunks = append(source.chunks, meta)
		source.refs = append(source.refs, rangecache.ChunkRef{
			Index: len(source.refs),
			Key:   segment.MessageID,
			Size:  size,
		})
	}
	if len(source.refs) == 0 {
		return nil
	}
	return source
}

// GetSegment returns the decoded bytes for one NZB segment under the
// configured mode (disk-cached in disk/full modes). Used by the RAR
// streaming write loop (rarstream.go) for per-segment demand fetches.
func (sc *SegmentCache) GetSegment(ctx context.Context, seg NZBSegment) ([]byte, error) {
	src := &nntpChunkSource{sc: sc, segments: []NZBSegment{seg}}
	ref := rangecache.ChunkRef{Index: seg.Number, Key: seg.MessageID, Size: seg.Bytes}
	return sc.rc.GetChunk(ctx, src, sc.mode, ref)
}

// GetSegmentWithMeta is like GetSegment but records the actual decoded size
// into segment_offsets for RAR items, enabling corrected seek offset mapping
// on subsequent plays. itemID and fileIndex identify the NZB file within the
// item; segIndex is the 0-based position of seg within that file's segment list.
func (sc *SegmentCache) GetSegmentWithMeta(ctx context.Context, seg NZBSegment, itemID, contentKey string, fileIndex, segIndex int) ([]byte, error) {
	src := &nntpChunkSource{sc: sc, segments: []NZBSegment{seg}, fileKey: "nntp|" + itemID}
	ref := rangecache.ChunkRef{Index: seg.Number, Key: seg.MessageID, Size: seg.Bytes}
	data, err := sc.rc.GetChunk(ctx, src, sc.mode, ref)
	if err != nil {
		return nil, err
	}
	if data != nil {
		sc.recordSegmentSize(itemID, contentKey, fileIndex, segIndex, len(data))
	}
	return data, nil
}

// rarSegSource adapts a RAR manifest to rangecache.ChunkSource at segment
// granularity — one chunk per NNTP segment — so individual Fetch calls return
// in ~200ms (one segment decode) rather than minutes (one full RAR part).
// correctedOffsets (built from manifest in StreamRARWithCache) accounts for
// the RAR header bytes trimmed from the first segment of each part, keeping
// seek byte arithmetic in the same coordinate space as totalVideoBytes.
type rarSegSource struct {
	sc         *SegmentCache
	itemID     string
	contentKey string
	fileKey    string
	sem        chan struct{}
	// flat list of (partIdx, segIdx, trimHeader) for each chunk index
	meta []rarSegMeta
	refs []rangecache.ChunkRef
	op   string
}

type rarSegMeta struct {
	part       *RARPart
	partIdx    int
	segIdx     int
	trimHeader int64 // bytes to strip from decoded data (only on seg 0 of each part)
}

func (s *rarSegSource) Key() string      { return s.fileKey }
func (s *rarSegSource) ExactSizes() bool { return false }
func (s *rarSegSource) PayloadKey(ref rangecache.ChunkRef) string {
	return "nntp|" + ref.Key
}
func (s *rarSegSource) TransformChunk(ref rangecache.ChunkRef, data []byte) []byte {
	trim := s.meta[ref.Index].trimHeader
	if trim <= 0 {
		return data
	}
	if int64(len(data)) <= trim {
		return nil
	}
	return data[trim:]
}
func (s *rarSegSource) ObserveChunk(ref rangecache.ChunkRef, data []byte) {
	m := s.meta[ref.Index]
	if data != nil {
		offsetKey := SegmentOffsetKey(s.contentKey, m.part.Segments)
		s.sc.recordSegmentSize(s.itemID, offsetKey, m.partIdx, m.segIdx, len(data))
	}
}

func (s *rarSegSource) Chunks() []rangecache.ChunkRef { return s.refs }

func (s *rarSegSource) Fetch(ctx context.Context, ref rangecache.ChunkRef, class rangecache.FetchClass) ([]byte, error) {
	pool := s.sc.demandPool
	poolClass := class
	if s.op == "prewarm" {
		poolClass = rangecache.FetchReadahead
	}
	if poolClass == rangecache.FetchReadahead {
		if s.sc.raPool == nil {
			return nil, nil
		}
		pool = s.sc.raPool
	}
	s.sc.logPoolUse(poolClass)
	if poolClass == rangecache.FetchDemand && s.sem != nil {
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	m := s.meta[ref.Index]
	seg := m.part.Segments[m.segIdx]
	var data []byte
	op := s.op
	if op == "" {
		op = "rar seg"
	}
	if err := fetchArticleWithRetryAtCRC(ctx, pool, s.sc.log, op, seg.Number, seg.MessageID, seg.PostedAt, !s.sc.skipCRC, func(decoded []byte) error {
		data = decoded
		return nil
	}); err != nil {
		return nil, fmt.Errorf("nntp: rar seg: %w", err)
	}

	return data, nil
}

func (sc *SegmentCache) loadRARLayout(ctx context.Context, itemID, contentKey string, manifest []RARPart) rarLayoutCacheEntry {
	if contentKey == "" {
		return sc.buildRARLayout(ctx, itemID, contentKey, manifest)
	}

	now := time.Now()
	sc.rarMu.Lock()
	defer sc.rarMu.Unlock()
	if sc.rarLayouts == nil {
		sc.rarLayouts = make(map[string]rarLayoutCacheEntry)
	}
	for key, entry := range sc.rarLayouts {
		if !entry.expires.After(now) {
			delete(sc.rarLayouts, key)
		}
	}
	if entry, ok := sc.rarLayouts[contentKey]; ok {
		return entry
	}
	entry := sc.buildRARLayout(ctx, itemID, contentKey, manifest)
	entry.expires = now.Add(rarLayoutTTL)
	sc.rarLayouts[contentKey] = entry
	return entry
}

func (sc *SegmentCache) buildRARLayout(ctx context.Context, itemID, contentKey string, manifest []RARPart) rarLayoutCacheEntry {
	segmentCount := 0
	for i := range manifest {
		segmentCount += len(manifest[i].Segments)
	}
	entry := rarLayoutCacheEntry{
		meta:             make([]rarSegMeta, 0, segmentCount),
		refs:             make([]rangecache.ChunkRef, 0, segmentCount),
		correctedOffsets: make([]int64, 0, segmentCount+1),
	}

	var cumulative int64
	for partIdx := range manifest {
		part := &manifest[partIdx]
		var decodedSizes []int64
		if sc.offsets != nil && itemID != "" {
			declaredSizes := make([]int64, len(part.Segments))
			for i, segment := range part.Segments {
				declaredSizes[i] = segment.Bytes
				if i == 0 {
					declaredSizes[i] -= part.HeaderBytes
				}
				if declaredSizes[i] < 0 {
					declaredSizes[i] = 0
				}
			}
			offsetKey := SegmentOffsetKey(contentKey, part.Segments)
			offsets, recorded, err := sc.offsets.GetSegmentOffsets(ctx, itemID, offsetKey, partIdx, len(part.Segments), declaredSizes)
			if err != nil {
				if sc.log != nil {
					sc.log.Debug("nntp: get RAR segment offsets failed", "item", itemID, "part", partIdx, "err", err)
				}
			} else {
				sc.seedRecorded(itemID, partIdx, recorded)
				if len(offsets) == len(part.Segments)+1 {
					decodedSizes = make([]int64, len(part.Segments))
					for i := range part.Segments {
						decodedSizes[i] = offsets[i+1] - offsets[i]
					}
				}
			}
		}

		for segIdx := range part.Segments {
			segment := part.Segments[segIdx]
			trim := int64(0)
			if segIdx == 0 {
				trim = part.HeaderBytes
			}
			declaredSize := segment.Bytes - trim
			if declaredSize < 0 {
				declaredSize = 0
			}
			size := declaredSize
			if segIdx < len(decodedSizes) && decodedSizes[segIdx] > 0 {
				size = decodedSizes[segIdx]
			}

			entry.correctedOffsets = append(entry.correctedOffsets, cumulative)
			cumulative += size
			entry.meta = append(entry.meta, rarSegMeta{
				part:       part,
				partIdx:    partIdx,
				segIdx:     segIdx,
				trimHeader: trim,
			})
			entry.refs = append(entry.refs, rangecache.ChunkRef{
				Index: len(entry.refs),
				Key:   segment.MessageID,
				Size:  declaredSize,
			})
		}
	}
	entry.correctedOffsets = append(entry.correctedOffsets, cumulative)
	entry.fileKey = fmt.Sprintf("rar|%s|%d", itemID, len(entry.refs))
	return entry
}

// StreamRARWithCache streams a RAR-split NZB through rangecache at segment
// granularity. The expensive flat layout and per-part offset reads are reused
// briefly across Jellyfin's burst of range requests, then refreshed so newly
// recorded decoded sizes become visible.
func (sc *SegmentCache) StreamRARWithCache(
	ctx context.Context,
	itemID, contentKey string,
	manifest []RARPart,
	totalVideoBytes int64,
	w http.ResponseWriter,
	rangeHeader string,
) error {
	if len(manifest) == 0 {
		return fmt.Errorf("rar: StreamRARWithCache: empty manifest")
	}

	layout := sc.loadRARLayout(ctx, itemID, contentKey, manifest)
	layout.fileKey = fmt.Sprintf("rar|%s|%d", itemID, len(layout.refs))

	var sem chan struct{}
	if sc.cfg.MaxDemandConnsPerFile > 0 {
		sem = make(chan struct{}, sc.cfg.MaxDemandConnsPerFile)
	}
	src := &rarSegSource{sc: sc, itemID: itemID, contentKey: contentKey, fileKey: layout.fileKey, sem: sem, meta: layout.meta, refs: layout.refs}

	sc.log.Info("nntp: StreamRARWithCache", "item_id", itemID, "chunks", len(layout.meta), "total_bytes", totalVideoBytes, "range", rangeHeader)

	w.Header().Set("Content-Type", "video/x-matroska")
	w.Header().Set("Accept-Ranges", "bytes")

	return sc.rc.StreamWithOffsets(ctx, src, sc.mode, totalVideoBytes, w, rangeHeader, 1, layout.correctedOffsets)
}

// Close shuts down the NNTP-owned connection pools. The shared rangecache
// instance is owned and closed by the process wiring (cmd/darkharrbor).
func (sc *SegmentCache) Close() {
	if sc.demandPool != nil {
		sc.demandPool.Close()
	}
	if sc.raPool != nil {
		sc.raPool.Close()
	}
}

// ConnectionCeilings returns the currently effective demand and readahead
// connection ceilings (HR1.4 AIMD-adjusted values where a rejection has
// occurred; otherwise the static configured pool size). readahead is 0 when
// no read-ahead pool is configured.
func (sc *SegmentCache) ConnectionCeilings() (demand, readahead int) {
	if sc.demandPool != nil {
		demand = sc.demandPool.EffectiveMaxConns()
	}
	if sc.raPool != nil {
		readahead = sc.raPool.EffectiveMaxConns()
	}
	return demand, readahead
}

// WarmTick runs one bounded NS-5.1 warm-pool maintenance pass against the
// demand pool only (see the row's own scope note at demandPool
// construction in NewSegmentCache). Nil-safe; a no-op when sc is nil or
// warm-keeping is disabled.
func (sc *SegmentCache) WarmTick(log *slog.Logger) {
	if sc == nil {
		return
	}
	sc.demandPool.WarmTick(log)
}

// WarmSnapshot returns the demand pool's current NS-5.1 warm-pool state
// (see Pool.WarmSnapshot). Nil-safe.
func (sc *SegmentCache) WarmSnapshot() (open, target int, cold, warm int64) {
	if sc == nil {
		return 0, 0, 0, 0
	}
	return sc.demandPool.WarmSnapshot()
}
