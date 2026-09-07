package wizard

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/securefile"
)

const maxAnswersFileBytes = 1 << 20

type onboardingPlan struct {
	ArrContext       string   `json:"arr_context"`
	ArrConnection    string   `json:"arr_connection"`
	SelectedArrs     []string `json:"selected_arrs,omitempty"`
	ProwlarrName     string   `json:"prowlarr_name,omitempty"`
	Topology         string   `json:"topology"`
	AcquisitionLanes []string `json:"acquisition_lanes"`
	DebridProviders  []string `json:"debrid_providers,omitempty"`
	UsenetProviders  []string `json:"usenet_providers,omitempty"`
	Performance      string   `json:"performance"`
	BaseArrTopology  bool     `json:"base_arr_topology"`
	WrapperRepair    bool     `json:"wrapper_repair"`
	ReactiveCommit   bool     `json:"reactive_commit"`
	Aggregator       bool     `json:"aggregator"`
	StremioAddon     bool     `json:"stremio_addon"`
	Prowlarr         bool     `json:"prowlarr"`
	Jellyfin         bool     `json:"jellyfin"`
	JellyfinName     string   `json:"jellyfin_name,omitempty"`
	Promotion        bool     `json:"promotion"`
	Backups          bool     `json:"backups"`
	Demo             bool     `json:"demo"`
}

type setupInventory struct {
	ProxyOK   bool
	Arrs      []string
	Prowlarrs []string
	Jellyfins []string
}

type answersDocument struct {
	Responses []string `json:"responses"`
}

var answerReplayActive bool

func prepareAnswers(path string) (func(), error) {
	if strings.TrimSpace(path) == "" {
		return func() {}, nil
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve answers file: %w", err)
	}
	raw, err := securefile.Read(path, maxAnswersFileBytes)
	if err != nil {
		return nil, fmt.Errorf("answers file is unavailable or unsafe: %w", err)
	}
	var doc answersDocument
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse answers file: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("parse answers file: trailing JSON content is not allowed")
	}
	if len(doc.Responses) == 0 {
		return nil, fmt.Errorf("answers file contains no responses")
	}
	oldReader := stdinReader
	oldReplay := answerReplayActive
	stdinReader = newAnswersReader(doc.Responses)
	answerReplayActive = true
	return func() {
		stdinReader = oldReader
		answerReplayActive = oldReplay
	}, nil
}

func newAnswersReader(responses []string) *bufio.Reader {
	var b strings.Builder
	for _, response := range responses {
		b.WriteString(response)
		b.WriteByte('\n')
	}
	return bufio.NewReader(strings.NewReader(b.String()))
}

