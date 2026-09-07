package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"log/slog"
	mrand "math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/alldebrid"
	"github.com/darkharrbor/darkharrbor/internal/api"
	archiveparser "github.com/darkharrbor/darkharrbor/internal/archive"
	"github.com/darkharrbor/darkharrbor/internal/auth"
	"github.com/darkharrbor/darkharrbor/internal/availability"
	"github.com/darkharrbor/darkharrbor/internal/backup"
	"github.com/darkharrbor/darkharrbor/internal/cachegate"
	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/dockerctl"
	"github.com/darkharrbor/darkharrbor/internal/doctor"
	"github.com/darkharrbor/darkharrbor/internal/eventwatcher"
	"github.com/darkharrbor/darkharrbor/internal/experience"
	"github.com/darkharrbor/darkharrbor/internal/governor"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/httpstream/generic"
	"github.com/darkharrbor/darkharrbor/internal/httpstream/ia"
	"github.com/darkharrbor/darkharrbor/internal/httpstream/omss"
	"github.com/darkharrbor/darkharrbor/internal/httpstream/stremio"
	"github.com/darkharrbor/darkharrbor/internal/identitycheck"
	"github.com/darkharrbor/darkharrbor/internal/mediatruth"
	"github.com/darkharrbor/darkharrbor/internal/nntp"
	"github.com/darkharrbor/darkharrbor/internal/output"
	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
	"github.com/darkharrbor/darkharrbor/internal/premiumize"
	"github.com/darkharrbor/darkharrbor/internal/probe"
	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
	"github.com/darkharrbor/darkharrbor/internal/reactivecommit"
	"github.com/darkharrbor/darkharrbor/internal/reactivedispatch"
	"github.com/darkharrbor/darkharrbor/internal/reactivequeue"
	"github.com/darkharrbor/darkharrbor/internal/realdebrid"
	"github.com/darkharrbor/darkharrbor/internal/securefile"
	"github.com/darkharrbor/darkharrbor/internal/sidecar"
	"github.com/darkharrbor/darkharrbor/internal/store"
	"github.com/darkharrbor/darkharrbor/internal/suppress"
	"github.com/darkharrbor/darkharrbor/internal/torbox"
	"github.com/darkharrbor/darkharrbor/internal/torrentmeta"
	"github.com/darkharrbor/darkharrbor/internal/util"
	"github.com/darkharrbor/darkharrbor/internal/wizard"
)

const (
	maxResolveAttempts   = 6
	maxStubAttempts      = 6
	maintenanceInterval  = time.Hour
	nestedArchiveFailure = "nested archive not supported for live streaming"
	// warmPoolTickInterval is NS-5.1's fixed warm-pool keeper cadence --
	// comfortably inside the NNTP pool's own 5-minute IdleTimeout so a
	// warm connection is pinged well before it would otherwise lapse.
	// Not operator-configurable: HARRBOR_NNTP_WARM_CONNS (the target) is
	// the row's own named config surface per the frozen plan.
	warmPoolTickInterval = 2 * time.Minute
	// keepWarmTickInterval is TS-4.2's fixed keep-warm janitor cadence --
	// deliberately fixed, not configurable (mirrors warmPoolTickInterval's
	// own precedent): only the "recent play" window
	// (HARRBOR_TORRENT_KEEPWARM_DAYS) is this row's own named config knob.
	// An hour is comfortably fine-grained against the row's own 5-day
	// keepWarmLeadDays buffer.
	keepWarmTickInterval = time.Hour
	// decayAuditTickInterval is TS-4.3's fixed decay-audit janitor cadence
	// -- deliberately fixed, not configurable (same warmPoolTickInterval/
	// keepWarmTickInterval precedent): only the whole-pass on/off switch
	// (HARRBOR_TORRENT_DECAYAUDIT_ENABLED) is this row's own named config
	// knob. Ten minutes between ticks, each sampling up to
	// decayAuditCandidatesPerTick (5) items via one live provider
	// CheckCached API call per item, keeps a realistically-sized library's
	// full-rotation period on the order of hours-to-a-day while staying
	// comfortably rate-capped against provider API load, mirroring NS-6.1's
	// own "slow cadence, rate-capped" design intent for its NNTP sibling.
	decayAuditTickInterval = 10 * time.Minute
)

// providerLane bundles one debrid provider with its own independent gate and
// governor. Each lane is self-contained — no cross-lane knowledge or fallback.
// C-5: submitItem walks lanes in order; first to accept wins, binding the item
// to that provider via item.Provider.
type providerLane struct {
	prov provider.Provider
	gate *cachegate.Gate
	gov  *governor.Governor
}

