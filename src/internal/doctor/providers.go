package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/alldebrid"
	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/nntp"
	"github.com/darkharrbor/darkharrbor/internal/premiumize"
	"github.com/darkharrbor/darkharrbor/internal/realdebrid"
	"github.com/darkharrbor/darkharrbor/internal/store"
	"github.com/darkharrbor/darkharrbor/internal/torbox"
)

// runProviderChecks dials each configured provider's own cheapest real,
// non-mutating account/connectivity call: TorBox's GetUserInfo,
// Real-Debrid's and AllDebrid's GetUser, Premiumize's GetAccount, and, for every usenet provider with resolvable
// credentials (the legacy HARRBOR_NNTP_* fields, plus any TorBox News
// Server credential already persisted by the live server's own factory --
// see cmd/darkharrbor/main.go's registerUsenetFactory("torbox", ...) --
// nntp.NNTPProvider.TestConnection's real connect+AUTHINFO round trip
// through a throwaway one-connection pool. A provider with no resolvable
// credentials is reported StatusSkip, never guessed.
func runProviderChecks(ctx context.Context, r *Report, opts Options, st *store.Store, recordDate func(*http.Response)) {
	runTorBoxCheck(ctx, r, opts, recordDate)
	runRealDebridCheck(ctx, r, opts)
	runAllDebridCheck(ctx, r, opts)
	runPremiumizeCheck(ctx, r, opts)
	runNNTPChecks(ctx, r, opts, st)
}

func acquireAudit(ctx context.Context, opts Options) (*accountgov.Lease, string) {
	if opts.TorrentGov == nil || opts.TorrentGovOp == "" {
		return nil, "no governor available in this context"
	}
	lease, err := opts.TorrentGov.Acquire(ctx, opts.TorrentGovOp, accountgov.PriorityAudit, "doctor")
	if err != nil {
		return nil, "governor lease: " + errClass(err)
	}
	return lease, ""
}

func runTorBoxCheck(ctx context.Context, r *Report, opts Options, recordDate func(*http.Response)) {
	token := strings.TrimSpace(opts.Cfg.TorBox.APIToken)
	if token == "" {
		r.add(Check{Category: "provider", Name: "torbox", Status: StatusSkip, Detail: "not configured"})
		r.add(Check{Category: "provider", Name: "torbox: plan/caps", Status: StatusSkip, Detail: "not configured"})
		return
	}
	lease, note := acquireAudit(ctx, opts)
	if lease != nil {
		defer lease.Release()
	}
	policy := torbox.NewTorBoxPolicy(1, 0)
	defer policy.Stop()
	client := torbox.NewHTTPClient(nil, "https://api.torbox.app/v1", token, "darkharrbor-doctor/1", perCallTimeout, policy)
	callCtx, cancel := context.WithTimeout(ctx, perCallTimeout)
	defer cancel()
	info, err := client.GetUserInfo(callCtx)
	if err != nil {
		r.add(Check{Category: "provider", Name: "torbox", Status: StatusFail, Detail: errClass(err)})
		r.add(Check{Category: "provider", Name: "torbox: plan/caps", Status: StatusFail, Detail: "unavailable: account call failed"})
		return
	}
	detail := "account reachable"
	if note != "" {
		detail += " (" + note + ")"
	}
	r.add(Check{Category: "provider", Name: "torbox", Status: StatusOK, Detail: detail})
	planStatus := StatusOK
	if _, _, unknownFallback := torbox.AccountPlan(info, opts.now()); unknownFallback {
		planStatus = StatusWarn
	}
	r.add(Check{Category: "provider", Name: "torbox: plan/caps", Status: planStatus, Detail: torboxPlanDetail(info, opts.now())})
}

