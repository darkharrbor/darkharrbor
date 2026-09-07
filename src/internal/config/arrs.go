package config

import (
	"strconv"
	"strings"
)

// ArrTarget is one Sonarr/Radarr instance DH notifies after pruning failed
// items, so the arr's queue (a locally cached table, resynced with the
// download client on the arr's own internal timer — not on every read)
// reflects the cleanup immediately instead of showing a stale "failed"
// entry until the arr's next scheduled poll. Both Sonarr and Radarr expose
// the same command name (RefreshMonitoredDownloads) so one code path covers
// all instances regardless of kind.
type ArrTarget struct {
	// Name is the configured identifier (HARRBOR_ARR_NAMES entry), used only
	// for logging.
	Name string
	// BaseURL is the arr's API base, e.g. http://sonarr-modern:8989.
	BaseURL string
	// APIKey authenticates the command call (X-Api-Key header).
	APIKey string
	// RootFolder and QualityProfile are RD-34 D4's WIZARD-CAPTURED values,
	// taken from this instance's own real root folders and quality profiles.
	// They are what `addOwner` needs to add content no Arr yet owns. Absent
	// either one the instance can be routed TO but nothing can be ADDED, so
	// automatic dispatch parks. That is the shipping default.
	RootFolder     string
	QualityProfile int
}

// CanAdd reports whether automatic dispatch may ADD content to this instance.
// RD-34 D4: both wizard-captured values are required. Scope is deliberately
// NOT part of this -- the per-instance label already exists as the DarkHarrbor
// download-client category, and RD-34 D2 rules that DarkHarrbor must never
// interpret what a user's labels MEAN.
func (t ArrTarget) CanAdd() bool {
	return t.RootFolder != "" && t.QualityProfile > 0
}

// Env surface:
//
//	HARRBOR_ARR_NAMES              comma-separated instance names (unset/empty
//	                                = feature disabled, zero targets)
//	HARRBOR_ARR_<NAME>_URL          e.g. http://sonarr-modern:8989
//	HARRBOR_ARR_<NAME>_APIKEY       the arr's API key
//
// <NAME> is the name from HARRBOR_ARR_NAMES, uppercased with '-' replaced by
// '_' (so "sonarr-modern" reads HARRBOR_ARR_SONARR_MODERN_URL). Resolved
// through Config.Lookup (sealed secrets first, then env), matching the
// per-provider HARRBOR_PROVIDER_<NAME>_* convention elsewhere in this
// package. A name with no URL configured is skipped (logged, non-fatal) —
// this is deliberately optional infrastructure, not required for DH to run.
func applyArrTargets(cfg *Config) {
	raw, set := cfg.Lookup("HARRBOR_ARR_NAMES")
	if !set || strings.TrimSpace(raw) == "" {
		return
	}
	for _, part := range strings.Split(raw, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		envName := strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
		urlVal, ok := cfg.Lookup("HARRBOR_ARR_" + envName + "_URL")
		if !ok || strings.TrimSpace(urlVal) == "" {
			continue
		}
		apiKey, _ := cfg.Lookup("HARRBOR_ARR_" + envName + "_APIKEY")
		rootFolder, _ := cfg.Lookup("HARRBOR_ARR_" + envName + "_ROOTFOLDER")
		profileRaw, _ := cfg.Lookup("HARRBOR_ARR_" + envName + "_QUALITYPROFILE")
		profile, err := strconv.Atoi(strings.TrimSpace(profileRaw))
		if err != nil || profile <= 0 {
			profile = 0
		}
		cfg.Arrs = append(cfg.Arrs, ArrTarget{
			Name:           name,
			BaseURL:        strings.TrimRight(strings.TrimSpace(urlVal), "/"),
			APIKey:         strings.TrimSpace(apiKey),
			RootFolder:     strings.TrimSpace(rootFolder),
			QualityProfile: profile,
		})
	}
}
