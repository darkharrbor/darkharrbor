package reactivequeue

import (
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func sealed(pairs map[string]string) func(string) (string, bool) {
	return func(ref string) (string, bool) {
		v, ok := pairs[ref]
		return v, ok
	}
}

// RD-34 D8: a wizard-discovered deployment must yield real targets. Before this
// the router built from cfg.Arrs only and saw ZERO, parking everything.
func TestTargetsFromInstancesResolvesSealedCredentials(t *testing.T) {
	instances := []store.ArrInstance{
		{Name: "radarr", URL: "http://radarr:7878/", AppType: "radarr",
			APIKeyRef: "HARRBOR_ARR_RADARR_APIKEY", RootFolder: "/movies", QualityProfile: 14},
		{Name: "sonarr-modern", URL: "http://sonarr-modern:8989", AppType: "sonarr",
			APIKeyRef: "HARRBOR_ARR_SONARR_MODERN_APIKEY"},
	}
	targets, unresolved := TargetsFromInstances(instances, sealed(map[string]string{
		"HARRBOR_ARR_RADARR_APIKEY":        "rk",
		"HARRBOR_ARR_SONARR_MODERN_APIKEY": "sk",
	}))
	if len(targets) != 2 || len(unresolved) != 0 {
		t.Fatalf("targets=%d unresolved=%v", len(targets), unresolved)
	}
	if targets[0].BaseURL != "http://radarr:7878" {
		t.Fatalf("trailing slash not trimmed: %q", targets[0].BaseURL)
	}
	if targets[0].APIKey != "rk" || !targets[0].CanAdd() || targets[0].QualityProfile != 14 {
		t.Fatalf("radarr target = %+v", targets[0])
	}
	// Captured values absent means routable but not addable.
	if targets[1].CanAdd() {
		t.Fatal("an instance with no captured values must not be addable")
	}
}

// An unresolvable sealed reference must EXCLUDE the instance rather than
// contact it unauthenticated, and must surface the name for reporting.
func TestTargetsFromInstancesSkipsUnresolvableCredential(t *testing.T) {
	instances := []store.ArrInstance{
		{Name: "radarr", URL: "http://radarr:7878", AppType: "radarr", APIKeyRef: "MISSING"},
	}
	targets, unresolved := TargetsFromInstances(instances, sealed(map[string]string{}))
	if len(targets) != 0 {
		t.Fatalf("must not build a target without a credential: %+v", targets)
	}
	if len(unresolved) != 1 || unresolved[0] != "radarr" {
		t.Fatalf("unresolved = %v", unresolved)
	}
	// A nil lookup must be safe and must not panic or authenticate blindly.
	if got, _ := TargetsFromInstances(instances, nil); len(got) != 0 {
		t.Fatalf("nil lookup must yield no targets, got %+v", got)
	}
}

// Prowlarr owns no series or movies and is never a commit destination.
func TestTargetsFromInstancesExcludesProwlarr(t *testing.T) {
	targets, _ := TargetsFromInstances([]store.ArrInstance{
		{Name: "prowlarr", URL: "http://prowlarr:9696", AppType: "prowlarr", APIKeyRef: "K"},
	}, sealed(map[string]string{"K": "v"}))
	if len(targets) != 0 {
		t.Fatalf("prowlarr must never be a destination: %+v", targets)
	}
}

// RD-34 D8: discovery is authoritative; env targets are a fallback ONLY when
// discovery yields nothing, so neither surface silently overrides the other.
func TestResolveTargetsPrefersDiscoveryAndFallsBack(t *testing.T) {
	env := []config.ArrTarget{{Name: "env-radarr", BaseURL: "http://env", APIKey: "k"}}
	discovered := []store.ArrInstance{
		{Name: "radarr", URL: "http://radarr:7878", AppType: "radarr", APIKeyRef: "K"},
	}
	lookup := sealed(map[string]string{"K": "v"})

	got, _ := ResolveTargets(discovered, env, lookup)
	if len(got) != 1 || got[0].Name != "radarr" {
		t.Fatalf("discovery must win: %+v", got)
	}

	got, _ = ResolveTargets(nil, env, lookup)
	if len(got) != 1 || got[0].Name != "env-radarr" {
		t.Fatalf("env must be the fallback when discovery is empty: %+v", got)
	}

	if got, _ = ResolveTargets(nil, nil, lookup); len(got) != 0 {
		t.Fatalf("no sources means no targets: %+v", got)
	}
}
