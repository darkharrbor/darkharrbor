package wizard

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/securefile"
)

func configureStremioEdge(p *configurePrompter, cfg *config.Config, values map[string]string) (string, string, error) {
	fmt.Fprintln(p.out, "\nStremio client edge modes: internal, lan, tailscale, custom, funnel")
	fmt.Fprintln(p.out, "Tailscale is the private remote-access default; Funnel is an explicit public opt-in.")
	currentMode := cfg.Stremio.EdgeMode
	if currentMode == "" {
		currentMode = config.StremioEdgeInternal
	}
	mode, err := p.text("Stremio client edge mode", currentMode)
	if err != nil {
		return "", "", err
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if !config.ValidStremioEdgeMode(mode) {
		return "", "", fmt.Errorf("stremio client edge mode must be one of internal|lan|tailscale|custom|funnel")
	}
	values["HARRBOR_STREMIO_EDGE_MODE"] = mode
	if mode == config.StremioEdgeInternal {
		delete(values, "HARRBOR_STREMIO_CLIENT_BASE_URL")
		return mode, "", nil
	}
	baseURL, err := p.text("Stremio client HTTPS origin", cfg.Stremio.ClientBaseURL)
	if err != nil {
		return "", "", err
	}
	check := cfg.Stremio
	check.EdgeMode = mode
	check.ClientBaseURL = baseURL
	if check.ClientAddress == "" {
		check.ClientAddress = ":8382"
	}
	if err := check.Validate(cfg.Server.StreamSecret); err != nil {
		return "", "", err
	}
	values["HARRBOR_STREMIO_CLIENT_BASE_URL"] = baseURL
	return mode, baseURL, nil
}

func configureMediaFlowOrigin(p *configurePrompter, cfg *config.Config, values map[string]string, stremioBaseURL string) error {
	current := strings.TrimSpace(cfg.MediaFlow.BaseURL)
	if current == "" {
		current = strings.TrimSpace(stremioBaseURL)
	}
	origin, err := p.text("MediaFlow client origin (restricted listener)", current)
	if err != nil {
		return err
	}
	origin = strings.TrimSpace(origin)
	if origin == "" {
		delete(values, "HARRBOR_MEDIAFLOW_BASE_URL")
		return nil
	}
	values["HARRBOR_MEDIAFLOW_BASE_URL"] = origin
	return nil
}

func runStremioEdgeSetup(out io.Writer, cfgDir, streamSecret, installToken, handoffDir string) error {
	path := filepath.Join(cfgDir, filepath.Base(config.DefaultTuningPath))
	values, err := config.ReadTuning(path)
	if err != nil {
		return fmt.Errorf("read runtime configuration: %w", err)
	}
	currentMode := values["HARRBOR_STREMIO_EDGE_MODE"]
	if currentMode == "" {
		currentMode = config.StremioEdgeInternal
	}
	fmt.Fprintln(out, "\n── S7c Stremio Client Edge ────────────────────────────")
	fmt.Fprintln(out, "  Modes: internal (off), lan, tailscale (private), custom HTTPS, funnel (public opt-in)")
	mode := strings.ToLower(promptWithDefault(out, fmt.Sprintf("  Client edge mode [%s]: ", currentMode), currentMode))
	if !config.ValidStremioEdgeMode(mode) {
		return fmt.Errorf("client edge mode must be one of internal|lan|tailscale|custom|funnel")
	}
	values["HARRBOR_STREMIO_EDGE_MODE"] = mode
	baseURL := ""
	if mode == config.StremioEdgeInternal {
		delete(values, "HARRBOR_STREMIO_CLIENT_BASE_URL")
	} else {
		baseURL = promptWithDefault(out, "  Client HTTPS origin: ", values["HARRBOR_STREMIO_CLIENT_BASE_URL"])
		check := config.StremioConfig{EdgeMode: mode, ClientAddress: ":8382", ClientBaseURL: baseURL, InstallToken: installToken}
		if err := check.Validate(streamSecret); err != nil {
			return err
		}
		values["HARRBOR_STREMIO_CLIENT_BASE_URL"] = baseURL
	}
	if err := config.WriteTuning(path, values); err != nil {
		return fmt.Errorf("write runtime configuration: %w", err)
	}
	revealURL := true
	if strings.TrimSpace(handoffDir) != "" && mode != config.StremioEdgeInternal {
		installURL := strings.TrimRight(baseURL, "/") + "/stremio/" + installToken + "/manifest.json\n"
		if err := securefile.AtomicWrite(filepath.Join(filepath.Clean(handoffDir), "stremio-install.url"), []byte(installURL)); err != nil {
			return fmt.Errorf("write private Stremio handoff: %w", err)
		}
		revealURL = false
		fmt.Fprintln(out, "  ✓ Stremio install URL added to the private operator handoff; it was not printed.")
	}
	printStremioEdgeInstructions(out, mode, baseURL, installToken, revealURL)
	return nil
}

func printStremioEdgeInstructions(out io.Writer, mode, baseURL, installToken string, revealURL bool) {
	if mode == config.StremioEdgeInternal {
		fmt.Fprintln(out, "  ✓ Client listener remains disabled; rerun setup/configure to enable it.")
		return
	}
	fmt.Fprintln(out, "  ✓ Origin, TLS policy, and install-token shape validated.")
	switch mode {
	case config.StremioEdgeTailscale:
		fmt.Fprintln(out, "  Host action: tailscale serve --bg --https=443 http://127.0.0.1:8382")
	case config.StremioEdgeFunnel:
		fmt.Fprintln(out, "  WARNING: Funnel is public. The restricted client listener is the only permitted target.")
		fmt.Fprintln(out, "  Host action: tailscale funnel --bg --https=443 http://127.0.0.1:8382")
	case config.StremioEdgeCustom:
		fmt.Fprintln(out, "  Proxy the configured HTTPS origin to host port 8382 only; do not proxy port 8381.")
	case config.StremioEdgeLAN:
		fmt.Fprintln(out, "  LAN clients still require HTTPS unless the client runs on localhost.")
	}
	if revealURL {
		fmt.Fprintf(out, "  Stremio install URL: %s/stremio/%s/manifest.json\n", strings.TrimRight(baseURL, "/"), installToken)
	}
	fmt.Fprintln(out, "  Rollback: select mode 'internal' in `darkharrbor configure`.")
}
