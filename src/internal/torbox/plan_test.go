package torbox

import (
	"testing"
	"time"
)

func TestResolveCaps_NilInfo_FreeFallback(t *testing.T) {
	slots, maxBytes, usenet, freeTier := ResolveCaps(nil, time.Now())
	if slots != 1 {
		t.Errorf("slots = %d, want 1", slots)
	}
	if maxBytes != 10<<30 {
		t.Errorf("maxBytes = %d, want %d", maxBytes, int64(10<<30))
	}
	if usenet {
		t.Error("usenet = true, want false for free fallback")
	}
	if !freeTier {
		t.Error("freeTier = false, want true for nil info")
	}
}

func TestResolveCaps_Unsubscribed_FreeFallback(t *testing.T) {
	info := &UserInfo{Plan: 2, IsSubscribed: false, AdditionalSlots: 5}
	slots, _, usenet, freeTier := ResolveCaps(info, time.Now())
	if slots != 1 {
		t.Errorf("slots = %d, want 1 (unsubscribed forces Free fallback)", slots)
	}
	if usenet {
		t.Error("usenet = true, want false for unsubscribed")
	}
	if !freeTier {
		t.Error("freeTier = false, want true for unsubscribed account")
	}
}

func TestResolveCaps_ExpiredPremium_FreeFallback(t *testing.T) {
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	info := &UserInfo{
		Plan:             2,
		IsSubscribed:     true,
		PremiumExpiresAt: now.Add(-24 * time.Hour), // lapsed yesterday
	}
	slots, _, _, freeTier := ResolveCaps(info, now)
	if slots != 1 {
		t.Errorf("slots = %d, want 1 (expired premium forces Free fallback)", slots)
	}
	if !freeTier {
		t.Error("freeTier = false, want true for expired premium")
	}
}

func TestResolveCaps_Pro_NotFreeTier(t *testing.T) {
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	info := &UserInfo{
		Plan:             2, // Pro
		IsSubscribed:     true,
		PremiumExpiresAt: now.Add(30 * 24 * time.Hour),
		AdditionalSlots:  2,
	}
	slots, maxBytes, usenet, freeTier := ResolveCaps(info, now)
	if slots != 12 { // baseSlots 10 + 2 additional
		t.Errorf("slots = %d, want 12", slots)
	}
	if maxBytes != 1<<40 {
		t.Errorf("maxBytes = %d, want %d", maxBytes, int64(1<<40))
	}
	if !usenet {
		t.Error("usenet = false, want true for Pro")
	}
	if freeTier {
		t.Error("freeTier = true, want false for an active Pro subscription")
	}
}

func TestResolveCaps_UnknownPlanCode_FreeFallback(t *testing.T) {
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	info := &UserInfo{
		Plan:             99, // not in planTable
		IsSubscribed:     true,
		PremiumExpiresAt: now.Add(30 * 24 * time.Hour),
	}
	_, _, _, freeTier := ResolveCaps(info, now)
	if !freeTier {
		t.Error("freeTier = false, want true for an unrecognized plan code")
	}
}

func TestAccountPlanSeparatesUnknownFromFreeFallback(t *testing.T) {
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	label, verifiedFree, unknown := AccountPlan(&UserInfo{
		Plan:             99,
		IsSubscribed:     true,
		PremiumExpiresAt: now.Add(30 * 24 * time.Hour),
	}, now)
	if label != "Unknown (plan=99)" || verifiedFree || !unknown {
		t.Fatalf("AccountPlan unknown = (%q, %t, %t)", label, verifiedFree, unknown)
	}
}

func TestAccountPlanExpiredSubscriptionIsFreeState(t *testing.T) {
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	label, verifiedFree, unknown := AccountPlan(&UserInfo{
		Plan:             2,
		IsSubscribed:     true,
		PremiumExpiresAt: now.Add(-time.Hour),
	}, now)
	if label != "Free (not subscribed)" || !verifiedFree || unknown {
		t.Fatalf("AccountPlan expired = (%q, %t, %t)", label, verifiedFree, unknown)
	}
}

