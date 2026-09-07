package doctor

import (
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/premiumize"
	"github.com/darkharrbor/darkharrbor/internal/realdebrid"
	"github.com/darkharrbor/darkharrbor/internal/torbox"
)

// HR7.3: read-only display of detected provider plans/caps and effective
// operation overrides. These tests exercise the pure detail-construction
// helpers directly (no network, no real account) so the deterministic gate
// covers every branch, including the required "unknown provider" fixture
// (an unrecognized TorBox plan code) without needing a real account of
// that shape.

func TestTorboxPlanDetailNilInfo(t *testing.T) {
	got := torboxPlanDetail(nil, time.Now())
	if got != "no account data (nil response)" {
		t.Fatalf("got %q", got)
	}
}

func TestTorboxPlanDetailKnownPlan(t *testing.T) {
	info := &torbox.UserInfo{Plan: 2, IsSubscribed: true} // Pro
	got := torboxPlanDetail(info, time.Now())
	for _, want := range []string{"plan=Pro", "slots=10", "usenet_ingest=true", "news_server=true", "airlock_quota="} {
		if !strings.Contains(got, want) {
			t.Fatalf("detail %q missing %q", got, want)
		}
	}
}

// TestTorboxPlanDetailUnknownPlanFixture is the frozen spec's required
// "unknown-provider fixture" case: an account reporting a plan code DH's
// planTable has never seen (e.g. a new tier TorBox ships later) must
// render distinctly, not silently guess at a known tier or crash.
func TestTorboxPlanDetailUnknownPlanFixture(t *testing.T) {
	info := &torbox.UserInfo{Plan: 99, IsSubscribed: true}
	got := torboxPlanDetail(info, time.Now())
	if !strings.Contains(got, "Unknown (plan=99)") {
		t.Fatalf("detail %q does not distinctly flag the unknown plan", got)
	}
	if !strings.Contains(got, "conservative unknown-plan fallback in effect") {
		t.Fatalf("detail %q should disclose the conservative unknown-plan fallback", got)
	}
	if strings.Contains(got, "verified free-tier") {
		t.Fatalf("detail %q must not call an unknown subscribed plan a verified free tier", got)
	}
}

func TestTorboxPlanDetailUnsubscribedFallsBackFree(t *testing.T) {
	info := &torbox.UserInfo{Plan: 2, IsSubscribed: false}
	got := torboxPlanDetail(info, time.Now())
	if !strings.Contains(got, "plan=Free (not subscribed)") {
		t.Fatalf("detail %q should show the free fallback plan label", got)
	}
}

func TestTorboxPlanDetailNeverLeaksToken(t *testing.T) {
	info := &torbox.UserInfo{Plan: 2, IsSubscribed: true}
	got := torboxPlanDetail(info, time.Now())
	if strings.Contains(got, "://") {
		t.Fatalf("detail %q contains a URL-shaped substring", got)
	}
}

func TestRealDebridPlanDetailNilInfo(t *testing.T) {
	got := realDebridPlanDetail(nil)
	if got != "no account data (nil response)" {
		t.Fatalf("got %q", got)
	}
}

func TestRealDebridPlanDetailPremium(t *testing.T) {
	info := &realdebrid.AccountInfo{Premium: true, ExpirationDays: 42}
	got := realDebridPlanDetail(info)
	for _, want := range []string{"plan=Premium", "slots=100", "max_bytes=unlimited", "expires_in_days=42"} {
		if !strings.Contains(got, want) {
			t.Fatalf("detail %q missing %q", got, want)
		}
	}
}

func TestRealDebridPlanDetailNonPremium(t *testing.T) {
	info := &realdebrid.AccountInfo{Premium: false}
	got := realDebridPlanDetail(info)
	if !strings.Contains(got, "non-Premium") {
		t.Fatalf("detail %q should flag non-Premium", got)
	}
	if strings.Contains(got, "plan=Premium") {
		t.Fatalf("detail %q should not claim Premium", got)
	}
}

func TestRdPremiumMatchesResolveCaps(t *testing.T) {
	if rdPremium(nil) {
		t.Fatal("rdPremium(nil) = true, want false")
	}
	if !rdPremium(&realdebrid.AccountInfo{Premium: true}) {
		t.Fatal("rdPremium(Premium) = false, want true")
	}
}

