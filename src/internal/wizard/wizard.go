// Package wizard implements the `darkharrbor setup` interactive bootstrap wizard.
//
// W1 scope: S1 (preflight), S2 (TorBox plan detection), S5 (acquisition
// preference), S6 (internal secret generation), S7 (seal + write config).
// S4 (usenet providers), S8–S11 (arr discovery,
// auto-registration, shim, validate) are in subsequent gates W2–W5.
package wizard

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/arrdiscovery"
	"github.com/darkharrbor/darkharrbor/internal/arrsetup"
	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/dockerctl"
	"github.com/darkharrbor/darkharrbor/internal/dockerpolicy"
	"github.com/darkharrbor/darkharrbor/internal/doctor"
	"github.com/darkharrbor/darkharrbor/internal/jellyfinsetup"
	"github.com/darkharrbor/darkharrbor/internal/nntp"
	"github.com/darkharrbor/darkharrbor/internal/secretinput"
	"github.com/darkharrbor/darkharrbor/internal/securefile"
	"github.com/darkharrbor/darkharrbor/internal/shiminstall"
	"github.com/darkharrbor/darkharrbor/internal/store"
	"github.com/darkharrbor/darkharrbor/internal/torbox"
	"github.com/darkharrbor/darkharrbor/internal/util"
)

// Options controls wizard invocation.
type Options struct {
	// ConfigDir is the /config bind-mount path (default: /config).
	ConfigDir string
	// DockerProxyURL is the harrbor-dockerproxy base URL for reachability
	// probe in S1; empty disables the probe (marks steps as manual).
	DockerProxyURL string
	// AnswersFile enables --headless replay; empty = interactive.
	AnswersFile string
	// Stdout / Stderr allow injection in tests; default os.Stdout/os.Stderr.
	Stdout io.Writer
	Stderr io.Writer
	// DB is the open DarkHarrbor SQLite database (must have migration 0012).
	DB *sql.DB
	// DBPath is opened only after the operator approves the onboarding plan.
	// It is ignored when DB is supplied (primarily by focused tests).
	DBPath string
	// HandoffDir receives private one-time recovery/operator files. When empty,
	// setup retains the interactive terminal fallback for manual deployments.
	HandoffDir string
	// ForceReconfigure skips the "already configured, reconfigure?" prompt.
	ForceReconfigure bool
	// DeferArrRegistration leaves S9 for a post-start reconcile-arr command.
	// The public installer uses this because a fresh DarkHarrbor endpoint does
	// not exist yet for Sonarr/Radarr's mandatory download-client live test.
	DeferArrRegistration bool
	// DialBackend overrides first-run HTTP source validation in focused tests.
	DialBackend backendDialer
}

func (o *Options) configDir() string {
	if o.ConfigDir != "" {
		return o.ConfigDir
	}
	return "/config"
}

func (o *Options) stdout() io.Writer {
	if o.Stdout != nil {
		return o.Stdout
	}
	return os.Stdout
}

func (o *Options) stderr() io.Writer {
	if o.Stderr != nil {
		return o.Stderr
	}
	return os.Stderr
}

// stdinReader is a package-level buffered reader shared across all readLine
// calls. A new bufio.Scanner per call drains the fd on first use (its 4096-byte
// buffer consumes the entire pipe), leaving all subsequent reads empty.
var stdinReader = bufio.NewReader(os.Stdin)

