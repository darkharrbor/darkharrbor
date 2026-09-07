// Package rangecache implements an rclone-VFS-style configurable byte-range
// chunk cache shared by the NNTP /dav path and the torrent/CDN /stream
//
// Framing (binding): this is an rclone-VFS-STYLE configurable cache — it is
// NOT the rclone VFS layer itself and NOT a debrid-FUSE filesystem (the
// DFS-style mount used by some debrid front-ends). It generalizes the proven
// semantics
// of the former internal/nntp SegmentCache over a neutral ChunkSource:
// modes none / readahead / disk (LRU+TTL) / full, demand vs readahead
// concurrency split (owned by the source), readahead sessions that outlive
// HTTP connections (ffmpeg probe-then-seek), tail-evict + lookback, size/TTL
// eviction, atomic disk writes, sha-keyed disk entries.
//
//   - Declared chunk sizes position chunks in the file: chunk prefiltering
//     and the front-skip of the first overlapping chunk use DECLARED offsets
//     (Range coordinates are declared-space — HEAD/PROPFIND advertise the
//     declared total, and Content-Range reports it).
//   - Decoded (actual) byte counts govern emission and the stop condition:
//     declared sizes are never used to slice or pad fetched data, so a chunk
//     accounting intent, carried into this loop).
//   - Sources with ExactSizes()==false (NZB/NNTP) never get a Content-Length
//     preserved verbatim. Sources with ExactSizes()==true (HTTP/CDN windows)
//     get exact Content-Length.
package rangecache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/outcome"
	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
)

// Mode defines the caching behavior for chunk retrieval. Values and
// semantics are carried unchanged from the former nntp.CacheMode.
type Mode string

const (
	// ModeNone disables all caching. Every chunk is fetched live (demand).
	ModeNone Mode = "none"
	// ModeReadahead enables read-ahead prefetching but no disk caching.
	ModeReadahead Mode = "readahead"
	// ModeDisk enables disk caching of chunks with LRU/TTL eviction.
	ModeDisk Mode = "disk"
	// Config.ReadaheadEnabled is ignored in this mode — always true.
	ModeFull Mode = "full"
)

// ValidMode reports whether m is one of the four supported modes.
func ValidMode(m Mode) bool {
	switch m {
	case ModeNone, ModeReadahead, ModeDisk, ModeFull:
		return true
	}
	return false
}

// FetchClass tells a ChunkSource whether a fetch is a demand fetch (the
// write loop needs this chunk now) or a readahead prefetch. Sources with a
type FetchClass int

const (
	FetchDemand FetchClass = iota
	FetchReadahead
)

// ChunkRef identifies one chunk of a source.
type ChunkRef struct {
	// Index is the ordinal position of the chunk within the source.
	Index int
	// Key uniquely identifies the chunk within the source namespace
	// (NZB segment message-id, or CDN window ordinal).
	Key string
	// Size is the DECLARED size of the chunk in bytes. Exact if and only
	// if the source reports ExactSizes().
	Size int64
}

// ChunkSource enumerates chunks and fetches one. Fetch MUST return either
// the complete decoded chunk or an error — never partial data. A readahead
// fetch may return (nil, nil) meaning "readahead unavailable" (e.g. no
// readahead pool); the caller treats it as a silent no-op.
type ChunkSource interface {
	// Key returns a stable cache identity for this source. Disk entries and
	// readahead sessions are keyed from Key()+ChunkRef.Key, so sources whose
	// chunk keys are only ordinals must include file identity here.
	Key() string
	Chunks() []ChunkRef
	Fetch(ctx context.Context, ref ChunkRef, class FetchClass) ([]byte, error)
	// ExactSizes reports whether declared chunk sizes equal actual fetched
	// sizes. false for NNTP (yEnc decoded length diverges from seg.Bytes).
	ExactSizes() bool
}

type playbackContextKey struct{}

type playbackContext struct {
	lane    int
	started time.Time
}

// WithPlaybackTelemetry marks one real playback request for fixed-lane,
// press-play-to-first-byte accounting. Invalid lanes or zero timestamps
// abstain, so arbitrary caller data never becomes telemetry state.
func WithPlaybackTelemetry(ctx context.Context, lane string, started time.Time) context.Context {
	index := playbackLaneIndex(lane)
	if ctx == nil || index < 0 || started.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, playbackContextKey{}, playbackContext{lane: index, started: started})
}

type playbackStat struct {
	count atomic.Int64
	total atomic.Int64
	max   atomic.Int64
}

func playbackLaneIndex(lane string) int {
	switch lane {
	case "torrent":
		return 0
	case "nzb", "nntp":
		return 1
	case "http":
		return 2
	default:
		return -1
	}
}

// PayloadKeyer lets a source give payloads an identity independent of its
// file/session namespace. NNTP uses this to deduplicate globally unique
// message-IDs across overlapping NZBs; sources that do not implement it keep
// the legacy Key()+ChunkRef.Key identity.
type PayloadKeyer interface {
	PayloadKey(ChunkRef) string
}

// ChunkTransformer applies a source-specific view after the globally keyed
// payload is read. The cached bytes remain the unmodified upstream payload.
type ChunkTransformer interface {
	TransformChunk(ChunkRef, []byte) []byte
}

// ChunkObserver receives the bytes visible to the current source on both hits
// and misses, allowing item-scoped accounting to map onto shared payloads.
type ChunkObserver interface {
	ObserveChunk(ChunkRef, []byte)
}

// ChunkValidator may reject source-visible bytes before they are cached or
// returned. Proof/continuity consumers use this to stop conflicting bytes on
// both cache hits and misses; sources without a validator are unchanged.
type ChunkValidator interface {
	ValidateChunk(context.Context, ChunkRef, []byte) error
}

// WorkerLimiter is an optional interface a ChunkSource may implement to cap
// the number of parallel readahead goroutines used for that source. When a
// source implements this, the cache uses min(cfg.ReadaheadWorkers, MaxWorkers())
// so CDN sources (where concurrent fetches trigger rate limits) can constrain
// parallelism independently of the NNTP readahead pool size.
type WorkerLimiter interface {
	MaxWorkers() int
}

// SteadyWorkerRecommender (NS-5.5) is an optional interface a ChunkSource may
// implement to report its own adaptively-recommended RESTING readahead
// width -- distinct from WorkerLimiter's hard ceiling. A source that omits
// this keeps its ceiling as the always-active worker count (today's exact
// behavior for every lane that predates this row): activeLimit starts and
// stays at the ceiling, so a seek-triggered widen (see readaheadSession)
// never has headroom to widen into and is a complete no-op for that source.
// NNTP is this row's first consumer: SteadyWorkers reflects
// experience.Service.Workers' bitrate/throughput-based recommendation,
// while the ceiling stays the existing NNTP readahead-pool size, giving a
// seek real headroom to widen into up to that pool size.
type SteadyWorkerRecommender interface {
	SteadyWorkers() int
}

// Config holds shared rangecache knobs. Connection counts are NOT here —
// Numeric defaults match the former nntp.CacheConfig defaults exactly.
type Config struct {
	ReadaheadEnabled     bool
	ReadaheadMaxSegments int
	// ReadaheadWorkers: parallel prefetch goroutines. Default 1; set to raPool size for max throughput.
	ReadaheadWorkers  int
	MinBufferSegments int
	TailEvict         bool
	TailLookback      int
	DiskCacheSizeMB   int64
	DiskCacheTTLMin   int
	DiskCachePath     string
	FullEvictMode     string
	FullEvictTTLMin   int
	// PinnedBudgetMB (TS-1.4) bounds the separate hot-head pinned tier: MB
	// of GetChunkPinned entries exempt from the LRU/TTL sweep below and
	// instead bounded by their own eviction. <=0 disables pinning — a
	// GetChunkPinned call still fetches and caches, just through the
	// ordinary disk budget/eviction above like any other chunk.
	PinnedBudgetMB int64
	// SeekWidenDuration (NS-5.5) bounds how long a session's readahead width
	// stays widened to its ceiling after a seek flush before decaying back
	// to the source's SteadyWorkerRecommender value (or the ceiling, for a
	// source that doesn't implement it -- a permanent no-op widen in that
	// case). <=0 disables widening: activeLimit stays pinned at the ceiling
	// at all times, identical to pre-NS-5.5 behavior.
	SeekWidenDuration time.Duration
}