func main() {
	// `darkharrbor seal` (F-D): seal env-style KEY=VALUE lines from stdin into an
	// encrypted secrets file; prints the generated password exactly once.
	if len(os.Args) > 1 && os.Args[1] == "seal" {
		os.Exit(runSealCommand(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "deadletter" {
		os.Exit(runDeadletterCommand(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "setup" {
		os.Exit(runSetupCommand(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "catalog" {
		os.Exit(runCatalogCommand(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "configure" {
		os.Exit(runConfigureCommand(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "reconcile-arr" {
		os.Exit(runArrReconcileCommand(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "restore" {
		os.Exit(runRestoreCommand(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "backup" {
		os.Exit(runBackupCommand(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "readopt" {
		os.Exit(runReAdoptCommand(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "demo" {
		os.Exit(runDemoCommand(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "doctor" {
		os.Exit(runDoctorCommand(os.Args[2:]))
	}
	if len(os.Args) > 1 && (os.Args[1] == "reactive-commit" || os.Args[1] == "reactive-undo") {
		os.Exit(runReactiveCommand(os.Args[1], os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "reactive-pending" {
		os.Exit(runReactivePendingCommand(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "reactive-promote" {
		os.Exit(runReactivePromoteCommand(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "monitor-flip" {
		os.Exit(runMonitorFlipCommand(os.Args[2:]))
	}

	// Anything else that LOOKS like a subcommand must not fall through to
	// daemon startup. It previously did: `darkharrbor --help` and any typo
	// silently started the server, which on a host with no running daemon
	// means a second daemon rather than an error, and left the CLI with no
	// discoverable surface at all. A bare `darkharrbor` (no argument) is
	// still the daemon entrypoint, because that is what the container
	// entrypoint invokes.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "help", "-h", "--help":
			printCLIUsage(os.Stdout)
			os.Exit(0)
		default:
			fmt.Fprintf(os.Stderr, "darkharrbor: unknown command %q\n\n", os.Args[1])
			printCLIUsage(os.Stderr)
			os.Exit(2)
		}
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	level := configuredLogLevel(cfg)
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	for _, warning := range cfg.StartupWarnings {
		log.Warn("configuration warning", "detail", warning)
	}

	daemonLock, err := backup.AcquireLock(filepath.Dir(cfg.Database.Path))
	if err != nil {
		log.Error("failed to acquire daemon lock")
		os.Exit(1)
	}
	defer backup.ReleaseLock(daemonLock)

	startupHandler := newStartupSwitchHandler()
	httpSrv := &http.Server{
		Addr:              cfg.Server.Address,
		Handler:           startupHandler,
		ReadHeaderTimeout: 30 * time.Second,
	}
	var stremioHTTPServer *http.Server
	listener, err := net.Listen("tcp", cfg.Server.Address)
	if err != nil {
		log.Error("http listener bind failed", "error", err)
		os.Exit(1)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info("darkharrbor listener bound; initialization in progress", "addr", cfg.Server.Address)
		if err := httpSrv.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Error("http server error", "error", err)
			os.Exit(1)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db, err := store.Open(ctx, cfg.Database.Path, cfg.Database.BusyTimeout)
	if err != nil {
		log.Error("failed to open database", "error", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		log.Error("failed to run migrations", "error", err)
		os.Exit(1)
	}

	st := store.NewWithSealKey(db, credentialSealKey(cfg))
	offsetRecorder := store.NewSegmentOffsetRecorder(st, log)
	defer func() {
		if err := offsetRecorder.Close(); err != nil {
			log.Error("segment offset recorder shutdown failed", "error", err)
		}
	}()

	// TorBox client-side limits — the server remains the authority:
	//   createtorrent/createusenetdownload: 10/min edge + 60/hour budget
	//   all operations:                     300/min global
	// 429 responses with Retry-After install per-op-class hold-until
	// deadlines (fed back by the HTTP client). Plan pre-check values come
	// from configuration only; zero leaves them unenforced client-side.
	planSlots := lookupInt(cfg, "HARRBOR_PROVIDER_TORBOX_PLAN_SLOTS")
	planMaxBytes := lookupInt64(cfg, "HARRBOR_PROVIDER_TORBOX_PLAN_MAX_BYTES")
	torboxPolicy := torbox.NewTorBoxPolicy(planSlots, planMaxBytes)
	defer torboxPolicy.Stop()

	torboxClient := torbox.NewHTTPClient(
		log,
		cfg.TorBox.BaseURL,
		cfg.TorBox.APIToken,
		cfg.TorBox.UserAgent,
		cfg.TorBox.RequestTimeout,
		torboxPolicy,
	)

	// Plan awareness: discover slots/caps from TorBox /user/me once at startup.
	// Discovery never blocks startup; on failure, configured overrides are kept
	// and unset values fall back to conservative Free-plan caps.
	discoveredSlots, discoveredMaxBytes, discoveredUsenet := planSlots, planMaxBytes, false
	discoveredFreeTier := true // conservative default until discovery/fallback runs
	// newsServerCapable gates the "torbox" usenet-provider factory below
	// (TorBox NNTP News Server, v9.0.0) — same startup capability-detection
	// pattern as every other plan-derived gate here (discoveredUsenet,
	// discoveredSlots, etc.), not a separate ad-hoc check at use time.
	newsServerCapable := false
	// airlockQuotaBytes: TorBox AirLock permanent-storage quota (v9.0.0),
	// discovered the same way as every other plan-derived capability here.
	// No consumer yet (AirLock's tagging UX is still an open design
	// question, REDESIGN §16) — this only makes the fact discoverable at
	// startup, per the standing instruction that provider capabilities
	// always go through startup detection rather than being derived ad hoc
	// wherever they're eventually used.
	airlockQuotaBytes := int64(0)
	applyFreeFallback := func() {
		fallbackSlots, fallbackMaxBytes, fallbackUsenet, fallbackFreeTier := torbox.ResolveCaps(nil, time.Now())
		if discoveredSlots == 0 {
			discoveredSlots = fallbackSlots
		}
		if discoveredMaxBytes == 0 {
			discoveredMaxBytes = fallbackMaxBytes
		}
		discoveredUsenet = fallbackUsenet
		discoveredFreeTier = fallbackFreeTier
		newsServerCapable = false
		airlockQuotaBytes = 0
	}
	if cfg.TorBox.APIToken == "" {
		applyFreeFallback()
		log.Warn("plan discovery skipped, using config defaults", "reason", "HARRBOR_TORBOX_API_TOKEN is empty")
	} else {
		discCtx, discCancel := context.WithTimeout(context.Background(), 10*time.Second)
		info, discErr := torboxClient.GetUserInfo(discCtx)
		discCancel()
		if discErr != nil {
			applyFreeFallback()
			log.Warn("plan discovery failed, using config defaults", "error", discErr)
		} else {
			accountNow := time.Now()
			dSlots, dMaxBytes, dUsenet, dFreeTier := torbox.ResolveCaps(info, accountNow)
			planName, verifiedFreeTier, unknownPlanFallback := torbox.AccountPlan(info, accountNow)
			// Manual config overrides win over discovery.
			if planSlots == 0 {
				discoveredSlots = dSlots
			}
			if planMaxBytes == 0 {
				discoveredMaxBytes = dMaxBytes
			}
			discoveredUsenet = dUsenet
			discoveredFreeTier = dFreeTier
			newsServerCapable = torbox.NewsServerCapable(info, accountNow)
			airlockQuotaBytes = torbox.AirlockQuotaBytes(info, accountNow)
			log.Info("plan discovery OK",
				"plan_code", info.Plan,
				"plan_name", planName,
				"effective_slots", discoveredSlots,
				"max_bytes", discoveredMaxBytes,
				"usenet_capable", discoveredUsenet,
				"conservative_free_policy", discoveredFreeTier,
				"verified_free_tier", verifiedFreeTier,
				"unknown_plan_fallback", unknownPlanFallback,
				"news_server_capable", newsServerCapable,
				"airlock_quota_bytes", airlockQuotaBytes,
				"cooldown_until", info.CooldownUntil,
			)
		}
	}

	usenetCap := discoveredUsenet
	if override, ok := cfg.Lookup("HARRBOR_TORBOX_USENET"); ok {
		switch strings.ToLower(strings.TrimSpace(override)) {
		case "on":
			usenetCap = true
		case "off":
			usenetCap = false
		case "", "auto":
			// Use discovered/default capability.
		default:
			log.Warn("invalid HARRBOR_TORBOX_USENET value, using discovered capability", "value", override)
		}
	}
	torboxPolicy.SetPlan(discoveredSlots, discoveredMaxBytes)
	// C-1: TorBox has a CacheOracle (checkcached) and a meaningful SlotModel
	// (Allowed Active Slots). Set both on the capability struct so future
	// multi-provider fan-out code can branch without re-checking provider name.
	torboxCaps := provider.Capabilities{
		UsenetIngest: usenetCap,
		CacheOracle:  true,
		SlotModel:    true,
	}

	gov := governor.New(st, cfg.Governor.UncachedBudget, cfg.Governor.WindowDays)

	qbitAuth := auth.NewQBitSessionManager(st, cfg.Auth.QBitUsername, cfg.Auth.QBitPassword, cfg.Auth.SessionTTL)
	sabAuth := auth.NewSABAuth(cfg.Auth.SABAPIKey, cfg.Auth.SABNZBKey)

	stagingRoot := filepath.Join(cfg.Data.Root, ".staging")
	prober := probe.New(log, cfg.Probe.Timeout)
	writer := output.NewStrmWriter(cfg.Data.Strm, stagingRoot)
	writer.SetStreamSecret(cfg.Server.StreamSecret)
	if sidecars, sidecarErr := sidecar.New(st, sidecar.Options{}); sidecarErr != nil {
		log.Error("sidecar registry unavailable", "error", sidecarErr)
	} else {
		writer.SetSidecarRegistry(sidecars)
	}

	// With authority "db" (default), output.StrmWriter persists to SQLite so
	// .strm URLs are the authoritative source and on-disk copies are
	// materialization receipts reconciled DB→disk. Stubs were removed in P3.
	if cfg.Stub.Authority == "db" {
		writer.SetBlobSink(st, cfg.Data.Root)
		log.Info("strm authority: db (SQLite-authoritative, materialized to disk)")
	} else {
		log.Info("strm authority: fs (filesystem-only, no DB reconcile)")
	}

	// RX-5.2: supervised proposals already enter RX-4.2's sole queue through
	// its durable trigger. Explicit auto mode adds one bounded consumer; OFF
	// constructs no mutation owner and remains the shipping default.
	if cfg.Reactive.CommitMode != "off" {
		var dispatcher *reactivedispatch.Dispatcher
		var dispatchErr error
		if cfg.Reactive.CommitMode == "supervised" {
			dispatcher, dispatchErr = reactivedispatch.New("supervised", st, nil, nil, nil)
		} else {
			// RD-34 D8: `arr_instances` is the source of truth. Building from
			// cfg.Arrs alone left a wizard-discovered deployment with ZERO
			// targets, so every entry parked regardless of configuration.
			// Credentials are resolved from sealed secrets at use time and are
			// never persisted here.
			arrInstances, instErr := st.ListArrInstances(ctx)
			if instErr != nil {
				log.Warn("reactive: stored arr discovery unavailable, falling back to configured arrs")
			}
			arrTargets, unresolvedArrs := reactivequeue.ResolveTargets(arrInstances, cfg.Arrs, cfg.Lookup)
			for _, name := range unresolvedArrs {
				log.Warn("reactive: arr excluded, API key reference did not resolve", "arr", name)
			}
			commitService, err := reactivecommit.New(st, reactivecommit.NewArrClient(arrTargets), writer, reactivecommit.Options{
				MonitorEpisodes: cfg.Reactive.MonitorEpisodes,
				MonitorMovie:    cfg.Reactive.MonitorMovies,
			})
			if err == nil {
				var router *reactivequeue.Router
				router, err = reactivequeue.NewRouter(st, arrTargets, nil)
				if err == nil {
					router.SetDefaultDestinations(cfg.Reactive.DefaultSeriesArr, cfg.Reactive.DefaultMovieArr)
				}
				if err == nil {
					dispatcher, dispatchErr = reactivedispatch.New("auto", st, router, commitService, nil)
				}
			}
			if err != nil {
				dispatchErr = err
			}
		}
		if dispatchErr != nil {
			log.Error("reactive dispatcher unavailable; failing closed")
		} else if cfg.Reactive.CommitMode == "supervised" {
			dispatcher.SetLogger(log)
			syncCtx, syncCancel := context.WithTimeout(ctx, 5*time.Second)
			_, syncErr := dispatcher.Process(syncCtx)
			syncCancel()
			if syncErr != nil {
				log.Error("reactive supervised queue sync failed")
			}
			log.Info("reactive dispatcher enabled", "mode", "supervised")
		} else {
			dispatcher.SetLogger(log)
			wg.Add(1)
			go func() {
				defer wg.Done()
				// RD-29: this summary was previously emitted once per process
				// lifetime, so a queue entry failing identically on every
				// cycle produced exactly one line and then went silent. Log
				// whenever the outcome CHANGES instead. Because a handled
				// entry leaves supervised_review, the steady state is
				// Examined == 0, which logs nothing -- this is self-quieting
				// and cannot become a per-30s repeat of an unchanged result.
				var lastResult reactivedispatch.Result
				reported := false
				cycle := func() {
					cycleCtx, cycleCancel := context.WithTimeout(ctx, 25*time.Second)
					result, err := dispatcher.Process(cycleCtx)
					cycleCancel()
					if err != nil && ctx.Err() == nil {
						log.Error("reactive automatic dispatch cycle failed")
					} else if result.Examined > 0 && (!reported || result != lastResult) {
						log.Info("reactive automatic dispatch cycle", "examined", result.Examined,
							"committed", result.Committed, "parked", result.Parked, "errors", result.Errors)
						lastResult = result
						reported = true
					}
				}
				cycle()
				ticker := time.NewTicker(30 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						cycle()
					}
				}
			}()
			log.Info("reactive dispatcher enabled", "mode", "auto")
		}
	} else {
		log.Info("reactive dispatcher disabled")
	}

	// F-B: one shared byte-range cache serves both the NNTP /dav path and the
	// torrent/CDN /stream byte-proxy path. Construct it once here; the NNTP
	// SegmentCache (attachSegmentCache) and the api Server both reference it.
	// Pools are owned per-source; the cache is closed by this wiring.
	coverage, err := playbackcoverage.New(st, playbackcoverage.Options{
		ProposalEnabled: cfg.Reactive.Enabled, ProposalThreshold: cfg.Reactive.Threshold,
	})
	if err != nil {
		log.Error("playback coverage unavailable", "error", err)
	}
	streamCache := rangecache.New(rangecacheConfigFromCfg(cfg), log, coverage)
	defer streamCache.Close()

	// TS-1.4: shared account governor arbitrating priority between live
	// torrent/debrid-CDN playback fetches and prewarm for the same account
	// (LC-06); the experience Service is the shared prewarm/seek-target/
	// adaptive-readahead owner over rangecache/mediatruth. Both nil-safe
	// throughout if cfg.Prewarm.Enabled is false — every consuming call site
	// degrades to its pre-TS-1.4 behavior exactly.
	torrentGov := accountgov.New("torbox-cdn")
	torrentGov.SetCapacity(api.TorrentCDNGovOp, cfg.Governor.TorrentCDNCapacity)
	expSvc := experience.New(streamCache, torrentGov, api.TorrentCDNGovOp, experience.Config{
		Enabled:              cfg.Prewarm.Enabled,
		HotHeadBytes:         cfg.Prewarm.HotHeadMB * 1024 * 1024,
		Timeout:              time.Duration(cfg.Prewarm.TimeoutSec) * time.Second,
		NextEpisodeThreshold: cfg.Prewarm.NextEpisodeThreshold,
		MinReadaheadWorkers:  cfg.Prewarm.MinReadaheadWorkers,
		MaxReadaheadWorkers:  cfg.Prewarm.MaxReadaheadWorkers,
	}, log, nil)

	// HR5.3: a second, independent account governor for the HTTP lane
	// (distinct from torrentGov — HTTP sources are not one shared provider
	// account the way TorBox CDN is) plus a second experience.Service
	// instance bound to it. Both share the SAME streamCache (no parallel
	// cache hierarchy) and the SAME cfg.Prewarm settings TS-1.4 already
	// established; only the governor account/op differ. Nil-unsafe fields
	// throughout degrade exactly like torrentGov/expSvc do when
	// cfg.Governor.HTTPSourceCapacity is left at its default (0, unbounded).
	httpGov := accountgov.New("http-source")
	httpGov.SetCapacity(api.HTTPSourceGovOp, cfg.Governor.HTTPSourceCapacity)
	httpExpSvc := experience.New(streamCache, httpGov, api.HTTPSourceGovOp, experience.Config{
		Enabled:              cfg.Prewarm.Enabled,
		HotHeadBytes:         cfg.Prewarm.HotHeadMB * 1024 * 1024,
		Timeout:              time.Duration(cfg.Prewarm.TimeoutSec) * time.Second,
		NextEpisodeThreshold: cfg.Prewarm.NextEpisodeThreshold,
		MinReadaheadWorkers:  cfg.Prewarm.MinReadaheadWorkers,
		MaxReadaheadWorkers:  cfg.Prewarm.MaxReadaheadWorkers,
	}, log, nil)
	// NS-5.2: NNTP uses the same experience owner and shared stream cache,
	// but its reserved readahead connection pool is the admission boundary.
	// The lane-specific byte budget is independent from torrent/HTTP hot-head
	// policy and 0 disables only this wiring.
	nntpExpSvc := experience.New(streamCache, nil, "", experience.Config{
		Enabled:      cfg.Prewarm.Enabled && cfg.Prewarm.NNTPBytes > 0,
		HotHeadBytes: cfg.Prewarm.NNTPBytes,
		Timeout:      time.Duration(cfg.Prewarm.TimeoutSec) * time.Second,
		// NS-5.4: reuses the existing cross-lane NextEpisodeThreshold knob
		// (already consumed by expSvc/httpExpSvc above) rather than adding a
		// new NNTP-specific key. Without this, experience.Service.
		// NextEpisodePrewarm's threshold gate defaulted to the Go zero value
		// (0.0), which never actually holds back a prewarm at any nonzero
		// playback fraction.
		NextEpisodeThreshold: cfg.Prewarm.NextEpisodeThreshold,
		// NS-5.5: bound the adaptive readahead recommendation to the NNTP
		// lane's own readahead-pool ceiling. Left unset (both default to 1
		// via experience.New's own floor) this would silently clamp EVERY
		// NNTP recommendation to exactly 1 worker the moment any per-source
		// throughput measurement exists -- a real regression the row's
		// gates caught, not a design choice. MaxReadaheadWorkers matches
		// nntpReadaheadCeiling exactly so this service can never recommend
		// more workers than the session can actually spawn.
		MinReadaheadWorkers: 1,
		MaxReadaheadWorkers: nntpReadaheadCeiling(cfg),
	}, log, nil)
	// configured providers; refuse start on unknown / duplicate / explicitly
	// empty selections; a selected-but-unconfigured provider is skipped with
	// a warning (preserves the legacy unset-HARRBOR_NNTP_HOST boot).
	registry := provider.NewRegistry()
	if err := registry.Register("torbox", func(bc provider.BuildContext) (provider.Provider, error) {
		if cfg.TorBox.APIToken == "" {
			return nil, fmt.Errorf("%w: HARRBOR_TORBOX_API_TOKEN is empty", provider.ErrNotConfigured)
		}
		// C-2: Policy construction is now inside the factory so each provider
		// instance owns its own policy (required for multi-debrid correctness;
		// today only one TorBox instance exists, but the pattern is binding).
		// The outer torboxPolicy is kept alive for SetPlan updates that must
		// reach the same instance; since only one factory call occurs (single
		// TorBox provider), the inner policy IS the outer policy here.
		return torbox.NewAdapter(bc.Name, torboxClient, torboxPolicy).SetCapabilities(torboxCaps).SetSlotCaps(discoveredSlots, discoveredFreeTier), nil
	}); err != nil {
		log.Error("provider registry", "error", err)
		os.Exit(1)
	}

	// Real-Debrid provider (P6). Plan discovery at startup, same pattern as
	// TorBox: conservative non-premium fallback on failure. RD has no slot
	// ceiling (unlimited on Premium) so SlotModel=false; no usenet ingest.
	rdAPIToken, _ := cfg.Lookup("HARRBOR_RD_API_TOKEN")
	rdPremium := false
	if rdAPIToken != "" {
		rdPolicy := realdebrid.NewRealDebridPolicy()
		rdClient := realdebrid.NewHTTPClient(
			"", // default base URL
			rdAPIToken,
			"", // default user agent
			0,  // default timeout
			rdPolicy,
		)
		rdDiscCtx, rdDiscCancel := context.WithTimeout(context.Background(), 10*time.Second)
		rdInfo, rdDiscErr := rdClient.GetUser(rdDiscCtx)
		rdDiscCancel()
		if rdDiscErr != nil {
			log.Warn("rd: plan discovery failed, provider will be skipped", "error", rdDiscErr)
		} else {
			rdPremiumDiscovered, _, _ := realdebrid.ResolveCaps(rdInfo)
			rdPremium = rdPremiumDiscovered
			log.Info("rd: plan discovery OK",
				"premium", rdPremium,
				"expiration_days", rdInfo.ExpirationDays,
			)
		}

		rdCaps := provider.Capabilities{
			CacheOracle:  false, // instantAvailability disabled (error_code 37, 2026-07-11)
			SlotModel:    true,  // /torrents/activeCount limit=100 on Premium
			UsenetIngest: false,
		}
		rdGovBudget := cfg.Governor.UncachedBudget // reuse global default; RD-specific knob in P6-D
		rdGov := governor.New(st, rdGovBudget, cfg.Governor.WindowDays)
		// RD has SlotModel=true with ceiling=100; governor gates on slot headroom.
		// SlotSource is implemented by the adapter via SetSlotCaps.

		if err := registry.Register("realdebrid", func(bc provider.BuildContext) (provider.Provider, error) {
			token, _ := bc.Lookup("HARRBOR_RD_API_TOKEN")
			if token == "" {
				return nil, fmt.Errorf("%w: HARRBOR_RD_API_TOKEN is empty", provider.ErrNotConfigured)
			}
			if !rdPremium {
				return nil, fmt.Errorf("%w: RD account is not premium", provider.ErrNotConfigured)
			}
			rdPolicyInner := realdebrid.NewRealDebridPolicy()
			rdClientInner := realdebrid.NewHTTPClient("", token, "", 0, rdPolicyInner)
			rdAdapter := realdebrid.NewAdapter(bc.Name, rdClientInner, rdPolicyInner).
				SetCapabilities(rdCaps).SetPremium(rdPremium).
				SetSlotCaps(realdebrid.PremiumActiveSlots)
			// Gate stored for future C-4 multi-provider fan-out.
			// For now RD items route through the same gate as TorBox
			// (CacheOracle=false means gate.Check always allows uncached via governor).
			_ = cachegate.New(rdAdapter, rdGov, cfg.Governor.RequireCached)
			return rdAdapter, nil
		}); err != nil {
			log.Error("rd: provider registry", "error", err)
			os.Exit(1)
		}
	}

	// AllDebrid provider (PV-01). Upload reports instant readiness, but there
	// is no pre-submit oracle, so cached-only work uses add-then-verify.
	// Its 30-active-magnet ceiling is independent of TorBox's slot contract.
	adAPIKey, _ := cfg.Lookup("HARRBOR_ALLDEBRID_API_KEY")
	adPolicy := alldebrid.NewPolicy()
	defer adPolicy.Stop()
	adClient := alldebrid.NewHTTPClient("", adAPIKey, "", 0, adPolicy)
	adPremium := false
	if adAPIKey != "" {
		adDiscCtx, adDiscCancel := context.WithTimeout(context.Background(), 10*time.Second)
		adInfo, adDiscErr := adClient.GetUser(adDiscCtx)
		adDiscCancel()
		if adDiscErr != nil {
			log.Warn("alldebrid: plan discovery failed, provider will be skipped", "error", adDiscErr)
		} else {
			adPremium = adInfo.Premium
			log.Info("alldebrid: plan discovery OK", "premium", adPremium)
		}
	}
	if err := registry.Register("alldebrid", func(bc provider.BuildContext) (provider.Provider, error) {
		key, _ := bc.Lookup("HARRBOR_ALLDEBRID_API_KEY")
		if key == "" {
			return nil, fmt.Errorf("%w: HARRBOR_ALLDEBRID_API_KEY is empty", provider.ErrNotConfigured)
		}
		if !adPremium {
			return nil, fmt.Errorf("%w: AllDebrid account is not premium", provider.ErrNotConfigured)
		}
		return alldebrid.NewAdapter(bc.Name, adClient).
			SetPremium(true).
			SetCapabilities(provider.Capabilities{
				CacheOracle: false, SlotModel: true, UsenetIngest: false,
			}), nil
	}); err != nil {
		log.Error("alldebrid: provider registry", "error", err)
		os.Exit(1)
	}

	// Premiumize provider (PV-02). Its official cache/check endpoint is an
	// authoritative pre-submit oracle. Premiumize publishes no numeric request
	// or active-slot ceiling, so policy uses provider-directed holds only.
	pmAPIKey, _ := cfg.Lookup("HARRBOR_PREMIUMIZE_API_KEY")
	pmPolicy := premiumize.NewPolicy()
	defer pmPolicy.Stop()
	pmClient := premiumize.NewHTTPClient("", pmAPIKey, "", 0, pmPolicy)
	pmPremium := false
	if pmAPIKey != "" {
		pmDiscCtx, pmDiscCancel := context.WithTimeout(context.Background(), 10*time.Second)
		pmInfo, pmDiscErr := pmClient.GetAccount(pmDiscCtx)
		pmDiscCancel()
		if pmDiscErr != nil {
			log.Warn("premiumize: plan discovery failed, provider will be skipped", "error", pmDiscErr)
		} else {
			pmPremium = premiumize.PremiumAt(pmInfo, time.Now())
			log.Info("premiumize: plan discovery OK", "premium", pmPremium)
		}
	}
	if err := registry.Register("premiumize", func(bc provider.BuildContext) (provider.Provider, error) {
		key, _ := bc.Lookup("HARRBOR_PREMIUMIZE_API_KEY")
		if key == "" {
			return nil, fmt.Errorf("%w: HARRBOR_PREMIUMIZE_API_KEY is empty", provider.ErrNotConfigured)
		}
		if !pmPremium {
			return nil, fmt.Errorf("%w: Premiumize account is not premium", provider.ErrNotConfigured)
		}
		return premiumize.NewAdapter(bc.Name, pmClient).
			SetPremium(true).
			SetCapabilities(provider.Capabilities{
				CacheOracle: true, SlotModel: false, UsenetIngest: false,
			}), nil
	}); err != nil {
		log.Error("premiumize: provider registry", "error", err)
		os.Exit(1)
	}

	// Usenet factories: "newshosting" reads the legacy HARRBOR_NNTP_* keys;
	// any additional configured name reads HARRBOR_PROVIDER_<NAME>_* keys
	// through cfg.Lookup (tuning file for pool sizes, sealed store for
	// credentials; scenario U-1). Pool and
	// segment-cache closers are collected because factories run inside
	// registry.Build rather than at top-level defer scope.
	var providerClosers []func()
	defer func() {
		for i := len(providerClosers) - 1; i >= 0; i-- {
			providerClosers[i]()
		}
	}()

	registerUsenetFactory := func(name string, cfgFor func(bc provider.BuildContext) (nntp.Config, error)) {
		if err := registry.RegisterUsenet(name, func(bc provider.BuildContext) (provider.UsenetProvider, error) {
			ncfg, err := cfgFor(bc)
			if err != nil {
				return nil, err
			}
			ncfg.SkipCRC = !cfg.NNTP.VerifyCRC
			// NS-1.1: applies to every usenet provider built through this
			// factory, not just Newshosting -- each gets its own
			// per-provider missing-article cache instance.
			ncfg.NegCacheTTLMin = cfg.NNTP.NegCacheTTLMin
			// NS-5.1: same shared-config pattern as NegCacheTTLMin above --
			// every configured usenet provider's demand pool gets the same
			// warm-pool target.
			ncfg.WarmConns = cfg.NNTP.WarmConns
			ncfg.PipelineDepth = cfg.NNTP.PipelineDepth
			p := nntp.New(ncfg, log)
			p.SetProviderName(bc.Name)
			p.SetFinalRungReporter(func(ctx context.Context, event nntp.FinalRungEvent) (int64, error) {
				count, err := st.IncrementNNTPZeroFill(ctx, event.ItemID)
				if err == nil {
					// OBS-01: reuse NS-1.3's existing item-associated,
					// detached-context, best-effort reporter chokepoint for
					// the bounded per-item SF-01 error journal, rather than
					// adding a new call site. Journal-append failure never
					// masks the original zero-fill count or stream error.
					jerr := st.AppendErrorJournal(ctx, event.ItemID, store.ErrorJournalEntry{
						Class:    string(event.OutcomeClass),
						Rung:     event.Operation,
						Provider: event.Provider,
					}, cfg.ErrorJournal.MaxEntries)
					if jerr != nil {
						log.Warn("error journal: append failed (non-fatal)",
							"item_id", event.ItemID, "error", jerr)
					}
				}
				return count, err
			})
			// NS-4.2 rung 4: supply the persisted NS-4.1 IFSC proof index and
			// the per-repair budget. The lookup is read-only and returns nil
			// for any item without proof material, which disables rung 4 for
			// that stream without affecting any other.
			p.SetPAR2Repair(func(ctx context.Context, itemID string) *archiveparser.PAR2ProofIndex {
				if itemID == "" {
					return nil
				}
				item, err := st.GetItemByID(ctx, itemID)
				if err != nil || item == nil {
					return nil
				}
				return item.Metadata.NNTPPAR2ProofIndex
			}, nntp.PAR2RepairBudget{
				MaxBytes:    cfg.NNTP.RepairBudgetMB << 20,
				MaxDuration: time.Duration(cfg.NNTP.RepairBudgetSeconds) * time.Second,
			})
			providerClosers = append(providerClosers, p.Close)
			log.Info("nntp provider initialized", "provider", bc.Name, "host", ncfg.Host, "connections", ncfg.Connections)
			attachSegmentCache(cfg, bc.Name, ncfg, p, streamCache, offsetRecorder, nntpExpSvc, log, &providerClosers)
			return p, nil
		}); err != nil {
			log.Error("provider registry", "error", err)
			os.Exit(1)
		}
	}

	registerUsenetFactory("newshosting", func(bc provider.BuildContext) (nntp.Config, error) {
		if cfg.NNTP.Host == "" {
			return nntp.Config{}, fmt.Errorf("%w: HARRBOR_NNTP_HOST not set", provider.ErrNotConfigured)
		}
		return nntp.Config{
			Host:        cfg.NNTP.Host,
			Port:        cfg.NNTP.Port,
			TLS:         cfg.NNTP.TLS,
			Username:    cfg.NNTP.Username,
			Password:    cfg.NNTP.Password,
			Connections: cfg.NNTP.Connections,
		}, nil
	})

	// TorBox NNTP News Server (v9.0.0) as an additional selectable usenet
	// provider — REDESIGN §16's multi-provider principle in practice:
	// internal/torbox is the only TorBox-specific code involved; once
	// credentials are in hand they build exactly the same nntp.Config as
	// any other named provider and flow through the same
	// registerUsenetFactory -> nntp.New -> pool/segment-cache wiring as
	// Newshosting. Never auto-enabled: opts in by being named in
	// HARRBOR_USENET_PROVIDERS. Capability-gated at startup
	// (newsServerCapable, same plan-discovery pass every other TorBox
	// capability uses).
	//
	// BUGFIX 2026-07-01 session, found via the FIRST real live test of this
	// provider: GET /usenet/provider/account only returns the real password
	// on the very first call after account provisioning — every call after
	// that (even from a fresh process, even seconds later) returns the
	// literal masked placeholder "********" instead, which a naive
	// call-every-boot design (the original version of this factory) treats
	// as a real credential and hands to the NNTP pool, which then fails
	// AUTHINFO PASS with "482 Invalid username or password" on every single
	// connection. Fixed: persist the real credential the first time one is
	// actually seen (store.provider_credentials — see migration 0010's doc
	// comment for why this can't go through the sealed-secrets store
	// instead), and reuse it on every subsequent boot without calling the
	// TorBox API again at all. Only if no persisted credential exists AND a
	// fresh GET also comes back masked (e.g. the News Server was already
	// provisioned via TorBox's own website before DH ever ran) does this
	// fall back to POST resetpw — a deliberate one-time, logged action, not
	// something retried routinely, since every resetpw call rotates the
	// password again and would otherwise silently break any other consumer
	// of the account on every DH restart.
	registerUsenetFactory("torbox", func(bc provider.BuildContext) (nntp.Config, error) {
		if cfg.TorBox.APIToken == "" {
			return nntp.Config{}, fmt.Errorf("%w: HARRBOR_TORBOX_API_TOKEN is empty", provider.ErrNotConfigured)
		}
		if !newsServerCapable {
			return nntp.Config{}, fmt.Errorf("%w: TorBox News Server requires a Pro plan (account plan not News-Server-capable per startup discovery)", provider.ErrNotConfigured)
		}

		const credentialKey = "torbox-nntp"

		if raw, found, credErr := st.GetProviderCredential(context.Background(), credentialKey); credErr != nil {
			log.Warn("torbox news server: read persisted credential failed (will re-provision)", "error", credErr)
		} else if found {
			var ncfg nntp.Config
			if jsonErr := json.Unmarshal([]byte(raw), &ncfg); jsonErr == nil && ncfg.Host != "" && ncfg.Password != "" {
				return ncfg, nil
			}
			log.Warn("torbox news server: persisted credential unusable, re-provisioning")
		}

		acctCtx, acctCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer acctCancel()
		acct, err := torboxClient.GetUsenetProviderAccount(acctCtx)
		if err != nil {
			return nntp.Config{}, fmt.Errorf("torbox news server: fetch credentials: %w", err)
		}
		if torbox.IsMaskedPassword(acct.Password) {
			log.Warn("torbox news server: no persisted credential and current password is masked; performing a one-time reset to obtain a usable value (this invalidates any previously-issued News Server password for this account)")
			resetCtx, resetCancel := context.WithTimeout(context.Background(), 10*time.Second)
			acct, err = torboxClient.ResetUsenetProviderPassword(resetCtx)
			resetCancel()
			if err != nil {
				return nntp.Config{}, fmt.Errorf("torbox news server: reset credentials: %w", err)
			}
		}

		connections := acct.Connections
		if connections <= 0 {
			connections = 8
		}
		ncfg := nntp.Config{
			Host:        acct.Host,
			Port:        acct.Port,
			TLS:         acct.TLS,
			Username:    acct.Username,
			Password:    acct.Password,
			Connections: connections,
		}

		if blob, jsonErr := json.Marshal(ncfg); jsonErr != nil {
			log.Warn("torbox news server: marshal credential for persistence failed (will re-provision next boot)", "error", jsonErr)
		} else if setErr := st.SetProviderCredential(context.Background(), credentialKey, string(blob)); setErr != nil {
			log.Warn("torbox news server: persist credential failed (will re-provision next boot)", "error", setErr)
		} else {
			log.Info("torbox news server: credential persisted for reuse on future boots")
		}

		return ncfg, nil
	})

	for _, name := range cfg.Providers.UsenetProviders {
		if name == "newshosting" || name == "torbox" {
			continue
		}
		registerUsenetFactory(name, nntpConfigFromLookup)
	}

	provSet, err := registry.Build(provider.Selection{
		Providers:         cfg.Providers.Providers,
		ProvidersExplicit: cfg.Providers.ProvidersExplicit,
		UsenetProviders:   cfg.Providers.UsenetProviders,
		UsenetExplicit:    cfg.Providers.UsenetExplicit,
		Lookup:            cfg.Lookup,
		Log:               log,
	})
	if err != nil {
		log.Error("provider registry refused start", "error", err)
		os.Exit(1)
	}
	log.Info("providers selected",
		"providers", strings.Join(provSet.ProviderOrder, ","),
		"usenet_providers", strings.Join(provSet.UsenetOrder, ","),
	)

	prov := provSet.DefaultProvider()
	if prov == nil && !cfg.HTTPStreamEnabled() && len(provSet.UsenetOrder) == 0 {
		log.Error("no usable acquisition provider built; configure debrid, HTTP, or Usenet")
		os.Exit(1)
	}

	// R2 governor redesign: wire the live provider in so Allow() gates on
	// real Allowed Active Slots headroom + cooldown_until instead of the
	// legacy rolling 20-adds/15-day counter. Providers without a slot
	// concept (provider.SlotSource) leave the Governor in legacy fallback.
	var gate *cachegate.Gate
	if prov != nil {
		gov.SetProvider(prov)
		gate = cachegate.New(prov, gov, cfg.Governor.RequireCached)
	} else {
		log.Info("debrid provider disabled; serving selected HTTP/Usenet lanes only")
	}

	// C-5: build independent per-provider lanes in ProviderOrder.
	// Each lane has its own provider, gate, and governor — no cross-lane coupling.
	// TorBox is always first (primary); additional providers follow in order.
	lanes := make([]providerLane, 0, len(provSet.ProviderOrder))
	for _, name := range provSet.ProviderOrder {
		p := provSet.Providers[name]
		if p == nil {
			continue
		}
		if name == prov.Name() {
			// Primary provider: reuse already-constructed gate + governor.
			lanes = append(lanes, providerLane{prov: p, gate: gate, gov: gov})
			continue
		}
		// Additional providers: construct independent gate + governor.
		// RD-specific: governor runs rolling-window-only (SlotModel=true but
		// no per-item slot ceiling enforced here — RD's SlotSource is queried
		// inside the gate via governor.Allow → prov.SlotStatus).
		laneGov := governor.New(st, cfg.Governor.UncachedBudget, cfg.Governor.WindowDays)
		laneGov.SetProvider(p)
		laneGate := cachegate.New(p, laneGov, cfg.Governor.RequireCached)
		lanes = append(lanes, providerLane{prov: p, gate: laneGate, gov: laneGov})
	}

	// api.NewServerWithContext consumes every configured usenet provider through the
	// neutral interface (provSet.Usenet + provSet.UsenetOrder) — each one
	// self-contained (own connection pool/segment cache), all usable, not
	// just the first. The resolve path below still needs the concrete
	// *nntp.NNTPProvider type for BuildRARManifest (deliberately NOT on the
	// interface; see provider.go §F-A), so main additionally resolves the
	// concrete type for every configured provider, keyed by name, for its
	// own resolve loop -- the same set, just typed differently for the two
	// different consumers.
	//
	// BUGFIX 2026-07-02: previously only provSet.DefaultUsenet() (the
	// first-configured provider) was ever wired into the resolve/stream
	// paths at all -- every other registered usenet provider (e.g.
	// Newshosting AND TorBox's NNTP News Server both configured via
	// HARRBOR_USENET_PROVIDERS) built its own live connection pool for
	// nothing, since nothing ever used it. Each usenet provider is a
	// self-contained instance (own pool/credentials/segment cache) exactly
	// like each debrid provider is meant to be -- there is no reason NNTP
	// resolution should be limited to a single hardcoded source when
	// multiple are configured. Fixed: the resolve path (resolveNZBItem /
	// resolveRARItem below) now tries every provider in usenetOrder in
	// turn -- automatic failover if the first one fails to fetch an
	// article -- and records which one actually worked on item.Provider
	// (the pre-existing but previously-dead "which provider resolved this
	// item" field). The stream path (api/router.go's pickUsenetProvider)
	// reads that same field back so a later play reconnects through the
	// provider that's known to work for this item, falling back to the
	// configured default if the field isn't set (older items, or a
	// provider that's since been removed from config).
	nntpProviders := make(map[string]*nntp.NNTPProvider, len(provSet.UsenetOrder))
	for _, name := range provSet.UsenetOrder {
		up := provSet.Usenet[name]
		np, ok := up.(*nntp.NNTPProvider)
		if !ok {
			log.Error("usenet provider is not an NNTP provider; unsupported in this build", "provider", name)
			os.Exit(1)
		}
		nntpProviders[name] = np
	}
	// NS-1.2: per-segment provider failover. One shared health/backoff
	// tracker across every configured usenet provider -- a failure
	// learned against a provider via one item's stream must protect the
	// very next segment of a DIFFERENT item streaming through the same
	// provider, not just the one that discovered it. A no-op when only
	// one (or zero) usenet provider is configured: SetFailoverProviders
	// leaves pool.failover nil in that case, reproducing NS-1.1's exact
	// single-provider ladder.
	retentionDays := nntpProviderRetentionDays(cfg, provSet.UsenetOrder, log)
	nntpHealth := nntp.NewProviderHealthWithRouting(cfg.NNTP.Stripe, retentionDays)
	// NS-9.2: per-provider article-propagation lag model. Wired via the
	// same before-first-Acquire SetXxx convention as SetFailoverProviders
	// below; 0/negative HARRBOR_NNTP_LAGMODEL_YOUNG_HOURS disables it,
	// reproducing exactly the pre-NS-9.2 preference/retention/stripe
	// ordering.
	nntpHealth.SetLagModel(int64(cfg.NNTP.LagModelYoungHours)*3600, cfg.NNTP.LagModelMinSamples)
	log.Info("nntp: provider scheduler initialized",
		"stripe", cfg.NNTP.Stripe,
		"retention_provider_count", len(retentionDays),
		"lag_model_young_hours", cfg.NNTP.LagModelYoungHours,
		"lag_model_min_samples", cfg.NNTP.LagModelMinSamples)
	for _, np := range nntpProviders {
		np.SetFailoverProviders(provSet.UsenetOrder, nntpProviders, nntpHealth)
	}
	if len(nntpProviders) == 0 {
		log.Info("nntp provider not configured (no usenet provider built)")
	}

	if err := writer.CleanStaging(); err != nil {
		log.Warn("startup: clean staging failed (non-fatal)", "error", err)
	}

	// Convert lanes to api.ProviderLane for the server (C-5).
	apiLanes := make([]api.ProviderLane, 0, len(lanes))
	for _, l := range lanes {
		apiLanes = append(apiLanes, api.ProviderLane{Prov: l.prov, Gate: l.gate})
	}
	srv := api.NewServerWithContext(ctx, cfg, log, st, qbitAuth, sabAuth, prov, provSet.Providers, provSet.ProviderOrder, gate, apiLanes, writer, provSet.Usenet, provSet.UsenetOrder, streamCache, torrentGov, expSvc, httpGov, httpExpSvc)
	srv.SetPlaybackCoverage(coverage)
	srv.SetNNTPExperience(nntpExpSvc)
	for _, usenetProvider := range provSet.Usenet {
		if recoverer, ok := usenetProvider.(interface{ SetCrossLaneRecovery(nntp.CrossLaneRecover) }); ok {
			recoverer.SetCrossLaneRecovery(srv.RecoverNNTPCrossLaneBlock)
		}
	}
	srv.SetDoctorDockerProxyURL(strings.TrimSpace(os.Getenv("HARRBOR_DOCKER_PROXY_URL")))

	// HTTP stream protocol handler registry (HTTP-STREAM-MASTER-PLAN.md D5).
	// Handlers are instantiated per configured named backend; the registry is
	// shared by the immediate per-grab path and the recovery resolver cycle.
	// With no backends configured the registry stays empty and HTTP grabs
	// fail fast with no_handler.
	httpRegistry := httpstream.NewRegistry()
	if len(cfg.HTTPStream.Backends) > 0 {
		// Bounded metadata-class client for backend detection/search calls.
		// Configured backend origins are user-trusted (D11 TrustBackend).
		mapperMetaClient := httpstream.NewHTTPClient(&httpstream.SecurityPolicy{}, httpstream.TrustBackend, httpstream.TransportMetadata)
		// Shared TMDB tvdb->tmdb(+imdb) mapper for OMSS and Stremio episode
		// queries (HS-3.6 infrastructure, HS-3.5/HS-3.7 consumers). Safe to
		// construct even with an empty HARRBOR_TMDB_API_KEY: MapTVDB then
		// always reports ErrMappingUnavailable, so episode queries without a
		// direct tmdbid/imdbid are simply skipped by the relevant handler
		// (movies still resolve via Radarr's direct tmdbid/imdbid, D7/D8).
		omssIDMapper := httpstream.NewIDMapper(mapperMetaClient, st, cfg.HTTPStream.TMDBAPIKey)
		for priority, b := range cfg.HTTPStream.Backends {
			// Redirect authority is per backend. Never share a client carrying
			// one backend's approved origins with another configured backend.
			httpMetaClient := httpstream.NewHTTPClient(&httpstream.SecurityPolicy{}, httpstream.TrustBackend,
				httpstream.TransportMetadata, b.RedirectOrigins...)
			backendType := b.Type
			if backendType == "" {
				cached, cacheErr := st.GetHTTPBackendDetection(ctx, b.Name)
				if cacheErr != nil {
					log.Warn("http-stream: detector cache read failed", "backend_id", b.Name, "error", cacheErr)
				}
				detected, detectErr := httpstream.DetectBackend(ctx, httpMetaClient, b.URL)
				if detectErr == nil {
					backendType = detected
					if err := st.PutHTTPBackendDetection(ctx, store.HTTPBackendDetection{
						BackendID: b.Name, BaseURL: b.URL,
						DetectorVersion: httpstream.DetectorVersion, BackendType: detected,
					}); err != nil {
						log.Warn("http-stream: detector cache write failed", "backend_id", b.Name, "error", err)
					}
				} else if errors.Is(detectErr, httpstream.ErrDetectionSchemaMismatch) {
					_ = st.DeleteHTTPBackendDetection(ctx, b.Name)
					log.Warn("http-stream: backend detection rejected schema", "backend_id", b.Name, "error", detectErr)
					continue
				} else if cached != nil && cached.BaseURL == b.URL && cached.DetectorVersion == httpstream.DetectorVersion {
					backendType = cached.BackendType
					log.Warn("http-stream: backend detection transient failure; retaining last good type", "backend_id", b.Name, "type", backendType)
				} else {
					if cached != nil {
						_ = st.DeleteHTTPBackendDetection(ctx, b.Name)
					}
					log.Warn("http-stream: backend detection failed", "backend_id", b.Name, "error", detectErr)
					continue
				}
			}

			switch backendType {
			case "ia":
				httpRegistry.Register(b.Name, priority, ia.New(b.Name, b.URL, httpMetaClient,
					ia.WithFilePreference(ia.FilePreference(cfg.HTTPStream.IAFilePreference))))
				log.Info("http-stream: backend registered", "backend_id", b.Name, "type", backendType)
			case "omss":
				// Aggregate discovery has a larger bounded request budget but the
				// same backend-specific exact-origin redirect authority.
				omssMetaClient := httpstream.NewHTTPClient(&httpstream.SecurityPolicy{}, httpstream.TrustBackend,
					httpstream.TransportAggregateMetadata, b.RedirectOrigins...)
				httpRegistry.Register(b.Name, priority, omss.New(b.Name, b.URL, omssMetaClient, omssIDMapper))
				log.Info("http-stream: backend registered", "backend_id", b.Name, "type", backendType)
			case "stremio":
				httpRegistry.Register(b.Name, priority, stremio.New(b.Name, b.URL, httpMetaClient, omssIDMapper))
				log.Info("http-stream: backend registered", "backend_id", b.Name, "type", backendType)
			case "generic":
				httpRegistry.Register(b.Name, priority, generic.New(b.Name, b.DescriptorsFile, httpMetaClient))
				log.Info("http-stream: backend registered", "backend_id", b.Name, "type", backendType)
			default:
				log.Warn("http-stream: backend skipped (unsupported type)", "backend_id", b.Name, "type", backendType)
			}
		}
		if !cfg.HTTPStreamEnabled() {
			log.Warn("http-stream: backends configured but HARRBOR_HTTP_STREAM_ENABLED is false — lane stays dark")
		} else {
			httpRegistry.Start(ctx, log)
		}
	} else if cfg.HTTPStreamEnabled() {
		log.Warn("http-stream: enabled but no backends configured (HARRBOR_HTTP_BACKENDS) — searches return empty, grabs fail fast")
	}
	srv.SetHTTPStreamRegistry(httpRegistry)

	// B-O4: wire TorBox client metrics into the server for /healthz + hourly log.
	if tbAdapter, ok := prov.(interface{ Metrics() *torbox.Metrics }); ok {
		if m := tbAdapter.Metrics(); m != nil {
			srv.SetEgressMeter(m)
		}
	}

	if err := st.ReleaseAllClaims(ctx); err != nil {
		log.Warn("failed to release claims on startup", "error", err)
	}

	// Periodic maintenance also runs once at startup.
	runMaintenanceCycle(ctx, log, st, srv, "startup")

	// Prune (removed + failed item retention) also runs once at startup —
	// previously only ran on the PruneInterval ticker (default 12h), so a
	// long-failed item (e.g. a dead NNTP article from days ago) sat fully
	// eligible for pruning but unpruned for up to 12h after every restart,
	// continuing to nag the arr's queue via SAB history the whole time
	// (found 2026-07-01: a failed item from 2026-06-25 was still being
	// reported, well past FailedRetention's 24h default, simply because
	// pruning hadn't ticked yet since the last restart).
	runPruneCycle(ctx, log, st, writer, cfg.Lifecycle.RemovedRetention)
	runFailedPruneCycle(ctx, log, st, writer, cfg.Lifecycle.FailedRetention, cfg.Arrs)

	// A-B6: one-time orphan purge for satellite tables (segment_offsets, strm_blobs,
	// item_file_probes, item_stream_failures, synthetic_release). These tables
	// had no cascade delete before this fix; any item deleted prior to this
	// deploy left orphaned rows. Safe to run on every startup — the query is
	// a single indexed DELETE and is a no-op once rows are clean.
	if n, err := st.PruneOrphanSatelliteRows(ctx); err != nil {
		log.Warn("startup: orphan satellite prune failed (non-fatal)", "error", err)
	} else if n > 0 {
		log.Info("startup: pruned orphan satellite rows", "count", n)
	}
	if n, err := st.PruneOrphanSyntheticReleases(ctx); err != nil {
		log.Warn("startup: orphan synthetic_release prune failed (non-fatal)", "error", err)
	} else if n > 0 {
		log.Info("startup: pruned orphan synthetic_release rows", "count", n)
	}

	// B-N3: one-time scrub of requestdl_url (token-bearing) from file_list rows.
	// Safe no-op once all rows are clean; runs in O(dirty rows) after that.
	if n, err := st.ScrubFileListRequestDLURLs(ctx); err != nil {
		log.Warn("startup: file_list requestdl_url scrub failed (non-fatal)", "error", err)
	} else if n > 0 {
		log.Info("startup: scrubbed requestdl_url from file_list rows", "count", n)
	}

	// Probe-cache rekey: update item_file_probes rows that use positional
	// integer file_id (pre-B-N3) to the real provider file_id from file_list.
	// Safe no-op once all rows use real IDs.
	if n, err := st.RekeyFileProbesByFileID(ctx); err != nil {
		log.Warn("startup: probe cache rekey failed (non-fatal)", "error", err)
	} else if n > 0 {
		log.Info("startup: rekeyed probe cache entries to real file_id", "count", n)
	}

	// for items that were materialized (remote_id IS NOT NULL) before this
	// restart. Items overdue for cleanup are fired immediately; others get
	// a timer for the remaining window.
	{
		cleanupDuration := time.Duration(cfg.Lifecycle.CleanupHours) * time.Hour
		materializedItems, listErr := st.ListMaterializedItems(ctx)
		if listErr != nil {
			log.Warn("startup: list materialized items failed (non-fatal)", "error", listErr)
		} else {
			now := time.Now()
			for _, mi := range materializedItems {
				remaining := mi.LastPlayedAt.Add(cleanupDuration).Sub(now)
				if remaining <= 0 {
					srv.ResetJanitorTimer(mi.ID, 0)
				} else {
					srv.ResetJanitorTimer(mi.ID, remaining)
				}
			}
			if len(materializedItems) > 0 {
				log.Info("startup: restored janitor timers", "count", len(materializedItems))
			}
		}
	}

	// TS-0.5 (T8c) consistency sweep: restores any deleted or corrupted
	// on-disk stub/.strm from the SQLite-authoritative copy before serving
	// traffic (COR-3-safe: unconsumed, non-terminal items only, the exact
	// boundary C0.1 established), and reports drift outside that boundary
	// (no persisted output on a Ready item, a consumed item whose
	// download-side rows are still present, or an orphan strm_blobs row) as
	// legible findings rather than silently ignoring or force-restoring it.
	// Supersedes the former HANDOFF #3 rescan-reconcile, which only checked
	// strm_path on disk, was not download_consumed-aware (false-positive
	// WARNs on correctly-consumed items), and never healed. Only runs under
	// authority "db".
	if cfg.Stub.Authority == "db" {
		logConsistencySweep(ctx, log, st, cfg.Data.Root, "startup")
	}

	// F-E: periodic blob reconciliation. Re-materializes DB→disk on an
	// interval so a stub/.strm deleted while the daemon runs is restored
	// (HARRBOR_RECONCILE_INTERVAL_SEC; 0 disables). Only runs under
	// authority "db".
	if cfg.Stub.Authority == "db" && cfg.Lifecycle.ReconcileIntervalSec > 0 {
		interval := time.Duration(cfg.Lifecycle.ReconcileIntervalSec) * time.Second
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					logConsistencySweep(ctx, log, st, cfg.Data.Root, "periodic")
				}
			}
		}()
	}

	// Periodic maintenance — expires stale blacklists, qBit sessions, and old
	// governor_log rows. The store methods are idempotent and non-fatal.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(maintenanceInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runMaintenanceCycle(ctx, log, st, srv, "periodic")
			}
		}
	}()

	// Submit loop — picks up StateResolving items with no RemoteID/QueuedID,
	// runs the cachegate check, and submits to TorBox. Runs every 30s;
	// individual grabs are submitted inline at accept time, so this loop
	// only handles recovery (e.g. items in StateResolving after a restart).
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runSubmitCycle(ctx, log, lanes, st)
			}
		}
	}()

	// B-N9: readyNotifier fires a debounced arr refresh when items go StateReady,
	// eliminating the 60–90 s arr queue-poll gap between DH ready and arr import.
	readyNotify := newReadyNotifier(ctx, log, cfg.Arrs)
	srv.SetArrRefreshNotifier(readyNotify)

	// HR3.4: wire the HTTP-lane bounded self-heal's optional Arr re-search
	// escalation to NS-6.2's existing, already lane-agnostic
	// triggerReSearchOnDecay -- reused verbatim (WORKFLOW.md rule 3), not
	// duplicated. httpResearchCooldown is this row's OWN instance (item-ID
	// keyed, same type/shape as nntpResearchCooldown below): a separate
	// instance per row matches NS-6.2's own precedent of one dedicated
	// cooldown tracker per trigger owner, not a shared cross-lane one.
	httpResearchCooldown := newResearchCooldown()
	httpSelfHealCooldown := time.Duration(cfg.HTTPStream.SelfHealCooldownMin) * time.Minute
	srv.SetHTTPDecayEscalator(httpDecayEscalatorFunc(func(item *store.Item) {
		triggerReSearchOnDecay(ctx, log, st, item, cfg.Arrs, httpResearchCooldown, httpSelfHealCooldown, cfg.SuppressionTTL())
	}))

	// Resolver loop — advances StateResolving items that already have a
	// RemoteID/QueuedID to StateReady or StateFailed.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(cfg.Lifecycle.ResolveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runResolveCycle(ctx, log, prov, provSet.Providers, st, writer, prober, cfg.Probe.ProbeBytes, cfg.Server.BaseURL, nntpProviders, provSet.UsenetOrder, cfg.Server.StreamSecret, cfg.Data.Root, cfg.Stub.Authority, readyNotify, cfg, srv.ResolveHTTPItem, expSvc, nntpExpSvc, torrentGov, srv.TriggerRepairAsync, srv.CheckIdentity)
			}
		}
	}()

	// Prune loop — deletes StateRemoved items older than the retention window.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(cfg.Lifecycle.PruneInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runPruneCycle(ctx, log, st, writer, cfg.Lifecycle.RemovedRetention)
			}
		}
	}()

	// Accepted-recovery sweep (COR-14). CreateItem (StateAccepted) and the
	// Accepted->Resolving queue write in enqueueSubmission are separate
	// statements; a crash between them strands the item -- no worker claims
	// Accepted, and duplicate detection returns the orphan for every re-grab
	// of the same release. Requeue anything stuck in Accepted far longer than
	// the create->queue window could ever legitimately last. Startup pass
	// first: recover items stranded by the previous run before grabs arrive.
	wg.Add(1)
	go func() {
		defer wg.Done()
		const acceptedStaleAfter = 2 * time.Minute
		sweep := func() {
			ids, err := st.RequeueStaleAccepted(ctx, time.Now().UTC().Add(-acceptedStaleAfter), 100)
			if err != nil {
				log.Error("accepted-recovery: sweep failed", "error", err)
				return
			}
			for _, id := range ids {
				log.Info("accepted-recovery: stale accepted item requeued to resolving", "item_id", id)
			}
		}
		sweep()
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweep()
			}
		}
	}()

	// Failed-item prune loop — dedicated, much faster than the general prune
	// loop above (default 2 min vs 12h): a StateFailed item has no ongoing
	// value once DH has recorded it for retry-prevention purposes, so it
	// should clear out of the arr's queue fast, not linger for hours. Notifies
	// every configured arr (HARRBOR_ARR_NAMES) right after actually pruning
	// something, so the arr's queue reflects it immediately instead of
	// waiting on its own polling schedule (found 2026-07-01).
	if cfg.Lifecycle.FailedRetention > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(cfg.Lifecycle.FailedPruneInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					runFailedPruneCycle(ctx, log, st, writer, cfg.Lifecycle.FailedRetention, cfg.Arrs)
				}
			}
		}()
	}

	// Ready auto-remove loop — transitions stale StateReady items to
	// StateRemoved after the configured window. The arr should have
	// imported the .strm by then; streaming continues after removal.
	//
	// BUGFIX 2026-07-01: found live during a real bulk grab (All in the
	// Family S02/S03/S04/S09, 24-25 episodes each, torrent lane). This
	// deployment's HARRBOR_READY_AUTO_REMOVE_SEC=60 (vs. the 1800s/30min
	// code default) is nowhere near enough time for the arr to probe+import
	// every episode in a large season pack -- each needs its own
	// ffprobe-relay round-trip. The item got yanked from
	// ListVisibleClientItems mid-import; Sonarr, seeing its download vanish
	// from the client's queue with episodes still pending, sent a DELETE
	// (removeFromClient=true) for it; markRemoved -> removeOutput ->
	// writer.Remove() then IMMEDIATELY deleted the entire, fully-fetched
	// season-pack directory from disk -- real TorBox bandwidth spent
	// materializing 25 real files, destroyed within minutes, zero episodes
	// actually landing in the library. This happened identically twice for
	// the same release (2026-06-29 and again 2026-07-01), and will keep
	// recurring on every re-search until fixed, each time re-fetching and
	// re-discarding real content. A single flat TTL can't be tuned safely
	// for both "single episode" and "100-episode season dump" grabs at
	// once, so beyond fixing the operational value (docker-compose.yml /
	// .env), items with more than one file now get additional grace scaled
	// by file count (5s/file, capped at 20 extra minutes) on top of the
	// base TTL, checked precisely per-item rather than relying solely on
	// the coarse SQL cutoff used to shortlist candidates.
	if cfg.Lifecycle.ReadyAutoRemoveSec > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ttl := time.Duration(cfg.Lifecycle.ReadyAutoRemoveSec) * time.Second
			ticker := time.NewTicker(5 * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					// SQL shortlist uses the base ttl (the shortest any
					// item could legitimately qualify at); the per-item
					// scaled check below is the real authority on whether
					// an individual item has actually earned removal yet.
					cutoff := time.Now().UTC().Add(-ttl)
					items, err := st.ListReadyTorrentsBefore(ctx, cutoff, 200)
					if err != nil {
						log.Error("ready-auto-remove: list", "error", err)
						continue
					}
					for _, item := range items {
						effectiveTTL := readyAutoRemoveEffectiveTTL(ttl, item.FileList)
						if time.Since(item.UpdatedAt) < effectiveTTL {
							continue // multi-file item hasn't earned its scaled grace yet
						}
						if err := st.UpdateItemState(ctx, item, store.StateRemoved, "auto-removed: exceeded ready TTL"); err != nil {
							log.Warn("ready-auto-remove: transition failed", "item_id", item.ID, "error", err)
						} else {
							log.Info("ready-auto-remove: item transitioned to removed",
								"item_id", item.ID, "display_name", item.DisplayName, "effective_ttl", effectiveTTL.String())
						}
					}
				}
			}
		}()
	}

	// B-O4: Hourly metrics summary — logs TorBox API call counts and egress
	// bytes so bandwidth-budget trends are visible in the container log.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		var prevCalls, prevCDN, prevPassthrough int64
		var tbMetrics *torbox.Metrics
		if tbAdapter, ok := prov.(interface{ Metrics() *torbox.Metrics }); ok {
			tbMetrics = tbAdapter.Metrics()
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if tbMetrics == nil {
					continue
				}
				snap := tbMetrics.Snapshot()
				deltaCalls := snap.CallsTotal - prevCalls
				deltaCDN := snap.BytesCDN - prevCDN
				deltaPass := snap.BytesPassthrough - prevPassthrough
				prevCalls = snap.CallsTotal
				prevCDN = snap.BytesCDN
				prevPassthrough = snap.BytesPassthrough
				log.Info("metering: hourly summary",
					"calls_this_hour", deltaCalls,
					"calls_total", snap.CallsTotal,
					"cdn_bytes_this_hour", deltaCDN,
					"passthrough_bytes_this_hour", deltaPass,
					"cdn_bytes_total", snap.BytesCDN,
					"probe_range_total", snap.ProbeRangeCount,
				)
			}
		}
	}()

	// R1 — never-played uncached cleanup loop. A StateReady torrent item
	// submitted uncached and never played would otherwise hold an Allowed
	// Active Slot for the provider's full seed window (TorBox Pro: 30 days).
	// Reuses the same removal path as the post-play janitor (srv.JanitorCleanup
	// is idempotent — a no-op if remote_id is already cleared).
	if cfg.Lifecycle.NeverPlayedUncachedHours > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ttl := time.Duration(cfg.Lifecycle.NeverPlayedUncachedHours) * time.Hour
			ticker := time.NewTicker(15 * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					cutoff := time.Now().UTC().Add(-ttl)
					items, err := st.ListNeverPlayedUncachedBefore(ctx, cutoff, 200)
					if err != nil {
						log.Error("never-played-uncached-cleanup: list", "error", err)
						continue
					}
					for _, item := range items {
						log.Info("never-played-uncached-cleanup: reclaiming slot",
							"item_id", item.ID, "display_name", item.DisplayName)
						srv.JanitorCleanup(item.ID)
					}
				}
			}
		}()
	}

	// Orphaned-slot reclaim loop — terminal (removed/failed) items that still
	// hold a remote_id are TorBox transfers nothing else will ever delete:
	// the post-play janitor covers played items, R1 covers StateReady, and a
	// grab→import→remove cycle leaves the transfer behind (Sonarr's
	// post-import client delete transitions the item, but markRemoved keeps
	// remote_id by design so removed items stay streamable). Live 2026-07-08:
	// all 10 Allowed Active Slots pinned by such orphans; every new grab
	// bounced off the governor. Same removal path as R1 (JanitorCleanup is
	// idempotent and clears remote_id even when the provider delete fails).
	// First sweep runs shortly after startup so a slot-exhausted deployment
	// recovers without waiting for the first tick.
	wg.Add(1)
	go func() {
		defer wg.Done()
		const orphanGrace = 30 * time.Minute
		sweep := func() {
			cutoff := time.Now().UTC().Add(-orphanGrace)
			items, err := st.ListOrphanedRemoteBefore(ctx, cutoff, 200)
			if err != nil {
				log.Error("orphan-slot-reclaim: list", "error", err)
				return
			}
			for _, item := range items {
				log.Info("orphan-slot-reclaim: releasing provider resource",
					"item_id", item.ID, "state", item.State, "display_name", item.DisplayName)
				srv.JanitorCleanup(item.ID)
			}
		}
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		select {
		case <-ctx.Done():
			return
		case <-time.After(90 * time.Second):
			sweep()
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweep()
			}
		}
	}()

	// NS-6.1: rate-capped STAT health-audit janitor pass. Selects the
	// single least-recently-audited (or never-audited) Ready NNTP item each
	// tick -- a fixed per-tick cadence over the whole eligible library gives
	// the frozen plan's "full-library period" property for free, with no
	// separate rotation cursor (see store.NextHealthAuditItem). This row
	// measures and persists; NS-6.2 (below, same cycle) triggers Arr
	// re-search on newly-confirmed decay. NS-4.2 repair remains stream-time
	// only and is not invoked by this audit loop.
	nntpResearchCooldown := newResearchCooldown()
	if cfg.HealthAudit.Enabled {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(time.Duration(cfg.HealthAudit.IntervalSec) * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					runHealthAuditCycle(ctx, log, st, nntpProviders, provSet.UsenetOrder, cfg, nntpResearchCooldown)
				}
			}
		}()
	}

	// NS-5.1: warm-pool keeper. One bounded, cancellable goroutine drives
	// every configured usenet provider's demand-pool WarmTick on a fixed
	// cadence -- deliberately fixed, not configurable, since only the
	// warm-pool TARGET (HARRBOR_NNTP_WARM_CONNS) is the row's own named
	// config knob per the frozen plan; the cadence just needs to stay
	// safely inside the pool's own 5-minute IdleTimeout. A provider whose
	// resolved WarmConns is 0 costs nothing here: Pool.WarmTick no-ops
	// immediately for it.
	if cfg.NNTP.WarmConns > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(warmPoolTickInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					for _, name := range provSet.UsenetOrder {
						np, ok := nntpProviders[name]
						if !ok || np == nil {
							continue
						}
						np.Cache().WarmTick(log)
					}
				}
			}
		}()
	}

	// TS-4.2 (frozen plan T8a): keep-warm janitor. Bounded, rate-capped
	// per-tick re-add of recently-played Ready torrent items whose derived
	// provider-expiry horizon (internal/api's assumedProviderExpiryDays) is
	// approaching, via the exact TS-4.1 same-provider re-add primitive
	// (Server.repairViaLane) -- no second submission path. Fixed hourly
	// cadence (keepWarmTickInterval); only the "recent play" window
	// (HARRBOR_TORRENT_KEEPWARM_DAYS) is this row's own named config knob.
	if cfg.Governor.TorrentKeepWarmDays > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(keepWarmTickInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					srv.RunKeepWarmCycle(ctx, cfg.Governor.TorrentKeepWarmDays)
				}
			}
		}()
	}

	// TS-4.3 (frozen plan T8b): decay-audit janitor. Bounded, rate-capped
	// per-tick remote-presence sampling of Ready torrent items via each
	// item's bound provider's own cache oracle (Provider.CheckCached, no
	// CDN bytes fetched); a conclusive "no longer cached" verdict hands the
	// item to TS-4.1's existing repair chain (Server.TriggerRepairAsync) --
	// no second repair/blacklist path. Fixed cadence (decayAuditTickInterval);
	// HARRBOR_TORRENT_DECAYAUDIT_ENABLED is this row's own only named config
	// knob.
	if cfg.Governor.TorrentDecayAuditEnabled {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(decayAuditTickInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					srv.RunDecayAuditCycle(ctx)
				}
			}
		}()
	}

	// ID-03: retroactive slow-cadence identity-audit pass over already-Ready
	// items (any lane) -- selects the single least-recently-audited item
	// each tick, mirroring NS-6.1's own cadence-capped round-robin exactly
	// (see store.NextIdentityAuditItem), and re-runs ID-01's
	// identitycheck.Verify against already-persisted ProviderIdentity/
	// MediaFacts -- no second probe. Report-only: never suppresses,
	// blacklists, or fails an item. The on-demand HTTP trigger
	// (POST /api/v1/identity-audit/run) is unaffected by this flag and
	// shares srv.RunIdentityAuditCycle's own mutex, so the two paths can
	// never race the same selection.
	if cfg.IdentityAudit.Enabled {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(time.Duration(cfg.IdentityAudit.IntervalSec) * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if _, err := srv.RunIdentityAuditCycle(ctx); err != nil {
						log.Error("identityaudit: periodic pass failed", "error", err)
					}
				}
			}
		}()
	}

	// .strm-mode installer (REDESIGN S10, Gates 1-6, all closed 2026-07-01) --
	// startup reconcile + live Docker-events watch that keeps the ffprobe
	// relay shim installed only in configured arr containers. The source/manual
	// default is off; public first-run setup enables it for a reviewed scoped-
	// discovery + base-Arr plan. It still mirrors docker-proxy's own "strm"
	// compose profile gate (D-DOCKER). Until this
	// wiring, every gate's live verification had only ever invoked
	// dockerctl/arrdiscovery/shiminstall/eventwatcher standalone via a
	// throwaway harness against the live stack, never via the real
	// darkharrbor binary starting them itself.
	if cfg.StrmInstaller.Enabled {
		dclient := dockerctl.New(cfg.StrmInstaller.DockerProxyURL, &http.Client{Timeout: 15 * time.Second})
		arrNames := make([]string, 0, len(cfg.Arrs))
		for _, arr := range cfg.Arrs {
			arrNames = append(arrNames, arr.Name)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Info("strm-installer: starting (eventwatcher)", "docker_proxy_configured", cfg.StrmInstaller.DockerProxyURL != "")
			if err := eventwatcher.Run(ctx, dclient, arrNames); err != nil && ctx.Err() == nil {
				log.Error("strm-installer: eventwatcher exited unexpectedly", "error", err)
			}
		}()
	} else {
		log.Info("strm-installer: disabled (set HARRBOR_STRM_INSTALLER_ENABLED=true and the \"strm\" compose profile to enable)")
	}

	if cfg.Backup.Enabled {
		backupService := backup.New(db, backup.Config{
			Target:      cfg.Backup.Target,
			SecretsPath: cfg.Backup.SecretsPath,
			Interval:    time.Duration(cfg.Backup.IntervalHours) * time.Hour,
			Keep:        cfg.Backup.Keep,
		}, log)
		wg.Add(1)
		go func() {
			defer wg.Done()
			backupService.Run(ctx)
		}()
	} else {
		log.Info("scheduled backups disabled")
	}

	if cfg.Stremio.Enabled() {
		clientListener, err := net.Listen("tcp", cfg.Stremio.ClientAddress)
		if err != nil {
			log.Error("stremio client listener bind failed", "error", err)
			os.Exit(1)
		}
		stremioHTTPServer = &http.Server{
			Addr:              cfg.Stremio.ClientAddress,
			Handler:           srv.ClientRouter(),
			ReadHeaderTimeout: 30 * time.Second,
			IdleTimeout:       2 * time.Minute,
			MaxHeaderBytes:    1 << 20,
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Info("stremio restricted client listener enabled", "mode", cfg.Stremio.EdgeMode, "addr", cfg.Stremio.ClientAddress)
			if err := stremioHTTPServer.Serve(clientListener); err != nil && err != http.ErrServerClosed {
				log.Error("stremio client server error", "error", err)
			}
		}()
	} else {
		log.Info("stremio restricted client listener disabled")
	}

	startupHandler.Ready(srv.Router())
	log.Info("darkharrbor initialized", "addr", cfg.Server.Address)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("shutting down...")
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("http server shutdown error", "error", err)
	}
	if stremioHTTPServer != nil {
		if err := stremioHTTPServer.Shutdown(shutdownCtx); err != nil {
			log.Error("stremio client server shutdown error", "error", err)
		}
	}
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("background shutdown error", "error", err)
	}
	wg.Wait()
	log.Info("darkharrbor shutdown complete")
}