// Run executes the wizard setup steps in order. W1/W2 cover S1-S7; W3 adds
// S8 arr discovery + key extraction when docker-proxy is reachable.
// Each step is idempotent: if app_config records completion the step is
// refreshed (re-display) but not re-executed against external services unless
// ForceReconfigure is set.
func Run(ctx context.Context, opts Options) error {
	out := opts.stdout()
	errOut := opts.stderr()
	cfgDir := opts.configDir()
	restoreInput, err := prepareAnswers(opts.AnswersFile)
	if err != nil {
		return err
	}
	defer restoreInput()

	fmt.Fprintln(out, "Welcome to DarkHarrbor — the dark port for your media library.")
	fmt.Fprintln(out, "\n╔══════════════════════════════════════════╗")
	fmt.Fprintln(out, "║     DarkHarrbor  Bootstrap  Wizard       ║")
	fmt.Fprintln(out, "╚══════════════════════════════════════════╝")
	fmt.Fprintln(out)

	proxyURL := opts.DockerProxyURL
	if proxyURL == "" {
		proxyURL = "http://docker-proxy:2375"
	}
	inventory := inspectSetupInventory(ctx, out, proxyURL)
	plan, err := collectOnboardingChoices(out, inventory)
	if err != nil {
		return fmt.Errorf("onboarding plan: %w", err)
	}
	var initialHTTPValues, initialHTTPSecrets map[string]string
	if plan.hasLane("http") {
		fmt.Fprintln(out, "\n── HTTP Sources  First-Run Configuration ─────────────")
		initialHTTPValues, initialHTTPSecrets, err = collectInitialHTTPConfiguration(out, opts.DialBackend)
		if err != nil {
			return fmt.Errorf("HTTP source setup: %w", err)
		}
	}
	printOnboardingPlan(out, plan)
	printInitialHTTPConfiguration(out, initialHTTPValues, initialHTTPSecrets)
	if !promptYesNo(out, "  Apply this complete plan? [y/N]: ", false) {
		fmt.Fprintln(out, "  Aborted — no changes made.")
		return nil
	}
	if err := securefile.PrepareDir(cfgDir); err != nil {
		return fmt.Errorf("prepare private config directory %s: %w", cfgDir, err)
	}
	if opts.DB == nil && strings.TrimSpace(opts.DBPath) != "" {
		db, openErr := store.Open(ctx, opts.DBPath, 5*time.Second)
		if openErr != nil {
			return fmt.Errorf("open database %s: %w", opts.DBPath, openErr)
		}
		defer func() { _ = db.Close() }()
		if migrationErr := store.RunMigrationsFS(db, store.EmbeddedMigrations); migrationErr != nil {
			return fmt.Errorf("run database migrations: %w", migrationErr)
		}
		opts.DB = db
	}
	if opts.DB == nil {
		return fmt.Errorf("setup database is required after plan approval")
	}

	// ── S1: Preflight ────────────────────────────────────────────────────────
	fmt.Fprintln(out, "── S1  Preflight ──────────────────────────────────────")
	// WZ-02. Stage IDs are STABLE REFERENCES used across the register, the
	// WORKLOG and this documentation -- they are deliberately NOT a running
	// order, and renumbering them would invalidate every historical citation.
	// Say so plainly, because a user watching S2 be followed by S4 and then S8
	// has no way to tell whether something was skipped.
	fmt.Fprintln(out, "  ℹ Stage IDs are stable labels, not a running order.")
	fmt.Fprintln(out, "    Actual order: S1, S2, S2b, S3, S3b, S4, S8, S5, S6, S9, S7, S7c, S7b, S7d, S10, S10b, S11, S12.")
	listenAddress := strings.TrimSpace(os.Getenv("HARRBOR_SERVER_ADDRESS"))
	if listenAddress == "" {
		listenAddress = "127.0.0.1:8381"
	}
	if wildcardListenAddress(listenAddress) {
		fmt.Fprintln(errOut, "  ⚠ DarkHarrbor listens on all interfaces. Keep the service unpublished on a trusted container network, or place a TLS reverse proxy with network access controls in front of it.")
	}

	// Existing config detection.
	marker, _ := appConfigGet(opts.DB, "bootstrap_complete")
	if marker != "" && !opts.ForceReconfigure {
		fmt.Fprintf(out, "  ✓ Existing configuration detected (completed %s).\n", marker)
		fmt.Fprint(out, "  Reconfigure? [y/N]: ")
		ans := readLine(out)
		if !strings.EqualFold(ans, "y") {
			fmt.Fprintln(out, "  Aborted — no changes made.")
			return nil
		}
	}

	fmt.Fprintf(out, "  ✓ Config dir private and writable: %s (mode 0700)\n", cfgDir)

	// Docker-proxy reachability (needed for S8 discovery and S10 auto-install;
	// authenticated S8 manual entry remains available without it).
	dockerProxyOK := inventory.ProxyOK
	if dockerProxyOK {
		fmt.Fprintf(out, "  ✓ Docker proxy reachable: %s\n", proxyURL)
	} else {
		fmt.Fprintf(errOut, "  ⚠ Docker proxy unreachable (%s) — use authenticated manual Arr entry; S10 auto-install is unavailable.\n", proxyURL)
	}
	planJSON, _ := json.Marshal(plan)
	_ = appConfigSet(opts.DB, "onboarding_plan", string(planJSON))
	_ = appConfigSet(opts.DB, "s1_complete", time.Now().UTC().Format(time.RFC3339))
	_ = appConfigSet(opts.DB, "docker_proxy_ok", boolStr(dockerProxyOK))

	now := time.Now().UTC()
	var discoveredArrs []discoveredArrInstance
	var runtimeConnectorTargets []string

	// ── S2: TorBox plan detection ────────────────────────────────────────────
	//
	// WZ-01. TorBox was previously mandatory here while every sibling provider
	// was implicitly optional. The reviewed provider-neutral plan now decides
	// which provider stages exist; a selected provider requires verification,
	// while an unselected provider consumes no credential input.
	//
	// Plan/capability/slot self-configuration still runs, but ONLY when a token
	// is supplied -- that detection is the real reason this stage is more than
	// a credential prompt, and it is meaningless without an account.
	fmt.Fprintln(out, "\n── S2  TorBox + Plan Self-Config ─────────────────────")

	var token string
	if plan.hasProvider("torbox") {
		token, err = promptSecret(out, "  TorBox API token: ")
		if err != nil {
			return fmt.Errorf("S2: read token: %w", err)
		}
		if strings.TrimSpace(token) == "" {
			return fmt.Errorf("S2: TorBox was selected but no API token was supplied")
		}
	} else {
		fmt.Fprintln(out, "  · Skipped: TorBox was not selected.")
	}
	torboxConfigured := strings.TrimSpace(token) != ""

	// Build a minimal TorBox client for the single GetUserInfo capability-discovery call.
	// No rate-limit policy needed: one GET, no retries, 10s timeout.
	tbPolicy := torbox.NewTorBoxPolicy(1, 0)
	defer tbPolicy.Stop()
	tbClient := torbox.NewHTTPClient(
		slog.Default(),
		"https://api.torbox.app/v1",
		strings.TrimSpace(token),
		"darkharrbor-wizard/1",
		10*time.Second,
		tbPolicy,
	)
	// Defaults for a TorBox-less deployment. usenetCapable and newsServer stay
	// FALSE so S4 does not offer TorBox's News Server, and freeTier stays false
	// so S5 does not hard-suggest RequireCached on the strength of a plan that
	// was never queried. These are absences, not assumptions.
	var (
		slots                                 int
		maxBytes, airlockBytes                int64
		usenetCapable, conservativeFreePolicy bool
		verifiedFreeTier, unknownPlanFallback bool
		newsServer                            bool
		planName                              = "not configured"
	)

	if torboxConfigured {
		info, err := tbClient.GetUserInfo(ctx)
		if err != nil {
			return fmt.Errorf("S2: TorBox API call failed: %w", err)
		}
		slots, maxBytes, usenetCapable, conservativeFreePolicy = torbox.ResolveCaps(info, now)
		newsServer = torbox.NewsServerCapable(info, now)
		airlockBytes = torbox.AirlockQuotaBytes(info, now)
		planName, verifiedFreeTier, unknownPlanFallback = torbox.AccountPlan(info, now)
	} else {
		fmt.Fprintln(out, "  ℹ TorBox skipped. Plan detection, News Server and AirLock are unavailable.")
		fmt.Fprintln(out, "    At least one acquisition lane is still required (debrid or NNTP); verified before sealing.")
	}
	otherDebrid, err := promptOtherDebridProviders(ctx, out, plan.DebridProviders)
	if err != nil {
		return err
	}

	if torboxConfigured {
		fmt.Fprintf(out, "  ✓ TorBox account verified.\n")
		fmt.Fprintf(out, "    Plan:          %s\n", planName)
		fmt.Fprintf(out, "    Active slots:  %d\n", slots)
		fmt.Fprintf(out, "    Max bandwidth: %s\n", fmtBytes(maxBytes))
		fmt.Fprintf(out, "    Usenet ingest: %s\n", yesNo(usenetCapable))
		fmt.Fprintf(out, "    News server:   %s\n", yesNo(newsServer))
		if airlockBytes > 0 {
			fmt.Fprintf(out, "    AirLock quota: %s\n", fmtBytes(airlockBytes))
		}
		if verifiedFreeTier {
			fmt.Fprintf(out, "    ℹ Free tier — RequireCached will be hard-suggested.\n")
		}
		if unknownPlanFallback {
			fmt.Fprintln(out, "    ⚠ Unrecognized plan — conservative limits apply; no paid capability is assumed.")
		}
	}

	// Persist plan caps to app_config.
	capsJSON, _ := json.Marshal(map[string]interface{}{
		"configured":               torboxConfigured,
		"plan_name":                planName,
		"slots":                    slots,
		"max_bytes":                maxBytes,
		"usenet_capable":           usenetCapable,
		"news_server":              newsServer,
		"airlock_bytes":            airlockBytes,
		"free_tier":                verifiedFreeTier,
		"conservative_free_policy": conservativeFreePolicy,
		"unknown_plan_fallback":    unknownPlanFallback,
		"resolved_at":              now.Format(time.RFC3339),
	})
	_ = appConfigSet(opts.DB, "provider_caps", string(capsJSON))
	_ = appConfigSet(opts.DB, "torbox_plan", planName)
	_ = appConfigSet(opts.DB, "s2_complete", now.Format(time.RFC3339))

	// ── S4: Usenet providers ─────────────────────────────────────────────────
	fmt.Fprintln(out, "\n── S4  Usenet Providers ───────────────────────────────")

	var usenetSecrets []string
	var nntpAvailable bool
	if plan.hasLane("nntp") {
		usenetSecrets, nntpAvailable, err = configureUsenetProviders(ctx, out, opts.DB, tbClient, plan.UsenetProviders, newsServer)
		if err != nil {
			return fmt.Errorf("S4: %w", err)
		}
	} else {
		fmt.Fprintln(out, "  · Skipped: the goal plan did not select the NNTP lane.")
	}
	if nntpAvailable {
		fmt.Fprintln(out, "  ✓ Ordered usenet providers configured.")
	} else {
		fmt.Fprintln(out, "  ℹ No NNTP providers configured; nntp_nzb remains unavailable.")
	}
	if plan.hasLane("nntp") && !usableUsenetSource(torboxConfigured, usenetCapable, nntpAvailable) {
		return fmt.Errorf("S4: Usenet was selected, but authenticated provider capabilities expose neither cached NZB nor direct NNTP; rerun setup and choose a usable provider or omit Usenet")
	}
	_ = appConfigSet(opts.DB, "s4_complete", now.Format(time.RFC3339))

	// ── S8: Arr connection + key extraction ──────────────────────────────────
	// Provider/account prerequisites are validated first. Discovery or manual
	// entry then converges on one target model before S9 registration.
	fmt.Fprintln(out, "\n── S8  Arr Connection + Key Extraction ────────────────")
	if plan.ArrConnection == "discover" && dockerProxyOK {
		selectedServices := append([]string(nil), plan.SelectedArrs...)
		if plan.ProwlarrName != "" {
			selectedServices = append(selectedServices, plan.ProwlarrName)
		}
		discoveredArrs, err = runArrDiscovery(ctx, out, proxyURL, plan.Prowlarr, selectedServices)
		if err != nil {
			return fmt.Errorf("S8: %w", err)
		}
	} else if plan.ArrConnection == "manual" {
		discoveredArrs, err = promptManualArrInstances(ctx, out, plan.Prowlarr)
		if err != nil {
			return fmt.Errorf("S8: %w", err)
		}
	} else {
		return fmt.Errorf("S8: scoped discovery was selected but Docker proxy %s is unreachable; start the proxy or rerun setup and choose manual entry", proxyURL)
	}
	if err := persistArrInstancesToDB(ctx, opts.DB, discoveredArrs); err != nil {
		return fmt.Errorf("S8: persist targets: %w", err)
	}
	_ = appConfigSet(opts.DB, "s8_complete", now.Format(time.RFC3339))
	if len(discoveredArrs) > 0 {
		names := make([]string, 0, len(discoveredArrs))
		for _, instance := range discoveredArrs {
			names = append(names, instance.Name)
		}
		fmt.Fprintf(out, "  ℹ Later stages will use these instances: %s\n", strings.Join(names, ", "))
	}
	runtimeConnectorTargets = runtimeArrConnectorTargets(plan, discoveredArrs)

	// ── S5: Acquisition preference ───────────────────────────────────────────
	fmt.Fprintln(out, "\n── S5  Acquisition Preference ─────────────────────────")

	var preference []string
	if plan.hasLane("torrent") || plan.hasLane("nntp") {
		preference, err = promptPreference(out, torboxConfigured || otherDebrid.any(), torboxConfigured, usenetCapable, nntpAvailable, verifiedFreeTier)
		if err != nil {
			return fmt.Errorf("S5: %w", err)
		}
		fmt.Fprintf(out, "  ✓ HARRBOR_PREFERENCE=%s\n", strings.Join(preference, ","))
		_ = appConfigSet(opts.DB, "harrbor_preference", strings.Join(preference, ","))
	} else {
		fmt.Fprintln(out, "  · Skipped: HTTP backends are configured independently of torrent/NNTP preference.")
	}
	_ = appConfigSet(opts.DB, "s5_complete", now.Format(time.RFC3339))

	// ── S6: Internal secret generation ───────────────────────────────────────
	fmt.Fprintln(out, "\n── S6  Internal Secrets ───────────────────────────────")

	qbitPw, err := util.RandomHex(32)
	if err != nil {
		return fmt.Errorf("S6: generate QBIT_PASSWORD: %w", err)
	}
	sabKey, err := util.RandomHex(32)
	if err != nil {
		return fmt.Errorf("S6: generate SAB_API_KEY: %w", err)
	}
	streamSecret, err := util.RandomHex(32)
	if err != nil {
		return fmt.Errorf("S6: generate STREAM_SECRET: %w", err)
	}
	fmt.Fprintln(out, "  ✓ HARRBOR_QBIT_PASSWORD generated (64 hex chars)")
	fmt.Fprintln(out, "  ✓ HARRBOR_SAB_API_KEY generated (64 hex chars)")
	fmt.Fprintln(out, "  ✓ HARRBOR_STREAM_SECRET generated (64 hex chars)")
	var mediaFlowPassword string
	if plan.Aggregator {
		mediaFlowPassword, err = util.RandomHex(32)
		if err != nil {
			return fmt.Errorf("S6: generate MEDIAFLOW_PASSWORD: %w", err)
		}
		fmt.Fprintln(out, "  ✓ HARRBOR_MEDIAFLOW_PASSWORD generated (64 hex chars)")
	} else {
		fmt.Fprintln(out, "  · MediaFlow credential skipped with aggregator integration")
	}
	var stremioInstallToken string
	if plan.StremioAddon {
		stremioInstallToken, err = util.RandomHex(32)
		if err != nil {
			return fmt.Errorf("S6: generate STREMIO_INSTALL_TOKEN: %w", err)
		}
		fmt.Fprintln(out, "  ✓ HARRBOR_STREMIO_INSTALL_TOKEN generated (64 hex chars)")
	} else {
		fmt.Fprintln(out, "  · Stremio install token skipped by plan")
	}
	_ = appConfigSet(opts.DB, "s6_complete", now.Format(time.RFC3339))

	// ── S9: Arr auto-registration (before S7 seal, using in-memory creds) ────
	fmt.Fprintln(out, "\n── S9  Arr Auto-Registration ──────────────────────────")
	if plan.BaseArrTopology && len(discoveredArrs) > 0 {
		lanes := arrsetup.Lanes{Torrent: plan.hasLane("torrent"), NNTP: plan.hasLane("nntp"), HTTP: plan.hasLane("http")}
		if opts.DeferArrRegistration {
			fmt.Fprintln(out, "  · Deferred until DarkHarrbor is healthy; the installer will run lane-scoped reconciliation.")
		} else {
			if err := runArrSetupDirect(ctx, out, discoveredArrs, arrsetup.Creds{QBitPassword: qbitPw, SABAPIKey: sabKey}, lanes); err != nil {
				return fmt.Errorf("S9: %w", err)
			}
			_ = appConfigSet(opts.DB, "s9_complete", now.Format(time.RFC3339))
		}
	} else if !plan.BaseArrTopology {
		fmt.Fprintln(out, "  · Skipped by plan; reactive-only use does not require indexers or download clients.")
	}

	// ── S7: Seal all secrets in one shot (after S8 arr keys are known) ────────
	fmt.Fprintln(out, "\n── S7  Seal + Write Config ────────────────────────────")

	// Build secrets plaintext: TorBox + S6 generated + S4 NNTP + S8 arr keys.
	var secretsBuf strings.Builder
	fmt.Fprintf(&secretsBuf, "HARRBOR_TORBOX_API_TOKEN=%s\n", strings.TrimSpace(token))
	fmt.Fprintf(&secretsBuf, "HARRBOR_QBIT_PASSWORD=%s\n", qbitPw)
	// WZ-01. THE ACQUISITION-LANE RULE.
	//
	// Enforced HERE, not at S2. Stage LABELS do not match execution order --
	// the wizard runs S1, S2, S4, S5, S6, S8, S9, S2b, S3, S3b, S7 -- so this
	// is the first point at which every lane is known, and it is still before
	// S7 seals anything, which makes it the last point where the operator can
	// act on the failure without a rollback.
	//
	// Failing at S2 could only ever name TorBox, which is exactly what made
	// TorBox wrongly mandatory. The message names EVERY acceptable lane, so an
	// operator with none learns what would satisfy the requirement rather than
	// being told about the first one the code happened to check.
	if !torboxConfigured && !otherDebrid.any() && !nntpAvailable && !plan.hasLane("http") {
		return fmt.Errorf("S7: no acquisition lane is configured. DarkHarrbor needs at " +
			"least one source of content: a debrid provider (TorBox at S2, Real-Debrid at " +
			"S2b, AllDebrid at S3, or Premiumize at S3b) or at least one NNTP provider at " +
			"S4. Re-run setup and configure at least one of them")
	}

	// WZ-01: "torbox" is no longer unconditional. Declaring a provider that was
	// never configured produced a HARRBOR_PROVIDERS naming a lane with no
	// credentials, which fails at acquisition time rather than at setup time.
	if otherDebrid.RealDebrid != "" {
		fmt.Fprintf(&secretsBuf, "HARRBOR_RD_API_TOKEN=%s\n", otherDebrid.RealDebrid)
	}
	if otherDebrid.AllDebrid != "" {
		fmt.Fprintf(&secretsBuf, "HARRBOR_ALLDEBRID_API_KEY=%s\n", otherDebrid.AllDebrid)
	}
	if otherDebrid.Premiumize != "" {
		fmt.Fprintf(&secretsBuf, "HARRBOR_PREMIUMIZE_API_KEY=%s\n", otherDebrid.Premiumize)
	}
	providers := append([]string(nil), plan.DebridProviders...)
	providerSelection := strings.Join(providers, ",")
	if providerSelection == "" {
		providerSelection = config.PreferenceNone
	}
	fmt.Fprintf(&secretsBuf, "HARRBOR_PROVIDERS=%s\n", providerSelection)
	fmt.Fprintf(&secretsBuf, "HARRBOR_SAB_API_KEY=%s\n", sabKey)
	fmt.Fprintf(&secretsBuf, "HARRBOR_STREAM_SECRET=%s\n", streamSecret)
	if mediaFlowPassword != "" {
		fmt.Fprintf(&secretsBuf, "HARRBOR_MEDIAFLOW_PASSWORD=%s\n", mediaFlowPassword)
	}
	fmt.Fprintf(&secretsBuf, "HARRBOR_STREMIO_INSTALL_TOKEN=%s\n", stremioInstallToken)
	preferenceSelection := strings.Join(preference, ",")
	if preferenceSelection == "" {
		preferenceSelection = config.PreferenceNone
	}
	fmt.Fprintf(&secretsBuf, "HARRBOR_PREFERENCE=%s\n", preferenceSelection)
	for _, line := range usenetSecrets {
		fmt.Fprintln(&secretsBuf, line)
	}
	// Arr API keys and URLs from S8.
	// Prowlarr selection-mode keys are also sealed here so HARRBOR_PROWLARR_BASE_URL
	// and HARRBOR_PROWLARR_API_KEY are available at boot without a compose override.
	var notifyNames []string
	for _, inst := range discoveredArrs {
		fmt.Fprintf(&secretsBuf, "HARRBOR_ARR_%s_URL=%s\n", envSuffix(inst.Name), inst.URL)
		fmt.Fprintf(&secretsBuf, "%s=%s\n", inst.APIKeyRef, inst.APIKey)
		if inst.AppType == string(arrdiscovery.AppSonarr) || inst.AppType == string(arrdiscovery.AppRadarr) {
			notifyNames = append(notifyNames, inst.Name)
		}
		if inst.AppType == string(arrAppProwlarr) {
			// Seal selection-mode config so DH boots into selection mode without
			// a compose override. These are the two keys config.go reads for
			// HARRBOR_PROWLARR_BASE_URL / HARRBOR_PROWLARR_API_KEY.
			fmt.Fprintf(&secretsBuf, "HARRBOR_PROWLARR_BASE_URL=%s\n", inst.URL)
			fmt.Fprintf(&secretsBuf, "HARRBOR_PROWLARR_API_KEY=%s\n", inst.APIKey)
		}
	}
	if len(notifyNames) > 0 {
		sort.Strings(notifyNames)
		fmt.Fprintf(&secretsBuf, "HARRBOR_ARR_NAMES=%s\n", strings.Join(notifyNames, ","))
	}
	initialHTTPSecretKeys := make([]string, 0, len(initialHTTPSecrets))
	for key := range initialHTTPSecrets {
		initialHTTPSecretKeys = append(initialHTTPSecretKeys, key)
	}
	sort.Strings(initialHTTPSecretKeys)
	for _, key := range initialHTTPSecretKeys {
		fmt.Fprintf(&secretsBuf, "%s=%s\n", key, initialHTTPSecrets[key])
	}

	secretsPlain := []byte(secretsBuf.String())
	count, err := config.ValidateSecretLines(secretsPlain)
	if err != nil {
		return fmt.Errorf("S7: secret validation: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("S7: no secret lines produced")
	}

	// Generate password and seal.
	sealPassword, err := util.RandomHex(32)
	if err != nil {
		return fmt.Errorf("S7: generate seal password: %w", err)
	}
	blob, err := util.SealSecrets(secretsPlain, sealPassword)
	if err != nil {
		return fmt.Errorf("S7: seal: %w", err)
	}

	// Write sealed file.
	sealedPath := secretOutputPath("HARRBOR_SECRETS_FILE", filepath.Join(cfgDir, "secrets.sealed"))
	if err := securefile.AtomicWrite(sealedPath, blob); err != nil {
		return fmt.Errorf("S7: write %s: %w", sealedPath, err)
	}
	fmt.Fprintf(out, "  ✓ Sealed file written: %s (%d secret lines)\n", sealedPath, count)

	keyPath := secretOutputPath("HARRBOR_SECRETS_KEY_FILE", filepath.Join(cfgDir, "secrets.key"))
	if keyPath == sealedPath {
		return fmt.Errorf("S7: sealed secrets and key file must use different paths")
	}
	if err := securefile.PrepareDir(filepath.Dir(keyPath)); err != nil {
		return fmt.Errorf("S7: prepare key directory %s: %w", filepath.Dir(keyPath), err)
	}
	if err := securefile.AtomicWrite(keyPath, []byte(sealPassword+"\n")); err != nil {
		return fmt.Errorf("S7: write key file %s: %w", keyPath, err)
	}
	fmt.Fprintf(out, "  ✓ Key file written: %s\n", keyPath)
	fmt.Fprintln(out, "  ✓ Boot key persisted in the configured private key location")
	if strings.TrimSpace(opts.HandoffDir) != "" {
		if err := writeSetupHandoff(opts.HandoffDir, sealPassword, mediaFlowPassword); err != nil {
			return fmt.Errorf("S7: write private operator handoff: %w", err)
		}
		fmt.Fprintf(out, "  ✓ Private operator handoff written under %s; no credential was printed\n", filepath.Clean(opts.HandoffDir))
	} else {
		printSetupSecrets(errOut, sealPassword, mediaFlowPassword)
	}

	_ = appConfigSet(opts.DB, "s7_complete", now.Format(time.RFC3339))

	// Round-trip verification: unseal the file we just wrote.
	if err := verifySeal(sealedPath, sealPassword, count); err != nil {
		return fmt.Errorf("S7: round-trip unseal verification failed: %w", err)
	}
	fmt.Fprintln(out, "  ✓ Sealed file round-trip verified (unseal → correct line count)")
	if plan.StremioAddon {
		if err := runStremioEdgeSetup(out, cfgDir, streamSecret, stremioInstallToken, opts.HandoffDir); err != nil {
			return fmt.Errorf("S7c: %w", err)
		}
	} else {
		fmt.Fprintln(out, "\n── S7c Stremio Client Edge ────────────────────────────")
		fmt.Fprintln(out, "  · Skipped by plan; no client-facing addon URL was published.")
	}
	if plan.Aggregator {
		fmt.Fprintln(out, "\n── Existing AIOStreams handoff ───────────────────────")
		fmt.Fprintln(out, "  Preserve the existing image, data volume, saved user, services, and StremThru settings.")
		fmt.Fprintln(out, "  Add these AIOStreams environment values and recreate only its container:")
		fmt.Fprintln(out, "    FORCE_PROXY_ENABLED=true")
		fmt.Fprintln(out, "    FORCE_PROXY_ID=mediaflow")
		fmt.Fprintln(out, "    FORCE_PROXY_URL=http://darkharrbor:8381")
		fmt.Fprintln(out, "    FORCE_PROXY_PROXIED_SERVICES=[]")
		fmt.Fprintln(out, "    FORCE_PROXY_DISABLE_PROXIED_ADDONS=true")
		fmt.Fprintln(out, "    FORCE_PROXY_CREDENTIALS=<HARRBOR_MEDIAFLOW_PASSWORD from the private handoff or manual terminal fallback>")
		fmt.Fprintln(out, "    DISABLE_RATE_LIMITS=true")
		fmt.Fprintln(out, "    NODE_OPTIONS=--dns-result-order=ipv4first")
		fmt.Fprintln(out, "  Add the Library manifest from the private handoff (or manual terminal fallback) to the saved AIOStreams user, then save.")
		fmt.Fprintln(out, "  Copy the resulting Direct Manifest URL only into the hidden prompt from:")
		fmt.Fprintln(out, "    darkharrbor configure --existing-aiostreams")
	}
	if err := runPerformanceSetup(out, cfgDir, plan.Performance); err != nil {
		return fmt.Errorf("performance profile: %w", err)
	}
	if err := persistInitialHTTPConfiguration(cfgDir, initialHTTPValues); err != nil {
		return fmt.Errorf("HTTP runtime configuration: %w", err)
	}
	if err := persistPlanSwitches(cfgDir, plan.hasLane("http"), plan.ReactiveCommit); err != nil {
		return fmt.Errorf("reviewed plan switches: %w", err)
	}
	if err := runWrapperRepairSetup(out, cfgDir, plan.WrapperRepair); err != nil {
		return fmt.Errorf("wrapper repair: %w", err)
	}

	// ── S7b: Scheduled backup defaults ───────────────────────────────────────
	if err := runBackupSetup(out, cfgDir, plan.Backups); err != nil {
		return fmt.Errorf("S7b: %w", err)
	}
	_ = appConfigSet(opts.DB, "s7b_complete", now.Format(time.RFC3339))
	if plan.ReactiveCommit {
		if err := runReactiveSetup(out, cfgDir, opts.DB, discoveredArrs, plan); err != nil {
			return fmt.Errorf("S7d: %w", err)
		}
	} else {
		fmt.Fprintln(out, "\n── S7d Reactive Library ───────────────────────────────")
		fmt.Fprintln(out, "  · Skipped by plan; playback will not add items to an Arr.")
	}
	_ = appConfigSet(opts.DB, "s7d_complete", now.Format(time.RFC3339))

	// ── S10: Shim install ────────────────────────────────────────────────────
	fmt.Fprintln(out, "\n── S10 Shim Install ───────────────────────────────────")
	var shimResults []s10Result
	if plan.WrapperRepair && dockerProxyOK {
		shimResults = runShimInstall(ctx, out, proxyURL, plan.SelectedArrs)
		_ = appConfigSet(opts.DB, "s10_complete", now.Format(time.RFC3339))
	} else if !plan.BaseArrTopology {
		fmt.Fprintln(out, "  · Skipped by plan; reactive-only direct ManualImport does not require the optional ffprobe relay.")
	} else {
		fmt.Fprintln(out, "  ⚠ Scoped discovery/repair was not selected; S10 skipped.")
		fmt.Fprintln(out, "    externally pending: install the documented ffprobe shim in each manually entered Arr")
	}

	// ── S10b: Jellyfin ffmpeg wrapper + bitrate config ───────────────────────
	fmt.Fprintln(out, "\n── S10b Jellyfin Setup ────────────────────────────────")
	if plan.Jellyfin && dockerProxyOK {
		proxyClient := dockerctl.New(proxyURL, nil)
		jfResult, jfErr := jellyfinsetup.Setup(ctx, out, proxyClient, plan.JellyfinName)
		if jfErr != nil {
			fmt.Fprintf(out, "  ⚠ Jellyfin setup error: %v\n", jfErr)
		} else if jfResult == nil {
			fmt.Fprintln(out, "  ℹ No Jellyfin container found — skipped.")
		} else {
			fmt.Fprintf(out, "  ✓ Jellyfin %s: wrapper=%s bitrate=%s ffmpegEnv=%v\n",
				jfResult.ContainerName, jfResult.WrapperInstalled, jfResult.BitrateConfigured, jfResult.FFmpegEnvOK)
			runtimeConnectorTargets = append(runtimeConnectorTargets, jfResult.ContainerName)
			for _, w := range jfResult.Warnings {
				fmt.Fprintf(out, "    ⚠ %s\n", w)
			}
			if jfResult.Err != nil {
				fmt.Fprintf(out, "    ⚠ partial failure: %v\n", jfResult.Err)
			}
		}
		_ = appConfigSet(opts.DB, "s10b_complete", now.Format(time.RFC3339))
	} else if !plan.Jellyfin {
		fmt.Fprintln(out, "  · Skipped by plan; Jellyfin was not changed.")
	} else {
		fmt.Fprintln(out, "  ⚠ Docker proxy unavailable; Jellyfin setup skipped.")
		fmt.Fprintln(out, "    Manual: install ffmpeg wrapper to /config/ffmpeg-wrapper/ffmpeg")
		fmt.Fprintln(out, "    Manual: set JELLYFIN_FFMPEG=/config/ffmpeg-wrapper/ffmpeg in Jellyfin compose")
		fmt.Fprintln(out, "    Manual: set RemoteClientBitrateLimit=0 in Jellyfin server config")
	}
	if strings.TrimSpace(opts.HandoffDir) != "" {
		if err := writeConnectorTargetsHandoff(opts.HandoffDir, runtimeConnectorTargets); err != nil {
			return fmt.Errorf("S10: write runtime connector targets: %w", err)
		}
		fmt.Fprintln(out, "  ✓ Runtime connector access narrowed to selected managed services")
	}
	// ── S11: Validate + finalize ──────────────────────────────────────────────
	fmt.Fprintln(out, "\n── S11 Validate + Finalize ────────────────────────────")
	if err := runValidate(ctx, out, opts.DB, discoveredArrs, shimResults, plan); err != nil {
		return fmt.Errorf("S11: %w", err)
	}
	_ = appConfigSet(opts.DB, "s11_complete", now.Format(time.RFC3339))

	// ── S12: Zero-account demo (ONB-01) ───────────────────────────────────────
	fmt.Fprintln(out, "\n── S12 Zero-Account Demo ──────────────────────────────")
	// WZ-02. The demo grabs and plays a real public-domain movie, so it needs
	// BOTH a Radarr to commit to AND an acquisition lane able to fetch it.
	// Previously only the Radarr half was checked, so a lane-less deployment
	// reached the prompt and failed inside the demo with an acquisition error
	// that named nothing about the missing lane.
	// Retained as a BACKSTOP only. The S5 acquisition-lane rule already stops a
	// lane-less deployment reaching this point, so this branch is unreachable
	// via `setup`; the reachable equivalent lives in `darkharrbor demo`. Kept
	// so that any future path which loosens S5 cannot silently reintroduce the
	// failure, and marked so nobody mistakes it for live behaviour.
	if !plan.Demo {
		fmt.Fprintln(out, "  · Skipped by plan. Re-run anytime with `darkharrbor demo`.")
	} else if radarrTarget, ok := firstRadarrTarget(discoveredArrs); ok {
		runDemoStep(ctx, out, radarrTarget)
	} else {
		fmt.Fprintln(out, "  ⚠ No Radarr discovered; demo skipped. Re-run anytime with `darkharrbor demo` once Radarr is reachable.")
	}
	_ = appConfigSet(opts.DB, "s12_complete", now.Format(time.RFC3339))

	// ── Doctor diagnostics (ONB-02), auto-run at wizard end ──────────────────
	fmt.Fprintln(out, "\n── Doctor Diagnostics ─────────────────────────────────")
	runWizardDoctor(ctx, out, opts.DB, opts.DockerProxyURL)

	// Write bootstrap_complete marker.
	_ = appConfigSet(opts.DB, "bootstrap_complete", now.Format(time.RFC3339))
	printSetupOutcome(out, plan)
	return nil
}

func writeSetupHandoff(dir, recoveryKey, mediaFlowPassword string) error {
	dir = filepath.Clean(strings.TrimSpace(dir))
	if err := securefile.PrepareDir(dir); err != nil {
		return err
	}
	for _, name := range []string{"recovery.key", "mediaflow.env", "stremio-install.url", "connector-targets"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("clear stale handoff %s: %w", name, err)
		}
	}
	if err := securefile.AtomicWrite(filepath.Join(dir, "recovery.key"), []byte(recoveryKey+"\n")); err != nil {
		return fmt.Errorf("recovery key: %w", err)
	}
	if mediaFlowPassword != "" {
		data := []byte("HARRBOR_MEDIAFLOW_PASSWORD=" + mediaFlowPassword + "\n")
		if err := securefile.AtomicWrite(filepath.Join(dir, "mediaflow.env"), data); err != nil {
			return fmt.Errorf("MediaFlow credential: %w", err)
		}
	}
	return nil
}