// readaheadSession holds a background prefetch goroutine and shared buffer
// for one stream. It outlives individual HTTP connections so that ffmpeg's
// probe-then-seek pattern doesn't discard prefetched chunks. Carried
// unchanged from the former SegmentCache.
type readaheadSession struct {
	cancel   context.CancelFunc
	buf      map[int][]byte
	claimed  map[int]bool // chunks currently being fetched; prevents duplicate parallel fetches
	mu       sync.Mutex
	writePos int64      // atomic; set by each Stream write loop
	headCond *sync.Cond // PERF-7: signaled on head advance; readahead goroutines wait instead of polling
	lastUsed time.Time
	refs     []ChunkRef
	work     []int

	// NS-5.5 adaptive-readahead-width fields. ceiling is the fixed number of
	// goroutines spawned for this session (unchanged sizing rule: cfg.ReadaheadWorkers
	// capped by WorkerLimiter.MaxWorkers() when implemented). steady is the
	// resting worker count activeLimit decays back to (SteadyWorkerRecommender's
	// value when implemented, else == ceiling -- a permanent no-op widen for
	// every pre-NS-5.5 source). activeLimit (atomic) gates which goroutine
	// indices (0..ceiling-1) may currently claim a readahead target; a seek
	// flush raises it to ceiling for SeekWidenDuration, then a later session
	// touch decays it back to steady once widenUntil has passed. All reads/
	// writes use atomic ops so readaheadGoroutine never needs sess.mu to check
	// its own gate.
	ceiling     int32
	steady      int32
	activeLimit atomic.Int32
	widenUntil  atomic.Int64 // unix nano deadline; 0 = not currently widened
}

// diskEntry tracks one cached chunk on disk. mode records the stream mode
// that wrote the entry so the TTL sweeper applies the matching policy
// (disk-mode TTL vs full-mode evict policy) under the single shared budget.
type diskEntry struct {
	key        string
	path       string
	sizeB      int64
	lastUsed   time.Time
	mode       Mode
	contentSHA string
	sourceKey  string
	// pinned marks a TS-1.4 hot-head tier entry: exempt from evictLRU and
	// sweepOnce, bounded instead by pinnedEvictLocked/Config.PinnedBudgetMB.
	pinned bool
}

// Cache is the shared byte-range cache. One instance serves both the NNTP
// path and the /stream byte-proxy path; per-stream Mode is passed to Stream.
type Cache struct {
	cfg         Config
	log         *slog.Logger
	mu          sync.Mutex
	diskEntries map[string]diskEntry
	diskSizeB   int64
	// pinnedSizeB (TS-1.4) tracks bytes held in the separate pinned tier —
	// a subset of diskEntries/diskSizeB, guarded by the same c.mu — so
	// pinnedEvictLocked can bound it independently of the main disk budget.
	pinnedSizeB   int64
	sweeperCtx    context.Context
	sweeperCancel context.CancelFunc
	sweeperDone   chan struct{}
	sessionMu     sync.Mutex
	sessions      map[string]*readaheadSession
	now           func() time.Time // test seam; time.Now in production
	minBufTimeout time.Duration    // test seam; 30s in production
	diskHits      atomic.Int64
	diskMisses    atomic.Int64
	crossHits     atomic.Int64
	// Fixed lane x cold/warm press-play-to-first-byte counters. No labels,
	// maps, allocations, or background work occur on the hot path.
	playback [3][2]playbackStat
	coverage *playbackcoverage.Tracker

	// HR1.3 global in-flight payload coalescing. inflight maps payloadKey to
	// the single upstream fetch every concurrent caller for that payload
	// shares. fetchCtx is the cache-owned parent for leader fetches, so no
	// caller's disconnect can cancel work other callers are waiting on.
	inflightMu      sync.Mutex
	inflight        map[string]*inflightFetch
	fetchCtx        context.Context
	fetchCancel     context.CancelFunc
	coalescedJoins  atomic.Int64
	coalescedAborts atomic.Int64
	demandJoins     atomic.Int64

	// limiter is the shared SF-01 per-(source, class, key) WARN rate limiter,
	// used by readaheadGoroutine to fold repeated consecutive prefetch
	// failures into one periodic summary instead of the former fixed
	// fail#1-and-every-10th log cadence. Backoff scheduling itself
	// (readaheadPrefetchBackoff) is unchanged; only the logging cadence
	// moved to the shared mechanism.
	limiter *outcome.Limiter
}

// New creates the shared Cache. Numeric/string defaults are applied here
// (carried verbatim from NewSegmentCache); booleans are caller-set.
func New(cfg Config, log *slog.Logger, coverage ...*playbackcoverage.Tracker) *Cache {
	if cfg.ReadaheadMaxSegments == 0 {
		cfg.ReadaheadMaxSegments = 16
	}
	if cfg.MinBufferSegments == 0 {
		cfg.MinBufferSegments = 4
	}
	if cfg.TailLookback == 0 {
		cfg.TailLookback = 4
	}
	if cfg.DiskCacheSizeMB == 0 {
		cfg.DiskCacheSizeMB = 2048
	}
	if cfg.DiskCacheTTLMin == 0 {
		cfg.DiskCacheTTLMin = 60
	}
	if cfg.DiskCachePath == "" {
		cfg.DiskCachePath = ".cache"
	}
	if cfg.FullEvictMode == "" {
		cfg.FullEvictMode = "ttl"
	}
	if cfg.FullEvictTTLMin == 0 {
		cfg.FullEvictTTLMin = 120
	}

	if err := os.MkdirAll(cfg.DiskCachePath, 0o750); err != nil {
		log.Warn("rangecache: failed to create disk cache dir", "path", cfg.DiskCachePath, "err", err)
	}

	sweeperCtx, sweeperCancel := context.WithCancel(context.Background())
	c := &Cache{
		cfg:           cfg,
		log:           log,
		diskEntries:   make(map[string]diskEntry),
		sweeperCtx:    sweeperCtx,
		sweeperCancel: sweeperCancel,
		sweeperDone:   make(chan struct{}),
		sessions:      make(map[string]*readaheadSession),
		now:           time.Now,
		minBufTimeout: 30 * time.Second,
		limiter:       outcome.NewLimiter(),
	}
	if len(coverage) > 0 {
		c.coverage = coverage[0]
	}
	c.initInflight()

	// Restore disk cache index from existing files on disk. Without this,
	// every container restart treats the cache as cold even though the files
	// are still on disk — causing full NNTP re-fetches for every segment.
	// We register path and size; content-SHA is verified on first read.
	if cfg.DiskCachePath != "" {
		entries, _ := os.ReadDir(cfg.DiskCachePath)
		restored := 0
		for _, de := range entries {
			if de.IsDir() {
				continue
			}
			key := de.Name()
			if len(key) != 64 { // sha256 hex is 64 chars
				continue
			}
			info, err := de.Info()
			if err != nil {
				continue
			}
			// TS-1.4: a sibling ".pin" marker file (see pinnedMarkerPath)
			// restores the pinned designation across a restart, so a pinned
			// hot-head entry stays exempt from the ordinary LRU/TTL sweep
			// after a container recreate rather than silently reverting to
			// an ordinary evictable entry (the bytes would otherwise
			// survive but "persists outside LRU eviction" would not).
			pinned := false
			if _, statErr := os.Stat(pinnedMarkerPath(filepath.Join(cfg.DiskCachePath, key))); statErr == nil {
				pinned = true
			}
			c.diskEntries[key] = diskEntry{
				key:      key,
				path:     filepath.Join(cfg.DiskCachePath, key),
				sizeB:    info.Size(),
				lastUsed: info.ModTime(),
				// Restored entries get the longer TTL mode so they survive the
				// normal sweep cycle. disk-mode entries would be evicted after
				// DiskCacheTTLMin (60min default) which would flush the cache on
				// every restart. Use ModeFull so FullEvictTTLMin (7 days) applies.
				mode: ModeFull,
				// contentSHA left empty: verified on first read, re-fetched if corrupt
				pinned: pinned,
			}
			c.diskSizeB += info.Size()
			if pinned {
				c.pinnedSizeB += info.Size()
			}
			restored++
		}
		if restored > 0 {
			log.Info("rangecache: restored disk cache index", "entries", restored, "size_mb", c.diskSizeB/1024/1024, "pinned_bytes", c.pinnedSizeB)
		}
	}

	go c.startTTLSweeper(sweeperCtx)
	return c
}

