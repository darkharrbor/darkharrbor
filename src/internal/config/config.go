package config

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/topology"
)

const (
	defaultDataRoot     = "/data"
	defaultDatabasePath = "/config/darkharrbor.db"
	defaultQBitUser     = "admin"
	defaultServerAddr   = "127.0.0.1:8381"
	defaultCategory     = "darkharrbor"
	maxRepairBudgetMB   = int64((1<<63)-1) >> 20
	maxRepairBudgetSecs = int64((1<<63)-1) / int64(time.Second)
)

type Config struct {
	// tuning contains the strict, non-secret overrides written by
	// `darkharrbor configure`. Config.Lookup uses it ahead of legacy sealed
	// tunables while credentials remain sealed-only.
	tuning map[string]string
	// StartupWarnings reports tolerated legacy configuration that no longer
	// changes behavior. Callers log these after configuring the logger.
	StartupWarnings []string

	Topology struct {
		// Active is the named deployment profile. The profile is declarative:
		// selecting T2/T3 cannot enable capabilities absent from this binary.
		Active topology.Name
		// AchievedPostureStalenessHours bounds how long an observed aggregator
		// playback delivery substantiates T2/T3's universal-proxy claim.
		// Default 48h, not a week: the window is also the blind interval after
		// a posture revert, and the 2026-08-14 revert stayed invisible for two
		// days under a 168h window. A shorter window trades occasional WARN for
		// a light-usage operator against detecting a real revert promptly, and
		// the check is advisory rather than fatal.
		AchievedPostureStalenessHours int
	}

	Server struct {
		Address string
		BaseURL string
		// StreamSecret is the HMAC-SHA256 signing secret for stream tokens.
		// Required; must be at least 32 characters. Set via HARRBOR_STREAM_SECRET.
		StreamSecret string
	}

	Stremio StremioConfig

	MediaFlow struct {
		// Password authenticates MediaFlow-compatible URL generation and public
		// IP lookup. Empty keeps the compatibility surface fail-closed.
		Password string
		// PublicIP is an optional literal override. When empty, the MediaFlow
		// compatibility surface resolves and caches the public egress address.
		PublicIP string
		// BaseURL is the CLIENT-FACING origin used to build playback URLs.
		// Server.BaseURL is deliberately private to the container network, but
		// MediaFlow URLs are fetched by viewing devices, not by DarkHarrbor, so
		// an address correct from the server is wrong from the client and fails
		// opaquely at the player. Empty falls back to Server.BaseURL, preserving
		// existing single-network deployments byte-for-byte.
		BaseURL string
	}

	Database struct {
		Path        string
		BusyTimeout time.Duration
	}

	Backup struct {
		Enabled       bool
		Target        string
		IntervalHours int
		Keep          int
		SecretsPath   string
	}

	Data struct {
		Root string
		Strm string
		// ReportedPathPrefix is the path prefix under which Sonarr/Radarr see
		// Dark Harrbor's output directory. Dark Harrbor writes to /data/<category>/ inside
		// its container; Sonarr sees the same files at /mnt/darkharrbor/<category>/
		// via its /mnt bind mount. This prefix is used when reporting save_path
		// and content_path to the arr download client API so no remote path
		// mapping is required. Defaults to /data (container-internal, safe fallback).
		ReportedPathPrefix string
	}

	TorBox struct {
		BaseURL        string
		SearchBaseURL  string
		APIToken       string
		RequestTimeout time.Duration
		UserAgent      string
	}

	Auth struct {
		QBitUsername string
		QBitPassword string
		SABAPIKey    string
		SABNZBKey    string
		SessionTTL   time.Duration
	}

	Compatibility struct {
		QBitVersion     string
		QBitWebAPI      string
		SABVersion      string
		DefaultCategory string
	}

	Governor struct {
		// UncachedBudget is the max uncached transfers allowed in the rolling
		// 15-day window before new uncached submissions are blocked.
		UncachedBudget int
		// WindowDays is the rolling window size (TorBox uses 15).
		WindowDays int
		// StallTimeoutMin is how long (minutes) an uncached torrent may remain
		// stalled (no seeds / dead magnet) before the resolver fails it so the
		// arr can blocklist and re-grab. Default 30. (COR-11)
		StallTimeoutMin int
		// DeadSentinelGraceMin is how long (minutes) TorBox's own
		// checking+zero-seeders+100-day-sentinel-ETA signal may persist
		// before being treated as terminal. Shorter than StallTimeoutMin —
		// this signal was observed live to appear within seconds of an
		// uncached add and later resolve successfully, so it is not trusted
		// as instantly definitive, but it is still TorBox's own stronger
		// "no metadata/swarm found" signal, not a generic stall, so it does
		// not need the full stall timeout either. Default 5.
		DeadSentinelGraceMin int
		// RequireCached enforces that ALL submissions must be cached on TorBox.
		// When true (default), uncached grabs are rejected immediately with a
		// clear error regardless of the governor budget. Set to false only if
		// you deliberately want to allow uncached TorBox transfers.
		RequireCached bool
		// TorrentCDNCapacity bounds concurrent debrid-CDN fetch operations
		// (playback + TS-1.4 prewarm together) admitted through the shared
		// accountgov.Governor at once. This is NOT a provider rate limit
		// (TorBox 429 handling and the existing per-session
		// StreamReadaheadWorkers cap already own that) — it exists solely so
		// prewarm (accountgov.PriorityPrewarm) and live playback
		// (accountgov.PriorityPlayback) actually contend for something when
		// both run at once, so LC-06's fixed priority has a real effect
		// instead of staying dormant. Default 4 — generous enough that
		// today's single/dual-stream playback never queues (see WORKLOG
		// "TS-1.4"). 0 makes the class unbounded (no arbitration at all).
		TorrentCDNCapacity int
		// HTTPSourceCapacity bounds concurrent HTTP-lane operations: live
		// playback at accountgov.PriorityPlayback, D9 grab-time preflight at
		// PriorityGrab, and startup prewarm at PriorityPrewarm.
		// Default 0 leaves all three operation classes unbounded. A positive
		// value makes playback, preflight, and prewarm share that capacity.
		HTTPSourceCapacity int
		// AltProviderFailoverEnabled gates TS-2.2's T3 chunk-acquisition-
		// ladder rung 4: bounded alternate-provider same-hash midstream
		// failover. Default true. Set false to keep rung 4 a structural
		// no-op (identical to pre-TS-2.2 behavior) -- e.g. an operator who
		// does not want a second provider account touched by playback
		// recovery, even bounded and read-mostly.
		AltProviderFailoverEnabled bool
		// RepairEnabled gates TS-4.1's torrent-lane self-repair hierarchy
		// (frozen plan T5): same-provider re-add -> alternate-provider
		// re-add -> blacklist/re-search. Default true. Set false to leave a
		// stream-time exhaustion terminal exactly as before this row
		// (immediate blacklist-less failure) -- e.g. an operator who does not
		// want a second re-add attempt made automatically.
		RepairEnabled bool
		// RepairCooldownMin bounds how soon, in minutes, the SAME torrent
		// identity (infohash) may be repaired again after a repair attempt
		// (success or terminal blacklist) completes. Prevents a readahead
		// storm of concurrent chunk-fetch failures for one broken item from
		// re-triggering repair or re-blacklisting repeatedly. Default 30.
		RepairCooldownMin int
		// SubmitBudgetMin (TS-3.2, T4) bounds how long, in minutes, an
		// uncached torrent submission may show literally zero download
		// progress before the resolver fails it, blacklists the infohash,
		// and lets the arr re-search. Distinct from and stricter than
		// StallTimeoutMin: StallTimeoutMin only engages once the provider
		// explicitly reports a stalled/no-swarm state and never blacklists
		// (later swarm recovery remains possible); this budget extends
		// stall detection to the acceptance window itself -- a source that
		// never starts transferring at all, even while the provider still
		// reports an ordinary "downloading"/unknown state, is a stronger,
		// blacklist-worthy signal. Anchored on item.CreatedAt, the same
		// convention DeadSentinelGraceMin already uses. Default 15. Env
		// var is deliberately HARRBOR_TORRENT_SUBMIT_BUDGET_MIN (not
		// HARRBOR_GOVERNOR_*), matching the frozen source plan's naming.
		SubmitBudgetMin int
		// TorrentKeepWarmDays (TS-4.2, frozen plan T8a) is the "recent
		// play" window: a Ready torrent item is a keep-warm candidate only
		// if it was played within this many days. The separate, fixed,
		// non-configurable lead distance ahead of the derived provider-
		// expiry horizon (keepWarmLeadDays in internal/api/keepwarm.go) is
		// a different concept -- this value controls only recency-of-play,
		// matching the frozen plan's single named knob. 0 disables the
		// keep-warm janitor entirely (frozen plan's own documented
		// "0 = off"). Default 20. Env var is deliberately
		// HARRBOR_TORRENT_KEEPWARM_DAYS (not HARRBOR_GOVERNOR_*), matching
		// the frozen source plan's own naming, same precedent as
		// SubmitBudgetMin above.
		TorrentKeepWarmDays int
		// TorrentDecayAuditEnabled (TS-4.3, frozen plan T8b) gates the
		// whole rate-capped remote-presence decay-audit janitor to a
		// no-op. Default true. Mirrors HealthAudit.Enabled/GrabHealth.
		// Enabled/IdentityAudit.Enabled's established per-janitor toggle
		// convention. This row names no other config knob: candidate
		// rate cap (decayAuditCandidatesPerTick) and tick cadence
		// (decayAuditTickInterval) are fixed, non-configurable constants,
		// the same precedent TS-4.2's own keepWarmCandidatesPerTick/
		// keepWarmLeadDays established for its sibling janitor.
		TorrentDecayAuditEnabled bool
	}

	// Prewarm configures TS-1.4's shared prewarm/seek-target/next-episode/
	// adaptive-readahead service (internal/experience). It is lane-agnostic
	// over rangecache.ChunkSource; this row wires only the torrent/debrid-CDN
	// lane (NS-5.x and HR5.3/HR5.4 wire NNTP/HTTP onto the same service in
	// their own future rows).
	Prewarm struct {
		// Enabled gates the whole service to a no-op. Default true.
		Enabled bool
		// NNTPBytes bounds NS-5.2's post-manifest leading-byte prewarm.
		// Default 16 MiB; 0 disables NNTP prewarm without disabling the
		// shared experience service for other lanes.
		NNTPBytes int64
		// HotHeadMB is how many leading MB of a just-resolved item are
		// pinned at grab time, ahead of any real play request. Default 32.
		HotHeadMB int64
		// TimeoutSec bounds one prewarm/seek-target-prefetch call — the
		// DG-09 prewarm budget — regardless of the caller's own context.
		// Default 45.
		TimeoutSec int
		// NextEpisodeThreshold is the playback-fraction (0.0-1.0) past which
		// NextEpisodePrewarm actually fires. Default 0.85. No current lane
		// reaches an end-of-playback signal into this row's call sites, so
		// this value is exercised only by deterministic tests until a future
		// consuming row (HR5.4) wires a live playback-position signal.
		NextEpisodeThreshold float64
		// MinReadaheadWorkers / MaxReadaheadWorkers bound
		// AdaptiveReadahead's recommendation. Defaults 1 and 4, paired with
		// TorrentCDNCapacity's default.
		MinReadaheadWorkers int
		MaxReadaheadWorkers int
	}

	// Suppression controls SF-03's own-measurement failed-release
	// suppression (internal/suppress + internal/store's
	// release_suppressions table): a release fingerprint (normalized
	// name+size+lane) that DarkHarrbor itself observed failing with a
	// deterministic, distinct reason is filtered from DarkHarrbor's own
	// feeds for this TTL.
	Suppression struct {
		// TTLHours bounds how long a recorded suppression applies before it
		// naturally expires and the fingerprint can be offered again (e.g.
		// a repost under the same name/size might genuinely be fixed).
		// Default 72 (3 days).
		TTLHours int
	}

	// Availability controls TS-5.1's own-traffic per-provider hash
	// availability observation store (internal/store's hash_availability
	// table): each real submit-time CheckCached result or stream-time
	// capability probe/genuine-failure outcome is persisted as one
	// (provider, info_hash) observation, upserted on every new observation.
	Availability struct {
		// TTLHours bounds how long a recorded observation is considered
		// live before it naturally expires (a stale observation should not
		// be treated as current -- provider cache state changes over time).
		// Default 72 (3 days), matching Suppression.TTLHours.
		TTLHours int
	}

	// SearchBudget bounds repeated successful upstream searches that return no
	// candidates for one stable media identity. It ships enabled; identities
	// without a stable external ID abstain from budgeting.
	SearchBudget struct {
		Cap          int
		SuppressDays int
	}

	// Reactive owns RX-3.2's proposal gate and RX-5.2's explicit dispatcher
	// mode. Pending decisions remain owned by RX-4.2.
	Reactive struct {
		Enabled              bool
		CommitMode           string
		Threshold            float64
		MonitorEpisodes      bool
		MonitorMovies        bool
		PromotionEnabled     bool
		PromotionFreePercent int
		// RD-34 D3: OPTIONAL per-kind default destination, named by Arr
		// instance. Applied only when NOTHING owns the identity and more
		// than one instance of that kind exists, which RD-34 D2 rules is
		// otherwise not determined. Documented honestly as "captured content
		// lands here unless you move it", NOT as routing. Unset is the
		// shipping default and parks for a human decision.
		DefaultSeriesArr string
		DefaultMovieArr  string
	}

	// Identity controls ID-01's grab-time identity verification.
	Identity struct {
		// Strictness is "off" (verification does not run), "warn" (verdicts
		// are logged/counted only, default), or "enforce" (a verdict also
		// triggers repair/blacklist+SF-03 suppression). An unrecognized
		// configured value parses to "warn", never fails startup.
		Strictness string
	}

	// HealthAudit controls NS-6.1's rate-capped STAT health-audit janitor
	// pass over Ready NNTP items: sample a bounded number of segments per
	// item on a slow cadence, persist a completeness estimate and a
	// bounded dead-region map, and mark the item decayed below threshold.
	// This row never triggers Arr re-search (NS-6.2's job) and never
	// repairs (NS-4.2's job); it only measures and persists.
	HealthAudit struct {
		// Enabled gates the whole pass to a no-op. Default true.
		Enabled bool
		// IntervalSec is the cadence between successive audited items
		// (frozen plan N13 default: 1 item/min = 60s). Combined with the
		// Ready NNTP library size, this determines the full-library audit
		// period (frozen plan default target: >= 7 days).
		IntervalSec int
		// SampleSize is the number of STAT samples drawn per audited item
		// (frozen plan N13 default: 8).
		SampleSize int
		// CompletenessThreshold is the fraction of conclusively-sampled
		// segments that must confirm present for an item to stay healthy
		// (frozen plan N13 default: 0.95, "after par2 headroom").
		CompletenessThreshold float64
		// MaxDeadRegions bounds how many distinct dead-region entries are
		// persisted per item (DG-09 prep cap), so a pathological
		// all-dead item cannot grow its dead-region map unbounded across
		// many audit passes. Default 64.
		MaxDeadRegions int
		// ReSearchEnabled gates NS-6.2: when a health-audit pass finds an
		// item newly decayed (this pass's decayed=true where the prior
		// persisted pass was not decayed, or never audited), DarkHarrbor
		// blacklists the item's exact NZB content-key, records an SF-03
		// dead_post suppression fingerprint, and marks the item
		// StateFailed so the owning arr's own queue polling blocklists and
		// re-searches it -- mirroring handleAllProvidersDead's existing
		// terminal-failure convention exactly (no new arr-client code
		// path). Default true. Set false to leave NS-6.1's decayed marking
		// purely measurement-only, exactly as it behaved before this row.
		ReSearchEnabled bool
		// ReSearchCooldownMin bounds how soon, in minutes, the SAME item
		// may trigger another re-search after one fires. Primarily
		// defense-in-depth: a StateFailed item already leaves
		// NextHealthAuditItem's 'ready'-only rotation on its own, so under
		// normal operation this cooldown is never even tested by a second
		// audit pass for the same item. Mirrors RepairCooldownMin's
		// established convention. Default 60.
		ReSearchCooldownMin int
	}

	// GrabHealth controls NS-6.3's bounded, synchronous grab-time STAT
	// check in handleSABAddFile only. C0.5 fetches addurl bytes but does not
	// widen NS-6.3's policy to that path. Reject a
	// conclusively fully-dead NZB (every conclusively-sampled segment
	// missing) before it is ever queued, so the arr's own add-failure
	// handling re-searches promptly instead of waiting for the much later
	// async resolve/probe cycle. Reuses NS-6.1's exact sampling/audit
	// primitives; adds no new persistence, no new STAT mechanism, and
	// never rejects on a partial/inconclusive sample.
	GrabHealth struct {
		// Enabled gates the whole check to a no-op. Default true.
		Enabled bool
		// SampleSize is the number of STAT samples drawn per grab, spread
		// across the NZB (mirrors HealthAudit.SampleSize's selection
		// algorithm exactly, via the same SelectSampleSegments call).
		// Deliberately smaller than HealthAudit's periodic default (8):
		// the frozen plan's own grab-time budget is "bounded, ~1s",
		// tighter than the periodic pass's cost tolerance. Default 4.
		SampleSize int
		// TimeoutMS bounds the whole STAT sample pass (item accept must
		// still respond well inside this row's own "<2s" DONE-condition
		// ceiling). A timeout mid-sample degrades to whatever conclusive
		// samples were already collected -- RunHealthAudit's own
		// per-segment ctx.Err() check -- never a hard failure. Default
		// 1500 (1.5s), leaving headroom under the 2s ceiling for the rest
		// of the accept handler's own work.
		TimeoutMS int
	}

	// IdentityAudit controls ID-03's retroactive on-demand/slow-cadence
	// re-run of ID-01's identitycheck.Verify() over already-Ready items
	// (any lane). This row only measures and persists a legible possible-
	// wrong-content report; it never suppresses, blacklists, or fails an
	// item -- that remains ID-01's own live grab-time enforce path.
	IdentityAudit struct {
		// Enabled gates the periodic slow-cadence pass to a no-op; the
		// on-demand HTTP trigger is unaffected by this flag (an operator
		// explicitly asking for one pass right now is always honored).
		// Default true.
		Enabled bool
		// IntervalSec is the cadence between successive audited items.
		// Deliberately slower than HealthAudit's 60s default (this row's
		// own master-plan wording is "on demand/slow cadence") since a
		// pure in-memory/DB pass has no real cost pressure forcing a
		// faster cadence; default 300 (5 minutes).
		IntervalSec int
	}

	// Metrics controls OBS-01's Prometheus /metrics endpoint. The endpoint
	// renders only already-computed, already-bounded shared-owner state
	// (item-state counts, NS-1.3's zero-fill total, OBS-01's own error-
	// journal aggregate, and the existing rangecache/origin-score/CDN-score
	// snapshot numbers already exposed at /healthz) -- no new hot-path
	// instrumentation is added by this row.
	Metrics struct {
		// Enabled gates route registration only; default true. The route
		// shares the existing API listener/address -- there is no separate
		// bind-address surface to configure.
		Enabled bool
	}

	// ErrorJournal controls OBS-01's bounded per-item persisted SF-01
	// outcome-event ring (metadata_json.$.error_journal).
	ErrorJournal struct {
		// MaxEntries bounds how many recent events are retained per item,
		// oldest dropped first. A non-positive or unset value falls back to
		// store.DefaultErrorJournalMaxEntries (20) -- unknown/invalid
		// configuration never fails startup. Default 20.
		MaxEntries int
	}

	// Routing controls per-item acquisition-lane selection across providers.
	Routing struct {
		// NZBTorBoxCache enables a TorBox usenet checkcached pass for NZB items
		// (HARRBOR_NZB_CACHE, default false). When false, NZBs go straight to the
		// NNTP lane. When true, an NZB whose content hash is cached on TorBox is
		// fulfilled via TorBox (CDN stream); a miss falls through to NNTP. TorBox's
		// usenet cache is thin, so this is off by default to avoid wasted calls.
		// Derived from Preference (torbox_nzb present); retained as the bridge the
		// fulfillment engine reads.
		NZBTorBoxCache bool
		// Preference is the ordered acquisition-lane list (HARRBOR_PREFERENCE):
		// position = priority, presence = enabled. Canonical source of truth for
		// selection eligibility and fulfillment ordering. See routing.go.
		Preference []string
		// PreferenceExplicit is true when HARRBOR_PREFERENCE was set by the user.
		// When false, Preference is synthesized from the legacy RequireCached /
		// NZBTorBoxCache booleans for back-compat.
		PreferenceExplicit bool
	}

	// Prowlarr configures cache-aware SELECTION mode (the meta-indexer). When a
	// base URL + API key are set, DH fans arr searches to the user's Prowlarr
	// indexers, cache-prefilters per Preference, and returns the result so the arr
	// only sees eligible releases. Absent => fulfillment-only (DH's Torznab inert).
	Prowlarr struct {
		BaseURL string
		APIKey  string
		// IndexerIDs optionally restricts the fan-out to specific Prowlarr indexer
		// IDs. Empty => all enabled indexers except DH's own (loop-safety, resolved
		// by matching DH's base URL in the indexer list).
		IndexerIDs []int
	}

	Probe struct {
		// Timeout for a single ffprobe invocation.
		Timeout time.Duration
		// MaxRetries before marking probe as failed.
		MaxRetries int
		// ProbeBytes is the number of bytes to Range-GET from the CDN for the
		// byte-range probe (v1). Default 1MB; minimum 65536 (64KB).
		ProbeBytes int64
	}

	// Relay configures the .strm-mode D-RELAY endpoint (POST /api/v1/probe,
	// REDESIGN S10 Gate 5) that runs ffprobe-full against a DH /stream URL on
	// behalf of the arr-side shim, so ffprobe-full only ever needs to exist
	// inside the DH image, not in every arr container.
	Relay struct {
		// Concurrency bounds simultaneous ffprobe-full invocations. Excess
		// requests queue behind QueueTimeout rather than fanning out
		// unbounded (Gate 5 AC).
		Concurrency int
		// Timeout is the max duration for a single ffprobe-full invocation.
		Timeout time.Duration
		// QueueTimeout is the max time a request waits for a concurrency
		// slot before being rejected with 503.
		QueueTimeout time.Duration
		// MaxBodyBytes caps the request body size; requests over this limit
		// are rejected before touching the concurrency semaphore.
		MaxBodyBytes int64
		// FFprobeFullPath is where the network-capable ffprobe-full static
		// binary is staged inside the DH image (see Dockerfile's ffprobefull
		// build stage).
		FFprobeFullPath string
		// NegativeTTLMin (SF-05) is how many minutes a deterministic probe
		// failure (ffprobe ran and exited nonzero) is replayed from the
		// negative cache without any upstream byte movement, bounding the
		// provider cost of an arr's infinite import-retry of a broken
		// release. 0 disables negative caching entirely. Relay-level
		// failures (the process could not run) are never cached.
		NegativeTTLMin int
	}

	// StrmInstaller wires REDESIGN S10's eventwatcher (Gate 4) + docker-proxy
	// (Gate 1) into DH's own boot sequence: a startup reconcile pass over
	// every configured arr container, then a live Docker-events watch that
	// re-installs the .strm-mode ffprobe shim on each selected arr "start".
	// The source/manual default is off; public first-run setup enables it for
	// reviewed scoped-discovery + base-Arr plans. This mirrors docker-compose.yml's docker-proxy
	// service, which only exists under the "strm" compose profile (D-DOCKER:
	// VFS-only/hardened deployments should never need Docker socket access
	// at all). Until 2026-07-01 every gate (1-6) had only ever been
	// exercised standalone/ad-hoc against the live stack via a throwaway
	// harness; this wires the real thing into cmd/darkharrbor/main.go.
	StrmInstaller struct {
		// Enabled turns on the background event-watcher goroutine. Requires
		// the docker-proxy sidecar to actually be running (compose profile
		// "strm") -- if it isn't reachable, eventwatcher logs and retries
		// with backoff rather than blocking startup or crashing.
		Enabled bool
		// DockerProxyURL is where the scoped socket-proxy (cmd/harrbor-
		// dockerproxy) listens -- arr-net only, matching the compose
		// service name/port.
		DockerProxyURL string
	}

	NNTP struct {
		// Host is the Newshosting NNTP server hostname.
		Host string
		// Port is the NNTP server port (default 563 for TLS).
		Port int
		// TLS enables TLS for the NNTP connection (default true).
		TLS bool
		// Username and Password are Newshosting credentials.
		Username string
		Password string
		// Connections is the pool size (default 8).
		Connections int
		// VerifyCRC rejects yEnc segments whose pcrc32/crc32 trailer does not
		// match the decoded payload (default true).
		VerifyCRC bool
		// Stripe round-robins healthy providers per segment (NS-8.4). Default false.
		Stripe bool
		// NegCacheTTLMin is how many minutes a definitive missing-article
		// (430/423) answer from a usenet provider is remembered, so a
		// repeat demand for a known-dead segment short-circuits instead
		// of spending another BODY command on a certain failure
		// (NS-1.1). In-memory and per-process: a restart forgets, which
		// costs one re-fetch per segment. 0 disables. Default 60.
		NegCacheTTLMin int
		// WarmConns is the NS-5.1 per-provider warm-pool target: the
		// number of already-dialed, authenticated, keepalive-pinged
		// connections kept idle in each configured provider's demand
		// pool so playback start finds one ready instead of paying a
		// fresh dial+TLS+AUTHINFO round trip. Applied to every
		// configured usenet provider (same shared-config pattern as
		// NegCacheTTLMin above). 0 disables warm-keeping entirely.
		// Default 1.
		WarmConns int
		// PipelineDepth is the maximum number of BODY commands allowed in
		// flight on one authenticated connection. 1 disables pipelining.
		// Default 4; values above 32 are clamped with a startup warning.
		PipelineDepth int
		// LagModelYoungHours is NS-9.2's per-provider article-propagation
		// lag model threshold: posts younger than this many hours are
		// eligible for ordering by observed average age-at-first-success
		// instead of static preference/stripe, once enough samples exist.
		// 0 or negative disables the model entirely (in-memory only, no
		// persistence, matching ProviderHealth's existing convention).
		// Default 24.
		LagModelYoungHours int
		// LagModelMinSamples is the number of observed age-at-first-success
		// samples a provider needs before its average is trusted enough to
		// influence lag-model ordering. Values below 1 are clamped to 1.
		// Default 3.
		LagModelMinSamples int
		// RepairBudgetMB is NS-4.2's per-repair byte ceiling: the maximum
		// number of megabytes one PAR2 rung-4 reconstruction may read
		// (every surviving input block of the recovery set plus the
		// recovery slices themselves). Reed-Solomon recovery blocks are
		// linear combinations of every input block, so a repair cannot be
		// cheaper than the recovery set's surviving payload; this budget is
		// what keeps that from becoming an unbounded read behind a live
		// stream. 0 disables grab-free stream-time repair entirely, leaving
		// the pre-NS-4.2 final-rung exhaustion error untouched. Default 256.
		RepairBudgetMB int64
		// RepairBudgetSeconds is NS-4.2's per-repair wallclock ceiling,
		// measured on the injected clock. Exceeding it abandons the repair
		// and returns the original rung-1..3 error unchanged. Values below 1
		// disable stream-time repair. Default 45.
		RepairBudgetSeconds int
		// CrossLaneSplice enables NS-7's NNTP recovery candidate discovery.
		// It is off by default; NS-7.1 only exposes URL-free read-only
		// candidates, while NS-7.2 owns byte fetching and IFSC verification.
		CrossLaneSplice bool
	}

	Cache struct {
		// Mode selects the caching strategy (none|readahead|disk|full).
		// Default: readahead.
		Mode string
		// TotalConnections is the total NNTP connection pool size (demand+readahead).
		TotalConnections int
		// DemandConnections is the pool size reserved for on-demand (playback) fetches.
		DemandConnections int
		// ReadaheadConnections is the pool size for prefetch goroutine.
		ReadaheadConnections int
		// ReadaheadEnabled toggles prefetching; ignored in full mode (always true).
		ReadaheadEnabled bool
		// ReadaheadMaxSegments is the exponential window cap for prefetch.
		ReadaheadMaxSegments int
		// MinBufferSegments is how many segments to prefetch before starting playback.
		MinBufferSegments int
		// TailEvict evicts readahead buffer entries behind the lookback window.
		TailEvict bool
		// TailLookback is the number of segments to keep behind current write position.
		TailLookback int
		// DiskCacheSizeMB is the max disk cache size in MB (disk/full modes).
		DiskCacheSizeMB int64
		// DiskCacheTTLMin is minutes before a disk-cached segment is evicted (disk mode).
		DiskCacheTTLMin int
		// DiskCachePath overrides the default cache directory (default: DATA_ROOT/.cache).
		DiskCachePath string
		// FullEvictMode controls eviction in full mode: ttl|manual|never.
		FullEvictMode string
		// FullEvictTTLMin is minutes before a full-mode disk segment is evicted (ttl mode).
		FullEvictTTLMin int
		// NNTPMode overrides Mode for the NNTP /dav path only. Empty falls back
		NNTPMode string
		// StreamMode overrides Mode for the torrent/CDN /stream byte-proxy path
		// only. Empty falls back to Mode.
		StreamMode string
		// StreamChunkSizeMB is the CDN byte-proxy window size in MB (default 16).
		StreamChunkSizeMB int
		// StreamReadaheadWorkers is the number of parallel CDN prefetch goroutines
		// (default 1). Kept low to avoid per-IP CDN rate limits (429s). Completely
		// independent of NNTP readahead connection counts.
		StreamReadaheadWorkers int
		// StreamMinBufferSegments is how many CDN chunks to prefetch before starting
		// playback on the /stream path (default 2). Lower = faster first-byte;
		// higher = fewer mid-stream stalls on slow uplinks.
		StreamMinBufferSegments int
		// PinnedBudgetMB is TS-1.4's separate, bounded "hot-head" pinned-tier
		// budget (MB): bytes fetched via rangecache.GetChunkPinned (prewarm's
		// leading bytes plus the mediatruth seek-index region) are exempt
		// from the main disk LRU/TTL sweep above and are instead bounded by
		// this budget with their own independent LRU eviction, so a prewarm
		// pass can never grow the cache unboundedly nor evict the ordinary
		// playback cache's own entries early. 0 disables pinning: prewarm
		// still runs but stores through the ordinary disk cache/budget
		// instead. Default 1024 (1 GiB).
		PinnedBudgetMB int64
		// ReadaheadSeekWidenMS (NS-5.5) bounds how long the NNTP lane's
		// readahead width stays widened to its pool ceiling after a seek,
		// before decaying back to the adaptive steady recommendation.
		// 0 (default) disables widening entirely: readahead width stays
		// pinned at the ceiling always, identical to pre-NS-5.5 behavior.
		ReadaheadSeekWidenMS int
	}

	// Stub controls authoritative-stub behavior (F-E, plan §4.2).
	Stub struct {
		// Authority selects where stub/.strm content is authoritative:
		// "db" (default) persists to SQLite and materializes to disk;
		// "fs" is a legacy escape hatch that skips DB persistence and writes
		// only to the filesystem (no reconcile coverage).
		Authority string
	}

	// HTTPStream configures the HTTP stream provider feature
	// (HTTP-STREAM-MASTER-PLAN.md). Disabled by default; when off, the
	// /torznab/http-stream feed returns empty, /grab/http rejects, and the
	// qBit adapter refuses HTTP-marker submissions.
	HTTPStream struct {
		Enabled    bool
		TMDBAPIKey string
		// Backends is the ordered named-instance backend list (D5),
		// parsed from HARRBOR_HTTP_BACKENDS + HARRBOR_HTTPBACKEND_<NAME>_*.
		Backends []HTTPBackend
		// AllowPrivateSourceCIDRs opts this deployment into permitting
		// TrustSource dials (grab-time preflight + live relay, D9/D11) to
		// RFC1918/ULA private-range destinations — e.g. a self-hosted
		// backend (AIOStreams and similar) that itself hands back further
		// LAN-hosted playback URLs. Off by default (D11's SSRF boundary);
		// the operator must explicitly opt in via
		// HARRBOR_HTTP_ALLOW_PRIVATE_SOURCES. A Tailscale/CGNAT
		// (100.64.0.0/10) address is NOT covered by Go's IsPrivate() and
		// therefore does not require this flag at all (HR0.2 live-gate
		// finding, 2026-07-18).
		AllowPrivateSourceCIDRs bool
		// IAFilePreference chooses which IA representation source wins before
		// format/size tie-breaking: "derivative" (default) or "original".
		IAFilePreference string
		// PredictiveResolveEnabled (HR3.2) enables background predictive
		// midstream re-resolve, layered on top of the always-on reactive
		// re-resolve-on-expiry path. Default true; the reactive path is
		// unaffected either way.
		PredictiveResolveEnabled bool
		// PredictiveResolveMinLeadMS is the floor lead time, in
		// milliseconds, before a source's ExpiresAt at which a background
		// predictive re-resolve may start. Default 5000.
		PredictiveResolveMinLeadMS int
		// PredictiveResolveMaxLeadMS caps the lead computed from observed
		// per-origin-class token lifetimes. 0 means no cap. Default 60000.
		PredictiveResolveMaxLeadMS int
		// SelfHealEnabled (HR3.4) gates the bounded, two-pass-reconfirmed
		// Arr re-search escalation on repeated HTTP-lane stream-time
		// ladder exhaustion (HR-D14's final rung). Default true.
		SelfHealEnabled bool
		// SelfHealCooldownMin bounds how soon, in minutes, the SAME item
		// may trigger another escalation, mirroring
		// HealthAudit.ReSearchCooldownMin's own convention. Default 60.
		SelfHealCooldownMin int
	}

	Lifecycle struct {
		// ResolveInterval is how often the resolver loop ticks.
		ResolveInterval time.Duration
		// PruneInterval is how often expired/removed items are cleaned up.
		PruneInterval time.Duration
		// RemovedRetention is how long removed items stay in the DB.
		RemovedRetention time.Duration
		// CleanupHours is how long after the last proxy hit before a materialized
		// TorBox item is removed from the account (1-24, default 2).
		CleanupHours int
		// ReconcileIntervalSec is how often the blob materializer reconciles
		// DB→disk while running (F3/X.9 guard; 0 disables periodic reconcile,
		// startup reconcile always runs). Default 300.
		ReconcileIntervalSec int
		// ReadyAutoRemoveSec is how many seconds a StateReady item stays in the
		// qbit/sab feed before being auto-transitioned to StateRemoved. The arr
		// should have imported the .strm well within this window. Streaming
		// continues to work after removal. 0 disables. Default 1800 (30 min).
		ReadyAutoRemoveSec int
		// NeverPlayedUncachedHours (R1): a StateReady torrent item that was
		// submitted uncached and has never been played is removed from the
		// provider (TorBox) after this many hours, reclaiming the Allowed
		// Active Slot it would otherwise hold for the provider's full seed
		// window (TorBox Pro: 30 days). Streaming continues to work after
		// removal (idempotent re-materialize on next play, same as the
		// post-play janitor). 0 disables. Default 24.
		NeverPlayedUncachedHours int
		// FailedRetention is how long a StateFailed item is kept before being
		// pruned. Deliberately much shorter than RemovedRetention: DH's
		// SAB-emulator history reports StateFailed items to the arr on every
		// queue poll, so an un-pruned failure stays visibly "stuck" in Sonarr's
		// queue indefinitely (found 2026-07-01). 0 disables (retained forever —
		// the pre-fix behavior). Default 5 minutes — a failure has no ongoing
		// value once DH itself has recorded it (governor_log / failed_hashes
		// blacklist already capture what's needed for the retry-prevention
		// logic); the arr should just move on to another release. Notified
		// arrs (HARRBOR_ARR_NAMES) get an active refresh right after pruning,
		// so they don't have to wait out their own polling interval to notice.
		FailedRetention time.Duration
		// FailedPruneInterval is how often the dedicated failed-item prune (+
		// arr notify) loop runs — separate from and much more frequent than
		// PruneInterval (general removed-item cleanup, default 12h), since
		// failed items should clear out fast, not wait up to half a day.
		// Default 2 minutes.
		FailedPruneInterval time.Duration
	}

	// Arrs is the parsed set of Sonarr/Radarr instances to notify after
	// pruning failed items (arrs.go). Empty unless HARRBOR_ARR_NAMES is set —
	// optional infrastructure, not required for DH to run.
	Arrs []ArrTarget

	// Providers is the parsed provider selection (providers.go,
	Providers ProvidersConfig

	// Secrets holds decrypted sealed secrets when HARRBOR_SECRETS_FILE is
	// configured (secrets.go); nil otherwise. Never exported to child env.
	Secrets *SecretStore
}