// torboxPlanDetail reuses the exact same capability-resolution owner the
// setup wizard already calls (torbox.ResolveCaps/NewsServerCapable/
// AirlockQuotaBytes, torbox.PlanLabel) so plan/caps display can never drift
// from what actually governs admission -- no second discovery or
// capability-table mechanism (WORKFLOW rule 3). An unrecognized plan code
// or nil info renders distinctly via AccountPlan and is never silently
// guessed as a known or Free tier; this is the "unknown provider" fixture
// case exercised by the accompanying tests. Detail is plan/quota facts only --
// never a token, account ID, or email (DG-04).
func torboxPlanDetail(info *torbox.UserInfo, now time.Time) string {
	if info == nil {
		return "no account data (nil response)"
	}
	slots, maxBytes, usenetCapable, freeTier := torbox.ResolveCaps(info, now)
	newsServer := torbox.NewsServerCapable(info, now)
	airlockBytes := torbox.AirlockQuotaBytes(info, now)
	planName, verifiedFreeTier, unknownFallback := torbox.AccountPlan(info, now)
	detail := fmt.Sprintf("plan=%s slots=%d max_bytes=%s usenet_ingest=%t news_server=%t",
		planName, slots, humanBytes(uint64(maxBytes)), usenetCapable, newsServer)
	if airlockBytes > 0 {
		detail += fmt.Sprintf(" airlock_quota=%s", humanBytes(uint64(airlockBytes)))
	}
	if unknownFallback {
		detail += " (conservative unknown-plan fallback in effect)"
	} else if freeTier && verifiedFreeTier {
		detail += " (verified free-tier policy in effect)"
	}
	return detail
}

func runRealDebridCheck(ctx context.Context, r *Report, opts Options) {
	token, ok := opts.Cfg.Lookup("HARRBOR_RD_API_TOKEN")
	if !ok || strings.TrimSpace(token) == "" {
		r.add(Check{Category: "provider", Name: "real-debrid", Status: StatusSkip, Detail: "not configured"})
		r.add(Check{Category: "provider", Name: "real-debrid: plan/caps", Status: StatusSkip, Detail: "not configured"})
		return
	}
	lease, note := acquireAudit(ctx, opts)
	if lease != nil {
		defer lease.Release()
	}
	policy := realdebrid.NewRealDebridPolicy()
	client := realdebrid.NewHTTPClient("", token, "", 0, policy)
	callCtx, cancel := context.WithTimeout(ctx, perCallTimeout)
	defer cancel()
	rdInfo, err := client.GetUser(callCtx)
	if err != nil {
		r.add(Check{Category: "provider", Name: "real-debrid", Status: StatusFail, Detail: errClass(err)})
		r.add(Check{Category: "provider", Name: "real-debrid: plan/caps", Status: StatusFail, Detail: "unavailable: account call failed"})
		return
	}
	detail := "account reachable"
	if note != "" {
		detail += " (" + note + ")"
	}
	r.add(Check{Category: "provider", Name: "real-debrid", Status: StatusOK, Detail: detail})
	planStatus := StatusOK
	if !rdPremium(rdInfo) {
		planStatus = StatusWarn
	}
	r.add(Check{Category: "provider", Name: "real-debrid: plan/caps", Status: planStatus, Detail: realDebridPlanDetail(rdInfo)})
}

// rdPremium mirrors realDebridPlanDetail's own premium check so the Check
// status (StatusWarn on non-Premium, since RD is non-functional for DH
// without it -- realdebrid/plan.go's own doc comment) agrees with the
// rendered detail string exactly.
func rdPremium(info *realdebrid.AccountInfo) bool {
	premium, _, _ := realdebrid.ResolveCaps(info)
	return premium
}

// realDebridPlanDetail reuses the exact same realdebrid.ResolveCaps owner
// the setup wizard already calls -- no second capability table (WORKFLOW
// rule 3). Detail is plan/quota facts only -- never a token or account ID
// (DG-04).
func realDebridPlanDetail(info *realdebrid.AccountInfo) string {
	if info == nil {
		return "no account data (nil response)"
	}
	premium, slots, maxBytes := realdebrid.ResolveCaps(info)
	if !premium {
		return "non-Premium account (provider will be skipped at boot; Real-Debrid is non-functional for DH without Premium)"
	}
	maxBytesDetail := "unlimited"
	if maxBytes > 0 {
		maxBytesDetail = humanBytes(uint64(maxBytes))
	}
	return fmt.Sprintf("plan=Premium slots=%d max_bytes=%s expires_in_days=%d", slots, maxBytesDetail, info.ExpirationDays)
}