// initInflight prepares the HR1.3 coalescing state. Split out so every
// construction path — including tests that build a Cache directly — gets a
// usable map and parent context.
func (c *Cache) initInflight() {
	if c.inflight == nil {
		c.inflight = make(map[string]*inflightFetch)
	}
	if c.fetchCtx == nil {
		c.fetchCtx, c.fetchCancel = context.WithCancel(context.Background())
	}
}

// Close shuts down the cache: cancels TTL sweeper and all sessions.
// Source-owned resources (NNTP pools) are closed by their owners.
func (c *Cache) Close() {
	if c.sweeperCancel != nil {
		c.sweeperCancel() // also signals all session reapers via sweeperCtx
	}
	// Cancel any leader fetch still in flight; each waiter observes its own
	// context or the shared done channel and returns normally.
	if c.fetchCancel != nil {
		c.fetchCancel()
	}
	c.sessionMu.Lock()
	for _, sess := range c.sessions {
		sess.cancel()
	}
	c.sessions = make(map[string]*readaheadSession)
	c.sessionMu.Unlock()
}

// raActiveFor reports whether read-ahead runs for a stream in mode m.
func (c *Cache) raActiveFor(m Mode) bool {
	if m == ModeFull {
		return true
	}
	return c.cfg.ReadaheadEnabled && (m == ModeReadahead || m == ModeDisk)
}

// Stream is the cache-aware streaming entry point shared by both paths.
// The caller sets Content-Type before calling; Stream owns Accept-Ranges,
// Content-Range, Content-Length policy (by ExactSizes), and the status line.
// StreamWithOffsets is like Stream but accepts a correctedOffsets array
// (len = nChunks+1, correctedOffsets[i] = decoded byte start of chunk i).
// When provided and length matches, it is used instead of declared chunk sizes
// for range-to-chunk mapping, fixing seek accuracy on raw NNTP streams where
// yEnc encoding overhead causes declared sizes to diverge from decoded sizes.
func (c *Cache) StreamWithOffsets(
	ctx context.Context,
	src ChunkSource,
	mode Mode,
	totalBytes int64,
	w http.ResponseWriter,
	rangeHeader string,
	minBuffer int,
	correctedOffsets []int64,
) error {
	return c.stream(ctx, src, mode, totalBytes, w, rangeHeader, minBuffer, correctedOffsets)
}

func (c *Cache) Stream(
	ctx context.Context,
	src ChunkSource,
	mode Mode,
	totalBytes int64,
	w http.ResponseWriter,
	rangeHeader string,
	minBuffer int,
) error {
	return c.stream(ctx, src, mode, totalBytes, w, rangeHeader, minBuffer, nil)
}

