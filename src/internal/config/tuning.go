package config

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/topology"
)

const DefaultTuningPath = "/config/darkharrbor.conf"

var tuningKeys = map[string]bool{
	"HARRBOR_BACKUP_ENABLED":                    true,
	"HARRBOR_BACKUP_TARGET":                     true,
	"HARRBOR_BACKUP_INTERVAL_HOURS":             true,
	"HARRBOR_BACKUP_KEEP":                       true,
	"HARRBOR_NNTP_CACHE_MODE":                   true,
	"HARRBOR_STREAM_CACHE_MODE":                 true,
	"HARRBOR_DISK_CACHE_SIZE_MB":                true,
	"HARRBOR_DISK_CACHE_TTL_MIN":                true,
	"HARRBOR_FULL_EVICT_MODE":                   true,
	"HARRBOR_FULL_EVICT_TTL_MIN":                true,
	"HARRBOR_READAHEAD_MAX_SEGMENTS":            true,
	"HARRBOR_STREAM_CHUNK_SIZE_MB":              true,
	"HARRBOR_STREAM_MIN_BUFFER_SEGMENTS":        true,
	"HARRBOR_STREAM_READAHEAD_WORKERS":          true,
	"HARRBOR_READAHEAD_SEEK_WIDEN_MS":           true,
	"HARRBOR_NNTP_CONNECTIONS":                  true,
	"HARRBOR_NNTP_STRIPE":                       true,
	"HARRBOR_NNTP_CROSSLANE_SPLICE":             true,
	"HARRBOR_NNTP_PREWARM_BYTES":                true,
	"HARRBOR_PREFERENCE":                        true,
	"HARRBOR_GOVERNOR_UNCACHED_BUDGET":          true,
	"HARRBOR_GOVERNOR_STALL_TIMEOUT_MIN":        true,
	"HARRBOR_GOVERNOR_DEAD_SENTINEL_GRACE_MIN":  true,
	"HARRBOR_TORRENT_SUBMIT_BUDGET_MIN":         true,
	"HARRBOR_TORRENT_KEEPWARM_DAYS":             true,
	"HARRBOR_SUPPRESSION_TTL_HOURS":             true,
	"HARRBOR_SEARCH_BUDGET_CAP":                 true,
	"HARRBOR_SEARCH_BUDGET_SUPPRESS_DAYS":       true,
	"HARRBOR_REACTIVE_ENABLED":                  true,
	"HARRBOR_REACTIVE_COMMIT_MODE":              true,
	"HARRBOR_REACTIVE_THRESHOLD":                true,
	"HARRBOR_REACTIVE_MONITOR_EPISODES":         true,
	"HARRBOR_REACTIVE_MONITOR_MOVIES":           true,
	"HARRBOR_REACTIVE_PROMOTION_ENABLED":        true,
	"HARRBOR_REACTIVE_DEFAULT_SERIES_ARR":       true,
	"HARRBOR_REACTIVE_DEFAULT_MOVIE_ARR":        true,
	"HARRBOR_REACTIVE_PROMOTION_FREE_PERCENT":   true,
	"HARRBOR_HTTP_STREAM_ENABLED":               true,
	"HARRBOR_HTTP_ALLOW_PRIVATE_SOURCES":        true,
	"HARRBOR_HTTP_BACKENDS":                     true,
	"HARRBOR_STRM_INSTALLER_ENABLED":            true,
	"HARRBOR_STREMIO_EDGE_MODE":                 true,
	"HARRBOR_STREMIO_CLIENT_BASE_URL":           true,
	"HARRBOR_LOG_LEVEL":                         true,
	"HARRBOR_TOPOLOGY":                          true,
	"HARRBOR_TOPOLOGY_ACHIEVED_STALENESS_HOURS": true,
	"HARRBOR_MEDIAFLOW_PUBLIC_IP":               true,
	"HARRBOR_MEDIAFLOW_BASE_URL":                true,
}