func writeConnectorTargetsHandoff(dir string, targets []string) error {
	if len(targets) > 64 {
		return errors.New("too many runtime connector targets")
	}
	seen := make(map[string]bool, len(targets))
	clean := make([]string, 0, len(targets))
	for _, target := range targets {
		target = strings.TrimSpace(target)
		if !validConnectorTargetName(target) {
			return fmt.Errorf("invalid runtime connector target %q", target)
		}
		if !seen[target] {
			seen[target] = true
			clean = append(clean, target)
		}
	}
	sort.Strings(clean)
	return securefile.AtomicWrite(filepath.Join(filepath.Clean(dir), "connector-targets"), []byte(strings.Join(clean, "\n")+"\n"))
}

func runtimeArrConnectorTargets(plan onboardingPlan, instances []discoveredArrInstance) []string {
	if !plan.WrapperRepair {
		return nil
	}
	var targets []string
	for _, instance := range instances {
		if instance.AppType == string(arrdiscovery.AppSonarr) || instance.AppType == string(arrdiscovery.AppRadarr) {
			targets = append(targets, instance.Name)
		}
	}
	return targets
}

func validConnectorTargetName(name string) bool {
	if len(name) == 0 || len(name) > 255 {
		return false
	}
	for i, r := range name {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || (i > 0 && (r == '_' || r == '.' || r == '-')) {
			continue
		}
		return false
	}
	return true
}