func runAllDebridCheck(ctx context.Context, r *Report, opts Options) {
	key, ok := opts.Cfg.Lookup("HARRBOR_ALLDEBRID_API_KEY")
	if !ok || strings.TrimSpace(key) == "" {
		r.add(Check{Category: "provider", Name: "alldebrid", Status: StatusSkip, Detail: "not configured"})
		r.add(Check{Category: "provider", Name: "alldebrid: plan/caps", Status: StatusSkip, Detail: "not configured"})
		return
	}
	policy := alldebrid.NewPolicy()
	defer policy.Stop()
	client := alldebrid.NewHTTPClient("", key, "darkharrbor-doctor/1", perCallTimeout, policy)
	callCtx, cancel := context.WithTimeout(ctx, perCallTimeout)
	defer cancel()
	info, err := client.GetUser(callCtx)
	if err != nil {
		r.add(Check{Category: "provider", Name: "alldebrid", Status: StatusFail, Detail: errClass(err)})
		r.add(Check{Category: "provider", Name: "alldebrid: plan/caps", Status: StatusFail, Detail: "unavailable: account call failed"})
		return
	}
	r.add(Check{Category: "provider", Name: "alldebrid", Status: StatusOK, Detail: "account reachable"})
	status := StatusOK
	detail := fmt.Sprintf("plan=Premium active_magnet_limit=%d", alldebrid.ActiveMagnetLimit)
	if info == nil || !info.Premium {
		status = StatusWarn
		detail = "non-Premium account (provider will be skipped at boot)"
	}
	r.add(Check{Category: "provider", Name: "alldebrid: plan/caps", Status: status, Detail: detail})
}

func runPremiumizeCheck(ctx context.Context, r *Report, opts Options) {
	key, ok := opts.Cfg.Lookup("HARRBOR_PREMIUMIZE_API_KEY")
	if !ok || strings.TrimSpace(key) == "" {
		r.add(Check{Category: "provider", Name: "premiumize", Status: StatusSkip, Detail: "not configured"})
		r.add(Check{Category: "provider", Name: "premiumize: plan/caps", Status: StatusSkip, Detail: "not configured"})
		return
	}
	lease, note := acquireAudit(ctx, opts)
	if lease != nil {
		defer lease.Release()
	}
	policy := premiumize.NewPolicy()
	client := premiumize.NewHTTPClient("", key, "darkharrbor-doctor/1", perCallTimeout, policy)
	callCtx, cancel := context.WithTimeout(ctx, perCallTimeout)
	defer cancel()
	info, err := client.GetAccount(callCtx)
	if err != nil {
		r.add(Check{Category: "provider", Name: "premiumize", Status: StatusFail, Detail: errClass(err)})
		r.add(Check{Category: "provider", Name: "premiumize: plan/caps", Status: StatusFail, Detail: "unavailable: account call failed"})
		return
	}
	detail := "account reachable"
	if note != "" {
		detail += " (" + note + ")"
	}
	r.add(Check{Category: "provider", Name: "premiumize", Status: StatusOK, Detail: detail})
	status := StatusOK
	if !premiumize.PremiumAt(info, opts.now()) {
		status = StatusWarn
	}
	r.add(Check{Category: "provider", Name: "premiumize: plan/caps", Status: status, Detail: premiumizePlanDetail(info, opts.now())})
}

func premiumizePlanDetail(info *premiumize.AccountInfo, now time.Time) string {
	if info == nil {
		return "no account data (nil response)"
	}
	if !premiumize.PremiumAt(info, now) {
		return "non-Premium account (provider will be skipped at boot)"
	}
	days := int64(0)
	if remaining := time.Unix(info.PremiumUntil, 0).Sub(now); remaining > 0 {
		days = int64((remaining + 24*time.Hour - 1) / (24 * time.Hour))
	}
	return fmt.Sprintf("plan=Premium fair_use_used=%.1f%% booster_points=%.0f expires_in_days=%d",
		info.LimitUsed*100, info.BoosterPoints, days)
}