var providerTuningSuffixes = []string{
	"TOTAL_CONNECTIONS",
	"DEMAND_CONNECTIONS",
	"READAHEAD_CONNECTIONS",
	"MAX_CONNS_PER_FILE",
	"CONNECTIONS",
	"RETENTION_DAYS",
}

var httpBackendTuningSuffixes = []string{
	"PUBLIC_URL",
	"REDIRECT_ORIGINS",
	"URL",
	"TYPE",
	"DESCRIPTORS_FILE",
}

// TuningPath returns the non-secret runtime configuration path. The override
// exists mainly for tests and unusual mounts; normal deployments use /config.
func TuningPath() string {
	if path := strings.TrimSpace(os.Getenv("HARRBOR_TUNING_FILE")); path != "" {
		return path
	}
	return DefaultTuningPath
}

// ReadTuning reads the configure-managed non-secret settings file. A missing
// file is the normal pre-configure state.
func ReadTuning(path string) (map[string]string, error) {
	values, _, err := ReadTuningWithWarnings(path)
	return values, err
}

// ReadTuningWithWarnings ignores unknown or retired keys so a stale wizard
// file can never prevent startup. Warnings contain key names only.
func ReadTuningWithWarnings(path string) (map[string]string, []string, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]string{}, nil, nil
		}
		return nil, nil, fmt.Errorf("open tuning file: %w", err)
	}
	defer func() { _ = file.Close() }()

	values := map[string]string{}
	var warnings []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), 8*1024)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, raw, ok := strings.Cut(line, "=")
		if !ok {
			return nil, nil, fmt.Errorf("parse tuning file line %d: missing '='", lineNo)
		}
		key = strings.TrimSpace(key)
		if !isTuningKey(key) {
			warnings = append(warnings, fmt.Sprintf("configure tuning ignored unknown key %s", key))
			continue
		}
		if _, exists := values[key]; exists {
			return nil, nil, fmt.Errorf("parse tuning file line %d: duplicate %s", lineNo, key)
		}
		value, err := parseDotEnvValue(strings.TrimSpace(raw))
		if err != nil {
			return nil, nil, fmt.Errorf("parse tuning file line %d (%s): %w", lineNo, key, err)
		}
		if key == "HARRBOR_TOPOLOGY" {
			if _, ok := topology.Parse(value); !ok {
				warnings = append(warnings, "configure tuning ignored unknown HARRBOR_TOPOLOGY value; using t1")
				continue
			}
		}
		if key == "HARRBOR_STREMIO_EDGE_MODE" && !ValidStremioEdgeMode(value) {
			warnings = append(warnings, "configure tuning ignored unknown HARRBOR_STREMIO_EDGE_MODE value; using internal")
			continue
		}
		if key == "HARRBOR_REACTIVE_COMMIT_MODE" && !ValidReactiveCommitMode(value) {
			warnings = append(warnings, "configure tuning ignored unknown HARRBOR_REACTIVE_COMMIT_MODE value; using off")
			continue
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("scan tuning file: %w", err)
	}
	if err := ValidateTuning(values); err != nil {
		return nil, nil, err
	}
	return values, warnings, nil
}

// WriteTuning atomically replaces the configure-managed settings file.
func WriteTuning(path string, values map[string]string) error {
	if err := ValidateTuning(values); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create tuning directory: %w", err)
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	tmp, err := os.CreateTemp(filepath.Dir(path), ".darkharrbor.conf-*")
	if err != nil {
		return fmt.Errorf("create temporary tuning file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temporary tuning file: %w", err)
	}
	if _, err = fmt.Fprintln(tmp, "# Managed by `darkharrbor configure`; credentials are rejected."); err == nil {
		for _, key := range keys {
			_, err = fmt.Fprintf(tmp, "%s=%s\n", key, values[key])
			if err != nil {
				break
			}
		}
	}
	if err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write tuning file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync tuning file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tuning file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace tuning file: %w", err)
	}
	return nil
}

