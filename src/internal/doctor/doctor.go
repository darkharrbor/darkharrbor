// Package doctor implements ONB-02: read-only diagnostics ("darkharrbor
// doctor" CLI subcommand and GET /api/v1/doctor endpoint, auto-run at
// wizard end). Every check reuses an already-existing shared owner where
// one exists (arrsetup's own client/identity rules for topology,
// jellyfinsetup's own discovery/exec for Jellyfin visibility, each
// provider's own real account/dial call, the NNTP provider's own
// connection pool) rather than inventing a parallel path. Nothing here
// mutates Arr, provider, or DarkHarrbor state; the only write is a small,
// bounded, always-cleaned-up marker file used by the path-mapping probe.
package doctor

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/arrsetup"
	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/dockerctl"
	"github.com/darkharrbor/darkharrbor/internal/jellyfinsetup"
	"github.com/darkharrbor/darkharrbor/internal/publicip"
	"github.com/darkharrbor/darkharrbor/internal/store"
	"github.com/darkharrbor/darkharrbor/internal/topology"
)

// Status is one check's outcome.
type Status string

const (
	StatusOK   Status = "ok"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
	// StatusSkip marks a check that legitimately does not apply (e.g. no
	// Real-Debrid token configured, no governor available in standalone
	// CLI context) -- an honest abstain, never a guessed pass or fail.
	StatusSkip Status = "skip"
)

// Check is one diagnostic result. Detail is always secret-redacted:
// hostnames and container names are fine, but no API key, token, signed
// tok= query value, or full upstream URL is ever placed here (DG-04/LG-10).
type Check struct {
	Category string `json:"category"`
	Name     string `json:"name"`
	Status   Status `json:"status"`
	Detail   string `json:"detail"`
}

// Report is the full doctor sweep result.
type Report struct {
	GeneratedAt time.Time `json:"generated_at"`
	Checks      []Check   `json:"checks"`
}

// OK reports whether every check passed or was skipped (no fail). A warn
// does not fail the overall report.
func (r *Report) OK() bool {
	for _, c := range r.Checks {
		if c.Status == StatusFail {
			return false
		}
	}
	return true
}

func (r *Report) add(c Check) {
	r.Checks = append(r.Checks, c)
}

// Options carries everything Run needs. Fields with a nil/empty zero value
// cause that check group to be skipped (StatusSkip), never guessed.
type Options struct {
	DB  *sql.DB
	Cfg *config.Config

	// PublicIPResolver is the running MediaFlow resolver when doctor is
	// invoked through the API. Nil makes the standalone CLI perform its own
	// bounded lookup instead of guessing from another process's memory.
	PublicIPResolver *publicip.Resolver

	// DockerProxyURL enables the Jellyfin visibility check via docker exec,
	// the same reachability precondition wizard S8-S10 already require.
	// Empty disables the check (StatusSkip), matching wizard's own
	// "docker proxy unreachable" degraded-but-non-fatal handling.
	DockerProxyURL string

	// TorrentGov/TorrentGovOp, when both set, govern the TorBox/Real-Debrid
	// dial tests through the real production accountgov.Governor + op
	// (accountgov.PriorityAudit), exactly the pattern OBS-02's debug-resolve
	// endpoint already established. Nil in the standalone CLI context,
	// where no live governor exists in-process (disclosed scope limit,
	// same shape as debugresolve.go's own NNTP "not applicable" note).
	TorrentGov   *accountgov.Governor
	TorrentGovOp string

	// Now overrides the clock in tests; nil means time.Now.
	Now func() time.Time
}

func (o *Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now().UTC()
}

const (
	defaultTimeout    = 60 * time.Second
	perCallTimeout    = 10 * time.Second
	defaultTimeoutEnv = "HARRBOR_DOCTOR_TIMEOUT_SECONDS"
)