func Load() (*Config, error) {
	cfg := defaultConfig()
	if err := loadDotEnv(".env"); err != nil {
		return nil, err
	}
	applyEnv(&cfg)
	secrets, err := loadSealedSecrets()
	if err != nil {
		return nil, err
	}
	cfg.applySecrets(secrets) // sealed wins over plaintext env (F-D)
	tuning, tuningWarnings, err := ReadTuningWithWarnings(TuningPath())
	if err != nil {
		return nil, err
	}
	cfg.StartupWarnings = append(cfg.StartupWarnings, tuningWarnings...)
	applyTuning(&cfg, tuning) // strict non-secret file wins for tunables only
	applyProviderSelection(&cfg)
	if err := applyHTTPBackends(&cfg); err != nil {
		return nil, err
	}
	applyArrTargets(&cfg)
	cfg.applyDerived()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func defaultConfig() Config {
	var cfg Config
	cfg.Topology.Active = topology.T1
	cfg.Topology.AchievedPostureStalenessHours = 48
	cfg.Server.Address = defaultServerAddr
	cfg.Server.BaseURL = "http://localhost:8381"
	cfg.Stremio.EdgeMode = StremioEdgeInternal
	cfg.Stremio.ClientAddress = ":8382"
	cfg.Database.Path = defaultDatabasePath
	cfg.Database.BusyTimeout = 5 * time.Second
	cfg.Backup.Enabled = true
	cfg.Backup.Target = "/backup"
	cfg.Backup.IntervalHours = 24
	cfg.Backup.Keep = 7
	cfg.Backup.SecretsPath = "/config/secrets.sealed"
	cfg.Data.Root = defaultDataRoot
	cfg.Data.ReportedPathPrefix = defaultDataRoot
	cfg.TorBox.BaseURL = "https://api.torbox.app/v1"
	cfg.TorBox.SearchBaseURL = "https://search-api.torbox.app"
	cfg.TorBox.RequestTimeout = 45 * time.Second
	cfg.HTTPStream.IAFilePreference = "derivative"
	cfg.HTTPStream.PredictiveResolveEnabled = true
	cfg.HTTPStream.PredictiveResolveMinLeadMS = 5000
	cfg.HTTPStream.PredictiveResolveMaxLeadMS = 60000
	cfg.HTTPStream.SelfHealEnabled = true
	cfg.HTTPStream.SelfHealCooldownMin = 60
	cfg.TorBox.UserAgent = "github.com/darkharrbor/darkharrbor/0.1"
	cfg.Auth.QBitUsername = defaultQBitUser
	cfg.Auth.SessionTTL = 24 * time.Hour
	cfg.Compatibility.QBitVersion = "5.0.0"
	cfg.Compatibility.QBitWebAPI = "2.11.3"
	cfg.Compatibility.SABVersion = "4.5.1"
	cfg.Compatibility.DefaultCategory = defaultCategory
	cfg.Governor.UncachedBudget = 20
	cfg.Governor.WindowDays = 15
	cfg.Governor.StallTimeoutMin = 30
	cfg.Governor.DeadSentinelGraceMin = 5
	cfg.Governor.SubmitBudgetMin = 15
	cfg.Governor.RequireCached = true
	cfg.Governor.TorrentCDNCapacity = 4
	cfg.Governor.HTTPSourceCapacity = 0
	cfg.Governor.AltProviderFailoverEnabled = true
	cfg.Governor.RepairEnabled = true
	cfg.Governor.RepairCooldownMin = 30
	cfg.Governor.TorrentKeepWarmDays = 20
	cfg.Governor.TorrentDecayAuditEnabled = true
	cfg.Prewarm.Enabled = true
	cfg.Prewarm.NNTPBytes = 16 * 1024 * 1024
	cfg.Prewarm.HotHeadMB = 32
	cfg.Prewarm.TimeoutSec = 45
	cfg.Prewarm.NextEpisodeThreshold = 0.85
	cfg.Prewarm.MinReadaheadWorkers = 1
	cfg.Prewarm.MaxReadaheadWorkers = 4
	cfg.Suppression.TTLHours = 72
	cfg.Availability.TTLHours = 72
	cfg.SearchBudget.Cap = 5
	cfg.SearchBudget.SuppressDays = 7
	cfg.Reactive.Enabled = false
	cfg.Reactive.CommitMode = "off"
	cfg.Reactive.Threshold = 0.5
	cfg.Reactive.MonitorEpisodes = true
	cfg.Reactive.MonitorMovies = true
	cfg.Reactive.PromotionEnabled = true
	cfg.Reactive.PromotionFreePercent = 10
	cfg.Identity.Strictness = "warn"
	cfg.HealthAudit.Enabled = true
	cfg.HealthAudit.IntervalSec = 60
	cfg.HealthAudit.SampleSize = 8
	cfg.HealthAudit.CompletenessThreshold = 0.95
	cfg.HealthAudit.MaxDeadRegions = 64
	cfg.HealthAudit.ReSearchEnabled = true
	cfg.HealthAudit.ReSearchCooldownMin = 60
	cfg.GrabHealth.Enabled = true
	cfg.GrabHealth.SampleSize = 4
	cfg.GrabHealth.TimeoutMS = 1500
	cfg.IdentityAudit.Enabled = true
	cfg.IdentityAudit.IntervalSec = 300
	cfg.Metrics.Enabled = true
	cfg.ErrorJournal.MaxEntries = 20
	cfg.Probe.Timeout = 60 * time.Second
	cfg.Probe.MaxRetries = 3
	cfg.Probe.ProbeBytes = 1048576
	// BUGFIX 2026-07-01: found live during a real bulk multi-season grab
	// (~98 episodes across 4 season packs at once) -- the original 4/20s/10s
	// defaults were sized around single-file testing and left dozens of
	// real episodes stuck in Sonarr's importPending/importing state under
	// concurrent load (compounded by the exec-timeout misclassification bug
	// fixed the same session, but even after that fix, 4 concurrent slots
	// for a 98-probe burst means most requests queue for tens of seconds).
	// ffprobe-full's latency here is dominated by network fetch time (CDN/
	// NNTP), not CPU, so raising concurrency is safe on a 4-core host --
	// waiting goroutines/processes are I/O-bound, not CPU-bound.
	cfg.Relay.Concurrency = 8
	cfg.Relay.Timeout = 45 * time.Second
	cfg.Relay.QueueTimeout = 20 * time.Second
	cfg.Relay.MaxBodyBytes = 65536
	cfg.Relay.FFprobeFullPath = "/usr/local/bin/ffprobe-full"
	cfg.Relay.NegativeTTLMin = 10080 // 7 days; deterministic on identical bytes
	cfg.StrmInstaller.Enabled = false
	cfg.StrmInstaller.DockerProxyURL = "http://docker-proxy:2375"
	cfg.HTTPStream.Enabled = false
	cfg.NNTP.Port = 563
	cfg.NNTP.TLS = true
	cfg.NNTP.Connections = 8
	cfg.NNTP.VerifyCRC = true
	cfg.NNTP.NegCacheTTLMin = 60
	cfg.NNTP.WarmConns = 1
	cfg.NNTP.PipelineDepth = 4
	cfg.NNTP.LagModelYoungHours = 24
	cfg.NNTP.LagModelMinSamples = 3
	cfg.NNTP.RepairBudgetMB = 256
	cfg.NNTP.RepairBudgetSeconds = 45
	cfg.Cache.Mode = "readahead"
	cfg.Cache.TotalConnections = 16
	cfg.Cache.DemandConnections = 8
	cfg.Cache.ReadaheadConnections = 8
	cfg.Cache.ReadaheadEnabled = true
	cfg.Cache.ReadaheadMaxSegments = 16
	cfg.Cache.MinBufferSegments = 4
	cfg.Cache.TailEvict = true
	cfg.Cache.TailLookback = 4
	cfg.Cache.DiskCacheSizeMB = 2048
	cfg.Cache.DiskCacheTTLMin = 60
	cfg.Cache.FullEvictMode = "ttl"
	cfg.Cache.FullEvictTTLMin = 120
	cfg.Cache.StreamChunkSizeMB = 16
	cfg.Cache.StreamMinBufferSegments = 2
	cfg.Cache.StreamReadaheadWorkers = 1
	cfg.Cache.PinnedBudgetMB = 1024
	cfg.Stub.Authority = "db"
	cfg.Lifecycle.ResolveInterval = 3 * time.Second
	cfg.Lifecycle.CleanupHours = 8
	cfg.Lifecycle.ReconcileIntervalSec = 300
	cfg.Lifecycle.PruneInterval = 12 * time.Hour
	cfg.Lifecycle.ReadyAutoRemoveSec = 1800
	cfg.Lifecycle.NeverPlayedUncachedHours = 24
	cfg.Lifecycle.FailedRetention = 5 * time.Minute
	cfg.Lifecycle.FailedPruneInterval = 2 * time.Minute
	cfg.Lifecycle.RemovedRetention = 30 * 24 * time.Hour
	cfg.applyDerived()
	return cfg
}

func (c *Config) applyDerived() {
	root := strings.TrimSpace(c.Data.Root)
	if root == "" {
		root = defaultDataRoot
	}
	c.Data.Root = root
	// Strm root IS the data root — category subdirs are created per-item.
	// e.g. root=/data → tv-modern goes to /data/tv-modern/<release>/
	c.Data.Strm = root
	// ReportedPathPrefix is what Sonarr/Radarr see for this path.
	// Defaults to root (safe fallback; set HARRBOR_REPORTED_PATH_PREFIX
	// to the host-visible path when the container path differs).
	if strings.TrimSpace(c.Data.ReportedPathPrefix) == "" {
		c.Data.ReportedPathPrefix = root
	}

	if strings.TrimSpace(c.Auth.QBitUsername) == "" {
		c.Auth.QBitUsername = defaultQBitUser
	}
	if strings.TrimSpace(c.Auth.SABNZBKey) == "" {
		c.Auth.SABNZBKey = c.Auth.SABAPIKey
	}
	// Resolve DiskCachePath: default to DATA_ROOT/.cache if not set.
	if strings.TrimSpace(c.Cache.DiskCachePath) == "" {
		c.Cache.DiskCachePath = strings.TrimRight(c.Data.Root, "/") + "/.cache"
	}
	c.resolvePreference()
}

// resolvePreference establishes the single runtime source of truth for
// acquisition lanes. An explicit HARRBOR_PREFERENCE is canonical and derives
// the legacy RequireCached / NZBTorBoxCache booleans. Omission selects no lane:
// credentials and historical booleans must never silently choose a provider.
func (c *Config) resolvePreference() {
	if c.Routing.PreferenceExplicit {
		c.Governor.RequireCached = !c.UncachedTorrentAllowed()
		c.Routing.NZBTorBoxCache = c.NZBViaTorBox()
	}
}

func applyEnv(cfg *Config) {
	setString := func(ptr *string, key string) {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			*ptr = v
		}
	}
	setString(&cfg.Server.Address, "HARRBOR_SERVER_ADDRESS")
	setString(&cfg.Server.BaseURL, "HARRBOR_SERVER_BASE_URL")
	if raw := strings.ToLower(strings.TrimSpace(os.Getenv("HARRBOR_STREMIO_EDGE_MODE"))); raw != "" {
		if ValidStremioEdgeMode(raw) {
			cfg.Stremio.EdgeMode = raw
		} else {
			cfg.StartupWarnings = append(cfg.StartupWarnings, "HARRBOR_STREMIO_EDGE_MODE is unknown and ignored; using internal")
			cfg.Stremio.EdgeMode = StremioEdgeInternal
		}
	}
	setString(&cfg.Stremio.ClientAddress, "HARRBOR_STREMIO_CLIENT_ADDRESS")
	setString(&cfg.Stremio.ClientBaseURL, "HARRBOR_STREMIO_CLIENT_BASE_URL")
	if raw := strings.TrimSpace(os.Getenv("HARRBOR_TOPOLOGY")); raw != "" {
		if active, ok := topology.Parse(raw); ok {
			cfg.Topology.Active = active
		} else {
			cfg.StartupWarnings = append(cfg.StartupWarnings, "HARRBOR_TOPOLOGY is unknown and ignored; using t1")
			cfg.Topology.Active = topology.T1
		}
	}
	if raw := strings.TrimSpace(os.Getenv("HARRBOR_TOPOLOGY_ACHIEVED_STALENESS_HOURS")); raw != "" {
		if hours, err := strconv.Atoi(raw); err == nil && hours > 0 {
			cfg.Topology.AchievedPostureStalenessHours = hours
		} else {
			cfg.StartupWarnings = append(cfg.StartupWarnings, "HARRBOR_TOPOLOGY_ACHIEVED_STALENESS_HOURS is invalid and ignored; using 48")
		}
	}
	setString(&cfg.Data.Root, "HARRBOR_DATA_ROOT")
	setString(&cfg.Data.ReportedPathPrefix, "HARRBOR_REPORTED_PATH_PREFIX")
	setString(&cfg.Database.Path, "HARRBOR_DATABASE_PATH")
	setString(&cfg.Backup.Target, "HARRBOR_BACKUP_TARGET")
	setString(&cfg.Backup.SecretsPath, "HARRBOR_SECRETS_FILE")
	setString(&cfg.TorBox.BaseURL, "HARRBOR_TORBOX_BASE_URL")
	setString(&cfg.TorBox.SearchBaseURL, "HARRBOR_TORBOX_SEARCH_BASE_URL")
	setString(&cfg.TorBox.APIToken, "HARRBOR_TORBOX_API_TOKEN")
	setString(&cfg.Auth.QBitPassword, "HARRBOR_QBIT_PASSWORD")
	setString(&cfg.Auth.SABAPIKey, "HARRBOR_SAB_API_KEY")
	setString(&cfg.Auth.SABNZBKey, "HARRBOR_SAB_NZB_KEY")
	setString(&cfg.Server.StreamSecret, "HARRBOR_STREAM_SECRET")
	setString(&cfg.MediaFlow.Password, "HARRBOR_MEDIAFLOW_PASSWORD")
	setString(&cfg.MediaFlow.PublicIP, "HARRBOR_MEDIAFLOW_PUBLIC_IP")
	setString(&cfg.MediaFlow.BaseURL, "HARRBOR_MEDIAFLOW_BASE_URL")
	setString(&cfg.NNTP.Host, "HARRBOR_NNTP_HOST")
	setString(&cfg.NNTP.Username, "HARRBOR_NNTP_USERNAME")
	setString(&cfg.NNTP.Password, "HARRBOR_NNTP_PASSWORD")
	if v := strings.TrimSpace(os.Getenv("HARRBOR_NNTP_PORT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.NNTP.Port = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_NNTP_TLS")); v != "" {
		cfg.NNTP.TLS = v == "true" || v == "1"
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_NNTP_CONNECTIONS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.NNTP.Connections = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_NNTP_VERIFY_CRC")); v != "" {
		if enabled, err := strconv.ParseBool(v); err == nil {
			cfg.NNTP.VerifyCRC = enabled
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_NNTP_NEGCACHE_TTL_MIN")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.NNTP.NegCacheTTLMin = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_NNTP_STRIPE")); v != "" {
		if enabled, err := strconv.ParseBool(v); err == nil {
			cfg.NNTP.Stripe = enabled
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_NNTP_CROSSLANE_SPLICE")); v != "" {
		if enabled, err := strconv.ParseBool(v); err == nil {
			cfg.NNTP.CrossLaneSplice = enabled
		} else {
			cfg.StartupWarnings = append(cfg.StartupWarnings, "HARRBOR_NNTP_CROSSLANE_SPLICE is invalid and ignored; using false")
		}
	}

	// Cache config
	setString(&cfg.Cache.Mode, "HARRBOR_CACHE_MODE")
	setString(&cfg.Cache.NNTPMode, "HARRBOR_NNTP_CACHE_MODE")
	setString(&cfg.Cache.StreamMode, "HARRBOR_STREAM_CACHE_MODE")
	setString(&cfg.Cache.DiskCachePath, "HARRBOR_DISK_CACHE_PATH")
	setString(&cfg.Cache.FullEvictMode, "HARRBOR_FULL_EVICT_MODE")
	setString(&cfg.Stub.Authority, "HARRBOR_STUB_AUTHORITY")
	for _, pair := range []struct {
		env string
		ptr *int
	}{
		{"HARRBOR_NNTP_TOTAL_CONNECTIONS", &cfg.Cache.TotalConnections},
		{"HARRBOR_NNTP_DEMAND_CONNECTIONS", &cfg.Cache.DemandConnections},
		{"HARRBOR_NNTP_READAHEAD_CONNECTIONS", &cfg.Cache.ReadaheadConnections},
		{"HARRBOR_READAHEAD_MAX_SEGMENTS", &cfg.Cache.ReadaheadMaxSegments},
		{"HARRBOR_READAHEAD_MIN_BUFFER_SEGMENTS", &cfg.Cache.MinBufferSegments},
		{"HARRBOR_READAHEAD_TAIL_LOOKBACK", &cfg.Cache.TailLookback},
		{"HARRBOR_DISK_CACHE_TTL_MIN", &cfg.Cache.DiskCacheTTLMin},
		{"HARRBOR_FULL_EVICT_TTL_MIN", &cfg.Cache.FullEvictTTLMin},
		{"HARRBOR_STREAM_CHUNK_SIZE_MB", &cfg.Cache.StreamChunkSizeMB},
		{"HARRBOR_STREAM_MIN_BUFFER_SEGMENTS", &cfg.Cache.StreamMinBufferSegments},
		{"HARRBOR_STREAM_READAHEAD_WORKERS", &cfg.Cache.StreamReadaheadWorkers},
		{"HARRBOR_RECONCILE_INTERVAL_SEC", &cfg.Lifecycle.ReconcileIntervalSec},
		{"HARRBOR_READY_AUTO_REMOVE_SEC", &cfg.Lifecycle.ReadyAutoRemoveSec},
		{"HARRBOR_GOVERNOR_TORRENT_CDN_CAPACITY", &cfg.Governor.TorrentCDNCapacity},
		{"HARRBOR_GOVERNOR_HTTP_CAPACITY", &cfg.Governor.HTTPSourceCapacity},
		{"HARRBOR_PREWARM_TIMEOUT_SEC", &cfg.Prewarm.TimeoutSec},
		{"HARRBOR_PREWARM_MIN_READAHEAD_WORKERS", &cfg.Prewarm.MinReadaheadWorkers},
		{"HARRBOR_PREWARM_MAX_READAHEAD_WORKERS", &cfg.Prewarm.MaxReadaheadWorkers},
		{"HARRBOR_READAHEAD_SEEK_WIDEN_MS", &cfg.Cache.ReadaheadSeekWidenMS},
		{"HARRBOR_HEALTHAUDIT_INTERVAL_SEC", &cfg.HealthAudit.IntervalSec},
		{"HARRBOR_HEALTHAUDIT_SAMPLE_SIZE", &cfg.HealthAudit.SampleSize},
		{"HARRBOR_HEALTHAUDIT_MAX_DEAD_REGIONS", &cfg.HealthAudit.MaxDeadRegions},
		{"HARRBOR_HEALTHAUDIT_RESEARCH_COOLDOWN_MIN", &cfg.HealthAudit.ReSearchCooldownMin},
		{"HARRBOR_GRABHEALTH_SAMPLE_SIZE", &cfg.GrabHealth.SampleSize},
		{"HARRBOR_GRABHEALTH_TIMEOUT_MS", &cfg.GrabHealth.TimeoutMS},
		{"HARRBOR_IDENTITYAUDIT_INTERVAL_SEC", &cfg.IdentityAudit.IntervalSec},
		{"HARRBOR_ERRORJOURNAL_MAX_ENTRIES", &cfg.ErrorJournal.MaxEntries},
		{"HARRBOR_BACKUP_INTERVAL_HOURS", &cfg.Backup.IntervalHours},
		{"HARRBOR_BACKUP_KEEP", &cfg.Backup.Keep},
		{"HARRBOR_NNTP_WARM_CONNS", &cfg.NNTP.WarmConns},
		{"HARRBOR_NNTP_LAGMODEL_YOUNG_HOURS", &cfg.NNTP.LagModelYoungHours},
		{"HARRBOR_NNTP_LAGMODEL_MIN_SAMPLES", &cfg.NNTP.LagModelMinSamples},
	} {
		if v := strings.TrimSpace(os.Getenv(pair.env)); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*pair.ptr = n
			}
		}
	}
	for _, setting := range []struct {
		env      string
		ptr      *int
		fallback int
		max      int
	}{
		{"HARRBOR_SEARCH_BUDGET_CAP", &cfg.SearchBudget.Cap, 5, 1000},
		{"HARRBOR_SEARCH_BUDGET_SUPPRESS_DAYS", &cfg.SearchBudget.SuppressDays, 7, 36500},
	} {
		if raw := strings.TrimSpace(os.Getenv(setting.env)); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > setting.max {
				cfg.StartupWarnings = append(cfg.StartupWarnings,
					setting.env+" is invalid and ignored; using "+strconv.Itoa(setting.fallback))
				*setting.ptr = setting.fallback
			} else {
				*setting.ptr = n
			}
		}
	}
	if raw := strings.TrimSpace(os.Getenv("HARRBOR_REACTIVE_ENABLED")); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			cfg.StartupWarnings = append(cfg.StartupWarnings, "HARRBOR_REACTIVE_ENABLED is invalid and ignored; using false")
		} else {
			cfg.Reactive.Enabled = enabled
		}
	}
	if raw := strings.ToLower(strings.TrimSpace(os.Getenv("HARRBOR_REACTIVE_COMMIT_MODE"))); raw != "" {
		if !ValidReactiveCommitMode(raw) {
			cfg.StartupWarnings = append(cfg.StartupWarnings, "HARRBOR_REACTIVE_COMMIT_MODE is invalid and ignored; using off")
		} else {
			cfg.Reactive.CommitMode = raw
		}
	}
	if raw := strings.TrimSpace(os.Getenv("HARRBOR_REACTIVE_THRESHOLD")); raw != "" {
		threshold, err := strconv.ParseFloat(raw, 64)
		if err != nil || threshold <= 0 || threshold > 1 || math.IsNaN(threshold) || math.IsInf(threshold, 0) {
			cfg.StartupWarnings = append(cfg.StartupWarnings, "HARRBOR_REACTIVE_THRESHOLD is invalid and ignored; using 0.5")
		} else {
			cfg.Reactive.Threshold = threshold
		}
	}
	if raw := strings.TrimSpace(os.Getenv("HARRBOR_REACTIVE_PROMOTION_FREE_PERCENT")); raw != "" {
		percent, err := strconv.Atoi(raw)
		if err != nil || percent < 1 || percent > 99 {
			cfg.StartupWarnings = append(cfg.StartupWarnings, "HARRBOR_REACTIVE_PROMOTION_FREE_PERCENT is invalid and ignored; using 10")
		} else {
			cfg.Reactive.PromotionFreePercent = percent
		}
	}
	for _, setting := range []struct {
		env string
		ptr *bool
	}{
		{"HARRBOR_REACTIVE_MONITOR_EPISODES", &cfg.Reactive.MonitorEpisodes},
		{"HARRBOR_REACTIVE_MONITOR_MOVIES", &cfg.Reactive.MonitorMovies},
		{"HARRBOR_REACTIVE_PROMOTION_ENABLED", &cfg.Reactive.PromotionEnabled},
	} {
		if raw := strings.TrimSpace(os.Getenv(setting.env)); raw != "" {
			value, err := strconv.ParseBool(raw)
			if err != nil {
				cfg.StartupWarnings = append(cfg.StartupWarnings, setting.env+" is invalid and ignored; using true")
			} else {
				*setting.ptr = value
			}
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_NNTP_REPAIR_BUDGET_MB")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err != nil || n < 0 || n > maxRepairBudgetMB {
			cfg.StartupWarnings = append(cfg.StartupWarnings,
				"HARRBOR_NNTP_REPAIR_BUDGET_MB is invalid and ignored; using 256")
		} else {
			cfg.NNTP.RepairBudgetMB = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_NNTP_REPAIR_BUDGET_SECONDS")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err != nil || n < 0 || n > maxRepairBudgetSecs {
			cfg.StartupWarnings = append(cfg.StartupWarnings,
				"HARRBOR_NNTP_REPAIR_BUDGET_SECONDS is invalid and ignored; using 45")
		} else {
			cfg.NNTP.RepairBudgetSeconds = int(n)
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_NNTP_PIPELINE_DEPTH")); v != "" {
		if n, err := strconv.Atoi(v); err != nil || n < 1 {
			cfg.StartupWarnings = append(cfg.StartupWarnings, "HARRBOR_NNTP_PIPELINE_DEPTH is invalid and ignored; using 4")
			cfg.NNTP.PipelineDepth = 4
		} else if n > 32 {
			cfg.StartupWarnings = append(cfg.StartupWarnings, "HARRBOR_NNTP_PIPELINE_DEPTH exceeds 32 and was clamped")
			cfg.NNTP.PipelineDepth = 32
		} else {
			cfg.NNTP.PipelineDepth = n
		}
	}
	for _, pair := range []struct {
		env string
		ptr *int64
	}{
		{"HARRBOR_DISK_CACHE_SIZE_MB", &cfg.Cache.DiskCacheSizeMB},
		{"HARRBOR_PINNED_BUDGET_MB", &cfg.Cache.PinnedBudgetMB},
		{"HARRBOR_NNTP_PREWARM_BYTES", &cfg.Prewarm.NNTPBytes},
		{"HARRBOR_PREWARM_HOTHEAD_MB", &cfg.Prewarm.HotHeadMB},
	} {
		if v := strings.TrimSpace(os.Getenv(pair.env)); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				*pair.ptr = n
			}
		}
	}
	for _, pair := range []struct {
		env string
		ptr *bool
	}{
		{"HARRBOR_READAHEAD_ENABLED", &cfg.Cache.ReadaheadEnabled},
		{"HARRBOR_READAHEAD_TAIL_EVICT", &cfg.Cache.TailEvict},
		{"HARRBOR_PREWARM_ENABLED", &cfg.Prewarm.Enabled},
		{"HARRBOR_HEALTHAUDIT_ENABLED", &cfg.HealthAudit.Enabled},
		{"HARRBOR_HEALTHAUDIT_RESEARCH_ENABLED", &cfg.HealthAudit.ReSearchEnabled},
		{"HARRBOR_GRABHEALTH_ENABLED", &cfg.GrabHealth.Enabled},
		{"HARRBOR_IDENTITYAUDIT_ENABLED", &cfg.IdentityAudit.Enabled},
		{"HARRBOR_TORRENT_DECAYAUDIT_ENABLED", &cfg.Governor.TorrentDecayAuditEnabled},
		{"HARRBOR_METRICS_ENABLED", &cfg.Metrics.Enabled},
		{"HARRBOR_BACKUP_ENABLED", &cfg.Backup.Enabled},
	} {
		if v := strings.TrimSpace(os.Getenv(pair.env)); v != "" {
			*pair.ptr = v == "true" || v == "1"
		}
	}

	if _, configured := os.LookupEnv("HARRBOR_DIRECT_STREAM"); configured {
		cfg.StartupWarnings = append(cfg.StartupWarnings, "HARRBOR_DIRECT_STREAM is removed and ignored; DarkHarrbor always proxies stream bytes")
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_HTTP_STREAM_ENABLED")); v != "" {
		cfg.HTTPStream.Enabled = v == "true" || v == "1"
	}
	setString(&cfg.HTTPStream.TMDBAPIKey, "HARRBOR_TMDB_API_KEY")
	if v := strings.TrimSpace(os.Getenv("HARRBOR_HTTP_ALLOW_PRIVATE_SOURCES")); v != "" {
		cfg.HTTPStream.AllowPrivateSourceCIDRs = v == "true" || v == "1"
	}
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("HARRBOR_HTTP_IA_FILE_PREFERENCE"))); v != "" {
		switch v {
		case "derivative", "original":
			cfg.HTTPStream.IAFilePreference = v
		default:
			cfg.StartupWarnings = append(cfg.StartupWarnings, "HARRBOR_HTTP_IA_FILE_PREFERENCE is unknown and ignored; using derivative")
			cfg.HTTPStream.IAFilePreference = "derivative"
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_HTTP_PREDICTIVE_RESOLVE_ENABLED")); v != "" {
		cfg.HTTPStream.PredictiveResolveEnabled = v == "true" || v == "1"
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_HTTP_PREDICTIVE_RESOLVE_MIN_LEAD_MS")); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil {
			cfg.HTTPStream.PredictiveResolveMinLeadMS = n
		} else {
			cfg.StartupWarnings = append(cfg.StartupWarnings, "HARRBOR_HTTP_PREDICTIVE_RESOLVE_MIN_LEAD_MS is not a valid integer and ignored")
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_HTTP_PREDICTIVE_RESOLVE_MAX_LEAD_MS")); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil {
			cfg.HTTPStream.PredictiveResolveMaxLeadMS = n
		} else {
			cfg.StartupWarnings = append(cfg.StartupWarnings, "HARRBOR_HTTP_PREDICTIVE_RESOLVE_MAX_LEAD_MS is not a valid integer and ignored")
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_HTTP_SELFHEAL_ENABLED")); v != "" {
		cfg.HTTPStream.SelfHealEnabled = v == "true" || v == "1"
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_HTTP_SELFHEAL_COOLDOWN_MIN")); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil {
			cfg.HTTPStream.SelfHealCooldownMin = n
		} else {
			cfg.StartupWarnings = append(cfg.StartupWarnings, "HARRBOR_HTTP_SELFHEAL_COOLDOWN_MIN is not a valid integer and ignored")
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_REQUIRE_CACHED")); v != "" {
		cfg.Governor.RequireCached = v == "true" || v == "1"
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_GOVERNOR_UNCACHED_BUDGET")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Governor.UncachedBudget = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_GOVERNOR_STALL_TIMEOUT_MIN")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Governor.StallTimeoutMin = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_GOVERNOR_DEAD_SENTINEL_GRACE_MIN")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Governor.DeadSentinelGraceMin = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_TORRENT_SUBMIT_BUDGET_MIN")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Governor.SubmitBudgetMin = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_GOVERNOR_ALTPROVIDER_FAILOVER")); v != "" {
		cfg.Governor.AltProviderFailoverEnabled = v == "true" || v == "1"
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_GOVERNOR_REPAIR_ENABLED")); v != "" {
		cfg.Governor.RepairEnabled = v == "true" || v == "1"
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_GOVERNOR_REPAIR_COOLDOWN_MIN")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.Governor.RepairCooldownMin = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_TORRENT_KEEPWARM_DAYS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.Governor.TorrentKeepWarmDays = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_SUPPRESSION_TTL_HOURS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Suppression.TTLHours = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_TORRENT_AVAILABILITY_TTL_HOURS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Availability.TTLHours = n
		}
	}
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("HARRBOR_IDENTITY_STRICTNESS"))); v != "" {
		switch v {
		case "off", "warn", "enforce":
			cfg.Identity.Strictness = v
		default:
			cfg.StartupWarnings = append(cfg.StartupWarnings, "HARRBOR_IDENTITY_STRICTNESS is unknown and ignored; using warn")
			cfg.Identity.Strictness = "warn"
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_PREWARM_NEXTEP_THRESHOLD")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.Prewarm.NextEpisodeThreshold = f
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_HEALTHAUDIT_COMPLETENESS_THRESHOLD")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.HealthAudit.CompletenessThreshold = f
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_NZB_CACHE")); v != "" {
		cfg.Routing.NZBTorBoxCache = v == "true" || v == "1"
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_PREFERENCE")); v != "" {
		cfg.Routing.Preference, cfg.Routing.PreferenceExplicit = applyPreference(v)
	}
	setString(&cfg.Prowlarr.BaseURL, "HARRBOR_PROWLARR_BASE_URL")
	setString(&cfg.Prowlarr.APIKey, "HARRBOR_PROWLARR_API_KEY")
	if v := strings.TrimSpace(os.Getenv("HARRBOR_PROWLARR_INDEXER_IDS")); v != "" {
		cfg.Prowlarr.IndexerIDs = cfg.Prowlarr.IndexerIDs[:0]
		for _, part := range strings.Split(v, ",") {
			if n, err := strconv.Atoi(strings.TrimSpace(part)); err == nil {
				cfg.Prowlarr.IndexerIDs = append(cfg.Prowlarr.IndexerIDs, n)
			}
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_PROBE_BYTES")); v != "" {
		if b, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.Probe.ProbeBytes = b
		}
	}
	setString(&cfg.Relay.FFprobeFullPath, "HARRBOR_FFPROBE_FULL_PATH")
	if v := strings.TrimSpace(os.Getenv("HARRBOR_RELAY_CONCURRENCY")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Relay.Concurrency = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_RELAY_TIMEOUT_SEC")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Relay.Timeout = time.Duration(n) * time.Second
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_RELAY_QUEUE_TIMEOUT_SEC")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Relay.QueueTimeout = time.Duration(n) * time.Second
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_RELAY_MAX_BODY_BYTES")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			cfg.Relay.MaxBodyBytes = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_RELAY_NEGATIVE_TTL_MIN")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.Relay.NegativeTTLMin = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_STRM_INSTALLER_ENABLED")); v != "" {
		cfg.StrmInstaller.Enabled = v == "true" || v == "1"
	}
	setString(&cfg.StrmInstaller.DockerProxyURL, "HARRBOR_DOCKER_PROXY_URL")
	if v := strings.TrimSpace(os.Getenv("HARRBOR_CLEANUP_HOURS")); v != "" {
		if h, err := strconv.Atoi(v); err == nil {
			cfg.Lifecycle.CleanupHours = h
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_NEVER_PLAYED_UNCACHED_HOURS")); v != "" {
		if h, err := strconv.Atoi(v); err == nil && h >= 0 {
			cfg.Lifecycle.NeverPlayedUncachedHours = h
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_FAILED_RETENTION_HOURS")); v != "" {
		if h, err := strconv.Atoi(v); err == nil && h >= 0 {
			cfg.Lifecycle.FailedRetention = time.Duration(h) * time.Hour
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_FAILED_RETENTION_MIN")); v != "" {
		if m, err := strconv.Atoi(v); err == nil && m >= 0 {
			cfg.Lifecycle.FailedRetention = time.Duration(m) * time.Minute
		}
	}
	if v := strings.TrimSpace(os.Getenv("HARRBOR_FAILED_PRUNE_INTERVAL_SEC")); v != "" {
		if s, err := strconv.Atoi(v); err == nil && s > 0 {
			cfg.Lifecycle.FailedPruneInterval = time.Duration(s) * time.Second
		}
	}
}

// SuppressionTTL returns the SF-03 suppression TTL as a time.Duration.
func (c *Config) SuppressionTTL() time.Duration {
	return time.Duration(c.Suppression.TTLHours) * time.Hour
}

// AvailabilityTTL returns the TS-5.1 hash-availability observation TTL as a
// time.Duration.
func (c *Config) AvailabilityTTL() time.Duration {
	return time.Duration(c.Availability.TTLHours) * time.Hour
}

func (c *Config) Validate() error {
	switch {
	case c.Server.Address == "":
		return errors.New("server.address is required")
	case c.Server.BaseURL == "":
		return errors.New("server.base_url is required")
	case c.Database.Path == "":
		return errors.New("database.path is required")
	case c.Data.Root == "":
		return errors.New("data.root is required")
	case c.Backup.Enabled && (!filepath.IsAbs(c.Backup.Target) || filepath.Clean(c.Backup.Target) == string(filepath.Separator) || strings.Contains(c.Backup.Target, "://")):
		return errors.New("backup.target must be a safe absolute directory")
	case c.Backup.IntervalHours < 1:
		return errors.New("backup.interval_hours must be >= 1")
	case c.Backup.Keep < 1:
		return errors.New("backup.keep must be >= 1")
	case c.Backup.Enabled && strings.TrimSpace(c.Backup.SecretsPath) == "":
		return errors.New("backup.secrets_path is required when backups are enabled")
	case c.Auth.QBitUsername == "":
		return errors.New("auth.qbit_username is required")
	case c.Governor.UncachedBudget < 1:
		return errors.New("governor.uncached_budget must be >= 1")
	case c.Governor.StallTimeoutMin < 1:
		return errors.New("governor.stall_timeout_min must be >= 1")
	case c.Governor.DeadSentinelGraceMin < 1:
		return errors.New("governor.dead_sentinel_grace_min must be >= 1")
	case c.Governor.SubmitBudgetMin < 1:
		return errors.New("governor.submit_budget_min must be >= 1")
	case c.Suppression.TTLHours < 1:
		return errors.New("suppression.ttl_hours must be >= 1")
	case c.Governor.WindowDays < 1:
		return errors.New("governor.window_days must be >= 1")
	case c.Probe.ProbeBytes < 65536:
		return errors.New("probe.probe_bytes must be >= 65536")
	case c.Lifecycle.CleanupHours < 1 || c.Lifecycle.CleanupHours > 48:
		return errors.New("lifecycle.cleanup_hours must be between 1 and 48")
	case c.Relay.Concurrency < 1:
		return errors.New("relay.concurrency must be >= 1")
	case c.Relay.MaxBodyBytes < 1:
		return errors.New("relay.max_body_bytes must be >= 1")
	case c.StrmInstaller.Enabled && strings.TrimSpace(c.StrmInstaller.DockerProxyURL) == "":
		return errors.New("strm_installer.docker_proxy_url is required when HARRBOR_STRM_INSTALLER_ENABLED=true")
	case c.Governor.TorrentCDNCapacity < 0:
		return errors.New("governor.torrent_cdn_capacity must be >= 0")
	case c.Governor.HTTPSourceCapacity < 0:
		return errors.New("governor.http_source_capacity must be >= 0")
	case c.HTTPStream.PredictiveResolveMinLeadMS < 0:
		return errors.New("http_stream.predictive_resolve_min_lead_ms must be >= 0")
	case c.HTTPStream.PredictiveResolveMaxLeadMS < 0:
		return errors.New("http_stream.predictive_resolve_max_lead_ms must be >= 0")
	case c.HTTPStream.PredictiveResolveMaxLeadMS > 0 && c.HTTPStream.PredictiveResolveMaxLeadMS < c.HTTPStream.PredictiveResolveMinLeadMS:
		return errors.New("http_stream.predictive_resolve_max_lead_ms must be >= predictive_resolve_min_lead_ms when set")
	case c.Cache.PinnedBudgetMB < 0:
		return errors.New("cache.pinned_budget_mb must be >= 0")
	case c.Cache.ReadaheadSeekWidenMS < 0:
		return errors.New("cache.readahead_seek_widen_ms must be >= 0")
	case c.Prewarm.NNTPBytes < 0:
		return errors.New("prewarm.nntp_bytes must be >= 0")
	case c.Prewarm.TimeoutSec < 1:
		return errors.New("prewarm.timeout_sec must be >= 1")
	case c.Prewarm.NextEpisodeThreshold < 0 || c.Prewarm.NextEpisodeThreshold > 1:
		return errors.New("prewarm.nextep_threshold must be between 0 and 1")
	case c.Prewarm.MinReadaheadWorkers < 1:
		return errors.New("prewarm.min_readahead_workers must be >= 1")
	case c.Prewarm.MaxReadaheadWorkers < c.Prewarm.MinReadaheadWorkers:
		return errors.New("prewarm.max_readahead_workers must be >= prewarm.min_readahead_workers")
	case c.HealthAudit.IntervalSec < 1:
		return errors.New("healthaudit.interval_sec must be >= 1")
	case c.HealthAudit.SampleSize < 1:
		return errors.New("healthaudit.sample_size must be >= 1")
	case c.HealthAudit.CompletenessThreshold < 0 || c.HealthAudit.CompletenessThreshold > 1:
		return errors.New("healthaudit.completeness_threshold must be between 0 and 1")
	case c.HealthAudit.MaxDeadRegions < 0:
		return errors.New("healthaudit.max_dead_regions must be >= 0")
	case c.HealthAudit.ReSearchCooldownMin < 0:
		return errors.New("healthaudit.research_cooldown_min must be >= 0")
	case c.HTTPStream.SelfHealCooldownMin < 0:
		return errors.New("http_stream.selfheal_cooldown_min must be >= 0")
	case c.GrabHealth.SampleSize < 1:
		return errors.New("grabhealth.sample_size must be >= 1")
	case c.GrabHealth.TimeoutMS < 1:
		return errors.New("grabhealth.timeout_ms must be >= 1")
	case c.IdentityAudit.IntervalSec < 1:
		return errors.New("identityaudit.interval_sec must be >= 1")
	case c.NNTP.WarmConns < 0:
		return errors.New("nntp.warm_conns must be >= 0")
	case c.Governor.TorrentKeepWarmDays < 0:
		return errors.New("governor.torrent_keepwarm_days must be >= 0")
	}
	if strings.TrimSpace(c.Server.StreamSecret) == "" {
		return errors.New("HARRBOR_STREAM_SECRET is required (min 32 chars); set in .env")
	}
	if len(strings.TrimSpace(c.Server.StreamSecret)) < 32 {
		return errors.New("HARRBOR_STREAM_SECRET must be at least 32 characters")
	}
	if err := c.Stremio.Validate(c.Server.StreamSecret); err != nil {
		return err
	}
	if strings.TrimSpace(c.Auth.QBitPassword) == "darkharrbor" {
		return errors.New("auth.qbit_password must not be the default value 'darkharrbor'; set HARRBOR_QBIT_PASSWORD in .env")
	}
	if strings.TrimSpace(c.Auth.SABAPIKey) == "darkharrbor-sab-key" {
		return errors.New("auth.sab_api_key must not be the default value 'darkharrbor-sab-key'; set HARRBOR_SAB_API_KEY in .env")
	}
	if c.LaneEnabled(LaneTorBoxTorrent) || c.LaneEnabled(LaneTorBoxNZB) {
		if err := validateSecret("torbox.api_token", c.TorBox.APIToken); err != nil {
			return err
		}
	}
	if err := validateSecret("auth.qbit_password", c.Auth.QBitPassword); err != nil {
		return err
	}
	if err := validateSecret("auth.sab_api_key", c.Auth.SABAPIKey); err != nil {
		return err
	}
	if err := validateCacheModes(c); err != nil {
		return err
	}
	return nil
}

// validCacheMode reports whether m is a supported cache mode string.
func validCacheMode(m string) bool {
	switch m {
	case "none", "readahead", "disk", "full":
		return true
	}
	return false
}

// validateCacheModes checks Mode and the per-path overrides, the stub
// authority selector, and the CDN window size (gate
// 11.feature_cache_unification).
func validateCacheModes(c *Config) error {
	if !validCacheMode(c.Cache.Mode) {
		return fmt.Errorf("HARRBOR_CACHE_MODE must be one of none|readahead|disk|full, got %q", c.Cache.Mode)
	}
	if c.Cache.NNTPMode != "" && !validCacheMode(c.Cache.NNTPMode) {
		return fmt.Errorf("HARRBOR_NNTP_CACHE_MODE must be one of none|readahead|disk|full, got %q", c.Cache.NNTPMode)
	}
	if c.Cache.StreamMode != "" && !validCacheMode(c.Cache.StreamMode) {
		return fmt.Errorf("HARRBOR_STREAM_CACHE_MODE must be one of none|readahead|disk|full, got %q", c.Cache.StreamMode)
	}
	switch c.Cache.FullEvictMode {
	case "ttl", "manual", "never":
	default:
		return fmt.Errorf("HARRBOR_FULL_EVICT_MODE must be one of ttl|manual|never, got %q", c.Cache.FullEvictMode)
	}
	switch c.Stub.Authority {
	case "db", "fs":
	default:
		return fmt.Errorf("HARRBOR_STUB_AUTHORITY must be one of db|fs, got %q", c.Stub.Authority)
	}
	if c.Cache.StreamChunkSizeMB < 1 {
		return fmt.Errorf("HARRBOR_STREAM_CHUNK_SIZE_MB must be >= 1, got %d", c.Cache.StreamChunkSizeMB)
	}
	if c.Cache.StreamMinBufferSegments < 0 {
		return fmt.Errorf("HARRBOR_STREAM_MIN_BUFFER_SEGMENTS must be >= 0, got %d", c.Cache.StreamMinBufferSegments)
	}
	return nil
}

func validateSecret(field, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("%s is required", field)
	}
	if isEnvPlaceholder(value) {
		return fmt.Errorf("%s must be resolved before startup", field)
	}
	return nil
}

func isEnvPlaceholder(value string) bool {
	return strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}")
}

func loadDotEnv(path string) error {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("parse %s:%d: missing '='", path, lineNo)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			return fmt.Errorf("parse %s:%d: empty key", path, lineNo)
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		parsed, err := parseDotEnvValue(value)
		if err != nil {
			return fmt.Errorf("parse %s:%d: %w", path, lineNo, err)
		}
		if err := os.Setenv(key, parsed); err != nil {
			return fmt.Errorf("set %s from %s:%d: %w", key, path, lineNo, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan %s: %w", path, err)
	}
	return nil
}

func parseDotEnvValue(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if strings.HasPrefix(value, "\"") {
		parsed, err := strconv.Unquote(value)
		if err != nil {
			return "", fmt.Errorf("invalid quoted value: %w", err)
		}
		return parsed, nil
	}
	if strings.HasPrefix(value, "'") {
		if len(value) < 2 || !strings.HasSuffix(value, "'") {
			return "", errors.New("invalid single-quoted value")
		}
		return value[1 : len(value)-1], nil
	}
	return value, nil
}