func (c *Cache) stream(
	ctx context.Context,
	src ChunkSource,
	mode Mode,
	totalBytes int64,
	w http.ResponseWriter,
	rangeHeader string,
	minBuffer int,
	correctedOffsets []int64,
) error {
	var playback playbackContext
	playbackOK := false
	if ctx != nil {
		playback, playbackOK = ctx.Value(playbackContextKey{}).(playbackContext)
	}
	firstByteRecorded := false

	// B-N8: if minBuffer > 0, use it instead of the configured default.
	// Stream path sets minBuffer=1 for fast first-byte; /dav passes 0 (uses cfg default of 4).
	effectiveMinBuffer := c.cfg.MinBufferSegments
	if minBuffer > 0 {
		effectiveMinBuffer = minBuffer
	}
	refs := src.Chunks()
	if len(refs) == 0 {
		return fmt.Errorf("rangecache: stream: source has no chunks")
	}
	if !ValidMode(mode) {
		mode = ModeReadahead
	}

	rangeStart, rangeEnd, isPartial := parseRange(rangeHeader, totalBytes)
	if isPartial {
		if totalBytes <= 0 || rangeStart < 0 || rangeStart >= totalBytes || rangeEnd < rangeStart {
			writeRangeNotSatisfiable(w, totalBytes)
			return fmt.Errorf("rangecache: unsatisfiable range %q for total %d", rangeHeader, totalBytes)
		}
		if rangeEnd >= totalBytes {
			rangeEnd = totalBytes - 1
		}
	}

	// Byte offsets for range-to-chunk mapping. When corrected offsets are
	// available (decoded sizes recorded from prior streaming passes), use them
	// for accurate seek. Fall back to NZB-declared sizes otherwise — these
	// diverge from actual decoded sizes by ~2% yEnc overhead per segment,
	// causing seek to land on the wrong segment. correctedOffsets is set by
	// StreamWithOffsets; plain Stream calls pass nil (declared sizes only).
	declStart := make([]int64, len(refs))
	if len(correctedOffsets) == len(refs)+1 {
		// Use corrected cumulative offsets from decoded-size cache.
		for i := range refs {
			declStart[i] = correctedOffsets[i]
		}
		// Only update totalBytes to the corrected total if it exceeds the declared
		// value — corrected totals are built from recorded decoded sizes which may
		// be partial (not all segments played yet), making the corrected total
		// smaller than declared. Shrinking totalBytes causes rangeEnd>=totalBytes
		// for requests using the declared size, producing spurious 416 responses.
		if ct := correctedOffsets[len(refs)]; ct > totalBytes {
			totalBytes = ct
		}
	} else {
		var offset int64
		for i, ref := range refs {
			declStart[i] = offset
			offset += ref.Size
		}
	}

	// Ordered chunk indices that overlap [rangeStart, rangeEnd] (declared).
	var work []int
	for i, ref := range refs {
		declEnd := declStart[i] + ref.Size
		if declEnd <= rangeStart || declStart[i] > rangeEnd {
			continue
		}
		work = append(work, i)
	}
	if isPartial && len(work) > 0 {
		c.log.Debug("rangecache: range mapped",
			"range_start", rangeStart, "range_end", rangeEnd,
			"first_chunk", work[0], "last_chunk", work[len(work)-1],
			"using_corrected", len(correctedOffsets) > 0,
			"total_chunks", len(refs))
	}
	if len(work) == 0 {
		if isPartial {
			writeRangeNotSatisfiable(w, totalBytes)
			return fmt.Errorf("rangecache: unsatisfiable range %q for total %d", rangeHeader, totalBytes)
		}
		return nil
	}

	// Response headers. Always set Content-Length — ffmpeg requires it on
	// 206 responses to seek correctly. Without it, Go's http.ResponseWriter
	// falls back to Transfer-Encoding: chunked which ffmpeg cannot seek in,
	// causing it to close the connection immediately after reading a few bytes.
	// For NZB/NNTP (ExactSizes==false) the NZB-declared size is used as the
	// Content-Length. yEnc decoding produces slightly fewer bytes than declared
	// (~2% overhead stripped), so we may over-declare by a small amount. This
	// is safe: ffmpeg reads until real EOF (connection close) or Content-Length
	// bytes, whichever comes first. Over-declaring is harmless; under-declaring
	// would truncate the stream.
	w.Header().Set("Accept-Ranges", "bytes")
	if isPartial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d",
			rangeStart, rangeEnd, totalBytes))
		w.Header().Set("Content-Length", strconv.FormatInt(rangeEnd-rangeStart+1, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		if totalBytes > 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(totalBytes, 10))
		}
		w.WriteHeader(http.StatusOK)
	}

	raActive := c.raActiveFor(mode)

	// Readahead: get-or-create a per-stream session keyed by source namespace
	// plus first chunk key. Sessions outlive individual HTTP connections.
	var sess *readaheadSession
	if raActive {
		sessKey := sha256hex(src.Key() + "|" + refs[0].Key)
		c.sessionMu.Lock()
		sess = c.sessions[sessKey]
		if sess == nil {
			sessCtx, sessCancel := context.WithCancel(c.sweeperCtx)
			sess = &readaheadSession{
				cancel:   sessCancel,
				buf:      make(map[int][]byte),
				claimed:  make(map[int]bool),
				refs:     refs,
				work:     work,
				lastUsed: c.now(),
			}
			sess.headCond = sync.NewCond(&sess.mu)
			atomic.StoreInt64(&sess.writePos, int64(work[0]))
			c.sessions[sessKey] = sess
			// Spawn N parallel readahead goroutines — one per readahead connection.
			// A single goroutine does sequential 230ms NNTP fetches and can never
			// outpace the write loop. N goroutines each claim a different target
			// chunk from the window and fetch in parallel, giving N× throughput.
			ceiling := c.cfg.ReadaheadWorkers
			if ceiling < 1 {
				ceiling = 1
			}
			// WorkerLimiter: let the source cap parallelism (e.g. CDN sources
			// that 429 under concurrent load should set MaxWorkers()=1).
			if wl, ok := src.(WorkerLimiter); ok {
				if max := wl.MaxWorkers(); max > 0 && max < ceiling {
					ceiling = max
				}
			}
			// NS-5.5: steady is the resting worker count -- a source's own
			// bitrate/throughput-based recommendation (SteadyWorkerRecommender)
			// when it has one, clamped into [1, ceiling]. Widening disabled
			// (SeekWidenDuration<=0) or the source doesn't implement the
			// interface: steady==ceiling, so activeLimit never has anywhere to
			// widen to -- a complete no-op, matching every pre-NS-5.5 source
			// exactly (CDN/HTTP today).
			steady := ceiling
			if c.cfg.SeekWidenDuration > 0 {
				if rec, ok := src.(SteadyWorkerRecommender); ok {
					if want := rec.SteadyWorkers(); want > 0 && want < ceiling {
						steady = want
					}
				}
			}
			sess.ceiling = int32(ceiling)
			sess.steady = int32(steady)
			sess.activeLimit.Store(int32(steady))
			for ra := 0; ra < ceiling; ra++ {
				go c.readaheadGoroutine(sessCtx, src, mode, refs, &sess.writePos, sess.buf, sess.claimed, &sess.mu, sess.headCond, sess, int32(ra))
			}
			go c.sessionReaper(sessKey, sess)
			c.log.Debug("rangecache: new readahead session", "key", sessKey[:8], "ns", src.Key(), "ceiling", ceiling, "steady", steady)
		} else {
			// Re-aim the live prefetcher at this request's start chunk so a
			// seek or reconnect doesn't wait on the min-buffer timeout.
			prevHead := atomic.LoadInt64(&sess.writePos)
			newHead := int64(work[0])
			seekDelta := newHead - prevHead
			if seekDelta < 0 {
				seekDelta = -seekDelta
			}
			atomic.StoreInt64(&sess.writePos, newHead)
			// A-B9: lastUsed must be written under sess.mu (not only sessionMu)
			// because sessionReaper reads it under sess.mu.
			sess.mu.Lock()
			now := c.now()
			// NS-5.5: decay a prior widen back to steady once its bounded
			// window has elapsed. Checked on every session touch (not just
			// seeks) so an idle-then-resumed stream doesn't stay artificially
			// widened forever (DG-09 bounded, DG-07 no unbounded state).
			if wu := sess.widenUntil.Load(); wu != 0 && now.UnixNano() >= wu {
				sess.activeLimit.Store(sess.steady)
				sess.widenUntil.Store(0)
			}
			// Seek flush: if the read head jumped more than ReadaheadMaxSegments,
			// the buffered chunks are for the wrong region. Evict only chunks
			// that are outside the new window [newHead, newHead+ReadaheadMaxSegments].
			// Critically: always keep chunk 0 — Jellyfin re-probes the file header
			// (byte offset 0) on every seek attempt before committing to the target
			// position. Evicting chunk 0 guarantees a cold header fetch on every
			// seek which causes the probe-and-reset loop.
			if seekDelta > int64(c.cfg.ReadaheadMaxSegments) {
				newWindowEnd := newHead + int64(c.cfg.ReadaheadMaxSegments)
				for k := range sess.buf {
					if k == 0 {
						continue // always keep the header chunk
					}
					if int64(k) < newHead || int64(k) > newWindowEnd {
						delete(sess.buf, k)
					}
				}
				clear(sess.claimed) // release stale claims so goroutines pick up new targets
				// NS-5.5: widen to the session's ceiling for a bounded window so the
				// extra (already-spawned, currently gated-out) readahead goroutines
				// help refill the window faster right after a seek, then decay back
				// to steady above once SeekWidenDuration elapses. A no-op when
				// SeekWidenDuration<=0 or ceiling==steady (no headroom -- e.g. a
				// source with no SteadyWorkerRecommender).
				if c.cfg.SeekWidenDuration > 0 && sess.ceiling > sess.steady {
					sess.activeLimit.Store(sess.ceiling)
					sess.widenUntil.Store(now.Add(c.cfg.SeekWidenDuration).UnixNano())
					c.log.Debug("rangecache: seek widen", "prev", prevHead, "new", newHead, "delta", seekDelta, "widened_to", sess.ceiling)
				}
				c.log.Debug("rangecache: seek flush", "prev", prevHead, "new", newHead, "delta", seekDelta)
			}
			sess.headCond.Broadcast() // PERF-7: wake readahead goroutines on seek/widen/decay
			sess.lastUsed = now
			sess.mu.Unlock()
			c.log.Debug("rangecache: reusing readahead session", "key", sessKey[:8], "ns", src.Key())
		}
		c.sessionMu.Unlock()

		// Wait for min buffer chunks before starting the write loop.
		// Skip for disk/full mode when the first chunk is already on disk —
		// GetChunk will serve it immediately without needing the readahead
		// goroutine. This is the key fix: after a seek flush the in-memory
		// readahead buffer is empty, but disk-cached chunks are immediately
		// available. Without this skip, we wait up to 30s for the readahead
		// goroutine to fetch from NNTP while Jellyfin times out in ~50ms.
		firstChunkOnDisk := false
		if (mode == ModeDisk || mode == ModeFull) && len(work) > 0 {
			firstKey := c.payloadKey(src, refs[work[0]])
			c.mu.Lock()
			_, firstChunkOnDisk = c.diskEntries[firstKey]
			c.mu.Unlock()
		}
		if !firstChunkOnDisk {
			c.log.Debug("rangecache: waiting for min buffer", "min", effectiveMinBuffer)
			timeout := time.NewTimer(c.minBufTimeout)
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
		minBuf:
			for {
				sess.mu.Lock()
				count := len(sess.buf)
				sess.mu.Unlock()
				if count >= effectiveMinBuffer || count >= len(work) {
					timeout.Stop()
					break minBuf
				}
				select {
				case <-ctx.Done():
					timeout.Stop()
					return ctx.Err()
				case <-timeout.C:
					c.log.Warn("rangecache: min buffer timeout, proceeding")
					break minBuf
				case <-ticker.C:
				}
			}
		}
	}

	// Write loop. Positioning in declared space (front-skip of the first
	// overlapping chunk); emission and stop in decoded space.
	flusher, canFlush := w.(http.Flusher)
	var emitted int64
	span := rangeEnd - rangeStart + 1

	for _, wi := range work {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if sess != nil {
			atomic.StoreInt64(&sess.writePos, int64(wi))
		}
		ref := refs[wi]

		// Try read-ahead buffer first (via session).
		var data []byte
		var hit bool
		if raActive && sess != nil {
			sess.mu.Lock()
			sess.headCond.Broadcast() // PERF-7: wake readahead goroutines on head advance
			data, hit = sess.buf[wi]
			if hit {
				delete(sess.buf, wi)
			}
			sess.mu.Unlock()
			if hit {
				c.log.Debug("rangecache: readahead hit", "chunk", ref.Index)
			}
		}

		// Classify only the first emitted byte. A readahead buffer hit is
		// warm; disk/full is warm only when this exact payload already exists
		// before demand fetch begins.
		warm := hit
		if playbackOK && !firstByteRecorded && !warm && (mode == ModeDisk || mode == ModeFull) {
			key := c.payloadKey(src, ref)
			c.mu.Lock()
			_, warm = c.diskEntries[key]
			c.mu.Unlock()
		}

		// Fall back to demand fetch.
		if !hit {
			c.log.Debug("rangecache: readahead miss, demand fetch", "chunk", ref.Index)
			start := c.now()
			var err error
			data, err = c.GetChunk(ctx, src, mode, ref)
			if err != nil {
				return fmt.Errorf("rangecache: chunk fetch error: %w", err)
			}
			if elapsed := c.now().Sub(start); elapsed > 5*time.Second {
				c.log.Warn("rangecache: stall detected", "chunk", ref.Index, "elapsed", elapsed)
			}
		}

		// Front-skip the first overlapping chunk in declared coordinates,
		// clamped to the actual decoded length (never slice past real bytes).
		if frontSkip := rangeStart - declStart[wi]; frontSkip > 0 {
			if frontSkip >= int64(len(data)) {
				continue
			}
			data = data[frontSkip:]
		}

		// Tail-trim by remaining requested span, in decoded bytes.
		if isPartial {
			remain := span - emitted
			if remain <= 0 {
				break
			}
			if int64(len(data)) > remain {
				data = data[:remain]
			}
		}

		if len(data) > 0 {
			n, err := w.Write(data)
			if n > 0 {
				c.observeDelivered(ctx, rangeStart+emitted, int64(n))
			}
			if err != nil {
				return fmt.Errorf("rangecache: write error: %w", err)
			}
			if n != len(data) {
				return fmt.Errorf("rangecache: write error: %w", io.ErrShortWrite)
			}
			if playbackOK && !firstByteRecorded && n > 0 {
				c.recordPlaybackFirstByte(playback, warm)
				firstByteRecorded = true
			}
			emitted += int64(n)
			if canFlush {
				flusher.Flush()
			}
		}

		// Keep session alive.
		if sess != nil {
			sess.mu.Lock()
			sess.lastUsed = c.now()
			sess.mu.Unlock()
		}

		// Tail eviction: drop read-ahead buffer entries behind the lookback
		// window (RAM buffer hygiene, unchanged).
		if c.cfg.TailEvict && raActive && sess != nil {
			cutoff := wi - c.cfg.TailLookback
			if cutoff > 0 {
				sess.mu.Lock()
				for k := range sess.buf {
					if k < cutoff {
						delete(sess.buf, k)
					}
				}
				sess.mu.Unlock()
			}
		}

		if isPartial && emitted >= span {
			break
		}
	}

	return nil
}