func printSetupSecrets(out io.Writer, recoveryKey, mediaFlowPassword string) {
	fmt.Fprintln(out, "  ┌──────────────────────────────────────────────────")
	fmt.Fprintln(out, "  │  STORE THIS PASSWORD NOW — it will not be shown again.")
	fmt.Fprintln(out, "  │  It is the recovery credential; canonical Compose boots from secrets.key.")
	fmt.Fprintln(out, "  │")
	fmt.Fprintf(out, "  │  HARRBOR_SECRETS_PASSWORD=%s\n", recoveryKey)
	fmt.Fprintln(out, "  └──────────────────────────────────────────────────")
	if mediaFlowPassword != "" {
		fmt.Fprintln(out, "  ┌──────────────────────────────────────────────────")
		fmt.Fprintln(out, "  │  COPY ONCE into the aggregator's FORCE_PROXY_CREDENTIALS api_password.")
		fmt.Fprintf(out, "  │  HARRBOR_MEDIAFLOW_PASSWORD=%s\n", mediaFlowPassword)
		fmt.Fprintln(out, "  └──────────────────────────────────────────────────")
	}
}

// inspectSetupInventory performs the wizard's read-only discovery preview.
// The scoped connector returns sanitized container metadata only; no API key,
// environment, mount, label, or full Docker inspect data reaches this path.
func inspectSetupInventory(ctx context.Context, out io.Writer, proxyURL string) setupInventory {
	fmt.Fprintln(out, "── Discover  Existing Services ────────────────────────")
	client := dockerctl.New(proxyURL, &http.Client{Timeout: 3 * time.Second})
	containers, err := client.ListContainers(ctx)
	if err != nil {
		fmt.Fprintln(out, "  ℹ Scoped connector unavailable; Arr URLs and API keys can be entered manually.")
		return setupInventory{}
	}
	inventory := setupInventory{ProxyOK: true}
	for _, container := range containers {
		if container.State != "running" {
			continue
		}
		name := primaryContainerName(container.Names)
		if app := arrdiscovery.MatchApp(container.Image); app != "" {
			inventory.Arrs = append(inventory.Arrs, name)
			fmt.Fprintf(out, "  ✓ %s (%s)\n", name, app)
			continue
		}
		if matchProwlarr(container.Image) != "" {
			inventory.Prowlarrs = append(inventory.Prowlarrs, name)
			fmt.Fprintf(out, "  ✓ %s (prowlarr)\n", name)
			continue
		}
		if jellyfinsetup.IsSupportedImage(container.Image) {
			inventory.Jellyfins = append(inventory.Jellyfins, name)
			fmt.Fprintf(out, "  ✓ %s (jellyfin)\n", name)
		}
	}
	if len(inventory.Arrs) == 0 {
		fmt.Fprintln(out, "  ℹ No supported running Sonarr/Radarr containers were visible to the scoped connector.")
	} else {
		sort.Strings(inventory.Arrs)
	}
	sort.Strings(inventory.Prowlarrs)
	sort.Strings(inventory.Jellyfins)
	return inventory
}

func wildcardListenAddress(address string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return false
	}
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// firstRadarrTarget picks the first discovered Radarr instance, if any.
func firstRadarrTarget(arrs []discoveredArrInstance) (demoArrTarget, bool) {
	for _, inst := range arrs {
		if inst.AppType == string(arrdiscovery.AppRadarr) {
			return demoArrTarget{Name: inst.Name, URL: inst.URL, APIKey: util.Redacted(inst.APIKey)}, true
		}
	}
	return demoArrTarget{}, false
}

