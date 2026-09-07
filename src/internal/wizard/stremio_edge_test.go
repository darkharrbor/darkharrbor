package wizard

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/config"
)

func TestConfigureStremioEdgePersistsExplicitMode(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.StreamSecret = strings.Repeat("s", 32)
	cfg.Stremio.EdgeMode = config.StremioEdgeInternal
	cfg.Stremio.ClientAddress = ":8382"
	cfg.Stremio.InstallToken = strings.Repeat("i", 32)
	p := configurePrompter{
		reader: bufio.NewReaderSize(strings.NewReader("tailscale\nhttps://dh.example.test\n"), 8*1024+1),
		out:    &bytes.Buffer{},
	}
	values := map[string]string{}
	mode, baseURL, err := configureStremioEdge(&p, cfg, values)
	if err != nil {
		t.Fatal(err)
	}
	if mode != config.StremioEdgeTailscale || baseURL != "https://dh.example.test" || values["HARRBOR_STREMIO_EDGE_MODE"] != mode || values["HARRBOR_STREMIO_CLIENT_BASE_URL"] != baseURL {
		t.Fatal("Stremio edge settings did not round-trip")
	}
}

func TestConfigureMediaFlowOriginDefaultsToStremioOrigin(t *testing.T) {
	cfg := &config.Config{}
	values := map[string]string{}
	p := configurePrompter{
		reader: bufio.NewReaderSize(strings.NewReader("\n"), 8*1024+1),
		out:    &bytes.Buffer{},
	}
	if err := configureMediaFlowOrigin(&p, cfg, values, "https://dh.example.test"); err != nil {
		t.Fatal(err)
	}
	if got := values["HARRBOR_MEDIAFLOW_BASE_URL"]; got != "https://dh.example.test" {
		t.Fatalf("MediaFlow origin = %q, want reviewed Stremio origin", got)
	}
}

func TestRunStremioEdgeSetupWritesTuningAndCanRollback(t *testing.T) {
	cfgDir := t.TempDir()
	secret := strings.Repeat("s", 32)
	installToken := strings.Repeat("i", 32)
	oldReader := stdinReader
	defer func() { stdinReader = oldReader }()

	stdinReader = bufio.NewReader(strings.NewReader("tailscale\nhttps://dh.example.test\n"))
	if err := runStremioEdgeSetup(&bytes.Buffer{}, cfgDir, secret, installToken, ""); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfgDir, filepath.Base(config.DefaultTuningPath))
	values, err := config.ReadTuning(path)
	if err != nil || values["HARRBOR_STREMIO_EDGE_MODE"] != config.StremioEdgeTailscale || values["HARRBOR_STREMIO_CLIENT_BASE_URL"] == "" {
		t.Fatalf("enabled values were not persisted: err=%v", err)
	}

	stdinReader = bufio.NewReader(strings.NewReader("internal\n"))
	if err := runStremioEdgeSetup(&bytes.Buffer{}, cfgDir, secret, installToken, ""); err != nil {
		t.Fatal(err)
	}
	values, err = config.ReadTuning(path)
	if err != nil || values["HARRBOR_STREMIO_EDGE_MODE"] != config.StremioEdgeInternal || values["HARRBOR_STREMIO_CLIENT_BASE_URL"] != "" {
		t.Fatalf("rollback values were not persisted: err=%v", err)
	}
}

func TestRunStremioEdgeSetupUsesPrivateHandoff(t *testing.T) {
	cfgDir := t.TempDir()
	handoffDir := filepath.Join(t.TempDir(), "handoff")
	if err := writeSetupHandoff(handoffDir, "recovery", ""); err != nil {
		t.Fatal(err)
	}
	oldReader := stdinReader
	defer func() { stdinReader = oldReader }()
	stdinReader = bufio.NewReader(strings.NewReader("tailscale\nhttps://dh.example.test\n"))
	installToken := strings.Repeat("i", 32)
	var out bytes.Buffer
	if err := runStremioEdgeSetup(&out, cfgDir, strings.Repeat("s", 32), installToken, handoffDir); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), installToken) {
		t.Fatal("install token was printed despite private handoff")
	}
	data, err := os.ReadFile(filepath.Join(handoffDir, "stremio-install.url"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), installToken) {
		t.Fatal("private install URL handoff omitted token")
	}
}