func collectOnboardingChoices(out io.Writer, inventory setupInventory) (onboardingPlan, error) {
	plan := onboardingPlan{ArrContext: "existing"}
	fmt.Fprintln(out, "── Plan  What DarkHarrbor Should Do ────────────────────")
	fmt.Fprintln(out, "  DarkHarrbor connects to the Sonarr and Radarr instances you already run.")

	if inventory.ProxyOK && len(inventory.Arrs) > 0 {
		if promptYesNo(out, "  Use the discovered Arr instances? [Y/n]: ", true) {
			plan.ArrConnection = "discover"
		} else {
			plan.ArrConnection = "manual"
		}
	} else {
		plan.ArrConnection = "manual"
		fmt.Fprintln(out, "  ℹ Automatic Arr discovery is unavailable; authenticated manual entry will be used.")
	}
	if plan.ArrConnection == "discover" {
		fmt.Fprintln(out, "  ℹ Discovery uses only the scoped connector; DarkHarrbor never receives the Docker socket.")
		plan.SelectedArrs = promptArrSelection(out, inventory.Arrs)
	} else {
		fmt.Fprintln(out, "  ℹ Manual entry asks for each Arr base URL and API key, verifies it with authentication, then seals the key.")
	}

	lanes, err := promptLanes(out, false)
	if err != nil {
		return onboardingPlan{}, err
	}
	plan.AcquisitionLanes = lanes
	plan.DebridProviders = promptDebridProviderSelection(out, plan)
	if plan.hasLane("nntp") {
		for {
			plan.UsenetProviders = promptUsenetProviderSelection(out, plan.hasProvider("torbox"))
			if len(plan.UsenetProviders) > 0 || plan.hasProvider("torbox") {
				break
			}
			fmt.Fprintln(out, "  ⚠ Usenet was selected; choose at least one direct NNTP provider or select the TorBox account first.")
		}
	}
	plan.Performance = promptPerformanceProfile(out, detectPerformanceRecommendation(demoDataRoot()))

	plan.BaseArrTopology = promptYesNo(out, "  Use DarkHarrbor for normal Sonarr/Radarr searches and grabs? [Y/n]: ", true)
	if !plan.BaseArrTopology {
		fmt.Fprintln(out, "    Skipped consequence: Arr searches will not grab through DarkHarrbor.")
	}
	plan.WrapperRepair = plan.BaseArrTopology && plan.ArrConnection == "discover" && inventory.ProxyOK
	if plan.BaseArrTopology && !plan.WrapperRepair {
		fmt.Fprintln(out, "    Wrapper repair: manual installation is required because scoped discovery was not selected.")
	}
	if plan.ArrConnection == "discover" && len(inventory.Prowlarrs) > 0 {
		plan.Prowlarr = promptYesNo(out, "  Use Prowlarr indexers through DarkHarrbor when Prowlarr is discovered? [Y/n]: ", true)
		if plan.Prowlarr {
			plan.ProwlarrName = promptProwlarrSelection(out, inventory.Prowlarrs)
		}
	} else if plan.ArrConnection == "manual" {
		plan.Prowlarr = promptYesNo(out, "  Add an existing Prowlarr instance manually? [y/N]: ", false)
	}
	if len(inventory.Jellyfins) > 0 {
		plan.Jellyfin = promptYesNo(out, "  Configure the discovered Jellyfin instance? [y/N]: ", false)
		if plan.Jellyfin {
			plan.JellyfinName = promptJellyfinSelection(out, inventory.Jellyfins)
		}
	}
	plan.Backups = promptYesNo(out, "  Keep recommended scheduled local backups? [Y/n]: ", true)

	plan.Aggregator = promptYesNo(out, "  Connect an existing self-hosted AIOStreams setup for optional streaming? [y/N]: ", false)
	if plan.Aggregator && !plan.hasLane("http") {
		plan.AcquisitionLanes = append(plan.AcquisitionLanes, "http")
		fmt.Fprintln(out, "  ✓ HTTP/stream support added because AIOStreams playback uses it.")
	}
	if plan.Aggregator {
		plan.ReactiveCommit = promptYesNo(out, "  Enable play-to-commit and its required DarkHarrbor addon? [y/N]: ", false)
	} else {
		fmt.Fprintln(out, "  Play-to-commit: skipped because it requires an operator-controlled AIOStreams connection.")
	}
	if plan.ReactiveCommit {
		plan.StremioAddon = true
		fmt.Fprintln(out, "  ✓ DarkHarrbor addon enabled for safe movie/episode identity correlation.")
	} else {
		plan.StremioAddon = promptYesNo(out, "  Publish the optional DarkHarrbor Stremio addon? [y/N]: ", false)
	}
	if plan.ReactiveCommit {
		plan.Promotion = promptYesNo(out, "  Also allow promotion from self-healing pointers to materialized files? [y/N]: ", false)
	}
	plan.Topology = deriveTopology(plan)
	if plan.hasLane("http") {
		plan.Demo = promptYesNo(out, "  Run the optional public-domain acceptance demo after setup? [y/N]: ", false)
	} else {
		fmt.Fprintln(out, "  Demo: skipped because it requires an ID-capable HTTP backend.")
	}

	return plan, nil
}

func promptProwlarrSelection(out io.Writer, available []string) string {
	if len(available) == 1 {
		fmt.Fprintf(out, "  ✓ Selected Prowlarr instance: %s\n", available[0])
		return available[0]
	}
	fmt.Fprintln(out, "  Select the one Prowlarr instance DarkHarrbor may use:")
	for i, name := range available {
		fmt.Fprintf(out, "    %d. %s\n", i+1, name)
	}
	for {
		fmt.Fprint(out, "  Prowlarr instance (name or number): ")
		value := strings.TrimSpace(readLine(out))
		for i, name := range available {
			if strings.EqualFold(value, name) || value == fmt.Sprint(i+1) {
				return name
			}
		}
		fmt.Fprintln(out, "  ⚠ Select exactly one listed Prowlarr instance by name or number.")
	}
}