// runDemoStep runs the ONB-01 demo, printing sanitized progress/results and
// asking for cleanup confirmation only for a candidate the demo itself
// added. Never blocks longer than demoTimeoutFromEnv's bound.
func runDemoStep(ctx context.Context, out io.Writer, radarr demoArrTarget) {
	timeout := demoTimeoutFromEnv(out)
	res := runZeroAccountDemo(ctx, out, radarr, demoDataRoot(), timeout, func(title string) bool {
		return promptYesNo(out, fmt.Sprintf("  Remove demo item %q now? [Y/n]: ", title), true)
	})
	if res.Err != nil {
		fmt.Fprintf(out, "  ⚠ Zero-account demo did not complete: %v\n", res.Err)
		return
	}
	fmt.Fprintf(out, "  ✅ Zero-account demo verified %q end-to-end.\n", res.Title)
}

// runWizardDoctor runs ONB-02's full doctor sweep at wizard end and prints a
// compact, secret-redacted one-line-per-check summary. Best-effort and
// non-fatal: a failure to load *config.Config this soon after S7's secrets
// seal (or any other doctor failure) is reported as a warning, exactly like
// this function's neighbors (S10 shim install, Jellyfin setup) degrade
// rather than fail the whole wizard. Re-run anytime with `darkharrbor doctor`.
func runWizardDoctor(ctx context.Context, out io.Writer, db *sql.DB, dockerProxyURL string) {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(out, "  ⚠ Doctor skipped: could not load config yet (%v). Re-run anytime with `darkharrbor doctor`.\n", err)
		return
	}
	timeout := doctor.TimeoutFromEnv(func(msg string) { fmt.Fprintf(out, "  ⚠ %s\n", msg) })
	report, err := doctor.Run(ctx, doctor.Options{DB: db, Cfg: cfg, DockerProxyURL: dockerProxyURL}, timeout)
	if err != nil {
		fmt.Fprintf(out, "  ⚠ Doctor did not complete: %v\n", err)
		return
	}
	for _, c := range report.Checks {
		symbol := "?"
		switch c.Status {
		case doctor.StatusOK:
			symbol = "✓"
		case doctor.StatusWarn:
			symbol = "⚠"
		case doctor.StatusFail:
			symbol = "✗"
		case doctor.StatusSkip:
			symbol = "·"
		}
		fmt.Fprintf(out, "  %s [%s] %s: %s\n", symbol, c.Category, c.Name, c.Detail)
	}
	if report.OK() {
		fmt.Fprintln(out, "  ✅ Doctor: all checks passed (see any ⚠/· above). Re-run anytime with `darkharrbor doctor`.")
	} else {
		fmt.Fprintln(out, "  ⚠ Doctor found issues above. Re-run anytime with `darkharrbor doctor` after addressing them.")
	}
}

// demoDataRoot resolves DarkHarrbor's own data root the same way the config
// package does, without requiring a loaded *config.Config in the wizard.
func demoDataRoot() string {
	if v := strings.TrimSpace(os.Getenv("HARRBOR_DATA_ROOT")); v != "" {
		return v
	}
	return "/data"
}

func runBackupSetup(out io.Writer, cfgDir string, enabled bool) error {
	fmt.Fprintln(out, "\n── S7b Scheduled Backups ──────────────────────────────")
	path := filepath.Join(cfgDir, filepath.Base(config.DefaultTuningPath))
	values, err := config.ReadTuning(path)
	if err != nil {
		return fmt.Errorf("read runtime configuration: %w", err)
	}

	target := values["HARRBOR_BACKUP_TARGET"]
	if target == "" {
		target = "/backup"
	}
	interval := tuningDefaultInt(values, "HARRBOR_BACKUP_INTERVAL_HOURS", 24)
	keep := tuningDefaultInt(values, "HARRBOR_BACKUP_KEEP", 7)

	if !enabled {
		values["HARRBOR_BACKUP_ENABLED"] = "false"
		if err := config.WriteTuning(path, values); err != nil {
			return fmt.Errorf("write runtime configuration: %w", err)
		}
		fmt.Fprintln(out, "  · Skipped by plan; no backup target, cadence, or retention prompts were shown.")
		return nil
	}
	target = promptWithDefault(out, fmt.Sprintf("  Backup target directory [%s]: ", target), target)
	interval = promptInt(out, fmt.Sprintf("  Backup cadence in hours [%d]: ", interval), interval)
	keep = promptInt(out, fmt.Sprintf("  Backup snapshots to keep [%d]: ", keep), keep)

	values["HARRBOR_BACKUP_ENABLED"] = strconv.FormatBool(enabled)
	values["HARRBOR_BACKUP_TARGET"] = target
	values["HARRBOR_BACKUP_INTERVAL_HOURS"] = strconv.Itoa(interval)
	values["HARRBOR_BACKUP_KEEP"] = strconv.Itoa(keep)
	if err := config.WriteTuning(path, values); err != nil {
		return fmt.Errorf("write runtime configuration: %w", err)
	}
	fmt.Fprintf(out, "  ✓ Backups: enabled=%t cadence=%dh keep=%d\n", enabled, interval, keep)
	return nil
}

func tuningDefaultInt(values map[string]string, key string, fallback int) int {
	if n, err := strconv.Atoi(values[key]); err == nil && n > 0 {
		return n
	}
	return fallback
}

// ── helpers ──────────────────────────────────────────────────────────────────

// promptSecret reads a line from stdin with fail-closed terminal echo control.
// Answer replay and redirected input remain available for deliberate automation.
func promptSecret(out io.Writer, prompt string) (string, error) {
	if answerReplayActive {
		fmt.Fprint(out, prompt)
		return readLine(out), nil
	}
	line, err := secretinput.ReadLine(out, prompt, os.Stdin, stdinReader)
	return strings.TrimRight(line, "\r\n"), err
}

// readLine reads one line from stdin (strips trailing newline).
// Uses the package-level stdinReader so repeated calls share the same buffered
// reader — a fresh bufio.Scanner per call would drain the entire fd on first use.
func readLine(_ io.Writer) string {
	line, _ := stdinReader.ReadString('\n')
	return strings.TrimRight(line, "\r\n")
}

// promptPreference builds HARRBOR_PREFERENCE interactively from detected caps.
// WZ-01: torboxConfigured gates every TorBox-backed lane. Previously all three
// were offered unconditionally and parsePreference accepted them regardless, so
// a deployment with no TorBox could be configured with HARRBOR_PREFERENCE=
// torbox_torrent and would fail at ACQUISITION time -- long after setup, with
// nothing pointing back at the cause.
// availableLaneList names only the lanes this deployment can actually use, so
// an error message cannot advertise a lane the operator has no provider for.
func availableLaneList(torrentAvailable, torboxConfigured, nzbAvailable, nntpAvailable bool) string {
	var lanes []string
	if torrentAvailable {
		lanes = append(lanes, "cached-torrent")
	}
	if torboxConfigured {
		if nzbAvailable {
			lanes = append(lanes, "cached-nzb")
		}
		lanes = append(lanes, "uncached-torrent", "uncached-torrent-last")
	}
	if nntpAvailable {
		lanes = append(lanes, "direct-usenet")
	}
	if len(lanes) == 0 {
		return "(none — configure a provider)"
	}
	return strings.Join(lanes, ", ")
}

func promptPreference(out io.Writer, torrentAvailable, torboxConfigured, usenetCapable, nntpAvailable, freeTier bool) ([]string, error) {
	if torrentAvailable {
		fmt.Fprintln(out, "  Available source priorities (based on verified providers):")
		fmt.Fprintln(out, "    1  cached-torrent          — cached torrent through any configured debrid provider")
	} else {
		fmt.Fprintln(out, "  Available source priorities (no debrid provider configured):")
	}

	nzbAvailable := usenetCapable && torboxConfigured
	if nzbAvailable {
		fmt.Fprintln(out, "    2  cached-nzb              — provider-cached NZB content")
	}
	if nntpAvailable {
		fmt.Fprintln(out, "    3  direct-usenet           — direct NNTP streaming")
	}
	if torboxConfigured {
		fmt.Fprintln(out, "    4  uncached-torrent        — download an uncached torrent (uses a provider slot)")
		fmt.Fprintln(out, "    5  uncached-torrent-last   — same path, ranked behind cached results")
	}

	if freeTier {
		fmt.Fprintln(out, "  ℹ  Free tier: RequireCached=true is strongly recommended.")
		fmt.Fprintln(out, "     Default priority: cached-torrent")
		fmt.Fprint(out, "  Accept default? [Y/n]: ")
		ans := readLine(out)
		if ans == "" || strings.EqualFold(ans, "y") {
			return []string{config.LaneTorBoxTorrent}, nil
		}
	}

	// Build default for paid accounts with usenet. Without TorBox the only
	// lane that can be defaulted to is the NNTP floor.
	defaultPref := "cached-torrent"
	if torrentAvailable && nzbAvailable {
		defaultPref = "cached-torrent,cached-nzb"
	}
	if !torrentAvailable {
		if !nntpAvailable {
			return nil, fmt.Errorf("no acquisition lane is available: no debrid or NNTP provider was configured")
		}
		defaultPref = "direct-usenet"
	}

	fmt.Fprintf(out, "  Enter priority order by name or number [%s]: ", defaultPref)
	raw := readLine(out)
	if strings.TrimSpace(raw) == "" {
		raw = defaultPref
	}

	// Validate via the same parser config uses.
	preference := parsePreference(raw, nzbAvailable, nntpAvailable)

	// WZ-01: reject TorBox lanes when TorBox is not configured, HERE rather
	// than at acquisition time. parsePreference has no notion of which
	// providers exist, so this is the layer that must know.
	if !torrentAvailable || !torboxConfigured {
		kept := preference[:0]
		var dropped []string
		for _, l := range preference {
			switch l {
			case config.LaneTorBoxTorrent:
				if !torrentAvailable {
					dropped = append(dropped, l)
					continue
				}
				kept = append(kept, l)
			case config.LaneTorBoxNZB, config.LaneUncachedTorrent, config.LaneUncachedTorrentDerank:
				if !torboxConfigured {
					dropped = append(dropped, l)
					continue
				}
				kept = append(kept, l)
			default:
				kept = append(kept, l)
			}
		}
		preference = kept
		if len(dropped) > 0 {
			fmt.Fprintf(out, "  ⚠ Ignoring unavailable choices (%s): required provider capability was not configured.\n",
				strings.Join(dropped, ", "))
		}
	}

	if len(preference) == 0 {
		return nil, fmt.Errorf("no valid lanes in %q — valid lanes for THIS deployment: %s", raw, availableLaneList(torrentAvailable, torboxConfigured, nzbAvailable, nntpAvailable))
	}

	// Either uncached policy requires explicit confirmation.
	wantsUncached := false
	for _, l := range preference {
		if l == config.LaneUncachedTorrent || l == config.LaneUncachedTorrentDerank {
			wantsUncached = true
		}
	}
	if wantsUncached {
		fmt.Fprintln(out, "  ⚠  Uncached torrent downloads use a provider slot and may consume transfer quota.")
		fmt.Fprintln(out, "     On Pro (10 slots) this limits simultaneous uncached downloads.")
		fmt.Fprint(out, "  Allow uncached torrent downloads? [y/N]: ")
		confirm := readLine(out)
		if !strings.EqualFold(confirm, "y") {
			// Remove uncached_torrent from the list.
			filtered := preference[:0]
			for _, l := range preference {
				if l != config.LaneUncachedTorrent && l != config.LaneUncachedTorrentDerank {
					filtered = append(filtered, l)
				}
			}
			preference = filtered
			fmt.Fprintln(out, "  uncached torrent policy removed.")
		}
	}

	return preference, nil
}

