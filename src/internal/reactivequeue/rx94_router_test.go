package reactivequeue

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// seriesTargets stands up n Sonarr-shaped servers that own nothing, which is
// the first-seen case RX-9.4 exists for.
func seriesTargets(t *testing.T, n int, decorate func(int, *config.ArrTarget)) []config.ArrTarget {
	t.Helper()
	var targets []config.ArrTarget
	for i := 0; i < n; i++ {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v3/series" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(`[]`))
		}))
		t.Cleanup(srv.Close)
		target := config.ArrTarget{Name: fmt.Sprintf("sonarr-%d", i), BaseURL: srv.URL, APIKey: "redacted"}
		if decorate != nil {
			decorate(i, &target)
		}
		targets = append(targets, target)
	}
	return targets
}

func resolveSeries(t *testing.T, targets []config.ArrTarget, defaultArr ...string) RouteResult {
	t.Helper()
	repo := &assignmentMemory{items: make(map[string]store.ReactiveAssignment)}
	router, err := NewRouter(repo, targets, func() time.Time { return time.Unix(1, 0) })
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	if len(defaultArr) == 1 {
		router.SetDefaultDestinations(defaultArr[0], "")
	}
	route, err := router.Resolve(context.Background(), &store.ProviderIdentity{
		Kind: "series", IDs: store.ProviderIDs{IMDB: "tt0944947"},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return route
}

// RD-34 D2: more than one instance of a kind is NOT DETERMINED. Wizard-captured
// add values do NOT make it determined -- only an explicitly named default does.
func TestResolveMultipleInstancesParkWithoutNamedDefault(t *testing.T) {
	targets := seriesTargets(t, 3, func(_ int, target *config.ArrTarget) {
		target.RootFolder = "/tv"
		target.QualityProfile = 4
	})
	route := resolveSeries(t, targets)
	if route.Declared || route.Reason != "routing_unresolved" {
		t.Fatalf("multi-instance must park for a human, got %+v", route)
	}
}

// RD-34 D3: an explicitly named default IS the operator's choice, not an
// inference, so it routes and carries the add fields.
func TestResolveUsesNamedDefaultDestination(t *testing.T) {
	targets := seriesTargets(t, 3, func(_ int, target *config.ArrTarget) {
		target.RootFolder = "/tv"
		target.QualityProfile = 4
	})
	route := resolveSeries(t, targets, "sonarr-1")
	if !route.Declared || route.ArrName != "sonarr-1" || route.Owned {
		t.Fatalf("named default must route unowned, got %+v", route)
	}
	if route.RootFolder != "/tv" || route.QualityProfile != 4 {
		t.Fatalf("add fields not carried: %+v", route)
	}
}

// A stale or unaddable default must PARK rather than misfile.
func TestResolveIgnoresUnusableDefault(t *testing.T) {
	targets := seriesTargets(t, 3, func(i int, target *config.ArrTarget) {
		if i == 1 {
			target.RootFolder = "/tv"
		}
	})
	if route := resolveSeries(t, targets, "sonarr-1"); route.Declared {
		t.Fatalf("a default that cannot add must not route: %+v", route)
	}
	if route := resolveSeries(t, targets, "sonarr-does-not-exist"); route.Declared {
		t.Fatalf("an unknown default must not route: %+v", route)
	}
}

// A SINGLE compatible instance missing its wizard-captured values can be routed
// to but nothing may be ADDED, so dispatch still parks. Shipping default OFF.
func TestResolveSingleInstanceWithoutAddValuesStaysUnaddable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*config.ArrTarget)
	}{
		{"nothing captured", func(*config.ArrTarget) {}},
		{"no root folder", func(target *config.ArrTarget) { target.QualityProfile = 4 }},
		{"no quality profile", func(target *config.ArrTarget) { target.RootFolder = "/tv" }},
		{"zero quality profile", func(target *config.ArrTarget) {
			target.RootFolder, target.QualityProfile = "/tv", 0
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route := resolveSeries(t, seriesTargets(t, 1, func(_ int, target *config.ArrTarget) {
				tc.mutate(target)
			}))
			if route.Declared || route.RootFolder != "" || route.QualityProfile != 0 {
				t.Fatalf("must expose no add fields: %+v", route)
			}
		})
	}
}

// A SINGLE compatible target keeps its pre-RX-9.4 tier and name so RX-4.2's
// queue suggestion is unchanged; only the declared add fields appear.
func TestResolveSingleCompatibleKeepsTierAndGainsDeclaredFields(t *testing.T) {
	declared := seriesTargets(t, 1, func(_ int, target *config.ArrTarget) {
		target.RootFolder = "/data/series"
		target.QualityProfile = 7
	})
	route := resolveSeries(t, declared)
	if route.Tier != 0 || route.ArrName != "sonarr-0" || !route.Declared || route.QualityProfile != 7 {
		t.Fatalf("declared single compatible: %+v", route)
	}

	route = resolveSeries(t, seriesTargets(t, 1, nil))
	if route.Tier != 0 || route.ArrName != "sonarr-0" || route.Owned || route.Declared {
		t.Fatalf("undeclared single compatible must be unchanged: %+v", route)
	}
	if route.RootFolder != "" || route.QualityProfile != 0 {
		t.Fatalf("undeclared target must expose no add fields: %+v", route)
	}
}