// logConsistencySweep runs the TS-0.5 (T8c) janitor consistency sweep and
// logs its heal counts plus any legible findings. scope distinguishes the
// startup pass from the periodic pass in log output only. "no_output" and
// "orphan_blob" findings are logged individually at WARN (genuine drift or a
// defensive DB-resolves-cleanly miss); "consumed_present" is summarized only
// (expected retained metadata after Arr consumption, not drift -- COR-3
// deliberately does not touch it, and per-item WARNs would be noise on a
// library where most Ready items are eventually consumed).
func logConsistencySweep(ctx context.Context, log *slog.Logger, st *store.Store, dataRoot, scope string) {
	res, err := st.ConsistencySweep(ctx, dataRoot)
	if err != nil {
		log.Warn(scope+": consistency sweep failed (non-fatal)", "error", err)
		return
	}
	if res.Checked > 0 || res.Restored > 0 || res.Errors > 0 {
		log.Info(scope+": consistency sweep heal complete",
			"checked", res.Checked, "restored", res.Restored, "errors", res.Errors)
	}
	counts := map[string]int{}
	for _, f := range res.Findings {
		counts[f.Kind]++
		if f.Kind == "no_output" || f.Kind == "orphan_blob" {
			log.Warn(scope+": consistency sweep finding",
				"item_id", f.ItemID, "kind", f.Kind, "detail", f.Detail)
		}
	}
	log.Info(scope+": consistency sweep findings summary",
		"no_output", counts["no_output"],
		"consumed_present", counts["consumed_present"],
		"orphan_blob", counts["orphan_blob"])
}

// healthAuditPassTimeout bounds one NS-6.1 audit pass (item selection
// through persistence) regardless of the outer ctx's own lifetime -- the
// DG-09 prep budget for this row, mirroring the bounded-preparation
// convention every other grab-time/background pass in this file follows
// (ffprobe timeouts, prewarm TimeoutSec, etc.). A single pass samples at
// most cfg.HealthAudit.SampleSize segments (default 8), each a single
// cheap STAT, so this is generous headroom, not a tight budget.
const healthAuditPassTimeout = 30 * time.Second

// pickHealthAuditProvider mirrors internal/api.Server.pickUsenetProvider's
// selection logic (item's recorded provider if still configured, else the
// first configured provider in usenetOrder) without depending on *api.Server,
// which the periodic-loop goroutines in run() do not have direct access to.
// Returns nil only when no usenet provider is configured at all.
func pickHealthAuditProvider(item *store.Item, nntpProviders map[string]*nntp.NNTPProvider, usenetOrder []string) *nntp.NNTPProvider {
	if item != nil && item.Provider != nil && *item.Provider != "" {
		if p, ok := nntpProviders[*item.Provider]; ok {
			return p
		}
	}
	for _, name := range usenetOrder {
		if p, ok := nntpProviders[name]; ok {
			return p
		}
	}
	return nil
}

// runHealthAuditCycle runs one NS-6.1 pass: select the single
// least-recently-audited Ready NNTP item, STAT-sample a bounded set of its
// segments, merge the result into its persisted dead-region map, and record
// the new completeness/decayed state. It never triggers Arr re-search
// (NS-6.2's job) and never attempts NS-4.2's stream-time repair -- pure
// measurement and persistence, matching this row's scope exactly.
//
// Only counts are logged, never message-IDs (WORKFLOW.md: message-IDs must
// never appear in logs/evidence, even though they persist in the DB-side
// dead-region map exactly like RarManifest/ZIPManifest already do).
func runHealthAuditCycle(ctx context.Context, log *slog.Logger, st *store.Store, nntpProviders map[string]*nntp.NNTPProvider, usenetOrder []string, cfg *config.Config, rc *researchCooldown) {
	item, err := st.NextHealthAuditItem(ctx)
	if err != nil {
		log.Error("healthaudit: select next item failed", "error", err)
		return
	}
	if item == nil {
		return // no eligible Ready NNTP item yet
	}
	if item.SourceURI == nil || *item.SourceURI == "" {
		log.Warn("healthaudit: item has no stored NZB content; skipping", "item_id", item.ID)
		return
	}
	parsedNZB, err := nntp.ParseNZB([]byte(*item.SourceURI))
	if err != nil {
		log.Warn("healthaudit: failed to parse stored NZB; skipping", "item_id", item.ID, "error", err)
		return
	}
	np := pickHealthAuditProvider(item, nntpProviders, usenetOrder)
	if np == nil {
		log.Warn("healthaudit: no usenet provider configured; skipping", "item_id", item.ID)
		return
	}

	rng := mrand.New(mrand.NewSource(time.Now().UnixNano()))
	segs := nntp.SelectSampleSegments(&parsedNZB, cfg.HealthAudit.SampleSize, rng)
	if len(segs) == 0 {
		log.Warn("healthaudit: item has no sampleable segments; skipping", "item_id", item.ID)
		return
	}

	auditCtx, cancel := context.WithTimeout(ctx, healthAuditPassTimeout)
	defer cancel()
	result := np.AuditHealth(auditCtx, segs)

	existing, err := st.GetNNTPHealthAudit(ctx, item.ID)
	if err != nil {
		log.Error("healthaudit: load existing state failed (continuing with empty prior state)", "item_id", item.ID, "error", err)
	}
	var existingRegions []nntp.DeadRegion
	if existing != nil && existing.DeadRegionsJSON != "" {
		if uerr := json.Unmarshal([]byte(existing.DeadRegionsJSON), &existingRegions); uerr != nil {
			log.Warn("healthaudit: failed to parse existing dead-region map; resetting", "item_id", item.ID, "error", uerr)
			existingRegions = nil
		}
	}
	merged := nntp.MergeDeadRegions(existingRegions, result.Dead, result.Recovered, cfg.HealthAudit.MaxDeadRegions)
	mergedJSON, err := json.Marshal(merged)
	if err != nil {
		log.Error("healthaudit: marshal dead-region map failed", "item_id", item.ID, "error", err)
		return
	}
	decayed := !nntp.CompletenessThresholdMet(result.Completeness, cfg.HealthAudit.CompletenessThreshold)

	// NS-6.2: capture the PRIOR persisted pass's decayed verdict (before
	// this pass's own Upsert below overwrites it) so shouldTriggerReSearch
	// below can require reconfirmation on a SEPARATE, later pass with a
	// freshly-drawn random sample, not a single pass's evidence.
	wasDecayed := existing != nil && existing.Decayed

	if err := st.UpsertNNTPHealthAudit(ctx, item.ID, result.SegmentsSampled, result.SegmentsPresent, result.Completeness, decayed, string(mergedJSON)); err != nil {
		log.Error("healthaudit: persist result failed", "item_id", item.ID, "error", err)
		return
	}

	log.Info("healthaudit: pass complete",
		"item_id", item.ID,
		"sampled", result.SegmentsSampled,
		"present", result.SegmentsPresent,
		"completeness", result.Completeness,
		"decayed", decayed,
		"reconfirmed", decayed && wasDecayed,
		"dead_regions", len(merged))

	if shouldTriggerReSearch(decayed, wasDecayed) && cfg.HealthAudit.ReSearchEnabled {
		triggerReSearchOnDecay(ctx, log, st, item, cfg.Arrs, rc, time.Duration(cfg.HealthAudit.ReSearchCooldownMin)*time.Minute, cfg.SuppressionTTL())
	}
}

// shouldTriggerReSearch is NS-6.2's destructive-action gate: it requires
// decay to be conclusively observed on two separate, consecutive audit
// passes -- this pass's own verdict (decayed) AND the prior persisted
// pass's verdict (wasDecayed), each drawing its own independent random
// 8-segment sample via nntp.SelectSampleSegments -- before blacklisting,
// failing, and removing an item's .strm.
//
// Deliberately NOT a one-shot "rising edge" latch (decayed && !wasDecayed):
// that design was tried and rejected in this same session because it would
// never re-fire for an item already recorded decayed from a prior
// pass/session, and would never retry after a transient StateFailed
// persist failure (a failed persist leaves the item Ready+decayed, so the
// NEXT pass must see wasDecayed=true again and retry, not skip it forever).
//
// Real live-gate evidence is why the SECOND observation is required at
// all: one real item was conclusively flagged decayed=true at 7/8 sampled
// segments present on one pass, then fully recovered to 8/8 on the very
// next pass over the same item -- the "missing" segment was a transient
// provider condition (e.g. a momentary propagation gap), not real content
// loss, and acting on that single sample would have destroyed a still-
// playable stable DH .strm pointer. Requiring both readings to agree
// filters exactly that false positive while still catching every
// genuinely dead item, which reproduces near-zero completeness on every
// independent resample by definition -- just one full library-rotation
// later than a single-pass trigger would have fired. rc's cooldown (in
// triggerReSearchOnDecay) remains the separate defense-in-depth loop
// guard; this function only decides whether evidence is strong enough to
// act on at all.
func shouldTriggerReSearch(decayed, wasDecayed bool) bool {
	return decayed && wasDecayed
}

func runMaintenanceCycle(ctx context.Context, log *slog.Logger, st *store.Store, srv *api.Server, scope string) {
	if n := srv.SweepCaches(); n > 0 {
		log.Debug(scope+": swept in-memory caches", "count", n)
	}
	if n, err := st.ExpireBlacklists(ctx); err != nil {
		log.Warn(scope+": expire blacklists failed (non-fatal)", "error", err)
	} else if n > 0 {
		log.Debug(scope+": expired blacklist entries", "count", n)
	}
	if n, err := st.ExpireStreamDead(ctx); err != nil {
		log.Warn(scope+": expire stream-dead failed (non-fatal)", "error", err)
	} else if n > 0 {
		log.Debug(scope+": expired stream-dead entries", "count", n)
	}
	if n, err := st.PruneExpiredQBitSessions(ctx); err != nil {
		log.Warn(scope+": prune qbit sessions failed (non-fatal)", "error", err)
	} else if n > 0 {
		log.Debug(scope+": pruned qbit sessions", "count", n)
	}
	if n, err := st.PruneGovernorLog(ctx); err != nil {
		log.Warn(scope+": prune governor log failed (non-fatal)", "error", err)
	} else if n > 0 {
		log.Debug(scope+": pruned governor log", "count", n)
	}
}

// runSubmitCycle picks up StateResolving items with no RemoteID or QueuedID
// (i.e. freshly enqueued, not yet submitted to TorBox) and runs the full
// cachegate → createtorrent/createusenetdownload pipeline.
func runSubmitCycle(
	ctx context.Context,
	log *slog.Logger,
	lanes []providerLane,
	st *store.Store,
) {
	workerID := "submit-cycle"
	items, err := st.ClaimItemsDue(ctx, workerID, []store.ItemState{store.StateResolving}, time.Now(), 20)
	if err != nil {
		log.Error("submit: claim items due", "error", err)
		return
	}
	for _, item := range items {
		func(item *store.Item) {
			defer func() {
				if err := st.ReleaseItemClaim(ctx, item.ID, workerID); err != nil {
					log.Warn("submit: release item claim failed", "item_id", item.ID, "error", err)
				}
			}()

			// Skip items already submitted — the resolver handles those.
			if item.RemoteID != nil || item.QueuedID != nil {
				return
			}
			submitItem(ctx, log, lanes, st, item)
		}(item)
	}
}

// submitItem walks the configured provider lanes in order (providerOrder),
// trying each independently with its own gate and governor. The first lane
// that accepts the item wins; the item is bound to that provider via
// item.Provider. No cross-lane knowledge: a gate rejection on lane N does not
// inform lane N+1 — each lane makes its own independent decision.
//
// C-5: independent provider lanes. Each lane owns: provider, gate (oracle +
// governor), governor. Failures on one lane never cascade to another lane's
// governor or gate state.
func submitItem(
	ctx context.Context,
	log *slog.Logger,
	lanes []providerLane,
	st *store.Store,
	item *store.Item,
) {
	// NZB items route to NNTP, not debrid lanes.
	if item.SourceType == store.SourceTypeNZB {
		log.Info("submit: NZB item routed to NNTP lane (recovery path)", "item_id", item.ID)
		return
	}

	// HTTP items never touch debrid lanes: they resolve in one pass through
	// the HTTP resolver (recovery via the resolve cycle's dispatch, D1).
	if item.SourceType == store.SourceTypeHTTP {
		log.Info("submit: HTTP item routed to HTTP resolver (recovery path)", "item_id", item.ID)
		return
	}

	if len(lanes) == 0 {
		msg := "no debrid provider lanes configured"
		item.ErrorMessage = &msg
		if err := st.UpdateItemState(ctx, item, store.StateFailed, msg); err != nil {
			log.Error("submit: no lanes — persist failed state", "item_id", item.ID, "error", err)
		}
		return
	}

	// Walk lanes independently. Each lane's gate makes its own oracle +
	// governor decision. We stop at the first lane that both accepts the
	// item AND successfully submits it to the provider.
	var lastGateErr error
	for _, lane := range lanes {
		accepted, resp := submitViaLane(ctx, log, lane, st, item)
		if !accepted {
			// Gate rejected — try next lane. lastGateErr captures the most
			// recent rejection reason for the final-failure message.
			if lastGateErr == nil {
				lastGateErr = fmt.Errorf("all lanes rejected: %s gate rejected", lane.prov.Name())
			}
			continue
		}
		if resp == nil {
			// Provider submit failed on a non-retryable error — item already
			// transitioned to StateFailed by submitViaLane. Stop walking.
			return
		}

		// Success: bind item to this lane's provider and persist.
		persistSubmitResult(ctx, log, lane.prov, lane.gov, st, item, resp)
		return
	}

	// All lanes rejected. If any rejection was retryable (governor budget
	// exhausted, slot full), schedule a retry rather than permanently failing.
	// If all rejections are hard (non-retryable gate errors), fail permanently.
	if lastGateErr != nil {
		log.Warn("submit: all provider lanes rejected",
			"item_id", item.ID,
			"display_name", item.DisplayName,
			"error", lastGateErr,
		)
		msg := lastGateErr.Error()
		item.ErrorMessage = &msg
		if err := st.UpdateItemState(ctx, item, store.StateFailed, msg); err != nil {
			log.Error("submit: persist all-lanes-rejected failed state", "item_id", item.ID, "error", err)
		}
	}
}

// submitViaLane attempts submission through one provider lane.
// Returns (true, resp) on gate pass + successful provider submit.
// Returns (false, nil) when the gate rejects (try next lane).
// Returns (true, nil) when the gate passed but the provider submit failed
// non-retryably (item already transitioned to StateFailed; stop walking).
// Retryable provider errors reschedule via item.NextRunAt (returns false, nil).
func submitViaLane(
	ctx context.Context,
	log *slog.Logger,
	lane providerLane,
	st *store.Store,
	item *store.Item,
) (accepted bool, resp *provider.CreateTaskResponse) {
	provName := lane.prov.Name()
	llog := log.With("item_id", item.ID, "display_name", item.DisplayName, "provider", provName)

	// Step 1: independent gate check (oracle + governor) for this lane.
	result, gateErr := lane.gate.Check(ctx, item)
	if gateErr != nil {
		llog.Debug("submit: lane gate rejected", "error", gateErr)
		return false, nil // not accepted by this lane; caller tries next
	}

	llog.Info("submit: cachegate passed", "cached", result.Cached)

	// Step 2: provider submit.
	provResp, submitErr := lane.prov.Submit(ctx, item, provider.SubmitOptions{
		AddOnlyIfCached: result.Cached,
	})
	if submitErr != nil {
		llog.Warn("submit: provider createtask failed", "error", submitErr)
		if isRetryableSubmitError(submitErr) {
			// Retryable: back off, do not try other lanes (this item needs
			// to cool down; its governor budget slot is still open).
			next := time.Now().Add(30 * time.Second)
			item.NextRunAt = &next
			item.RetryCount++
			if err := st.UpdateItem(ctx, item); err != nil {
				log.Error("submit: persist retry state", "item_id", item.ID, "error", err)
			}
			return false, nil
		}
		// Non-retryable (DMCA block, auth failure, etc.): fail the item.
		msg := submitErr.Error()
		item.ErrorMessage = &msg
		if err := st.UpdateItemState(ctx, item, store.StateFailed, msg); err != nil {
			log.Error("submit: persist non-retryable failed state", "item_id", item.ID, "error", err)
		}
		return true, nil // accepted by gate; submit failed non-retryably; stop walking
	}

	// Record uncached add against this lane's governor budget.
	if !result.Cached && item.SourceType == store.SourceTypeTorrent {
		if recordErr := lane.gov.Record(ctx, item.ID); recordErr != nil {
			llog.Warn("submit: governor record failed (non-fatal)", "error", recordErr)
		}
	}

	return true, provResp
}

// isRetryableSubmitError reports whether a provider submit error is transient
// and the item should be retried after a backoff. Permanent errors (DMCA
// blocks, auth failures, unsupported content) should fail the item immediately.
func isRetryableSubmitError(err error) bool {
	if torbox.IsRetryable(err) {
		return true
	}
	if realdebrid.IsInfringingFile(err) {
		return false // DMCA block: permanent, fail immediately
	}
	// Unknown error types default to retryable (conservative: don't lose items).
	return provider.IsRetryable(err)
}

// persistSubmitResult binds a successful submission to the item and persists it.
// Mirrors the original BUGFIX 2026-07-01 bounded-retry logic for the persist step.
func persistSubmitResult(
	ctx context.Context,
	log *slog.Logger,
	prov provider.Provider,
	gov *governor.Governor,
	st *store.Store,
	item *store.Item,
	resp *provider.CreateTaskResponse,
) {
	_ = gov // governor record already done in submitViaLane
	provName := prov.Name()
	item.Provider = &provName
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
	now := time.Now()
	item.NextRunAt = &now

	// BUGFIX 2026-07-01: bounded retry on persist — provider resource already
	// created; a failed persist leaves item resubmittable, silently leaking
	// the provider-side resource.
	persistErr := st.UpdateItem(ctx, item)
	for attempt := 0; persistErr != nil && attempt < 2; attempt++ {
		time.Sleep(2 * time.Second)
		persistErr = st.UpdateItem(ctx, item)
	}
	if persistErr != nil {
		msg := fmt.Sprintf("created on %s (remote_id=%q queued_id=%q) but failed to persist locally after retries: %v",
			provName, resp.RemoteID, resp.QueuedID, persistErr)
		log.Error("submit: update item after createtask — provider resource may be leaked",
			"item_id", item.ID, "provider", provName,
			"remote_id", resp.RemoteID, "queued_id", resp.QueuedID, "error", persistErr)
		item.ErrorMessage = &msg
		if ferr := st.UpdateItemState(ctx, item, store.StateFailed, msg); ferr != nil {
			log.Error("submit: could not persist failed state after leak", "item_id", item.ID, "error", ferr)
		}
		return
	}

	log.Info("submit: submitted",
		"item_id", item.ID,
		"display_name", item.DisplayName,
		"provider", provName,
		"remote_id", resp.RemoteID,
		"queued_id", resp.QueuedID,
		"cached", item.Cached,
	)
}

func runResolveCycle(
	ctx context.Context,
	log *slog.Logger,
	prov provider.Provider,
	allProviders map[string]provider.Provider,
	st *store.Store,
	writer *output.StrmWriter,
	prober *probe.Prober,
	probeBytes int64,
	baseProxyURL string,
	nntpProviders map[string]*nntp.NNTPProvider,
	usenetOrder []string,
	streamSecret string,
	dataRoot string,
	stubAuthority string,
	rn *readyNotifier,
	cfg *config.Config,
	httpResolve func(context.Context, *store.Item),
	expSvc *experience.Service,
	nntpExpSvc *experience.Service,
	torrentGov *accountgov.Governor,
	triggerRepair func(*store.Item),
	checkIdentity func(context.Context, *store.Item, int, int, *mediatruth.Facts),
) {
	workerID := "resolve-cycle"
	items, err := st.ClaimItemsDue(ctx, workerID, []store.ItemState{store.StateResolving}, time.Now(), 20)
	if err != nil {
		log.Error("resolver: claim items due", "error", err)
		return
	}

	for _, item := range items {
		func(item *store.Item) {
			defer func() {
				if err := st.ReleaseItemClaim(ctx, item.ID, workerID); err != nil {
					log.Warn("resolver: release item claim failed", "item_id", item.ID, "error", err)
				}
			}()

			itemProv := prov
			if item.Provider != nil && *item.Provider != "" {
				if p, exists := allProviders[*item.Provider]; exists {
					itemProv = p
				}
			}
			resolveItem(ctx, log, itemProv, st, writer, prober, probeBytes, baseProxyURL, nntpProviders, usenetOrder, item, streamSecret, dataRoot, stubAuthority, rn, cfg, httpResolve, expSvc, nntpExpSvc, torrentGov, triggerRepair, checkIdentity)
		}(item)
	}
}

// largestCachedFile returns the largest-by-size entry in files, or nil for
// an empty slice. TS-1.4 uses this to target prewarm at the actual video
// file on a multi-file release rather than assuming index 0 (which can be
// a small cover-art/NFO/sample file — see the prewarm call site's comment).
func largestCachedFile(files []torbox.CachedFile) *torbox.CachedFile {
	if len(files) == 0 {
		return nil
	}
	best := files[0]
	for _, f := range files[1:] {
		if f.Size > best.Size {
			best = f
		}
	}
	return &best
}

func canonicalTorrentHash(item *store.Item, status *provider.TaskStatus) string {
	if item == nil {
		return ""
	}
	if hash := strings.ToLower(strings.TrimSpace(item.Metadata.RealInfoHash)); hash != "" {
		return hash
	}
	if item.InfoHash != nil {
		if hash := strings.ToLower(strings.TrimSpace(*item.InfoHash)); hash != "" {
			return hash
		}
	}
	if status != nil {
		return strings.ToLower(strings.TrimSpace(status.Hash))
	}
	return ""
}

func torrentLogIdentity(item *store.Item, status *provider.TaskStatus) (canonical, alias string) {
	canonical = canonicalTorrentHash(item, status)
	if item != nil && item.InfoHash != nil {
		alias = strings.ToLower(strings.TrimSpace(*item.InfoHash))
		if alias == canonical {
			alias = ""
		}
	}
	return canonical, alias
}

func logTorrentOutcome(log *slog.Logger, item *store.Item, status *provider.TaskStatus) {
	if log == nil || item == nil || status == nil {
		return
	}
	canonical, alias := torrentLogIdentity(item, status)
	args := []any{
		"event", "torrent_outcome_observed",
		"item_id", item.ID,
		"source_type", store.SourceTypeTorrent,
		"outcome", status.Outcome,
		"failure_code", status.FailureCode,
		"info_hash", canonical,
	}
	if alias != "" {
		args = append(args, "synthetic_alias", alias)
	}
	log.Info("resolver: normalized torrent outcome observed", args...)
}

func handleTerminalTorrentFailure(
	ctx context.Context,
	log *slog.Logger,
	st terminalFailureStore,
	item *store.Item,
	status *provider.TaskStatus,
	rn arrRefreshSignal,
) bool {
	if item == nil || status == nil {
		return false
	}
	if status.Outcome != provider.TorrentOutcomeTerminalDeadSource && status.Outcome != provider.TorrentOutcomeDeadSentinel {
		return false
	}
	failureCode := status.FailureCode
	if failureCode == provider.TorrentFailureNone {
		failureCode = provider.TorrentFailureTerminalDeadSource
	}
	hash, alias := torrentLogIdentity(item, status)
	failureArgs := []any{
		"event", "source_failure_recorded",
		"item_id", item.ID,
		"source_type", store.SourceTypeTorrent,
		"outcome", status.Outcome,
		"failure_code", failureCode,
		"info_hash", hash,
	}
	if alias != "" {
		failureArgs = append(failureArgs, "synthetic_alias", alias)
	}
	log.Warn("resolver: terminal torrent source failure recorded", failureArgs...)
	decisionArgs := []any{
		"event", "torrent_terminal_decision",
		"item_id", item.ID,
		"source_type", store.SourceTypeTorrent,
		"outcome", status.Outcome,
		"failure_code", failureCode,
		"info_hash", hash,
		"decision", "immediate_blacklist",
	}
	if alias != "" {
		decisionArgs = append(decisionArgs, "synthetic_alias", alias)
	}
	log.Warn("resolver: terminal torrent decision", decisionArgs...)
	if hash != "" {
		result, err := st.BlacklistImmediately(ctx, hash)
		if err != nil {
			log.Warn("resolver: terminal torrent blacklist failed (non-fatal)", "event", "item_failure_persist_failed", "item_id", item.ID, "source_type", store.SourceTypeTorrent, "failure_code", failureCode, "info_hash", hash, "error", err)
		} else {
			args := []any{"event", "source_blacklisted", "item_id", item.ID, "source_type", store.SourceTypeTorrent, "failure_code", failureCode, "info_hash", hash, "fail_count", result.FailCount, "blacklisted_until", result.BlacklistedUntil}
			if alias != "" {
				args = append(args, "synthetic_alias", alias)
			}
			log.Warn("resolver: terminal torrent source blacklisted", args...)
		}
	}
	errMsg := status.Error
	if errMsg == "" {
		errMsg = string(failureCode)
	}
	item.ErrorMessage = &errMsg
	previousState := item.State
	if err := st.UpdateItemState(ctx, item, store.StateFailed, errMsg); err != nil {
		log.Error("resolver: persist terminal torrent failure", "event", "item_failure_persist_failed", "item_id", item.ID, "source_type", store.SourceTypeTorrent, "failure_code", failureCode, "error", err)
		return true
	}
	log.Warn("resolver: terminal torrent state transition", "event", "item_state_transition", "item_id", item.ID, "source_type", store.SourceTypeTorrent, "previous_state", previousState, "state", store.StateFailed, "failure_code", failureCode, "info_hash", hash)
	log.Warn("resolver: terminal torrent failure persisted", "event", "item_failure_persisted", "item_id", item.ID, "source_type", store.SourceTypeTorrent, "failure_code", failureCode, "info_hash", hash)
	if rn != nil {
		rn.Notify()
	}
	return true
}

func scheduleResolveRetry(ctx context.Context, log *slog.Logger, st *store.Store, item *store.Item, delay time.Duration, failureMessage string) {
	item.RetryCount++
	if item.RetryCount >= maxResolveAttempts {
		item.NextRunAt = nil
		if failureMessage == "" {
			failureMessage = "resolve attempt failed"
		}
		msg := fmt.Sprintf("resolve attempts exhausted after %d attempts: %s", item.RetryCount, failureMessage)
		item.ErrorMessage = &msg
		if err := st.UpdateItemState(ctx, item, store.StateFailed, msg); err != nil {
			log.Error("resolver: transition to failed after max attempts", "item_id", item.ID, "attempts", item.RetryCount, "error", err)
		}
		// Record the failure against the NZB's content hash so a permanently
		// broken release (dead/expired NNTP articles) is rejected fast on the
		// next grab instead of silently repeating this same losing cycle every
		// time an arr's automatic search re-selects it (api.NZBBlacklistKey /
		// enqueueSubmission's blacklist check). Best-effort: a failure here
		// just means the release isn't blacklisted yet, not a hard error.
		if item.SourceType == store.SourceTypeNZB && item.SourceURI != nil && *item.SourceURI != "" {
			key := api.NZBBlacklistKey(*item.SourceURI)
			if blErr := st.IncrementFailCount(ctx, key); blErr != nil {
				log.Warn("resolver: nzb blacklist record failed (non-fatal)", "item_id", item.ID, "error", blErr)
			}
		}
		return
	}

	next := time.Now().Add(delay)
	item.NextRunAt = &next
	item.UpdatedAt = time.Now().UTC()
	if err := st.UpdateItem(ctx, item); err != nil {
		log.Error("resolver: schedule retry", "item_id", item.ID, "attempts", item.RetryCount, "error", err)
	}
}