// TimeoutFromEnv reads HARRBOR_DOCTOR_TIMEOUT_SECONDS, an optional bound on
// the whole sweep. An absent or invalid value warns (via warn, which may be
// nil) and falls back to the default; it never fails startup or the sweep,
// matching HARRBOR_DEMO_TIMEOUT_SECONDS's own established convention.
func TimeoutFromEnv(warn func(string)) time.Duration {
	raw := strings.TrimSpace(os.Getenv(defaultTimeoutEnv))
	if raw == "" {
		return defaultTimeout
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs <= 0 {
		if warn != nil {
			warn(fmt.Sprintf("%s=%q invalid -- using default %s", defaultTimeoutEnv, raw, defaultTimeout))
		}
		return defaultTimeout
	}
	return time.Duration(secs) * time.Second
}

// Run performs the full bounded diagnostic sweep, using timeout as the
// overall ceiling (callers pass TimeoutFromEnv's result, or their own
// bound). It never returns an error for an individual check's failure --
// every check result lands in the Report as a Check -- only a hard
// construction failure (e.g. a nil DB) returns a top-level error.
func Run(ctx context.Context, opts Options, timeout time.Duration) (*Report, error) {
	if opts.DB == nil {
		return nil, fmt.Errorf("doctor: nil DB")
	}
	if opts.Cfg == nil {
		return nil, fmt.Errorf("doctor: nil Cfg")
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	r := &Report{GeneratedAt: opts.now()}
	sealKey, _ := opts.Cfg.Secrets.SealKey()
	st := store.NewWithSealKey(opts.DB, sealKey)
	runSetupPlanDrift(r, opts.DB, opts.Cfg)
	runDeploymentTopology(ctx, r, opts.Cfg, st, opts.now())
	runAggregatorResolution(r, opts.Cfg)
	// RX-0.6: resolution proves the aggregator ANSWERS; egress proves
	// DarkHarrbor can actually FETCH what it hands back. Under universal
	// proxying those are different properties and the second one failed
	// invisibly on 2026-08-21.
	runAggregatorEgress(ctx, r, opts.Cfg)
	runProxyPostureVisibility(r, opts.Cfg)
	runAggregatorDelivery(ctx, r, opts.Cfg, st, opts.now(), time.Duration(opts.Cfg.Topology.AchievedPostureStalenessHours)*time.Hour)
	runMediaFlowPublicIP(ctx, r, opts.Cfg, opts.PublicIPResolver, desiredAggregatorFromDB(opts.DB, opts.Cfg))

	instances, err := st.ListArrInstances(ctx)
	if err != nil {
		r.add(Check{Category: "arr", Name: "list-instances", Status: StatusFail, Detail: "could not read discovered arr instances: " + errClass(err)})
	}

	var dateHeaderOnce sync.Once
	var remoteDate time.Time
	recordDate := func(resp *http.Response) {
		if resp == nil {
			return
		}
		dateHeaderOnce.Do(func() {
			if d, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
				remoteDate = d
			}
		})
	}

	desiredTopology := desiredArrTopologyFromDB(opts.DB)
	resolvedArrs := make([]arrsetup.ArrTarget, 0, len(instances)+len(opts.Cfg.Arrs))
	if len(instances) == 0 {
		for _, envTarget := range opts.Cfg.Arrs {
			appType, detectErr := arrsetup.DetectAppType(ctx, envTarget.BaseURL, envTarget.APIKey)
			if detectErr != nil {
				r.add(Check{Category: "arr", Name: envTarget.Name + ": type", Status: StatusFail, Detail: "could not identify authenticated Sonarr/Radarr target"})
				continue
			}
			target := arrsetup.ArrTarget{Name: envTarget.Name, URL: envTarget.BaseURL, AppType: appType, APIKey: envTarget.APIKey}
			resolvedArrs = append(resolvedArrs, target)
			runArrReachability(ctx, r, target, recordDate)
			runDesiredArrTopology(ctx, r, target, desiredTopology)
		}
	}
	for _, inst := range instances {
		apiKey, ok := opts.Cfg.Lookup(inst.APIKeyRef)
		if !ok || strings.TrimSpace(apiKey) == "" {
			r.add(Check{Category: "arr", Name: inst.Name + ": api-key", Status: StatusFail,
				Detail: fmt.Sprintf("could not resolve API key ref %q from sealed secrets or environment", inst.APIKeyRef)})
			continue
		}
		target := arrsetup.ArrTarget{Name: inst.Name, URL: inst.URL, AppType: inst.AppType, APIKey: apiKey}
		if inst.AppType == "sonarr" || inst.AppType == "radarr" {
			resolvedArrs = append(resolvedArrs, target)
		}
		runArrReachability(ctx, r, target, recordDate)
		if inst.AppType == "sonarr" || inst.AppType == "radarr" {
			runDesiredArrTopology(ctx, r, target, desiredTopology)
		}
	}

	if len(resolvedArrs) > 0 {
		runPathMappingProbe(ctx, r, opts.Cfg, resolvedArrs)
	} else {
		r.add(Check{Category: "path-mapping", Name: "probe", Status: StatusSkip, Detail: "no effective Arr instances configured"})
	}

	plan, hasPlan := readDesiredSetupPlan(opts.DB)
	if hasPlan && !plan.Jellyfin {
		r.add(Check{Category: "jellyfin", Name: "visibility", Status: StatusSkip, Detail: "Jellyfin integration was intentionally declined"})
	} else {
		runJellyfinCheck(ctx, r, opts)
	}

	runProviderChecks(ctx, r, opts, st, recordDate)

	runDNSChecks(ctx, r, opts, instances)
	runClockCheck(r, opts, remoteDate)
	runDiskChecks(r, opts)

	return r, nil
}

type desiredArrTopology struct {
	Selected bool
	Lanes    arrsetup.Lanes
}

type desiredSetupPlan struct {
	Topology         string   `json:"topology"`
	AcquisitionLanes []string `json:"acquisition_lanes"`
	ReactiveCommit   bool     `json:"reactive_commit"`
	Aggregator       bool     `json:"aggregator"`
	StremioAddon     bool     `json:"stremio_addon"`
	Prowlarr         bool     `json:"prowlarr"`
	Jellyfin         bool     `json:"jellyfin"`
	WrapperRepair    bool     `json:"wrapper_repair"`
	Promotion        bool     `json:"promotion"`
	Backups          bool     `json:"backups"`
}

func readDesiredSetupPlan(db *sql.DB) (desiredSetupPlan, bool) {
	var raw string
	if err := db.QueryRow(`SELECT value FROM app_config WHERE key = 'onboarding_plan'`).Scan(&raw); err != nil {
		return desiredSetupPlan{}, false
	}
	var plan desiredSetupPlan
	if err := json.Unmarshal([]byte(raw), &plan); err != nil || strings.TrimSpace(plan.Topology) == "" {
		return desiredSetupPlan{}, false
	}
	return plan, true
}

func runSetupPlanDrift(r *Report, db *sql.DB, cfg *config.Config) {
	const name = "applied onboarding plan"
	plan, ok := readDesiredSetupPlan(db)
	if !ok {
		r.add(Check{Category: "configuration", Name: name, Status: StatusSkip, Detail: "no goal-first setup plan is recorded (legacy installation)"})
		return
	}
	missing := []string{}
	if plan.Topology != string(cfg.Topology.Active) {
		missing = append(missing, "topology")
	}
	hasPreference := func(want ...string) bool {
		for _, got := range cfg.Routing.Preference {
			for _, candidate := range want {
				if got == candidate {
					return true
				}
			}
		}
		return false
	}
	for _, lane := range plan.AcquisitionLanes {
		switch lane {
		case "torrent":
			if !hasPreference(config.LaneTorBoxTorrent, config.LaneUncachedTorrent, config.LaneUncachedTorrentDerank) {
				missing = append(missing, "torrent lane")
			}
		case "nntp":
			if !hasPreference(config.LaneTorBoxNZB, config.LaneNNTPNZB) {
				missing = append(missing, "NNTP lane")
			}
		case "http":
			if !cfg.HTTPStream.Enabled || len(cfg.HTTPStream.Backends) == 0 {
				missing = append(missing, "HTTP backends")
			}
		}
	}
	if plan.Aggregator && strings.TrimSpace(cfg.MediaFlow.Password) == "" {
		missing = append(missing, "MediaFlow credential")
	}
	if plan.ReactiveCommit && (!cfg.Reactive.Enabled || cfg.Reactive.CommitMode == "off") {
		missing = append(missing, "reactive commit")
	}
	if plan.Promotion != cfg.Reactive.PromotionEnabled {
		missing = append(missing, "promotion choice")
	}
	if plan.StremioAddon && strings.TrimSpace(cfg.Stremio.InstallToken) == "" {
		missing = append(missing, "Stremio install token")
	}
	if plan.Prowlarr && (strings.TrimSpace(cfg.Prowlarr.BaseURL) == "" || strings.TrimSpace(cfg.Prowlarr.APIKey) == "") {
		missing = append(missing, "Prowlarr selection mode")
	}
	if plan.Backups != cfg.Backup.Enabled {
		missing = append(missing, "backup choice")
	}
	if plan.WrapperRepair != cfg.StrmInstaller.Enabled {
		missing = append(missing, "wrapper repair choice")
	}
	if len(missing) > 0 {
		r.add(Check{Category: "configuration", Name: name, Status: StatusFail,
			Detail: "effective configuration differs from the reviewed plan: " + strings.Join(missing, ", ") + "; run `darkharrbor configure`, then `darkharrbor reconcile-arr`"})
		return
	}
	r.add(Check{Category: "configuration", Name: name, Status: StatusOK, Detail: "effective runtime configuration matches the reviewed goal plan"})
}

func desiredArrTopologyFromDB(db *sql.DB) desiredArrTopology {
	legacy := desiredArrTopology{Selected: true, Lanes: arrsetup.Lanes{Torrent: true, NNTP: true, HTTP: true}}
	var raw string
	if err := db.QueryRow(`SELECT value FROM app_config WHERE key = 'onboarding_plan'`).Scan(&raw); err != nil {
		return legacy
	}
	var plan struct {
		BaseArrTopology  bool     `json:"base_arr_topology"`
		AcquisitionLanes []string `json:"acquisition_lanes"`
	}
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		return legacy
	}
	desired := desiredArrTopology{Selected: plan.BaseArrTopology}
	for _, lane := range plan.AcquisitionLanes {
		switch lane {
		case "torrent":
			desired.Lanes.Torrent = true
		case "nntp":
			desired.Lanes.NNTP = true
		case "http":
			desired.Lanes.HTTP = true
		}
	}
	return desired
}