// parsePreference parses a comma-separated lane list, only admitting lanes that
// are supported by the account's discovered capabilities.
func parsePreference(raw string, usenetCapable, nntpAvailable bool) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, part := range strings.Split(raw, ",") {
		lane := strings.ToLower(strings.TrimSpace(part))
		switch lane {
		case "1", "torrent", "cached-torrent":
			lane = config.LaneTorBoxTorrent
		case "2", "cached-nzb":
			lane = config.LaneTorBoxNZB
		case "3", "usenet", "nntp", "direct-usenet":
			lane = config.LaneNNTPNZB
		case "4", "uncached-torrent":
			lane = config.LaneUncachedTorrent
		case "5", "uncached-torrent-last":
			lane = config.LaneUncachedTorrentDerank
		}
		if lane == "" || seen[lane] {
			continue
		}
		switch lane {
		case config.LaneTorBoxTorrent:
			// always available
		case config.LaneUncachedTorrent, config.LaneUncachedTorrentDerank:
			if seen[config.LaneUncachedTorrent] || seen[config.LaneUncachedTorrentDerank] {
				continue
			}
		case config.LaneTorBoxNZB:
			if !usenetCapable {
				continue // plan does not support usenet ingest
			}
		case config.LaneNNTPNZB:
			if !nntpAvailable {
				continue
			}
		default:
			continue // unknown lane
		}
		seen[lane] = true
		out = append(out, lane)
	}
	return out
}

func usableUsenetSource(torboxConfigured, torboxUsenetCapable, directNNTPAvailable bool) bool {
	return directNNTPAvailable || (torboxConfigured && torboxUsenetCapable)
}

func configureUsenetProviders(ctx context.Context, out io.Writer, db *sql.DB, tbClient *torbox.HTTPClient, ordered []string, newsServerCapable bool) ([]string, bool, error) {
	if len(ordered) == 0 {
		fmt.Fprintln(out, "  · No direct NNTP provider selected.")
		return nil, false, nil
	}
	secretLines := []string{}
	for _, name := range ordered {
		switch name {
		case "torbox":
			if !newsServerCapable {
				return nil, false, fmt.Errorf("TorBox News Server was selected but the verified plan does not provide it")
			}
			ncfg, err := validateTorBoxNewsServer(ctx, db, tbClient)
			if err != nil {
				return nil, false, fmt.Errorf("TorBox News Server validation failed: %w", err)
			}
			fmt.Fprintf(out, "  ✓ TorBox News Server validated (%s:%d, tls=%t, connections=%d)\n", ncfg.Host, ncfg.Port, ncfg.TLS, ncfg.Connections)
		default:
			prefix := "HARRBOR_PROVIDER_" + envSuffix(name) + "_"
			if name == "newshosting" {
				prefix = "HARRBOR_NNTP_"
			}
			ncfg, lines, err := promptNamedNNTPProvider(out, name, prefix)
			if err != nil {
				return nil, false, err
			}
			secretLines = append(secretLines, lines...)
			fmt.Fprintf(out, "  ✓ %s validated (%s:%d, tls=%t, connections=%d)\n", name, ncfg.Host, ncfg.Port, ncfg.TLS, ncfg.Connections)
		}
	}
	secretLines = append(secretLines, "HARRBOR_USENET_PROVIDERS="+strings.Join(ordered, ","))
	return secretLines, true, nil
}

const maxUsenetProviders = 16

func promptUsenetProviderSelection(out io.Writer, torboxSelected bool) []string {
	fmt.Fprintln(out, "  Select only the direct NNTP providers you already use, in fallback order.")
	fmt.Fprintln(out, "    newshosting — built-in legacy field names")
	if torboxSelected {
		fmt.Fprintln(out, "    torbox      — included only if account verification confirms News Server")
	}
	fmt.Fprintln(out, "    custom names use the generic provider field family")
	for {
		fmt.Fprint(out, "  NNTP providers (comma-separated names; 'none' for none) [none]: ")
		names, err := parseUsenetProviderSelection(readLine(out), torboxSelected)
		if err == nil {
			return names
		}
		fmt.Fprintf(out, "  ⚠ %v\n", err)
	}
}

func parseUsenetProviderSelection(raw string, torboxSelected bool) ([]string, error) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" || raw == "none" {
		return nil, nil
	}
	seen := map[string]bool{}
	seenSuffix := map[string]string{}
	var names []string
	for _, part := range strings.Split(raw, ",") {
		name := strings.TrimSpace(part)
		if name == "" || len(name) > 64 || name == "none" || !validProviderName(name) {
			return nil, fmt.Errorf("invalid NNTP provider name %q", name)
		}
		if name == "torbox" && !torboxSelected {
			return nil, fmt.Errorf("select the TorBox account first before choosing its News Server")
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate NNTP provider %q", name)
		}
		suffix := envSuffix(name)
		if prior := seenSuffix[suffix]; prior != "" {
			return nil, fmt.Errorf("NNTP provider names %q and %q use the same configuration key", prior, name)
		}
		seen[name] = true
		seenSuffix[suffix] = name
		names = append(names, name)
		if len(names) > maxUsenetProviders {
			return nil, fmt.Errorf("at most %d NNTP providers may be selected", maxUsenetProviders)
		}
	}
	return names, nil
}

func promptNamedNNTPProvider(out io.Writer, label, prefix string) (nntp.Config, []string, error) {
	fmt.Fprintf(out, "  %s settings:\n", label)
	host := promptNonEmpty(out, "    Host: ")
	port := promptInt(out, "    Port [563]: ", 563)
	tlsEnabled := promptYesNo(out, "    TLS [Y/n]: ", true)
	username := promptNonEmpty(out, "    Username: ")
	password, err := promptSecret(out, "    Password: ")
	if err != nil {
		return nntp.Config{}, nil, fmt.Errorf("%s password: %w", label, err)
	}
	password = strings.TrimSpace(password)
	if password == "" {
		return nntp.Config{}, nil, fmt.Errorf("%s password is required", label)
	}
	connections := promptInt(out, "    Connections [8]: ", 8)
	ncfg := nntp.Config{
		Host:        host,
		Port:        port,
		TLS:         tlsEnabled,
		Username:    username,
		Password:    password,
		Connections: connections,
	}
	if err := validateNNTPAccount(context.Background(), ncfg); err != nil {
		return nntp.Config{}, nil, fmt.Errorf("%s NNTP login failed: %w", label, err)
	}
	lines := []string{
		prefix + "HOST=" + ncfg.Host,
		prefix + "PORT=" + strconv.Itoa(ncfg.Port),
		prefix + "TLS=" + strconv.FormatBool(ncfg.TLS),
		prefix + "USERNAME=" + ncfg.Username,
		prefix + "PASSWORD=" + ncfg.Password,
		prefix + "CONNECTIONS=" + strconv.Itoa(ncfg.Connections),
	}
	return ncfg, lines, nil
}

