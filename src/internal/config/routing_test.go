package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestUncachedTorrentPolicyModes(t *testing.T) {
	tests := []struct {
		name       string
		preference []string
		allowed    bool
		deranked   bool
	}{
		{name: "deny", preference: []string{LaneTorBoxTorrent}},
		{name: "allow", preference: []string{LaneTorBoxTorrent, LaneUncachedTorrent}, allowed: true},
		{name: "derank", preference: []string{LaneTorBoxTorrent, LaneUncachedTorrentDerank}, allowed: true, deranked: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{}
			cfg.Routing.Preference = tt.preference
			if got := cfg.UncachedTorrentAllowed(); got != tt.allowed {
				t.Fatalf("UncachedTorrentAllowed() = %v, want %v", got, tt.allowed)
			}
			if got := cfg.UncachedTorrentDeranked(); got != tt.deranked {
				t.Fatalf("UncachedTorrentDeranked() = %v, want %v", got, tt.deranked)
			}
		})
	}
}

func TestParsePreferenceKeepsFirstUncachedPolicy(t *testing.T) {
	got := parsePreference(strings.Join([]string{
		LaneTorBoxTorrent,
		LaneUncachedTorrentDerank,
		LaneUncachedTorrent,
	}, ","))
	want := []string{LaneTorBoxTorrent, LaneUncachedTorrentDerank}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsePreference() = %v, want %v", got, want)
	}
}

func TestValidateTuningRejectsBothUncachedPolicies(t *testing.T) {
	err := ValidateTuning(map[string]string{
		"HARRBOR_PREFERENCE": LaneTorBoxTorrent + "," + LaneUncachedTorrent + "," + LaneUncachedTorrentDerank,
	})
	if err == nil || !strings.Contains(err.Error(), "cannot contain both") {
		t.Fatalf("ValidateTuning() error = %v, want mutually-exclusive-policy error", err)
	}
}

func TestExplicitNoProviderLanesDoesNotRequireTorBox(t *testing.T) {
	cfg := defaultConfig()
	cfg.Server.StreamSecret = strings.Repeat("s", 32)
	cfg.Auth.QBitPassword = "not-the-default"
	cfg.Auth.SABAPIKey = "not-the-default"
	cfg.Routing.Preference, cfg.Routing.PreferenceExplicit = applyPreference(PreferenceNone)
	cfg.applyDerived()

	if err := cfg.Validate(); err != nil {
		t.Fatalf("HTTP-only configuration rejected: %v", err)
	}
	if len(cfg.Routing.Preference) != 0 || !cfg.Routing.PreferenceExplicit {
		t.Fatalf("preference = %v explicit=%v, want explicit none", cfg.Routing.Preference, cfg.Routing.PreferenceExplicit)
	}
}

func TestOmittedPreferenceSelectsNoLane(t *testing.T) {
	cfg := defaultConfig()
	cfg.resolvePreference()

	if len(cfg.Routing.Preference) != 0 || cfg.Routing.PreferenceExplicit {
		t.Fatalf("preference = %v explicit=%v, want unselected", cfg.Routing.Preference, cfg.Routing.PreferenceExplicit)
	}
}

func TestSelectedTorBoxLaneStillRequiresCredential(t *testing.T) {
	cfg := defaultConfig()
	cfg.Server.StreamSecret = strings.Repeat("s", 32)
	cfg.Auth.QBitPassword = "not-the-default"
	cfg.Auth.SABAPIKey = "not-the-default"
	cfg.Routing.Preference = []string{LaneTorBoxTorrent}
	cfg.Routing.PreferenceExplicit = true

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "torbox.api_token is required") {
		t.Fatalf("Validate() error = %v, want selected-TorBox credential failure", err)
	}
}