func desiredAggregatorFromDB(db *sql.DB, cfg *config.Config) bool {
	legacy := cfg.Topology.Active == topology.T2 || cfg.Topology.Active == topology.T3
	var raw string
	if err := db.QueryRow(`SELECT value FROM app_config WHERE key = 'onboarding_plan'`).Scan(&raw); err != nil {
		return legacy
	}
	var plan struct {
		Aggregator bool `json:"aggregator"`
	}
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		return legacy
	}
	return plan.Aggregator
}

func runDesiredArrTopology(ctx context.Context, r *Report, target arrsetup.ArrTarget, desired desiredArrTopology) {
	if !desired.Selected {
		r.add(Check{Category: "topology", Name: target.Name, Status: StatusSkip, Detail: "base Arr search/grab topology was intentionally declined"})
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, perCallTimeout)
	defer cancel()
	top := arrsetup.CheckTopology(callCtx, target)
	if top.Err != nil {
		r.add(Check{Category: "topology", Name: target.Name, Status: StatusFail, Detail: "could not read selected indexers/clients: " + errClass(top.Err)})
		return
	}
	missing := []string{}
	if (desired.Lanes.Torrent || desired.Lanes.HTTP) && !top.QBitPresent {
		missing = append(missing, "qbit-client")
	}
	if desired.Lanes.NNTP && !top.SABPresent {
		missing = append(missing, "sab-client")
	}
	if desired.Lanes.Torrent && !top.TorznabPresent {
		missing = append(missing, "torznab-indexer")
	}
	if desired.Lanes.NNTP && !top.NewznabPresent {
		missing = append(missing, "newznab-indexer")
	}
	if desired.Lanes.HTTP && !top.HTTPStreamPresent {
		missing = append(missing, "http-stream-indexer")
	}
	if len(missing) > 0 {
		r.add(Check{Category: "topology", Name: target.Name, Status: StatusFail, Detail: "missing selected surfaces: " + strings.Join(missing, ", ")})
		return
	}
	r.add(Check{Category: "topology", Name: target.Name, Status: StatusOK, Detail: "all selected Arr lane surfaces are present"})
}