func (c *Cache) observeDelivered(ctx context.Context, offset, length int64) {
	if c == nil || c.coverage == nil || length <= 0 {
		return
	}
	representationID, ok := playbackcoverage.Representation(ctx)
	if !ok {
		return
	}
	if _, err := c.coverage.Observe(ctx, representationID, offset, length); err != nil {
		c.log.Warn("rangecache: delivered-byte observation failed (non-fatal)", "error", err)
	}
}

// GetChunk returns the bytes for one chunk under the given mode.
// none/readahead: always fetches live (demand class).
// disk/full: checks the shared disk cache first; fetches and caches on miss.
// Disk entries are content-sha-verified on read: a corrupt entry is evicted
// and refetched, never served.
func (c *Cache) GetChunk(ctx context.Context, src ChunkSource, mode Mode, ref ChunkRef) ([]byte, error) {
	return c.getChunk(ctx, src, mode, ref, FetchDemand, false)
}

// GetChunkPinned (TS-1.4) fetches and durably persists one chunk exactly
// like GetChunk under ModeFull, but marks the resulting disk entry pinned:
// the budgeted hot-head tier. Pinned entries are exempt from evictLRU and
// sweepOnce and are instead bounded by their own separate
// Config.PinnedBudgetMB budget and eviction (pinnedEvictLocked), so a
// prewarm/seek-index pass can never grow the cache unboundedly nor starve
// the ordinary playback cache's own budget. Storage is always ModeFull-
// shaped regardless of any runtime stream mode a caller might otherwise be
// using — pinning implies durable persistence by definition.
// Config.PinnedBudgetMB<=0 disables pinning specifically: the fetch still
// runs and is still cached, just through the ordinary disk-cache
// budget/eviction like any other ModeFull chunk.
func (c *Cache) GetChunkPinned(ctx context.Context, src ChunkSource, ref ChunkRef) ([]byte, error) {
	return c.getChunk(ctx, src, ModeFull, ref, FetchDemand, c.cfg.PinnedBudgetMB > 0)
}

// YieldForPromotion evicts cache data before one deliberate durable write and
// returns the projected free bytes above the configured volume floor. Both
// ordinary and pinned cache entries are disposable here; promoted media is
// never part of this index and therefore can never be selected.
func (c *Cache) YieldForPromotion(ctx context.Context, target string, pending int64, floorPercent int) (int64, error) {
	if c == nil || ctx == nil || target == "" || pending <= 0 || floorPercent < 1 || floorPercent > 99 {
		return 0, errors.New("rangecache: invalid promotion space request")
	}
	space := func() (free, floor int64, err error) {
		var stat syscall.Statfs_t
		if err = syscall.Statfs(filepath.Dir(target), &stat); err != nil {
			return 0, 0, err
		}
		free = int64(stat.Bavail) * int64(stat.Bsize)
		total := int64(stat.Blocks) * int64(stat.Bsize)
		floor = total * int64(floorPercent) / 100
		return free, floor, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		free, floor, err := space()
		if err != nil {
			return 0, err
		}
		if free >= pending && free-pending >= floor {
			return free - pending - floor, nil
		}

		c.mu.Lock()
		var oldestKey string
		var oldest diskEntry
		first := true
		for key, entry := range c.diskEntries {
			if first || entry.lastUsed.Before(oldest.lastUsed) {
				oldestKey, oldest, first = key, entry, false
			}
		}
		if first {
			c.mu.Unlock()
			return 0, errors.New("rangecache: promotion would cross free-space floor")
		}
		if err := os.Remove(oldest.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			c.mu.Unlock()
			return 0, errors.New("rangecache: promotion cache eviction failed")
		}
		if oldest.pinned {
			_ = os.Remove(pinnedMarkerPath(oldest.path))
		}
		delete(c.diskEntries, oldestKey)
		c.diskSizeB -= oldest.sizeB
		if oldest.pinned {
			c.pinnedSizeB -= oldest.sizeB
		}
		if c.diskSizeB < 0 {
			c.diskSizeB = 0
		}
		if c.pinnedSizeB < 0 {
			c.pinnedSizeB = 0
		}
		c.mu.Unlock()
	}
}