// errEmptyPersistedCredential guards against an unmarshalable-but-empty
// persisted News Server credential row.
var errEmptyPersistedCredential = errors.New("doctor: persisted nntp credential has empty host/password")

// runNNTPChecks dial-tests every usenet provider it can resolve
// credentials for without needing the live server's more elaborate
// startup factory (TorBox News Server credential provisioning, in
// particular, is deliberately NOT re-run here -- a doctor sweep must never
// mutate provider state, and provisioning/resetting a password is exactly
// that). "newshosting" (the legacy HARRBOR_NNTP_* keys) is dial-tested
// directly; a TorBox News Server credential is dial-tested only if
// cmd/darkharrbor's own factory has already persisted one via
// store.SetProviderCredential -- reading it is read-only.
func runNNTPChecks(ctx context.Context, r *Report, opts Options, st *store.Store) {
	r.add(Check{Category: "provider", Name: "nntp: connection pools", Status: StatusOK, Detail: nntpOverridesDetail(opts.Cfg)})

	if opts.Cfg.NNTP.Host != "" {
		cfg := nntp.Config{
			Host:        opts.Cfg.NNTP.Host,
			Port:        opts.Cfg.NNTP.Port,
			TLS:         opts.Cfg.NNTP.TLS,
			Username:    opts.Cfg.NNTP.Username,
			Password:    opts.Cfg.NNTP.Password,
			Connections: 1,
		}
		dialNNTP(ctx, r, "newshosting", cfg)
	} else {
		r.add(Check{Category: "provider", Name: "nntp: newshosting", Status: StatusSkip, Detail: "not configured"})
	}

	raw, found, err := st.GetProviderCredential(ctx, "torbox-nntp")
	switch {
	case err != nil:
		r.add(Check{Category: "provider", Name: "nntp: torbox", Status: StatusFail, Detail: "read persisted credential: " + errClass(err)})
	case !found:
		r.add(Check{Category: "provider", Name: "nntp: torbox", Status: StatusSkip, Detail: "no persisted News Server credential (not provisioned, or server never started with it configured)"})
	default:
		cfg, decodeErr := decodeNNTPCredential(raw)
		if decodeErr != nil {
			r.add(Check{Category: "provider", Name: "nntp: torbox", Status: StatusFail, Detail: "persisted credential unusable: " + errClass(decodeErr)})
		} else {
			cfg.Connections = 1
			dialNNTP(ctx, r, "torbox", cfg)
		}
	}
}

func dialNNTP(ctx context.Context, r *Report, name string, cfg nntp.Config) {
	p := nntp.New(cfg, nil)
	defer p.Close()
	callCtx, cancel := context.WithTimeout(ctx, perCallTimeout)
	defer cancel()
	if err := p.TestConnection(callCtx); err != nil {
		r.add(Check{Category: "provider", Name: "nntp: " + name, Status: StatusFail, Detail: "connect+auth failed: " + errClass(err)})
		return
	}
	r.add(Check{Category: "provider", Name: "nntp: " + name, Status: StatusOK, Detail: "connect+auth OK (ungoverned -- NNTP lane admission is a per-provider connection-pool ceiling, not an accountgov.Governor lease, per HR1.4/NS-6.1)"})
}

// decodeNNTPCredential mirrors cmd/darkharrbor/main.go's own persisted
// News Server credential unmarshal exactly (same JSON shape), read-only.
func decodeNNTPCredential(raw string) (nntp.Config, error) {
	var cfg nntp.Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nntp.Config{}, err
	}
	if cfg.Host == "" || cfg.Password == "" {
		return nntp.Config{}, errEmptyPersistedCredential
	}
	return cfg, nil
}