func TestNewsServerCapable_NilInfo(t *testing.T) {
	if NewsServerCapable(nil, time.Now()) {
		t.Error("NewsServerCapable(nil) = true, want false")
	}
}

func TestNewsServerCapable_Unsubscribed(t *testing.T) {
	info := &UserInfo{Plan: 2, IsSubscribed: false}
	if NewsServerCapable(info, time.Now()) {
		t.Error("NewsServerCapable(unsubscribed Pro) = true, want false")
	}
}

func TestNewsServerCapable_ExpiredPremium(t *testing.T) {
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	info := &UserInfo{
		Plan:             2,
		IsSubscribed:     true,
		PremiumExpiresAt: now.Add(-24 * time.Hour),
	}
	if NewsServerCapable(info, now) {
		t.Error("NewsServerCapable(expired Pro) = true, want false")
	}
}

func TestNewsServerCapable_Pro(t *testing.T) {
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	info := &UserInfo{
		Plan:             2,
		IsSubscribed:     true,
		PremiumExpiresAt: now.Add(30 * 24 * time.Hour),
	}
	if !NewsServerCapable(info, now) {
		t.Error("NewsServerCapable(active Pro) = false, want true")
	}
}

func TestNewsServerCapable_NonProPaidPlans(t *testing.T) {
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for _, plan := range []int{0, 1, 3} { // Free, Essential, Standard
		info := &UserInfo{
			Plan:             plan,
			IsSubscribed:     true,
			PremiumExpiresAt: now.Add(30 * 24 * time.Hour),
		}
		if NewsServerCapable(info, now) {
			t.Errorf("NewsServerCapable(plan=%d) = true, want false (Pro-only)", plan)
		}
	}
}

func TestNewsServerCapable_UnknownPlanCode(t *testing.T) {
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	info := &UserInfo{
		Plan:             99,
		IsSubscribed:     true,
		PremiumExpiresAt: now.Add(30 * 24 * time.Hour),
	}
	if NewsServerCapable(info, now) {
		t.Error("NewsServerCapable(unknown plan) = true, want false")
	}
}

func TestAirlockQuotaBytes_NilInfo(t *testing.T) {
	if got := AirlockQuotaBytes(nil, time.Now()); got != 0 {
		t.Errorf("AirlockQuotaBytes(nil) = %d, want 0", got)
	}
}

func TestAirlockQuotaBytes_Unsubscribed(t *testing.T) {
	info := &UserInfo{Plan: 2, IsSubscribed: false}
	if got := AirlockQuotaBytes(info, time.Now()); got != 0 {
		t.Errorf("AirlockQuotaBytes(unsubscribed) = %d, want 0", got)
	}
}

func TestAirlockQuotaBytes_PerPlan(t *testing.T) {
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		plan int
		want int64
	}{
		{0, 0},         // Free
		{1, 300 << 30}, // Essential
		{3, 500 << 30}, // Standard
		{2, 1 << 40},   // Pro
	}
	for _, tc := range cases {
		info := &UserInfo{
			Plan:             tc.plan,
			IsSubscribed:     true,
			PremiumExpiresAt: now.Add(30 * 24 * time.Hour),
		}
		if got := AirlockQuotaBytes(info, now); got != tc.want {
			t.Errorf("AirlockQuotaBytes(plan=%d) = %d, want %d", tc.plan, got, tc.want)
		}
	}
}

func TestAirlockQuotaBytes_ExpiredPremium(t *testing.T) {
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	info := &UserInfo{
		Plan:             2,
		IsSubscribed:     true,
		PremiumExpiresAt: now.Add(-24 * time.Hour),
	}
	if got := AirlockQuotaBytes(info, now); got != 0 {
		t.Errorf("AirlockQuotaBytes(expired Pro) = %d, want 0", got)
	}
}

func TestAirlockQuotaBytes_UnknownPlanCode(t *testing.T) {
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	info := &UserInfo{
		Plan:             99,
		IsSubscribed:     true,
		PremiumExpiresAt: now.Add(30 * 24 * time.Hour),
	}
	if got := AirlockQuotaBytes(info, now); got != 0 {
		t.Errorf("AirlockQuotaBytes(unknown plan) = %d, want 0", got)
	}
}