func runMediaFlowPublicIP(ctx context.Context, r *Report, cfg *config.Config, resolver *publicip.Resolver, selected bool) {
	if !selected {
		r.add(Check{Category: "mediaflow", Name: "public egress address", Status: StatusSkip,
			Detail: "aggregator integration was intentionally declined"})
		return
	}
	if strings.TrimSpace(cfg.MediaFlow.Password) == "" {
		detail := "MediaFlow compatibility surface is disabled"
		status := StatusSkip
		if selected {
			detail = "HARRBOR_MEDIAFLOW_PASSWORD is unset; rerun setup for the selected aggregator topology"
			status = StatusFail
		}
		r.add(Check{Category: "mediaflow", Name: "public egress address", Status: status,
			Detail: detail})
		return
	}
	literal := strings.TrimSpace(cfg.MediaFlow.PublicIP)
	if literal != "" {
		if net.ParseIP(literal) == nil {
			r.add(Check{Category: "mediaflow", Name: "public egress address", Status: StatusFail,
				Detail: "configured literal is not a parseable address"})
			return
		}
		r.add(Check{Category: "mediaflow", Name: "public egress address", Status: StatusOK,
			Detail: "configured literal override is valid"})
		return
	}
	owned := false
	if resolver == nil {
		resolver = publicip.New(ctx, publicip.Options{})
		owned = true
	}
	if owned {
		defer resolver.Close()
	}
	_, err := resolver.Resolve(ctx)
	status := resolver.Status()
	if err != nil && status.Address == "" {
		r.add(Check{Category: "mediaflow", Name: "public egress address", Status: StatusFail,
			Detail: "runtime lookup failed and no last known good address is available"})
		return
	}
	if status.Stale || status.LastAttemptFail {
		r.add(Check{Category: "mediaflow", Name: "public egress address", Status: StatusWarn,
			Detail: "serving a stale last known good address while runtime refresh is degraded"})
		return
	}
	r.add(Check{Category: "mediaflow", Name: "public egress address", Status: StatusOK,
		Detail: "runtime lookup has a current address"})
}

type playbackCoverageReader interface {
	LatestPlaybackCoverage(context.Context, string) (time.Time, bool, error)
}