// getChunk is the class-aware implementation. GetChunk uses FetchDemand;
// PrefetchChunk uses FetchReadahead so disk-mode prefetch routes through the
// readahead pool instead of contending on the demand pool (PERF-1). pin
// (TS-1.4) requests hot-head tier promotion; see GetChunkPinned.
func (c *Cache) getChunk(ctx context.Context, src ChunkSource, mode Mode, ref ChunkRef, class FetchClass, pin bool) ([]byte, error) {
	if mode == ModeNone || mode == ModeReadahead {
		data, err := src.Fetch(ctx, ref, FetchDemand)
		if err == nil {
			data = transformChunk(src, ref, data)
			if err = validateChunk(ctx, src, ref, data); err != nil {
				return nil, err
			}
			observeChunk(src, ref, data)
		}
		return data, err
	}

	key := c.payloadKey(src, ref)

	c.mu.Lock()
	entry, ok := c.diskEntries[key]
	c.mu.Unlock()

	if ok {
		data, err := os.ReadFile(entry.path)
		if err == nil {
			// PERF-5: SHA-verify only unverified entries (restored from disk
			// at startup with empty contentSHA). Once verified, trust the
			// content on subsequent reads — removes per-hit SHA-256 CPU tax.
			needsVerify := entry.contentSHA == ""
			if needsVerify {
				sha := sha256hexBytes(data)
				c.mu.Lock()
				if e, still := c.diskEntries[key]; still && e.contentSHA == "" {
					e.contentSHA = sha
					e.lastUsed = c.now()
					c.promoteAndStoreLocked(key, e, pin)
				}
				c.mu.Unlock()
			} else {
				c.mu.Lock()
				if e, still := c.diskEntries[key]; still {
					e.lastUsed = c.now()
					c.promoteAndStoreLocked(key, e, pin)
				}
				c.mu.Unlock()
			}
			c.log.Debug("rangecache: disk hit", "chunk", ref.Index, "key", key[:8])
			c.diskHits.Add(1)
			if entry.sourceKey != "" && entry.sourceKey != src.Key() {
				c.crossHits.Add(1)
			}
			data = transformChunk(src, ref, data)
			if err := validateChunk(ctx, src, ref, data); err != nil {
				return nil, err
			}
			observeChunk(src, ref, data)
			return data, nil
		}
		// Read error: evict and fall through to live fetch.
		c.log.Warn("rangecache: disk entry read error, evicting and re-fetching",
			"key", key[:8], "read_err", err)
		c.mu.Lock()
		wasPinned := false
		if e, still := c.diskEntries[key]; still {
			delete(c.diskEntries, key)
			c.diskSizeB -= e.sizeB
			if c.diskSizeB < 0 {
				c.diskSizeB = 0
			}
			if e.pinned {
				wasPinned = true
				c.pinnedSizeB -= e.sizeB
				if c.pinnedSizeB < 0 {
					c.pinnedSizeB = 0
				}
			}
		}
		c.mu.Unlock()
		_ = os.Remove(entry.path)
		if wasPinned {
			_ = os.Remove(pinnedMarkerPath(entry.path))
		}
	}

	c.log.Debug("rangecache: disk miss", "chunk", ref.Index, "key", key[:8], "class", class)
	c.diskMisses.Add(1)
	// HR1.3: concurrent callers wanting this exact payload share one upstream
	// read. The (nil,nil) readahead fallback moved inside fetchCoalesced so
	// followers inherit it identically.
	data, err, leader := c.fetchCoalesced(ctx, src, ref, key, class)
	if err != nil {
		return nil, fmt.Errorf("rangecache: fetch failed: %w", err)
	}
	if !leader {
		// A follower already holds its own copy of bytes the leader is
		// persisting; writing again would duplicate the disk work.
		data = transformChunk(src, ref, data)
		if err := validateChunk(ctx, src, ref, data); err != nil {
			return nil, err
		}
		observeChunk(src, ref, data)
		return data, nil
	}

	visibleData := transformChunk(src, ref, data)
	if err := validateChunk(ctx, src, ref, visibleData); err != nil {
		return nil, err
	}

	// Persist only complete fetches: Fetch either returned full data or an
	// error above — a failed/partial fetch is never written to disk.
	path := filepath.Join(c.cfg.DiskCachePath, key)
	if werr := atomicWriteFile(path, data, 0o644); werr != nil {
		c.log.Warn("rangecache: disk write error", "key", key[:8], "err", werr)
	} else {
		c.mu.Lock()
		c.diskEntries[key] = diskEntry{
			key:        key,
			path:       path,
			sizeB:      int64(len(data)),
			lastUsed:   c.now(),
			mode:       mode,
			contentSHA: sha256hexBytes(data),
			sourceKey:  src.Key(),
			pinned:     pin,
		}
		c.diskSizeB += int64(len(data))
		if pin {
			c.pinnedSizeB += int64(len(data))
			if werr := os.WriteFile(pinnedMarkerPath(path), nil, 0o644); werr != nil {
				c.log.Warn("rangecache: pin marker write failed (non-fatal)", "key", key[:8], "err", werr)
			}
			c.pinnedEvictLocked()
		} else if c.diskSizeB > c.cfg.DiskCacheSizeMB*1024*1024 {
			c.evictLRU()
		}
		c.mu.Unlock()
	}

	observeChunk(src, ref, visibleData)
	return visibleData, nil
}

func transformChunk(src ChunkSource, ref ChunkRef, data []byte) []byte {
	if transformer, ok := src.(ChunkTransformer); ok {
		return transformer.TransformChunk(ref, data)
	}
	return data
}

func validateChunk(ctx context.Context, src ChunkSource, ref ChunkRef, data []byte) error {
	if validator, ok := src.(ChunkValidator); ok {
		return validator.ValidateChunk(ctx, ref, data)
	}
	return nil
}

func observeChunk(src ChunkSource, ref ChunkRef, data []byte) {
	if observer, ok := src.(ChunkObserver); ok {
		observer.ObserveChunk(ref, data)
	}
}

func (c *Cache) payloadKey(src ChunkSource, ref ChunkRef) string {
	identity := src.Key() + "|" + ref.Key
	if keyer, ok := src.(PayloadKeyer); ok {
		identity = keyer.PayloadKey(ref)
	}
	return sha256hex(identity)
}

// pinnedMarkerPath returns the sibling marker-file path that records a
// pinned disk entry's status durably across a restart (TS-1.4). It is a
// zero-byte sentinel; its mere existence means "path is pinned". Kept
// deliberately outside the diskEntries index format so it cannot be
// confused with an ordinary 64-hex-char cache entry by the restore loop's
// existing len(key)==64 filter.
func pinnedMarkerPath(path string) string { return path + ".pin" }

// promoteAndStoreLocked (TS-1.4) stores e for key and, when pin is requested
// and e was not already pinned, promotes it into the pinned tier — bumping
// pinnedSizeB, writing the durable pin marker, and triggering
// pinnedEvictLocked if the separate pinned budget is now exceeded. Must be
// called with c.mu held.
func (c *Cache) promoteAndStoreLocked(key string, e diskEntry, pin bool) {
	if pin && !e.pinned {
		e.pinned = true
		c.pinnedSizeB += e.sizeB
		if werr := os.WriteFile(pinnedMarkerPath(e.path), nil, 0o644); werr != nil {
			c.log.Warn("rangecache: pin marker write failed (non-fatal)", "key", key[:8], "err", werr)
		}
	}
	c.diskEntries[key] = e
	if e.pinned {
		c.pinnedEvictLocked()
	}
}

// pinnedEvictLocked (TS-1.4) evicts least-recently-used PINNED disk entries
// until pinnedSizeB is back within Config.PinnedBudgetMB, mirroring
// evictLRU's sampled-LRU approach but scoped to the pinned subset only, so
// the pinned tier is bounded independently of — and never touches — the
// ordinary disk cache's own LRU/TTL policy. A no-op when PinnedBudgetMB<=0
// (pinning disabled; GetChunkPinned never sets pin=true in that case, so
// pinnedSizeB stays zero and this never has work to do).
// Must be called with c.mu held.
func (c *Cache) pinnedEvictLocked() {
	budget := c.cfg.PinnedBudgetMB * 1024 * 1024
	if budget <= 0 {
		return
	}
	lowWater := budget * 95 / 100

	const sampleK = 16
	for c.pinnedSizeB > lowWater {
		var oldestKey string
		var oldest diskEntry
		first := true
		i := 0
		for k, entry := range c.diskEntries {
			if !entry.pinned {
				continue
			}
			if first || entry.lastUsed.Before(oldest.lastUsed) {
				oldestKey, oldest = k, entry
				first = false
			}
			i++
			if i >= sampleK {
				break
			}
		}
		if first {
			// No pinned entries found (an accounting-drift guard) — stop
			// rather than loop forever.
			break
		}
		_ = os.Remove(oldest.path)
		_ = os.Remove(pinnedMarkerPath(oldest.path))
		delete(c.diskEntries, oldestKey)
		c.diskSizeB -= oldest.sizeB
		c.pinnedSizeB -= oldest.sizeB
		if c.diskSizeB < 0 {
			c.diskSizeB = 0
		}
		if c.pinnedSizeB < 0 {
			c.pinnedSizeB = 0
		}
		c.log.Info("rangecache: pinned tier evicted LRU", "key", oldestKey[:8], "freed_bytes", oldest.sizeB)
	}
}

func (c *Cache) recordPlaybackFirstByte(playback playbackContext, warm bool) {
	if playback.lane < 0 || playback.lane >= len(c.playback) {
		return
	}
	temperature := 0
	if warm {
		temperature = 1
	}
	elapsed := c.now().Sub(playback.started)
	if elapsed < 0 {
		elapsed = 0
	}
	stat := &c.playback[playback.lane][temperature]
	nanos := elapsed.Nanoseconds()
	stat.count.Add(1)
	stat.total.Add(nanos)
	for current := stat.max.Load(); nanos > current && !stat.max.CompareAndSwap(current, nanos); current = stat.max.Load() {
	}
}

