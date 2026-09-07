package config

import "testing"

// RD-34 D4: automatic dispatch may ADD to an instance only when both
// wizard-captured values are present. Scope is deliberately not part of this.
func TestArrTargetCanAddRequiresBothCapturedValues(t *testing.T) {
	if !(ArrTarget{RootFolder: "/data", QualityProfile: 4}).CanAdd() {
		t.Fatal("both captured values must permit adding")
	}
	for name, target := range map[string]ArrTarget{
		"nothing captured": {},
		"no root":          {QualityProfile: 4},
		"no profile":       {RootFolder: "/data"},
		"zero profile":     {RootFolder: "/data", QualityProfile: 0},
		"negative profile": {RootFolder: "/data", QualityProfile: -1},
	} {
		if target.CanAdd() {
			t.Fatalf("%s must not permit adding", name)
		}
	}
}

// A malformed quality profile must leave the instance unaddable rather than
// defaulting to some profile the operator never chose.
func TestApplyArrTargetsRejectsMalformedQualityProfile(t *testing.T) {
	withEnv(t, map[string]string{
		"HARRBOR_ARR_NAMES":                 "radarr",
		"HARRBOR_ARR_RADARR_URL":            "http://radarr:7878",
		"HARRBOR_ARR_RADARR_ROOTFOLDER":     "/movies",
		"HARRBOR_ARR_RADARR_QUALITYPROFILE": "not-a-number",
	})
	cfg := &Config{}
	applyArrTargets(cfg)
	if len(cfg.Arrs) != 1 {
		t.Fatalf("targets = %d", len(cfg.Arrs))
	}
	if cfg.Arrs[0].CanAdd() {
		t.Fatal("a malformed quality profile must leave the instance unaddable")
	}
	if cfg.Arrs[0].RootFolder != "/movies" {
		t.Fatalf("root folder = %q", cfg.Arrs[0].RootFolder)
	}
}

// The captured values parse into the target when well formed.
func TestApplyArrTargetsCapturesAddValues(t *testing.T) {
	withEnv(t, map[string]string{
		"HARRBOR_ARR_NAMES":                 "radarr",
		"HARRBOR_ARR_RADARR_URL":            "http://radarr:7878",
		"HARRBOR_ARR_RADARR_ROOTFOLDER":     "/movies",
		"HARRBOR_ARR_RADARR_QUALITYPROFILE": "14",
	})
	cfg := &Config{}
	applyArrTargets(cfg)
	if len(cfg.Arrs) != 1 || !cfg.Arrs[0].CanAdd() || cfg.Arrs[0].QualityProfile != 14 {
		t.Fatalf("targets = %+v", cfg.Arrs)
	}
}