func runDeploymentTopology(ctx context.Context, r *Report, cfg *config.Config, coverage playbackCoverageReader, now time.Time) {
	active := cfg.Topology.Active
	profile, known := topology.Profile(active)
	if !known {
		r.add(Check{Category: "deployment-topology", Name: "unknown", Status: StatusWarn,
			Detail: "unsupported profile; using t1 is recommended"})
		return
	}
	if matched, exact := topology.Match(profile); !exact || matched != active {
		r.add(Check{Category: "deployment-topology", Name: string(active), Status: StatusWarn,
			Detail: "feature combination is outside the supported topology set"})
		return
	}
	missing := topology.MissingCapabilities(active, topology.Implemented())
	if len(missing) > 0 {
		r.add(Check{Category: "deployment-topology", Name: string(active), Status: StatusWarn,
			Detail: "recognized profile; unavailable capabilities: " + strings.Join(missing, ", ")})
		return
	}
	detail := "arr-native; aggregator proxy and reactive commit disabled"
	if active == topology.T2 {
		detail = "aggregated discovery with universal playback proxy; reactive commit disabled"
	} else if active == topology.T3 {
		detail = "aggregated discovery with universal playback proxy and reactive commit"
	}
	if active != topology.T2 && active != topology.T3 {
		r.add(Check{Category: "deployment-topology", Name: string(active), Status: StatusOK, Detail: detail})
		return
	}
	if coverage == nil {
		r.add(Check{Category: "deployment-topology", Name: string(active), Status: StatusWarn,
			Detail: "universal playback proxy claim is unsubstantiated: durable coverage is unavailable"})
		return
	}
	at, found, err := coverage.LatestPlaybackCoverage(ctx, "mediaflow:")
	if err != nil {
		r.add(Check{Category: "deployment-topology", Name: string(active), Status: StatusWarn,
			Detail: "universal playback proxy claim is unsubstantiated: durable coverage could not be read"})
		return
	}
	if !found {
		r.add(Check{Category: "deployment-topology", Name: string(active), Status: StatusWarn,
			Detail: "universal playback proxy claim is unsubstantiated: no aggregator-proxied playback has been observed"})
		return
	}
	staleness := time.Duration(cfg.Topology.AchievedPostureStalenessHours) * time.Hour
	if staleness <= 0 {
		staleness = 48 * time.Hour
	}
	if at.Before(now.Add(-staleness)) {
		r.add(Check{Category: "deployment-topology", Name: string(active), Status: StatusWarn,
			Detail: "universal playback proxy claim is unsubstantiated: aggregator-proxied playback is stale"})
		return
	}
	r.add(Check{Category: "deployment-topology", Name: string(active), Status: StatusOK, Detail: detail})
}

// aggregatorDeliveryReader exposes RX-8.2's durable delivery observation.
type aggregatorDeliveryReader interface {
	CurrentPlaybackRepresentationMetrics(context.Context, time.Time) (store.PlaybackRepresentationCounts, error)
}

// runAggregatorDelivery names an AGGREGATOR-SIDE failure (RD-28 RX-0.3) instead
// of leaving it to present at the client as an unexplained playback error.
//
// It reads the durable playback-time state owned by RX-8.2 rather than
// inferring from discovery data. That distinction is load-bearing and was
// established by fault injection on 2026-08-17: stopping the shared debrid tier
// moved the aggregator's source count only 520 -> 515, and a playable-URL-only
// count moved identically even with the aggregator's resolved-link cache
// flushed, because the aggregator emits playback URLs BEFORE resolution is
// attempted. Discovery data provably cannot carry this signal; delivery data
// can, because a resolution failure stops bytes from ever transiting DH.
//
// The three states are diagnostically distinct and are reported as such:
//
//	discovery fails          -> the configured backend is unreachable
//	discovery ok, never      -> the posture has never worked at all
//	discovery ok, then stops -> resolution or playback is failing DOWNSTREAM
//	                            of discovery, which is the shape of the
//	                            2026-08-16 shared-tier outage
func runAggregatorDelivery(ctx context.Context, r *Report, cfg *config.Config, deliveries aggregatorDeliveryReader, now time.Time, staleAfter time.Duration) {
	active := cfg.Topology.Active
	if active != topology.T2 && active != topology.T3 {
		return
	}
	if !hasAggregatorBackend(cfg) || deliveries == nil {
		return
	}
	const name = "aggregator delivery"
	counts, err := deliveries.CurrentPlaybackRepresentationMetrics(ctx, now)
	if err != nil {
		r.add(Check{Category: "deployment-topology", Name: name, Status: StatusWarn,
			Detail: "delivery observation unavailable: " + errClass(err)})
		return
	}
	if !counts.AggregatorKnown {
		r.add(Check{Category: "deployment-topology", Name: name, Status: StatusFail,
			Detail: "NO aggregator-proxied playback has EVER transited DarkHarrbor. Discovery reaching the aggregator is " +
				"not sufficient: the aggregator emits playback URLs before resolving them, so a client can receive sources " +
				"that never resolve. Check that the aggregator proxies to this instance and that the playback base URL is " +
				"reachable FROM CLIENT DEVICES, not merely from this host."})
		return
	}
	if staleAfter > 0 && counts.AggregatorAge > staleAfter {
		r.add(Check{Category: "deployment-topology", Name: name, Status: StatusFail,
			Detail: fmt.Sprintf("aggregator-proxied playback previously worked but has delivered NOTHING for %s. "+
				"This is an aggregator-side failure DOWNSTREAM of discovery -- typically the shared debrid-resolution tier "+
				"or the playback path -- not a content-source outage, which the aggregator skips without stopping delivery.",
				counts.AggregatorAge.Round(time.Minute))})
		return
	}
	r.add(Check{Category: "deployment-topology", Name: name, Status: StatusOK,
		Detail: fmt.Sprintf("last aggregator-proxied delivery %s ago", counts.AggregatorAge.Round(time.Second))})
}

