package config

import "testing"

func TestMediaFlowConfigurationIsOptionalAndSealable(t *testing.T) {
	cfg := defaultConfig()
	if cfg.MediaFlow.Password != "" || cfg.MediaFlow.PublicIP != "" {
		t.Fatal("MediaFlow must default disabled")
	}

	t.Setenv("HARRBOR_MEDIAFLOW_PASSWORD", "environment-value")
	t.Setenv("HARRBOR_MEDIAFLOW_PUBLIC_IP", "192.0.2.1")
	applyEnv(&cfg)
	if cfg.MediaFlow.Password != "environment-value" || cfg.MediaFlow.PublicIP != "192.0.2.1" {
		t.Fatal("MediaFlow environment configuration not applied")
	}

	secrets, err := parseSecretLines([]byte("HARRBOR_MEDIAFLOW_PASSWORD=sealed-value\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.applySecrets(secrets)
	if cfg.MediaFlow.Password != "sealed-value" {
		t.Fatal("sealed MediaFlow password did not override environment")
	}
}