// SnapshotMap returns secret-safe cache counters for health evidence.
func (c *Cache) SnapshotMap() map[string]any {
	c.mu.Lock()
	pinnedEntries := 0
	for _, e := range c.diskEntries {
		if e.pinned {
			pinnedEntries++
		}
	}
	pinnedBytes := c.pinnedSizeB
	c.mu.Unlock()
	snapshot := map[string]any{
		"disk_hits":        c.diskHits.Load(),
		"disk_misses":      c.diskMisses.Load(),
		"cross_item_hits":  c.crossHits.Load(),
		"coalesced_joins":  c.coalescedJoins.Load(),
		"coalesced_aborts": c.coalescedAborts.Load(),
		"demand_joins":     c.demandJoins.Load(),
		"inflight":         c.inflightLen(),
		"pinned_bytes":     pinnedBytes,
		"pinned_entries":   pinnedEntries,
	}
	lanes := [...]string{"torrent", "nntp", "http"}
	temperatures := [...]string{"cold", "warm"}
	for lane := range lanes {
		for temperature := range temperatures {
			stat := &c.playback[lane][temperature]
			prefix := "first_byte_" + lanes[lane] + "_" + temperatures[temperature]
			snapshot[prefix+"_count"] = stat.count.Load()
			snapshot[prefix+"_total_ms"] = stat.total.Load() / int64(time.Millisecond)
			snapshot[prefix+"_max_ms"] = stat.max.Load() / int64(time.Millisecond)
		}
	}
	return snapshot
}

// PrefetchChunk fetches one chunk for read-ahead. In disk/full mode it routes
// through GetChunk so the chunk is also disk-cached (unchanged quirk: those
// prefetches use the source's demand class). In readahead mode it issues a
// readahead-class fetch; (nil, nil) means readahead unavailable.
func (c *Cache) PrefetchChunk(ctx context.Context, src ChunkSource, mode Mode, ref ChunkRef) ([]byte, error) {
	if mode == ModeDisk || mode == ModeFull {
		return c.getChunk(ctx, src, mode, ref, FetchReadahead, false)
	}
	data, err := src.Fetch(ctx, ref, FetchReadahead)
	if err == nil {
		data = transformChunk(src, ref, data)
		observeChunk(src, ref, data)
	}
	return data, err
}

// sessionReaper monitors a readahead session and cancels it after 2 minutes
// of inactivity. Unchanged from the former SegmentCache.
func (c *Cache) sessionReaper(key string, sess *readaheadSession) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.sweeperCtx.Done():
			sess.cancel()
			return
		case <-ticker.C:
			sess.mu.Lock()
			idle := c.now().Sub(sess.lastUsed)
			sess.mu.Unlock()
			if idle > 2*time.Minute {
				c.log.Debug("rangecache: session idle, reaping", "key", key[:8])
				sess.cancel()
				c.sessionMu.Lock()
				delete(c.sessions, key)
				c.sessionMu.Unlock()
				return
			}
		}
	}
}

// readaheadGoroutine prefetches chunks ahead of the live read position into
// the session buffer and follows that position across sequential reads and
// seeks. It keeps at most ReadaheadMaxSegments chunks prefetched ahead of the
// read head and idles once caught up, so a probe-then-disconnect never pulls
// the whole file. Unlike the prior implementation it does NOT terminate at the
// end of any single request's range: the session is per-file and outlives
// individual HTTP connections (ffmpeg probe-then-seek, Jellyfin reconnects),
// so the prefetcher lives until the session is cancelled (idle reap or Close).
// writePos is a chunk ordinal set by each Stream write loop.
func (c *Cache) readaheadGoroutine(
	ctx context.Context,
	src ChunkSource,
	mode Mode,
	refs []ChunkRef,
	writePos *int64,
	readaheadBuf map[int][]byte,
	claimed map[int]bool, // chunks claimed by this or sibling goroutines
	readaheadMu *sync.Mutex,
	headCond *sync.Cond, // PERF-7: signaled on head advance
	sess *readaheadSession, // NS-5.5: source of this goroutine's activeLimit gate
	myIndex int32, // NS-5.5: this goroutine's fixed ordinal (0..ceiling-1)
) {
	n := len(refs)
	ahead := c.cfg.ReadaheadMaxSegments
	if ahead < 1 {
		ahead = 1
	}
	// BUGFIX 2026-07-01 (torbox API-hit / playback-area audit): found by
	// tracing the exact "TorBox CDN outage -> Jellyfin loops" incident
	// already documented in REDESIGN S14. cdnChunkSource.Fetch issues a
	// direct HTTP range GET against the CDN URL -- it does NOT go through
	// torbox.HTTPClient.do(), so it is NOT covered by provider.Policy's
	// rate limiter at all. Before this fix, a persistently failing chunk
	// (a dead CDN node, exactly the documented nexus-074/090 scenario) was
	// retried here on a flat, unconditional 50ms backoff FOREVER -- 20
	// requests/second against a known-dead upstream, completely
	// unthrottled, for as long as the session lives (which a still-playing
	// client keeps alive well past any reasonable retry window). consecFails
	// is keyed per chunk index (not global) so one bad chunk backs off
	// independently of the window's other, healthy chunks; it resets to 0
	// the moment that chunk's fetch succeeds. Backoff escalates 50ms -> ...
	// -> capped at 10s, and repeated failure on the same chunk is logged at
	// WARN (was silent at Debug) so an outage like this is visible in the
	// normal log stream instead of only discoverable by noticing the
	// downstream Jellyfin symptom.
	// PERF-7: ensure cond.Wait() unblocks when context is cancelled.
	go func() {
		<-ctx.Done()
		headCond.Broadcast()
	}()
	// SF-01: flush any pending suppressed-prefetch-failure summary once this
	// session's readahead goroutine exits (session reaped/cancelled), so a
	// final in-window burst is never silently dropped.
	defer c.limiter.FlushItem(c.log, src.Key())
	consecFails := map[int]int{}
	for ctx.Err() == nil {
		head := atomic.LoadInt64(writePos)
		if head < 0 {
			head = 0
		}

		// NS-5.5: a goroutine whose index is at or beyond the session's
		// current activeLimit is benched -- it does not claim work until a
		// seek widens activeLimit past its index, or (for a source with no
		// SteadyWorkerRecommender / widening disabled) activeLimit==ceiling
		// always, so this check is always true and every goroutine is
		// always active -- the exact pre-NS-5.5 behavior.
		target := -1
		if myIndex < sess.activeLimit.Load() {
			// Prefetch the lowest not-yet-buffered chunk in the bounded window
			// (head, head+ahead], clamped to EOF. The current chunk (head) is
			// owned by the write loop; starting at head+1 avoids re-fetching a
			// chunk the write loop just consumed. A seek (head jumps) re-aims the
			// window instantly with no explicit seek detection.
			// Pick the lowest unfetched, unclaimed chunk in the window.
			// claimed prevents N sibling goroutines from all fetching the same chunk.
			readaheadMu.Lock()
			for off := int64(1); off <= int64(ahead) && head+off < int64(n); off++ {
				idx := int(head + off)
				_, have := readaheadBuf[idx]
				_, isClaimed := claimed[idx]
				if !have && !isClaimed {
					target = idx
					claimed[idx] = true // claim before releasing lock
					break
				}
			}
			readaheadMu.Unlock()
		}

		if target < 0 {
			// PERF-7: wait for the write-head to advance instead of 10ms
			// polling. headCond is broadcast on every head advance and on
			// buffer consumption. A 200ms timeout ensures ctx cancellation
			// and seek detection are still checked promptly.
			if ctx.Err() != nil {
				return
			}
			readaheadMu.Lock()
			headCond.Wait()
			readaheadMu.Unlock()
			continue
		}

		data, err := c.PrefetchChunk(ctx, src, mode, refs[target])
		if err != nil {
			consecFails[target]++
			fails := consecFails[target]
			backoff := readaheadPrefetchBackoff(fails)
			// SF-01: shared rate-limited WARN + periodic summary counter
			// (outcome.Limiter) replaces the former fixed fail#1/every-10th
			// cadence. Backoff scheduling below is unchanged. A persistently
			// failing chunk (dead NNTP article, downed CDN node) is neither a
			// one-off transient blip nor proof the source itself is gone —
			// only sustained failure past a caller's own dead-source signal
			// (e.g. ErrDeadPost) earns permanent-here, so this stays
			// transient-here: a future alternate-provider rung may still
			// recover it.
			c.limiter.Warn(c.log, src.Key(), outcome.ClassTransientHere, "prefetch_backoff",
				"rangecache: prefetch repeatedly failing, backing off",
				"chunk", refs[target].Index, "consecutive_fails", fails, "backoff", backoff.String(),
				"class", string(outcome.ClassTransientHere), "err", err)
			readaheadMu.Lock()
			delete(claimed, target) // release claim on failure
			readaheadMu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			continue
		}
		delete(consecFails, target)
		readaheadMu.Lock()
		delete(claimed, target) // release claim
		if data != nil {
			readaheadBuf[target] = data
		}
		readaheadMu.Unlock()
	}
}