func promptJellyfinSelection(out io.Writer, available []string) string {
	if len(available) == 1 {
		fmt.Fprintf(out, "  ✓ Selected Jellyfin instance: %s\n", available[0])
		return available[0]
	}
	fmt.Fprintln(out, "  Select the one Jellyfin instance DarkHarrbor may configure:")
	for i, name := range available {
		fmt.Fprintf(out, "    %d. %s\n", i+1, name)
	}
	for {
		fmt.Fprint(out, "  Jellyfin instance (name or number): ")
		value := strings.TrimSpace(readLine(out))
		for i, name := range available {
			if strings.EqualFold(value, name) || value == fmt.Sprint(i+1) {
				return name
			}
		}
		fmt.Fprintln(out, "  ⚠ Select exactly one listed Jellyfin instance by name or number.")
	}
}

func promptArrSelection(out io.Writer, available []string) []string {
	if len(available) == 1 {
		fmt.Fprintf(out, "  ✓ Selected Arr instance: %s\n", available[0])
		return append([]string(nil), available...)
	}
	fmt.Fprintln(out, "  Select the Sonarr/Radarr instances DarkHarrbor may configure:")
	for i, name := range available {
		fmt.Fprintf(out, "    %d. %s\n", i+1, name)
	}
	for {
		fmt.Fprint(out, "  Arr instances (names or numbers, comma-separated) [all]: ")
		raw := strings.TrimSpace(readLine(out))
		if raw == "" || strings.EqualFold(raw, "all") {
			return append([]string(nil), available...)
		}
		selected := make([]string, 0, len(available))
		seen := map[string]bool{}
		valid := true
		for _, part := range strings.Split(raw, ",") {
			value := strings.TrimSpace(part)
			name := ""
			for i, candidate := range available {
				if strings.EqualFold(value, candidate) || value == fmt.Sprint(i+1) {
					name = candidate
					break
				}
			}
			if name == "" {
				valid = false
				break
			}
			if !seen[name] {
				selected = append(selected, name)
				seen[name] = true
			}
		}
		if valid && len(selected) > 0 {
			return selected
		}
		fmt.Fprintln(out, "  ⚠ Select at least one listed instance by name or number, or choose all.")
	}
}

func promptPerformanceProfile(out io.Writer, recommendation performanceRecommendation) string {
	fmt.Fprintln(out, "  Choose a performance profile:")
	fmt.Fprintln(out, "    recommended     — balanced defaults for most installations")
	fmt.Fprintln(out, "    low-memory      — smaller buffers for constrained systems")
	fmt.Fprintln(out, "    high-throughput — larger buffers and disk cache for fast storage")
	fmt.Fprintln(out, "  Advanced tuning remains available later with `darkharrbor configure`.")
	if recommendation.Reason != "" {
		fmt.Fprintf(out, "  ℹ %s\n", recommendation.Reason)
	}
	for {
		fmt.Fprintf(out, "  Performance profile [%s]: ", recommendation.Default)
		switch value := strings.ToLower(strings.TrimSpace(readLine(out))); value {
		case "":
			return recommendation.Default
		case "1", "recommended", "balanced":
			return "recommended"
		case "2", "low-memory", "low memory":
			return "low-memory"
		case "3", "high-throughput", "high throughput":
			if !recommendation.HighThroughputAvailable {
				fmt.Fprintln(out, "  ⚠ High throughput needs at least 10 GiB free on the application-owned /data filesystem.")
				continue
			}
			return "high-throughput"
		default:
			fmt.Fprintln(out, "  ⚠ Choose recommended, low-memory, or high-throughput.")
		}
	}
}

func promptChoice(out io.Writer, prompt, fallback string, allowed ...string) string {
	for {
		fmt.Fprint(out, prompt)
		value := strings.ToLower(strings.TrimSpace(readLine(out)))
		if value == "" {
			return fallback
		}
		for _, candidate := range allowed {
			if value == candidate {
				return value
			}
		}
		fmt.Fprintf(out, "  ⚠ Choose one of: %s.\n", strings.Join(allowed, ", "))
	}
}