// ValidateTuning rejects secrets, typos, malformed values, and unsafe pool
// splits before they can make a restart fail.
func ValidateTuning(values map[string]string) error {
	for key, value := range values {
		if !isTuningKey(key) {
			return fmt.Errorf("%s is not an allowed non-secret setting", key)
		}
		value = strings.TrimSpace(value)
		if value == "" || strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("%s must not be empty or contain newlines", key)
		}
		switch key {
		case "HARRBOR_TOPOLOGY":
			if _, ok := topology.Parse(value); !ok {
				return fmt.Errorf("%s must be one of t1|t2|t3", key)
			}
		case "HARRBOR_TOPOLOGY_ACHIEVED_STALENESS_HOURS":
			if hours, err := strconv.Atoi(value); err != nil || hours <= 0 {
				return fmt.Errorf("%s must be a positive integer number of hours", key)
			}
		case "HARRBOR_MEDIAFLOW_PUBLIC_IP":
			if net.ParseIP(value) == nil {
				return fmt.Errorf("%s must be a valid IP address", key)
			}
		case "HARRBOR_MEDIAFLOW_BASE_URL":
			if _, err := url.Parse(value); err != nil {
				return fmt.Errorf("%s must be a valid absolute URL", key)
			}
		case "HARRBOR_STREMIO_EDGE_MODE":
			if !ValidStremioEdgeMode(value) {
				return fmt.Errorf("%s must be one of internal|lan|tailscale|custom|funnel", key)
			}
		case "HARRBOR_STREMIO_CLIENT_BASE_URL":
			if _, err := ParseStremioClientBaseURL(value); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
		case "HARRBOR_REACTIVE_COMMIT_MODE":
			if !ValidReactiveCommitMode(value) {
				return fmt.Errorf("%s must be one of off|supervised|auto", key)
			}
		case "HARRBOR_NNTP_CACHE_MODE", "HARRBOR_STREAM_CACHE_MODE":
			if !validCacheMode(value) {
				return fmt.Errorf("%s must be one of none|readahead|disk|full", key)
			}
		case "HARRBOR_FULL_EVICT_MODE":
			if value != "ttl" && value != "manual" && value != "never" {
				return fmt.Errorf("%s must be one of ttl|manual|never", key)
			}
		case "HARRBOR_BACKUP_ENABLED", "HARRBOR_NNTP_STRIPE", "HARRBOR_NNTP_CROSSLANE_SPLICE", "HARRBOR_HTTP_STREAM_ENABLED", "HARRBOR_HTTP_ALLOW_PRIVATE_SOURCES", "HARRBOR_STRM_INSTALLER_ENABLED", "HARRBOR_REACTIVE_ENABLED", "HARRBOR_REACTIVE_MONITOR_EPISODES", "HARRBOR_REACTIVE_MONITOR_MOVIES", "HARRBOR_REACTIVE_PROMOTION_ENABLED":
			if _, err := strconv.ParseBool(value); err != nil {
				return fmt.Errorf("%s must be true or false", key)
			}
		case "HARRBOR_REACTIVE_THRESHOLD":
			threshold, err := strconv.ParseFloat(value, 64)
			if err != nil || threshold <= 0 || threshold > 1 || math.IsNaN(threshold) || math.IsInf(threshold, 0) {
				return fmt.Errorf("%s must be greater than 0 and at most 1", key)
			}
		case "HARRBOR_REACTIVE_PROMOTION_FREE_PERCENT":
			percent, err := strconv.Atoi(value)
			if err != nil || percent < 1 || percent > 99 {
				return fmt.Errorf("%s must be an integer from 1 through 99", key)
			}
		case "HARRBOR_REACTIVE_DEFAULT_SERIES_ARR", "HARRBOR_REACTIVE_DEFAULT_MOVIE_ARR":
			if !backendNameRe.MatchString(value) {
				return fmt.Errorf("%s must be a valid Arr instance name", key)
			}
		case "HARRBOR_HTTP_BACKENDS":
			seen := map[string]bool{}
			for _, raw := range strings.Split(value, ",") {
				name := strings.ToLower(strings.TrimSpace(raw))
				if name == "" || !backendNameRe.MatchString(name) {
					return fmt.Errorf("HARRBOR_HTTP_BACKENDS contains invalid backend name")
				}
				if seen[name] {
					return fmt.Errorf("HARRBOR_HTTP_BACKENDS contains a duplicate backend name")
				}
				seen[name] = true
			}
		case "HARRBOR_BACKUP_TARGET":
			if !filepath.IsAbs(value) || filepath.Clean(value) == string(filepath.Separator) || strings.Contains(value, "://") {
				return fmt.Errorf("%s must be a safe absolute directory", key)
			}
		case "HARRBOR_LOG_LEVEL":
			switch strings.ToUpper(value) {
			case "DEBUG", "INFO", "WARN", "ERROR":
			default:
				return fmt.Errorf("%s must be one of DEBUG|INFO|WARN|ERROR", key)
			}
		case "HARRBOR_PREFERENCE":
			if err := validateTuningPreference(value); err != nil {
				return err
			}
		case "HARRBOR_SEARCH_BUDGET_CAP", "HARRBOR_SEARCH_BUDGET_SUPPRESS_DAYS":
			n, err := strconv.Atoi(value)
			limit := 1000
			if key == "HARRBOR_SEARCH_BUDGET_SUPPRESS_DAYS" {
				limit = 36500
			}
			if err != nil || n < 1 || n > limit {
				return fmt.Errorf("%s must be an integer from 1 through %d", key, limit)
			}
		default:
			if _, suffix, ok := httpBackendTuningKey(key); ok {
				switch suffix {
				case "URL":
					u, err := url.Parse(value)
					if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
						return fmt.Errorf("%s must be an absolute http(s) URL without userinfo", key)
					}
				case "PUBLIC_URL":
					u, err := url.Parse(value)
					if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
						u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
						return fmt.Errorf("%s must be an absolute http(s) origin without userinfo, path, query, or fragment", key)
					}
				case "REDIRECT_ORIGINS":
					if _, err := ParseHTTPBackendRedirectOrigins(value); err != nil {
						return fmt.Errorf("%s invalid: %w", key, err)
					}
				case "TYPE":
					switch strings.ToLower(value) {
					case "ia", "omss", "stremio", "generic":
					default:
						return fmt.Errorf("%s must be one of ia|omss|stremio|generic", key)
					}
				case "DESCRIPTORS_FILE":
					if !filepath.IsAbs(value) {
						return fmt.Errorf("%s must be an absolute path", key)
					}
				}
				continue
			}
			if strings.HasSuffix(key, "_RETENTION_DAYS") {
				days, err := strconv.Atoi(value)
				if err != nil || days < 1 || days > 36500 {
					return fmt.Errorf("%s must be an integer from 1 through 36500", key)
				}
				continue
			}
			minimum := int64(1)
			if strings.HasSuffix(key, "READAHEAD_CONNECTIONS") {
				minimum = 2
			}
			if key == "HARRBOR_STREAM_MIN_BUFFER_SEGMENTS" {
				minimum = 0
			}
			if key == "HARRBOR_NNTP_PREWARM_BYTES" {
				minimum = 0
			}
			if key == "HARRBOR_TORRENT_KEEPWARM_DAYS" {
				minimum = 0
			}
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < minimum {
				return fmt.Errorf("%s must be an integer >= %d", key, minimum)
			}
		}
	}

	providers := map[string]bool{}
	for key := range values {
		if provider, _, ok := providerTuningKey(key); ok {
			providers[provider] = true
		}
	}
	for provider := range providers {
		prefix := "HARRBOR_PROVIDER_" + provider + "_"
		total, hasTotal := tuningInt(values, prefix+"TOTAL_CONNECTIONS")
		demand, hasDemand := tuningInt(values, prefix+"DEMAND_CONNECTIONS")
		readahead, hasReadahead := tuningInt(values, prefix+"READAHEAD_CONNECTIONS")
		if hasTotal && hasDemand && hasReadahead && demand+readahead != total {
			return fmt.Errorf("%s pool split must satisfy demand + readahead = total", provider)
		}
		if maxPerFile, ok := tuningInt(values, prefix+"MAX_CONNS_PER_FILE"); ok && hasDemand && maxPerFile > demand {
			return fmt.Errorf("%s max connections per file must not exceed demand connections", provider)
		}
	}
	return nil
}