func resolveItem(
	ctx context.Context,
	log *slog.Logger,
	prov provider.Provider,
	st *store.Store,
	writer *output.StrmWriter,
	prober *probe.Prober,
	probeBytes int64,
	baseProxyURL string,
	nntpProviders map[string]*nntp.NNTPProvider,
	usenetOrder []string,
	item *store.Item,
	streamSecret string,
	dataRoot string,
	stubAuthority string,
	rn *readyNotifier,
	cfg *config.Config,
	httpResolve func(context.Context, *store.Item),
	expSvc *experience.Service,
	nntpExpSvc *experience.Service,
	torrentGov *accountgov.Governor,
	triggerRepair func(*store.Item),
	checkIdentity func(context.Context, *store.Item, int, int, *mediatruth.Facts),
) {
	// HTTP items dispatch to the shared one-pass HTTP resolver
	// (api.Server.ResolveHTTPItem) — the same pass the per-grab goroutine
	// runs, so a crash inside the accepted→resolving window recovers into an
	// identical resolve (HS-1.2). HTTP never reaches the remote-ID ladder,
	// provider polling, or the debrid CDN path below.
	if item.SourceType == store.SourceTypeHTTP {
		if httpResolve != nil {
			httpResolve(ctx, item)
			return
		}
		msg := "http resolver not wired"
		item.ErrorMessage = &msg
		if err := st.UpdateItemState(ctx, item, store.StateFailed, msg); err != nil {
			log.Error("resolver: http item with no resolver — persist failed state", "item_id", item.ID, "error", err)
		}
		return
	}

	// NNTP-lane NZBs (no remote/queued id) materialize via the usenet provider.
	// TorBox-lane NZBs carry a remote/queued id and resolve through the shared
	// poll/CDN path below, identical to torrents.
	if item.SourceType == store.SourceTypeNZB && item.RemoteID == nil && item.QueuedID == nil {
		resolveNZBItem(ctx, log, st, writer, prober, probeBytes, baseProxyURL, nntpProviders, usenetOrder, item, streamSecret, dataRoot, stubAuthority, rn, cfg.SuppressionTTL(), nntpExpSvc, checkIdentity)
		return
	}

	// No IDs yet — submit loop hasn't run yet. Skip until next tick.
	if item.QueuedID == nil && item.RemoteID == nil {
		return
	}

	// The queued/remote id ladder moved verbatim into the TorBox adapter
	// (Poll, internal/torbox/adapter.go).
	status, err := prov.Poll(ctx, item)

	if err != nil {
		log.Warn("resolver: get status failed", "item_id", item.ID, "error", err)
		scheduleResolveRetry(ctx, log, st, item, 30*time.Second, err.Error())
		return
	}
	if item.SourceType == store.SourceTypeTorrent {
		logTorrentOutcome(log, item, status)
	}

	switch {
	case status.DownloadReady:
		remoteID := ""
		if item.RemoteID != nil {
			remoteID = *item.RemoteID
		}

		// Update display name from TorBox status when we only have the infohash.
		// TorBox's createtorrent response doesn't include the name; mylist does.
		if status.Name != "" && item.InfoHash != nil && item.DisplayName == *item.InfoHash {
			item.DisplayName = status.Name
		}

		// the real (multi-season) torrent; materialise only the grabbed season.
		wantSeason := 0
		if item.InfoHash != nil {
			if sr, srErr := st.GetSyntheticRelease(ctx, *item.InfoHash); srErr == nil && sr != nil {
				wantSeason = sr.Season
			}
		}
		cachedFiles := make([]torbox.CachedFile, 0, len(status.Files))
		for _, f := range status.Files {
			if wantSeason > 0 && output.SeasonFromName(f.Name) != wantSeason {
				continue
			}
			dlURL := ""
			if remoteID != "" && f.FileID != "" {
				var urlErr error
				dlURL, urlErr = prov.RequestDownloadURL(ctx, item, f.FileID)
				if urlErr != nil {
					log.Warn("resolver: requestdl failed for file",
						"item_id", item.ID,
						"file_id", f.FileID,
						"error", urlErr,
					)
				}
			}
			cachedFiles = append(cachedFiles, torbox.CachedFile{
				FileID:       f.FileID,
				Name:         f.Name,
				RelativePath: f.RelativePath,
				Size:         f.Size,
				RequestDLURL: dlURL,
			})
		}

		// TS-0.3 (T11): look up this item's persisted TorrentMeta (TS-0.1) --
		// same RealInfoHash-then-InfoHash resolution backfillTorrentMeta uses
		// below -- so Write can prefer its verified exact file names/sizes
		// over provider-reported names. A miss (no arr-uploaded .torrent for
		// this infohash, e.g. the common magnet-only case) is always
		// non-fatal: tm stays nil and Write falls back to today's behavior.
		var tm *torrentmeta.TorrentMeta
		tmHash := item.Metadata.RealInfoHash
		if tmHash == "" && item.InfoHash != nil {
			tmHash = *item.InfoHash
		}
		if tmHash != "" {
			if found, foundOk, tmErr := st.GetTorrentMeta(ctx, tmHash); tmErr != nil {
				log.Warn("resolver: torrentmeta lookup failed (continuing without it)",
					"item_id", item.ID, "info_hash", tmHash, "error", tmErr)
			} else if foundOk {
				tm = found
			}
		}

		strmPath, writeErr := writer.Write(ctx, item, cachedFiles, baseProxyURL, tm)
		if writeErr != nil {
			// ZIP/archive container: the provider delivered a ZIP (or other
			// archive) instead of raw video files. Build a CDN-ZIP manifest
			// so we can stream individual entries from their byte offsets.
			// This handles TorBox torrents where the cached content is a
			// single ZIP (e.g. RARBG full-series packs).
			if archErr, ok := writeErr.(output.ErrArchiveContainer); ok {
				log.Info("resolver: archive container detected",
					"item_id", item.ID,
					"archive_kind", archErr.Kind,
				)
				switch archErr.Kind {
				case "rar":
					// TS-1.3: stream only the selected volume set. The full
					// provider list carries companions (.nfo/.sfv/sample/
					// subtitle archive) that rarVolumeOrder cannot rank and
					// that must never become manifest parts.
					rarVolumes := archErr.ArchiveVolumes
					if len(rarVolumes) == 0 {
						rarVolumes = cachedFiles
					}
					resolveCDNRARItem(ctx, log, st, writer, baseProxyURL, item,
						rarVolumes, cachedFiles, tm, cfg, prov, torrentGov, streamSecret, rn)
				case "zip":
					resolveCDNZIPItem(ctx, log, st, writer, baseProxyURL, item,
						archErr.ArchiveFile, cachedFiles, tm, cfg, prov, torrentGov)
				case "7z":
					msg := "cdn archive: 7z streaming awaits the shared 7z reader"
					if err := st.UpdateItemState(ctx, item, store.StateFailed, msg); err != nil {
						log.Error("resolver: persist unsupported 7z state", "item_id", item.ID, "error", err)
					}
				default:
					log.Error("resolver: unsupported archive kind", "item_id", item.ID, "archive_kind", archErr.Kind)
					scheduleResolveRetry(ctx, log, st, item, 60*time.Second, "unsupported archive kind")
				}
				return
			}
			// BUGFIX 2026-07-01: this branch used to log and return without
			// scheduling a retry, unlike the sibling "get status failed"
			// branch above. Since ClaimItemsDue reclaims any item whose
			// next_run_at is unchanged (i.e. still due), a persistently
			// failing write here retried on every resolve-cycle tick
			// forever, with no backoff and no RetryCount increment ever
			// reaching maxResolveAttempts -- found live via a real stuck
			// item retrying ~1x/3s indefinitely since 2026-06-30 (see
			// HANDOFF session log). Route through scheduleResolveRetry like
			// every other resolve failure so it backs off and eventually
			// fails out instead of hot-looping.
			log.Error("resolver: write strm failed", "item_id", item.ID, "error", writeErr)
			scheduleResolveRetry(ctx, log, st, item, 30*time.Second, writeErr.Error())
			return
		}

		// TS-6.1: acquire direct torrent subtitle files through the same
		// governed CDN ByteSource used by torrent media, then hand only bounded
		// bytes and stable video file IDs to HR5.5 before Ready.
		subtitleTargets := torrentSubtitleTargets(cachedFiles, tm)
		subtitleWindowBytes := int64(cfg.Cache.StreamChunkSizeMB) << 20
		subtitleTimeout := time.Duration(cfg.Prewarm.TimeoutSec) * time.Second
		if subtitleTimeout <= 0 {
			subtitleTimeout = 45 * time.Second
		}
		subtitleCtx, subtitleCancel := context.WithTimeout(ctx, subtitleTimeout)
		payloads, subtitleErr := readTorrentFileSubtitles(subtitleCtx, item, cachedFiles, tm,
			subtitleTargets, prov, torrentGov, subtitleWindowBytes)
		subtitleCancel()
		if subtitleErr != nil && ctx.Err() != nil {
			return
		}
		if err := registerTorrentSubtitles(ctx, log, writer, item, payloads); err != nil {
			scheduleResolveRetry(ctx, log, st, item, 60*time.Second, "persist torrent subtitles failed")
			return
		}

		item.StrmPath = &strmPath
		item.Cached = true

		// Sum real file sizes and store file list for accurate qBit API reporting.
		// B-N3: file_list stores provider IDs only (file_id, name, size).
		// requestdl_url is NOT persisted — it carries the API token and is
		// constructed transiently at stream time via RequestDownloadURL.
		var totalSize int64
		type fileEntry struct {
			FileID string `json:"file_id"`
			Name   string `json:"name"`
			Size   int64  `json:"size"`
		}
		entries := make([]fileEntry, 0, len(cachedFiles))
		for _, cf := range cachedFiles {
			totalSize += cf.Size
			entries = append(entries, fileEntry{
				FileID: cf.FileID,
				Name:   cf.Name,
				Size:   cf.Size,
			})
		}
		item.TotalSize = totalSize
		if fileListJSON, jsonErr := json.Marshal(entries); jsonErr == nil {
			s := string(fileListJSON)
			item.FileList = &s
		}

		if updateErr := st.UpdateItemState(ctx, item, store.StateReady, "resolved"); updateErr != nil {
			// BUGFIX 2026-07-01 (torbox API-hit audit): same gap as the
			// write-strm-failed branch above -- Write() had just succeeded
			// (a real TorBox-backed materialization), but if persisting the
			// StateReady transition itself fails, the item stays
			// StateResolving with next_run_at unchanged, so it re-polls
			// TorBox (a real mylist call) every 3s forever with zero
			// backoff, indefinitely re-running Write() too (harmless after
			// the idempotency fix, but still wasted work).
			log.Error("resolver: transition to ready", "item_id", item.ID, "error", updateErr)
			scheduleResolveRetry(ctx, log, st, item, 30*time.Second, updateErr.Error())
			return
		}

		// TS-3.1 (T4): grab-time cached preflight. A "claimed-cached" grab
		// (the provider's own list/status API said the content is cached)
		// is not yet proof DH can actually fetch bytes from it -- the same
		// D9 bytes=0-0 capability probe resolveCDNRARItem already uses
		// under PriorityGrab (internal/api/bytesource_cdn.go) proves that
		// before this grab is ever reported complete to the arr. Targets
		// the largest cached file, the same TS-1.4 selection convention
		// (a real release once put a 52KB cover-art file at index 0; the
		// largest file is the one DH will actually serve). Bounded by the
		// existing prewarm timeout so a slow/dead origin cannot hang the
		// resolve cycle (DG-09's standing bounded-preparation contract).
		// Absent/malformed target file abstains (preflightOK stays true)
		// rather than guessing a failure DH has no evidence for.
		preflightOK := true
		preflightFile := largestCachedFile(cachedFiles)
		if preflightFile != nil && preflightFile.RequestDLURL != "" {
			pfFileID := preflightFile.FileID
			preflightReresolve := func(rctx context.Context) (string, error) {
				return prov.RequestDownloadURL(rctx, item, pfFileID)
			}
			pfWindowBytes := int64(cfg.Cache.StreamChunkSizeMB) * 1024 * 1024
			if pfWindowBytes <= 0 {
				pfWindowBytes = 16 * 1024 * 1024
			}
			pfCtx, pfCancel := context.WithTimeout(ctx, time.Duration(cfg.Prewarm.TimeoutSec)*time.Second)
			_, pfRangeCapable, pfErr := api.NewProbedCDNByteSource(pfCtx,
				"torrent|preflight|"+api.ResolveKey(item.ID, pfFileID), preflightFile.RequestDLURL,
				pfWindowBytes, preflightReresolve, nil, torrentGov, accountgov.PriorityGrab, item.ID)
			pfCancel()
			preflightOK = pfErr == nil && pfRangeCapable
			if !preflightOK {
				if pfErr != nil {
					log.Warn("resolver: grab-time preflight capability probe failed", "item_id", item.ID, "error", pfErr)
				} else {
					log.Warn("resolver: grab-time preflight found origin not range-capable", "item_id", item.ID)
				}
			}

			// TS-5.1 (T6): record this grab-time observation as the
			// reserved "preflight" source either way -- the same
			// own-traffic availability signal internal/api's submit/
			// stream writers already record, filling the third source
			// TS-5.1 declared but deliberately left unwired for this row
			// (internal/availability.SourcePreflight).
			if item.Provider != nil {
				if hash := canonicalTorrentHash(item, nil); hash != "" {
					if provNorm, ok := availability.NormalizeProvider(*item.Provider); ok {
						if hashNorm, ok := availability.NormalizeHash(hash); ok {
							if rerr := st.RecordHashAvailability(ctx, provNorm, hashNorm, preflightOK,
								string(availability.SourcePreflight), cfg.AvailabilityTTL()); rerr != nil {
								log.Warn("resolver: preflight availability record failed (non-fatal)", "item_id", item.ID, "error", rerr)
							}
						}
					}
				}
			}
		}

		log.Info("resolver: item ready",
			"item_id", item.ID,
			"display_name", item.DisplayName,
			"strm_path", strmPath,
			"files", len(cachedFiles),
			"preflight_ok", preflightOK,
		)
		if preflightOK {
			// B-N9: debounced arr refresh — kills the 60–90 s queue-poll gap.
			rn.Notify()
		} else {
			// TS-3.1: a broken-cached grab must never be reported complete.
			// Route into TS-4.1's existing repair chain instead of
			// notifying the arr this cycle -- repair's own re-add path
			// returns the item to StateResolving (silently re-resolved,
			// re-run through this same preflight) on success, or to
			// StateFailed (with its own arr refresh) on exhaustion. Either
			// outcome is a correct, already-established terminal state;
			// skipping Notify here is what keeps a still-broken Ready row
			// from being surfaced before repair has had a chance to run.
			log.Warn("resolver: grab-time preflight failed, entering repair before reporting completion",
				"item_id", item.ID)
			if triggerRepair != nil {
				triggerRepair(item)
			}
		}

		// TS-0.2 (T1): best-effort provider metainfo backfill. Torrent-lane
		// magnet-only submissions never populate torrent_meta (only the
		// arr-uploaded .torrent-file path, TS-0.1, does); this covers both
		// a freshly-submitted magnet item and any pre-existing item that
		// resolves again with no persisted row for its infohash. Never
		// blocks or reopens StateReady -- same non-fatal shape as the
		// RangeProbe/TS-1.4 prewarm calls immediately around it.
		if item.SourceType == store.SourceTypeTorrent {
			backfillTorrentMeta(ctx, log, prov, st, item)
		}

		// v1 byte-range probe: Range GET first probeBytes bytes → ffprobe.
		// Best-effort: probe failure does not block StateReady.
		// Stub .mkv writing is deprecated — the arr-side ffprobe wrapper
		// probes the .strm via DH's stream endpoint, so a local .mkv
		// sidecar is no longer needed.
		if len(cachedFiles) > 0 && cachedFiles[0].RequestDLURL != "" {
			probeResult, probeErr := prober.RangeProbe(ctx, cachedFiles[0].RequestDLURL, probeBytes)
			if probeErr != nil {
				log.Warn("resolver: range probe failed (non-fatal)", "item_id", item.ID, "error", probeErr)
			} else {
				probeJSON := probeResult.JSON
				item.ProbeJSON = &probeJSON
				if updateErr := st.UpdateItem(ctx, item); updateErr != nil {
					log.Warn("resolver: store probe json failed (non-fatal)", "item_id", item.ID, "error", updateErr)
				}
			}
		}

		// TS-1.4: grab-time prewarm — pin the item's hot-head bytes plus
		// whatever mediatruth reads while locating the seek index, under a
		// PriorityPrewarm governor lease so live playback demand is never
		// starved (LC-06). Best-effort and bounded, exactly like the
		// RangeProbe above: failure never blocks or reopens StateReady, and
		// the capability probe treats a Range-ignoring origin distinctly
		// (DG-09) rather than assuming it.
		//
		// The largest cached file is targeted, not cachedFiles[0]: a real
		// live-gate grab (WORKLOG "TS-1.4") caught cachedFiles[0] being a
		// 52KB cover-art JPEG on a real multi-file release while the actual
		// 5.89GB video sat at a later index — prewarming index 0
		// unconditionally would silently warm the wrong file on exactly
		// this (common, real) release shape.
		prewarmFile := largestCachedFile(cachedFiles)
		if expSvc != nil && prewarmFile != nil && prewarmFile.RequestDLURL != "" {
			fileID := prewarmFile.FileID
			windowBytes := int64(cfg.Cache.StreamChunkSizeMB) * 1024 * 1024
			if windowBytes <= 0 {
				windowBytes = 16 * 1024 * 1024
			}
			reresolveFn := func(rctx context.Context) (string, error) {
				return prov.RequestDownloadURL(rctx, item, fileID)
			}
			pctx, pcancel := context.WithTimeout(ctx, time.Duration(cfg.Prewarm.TimeoutSec)*time.Second)
			src, rangeCapable, perr := api.NewCDNChunkSourceProbe(pctx, "torrent|"+api.ResolveKey(item.ID, fileID),
				prewarmFile.RequestDLURL, windowBytes, reresolveFn, nil, torrentGov, item.ID, log)
			switch {
			case perr != nil:
				log.Warn("resolver: prewarm cdn probe failed (non-fatal)", "item_id", item.ID, "error", perr)
			case !rangeCapable:
				log.Debug("resolver: prewarm skipped, origin not range-capable", "item_id", item.ID)
			default:
				if presult, perr := expSvc.PrewarmHotHead(pctx, src); perr != nil {
					log.Warn("resolver: prewarm hot-head failed (non-fatal)", "item_id", item.ID, "error", perr)
				} else {
					log.Info("resolver: prewarm hot-head complete", "item_id", item.ID, "file_id", fileID)
					// SF-04: persist any facts this same bounded analysis
					// already computed, so a later Arr ffprobe import can be
					// answered instantly instead of a live network probe.
					// Best-effort and non-fatal, matching every other
					// persistence step in this prewarm path -- an absent or
					// failed persist here only means SF-04's fast path is
					// unavailable for this item, never a playback regression.
					if presult != nil && !presult.Facts.Empty() && st != nil {
						if merr := st.SetMediaFacts(ctx, item.ID, fileID, presult.SourceKey, presult.Facts); merr != nil {
							log.Warn("resolver: media facts persist failed (non-fatal)", "item_id", item.ID, "error", merr)
						}
					}
					// TS-3.3 (T10/T4): a genuine prober verdict against the
					// real resolved bytes -- not a merely-unsupported
					// container, not an unconfigured prober, not a local
					// DH-host temp-file prep error (mediatruth.Result.
					// ProbeFailed is deliberately narrow; see its own doc
					// comment) -- is real broken-media evidence. Route it
					// into TS-4.1's existing repair chain instead of
					// silently discarding it as SF-04 alone did: the same
					// item that would otherwise import broken now gets one
					// bounded repair attempt before it ever reaches the arr.
					// triggerRepair itself is a fast cooldown/in-flight
					// no-op for anything already mid-repair (DG-07), and a
					// no-op for a non-torrent item, so this call is always
					// safe to make unconditionally.
					if presult.ProbeFailed() && triggerRepair != nil {
						log.Warn("resolver: media probe rejected resolved bytes, routing to repair",
							"item_id", item.ID, "file_id", fileID)
						triggerRepair(item)
					}
					// ID-01: grab-time identity verification, reusing the
					// same presult.Facts this prewarm pass already
					// computed -- no second probe, no second analysis
					// pass. Season/episode are best-effort from the
					// release title only; a movie or an ambiguous/
					// season-pack title correctly abstains those checks
					// (see identitycheck.ParseSeasonEpisode).
					if presult != nil && checkIdentity != nil {
						season, episode, _ := identitycheck.ParseSeasonEpisode(item.DisplayName)
						checkIdentity(ctx, item, season, episode, &presult.Facts)
					}
				}
			}
			pcancel()
		}

	case status.Outcome == provider.TorrentOutcomeTerminalDeadSource:
		handleTerminalTorrentFailure(ctx, log, st, item, status, rn)

	case status.Outcome == provider.TorrentOutcomeDeadSentinel:
		// TorBox's own checking+zero-seed+100-day-sentinel-ETA signal is
		// its stronger "no metadata/swarm found" claim, but has been
		// observed live to appear within seconds of an uncached add and
		// later resolve successfully -- so it gets its own short dedicated
		// grace period before being treated as terminal (2026-07-18 soak
		// finding), distinct from both the instant explicit-failure path
		// and the longer generic stall timeout.
		age := time.Since(item.CreatedAt)
		grace := time.Duration(cfg.Governor.DeadSentinelGraceMin) * time.Minute
		if age >= grace {
			log.Warn("resolver: dead-sentinel torrent — grace period elapsed, failing",
				"item_id", item.ID,
				"display_name", item.DisplayName,
				"age", age.Round(time.Second),
				"grace", grace,
				"state", status.State,
				"seeds", status.Seeds,
				"eta", status.ETA,
			)
			handleTerminalTorrentFailure(ctx, log, st, item, status, rn)
			return
		}
		log.Info("resolver: dead-sentinel torrent — within grace period",
			"item_id", item.ID,
			"display_name", item.DisplayName,
			"age", age.Round(time.Second),
			"grace", grace,
			"state", status.State,
			"seeds", status.Seeds,
			"eta", status.ETA,
		)
		scheduleResolvePoll(ctx, log, st, item, time.Now, 15*time.Second)

	case status.Outcome == provider.TorrentOutcomeCancelled || status.Outcome == provider.TorrentOutcomeRemoteRemoved:
		failureCode := status.FailureCode
		if failureCode == provider.TorrentFailureNone {
			if status.Outcome == provider.TorrentOutcomeCancelled {
				failureCode = provider.TorrentFailureCancelled
			} else {
				failureCode = provider.TorrentFailureRemoteRemoved
			}
		}
		if updateErr := st.UpdateItemState(ctx, item, store.StateFailed, string(failureCode)); updateErr != nil {
			log.Error("resolver: persist non-blacklisting terminal torrent failure", "item_id", item.ID, "failure_code", failureCode, "error", updateErr)
			scheduleResolveRetry(ctx, log, st, item, 30*time.Second, updateErr.Error())
		}

	case status.Outcome == provider.TorrentOutcomeTransientError || status.Outcome == provider.TorrentOutcomeCapacityLimited:
		failureCode := status.FailureCode
		if failureCode == provider.TorrentFailureNone {
			failureCode = provider.TorrentFailureProviderUnavailable
		}
		scheduleResolveRetry(ctx, log, st, item, 30*time.Second, string(failureCode))
		return

	default:
		// TS-3.2 (T4): extends stall detection to the acceptance window
		// itself, ahead of and stricter than COR-11's own stall handling
		// immediately below. An uncached torrent that has made literally
		// zero download progress within the configured budget is treated
		// as genuinely dead -- unlike COR-11, which only engages once the
		// provider explicitly reports TorrentOutcomeStalled and never
		// blacklists -- so it is blacklisted here and the arr can
		// re-search a different release. Scoped to torrent items only
		// (TorBox-lane NZBs share this same poll path but are out of this
		// row's scope; NZB already has its own budget/blacklist handling
		// via scheduleResolveRetry's maxResolveAttempts).
		if item.SourceType == store.SourceTypeTorrent && !item.Cached {
			submitBudget := time.Duration(cfg.Governor.SubmitBudgetMin) * time.Minute
			if budgetAge, budgetExpired := torrentSubmitBudgetExpired(item.CreatedAt, time.Now(), status.Progress, submitBudget); budgetExpired {
				log.Warn("resolver: uncached torrent submit-progress budget exceeded — blacklisting",
					"item_id", item.ID,
					"display_name", item.DisplayName,
					"failure_code", provider.TorrentFailureSubmitBudgetExceeded,
					"budget_age", budgetAge.Round(time.Second),
					"budget", submitBudget,
					"state", status.State,
					"seeds", status.Seeds,
					"eta", status.ETA,
				)
				budgetStatus := *status
				budgetStatus.Outcome = provider.TorrentOutcomeTerminalDeadSource
				budgetStatus.FailureCode = provider.TorrentFailureSubmitBudgetExceeded
				handleTerminalTorrentFailure(ctx, log, st, item, &budgetStatus, rn)
				cleanupSubmitBudgetRemote(ctx, log, prov.Name(), prov.Remove, item)
				triggerSubmitBudgetReSearch(ctx, log, st, item, cfg.Arrs)
				return
			}
		}

		// COR-11: ambiguous provider-neutral stalls receive a grace period.
		// They may fail the current item after the timeout, but they never
		// blacklist the source because later swarm recovery remains possible.
		stallTimeout := time.Duration(cfg.Governor.StallTimeoutMin) * time.Minute
		stallAge, stallExpired := observeTorrentStall(
			&item.Metadata,
			item.SourceType == store.SourceTypeTorrent && !item.Cached && status.Outcome == provider.TorrentOutcomeStalled,
			time.Now().UTC(),
			stallTimeout,
		)
		if status.Outcome == provider.TorrentOutcomeStalled {
			if stallExpired {
				msg := fmt.Sprintf("%s after %v (state=%s, seeds=%d, eta=%d)",
					provider.TorrentFailureTransientStall, stallAge.Round(time.Second), status.State, status.Seeds, status.ETA)
				log.Warn("resolver: stalled torrent — failing without blacklist",
					"item_id", item.ID,
					"display_name", item.DisplayName,
					"failure_code", provider.TorrentFailureTransientStall,
					"stall_age", stallAge.Round(time.Second),
					"state", status.State,
					"seeds", status.Seeds,
					"eta", status.ETA,
				)
				if err := st.UpdateItemState(ctx, item, store.StateFailed, msg); err != nil {
					log.Error("resolver: transition stalled torrent to failed", "item_id", item.ID, "error", err)
					scheduleResolveRetry(ctx, log, st, item, 30*time.Second, msg)
				}
				return
			}
			log.Info("resolver: stalled torrent — within grace period",
				"item_id", item.ID,
				"display_name", item.DisplayName,
				"failure_code", provider.TorrentFailureTransientStall,
				"stall_age", stallAge.Round(time.Second),
				"timeout", stallTimeout,
				"state", status.State,
				"seeds", status.Seeds,
			)
		}

		// F-7: Persist real download progress from the provider so the qBit
		// emulator can report it to Sonarr instead of a frozen 0%.
		if status.Progress > 0 {
			item.Metadata.DownloadProgress = status.Progress
		}
		// F-9: Persist the provider ETA alongside progress so both emulators
		// can render a live time-left. TorBox's 8640000 "never" sentinel and
		// non-positive values clear the field -- a stalled grab must not show
		// a stale countdown.
		if status.ETA > 0 && status.ETA < 8640000 {
			item.Metadata.DownloadETASeconds = status.ETA
		} else {
			item.Metadata.DownloadETASeconds = 0
		}

		// B-N5: Still downloading — set an adaptive next_run_at so the resolve
		// loop backs off instead of polling at the fixed 3s ResolveInterval.
		// Backoff is keyed on item age (time since creation) rather than a
		// separate counter so it requires no new DB field and resets naturally
		// when an item transitions state. Ladder: <30s → 3s, <5m → 10s,
		// <30m → 30s, else → 60s. Uncached items (downloading from scratch)
		// typically take minutes, so even the 10s tier cuts 1,200 polls/hr to
		// ~360; cached items rarely linger in this branch at all.
		pollDelay := downloadPollDelay(time.Since(item.CreatedAt))
		scheduleResolvePoll(ctx, log, st, item, time.Now, pollDelay)
	}
}

// scheduleResolvePoll schedules normal provider-state observation without
// consuming the error retry budget. Downloading and dead-sentinel-within-grace
// are provider states, not failed resolve attempts; counting them against
// maxResolveAttempts can terminate a valid uncached grab before its configured
// grace period elapses. The clock is injected for deterministic gates.
func scheduleResolvePoll(ctx context.Context, log *slog.Logger, st *store.Store, item *store.Item, now func() time.Time, delay time.Duration) {
	current := now().UTC()
	next := current.Add(delay)
	item.NextRunAt = &next
	item.UpdatedAt = current
	if err := st.UpdateItem(ctx, item); err != nil {
		log.Warn("resolver: set provider poll delay failed (non-fatal)", "item_id", item.ID, "error", err)
	}
}

// observeTorrentStall tracks continuous provider-reported stall duration.
// Total item age is intentionally irrelevant: a long active download must not
// fail on its first transient stall merely because it is older than the stall
// timeout. The caller supplies now so every boundary is deterministic.
func observeTorrentStall(metadata *store.SubmissionMetadata, stalled bool, now time.Time, timeout time.Duration) (time.Duration, bool) {
	if !stalled {
		metadata.TorrentStalledAt = nil
		return 0, false
	}
	now = now.UTC()
	if metadata.TorrentStalledAt == nil {
		metadata.TorrentStalledAt = &now
		return 0, false
	}
	age := now.Sub(metadata.TorrentStalledAt.UTC())
	if age < 0 {
		age = 0
	}
	return age, age >= timeout
}

// torrentSubmitBudgetExpired implements TS-3.2 (T4): extends stall detection
// to the acceptance window itself. Unlike observeTorrentStall (COR-11, which
// only engages once the provider explicitly reports TorrentOutcomeStalled
// and deliberately never blacklists, since later swarm recovery remains
// possible), this checks a stronger, provider-independent signal: an
// uncached submission that has made literally zero download progress since
// it was accepted. A source that cannot even begin transferring within a
// bounded acceptance window is treated as genuinely dead, not merely slow,
// so it is blacklisted and the arr can re-search a different release
// instead of waiting indefinitely on one that will never start.
//
// Anchored on createdAt (item.CreatedAt, the grab-acceptance time) rather
// than a separate persisted clock field -- the same convention
// TorrentOutcomeDeadSentinel's own grace-period gate already uses -- so this
// needs no new schema/migration/config-persistence. Any progress at all
// (progress > 0) permanently disqualifies the item from this check for the
// rest of its lifetime in this branch; a source that starts and later stalls
// mid-download is COR-11's job, not this one's. now is caller-supplied so
// every boundary is deterministic and testable, matching observeTorrentStall.
func torrentSubmitBudgetExpired(createdAt time.Time, now time.Time, progress float64, budget time.Duration) (time.Duration, bool) {
	if progress > 0 {
		return 0, false
	}
	age := now.UTC().Sub(createdAt.UTC())
	if age < 0 {
		age = 0
	}
	return age, age >= budget
}

// cleanupSubmitBudgetRemote removes the zero-progress task from the provider
// after TS-3.2 has durably failed and blacklisted it. The request is bounded;
// failure never rolls back the terminal decision or exposes provider details.
func cleanupSubmitBudgetRemote(ctx context.Context, log *slog.Logger, providerName string, remove func(context.Context, *store.Item) error, item *store.Item) bool {
	if remove == nil || item == nil {
		return false
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := remove(cleanupCtx, item); err != nil {
		log.Warn("resolver: submit-budget provider cleanup failed (non-fatal)",
			"event", "torrent_submit_budget_cleanup_failed",
			"provider", providerName,
		)
		return false
	}
	log.Info("resolver: submit-budget provider cleanup complete",
		"event", "torrent_submit_budget_cleanup_succeeded",
		"provider", providerName,
	)
	return true
}

// triggerSubmitBudgetReSearch reuses NS-6.2's exact media-identity, Arr-owner,
// and title-scoped search owners. RefreshMonitoredDownloads alone cannot
// replace a terminal qBit grab; a direct SeriesSearch/MoviesSearch is needed.
// Missing or ambiguous identity safely abstains rather than guessing.
func triggerSubmitBudgetReSearch(ctx context.Context, log *slog.Logger, st terminalFailureStore, item *store.Item, arrs []config.ArrTarget) bool {
	identity := resolveDecayedItemIdentity(ctx, log, st, item)
	if identity == nil {
		log.Warn("resolver: submit-budget re-search has no usable media identity; abstaining",
			"event", "torrent_submit_budget_research_no_identity")
		return false
	}
	target, ok := resolveArrSearchTarget(ctx, log, arrs, identity)
	if !ok {
		log.Warn("resolver: submit-budget re-search has no unique Arr owner; abstaining",
			"event", "torrent_submit_budget_research_no_target",
			"kind", identity.Kind,
		)
		return false
	}
	fireArrSearchCommand(ctx, log, target)
	return true
}

// backfillTorrentMeta is TS-0.2's best-effort provider metainfo fetch/
// backfill (T1). It is called from resolveItem immediately after a torrent
// item reaches StateReady. If the provider implements provider.MetainfoSource
// and no torrent_meta row exists yet for this item's real infohash, it fetches
// the provider's held .torrent bytes, parses them with the existing TS-0.1
// torrentmeta.Parse, and persists the result via the existing
// store.UpsertTorrentMeta path -- the same persistence TS-0.1 established for
// arr-uploaded .torrent files, now also reachable for magnet-only submissions
// and any pre-existing item that never got a chance to populate one.
//
// Every failure mode here (capability absent, provider export rejected,
// malformed export, infohash mismatch, persist failure) is logged at Warn and
// returns without error: this must never block, retry, or reopen StateReady.
// Non-fatal by design, exactly like T1 specifies and exactly like the
// RangeProbe/TS-1.4 prewarm calls that already follow this same StateReady
// transition.
func backfillTorrentMeta(ctx context.Context, log *slog.Logger, prov provider.Provider, st *store.Store, item *store.Item) {
	hash := item.Metadata.RealInfoHash
	if hash == "" && item.InfoHash != nil {
		hash = *item.InfoHash
	}
	if hash == "" || item.RemoteID == nil {
		return
	}

	src, ok := prov.(provider.MetainfoSource)
	if !ok {
		return
	}

	if existing, found, err := st.GetTorrentMeta(ctx, hash); err != nil {
		log.Warn("resolver: torrentmeta backfill: existing lookup failed (continuing)",
			"item_id", item.ID, "info_hash", hash, "error", err)
	} else if found && existing != nil {
		// Already backfilled (or arr-uploaded a .torrent for this exact
		// infohash originally) -- nothing to do. This also makes repeated
		// synthetic per-season items sharing one real remote id and
		// infohash a single fetch, not one per season.
		return
	}

	data, err := src.FetchTorrentFile(ctx, *item.RemoteID)
	if err != nil {
		log.Warn("resolver: torrentmeta backfill: provider metainfo fetch failed (non-fatal)",
			"item_id", item.ID, "info_hash", hash, "error", err)
		return
	}

	meta, err := torrentmeta.Parse(data)
	if err != nil {
		log.Warn("resolver: torrentmeta backfill: parse failed (non-fatal)",
			"item_id", item.ID, "info_hash", hash, "error", err)
		return
	}

	if meta.InfoHashV1 != "" && !strings.EqualFold(meta.InfoHashV1, hash) {
		// Defensive: the provider's exported .torrent doesn't match this
		// item's own tracked infohash. Should not happen when remoteID is
		// scoped correctly, but persisting under the item's identity would
		// be silently wrong data -- discard rather than risk a mismatched
		// verification/rename oracle for a future consumer (TS-0.3/TS-3.4).
		log.Warn("resolver: torrentmeta backfill: exported infohash mismatch (discarding)",
			"item_id", item.ID, "item_info_hash", hash, "exported_info_hash", meta.InfoHashV1)
		return
	}

	if err := st.UpsertTorrentMeta(ctx, meta); err != nil {
		log.Warn("resolver: torrentmeta backfill: persist failed (non-fatal)",
			"item_id", item.ID, "info_hash", hash, "error", err)
		return
	}

	log.Info("resolver: torrentmeta backfilled from provider",
		"item_id", item.ID, "info_hash", hash, "files", len(meta.Files))
}

// downloadPollDelay returns the B-N5 adaptive backoff delay for an item that
// is still in a downloading state. Keyed on item age so it requires no extra
// DB field and self-resets on state transitions.
func downloadPollDelay(age time.Duration) time.Duration {
	switch {
	case age < 30*time.Second:
		return 3 * time.Second
	case age < 5*time.Minute:
		return 10 * time.Second
	case age < 30*time.Minute:
		return 30 * time.Second
	default:
		return 60 * time.Second
	}
}

// readyAutoRemoveEffectiveTTL returns the actual TTL an item must exceed
// before ready-auto-remove will touch it: the configured base ttl, plus
// extra grace for multi-file items (5s per file beyond the first, capped at
// 20 extra minutes) so a large season pack has a realistic chance to finish
// importing before DH pulls the content out from under the arr. A
// missing/unparseable FileList is treated as a single file (no extra
// grace) -- fails safe toward the existing (shorter) behavior rather than
// toward holding an item indefinitely.
func readyAutoRemoveEffectiveTTL(base time.Duration, fileListJSON *string) time.Duration {
	const (
		perFileGrace = 5 * time.Second
		maxGrace     = 20 * time.Minute
	)
	if fileListJSON == nil || *fileListJSON == "" {
		return base
	}
	var entries []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(*fileListJSON), &entries); err != nil || len(entries) <= 1 {
		return base
	}
	grace := time.Duration(len(entries)-1) * perFileGrace
	if grace > maxGrace {
		grace = maxGrace
	}
	return base + grace
}

func resolveCDNRARItem(
	ctx context.Context,
	log *slog.Logger,
	st *store.Store,
	writer *output.StrmWriter,
	baseProxyURL string,
	item *store.Item,
	files []torbox.CachedFile,
	allFiles []torbox.CachedFile,
	meta *torrentmeta.TorrentMeta,
	cfg *config.Config,
	prov provider.Provider,
	torrentGov *accountgov.Governor,
	streamSecret string,
	rn *readyNotifier,
) {
	log = log.With("item_id", item.ID)
	volumes := append([]torbox.CachedFile(nil), files...)
	sort.SliceStable(volumes, func(i, j int) bool {
		return rarVolumeOrder(volumes[i].Name) < rarVolumeOrder(volumes[j].Name)
	})

	windowBytes := int64(cfg.Cache.StreamChunkSizeMB) * 1024 * 1024
	if windowBytes <= 0 {
		windowBytes = 16 << 20
	}
	manifest := archiveparser.StoredRARManifest{Parts: make([]archiveparser.StoredRARPart, 0, len(volumes))}
	for _, volume := range volumes {
		if volume.FileID == "" || volume.Size <= 0 {
			scheduleResolveRetry(ctx, log, st, item, 60*time.Second, "cdnrar: invalid provider volume metadata")
			return
		}
		dlURL, err := prov.RequestDownloadURL(ctx, item, volume.FileID)
		if err != nil || dlURL == "" {
			scheduleResolveRetry(ctx, log, st, item, 60*time.Second, "cdnrar: request volume: "+httpstream.Sanitize(err))
			return
		}
		fileID := volume.FileID
		reresolve := func(rctx context.Context) (string, error) {
			return prov.RequestDownloadURL(rctx, item, fileID)
		}
		src, ranged, err := api.NewProbedCDNByteSource(ctx,
			"cdnrar|"+api.ResolveKey(item.ID, fileID), dlURL, windowBytes, reresolve, nil,
			torrentGov, accountgov.PriorityGrab, item.ID)
		if err != nil {
			scheduleResolveRetry(ctx, log, st, item, 60*time.Second, "cdnrar: range probe: "+httpstream.Sanitize(err))
			return
		}
		if !ranged {
			msg := "cdnrar: provider volume does not support byte ranges"
			if err := st.UpdateItemState(ctx, item, store.StateFailed, msg); err != nil {
				log.Error("resolveCDNRAR: persist range failure", "error", err)
			}
			return
		}
		member, err := archiveparser.ParseStoredRARMember(ctx, src)
		if err != nil {
			msg := err.Error()
			if errors.Is(err, archiveparser.ErrCompressedRAR) {
				if uerr := st.UpdateItemState(ctx, item, store.StateFailed, msg); uerr != nil {
					log.Error("resolveCDNRAR: persist compressed failure", "error", uerr)
				}
				return
			}
			scheduleResolveRetry(ctx, log, st, item, 60*time.Second, "cdnrar: parse stored member: "+msg)
			return
		}
		archiveSize := src.Size()
		if member.DataOffset > archiveSize || member.DataSize > archiveSize-member.DataOffset {
			scheduleResolveRetry(ctx, log, st, item, 60*time.Second, "cdnrar: stored member exceeds volume")
			return
		}
		if member.Name != "" {
			if manifest.MemberName != "" && manifest.MemberName != member.Name {
				msg := "cdnrar: volumes contain different members"
				if err := st.UpdateItemState(ctx, item, store.StateFailed, msg); err != nil {
					log.Error("resolveCDNRAR: persist member mismatch", "error", err)
				}
				return
			}
			manifest.MemberName = member.Name
		}
		manifest.Parts = append(manifest.Parts, archiveparser.StoredRARPart{
			FileID:      volume.FileID,
			ArchiveSize: archiveSize,
			DataOffset:  member.DataOffset,
			DataSize:    member.DataSize,
		})
		manifest.TotalSize += member.DataSize
	}
	if manifest.TotalSize <= 0 {
		scheduleResolveRetry(ctx, log, st, item, 60*time.Second, "cdnrar: empty stored member")
		return
	}
	memberName := filepath.Base(manifest.MemberName)
	if !isVideoFilename(memberName) {
		memberName = output.SanitizeName(item.DisplayName) + ".mkv"
	}
	manifest.MemberName = memberName

	const fileID = "rar:0"
	strmURL := strings.TrimRight(baseProxyURL, "/") + "/stream/" + item.ID + "/" + fileID
	if streamSecret != "" {
		strmURL += "?tok=" + output.GenerateStreamToken(streamSecret, item.ID, fileID)
	}
	strmPath, err := writer.WriteRaw(ctx, item, fileID, strmURL, memberName)
	if err != nil {
		scheduleResolveRetry(ctx, log, st, item, 60*time.Second, "cdnrar: write strm: "+err.Error())
		return
	}
	subtitleTimeout := time.Duration(cfg.Prewarm.TimeoutSec) * time.Second
	if subtitleTimeout <= 0 {
		subtitleTimeout = 45 * time.Second
	}
	subtitleCtx, subtitleCancel := context.WithTimeout(ctx, subtitleTimeout)
	payloads, subtitleErr := readTorrentFileSubtitles(subtitleCtx, item, allFiles, meta,
		[]sidecar.SubtitleTarget{{Filename: memberName, FileID: fileID}}, prov, torrentGov,
		int64(cfg.Cache.StreamChunkSizeMB)<<20)
	subtitleCancel()
	if subtitleErr != nil && ctx.Err() != nil {
		return
	}
	if err := registerTorrentSubtitles(ctx, log, writer, item, payloads); err != nil {
		scheduleResolveRetry(ctx, log, st, item, 60*time.Second, "persist torrent RAR subtitles failed")
		return
	}
	fileList, err := json.Marshal([]struct {
		FileID string `json:"file_id"`
		Name   string `json:"name"`
		Size   int64  `json:"size"`
	}{{FileID: fileID, Name: memberName, Size: manifest.TotalSize}})
	if err != nil {
		scheduleResolveRetry(ctx, log, st, item, 60*time.Second, "cdnrar: marshal file list")
		return
	}
	fileListJSON := string(fileList)
	item.Metadata.TorrentRARManifest = &manifest
	item.FileList = &fileListJSON
	item.TotalSize = manifest.TotalSize
	item.StrmPath = &strmPath
	item.Cached = true
	if err := st.UpdateItem(ctx, item); err != nil {
		scheduleResolveRetry(ctx, log, st, item, 60*time.Second, "cdnrar: persist manifest")
		return
	}
	if err := st.UpdateItemState(ctx, item, store.StateReady, "resolved"); err != nil {
		scheduleResolveRetry(ctx, log, st, item, 60*time.Second, "cdnrar: persist ready")
		return
	}
	log.Info("resolveCDNRAR: item ready", "parts", len(manifest.Parts), "total_bytes", manifest.TotalSize)
	rn.Notify()
}

