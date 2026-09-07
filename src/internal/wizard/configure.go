package wizard

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/secretinput"
)

// ConfigureOptions controls the non-secret runtime tuning wizard.
type ConfigureOptions struct {
	Path        string
	SecretsPath string
	Stdin       io.Reader
	Stdout      io.Writer
	Current     *config.Config
	DialBackend backendDialer
	CheckArr    arrReachability
	ArrStore    arrDestinationStore
}

// RunConfigure interactively writes the strict non-secret tuning file. It
// returns false when the user declines the final save.
func RunConfigure(opts ConfigureOptions) (bool, error) {
	if opts.Current == nil {
		return false, fmt.Errorf("current configuration is required")
	}
	if opts.Path == "" {
		opts.Path = config.DefaultTuningPath
	}
	if opts.Stdin == nil {
		opts.Stdin = os.Stdin
	}
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	p := configurePrompter{reader: bufio.NewReaderSize(opts.Stdin, 8*1024+1), out: opts.Stdout}
	cfg := opts.Current
	values, err := config.ReadTuning(opts.Path)
	if err != nil {
		return false, err
	}
	originalValues := cloneStringMap(values)
	secretUpdates := map[string]string{}

	fmt.Fprintln(p.out, "\nDarkHarrbor runtime configuration")
	fmt.Fprintln(p.out, "Credentials remain sealed. Sensitive HTTP backend URLs are entered without echo and updated in the existing sealed store.")

	nntpMode := effectiveMode(cfg.Cache.NNTPMode, cfg.Cache.Mode)
	streamMode := effectiveMode(cfg.Cache.StreamMode, cfg.Cache.Mode)
	if nntpMode, err = p.text("NNTP cache mode (none|readahead|disk|full)", nntpMode); err != nil {
		return false, err
	}
	if streamMode, err = p.text("Torrent cache mode (none|readahead|disk|full)", streamMode); err != nil {
		return false, err
	}
	diskSize, _, err := p.integer("Shared disk cache size (MB)", int(cfg.Cache.DiskCacheSizeMB), 1)
	if err != nil {
		return false, err
	}
	evictMode, err := p.text("Disk eviction mode (ttl|manual|never)", cfg.Cache.FullEvictMode)
	if err != nil {
		return false, err
	}
	ttl := cfg.Cache.FullEvictTTLMin
	if ttl <= 0 {
		ttl = cfg.Cache.DiskCacheTTLMin
	}
	ttl, _, err = p.integer("Disk cache TTL (minutes)", ttl, 1)
	if err != nil {
		return false, err
	}
	window := cfg.Cache.ReadaheadMaxSegments
	if legacyWindow := lookupConfigureInt(cfg, "HARRBOR_PROVIDER_NEWSHOSTING_READAHEAD_MAX_SEGMENTS", 0); legacyWindow > 0 {
		window = legacyWindow
	}
	window, _, err = p.integer("NNTP readahead window (segments)", window, 1)
	if err != nil {
		return false, err
	}
	streamChunkSize, _, err := p.integer("Torrent cache chunk size (MB)", cfg.Cache.StreamChunkSizeMB, 1)
	if err != nil {
		return false, err
	}
	streamMinBuffer, _, err := p.integer("Torrent minimum buffer (chunks)", cfg.Cache.StreamMinBufferSegments, 0)
	if err != nil {
		return false, err
	}
	streamWorkers, _, err := p.integer("Torrent readahead workers", cfg.Cache.StreamReadaheadWorkers, 1)
	if err != nil {
		return false, err
	}
	preference, err := p.text("Acquisition preference", strings.Join(cfg.Routing.Preference, ","))
	if err != nil {
		return false, err
	}
	budget, _, err := p.integer("Uncached governor budget", cfg.Governor.UncachedBudget, 1)
	if err != nil {
		return false, err
	}
	stallTimeout, _, err := p.integer("Stall timeout (min) — fail uncached torrents with no seeders after this long", cfg.Governor.StallTimeoutMin, 1)
	if err != nil {
		return false, err
	}
	deadSentinelGrace, _, err := p.integer("Dead-sentinel grace (min) — TorBox's own checking+zero-seed+100-day-ETA signal must persist this long before failing (shorter than stall timeout; not instantly trusted)", cfg.Governor.DeadSentinelGraceMin, 1)
	if err != nil {
		return false, err
	}
	backupEnabled, err := p.yesNo("Scheduled backups enabled", cfg.Backup.Enabled)
	if err != nil {
		return false, err
	}
	backupTarget := cfg.Backup.Target
	backupInterval := cfg.Backup.IntervalHours
	backupKeep := cfg.Backup.Keep
	if backupEnabled {
		backupTarget, err = p.text("Backup target directory", backupTarget)
		if err != nil {
			return false, err
		}
		backupInterval, _, err = p.integer("Backup cadence (hours)", backupInterval, 1)
		if err != nil {
			return false, err
		}
		backupKeep, _, err = p.integer("Backup snapshots to keep", backupKeep, 1)
		if err != nil {
			return false, err
		}
	}
	logLevel, _ := cfg.Lookup("HARRBOR_LOG_LEVEL")
	if logLevel == "" {
		logLevel = "INFO"
	}
	logLevel, err = p.text("Log level (DEBUG|INFO|WARN|ERROR)", strings.ToUpper(logLevel))
	if err != nil {
		return false, err
	}

	values["HARRBOR_BACKUP_ENABLED"] = strconv.FormatBool(backupEnabled)
	if backupEnabled {
		values["HARRBOR_BACKUP_TARGET"] = backupTarget
		values["HARRBOR_BACKUP_INTERVAL_HOURS"] = strconv.Itoa(backupInterval)
		values["HARRBOR_BACKUP_KEEP"] = strconv.Itoa(backupKeep)
	} else {
		delete(values, "HARRBOR_BACKUP_TARGET")
		delete(values, "HARRBOR_BACKUP_INTERVAL_HOURS")
		delete(values, "HARRBOR_BACKUP_KEEP")
	}
	values["HARRBOR_NNTP_CACHE_MODE"] = strings.ToLower(nntpMode)
	values["HARRBOR_STREAM_CACHE_MODE"] = strings.ToLower(streamMode)
	values["HARRBOR_DISK_CACHE_SIZE_MB"] = strconv.Itoa(diskSize)
	values["HARRBOR_DISK_CACHE_TTL_MIN"] = strconv.Itoa(ttl)
	values["HARRBOR_FULL_EVICT_MODE"] = strings.ToLower(evictMode)
	values["HARRBOR_FULL_EVICT_TTL_MIN"] = strconv.Itoa(ttl)
	values["HARRBOR_READAHEAD_MAX_SEGMENTS"] = strconv.Itoa(window)
	values["HARRBOR_STREAM_CHUNK_SIZE_MB"] = strconv.Itoa(streamChunkSize)
	values["HARRBOR_STREAM_MIN_BUFFER_SEGMENTS"] = strconv.Itoa(streamMinBuffer)
	values["HARRBOR_STREAM_READAHEAD_WORKERS"] = strconv.Itoa(streamWorkers)
	values["HARRBOR_PREFERENCE"] = strings.ToLower(preference)
	values["HARRBOR_GOVERNOR_UNCACHED_BUDGET"] = strconv.Itoa(budget)
	values["HARRBOR_GOVERNOR_STALL_TIMEOUT_MIN"] = strconv.Itoa(stallTimeout)
	values["HARRBOR_GOVERNOR_DEAD_SENTINEL_GRACE_MIN"] = strconv.Itoa(deadSentinelGrace)
	values["HARRBOR_LOG_LEVEL"] = strings.ToUpper(logLevel)

	providerDefault := strings.Join(defaultTunedProviders(cfg), ",")
	providerRaw, err := p.text("NNTP provider pools to tune (comma-separated; 'none' to clear)", providerDefault)
	if err != nil {
		return false, err
	}
	providers, err := parseConfigureProviders(providerRaw)
	if err != nil {
		return false, err
	}
	for _, provider := range providers {
		if err := configureProviderPool(&p, cfg, values, provider); err != nil {
			return false, err
		}
	}
	if err := configureHTTPBackends(&p, cfg, values, secretUpdates, opts.DialBackend, false); err != nil {
		return false, err
	}
	crossLaneSplice, err := p.yesNo("Proof-gated cross-lane continuity (required for durable aggregator refresh)", cfg.NNTP.CrossLaneSplice)
	if err != nil {
		return false, err
	}
	values["HARRBOR_NNTP_CROSSLANE_SPLICE"] = strconv.FormatBool(crossLaneSplice)
	stremioMode, stremioBaseURL, err := configureStremioEdge(&p, cfg, values)
	if err != nil {
		return false, err
	}
	if cfg.Topology.Active == "t2" || cfg.Topology.Active == "t3" || strings.TrimSpace(cfg.MediaFlow.Password) != "" {
		if err := configureMediaFlowOrigin(&p, cfg, values, stremioBaseURL); err != nil {
			return false, err
		}
	}
	destinationStage := newStagedArrDestinationStore(opts.ArrStore)
	if err := configureReactive(&p, cfg, values, opts.CheckArr, destinationStage, cfg.Reactive.PromotionEnabled, false); err != nil {
		return false, err
	}

	if err := config.ValidateTuning(values); err != nil {
		return false, err
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	fmt.Fprintln(p.out, "\nSettings to save:")
	for _, key := range keys {
		value := values[key]
		if strings.HasSuffix(key, "_URL") {
			value = "[redacted]"
		}
		fmt.Fprintf(p.out, "  %s=%s\n", key, value)
	}
	confirmed, err := p.yesNo("Save and apply on restart", true)
	if err != nil {
		return false, err
	}
	if !confirmed {
		fmt.Fprintln(p.out, "No changes made.")
		return false, nil
	}
	if err := applyConfigureFiles(opts.Path, opts.SecretsPath, cfg.Secrets, values, originalValues, secretUpdates); err != nil {
		return false, err
	}
	if len(destinationStage.pending) > 0 {
		if err := destinationStage.Apply(); err != nil {
			return false, fmt.Errorf("save reactive Arr destinations: %w", err)
		}
	}
	fmt.Fprintf(p.out, "Saved %s\n", opts.Path)
	printStremioEdgeInstructions(p.out, stremioMode, stremioBaseURL, cfg.Stremio.EffectiveInstallToken(cfg.Server.StreamSecret), true)
	return true, nil
}

func applyConfigureFiles(tuningPath, secretsPath string, secrets *config.SecretStore, values, originalValues, secretUpdates map[string]string) error {
	if err := config.WriteTuning(tuningPath, values); err != nil {
		return err
	}
	if len(secretUpdates) == 0 {
		return nil
	}
	rollback := func(err error) error {
		if rollbackErr := config.WriteTuning(tuningPath, originalValues); rollbackErr != nil {
			return fmt.Errorf("%w; tuning rollback failed: %v", err, rollbackErr)
		}
		return err
	}
	if strings.TrimSpace(secretsPath) == "" {
		return rollback(fmt.Errorf("sealed secrets path is required for HTTP backend URLs"))
	}
	if err := config.UpdateSealedSecrets(secretsPath, secrets, secretUpdates); err != nil {
		return rollback(fmt.Errorf("update sealed HTTP backend URLs: %w", err))
	}
	return nil
}

func cloneStringMap(values map[string]string) map[string]string {
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

type configurePrompter struct {
	reader *bufio.Reader
	out    io.Writer
}

func (p *configurePrompter) readLine() (string, error) {
	line, err := p.reader.ReadSlice('\n')
	if err == bufio.ErrBufferFull {
		return "", fmt.Errorf("input exceeds 8192-byte limit; no changes made")
	}
	return string(line), err
}

func (p *configurePrompter) text(label, current string) (string, error) {
	fmt.Fprintf(p.out, "%s [%s]: ", label, current)
	line, err := p.readLine()
	if err == io.EOF && line == "" {
		return "", fmt.Errorf("input closed; no changes made")
	}
	if err != nil && err != io.EOF {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		line = current
	}
	return line, nil
}

func (p *configurePrompter) secret(label string, configured bool) (string, bool, error) {
	state := "unset"
	if configured {
		state = "configured; blank preserves"
	}
	line, err := secretinput.ReadLine(p.out, fmt.Sprintf("%s [%s]: ", label, state), os.Stdin, p.reader)
	if err == io.EOF && line == "" {
		return "", false, fmt.Errorf("input closed; no changes made")
	}
	if err != nil && err != io.EOF {
		return "", false, err
	}
	line = strings.TrimSpace(line)
	return line, line != "", nil
}

func (p *configurePrompter) integer(label string, current, minimum int) (value int, changed bool, err error) {
	raw, err := p.text(label, strconv.Itoa(current))
	if err != nil {
		return 0, false, err
	}
	value, err = strconv.Atoi(raw)
	if err != nil || value < minimum {
		return 0, false, fmt.Errorf("%s must be an integer >= %d", label, minimum)
	}
	return value, value != current, nil
}

func (p *configurePrompter) yesNo(label string, def bool) (bool, error) {
	suffix := "[Y/n]"
	if !def {
		suffix = "[y/N]"
	}
	fmt.Fprintf(p.out, "%s %s: ", label, suffix)
	line, err := p.readLine()
	if err == io.EOF && line == "" {
		return false, fmt.Errorf("input closed; no changes made")
	}
	if err != nil && err != io.EOF {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "":
		return def, nil
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	default:
		return false, fmt.Errorf("%s expects yes or no", label)
	}
}

func configureProviderPool(p *configurePrompter, cfg *config.Config, values map[string]string, provider string) error {
	prefix := "HARRBOR_PROVIDER_" + strings.ToUpper(provider) + "_"
	currentTotal := lookupConfigureInt(cfg, prefix+"TOTAL_CONNECTIONS", 0)
	if currentTotal == 0 {
		if provider == "newshosting" {
			currentTotal = cfg.NNTP.Connections
		} else {
			currentTotal = lookupConfigureInt(cfg, prefix+"CONNECTIONS", cfg.Cache.TotalConnections)
		}
	}
	if currentTotal < 3 {
		currentTotal = 8
	}

	currentDemand := lookupConfigureInt(cfg, prefix+"DEMAND_CONNECTIONS", 0)
	currentReadahead := lookupConfigureInt(cfg, prefix+"READAHEAD_CONNECTIONS", 0)
	if currentDemand < 1 || currentReadahead < 2 || currentDemand+currentReadahead != currentTotal {
		currentDemand, currentReadahead = splitConnections(currentTotal)
	}

	total, changed, err := p.integer(provider+" usable connection limit", currentTotal, 3)
	if err != nil {
		return err
	}
	demand, readahead := currentDemand, currentReadahead
	if changed {
		demand, readahead = splitConnections(total)
	}
	fmt.Fprintf(p.out, "  %s split: demand=%d readahead=%d\n", provider, demand, readahead)

	currentMax := lookupConfigureInt(cfg, prefix+"MAX_CONNS_PER_FILE", min(16, demand))
	if currentMax > demand {
		currentMax = demand
	}
	maxPerFile, _, err := p.integer(provider+" max demand connections per file", currentMax, 1)
	if err != nil {
		return err
	}
	if maxPerFile > demand {
		return fmt.Errorf("%s max demand connections per file must not exceed %d", provider, demand)
	}
	if provider == "newshosting" {
		values["HARRBOR_NNTP_CONNECTIONS"] = strconv.Itoa(total)
	} else {
		values[prefix+"CONNECTIONS"] = strconv.Itoa(total)
	}
	values[prefix+"TOTAL_CONNECTIONS"] = strconv.Itoa(total)
	values[prefix+"DEMAND_CONNECTIONS"] = strconv.Itoa(demand)
	values[prefix+"READAHEAD_CONNECTIONS"] = strconv.Itoa(readahead)
	values[prefix+"MAX_CONNS_PER_FILE"] = strconv.Itoa(maxPerFile)
	return nil
}

func defaultTunedProviders(cfg *config.Config) []string {
	var providers []string
	for _, provider := range cfg.Providers.UsenetProviders {
		prefix := "HARRBOR_PROVIDER_" + strings.ToUpper(provider) + "_"
		if _, ok := cfg.Lookup(prefix + "TOTAL_CONNECTIONS"); ok {
			providers = append(providers, provider)
		}
	}
	if len(providers) > 0 {
		return providers
	}
	for _, provider := range cfg.Providers.UsenetProviders {
		if provider != "torbox" {
			return []string{provider}
		}
	}
	return nil
}

func parseConfigureProviders(raw string) ([]string, error) {
	if strings.EqualFold(strings.TrimSpace(raw), "none") || strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	seen := map[string]bool{}
	var providers []string
	for _, part := range strings.Split(raw, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		if name == "" || seen[name] {
			continue
		}
		for _, r := range name {
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' {
				return nil, fmt.Errorf("invalid provider name %q", name)
			}
		}
		seen[name] = true
		providers = append(providers, name)
	}
	return providers, nil
}

func splitConnections(total int) (demand, readahead int) {
	readahead = total * 2 / 5
	if readahead < 2 {
		readahead = 2
	}
	return total - readahead, readahead
}

func lookupConfigureInt(cfg *config.Config, key string, fallback int) int {
	if raw, ok := cfg.Lookup(key); ok {
		if value, err := strconv.Atoi(raw); err == nil && value > 0 {
			return value
		}
	}
	return fallback
}

func effectiveMode(override, fallback string) string {
	if strings.TrimSpace(override) != "" {
		return override
	}
	return fallback
}