func applyTuning(cfg *Config, values map[string]string) {
	cfg.tuning = values
	if value := values["HARRBOR_TOPOLOGY"]; value != "" {
		cfg.Topology.Active, _ = topology.Parse(value)
	}
	if value := values["HARRBOR_TOPOLOGY_ACHIEVED_STALENESS_HOURS"]; value != "" {
		if hours, err := strconv.Atoi(value); err == nil && hours > 0 {
			cfg.Topology.AchievedPostureStalenessHours = hours
		}
	}
	if value := values["HARRBOR_MEDIAFLOW_PUBLIC_IP"]; value != "" {
		cfg.MediaFlow.PublicIP = value
	}
	if value := values["HARRBOR_MEDIAFLOW_BASE_URL"]; value != "" {
		cfg.MediaFlow.BaseURL = value
	}
	if value := values["HARRBOR_BACKUP_ENABLED"]; value != "" {
		cfg.Backup.Enabled, _ = strconv.ParseBool(value)
	}
	if value := values["HARRBOR_BACKUP_TARGET"]; value != "" {
		cfg.Backup.Target = value
	}
	if value, ok := tuningInt(values, "HARRBOR_BACKUP_INTERVAL_HOURS"); ok {
		cfg.Backup.IntervalHours = value
	}
	if value, ok := tuningInt(values, "HARRBOR_BACKUP_KEEP"); ok {
		cfg.Backup.Keep = value
	}
	if value := values["HARRBOR_NNTP_CACHE_MODE"]; value != "" {
		cfg.Cache.NNTPMode = value
	}
	if value := values["HARRBOR_STREAM_CACHE_MODE"]; value != "" {
		cfg.Cache.StreamMode = value
	}
	if value := values["HARRBOR_FULL_EVICT_MODE"]; value != "" {
		cfg.Cache.FullEvictMode = value
	}
	if value, ok := tuningInt(values, "HARRBOR_DISK_CACHE_SIZE_MB"); ok {
		cfg.Cache.DiskCacheSizeMB = int64(value)
	}
	if value, ok := tuningInt(values, "HARRBOR_DISK_CACHE_TTL_MIN"); ok {
		cfg.Cache.DiskCacheTTLMin = value
	}
	if value, ok := tuningInt(values, "HARRBOR_FULL_EVICT_TTL_MIN"); ok {
		cfg.Cache.FullEvictTTLMin = value
	}
	if value, ok := tuningInt(values, "HARRBOR_READAHEAD_MAX_SEGMENTS"); ok {
		cfg.Cache.ReadaheadMaxSegments = value
	}
	if value, ok := tuningInt(values, "HARRBOR_STREAM_CHUNK_SIZE_MB"); ok {
		cfg.Cache.StreamChunkSizeMB = value
	}
	if value, ok := tuningInt(values, "HARRBOR_STREAM_MIN_BUFFER_SEGMENTS"); ok {
		cfg.Cache.StreamMinBufferSegments = value
	}
	if value, ok := tuningInt(values, "HARRBOR_STREAM_READAHEAD_WORKERS"); ok {
		cfg.Cache.StreamReadaheadWorkers = value
	}
	if value, ok := tuningInt(values, "HARRBOR_READAHEAD_SEEK_WIDEN_MS"); ok {
		cfg.Cache.ReadaheadSeekWidenMS = value
	}
	if value, ok := tuningInt(values, "HARRBOR_NNTP_CONNECTIONS"); ok {
		cfg.NNTP.Connections = value
	}
	if value := values["HARRBOR_NNTP_STRIPE"]; value != "" {
		cfg.NNTP.Stripe, _ = strconv.ParseBool(value)
	}
	if value := values["HARRBOR_NNTP_CROSSLANE_SPLICE"]; value != "" {
		cfg.NNTP.CrossLaneSplice, _ = strconv.ParseBool(value)
	}
	if value, ok := tuningInt(values, "HARRBOR_NNTP_PREWARM_BYTES"); ok {
		cfg.Prewarm.NNTPBytes = int64(value)
	}
	if value, ok := tuningInt(values, "HARRBOR_GOVERNOR_UNCACHED_BUDGET"); ok {
		cfg.Governor.UncachedBudget = value
	}
	if value, ok := tuningInt(values, "HARRBOR_GOVERNOR_STALL_TIMEOUT_MIN"); ok {
		cfg.Governor.StallTimeoutMin = value
	}
	if value, ok := tuningInt(values, "HARRBOR_GOVERNOR_DEAD_SENTINEL_GRACE_MIN"); ok {
		cfg.Governor.DeadSentinelGraceMin = value
	}
	if value, ok := tuningInt(values, "HARRBOR_TORRENT_SUBMIT_BUDGET_MIN"); ok {
		cfg.Governor.SubmitBudgetMin = value
	}
	if value, ok := tuningInt(values, "HARRBOR_TORRENT_KEEPWARM_DAYS"); ok {
		cfg.Governor.TorrentKeepWarmDays = value
	}
	if value, ok := tuningInt(values, "HARRBOR_SEARCH_BUDGET_CAP"); ok {
		cfg.SearchBudget.Cap = value
	}
	if value, ok := tuningInt(values, "HARRBOR_SEARCH_BUDGET_SUPPRESS_DAYS"); ok {
		cfg.SearchBudget.SuppressDays = value
	}
	if value := values["HARRBOR_REACTIVE_ENABLED"]; value != "" {
		cfg.Reactive.Enabled, _ = strconv.ParseBool(value)
	}
	if value := values["HARRBOR_REACTIVE_COMMIT_MODE"]; value != "" {
		cfg.Reactive.CommitMode = strings.ToLower(strings.TrimSpace(value))
	}
	if value := values["HARRBOR_REACTIVE_THRESHOLD"]; value != "" {
		cfg.Reactive.Threshold, _ = strconv.ParseFloat(value, 64)
	}
	if value := values["HARRBOR_REACTIVE_MONITOR_EPISODES"]; value != "" {
		cfg.Reactive.MonitorEpisodes, _ = strconv.ParseBool(value)
	}
	if value := values["HARRBOR_REACTIVE_MONITOR_MOVIES"]; value != "" {
		cfg.Reactive.MonitorMovies, _ = strconv.ParseBool(value)
	}
	if value := values["HARRBOR_REACTIVE_PROMOTION_ENABLED"]; value != "" {
		cfg.Reactive.PromotionEnabled, _ = strconv.ParseBool(value)
	}
	if value := strings.TrimSpace(values["HARRBOR_REACTIVE_DEFAULT_SERIES_ARR"]); value != "" {
		cfg.Reactive.DefaultSeriesArr = value
	}
	if value := strings.TrimSpace(values["HARRBOR_REACTIVE_DEFAULT_MOVIE_ARR"]); value != "" {
		cfg.Reactive.DefaultMovieArr = value
	}
	if value, ok := tuningInt(values, "HARRBOR_REACTIVE_PROMOTION_FREE_PERCENT"); ok {
		cfg.Reactive.PromotionFreePercent = value
	}
	if value := values["HARRBOR_PREFERENCE"]; value != "" {
		cfg.Routing.Preference, cfg.Routing.PreferenceExplicit = applyPreference(value)
	}
	if value := values["HARRBOR_HTTP_STREAM_ENABLED"]; value != "" {
		cfg.HTTPStream.Enabled, _ = strconv.ParseBool(value)
	}
	if value := values["HARRBOR_HTTP_ALLOW_PRIVATE_SOURCES"]; value != "" {
		cfg.HTTPStream.AllowPrivateSourceCIDRs, _ = strconv.ParseBool(value)
	}
	if value := values["HARRBOR_STRM_INSTALLER_ENABLED"]; value != "" {
		cfg.StrmInstaller.Enabled, _ = strconv.ParseBool(value)
	}
	if value := values["HARRBOR_STREMIO_EDGE_MODE"]; value != "" {
		cfg.Stremio.EdgeMode = value
	}
	if value := values["HARRBOR_STREMIO_CLIENT_BASE_URL"]; value != "" {
		cfg.Stremio.ClientBaseURL = value
	}
}