func TestPremiumizePlanDetail(t *testing.T) {
	now := time.Unix(1_000, 0)
	if got := premiumizePlanDetail(nil, now); got != "no account data (nil response)" {
		t.Fatalf("nil detail=%q", got)
	}
	if got := premiumizePlanDetail(&premiumize.AccountInfo{PremiumUntil: 1_000}, now); !strings.HasPrefix(got, "non-Premium") {
		t.Fatalf("expired detail=%q", got)
	}
	got := premiumizePlanDetail(&premiumize.AccountInfo{
		PremiumUntil: now.Add(25 * time.Hour).Unix(), LimitUsed: .125, BoosterPoints: 3,
	}, now)
	if got != "plan=Premium fair_use_used=12.5% booster_points=3 expires_in_days=2" {
		t.Fatalf("detail=%q", got)
	}
}

func TestNntpOverridesDetailNoOverridesConfigured(t *testing.T) {
	cfg := &config.Config{}
	cfg.Cache.TotalConnections = 16
	cfg.Cache.DemandConnections = 8
	cfg.Cache.ReadaheadConnections = 8
	got := nntpOverridesDetail(cfg)
	// Labels changed 2026-08-22: these are DEFAULTS, not effective values.
	// The old wording asserted "total_connections=16" under a check named
	// "effective overrides" while per-provider overrides governed the pool --
	// which is precisely how the defect this test now guards went unnoticed.
	for _, want := range []string{"defaults: total=16", "demand=8", "readahead=8", "stripe=false", "retention_overrides=none", "per-provider: none"} {
		if !strings.Contains(got, want) {
			t.Fatalf("detail %q missing %q", got, want)
		}
	}
}

// The defect this guards: per-provider pools OVERRIDE the globals, and the
// check must show them. Reporting only the globals is confidently wrong, which
// is worse than reporting nothing -- it misled this project's own operator into
// believing Newshosting ran 16 connections when it runs 95.
func TestNntpOverridesDetailShowsPerProviderPools(t *testing.T) {
	t.Setenv("HARRBOR_PROVIDER_NEWSHOSTING_TOTAL_CONNECTIONS", "95")
	t.Setenv("HARRBOR_PROVIDER_NEWSHOSTING_DEMAND_CONNECTIONS", "56")
	t.Setenv("HARRBOR_PROVIDER_NEWSHOSTING_READAHEAD_CONNECTIONS", "39")
	cfg := &config.Config{}
	cfg.Cache.TotalConnections = 16
	cfg.Cache.DemandConnections = 8
	cfg.Cache.ReadaheadConnections = 8
	got := nntpOverridesDetail(cfg)
	for _, want := range []string{"per-provider:", "newshosting=total:95", "split:56/39"} {
		if !strings.Contains(got, want) {
			t.Fatalf("detail %q missing %q -- per-provider pools must be reported", got, want)
		}
	}
	if strings.Contains(got, "per-provider: none") {
		t.Fatalf("detail claims no per-provider pools while one is configured: %q", got)
	}
}

func TestNntpOverridesDetailWithRetentionOverrides(t *testing.T) {
	t.Setenv("HARRBOR_PROVIDER_NEWSHOSTING_RETENTION_DAYS", "3000")
	t.Setenv("HARRBOR_PROVIDER_TORBOX_RETENTION_DAYS", "1200")
	cfg := &config.Config{}
	cfg.Cache.TotalConnections = 20
	cfg.NNTP.Stripe = true
	got := nntpOverridesDetail(cfg)
	if !strings.Contains(got, "stripe=true") {
		t.Fatalf("detail %q missing stripe=true", got)
	}
	if !strings.Contains(got, "newshosting=3000d") || !strings.Contains(got, "torbox=1200d") {
		t.Fatalf("detail %q missing expected retention overrides", got)
	}
}

func TestNntpOverridesDetailIgnoresInvalidRetention(t *testing.T) {
	t.Setenv("HARRBOR_PROVIDER_NEWSHOSTING_RETENTION_DAYS", "not-a-number")
	cfg := &config.Config{}
	got := nntpOverridesDetail(cfg)
	if !strings.Contains(got, "retention_overrides=none") {
		t.Fatalf("detail %q should abstain on an invalid retention value, not guess", got)
	}
}