func rarVolumeOrder(name string) int {
	lower := strings.ToLower(filepath.Base(name))
	ext := filepath.Ext(lower)
	if len(ext) == 4 && ext[1] == 'r' {
		if n, err := strconv.Atoi(ext[2:]); err == nil {
			return 1000 + n
		}
	}
	base := strings.TrimSuffix(lower, ext)
	if i := strings.LastIndex(base, ".part"); i >= 0 {
		if n, err := strconv.Atoi(base[i+5:]); err == nil {
			return n
		}
	}
	return 0
}

func isVideoFilename(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mkv", ".mp4", ".avi", ".m4v", ".mov", ".wmv", ".flv", ".ts", ".m2ts", ".webm":
		return true
	default:
		return false
	}
}

// resolveCDNZIPItem handles TorBox-cached torrents delivered as ZIP archives.
// The ZIP is on the TorBox CDN. We fetch its central directory via HTTP range
// requests (last 64KB), build a CDN-ZIPManifest of video entry offsets, write
// one .strm per video file, and store the manifest on the item.
// At stream time, the /stream handler calls RequestDownloadURL (getting a fresh
// CDN URL), then sends HTTP range requests to serve the correct byte range for
// the requested entry's data — no decompression needed for stored entries.
func resolveCDNZIPItem(
	ctx context.Context,
	log *slog.Logger,
	st *store.Store,
	writer *output.StrmWriter,
	baseProxyURL string,
	item *store.Item,
	archiveFile provider.CachedFile,
	allFiles []provider.CachedFile,
	meta *torrentmeta.TorrentMeta,
	cfg *config.Config,
	prov provider.Provider,
	torrentGov *accountgov.Governor,
) {
	log = log.With("item_id", item.ID, "display_name", item.DisplayName)

	// Step 1: get a fresh CDN URL for the ZIP file.
	dlURL, dlErr := prov.RequestDownloadURL(ctx, item, archiveFile.FileID)
	if dlErr != nil || dlURL == "" {
		log.Warn("resolveCDNZIP: requestdl failed (retry)", "error", dlErr)
		scheduleResolveRetry(ctx, log, st, item, 60*time.Second, fmt.Sprintf("cdnzip: requestdl: %v", dlErr))
		return
	}

	// Step 2: discover the archive size and expose it through the shared
	// ByteSource adapter.
	headReq, _ := http.NewRequestWithContext(ctx, http.MethodHead, dlURL, nil)
	headClient := &http.Client{Timeout: 15 * time.Second}
	headResp, headErr := headClient.Do(headReq)
	var zipSize int64
	if headErr == nil && headResp.ContentLength > 0 {
		zipSize = headResp.ContentLength
		headResp.Body.Close()
	}
	if zipSize <= 0 {
		// Fallback: attempt a range request for the last 64KB and see if
		// server provides Content-Range with total size.
		zipSize = archiveFile.Size
	}
	if zipSize <= 0 {
		log.Warn("resolveCDNZIP: unknown ZIP size (retry)")
		scheduleResolveRetry(ctx, log, st, item, 60*time.Second, "cdnzip: unknown zip size")
		return
	}

	src, sourceErr := api.NewCDNByteSource(
		"cdnzip|"+item.ID+"|"+archiveFile.FileID,
		dlURL,
		zipSize,
		64<<10,
		"application/zip",
		nil,
		nil,
	)
	if sourceErr != nil {
		log.Warn("resolveCDNZIP: byte source failed (retry)", "error", sourceErr)
		scheduleResolveRetry(ctx, log, st, item, 60*time.Second, fmt.Sprintf("cdnzip: byte source: %v", sourceErr))
		return
	}

	// Step 3: parse through the same archive reader used by NNTP.
	parsed, totalUncompressed, parseErr := archiveparser.ParseZIPVideoEntries(ctx, src)
	if parseErr != nil {
		if errors.Is(parseErr, archiveparser.ErrNoVideoEntries) {
			msg := "cdnzip: no video files found in ZIP"
			log.Warn("resolveCDNZIP: " + msg)
			if err := st.UpdateItemState(ctx, item, store.StateFailed, msg); err != nil {
				log.Error("store: persist failed state", "error", err)
			}
			return
		}
		log.Warn("resolveCDNZIP: parse central directory failed (retry)", "error", parseErr)
		scheduleResolveRetry(ctx, log, st, item, 60*time.Second, fmt.Sprintf("cdnzip: parse: %v", parseErr))
		return
	}
	entries := make([]nntp.ZIPEntry, len(parsed))
	for i, entry := range parsed {
		entries[i] = nntp.ZIPEntry{
			EntryIdx:       i,
			Filename:       entry.Filename,
			LocalHeaderOff: entry.LocalHeaderOff,
			DataOff:        entry.DataOff,
			CompressedSize: entry.CompressedSize,
			Uncompressed:   entry.Uncompressed,
			Method:         entry.Method,
		}
	}
	manifestJSON, merr := nntp.MarshalZIPManifest(entries)
	if merr != nil {
		log.Error("resolveCDNZIP: marshal manifest", "error", merr)
		scheduleResolveRetry(ctx, log, st, item, 60*time.Second, merr.Error())
		return
	}

	// Step 4: write one strm per video entry.
	var firstStrmPath string
	for _, e := range entries {
		// Use archive file's FileID with entry index encoded so the stream
		// handler can route to the right byte range.
		// FileID format: "<archive_file_id>:zip:<entryIdx>"
		fileID := fmt.Sprintf("%s:zip:%d", archiveFile.FileID, e.EntryIdx)
		strmURL := fmt.Sprintf("%s/stream/%s/%s", strings.TrimRight(baseProxyURL, "/"), item.ID, fileID)
		strmName := episodeNameFromSubject(e.Filename)
		if strmName == "" {
			strmName = e.Filename
		}
		strmPath, wErr := writer.WriteRawAt(ctx, item, e.EntryIdx, fileID, strmURL, strmName)
		if wErr != nil {
			log.Warn("resolveCDNZIP: write strm failed", "filename", e.Filename, "error", wErr)
			continue
		}
		if firstStrmPath == "" {
			firstStrmPath = strmPath
		}
	}

	if firstStrmPath == "" {
		msg := "cdnzip: no strm files written"
		log.Error("resolveCDNZIP: " + msg)
		scheduleResolveRetry(ctx, log, st, item, 60*time.Second, msg)
		return
	}

	// TS-6.1: the already-open shared ZIP ByteSource supplies internal
	// subtitle members; direct sibling files use the same governed CDN
	// adapter as ordinary torrent playback. Both terminate at HR5.5.
	subtitleTargets := make([]sidecar.SubtitleTarget, 0, len(entries))
	for _, entry := range entries {
		subtitleTargets = append(subtitleTargets, sidecar.SubtitleTarget{
			Filename: entry.Filename,
			FileID:   fmt.Sprintf("%s:zip:%d", archiveFile.FileID, entry.EntryIdx),
		})
	}
	subtitleTimeout := time.Duration(cfg.Prewarm.TimeoutSec) * time.Second
	if subtitleTimeout <= 0 {
		subtitleTimeout = 45 * time.Second
	}
	subtitleCtx, subtitleCancel := context.WithTimeout(ctx, subtitleTimeout)
	payloads, subtitleErr := sidecar.ReadZIPSubtitles(subtitleCtx, src, subtitleTargets,
		sidecar.DefaultMaxResourceBytes, sidecar.DefaultMaxResourcesPerItem, sidecar.DefaultMaxBytesPerItem)
	remaining := sidecar.DefaultMaxResourcesPerItem - len(payloads)
	remainingBytes := sidecar.DefaultMaxBytesPerItem - subtitlePayloadBytes(payloads)
	if remaining > 0 && remainingBytes > 0 {
		direct, directErr := readTorrentDirectSubtitles(subtitleCtx, item, allFiles, meta,
			subtitleTargets, prov, torrentGov, int64(cfg.Cache.StreamChunkSizeMB)<<20,
			sidecar.DefaultMaxResourceBytes, remaining, remainingBytes)
		payloads = append(payloads, direct...)
		if subtitleErr == nil {
			subtitleErr = directErr
		}
	}
	subtitleCancel()
	if subtitleErr != nil && ctx.Err() != nil {
		return
	}
	if err := registerTorrentSubtitles(ctx, log, writer, item, payloads); err != nil {
		scheduleResolveRetry(ctx, log, st, item, 60*time.Second, "persist torrent ZIP subtitles failed")
		return
	}

	item.ZIPManifest = &manifestJSON
	item.TotalSize = totalUncompressed
	item.StrmPath = &firstStrmPath
	item.Cached = true

	persistErr := st.UpdateItem(ctx, item)
	for attempt := 0; persistErr != nil && attempt < 2; attempt++ {
		time.Sleep(2 * time.Second)
		persistErr = st.UpdateItem(ctx, item)
	}
	if persistErr != nil {
		log.Error("resolveCDNZIP: persist item", "error", persistErr)
		return
	}
	if err := st.UpdateItemState(ctx, item, store.StateReady, ""); err != nil {
		log.Error("resolveCDNZIP: persist state ready", "error", err)
		return
	}
	log.Info("resolveCDNZIP: item ready",
		"strm_path", firstStrmPath,
		"entries", len(entries),
		"total_size_mb", totalUncompressed/1024/1024,
	)
}

// resolveZIPItem handles NZB items containing a ZIP archive.
// Mirrors resolveRARItem: builds a ZIPManifest by reading the ZIP central
// directory from NNTP, writes one .strm per video entry, transitions to StateReady.
func resolveZIPItem(
	ctx context.Context,
	log *slog.Logger,
	st *store.Store,
	writer *output.StrmWriter,
	baseProxyURL string,
	nntpProviders map[string]*nntp.NNTPProvider,
	usenetOrder []string,
	item *store.Item,
	dataRoot string,
	stubAuthority string,
	rn *readyNotifier,
	nntpExpSvc *experience.Service,
	checkIdentity func(context.Context, *store.Item, int, int, *mediatruth.Facts),
) {
	log = log.With("item_id", item.ID, "display_name", item.DisplayName)

	if len(nntpProviders) == 0 {
		msg := "NNTP provider not configured"
		log.Warn("resolveZIP: " + msg)
		if err := st.UpdateItemState(ctx, item, store.StateFailed, msg); err != nil {
			log.Error("store: persist failed state", "error", err)
		}
		return
	}

	parsedNZB, err := nntp.ParseNZB([]byte(*item.SourceURI))
	if err != nil {
		log.Warn("resolveZIP: parse NZB failed (retry)", "error", err)
		scheduleResolveRetry(ctx, log, st, item, 60*time.Second, fmt.Sprintf("parse ZIP NZB failed: %v", err))
		return
	}
	nzbFiles := parsedNZB.Files
	contentKey := nntp.ContentKey([]byte(*item.SourceURI))

	log.Info("resolveZIP: building ZIP manifest", "nzb_files", len(nzbFiles))

	// Try each NNTP provider in order (failover, same as resolveRARItem).
	var manifest []nntp.ZIPEntry
	var totalSize int64
	var winnerName string
	if artifacts, cacheErr := st.GetNNTPArtifacts(ctx, contentKey); cacheErr != nil {
		log.Warn("resolveZIP: shared manifest lookup failed; rebuilding", "error", cacheErr)
	} else if artifacts.ZIPManifest != nil {
		if cached, unmarshalErr := nntp.UnmarshalZIPManifest(*artifacts.ZIPManifest); unmarshalErr == nil {
			manifest = cached
			for _, entry := range manifest {
				totalSize += entry.Uncompressed
			}
			if item.Provider != nil {
				winnerName = *item.Provider
			}
			if _, ok := nntpProviders[winnerName]; !ok && len(usenetOrder) > 0 {
				winnerName = usenetOrder[0]
			}
			log.Info("resolveZIP: reused content-keyed manifest", "entries", len(manifest))
		}
	}
	if len(manifest) == 0 {
		for _, name := range usenetOrder {
			prov, ok := nntpProviders[name]
			if !ok {
				continue
			}
			m, sz, merr := prov.BuildZIPManifestForItem(ctx, item.ID, nzbFiles)
			if merr != nil {
				if errors.Is(merr, archiveparser.ErrNestedArchive) {
					failNestedArchive(ctx, log, st, item, "resolveZIP")
					return
				}
				log.Warn("resolveZIP: manifest build failed", "provider", name, "error", merr)
				continue
			}
			manifest = m
			totalSize = sz
			winnerName = name
			break
		}
	}

	if len(manifest) == 0 {
		// All providers failed — schedule retry.
		msg := "zip: all NNTP providers failed to build ZIP manifest"
		log.Warn("resolveZIP: " + msg)
		scheduleResolveRetry(ctx, log, st, item, 2*time.Minute, msg)
		return
	}

	manifestJSON, merr := nntp.MarshalZIPManifest(manifest)
	if merr != nil {
		log.Error("resolveZIP: marshal manifest failed", "error", merr)
		scheduleResolveRetry(ctx, log, st, item, 60*time.Second, fmt.Sprintf("zip: marshal manifest: %v", merr))
		return
	}
	if err := st.PromoteNNTPArtifacts(ctx, contentKey, item.ID, nil, &manifestJSON); err != nil {
		log.Warn("resolveZIP: shared manifest persist failed (non-fatal)", "error", err)
	}

	// Write one .strm per video entry, keyed by EntryIdx.
	var firstStrmPath string
	var strmURL string
	for _, entry := range manifest {
		sp, su, serr := writeNZBStrm(item, baseProxyURL, writer.StrmRoot(), entry.EntryIdx, entry.Filename)
		if serr != nil {
			log.Warn("resolveZIP: write strm failed", "filename", entry.Filename, "error", serr)
			continue
		}
		if firstStrmPath == "" {
			firstStrmPath = sp
			strmURL = su
		}
	}
	if firstStrmPath == "" {
		msg := "zip: no strm files written"
		log.Error("resolveZIP: " + msg)
		scheduleResolveRetry(ctx, log, st, item, 60*time.Second, msg)
		return
	}
	if np := nntpProviders[winnerName]; np != nil {
		targets := make([]nntp.SubtitleTarget, 0, len(manifest))
		for _, entry := range manifest {
			targets = append(targets, nntp.SubtitleTarget{Filename: entry.Filename, FileID: fmt.Sprintf("nzb-%d", entry.EntryIdx)})
		}
		payloads, subtitleErr := np.ReadZIPSubtitles(ctx, item.ID, nzbFiles, targets,
			sidecar.DefaultMaxResourceBytes, sidecar.DefaultMaxResourcesPerItem)
		if subtitleErr != nil && ctx.Err() != nil {
			return
		}
		direct, directErr := np.ReadDirectSubtitles(ctx, item.ID, parsedNZB, targets,
			sidecar.DefaultMaxResourceBytes, sidecar.DefaultMaxResourcesPerItem-len(payloads))
		if directErr != nil && ctx.Err() != nil {
			return
		}
		payloads = append(payloads, direct...)
		if err := registerNNTPSubtitles(ctx, log, writer, item, payloads); err != nil {
			scheduleResolveRetry(ctx, log, st, item, 60*time.Second, "persist ZIP subtitles failed")
			return
		}
	}

	// Persist manifest + transition to StateReady.
	item.ZIPManifest = &manifestJSON
	item.TotalSize = totalSize
	item.StrmPath = &firstStrmPath
	_ = strmURL
	item.Provider = &winnerName

	if err := st.UpdateItem(ctx, item); err != nil {
		log.Error("resolveZIP: persist item update failed", "error", err)
		scheduleResolveRetry(ctx, log, st, item, 60*time.Second, fmt.Sprintf("zip: persist: %v", err))
		return
	}
	if err := st.UpdateItemState(ctx, item, store.StateReady, ""); err != nil {
		log.Error("resolveZIP: persist state ready failed", "error", err)
		return
	}
	log.Info("resolveZIP: item ready",
		"strm_path", firstStrmPath,
		"entries", len(manifest),
		"total_size_mb", totalSize/1024/1024,
		"provider", winnerName,
	)
	// ID-01: title-only checks only (no NNTP-lane Facts; see resolveNZBItem's
	// own identical comment).
	if checkIdentity != nil {
		season, episode, _ := identitycheck.ParseSeasonEpisode(item.DisplayName)
		checkIdentity(ctx, item, season, episode, nil)
	}
	if rn != nil {
		rn.Notify()
	}
	if np := nntpProviders[winnerName]; np != nil {
		largest := 0
		for i := 1; i < len(manifest); i++ {
			if manifest[i].Uncompressed > manifest[largest].Uncompressed {
				largest = i
			}
		}
		src := np.SeekZIPSource(ctx, item.ID, contentKey, manifestJSON, largest)
		if src == nil {
			src = np.PrewarmZIPSource(ctx, item.ID, contentKey, manifest[largest])
		}
		runNNTPPrewarm(ctx, log, st, item, nntpExpSvc, src)
	}
}

func firstNNTPProvider(providers map[string]*nntp.NNTPProvider, order []string, preferred string) *nntp.NNTPProvider {
	if provider := providers[preferred]; provider != nil {
		return provider
	}
	for _, name := range order {
		if provider := providers[name]; provider != nil {
			return provider
		}
	}
	return nil
}

func registerNNTPSubtitles(ctx context.Context, log *slog.Logger, writer *output.StrmWriter, item *store.Item, payloads []nntp.SubtitlePayload) error {
	registered := 0
	for _, payload := range payloads {
		if err := ctx.Err(); err != nil {
			return err
		}
		resource, err := writer.RegisterSubtitle(ctx, item, payload.FileID, payload.Filename, payload.Bytes)
		if errors.Is(err, sidecar.ErrRejected) {
			continue
		}
		if errors.Is(err, sidecar.ErrCapacity) {
			break
		}
		if err != nil {
			return err
		}
		if resource.ID != "" {
			registered++
		}
	}
	if registered > 0 {
		log.Info("resolver: NNTP subtitles registered", "count", registered)
	}
	return nil
}

func runNNTPPrewarm(ctx context.Context, log *slog.Logger, st *store.Store, item *store.Item, svc *experience.Service, src rangecache.ChunkSource) {
	if svc == nil || src == nil {
		return
	}
	result, err := svc.PrewarmHotHead(ctx, src)
	if err != nil {
		log.Warn("resolver: NNTP media prewarm stopped (non-fatal)")
		return
	}
	if result == nil || result.Index.Empty() || st == nil || item == nil {
		return
	}
	if err := st.SetNNTPSeekIndex(ctx, item.ID, result.SourceKey, result.Index); err != nil {
		log.Warn("resolver: NNTP seek index persist failed (non-fatal)", "item_id", item.ID)
		return
	}
	item.Metadata.NNTPSeekIndex = &result.Index
	item.Metadata.NNTPSeekSourceKey = result.SourceKey
	log.Info("resolver: NNTP seek index ready",
		"item_id", item.ID,
		"source", result.Index.Source,
		"entries", len(result.Index.Entries),
		"estimated", result.Index.Estimated,
	)
}

func runPruneCycle(ctx context.Context, log *slog.Logger, st *store.Store, writer output.Writer, removedRetention time.Duration) {
	pruneRemovedItems(ctx, log, st, writer, removedRetention)
}

// pruneRemovedItems deletes StateRemoved rows past the retention window --
// but ONLY rows that never materialized output (COR-13). Materialized rows
// are the permanent playback identity behind library .strm files and are
// never pruned; see store.ListPrunableRemovedOlderThan.
func pruneRemovedItems(ctx context.Context, log *slog.Logger, st *store.Store, writer output.Writer, retention time.Duration) {
	cutoff := time.Now().UTC().Add(-retention)
	items, err := st.ListPrunableRemovedOlderThan(ctx, cutoff, 500)
	if err != nil {
		log.Error("prune: list prunable removed items", "error", err)
		return
	}
	if len(items) == 0 {
		return
	}

	cleanedIDs := make([]string, 0, len(items))
	for _, item := range items {
		if writer != nil {
			if err := writer.Remove(ctx, item); err != nil {
				log.Warn("prune: remove local output failed", "item_id", item.ID, "error", err)
				continue
			}
		}
		cleanedIDs = append(cleanedIDs, item.ID)
	}
	n, err := st.DeleteRemovedItemsByIDs(ctx, cleanedIDs)
	if err != nil {
		log.Error("prune: delete cleaned removed items", "error", err)
		return
	}
	if n > 0 {
		log.Info("prune: deleted old removed items", "count", n)
	}
}

// pruneFailedItems ages out StateFailed items after failedRetention (0
// disables — items are retained forever, the pre-2026-07-01 behavior).
// Unlike removed items, failed items were never materialized (no strm/output
// on disk in the normal case), but a resolve can fail after partial output
// was written (e.g. a multi-file NZB where only some files wrote before a
// later one failed) — route through the same writer.Remove cleanup for
// safety rather than assuming there's nothing to clean up.
func pruneFailedItems(ctx context.Context, log *slog.Logger, st *store.Store, writer output.Writer, retention time.Duration) int64 {
	if retention <= 0 {
		return 0
	}
	cutoff := time.Now().UTC().Add(-retention)
	items, err := st.ListFailedOlderThan(ctx, cutoff, 500)
	if err != nil {
		log.Error("prune: list failed items", "error", err)
		return 0
	}
	if len(items) == 0 {
		return 0
	}

	cleanedIDs := make([]string, 0, len(items))
	for _, item := range items {
		if writer != nil {
			if err := writer.Remove(ctx, item); err != nil {
				log.Warn("prune: remove local output for failed item failed (non-fatal)", "item_id", item.ID, "error", err)
			}
		}
		cleanedIDs = append(cleanedIDs, item.ID)
		log.Info("prune: failed item prepared for deletion",
			"event", "failed_item_pruned",
			"item_id", item.ID,
			"public_id", item.PublicID,
			"source_type", item.SourceType,
			"display_name", item.DisplayName,
			"category", item.Category,
			"state", item.State,
		)
	}
	n, err := st.DeleteFailedItemsByIDs(ctx, cleanedIDs)
	if err != nil {
		log.Error("prune: delete cleaned failed items", "error", err)
		return 0
	}
	if n > 0 {
		log.Info("prune: deleted old failed items",
			"event", "failed_items_pruned",
			"count", n,
		)
	}
	return n
}

// runFailedPruneCycle is the dedicated, fast-ticking counterpart to
// runPruneCycle's removed-item cleanup: prunes StateFailed items past
// cfg.Lifecycle.FailedRetention, and — only when it actually deleted
// something — actively tells every configured arr (HARRBOR_ARR_NAMES) to
// refresh its download-client queue view immediately, rather than leaving
// it to notice on its own polling schedule. Both the arr instances and this
// loop's fast interval exist because of the same finding (2026-07-01): a
// dead release has no ongoing value once DH has recorded the failure for
// its own retry-prevention (blacklist) purposes, and every extra minute an
// arr keeps showing it in the queue is a minute the person might mistake
// for "still broken" instead of "already handled, will try something else."
func runFailedPruneCycle(ctx context.Context, log *slog.Logger, st *store.Store, writer output.Writer, retention time.Duration, arrs []config.ArrTarget) {
	n := pruneFailedItems(ctx, log, st, writer, retention)
	if n > 0 {
		notifyArrsRefresh(ctx, log, arrs)
	}
}

// readyNotifier coalesces StateReady signals and fires notifyArrsRefresh after
// a short debounce window. A burst of season-pack episodes landing ready in
// quick succession produces exactly one arr refresh call per arr instead of N.
// The notifier runs a background goroutine for the lifetime of ctx.
type readyNotifier struct {
	ch   chan struct{}
	arrs []config.ArrTarget
	log  *slog.Logger
}

func newReadyNotifier(ctx context.Context, log *slog.Logger, arrs []config.ArrTarget) *readyNotifier {
	rn := &readyNotifier{
		ch:   make(chan struct{}, 1),
		arrs: arrs,
		log:  log,
	}
	go rn.run(ctx)
	return rn
}

// Notify signals that an item became ready. Non-blocking: if a signal is already
// pending the debounce window is already running and this call is a no-op.
func (rn *readyNotifier) Notify() {
	if len(rn.arrs) == 0 {
		return
	}
	select {
	case rn.ch <- struct{}{}:
	default:
	}
}

// run is the debounce loop. It waits for the first signal, then sleeps 2 s
// to coalesce any burst (season pack), then fires one refresh per arr.
func (rn *readyNotifier) run(ctx context.Context) {
	const debounce = 2 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-rn.ch:
		}
		// Drain any coalesced signals during the debounce window.
		timer := time.NewTimer(debounce)
		draining := true
		for draining {
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-rn.ch:
				// coalesced — reset debounce
				if !timer.Stop() {
					<-timer.C
				}
				timer.Reset(debounce)
			case <-timer.C:
				draining = false
			}
		}
		notifyArrsRefresh(ctx, rn.log, rn.arrs)
	}
}

// notifyArrsRefresh POSTs Sonarr/Radarr's RefreshMonitoredDownloads command
// (same command name, same endpoint shape on both) to every configured arr.
// Best-effort: one arr being unreachable never blocks the others or fails
// the caller — this is a convenience notification, not a critical path DH's
// own correctness depends on (an un-notified arr still catches up on its
// own next poll; the pruned item is gone from DH's side either way).
func notifyArrsRefresh(ctx context.Context, log *slog.Logger, arrs []config.ArrTarget) {
	if len(arrs) == 0 {
		return
	}
	client := &http.Client{Timeout: 10 * time.Second}
	for _, arr := range arrs {
		log.Info("arr notify: refresh requested",
			"event", "arr_refresh_requested",
			"arr", arr.Name,
		)
		reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, arr.BaseURL+"/api/v3/command",
			strings.NewReader(`{"name":"RefreshMonitoredDownloads"}`))
		if err != nil {
			log.Warn("arr notify: build request failed",
				"event", "arr_refresh_failed",
				"arr", arr.Name,
				"error", err,
			)
			cancel()
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Api-Key", arr.APIKey)
		resp, err := client.Do(req)
		cancel()
		if err != nil {
			log.Warn("arr notify: request failed (non-fatal)",
				"event", "arr_refresh_failed",
				"arr", arr.Name,
				"error", err,
			)
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 300 {
			log.Warn("arr notify: non-2xx response",
				"event", "arr_refresh_failed",
				"arr", arr.Name,
				"status", resp.StatusCode,
			)
			continue
		}
		log.Info("arr notify: refresh triggered",
			"event", "arr_refresh_succeeded",
			"arr", arr.Name,
			"status", resp.StatusCode,
		)
	}
}

const failureCodeNNTPDeadAllProviders = "nntp_dead_all_providers"
const failureCodeNNTPDecayed = "nntp_decayed"

type terminalFailureStore interface {
	BlacklistImmediately(context.Context, string) (store.FailureRecordResult, error)
	UpdateItemState(context.Context, *store.Item, store.ItemState, string) error
	// RecordSuppression is SF-03's own-measurement failed-release
	// suppression write side (internal/suppress + release_suppressions).
	RecordSuppression(ctx context.Context, fingerprint, lane, reason string, ttl time.Duration) error
	// GetGrabProviderIdentity is ID-02's own grab-time identity cache
	// read side (internal/store + grab_provider_identities, 30-day TTL).
	// NS-6.2 uses it as a fallback identity source (see
	// triggerReSearchOnDecay) when an item's own durable
	// Metadata.ProviderIdentity was never populated.
	GetGrabProviderIdentity(ctx context.Context, key string) (*store.ProviderIdentity, bool, error)
}

type arrRefreshSignal interface {
	Notify()
}

// httpDecayEscalatorFunc adapts a plain func to api.HTTPDecayEscalator
// (HR3.4), the same func-to-interface convention http.HandlerFunc
// establishes -- avoids a named struct type for a single-method wrapper
// around triggerReSearchOnDecay.
type httpDecayEscalatorFunc func(item *store.Item)

func (f httpDecayEscalatorFunc) EscalateDecay(item *store.Item) { f(item) }

// researchCooldown is NS-6.2's bounded, in-memory, per-item cooldown guard
// preventing a single decayed item from triggering more than one Arr
// re-search per configured window. Mirrors internal/api's TS-4.1
// repairState.tryClaim pattern exactly, scoped to item ID rather than
// torrent infohash -- NNTP items have no infohash. No DG-02 migration:
// process-lifetime-only bookkeeping, the same convention
// Governor.RepairCooldownMin already established. Primarily defense in
// depth: a StateFailed item already leaves NextHealthAuditItem's
// 'ready'-only selection on its own, so under normal single-tick-at-a-time
// operation this cooldown is never even exercised twice for the same item;
// it exists to make that guarantee explicit and testable (DG-05 cooldown
// injection) rather than implicit in NextHealthAuditItem's query alone.
type researchCooldown struct {
	mu       sync.Mutex
	cooldown map[string]time.Time // item ID -> earliest time a new trigger may fire
	now      func() time.Time
}

func newResearchCooldown() *researchCooldown {
	return &researchCooldown{
		cooldown: make(map[string]time.Time),
		now:      func() time.Time { return time.Now().UTC() },
	}
}

// SetClock overrides the injectable clock (tests only; mirrors
// store.Store.SetClock / accountgov.ConnCeiling.SetClock).
func (rc *researchCooldown) SetClock(fn func() time.Time) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.now = fn
}

// tryClaim reports whether a re-search trigger may fire now for itemID,
// atomically opening a fresh cooldown window of the given duration if so.
// cooldown <= 0 disables the guard (always claims, opens no window).
func (rc *researchCooldown) tryClaim(itemID string, cooldown time.Duration) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	now := rc.now()
	if until, ok := rc.cooldown[itemID]; ok && now.Before(until) {
		return false
	}
	if cooldown > 0 {
		rc.cooldown[itemID] = now.Add(cooldown)
	} else {
		delete(rc.cooldown, itemID)
	}
	return true
}

// triggerReSearchOnDecay is NS-6.2's destructive-and-corrective action for
// a two-pass-reconfirmed decayed item (see shouldTriggerReSearch). Per
// WORKFLOW.md's own locked constraint ("preserve permanent DH-proxied .strm
// identity, lazy playback"), this NEVER touches the item's StateFailed
// transition or its materialized .strm -- an earlier version of this row
// did both, and that was wrong: the .strm is a stable pointer to
// DarkHarrbor's own resolver, not a raw source URL, and destroying it to
// get an Arr's attention is backwards. The item stays Ready.
//
// What actually happens:
//  1. Blacklist the exact dead NZB content-key (existing failed_hashes
//     store, api.NZBBlacklistKey) so this specific release is never
//     re-grabbed.
//  2. Record an SF-03 suppression fingerprint reusing suppress.ReasonDeadPost
//     (a confirmed-decayed item is the same "provider(s) no longer carry
//     this content" condition dead_post already names -- not a new
//     suppression vocabulary entry), so the same effective release
//     resurfacing under a different NZB is also suppressed.
//  3. Resolve the item's media identity (durable item.Metadata.ProviderIdentity
//     first; ID-02's own grab-time cache, store.GetGrabProviderIdentity keyed
//     by nntp.ContentKey(item.SourceURI), as a 30-day-bounded fallback for
//     items ID-02 never durably wrote back to the item itself -- a real,
//     disclosed gap this row does not attempt to close). If neither yields
//     a kind+TVDB/TMDB id, abstain from any Arr call: blacklist/suppression
//     above already prevents the SAME dead release from resurfacing, and
//     guessing which Arr or which title to search is exactly the kind of
//     guess WORKFLOW.md's "abstain or fail closed" rule forbids.
//  4. Resolve exactly which configured Arr owns that title by querying each
//     Arr's own library for the id (resolveArrSearchTarget) -- ownership is
//     determined by which Arr actually has it, not by a category-name
//     convention DarkHarrbor doesn't track structurally. Zero or more than
//     one match abstains.
//  5. Fire a real, title-scoped search command (SeriesSearch/MoviesSearch)
//     at that one Arr -- confirmed live against real Sonarr and Radarr
//     instances to actually queue a targeted search, unlike
//     RefreshMonitoredDownloads (which only re-checks an Arr's own active
//     download-client queue and does nothing for already-imported content
//     -- the real gap this correction closes).
//
// rc's cooldown claim is checked first so a single item can never fire this
// more than once per cfg.HealthAudit.ReSearchCooldownMin.
func triggerReSearchOnDecay(
	ctx context.Context,
	log *slog.Logger,
	st terminalFailureStore,
	item *store.Item,
	arrs []config.ArrTarget,
	rc *researchCooldown,
	cooldown time.Duration,
	suppressionTTL time.Duration,
) {
	if item == nil || rc == nil {
		return
	}
	log = log.With(
		"item_id", item.ID,
		"public_id", item.PublicID,
		"source_type", item.SourceType,
		"display_name", item.DisplayName,
	)
	if !rc.tryClaim(item.ID, cooldown) {
		log.Info("healthaudit: re-search trigger skipped (cooldown active)",
			"event", "healthaudit_research_skipped")
		return
	}

	if item.SourceURI != nil && *item.SourceURI != "" {
		key := api.NZBBlacklistKey(*item.SourceURI)
		result, err := st.BlacklistImmediately(ctx, key)
		if err != nil {
			log.Error("healthaudit: blacklist persist failed",
				"event", "item_failure_persist_failed",
				"failure_code", failureCodeNNTPDecayed,
				"nzb_key", key,
				"error", err,
			)
		} else {
			log.Warn("healthaudit: source blacklisted after confirmed decay",
				"event", "source_blacklisted",
				"failure_code", failureCodeNNTPDecayed,
				"nzb_key", key,
				"fail_count", result.FailCount,
				"blacklisted", result.Blacklisted,
				"blacklisted_until", result.BlacklistedUntil,
			)
		}

		// SF-03: also fingerprint on normalized name+size+lane, reusing
		// ReasonDeadPost (see doc comment above), so the SAME effective
		// release re-surfacing under a DIFFERENT NZB is still suppressed
		// from DarkHarrbor's own feeds for the configured TTL. Best-
		// effort: never blocks or fails this trigger.
		fp := suppress.Fingerprint(item.DisplayName, item.TotalSize, suppress.LaneNZB)
		if serr := st.RecordSuppression(ctx, fp, string(suppress.LaneNZB), string(suppress.ReasonDeadPost), suppressionTTL); serr != nil {
			log.Warn("healthaudit: SF-03 suppression record failed (non-fatal)",
				"event", "suppression_record_failed",
				"error", serr,
			)
		}
	} else {
		log.Warn("healthaudit: no stored NZB source URI; skipping blacklist/suppression",
			"event", "healthaudit_research_no_source_uri")
	}

	identity := resolveDecayedItemIdentity(ctx, log, st, item)
	if identity == nil {
		log.Warn("healthaudit: no usable media identity for this item; cannot target a re-search, abstaining",
			"event", "healthaudit_research_no_identity")
		return
	}

	target, ok := resolveArrSearchTarget(ctx, log, arrs, identity)
	if !ok {
		log.Warn("healthaudit: could not resolve exactly one owning arr for this title; abstaining from re-search",
			"event", "healthaudit_research_no_target", "kind", identity.Kind)
		return
	}

	// The real gap a bare search command left open (live-verified this
	// session): a SeriesSearch/MoviesSearch only makes the Arr chase
	// content it ALREADY believes is missing. For a decayed item the Arr
	// still thinks it has -- exactly this row's whole reason to exist --
	// firing the search alone accomplishes nothing; a real live test
	// against Roseanne showed Sonarr searching only the seasons it already
	// considered incomplete, never touching the season holding the actual
	// decayed episodes. markArrFileMissing closes that gap by telling the
	// Arr this ONE file specifically is gone, clearing only its own
	// bookkeeping -- verified live via direct filesystem inspection that
	// this never touches DarkHarrbor's own .strm. If that can't be done
	// safely (ambiguous season/episode, lookup failure), the search is
	// skipped entirely: firing it without first marking the file missing
	// would be a no-op given what was just proven live, not a smaller,
	// safer action.
	season, episode := 0, 0
	if target.kind == "series" {
		season, episode, ok = identitycheck.ParseSeasonEpisode(item.DisplayName)
		if !ok {
			log.Warn("healthaudit: release title has no exact season/episode token; abstaining from marking any arr file missing",
				"event", "healthaudit_research_no_season_episode")
			return
		}
	}
	if !markArrFileMissing(ctx, log, target, season, episode) {
		log.Warn("healthaudit: could not mark the arr's file record missing; skipping search (would be a no-op)",
			"event", "healthaudit_research_mark_missing_failed", "arr", target.arr.Name, "kind", target.kind)
		return
	}

	fireArrSearchCommand(ctx, log, target)
}