func isTuningKey(key string) bool {
	if tuningKeys[key] {
		return true
	}
	if _, _, ok := httpBackendTuningKey(key); ok {
		return true
	}
	_, _, ok := providerTuningKey(key)
	return ok
}

func httpBackendTuningKey(key string) (backend, suffix string, ok bool) {
	const prefix = "HARRBOR_HTTPBACKEND_"
	if !strings.HasPrefix(key, prefix) {
		return "", "", false
	}
	for _, candidate := range httpBackendTuningSuffixes {
		marker := "_" + candidate
		if !strings.HasSuffix(key, marker) {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(key, prefix), marker)
		if name == "" {
			return "", "", false
		}
		for _, r := range name {
			if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
				return "", "", false
			}
		}
		return name, candidate, true
	}
	return "", "", false
}

func providerTuningKey(key string) (provider, suffix string, ok bool) {
	const prefix = "HARRBOR_PROVIDER_"
	if !strings.HasPrefix(key, prefix) {
		return "", "", false
	}
	for _, candidate := range providerTuningSuffixes {
		marker := "_" + candidate
		if !strings.HasSuffix(key, marker) {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(key, prefix), marker)
		if name == "" {
			return "", "", false
		}
		for _, r := range name {
			if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
				return "", "", false
			}
		}
		return name, candidate, true
	}
	return "", "", false
}

func validateTuningPreference(value string) error {
	if strings.EqualFold(strings.TrimSpace(value), PreferenceNone) {
		return nil
	}
	seen := map[string]bool{}
	for _, raw := range strings.Split(value, ",") {
		lane := strings.TrimSpace(raw)
		switch lane {
		case LaneTorBoxTorrent, LaneTorBoxNZB, LaneNNTPNZB, LaneUncachedTorrent, LaneUncachedTorrentDerank:
		default:
			return fmt.Errorf("HARRBOR_PREFERENCE contains unknown lane %q", lane)
		}
		if seen[lane] {
			return fmt.Errorf("HARRBOR_PREFERENCE contains duplicate lane %q", lane)
		}
		seen[lane] = true
	}
	if seen[LaneUncachedTorrent] && seen[LaneUncachedTorrentDerank] {
		return fmt.Errorf("HARRBOR_PREFERENCE cannot contain both %q and %q", LaneUncachedTorrent, LaneUncachedTorrentDerank)
	}
	if len(seen) == 0 {
		return errors.New("HARRBOR_PREFERENCE must contain at least one lane")
	}
	return nil
}

func tuningInt(values map[string]string, key string) (int, bool) {
	value, ok := values[key]
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(value))
	return n, err == nil
}
