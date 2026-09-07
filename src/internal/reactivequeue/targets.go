package reactivequeue

import (
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// TargetsFromInstances builds router targets from the wizard-discovered
// `arr_instances` records, which RD-34 D8 makes the single source of truth.
//
// Before this the router was constructed from `cfg.Arrs`, populated only when
// `HARRBOR_ARR_NAMES` is set. A deployment that discovered its arrs through
// `darkharrbor setup` leaves that key unset, so the router saw ZERO targets and
// parked everything regardless of what was configured.
//
// The credential is NEVER stored here. `APIKeyRef` is a sealed-secrets key
// NAME, and lookup resolves it at use time through the same path doctor and
// reconcile-arr already use. An instance whose reference cannot be resolved is
// SKIPPED rather than contacted without authentication, and its name is
// returned so a caller can report it without printing the reference value.
func TargetsFromInstances(instances []store.ArrInstance, lookup func(string) (string, bool)) ([]config.ArrTarget, []string) {
	var targets []config.ArrTarget
	var unresolved []string
	for _, inst := range instances {
		// Prowlarr is not a commit destination; it owns no series or movies.
		switch strings.ToLower(strings.TrimSpace(inst.AppType)) {
		case "sonarr", "radarr":
		default:
			continue
		}
		url := strings.TrimRight(strings.TrimSpace(inst.URL), "/")
		if url == "" {
			continue
		}
		apiKey := ""
		if lookup != nil {
			if resolved, ok := lookup(strings.TrimSpace(inst.APIKeyRef)); ok {
				apiKey = strings.TrimSpace(resolved)
			}
		}
		if apiKey == "" {
			unresolved = append(unresolved, inst.Name)
			continue
		}
		targets = append(targets, config.ArrTarget{
			Name:           inst.Name,
			BaseURL:        url,
			APIKey:         apiKey,
			RootFolder:     strings.TrimSpace(inst.RootFolder),
			QualityProfile: inst.QualityProfile,
		})
	}
	return targets, unresolved
}

// ResolveTargets picks the target list the router should use. RD-34 D8 makes
// `arr_instances` authoritative; the environment-declared `cfg.Arrs` remains a
// fallback ONLY for deployments that never ran discovery, so neither surface
// silently overrides the other.
func ResolveTargets(instances []store.ArrInstance, envTargets []config.ArrTarget, lookup func(string) (string, bool)) ([]config.ArrTarget, []string) {
	targets, unresolved := TargetsFromInstances(instances, lookup)
	if len(targets) == 0 {
		return append([]config.ArrTarget(nil), envTargets...), unresolved
	}
	return targets, unresolved
}
