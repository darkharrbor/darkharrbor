package torbox

import (
	"fmt"
	"time"
)

type planCaps struct {
	baseSlots         int
	maxBytes          int64
	usenet            bool
	newsServer        bool  // TorBox NNTP News Server (v9.0.0) — Pro-only, see NewsServerCapable
	airlockQuotaBytes int64 // TorBox AirLock permanent-storage quota (v9.0.0), see AirlockQuotaBytes
}

// planTable maps TorBox plan code to the plan capabilities Dark Harrbor needs
// for startup policy/capability routing.
//
// AirLock quota source: support.torbox.app "TorBox AirLock" article
// (live-confirmed 2026-07-02): Pro 1TB, Standard 500GB, Essential 300GB,
// Free 0GB — a free perk on every paid tier, not a separate purchase.
var planTable = map[int]planCaps{
	0: {baseSlots: 1, maxBytes: 10 << 30, usenet: false, newsServer: false, airlockQuotaBytes: 0},          // Free
	1: {baseSlots: 3, maxBytes: 200 << 30, usenet: false, newsServer: false, airlockQuotaBytes: 300 << 30}, // Essential
	3: {baseSlots: 5, maxBytes: 200 << 30, usenet: false, newsServer: false, airlockQuotaBytes: 500 << 30}, // Standard
	2: {baseSlots: 10, maxBytes: 1 << 40, usenet: true, newsServer: true, airlockQuotaBytes: 1 << 40},      // Pro
}

// freeCaps is the conservative fallback when discovery fails, the plan is
// unknown, or subscription status says premium capabilities are not active.
var freeCaps = planCaps{baseSlots: 1, maxBytes: 10 << 30, usenet: false}

// ResolveCaps computes effective TorBox capabilities from discovered account
// data. Nil info, lapsed subscription, or unknown plan codes conservatively
// return Free capabilities (and freeTier=true). Additional slots are added to
// the plan base slots, with a minimum effective slot count of one.
//
// freeTier is true whenever the effective caps are the Free-plan fallback —
// it feeds the R2 governor redesign's extra Free-tier time-windowed budgets
// (1/24h + 10/month), which do not apply to paid plans.
func ResolveCaps(info *UserInfo, now time.Time) (slots int, maxBytes int64, usenetCapable bool, freeTier bool) {
	if info == nil {
		c := freeCaps
		return c.baseSlots, c.maxBytes, c.usenet, true
	}
	if !info.IsSubscribed || (!now.IsZero() && !info.PremiumExpiresAt.IsZero() && info.PremiumExpiresAt.Before(now)) {
		c := freeCaps
		return c.baseSlots, c.maxBytes, c.usenet, true
	}
	caps, ok := planTable[info.Plan]
	if !ok {
		caps = freeCaps
		slots = caps.baseSlots + info.AdditionalSlots
		if slots < 1 {
			slots = 1
		}
		return slots, caps.maxBytes, caps.usenet, true
	}
	slots = caps.baseSlots + info.AdditionalSlots
	if slots < 1 {
		slots = 1
	}
	// info.Plan == 0 (Free) with an active subscription flag is not a real
	// TorBox combination, but treat it as free-tier defensively all the same.
	freeTier = info.Plan == 0
	return slots, caps.maxBytes, caps.usenet, freeTier
}

// AccountPlan describes the account state separately from ResolveCaps' safe
// runtime fallback. An active, unrecognized plan is not a verified Free tier:
// DarkHarrbor still applies conservative Free-level caps, but callers must
// present the tier as unknown and must not make Free-tier recommendations.
func AccountPlan(info *UserInfo, now time.Time) (label string, verifiedFreeTier, unknownFallback bool) {
	if info == nil {
		return "Unknown (no account data)", false, true
	}
	if !info.IsSubscribed || (!now.IsZero() && !info.PremiumExpiresAt.IsZero() && info.PremiumExpiresAt.Before(now)) {
		return "Free (not subscribed)", true, false
	}
	if _, ok := planTable[info.Plan]; !ok {
		return PlanLabel(info.Plan, true), false, true
	}
	return PlanLabel(info.Plan, true), info.Plan == 0, false
}

// NewsServerCapable reports whether the account's discovered plan grants
// access to TorBox's NNTP News Server (v9.0.0). Deliberately kept separate
// from ResolveCaps' usenetCapable (which currently happens to coincide with
// it, both Pro-gated today) rather than reusing that flag directly: the two
// are logically distinct TorBox account capabilities (one is "TorBox can
// download usenet content for you", the other is "TorBox will hand you raw
// NNTP credentials") that happen to share a plan gate today but are not
// guaranteed to stay coupled if TorBox's plan tiers change independently.
// Same conservative-on-failure shape as ResolveCaps: nil info, a lapsed
// subscription, or an unrecognized plan code all report false.
func NewsServerCapable(info *UserInfo, now time.Time) bool {
	if info == nil {
		return false
	}
	if !info.IsSubscribed || (!now.IsZero() && !info.PremiumExpiresAt.IsZero() && info.PremiumExpiresAt.Before(now)) {
		return false
	}
	caps, ok := planTable[info.Plan]
	if !ok {
		return false
	}
	return caps.newsServer
}

// AirlockQuotaBytes returns the account's TorBox AirLock permanent-storage
// quota (v9.0.0) in bytes, per its discovered plan. Unlike NewsServerCapable
// this is available on every paid tier, not just Pro — Free-tier accounts
// (including nil info, lapsed subscription, or an unrecognized plan code)
// get 0, meaning "no AirLock capability", consistent with TorBox's own
// published Free-tier allotment. No governor/janitor interaction needed:
// Airlocked items behave identically to normal cached files (confirmed via
// support docs — they don't occupy an Allowed Active Slot any differently
// than a non-Airlocked cached item, they're just exempt from the 30-day
// inactivity purge), so this is a pure capability/quota fact for a future
// UI to use, not a scheduling input like slots or the governor budget.
func AirlockQuotaBytes(info *UserInfo, now time.Time) int64 {
	if info == nil {
		return 0
	}
	if !info.IsSubscribed || (!now.IsZero() && !info.PremiumExpiresAt.IsZero() && info.PremiumExpiresAt.Before(now)) {
		return 0
	}
	caps, ok := planTable[info.Plan]
	if !ok {
		return 0
	}
	return caps.airlockQuotaBytes
}

// PlanLabel renders a TorBox plan code as an operator-facing name. Unknown
// plan codes render distinctly ("Unknown (plan=N)") rather than being
// silently mapped to a guessed tier name -- the same conservative-on-unknown
// posture as ResolveCaps/NewsServerCapable/AirlockQuotaBytes. Shared owner
// for both the setup wizard and read-only diagnostics (HR7.3): no second
// plan-naming table.
func PlanLabel(plan int, subscribed bool) string {
	if !subscribed {
		return "Free (not subscribed)"
	}
	switch plan {
	case 0:
		return "Free"
	case 1:
		return "Essential"
	case 2:
		return "Pro"
	case 3:
		return "Standard"
	default:
		return fmt.Sprintf("Unknown (plan=%d)", plan)
	}
}