// readaheadPrefetchBackoff returns the delay before retrying a readahead
// chunk after its n-th consecutive failure: 50ms, 100ms, 200ms, ... doubling
// up to a 10s ceiling, so a persistently dead upstream (a downed CDN node,
// an NNTP article that will never resolve) is retried at a bounded,
// sustainable rate instead of an unconditional 20/s forever.
func readaheadPrefetchBackoff(consecutiveFails int) time.Duration {
	const (
		base = 50 * time.Millisecond
		cap_ = 10 * time.Second
	)
	if consecutiveFails < 1 {
		consecutiveFails = 1
	}
	if consecutiveFails > 20 { // 50ms * 2^20 overflows well past cap_; clamp the shift itself
		return cap_
	}
	d := base << uint(consecutiveFails-1)
	if d > cap_ || d <= 0 {
		return cap_
	}
	return d
}

// evictLRU evicts least-recently-used disk entries until the shared budget
// is satisfied. Must be called with c.mu held. PERF-6: evicts to a 95%
// low-water mark using sampled-LRU (oldest of K=16 random entries per round)
// instead of a full O(n log n) sort of all entries.
func (c *Cache) evictLRU() {
	budget := c.cfg.DiskCacheSizeMB * 1024 * 1024
	lowWater := budget * 95 / 100 // stop at 95% — avoid re-triggering on the next write

	const sampleK = 16
	for c.diskSizeB > lowWater {
		// Pick the oldest of up to sampleK random non-pinned entries (Go map
		// iteration is random). TS-1.4: pinned entries are exempt from this
		// sweep — pinnedEvictLocked bounds them independently.
		var oldestKey string
		var oldest diskEntry
		first := true
		i := 0
		for k, entry := range c.diskEntries {
			if entry.pinned {
				continue
			}
			if first || entry.lastUsed.Before(oldest.lastUsed) {
				oldestKey, oldest = k, entry
				first = false
			}
			i++
			if i >= sampleK {
				break
			}
		}
		if first {
			// Nothing left to evict under the main budget — every remaining
			// entry is pinned. Stop rather than loop forever; the pinned
			// tier's own budget (pinnedEvictLocked) governs those entries.
			break
		}
		_ = os.Remove(oldest.path)
		delete(c.diskEntries, oldestKey)
		c.diskSizeB -= oldest.sizeB
		if c.diskSizeB < 0 {
			c.diskSizeB = 0
		}
		c.log.Info("rangecache: evicted LRU", "key", oldestKey[:8], "freed_bytes", oldest.sizeB)
	}
}

// sweepOnce evicts disk entries whose mode-specific TTL has expired as of
// now. disk-mode entries use DiskCacheTTLMin; full-mode entries use
// FullEvictTTLMin when FullEvictMode=="ttl" and are never swept otherwise
// (manual/never policies). Returns the number of entries evicted.
func (c *Cache) sweepOnce(now time.Time) int {
	c.mu.Lock()
	var toEvict []diskEntry
	for key, entry := range c.diskEntries {
		if entry.pinned {
			// TS-1.4: pinned entries are exempt from the TTL sweep;
			// pinnedEvictLocked bounds them independently.
			continue
		}
		var ttl time.Duration
		switch entry.mode {
		case ModeDisk:
			ttl = time.Duration(c.cfg.DiskCacheTTLMin) * time.Minute
		case ModeFull:
			if c.cfg.FullEvictMode != "ttl" {
				continue
			}
			ttl = time.Duration(c.cfg.FullEvictTTLMin) * time.Minute
		default:
			continue
		}
		if now.Sub(entry.lastUsed) > ttl {
			toEvict = append(toEvict, entry)
			delete(c.diskEntries, key)
			c.diskSizeB -= entry.sizeB
			if c.diskSizeB < 0 {
				c.diskSizeB = 0
			}
		}
	}
	c.mu.Unlock()

	for _, entry := range toEvict {
		_ = os.Remove(entry.path)
		c.log.Info("rangecache: ttl sweep evicted",
			"key", entry.key[:8],
			"age_min", now.Sub(entry.lastUsed).Minutes())
	}
	return len(toEvict)
}

// startTTLSweeper periodically runs sweepOnce. Interval is the smallest
// applicable TTL / 4, clamped to >= 1 minute.
func (c *Cache) startTTLSweeper(ctx context.Context) {
	defer close(c.sweeperDone)
	ttl := time.Duration(c.cfg.DiskCacheTTLMin) * time.Minute
	if c.cfg.FullEvictMode == "ttl" {
		if ft := time.Duration(c.cfg.FullEvictTTLMin) * time.Minute; ft < ttl {
			ttl = ft
		}
	}
	interval := ttl / 4
	if interval < time.Minute {
		interval = time.Minute
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		c.sweepOnce(c.now())
	}
}

// atomicWriteFile writes data to path via a same-directory temp file and
// rename, so a crash or concurrent reader never observes a partial chunk.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("rangecache: atomic write mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("rangecache: atomic write temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("rangecache: atomic write temp %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("rangecache: atomic chmod temp %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("rangecache: atomic close temp %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rangecache: atomic rename %s to %s: %w", tmpName, path, err)
	}
	keep = true
	return nil
}

func writeRangeNotSatisfiable(w http.ResponseWriter, totalBytes int64) {
	if totalBytes < 0 {
		totalBytes = 0
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", totalBytes))
	w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
}

// parseRange parses an HTTP Range header and returns (start, end, isPartial).
// Returns (0, totalBytes-1, false) for missing or malformed headers.
// Carried verbatim from the former SegmentCache.
// parseRange parses an HTTP Range header per RFC 7233 and returns
// (rangeStart, rangeEnd, isPartial).  Both endpoints are inclusive.
// Suffix ranges ("bytes=-N") are correctly translated to the last N bytes.
// A-B4: previous implementation served the first N bytes for suffix ranges.
func parseRange(rangeHeader string, totalBytes int64) (int64, int64, bool) {
	if rangeHeader == "" || !strings.HasPrefix(rangeHeader, "bytes=") {
		return 0, totalBytes - 1, false
	}

	spec := strings.TrimPrefix(rangeHeader, "bytes=")
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, totalBytes - 1, false
	}

	// Suffix range: "bytes=-N" means the last N bytes.
	if parts[0] == "" {
		if parts[1] == "" {
			return 0, totalBytes - 1, false
		}
		n, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || n <= 0 {
			return 0, totalBytes - 1, false
		}
		start := totalBytes - n
		if start < 0 {
			start = 0
		}
		return start, totalBytes - 1, true
	}

	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, totalBytes - 1, false
	}

	end := totalBytes - 1
	if parts[1] != "" {
		v, verr := strconv.ParseInt(parts[1], 10, 64)
		if verr != nil {
			return 0, totalBytes - 1, false
		}
		end = v
	}

	return start, end, true
}

// sha256hex returns the hex-encoded SHA-256 of s.
func sha256hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// sha256hexBytes returns the hex-encoded SHA-256 of b without converting to
// string first. B-O6: avoids the full-chunk allocation from sha256hexBytes(data).
func sha256hexBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
