package main

import (
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// applyCgroupMemoryLimit sets the Go soft memory limit to 90% of the
// container's cgroup v2 memory.max when the operator has not set GOMEMLIMIT
// and a finite limit exists, so the GC works harder before the kernel OOM
// killer is reached. It returns the limit applied (0 when none).
func applyCgroupMemoryLimit() int64 {
	if strings.TrimSpace(os.Getenv("GOMEMLIMIT")) != "" {
		return 0
	}
	raw, err := os.ReadFile("/sys/fs/cgroup/memory.max")
	if err != nil {
		return 0
	}
	limit, ok := parseCgroupMemoryMax(string(raw))
	if !ok {
		return 0
	}
	soft := limit / 10 * 9
	debug.SetMemoryLimit(soft)
	return soft
}

// parseCgroupMemoryMax parses a cgroup v2 memory.max value; "max" (no limit)
// and anything unparseable or non-positive report ok=false.
func parseCgroupMemoryMax(v string) (int64, bool) {
	v = strings.TrimSpace(v)
	if v == "" || v == "max" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
