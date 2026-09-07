package api

import (
	"context"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/auth"
	"github.com/darkharrbor/darkharrbor/internal/availability"
	"github.com/darkharrbor/darkharrbor/internal/cachegate"
	"github.com/darkharrbor/darkharrbor/internal/compat"
	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/contentproof"
	"github.com/darkharrbor/darkharrbor/internal/crosslane"
	"github.com/darkharrbor/darkharrbor/internal/experience"
	"github.com/darkharrbor/darkharrbor/internal/hlssession"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/identitycheck"
	"github.com/darkharrbor/darkharrbor/internal/mediatruth"
	"github.com/darkharrbor/darkharrbor/internal/nntp"
	"github.com/darkharrbor/darkharrbor/internal/output"
	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/prowlarr"
	"github.com/darkharrbor/darkharrbor/internal/publicip"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
	"github.com/darkharrbor/darkharrbor/internal/reactivecommit"
	"github.com/darkharrbor/darkharrbor/internal/reactivepromotion"
	"github.com/darkharrbor/darkharrbor/internal/searchbudget"
	"github.com/darkharrbor/darkharrbor/internal/sidecar"
	"github.com/darkharrbor/darkharrbor/internal/store"
	"github.com/darkharrbor/darkharrbor/internal/torrentmeta"
	"github.com/darkharrbor/darkharrbor/internal/util"
)

// ProviderLane bundles one debrid provider with its own independent gate and
// governor. Each lane is self-contained — no cross-lane knowledge or fallback.
// C-5: submitItemDirect and the submit recovery loop walk lanes in order;
// the first to accept wins, binding the item to that provider via item.Provider.
type ProviderLane struct {
	Prov provider.Provider
	Gate *cachegate.Gate
}

type Server struct {
	cfg *config.Config
	log *slog.Logger
	// nowFn overrides the clock in tests (standing injectable-clock
	// convention); nil means time.Now.
	nowFn     func() time.Time
	store     *store.Store
	qbitAuth  *auth.QBitSessionManager
	sabAuth   *auth.SABAuth
	startedAt time.Time

	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
	wg             sync.WaitGroup

	// rx95 binds an aggregator playback batch to the Stremio coordinate
	// observed at the addon boundary moments earlier (RX-9.5 / RD-31).
	rx95 *rx95Store
	// rx93CaptureSem bounds aggregator-to-library capture work; the
	// singleflight group collapses simultaneous plays of one representation.
	rx93CaptureSem   chan struct{}
	rx93CaptureGroup singleflight.Group
	rx93Selectors    *rx93SelectorStore
	// janitorMu protects janitorTimers map.
	janitorMu sync.Mutex
	// janitorTimers maps item.ID -> cancel function for the active cleanup timer.
	janitorTimers map[string]context.CancelFunc
	// lastPlayedMu protects lastPlayedWrites, the in-memory write throttle for
	// last_played_at persistence. Janitor timers are still reset on every play.
	lastPlayedMu     sync.Mutex
	lastPlayedWrites map[string]time.Time
	// interface-only consumption; internal/torbox satisfies provider.Provider).
	// The proxy handler and janitor call provider methods, never torbox APIs.
	// prov is the primary (first) debrid provider, kept for single-provider paths.
	// C-3/C-5: allProviders and providerOrder enable multi-provider submit failover.
	prov          provider.Provider
	allProviders  map[string]provider.Provider
	providerOrder []string
	gate          *cachegate.Gate
	// lanes is the ordered list of independent debrid provider lanes (C-5).
	// Each entry owns its provider, gate, and governor — fully self-contained.
	lanes  []ProviderLane
	writer output.Writer
	// itemMu protects activeGoroutines map.
	itemMu           sync.Mutex
	activeGoroutines map[string]struct{}
	// submitGroup dedups provider createtorrent by real infohash so a full-series
	// grab (many synthetic per-season releases sharing one real torrent) creates
	submitGroup singleflight.Group
	// torznabCacheMu protects torznabCache.
	torznabCacheMu sync.RWMutex
	torznabCache   map[string]torznabCacheEntry
	// magnetCacheMu protects magnetCache.
	magnetCacheMu sync.RWMutex
	// magnetCache maps infohash -> {magnet, release title} for the /download/
	// handler and for naming qbit adds whose magnets carry no dn.
	magnetCache      map[string]magnetEntry
	magnetCacheOrder []string
	// prowlarr is the selection-mode meta-indexer client; nil unless SelectionMode.
	prowlarr     *prowlarr.Client
	searchBudget *searchbudget.Service
	// httpHandlers is the HTTP stream protocol handler registry (D5). Set
	// once at startup via SetHTTPStreamRegistry; empty until H2 registers
	// the first backend handler. Nil-safe throughout.
	httpHandlers *httpstream.Registry
	// httpClientOnce initializes separate D11-policy-wired source clients:
	// one bounded probe client and one no-whole-movie-deadline relay client
	// (HS-3.3/HS-3.10). Isolated from provider and NNTP transports.
	httpClientOnce  sync.Once
	httpProbeClient *http.Client
	// doctorDockerProxyURL is set post-construction via SetDoctorDockerProxyURL.
	doctorDockerProxyURL string
	publicIPResolver     *publicip.Resolver
	httpRelayClient      *http.Client
	// arrMetadataClient fetches bounded authoritative episode facts for
	// HR2.8 matching. Redirects are rejected so X-Api-Key never crosses hosts.
	arrMetadataClient *http.Client
	// httpResolveCoord coalesces concurrent play-time resolve/refresh calls
	// per (itemID,fileID) so a Jellyfin probe burst never fans out into N
	// independent backend calls (HS-3.8, A19).
	httpResolveCoord *httpResolveCoordinator
	// nzbProxyMu protects nzbProxyCache.
	nzbProxyMu sync.RWMutex
	// nzbProxyCache maps a feed token -> the upstream Prowlarr .nzb download URL,
	// so /getnzb/{token} can fetch and serve the NZB on the arr's behalf (the arr
	// has no Prowlarr API key in selection mode).
	nzbProxyCache      map[string]string
	nzbProxyCacheOrder []string
	// usenetProviders holds every configured usenet provider (Newshosting,
	// TorBox NNTP, any additional named ones), keyed by registry name — each
	// one self-contained (own connection pool/segment cache, built in
	// cmd/darkharrbor/main.go via the same registerUsenetFactory path
	// regardless of provider). usenetOrder is the configured preference
	// order (HARRBOR_USENET_PROVIDERS); empty map/slice means no usenet
	// provider is configured at all. pickUsenetProvider is the single
	// chokepoint that resolves "which provider serves this item" — an
	// item's persisted Provider field if set and still configured, else the
	// first in usenetOrder — so resolve-time provider selection and
	// stream-time provider selection can never silently disagree.
	usenetProviders map[string]provider.UsenetProvider
	usenetOrder     []string
	// streamCache is the shared byte-range cache for torrent/CDN and
	// progressive HTTP playback.
	streamCache      *rangecache.Cache
	promotion        *reactivepromotion.Service
	playbackCoverage *playbackcoverage.Tracker
	// playbackObserve overrides byte-coverage persistence in tests. Production
	// leaves it nil and uses playbackCoverage.Observe.
	playbackObserve func(context.Context, string, int64, int64) (playbackcoverage.Snapshot, error)
	// torrentGov (TS-1.4) is the shared accountgov.Governor arbitrating
	// priority between live torrent/debrid-CDN playback fetches
	// (accountgov.PriorityPlayback) and TS-1.4 prewarm
	// (accountgov.PriorityPrewarm) for the same op class, TorrentCDNGovOp.
	// nil when unconfigured (every fetch then runs unleased — no priority
	// arbitration, identical to pre-TS-1.4 behavior).
	torrentGov *accountgov.Governor
	// experienceSvc (TS-1.4) is the shared prewarm/seek-target/adaptive-
	// readahead owner (internal/experience), consulted by streamViaCache
	// for its per-session readahead worker count. nil-safe throughout.
	experienceSvc *experience.Service
	// httpGov arbitrates HTTP playback, the D9 grab-time preflight, and
	// startup prewarm on one HTTPSourceGovOp capacity.
	httpGov *accountgov.Governor
	// httpExpSvc (HR5.3) is a second experience.Service instance sharing
	// the SAME streamCache as the torrent lane's expSvc (no parallel cache
	// hierarchy) but bound to httpGov/HTTPSourceGovOp, so HTTP's startup
	// prewarm (prewarmHTTPFile) uses the one shared prewarm/mediatruth
	// implementation with its own lane-appropriate governor account.
	httpExpSvc *experience.Service
	// nntpExpSvc is the same shared experience owner bound to NNTP's reserved
	// readahead pool. NS-5.3 uses it only for persisted-index seek prefetch.
	nntpExpSvc *experience.Service
	// nextEpisodePrewarmed is HR5.4's bounded process-local reservation set.
	// It prevents a completed episode from scheduling duplicate predictive
	// work while leaving the shared experience service as the sole prewarmer.
	nextEpisodeMu        sync.Mutex
	nextEpisodePrewarmed map[string]struct{}
	nextEpisodeOrder     []string
	// httpContinuity validates and records exact progressive-HTTP blocks on
	// both cache hits and misses before they reach a player.
	httpContinuity *contentproof.Ledger
	// httpProofGraph (HR2.7) records authoritative IA/HTTP-Digest-header/
	// Metalink whole-object digests and same-origin ETag observations at
	// HTTP grab-time preflight. Same shared owner/table as httpContinuity
	// above (contentproof.Graph, not a second proof mechanism); nil-safe.
	httpProofGraph *contentproof.Graph
	// representationConvergence records only fully proof-covered native route
	// aliases observed by the shared cross-lane coordinator (RX-2.2).
	representationConvergence *contentproof.Converger
	// httpOriginScores is HR3.3's single bounded, process-local owner of
	// passive HTTP origin/pathway health evidence.
	httpOriginScores *httpstream.OriginScorer
	// torrentCDNScores is TS-2.3's lane-scoped bounded passive CDN-node
	// evidence owner. It deliberately does not share identities with HTTP.
	torrentCDNScores *httpstream.OriginScorer
	// arrRefresh (TS-4.1) is the optional immediate arr-refresh convenience
	// fired after a repair chain's terminal blacklist step. Nil-safe: an
	// unset notifier just skips the convenience refresh.
	arrRefresh ArrRefreshNotifier
	// repairState (TS-4.1) is the bounded in-memory per-infohash repair
	// in-flight/cooldown/attempt-count tracker. No schema/migration is
	// introduced by this row.
	repairState *repairState
	// hlsSessions (HR4.1) is the shared bounded transient playback-session
	// graph: stable opaque DH resource IDs for a manifest-based playback
	// attempt, durably persisted so a resource ID a player already holds
	// still resolves after a restart without a full re-resolve.
	hlsSessions *hlssession.Graph
	// hlsGraphMu makes one playlist's list→mint→prune operation atomic, so
	// concurrent live refreshes cannot mint competing IDs for one sequence.
	hlsGraphMu sync.Mutex
	// sidecars (HR5.5) owns stable, bounded subtitle/chapter/attachment/
	// sidecar resources. Lane adapters register their resources in later rows.
	sidecars *sidecar.Registry
	// relaySem bounds concurrent ffprobe-full invocations from the D-RELAY
	// endpoint (Gate 5) to cfg.Relay.Concurrency; a buffered channel used as
	// a semaphore (send to acquire, receive to release).
	relaySem chan struct{}
	// mediaFlowSem fail-fast bounds generic aggregator playback. It has no
	// waiter queue: a saturated proxy returns immediately instead of retaining
	// an unbounded goroutine per client.
	mediaFlowSem chan struct{}

	// resolveCache is a short-lived per-(item,file) cache of the resolved CDN
	// URL + metadata (B-O1). A cache hit skips CheckCached, RequestDownloadURL,
	// and the followToCDN preflight — the three TorBox calls that fire on every
	// GET /stream before this fix.
	//
	// resolveGroup ensures concurrent GETs for the same (item,file) share one
	// resolve call rather than racing N identical TorBox round-trips (B-N1).
	resolveCache sync.Map           // key: "itemID/fileID" → *resolveCacheEntry
	resolveGroup singleflight.Group // key: "itemID/fileID"

	// B-O4: optional byte-egress meter (set via SetEgressMeter). When non-nil,
	// CDN passthrough bytes and rangecache bytes are credited to the meter.
	egressMeter EgressMeter

	// identityAuditMu (ID-03) serializes RunIdentityAuditCycle invocations
	// so the periodic slow-cadence ticker and the on-demand HTTP trigger
	// can never race the same select-next-item-then-persist operation --
	// the row's own DG-07 coalescing/race requirement, closed with the
	// simplest possible mechanism rather than a second queue/singleflight
	// owner.
	identityAuditMu sync.Mutex

	// decayAuditState (TS-4.3) is the bounded in-memory rotation cursor
	// for the remote-presence decay audit. No schema/migration is
	// introduced by this row (no DG-02).
	decayAuditState *decayAuditState

	// httpSelfHeal (HR3.4) is the bounded in-memory per-item two-pass-
	// reconfirm tracker for HTTP-lane stream-time terminal failures. No
	// schema/migration is introduced by this row (no DG-02).
	httpSelfHeal *httpSelfHealState
	// httpDecayEscalator (HR3.4) is the optional Arr re-search escalation
	// fired after a reconfirmed HTTP-lane decay -- a thin wrapper around
	// NS-6.2's existing triggerReSearchOnDecay, wired post-construction via
	// SetHTTPDecayEscalator. Nil-safe: an unset escalator just skips the
	// convenience escalation.
	httpDecayEscalator HTTPDecayEscalator
}

func (s *Server) SetPlaybackCoverage(tracker *playbackcoverage.Tracker) {
	if s != nil {
		s.playbackCoverage = tracker
	}
}

// EgressMeter is satisfied by *torbox.Metrics. Kept as an interface so
// the api package does not import internal/torbox (avoids import cycle).
type EgressMeter interface {
	AddCDNBytes(n int64)
	AddPassthroughBytes(n int64)
}

type SubmissionRequest struct {
	SourceType  store.SourceType
	ClientKind  store.ClientKind
	Category    string
	DisplayName string
	SourceURI   string
	InfoHash    string
	// ResolveKey is the canonical resolve key JSON for SourceTypeHTTP
	// submissions (D10); empty otherwise. Persisted on the item at
	// creation, before StateResolving, so a crash never strands an
	// unresolvable HTTP item (HS-1.2).
	ResolveKey string
	Metadata   store.SubmissionMetadata
}

// resolveCacheEntry is a short-lived record of a successful CDN resolve for a
// given (item_id, file_id) pair. It lets subsequent GETs skip the three
// TorBox round-trips (CheckCached + RequestDownloadURL + followToCDN) until
// the entry expires (B-O1) or the item is evicted.
type resolveCacheEntry struct {
	cdnURL       string
	contentType  string
	total        int64
	rangeCapable *bool // nil until a bytes=0-0 probe proves or rejects range support
	resolvedAt   time.Time
}

// resolveKey returns the singleflight / sync.Map key for a (item, file) pair.
func resolveKey(itemID, fileID string) string { return itemID + "/" + fileID }

// ResolveKey is the exported form of resolveKey (TS-1.4): callers outside
// this package (cmd/darkharrbor/main.go's grab-time prewarm hook) construct
// a rangecache.ChunkSource Key() using the exact same identity streamViaCache
// uses at play time, so a grab-time pinned chunk is actually reused rather
// than landing under a different cache identity.
func ResolveKey(itemID, fileID string) string { return resolveKey(itemID, fileID) }

// TorrentCDNGovOp (TS-1.4) is the shared accountgov.Governor operation-class
// name for every torrent/debrid-CDN chunk fetch — both live playback
// (streamViaCache, accountgov.PriorityPlayback) and grab-time prewarm
// (cmd/darkharrbor/main.go via NewCDNChunkSourceProbe, PriorityPrewarm).
// A single shared op class is what makes LC-06 priority ordering actually
// arbitrate between the two, rather than each running under its own
// disconnected budget.
const TorrentCDNGovOp = "torrent_cdn_fetch"

// HTTPSourceGovOp is shared by progressive playback, D9 preflight, and
// startup prewarm so LC-06 priority ordering applies to all HTTP source work.
const HTTPSourceGovOp = "http_source_fetch"

// resolveTTL is how long a resolve cache entry is considered fresh.
// TorBox CDN URLs are valid for ~1h; 45 min gives comfortable headroom.
const resolveTTL = 45 * time.Minute

const (
	lastPlayedWriteInterval  = time.Minute
	ephemeralCacheMaxEntries = 10_000
)

func trimInsertionCache[V any](cache map[string]V, order *[]string) int {
	removed := 0
	for len(cache) > ephemeralCacheMaxEntries && len(*order) > 0 {
		key := (*order)[0]
		(*order)[0] = ""
		*order = (*order)[1:]
		if _, ok := cache[key]; ok {
			delete(cache, key)
			removed++
		}
	}
	return removed
}

// resolveEntryFor returns the cached entry for (itemID, fileID) if it exists
// and is younger than resolveTTL; otherwise returns nil.
func (s *Server) resolveEntryFor(itemID, fileID string) *resolveCacheEntry {
	v, ok := s.resolveCache.Load(resolveKey(itemID, fileID))
	if !ok {
		return nil
	}
	e := v.(*resolveCacheEntry)
	if time.Since(e.resolvedAt) > resolveTTL {
		s.resolveCache.Delete(resolveKey(itemID, fileID))
		return nil
	}
	return e
}

// cacheResolveEntry stores a resolve result into the short-lived cache.
func (s *Server) cacheResolveEntry(itemID, fileID, cdnURL, contentType string, total int64, rangeCapable *bool) {
	s.resolveCache.Store(resolveKey(itemID, fileID), &resolveCacheEntry{
		cdnURL:       cdnURL,
		contentType:  contentType,
		total:        total,
		rangeCapable: rangeCapable,
		resolvedAt:   time.Now(),
	})
}

// invalidateResolveEntry removes any cached resolve for (itemID, fileID).
// Called on cache eviction and janitor cleanup so stale CDN URLs are not served.
func (s *Server) invalidateResolveEntry(itemID, fileID string) {
	s.resolveCache.Delete(resolveKey(itemID, fileID))
}

// persistLastPlayedAt writes at most once per item per minute. Reserving the
// timestamp before the DB call keeps concurrent playback requests to one
// write; a failed write rolls the reservation back so the next request retries.
func (s *Server) persistLastPlayedAt(ctx context.Context, item *store.Item) error {
	now := time.Now().UTC()

	s.lastPlayedMu.Lock()
	if s.lastPlayedWrites == nil {
		s.lastPlayedWrites = make(map[string]time.Time)
	}
	previous := s.lastPlayedWrites[item.ID]
	if item.LastPlayedAt != nil && item.LastPlayedAt.After(previous) {
		previous = *item.LastPlayedAt
	}
	if !previous.IsZero() && now.Before(previous.Add(lastPlayedWriteInterval)) {
		s.lastPlayedWrites[item.ID] = previous
		s.lastPlayedMu.Unlock()
		return nil
	}
	s.lastPlayedWrites[item.ID] = now
	s.lastPlayedMu.Unlock()

	if err := s.store.UpdateLastPlayedAt(ctx, item.ID, item.State, now); err != nil {
		s.lastPlayedMu.Lock()
		if current, ok := s.lastPlayedWrites[item.ID]; ok && current.Equal(now) {
			if previous.IsZero() {
				delete(s.lastPlayedWrites, item.ID)
			} else {
				s.lastPlayedWrites[item.ID] = previous
			}
		}
		s.lastPlayedMu.Unlock()
		return err
	}
	if s.log != nil {
		s.log.Info("stream: last_played_at persisted", "item_id", item.ID, "last_played_at", now)
	}
	return nil
}