// runAggregatorResolution records why doctor intentionally abstains from a
// full catalog lookup. A lookup is not read-only: an aggregator can call the
// installed DarkHarrbor Library addon while resolving the probe title, which
// seeds reactive identity state in the running daemon. Safe socket and
// playback-delivery checks below still validate the configured route without
// manufacturing operator activity.
func runAggregatorResolution(r *Report, cfg *config.Config) {
	active := cfg.Topology.Active
	if active != topology.T2 && active != topology.T3 {
		return
	}
	if !hasAggregatorBackend(cfg) {
		return
	}
	const name = "aggregator resolution"
	r.add(Check{Category: "deployment-topology", Name: name, Status: StatusSkip,
		Detail: "full catalog lookup is deliberately excluded because it can call the installed Library addon and alter reactive identity state; safe aggregator egress and observed playback-delivery checks follow"})
}

func hasAggregatorBackend(cfg *config.Config) bool {
	for _, backend := range cfg.HTTPStream.Backends {
		// AIOStreams permits the type to remain auto-detected; its stable
		// configured backend identity is still the aggregator contract.
		if backend.Type == "stremio" || backend.Name == "aiostreams" {
			return true
		}
	}
	return false
}

// errClass renders an error safely for a Check.Detail (DG-04): anything
// from the "://" marker onward is summarized rather than included verbatim,
// since net/http errors can otherwise embed a full request URL.
func errClass(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if i := strings.Index(s, "://"); i >= 0 {
		return s[:i] + "<redacted-url>"
	}
	return s
}

func runArrReachability(ctx context.Context, r *Report, t arrsetup.ArrTarget, recordDate func(*http.Response)) {
	callCtx, cancel := context.WithTimeout(ctx, perCallTimeout)
	defer cancel()
	statusPath := "/api/v3/system/status"
	if t.AppType == "prowlarr" {
		statusPath = "/api/v1/system/status"
	}
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, strings.TrimRight(t.URL, "/")+statusPath, nil)
	if err != nil {
		r.add(Check{Category: "arr", Name: t.Name + ": reachability", Status: StatusFail, Detail: errClass(err)})
		return
	}
	req.Header.Set("X-Api-Key", t.APIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.add(Check{Category: "arr", Name: t.Name + ": reachability", Status: StatusFail, Detail: "unreachable: " + errClass(err)})
		return
	}
	defer resp.Body.Close()
	recordDate(resp)
	switch resp.StatusCode {
	case http.StatusOK:
		r.add(Check{Category: "arr", Name: t.Name + ": reachability", Status: StatusOK, Detail: fmt.Sprintf("HTTP %d", resp.StatusCode)})
	case http.StatusUnauthorized, http.StatusForbidden:
		r.add(Check{Category: "arr", Name: t.Name + ": api-key", Status: StatusFail, Detail: fmt.Sprintf("HTTP %d -- API key rejected", resp.StatusCode)})
	default:
		r.add(Check{Category: "arr", Name: t.Name + ": reachability", Status: StatusFail, Detail: fmt.Sprintf("HTTP %d", resp.StatusCode)})
	}
}