// markArrFileMissing tells the owning Arr that this item's specific
// episode/movie file no longer exists, clearing ONLY the Arr's own
// database bookkeeping -- never DarkHarrbor's own .strm or item state --
// so a subsequent search command actually attempts a replacement instead
// of silently skipping content the Arr still believes is intact. Verified
// live this session: DELETE /api/v3/episodefile/{id} (and, by the same
// contract, /api/v3/moviefile/{id}) clears only the Arr's hasFile/
// episodeFileId-or-movieFileId bookkeeping; the underlying .strm file on
// disk was confirmed byte-identical and untouched via direct filesystem
// inspection before and after a real call, and a subsequent real
// EpisodeSearch on the same episode genuinely found and downloaded a
// replacement release ("1 reports downloaded"). If the file is already
// absent per the Arr's own bookkeeping (hasFile=false), there is nothing
// to delete but the search is still worth firing, so this returns true.
// If no replacement is ultimately found, the Arr is left believing the
// episode/movie is missing while the original (still on-disk) .strm
// remains exactly as playable as before this call -- no worse off, and
// the Arr's own next disk-rescan naturally reconciles it.
func markArrFileMissing(ctx context.Context, log *slog.Logger, target arrSearchTarget, season, episode int) bool {
	client := &http.Client{Timeout: 10 * time.Second}
	fileID, hasFile, ok := arrFileIDForTarget(ctx, client, log, target, season, episode)
	if !ok {
		return false
	}
	if !hasFile {
		// Already missing per the Arr's own records -- nothing to
		// delete, but a search is still worthwhile.
		return true
	}

	deletePath := "/api/v3/episodefile/"
	if target.kind == "movie" {
		deletePath = "/api/v3/moviefile/"
	}
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodDelete, target.arr.BaseURL+deletePath+strconv.Itoa(fileID), nil)
	if err != nil {
		log.Warn("healthaudit: build mark-missing request failed", "arr", target.arr.Name, "kind", target.kind, "error", err)
		return false
	}
	req.Header.Set("X-Api-Key", target.arr.APIKey)
	resp, err := client.Do(req)
	if err != nil {
		log.Warn("healthaudit: mark-missing delete request failed (non-fatal)",
			"event", "arr_file_mark_missing_failed", "arr", target.arr.Name, "kind", target.kind, "error", err)
		return false
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Warn("healthaudit: mark-missing delete returned non-2xx",
			"event", "arr_file_mark_missing_failed", "arr", target.arr.Name, "kind", target.kind, "status", resp.StatusCode)
		return false
	}
	log.Info("healthaudit: arr file record cleared (bookkeeping only, .strm untouched)",
		"event", "arr_file_marked_missing", "arr", target.arr.Name, "kind", target.kind, "file_id", fileID, "status", resp.StatusCode)
	return true
}

// arrFileIDForTarget resolves the specific episodeFileId (series, matched
// by exact season+episode number) or movieFileId (movie, the target's own
// single record) to delete, along with whether the Arr currently has a
// file for it at all. ok=false means abstain: an ambiguous or absent
// season/episode, an unreachable Arr, or a malformed response -- never a
// guessed file ID.
func arrFileIDForTarget(ctx context.Context, client *http.Client, log *slog.Logger, target arrSearchTarget, season, episode int) (fileID int, hasFile bool, ok bool) {
	switch target.kind {
	case "series":
		if season < 1 || episode < 1 {
			return 0, false, false
		}
		reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
			fmt.Sprintf("%s/api/v3/episode?seriesId=%d&seasonNumber=%d", target.arr.BaseURL, target.internalID, season), nil)
		if err != nil {
			cancel()
			return 0, false, false
		}
		req.Header.Set("X-Api-Key", target.arr.APIKey)
		resp, err := client.Do(req)
		cancel()
		if err != nil {
			log.Warn("healthaudit: episode lookup failed (non-fatal)", "arr", target.arr.Name, "error", err)
			return 0, false, false
		}
		var episodes []struct {
			EpisodeNumber int  `json:"episodeNumber"`
			HasFile       bool `json:"hasFile"`
			EpisodeFileID int  `json:"episodeFileId"`
		}
		decErr := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&episodes)
		resp.Body.Close()
		if decErr != nil {
			return 0, false, false
		}
		for _, e := range episodes {
			if e.EpisodeNumber == episode {
				return e.EpisodeFileID, e.HasFile, true
			}
		}
		return 0, false, false
	case "movie":
		reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
			fmt.Sprintf("%s/api/v3/movie/%d", target.arr.BaseURL, target.internalID), nil)
		if err != nil {
			cancel()
			return 0, false, false
		}
		req.Header.Set("X-Api-Key", target.arr.APIKey)
		resp, err := client.Do(req)
		cancel()
		if err != nil {
			log.Warn("healthaudit: movie lookup failed (non-fatal)", "arr", target.arr.Name, "error", err)
			return 0, false, false
		}
		var movie struct {
			HasFile     bool `json:"hasFile"`
			MovieFileID int  `json:"movieFileId"`
		}
		decErr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&movie)
		resp.Body.Close()
		if decErr != nil {
			return 0, false, false
		}
		return movie.MovieFileID, movie.HasFile, true
	default:
		return 0, false, false
	}
}

// resolveDecayedItemIdentity returns the best available media identity for
// item: its own durable Metadata.ProviderIdentity if ID-02 ever wrote one
// back to it, otherwise ID-02's grab-time cache (30-day TTL) keyed by the
// exact same nntp.ContentKey hash the grab path itself used
// (internal/api/router.go's attachGrabProviderIdentity). Returns nil,
// meaning abstain, if neither source has a usable kind+id -- this function
// never guesses an identity from title text alone.
func resolveDecayedItemIdentity(ctx context.Context, log *slog.Logger, st terminalFailureStore, item *store.Item) *store.ProviderIdentity {
	if item.Metadata.ProviderIdentity != nil {
		id := item.Metadata.ProviderIdentity
		if id.Kind == "series" || id.Kind == "movie" {
			return id
		}
	}
	if item.SourceURI == nil || *item.SourceURI == "" {
		return nil
	}
	key := nntp.ContentKey([]byte(*item.SourceURI))
	identity, found, err := st.GetGrabProviderIdentity(ctx, key)
	if err != nil {
		log.Debug("healthaudit: grab-time identity fallback lookup failed (non-fatal)",
			"event", "healthaudit_research_identity_fallback_failed", "error", err)
		return nil
	}
	if !found || identity == nil {
		return nil
	}
	if identity.Kind != "series" && identity.Kind != "movie" {
		return nil
	}
	return identity
}

// arrSearchTarget names one Arr instance's own internal identity for a
// specific series or movie.
type arrSearchTarget struct {
	arr        config.ArrTarget
	internalID int
	kind       string // "series" or "movie"
}

// resolveArrSearchTarget determines which SINGLE configured Arr owns the
// given identity by querying each Arr's own library for it -- ownership by
// asking each Arr the ground truth, not by any category-name convention
// DarkHarrbor doesn't track structurally (item.Category is set by whoever
// submitted the grab, not read back from any Arr). Zero or more than one
// match is ambiguous and abstains (nil, false); this must never guess which
// Arr or which title to search.
func resolveArrSearchTarget(ctx context.Context, log *slog.Logger, arrs []config.ArrTarget, identity *store.ProviderIdentity) (arrSearchTarget, bool) {
	if identity == nil || (identity.Kind != "series" && identity.Kind != "movie") {
		return arrSearchTarget{}, false
	}
	var path string
	switch identity.Kind {
	case "series":
		if identity.IDs.TVDB == "" {
			return arrSearchTarget{}, false
		}
		path = "/api/v3/series?tvdbId=" + identity.IDs.TVDB
	case "movie":
		if identity.IDs.TMDB == "" {
			return arrSearchTarget{}, false
		}
		path = "/api/v3/movie?tmdbId=" + identity.IDs.TMDB
	}

	client := &http.Client{Timeout: 10 * time.Second}
	var matches []arrSearchTarget
	for _, arr := range arrs {
		if arr.BaseURL == "" || arr.APIKey == "" {
			continue
		}
		reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, arr.BaseURL+path, nil)
		if err != nil {
			cancel()
			continue
		}
		req.Header.Set("X-Api-Key", arr.APIKey)
		resp, err := client.Do(req)
		cancel()
		if err != nil {
			log.Debug("healthaudit: arr identity lookup failed (non-fatal)",
				"arr", arr.Name, "kind", identity.Kind, "error", err)
			continue
		}
		var candidates []struct {
			ID int `json:"id"`
		}
		decErr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&candidates)
		resp.Body.Close()
		if decErr != nil {
			continue
		}
		for _, c := range candidates {
			if c.ID > 0 {
				matches = append(matches, arrSearchTarget{arr: arr, internalID: c.ID, kind: identity.Kind})
			}
		}
	}
	if len(matches) != 1 {
		if len(matches) > 1 {
			log.Warn("healthaudit: ambiguous arr match for re-search, abstaining",
				"event", "healthaudit_research_ambiguous_target", "kind", identity.Kind, "match_count", len(matches))
		}
		return arrSearchTarget{}, false
	}
	return matches[0], true
}

// fireArrSearchCommand posts a real, title-scoped search command to the
// resolved Arr -- SeriesSearch for Sonarr, MoviesSearch for Radarr -- the
// same POST /api/v3/command shape and X-Api-Key header pattern
// notifyArrsRefresh already uses elsewhere in this file, just a different
// command name and body. Confirmed live against real Sonarr and Radarr
// instances to actually queue a targeted search (status "queued"), unlike
// RefreshMonitoredDownloads.
func fireArrSearchCommand(ctx context.Context, log *slog.Logger, target arrSearchTarget) {
	var body string
	switch target.kind {
	case "series":
		body = fmt.Sprintf(`{"name":"SeriesSearch","seriesId":%d}`, target.internalID)
	case "movie":
		body = fmt.Sprintf(`{"name":"MoviesSearch","movieIds":[%d]}`, target.internalID)
	default:
		return
	}
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target.arr.BaseURL+"/api/v3/command", strings.NewReader(body))
	if err != nil {
		log.Warn("healthaudit: build arr search request failed",
			"event", "arr_research_request_build_failed", "arr", target.arr.Name, "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", target.arr.APIKey)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Warn("healthaudit: arr search request failed (non-fatal)",
			"event", "arr_research_request_failed", "arr", target.arr.Name, "error", err)
		return
	}
	resp.Body.Close()
	log.Info("healthaudit: arr targeted re-search requested",
		"event", "arr_research_requested",
		"arr", target.arr.Name, "kind", target.kind, "internal_id", target.internalID, "status", resp.StatusCode)
}

func handleAllProvidersDead(
	ctx context.Context,
	log *slog.Logger,
	st terminalFailureStore,
	item *store.Item,
	rn arrRefreshSignal,
	probeProviderUsed string,
	providersTried int,
	deadPostCount int,
	suppressionTTL time.Duration,
) bool {
	if item == nil || item.SourceURI == nil || *item.SourceURI == "" ||
		probeProviderUsed != "" || providersTried == 0 || deadPostCount != providersTried {
		return false
	}

	log = log.With(
		"item_id", item.ID,
		"public_id", item.PublicID,
		"source_type", item.SourceType,
		"display_name", item.DisplayName,
		"category", item.Category,
	)
	key := api.NZBBlacklistKey(*item.SourceURI)
	result, err := st.BlacklistImmediately(ctx, key)
	if err != nil {
		log.Error("resolveNZB: immediate blacklist persist failed",
			"event", "item_failure_persist_failed",
			"failure_code", failureCodeNNTPDeadAllProviders,
			"nzb_key", key,
			"error", err,
		)
	} else {
		log.Warn("resolveNZB: source failure recorded",
			"event", "source_failure_recorded",
			"failure_code", failureCodeNNTPDeadAllProviders,
			"nzb_key", key,
			"fail_count", result.FailCount,
			"blacklisted", result.Blacklisted,
			"blacklisted_until", result.BlacklistedUntil,
		)
		if result.Blacklisted {
			log.Warn("resolveNZB: source blacklisted after all-provider dead result",
				"event", "source_blacklisted",
				"failure_code", failureCodeNNTPDeadAllProviders,
				"nzb_key", key,
				"fail_count", result.FailCount,
				"blacklisted_until", result.BlacklistedUntil,
			)
		}
	}

	// SF-03: in addition to the existing exact-identity blacklist above
	// (keyed on this exact NZB's content hash), also fingerprint on
	// normalized name+size+lane so the SAME effective release re-surfacing
	// under a DIFFERENT NZB (a re-post, a different indexer's copy) is still
	// suppressed from DarkHarrbor's own feeds for the configured TTL.
	// Best-effort: never blocks or fails this resolve path.
	fp := suppress.Fingerprint(item.DisplayName, item.TotalSize, suppress.LaneNZB)
	if err := st.RecordSuppression(ctx, fp, string(suppress.LaneNZB), string(suppress.ReasonDeadPost), suppressionTTL); err != nil {
		log.Warn("resolveNZB: SF-03 suppression record failed (non-fatal)",
			"event", "suppression_record_failed",
			"error", err,
		)
	}

	msg := failureCodeNNTPDeadAllProviders + ": NZB post is dead on every configured usenet provider (consecutive missing segments)"
	statePersisted := false
	if err := st.UpdateItemState(ctx, item, store.StateFailed, msg); err != nil {
		log.Error("resolveNZB: persist failed state",
			"event", "item_failure_persist_failed",
			"failure_code", failureCodeNNTPDeadAllProviders,
			"error", err,
		)
	} else {
		statePersisted = true
		log.Warn("resolveNZB: item state transitioned",
			"event", "item_state_transition",
			"failure_code", failureCodeNNTPDeadAllProviders,
			"previous_state", store.StateResolving,
			"state", store.StateFailed,
		)
		log.Warn("resolveNZB: terminal all-provider dead failure persisted",
			"event", "item_failure_persisted",
			"failure_code", failureCodeNNTPDeadAllProviders,
			"previous_state", store.StateResolving,
			"state", store.StateFailed,
		)
	}

	if statePersisted && rn != nil {
		rn.Notify()
	}
	return true
}

// resolveNZBItem handles the full pipeline for NZB/usenet items.
// Called from resolveItem when item.SourceType == SourceTypeNZB.
func resolveNZBItem(
	ctx context.Context,
	log *slog.Logger,
	st *store.Store,
	writer *output.StrmWriter,
	prober *probe.Prober,
	probeBytes int64,
	baseProxyURL string,
	nntpProviders map[string]*nntp.NNTPProvider,
	usenetOrder []string,
	item *store.Item,
	streamSecret string,
	dataRoot string,
	stubAuthority string,
	rn *readyNotifier,
	suppressionTTL time.Duration,
	nntpExpSvc *experience.Service,
	checkIdentity func(context.Context, *store.Item, int, int, *mediatruth.Facts),
) {
	log = log.With("item_id", item.ID, "display_name", item.DisplayName)

	if item.SourceURI == nil || *item.SourceURI == "" {
		log.Warn("resolveNZB: no NZB data in source_uri")
		msg := "no NZB data stored"
		if err := st.UpdateItemState(ctx, item, store.StateFailed, msg); err != nil {
			log.Error("store: persist failed state failed", "item_id", item.ID, "error", err)
		}
		return
	}

	if len(nntpProviders) == 0 {
		log.Warn("resolveNZB: NNTP provider not configured")
		msg := "NNTP provider not configured"
		if err := st.UpdateItemState(ctx, item, store.StateFailed, msg); err != nil {
			log.Error("store: persist failed state failed", "item_id", item.ID, "error", err)
		}
		return
	}

	// Each video file gets its own .strm pointing at /stream/{id}/{index}.
	parsedNZB, parseErr := nntp.ParseNZB([]byte(*item.SourceURI))
	if parseErr != nil {
		log.Warn("resolveNZB: parse NZB failed (will retry)", "error", parseErr)
		scheduleResolveRetry(ctx, log, st, item, 60*time.Second, fmt.Sprintf("parse NZB failed: %v", parseErr))
		return
	}
	nzbFiles := parsedNZB.Files
	if len(nzbFiles) == 0 {
		msg := "NZB contains no files"
		if err := st.UpdateItemState(ctx, item, store.StateFailed, msg); err != nil {
			log.Error("store: persist failed state failed", "item_id", item.ID, "error", err)
		}
		return
	}

	// Resolve content in the frozen N4/N5 order: exact PAR2 identity, then
	// bounded magic bytes, then legacy subject hints. The original source XML
	// is never changed.
	parsedNZB, exactVideoIndexes := recoverNZBNamesFromPAR2(ctx, log, nntpProviders, usenetOrder, item, parsedNZB)
	nzbFiles = parsedNZB.Files
	if len(exactVideoIndexes) > 0 {
		log.Info("resolveNZB: video file(s) recovered via exact PAR2 identity",
			"event", "par2_video_recovery", "count", len(exactVideoIndexes))
	}
	magic, allowSubjectFallback := classifyNZBContent(ctx, log, nntpProviders, usenetOrder, item.ID, parsedNZB)
	videoFiles, exactVideoIndexes := selectNZBVideoFilesByEvidence(nzbFiles, exactVideoIndexes, magic, allowSubjectFallback)

	// RAR-split NZBs have no direct video entry. Pass the PAR2-enriched parsed
	// copy so recovered volume names drive exact part selection and ordering.
	if len(videoFiles) == 0 {
		resolveRARItem(ctx, log, st, writer, baseProxyURL, nntpProviders, usenetOrder, item, parsedNZB, magic, allowSubjectFallback, dataRoot, stubAuthority, rn, nntpExpSvc, checkIdentity)
		return
	}

	// Ordinary subjects retain the historical filtered-video index. A PAR2-
	// recovered direct file carries its exact ParseNZB index in the WebDAV URL
	// so playback cannot silently select a PAR2/NFO/other payload.
	var firstStrmPath string
	var totalSize int64
	for i, selection := range videoFiles {
		var strmPath, strmURL string
		var err error
		if exactVideoIndexes[selection.NZBFileIdx] {
			strmPath, strmURL, err = writeNZBStrmExactNZBFile(item, baseProxyURL, writer.StrmRoot(), selection.NZBFileIdx, selection.File.Subject)
		} else {
			strmPath, strmURL, err = writeNZBStrm(item, baseProxyURL, writer.StrmRoot(), i, selection.File.Subject)
		}
		if err != nil {
			log.Warn("resolveNZB: write strm failed (will retry)", "error", err, "file_index", i)
			scheduleResolveRetry(ctx, log, st, item, 60*time.Second, fmt.Sprintf("write NZB strm failed: %v", err))
			return
		}
		// F-E: persist the NZB .strm as authoritative DB content (authority
		// "db"); the on-disk .strm is the materialization receipt. NZB .strm
		// files are written here (not by output.StrmWriter), so persistence is
		// done here rather than via the writer's blob sink.
		if stubAuthority == "db" {
			if rel, relErr := filepath.Rel(dataRoot, strmPath); relErr == nil && !strings.HasPrefix(rel, "..") {
				if perr := st.UpsertStrmBlobAt(ctx, item.ID, i, rel, strmURL); perr != nil {
					log.Warn("resolveNZB: persist strm blob failed (non-fatal)", "file_index", i, "error", perr)
				}
			}
		}
		if nfoErr := writer.WriteProviderNFO(ctx, item, fmt.Sprintf("nzb-%d", i), strmPath); nfoErr != nil {
			log.Warn("resolveNZB: write provider NFO failed (will retry)", "error", nfoErr, "file_index", i)
			scheduleResolveRetry(ctx, log, st, item, 60*time.Second, "write NZB provider NFO failed")
			return
		}
		if i == 0 {
			firstStrmPath = strmPath
		}
		totalSize += selection.File.TotalBytes
	}

	item.StrmPath = &firstStrmPath
	item.Cached = true
	item.TotalSize = totalSize

	// Real NNTP probe: fetch first probeBytes via NNTP, run ffprobe, persist
	// ProbeJSON so stubs can be regenerated without re-probing. Tries every
	// configured usenet provider in order (self-contained instances, e.g.
	// Newshosting then TorBox's NNTP News Server) before falling back to
	// inferNZBProbeResult — a provider-specific hiccup (auth, propagation
	// gap for this group, transient outage) no longer has to fail the whole
	// probe when another configured source could serve the same article.
	// The provider that actually succeeds is recorded on item.Provider so
	// a later stream request reconnects through the same one instead of
	// re-discovering it (api/router.go's pickUsenetProvider reads this
	// field back).
	var probeProviderUsed string
	providersTried := 0
	deadPostCount := 0
	for _, name := range usenetOrder {
		np, ok := nntpProviders[name]
		if !ok {
			continue
		}
		providersTried++
		tmpPath, probeErr := np.ProbeForItem(ctx, item.ID, []byte(*item.SourceURI), probeBytes)
		if probeErr != nil {
			if errors.Is(probeErr, nntp.ErrDeadPost) {
				deadPostCount++
			}
			log.Warn("resolveNZB: NNTP probe failed, trying next provider",
				"event", "nntp_probe_failed",
				"provider", name,
				"nzb_key", api.NZBBlacklistKey(*item.SourceURI),
				"dead_post", errors.Is(probeErr, nntp.ErrDeadPost),
				"providers_tried", providersTried,
			)
			continue
		}
		realResult, ffErr := prober.ProbeFromFile(ctx, tmpPath)
		_ = os.Remove(tmpPath)
		if ffErr != nil {
			log.Warn("resolveNZB: ffprobe on NNTP temp failed, trying next provider", "provider", name, "error", ffErr)
			continue
		}
		probeJSON := realResult.JSON
		item.ProbeJSON = &probeJSON
		probeProviderUsed = name
		break
	}
	log.Info("resolveNZB: NNTP provider probe summary",
		"event", "nntp_probe_summary",
		"nzb_key", api.NZBBlacklistKey(*item.SourceURI),
		"providers_tried", providersTried,
		"dead_post_count", deadPostCount,
		"provider_used", probeProviderUsed,
		"terminal", probeProviderUsed == "" && providersTried > 0 && deadPostCount == providersTried,
	)
	if handleAllProvidersDead(ctx, log, st, item, rn, probeProviderUsed, providersTried, deadPostCount, suppressionTTL) {
		return
	}
	if probeProviderUsed == "" {
		log.Warn("resolveNZB: NNTP probe failed on every configured provider, using inferred result (non-fatal)")
	} else {
		// A-B8: do NOT UpdateItem here — the mid-flight write clears claimed_by
		// and opens a double-claim window. Provider + ProbeJSON are already set
		// on the item struct; UpdateItemState (below) persists them atomically.
		item.Provider = &probeProviderUsed
	}
	if np := firstNNTPProvider(nntpProviders, usenetOrder, probeProviderUsed); np != nil {
		targets := make([]nntp.SubtitleTarget, 0, len(videoFiles))
		for i, selection := range videoFiles {
			targets = append(targets, nntp.SubtitleTarget{
				Filename: selection.File.Subject,
				FileID:   fmt.Sprintf("nzb-%d", i),
			})
		}
		payloads, subtitleErr := np.ReadDirectSubtitles(ctx, item.ID, parsedNZB, targets,
			sidecar.DefaultMaxResourceBytes, sidecar.DefaultMaxResourcesPerItem)
		if subtitleErr != nil && ctx.Err() != nil {
			return
		}
		if err := registerNNTPSubtitles(ctx, log, writer, item, payloads); err != nil {
			scheduleResolveRetry(ctx, log, st, item, 60*time.Second, "persist NNTP subtitles failed")
			return
		}
	}
	log.Info("resolveNZB: strms written successfully",
		"strm_path", firstStrmPath,
		"file_count", len(videoFiles),
	)
	if err := st.UpdateItemState(ctx, item, store.StateReady, ""); err != nil {
		// BUGFIX 2026-07-01 (torbox API-hit audit): same missing-backoff
		// class as the torrent-lane fixes above -- a persistent failure
		// here would re-run the whole NZB parse + NNTP probe + stub-write
		// sequence every 3s forever (NNTP-only, not TorBox, but the same
		// hot-loop shape the rest of this audit is eliminating).
		log.Error("resolveNZB: update state failed", "error", err)
		scheduleResolveRetry(ctx, log, st, item, 30*time.Second, err.Error())
		return
	}
	// ID-01: grab-time identity verification. The NNTP lane has no
	// persisted media-truth Facts today (SF-04's disclosed limitation),
	// so this only exercises the title-only checks (release year,
	// conflicting source tags) -- runtime/audio-language safely abstain
	// on the nil facts, exactly as internal/identitycheck documents.
	if checkIdentity != nil {
		season, episode, _ := identitycheck.ParseSeasonEpisode(item.DisplayName)
		checkIdentity(ctx, item, season, episode, nil)
	}
	// B-N9: debounced arr refresh.
	rn.Notify()
	if np := nntpProviders[probeProviderUsed]; np != nil {
		largestIndex := 0
		for i := 1; i < len(videoFiles); i++ {
			if videoFiles[i].File.TotalBytes > videoFiles[largestIndex].File.TotalBytes {
				largestIndex = i
			}
		}
		prewarmIndex := largestIndex
		if exactVideoIndexes[videoFiles[largestIndex].NZBFileIdx] {
			prewarmIndex = videoFiles[largestIndex].NZBFileIdx
		}
		runNNTPPrewarm(ctx, log, st, item, nntpExpSvc, np.PrewarmFileSource(
			item.ID, nntp.ContentKey([]byte(*item.SourceURI)), prewarmIndex, videoFiles[largestIndex].File.Segments,
		))
	}
}

// resolveSevenZipItem builds and persists a Copy-coded 7z manifest, writes
// one pointer per video member, and leaves all byte acquisition to NNTPProvider.
func resolveSevenZipItem(
	ctx context.Context,
	log *slog.Logger,
	st *store.Store,
	writer *output.StrmWriter,
	baseProxyURL string,
	nntpProviders map[string]*nntp.NNTPProvider,
	usenetOrder []string,
	item *store.Item,
	parsedNZB nntp.NZB,
	dataRoot string,
	stubAuthority string,
	rn *readyNotifier,
	checkIdentity func(context.Context, *store.Item, int, int, *mediatruth.Facts),
) {
	log = log.With("item_id", item.ID, "display_name", item.DisplayName)
	var manifest nntp.SevenZipManifest
	var totalSize int64
	var winner string
	for _, name := range usenetOrder {
		provider := nntpProviders[name]
		if provider == nil {
			continue
		}
		built, size, err := provider.BuildSevenZipManifestForItem(ctx, item.ID, parsedNZB)
		if err != nil {
			// Parser/acquisition errors may carry source-controlled identifiers.
			// Keep the operational signal while withholding raw error text.
			log.Warn("resolve7z: manifest build failed on provider", "provider", name)
			continue
		}
		manifest, totalSize, winner = built, size, name
		break
	}
	if winner == "" {
		scheduleResolveRetry(ctx, log, st, item, 90*time.Second, "build 7z manifest failed on every configured provider")
		return
	}
	manifestJSON, err := nntp.MarshalSevenZipManifest(manifest)
	if err != nil {
		scheduleResolveRetry(ctx, log, st, item, 60*time.Second, "marshal 7z manifest failed validation")
		return
	}
	var firstPath string
	for _, entry := range manifest.Entries {
		path, strmURL, writeErr := writeNZBStrm(item, baseProxyURL, writer.StrmRoot(), entry.EntryIdx, entry.Filename)
		if writeErr != nil {
			log.Warn("resolve7z: write strm failed", "entry_idx", entry.EntryIdx, "error", writeErr)
			scheduleResolveRetry(ctx, log, st, item, 60*time.Second, "write 7z stream pointer failed")
			return
		}
		if stubAuthority == "db" {
			if rel, relErr := filepath.Rel(dataRoot, path); relErr == nil && !strings.HasPrefix(rel, "..") {
				if persistErr := st.UpsertStrmBlobAt(ctx, item.ID, entry.EntryIdx, rel, strmURL); persistErr != nil {
					log.Warn("resolve7z: persist strm blob failed (non-fatal)", "entry_idx", entry.EntryIdx, "error", persistErr)
				}
			}
		}
		if nfoErr := writer.WriteProviderNFO(ctx, item, fmt.Sprintf("nzb-%d", entry.EntryIdx), path); nfoErr != nil {
			log.Warn("resolve7z: write provider NFO failed", "entry_idx", entry.EntryIdx, "error", nfoErr)
			scheduleResolveRetry(ctx, log, st, item, 60*time.Second, "write 7z provider NFO failed")
			return
		}
		if firstPath == "" {
			firstPath = path
		}
	}
	if firstPath == "" {
		scheduleResolveRetry(ctx, log, st, item, 60*time.Second, "7z: no strm files written")
		return
	}
	if np := nntpProviders[winner]; np != nil {
		targets := make([]nntp.SubtitleTarget, 0, len(manifest.Entries))
		for _, entry := range manifest.Entries {
			targets = append(targets, nntp.SubtitleTarget{Filename: entry.Filename, FileID: fmt.Sprintf("nzb-%d", entry.EntryIdx)})
		}
		payloads, subtitleErr := np.ReadSevenZipSubtitles(ctx, item.ID, manifest, targets,
			sidecar.DefaultMaxResourceBytes, sidecar.DefaultMaxResourcesPerItem)
		if subtitleErr != nil && ctx.Err() != nil {
			return
		}
		direct, directErr := np.ReadDirectSubtitles(ctx, item.ID, parsedNZB, targets,
			sidecar.DefaultMaxResourceBytes, sidecar.DefaultMaxResourcesPerItem-len(payloads))
		if directErr != nil && ctx.Err() != nil {
			return
		}
		payloads = append(payloads, direct...)
		if err := registerNNTPSubtitles(ctx, log, writer, item, payloads); err != nil {
			scheduleResolveRetry(ctx, log, st, item, 60*time.Second, "persist 7z subtitles failed")
			return
		}
	}
	item.Metadata.SevenZipManifest = &manifestJSON
	item.TotalSize = totalSize
	item.StrmPath = &firstPath
	item.Provider = &winner
	item.Cached = true
	if err := st.UpdateItem(ctx, item); err != nil {
		scheduleResolveRetry(ctx, log, st, item, 60*time.Second, fmt.Sprintf("7z: persist: %v", err))
		return
	}
	if err := st.UpdateItemState(ctx, item, store.StateReady, ""); err != nil {
		log.Error("resolve7z: persist ready state failed", "error", err)
		return
	}
	log.Info("resolve7z: item ready", "entries", len(manifest.Entries), "total_size_mb", totalSize/1024/1024, "provider", winner)
	if checkIdentity != nil {
		season, episode, _ := identitycheck.ParseSeasonEpisode(item.DisplayName)
		checkIdentity(ctx, item, season, episode, nil)
	}
	if rn != nil {
		rn.Notify()
	}
}

// resolveRARItem handles RAR-split (and 7z-detected) NZB items.
// For RAR archives: builds a RARPart manifest by fetching the first segment of
// each RAR part, parsing RAR3/RAR5 headers to find data offsets, stores the
// manifest as JSON on the item, writes a single .strm, transitions to StateReady.
// For unknown types: fails fast.
func resolveRARItem(
	ctx context.Context,
	log *slog.Logger,
	st *store.Store,
	writer *output.StrmWriter,
	baseProxyURL string,
	nntpProviders map[string]*nntp.NNTPProvider,
	usenetOrder []string,
	item *store.Item,
	parsedNZB nntp.NZB,
	magic nntp.MagicClassification,
	allowSubjectFallback bool,
	dataRoot string,
	stubAuthority string,
	rn *readyNotifier,
	nntpExpSvc *experience.Service,
	checkIdentity func(context.Context, *store.Item, int, int, *mediatruth.Facts),
) {
	log = log.With("item_id", item.ID, "display_name", item.DisplayName)

	if parsedNZB.Password == "" {
		parsedNZB.Password = item.Metadata.Password
	}
	if len(nntpProviders) == 0 {
		msg := "NNTP provider not configured"
		log.Warn("resolveRAR: " + msg)
		if err := st.UpdateItemState(ctx, item, store.StateFailed, msg); err != nil {
			log.Error("store: persist failed state failed", "item_id", item.ID, "error", err)
		}
		return
	}

	nzbFiles := parsedNZB.Files
	contentKey := nntp.ContentKey([]byte(*item.SourceURI))

	archiveType := magic.Format
	if archiveType == archiveparser.FormatUnknown && allowSubjectFallback {
		archiveType = hintNZBArchiveType(nzbFiles)
	}
	switch archiveType {
	case archiveparser.FormatZIP:
		// Delegate to ZIP streaming path — builds central-directory manifest.
		resolveZIPItem(ctx, log, st, writer, baseProxyURL, nntpProviders, usenetOrder, item, dataRoot, stubAuthority, rn, nntpExpSvc, checkIdentity)
		return
	case archiveparser.Format7z:
		resolveSevenZipItem(ctx, log, st, writer, baseProxyURL, nntpProviders, usenetOrder, item, parsedNZB, dataRoot, stubAuthority, rn, checkIdentity)
		return
	case archiveparser.FormatMKV, archiveparser.FormatMP4:
		msg := fmt.Sprintf("direct %s content detected but no trustworthy video filename is available", archiveType)
		log.Warn("resolveRAR: " + msg)
		if err := st.UpdateItemState(ctx, item, store.StateFailed, msg); err != nil {
			log.Error("store: persist failed state failed", "item_id", item.ID, "error", err)
		}
		return
	case archiveparser.FormatNested:
		failNestedArchive(ctx, log, st, item, "resolveRAR")
		return
	case archiveparser.FormatUnknown:
		msg := "unrecognised archive format — requires direct mkv or RAR release"
		log.Warn("resolveRAR: " + msg)
		if err := st.UpdateItemState(ctx, item, store.StateFailed, msg); err != nil {
			log.Error("store: persist failed state failed", "item_id", item.ID, "error", err)
		}
		return
	}

	// Build manifest — tries each configured usenet provider in order
	// (automatic failover, same principle as resolveNZBItem's probe loop
	// above) until one succeeds fetching every RAR part's first segment;
	// only schedules a retry if every configured provider fails this pass.
	// The winning provider is recorded on item.Provider so the stream path
	// (which reads the manifest at play time, not this build) reconnects
	// through the one already known to work for this item.
	log.Info("resolveRAR: building RAR manifest", "nzb_files", len(nzbFiles))
	var manifest []nntp.RARPart
	var totalVideoBytes int64
	var manifestErr error
	var manifestProviderUsed string
	if artifacts, cacheErr := st.GetNNTPArtifacts(ctx, contentKey); cacheErr != nil {
		log.Warn("resolveRAR: shared manifest lookup failed; rebuilding", "error", cacheErr)
	} else if artifacts.RARManifest != nil {
		if cached, unmarshalErr := nntp.UnmarshalRARManifest(*artifacts.RARManifest); unmarshalErr == nil && len(cached) > 0 && cached[0].PackedBytes > 0 && cached[0].Encryption == nil {
			manifest = cached
			for _, part := range manifest {
				totalVideoBytes += part.DataBytes
			}
			if item.Provider != nil {
				manifestProviderUsed = *item.Provider
			}
			if _, ok := nntpProviders[manifestProviderUsed]; !ok && len(usenetOrder) > 0 {
				manifestProviderUsed = usenetOrder[0]
			}
			log.Info("resolveRAR: reused content-keyed manifest", "parts", len(manifest))
		}
	}
	if len(manifest) == 0 {
		for _, name := range usenetOrder {
			np, ok := nntpProviders[name]
			if !ok {
				continue
			}
			m, tb, buildErr := np.BuildRARManifestForItem(ctx, item.ID, parsedNZB)
			if buildErr != nil {
				if errors.Is(buildErr, archiveparser.ErrNestedArchive) {
					failNestedArchive(ctx, log, st, item, "resolveRAR")
					return
				}
				log.Warn("resolveRAR: build manifest failed on provider, trying next", "provider", name, "error", buildErr)
				manifestErr = buildErr
				continue
			}
			manifest, totalVideoBytes, manifestProviderUsed = m, tb, name
			manifestErr = nil
			break
		}
	}
	if manifestProviderUsed == "" {
		log.Warn("resolveRAR: build manifest failed on every configured provider (will retry)", "error", manifestErr)
		scheduleResolveRetry(ctx, log, st, item, 90*time.Second, fmt.Sprintf("build RAR manifest failed: %v", manifestErr))
		return
	}
	item.Provider = &manifestProviderUsed

	// Serialize manifest to JSON and store on item.
	manifestJSON, err := nntp.MarshalRARManifest(manifest)
	if err != nil {
		log.Error("resolveRAR: marshal manifest", "error", err)
		if updateErr := st.UpdateItemState(ctx, item, store.StateFailed, err.Error()); updateErr != nil {
			log.Error("store: persist failed state failed", "item_id", item.ID, "error", updateErr)
		}
		return
	}
	item.RarManifest = &manifestJSON
	item.TotalSize = totalVideoBytes
	if err := st.PromoteNNTPArtifacts(ctx, contentKey, item.ID, item.RarManifest, nil); err != nil {
		log.Warn("resolveRAR: shared manifest persist failed (non-fatal)", "error", err)
	}

	// Write a single strm with fileIndex=0. StreamRAR uses the manifest at
	// play time — the fileIndex in the URL is unused for RAR items.
	strmPath, strmURL, err := writeNZBStrm(item, baseProxyURL, writer.StrmRoot(), 0, "")
	if err != nil {
		log.Warn("resolveRAR: write strm failed (will retry)", "error", err)
		scheduleResolveRetry(ctx, log, st, item, 60*time.Second, fmt.Sprintf("write RAR strm failed: %v", err))
		return
	}
	if stubAuthority == "db" {
		if rel, relErr := filepath.Rel(dataRoot, strmPath); relErr == nil && !strings.HasPrefix(rel, "..") {
			if perr := st.UpsertStrmBlobAt(ctx, item.ID, 0, rel, strmURL); perr != nil {
				log.Warn("resolveRAR: persist strm blob failed (non-fatal)", "error", perr)
			}
		}
	}
	if nfoErr := writer.WriteProviderNFO(ctx, item, "nzb-0", strmPath); nfoErr != nil {
		log.Warn("resolveRAR: write provider NFO failed (will retry)", "error", nfoErr)
		scheduleResolveRetry(ctx, log, st, item, 60*time.Second, "write RAR provider NFO failed")
		return
	}
	if np := nntpProviders[manifestProviderUsed]; np != nil {
		payloads, subtitleErr := np.ReadDirectSubtitles(ctx, item.ID, parsedNZB,
			[]nntp.SubtitleTarget{{Filename: item.DisplayName, FileID: "nzb-0"}},
			sidecar.DefaultMaxResourceBytes, sidecar.DefaultMaxResourcesPerItem)
		if subtitleErr != nil && ctx.Err() != nil {
			return
		}
		if err := registerNNTPSubtitles(ctx, log, writer, item, payloads); err != nil {
			scheduleResolveRetry(ctx, log, st, item, 60*time.Second, "persist RAR companion subtitles failed")
			return
		}
	}
	item.StrmPath = &strmPath
	item.Cached = true

	log.Info("resolveRAR: manifest built, item ready",
		"parts", len(manifest),
		"total_video_bytes", totalVideoBytes,
		"strm_path", strmPath,
	)
	if err := st.UpdateItemState(ctx, item, store.StateReady, ""); err != nil {
		// BUGFIX 2026-07-01 (torbox API-hit audit): same class -- a
		// persistent failure here would re-fetch RAR headers over NNTP and
		// rebuild the manifest every 3s forever.
		log.Error("resolveRAR: update state", "error", err)
		scheduleResolveRetry(ctx, log, st, item, 30*time.Second, err.Error())
		return
	}
	// ID-01: title-only checks only (no NNTP-lane Facts; see resolveNZBItem's
	// own identical comment).
	if checkIdentity != nil {
		season, episode, _ := identitycheck.ParseSeasonEpisode(item.DisplayName)
		checkIdentity(ctx, item, season, episode, nil)
	}
	// B-N9: debounced arr refresh.
	rn.Notify()
	if np := nntpProviders[manifestProviderUsed]; np != nil {
		runNNTPPrewarm(ctx, log, st, item, nntpExpSvc, np.PrewarmRARSource(ctx, item.ID, contentKey, manifest))
	}
}