// SweepCaches bounds the two insertion-ordered relay caches, removes expired
// CDN resolves, and forgets old last-play write-throttle entries.
func (s *Server) SweepCaches() int {
	removed := 0
	now := time.Now()

	s.magnetCacheMu.Lock()
	removed += trimInsertionCache(s.magnetCache, &s.magnetCacheOrder)
	s.magnetCacheMu.Unlock()

	s.nzbProxyMu.Lock()
	removed += trimInsertionCache(s.nzbProxyCache, &s.nzbProxyCacheOrder)
	s.nzbProxyMu.Unlock()

	s.resolveCache.Range(func(key, value any) bool {
		entry, ok := value.(*resolveCacheEntry)
		if ok && now.Sub(entry.resolvedAt) > resolveTTL {
			s.resolveCache.Delete(key)
			removed++
		}
		return true
	})

	s.lastPlayedMu.Lock()
	for itemID, persistedAt := range s.lastPlayedWrites {
		if now.Sub(persistedAt) > 2*lastPlayedWriteInterval {
			delete(s.lastPlayedWrites, itemID)
			removed++
		}
	}
	s.lastPlayedMu.Unlock()

	return removed
}

// fileListEntry is the JSON shape stored in items.file_list.
// B-N3: file_id, name, size only — requestdl_url stripped at write time and
// scrubbed from existing rows by the B-N3 maintenance function at startup.
// FileID may be empty on legacy rows written before B-N3; callers that need
// it (handleStreamProxy) fall back to the positional fileID path param.
type fileListEntry struct {
	FileID string `json:"file_id,omitempty"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
}

// parseFileList decodes items.file_list into a slice of fileListEntry.
// Returns nil on any error (callers fall back to provider path).
func parseFileList(raw *string) []fileListEntry {
	if raw == nil || *raw == "" {
		return nil
	}
	var entries []fileListEntry
	if err := json.Unmarshal([]byte(*raw), &entries); err != nil {
		return nil
	}
	return entries
}

// fileListEntryForID resolves the file_list entry matching fileID, the
// provider's own per-file identifier embedded in .strm URLs by
// output.Write() -- NOT guaranteed to equal its position in file_list.
//
// [Found live 2026-07-27 during TS-0.3's CG-02 gate, fixed as a drive-by
// same-session commit per Dewayne's direction rather than risk losing it.]
// Two call sites here previously did entries[Atoi(fileID)] directly,
// silently returning the WRONG file's size/content-type whenever a
// provider's FileID string didn't equal its position in file_list -- real
// and reproducible: TorBox returned this real item's files in the order
// [FileID "2", FileID "0", FileID "1"], so a request for FileID "2" (the
// actual video) landed on entries[2], which was FileID "1" (a jpg). GET/
// range playback was unaffected (RequestDownloadURL is keyed by the real
// FileID string, never by array position), but the HEAD shortcut and the
// resolve-time total/content-type hint both served the wrong file's
// metadata for exactly this shape.
//
// Primary lookup is now an exact FileID match. The positional-index path
// remains, but only as a fallback for legacy rows written before B-N3
// started populating FileID at all (fileListEntry's own doc comment) --
// preserving that pre-existing, intentional backward-compat behavior
// rather than changing it.
func fileListEntryForID(entries []fileListEntry, fileID string) (fileListEntry, bool) {
	for _, e := range entries {
		if e.FileID != "" && e.FileID == fileID {
			return e, true
		}
	}
	if fileIdx, idxErr := strconv.Atoi(fileID); idxErr == nil && fileIdx >= 0 && fileIdx < len(entries) {
		if entries[fileIdx].FileID == "" {
			return entries[fileIdx], true
		}
	}
	if len(entries) == 1 {
		return entries[0], true
	}
	return fileListEntry{}, false
}

// fileExtContentType returns a best-effort Content-Type for a filename
// extension. Falls back to "video/x-matroska" for unknown video extensions.
func fileExtContentType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".avi":
		return "video/x-msvideo"
	case ".mov":
		return "video/quicktime"
	case ".ts":
		return "video/mp2t"
	case ".webm":
		return "video/webm"
	default:
		return "video/x-matroska"
	}
}

func NewServerWithContext(parent context.Context, cfg *config.Config, log *slog.Logger, st *store.Store, qbitAuth *auth.QBitSessionManager, sabAuth *auth.SABAuth, prov provider.Provider, allProviders map[string]provider.Provider, providerOrder []string, gate *cachegate.Gate, lanes []ProviderLane, writer output.Writer, usenetProviders map[string]provider.UsenetProvider, usenetOrder []string, streamCache *rangecache.Cache, torrentGov *accountgov.Governor, experienceSvc *experience.Service, httpGov *accountgov.Governor, httpExpSvc *experience.Service) *Server {
	if parent == nil {
		parent = context.Background()
	}
	shutdownCtx, shutdownCancel := context.WithCancel(parent)
	var prowlarrClient *prowlarr.Client
	if cfg.SelectionMode() {
		prowlarrClient = prowlarr.New(cfg.Prowlarr.BaseURL, cfg.Prowlarr.APIKey, cfg.TorBox.UserAgent, 90*time.Second, log)
		log.Info("selection mode enabled (cache-aware meta-indexer over Prowlarr)")
	}
	srv := &Server{
		cfg:                  cfg,
		log:                  log,
		store:                st,
		qbitAuth:             qbitAuth,
		sabAuth:              sabAuth,
		startedAt:            time.Now().UTC(),
		shutdownCtx:          shutdownCtx,
		shutdownCancel:       shutdownCancel,
		rx95:                 newRX95Store(),
		rx93CaptureSem:       make(chan struct{}, rx93MaxConcurrent),
		rx93Selectors:        newRX93SelectorStore(),
		janitorTimers:        make(map[string]context.CancelFunc),
		lastPlayedWrites:     make(map[string]time.Time),
		prov:                 prov,
		allProviders:         allProviders,
		providerOrder:        providerOrder,
		gate:                 gate,
		lanes:                lanes,
		writer:               writer,
		activeGoroutines:     make(map[string]struct{}),
		torznabCache:         make(map[string]torznabCacheEntry),
		magnetCache:          make(map[string]magnetEntry),
		prowlarr:             prowlarrClient,
		nzbProxyCache:        make(map[string]string),
		usenetProviders:      usenetProviders,
		usenetOrder:          usenetOrder,
		streamCache:          streamCache,
		torrentGov:           torrentGov,
		experienceSvc:        experienceSvc,
		httpGov:              httpGov,
		httpExpSvc:           httpExpSvc,
		nextEpisodePrewarmed: make(map[string]struct{}),
		relaySem:             make(chan struct{}, cfg.Relay.Concurrency),
		mediaFlowSem:         make(chan struct{}, mediaFlowMaxConcurrent),
		httpResolveCoord:     newHTTPResolveCoordinator(),
		publicIPResolver:     publicip.New(shutdownCtx, publicip.Options{}),
		arrMetadataClient: &http.Client{
			Timeout: 8 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	srv.httpOriginScores = httpstream.NewOriginScorer(srv.nowUTC)
	srv.torrentCDNScores = httpstream.NewOriginScorer(srv.nowUTC)
	srv.repairState = newRepairState()
	srv.decayAuditState = newDecayAuditState()
	srv.httpSelfHeal = newHTTPSelfHealState(srv.nowUTC)
	budget, err := searchbudget.New(st, searchbudget.Options{
		Cap: cfg.SearchBudget.Cap, Suppress: time.Duration(cfg.SearchBudget.SuppressDays) * 24 * time.Hour,
		Now: srv.nowUTC,
	})
	if err != nil {
		log.Error("search budget unavailable", "error", err)
	} else {
		srv.searchBudget = budget
	}
	// The store satisfies both continuity and proof repositories and the
	// program clock remains injectable through srv.nowUTC.
	ledger, err := contentproof.NewLedger(st, contentproof.Options{Now: srv.nowUTC})
	if err != nil {
		log.Error("http continuity ledger unavailable", "error", err)
	} else {
		srv.httpContinuity = ledger
	}
	// HR2.7: same store, same clock, same content_proofs table as the
	// continuity ledger above -- Graph and Ledger are two existing owners
	// over one shared repository, not a second cache/store.
	proofGraph, err := contentproof.New(st, contentproof.Options{Now: srv.nowUTC})
	if err != nil {
		log.Error("http content proof graph unavailable", "error", err)
	} else {
		srv.httpProofGraph = proofGraph
		if torrentGov != nil {
			torrentGov.SetCapacity(crosslane.Operation, 1)
		}
		if cfg.Reactive.PromotionEnabled {
			if recoverErr := st.RecoverReactivePromotions(parent, srv.nowUTC()); recoverErr != nil {
				log.Error("reactive promotion recovery unavailable", "error", recoverErr)
			}
			promotion, promotionErr := reactivepromotion.New(st, streamCache, srv, reactivecommit.NewArrClient(cfg.Arrs), proofGraph, reactivepromotion.Options{
				Root: cfg.Data.Root, FloorPercent: cfg.Reactive.PromotionFreePercent, Now: srv.nowUTC,
			})
			if promotionErr != nil {
				log.Error("reactive promotion unavailable", "error", promotionErr)
			} else {
				srv.promotion = promotion
			}
		}
	}
	convergence, err := contentproof.NewConverger(st, contentproof.Options{Now: srv.nowUTC})
	if err != nil {
		log.Error("representation convergence unavailable", "error", err)
	} else {
		srv.representationConvergence = convergence
	}
	// HR4.1: the store satisfies hlssession.Repository directly, the same
	// pattern used for the continuity ledger above.
	graph, err := hlssession.New(st, hlssession.Options{Now: srv.nowUTC})
	if err != nil {
		log.Error("hls session graph unavailable", "error", err)
	} else {
		srv.hlsSessions = graph
	}
	sidecars, err := sidecar.New(st, sidecar.Options{Now: srv.nowUTC})
	if err != nil {
		log.Error("sidecar registry unavailable", "error", err)
	} else {
		srv.sidecars = sidecars
	}
	return srv
}

// SetNNTPExperience attaches the NNTP consumer after construction without
// widening the already-large server constructor. Call once before serving.
func (s *Server) SetNNTPExperience(svc *experience.Service) {
	s.nntpExpSvc = svc
}

// SetDoctorDockerProxyURL wires the docker-proxy URL ONB-02's doctor
// endpoint needs for its Jellyfin-visibility check, without widening the
// already-large server constructor. Empty (the default) skips that one
// check (StatusSkip), matching doctor.Options' own convention.
func (s *Server) SetDoctorDockerProxyURL(url string) {
	s.doctorDockerProxyURL = url
}

type encryptedRARStreamProvider interface {
	StreamEncryptedRARManifest(context.Context, string, int64, http.ResponseWriter, string, string, string, string) error
}

func nntpRARPassword(item *store.Item) string {
	if item == nil {
		return ""
	}
	if item.SourceURI != nil {
		if parsed, err := nntp.ParseNZB([]byte(*item.SourceURI)); err == nil && parsed.Password != "" {
			return parsed.Password
		}
	}
	return item.Metadata.Password
}

type nntpSeekSourceProvider interface {
	SeekFileSource(itemID string, nzbData []byte, fileIndex int) rangecache.ChunkSource
	SeekRARSource(ctx context.Context, itemID, contentKey, manifestJSON string) rangecache.ChunkSource
	SeekZIPSource(ctx context.Context, itemID, contentKey, manifestJSON string, entryIdx int) rangecache.ChunkSource
}

func seekTargetForRange(rangeHeader string, index *mediatruth.Index) (int64, bool) {
	if index == nil || len(index.Entries) == 0 || !strings.HasPrefix(rangeHeader, "bytes=") {
		return 0, false
	}
	spec := strings.TrimPrefix(rangeHeader, "bytes=")
	if comma := strings.IndexByte(spec, ','); comma >= 0 {
		return 0, false
	}
	dash := strings.IndexByte(spec, '-')
	if dash <= 0 {
		return 0, false
	}
	offset, err := strconv.ParseInt(spec[:dash], 10, 64)
	if err != nil || offset <= 0 {
		return 0, false
	}
	n := sort.Search(len(index.Entries), func(i int) bool {
		return index.Entries[i].Offset > offset
	})
	if n == 0 {
		return 0, false
	}
	return index.Entries[n-1].TimeMS, true
}

func (s *Server) prefetchNNTPSeek(ctx context.Context, item *store.Item, prov provider.UsenetProvider, fileIndex int, contentKey, rangeHeader string) {
	if s.nntpExpSvc == nil || item == nil || item.Metadata.NNTPSeekIndex == nil {
		return
	}
	targetMS, ok := seekTargetForRange(rangeHeader, item.Metadata.NNTPSeekIndex)
	if !ok {
		return
	}
	sourceProvider, ok := prov.(nntpSeekSourceProvider)
	if !ok {
		return
	}
	var src rangecache.ChunkSource
	switch {
	case item.IsRARSplit():
		manifest, err := nntp.UnmarshalRARManifest(*item.RarManifest)
		if err != nil || len(manifest) == 0 || manifest[0].Encryption != nil {
			return
		}
		src = sourceProvider.SeekRARSource(ctx, item.ID, contentKey, *item.RarManifest)
	case item.IsZIPArchive():
		src = sourceProvider.SeekZIPSource(ctx, item.ID, contentKey, *item.ZIPManifest, fileIndex)
	case item.SourceURI != nil:
		src = sourceProvider.SeekFileSource(item.ID, []byte(*item.SourceURI), fileIndex)
	}
	if src == nil || src.Key() != item.Metadata.NNTPSeekSourceKey {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.nntpExpSvc.SeekTargetPrefetch(ctx, src, *item.Metadata.NNTPSeekIndex, targetMS); err == nil {
			if s.log != nil {
				s.log.Debug("nntp: seek-target prefetch complete", "item_id", item.ID, "target_ms", targetMS)
			}
		}
	}()
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.shutdownCancel()
	if s.publicIPResolver != nil {
		s.publicIPResolver.Close()
	}
	s.janitorMu.Lock()
	for itemID, cancel := range s.janitorTimers {
		cancel()
		delete(s.janitorTimers, itemID)
	}
	s.janitorMu.Unlock()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) Router() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/healthz", getOnly(http.HandlerFunc(s.handleHealth)))

	mux.Handle("/api/v2/auth/login", postOnly(http.HandlerFunc(s.handleQBitLogin)))
	mux.Handle("/api/v2/auth/logout", postOnly(http.HandlerFunc(s.handleQBitLogout)))
	// version/webapiVersion are pre-auth probes in the real qBittorrent WebUI
	// API (clients call them to check compatibility before logging in); they
	// must stay unauthenticated to match that contract, matching handlers
	// that only ever echo the fixed, non-sensitive compatibility strings.
	mux.Handle("/api/v2/app/version", getOnly(http.HandlerFunc(s.handleQBitVersion)))
	mux.Handle("/api/v2/app/webapiVersion", getOnly(http.HandlerFunc(s.handleQBitWebAPIVersion)))
	mux.Handle("/api/v2/app/preferences", getOnly(s.qbitProtected(http.HandlerFunc(s.handleQBitPreferences))))
	mux.Handle("/api/v2/app/defaultSavePath", getOnly(s.qbitProtected(http.HandlerFunc(s.handleQBitDefaultSavePath))))
	mux.HandleFunc("/download/", s.handleDownload)
	mux.Handle("/api/v2/torrents/add", postOnly(s.qbitProtected(http.HandlerFunc(s.handleQBitAdd))))
	mux.Handle("/api/v2/torrents/info", getOnly(s.qbitProtected(http.HandlerFunc(s.handleQBitInfo))))
	mux.Handle("/api/v2/torrents/delete", postOnly(s.qbitProtected(http.HandlerFunc(s.handleQBitDelete))))
	mux.Handle("/api/v2/torrents/categories", getOnly(s.qbitProtected(http.HandlerFunc(s.handleQBitCategories))))
	mux.Handle("/api/v2/torrents/createCategory", postOnly(s.qbitProtected(http.HandlerFunc(s.handleQBitCreateCategory))))
	mux.Handle("/api/v2/transfer/info", getOnly(s.qbitProtected(http.HandlerFunc(s.handleQBitTransferInfo))))

	mux.Handle("/stream/{item_id}/{file_id}", http.HandlerFunc(s.handleStreamProxy))

	s.registerTorznab(mux)
	s.registerHTTPStream(mux)
	s.registerHLS(mux)
	s.registerSidecars(mux)
	s.registerWebDAV(mux)
	s.registerRelay(mux)
	s.registerMediaFlow(mux)

	mux.Handle("/api/v1/identity-audit/run", postOnly(http.HandlerFunc(s.handleIdentityAuditRun)))
	mux.Handle("/api/v1/identity-audit/findings", getOnly(http.HandlerFunc(s.handleIdentityAuditFindings)))
	mux.Handle("/api/v1/reactive/promote", postOnly(s.qbitProtected(http.HandlerFunc(s.handleReactivePromote))))

	// OBS-01: Prometheus metrics plus the per-item error-journal exposure.
	// Metrics.Enabled gates route registration only (default true); there
	// is no separate bind-address surface since it shares this listener.
	if s.cfg == nil || s.cfg.Metrics.Enabled {
		mux.Handle("/metrics", getOnly(http.HandlerFunc(s.handleMetrics)))
	}
	mux.Handle("/api/v1/error-journal/{item_id}", getOnly(http.HandlerFunc(s.handleErrorJournal)))

	// OBS-02: ladder dry-run "explain this item" debug endpoint. Read-only;
	// never moves media bytes or contacts a provider.
	mux.Handle("/api/v1/debug/resolve/{item_id}", getOnly(http.HandlerFunc(s.handleDebugResolve)))
	mux.Handle("/api/v1/doctor", getOnly(http.HandlerFunc(s.handleDoctor)))

	mux.HandleFunc("/api", s.handleSABAPI)
	mux.HandleFunc("/sabnzbd/api", s.handleSABAPI)
	mux.HandleFunc("/config/categories", s.handleSABConfigCategories)
	mux.HandleFunc("/config/categories/", s.handleSABConfigCategories)

	return withLogging(s.log, mux)
}

func (s *Server) qbitProtected(next http.Handler) http.Handler {
	return s.qbitAuth.Middleware(next)
}

// SetEgressMeter wires a B-O4 egress byte counter into the server.
// Call once after NewServerWithContext, before serving requests.
func (s *Server) SetEgressMeter(m EgressMeter) { s.egressMeter = m }

// countingWriter wraps an http.ResponseWriter and counts bytes written (B-O4).
type countingWriter struct {
	http.ResponseWriter
	n int64
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	n, err := cw.ResponseWriter.Write(p)
	cw.n += int64(n)
	return n, err
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	uptime := int64(time.Since(s.startedAt).Seconds())
	body := map[string]any{
		"ok":             true,
		"status":         "ok",
		"version":        "v1",
		"uptime_seconds": uptime,
		"time":           time.Now().UTC().Format(time.RFC3339),
	}
	if s.httpHandlers != nil && !s.httpHandlers.Empty() {
		body["http_backends"] = s.httpHandlers.Statuses()
	}
	// B-O4: include live metering snapshot when available.
	if s.egressMeter != nil {
		if snap, ok := s.egressMeter.(interface {
			Snapshot() interface{ AsMap() map[string]any }
		}); ok {
			_ = snap
		}
		// Type-assert to the concrete snapshot type via a minimal interface.
		type snapshotter interface{ SnapshotMap() map[string]any }
		if sn, ok := s.egressMeter.(snapshotter); ok {
			body["metering"] = sn.SnapshotMap()
		}
	}
	if s.streamCache != nil {
		body["rangecache"] = s.streamCache.SnapshotMap()
	}
	if s.httpOriginScores != nil {
		body["http_origin_score_entries"] = s.httpOriginScores.Len()
		body["http_origin_sources"] = s.httpOriginScores.Snapshot()
	}
	if s.torrentCDNScores != nil {
		body["torrent_cdn_score_entries"] = s.torrentCDNScores.Len()
		body["torrent_cdn_sources"] = s.torrentCDNScores.Snapshot()
	}
	governorDenials := map[string][]accountgov.DenialSnapshot{}
	if s.torrentGov != nil {
		governorDenials["torrent"] = s.torrentGov.Denials(TorrentCDNGovOp)
	}
	if s.httpGov != nil {
		governorDenials["http"] = s.httpGov.Denials(HTTPSourceGovOp)
	}
	if len(governorDenials) > 0 {
		body["governor_denials"] = governorDenials
	}
	if s.store != nil {
		// OBS-01: bounded aggregate only (no per-item content), mirroring
		// the two exposures immediately above.
		if agg, err := s.store.ErrorJournalAggregateStats(r.Context()); err == nil {
			body["error_journal_entries"] = agg.Total
		}
	}
	if warm := s.nntpWarmPoolSnapshot(); len(warm) > 0 {
		// NS-5.1: bounded per-provider warm-pool state (a handful of
		// small ints per configured provider) -- no URL, no message-ID,
		// no credential, matching every other /healthz aggregate above.
		body["nntp_warm_pool"] = warm
	}
	writeJSON(w, http.StatusOK, body)
}

// nntpWarmPoolSnapshot returns NS-5.1's bounded per-provider warm-pool
// state for every configured usenet provider that resolves to a concrete
// *nntp.NNTPProvider with an attached SegmentCache. Shared by /healthz and
// /metrics so both surfaces agree by construction. A provider with no
// SegmentCache attached (should not happen once wired, but defensive) or a
// provider type this build doesn't recognize is simply omitted rather than
// guessed at.
func (s *Server) nntpWarmPoolSnapshot() map[string]map[string]int64 {
	if len(s.usenetProviders) == 0 {
		return nil
	}
	out := make(map[string]map[string]int64, len(s.usenetProviders))
	for name, up := range s.usenetProviders {
		np, ok := up.(*nntp.NNTPProvider)
		if !ok || np == nil {
			continue
		}
		cache := np.Cache()
		if cache == nil {
			continue
		}
		open, target, cold, warm := cache.WarmSnapshot()
		out[name] = map[string]int64{
			"open":   int64(open),
			"target": int64(target),
			"cold":   cold,
			"warm":   warm,
		}
	}
	return out
}

func (s *Server) handleQBitLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Fails.", http.StatusBadRequest)
		return
	}
	sid, err := s.qbitAuth.Login(r.Context(), r.PostForm.Get("username"), r.PostForm.Get("password"))
	if err != nil {
		s.log.Warn("qbit login failed", "remote_addr", r.RemoteAddr, "error", err.Error())
		http.Error(w, "Fails.", http.StatusOK)
		return
	}
	auth.SetQBitCookie(w, sid, qbitCookieSecure(s.cfg.Server.BaseURL))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("Ok."))
}

func (s *Server) handleQBitLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(auth.QBitSessionCookie); err == nil {
		_ = s.qbitAuth.Logout(r.Context(), cookie.Value)
	}
	auth.ClearQBitCookie(w, qbitCookieSecure(s.cfg.Server.BaseURL))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("Ok."))
}

func (s *Server) handleQBitVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(s.cfg.Compatibility.QBitVersion))
}

func (s *Server) handleQBitWebAPIVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(s.cfg.Compatibility.QBitWebAPI))
}

func (s *Server) handleQBitPreferences(w http.ResponseWriter, r *http.Request) {
	// Return the host-visible path (reportedPathPrefix) not the container-internal
	// root. Sonarr uses this to verify category subdirectories exist; it checks
	// from its own perspective where it sees /mnt/darkharrbor, not /data.
	writeJSON(w, http.StatusOK, map[string]any{
		"save_path":            s.cfg.Data.ReportedPathPrefix,
		"temp_path_enabled":    false,
		"start_paused_enabled": false,
		// dht=true tells Sonarr/Radarr that tracker-less magnet links are
		// acceptable. Dark Harrbor resolves torrents by infohash via TorBox
		// (not by real DHT peer discovery), so this is semantically correct.
		"dht":       true,
		"dht_nodes": 0,
	})
}

func (s *Server) handleQBitDefaultSavePath(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(s.cfg.Data.ReportedPathPrefix))
}

func (s *Server) handleQBitCategories(w http.ResponseWriter, r *http.Request) {
	payload, err := s.qbitCategories(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleQBitCreateCategory(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	category := strings.TrimSpace(r.PostForm.Get("category"))
	if err := validateCategory(category); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	candidate := strings.TrimSpace(r.PostForm.Get("savePath"))
	if candidate == "" {
		candidate = category
	}
	// PATCH-13 / G1: contain the attacker-controlled save path under the data
	// (Strm) root; a traversal or out-of-root absolute path is rejected, never
	// created. The resolved, contained path is what we MkdirAll.
	savePath, err := resolveUnderRoot(s.cfg.Data.Strm, candidate)
	if err != nil {
		http.Error(w, "savePath escapes data root", http.StatusBadRequest)
		return
	}
	if err := os.MkdirAll(savePath, 0o750); err != nil { // #nosec G703 -- savePath comes from resolveUnderRoot, which rejects traversal and out-of-root absolute paths.
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleQBitTransferInfo(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListVisibleClientItems(r.Context(), store.ClientKindQBit, "", 1000)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, compat.ProjectQBitTransferInfo(items))
}

func (s *Server) handleQBitAdd(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		if err2 := r.ParseForm(); err2 != nil {
			http.Error(w, err2.Error(), http.StatusBadRequest)
			return
		}
	}

	category := strings.TrimSpace(r.PostFormValue("category"))
	rename := strings.TrimSpace(r.PostFormValue("rename"))
	urlsRaw := strings.TrimSpace(r.PostFormValue("urls"))
	created := 0

	metadata := store.SubmissionMetadata{
		SavePath:     r.PostFormValue("savepath"),
		Rename:       rename,
		Tags:         splitCSV(r.PostFormValue("tags")),
		SkipChecking: parseBool(r.PostFormValue("skip_checking")),
		Paused:       parseBool(r.PostFormValue("paused")),
		RootFolder:   parseBool(r.PostFormValue("root_folder")),
	}

	// TS-0.1 (T1): the qBittorrent Web API's own multipart field name for an
	// uploaded .torrent file is "torrents" -- an arr uses this when an
	// indexer supplied only a torrent-file download link, never a magnet.
	// Previously unhandled entirely (created=0, "Fails."); this branch
	// parses/persists TorrentMeta and submits the same way the urls loop
	// below does, via a synthetic infohash magnet.
	for _, header := range multipartFiles(r.MultipartForm, "torrents") {
		if s.submitUploadedTorrentFile(r.Context(), header, category, rename, metadata) {
			created++
		}
	}

	if urlsRaw != "" {
		for _, line := range strings.FieldsFunc(urlsRaw, func(r rune) bool { return r == '\n' || r == '\r' }) {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			lineMetadata := metadata
			// HTTP stream marker magnets (from /grab/http) carry a signed
			// grab envelope in x.dh — route to the HTTP lane; never the
			// torrent/debrid path (D1). A rejected token is a failed
			// submission, never a retry.
			if tok := httpGrabTokenFromMagnet(line); tok != "" {
				if err := s.enqueueHTTPGrab(r, line, category, lineMetadata); err == nil {
					created++
				} else {
					s.log.Warn("qbit http-grab submission failed", "error", httpstream.Sanitize(err))
				}
				continue
			}
			infoHash := extractInfoHash(line)
			if identityContext := magnetProviderIdentityContext(line); identityContext != "" {
				s.attachGrabProviderIdentity(r.Context(), "torrent-context:"+identityContext, &lineMetadata)
			} else {
				s.attachGrabProviderIdentity(r.Context(), "torrent:"+strings.ToLower(infoHash), &lineMetadata)
			}
			// Prefer the magnet's dn (display name) over the bare infohash:
			// Sonarr's parser rejects downloads named by hash ("Rejected
			// Hashed Release Title"), flags the tracked download unknown, and
			// auto-removes it from the client minutes after grab — every
			// hash-named uncached item was silently deleted mid-download
			// (live 2026-07-08, "queued then went away").
			displayName := firstNonEmpty(rename, magnetDisplayName(line), s.magnetCacheTitle(r.Context(), infoHash), infoHash, line)
			if _, _, err := s.enqueueSubmission(r.Context(), SubmissionRequest{
				SourceType:  store.SourceTypeTorrent,
				ClientKind:  store.ClientKindQBit,
				Category:    category,
				DisplayName: displayName,
				SourceURI:   line,
				InfoHash:    infoHash,
				Metadata:    lineMetadata,
			}); err == nil {
				created++
			} else {
				s.log.Warn("qbit torrent submission failed",
					"event", "torrent_enqueue_failed",
					"source_type", store.SourceTypeTorrent,
					"info_hash", infoHash,
					"display_name", displayName,
					"category", category,
					"error", err.Error(),
				)
			}
		}
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if created == 0 {
		_, _ = w.Write([]byte("Fails."))
		return
	}
	_, _ = w.Write([]byte("Ok."))
}

// submitUploadedTorrentFile handles one multipart "torrents" file upload
// (TS-0.1/T1): parse bencode, persist compact TorrentMeta, then submit via
// the same enqueueSubmission path the magnet/urls loop uses, using a
// synthetic infohash magnet as SourceURI (providers key uncached/cached
// lookups by infohash; a real tracker list is not required for TorBox/RD).
// Any failure (read, parse, or submit) is logged and non-fatal to the
// overall request -- one bad upload among several must not fail the whole
// /api/v2/torrents/add call.
func (s *Server) submitUploadedTorrentFile(ctx context.Context, header *multipart.FileHeader, category, rename string, metadata store.SubmissionMetadata) bool {
	f, err := header.Open()
	if err != nil {
		s.log.Warn("qbit torrent-file submission failed: open",
			"event", "torrent_enqueue_failed", "source_type", store.SourceTypeTorrent,
			"filename", header.Filename, "error", err)
		return false
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, 32<<20))
	if err != nil {
		s.log.Warn("qbit torrent-file submission failed: read",
			"event", "torrent_enqueue_failed", "source_type", store.SourceTypeTorrent,
			"filename", header.Filename, "error", err)
		return false
	}

	meta, infoHash, displayName, magnet, err := resolveTorrentFileSubmission(data, rename)
	if err != nil {
		// Malformed/unsupported .torrent upload, or a pure v2-only torrent
		// with no v1 infohash to submit under: non-fatal per T1, logged
		// without the raw bytes (DG-03/DG-04 -- never log untrusted upload
		// content verbatim).
		s.log.Warn("qbit torrent-file submission declined",
			"event", "torrent_enqueue_failed", "source_type", store.SourceTypeTorrent,
			"filename", header.Filename, "category", category, "error", err)
		return false
	}
	s.attachGrabProviderIdentity(ctx, "torrent:"+strings.ToLower(infoHash), &metadata)

	if perr := s.store.UpsertTorrentMeta(ctx, meta); perr != nil {
		// Persistence failure must not block a real, already-parsed grab --
		// TorrentMeta is a verification/naming aid for later rows, not a
		// prerequisite for submission itself.
		s.log.Warn("qbit torrent-file torrentmeta persist failed (continuing with submission)",
			"event", "torrent_meta_persist_failed", "info_hash", infoHash, "error", perr)
	}

	if _, _, err := s.enqueueSubmission(ctx, SubmissionRequest{
		SourceType:  store.SourceTypeTorrent,
		ClientKind:  store.ClientKindQBit,
		Category:    category,
		DisplayName: displayName,
		SourceURI:   magnet,
		InfoHash:    infoHash,
		Metadata:    metadata,
	}); err != nil {
		s.log.Warn("qbit torrent-file submission failed",
			"event", "torrent_enqueue_failed", "source_type", store.SourceTypeTorrent,
			"info_hash", infoHash, "display_name", displayName, "category", category, "error", err)
		return false
	}
	return true
}

// resolveTorrentFileSubmission parses an arr-uploaded .torrent file and
// derives everything submitUploadedTorrentFile needs to hand off to
// enqueueSubmission. It is pure (no store, no network, no Server) so the
// parse/hash/naming logic is independently unit-testable without the
// cachegate/governor/background-resolve machinery enqueueSubmission itself
// requires.
func resolveTorrentFileSubmission(data []byte, rename string) (meta *torrentmeta.TorrentMeta, infoHash, displayName, magnet string, err error) {
	meta, err = torrentmeta.Parse(data)
	if err != nil {
		return nil, "", "", "", err
	}

	infoHash = meta.InfoHashV1
	if infoHash == "" {
		// Pure v2-only torrent (no v1 "pieces" field): no consumer in this
		// codebase can submit or track a bare v2 infohash today (item.InfoHash
		// and every provider adapter assume v1 BTIH). The caller still
		// persists TorrentMeta for a future v2-primary path; the grab itself
		// is declined rather than submitted under a fabricated identity.
		return meta, "", "", "", fmt.Errorf("torrentmeta: pure v2-only torrent has no v1 infohash; submission not supported")
	}

	// The .torrent file's own "name" field is the verified real release
	// name (harvested from TorrentMeta, not attacker/indexer-supplied magnet
	// dn text) -- prefer it over the bare infohash the same way the magnet
	// path prefers a magnet's dn param. deriveDisplayName's own priority
	// chain assumes a magnet sourceURI, which does not exist here, so the
	// degenerate-name recovery (avoid Sonarr's "Rejected Hashed Release
	// Title") is done directly against meta.Name instead.
	displayName = strings.TrimSpace(rename)
	if displayName == "" || strings.EqualFold(displayName, infoHash) {
		displayName = firstNonEmpty(meta.Name, infoHash)
	}
	magnet = "magnet:?xt=urn:btih:" + infoHash + "&dn=" + url.QueryEscape(displayName)
	return meta, infoHash, displayName, magnet, nil
}

func (s *Server) handleQBitInfo(w http.ResponseWriter, r *http.Request) {
	category := strings.TrimSpace(r.URL.Query().Get("category"))
	items, err := s.store.ListVisibleClientItems(r.Context(), store.ClientKindQBit, category, 1000)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	hashes := splitPipe(r.URL.Query().Get("hashes"))
	items = filterItemsByHashes(items, hashes)
	sort.SliceStable(items, func(i, j int) bool { return items[i].CreatedAt.Before(items[j].CreatedAt) })

	payload := make([]compat.QBitTorrentInfo, 0, len(items))
	failedCount := 0
	for _, item := range items {
		payload = append(payload, compat.ProjectQBitTorrent(item, s.cfg.Data.ReportedPathPrefix, s.cfg.Data.Root))
		if item.SourceType == store.SourceTypeTorrent && item.State == store.StateFailed {
			failedCount++
		}
	}
	if failedCount > 0 {
		s.log.Info("qbit: failed torrent history exposed",
			"event", "qbit_failed_history_exposed",
			"source_type", store.SourceTypeTorrent,
			"count", failedCount,
		)
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleQBitDelete(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	hashes := splitPipe(r.PostForm.Get("hashes"))
	s.log.Info("qbit delete requested", "hashes", hashes, "remote_addr", r.RemoteAddr)
	for _, hash := range hashes {
		if err := s.markRemoved(r.Context(), hash); err != nil {
			s.log.Warn("qbit delete failed", "hash", hash, "error", err)
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleSABAPI(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	mode := strings.ToLower(strings.TrimSpace(q.Get("mode")))
	apiKey := firstNonEmpty(q.Get("apikey"), q.Get("nzbkey"))

	if mode == "" || apiKey == "" {
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			_ = r.ParseForm()
			if mode == "" {
				mode = strings.ToLower(strings.TrimSpace(r.FormValue("mode")))
			}
			if apiKey == "" {
				apiKey = firstNonEmpty(r.FormValue("apikey"), r.FormValue("nzbkey"))
			}
		}
	}

	if !s.sabAuth.Allow(mode, apiKey) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "API Key Incorrect"})
		return
	}

	switch mode {
	case "addurl", "addfile", "queue", "history", "set_config":
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			if err := r.ParseMultipartForm(2 << 20); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid multipart body"})
				return
			}
		} else if err := r.ParseForm(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid form body"})
			return
		}
	}

	switch mode {
	case "version":
		writeJSON(w, http.StatusOK, map[string]string{"version": s.cfg.Compatibility.SABVersion})
	case "auth":
		writeJSON(w, http.StatusOK, map[string]bool{"auth": true})
	case "get_config":
		s.handleSABGetConfig(w, r)
	case "get_cats":
		s.handleSABGetCats(w, r)
	case "get_scripts":
		writeJSON(w, http.StatusOK, map[string]any{"scripts": []string{"None"}})
	case "set_config":
		s.handleSABSetConfig(w, r)
	case "addurl":
		s.handleSABAddURL(w, r)
	case "addfile":
		s.handleSABAddFile(w, r)
	case "queue":
		name := strings.ToLower(strings.TrimSpace(firstNonEmpty(r.FormValue("name"), r.URL.Query().Get("name"))))
		if name == "delete" {
			s.handleSABDeleteFromQueue(w, r)
			return
		}
		s.handleSABQueue(w, r)
	case "history":
		name := strings.ToLower(strings.TrimSpace(firstNonEmpty(r.FormValue("name"), r.URL.Query().Get("name"))))
		if name == "delete" {
			s.handleSABDeleteFromHistory(w, r)
			return
		}
		s.handleSABHistory(w, r)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported mode"})
	}
}

func (s *Server) handleSABGetConfig(w http.ResponseWriter, r *http.Request) {
	categories := s.sabCategoryConfigs(r)
	items := make([]map[string]any, 0, len(categories))
	for i, category := range categories {
		items = append(items, map[string]any{
			"name": category.Name, "order": i, "pp": "3",
			"script": "None", "dir": category.Dir,
			"newzbin": "", "priority": "-100",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"config": map[string]any{
			"misc": map[string]any{
				"download_dir": s.cfg.Data.ReportedPathPrefix,
				"complete_dir": s.cfg.Data.ReportedPathPrefix,
				"dirscan_dir":  "",
				"script_dir":   "",
			},
			"categories":   items,
			"servers":      []any{},
			"rss":          []any{},
			"sorters":      []any{},
			"scripts":      []string{"None"},
			"download_dir": s.cfg.Data.ReportedPathPrefix,
			"complete_dir": s.cfg.Data.ReportedPathPrefix,
		},
	})
}

func (s *Server) handleSABGetCats(w http.ResponseWriter, r *http.Request) {
	categories := s.sabCategoryNames(r)
	writeJSON(w, http.StatusOK, map[string]any{"categories": categories})
}

func (s *Server) handleSABSetConfig(w http.ResponseWriter, r *http.Request) {
	section := strings.ToLower(strings.TrimSpace(firstNonEmpty(r.FormValue("section"), r.URL.Query().Get("section"))))
	if section != "categories" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported section"})
		return
	}
	name := strings.ToLower(strings.TrimSpace(firstNonEmpty(
		r.FormValue("keyword"), r.URL.Query().Get("keyword"),
		r.FormValue("name"), r.URL.Query().Get("name"),
	)))
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "category name is required"})
		return
	}
	dir := strings.TrimSpace(firstNonEmpty(r.FormValue("dir"), r.URL.Query().Get("dir")))
	if name != "*" && dir == "" {
		dir = name
	}
	if dir != "" {
		// PATCH-13 / G1: contain the SAB category dir under the data (Strm) root.
		resolved, err := resolveUnderRoot(s.cfg.Data.Strm, dir)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "dir escapes data root"})
			return
		}
		if err := os.MkdirAll(resolved, 0o750); err != nil { // #nosec G703 -- resolved comes from resolveUnderRoot, which rejects traversal and out-of-root absolute paths.
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	s.handleSABGetConfig(w, r)
}

func (s *Server) handleSABConfigCategories(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/config/categories" && r.URL.Path != "/config/categories/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	categories := s.sabCategoryConfigs(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var b strings.Builder
	b.WriteString("<!doctype html><html><body><h1>Categories</h1><ul>")
	for _, category := range categories {
		b.WriteString("<li><strong>" + html.EscapeString(category.Name) + "</strong></li>")
	}
	b.WriteString("</ul></body></html>")
	_, _ = w.Write([]byte(b.String()))
}

func (s *Server) handleSABAddURL(w http.ResponseWriter, r *http.Request) {
	category := sabRequestCategory(r)
	link := firstNonEmpty(r.FormValue("name"), r.URL.Query().Get("name"), r.FormValue("url"), r.URL.Query().Get("url"))
	name := firstNonEmpty(r.FormValue("nzbname"), r.URL.Query().Get("nzbname"), r.FormValue("title"))
	postProcessing := parseInt(firstNonEmpty(r.FormValue("pp"), r.URL.Query().Get("pp")), -1)
	password := firstNonEmpty(r.FormValue("password"), r.URL.Query().Get("password"))
	metadata := store.SubmissionMetadata{PostProcessing: postProcessing, Password: password}
	if !s.allowedSABNZBURL(link) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"status": false, "error": "invalid nzb URL"})
		return
	}
	if parsed, err := url.Parse(link); err == nil {
		token := strings.TrimSpace(strings.TrimPrefix(parsed.Path, "/getnzb/"))
		if token != "" {
			s.attachGrabProviderIdentityWithFallback(r.Context(), "nzb-token:"+token, category, name, &metadata)
		}
	}

	// C0.5: every NZB consumer expects SourceURI to contain literal NZB
	// bytes. Fetch and validate addurl links before persisting them.
	data, err := s.fetchNZBBytes(r.Context(), link)
	if err != nil {
		s.log.Warn("sab addurl: nzb fetch failed",
			"event", "sab_addurl_fetch_failed",
			"category", category,
			"display_name", name,
			"error", httpstream.Sanitize(err),
		)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"status": false, "error": "nzb fetch failed"})
		return
	}
	if err := validateFetchedNZB(data); err != nil {
		s.log.Warn("sab addurl: invalid nzb payload",
			"event", "sab_addurl_invalid_nzb",
			"category", category,
			"display_name", name,
			"error", httpstream.Sanitize(err),
		)
		writeJSON(w, http.StatusBadRequest, map[string]any{"status": false, "error": "invalid nzb payload"})
		return
	}

	item, _, err := s.enqueueSubmission(r.Context(), SubmissionRequest{
		SourceType:  store.SourceTypeNZB,
		ClientKind:  store.ClientKindSAB,
		Category:    category,
		DisplayName: name,
		SourceURI:   string(data),
		Metadata:    metadata,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"status": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, compat.SABAddResponse{Status: true, NzoIDs: []string{compat.SABNZOID(item.PublicID)}})
}

func (s *Server) allowedSABNZBURL(rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return sameSchemeAndHost(rawURL, s.cfg.Server.BaseURL) || sameSchemeAndHost(rawURL, s.cfg.Prowlarr.BaseURL)
}

func validateFetchedNZB(data []byte) error {
	parsed, err := nntp.ParseNZB(data)
	if err != nil {
		return err
	}
	if len(parsed.Files) == 0 {
		return fmt.Errorf("nzb has no files")
	}
	for _, file := range parsed.Files {
		if len(file.Segments) == 0 {
			return fmt.Errorf("nzb file has no segments")
		}
		seen := make(map[int]struct{}, len(file.Segments))
		for _, segment := range file.Segments {
			if segment.Number <= 0 || segment.Bytes <= 0 || strings.TrimSpace(segment.MessageID) == "" {
				return fmt.Errorf("nzb has invalid segment metadata")
			}
			if _, ok := seen[segment.Number]; ok {
				return fmt.Errorf("nzb has duplicate segment number")
			}
			seen[segment.Number] = struct{}{}
		}
	}
	return nil
}

func (s *Server) handleSABAddFile(w http.ResponseWriter, r *http.Request) {
	category := sabRequestCategory(r)
	postProcessing := parseInt(firstNonEmpty(r.FormValue("pp"), r.URL.Query().Get("pp")), -1)
	password := firstNonEmpty(r.FormValue("password"), r.URL.Query().Get("password"))
	jobName := firstNonEmpty(r.FormValue("nzbname"), r.URL.Query().Get("nzbname"))

	var ids []string
	if r.MultipartForm != nil {
		for _, header := range multipartFiles(r.MultipartForm, "nzbfile", "name", "file") {
			file, err := header.Open()
			if err != nil {
				s.log.Warn("sab file open failed", "filename", header.Filename, "error", err.Error())
				continue
			}
			displayName := firstNonEmpty(jobName, header.Filename)
			// Read the NZB content to use as source URI (NZB data inline)
			data, _ := io.ReadAll(file)
			_ = file.Close()
			// NS-6.3: bounded grab-time STAT rejection of a conclusively
			// fully-dead NZB, before it is ever queued -- see
			// rejectDeadGrabTimeNZB's own doc comment.
			if s.rejectDeadGrabTimeNZB(r.Context(), data, category, displayName) {
				continue
			}
			metadata := store.SubmissionMetadata{
				UploadedFilename: header.Filename,
				OriginalFilename: header.Filename,
				PostProcessing:   postProcessing,
				Password:         password,
			}
			s.attachGrabProviderIdentityWithFallback(r.Context(), nntp.ContentKey(data), category, displayName, &metadata)
			item, _, err := s.enqueueSubmission(r.Context(), SubmissionRequest{
				SourceType:  store.SourceTypeNZB,
				ClientKind:  store.ClientKindSAB,
				Category:    category,
				DisplayName: displayName,
				SourceURI:   string(data),
				Metadata:    metadata,
			})
			if err == nil {
				ids = append(ids, compat.SABNZOID(item.PublicID))
			} else {
				s.log.Warn("sab file submission failed", "filename", header.Filename, "error", err.Error())
			}
		}
	}
	if len(ids) == 0 {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"status": false, "error": "no nzb files accepted"})
		return
	}
	writeJSON(w, http.StatusOK, compat.SABAddResponse{Status: true, NzoIDs: ids})
}

// rejectDeadGrabTimeNZB implements NS-6.3: a bounded, synchronous grab-time
// STAT check for handleSABAddFile only. C0.5 now fetches addurl bytes too, but
// extending NS-6.3's STAT policy to that path remains outside both rows' scope.
// Returns true when the sampled NZB is conclusively fully dead (every
// conclusively-answered STAT sample came back missing) and the caller must
// reject this file's submission instead of accepting it.
//
// A disabled check, an NNTP-disabled deployment (NNTP is the only lane
// this row's STAT mechanism can check against -- the TorBox-NZB-cache lane
// has no STAT equivalent), an unconfigured usenet provider, a parse
// failure, or an inconclusive sample (0 conclusive answers, e.g. every
// candidate provider transiently unreachable or the bounded timeout
// elapsed before any conclusive sample landed) all abstain (return false)
// -- this check must never manufacture a rejection out of uncertainty,
// mirroring NS-6.1's own established inconclusive-never-decay precedent.
// A partial miss (some sampled segments present, some missing) also
// abstains: that softer completeness-threshold judgment remains NS-6.1/
// NS-4.2's own job, not this row's binary dead-post signature.
//
// On a conclusive dead verdict, this also blacklists the exact NZB
// content-key via the existing shared failed_hashes owner
// (s.store.BlacklistImmediately / NZBBlacklistKey) -- the same mechanism
// NS-6.2 already uses -- so a later arr re-search that turns up the
// identical release (from cache or mirrored across indexers) doesn't loop
// back through this same rejection every time; enqueueSubmission's own
// existing blacklist check at the top of this function already consults
// the same table for any later submission of the same content.
func (s *Server) rejectDeadGrabTimeNZB(ctx context.Context, data []byte, category, displayName string) bool {
	if !s.cfg.GrabHealth.Enabled || !s.cfg.NZBViaNNTP() {
		return false
	}
	_, up, ok := s.pickUsenetProvider(nil)
	if !ok {
		return false
	}
	checkCtx, cancel := context.WithTimeout(ctx, time.Duration(s.cfg.GrabHealth.TimeoutMS)*time.Millisecond)
	defer cancel()
	allDead, sampled, err := up.GrabTimeHealthCheck(checkCtx, data, s.cfg.GrabHealth.SampleSize)
	if err != nil {
		// Parse failure: not this row's job to diagnose; the existing
		// accept path's own async parse-retry (resolveNZBItem) handles it.
		return false
	}
	if !allDead {
		return false
	}
	// NS-6.3 lane-scope correction: the STAT verdict above speaks only for
	// the NNTP lane. When torbox_nzb outranks nntp_nzb the TorBox usenet
	// cache is what would actually serve this grab, so a post dead on
	// Newshosting may still play perfectly from cache. Rejecting (and
	// blacklisting) on the NNTP verdict alone would deny a serviceable
	// release and poison its content key for the preferred lane too.
	// Consult the existing cache oracle (provider.CheckCached -> TorBox
	// checkcached, a read-only query) and abstain unless the preferred
	// lane conclusively cannot serve it. Any inconclusive answer abstains,
	// matching this row's established never-reject-out-of-uncertainty rule.
	if s.cfg.TorBoxNZBPreferredOverNNTP() {
		cacheCtx, cacheCancel := context.WithTimeout(ctx, time.Duration(s.cfg.GrabHealth.TimeoutMS)*time.Millisecond)
		mayServe, conclusive := s.torboxNZBCacheMayServe(cacheCtx, data)
		cacheCancel()
		if mayServe || !conclusive {
			s.log.Info("grabhealth: abstaining on nntp-dead nzb, preferred torbox usenet lane may serve it",
				"event", "grabhealth_abstain_preferred_lane",
				"display_name", displayName,
				"category", category,
				"sampled", sampled,
				"cache_cached", mayServe,
				"cache_conclusive", conclusive,
			)
			return false
		}
	}
	key := NZBBlacklistKey(string(data))
	if _, blErr := s.store.BlacklistImmediately(ctx, key); blErr != nil {
		s.log.Warn("grabhealth: blacklist dead nzb failed (non-fatal)",
			"event", "grabhealth_blacklist_failed", "nzb_key", key, "error", blErr)
	}
	s.log.Warn("grabhealth: rejected dead nzb at grab time",
		"event", "grabhealth_rejected",
		"display_name", displayName,
		"category", category,
		"sampled", sampled,
		"nzb_key", key,
	)
	return true
}

// torboxNZBCacheMayServe asks every configured debrid lane whether this exact
// NZB's content is already cached, reusing the established
// provider.Provider.CheckCached owner (TorBox's own read-only usenet
// checkcached endpoint) rather than adding a second cache oracle. It creates
// no item, mutates no provider state, and persists nothing -- the temporary
// item exists only to carry the NZB bytes into the existing content-hash
// derivation.
//
// mayServe is true as soon as any lane conclusively reports the content
// cached. conclusive is true only if at least one lane answered without
// error; a lane that cannot answer for NZB source types at all (e.g. a
// torrent-only provider) or that fails transiently is skipped rather than
// counted as a definitive "not cached". When every lane fails to answer,
// conclusive is false and the caller must abstain.
func (s *Server) torboxNZBCacheMayServe(ctx context.Context, data []byte) (mayServe bool, conclusive bool) {
	if len(data) == 0 || len(s.lanes) == 0 {
		return false, false
	}
	uri := string(data)
	probe := &store.Item{SourceType: store.SourceTypeNZB, SourceURI: &uri}
	for _, lane := range s.lanes {
		if lane.Prov == nil {
			continue
		}
		if ctx.Err() != nil {
			return false, false
		}
		res, err := lane.Prov.CheckCached(ctx, probe)
		if err != nil || res == nil {
			continue
		}
		conclusive = true
		if res.Cached {
			return true, true
		}
	}
	return false, conclusive
}

func (s *Server) attachGrabProviderIdentity(ctx context.Context, key string, metadata *store.SubmissionMetadata) {
	if s.store == nil || metadata == nil || metadata.ProviderIdentity != nil {
		return
	}
	keyKind, _, _ := strings.Cut(key, ":")
	identity, ok, err := s.store.GetGrabProviderIdentity(ctx, key)
	if err != nil {
		s.log.Warn("provider identity context: durable read failed", "event", "identity_attach_read_failed", "key_kind", keyKind, "error", err)
		return
	}
	if !ok {
		s.log.Debug("provider identity context: no captured identity for this grab key (correct abstain, not necessarily an error -- see ID-01/ID-02 WORKLOG for known causes)",
			"event", "identity_attach_miss", "key_kind", keyKind)
		return
	}
	if verr := sidecar.ValidateProviderIdentity(identity); verr != nil {
		s.log.Warn("provider identity context: captured identity failed validation, discarding",
			"event", "identity_attach_invalid", "key_kind", keyKind, "error", verr)
		return
	}
	s.log.Debug("provider identity context: attached at grab time", "event", "identity_attach_hit", "key_kind", keyKind, "kind", identity.Kind)
	metadata.ProviderIdentity = identity
}

// attachGrabProviderIdentityWithFallback is ID-04's grab-time fallback for
// the NZB/SAB submission paths. attachGrabProviderIdentity's cache lookup
// depends on a bridge (handleGetNZB) from the search-time "nzb-token:"
// cache entry to a content-hash-keyed one, which only fires when
// /getnzb/{token} is actually fetched before the arr uploads the NZB via
// SAB -- a real, verified gap on this deployment's Sonarr grabs (Radarr's
// own grab flow exercises it; Sonarr's does not), leaving every Sonarr-
// grabbed item with no identity at all despite search-time capture
// (persistGrabProviderIdentity) working correctly for both.
//
// When the cache lookup finds nothing, this re-derives identity directly
// from the release's own filename via the exact same per-Arr-library
// lookup providerIdentityForQuery already uses at search time -- not a new
// matching mechanism, just a second caller of an existing one. kind is
// inferred from category ("tv"-prefixed => series, else => movie) purely
// to choose which title-boundary parser to try (internal/identitycheck's
// SeriesTitleFromRelease/MovieTitleFromRelease); a wrong guess here only
// means this fallback finds nothing (abstain), it can never mislabel or
// overwrite an already-attached identity.
func (s *Server) attachGrabProviderIdentityWithFallback(ctx context.Context, cacheKey, category, releaseTitle string, metadata *store.SubmissionMetadata) {
	s.attachGrabProviderIdentity(ctx, cacheKey, metadata)
	if metadata == nil || metadata.ProviderIdentity != nil {
		return
	}
	kind := "movie"
	title, ok := identitycheck.MovieTitleFromRelease(releaseTitle)
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(category)), "tv") {
		kind = "series"
		title, ok = identitycheck.SeriesTitleFromRelease(releaseTitle)
	}
	if !ok {
		s.log.Debug("provider identity context: no boundary token in release name for fallback lookup, abstaining",
			"event", "identity_attach_fallback_no_boundary", "kind", kind)
		return
	}
	identity := s.providerIdentityForQuery(ctx, kind, title, "", "", "")
	if identity == nil {
		s.log.Debug("provider identity context: fallback per-arr-library lookup found no match, abstaining",
			"event", "identity_attach_fallback_no_match", "kind", kind)
		return
	}
	if verr := sidecar.ValidateProviderIdentity(identity); verr != nil {
		s.log.Warn("provider identity context: fallback identity failed validation, discarding",
			"event", "identity_attach_fallback_invalid", "kind", kind, "error", verr)
		return
	}
	s.log.Debug("provider identity context: attached via ID-04 grab-time fallback",
		"event", "identity_attach_fallback_hit", "kind", kind)
	metadata.ProviderIdentity = identity
}

func magnetProviderIdentityContext(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(parsed.Scheme, "magnet") {
		return ""
	}
	value := strings.ToLower(strings.TrimSpace(parsed.Query().Get("x.dh.id")))
	if len(value) != 24 {
		return ""
	}
	if _, err := hex.DecodeString(value); err != nil {
		return ""
	}
	return value
}

func (s *Server) handleSABQueue(w http.ResponseWriter, r *http.Request) {
	category := sabRequestCategory(r)
	if category == "*" {
		category = ""
	}
	items, err := s.store.ListVisibleClientItems(r.Context(), store.ClientKindSAB, category, 1000)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, compat.ProjectSABQueue(s.cfg.Compatibility.SABVersion, items))
}

func (s *Server) handleSABHistory(w http.ResponseWriter, r *http.Request) {
	category := sabRequestCategory(r)
	if category == "*" {
		category = ""
	}
	items, err := s.store.ListSABHistoryItems(r.Context(), store.ClientKindSAB, category, 1000)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	failed := 0
	for _, item := range items {
		if item.State == store.StateFailed {
			failed++
		}
	}
	s.log.Info("sab history: items exposed",
		"event", "sab_failed_history_exposed",
		"category", category,
		"items", len(items),
		"failed_items", failed,
	)
	writeJSON(w, http.StatusOK, compat.ProjectSABHistory(s.cfg.Compatibility.SABVersion, items, s.cfg.Data.Root, s.cfg.Data.ReportedPathPrefix))
}

func normalizeAggregatorCategory(category string) string {
	category = strings.TrimSpace(category)
	switch {
	case strings.EqualFold(category, "Movies"):
		return "movies"
	case strings.EqualFold(category, "TV"):
		return "tv"
	default:
		return category
	}
}

func sabRequestCategory(r *http.Request) string {
	return normalizeAggregatorCategory(firstNonEmpty(r.FormValue("cat"), r.FormValue("category")))
}

func (s *Server) handleSABDeleteFromQueue(w http.ResponseWriter, r *http.Request) {
	// No-op: SABnzbd queue delete signals the item completed and moved to history.
	// We do NOT mark the item removed here — it must remain visible in history
	// so Sonarr can read the storage path and trigger import. Real SABnzbd moves
	// completed items from queue to history automatically; queue delete from
	// Sonarr is just an acknowledgement, not a true deletion.
	value := firstNonEmpty(r.FormValue("value"), r.URL.Query().Get("value"))
	ids := splitCSV(value)
	writeJSON(w, http.StatusOK, map[string]any{"status": true, "nzo_ids": ids})
}

func (s *Server) handleSABDeleteFromHistory(w http.ResponseWriter, r *http.Request) {
	// Sonarr calls history delete after successful import. We must NOT mark
	// the item removed here — it must stay StateReady so the proxy can serve
	// /stream/{id}/... to Jellyfin — but the entry must also stop appearing
	// in history output, or Sonarr's DownloadEventHub re-issues this delete
	// on every poll (~90s) forever (live 2026-07-08). Hide it; lifecycle is
	// untouched.
	value := firstNonEmpty(r.FormValue("value"), r.URL.Query().Get("value"))
	ids := splitCSV(value)
	for _, id := range ids {
		publicID := compat.NormalizeSABNZOID(strings.TrimSpace(id))
		if publicID == "" {
			continue
		}
		hidden, err := s.store.HideFromSABHistory(r.Context(), publicID)
		if err != nil {
			s.log.Warn("sab history delete: hide failed", "nzo_id", id, "error", err)
			continue
		}
		if hidden {
			s.log.Info("sab history delete: entry hidden (lifecycle untouched)", "nzo_id", id)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": true, "nzo_ids": ids})
}

// NZBBlacklistKey derives the failed_hashes lookup key for one NZB's raw
// content. Shares the failed_hashes table with torrent infohash blacklisting
// (internal/store: IncrementFailCount/IsBlacklisted are hash-string-generic,
// no schema change needed) — the "nzb:" prefix keeps the two ID spaces from
// ever colliding. Used to stop a permanently-broken release (dead/expired
// NNTP articles, a torrent that will never seed, etc.) from being endlessly
// re-offered and re-grabbed: without this, an arr's periodic RSS/search-missing
// cycle re-selects the same doomed release every pass, DH burns through
// maxResolveAttempts retries each time, and the arr's queue fills up with
// duplicate failed entries pointing at the same underlying content — the
// "not grabbed by sonarr" / "not imported" confusion this was built to fix
// (2026-07-01: The Boys S01 RakuvArrow RAR release, expired retention,
// resubmitted repeatedly by Sonarr's automatic search before this existed).
func NZBBlacklistKey(rawNZB string) string {
	return nntp.ContentKey([]byte(rawNZB))
}

func (s *Server) nntpContentKey(ctx context.Context, item *store.Item) string {
	if item == nil || item.SourceURI == nil {
		return ""
	}
	key := nntp.ContentKey([]byte(*item.SourceURI))
	if artifacts, err := s.store.GetNNTPArtifacts(ctx, key); err != nil {
		s.log.Warn("nntp: content-cache lookup failed (non-fatal)", "item_id", item.ID, "error", err)
	} else {
		if item.RarManifest == nil {
			item.RarManifest = artifacts.RARManifest
		}
		if item.ZIPManifest == nil {
			item.ZIPManifest = artifacts.ZIPManifest
		}
	}
	if err := s.store.PromoteNNTPArtifacts(ctx, key, item.ID, item.RarManifest, item.ZIPManifest); err != nil {
		s.log.Warn("nntp: content-cache promotion failed (non-fatal)", "item_id", item.ID, "error", err)
	}
	return key
}

// enqueueSubmission creates a new Item and transitions it to StateResolving.
func (s *Server) enqueueSubmission(ctx context.Context, req SubmissionRequest) (*store.Item, bool, error) {
	// Preference gates: reject grabs for a fully-disabled protocol so the arr
	// falls through to another release/indexer instead of DH accepting something
	// it has no lane to fulfill.
	if req.SourceType == store.SourceTypeTorrent && !s.cfg.TorrentsEnabled() {
		return nil, false, fmt.Errorf("torrent acquisition disabled by preference")
	}
	if req.SourceType == store.SourceTypeNZB && !s.cfg.NZBEnabled() {
		return nil, false, fmt.Errorf("nzb acquisition disabled by preference")
	}
	if req.SourceType == store.SourceTypeHTTP {
		if !s.cfg.HTTPStreamEnabled() {
			return nil, false, fmt.Errorf("http stream provider disabled")
		}
		if strings.TrimSpace(req.ResolveKey) == "" {
			return nil, false, fmt.Errorf("http submission missing resolve key")
		}
	}
	// Reject NZB content that has already failed resolution repeatedly (see
	// NZBBlacklistKey) — fast, clear failure instead of another slow
	// multi-attempt resolve cycle for a release that will never succeed.
	if req.SourceType == store.SourceTypeNZB && strings.TrimSpace(req.SourceURI) != "" {
		key := NZBBlacklistKey(req.SourceURI)
		if blacklisted, blErr := s.store.IsBlacklisted(ctx, key); blErr != nil {
			s.log.Warn("enqueue: nzb blacklist check failed (non-fatal, proceeding)",
				"event", "nzb_blacklist_check_failed",
				"nzb_key", key,
				"display_name", req.DisplayName,
				"category", req.Category,
				"error", blErr,
			)
		} else if blacklisted {
			s.log.Warn("enqueue: rejected blacklisted nzb",
				"event", "enqueue_rejected",
				"reason", "temporarily_blacklisted",
				"nzb_key", key,
				"display_name", req.DisplayName,
				"category", req.Category,
			)
			return nil, false, fmt.Errorf("nzb previously failed resolution repeatedly; temporarily blacklisted")
		}
	}
	category := strings.TrimSpace(req.Category)
	if category == "" {
		category = s.cfg.Compatibility.DefaultCategory
	}

	itemID, err := util.RandomHex(16)
	if err != nil {
		return nil, false, err
	}
	publicID := util.SHA1Hex(itemID)

	sourceURI := strings.TrimSpace(req.SourceURI)
	infoHash := normalizeInfoHash(req.InfoHash)
	providerURI, realInfoHash := resolveSyntheticGrab(ctx, s.store, sourceURI, infoHash)
	sourceURI = providerURI
	if req.SourceType == store.SourceTypeTorrent {
		if blacklisted, identity, blErr := isTorrentBlacklisted(ctx, s.store, infoHash); blErr != nil {
			s.log.Warn("enqueue: torrent blacklist check failed (non-fatal, proceeding)",
				"event", "torrent_blacklist_check_failed",
				"info_hash", infoHash,
				"display_name", req.DisplayName,
				"category", req.Category,
				"error", blErr,
			)
		} else if blacklisted {
			s.log.Warn("enqueue: rejected blacklisted torrent",
				"event", "enqueue_rejected",
				"reason", "temporarily_blacklisted",
				"source_type", store.SourceTypeTorrent,
				"info_hash", identity,
				"submitted_hash", infoHash,
				"display_name", req.DisplayName,
				"category", category,
			)
			return nil, false, fmt.Errorf("torrent previously failed resolution; temporarily blacklisted")
		}
	}
	if realInfoHash != "" && realInfoHash != infoHash {
		// Synthetic grab: keep the synthetic hash client-facing (item.InfoHash)
		// but persist the real one so the provider submit path never falls
		// back to a synthetic-hash magnet and createtorrent dedup keys on the
		// real content.
		req.Metadata.RealInfoHash = realInfoHash
	}
	submissionKey := submissionFingerprint(req.SourceType, req.ClientKind, category, sourceURI, infoHash)

	active, err := s.store.FindActiveBySubmissionKey(ctx, submissionKey)
	if err != nil {
		return nil, false, err
	}
	if active != nil {
		// HTTP dedup compares the canonical resolve key, not just the
		// synthetic digest: a fingerprint hit with a different key is a
		// digest collision and must fail loudly, never merge (D10).
		if req.SourceType == store.SourceTypeHTTP {
			activeKey := ""
			if active.ResolveKey != nil {
				activeKey = *active.ResolveKey
			}
			if activeKey != req.ResolveKey {
				return nil, false, fmt.Errorf("http submission digest collision: active item %s has a different resolve key", active.ID)
			}
		}
		// For StateReady items, verify the strm file still exists on disk.
		// If it was deleted (e.g. during debugging or manual cleanup), treat as
		// a new submission so the pipeline re-resolves and re-writes it.
		// For StateResolving/StateAccepted items, always deduplicate — the
		// pipeline is already working on it.
		if active.State == store.StateReady {
			// Dedup on DB state alone. Previously we os.Stat'd the strm_path
			// to re-submit if the file was missing, but Sonarr moves strm files
			// to its library dir immediately after import — the path no longer
			// exists on DH's filesystem even though the item resolved correctly.
			// Stat'ing a moved path returns not-exist and creates a duplicate item.
			// A ready item in the DB is authoritative: the content resolved and
			// Sonarr imported it. Do not re-submit.
			s.log.Info("duplicate submission ignored (ready)",
				"item_id", active.ID, "public_id", active.PublicID)
			return active, true, nil
		} else {
			s.log.Info("duplicate submission ignored",
				"item_id", active.ID, "public_id", active.PublicID,
				"state", active.State)
			return active, true, nil
		}
	}

	// TS-8.1: a later torrent submission of the same real info-hash aliases
	// onto an already-provider-bound item's provider object/cache footprint
	// instead of a redundant CheckCached dial and createtorrent call. This
	// complements submitDedup's singleflight, which only coalesces truly
	// concurrent same-hash submissions; a sequential later submission (the
	// common three-Sonarr-instance/season-pack-episode-fan-out case) reaches
	// here after the earlier item already has a persisted provider binding.
	var aliasSource *store.Item
	if req.SourceType == store.SourceTypeTorrent && realInfoHash != "" {
		var aliasErr error
		aliasSource, aliasErr = s.store.FindAliasCandidateByInfoHash(ctx, realInfoHash)
		if aliasErr != nil {
			s.log.Warn("enqueue: alias candidate lookup failed (non-fatal, proceeding normally)",
				"event", "alias_lookup_failed",
				"info_hash", realInfoHash,
				"display_name", req.DisplayName,
				"error", aliasErr,
			)
			aliasSource = nil
		} else if aliasSource != nil {
			// Defensive: the store query already filters this, but a
			// candidate must never be aliased onto without an actual live
			// provider binding to copy.
			if aliasSource.Provider == nil || strings.TrimSpace(*aliasSource.Provider) == "" ||
				(aliasSource.RemoteID == nil && aliasSource.QueuedID == nil) {
				aliasSource = nil
			}
		}
	}

	var gateResult *cachegate.Result
	// The legacy precheck is only valid for the single-provider path. With
	// independent provider lanes, submitItemDirect must evaluate each lane in
	// order so a full or unavailable primary cannot mask a healthy fallback.
	if req.SourceType == store.SourceTypeTorrent && aliasSource == nil && len(s.lanes) == 0 {
		if s.gate == nil {
			return nil, false, fmt.Errorf("no debrid provider is configured")
		}
		tempItem := &store.Item{
			SourceType:    req.SourceType,
			SubmissionKey: submissionKey,
		}
		if realInfoHash != "" {
			h := realInfoHash
			tempItem.InfoHash = &h
		}
		var gateErr error
		gateResult, gateErr = s.gate.Check(ctx, tempItem)
		if gateErr != nil {
			s.log.Info("enqueue: cachegate rejected",
				"display_name", req.DisplayName,
				"source_type", req.SourceType,
				"error", gateErr,
			)
			return nil, false, gateErr
		}
	}

	now := time.Now().UTC()
	displayName := deriveDisplayName(req.DisplayName, sourceURI, infoHash, itemID)

	item := &store.Item{
		ID:            itemID,
		PublicID:      publicID,
		SourceType:    req.SourceType,
		ClientKind:    req.ClientKind,
		Category:      category,
		State:         store.StateAccepted,
		SubmissionKey: submissionKey,
		DisplayName:   displayName,
		Metadata:      req.Metadata,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if sourceURI != "" {
		item.SourceURI = &sourceURI
	}
	if infoHash != "" {
		item.InfoHash = &infoHash
	}
	if req.SourceType == store.SourceTypeHTTP {
		// Persisted at creation — before StateResolving — so a crash inside
		// the window leaves a recoverable item, never a keyless one (HS-1.2).
		rk := req.ResolveKey
		item.ResolveKey = &rk
	}

	if err := s.store.CreateItem(ctx, item); err != nil {
		return nil, false, err
	}

	item.NextRunAt = &now
	if err := s.store.UpdateItemState(ctx, item, store.StateResolving, "queued for resolution"); err != nil {
		return nil, false, err
	}

	s.log.Info("item accepted",
		"item_id", item.ID,
		"public_id", item.PublicID,
		"client", item.ClientKind,
		"source_type", item.SourceType,
		"category", item.Category,
		"display_name", item.DisplayName,
	)
	workerID := "resolve-goroutine:" + item.ID
	claimed, err := s.store.ClaimItem(ctx, item.ID, workerID)
	if err != nil {
		return nil, false, err
	}
	if !claimed {
		return item, false, nil
	}
	s.itemMu.Lock()
	s.activeGoroutines[item.ID] = struct{}{}
	s.itemMu.Unlock()
	s.wg.Add(1)
	go func(it *store.Item, workerID string) {
		defer s.wg.Done()
		defer func() {
			s.itemMu.Lock()
			delete(s.activeGoroutines, it.ID)
			s.itemMu.Unlock()
		}()
		bgCtx, cancel := context.WithTimeout(s.shutdownCtx, 5*time.Minute)
		defer cancel()
		defer func() {
			releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer releaseCancel()
			if err := s.store.ReleaseItemClaim(releaseCtx, it.ID, workerID); err != nil {
				s.log.Warn("resolve goroutine: release item claim failed", "item_id", it.ID, "error", err)
			}
		}()
		if aliasSource != nil {
			s.aliasItemFromExisting(bgCtx, s.log.With("item_id", it.ID, "display_name", it.DisplayName), it, aliasSource)
			return
		}
		s.resolveItemAsync(bgCtx, it, gateResult)
	}(item, workerID)
	return item, false, nil
}

// aliasItemFromExisting binds item directly onto an already-established
// provider object (TS-8.1): a later submission of the same real torrent
// info-hash reuses aliasSource's live provider binding -- no redundant
// cachegate CheckCached dial and no redundant provider Submit/createtorrent
// call -- while item keeps its own independent DB row and eventual .strm
// identity (LC-01). The daemon resolve loop dispatches on item.Provider (see
// cmd/darkharrbor/main.go), so once this persists, it polls the shared
// RemoteID/QueuedID through the correct provider adapter exactly as it would
// for a directly-submitted item; aliasing changes only how the binding was
// established, never how it is subsequently resolved, repaired, or removed.
func (s *Server) aliasItemFromExisting(ctx context.Context, log *slog.Logger, item *store.Item, aliasSource *store.Item) {
	provName := ""
	if aliasSource.Provider != nil {
		provName = *aliasSource.Provider
	}
	item.Provider = &provName
	resp := &provider.CreateTaskResponse{}
	if aliasSource.RemoteID != nil {
		resp.RemoteID = *aliasSource.RemoteID
	}
	if aliasSource.QueuedID != nil {
		resp.QueuedID = *aliasSource.QueuedID
	}
	if !s.persistSubmitResultOrFailLoudly(ctx, log, item, resp, aliasSource.Cached) {
		return
	}
	log.Info("submit: aliased onto existing provider object",
		"event", "torrent_alias_bound",
		"provider", provName,
		"remote_id", resp.RemoteID,
		"queued_id", resp.QueuedID,
		"cached", aliasSource.Cached,
		"alias_source_item_id", aliasSource.ID,
	)
}

// Runs cachegate → createtorrent/createusenet inline (no submit loop wait).
// The resolver loop (3s) then picks up the item once RemoteID is set.
// NZB items skip TorBox submission entirely — NNTP path handles them.
func (s *Server) resolveItemAsync(ctx context.Context, item *store.Item, gateResult *cachegate.Result) {
	log := s.log.With("item_id", item.ID, "display_name", item.DisplayName)
	log.Info("resolve goroutine: item registered, submitting inline")

	// HTTP items: synchronous one-pass resolve — no submit, no polling, no
	// provider lane (D1). Crash recovery re-enters the identical pass via
	// the resolver cycle (HS-1.2).
	if item.SourceType == store.SourceTypeHTTP {
		s.resolveHTTPItem(ctx, item)
		return
	}

	// NZB items: choose the acquisition lane (TorBox usenet cache vs NNTP).
	if item.SourceType == store.SourceTypeNZB {
		s.resolveNZBAsync(ctx, item)
		return
	}

	// Torrent items: run submit inline — no waiting for 30s submit loop tick.
	s.submitItemDirect(ctx, item, gateResult)
}

// submitItemDirect runs the multi-lane submit inline for a single item.
// Called from resolveItemAsync (per-grab goroutine path). Walks s.lanes in
// order; the first lane that accepts the item wins (C-5 independent lanes).
// gateResult is the already-computed gate result for the primary lane (from
// enqueueSubmission's cachegate pre-check); it is reused to avoid a redundant
// oracle call on the primary lane.
func (s *Server) submitItemDirect(ctx context.Context, item *store.Item, gateResult *cachegate.Result) {
	log := s.log.With("item_id", item.ID, "display_name", item.DisplayName)

	if len(s.lanes) == 0 {
		// Fallback: single-provider path (pre-C-5 items / single provider config).
		s.submitItemDirectSingle(ctx, log, item, gateResult)
		return
	}

	for i, lane := range s.lanes {
		var result *cachegate.Result

		if i == 0 && gateResult != nil {
			// Reuse the already-computed gate result for the primary lane.
			result = gateResult
		} else {
			var gateErr error
			result, gateErr = lane.Gate.Check(ctx, item)
			if gateErr != nil {
				log.Debug("submit: lane gate rejected", "provider", lane.Prov.Name(), "error", gateErr)
				continue // try next lane
			}
		}

		log.Info("submit: cachegate passed", "provider", lane.Prov.Name(), "cached", result.Cached)
		s.recordSubmitAvailability(ctx, item, lane.Prov.Name(), result.Cached)

		// Use submitDedup only for the primary provider (singleflight by infohash).
		// Secondary providers use direct Submit to avoid cross-provider singleflight collisions.
		var resp *provider.CreateTaskResponse
		var submitErr error
		if i == 0 {
			resp, submitErr = s.submitDedup(ctx, item, provider.SubmitOptions{AddOnlyIfCached: result.Cached})
		} else {
			resp, submitErr = lane.Prov.Submit(ctx, item, provider.SubmitOptions{AddOnlyIfCached: result.Cached})
		}

		if submitErr != nil {
			log.Warn("submit: provider createtask failed", "provider", lane.Prov.Name(), "error", submitErr)
			if provider.IsRetryable(submitErr) {
				// Retryable: back off and do not try other lanes.
				next := time.Now().Add(30 * time.Second)
				item.NextRunAt = &next
				item.RetryCount++
				if err := s.store.UpdateItem(ctx, item); err != nil {
					log.Error("submit: persist retry state failed", "error", err)
				}
				return
			}
			// Non-retryable on this lane: try next lane if available.
			continue
		}

		// Record uncached add against this lane's gate governor.
		if !result.Cached && item.SourceType == store.SourceTypeTorrent {
			if recordErr := lane.Gate.RecordUncached(ctx, item); recordErr != nil {
				log.Warn("submit: governor record failed (non-fatal)", "provider", lane.Prov.Name(), "error", recordErr)
			}
		}

		// Bind the item to this provider and persist.
		provName := lane.Prov.Name()
		item.Provider = &provName
		if !s.persistSubmitResultOrFailLoudly(ctx, log, item, resp, result.Cached) {
			return
		}
		log.Info("submit: submitted",
			"provider", provName,
			"remote_id", resp.RemoteID,
			"queued_id", resp.QueuedID,
			"cached", result.Cached,
		)
		return
	}

	// All lanes failed non-retryably.
	msg := "all debrid provider lanes rejected or failed non-retryably"
	log.Warn("submit: "+msg, "item_id", item.ID)
	item.ErrorMessage = &msg
	if err := s.store.UpdateItemState(ctx, item, store.StateFailed, msg); err != nil {
		log.Error("submit: persist all-lanes-failed state", "error", err)
	}
}

// submitItemDirectSingle is the pre-C-5 single-provider submit path,
// retained as a fallback for the edge case where s.lanes is empty.
func (s *Server) submitItemDirectSingle(ctx context.Context, log *slog.Logger, item *store.Item, gateResult *cachegate.Result) {
	if s.gate == nil || s.prov == nil {
		msg := "no debrid provider is configured"
		item.ErrorMessage = &msg
		if err := s.store.UpdateItemState(ctx, item, store.StateFailed, msg); err != nil {
			log.Error("submit: persist missing-provider state failed", "error", err)
		}
		return
	}
	result := gateResult
	if result == nil {
		var err error
		result, err = s.gate.Check(ctx, item)
		if err != nil {
			log.Warn("submit: cachegate rejected", "error", err)
			msg := err.Error()
			item.ErrorMessage = &msg
			if err := s.store.UpdateItemState(ctx, item, store.StateFailed, msg); err != nil {
				log.Error("submit: persist failed state failed", "error", err)
			}
			return
		}
	}
	if s.prov != nil {
		s.recordSubmitAvailability(ctx, item, s.prov.Name(), result.Cached)
	}
	resp, err := s.submitDedup(ctx, item, provider.SubmitOptions{AddOnlyIfCached: result.Cached})
	if err != nil {
		log.Warn("submit: createtask failed", "error", err)
		if provider.IsRetryable(err) {
			next := time.Now().Add(30 * time.Second)
			item.NextRunAt = &next
			item.RetryCount++
			if err := s.store.UpdateItem(ctx, item); err != nil {
				log.Error("submit: persist retry state failed", "error", err)
			}
		} else {
			msg := err.Error()
			item.ErrorMessage = &msg
			if err := s.store.UpdateItemState(ctx, item, store.StateFailed, msg); err != nil {
				log.Error("submit: persist failed state failed", "error", err)
			}
		}
		return
	}
	if !result.Cached && item.SourceType == store.SourceTypeTorrent {
		if recordErr := s.gate.RecordUncached(ctx, item); recordErr != nil {
			log.Warn("submit: governor record failed (non-fatal)", "error", recordErr)
		}
	}
	if !s.persistSubmitResultOrFailLoudly(ctx, log, item, resp, result.Cached) {
		return
	}
	log.Info("submit: submitted", "remote_id", resp.RemoteID, "cached", result.Cached)
}

// resolveNZBAsync selects the acquisition lane for a freshly-enqueued NZB item.
//
// Default (HARRBOR_NZB_CACHE=false) and the fallback for every miss/error is the
// NNTP lane: set NextRunAt so the resolve loop materializes the item via the
// usenet provider (resolveNZBItem). When the TorBox usenet cache is enabled, the
// NZB's content hash is checked against TorBox; a cache HIT is fulfilled via
// TorBox (submit -> the item gains a remote/queued id and resolves through the
// same CDN path as torrents), and a MISS falls through to NNTP. The decision is
// durable: a remote/queued id on an NZB marks the TorBox lane for the resolve and
// stream stages; its absence marks the NNTP lane. The per-grab goroutine holds
// the item claim throughout, so the submit/resolve loops never race this.
func (s *Server) resolveNZBAsync(ctx context.Context, item *store.Item) {
	log := s.log.With("item_id", item.ID, "display_name", item.DisplayName)

	// Try the TorBox usenet cache first only when it is preferred ahead of NNTP
	// (torbox_nzb enabled and ranked before nntp_nzb, or NNTP disabled). When NNTP
	// outranks TorBox it is the always-succeeding floor, so the TorBox pass is skipped.
	if s.cfg.TorBoxNZBPreferredOverNNTP() && providerCaps(s.prov).UsenetIngest {
		result, err := s.prov.CheckCached(ctx, item)
		switch {
		case err != nil:
			log.Warn("nzb: torbox usenet checkcached failed", "error", err)
		case result != nil && result.Cached:
			if s.submitNZBToTorBox(ctx, log, item) {
				return // TorBox lane: remote/queued id set, resolve loop polls it
			}
			// submit failed — fall through
		default:
			log.Info("nzb: not cached on TorBox")
		}
	}

	// NNTP lane (the usenet floor), when enabled.
	if s.cfg.NZBViaNNTP() {
		now := time.Now()
		item.NextRunAt = &now
		if err := s.store.UpdateItem(ctx, item); err != nil {
			log.Warn("nzb: update NextRunAt failed (non-fatal)", "error", err)
		}
		log.Info("nzb: NNTP lane — resolver will materialize via usenet provider")
		return
	}

	// No NZB lane could fulfill this item (TorBox cache miss/disabled, NNTP disabled).
	msg := "no NZB fulfillment lane available (TorBox cache miss or disabled, NNTP disabled)"
	item.ErrorMessage = &msg
	if err := s.store.UpdateItemState(ctx, item, store.StateFailed, msg); err != nil {
		log.Error("nzb: persist failed state", "error", err)
	}
}

// submitNZBToTorBox submits a cache-confirmed NZB to TorBox and persists the
// returned ids. Returns true when the item entered the TorBox lane (a remote or
// queued id is now set); false on any failure, leaving the caller to fall back to
// NNTP. AddOnlyIfCached is a redundant safety net behind the cache check.
func (s *Server) submitNZBToTorBox(ctx context.Context, log *slog.Logger, item *store.Item) bool {
	resp, err := s.submitDedup(ctx, item, provider.SubmitOptions{AddOnlyIfCached: true})
	if err != nil {
		log.Warn("nzb: torbox usenet submit failed, falling back to NNTP", "error", err)
		return false
	}
	if resp == nil || (resp.RemoteID == "" && resp.QueuedID == "") {
		log.Warn("nzb: torbox usenet submit returned no id, falling back to NNTP")
		return false
	}
	if !s.persistSubmitResultOrFailLoudly(ctx, log, item, resp, true) {
		return true
	}
	log.Info("nzb: TorBox usenet lane — submitted to cache",
		"remote_id", resp.RemoteID, "queued_id", resp.QueuedID)
	return true
}

const (
	submitPersistAttempts   = 3 // initial try + 2 bounded retries
	submitPersistRetryDelay = 250 * time.Millisecond
)

// itemPersister is the minimal store surface needed by persistSubmitResult.
// The concrete *store.Store satisfies it; tests supply a stub.
type itemPersister interface {
	UpdateItem(ctx context.Context, item *store.Item) error
	UpdateItemState(ctx context.Context, item *store.Item, next store.ItemState, message string) error
}

func (s *Server) persistSubmitResultOrFailLoudly(ctx context.Context, log *slog.Logger, item *store.Item, resp *provider.CreateTaskResponse, cached bool) bool {
	return persistSubmitResult(ctx, log, s.store, item, resp, cached)
}

// persistSubmitResult is the testable core of persistSubmitResultOrFailLoudly.
// It applies resp fields to item, retries UpdateItem up to submitPersistAttempts
// times, and transitions to StateFailed (logging leaked IDs) on exhaustion.
func persistSubmitResult(ctx context.Context, log *slog.Logger, st itemPersister, item *store.Item, resp *provider.CreateTaskResponse, cached bool) bool {
	if resp.RemoteID != "" {
		item.RemoteID = &resp.RemoteID
	}
	if resp.QueuedID != "" {
		item.QueuedID = &resp.QueuedID
	}
	if resp.RemoteHash != "" && item.InfoHash == nil {
		item.InfoHash = &resp.RemoteHash
	}
	if resp.DisplayName != "" {
		item.DisplayName = resp.DisplayName
	}
	item.Cached = cached
	now := time.Now()
	item.NextRunAt = &now // resolver picks up immediately on next tick

	var lastErr error
	for attempt := 1; attempt <= submitPersistAttempts; attempt++ {
		if err := st.UpdateItem(ctx, item); err != nil {
			lastErr = err
			log.Warn("submit: persist torbox ids failed",
				"attempt", attempt,
				"attempts", submitPersistAttempts,
				"remote_id", resp.RemoteID,
				"queued_id", resp.QueuedID,
				"error", err,
			)
			if attempt < submitPersistAttempts {
				timer := time.NewTimer(submitPersistRetryDelay)
				select {
				case <-ctx.Done():
					timer.Stop()
					lastErr = ctx.Err()
					attempt = submitPersistAttempts
				case <-timer.C:
				}
			}
			continue
		}
		return true
	}

	msg := fmt.Sprintf("TorBox create succeeded but persisting submission IDs failed after %d attempts; leaked remote_id=%q queued_id=%q: %v", submitPersistAttempts, resp.RemoteID, resp.QueuedID, lastErr)
	item.ErrorMessage = &msg
	if err := st.UpdateItemState(ctx, item, store.StateFailed, msg); err != nil {
		log.Error("submit: persist failed state after TorBox ID leak", "remote_id", resp.RemoteID, "queued_id", resp.QueuedID, "error", err)
	}
	return false
}

// noRedirectDo executes an HTTP request without following redirects.
// It is the shared helper for followToCDN preflights.
func noRedirectDo(client *http.Client, req *http.Request) (*http.Response, error) {
	return client.Do(req)
}

func (s *Server) removeOutput(ctx context.Context, item *store.Item) {
	if s.writer == nil || item == nil {
		return
	}
	if err := s.writer.Remove(ctx, item); err != nil {
		s.log.Warn("remove output failed", "item_id", item.ID, "error", err)
	}
}

func (s *Server) markRemoved(ctx context.Context, hashOrPublicID string) error {
	// Sonarr/Radarr pass the real torrent infohash on DELETE (the value emitted
	// in torrents/info → hash field). Multiple items can share the same
	// info_hash (re-grab after a previous removal), so we must remove ALL
	// non-removed items with this hash — a LIMIT 1 lookup would silently
	// hit the already-removed duplicate and skip the active one.
	items, err := s.store.GetItemsByInfoHash(ctx, hashOrPublicID)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		// Fall back to publicID for NZB items where publicID is the hash.
		item, err := s.store.GetItemByPublicID(ctx, hashOrPublicID)
		if err != nil {
			return err
		}
		if item != nil {
			items = []*store.Item{item}
		}
	}
	for _, item := range items {
		if item.State == store.StateRemoved {
			continue
		}
		// COR-3: mark download-side .strm as consumed before removal so
		// the reconciler does not recreate it if the state transition is
		// delayed or the item is later inspected in a non-terminal state.
		if err := s.store.MarkDownloadConsumed(ctx, item.ID); err != nil {
			s.log.Warn("mark download consumed failed (non-fatal)", "item_id", item.ID, "error", err)
		}
		s.removeOutput(ctx, item)
		s.log.Info("item marked for removal", "item_id", item.ID, "public_id", item.PublicID)
		if err := s.store.UpdateItemState(ctx, item, store.StateRemoved, "remove requested via API"); err != nil {
			return err
		}
	}
	return nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func submissionFingerprint(sourceType store.SourceType, clientKind store.ClientKind, category, sourceURI, infoHash string) string {
	parts := []string{
		string(sourceType), string(clientKind),
		strings.TrimSpace(category),
		normalizeSourceURI(sourceURI),
		normalizeInfoHash(infoHash),
	}
	return util.SHA1Hex(strings.Join(parts, "|"))
}

func normalizeSourceURI(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	parsed, err := url.Parse(v)
	if err != nil {
		return strings.ToLower(v)
	}
	parsed.Fragment = ""
	values := parsed.Query()
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := url.Values{}
	for _, key := range keys {
		items := append([]string(nil), values[key]...)
		sort.Strings(items)
		for _, item := range items {
			out.Add(key, item)
		}
	}
	parsed.RawQuery = out.Encode()
	return strings.ToLower(parsed.String())
}

func normalizeInfoHash(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}

func extractInfoHash(source string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return ""
	}
	if parsed, err := url.Parse(source); err == nil && strings.EqualFold(parsed.Scheme, "magnet") {
		for _, xt := range parsed.Query()["xt"] {
			const prefix = "urn:btih:"
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(xt)), prefix) {
				return normalizeBTIH(xt[len(prefix):])
			}
		}
	}
	return normalizeBTIH(source)
}

func normalizeBTIH(v string) string {
	v = strings.TrimSpace(v)
	switch len(v) {
	case 40:
		n := strings.ToLower(v)
		if _, err := hex.DecodeString(n); err == nil {
			return n
		}
	case 32:
		decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(v))
		if err == nil {
			return strings.ToLower(hex.EncodeToString(decoded))
		}
	}
	return ""
}

func deriveDisplayName(displayName, sourceURI, infoHash, fallback string) string {
	displayName = strings.TrimSpace(displayName)
	// A display name equal to the infohash is degenerate (Sonarr's parser
	// rejects hashed release titles and auto-removes the download); try to
	// recover the real name from the source magnet's dn before accepting it.
	if displayName != "" && !strings.EqualFold(displayName, strings.TrimSpace(infoHash)) {
		return displayName
	}
	if dn := magnetDisplayName(sourceURI); dn != "" {
		return dn
	}
	switch {
	case displayName != "":
		return displayName
	case strings.TrimSpace(infoHash) != "":
		return strings.TrimSpace(infoHash)
	case strings.TrimSpace(sourceURI) != "":
		return strings.TrimSpace(sourceURI)
	default:
		return fallback
	}
}

// magnetDisplayName returns the dn (display name) parameter of a magnet URI,
// or "" for non-magnets / magnets without one.
func magnetDisplayName(uri string) string {
	parsed, err := url.Parse(strings.TrimSpace(uri))
	if err != nil || !strings.EqualFold(parsed.Scheme, "magnet") {
		return ""
	}
	return strings.TrimSpace(parsed.Query().Get("dn"))
}

func qbitCookieSecure(baseURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	return err == nil && strings.EqualFold(parsed.Scheme, "https")
}

// filterItemsByHashes matches items against a pipe-separated list of hashes
// that Sonarr sends via ?hashes= on /api/v2/torrents/info.
// Sonarr always uses the real torrent infohash (from the magnet) as the
// lookup key — never our internal PublicID. Match against InfoHash first,
// fall back to PublicID for NZB items that have no infohash.
// All comparisons are case-insensitive (Sonarr may send uppercase).
func filterItemsByHashes(items []*store.Item, hashes []string) []*store.Item {
	if len(hashes) == 0 {
		return items
	}
	set := map[string]struct{}{}
	for _, hash := range hashes {
		set[strings.ToLower(hash)] = struct{}{}
	}
	filtered := make([]*store.Item, 0, len(items))
	for _, item := range items {
		matched := false
		if item.InfoHash != nil && *item.InfoHash != "" {
			if _, ok := set[strings.ToLower(*item.InfoHash)]; ok {
				matched = true
			}
		}
		if !matched {
			if _, ok := set[strings.ToLower(item.PublicID)]; ok {
				matched = true
			}
		}
		if matched {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func (s *Server) qbitCategories(r *http.Request) (map[string]compat.QBitCategory, error) {
	payload := map[string]compat.QBitCategory{}
	// Read actual dirs from the container-internal root (cfg.Data.Strm = /data).
	// Report savePath using the host-visible prefix (/mnt/darkharrbor) so Sonarr
	// can verify the category path exists from its own bind-mount perspective.
	entries, err := os.ReadDir(s.cfg.Data.Strm)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		cat := strings.TrimSpace(entry.Name())
		// Skip hidden/internal dirs (.staging, .sidecar, .cache etc.)
		if cat != "" && !strings.HasPrefix(cat, ".") {
			payload[cat] = compat.ProjectQBitCategory(cat, filepath.Join(s.cfg.Data.ReportedPathPrefix, cat))
		}
	}
	items, err := s.store.ListVisibleClientItems(r.Context(), store.ClientKindQBit, "", 1000)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if _, ok := payload[item.Category]; !ok {
			payload[item.Category] = compat.ProjectQBitCategory(item.Category, filepath.Join(s.cfg.Data.ReportedPathPrefix, item.Category))
		}
	}
	if len(payload) == 0 {
		cat := s.cfg.Compatibility.DefaultCategory
		payload[cat] = compat.ProjectQBitCategory(cat, filepath.Join(s.cfg.Data.ReportedPathPrefix, cat))
	}
	return payload, nil
}

type sabCategoryConfig struct{ Name, Dir string }

func (s *Server) sabCategoryNames(r *http.Request) []string {
	configs := s.sabCategoryConfigs(r)
	names := make([]string, 0, len(configs))
	for _, c := range configs {
		names = append(names, c.Name)
	}
	return names
}

func (s *Server) sabCategoryConfigs(r *http.Request) []sabCategoryConfig {
	seen := map[string]sabCategoryConfig{}
	add := func(name, dir string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if _, ok := seen[name]; !ok {
			seen[name] = sabCategoryConfig{Name: name, Dir: dir}
		}
	}
	for _, c := range s.defaultSABCategories() {
		add(c.Name, c.Dir)
	}
	entries, err := os.ReadDir(s.cfg.Data.Strm)
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				add(entry.Name(), entry.Name())
			}
		}
	}
	items, err := s.store.ListVisibleClientItems(r.Context(), store.ClientKindSAB, "", 1000)
	if err == nil {
		for _, item := range items {
			add(item.Category, item.Category)
		}
	}
	categories := make([]sabCategoryConfig, 0, len(seen))
	if def, ok := seen["*"]; ok {
		categories = append(categories, def)
		delete(seen, "*")
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		categories = append(categories, seen[name])
	}
	return categories
}

func (s *Server) defaultSABCategories() []sabCategoryConfig {
	def := strings.TrimSpace(s.cfg.Compatibility.DefaultCategory)
	cats := []sabCategoryConfig{
		{Name: "*"},
		{Name: "movies", Dir: "movies"},
		{Name: "tv", Dir: "tv"},
		{Name: "audio", Dir: "audio"},
		{Name: "software", Dir: "software"},
	}
	if def != "" && def != "*" {
		cats = append(cats, sabCategoryConfig{Name: def, Dir: def})
	}
	return cats
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

func withLogging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Debug("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(started).String(),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += n
	return n, err
}

func requireMethod(method string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func getOnly(next http.Handler) http.Handler  { return requireMethod(http.MethodGet, next) }
func postOnly(next http.Handler) http.Handler { return requireMethod(http.MethodPost, next) }

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func providerCaps(p provider.Provider) provider.Capabilities {
	if p == nil {
		return provider.Capabilities{}
	}
	return p.Capabilities()
}

// pickUsenetProvider resolves which configured usenet provider should serve
// item: its persisted Provider field (set at resolve time by
// cmd/darkharrbor/main.go's resolveNZBItem/resolveRARItem once a provider
// successfully fetches the content) if present and still configured;
// otherwise the first provider in usenetOrder (matches the pre-existing
// single-provider behavior for items resolved before this field was wired
// up, and for the resolve path's own first attempt). Returns (name, nil,
// false) only when no usenet provider is configured at all.
func (s *Server) pickUsenetProvider(item *store.Item) (name string, up provider.UsenetProvider, ok bool) {
	if item != nil && item.Provider != nil && *item.Provider != "" {
		if p, exists := s.usenetProviders[*item.Provider]; exists {
			return *item.Provider, p, true
		}
		// Item's recorded provider is no longer configured (e.g. removed
		// from HARRBOR_USENET_PROVIDERS since it resolved) — fall through
		// to the default rather than failing the stream outright.
	}
	if len(s.usenetOrder) == 0 {
		return "", nil, false
	}
	first := s.usenetOrder[0]
	p, exists := s.usenetProviders[first]
	if !exists {
		return "", nil, false
	}
	return first, p, true
}

// unzipManifestFromItem deserialises the ZIPManifest JSON from an item.
func unzipManifestFromItem(item *store.Item) ([]nntp.ZIPEntry, error) {
	if item.ZIPManifest == nil {
		return nil, fmt.Errorf("no ZIP manifest")
	}
	return nntp.UnmarshalZIPManifest(*item.ZIPManifest)
}

// resolveCDNZIPEntryOffset reads the ZIP local file header for an entry to
// find the actual data offset (30 + fnLen + extraLen bytes past LocalHeaderOff).
// Makes one HTTP range request of 30 bytes; safe to call on every play since
// the offset doesn't change for a given CDN ZIP.
func resolveCDNZIPEntryOffset(ctx context.Context, e nntp.ZIPEntry, cdnURL string) nntp.ZIPEntry {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cdnURL, nil)
	if err != nil {
		return e
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", e.LocalHeaderOff, e.LocalHeaderOff+29))
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return e
	}
	defer resp.Body.Close()
	hdr := make([]byte, 30)
	if _, err := io.ReadFull(resp.Body, hdr); err != nil {
		return e
	}
	if len(hdr) < 30 {
		return e
	}
	fnLen := int(hdr[26]) | int(hdr[27])<<8
	extraLen := int(hdr[28]) | int(hdr[29])<<8
	e.DataOff = e.LocalHeaderOff + 30 + int64(fnLen) + int64(extraLen)
	return e
}

// pickDebridProvider resolves which debrid provider should handle item operations
// (Submit, Poll, RequestDownloadURL, Remove, CheckCached). Uses item.Provider
// when set and still configured; falls back to s.prov (the first provider in
// providerOrder) for backwards compatibility with pre-P6 items.
//
// Each provider is fully independent — no cross-provider fallback here.
// Provider selection happens once at submission time (submitItem/submitDedup)
// and is persisted on item.Provider; all subsequent operations use that binding.
func (s *Server) pickDebridProvider(item *store.Item) provider.Provider {
	if item != nil && item.Provider != nil && *item.Provider != "" {
		name := *item.Provider
		// Only route through allProviders for debrid (torrent) items.
		// Usenet items use pickUsenetProvider; their Provider field names
		// a usenet provider, not a debrid one.
		if item.SourceType == store.SourceTypeTorrent {
			if p, exists := s.allProviders[name]; exists {
				return p
			}
			// Item's recorded provider no longer configured — fall through to default.
		}
	}
	return s.prov // default: first provider in providerOrder
}

func splitCSV(v string) []string {
	raw := strings.Split(strings.TrimSpace(v), ",")
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		item = strings.TrimSpace(item)
		if item != "" && item != "all" {
			out = append(out, item)
		}
	}
	return out
}

func splitPipe(v string) []string {
	raw := strings.Split(strings.TrimSpace(v), "|")
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		item = strings.TrimSpace(item)
		if item != "" && item != "all" {
			out = append(out, item)
		}
	}
	return out
}

func parseBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func parseInt(v string, fallback int) int {
	v = strings.TrimSpace(v)
	neg := false
	if v == "" {
		return fallback
	}
	if v[0] == '-' {
		neg = true
		v = v[1:]
	}
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return fallback
		}
		n = n*10 + int(c-'0')
	}
	if neg {
		n = -n
	}
	return n
}

func multipartFiles(form *multipart.Form, keys ...string) []*multipart.FileHeader {
	if form == nil {
		return nil
	}
	var files []*multipart.FileHeader
	for _, key := range keys {
		files = append(files, form.File[key]...)
	}
	return files
}

// handleStreamProxy handles GET and HEAD /stream/{item_id}/{file_id}.
// materialization. HEAD is allowed for NZB items — it returns Content-Type,
// Content-Length (item.TotalSize), and Accept-Ranges without opening any NNTP
// connection. This lets ffmpeg probe terminate immediately instead of reading
// GET requests: cache-eviction guard → materialize (idempotent) → byte proxy.
// accountNZBStreamResult records the outcome of an NZB/NNTP stream attempt for
// the dead-letter breaker (Surface 3). A clean serve clears prior strikes; a
// genuine content failure (error while the request context is still live) counts
// toward the dead threshold; a client abort (request ctx cancelled, e.g. Jellyfin
// seek/stop/disconnect) is neutral. Bookkeeping writes use a detached context so a
// client disconnect cannot abort them.
func (s *Server) accountNZBStreamResult(ctx context.Context, r *http.Request, item *store.Item, streamErr error) {
	writeCtx := context.WithoutCancel(ctx)
	if streamErr == nil {
		if rerr := s.store.ResetStreamFailure(writeCtx, item.ID); rerr != nil {
			s.log.Warn("stream: reset stream failure failed", "item_id", item.ID, "error", rerr)
		}
		return
	}
	if r.Context().Err() != nil {
		s.log.Debug("stream: nntp client aborted (not counted)", "item_id", item.ID, "error", streamErr)
		return
	}
	s.log.Warn("stream: nntp content failure", "item_id", item.ID, "error", streamErr)
	dead, derr := s.store.IncrementStreamFailure(writeCtx, item.ID)
	if derr != nil {
		s.log.Warn("stream: increment stream failure failed", "item_id", item.ID, "error", derr)
		return
	}
	if dead {
		s.log.Warn("stream: nntp item entered dead window, further plays short-circuit (NNTP spared)", "item_id", item.ID, "display_name", item.DisplayName)
	}
}

func (s *Server) handleStreamProxy(w http.ResponseWriter, r *http.Request) {
	playbackStarted := s.nowUTC()
	// Reject anything that is not GET or HEAD up front.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	itemID := r.PathValue("item_id")
	fileID := r.PathValue("file_id")
	if itemID == "" || fileID == "" {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	// Tokens are embedded in .strm URLs by output.Write(). They are static
	// HMAC(secret, item:file) values with NO expiry (output.VerifyStreamToken)
	// -- library .strm files must keep playing indefinitely. A missing or
	// invalid token returns 403.
	if s.cfg.Server.StreamSecret != "" {
		tok := r.URL.Query().Get("tok")
		if tok == "" || !output.VerifyStreamToken(s.cfg.Server.StreamSecret, itemID, fileID, tok) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
	}

	ctx := r.Context()
	item, err := s.store.GetItemByID(ctx, itemID)
	if err != nil {
		s.log.Warn("stream: db error", "item_id", itemID, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if item == nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	// StateReady and StateResolving are always streamable.
	// StateRemoved is streamable for TorBox torrent items — Sonarr deletes the
	// torrent from the queue after import, but the CDN URL remains obtainable
	// via RequestDownloadURL. It is equally streamable for HTTP items: their
	// resolve_key is permanent and the source is re-resolved live at play
	// time (D13). For NZB/NNTP items, StateRemoved means the segment cache
	// was freed, so 404 is correct.
	if item.State == store.StateRemoved && item.SourceType == store.SourceTypeNZB {
		s.log.Warn("stream: removed nzb item not streamable", "item_id", itemID)
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	if item.State != store.StateReady && item.State != store.StateResolving && item.State != store.StateRemoved {
		s.log.Warn("stream: item not streamable", "item_id", itemID, "state", item.State)
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	if r.Method == http.MethodGet {
		ctx = playbackcoverage.WithRepresentation(ctx, s.playbackRepresentationID(ctx, item, fileID))
		ctx = withPlaybackProposalTarget(ctx, item, fileID, 0)
		ctx = rangecache.WithPlaybackTelemetry(ctx, string(item.SourceType), playbackStarted)
		r = r.WithContext(ctx)
	}

	// HTTP items: always DH's live byte relay from the re-resolved source —
	// never DirectStream, never the CDN/rangecache paths (D4, HS-1.7).
	if item.SourceType == store.SourceTypeHTTP {
		s.streamHTTPItem(w, r, item)
		return
	}

	// NZB items are served via the usenet provider (NNTP lane) UNLESS they were
	// fulfilled through the TorBox usenet cache, in which case they carry a
	// remote/queued id and fall through to the TorBox CDN path below (same as
	// torrents). The NNTP lane never sets a remote/queued id.
	if item.SourceType == store.SourceTypeNZB && item.RemoteID == nil && item.QueuedID == nil {
		usenetName, usenetProv, usenetOK := s.pickUsenetProvider(item)
		if !usenetOK {
			s.log.Warn("stream: NZB item but usenet provider not configured", "item_id", itemID)
			http.Error(w, "NNTP provider not configured", http.StatusServiceUnavailable)
			return
		}
		if item.SourceURI == nil || *item.SourceURI == "" {
			http.Error(w, "NZB data not found", http.StatusNotFound)
			return
		}

		// any NNTP connection. ffmpeg issues HEAD before GET to learn the file
		// size and Content-Type; answering HEAD immediately prevents it from
		// issuing multiple parallel GET probes that saturate the NNTP pool.
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Type", "video/x-matroska")
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Cache-Control", "no-store")
			if item.TotalSize > 0 {
				w.Header().Set("Content-Length", strconv.FormatInt(item.TotalSize, 10))
			}
			w.WriteHeader(http.StatusOK)
			return
		}

		// Update last_played_at for NZB items (no janitor timer needed — nothing to delete from TorBox)
		if err := s.persistLastPlayedAt(ctx, item); err != nil {
			s.log.Warn("stream: update last_played_at failed", "item_id", item.ID, "error", err)
		}

		// Dead-letter breaker (Surface 3): if an item's NNTP stream has repeatedly
		// failed with content-gone, short-circuit before touching the provider so a
		// dead release stops hammering NNTP. The window auto-expires for a later retry.
		if dead, derr := s.store.IsStreamDead(ctx, item.ID); derr != nil {
			s.log.Warn("stream: dead-check failed", "item_id", itemID, "error", derr)
		} else if dead {
			s.log.Info("stream: nntp item dead, short-circuiting (NNTP spared)", "item_id", itemID)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error":       "stream_dead",
				"retry_after": "7d",
			})
			return
		}
		// manifest-JSON streaming method (provider.UsenetProvider; the adapter
		// unmarshals and delegates to the concrete StreamRAR).
		contentKey := s.nntpContentKey(ctx, item)
		fileIndex := 0
		if idx, idxErr := strconv.Atoi(fileID); idxErr == nil && idx >= 0 {
			fileIndex = idx
		}
		s.prefetchNNTPSeek(ctx, item, usenetProv, fileIndex, contentKey, r.Header.Get("Range"))
		if item.IsRARSplit() {
			s.log.Info("stream: nntp RAR play started",
				"item_id", itemID,
				"display_name", item.DisplayName,
				"usenet_provider", usenetName,
			)
			var streamErr error
			if encryptedProv, ok := usenetProv.(encryptedRARStreamProvider); ok {
				streamErr = encryptedProv.StreamEncryptedRARManifest(ctx, *item.RarManifest, item.TotalSize, w, r.Header.Get("Range"), item.ID, contentKey, nntpRARPassword(item))
			} else {
				streamErr = usenetProv.StreamRARManifest(ctx, *item.RarManifest, item.TotalSize, w, r.Header.Get("Range"), item.ID, contentKey)
			}
			s.accountNZBStreamResult(ctx, r, item, streamErr)
			return
		}

		// ZIP archive streaming — central-directory manifest, one entry per video file.
		// fileID is the ZIPEntry.EntryIdx (same slot as NZB file index).
		if item.IsZIPArchive() {
			s.log.Info("stream: nntp ZIP play started",
				"item_id", itemID,
				"display_name", item.DisplayName,
				"usenet_provider", usenetName,
				"entry_idx", fileIndex,
			)
			streamErr := usenetProv.StreamZIPEntry(ctx, *item.ZIPManifest, fileIndex, w, r.Header.Get("Range"), item.ID, contentKey)
			s.accountNZBStreamResult(ctx, r, item, streamErr)
			return
		}

		if item.IsSevenZipArchive() {
			sevenZipProv, ok := usenetProv.(interface {
				StreamSevenZipEntry(context.Context, string, int, http.ResponseWriter, string, string, string) error
			})
			if !ok {
				http.Error(w, "7z streaming unavailable", http.StatusServiceUnavailable)
				return
			}
			s.log.Info("stream: nntp 7z play started",
				"item_id", itemID,
				"usenet_provider", usenetName,
				"entry_idx", fileIndex,
			)
			streamErr := sevenZipProv.StreamSevenZipEntry(ctx, *item.Metadata.SevenZipManifest, fileIndex, w, r.Header.Get("Range"), item.ID, contentKey)
			s.accountNZBStreamResult(ctx, r, item, streamErr)
			return
		}

		// Direct mkv NZB — stream by file index.
		s.log.Info("stream: nntp play started",
			"item_id", itemID,
			"display_name", item.DisplayName,
			"usenet_provider", usenetName,
		)
		streamErr := usenetProv.Stream(ctx, item.ID, []byte(*item.SourceURI), fileIndex, w, r.Header.Get("Range"))
		s.accountNZBStreamResult(ctx, r, item, streamErr)
		return
	}

	// TS-1.3: a store-mode RAR torrent is one logical media file backed by
	// URL-free payload spans across provider-hosted archive volumes.
	if isTorrentRARFileID(fileID) {
		s.handleTorrentRAR(w, r, item, fileID)
		return
	}

	// B-N2: HEAD for torrent items is answered from DB metadata — no provider
	// calls. Content-Type from file extension, Content-Length from file_list.
	if r.Method == http.MethodHead {
		entries := parseFileList(item.FileList)
		var size int64
		var ct string
		if e, ok := fileListEntryForID(entries, fileID); ok {
			size = e.Size
			ct = fileExtContentType(e.Name)
		} else {
			// Fall back to item-level size if file_list unavailable.
			size = item.TotalSize
			ct = "video/x-matroska"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Cache-Control", "no-store")
		if size > 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	// resolveForStream returns the CDN URL + metadata for a torrent (item, file)
	// pair.  It is called at most once per (item, file) per resolveTTL thanks to
	// the B-O1 cache; concurrent GETs share one call via the B-N1 singleflight.
	type resolveResult struct {
		cdnURL       string
		contentType  string
		total        int64
		rangeCapable *bool
		remoteID     string
	}
	resolveForStream := func() (*resolveResult, error) {
		// Eviction checks are valid only for providers with a real cache
		// oracle. No-oracle providers use URL materialization/repair as their
		// availability authority.
		if item.InfoHash != nil {
			cacheEvicted, chkErr := providerCacheEvicted(ctx, s.pickDebridProvider(item), item)
			if chkErr != nil {
				return nil, fmt.Errorf("checkcached: %w", chkErr)
			}
			if cacheEvicted {
				if blerr := s.store.IncrementFailCount(ctx, *item.InfoHash); blerr != nil {
					s.log.Warn("stream: increment fail count", "item_id", itemID, "error", blerr)
				}
				evictMsg := "cache_evicted"
				item.ErrorMessage = &evictMsg
				if uerr := s.store.UpdateItemState(ctx, item, store.StateFailed, evictMsg); uerr != nil {
					s.log.Warn("stream: update state after eviction", "item_id", itemID, "error", uerr)
				}
				s.removeOutput(ctx, item)
				s.invalidateResolveEntry(itemID, fileID)
				s.log.Info("stream: cache evicted", "item_id", itemID, "hash", *item.InfoHash)
				return nil, fmt.Errorf("cache_evicted")
			}
			if blacklisted, blerr := s.store.IsBlacklisted(ctx, *item.InfoHash); blerr != nil {
				s.log.Warn("stream: blacklist check failed", "item_id", itemID, "error", blerr)
			} else if blacklisted {
				s.log.Info("stream: hash blacklisted", "item_id", itemID, "hash", *item.InfoHash)
				return nil, fmt.Errorf("blacklisted")
			}
		}

		// Materialize: if item already has a remote_id TorBox returns the same
		// id (~270ms); if not (first play or post-janitor) a new entry is created.
		var remoteID string
		if item.RemoteID != nil && *item.RemoteID != "" {
			remoteID = *item.RemoteID
		} else {
			resp, createErr := s.pickDebridProvider(item).Submit(ctx, item, provider.SubmitOptions{AddOnlyIfCached: true})
			if createErr != nil {
				return nil, fmt.Errorf("materialize: %w", createErr)
			}
			remoteID = resp.RemoteID
			item.RemoteID = &remoteID
			item.UpdatedAt = time.Now().UTC()
			if uerr := s.store.UpdateItem(ctx, item); uerr != nil {
				s.log.Warn("stream: persist remote_id", "item_id", itemID, "error", uerr)
			}
		}

		// CDN-ZIP: compound fileID "<archiveFileID>:zip:<entryIdx>" signals
		// that this item is a TorBox-cached ZIP. Extract the archive file's
		// CDN URL, then serve the requested entry's byte range directly.
		if strings.Contains(fileID, ":zip:") {
			parts := strings.SplitN(fileID, ":zip:", 2)
			archiveFileID := parts[0]
			entryIdx := 0
			if len(parts) == 2 {
				if n, err := strconv.Atoi(parts[1]); err == nil {
					entryIdx = n
				}
			}
			if item.ZIPManifest == nil || *item.ZIPManifest == "" {
				return nil, fmt.Errorf("cdnzip: no ZIP manifest on item")
			}
			manifest, merr := unzipManifestFromItem(item)
			if merr != nil || entryIdx >= len(manifest) {
				return nil, fmt.Errorf("cdnzip: invalid manifest or entry index")
			}
			entry := manifest[entryIdx]
			// Get fresh CDN URL for the archive file.
			archiveCDNURL, urlErr := s.pickDebridProvider(item).RequestDownloadURL(ctx, item, archiveFileID)
			if urlErr != nil || archiveCDNURL == "" {
				return nil, fmt.Errorf("cdnzip: requestdl for archive: %w", urlErr)
			}
			// Resolve DataOff if not yet set (lazy resolution on first play).
			if entry.DataOff == 0 {
				entry = resolveCDNZIPEntryOffset(ctx, entry, archiveCDNURL)
			}
			// Serve the entry's byte range from the CDN URL.
			return &resolveResult{
				cdnURL:      archiveCDNURL,
				contentType: "video/x-matroska",
				total:       entry.Uncompressed,
				remoteID:    remoteID,
			}, nil
		}

		// B-O3: pace the actual network hit (followToCDN preflight) — not the
		// RequestDownloadURL call, which is a pure URL construction with no
		// network I/O of its own.
		requestDLURL, rdlErr := s.pickDebridProvider(item).RequestDownloadURL(ctx, item, fileID)
		if rdlErr != nil || requestDLURL == "" {
			return nil, fmt.Errorf("requestdl: %w", rdlErr)
		}

		// followToCDN turns requestdl URL into the clean token-free CDN URL.
		// The preflight is the real network call; it is now inside the
		// singleflight so N concurrent GETs share one preflight (B-N1 + B-O3).
		cdnURL := requestDLURL
		if sameSchemeAndHost(requestDLURL, s.cfg.TorBox.BaseURL) {
			noRedirect := &http.Client{
				CheckRedirect: func(req *http.Request, via []*http.Request) error {
					return http.ErrUseLastResponse
				},
				Timeout: 10 * time.Second,
			}
			preflight, preErr := http.NewRequestWithContext(ctx, http.MethodGet, requestDLURL, nil) // #nosec G704 -- same-origin validated above.
			if preErr == nil {
				if preResp, preRespErr := noRedirectDo(noRedirect, preflight); preRespErr == nil { // #nosec G704
					_ = preResp.Body.Close()
					// B-N7: 404 from CDN is also a reresolve trigger (A-B7 adds
					// the per-incident reset; handled in reresolve closure below).
					if preResp.StatusCode == http.StatusNotFound {
						// Invalidate so next GET re-runs the full resolve.
						s.invalidateResolveEntry(itemID, fileID)
						return nil, fmt.Errorf("cdn_preflight_404")
					}
					if loc := preResp.Header.Get("Location"); loc != "" {
						cdnURL = loc
					}
				}
			}
		} else {
			s.log.Warn("stream: requestdl URL rejected (not TorBox origin)", "item_id", itemID)
		}

		// Probe the CDN URL for content-type + total size (needed by rangecache).
		// Re-uses the CDN response from initCDNSource; here we do a lightweight
		// HEAD to get headers without the full chunk-source setup overhead.
		var contentType string
		var total int64
		if entries := parseFileList(item.FileList); len(entries) > 0 {
			if e, ok := fileListEntryForID(entries, fileID); ok {
				total = e.Size
				contentType = fileExtContentType(e.Name)
			}
		}
		if total <= 0 {
			total = item.TotalSize
		}

		return &resolveResult{
			cdnURL:      cdnURL,
			contentType: contentType,
			total:       total,
			remoteID:    remoteID,
		}, nil
	}

	// B-O1: check the short-lived resolve cache first. A hit means we already
	// have a live CDN URL for this (item, file) — skip all TorBox calls.
	// B-N1: on a cache miss, run resolveForStream under a singleflight so that
	// N concurrent GETs (Jellyfin probe burst) share one resolve round-trip.
	var resolved *resolveResult
	if cached := s.resolveEntryFor(itemID, fileID); cached != nil {
		s.log.Debug("stream: resolve cache hit", "item_id", itemID, "file_id", fileID)
		resolved = &resolveResult{
			cdnURL:       cached.cdnURL,
			contentType:  cached.contentType,
			total:        cached.total,
			rangeCapable: cached.rangeCapable,
		}
	} else {
		v, sfErr, _ := s.resolveGroup.Do(resolveKey(itemID, fileID), func() (interface{}, error) {
			// Double-check cache inside the singleflight — a prior concurrent
			// call may have just populated it.
			if e := s.resolveEntryFor(itemID, fileID); e != nil {
				return &resolveResult{cdnURL: e.cdnURL, contentType: e.contentType, total: e.total, rangeCapable: e.rangeCapable}, nil
			}
			return resolveForStream()
		})
		if sfErr != nil {
			errStr := sfErr.Error()
			switch errStr {
			case "cache_evicted":
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "cache_evicted"})
			case "blacklisted":
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "blacklisted", "retry_after": "7d"})
			case "cdn_preflight_404":
				s.log.Warn("stream: cdn preflight 404, resolve cache invalidated", "item_id", itemID)
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "cdn_unavailable"})
			default:
				safeErr := httpstream.Sanitize(sfErr)
				s.log.Warn("stream: resolve failed", "item_id", itemID, "error", safeErr)
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": safeErr})
			}
			return
		}
		resolved = v.(*resolveResult)
		// Populate cache so the next GET within resolveTTL is free.
		s.cacheResolveEntry(itemID, fileID, resolved.cdnURL, resolved.contentType, resolved.total, resolved.rangeCapable)
	}

	cdnURL := resolved.cdnURL

	// Probe requests (the ffprobe relay tags its -i URL with probe=1) are
	// import-time metadata reads, not playback:
	//   - they must NOT count as a play (LastPlayedAt / janitor arm) — a
	//     probe-armed janitor is a false lifecycle signal for content the
	//     user never watched;
	//   - they retain the same bounded relay concurrency and timeouts as before,
	//     but remain on DH's byte-proxy path so no upstream URL reaches ffprobe.
	isProbe := r.URL.Query().Get("probe") == "1"

	// reconciliation has an accurate timestamp.
	if !isProbe {
		if uerr := s.persistLastPlayedAt(ctx, item); uerr != nil {
			s.log.Warn("stream: update last_played_at", "item_id", itemID, "error", uerr)
		}

		cleanupDuration := time.Duration(s.cfg.Lifecycle.CleanupHours) * time.Hour
		s.ResetJanitorTimer(item.ID, cleanupDuration)
	}

	s.log.Info("stream: play started",
		"item_id", itemID,
		"file_id", fileID,
		"display_name", item.DisplayName,
		"probe", isProbe,
		"resolve_cached", s.resolveEntryFor(itemID, fileID) != nil,
	)

	// A-B7: cdnChunkSource resets its per-incident reresolved flag after a
	// successful retry, so this closure can serve every later URL expiry.
	reresolve := func(rctx context.Context) (string, error) {
		// Invalidate the cache so the next full GET re-runs the resolve.
		s.invalidateResolveEntry(itemID, fileID)
		fresh, rerr := s.pickDebridProvider(item).RequestDownloadURL(rctx, item, fileID)
		if rerr != nil || fresh == "" {
			return "", rerr
		}
		if !sameSchemeAndHost(fresh, s.cfg.TorBox.BaseURL) {
			return fresh, nil
		}
		noRedirect := &http.Client{
			CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse },
			Timeout:       10 * time.Second,
		}
		preflight, preErr := http.NewRequestWithContext(rctx, http.MethodGet, fresh, nil) // #nosec G704
		if preErr != nil {
			return fresh, nil
		}
		if preResp, preRespErr := noRedirectDo(noRedirect, preflight); preRespErr == nil { // #nosec G704
			_ = preResp.Body.Close()
			if loc := preResp.Header.Get("Location"); loc != "" {
				return loc, nil
			}
		}
		return fresh, nil
	}
	// TS-2.2 rung 4: build alternate-provider candidates from this item's
	// own file_list entry (name+size — the only portable cross-provider
	// file identity, since file IDs are provider-specific). Built only at
	// this real playback call site, never for prewarm/RAR/ZIP paths.
	var altCandidates []altProviderCandidate
	var fileSize int64
	var haveFileSize bool
	if entries := parseFileList(item.FileList); len(entries) > 0 {
		if e, ok := fileListEntryForID(entries, fileID); ok {
			fileSize, haveFileSize = e.Size, true
			if s.cfg.Governor.AltProviderFailoverEnabled {
				altCandidates = buildAltProviderCandidates(item, e.Name, e.Size, s.pickDebridProvider(item), s.allProviders, s.providerOrder,
					func(ctx context.Context, providerName, infoHash string) {
						s.recordHashAvailability(ctx, providerName, infoHash, true, availability.SourceStream)
					})
			}
		}
	}

	// TS-3.4 (T12): resolve this item's persisted TorrentMeta (TS-0.1) and
	// match this exact file by byte-exact size -- the same RealInfoHash-
	// then-InfoHash lookup and the same ambiguous-size-stays-unmatched
	// convention cmd/darkharrbor/main.go's TS-0.3 resolver and
	// output.matchTorrentMetaBySize already established. A miss at any
	// step (no persisted meta, no size, ambiguous size) is always
	// non-fatal: both stay nil, and streamViaCache's verification is then
	// a structural no-op, identical to every stream before this row.
	var tsMeta *torrentmeta.TorrentMeta
	var tsMetaFile *torrentmeta.FileEntry
	if haveFileSize {
		tmHash := item.Metadata.RealInfoHash
		if tmHash == "" && item.InfoHash != nil {
			tmHash = *item.InfoHash
		}
		if tmHash != "" {
			if tm, found, tmErr := s.store.GetTorrentMeta(ctx, tmHash); tmErr != nil {
				s.log.Warn("stream: torrentmeta lookup failed (continuing without T12 verification)", "item_id", itemID, "error", tmErr)
			} else if found {
				var matches []*torrentmeta.FileEntry
				for i := range tm.Files {
					if tm.Files[i].Size == fileSize {
						matches = append(matches, &tm.Files[i])
					}
				}
				if len(matches) == 1 {
					tsMeta = tm
					tsMetaFile = matches[0]
				}
			}
		}
	}

	cw := &countingWriter{ResponseWriter: w}
	if s.streamViaCache(cw, r, item, itemID, fileID, cdnURL, reresolve, func() { s.invalidateResolveEntry(itemID, fileID) }, resolved.total, resolved.contentType, resolved.rangeCapable, altCandidates, tsMeta, tsMetaFile) {
		// B-O4: meter CDN-proxied bytes.
		if cw.n > 0 && s.egressMeter != nil {
			s.egressMeter.AddCDNBytes(cw.n)
		}
		return
	}
	w = cw.ResponseWriter // restore for passthrough path (countingWriter not needed there)
	if cached := s.resolveEntryFor(itemID, fileID); cached != nil {
		cdnURL = cached.cdnURL // capability probing may have reresolved to a fresh node
	}

	// chunked copy via 10MB buffer. For clients that cannot follow redirects.
	proxyReq, err := http.NewRequestWithContext(ctx, http.MethodGet, cdnURL, nil)
	if err != nil {
		s.log.Warn("stream: proxy req build failed", "item_id", itemID, "error", httpstream.Sanitize(err))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
		proxyReq.Header.Set("Range", rangeHeader)
	}
	proxyResp, err := http.DefaultClient.Do(proxyReq)
	if err != nil {
		s.log.Warn("stream: proxy upstream failed", "item_id", itemID, "error", httpstream.Sanitize(err))
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = proxyResp.Body.Close() }()

	for _, hdr := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
		if v := proxyResp.Header.Get(hdr); v != "" {
			w.Header().Set(hdr, v)
		}
	}
	w.WriteHeader(proxyResp.StatusCode)
	buf := make([]byte, 10*1024*1024)
	copyCtx := ctx
	offset := int64(0)
	if proxyResp.StatusCode < 200 || proxyResp.StatusCode >= 300 {
		copyCtx = context.Background()
	} else if proxyResp.StatusCode == http.StatusPartialContent {
		var rangeErr error
		offset, rangeErr = mediaFlowContentRangeStart(proxyResp.Header.Get("Content-Range"))
		if rangeErr != nil {
			copyCtx = context.Background()
		}
	}
	n, copyErr := s.copyPlaybackBody(copyCtx, w, proxyResp.Body, buf, offset, "cdn_passthrough", nil)
	if copyErr != nil {
		// Headers already sent — cannot change status code. Log and move on.
		s.log.Warn("stream: proxy copy failed", "item_id", itemID, "error", httpstream.Sanitize(copyErr))
	}
	// B-O4: meter egress bytes.
	if n > 0 && s.egressMeter != nil {
		s.egressMeter.AddPassthroughBytes(n)
	}
}

func providerCacheEvicted(ctx context.Context, p provider.Provider, item *store.Item) (bool, error) {
	if !p.Capabilities().CacheOracle {
		return false, nil
	}
	result, err := p.CheckCached(ctx, item)
	if err != nil {
		return false, err
	}
	return !result.Cached, nil
}

func (s *Server) playbackRepresentationID(ctx context.Context, item *store.Item, fileID string) string {
	if item == nil || fileID == "" {
		return ""
	}
	var representationID string
	switch item.SourceType {
	case store.SourceTypeHTTP:
		representationID = httpRepresentationID(item.ID, fileID)
	case store.SourceTypeTorrent:
		representationID = torrentCrossLaneRepresentationID(item.ID, fileID)
	case store.SourceTypeNZB:
		index, err := strconv.Atoi(fileID)
		if err != nil || index < 0 {
			return ""
		}
		representationID = nntpCrossLaneRepresentationID(item.ID, index)
	default:
		return ""
	}
	if s != nil && s.representationConvergence != nil {
		canonical, found, err := s.representationConvergence.CanonicalRepresentationID(ctx, representationID)
		if err != nil {
			s.log.Warn("playback coverage canonical identity unavailable (non-fatal)", "error", httpstream.Sanitize(err))
		} else if found {
			return canonical
		}
	}
	return representationID
}

// ResetJanitorTimer cancels any existing cleanup timer for itemID and
// starts a new one that fires JanitorCleanup after d.
// Safe to call from any goroutine; no locks must be held by the caller.
func (s *Server) ResetJanitorTimer(itemID string, d time.Duration) {
	s.janitorMu.Lock()
	if cancel, ok := s.janitorTimers[itemID]; ok {
		cancel()
	}
	ctx, cancel := context.WithCancel(s.shutdownCtx)
	s.janitorTimers[itemID] = cancel
	s.janitorMu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
		}
		s.JanitorCleanup(itemID)
	}()
}

// JanitorCleanup removes the TorBox item from the account after the idle
// window expires, then clears remote_id in the DB so the proxy re-materializes
// on next play. Errors are logged and non-fatal — TorBox expires items itself
// after 30 days if cleanup fails.
func (s *Server) JanitorCleanup(itemID string) {
	// Remove from timer map first so concurrent proxy hits can start a new timer.
	s.janitorMu.Lock()
	delete(s.janitorTimers, itemID)
	s.janitorMu.Unlock()

	ctx, cancel := context.WithTimeout(s.shutdownCtx, 30*time.Second)
	defer cancel()

	item, err := s.store.GetItemByID(ctx, itemID)
	if err != nil {
		s.log.Warn("janitor: get item failed", "item_id", itemID, "error", err)
		return
	}
	if item == nil || item.RemoteID == nil {
		// Already cleaned up or never materialized.
		return
	}

	// BUGFIX 2026-07-01 (torbox API-hit audit / HANDOFF "what's next" #4,
	// "janitor delete-failure hardening"): a transient TorBox 5xx used to
	// be given exactly one attempt before falling back to "expires in 30
	// days on its own" -- for an uncached torrent, that's 30 days of
	// unnecessarily holding an Allowed Active Slot over what could be a
	// single dropped request. Bounded retry (2 extra attempts, short delay)
	// covers the transient case without turning this into an unbounded
	// loop; JanitorCleanup already only runs once per fired timer, not on
	// any ticking schedule, so there's no risk of this itself becoming a
	// hot-loop.
	debridProv := s.pickDebridProvider(item)
	cleanErr := debridProv.Remove(ctx, item)
	for attempt := 0; cleanErr != nil && attempt < 2; attempt++ {
		select {
		case <-ctx.Done():
			cleanErr = ctx.Err()
			attempt = 2 // stop retrying, ctx is gone
		case <-time.After(3 * time.Second):
			cleanErr = debridProv.Remove(ctx, item)
		}
	}
	if cleanErr != nil {
		s.log.Warn("janitor: controltorrent delete failed after retries",
			"item_id", itemID,
			"remote_id", *item.RemoteID,
			"source_type", item.SourceType,
			"error", cleanErr,
		)
		// Non-fatal: TorBox expires after 30 days. Clear remote_id anyway
		// so Dark Harrbor re-materializes on next play (idempotent createtorrent).
	}

	item.RemoteID = nil
	item.UpdatedAt = time.Now().UTC()
	if uerr := s.store.UpdateItem(ctx, item); uerr != nil {
		s.log.Warn("janitor: clear remote_id", "item_id", itemID, "error", uerr)
	}
	// Invalidate the resolve cache so the next /stream hit does a fresh resolve.
	s.resolveCache.Range(func(k, _ any) bool {
		if key, ok := k.(string); ok && len(key) >= len(itemID) && key[:len(itemID)] == itemID {
			s.resolveCache.Delete(key)
		}
		return true
	})
	s.log.Info("janitor: item cleaned up",
		"item_id", itemID,
		"display_name", item.DisplayName,
	)
}

// ─── Magnet cache helpers ─────────────────────────────────────────────────────

// magnetEntry is a cached (magnet, release title) pair keyed by infohash.
type magnetEntry struct {
	magnet string
	title  string
}

// magnetCacheSet stores infohash -> (magnet, title) in memory AND durably
// (C0.4, store.UpsertMagnetTitle) so a DarkHarrbor restart between a Sonarr
// search (the only time DH ever learns a release's real title for a
// tracker-only magnet) and a later grab (which carries only the hash) does
// not reintroduce hash-named releases. The title is the torznab release
// title recorded at search time — the ONLY reliable name source for
// tracker-only magnets with no dn: without it, items get hash display names,
// Sonarr's parser rejects them ("Rejected Hashed Release Title") and silently
// auto-removes the download minutes after grab (live 2026-07-08; the
// in-memory-only cache regressed this failure mode again on 2026-07-18 after
// a restart — see WORKLOG C0.4).
//
// The durable write is synchronous but small and bounded (one upsert + one
// bounded delete on an indexed column); it is not on any latency-critical
// playback path, only the torznab search-render path. ctx should be the
// originating request's context; a background context is acceptable when
// none is available (e.g. tests).
func (s *Server) magnetCacheSet(ctx context.Context, infohash, magnet, title string) {
	s.magnetCacheMu.Lock()
	if s.magnetCache == nil {
		s.magnetCache = make(map[string]magnetEntry)
	}
	if _, exists := s.magnetCache[infohash]; !exists {
		s.magnetCacheOrder = append(s.magnetCacheOrder, infohash)
	}
	s.magnetCache[infohash] = magnetEntry{magnet: magnet, title: title}
	trimInsertionCache(s.magnetCache, &s.magnetCacheOrder)
	s.magnetCacheMu.Unlock()

	if s.store != nil && infohash != "" {
		if err := s.store.UpsertMagnetTitle(ctx, infohash, magnet, title); err != nil {
			s.log.Warn("magnet title cache: durable write failed (non-fatal, memory cache still populated)",
				"info_hash", infohash, "error", err)
		}
	}
}

// magnetCacheGet retrieves a stored magnet URL by infohash: memory first,
// falling back to the durable store (C0.4) on a miss (e.g. after a restart),
// re-populating memory on a durable hit so subsequent lookups are fast.
func (s *Server) magnetCacheGet(ctx context.Context, infohash string) (string, bool) {
	s.magnetCacheMu.RLock()
	v, ok := s.magnetCache[infohash]
	s.magnetCacheMu.RUnlock()
	if ok {
		return v.magnet, true
	}
	if s.store == nil || infohash == "" {
		return "", false
	}
	magnet, title, dbOK, err := s.store.GetMagnetTitle(ctx, infohash)
	if err != nil {
		s.log.Warn("magnet title cache: durable read failed", "info_hash", infohash, "error", err)
		return "", false
	}
	if !dbOK {
		return "", false
	}
	s.magnetCacheMu.Lock()
	if s.magnetCache == nil {
		s.magnetCache = make(map[string]magnetEntry)
	}
	if _, exists := s.magnetCache[infohash]; !exists {
		s.magnetCacheOrder = append(s.magnetCacheOrder, infohash)
	}
	s.magnetCache[infohash] = magnetEntry{magnet: magnet, title: title}
	trimInsertionCache(s.magnetCache, &s.magnetCacheOrder)
	s.magnetCacheMu.Unlock()
	return magnet, true
}

// magnetCacheTitle retrieves the release title recorded for an infohash:
// memory first, falling back to the durable store (C0.4) on a miss.
func (s *Server) magnetCacheTitle(ctx context.Context, infohash string) string {
	infohash = strings.ToLower(strings.TrimSpace(infohash))
	s.magnetCacheMu.RLock()
	v, ok := s.magnetCache[infohash]
	s.magnetCacheMu.RUnlock()
	if ok {
		return v.title
	}
	if s.store == nil || infohash == "" {
		return ""
	}
	magnet, title, dbOK, err := s.store.GetMagnetTitle(ctx, infohash)
	if err != nil {
		s.log.Warn("magnet title cache: durable read failed", "info_hash", infohash, "error", err)
		return ""
	}
	if !dbOK {
		return ""
	}
	s.magnetCacheMu.Lock()
	if s.magnetCache == nil {
		s.magnetCache = make(map[string]magnetEntry)
	}
	if _, exists := s.magnetCache[infohash]; !exists {
		s.magnetCacheOrder = append(s.magnetCacheOrder, infohash)
	}
	s.magnetCache[infohash] = magnetEntry{magnet: magnet, title: title}
	trimInsertionCache(s.magnetCache, &s.magnetCacheOrder)
	s.magnetCacheMu.Unlock()
	return title
}

// ─── /download/{infohash} ─────────────────────────────────────────────────────

// handleDownload serves GET /download/{infohash}.
// The Torznab feed enclosure URL points here instead of raw magnets so that
// Sonarr never sees a bare magnet — it fetches this URL, gets a 302 to the
// full magnet (with trackers), and passes it to the qBit shim normally.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	infohash := strings.TrimPrefix(r.URL.Path, "/download/")
	infohash = strings.ToLower(strings.TrimSpace(infohash))
	if infohash == "" {
		http.Error(w, "missing infohash", http.StatusBadRequest)
		return
	}
	magnet, ok := s.magnetCacheGet(r.Context(), infohash)
	if !ok {
		magnet = "magnet:?xt=urn:btih:" + infohash
	}
	// Inject dn from the recorded release title when the magnet has none, so
	// downstream (Sonarr → qbit add) names the item parseably.
	if !strings.Contains(magnet, "dn=") {
		if title := s.magnetCacheTitle(r.Context(), infohash); title != "" {
			magnet += "&dn=" + url.QueryEscape(title)
		}
	}
	if identityContext := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("idc"))); len(identityContext) == 24 {
		if _, err := hex.DecodeString(identityContext); err == nil {
			magnet += "&x.dh.id=" + identityContext
		}
	}
	// Sonarr rejects magnets with no trackers before sending to qBit.
	// Append public trackers if none are present.
	if !strings.Contains(magnet, "&tr=") && !strings.Contains(magnet, "?tr=") {
		magnet += "&tr=udp%3A%2F%2Ftracker.opentrackr.org%3A1337%2Fannounce" +
			"&tr=udp%3A%2F%2Fopen.tracker.cl%3A1337%2Fannounce" +
			"&tr=udp%3A%2F%2Ftracker.torrent.eu.org%3A451%2Fannounce" +
			"&tr=udp%3A%2F%2Ftracker.moeking.me%3A6969%2Fannounce"
	}
	http.Redirect(w, r, magnet, http.StatusFound)
}

// validateCategory rejects empty or path-traversing category names before they
// are joined into a filesystem path. (PATCH-13 / G1)
func validateCategory(category string) error {
	if category == "" {
		return fmt.Errorf("category is required")
	}
	if strings.IndexByte(category, 0) >= 0 {
		return fmt.Errorf("category contains NUL")
	}
	if filepath.IsAbs(category) {
		return fmt.Errorf("category must not be an absolute path")
	}
	for _, part := range strings.Split(category, "/") {
		if part == ".." {
			return fmt.Errorf("category must not contain traversal segments")
		}
	}
	return nil
}

// resolveUnderRoot resolves candidate against root (relative candidates joined
// under root; absolute candidates taken as-is), returning the cleaned absolute
// path or an error if it escapes root. Containment guard for attacker-controlled
// save paths / category dirs. (PATCH-13 / G1)
func sameSchemeAndHost(rawURL, baseURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	base, err := url.Parse(baseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return false
	}
	return strings.EqualFold(u.Scheme, base.Scheme) && strings.EqualFold(u.Host, base.Host)
}

func resolveUnderRoot(root, candidate string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve root %q: %w", root, err)
	}
	p := candidate
	if !filepath.IsAbs(p) {
		p = filepath.Join(absRoot, p)
	}
	p = filepath.Clean(p)
	rel, err := filepath.Rel(absRoot, p)
	if err != nil {
		return "", fmt.Errorf("resolve rel %q: %w", p, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path %q escapes data root %q", p, absRoot)
	}
	return p, nil
}

// resolveSyntheticGrab maps a synthetic per-season infohash to the real torrent.
// feature_multiseason_grab), it returns the real magnet — used as the provider
// source URI and as the hash source for the cachegate — together with the real
// infohash. Otherwise it returns the inputs unchanged. The caller keeps the
// original (synthetic) infohash as the client-facing hash so Sonarr tracks the
// per-season release it grabbed.
func resolveSyntheticGrab(ctx context.Context, st *store.Store, sourceURI, infoHash string) (string, string) {
	if st == nil || infoHash == "" {
		return sourceURI, infoHash
	}
	m, err := st.GetSyntheticRelease(ctx, infoHash)
	if err != nil || m == nil {
		return sourceURI, infoHash
	}
	realURI := sourceURI
	if m.Magnet != "" {
		realURI = m.Magnet
	}
	realHash := infoHash
	if m.RealInfohash != "" {
		realHash = m.RealInfohash
	}
	return realURI, realHash
}

// submitDedup wraps the provider Submit in a singleflight keyed on the real
// torrent infohash (derived from the item's source URI). When Sonarr grabs
// every season of one cached multi-season torrent — each a distinct synthetic
// release sharing the same real magnet — the provider createtorrent runs
// exactly once, respecting TorBox creation limits. Items without an infohash
func (s *Server) submitDedup(ctx context.Context, item *store.Item, opts provider.SubmitOptions) (*provider.CreateTaskResponse, error) {
	key := ""
	if item != nil && item.SourceURI != nil {
		key = extractInfoHash(*item.SourceURI)
	}
	if key == "" && item != nil && item.Metadata.RealInfoHash != "" {
		// Synthetic grabs carry an HTTP source URI (no hash to extract);
		// the persisted real infohash is the dedup identity.
		key = item.Metadata.RealInfoHash
	}
	if key == "" {
		return s.pickDebridProvider(item).Submit(ctx, item, opts)
	}
	v, err, _ := s.submitGroup.Do(key, func() (interface{}, error) {
		return s.pickDebridProvider(item).Submit(ctx, item, opts)
	})
	if err != nil {
		return nil, err
	}
	resp, _ := v.(*provider.CreateTaskResponse)
	return resp, nil
}