func validateTorBoxNewsServer(ctx context.Context, db *sql.DB, tbClient *torbox.HTTPClient) (nntp.Config, error) {
	validateCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	acct, err := tbClient.GetUsenetProviderAccount(validateCtx)
	if err != nil {
		return nntp.Config{}, err
	}
	if torbox.IsMaskedPassword(acct.Password) {
		resetCtx, resetCancel := context.WithTimeout(ctx, 15*time.Second)
		acct, err = tbClient.ResetUsenetProviderPassword(resetCtx)
		resetCancel()
		if err != nil {
			return nntp.Config{}, err
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
	if err := validateNNTPAccount(validateCtx, ncfg); err != nil {
		return nntp.Config{}, err
	}
	if db != nil {
		blob, err := json.Marshal(ncfg)
		if err != nil {
			return ncfg, fmt.Errorf("marshal TorBox NNTP credential: %w", err)
		}
		// Written UNSEALED on purpose (SEC-01). This is S4; the seal does not
		// exist until S7, so there is no key to seal with and no ordering that
		// would create one — the credential must be persisted at the moment
		// TorBox reveals it, because every later API call returns a mask. The
		// daemon re-seals this row in place on its first read, so the plaintext
		// window is bounded by the remainder of the wizard run.
		if err := store.New(db).SetProviderCredential(validateCtx, "torbox-nntp", string(blob)); err != nil {
			return ncfg, fmt.Errorf("persist TorBox NNTP credential: %w", err)
		}
	}
	return ncfg, nil
}

func validateNNTPAccount(ctx context.Context, cfg nntp.Config) error {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	var err error
	if cfg.TLS {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: cfg.Host})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	tp := textproto.NewConn(conn)
	defer func() { _ = tp.Close() }()
	if _, _, err := tp.ReadCodeLine(0); err != nil {
		return fmt.Errorf("greeting: %w", err)
	}
	if cfg.Username == "" {
		return nil
	}
	if err := nntpCmd(tp, 381, "AUTHINFO USER %s", cfg.Username); err != nil && !strings.Contains(err.Error(), "281") {
		return fmt.Errorf("AUTHINFO USER: %w", err)
	}
	if err := nntpCmd(tp, 281, "AUTHINFO PASS %s", cfg.Password); err != nil {
		return fmt.Errorf("AUTHINFO PASS: %w", err)
	}
	return nil
}

func nntpCmd(tp *textproto.Conn, expectCode int, format string, args ...any) error {
	id, err := tp.Cmd(format, args...)
	if err != nil {
		return err
	}
	tp.StartResponse(id)
	defer tp.EndResponse(id)
	_, _, err = tp.ReadCodeLine(expectCode)
	return err
}

func promptYesNo(out io.Writer, prompt string, def bool) bool {
	fmt.Fprint(out, prompt)
	ans := strings.ToLower(strings.TrimSpace(readLine(out)))
	if ans == "" {
		return def
	}
	return ans == "y" || ans == "yes"
}

func promptWithDefault(out io.Writer, prompt, def string) string {
	fmt.Fprint(out, prompt)
	if v := strings.TrimSpace(readLine(out)); v != "" {
		return v
	}
	return def
}

func promptInt(out io.Writer, prompt string, def int) int {
	for {
		raw := promptWithDefault(out, prompt, strconv.Itoa(def))
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err == nil && n > 0 {
			return n
		}
		fmt.Fprintln(out, "  ⚠ Enter a positive integer.")
	}
}

func promptNonEmpty(out io.Writer, prompt string) string {
	for {
		fmt.Fprint(out, prompt)
		if v := strings.TrimSpace(readLine(out)); v != "" {
			return v
		}
		fmt.Fprintln(out, "  ⚠ Value is required.")
	}
}

func validProviderName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

type arrAppKind string

const (
	arrAppProwlarr arrAppKind = "prowlarr"
)

type discoveredArrInstance struct {
	Name      string
	URL       string
	AppType   string
	APIKey    string
	APIKeyRef string
}

type arrConfigXML struct {
	Port   int    `xml:"Port"`
	APIKey string `xml:"ApiKey"`
}

type appVerifier func(context.Context, discoveredArrInstance) error

func runArrDiscovery(ctx context.Context, out io.Writer, proxyURL string, includeProwlarr bool, selectedArrs []string) ([]discoveredArrInstance, error) {
	client := dockerctl.New(proxyURL, &http.Client{Timeout: 10 * time.Second})
	instances, err := discoverChosenArrInstances(ctx, client, verifyDiscoveredArrInstance, includeProwlarr, selectedArrs)
	if err != nil {
		return nil, err
	}
	if len(instances) == 0 {
		return nil, fmt.Errorf("no arr targets discovered")
	}
	for _, inst := range instances {
		fmt.Fprintf(out, "  ✓ %s (%s) %s\n", inst.Name, inst.AppType, inst.URL)
	}
	return instances, nil
}

func withoutAppType(instances []discoveredArrInstance, appType string) []discoveredArrInstance {
	out := instances[:0]
	for _, inst := range instances {
		if inst.AppType != appType {
			out = append(out, inst)
		}
	}
	return out
}

func promptManualArrInstances(ctx context.Context, out io.Writer, includeProwlarr bool) ([]discoveredArrInstance, error) {
	fmt.Fprintln(out, "  Manual mode verifies every target before storing metadata or sealing its API key.")
	allowed := []string{string(arrdiscovery.AppSonarr), string(arrdiscovery.AppRadarr), "done"}
	if includeProwlarr {
		allowed = []string{string(arrdiscovery.AppSonarr), string(arrdiscovery.AppRadarr), string(arrAppProwlarr), "done"}
	}
	var instances []discoveredArrInstance
	seen := map[string]bool{}
	for {
		appType := promptChoice(out, fmt.Sprintf("  Instance type [%s]: ", strings.Join(allowed, "/")), "done", allowed...)
		if appType == "done" {
			if len(withoutAppType(append([]discoveredArrInstance(nil), instances...), string(arrAppProwlarr))) == 0 {
				fmt.Fprintln(out, "  ⚠ Enter at least one Sonarr or Radarr instance.")
				continue
			}
			return instances, nil
		}
		fmt.Fprint(out, "  Instance name (lowercase letters, digits, '-' or '_'): ")
		name := strings.TrimSpace(readLine(out))
		if !validProviderName(name) {
			return nil, fmt.Errorf("invalid instance name %q", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate instance name %q", name)
		}
		if appType == string(arrAppProwlarr) {
			for _, existing := range instances {
				if existing.AppType == string(arrAppProwlarr) {
					return nil, fmt.Errorf("only one Prowlarr instance can be selected")
				}
			}
		}
		fmt.Fprint(out, "  Base URL (reachable from DarkHarrbor): ")
		baseURL, err := normalizeArrBaseURL(readLine(out))
		if err != nil {
			return nil, fmt.Errorf("%s URL: %w", name, err)
		}
		apiKey, err := promptSecret(out, "  API key: ")
		if err != nil {
			return nil, fmt.Errorf("%s API key: %w", name, err)
		}
		apiKey = strings.TrimSpace(apiKey)
		if apiKey == "" {
			return nil, fmt.Errorf("%s API key is required", name)
		}
		inst := discoveredArrInstance{
			Name: name, URL: baseURL, AppType: appType, APIKey: apiKey,
			APIKeyRef: "HARRBOR_ARR_" + envSuffix(name) + "_APIKEY",
		}
		if err := verifyManualArrInstance(ctx, inst); err != nil {
			return nil, fmt.Errorf("%s verification failed: %w", name, err)
		}
		instances = append(instances, inst)
		seen[name] = true
		fmt.Fprintf(out, "  ✓ %s (%s) authenticated at %s\n", name, appType, baseURL)
	}
}

func verifyManualArrInstance(ctx context.Context, inst discoveredArrInstance) error {
	if inst.AppType == string(arrdiscovery.AppSonarr) || inst.AppType == string(arrdiscovery.AppRadarr) {
		detected, err := arrsetup.DetectAppType(ctx, inst.URL, inst.APIKey)
		if err != nil {
			return err
		}
		if detected != inst.AppType {
			return fmt.Errorf("target identifies as %s, not %s", detected, inst.AppType)
		}
		return nil
	}
	return verifyDiscoveredArrInstance(ctx, inst)
}

func normalizeArrBaseURL(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	u, err := url.Parse(value)
	if err != nil {
		return "", err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("must be an absolute http(s) URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("credentials, query strings, and fragments are not allowed")
	}
	u.Path = strings.TrimRight(u.EscapedPath(), "/")
	u.RawPath = ""
	return strings.TrimRight(u.String(), "/"), nil
}

func discoverArrInstances(ctx context.Context, client *dockerctl.Client, verifier appVerifier) ([]discoveredArrInstance, error) {
	return discoverSelectedArrInstances(ctx, client, verifier, true)
}

func discoverSelectedArrInstances(ctx context.Context, client *dockerctl.Client, verifier appVerifier, includeProwlarr bool) ([]discoveredArrInstance, error) {
	return discoverChosenArrInstances(ctx, client, verifier, includeProwlarr, nil)
}

func discoverChosenArrInstances(ctx context.Context, client *dockerctl.Client, verifier appVerifier, includeProwlarr bool, selectedNames []string) ([]discoveredArrInstance, error) {
	if client == nil {
		return nil, fmt.Errorf("docker proxy client is required")
	}
	instances := map[string]discoveredArrInstance{}

	targets, err := arrdiscovery.Discover(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("discover Sonarr/Radarr targets: %w", err)
	}
	selected := map[string]bool{}
	for _, name := range selectedNames {
		selected[name] = true
	}
	found := map[string]bool{}
	for _, target := range targets {
		if len(selected) > 0 && !selected[target.Name] {
			continue
		}
		inst, err := extractArrInstance(ctx, client, target.ContainerID, target.Name, string(target.App))
		if err != nil {
			return nil, err
		}
		instances[inst.Name] = inst
		found[inst.Name] = true
	}
	if includeProwlarr {
		containers, err := client.ListContainers(ctx)
		if err != nil {
			return nil, fmt.Errorf("list containers for Prowlarr discovery: %w", err)
		}
		for _, c := range containers {
			if c.State != "running" || matchProwlarr(c.Image) == "" {
				continue
			}
			name := primaryContainerName(c.Names)
			if len(selected) > 0 && !selected[name] {
				continue
			}
			inst, err := extractArrInstance(ctx, client, c.ID, name, string(arrAppProwlarr))
			if err != nil {
				return nil, err
			}
			instances[inst.Name] = inst
			found[inst.Name] = true
		}
	}
	for name := range selected {
		if !found[name] {
			return nil, fmt.Errorf("selected service %q is no longer available; rerun setup discovery", name)
		}
	}

	out := make([]discoveredArrInstance, 0, len(instances))
	for _, inst := range instances {
		if verifier != nil {
			if err := verifier(ctx, inst); err != nil {
				return nil, err
			}
		}
		out = append(out, inst)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AppType == out[j].AppType {
			return out[i].Name < out[j].Name
		}
		return out[i].AppType < out[j].AppType
	})
	return out, nil
}

func extractArrInstance(ctx context.Context, client *dockerctl.Client, containerID, name, appType string) (discoveredArrInstance, error) {
	result, err := client.Exec(ctx, containerID, "", dockerpolicy.ReadArrConfig())
	if err != nil {
		return discoveredArrInstance{}, fmt.Errorf("%s: read /config/config.xml: %w", name, err)
	}
	if result.ExitCode != 0 {
		return discoveredArrInstance{}, fmt.Errorf("%s: cat /config/config.xml exited %d", name, result.ExitCode)
	}

	cfg, err := parseArrConfigXML(result.Output)
	if err != nil {
		return discoveredArrInstance{}, fmt.Errorf("%s: parse config.xml: %w", name, err)
	}
	if cfg.APIKey == "" {
		return discoveredArrInstance{}, fmt.Errorf("%s: config.xml did not contain an ApiKey", name)
	}
	if cfg.Port <= 0 {
		return discoveredArrInstance{}, fmt.Errorf("%s: config.xml did not contain a valid Port", name)
	}

	ref := "HARRBOR_ARR_" + envSuffix(name) + "_APIKEY"
	return discoveredArrInstance{
		Name:      name,
		URL:       fmt.Sprintf("http://%s:%d", name, cfg.Port),
		AppType:   appType,
		APIKey:    strings.TrimSpace(cfg.APIKey),
		APIKeyRef: ref,
	}, nil
}

func parseArrConfigXML(raw string) (arrConfigXML, error) {
	var cfg arrConfigXML
	if err := xml.Unmarshal([]byte(strings.TrimSpace(raw)), &cfg); err != nil {
		return arrConfigXML{}, err
	}
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	return cfg, nil
}

func verifyDiscoveredArrInstance(ctx context.Context, inst discoveredArrInstance) error {
	statusPath := "/api/v3/system/status"
	if inst.AppType == string(arrAppProwlarr) {
		statusPath = "/api/v1/system/status"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, inst.URL+statusPath, nil)
	if err != nil {
		return fmt.Errorf("%s: build status request: %w", inst.Name, err)
	}
	req.Header.Set("X-Api-Key", inst.APIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s: system status probe: %w", inst.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s: system status probe returned %d", inst.Name, resp.StatusCode)
	}
	return nil
}

// persistArrInstancesToDB writes discovered arr targets to the DB for runtime
// use (arr-notify feature). Secrets (arr API keys + URLs) are NOT written here;
// they are accumulated in-memory and sealed in S7 along with all other secrets.
func persistArrInstancesToDB(ctx context.Context, db *sql.DB, instances []discoveredArrInstance) error {
	st := store.New(db)
	for _, inst := range instances {
		if err := st.UpsertArrInstance(ctx, store.ArrInstance{
			Name:      inst.Name,
			URL:       inst.URL,
			AppType:   inst.AppType,
			APIKeyRef: inst.APIKeyRef,
		}); err != nil {
			return err
		}
	}
	return nil
}

func envSuffix(name string) string {
	return strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

func primaryContainerName(names []string) string {
	if len(names) == 0 {
		return "?"
	}
	return strings.TrimPrefix(names[0], "/")
}

func matchProwlarr(image string) string {
	if strings.Contains(image, "/prowlarr:") || strings.HasSuffix(image, "/prowlarr") {
		return string(arrAppProwlarr)
	}
	return ""
}

// verifySeal unseals the file and checks that the line count matches.
func verifySeal(path, password string, wantCount int) error {
	blob, err := securefile.Read(path, 16<<20)
	if err != nil {
		return fmt.Errorf("read sealed file: %w", err)
	}
	plain, err := util.UnsealSecrets(blob, password)
	if err != nil {
		return fmt.Errorf("unseal: %w", err)
	}
	got, err := config.ValidateSecretLines(plain)
	if err != nil {
		return fmt.Errorf("validate unsealed lines: %w", err)
	}
	if got != wantCount {
		return fmt.Errorf("line count mismatch: sealed %d, unsealed %d", wantCount, got)
	}
	return nil
}

func secretOutputPath(envName, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(envName)); value != "" {
		return filepath.Clean(value)
	}
	return filepath.Clean(fallback)
}

// appConfigGet fetches a single key from app_config; returns "" on miss.
func appConfigGet(db *sql.DB, key string) (string, error) {
	if db == nil {
		return "", nil
	}
	var val string
	err := db.QueryRow(`SELECT value FROM app_config WHERE key = ?`, key).Scan(&val)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return val, err
}

// appConfigSet upserts a key into app_config.
func appConfigSet(db *sql.DB, key, value string) error {
	if db == nil {
		return nil
	}
	_, err := db.Exec(
		`INSERT INTO app_config(key, value, updated_at)
		 VALUES(?, ?, strftime('%Y-%m-%dT%H:%M:%SZ','now'))
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, value,
	)
	return err
}

// ── formatting helpers ────────────────────────────────────────────────────────

func fmtBytes(b int64) string {
	const (
		KB = 1 << 10
		MB = 1 << 20
		GB = 1 << 30
		TB = 1 << 40
	)
	switch {
	case b >= TB:
		return fmt.Sprintf("%.0f TB", float64(b)/TB)
	case b >= GB:
		return fmt.Sprintf("%.0f GB", float64(b)/GB)
	case b >= MB:
		return fmt.Sprintf("%.0f MB", float64(b)/MB)
	case b >= KB:
		return fmt.Sprintf("%.0f KB", float64(b)/KB)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// ── exported helpers for testing ─────────────────────────────────────────────

// DefaultPreference returns the default HARRBOR_PREFERENCE lane list derived
// from plan capabilities (mirrors the S5 interactive default logic). Used by
// the caps→derived-config unit test (W1 gate).
func DefaultPreference(usenetCapable, freeTier bool) []string {
	if freeTier || !usenetCapable {
		return []string{config.LaneTorBoxTorrent}
	}
	return []string{config.LaneTorBoxTorrent, config.LaneTorBoxNZB}
}

// PlanLabel is the exported wrapper for tests; delegates to the shared
// torbox.PlanLabel owner (HR7.3) rather than keeping a second plan-naming
// table.
func PlanLabel(plan int, subscribed bool) string { return torbox.PlanLabel(plan, subscribed) }

// runArrSetupDirect implements S9 using in-memory credentials (no unseal needed).
// Called within the same wizard session that generated the secrets, so creds are
// passed directly — eliminating the .env sync problem that arose when S7 sealed
// before S8/S9 ran.
func runArrSetupDirect(ctx context.Context, out io.Writer, discovered []discoveredArrInstance, creds arrsetup.Creds, lanes arrsetup.Lanes) error {
	var targets []arrsetup.ArrTarget
	for _, inst := range discovered {
		if inst.AppType != string(arrdiscovery.AppSonarr) && inst.AppType != string(arrdiscovery.AppRadarr) {
			continue
		}
		targets = append(targets, arrsetup.ArrTarget{
			Name:    inst.Name,
			URL:     inst.URL,
			AppType: inst.AppType,
			APIKey:  inst.APIKey,
		})
	}
	if len(targets) == 0 {
		fmt.Fprintln(out, "  ⚠ No Sonarr/Radarr targets — S9 skipped.")
		return nil
	}
	results := arrsetup.RegisterSelected(ctx, targets, creds, lanes)
	allOK := true
	for _, r := range results {
		if r.Err != nil {
			fmt.Fprintf(out, "  ✗ %s (%s): %v\n", r.Name, r.AppType, r.Err)
			allOK = false
			continue
		}
		fmt.Fprintf(out, "  ✓ %s (%s): %s\n", r.Name, r.AppType, strings.Join(r.Actions, ", "))
	}
	if !allOK {
		return fmt.Errorf("one or more arr registrations failed (see above)")
	}
	return nil
}

// ── S10/S11 helpers ───────────────────────────────────────────────────────────

type s10Result struct {
	Name     string
	Action   string
	Verified bool
	Err      error
}

// runShimInstall runs one shiminstall.Install pass over the selected Arr
// containers. The runtime event watcher receives the same configured names.
func runShimInstall(ctx context.Context, out io.Writer, proxyURL string, selectedArrs []string) []s10Result {
	client := dockerctl.New(proxyURL, &http.Client{Timeout: 10 * time.Second})
	targets, err := arrdiscovery.Discover(ctx, client)
	if err != nil {
		fmt.Fprintf(out, "  ✗ discover failed: %v\n", err)
		return nil
	}
	selected := map[string]bool{}
	for _, name := range selectedArrs {
		selected[name] = true
	}
	results := make([]s10Result, 0, len(targets))
	for _, target := range targets {
		if len(selected) > 0 && !selected[target.Name] {
			continue
		}
		result, err := shiminstall.Install(ctx, client, target)
		r := s10Result{Name: target.Name}
		if err != nil {
			r.Err = err
			fmt.Fprintf(out, "  ✗ %s: %v\n", target.Name, err)
		} else {
			r.Action = string(result.Action)
			r.Verified = result.Verified
			fmt.Fprintf(out, "  ✓ %s: action=%s verified=%v\n", target.Name, result.Action, result.Verified)
		}
		results = append(results, r)
	}
	return results
}

// runValidate implements S11: health summary + torznab caps test per arr +
// final bootstrap_complete write.
func runValidate(ctx context.Context, out io.Writer, db *sql.DB, arrs []discoveredArrInstance, shims []s10Result, plan onboardingPlan) error {
	shimByName := map[string]s10Result{}
	for _, r := range shims {
		shimByName[r.Name] = r
	}

	// Per-Arr authenticated reachability and selected shim status.
	allOK := true
	for _, inst := range arrs {
		if inst.AppType == "prowlarr" {
			continue
		}
		statusPath := "/api/v3/system/status"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, inst.URL+statusPath, nil)
		if err != nil {
			fmt.Fprintf(out, "  ✗ %s: build status request: %v\n", inst.Name, err)
			allOK = false
			continue
		}
		req.Header.Set("X-Api-Key", inst.APIKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Fprintf(out, "  ✗ %s: status probe: %v\n", inst.Name, err)
			allOK = false
			continue
		}
		resp.Body.Close()

		shimStatus := "skipped"
		if sr, ok := shimByName[inst.Name]; ok {
			if sr.Err != nil {
				shimStatus = "ERROR:" + sr.Err.Error()
				allOK = false
			} else {
				shimStatus = fmt.Sprintf("%s verified=%v", sr.Action, sr.Verified)
			}
		}
		fmt.Fprintf(out, "  ✓ %s (%s) status=%d shim=%s\n", inst.Name, inst.AppType, resp.StatusCode, shimStatus)
	}

	// Torznab caps test via sonarr-modern's first DH indexer.
	if plan.BaseArrTopology && plan.hasLane("torrent") {
		if err := runIndexerTest(ctx, out, arrs); err != nil {
			fmt.Fprintf(out, "  ⚠ torznab caps test: %v\n", err)
		}
	} else {
		fmt.Fprintln(out, "  · Torrent Torznab caps test skipped: that Arr surface was not selected.")
	}

	// Prowlarr sync-level reminder.
	for _, inst := range arrs {
		if inst.AppType == "prowlarr" {
			fmt.Fprintf(out, "  ℹ Prowlarr found at %s — ensure Sync Level = Disabled on all app entries (session-6 re-enable trap).\n", inst.URL)
		}
	}

	// Strm mode reminder.
	if plan.WrapperRepair {
		fmt.Fprintln(out, "  ✓ Steady-state wrapper repair enabled for the selected Arr instances.")
	} else if plan.BaseArrTopology {
		fmt.Fprintln(out, "  ⚠ Steady-state wrapper repair remains externally pending for manually entered Arrs.")
	}

	if !allOK {
		return fmt.Errorf("one or more arr health checks failed")
	}

	// Record completion timestamp.
	_ = appConfigSet(db, "bootstrap_complete", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintln(out, "  ✓ bootstrap_complete written")
	return nil
}

// runIndexerTest fires a POST /api/v3/indexer/test against the first DH
// Torznab indexer found on the first Sonarr/Radarr in the discovered list.
func runIndexerTest(ctx context.Context, out io.Writer, arrs []discoveredArrInstance) error {
	for _, inst := range arrs {
		if inst.AppType != "sonarr" && inst.AppType != "radarr" {
			continue
		}
		// List indexers, find DH torznab.
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, inst.URL+"/api/v3/indexer", nil)
		if err != nil {
			return err
		}
		req.Header.Set("X-Api-Key", inst.APIKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var indexers []struct {
			ID             int    `json:"id"`
			Implementation string `json:"implementation"`
		}
		if err := json.Unmarshal(raw, &indexers); err != nil {
			return err
		}
		for _, idx := range indexers {
			if idx.Implementation != "Torznab" {
				continue
			}
			// POST /api/v3/indexer/test with the indexer's current definition.
			defReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
				fmt.Sprintf("%s/api/v3/indexer/%d", inst.URL, idx.ID), nil)
			if err != nil {
				return err
			}
			defReq.Header.Set("X-Api-Key", inst.APIKey)
			defResp, err := http.DefaultClient.Do(defReq)
			if err != nil {
				return err
			}
			defRaw, _ := io.ReadAll(defResp.Body)
			defResp.Body.Close()

			testReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
				inst.URL+"/api/v3/indexer/test", bytes.NewReader(defRaw))
			if err != nil {
				return err
			}
			testReq.Header.Set("X-Api-Key", inst.APIKey)
			testReq.Header.Set("Content-Type", "application/json")
			testCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			testReq = testReq.WithContext(testCtx)
			testResp, err := http.DefaultClient.Do(testReq)
			cancel()
			if err != nil {
				return err
			}
			testResp.Body.Close()
			fmt.Fprintf(out, "  ✓ torznab caps test via %s indexer %d: HTTP %d\n", inst.Name, idx.ID, testResp.StatusCode)
			return nil
		}
		return fmt.Errorf("no Torznab indexer found on %s", inst.Name)
	}
	return fmt.Errorf("no Sonarr/Radarr in discovered list")
}