func promptLanes(out io.Writer, _ bool) ([]string, error) {
	fmt.Fprintln(out, "  What should DarkHarrbor download or stream?")
	fmt.Fprintln(out, "    torrent — torrent/debrid providers")
	fmt.Fprintln(out, "    usenet  — NZB services and direct NNTP providers")
	fmt.Fprintln(out, "    http    — HTTP streams, OMSS, Internet Archive, and compatible backends")
	for {
		fmt.Fprint(out, "  Select one or more (comma-separated; no default): ")
		raw := strings.TrimSpace(readLine(out))
		if raw == "" {
			fmt.Fprintln(out, "  ⚠ Select at least one source; DarkHarrbor will not assume a provider or lane.")
			continue
		}
		seen := map[string]bool{}
		valid := true
		for _, value := range strings.Split(raw, ",") {
			lane := strings.ToLower(strings.TrimSpace(value))
			if lane == "usenet" {
				lane = "nntp"
			}
			switch lane {
			case "torrent", "nntp", "http":
				seen[lane] = true
			default:
				fmt.Fprintf(out, "  ⚠ Unknown source %q; choose torrent, usenet, and/or http.\n", lane)
				valid = false
			}
		}
		if !valid {
			continue
		}
		lanes := make([]string, 0, len(seen))
		for _, lane := range []string{"torrent", "nntp", "http"} {
			if seen[lane] {
				lanes = append(lanes, lane)
			}
		}
		return lanes, nil
	}
}

func promptDebridProviderSelection(out io.Writer, plan onboardingPlan) []string {
	var available []string
	if plan.hasLane("torrent") || plan.hasLane("nntp") {
		available = append(available, "torbox")
	}
	if plan.hasLane("torrent") {
		available = append(available, "realdebrid", "alldebrid", "premiumize")
	}
	if len(available) == 0 {
		fmt.Fprintln(out, "  Debrid providers: none needed for the selected sources.")
		return nil
	}
	fmt.Fprintln(out, "  Select only the debrid providers you already use, in submission fallback order:")
	fmt.Fprintln(out, "  The first provider that accepts a torrent owns that item afterward.")
	for i, name := range available {
		fmt.Fprintf(out, "    %d. %s\n", i+1, providerDisplayName(name))
	}
	for {
		fmt.Fprint(out, "  Debrid providers (names or numbers, comma-separated; 'none' for none) [none]: ")
		raw := strings.TrimSpace(readLine(out))
		if raw == "" || strings.EqualFold(raw, "none") {
			if plan.hasLane("torrent") {
				fmt.Fprintln(out, "  ⚠ Torrent was selected; choose at least one listed provider. Rerun setup without torrent if it is not wanted.")
				continue
			}
			return nil
		}
		seen := map[string]bool{}
		selected := make([]string, 0, len(available))
		valid := true
		for _, part := range strings.Split(raw, ",") {
			value := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(part)), "-", "")
			name := ""
			for i, candidate := range available {
				if value == candidate || value == fmt.Sprint(i+1) {
					name = candidate
					break
				}
			}
			if name == "" {
				valid = false
				break
			}
			if !seen[name] {
				selected = append(selected, name)
				seen[name] = true
			}
		}
		if valid && len(selected) > 0 {
			return selected
		}
		fmt.Fprintln(out, "  ⚠ Choose only listed providers by name or number, or choose none.")
	}
}

func providerDisplayName(name string) string {
	switch name {
	case "realdebrid":
		return "Real-Debrid"
	case "alldebrid":
		return "AllDebrid"
	case "premiumize":
		return "Premiumize"
	default:
		return "TorBox"
	}
}

func deriveTopology(plan onboardingPlan) string {
	if plan.ReactiveCommit {
		return "t3"
	}
	if plan.Aggregator {
		return "t2"
	}
	return "t1"
}