func runArrTopology(ctx context.Context, r *Report, t arrsetup.ArrTarget) {
	callCtx, cancel := context.WithTimeout(ctx, perCallTimeout)
	defer cancel()
	top := arrsetup.CheckTopology(callCtx, t)
	if top.Err != nil {
		r.add(Check{Category: "topology", Name: t.Name, Status: StatusFail, Detail: "could not read indexers/clients: " + errClass(top.Err)})
		return
	}
	if top.Complete() {
		r.add(Check{Category: "topology", Name: t.Name, Status: StatusOK, Detail: "qbit+sab clients, torznab+newznab+http-stream indexers all present"})
		return
	}
	missing := make([]string, 0, 5)
	if !top.QBitPresent {
		missing = append(missing, "qbit-client")
	}
	if !top.SABPresent {
		missing = append(missing, "sab-client")
	}
	if !top.TorznabPresent {
		missing = append(missing, "torznab-indexer")
	}
	if !top.NewznabPresent {
		missing = append(missing, "newznab-indexer")
	}
	if !top.HTTPStreamPresent {
		missing = append(missing, "http-stream-indexer")
	}
	r.add(Check{Category: "topology", Name: t.Name, Status: StatusFail, Detail: "missing: " + strings.Join(missing, ", ")})
}

// runPathMappingProbe writes one small marker file under the shared data
// root and confirms every discovered arr's own filesystem view sees it at
// its configured ReportedPathPrefix, via each arr's GET /api/v3/filesystem
// browse endpoint -- a real write+stat round trip through the actual bind
// mount, not a guess. The marker is always removed afterward, even on
// early cancellation.
func runPathMappingProbe(ctx context.Context, r *Report, cfg *config.Config, arrs []arrsetup.ArrTarget) {
	root := cfg.Data.Strm
	if strings.TrimSpace(root) == "" {
		r.add(Check{Category: "path-mapping", Name: "probe", Status: StatusSkip, Detail: "no data root configured"})
		return
	}
	name := fmt.Sprintf(".darkharrbor-doctor-probe-%d", time.Now().UnixNano())
	localPath := filepath.Join(root, name)
	if err := os.WriteFile(localPath, []byte("darkharrbor doctor path-mapping probe\n"), 0o640); err != nil {
		r.add(Check{Category: "path-mapping", Name: "probe", Status: StatusFail, Detail: "could not write marker under data root: " + errClass(err)})
		return
	}
	defer func() { _ = os.Remove(localPath) }()

	reportedPath := filepath.Join(cfg.Data.ReportedPathPrefix, name)
	for _, t := range arrs {
		callCtx, cancel := context.WithTimeout(ctx, perCallTimeout)
		visible, err := arrSeesPath(callCtx, t, reportedPath)
		cancel()
		switch {
		case err != nil:
			r.add(Check{Category: "path-mapping", Name: t.Name, Status: StatusFail, Detail: "filesystem probe failed: " + errClass(err)})
		case visible:
			r.add(Check{Category: "path-mapping", Name: t.Name, Status: StatusOK, Detail: "marker file visible at reported path prefix"})
		default:
			r.add(Check{Category: "path-mapping", Name: t.Name, Status: StatusFail, Detail: "marker file NOT visible at reported path prefix -- remote path mapping likely required"})
		}
	}
}