// nntpKnownProviderNames are the provider identifiers this doctor sweep
// already knows how to dial (see runNNTPChecks) -- the same set
// HARRBOR_PROVIDER_<NAME>_RETENTION_DAYS overrides key off. Reused here
// rather than introducing a separate provider-name enumeration.
var nntpKnownProviderNames = []string{"newshosting", "torbox"}

// nntpOverridesDetail surfaces the NNTP lane's current effective
// operation overrides: the pool-size/stripe settings HR-D15 names plus any
// configured per-provider retention horizon, all read from already-loaded
// config -- no new parsing, no new config owner (WORKFLOW rule 3;
// config.ProviderRetentionDays is the existing shared owner NS-8.4/NS-9.1
// already use). This is a read-only display: it never mutates config, and
// nothing here is ever a secret (DG-04).
// nntpOverridesDetail reports the pools that ACTUALLY GOVERN this deployment.
//
// It previously printed cfg.Cache.TotalConnections and friends -- the GLOBAL
// NNTP defaults -- under a check named "effective overrides", while the
// per-provider HARRBOR_PROVIDER_<NAME>_* values are what the pool is built
// from and override them. The check therefore reported the very thing it is
// named for being overridden by. On 2026-08-22 it misled this project's own
// operator into believing Newshosting ran 16 connections when it runs 95, split
// 56 demand / 39 readahead. A diagnostic that is confidently wrong is worse
// than one that is absent.
//
// Globals are still shown, because they apply to any provider with no
// override, but they are now labelled as defaults rather than as effective.

// nntpProviderPools renders each provider's own pool override, which is what
// actually governs its connection budget. Deterministic ordering so repeated
// runs are diffable.
func nntpProviderPools(cfg *config.Config) string {
	var parts []string
	for _, name := range nntpKnownProviderNames {
		prefix := "HARRBOR_PROVIDER_" + strings.ToUpper(name) + "_"
		total, hasTotal := lookupInt(cfg, prefix+"TOTAL_CONNECTIONS")
		demand, hasDemand := lookupInt(cfg, prefix+"DEMAND_CONNECTIONS")
		readahead, hasReadahead := lookupInt(cfg, prefix+"READAHEAD_CONNECTIONS")
		maxPerFile, hasMaxPerFile := lookupInt(cfg, prefix+"MAX_CONNS_PER_FILE")
		if !hasTotal && !hasDemand && !hasReadahead && !hasMaxPerFile {
			continue
		}
		seg := name + "="
		if hasTotal {
			seg += fmt.Sprintf("total:%d", total)
		}
		if hasDemand && hasReadahead {
			seg += fmt.Sprintf(" split:%d/%d", demand, readahead)
		}
		if hasMaxPerFile {
			seg += fmt.Sprintf(" per_file:%d", maxPerFile)
		}
		parts = append(parts, seg)
	}
	return strings.Join(parts, ", ")
}

func lookupInt(cfg *config.Config, key string) (int, bool) {
	raw, ok := cfg.Lookup(key)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, false
	}
	return n, true
}

func nntpOverridesDetail(cfg *config.Config) string {
	detail := fmt.Sprintf("defaults: total=%d demand=%d readahead=%d stripe=%t",
		cfg.Cache.TotalConnections, cfg.Cache.DemandConnections, cfg.Cache.ReadaheadConnections, cfg.NNTP.Stripe)
	if pools := nntpProviderPools(cfg); pools != "" {
		detail += " | per-provider: " + pools
	} else {
		detail += " | per-provider: none (defaults apply to every provider)"
	}
	retention := config.ProviderRetentionDays(cfg, nntpKnownProviderNames, nil)
	if len(retention) == 0 {
		return detail + " retention_overrides=none"
	}
	parts := make([]string, 0, len(retention))
	for _, name := range nntpKnownProviderNames {
		if days, ok := retention[name]; ok {
			parts = append(parts, fmt.Sprintf("%s=%dd", name, days))
		}
	}
	return detail + " retention_overrides=" + strings.Join(parts, ",")
}
