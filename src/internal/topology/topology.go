// Package topology defines the three supported DarkHarrbor deployment profiles.
package topology

import "strings"

// Name identifies one supported deployment profile.
type Name string

const (
	T1 Name = "t1"
	T2 Name = "t2"
	T3 Name = "t3"
)

// Features is the capability set made available by a topology. Exact matching
// rejects partial base profiles; independently optional features such as
// promotion still require their own runtime enable switch.
type Features struct {
	AggregatedDiscovery bool
	UniversalProxy      bool
	ReactiveCommit      bool
	Promotion           bool
}

// Parse returns a canonical supported name. Invalid input returns the safe T1
// default with ok=false so callers can warn without failing startup.
func Parse(raw string) (name Name, ok bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(T1):
		return T1, true
	case string(T2):
		return T2, true
	case string(T3):
		return T3, true
	default:
		return T1, false
	}
}

// Profile returns the exact capability combination owned by name.
func Profile(name Name) (Features, bool) {
	switch name {
	case T1:
		return Features{}, true
	case T2:
		return Features{AggregatedDiscovery: true, UniversalProxy: true}, true
	case T3:
		return Features{AggregatedDiscovery: true, UniversalProxy: true, ReactiveCommit: true, Promotion: true}, true
	default:
		return Features{}, false
	}
}

// Match identifies an exact supported feature combination.
func Match(features Features) (Name, bool) {
	for _, name := range [...]Name{T1, T2, T3} {
		profile, _ := Profile(name)
		if profile == features {
			return name, true
		}
	}
	return "", false
}

// MissingCapabilities reports the compiled capabilities required by a profile
// but unavailable in this binary. Aggregated discovery is external and is not a
// binary capability.
func MissingCapabilities(name Name, available Features) []string {
	required, ok := Profile(name)
	if !ok {
		return []string{"supported-profile"}
	}
	missing := make([]string, 0, 3)
	if required.UniversalProxy && !available.UniversalProxy {
		missing = append(missing, "universal-playback-proxy")
	}
	if required.ReactiveCommit && !available.ReactiveCommit {
		missing = append(missing, "reactive-commit")
	}
	if required.Promotion && !available.Promotion {
		missing = append(missing, "promotion")
	}
	return missing
}

// Implemented describes capabilities present in the current binary. Later rows
// update this single owner when their capability is actually implemented.
func Implemented() Features {
	return Features{UniversalProxy: true, ReactiveCommit: true, Promotion: true}
}