func printOnboardingPlan(out io.Writer, plan onboardingPlan) {
	fmt.Fprintln(out, "\n── Review  No persistent changes have been made ───────")
	fmt.Fprintf(out, "  Arr connection:    %s\n", plan.ArrConnection)
	if len(plan.SelectedArrs) > 0 {
		fmt.Fprintf(out, "  Arr instances:     %s\n", strings.Join(plan.SelectedArrs, ", "))
	}
	if plan.ProwlarrName != "" {
		fmt.Fprintf(out, "  Prowlarr instance: %s\n", plan.ProwlarrName)
	}
	if plan.JellyfinName != "" {
		fmt.Fprintf(out, "  Jellyfin instance: %s\n", plan.JellyfinName)
	}
	fmt.Fprintf(out, "  Content sources:   %s\n", strings.Join(plan.AcquisitionLanes, ", "))
	if len(plan.DebridProviders) == 0 {
		fmt.Fprintln(out, "  Debrid providers:  none")
	} else {
		display := make([]string, 0, len(plan.DebridProviders))
		for _, provider := range plan.DebridProviders {
			display = append(display, providerDisplayName(provider))
		}
		fmt.Fprintf(out, "  Debrid providers:  %s\n", strings.Join(display, " → "))
	}
	if plan.hasLane("nntp") {
		if len(plan.UsenetProviders) == 0 {
			fmt.Fprintln(out, "  NNTP providers:    none")
		} else {
			fmt.Fprintf(out, "  NNTP providers:    %s\n", strings.Join(plan.UsenetProviders, " → "))
		}
	}
	fmt.Fprintf(out, "  Performance:       %s\n", plan.Performance)
	features := map[string]bool{
		"aggregator": plan.Aggregator, "arr-search-grab": plan.BaseArrTopology,
		"backups": plan.Backups, "jellyfin": plan.Jellyfin, "promotion": plan.Promotion,
		"prowlarr": plan.Prowlarr, "reactive-commit": plan.ReactiveCommit,
		"stremio-addon":  plan.StremioAddon,
		"wrapper-repair": plan.WrapperRepair,
		"demo":           plan.Demo,
	}
	names := make([]string, 0, len(features))
	for name := range features {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		state := "skipped"
		if features[name] {
			state = "enabled"
		}
		fmt.Fprintf(out, "  %-18s %s\n", name+":", state)
	}
	if plan.Aggregator {
		fmt.Fprintln(out, "  externally pending: preserve an existing working AIOStreams/StremThru stack (or deploy the optional locked example), save the DarkHarrbor integration in AIOStreams, then run configure --existing-aiostreams")
	}
}

func (p onboardingPlan) hasLane(name string) bool {
	for _, lane := range p.AcquisitionLanes {
		if lane == name {
			return true
		}
	}
	return false
}

func (p onboardingPlan) hasProvider(name string) bool {
	for _, provider := range p.DebridProviders {
		if provider == name {
			return true
		}
	}
	return false
}

func printSetupOutcome(out io.Writer, plan onboardingPlan) {
	enabled := []string{"arr=" + plan.ArrConnection, "sources=" + strings.Join(plan.AcquisitionLanes, "+"), "performance=" + plan.Performance}
	skipped := []string{}
	features := []struct {
		name    string
		enabled bool
	}{
		{"arr-search-grab", plan.BaseArrTopology}, {"aggregator", plan.Aggregator},
		{"reactive-commit", plan.ReactiveCommit}, {"stremio-addon", plan.StremioAddon},
		{"prowlarr", plan.Prowlarr}, {"jellyfin", plan.Jellyfin},
		{"promotion", plan.Promotion}, {"backups", plan.Backups},
		{"wrapper-repair", plan.WrapperRepair},
		{"demo", plan.Demo},
	}
	for _, feature := range features {
		if feature.enabled {
			enabled = append(enabled, feature.name)
		} else {
			skipped = append(skipped, feature.name)
		}
	}
	pending := []string{}
	if plan.Aggregator {
		pending = append(pending, "save the DarkHarrbor integration in the existing AIOStreams user, then run `darkharrbor configure --existing-aiostreams`")
	}
	if plan.hasLane("http") && !plan.Aggregator {
		pending = append(pending, "run `darkharrbor configure` for selected HTTP backends")
	}
	if plan.Promotion {
		pending = append(pending, "run `darkharrbor configure` for promotion storage policy")
	}
	if plan.BaseArrTopology && !plan.WrapperRepair {
		pending = append(pending, "install and maintain the ffprobe metadata relay in manually entered Arrs")
	}

	fmt.Fprintln(out, "\n✅ Setup applied")
	fmt.Fprintf(out, "  enabled: %s\n", strings.Join(enabled, ", "))
	fmt.Fprintf(out, "  skipped: %s\n", strings.Join(skipped, ", "))
	fmt.Fprintln(out, "  degraded: none reported by completed stages")
	fmt.Fprintln(out, "  failed: none")
	if len(pending) == 0 {
		fmt.Fprintln(out, "  externally pending: none")
	} else {
		for _, item := range pending {
			fmt.Fprintf(out, "  externally pending: %s\n", item)
		}
	}
	if plan.hasLane("http") {
		fmt.Fprintln(out, "  validate after HTTP backend configuration and restart: darkharrbor doctor")
	} else {
		fmt.Fprintln(out, "  validate now: darkharrbor doctor")
	}
	fmt.Fprintln(out, "  validate Arr wiring: darkharrbor reconcile-arr")
}