func failNestedArchive(ctx context.Context, log *slog.Logger, st *store.Store, item *store.Item, owner string) {
	log.Warn(owner + ": " + nestedArchiveFailure)
	message := nestedArchiveFailure
	item.ErrorMessage = &message
	if err := st.UpdateItemState(ctx, item, store.StateFailed, message); err != nil {
		log.Error("store: persist failed state failed", "item_id", item.ID, "error", err)
	}
}

// classifyNZBContent returns one bounded magic-byte verdict from the existing
// NNTP acquisition path. Subject hints are allowed after a conclusive
// unrecognized result or transient provider failure; ambiguity and cancellation
// abstain rather than guess.
func classifyNZBContent(
	ctx context.Context,
	log *slog.Logger,
	providers map[string]*nntp.NNTPProvider,
	order []string,
	itemID string,
	nzb nntp.NZB,
) (nntp.MagicClassification, bool) {
	unknown := nntp.MagicClassification{Format: archiveparser.FormatUnknown, FileIndex: -1}
	for _, name := range order {
		provider := providers[name]
		if provider == nil {
			continue
		}
		classification, err := provider.ClassifyNZBMagic(ctx, itemID, nzb)
		if err != nil {
			log.Warn("resolveRAR: content classification failed, trying next provider", "provider", name)
			if ctx.Err() != nil {
				return unknown, false
			}
			continue
		}
		if classification.Ambiguous {
			log.Warn("resolveRAR: ambiguous content signatures; refusing subject fallback", "files_inspected", classification.Inspected)
			return classification, false
		}
		if classification.Format != archiveparser.FormatUnknown {
			log.Info("resolveRAR: classified content by magic bytes", "format", classification.Format, "file_index", classification.FileIndex, "files_inspected", classification.Inspected)
			return classification, false
		}
		return classification, true
	}
	return unknown, false
}

func hintNZBArchiveType(files []nntp.NZBFile) archiveparser.Format {
	hasRAR, hasZIP, has7z := false, false, false
	for _, f := range files {
		lower := strings.ToLower(f.Subject)
		if strings.Contains(lower, ".rar") {
			hasRAR = true
		}
		if strings.Contains(lower, ".zip") {
			hasZIP = true
		}
		if strings.Contains(lower, ".7z") {
			has7z = true
		}
	}
	if hasRAR {
		return archiveparser.FormatRAR
	}
	if hasZIP {
		return archiveparser.FormatZIP
	}
	if has7z {
		return archiveparser.Format7z
	}
	return archiveparser.FormatUnknown
}

var nzbVideoExts = []string{".mkv", ".mp4", ".avi", ".ts", ".m2ts", ".webm"}

type nzbVideoSelection struct {
	NZBFileIdx int
	File       nntp.NZBFile
}

// selectNZBVideoFiles preserves the historical subject-substring classifier
// while retaining each file's exact index in ParseNZB's stable size-sorted
// list. PAR2-recovered subjects are quoted real names, so they pass this same
// compatibility path without introducing a second video classifier.
func selectNZBVideoFiles(files []nntp.NZBFile) []nzbVideoSelection {
	var out []nzbVideoSelection
	for idx, file := range files {
		lower := strings.ToLower(file.Subject)
		for _, ext := range nzbVideoExts {
			if strings.Contains(lower, ext) {
				out = append(out, nzbVideoSelection{NZBFileIdx: idx, File: file})
				break
			}
		}
	}
	return out
}

func selectNZBVideoFilesByEvidence(
	files []nntp.NZBFile,
	exactIndexes map[int]bool,
	magic nntp.MagicClassification,
	allowSubjectFallback bool,
) ([]nzbVideoSelection, map[int]bool) {
	if len(exactIndexes) > 0 {
		selected := selectNZBVideoFiles(files)
		out := selected[:0]
		for _, selection := range selected {
			if exactIndexes[selection.NZBFileIdx] {
				out = append(out, selection)
			}
		}
		return out, exactIndexes
	}
	if magic.Format == archiveparser.FormatMKV || magic.Format == archiveparser.FormatMP4 {
		if magic.FileIndex >= 0 && magic.FileIndex < len(files) {
			return []nzbVideoSelection{{NZBFileIdx: magic.FileIndex, File: files[magic.FileIndex]}}, map[int]bool{magic.FileIndex: true}
		}
		return nil, nil
	}
	if magic.Format != archiveparser.FormatUnknown || magic.Ambiguous || !allowSubjectFallback {
		return nil, nil
	}
	return selectNZBVideoFiles(files), nil
}

func isPAR2VideoName(name string) bool {
	lower := strings.ToLower(name)
	for _, ext := range nzbVideoExts {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

func isPAR2RARName(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".rar") {
		return true
	}
	dot := strings.LastIndexByte(lower, '.')
	if dot < 0 || len(lower)-dot != 4 || lower[dot+1] != 'r' {
		return false
	}
	return lower[dot+2] >= '0' && lower[dot+2] <= '9' &&
		lower[dot+3] >= '0' && lower[dot+3] <= '9'
}

// applyPAR2FileMatches enriches only a runtime copy of the parsed NZB. Direct
// videos may be applied independently because every match carries an exact
// original NZB index. RAR names are all-or-nothing: a partial recovered set
// is not allowed to influence archive selection or ordering.
func applyPAR2FileMatches(
	parsedNZB nntp.NZB,
	info archiveparser.PAR2Info,
	matches []nntp.PAR2FileMatch,
) (nntp.NZB, map[int]bool, int, int) {
	enriched := parsedNZB
	enriched.Files = append([]nntp.NZBFile(nil), parsedNZB.Files...)
	exactVideoIndexes := make(map[int]bool)

	requiredRAR := make(map[string]struct{})
	for _, desc := range info.Files {
		if isPAR2RARName(desc.Name) {
			requiredRAR[desc.FileID] = struct{}{}
		}
	}
	matchedRAR := make(map[string]struct{})
	for _, match := range matches {
		if isPAR2RARName(match.Desc.Name) {
			matchedRAR[match.Desc.FileID] = struct{}{}
		}
	}
	fullRARSet := len(requiredRAR) > 0 && len(matchedRAR) == len(requiredRAR)
	if fullRARSet {
		for fileID := range requiredRAR {
			if _, ok := matchedRAR[fileID]; !ok {
				fullRARSet = false
				break
			}
		}
	}

	appliedVideo := 0
	appliedRAR := 0
	for _, match := range matches {
		if match.NZBFileIdx < 0 || match.NZBFileIdx >= len(enriched.Files) {
			continue
		}
		switch {
		case isPAR2VideoName(match.Desc.Name):
			enriched.Files[match.NZBFileIdx].Subject = `"` + match.Desc.Name + `"`
			exactVideoIndexes[match.NZBFileIdx] = true
			appliedVideo++
		case fullRARSet && isPAR2RARName(match.Desc.Name):
			enriched.Files[match.NZBFileIdx].Subject = `"` + match.Desc.Name + `"`
			appliedRAR++
		}
	}
	return enriched, exactVideoIndexes, appliedVideo, appliedRAR
}

// recoverNZBNamesFromPAR2 performs the NS-2.2 orchestration: locate PAR2 by
// subject or bounded magic, parse real metadata, bind names to exact NZB files
// by FileDesc MD5-16k identity, and enrich a runtime-only parsed copy. Any
// missing, ambiguous, partial, or unavailable evidence safely abstains.
func recoverNZBNamesFromPAR2(
	ctx context.Context,
	log *slog.Logger,
	nntpProviders map[string]*nntp.NNTPProvider,
	usenetOrder []string,
	item *store.Item,
	parsedNZB nntp.NZB,
) (nntp.NZB, map[int]bool) {
	for _, name := range usenetOrder {
		np := nntpProviders[name]
		if np == nil {
			continue
		}
		info, par2FileIndex, hasFile, err := np.FetchPAR2ForItem(ctx, item.ID, parsedNZB)
		if err != nil {
			log.Warn("par2: fetch/discovery failed, trying next provider", "provider", name, "error", err)
			continue
		}
		if !hasFile || len(info.Files) == 0 {
			return parsedNZB, nil
		}

		subjects := make([]string, 0, len(parsedNZB.Files))
		for _, file := range parsedNZB.Files {
			subjects = append(subjects, file.Subject)
		}
		recoveryBlocks := archiveparser.PAR2RecoveryBlocksFromSubjects(subjects)
		sourceBlocks := archiveparser.PAR2SourceBlockCount(info, info.SliceSize)
		var density float64
		if sourceBlocks > 0 {
			density = float64(recoveryBlocks) / float64(sourceBlocks)
		}
		log.Info("par2: recovered metadata",
			"event", "par2_metadata_recovered",
			"recovered_files", len(info.Files),
			"recovery_blocks", recoveryBlocks,
			"source_blocks", sourceBlocks,
			"recovery_block_density", density,
		)

		matches, matchErr := np.MatchPAR2FilesForItem(ctx, item.ID, parsedNZB, info, par2FileIndex)
		proofIndex, proofErr := nntp.BuildPAR2ProofIndex(info, matches)
		if proofErr != nil {
			log.Warn("par2: proof index rejected", "provider", name, "error", proofErr)
		} else if proofIndex != nil {
			item.Metadata.NNTPPAR2ProofIndex = proofIndex
			log.Info("par2: IFSC proof index recovered",
				"event", "par2_ifsc_recovered",
				"files", len(proofIndex.Files),
			)
		}
		enriched, exactVideos, appliedVideos, appliedRAR := applyPAR2FileMatches(parsedNZB, info, matches)
		if appliedVideos > 0 || appliedRAR > 0 {
			log.Info("par2: exact file identity applied",
				"event", "par2_identity_applied",
				"video_files", appliedVideos,
				"rar_parts", appliedRAR,
			)
			if matchErr != nil {
				log.Warn("par2: identity scan incomplete after usable exact matches", "provider", name, "error", matchErr)
			}
			return enriched, exactVideos
		}
		if matchErr != nil {
			log.Warn("par2: identity scan incomplete, trying next provider", "provider", name, "error", matchErr)
			continue
		}
		return parsedNZB, nil
	}
	return parsedNZB, nil
}

func safeJoinUnder(root string, elems ...string) (string, error) {
	cleanRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("safe join: root %s: %w", root, err)
	}
	parts := append([]string{cleanRoot}, elems...)
	joined := filepath.Join(parts...)
	cleanJoined, err := filepath.Abs(joined)
	if err != nil {
		return "", fmt.Errorf("safe join: path %s: %w", joined, err)
	}
	rel, err := filepath.Rel(cleanRoot, cleanJoined)
	if err != nil {
		return "", fmt.Errorf("safe join: rel %s: %w", cleanJoined, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("safe join: path %s escapes root %s", cleanJoined, cleanRoot)
	}
	return cleanJoined, nil
}

func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("atomic write: mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("atomic write: create temp in %s: %w", dir, err)
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
		return fmt.Errorf("atomic write: write temp %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("atomic write: chmod temp %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("atomic write: close temp %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("atomic write: rename %s to %s: %w", tmpName, path, err)
	}
	keep = true
	return nil
}

// writeNZBStrm writes the historical filtered-video-index WebDAV URL. Ordinary
// subject-recognizable NZBs keep this byte-identical contract.
func writeNZBStrm(item *store.Item, baseProxyURL string, strmRoot string, fileIndex int, subject string) (strmPathOut string, strmURLOut string, err error) {
	return writeNZBStrmWithExactNZBFile(item, baseProxyURL, strmRoot, fileIndex, subject, nil)
}

// writeNZBStrmExactNZBFile writes a PAR2-recovered direct-video URL carrying
// the exact index in ParseNZB's stable size-sorted file list. The query is
// internal selection metadata; the visible path and recovered filename stay
// compatible with Sonarr and Jellyfin.
func writeNZBStrmExactNZBFile(item *store.Item, baseProxyURL string, strmRoot string, nzbFileIndex int, subject string) (strmPathOut string, strmURLOut string, err error) {
	return writeNZBStrmWithExactNZBFile(item, baseProxyURL, strmRoot, nzbFileIndex, subject, &nzbFileIndex)
}

func writeNZBStrmWithExactNZBFile(item *store.Item, baseProxyURL string, strmRoot string, fileIndex int, subject string, exactNZBFileIndex *int) (strmPathOut string, strmURLOut string, err error) {
	// Strip .nzb suffix from dir name so Sonarr can match episode patterns.
	dirName := item.DisplayName
	if strings.HasSuffix(strings.ToLower(dirName), ".nzb") {
		dirName = dirName[:len(dirName)-4]
	}
	releaseDirName := sanitizeNZBName(dirName)
	if releaseDirName == "." || releaseDirName == ".." {
		releaseDirName = "unknown"
	}

	categoryDir, err := safeJoinUnder(strmRoot, item.Category)
	if err != nil {
		return "", "", fmt.Errorf("writeNZBStrm: category path: %w", err)
	}
	releaseDir, err := safeJoinUnder(categoryDir, releaseDirName)
	if err != nil {
		return "", "", fmt.Errorf("writeNZBStrm: release path: %w", err)
	}

	// Derive episode filename from NZB subject; fall back to displayName
	// when the extracted name looks obfuscated (no SxxExx pattern) or empty.
	name := episodeNameFromSubject(subject)
	if name == "" || !looksLikeEpisodeName(name) {
		name = item.DisplayName
		if strings.HasSuffix(strings.ToLower(name), ".nzb") {
			name = name[:len(name)-4]
		}
		name = sanitizeNZBName(name)
	}
	if name == "." || name == ".." {
		name = "unknown"
	}

	// WebDAV URL: Jellyfin follows this URL, hits the WebDAV handler, probes
	// the virtual .mkv as a local seekable file. Path segments must be escaped
	// independently so spaces, &, %, and other special characters remain valid.
	strmURL := strings.TrimRight(baseProxyURL, "/") + "/dav/" + url.PathEscape(item.Category) + "/" + url.PathEscape(releaseDirName) + "/" + url.PathEscape(name+".mkv")
	if exactNZBFileIndex != nil {
		strmURL += "?nzb_file_index=" + strconv.Itoa(*exactNZBFileIndex)
	}

	strmPath, err := safeJoinUnder(releaseDir, name+".strm")
	if err != nil {
		return "", "", fmt.Errorf("writeNZBStrm: strm path: %w", err)
	}
	if err := atomicWriteFile(strmPath, []byte(strmURL), 0o644); err != nil {
		return "", "", fmt.Errorf("writeNZBStrm: write %s: %w", strmPath, err)
	}
	return strmPath, strmURL, nil
}

// episodeNameFromSubject derives a filesystem-safe episode name from an NZB
// file Subject line. NZB subjects commonly look like:
//
//	[1/25] "Show.S03E01.2160p.WEB-DL.mkv" yEnc (1/512) 2048000
//	Show.S03E01.2160p.WEB-DL.mkv - "file.mkv" yEnc (1/512)
//
// Priority: first quoted string → strip extension → sanitize.
// Fallback: whole subject with [N/N] prefix and yEnc suffix stripped.
func episodeNameFromSubject(subject string) string {
	// NZB subjects frequently HTML-encode quotes as &quot; — unescape first.
	subject = html.UnescapeString(subject)

	// 1. Try first quoted string.
	if first := strings.Index(subject, `"`); first != -1 {
		rest := subject[first+1:]
		if second := strings.Index(rest, `"`); second != -1 {
			name := rest[:second]
			name = stripVideoExtension(name)
			// Reject obfuscated names (no dot, all hex/alnum, short) —
			// indexers sometimes publish with random hash filenames.
			// A valid episode name always contains a dot.
			if strings.Contains(name, ".") {
				name = sanitizeNZBName(name)
				if name != "" {
					return name
				}
			}
		}
	}

	// 2. Fallback: strip [N/N] prefix and " yEnc ..." suffix.
	name := subject
	if end := strings.Index(name, "] "); end != -1 {
		name = name[end+2:]
	}
	// Also handle "- " prefix after [N/N]
	name = strings.TrimPrefix(name, "- ")
	if idx := strings.Index(name, " yEnc"); idx != -1 {
		name = name[:idx]
	}
	name = strings.TrimSpace(name)
	name = stripVideoExtension(name)
	name = sanitizeNZBName(name)
	if name == "" {
		return sanitizeNZBName(subject)
	}
	return name
}

// looksLikeEpisodeName returns true if the name contains an SxxExx pattern
// or at least two dots (typical of proper release names like Show.S01E01.Title).
// Used to reject obfuscated hash filenames from indexers.
func looksLikeEpisodeName(name string) bool {
	lower := strings.ToLower(name)
	// Must contain SxxExx or at least two dots to be a real release name.
	if strings.Count(name, ".") >= 2 {
		return true
	}
	for i := 0; i < len(lower)-4; i++ {
		if lower[i] == 's' && lower[i+1] >= '0' && lower[i+1] <= '9' &&
			lower[i+2] >= '0' && lower[i+2] <= '9' &&
			lower[i+3] == 'e' && lower[i+4] >= '0' && lower[i+4] <= '9' {
			return true
		}
	}
	return false
}

// stripVideoExtension removes a known video file extension (case-insensitive).
func stripVideoExtension(name string) string {
	lower := strings.ToLower(name)
	for _, ext := range []string{".mkv", ".mp4", ".avi", ".ts", ".m2ts", ".webm"} {
		if strings.HasSuffix(lower, ext) {
			return name[:len(name)-len(ext)]
		}
	}
	return name
}

// sanitizeNZBName delegates to the canonical output.SanitizeName (A-B11).
func sanitizeNZBName(s string) string {
	return output.SanitizeName(s)
}

// ---------------------------------------------------------------------------
// ---------------------------------------------------------------------------

// runSealCommand implements `darkharrbor seal --file PATH`: reads env-style
// KEY=VALUE lines from stdin, seals them under a freshly generated random
// password (util.SealSecrets: argon2id + AES-256-GCM), writes the file with
// 0600 permissions, and prints the password EXACTLY ONCE on stdout as
// HARRBOR_SECRETS_PASSWORD=<password> for capture. The password is not
// recoverable afterwards. Secret values are never echoed.
func runSealCommand(args []string) int {
	fs := flag.NewFlagSet("seal", flag.ContinueOnError)
	out := fs.String("file", "", "output path for the sealed secrets file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *out == "" {
		fmt.Fprintln(os.Stderr, "usage: darkharrbor seal --file PATH < secrets.env")
		return 2
	}
	plain, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "seal: read stdin:", err)
		return 1
	}
	count, err := config.ValidateSecretLines(plain)
	if err != nil {
		fmt.Fprintln(os.Stderr, "seal:", err)
		return 1
	}
	if count == 0 {
		fmt.Fprintln(os.Stderr, "seal: no KEY=VALUE lines on stdin")
		return 1
	}
	password, err := util.RandomHex(32)
	if err != nil {
		fmt.Fprintln(os.Stderr, "seal:", err)
		return 1
	}
	blob, err := util.SealSecrets(plain, password)
	if err != nil {
		fmt.Fprintln(os.Stderr, "seal:", err)
		return 1
	}
	if err := securefile.AtomicWrite(filepath.Clean(*out), blob); err != nil {
		fmt.Fprintln(os.Stderr, "seal: write:", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "sealed %d secret line(s) to %s\n", count, *out)
	fmt.Fprintln(os.Stderr, "store the password below now; it will not be shown again and is not recoverable:")
	fmt.Printf("HARRBOR_SECRETS_PASSWORD=%s\n", password)
	return 0
}

// runDeadletterCommand implements `darkharrbor deadletter <list|requeue ITEM_ID|clear>`,
// the operator surface over the stream-time dead-letter (item_stream_failures).
// Dead items short-circuit /stream so a gone release stops hammering the NNTP
// provider; this inspects them, requeues one for a fresh attempt, or clears all.
func runDeadletterCommand(args []string) int {
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "deadletter: load config:", err)
		return 1
	}
	ctx := context.Background()
	db, err := store.Open(ctx, cfg.Database.Path, cfg.Database.BusyTimeout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "deadletter: open db:", err)
		return 1
	}
	defer func() { _ = db.Close() }()
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		fmt.Fprintln(os.Stderr, "deadletter: migrate:", err)
		return 1
	}
	st := store.NewWithSealKey(db, credentialSealKey(cfg))

	switch sub {
	case "list":
		rows, err := st.ListDeadStreams(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "deadletter list:", err)
			return 1
		}
		if len(rows) == 0 {
			fmt.Println("no items in the stream dead-letter")
			return 0
		}
		fmt.Printf("%-40s  %5s  %-25s  %-25s\n", "ITEM_ID", "FAILS", "LAST_FAILED_AT", "DEAD_UNTIL")
		for _, d := range rows {
			fmt.Printf("%-40s  %5d  %-25s  %-25s\n", d.ItemID, d.FailCount, d.LastFailedAt.Format(time.RFC3339), d.DeadUntil.Format(time.RFC3339))
		}
		return 0
	case "requeue":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: darkharrbor deadletter requeue ITEM_ID")
			return 2
		}
		id := args[1]
		if err := st.ResetStreamFailure(ctx, id); err != nil {
			fmt.Fprintln(os.Stderr, "deadletter requeue:", err)
			return 1
		}
		fmt.Printf("requeued %s: strikes cleared; the next play retries NNTP\n", id)
		return 0
	case "clear":
		n, err := st.ClearStreamFailures(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "deadletter clear:", err)
			return 1
		}
		fmt.Printf("cleared %d dead-letter row(s)\n", n)
		return 0
	default:
		fmt.Fprintln(os.Stderr, "usage: darkharrbor deadletter <list|requeue ITEM_ID|clear>")
		return 2
	}
}

// nntpProviderRetentionDays reads only configured provider horizons. Missing,
// malformed, zero, or excessive values safely abstain from retention routing;
// the raw value is never logged. Thin wrapper: config.ProviderRetentionDays
// (internal/config/retention.go) is the single shared owner of this parsing,
// also reused by NS-9.1's search-time retention annotation.
func nntpProviderRetentionDays(cfg *config.Config, providerNames []string, log *slog.Logger) map[string]int {
	return config.ProviderRetentionDays(cfg, providerNames, log)
}

// a constructed NNTP provider; closers are appended for shutdown.
// nntpReadaheadCeiling (NS-5.5) is the single source of truth for the NNTP
// lane's readahead worker ceiling -- the largest configured provider
// readahead pool. Shared by rangecacheConfigFromCfg (the session goroutine
// count) and nntpExpSvc's MaxReadaheadWorkers (the adaptive recommendation's
// own ceiling), so the two can never disagree about what "full width" means.
func nntpReadaheadCeiling(cfg *config.Config) int {
	raWorkers := cfg.Cache.ReadaheadConnections
	for _, providerName := range cfg.Providers.UsenetProviders {
		key := "HARRBOR_PROVIDER_" + strings.ToUpper(providerName) + "_READAHEAD_CONNECTIONS"
		if v, ok := cfg.Lookup(key); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > raWorkers {
				raWorkers = n
			}
		}
	}
	return raWorkers
}

// rangecacheConfigFromCfg maps the Dark Harrbor cache config onto the shared
// rangecache.Config (connection counts are NNTP-only and stay out of it).
func rangecacheConfigFromCfg(cfg *config.Config) rangecache.Config {
	// The rangecache is shared by all NNTP providers, so size its worker pool
	// to the largest configured provider readahead pool.
	raWorkers := nntpReadaheadCeiling(cfg)
	return rangecache.Config{
		ReadaheadEnabled:     cfg.Cache.ReadaheadEnabled,
		ReadaheadMaxSegments: cfg.Cache.ReadaheadMaxSegments,
		ReadaheadWorkers:     raWorkers,
		MinBufferSegments:    cfg.Cache.MinBufferSegments,
		TailEvict:            cfg.Cache.TailEvict,
		TailLookback:         cfg.Cache.TailLookback,
		DiskCacheSizeMB:      cfg.Cache.DiskCacheSizeMB,
		DiskCacheTTLMin:      cfg.Cache.DiskCacheTTLMin,
		DiskCachePath:        cfg.Cache.DiskCachePath,
		FullEvictMode:        cfg.Cache.FullEvictMode,
		FullEvictTTLMin:      cfg.Cache.FullEvictTTLMin,
		PinnedBudgetMB:       cfg.Cache.PinnedBudgetMB,
		// NS-5.5: bounded seek-triggered widen window; <=0 (default off
		// until HARRBOR_READAHEAD_SEEK_WIDEN_MS is set) means activeLimit
		// stays pinned at the ceiling always -- zero behavior change.
		SeekWidenDuration: time.Duration(cfg.Cache.ReadaheadSeekWidenMS) * time.Millisecond,
	}
}

// attachSegmentCache wires the NNTP segment cache onto a constructed provider
// mode "none" routes through the shared rangecache as a sequential demand
// fetch (a timing change, never a semantic one), so the /stream and /dav
// paths share one code path across all four modes. The NNTP path's effective
// mode honors HARRBOR_NNTP_CACHE_MODE when set, else HARRBOR_CACHE_MODE.
// The shared rangecache instance rc is passed in; the SegmentCache owns only
// the NNTP connection pools (its Close closes pools, not rc).
func attachSegmentCache(cfg *config.Config, providerName string, ncfg nntp.Config, p *nntp.NNTPProvider, rc *rangecache.Cache, offsets nntp.SegmentOffsetStore, expSvc *experience.Service, log *slog.Logger, closers *[]func()) {
	mode := cfg.Cache.Mode
	if cfg.Cache.NNTPMode != "" {
		mode = cfg.Cache.NNTPMode
	}
	nntpCfg := nntp.Config{
		Host:          ncfg.Host,
		Port:          ncfg.Port,
		TLS:           ncfg.TLS,
		Username:      ncfg.Username,
		Password:      ncfg.Password,
		SkipCRC:       ncfg.SkipCRC,
		WarmConns:     ncfg.WarmConns,
		PipelineDepth: ncfg.PipelineDepth,
	}
	cacheCfg := nntp.CacheConfig{
		Mode:                 nntp.CacheMode(mode),
		TotalConnections:     cfg.Cache.TotalConnections,
		DemandConnections:    cfg.Cache.DemandConnections,
		ReadaheadConnections: cfg.Cache.ReadaheadConnections,
		ReadaheadEnabled:     cfg.Cache.ReadaheadEnabled,
		ReadaheadMaxSegments: cfg.Cache.ReadaheadMaxSegments,
		MinBufferSegments:    cfg.Cache.MinBufferSegments,
		TailEvict:            cfg.Cache.TailEvict,
		TailLookback:         cfg.Cache.TailLookback,
		DiskCacheSizeMB:      cfg.Cache.DiskCacheSizeMB,
		DiskCacheTTLMin:      cfg.Cache.DiskCacheTTLMin,
		DiskCachePath:        cfg.Cache.DiskCachePath,
		FullEvictMode:        cfg.Cache.FullEvictMode,
		FullEvictTTLMin:      cfg.Cache.FullEvictTTLMin,
	}
	// Per-provider connection overrides — HARRBOR_PROVIDER_<NAME>_* take
	// precedence over global HARRBOR_NNTP_* so each provider (newshosting,
	// torbox) can be tuned independently.
	provPfx := "HARRBOR_PROVIDER_" + strings.ToUpper(providerName) + "_"
	provTotalExplicit := hasConfigOverride(cfg, provPfx+"TOTAL_CONNECTIONS")
	provDemandExplicit := hasConfigOverride(cfg, provPfx+"DEMAND_CONNECTIONS")
	provReadaheadExplicit := hasConfigOverride(cfg, provPfx+"READAHEAD_CONNECTIONS")
	if v, ok := cfg.Lookup(provPfx + "TOTAL_CONNECTIONS"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			cacheCfg.TotalConnections = n
		}
	}
	if v, ok := cfg.Lookup(provPfx + "DEMAND_CONNECTIONS"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			cacheCfg.DemandConnections = n
		}
	}
	if v, ok := cfg.Lookup(provPfx + "READAHEAD_CONNECTIONS"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			cacheCfg.ReadaheadConnections = n
		}
	}
	if v, ok := cfg.Lookup(provPfx + "MAX_CONNS_PER_FILE"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			cacheCfg.MaxDemandConnsPerFile = n
		}
	}
	cacheCfg = nntp.NormalizeCacheConnections(cacheCfg, ncfg.Connections, nntp.CacheConnectionOverrides{
		TotalExplicit:     hasConfigOverride(cfg, "HARRBOR_NNTP_TOTAL_CONNECTIONS") || provTotalExplicit,
		DemandExplicit:    hasConfigOverride(cfg, "HARRBOR_NNTP_DEMAND_CONNECTIONS") || provDemandExplicit,
		ReadaheadExplicit: hasConfigOverride(cfg, "HARRBOR_NNTP_READAHEAD_CONNECTIONS") || provReadaheadExplicit,
	}, log)
	segCache := nntp.NewSegmentCache(cacheCfg, nntpCfg, rc, log)
	segCache.SetOffsetStore(offsets)
	// NS-5.5: wire the shared experience.Service so StreamWithCache/
	// StreamRARWithCache/StreamZIPEntry can consult an adaptive steady
	// readahead-worker recommendation. nil-safe: an unconfigured expSvc
	// (cfg.Prewarm.Enabled false) leaves every stream at the existing
	// static ReadaheadConnections default, unchanged from pre-NS-5.5.
	segCache.SetExperience(expSvc)
	*closers = append(*closers, segCache.Close)
	p.SetCache(segCache)
	log.Info("nntp segment cache initialized",
		"provider", p.Name(),
		"mode", mode,
		"total_conns", cacheCfg.TotalConnections,
		"demand_conns", cacheCfg.DemandConnections,
		"readahead_conns", cacheCfg.ReadaheadConnections,
		"readahead_enabled", cfg.Cache.ReadaheadEnabled,
		"disk_cache_path", cfg.Cache.DiskCachePath,
	)
}

func hasConfigOverride(cfg *config.Config, key string) bool {
	_, ok := cfg.Lookup(key)
	return ok
}

// nntpConfigFromLookup builds an nntp.Config for an additional named usenet
// provider from HARRBOR_PROVIDER_<NAME>_{HOST,PORT,TLS,USERNAME,PASSWORD,
// CONNECTIONS}, resolved sealed-secrets-first (scenario U-1).
func nntpConfigFromLookup(bc provider.BuildContext) (nntp.Config, error) {
	prefix := "HARRBOR_PROVIDER_" + strings.ToUpper(bc.Name) + "_"
	host, ok := bc.Lookup(prefix + "HOST")
	if !ok {
		return nntp.Config{}, fmt.Errorf("%w: %sHOST not set", provider.ErrNotConfigured, prefix)
	}
	ncfg := nntp.Config{Host: host, Port: 563, TLS: true, Connections: 8}
	if v, ok := bc.Lookup(prefix + "PORT"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			ncfg.Port = n
		}
	}
	if v, ok := bc.Lookup(prefix + "TLS"); ok {
		ncfg.TLS = parseBoolDefault(v, true)
	}
	if v, ok := bc.Lookup(prefix + "USERNAME"); ok {
		ncfg.Username = v
	}
	if v, ok := bc.Lookup(prefix + "PASSWORD"); ok {
		ncfg.Password = v
	}
	if v, ok := bc.Lookup(prefix + "CONNECTIONS"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			ncfg.Connections = n
		}
	}
	return ncfg, nil
}

func parseBoolDefault(v string, def bool) bool {
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return b
}

// lookupInt resolves an integer configuration value through cfg.Lookup
// (sealed secrets first, then env); absent or malformed yields 0.
func lookupInt(cfg *config.Config, key string) int {
	if v, ok := cfg.Lookup(key); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return 0
}

// lookupInt64 is lookupInt for int64 values (plan max bytes).
func lookupInt64(cfg *config.Config, key string) int64 {
	if v, ok := cfg.Lookup(key); ok {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			return n
		}
	}
	return 0
}

// runRestoreCommand restores the newest complete DR-01 snapshot. The daemon
// lock makes the command fail closed unless the normal service is stopped.
func runRestoreCommand(args []string) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	source := fs.String("from", "/backup", "snapshot directory or parent containing snapshots")
	configDir := fs.String("config", "/config", "DarkHarrbor config directory to restore")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	name, err := backup.Restore(context.Background(), *source, *configDir)
	if err != nil {
		if errors.Is(err, backup.ErrDaemonRunning) {
			fmt.Fprintln(os.Stderr, "restore: DarkHarrbor must be stopped before restore")
		} else {
			fmt.Fprintln(os.Stderr, "restore: failed")
		}
		return 1
	}
	fmt.Fprintf(os.Stdout, "Restore complete: snapshot=%s\n", name)
	return 0
}