// arrSeesPath queries one arr's own GET /api/v3/filesystem?path=... browse
// endpoint (the same one its UI root-folder picker uses) and reports
// whether it lists a file with exactly the marker's name.
func arrSeesPath(ctx context.Context, t arrsetup.ArrTarget, path string) (bool, error) {
	dir := filepath.Dir(path)
	want := filepath.Base(path)
	// LIVE-GATE-FOUND DEFECT (ONB-02): Sonarr/Radarr's GET /api/v3/filesystem
	// omits the "files" array entirely unless includeFiles=true is passed
	// (directories-only is the default, matching its own root-folder-picker
	// use), and a dir value with no trailing slash returns the PARENT
	// directory's listing instead of dir's own contents. Both are required
	// for this probe to ever see a real file; confirmed live against the
	// actual deployed Sonarr instances during this row's own gate.
	q := url.Values{}
	q.Set("path", strings.TrimRight(dir, "/")+"/")
	q.Set("includeFiles", "true")
	u := strings.TrimRight(t.URL, "/") + "/api/v3/filesystem?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("X-Api-Key", t.APIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return false, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var body struct {
		Files []struct {
			Name string `json:"name"`
		} `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, err
	}
	for _, f := range body.Files {
		if f.Name == want {
			return true, nil
		}
	}
	return false, nil
}

func runJellyfinCheck(ctx context.Context, r *Report, opts Options) {
	if strings.TrimSpace(opts.DockerProxyURL) == "" {
		r.add(Check{Category: "jellyfin", Name: "visibility", Status: StatusSkip, Detail: "docker proxy not configured"})
		return
	}
	client := dockerctl.New(opts.DockerProxyURL, &http.Client{Timeout: perCallTimeout})
	callCtx, cancel := context.WithTimeout(ctx, perCallTimeout)
	defer cancel()
	// LIVE-GATE-FOUND DEFECT (ONB-02): cfg.Data.Strm is DH's own
	// container-internal path (e.g. /data); Jellyfin, like the arrs, sees
	// the shared host bind mount at cfg.Data.ReportedPathPrefix instead
	// (confirmed live: Jellyfin bind-mounts the whole host /mnt at its
	// own /mnt, matching every arr's own /mnt/darkharrbor view -- DH's
	// container-internal /data has no meaning inside Jellyfin at all).
	res := jellyfinsetup.CheckVisibility(callCtx, client, opts.Cfg.Data.ReportedPathPrefix)
	switch {
	case res.Err != nil:
		r.add(Check{Category: "jellyfin", Name: "visibility", Status: StatusFail, Detail: errClass(res.Err)})
	case !res.Found:
		r.add(Check{Category: "jellyfin", Name: "visibility", Status: StatusSkip, Detail: "no running jellyfin/jellyfin container found"})
	case res.PathVisible:
		r.add(Check{Category: "jellyfin", Name: "visibility", Status: StatusOK, Detail: res.ContainerName + ": data root visible"})
	default:
		r.add(Check{Category: "jellyfin", Name: "visibility", Status: StatusFail, Detail: res.ContainerName + ": data root NOT visible"})
	}
}

func runDNSChecks(ctx context.Context, r *Report, opts Options, instances []store.ArrInstance) {
	hosts := map[string]struct{}{}
	if opts.Cfg.TorBox.APIToken != "" {
		hosts["api.torbox.app"] = struct{}{}
	}
	if opts.Cfg.NNTP.Host != "" {
		hosts[opts.Cfg.NNTP.Host] = struct{}{}
	}
	for _, inst := range instances {
		if h := hostOnly(inst.URL); h != "" {
			hosts[h] = struct{}{}
		}
	}
	resolver := net.DefaultResolver
	for host := range hosts {
		callCtx, cancel := context.WithTimeout(ctx, perCallTimeout)
		_, err := resolver.LookupHost(callCtx, host)
		cancel()
		if err != nil {
			r.add(Check{Category: "dns", Name: host, Status: StatusFail, Detail: errClass(err)})
		} else {
			r.add(Check{Category: "dns", Name: host, Status: StatusOK, Detail: "resolved"})
		}
	}
}

// hostOnly extracts the host[:port] portion of a plain http(s) base URL
// with no query or credentials (every URL passed in here was constructed
// by DH's own arr discovery, never from request input).
func hostOnly(rawURL string) string {
	s := rawURL
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?"); i >= 0 {
		s = s[:i]
	}
	host, _, err := net.SplitHostPort(s)
	if err != nil {
		return s
	}
	return host
}

func runClockCheck(r *Report, opts Options, remoteDate time.Time) {
	if remoteDate.IsZero() {
		r.add(Check{Category: "clock", Name: "skew", Status: StatusSkip, Detail: "no remote Date header observed this sweep"})
		return
	}
	skew := opts.now().Sub(remoteDate)
	if skew < 0 {
		skew = -skew
	}
	const warnThreshold = 5 * time.Minute
	if skew > warnThreshold {
		r.add(Check{Category: "clock", Name: "skew", Status: StatusWarn, Detail: fmt.Sprintf("local clock differs from a remote Date header by %s", skew.Round(time.Second))})
		return
	}
	r.add(Check{Category: "clock", Name: "skew", Status: StatusOK, Detail: fmt.Sprintf("within %s of a remote Date header", skew.Round(time.Second))})
}

func runDiskChecks(r *Report, opts Options) {
	if opts.Cfg.Data.Strm != "" {
		checkDisk(r, "data-root", opts.Cfg.Data.Strm)
	}
	if opts.Cfg.Database.Path != "" {
		checkDisk(r, "database", filepath.Dir(opts.Cfg.Database.Path))
	}
}

// checkDisk runs a bounded, read-only syscall.Statfs against path and
// reports free space. A missing/unreadable path abstains (StatusFail with
// the transport error) rather than guessing a free-space figure.
func checkDisk(r *Report, name, path string) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		r.add(Check{Category: "disk", Name: name, Status: StatusFail, Detail: errClass(err)})
		return
	}
	freeBytes := stat.Bavail * uint64(stat.Bsize)
	totalBytes := stat.Blocks * uint64(stat.Bsize)
	var pctFree float64
	if totalBytes > 0 {
		pctFree = float64(freeBytes) / float64(totalBytes) * 100
	}
	status := StatusOK
	if pctFree < 5 {
		status = StatusFail
	} else if pctFree < 15 {
		status = StatusWarn
	}
	r.add(Check{Category: "disk", Name: name, Status: status, Detail: fmt.Sprintf("%.1f%% free (%s)", pctFree, humanBytes(freeBytes))})
}

func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
