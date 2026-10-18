package config

import (
	"strings"
	"testing"
)

func TestValidateReportsAllMissingCredentialsTogether(t *testing.T) {
	cfg := defaultConfig()
	cfg.Server.StreamSecret = ""
	cfg.Auth.QBitPassword = ""
	cfg.Auth.SABAPIKey = ""
	cfg.Routing.Preference, cfg.Routing.PreferenceExplicit = applyPreference(PreferenceNone)
	cfg.applyDerived()

	err := cfg.Validate()
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"HARRBOR_STREAM_SECRET", "HARRBOR_QBIT_PASSWORD", "HARRBOR_SAB_API_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}