// runBackupCommand creates local snapshots or transfers authenticated encrypted
// backup bundles. Recovery keys are loaded from the sealed config or a private
// file, never a flag value, so they do not enter shell history or process
// listings.
func runBackupCommand(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: darkharrbor backup list|verify|snapshot|export|import [flags]")
		return 2
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("backup list", flag.ContinueOnError)
		source := fs.String("from", "/backup", "snapshot parent directory")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if fs.NArg() != 0 {
			fmt.Fprintln(os.Stderr, "backup list: no positional arguments are accepted")
			return 2
		}
		names, err := backup.ListSnapshotNames(*source)
		if err != nil {
			fmt.Fprintln(os.Stderr, "backup list: unavailable")
			return 1
		}
		for _, name := range names {
			fmt.Fprintln(os.Stdout, name)
		}
		return 0

	case "verify":
		fs := flag.NewFlagSet("backup verify", flag.ContinueOnError)
		source := fs.String("from", "", "exact snapshot directory (required)")
		defaultKeyFile := strings.TrimSpace(os.Getenv("HARRBOR_SECRETS_KEY_FILE"))
		keyFile := fs.String("key-file", defaultKeyFile, "private recovery-key file; defaults to HARRBOR_SECRETS_KEY_FILE")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if fs.NArg() != 0 || strings.TrimSpace(*source) == "" || strings.TrimSpace(*keyFile) == "" {
			fmt.Fprintln(os.Stderr, "backup verify: --from and a private --key-file are required")
			return 2
		}
		snapshot, err := backup.VerifySnapshot(context.Background(), *source)
		if err != nil || config.VerifySealedSecretsKey(filepath.Join(snapshot, "secrets.sealed"), *keyFile) != nil {
			fmt.Fprintln(os.Stderr, "backup verify: failed")
			return 1
		}
		fmt.Fprintln(os.Stdout, "Backup restore preflight: OK")
		return 0

	case "snapshot":
		fs := flag.NewFlagSet("backup snapshot", flag.ContinueOnError)
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if fs.NArg() != 0 {
			fmt.Fprintln(os.Stderr, "backup snapshot: no arguments are accepted")
			return 2
		}
		cfg, err := config.Load()
		if err != nil {
			fmt.Fprintln(os.Stderr, "backup snapshot: configuration unavailable")
			return 1
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		name, err := createBackupSnapshot(ctx, cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "backup snapshot: failed")
			return 1
		}
		fmt.Fprintf(os.Stdout, "Backup snapshot complete: snapshot=%s\n", name)
		return 0

	case "export":
		fs := flag.NewFlagSet("backup export", flag.ContinueOnError)
		source := fs.String("from", "/backup", "snapshot directory or parent containing snapshots")
		target := fs.String("to", "", "private directory for the encrypted bundle (required)")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if strings.TrimSpace(*target) == "" {
			fmt.Fprintln(os.Stderr, "backup export: --to is required")
			return 2
		}
		cfg, err := config.Load()
		if err != nil {
			fmt.Fprintln(os.Stderr, "backup export: configuration unavailable")
			return 1
		}
		key, ok := cfg.Secrets.SealKey()
		if !ok {
			fmt.Fprintln(os.Stderr, "backup export: sealed-store recovery key unavailable")
			return 1
		}
		name, err := backup.Export(context.Background(), *source, *target, key)
		if err != nil {
			fmt.Fprintln(os.Stderr, "backup export: failed")
			return 1
		}
		fmt.Fprintf(os.Stdout, "Encrypted backup export complete: %s\n", filepath.Join(*target, name))
		return 0

	case "import":
		fs := flag.NewFlagSet("backup import", flag.ContinueOnError)
		file := fs.String("file", "", "encrypted .dhbackup file (required)")
		target := fs.String("to", "/backup", "private directory for the imported snapshot")
		defaultKeyFile := strings.TrimSpace(os.Getenv("HARRBOR_SECRETS_KEY_FILE"))
		keyFile := fs.String("key-file", defaultKeyFile, "private recovery-key file; defaults to HARRBOR_SECRETS_KEY_FILE")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if strings.TrimSpace(*file) == "" || strings.TrimSpace(*keyFile) == "" {
			fmt.Fprintln(os.Stderr, "backup import: --file and a private --key-file are required")
			return 2
		}
		rawKey, err := securefile.Read(*keyFile, 4096)
		if err != nil || strings.TrimSpace(string(rawKey)) == "" {
			fmt.Fprintln(os.Stderr, "backup import: recovery key file unavailable or unsafe")
			return 1
		}
		name, err := backup.Import(context.Background(), *file, *target, strings.TrimSpace(string(rawKey)))
		clear(rawKey)
		if err != nil {
			fmt.Fprintln(os.Stderr, "backup import: failed")
			return 1
		}
		fmt.Fprintf(os.Stdout, "Encrypted backup import complete: snapshot=%s\n", name)
		return 0

	default:
		fmt.Fprintln(os.Stderr, "usage: darkharrbor backup list|verify|snapshot|export|import [flags]")
		return 2
	}
}

func createBackupSnapshot(ctx context.Context, cfg *config.Config) (string, error) {
	if cfg == nil {
		return "", errors.New("backup configuration is required")
	}
	databasePath := filepath.Clean(strings.TrimSpace(cfg.Database.Path))
	info, err := os.Lstat(databasePath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("backup database is unavailable")
	}
	db, err := store.Open(ctx, databasePath, cfg.Database.BusyTimeout)
	if err != nil {
		return "", errors.New("open backup database")
	}
	defer func() { _ = db.Close() }()
	service := backup.New(db, backup.Config{
		Target:      cfg.Backup.Target,
		SecretsPath: cfg.Backup.SecretsPath,
		Interval:    time.Hour,
		Keep:        cfg.Backup.Keep,
	}, nil)
	return service.Create(ctx)
}

// runReAdoptCommand audits permanent DH .strm pointers against existing
// SQLite authority. It is deliberately read-only: a pointer carries too
// little source identity to fabricate a playable missing item safely.
func runReAdoptCommand(args []string) int {
	fs := flag.NewFlagSet("readopt", flag.ContinueOnError)
	library := fs.String("library", "", "library root to scan")
	database := fs.String("database", "/config/darkharrbor.db", "DarkHarrbor database")
	maxEntries := fs.Int("max-entries", 100_000, "maximum filesystem entries and database rows")
	timeout := fs.Duration("timeout", 5*time.Minute, "whole-scan timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *library == "" || *maxEntries <= 0 || *maxEntries > store.MaxReAdoptionEntries ||
		*timeout <= 0 || *timeout > time.Hour {
		fmt.Fprintln(os.Stderr, "readopt: invalid library, limit, or timeout")
		return 2
	}
	dbInfo, err := os.Lstat(*database)
	if err != nil || !dbInfo.Mode().IsRegular() || dbInfo.Mode()&os.ModeSymlink != 0 {
		fmt.Fprintln(os.Stderr, "readopt: database must be an existing regular file")
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	db, err := store.OpenReadOnly(ctx, *database, 5*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "readopt: database open failed")
		return 1
	}
	defer func() { _ = db.Close() }()
	// No config in scope; this path does not read provider_credentials. A
	// sealed row reached from here would fail CLOSED with a named error
	// rather than returning ciphertext (SEC-01).
	res, err := store.New(db).ReAdoptionScan(ctx, *library, *maxEntries)
	if err != nil {
		fmt.Fprintln(os.Stderr, "readopt: scan failed")
		return 1
	}
	fmt.Fprintf(os.Stdout, "readopt: entries=%d strm=%d verified=%d orphan=%d ghost=%d partial=%d malformed=%d ambiguous=%d\n",
		res.Entries, res.StrmFiles, res.Verified, res.Orphans, res.Ghosts, res.Partial, res.Malformed, res.Ambiguous)
	for _, finding := range res.Findings {
		fmt.Fprintf(os.Stdout, "readopt: finding=%s item_id=%s path=%s offered_fix=%s\n",
			finding.Kind, finding.ItemID, finding.Path, finding.Fix)
	}
	return 0
}

// runSetupCommand implements `darkharrbor setup [--config DIR] [--answers FILE] [--handoff-dir DIR] [--force]`:
// goal-first bootstrap wizard. The wizard opens the database only after the
// operator reviews and approves the plan, then persists per-step state.
func runSetupCommand(args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	cfgDir := fs.String("config", "/config", "path to DarkHarrbor config directory (bind-mount of darkharrbor-config volume)")
	answersFile := fs.String("answers", "", "headless replay: private mode-0600 JSON response transcript")
	handoffDir := fs.String("handoff-dir", "", "private directory for one-time recovery/operator files instead of terminal display")
	force := fs.Bool("force", false, "force reconfiguration even when bootstrap_complete is set")
	deferArrRegistration := fs.Bool("defer-arr-registration", false, "defer Arr client/indexer registration until DarkHarrbor is running")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	opts := wizard.Options{
		ConfigDir:            *cfgDir,
		DBPath:               *cfgDir + "/darkharrbor.db",
		AnswersFile:          *answersFile,
		HandoffDir:           *handoffDir,
		DockerProxyURL:       strings.TrimSpace(os.Getenv("HARRBOR_DOCKER_PROXY_URL")),
		ForceReconfigure:     *force,
		DeferArrRegistration: *deferArrRegistration,
	}
	if err := wizard.Run(context.Background(), opts); err != nil {
		fmt.Fprintf(os.Stderr, "setup: %v\n", err)
		return 1
	}
	return 0
}

// runDemoCommand implements `darkharrbor demo [--config DIR] [--timeout SECONDS]
// [--yes] [--no-cleanup]`. Standalone, safely re-runnable equivalent of the
// wizard's S12 zero-account demo step (ONB-01) -- it never touches S1-S11,
// so it can be run anytime against an already-bootstrapped install without
// re-registering arrs, re-sealing secrets, or re-running shim/Jellyfin setup.
func runDemoCommand(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	cfgDir := fs.String("config", "/config", "path to DarkHarrbor config directory (bind-mount of darkharrbor-config volume)")
	timeoutSeconds := fs.Int("timeout", 0, "bounded wait for grab/import in seconds (0 = HARRBOR_DEMO_TIMEOUT_SECONDS or default)")
	autoYes := fs.Bool("yes", false, "automatically confirm cleanup of any item the demo itself added")
	noCleanup := fs.Bool("no-cleanup", false, "never remove a demo-added item, even if verification succeeds")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *autoYes && *noCleanup {
		fmt.Fprintln(os.Stderr, "demo: --yes and --no-cleanup are mutually exclusive")
		return 2
	}

	dbPath := *cfgDir + "/darkharrbor.db"
	ctx := context.Background()

	db, err := store.Open(ctx, dbPath, 5*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo: open database %s: %v\n", dbPath, err)
		return 1
	}
	defer func() { _ = db.Close() }()

	// No config in scope; this path does not read provider_credentials. A
	// sealed row reached from here would fail CLOSED with a named error
	// rather than returning ciphertext (SEC-01).
	instances, err := store.New(db).ListArrInstances(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo: list arr instances: %v\n", err)
		return 1
	}
	var radarr *store.ArrInstance
	for i := range instances {
		if instances[i].AppType == "radarr" {
			radarr = &instances[i]
			break
		}
	}
	if radarr == nil {
		fmt.Fprintln(os.Stderr, "demo: no Radarr instance found — run `darkharrbor setup` first")
		return 1
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo: load config: %v\n", err)
		return 1
	}
	apiKey, ok := cfg.Lookup(radarr.APIKeyRef)
	if !ok || strings.TrimSpace(apiKey) == "" {
		fmt.Fprintf(os.Stderr, "demo: could not resolve %s's API key (%s) from sealed secrets or environment\n", radarr.Name, radarr.APIKeyRef)
		return 1
	}

	// WZ-02. The demo GRABS AND PLAYS a real public-domain movie, so a Radarr
	// alone is not enough — it also needs an acquisition lane able to fetch it.
	// Without this the command reached the grab and failed with an acquisition
	// error that named nothing about the missing lane. The equivalent guard in
	// wizard S12 is unreachable, because the S5 acquisition-lane rule already
	// prevents a lane-less deployment from completing setup; THIS is the path
	// that can genuinely be run on one.
	if !demoAcquisitionConfigured(cfg) {
		fmt.Fprintln(os.Stderr, "demo: no acquisition lane is configured, so nothing could be fetched.")
		fmt.Fprintln(os.Stderr, "      Configure an HTTP backend, debrid provider, or NNTP provider with `darkharrbor setup`, then re-run.")
		return 1
	}

	timeout := time.Duration(*timeoutSeconds) * time.Second
	if *timeoutSeconds <= 0 {
		timeout = wizard.DemoTimeoutFromEnv(os.Stdout)
	}

	cleanupDecision := func(title string) bool {
		if *noCleanup {
			return false
		}
		if *autoYes {
			return true
		}
		fmt.Fprintf(os.Stdout, "Remove demo item %q now? [Y/n]: ", title)
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		line = strings.ToLower(strings.TrimSpace(line))
		return line == "" || line == "y" || line == "yes"
	}

	if err := wizard.RunDemo(ctx, os.Stdout, radarr.Name, radarr.URL, util.Redacted(apiKey), cfg.Data.Root, timeout, cleanupDecision); err != nil {
		fmt.Fprintf(os.Stderr, "demo: %v\n", err)
		return 1
	}
	return 0
}

func demoAcquisitionConfigured(cfg *config.Config) bool {
	return len(cfg.Routing.Preference) > 0 || (cfg.HTTPStream.Enabled && len(cfg.HTTPStream.Backends) > 0)
}

// runDoctorCommand implements `darkharrbor doctor [--config DIR] [--json]`
// (ONB-02). Standalone, read-only, safely re-runnable diagnostics sweep --
// never mutates Arr, provider, or DarkHarrbor state. Independent of S1-S11
// like `demo`, so it can be run anytime against an already-bootstrapped
// install. No live accountgov.Governor exists in this standalone process,
// so provider dial tests run ungoverned here (disclosed scope limit; the
// GET /api/v1/doctor endpoint governs them through the real running
// server's own governors instead -- see internal/api/doctor_handler.go).
func runDoctorCommand(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	cfgDir := fs.String("config", "/config", "path to DarkHarrbor config directory (bind-mount of darkharrbor-config volume)")
	asJSON := fs.Bool("json", false, "print the full report as JSON instead of a human-readable summary")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	dbPath := *cfgDir + "/darkharrbor.db"
	ctx := context.Background()

	db, err := store.Open(ctx, dbPath, 5*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doctor: open database %s: %v\n", dbPath, err)
		return 1
	}
	defer func() { _ = db.Close() }()

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "doctor: load config: %v\n", err)
		return 1
	}

	timeout := doctor.TimeoutFromEnv(func(msg string) { fmt.Fprintf(os.Stderr, "doctor: %s\n", msg) })
	report, err := doctor.Run(ctx, doctor.Options{
		DB:             db,
		Cfg:            cfg,
		DockerProxyURL: strings.TrimSpace(os.Getenv("HARRBOR_DOCKER_PROXY_URL")),
	}, timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doctor: %v\n", err)
		return 1
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintf(os.Stderr, "doctor: encode report: %v\n", err)
			return 1
		}
	} else {
		printDoctorReport(os.Stdout, report)
	}

	if !report.OK() {
		return 1
	}
	return 0
}

func runReactiveCommand(command string, args []string) int {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	representation := fs.String("representation", "", "opaque playback representation ID")
	mode := fs.String("mode", "supervised", "explicit commit mode: supervised or auto")
	target := fs.String("arr", "", "explicit Arr target required only when content is absent")
	rootFolder := fs.String("root-folder", "", "explicit Arr root required only when content is absent")
	qualityProfile := fs.Int("quality-profile", 0, "explicit Arr quality profile required only when content is absent")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*representation) == "" {
		fmt.Fprintln(os.Stderr, "reactive: representation is required")
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "reactive: configuration unavailable")
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	db, err := store.Open(ctx, cfg.Database.Path, cfg.Database.BusyTimeout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reactive: database unavailable")
		return 1
	}
	defer func() { _ = db.Close() }()
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		fmt.Fprintln(os.Stderr, "reactive: migration failed")
		return 1
	}
	st := store.NewWithSealKey(db, credentialSealKey(cfg))
	writer := output.NewStrmWriter(cfg.Data.Strm, filepath.Join(cfg.Data.Root, ".staging"))
	if cfg.Stub.Authority == "db" {
		writer.SetBlobSink(st, cfg.Data.Root)
	}
	service, err := reactivecommit.New(st, reactivecommit.NewArrClient(cfg.Arrs), writer, reactivecommit.Options{
		MonitorEpisodes: cfg.Reactive.MonitorEpisodes, MonitorMovie: cfg.Reactive.MonitorMovies,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "reactive: service unavailable")
		return 1
	}
	if command == "reactive-undo" {
		if err := service.Undo(ctx, *representation); err != nil {
			fmt.Fprintln(os.Stderr, "reactive: undo failed")
			return 1
		}
		fmt.Fprintln(os.Stdout, "state=undone")
		return 0
	}
	result, err := service.Commit(ctx, reactivecommit.Request{
		RepresentationID: *representation, Mode: strings.ToLower(strings.TrimSpace(*mode)),
		TargetName: strings.TrimSpace(*target), RootFolder: strings.TrimSpace(*rootFolder), QualityProfile: *qualityProfile,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "reactive: commit failed")
		return 1
	}
	fmt.Fprintf(os.Stdout, "state=%s review_required=%t reason=%s\n", result.State, result.ReviewRequired, result.Reason)
	return 0
}

// reactivePendingIdentity resolves the human-readable identity of a parked
// entry for display. It NEVER guesses: a missing item, missing metadata, or
// missing ProviderIdentity yields "<unknown>" so an operator can tell the
// difference between "this is a 1972 movie" and "DarkHarrbor does not know
// what this is" -- the second is itself the useful signal, and a fabricated
// title would make an unroutable entry look routable.
func reactivePendingIdentity(ctx context.Context, st *store.Store, itemID string) (title, kind, year string) {
	title, kind, year = "<unknown>", "<unknown>", "-"
	if st == nil || strings.TrimSpace(itemID) == "" {
		return title, kind, year
	}
	item, err := st.GetItemByID(ctx, itemID)
	if err != nil || item == nil || item.Metadata.ProviderIdentity == nil {
		return title, kind, year
	}
	identity := item.Metadata.ProviderIdentity
	if value := strings.TrimSpace(identity.Title); value != "" {
		title = value
	}
	if value := strings.TrimSpace(identity.Kind); value != "" {
		kind = value
	}
	if identity.Year > 0 {
		year = strconv.Itoa(identity.Year)
	}
	return title, kind, year
}

// printCLIUsage is the single discoverable index of every subcommand. Without
// it the CLI could only be learned by reading main.go, which is not a
// reasonable ask of an operator whose reactive queue has parked something.
// fetchArrDestinationOptions reads an Arr's own root folders and quality
// profiles. The wizard has an equivalent private reader for S7d; this is the
// read-only CLI counterpart. Ordering of profiles is deterministic by id so
// repeated runs are diffable.
func fetchArrDestinationOptions(ctx context.Context, target config.ArrTarget) ([]string, map[int]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	get := func(path string, out any) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(target.BaseURL, "/")+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("X-Api-Key", target.APIKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return json.NewDecoder(resp.Body).Decode(out)
	}
	var rootRows []struct {
		Path string `json:"path"`
	}
	if err := get("/api/v3/rootfolder", &rootRows); err != nil {
		return nil, nil, fmt.Errorf("fetch root folders: %w", err)
	}
	var profileRows []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	if err := get("/api/v3/qualityprofile", &profileRows); err != nil {
		return nil, nil, fmt.Errorf("fetch quality profiles: %w", err)
	}
	var roots []string
	for _, row := range rootRows {
		if value := strings.TrimSpace(row.Path); value != "" {
			roots = append(roots, value)
		}
	}
	profiles := make(map[int]string, len(profileRows))
	for _, row := range profileRows {
		if row.ID > 0 && strings.TrimSpace(row.Name) != "" {
			profiles[row.ID] = strings.TrimSpace(row.Name)
		}
	}
	return roots, profiles, nil
}

// credentialSealKey returns the seal key for provider credentials at rest
// (SEC-01), or "" when sealed secrets are not configured. Centralised so a
// call site cannot half-remember the config path.
func credentialSealKey(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	key, _ := cfg.Secrets.SealKey()
	return key
}

func printCLIUsage(w io.Writer) {
	fmt.Fprintln(w, "DarkHarrbor -- media automation bridge")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "usage: darkharrbor <command> [flags]")
	fmt.Fprintln(w, "       darkharrbor            (no command: run the daemon)")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Setup and configuration")
	fmt.Fprintln(w, "  setup                 Interactive bootstrap wizard (S1-S12); --force rotates credentials")
	fmt.Fprintln(w, "  catalog import        Validate and atomically install a private generic-source catalog from stdin")
	fmt.Fprintln(w, "  configure             Runtime editor; --existing-aiostreams connects an existing stack")
	fmt.Fprintln(w, "  seal                  Seal KEY=VALUE lines from stdin into an encrypted secrets file")
	fmt.Fprintln(w, "  reconcile-arr         Re-apply DarkHarrbor client/indexer registration to the Arrs")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Diagnostics")
	fmt.Fprintln(w, "  doctor                Full health report; -json for machine output. Exit 1 on any FAIL")
	fmt.Fprintln(w, "  demo                  Zero-account demo: grab and play one public-domain movie")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Reactive library")
	fmt.Fprintln(w, "  reactive-pending      List and decide parked entries (-action list|destinations|")
	fmt.Fprintln(w, "                        approve|assign|jellyfin-only|remove|acknowledge|undo)")
	fmt.Fprintln(w, "  reactive-commit       Commit one representation directly")
	fmt.Fprintln(w, "  reactive-undo         Undo a previous reactive commit")
	fmt.Fprintln(w, "  reactive-promote      Promote a representation to a materialized path, or remove it")
	fmt.Fprintln(w, "  monitor-flip          Flip Arr monitoring for committed content")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Maintenance")
	fmt.Fprintln(w, "  backup                List/create snapshots, or export/import encrypted backup bundles")
	fmt.Fprintln(w, "  restore               Restore from a DarkHarrbor backup")
	fmt.Fprintln(w, "  readopt               Audit existing .strm pointers against restored state")
	fmt.Fprintln(w, "  deadletter            Inspect the dead-letter queue (list|requeue ITEM_ID|clear)")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Run `darkharrbor <command> -h` for that command's flags.")
}

func runReactivePendingCommand(args []string) int {
	fs := flag.NewFlagSet("reactive-pending", flag.ContinueOnError)
	action := fs.String("action", "list", "list, destinations, approve, assign, jellyfin-only, remove, acknowledge, or undo")
	representation := fs.String("representation", "", "opaque playback representation ID")
	targetName := fs.String("arr", "", "explicit Arr target for assignment")
	rootFolder := fs.String("root-folder", "", "Arr root required when adding absent content")
	qualityProfile := fs.Int("quality-profile", 0, "Arr quality profile required when adding absent content")
	after := fs.String("after", "", "opaque representation cursor")
	limit := fs.Int("limit", 25, "bounded page size (1-100)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "reactive-pending: configuration unavailable: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	db, err := store.Open(ctx, cfg.Database.Path, cfg.Database.BusyTimeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reactive-pending: database unavailable: %v\n", err)
		return 1
	}
	defer func() { _ = db.Close() }()
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		fmt.Fprintf(os.Stderr, "reactive-pending: migration failed: %v\n", err)
		return 1
	}
	st := store.NewWithSealKey(db, credentialSealKey(cfg))
	writer := output.NewStrmWriter(cfg.Data.Strm, filepath.Join(cfg.Data.Root, ".staging"))
	if cfg.Stub.Authority == "db" {
		writer.SetBlobSink(st, cfg.Data.Root)
	}
	commitService, err := reactivecommit.New(st, reactivecommit.NewArrClient(cfg.Arrs), writer, reactivecommit.Options{
		MonitorEpisodes: cfg.Reactive.MonitorEpisodes, MonitorMovie: cfg.Reactive.MonitorMovies,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "reactive-pending: service unavailable: %v\n", err)
		return 1
	}
	manager, err := reactivequeue.New(st, commitService, writer, reactivequeue.Options{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "reactive-pending: queue unavailable: %v\n", err)
		return 1
	}
	// `destinations` closes the discovery gap that made -root-folder and
	// -quality-profile unusable: they are REQUIRED when adding absent
	// content, but nothing in the CLI enumerated valid values, so an
	// operator had to go read them out of an Arr by hand. This reuses the
	// live Arr data the wizard already reads at S7d.
	if *action == "destinations" {
		instances, err := st.ListArrInstances(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reactive-pending: list arr instances failed: %v\n", err)
			return 1
		}
		targets, unresolved := reactivequeue.ResolveTargets(instances, cfg.Arrs, cfg.Lookup)
		for _, name := range unresolved {
			fmt.Fprintf(os.Stderr, "reactive-pending: skipping %s -- its sealed API key reference could not be resolved\n", name)
		}
		if len(targets) == 0 {
			fmt.Fprintln(os.Stderr, "reactive-pending: no Arr target has resolvable credentials")
			return 1
		}
		for _, target := range targets {
			fmt.Fprintf(os.Stdout, "arr=%s\n", target.Name)
			if strings.TrimSpace(target.RootFolder) != "" || target.QualityProfile > 0 {
				fmt.Fprintf(os.Stdout, "  captured: root-folder=%s quality-profile=%d\n", target.RootFolder, target.QualityProfile)
			} else {
				fmt.Fprintln(os.Stdout, "  captured: NOT CAPTURED -- run `darkharrbor configure` to set a destination")
			}
			roots, profiles, err := fetchArrDestinationOptions(ctx, target)
			if err != nil {
				fmt.Fprintf(os.Stdout, "  live options unavailable: %v\n", err)
				continue
			}
			for _, root := range roots {
				fmt.Fprintf(os.Stdout, "  root-folder=%s\n", root)
			}
			ids := make([]int, 0, len(profiles))
			for id := range profiles {
				ids = append(ids, id)
			}
			sort.Ints(ids)
			for _, id := range ids {
				fmt.Fprintf(os.Stdout, "  quality-profile=%d (%s)\n", id, profiles[id])
			}
		}
		return 0
	}
	if *action == "list" {
		entries, err := manager.List(ctx, strings.TrimSpace(*after), *limit)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reactive-pending: list failed: %v\n", err)
			return 1
		}
		// A parked entry is a DECISION SURFACE. Printing only the opaque
		// representation id asked an operator to route content whose
		// identity was invisible, so the queue could be read but not acted
		// on. Title/kind/year come from the item's authoritative
		// ProviderIdentity (ID-02), never from a filename guess: an item
		// whose identity is genuinely unavailable prints title=<unknown>
		// rather than inventing one.
		for _, entry := range entries {
			title, kind, year := reactivePendingIdentity(ctx, st, entry.ItemID)
			fmt.Fprintf(os.Stdout, "representation=%s\n", entry.RepresentationID)
			fmt.Fprintf(os.Stdout, "  title=%s kind=%s year=%s\n", title, kind, year)
			fmt.Fprintf(os.Stdout, "  reason=%s parked=%s actions=%s\n",
				entry.Reason,
				entry.CreatedAt.UTC().Format(time.RFC3339),
				strings.Join(reactivequeue.Actions(entry.Reason), ","))
		}
		fmt.Fprintf(os.Stdout, "count=%d\n", len(entries))
		if len(entries) == *limit {
			fmt.Fprintf(os.Stdout, "next=%s\n", entries[len(entries)-1].RepresentationID)
		}
		return 0
	}
	representationID := strings.TrimSpace(*representation)
	if representationID == "" {
		fmt.Fprintln(os.Stderr, "reactive-pending: representation is required")
		return 2
	}
	normalizedAction := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(*action)), "-", "_")
	if err := st.SyncReactivePending(ctx, time.Now().UTC()); err != nil {
		fmt.Fprintf(os.Stderr, "reactive-pending: sync failed: %v\n", err)
		return 1
	}
	entry, found, err := st.GetReactivePending(ctx, representationID)
	if err != nil || !found {
		if err != nil {
			fmt.Fprintf(os.Stderr, "reactive-pending: entry unavailable: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "reactive-pending: no pending entry for representation %s -- run `darkharrbor reactive-pending -action list` to see current entries\n", representationID)
		}
		return 1
	}
	proposal, found, err := st.GetPlaybackProposal(ctx, representationID)
	if err != nil || !found {
		if err != nil {
			fmt.Fprintf(os.Stderr, "reactive-pending: proposal unavailable: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "reactive-pending: no playback proposal recorded for representation %s\n", representationID)
		}
		return 1
	}
	item, err := st.GetItemByID(ctx, proposal.ItemID)
	if err != nil || item == nil || item.Metadata.ProviderIdentity == nil {
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "reactive-pending: item identity unavailable: %v\n", err)
		case item == nil:
			fmt.Fprintf(os.Stderr, "reactive-pending: item %s referenced by this entry no longer exists\n", proposal.ItemID)
		default:
			fmt.Fprintf(os.Stderr, "reactive-pending: item %s has no authoritative provider identity, so it cannot be routed to an Arr\n", proposal.ItemID)
		}
		return 1
	}
	cliArrInstances, _ := st.ListArrInstances(ctx)
	cliArrTargets, _ := reactivequeue.ResolveTargets(cliArrInstances, cfg.Arrs, cfg.Lookup)
	router, err := reactivequeue.NewRouter(st, cliArrTargets, nil)
	if err == nil {
		router.SetDefaultDestinations(cfg.Reactive.DefaultSeriesArr, cfg.Reactive.DefaultMovieArr)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "reactive-pending: router unavailable: %v\n", err)
		return 1
	}
	arrName := strings.TrimSpace(*targetName)
	if normalizedAction == "approve" && arrName == "" {
		route, err := router.Resolve(ctx, item.Metadata.ProviderIdentity)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reactive-pending: routing failed: %v\n", err)
			return 1
		}
		if route.ArrName == "" {
			if err := st.EnqueueReactivePending(ctx, store.ReactivePending{RepresentationID: entry.RepresentationID, ItemID: entry.ItemID, Reason: route.Reason}, time.Now().UTC()); err != nil {
				fmt.Fprintf(os.Stderr, "reactive-pending: routing park failed: %v\n", err)
				return 1
			}
			fmt.Fprintf(os.Stdout, "state=pending reason=%s tier=%d\n", route.Reason, route.Tier)
			return 0
		}
		arrName = route.ArrName
		if entry.Reason == "routing_unresolved" || entry.Reason == "routing_ambiguous" {
			normalizedAction = "assign"
			if err := router.Remember(ctx, item.Metadata.ProviderIdentity, arrName); err != nil {
				fmt.Fprintf(os.Stderr, "reactive-pending: assignment failed: %v\n", err)
				return 1
			}
		}
	}
	if normalizedAction == "assign" {
		if err := router.Remember(ctx, item.Metadata.ProviderIdentity, arrName); err != nil {
			fmt.Fprintf(os.Stderr, "reactive-pending: assignment failed: %v\n", err)
			return 1
		}
	}
	result, err := manager.Decide(ctx, reactivequeue.Decision{
		RepresentationID: representationID, Action: normalizedAction, ArrName: arrName,
		RootFolder: strings.TrimSpace(*rootFolder), QualityProfile: *qualityProfile,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "reactive-pending: decision failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stdout, "state=%s review_required=%t reason=%s\n", result.State, result.ReviewRequired, result.Reason)
	return 0
}

func runReactivePromoteCommand(args []string) int {
	fs := flag.NewFlagSet("reactive-promote", flag.ContinueOnError)
	action := fs.String("action", "promote", "promote or remove")
	representation := fs.String("representation", "", "opaque playback representation ID")
	target := fs.String("target", "", "absolute materialized path under the configured data root")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*representation) == "" || (*action != "promote" && *action != "remove") || (*action == "promote" && strings.TrimSpace(*target) == "") {
		fmt.Fprintln(os.Stderr, "reactive-promote: invalid request")
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "reactive-promote: configuration unavailable")
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client := &http.Client{Timeout: 15 * time.Second}
	login := url.Values{"username": {cfg.Auth.QBitUsername}, "password": {cfg.Auth.QBitPassword}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.Server.BaseURL, "/")+"/api/v2/auth/login", strings.NewReader(login.Encode()))
	if err != nil {
		fmt.Fprintln(os.Stderr, "reactive-promote: request unavailable")
		return 1
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			_ = resp.Body.Close()
		}
		fmt.Fprintln(os.Stderr, "reactive-promote: authentication failed")
		return 1
	}
	cookies := resp.Cookies()
	_ = resp.Body.Close()
	client.Timeout = 0
	form := url.Values{"action": {*action}, "representation": {strings.TrimSpace(*representation)}, "target": {strings.TrimSpace(*target)}}
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.Server.BaseURL, "/")+"/api/v1/reactive/promote", strings.NewReader(form.Encode()))
	if err != nil {
		fmt.Fprintln(os.Stderr, "reactive-promote: request unavailable")
		return 1
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		if resp != nil {
			_ = resp.Body.Close()
		}
		fmt.Fprintln(os.Stderr, "reactive-promote: operation failed")
		return 1
	}
	_ = resp.Body.Close()
	fmt.Fprintln(os.Stdout, "state="+*action+"d")
	return 0
}

// printDoctorReport renders a copy-pasteable, secret-redacted plain-text
// summary grouped by category, one line per check.
func printDoctorReport(out io.Writer, report *doctor.Report) {
	fmt.Fprintf(out, "DarkHarrbor doctor -- %s\n", report.GeneratedAt.Format(time.RFC3339))
	lastCategory := ""
	for _, c := range report.Checks {
		if c.Category != lastCategory {
			fmt.Fprintf(out, "\n[%s]\n", c.Category)
			lastCategory = c.Category
		}
		symbol := "?"
		switch c.Status {
		case doctor.StatusOK:
			symbol = "OK"
		case doctor.StatusWarn:
			symbol = "WARN"
		case doctor.StatusFail:
			symbol = "FAIL"
		case doctor.StatusSkip:
			symbol = "SKIP"
		}
		fmt.Fprintf(out, "  [%-4s] %-40s %s\n", symbol, c.Name, c.Detail)
	}
	fmt.Fprintln(out)
	if report.OK() {
		fmt.Fprintln(out, "Overall: OK")
	} else {
		fmt.Fprintln(out, "Overall: FAIL -- see above")
	}
}

// runConfigureCommand implements `darkharrbor configure`. The normal path writes
// strict tuning; --existing-aiostreams also updates one sealed backend URL. When invoked through
// `docker compose exec`, it gracefully terminates the server PID so Compose's
// restart policy reloads the new file; `compose run` receives a manual command.
func runConfigureCommand(args []string) int {
	fs := flag.NewFlagSet("configure", flag.ContinueOnError)
	runtimePath := config.TuningPath()
	path := fs.String("file", runtimePath, "path to the non-secret runtime settings file")
	noRestart := fs.Bool("no-restart", false, "save without restarting the running service")
	reset := fs.Bool("reset", false, "ignore and replace an invalid existing settings file")
	existingAIOStreams := fs.Bool("existing-aiostreams", false, "connect an existing AIOStreams/StremThru deployment")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	loadPath := *path
	if *reset {
		loadPath = "/dev/null"
	}
	if err := os.Setenv("HARRBOR_TUNING_FILE", loadPath); err != nil {
		fmt.Fprintf(os.Stderr, "configure: set tuning path: %v\n", err)
		return 1
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure: load current settings: %v", err)
		if !*reset {
			fmt.Fprint(os.Stderr, " (use --reset to replace an invalid tuning file)")
		}
		fmt.Fprintln(os.Stderr)
		return 1
	}
	_ = os.Setenv("HARRBOR_TUNING_FILE", *path)
	db, err := store.Open(context.Background(), cfg.Database.Path, cfg.Database.BusyTimeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure: open database: %v\n", err)
		return 1
	}
	defer db.Close()
	arrStore := store.NewWithSealKey(db, credentialSealKey(cfg))
	arrInstances, err := arrStore.ListArrInstances(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure: list stored Arr instances: %v\n", err)
		return 1
	}
	arrTargets, unresolvedArrs := reactivequeue.ResolveTargets(arrInstances, cfg.Arrs, cfg.Lookup)
	if len(unresolvedArrs) > 0 {
		fmt.Fprintf(os.Stderr, "configure: unresolved Arr credentials for: %s\n", strings.Join(unresolvedArrs, ", "))
		return 1
	}
	configureCfg := *cfg
	configureCfg.Arrs = arrTargets
	secretsPath := strings.TrimSpace(os.Getenv("HARRBOR_SECRETS_FILE"))
	if secretsPath == "" {
		secretsPath = filepath.Join(filepath.Dir(cfg.Database.Path), "secrets.sealed")
	}
	configureOptions := wizard.ConfigureOptions{
		Path:        *path,
		SecretsPath: secretsPath,
		Current:     &configureCfg,
		ArrStore:    arrStore,
	}
	var saved bool
	if *existingAIOStreams {
		saved, err = wizard.RunConfigureExistingAIOStreams(configureOptions)
	} else {
		saved, err = wizard.RunConfigure(configureOptions)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure: %v\n", err)
		return 1
	}
	if !saved || *noRestart {
		return 0
	}
	if filepath.Clean(*path) != filepath.Clean(runtimePath) {
		fmt.Fprintf(os.Stdout, "Settings saved. Set HARRBOR_TUNING_FILE=%s for the service, then run: docker compose up -d --force-recreate darkharrbor\n", *path)
		return 0
	}

	restarted, err := restartServerPID1(os.Getpid(), os.Readlink, syscall.Kill)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure: settings saved, but restart failed: %v\n", err)
		return 1
	}
	if restarted {
		fmt.Fprintln(os.Stdout, "Restarting DarkHarrbor to apply the new settings...")
		return 0
	}
	fmt.Fprintln(os.Stdout, "Settings saved. Apply them with: docker compose up -d --force-recreate darkharrbor")
	return 0
}

func configuredLogLevel(cfg *config.Config) slog.Level {
	raw, _ := cfg.Lookup("HARRBOR_LOG_LEVEL")
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "DEBUG":
		return slog.LevelDebug
	case "WARN":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func restartServerPID1(currentPID int, readlink func(string) (string, error), kill func(int, syscall.Signal) error) (bool, error) {
	if currentPID == 1 {
		return false, nil
	}
	exe, err := readlink("/proc/1/exe")
	if err != nil || filepath.Base(exe) != "darkharrbor" {
		return false, nil
	}
	if err := kill(1, syscall.SIGTERM); err != nil {
		return false, err
	}
	return true, nil
}
