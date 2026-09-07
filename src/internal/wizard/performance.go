package wizard

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/darkharrbor/darkharrbor/internal/config"
)

const (
	lowMemoryThresholdBytes = 2 << 30
	highThroughputFreeBytes = 10 << 30
)

type performanceRecommendation struct {
	Default                 string
	HighThroughputAvailable bool
	Reason                  string
}

func detectPerformanceRecommendation(dataRoot string) performanceRecommendation {
	var stat syscall.Statfs_t
	diskKnown := syscall.Statfs(dataRoot, &stat) == nil
	free := int64(0)
	if diskKnown {
		free = int64(stat.Bavail) * int64(stat.Bsize)
	}
	return recommendPerformance(containerMemoryLimit(), free, diskKnown)
}

func recommendPerformance(memoryLimit, freeBytes int64, diskKnown bool) performanceRecommendation {
	recommendation := performanceRecommendation{Default: "recommended", HighThroughputAvailable: true}
	if memoryLimit > 0 && memoryLimit < lowMemoryThresholdBytes {
		recommendation.Default = "low-memory"
		recommendation.Reason = "Low memory is recommended because the container memory limit is below 2 GiB."
	}
	if diskKnown && freeBytes < highThroughputFreeBytes {
		recommendation.HighThroughputAvailable = false
		if recommendation.Reason == "" {
			recommendation.Reason = "High throughput is unavailable because /data has less than 10 GiB free."
		}
	}
	return recommendation
}

func containerMemoryLimit() int64 {
	for _, path := range []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		value := strings.TrimSpace(string(raw))
		if value == "" || value == "max" {
			return 0
		}
		limit, err := strconv.ParseInt(value, 10, 64)
		if err == nil && limit > 0 && limit < 1<<60 {
			return limit
		}
	}
	return 0
}

var performanceProfiles = map[string]map[string]string{
	"recommended": {
		"HARRBOR_NNTP_CACHE_MODE":            "readahead",
		"HARRBOR_STREAM_CACHE_MODE":          "readahead",
		"HARRBOR_DISK_CACHE_SIZE_MB":         "2048",
		"HARRBOR_READAHEAD_MAX_SEGMENTS":     "16",
		"HARRBOR_STREAM_CHUNK_SIZE_MB":       "16",
		"HARRBOR_STREAM_MIN_BUFFER_SEGMENTS": "2",
		"HARRBOR_STREAM_READAHEAD_WORKERS":   "1",
	},
	"low-memory": {
		"HARRBOR_NNTP_CACHE_MODE":            "readahead",
		"HARRBOR_STREAM_CACHE_MODE":          "readahead",
		"HARRBOR_DISK_CACHE_SIZE_MB":         "1024",
		"HARRBOR_READAHEAD_MAX_SEGMENTS":     "8",
		"HARRBOR_STREAM_CHUNK_SIZE_MB":       "8",
		"HARRBOR_STREAM_MIN_BUFFER_SEGMENTS": "1",
		"HARRBOR_STREAM_READAHEAD_WORKERS":   "1",
	},
	"high-throughput": {
		"HARRBOR_NNTP_CACHE_MODE":            "disk",
		"HARRBOR_STREAM_CACHE_MODE":          "disk",
		"HARRBOR_DISK_CACHE_SIZE_MB":         "8192",
		"HARRBOR_READAHEAD_MAX_SEGMENTS":     "32",
		"HARRBOR_STREAM_CHUNK_SIZE_MB":       "16",
		"HARRBOR_STREAM_MIN_BUFFER_SEGMENTS": "4",
		"HARRBOR_STREAM_READAHEAD_WORKERS":   "1",
	},
}

func applyPerformanceProfile(values map[string]string, name string) error {
	profile, ok := performanceProfiles[name]
	if !ok {
		return fmt.Errorf("unknown performance profile %q", name)
	}
	for key, value := range profile {
		values[key] = value
	}
	return config.ValidateTuning(values)
}

func runPerformanceSetup(out io.Writer, cfgDir, name string) error {
	fmt.Fprintln(out, "\n── Performance Profile ────────────────────────────────")
	path := filepath.Join(cfgDir, filepath.Base(config.DefaultTuningPath))
	values, err := config.ReadTuning(path)
	if err != nil {
		return fmt.Errorf("read runtime configuration: %w", err)
	}
	if err := applyPerformanceProfile(values, name); err != nil {
		return err
	}
	if err := config.WriteTuning(path, values); err != nil {
		return fmt.Errorf("write runtime configuration: %w", err)
	}
	fmt.Fprintf(out, "  ✓ Applied %s profile (provider-safe readahead workers=1)\n", name)
	return nil
}

func persistPlanSwitches(cfgDir string, httpSelected, reactiveSelected bool) error {
	path := filepath.Join(cfgDir, filepath.Base(config.DefaultTuningPath))
	values, err := config.ReadTuning(path)
	if err != nil {
		return fmt.Errorf("read runtime configuration: %w", err)
	}
	values["HARRBOR_HTTP_STREAM_ENABLED"] = strconv.FormatBool(httpSelected)
	if !reactiveSelected {
		values["HARRBOR_REACTIVE_ENABLED"] = "false"
		values["HARRBOR_REACTIVE_COMMIT_MODE"] = "off"
		values["HARRBOR_REACTIVE_PROMOTION_ENABLED"] = "false"
	}
	if err := config.WriteTuning(path, values); err != nil {
		return fmt.Errorf("write reviewed plan switches: %w", err)
	}
	return nil
}

func runWrapperRepairSetup(out io.Writer, cfgDir string, enabled bool) error {
	path := filepath.Join(cfgDir, filepath.Base(config.DefaultTuningPath))
	values, err := config.ReadTuning(path)
	if err != nil {
		return fmt.Errorf("read runtime configuration: %w", err)
	}
	values["HARRBOR_STRM_INSTALLER_ENABLED"] = strconv.FormatBool(enabled)
	if err := config.WriteTuning(path, values); err != nil {
		return fmt.Errorf("write runtime configuration: %w", err)
	}
	fmt.Fprintf(out, "  ✓ Persistent ffprobe wrapper repair enabled=%t\n", enabled)
	return nil
}
