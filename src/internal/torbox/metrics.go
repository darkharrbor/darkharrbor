package torbox

import (
	"sync/atomic"
	"time"
)

// Metrics tracks TorBox API call counts and CDN/passthrough egress bytes for
// B-O4: metering at do() + CDN byte paths. Counters are all-time atomics;
// callers snapshot and diff for windowed summaries (e.g. hourly log line).
type Metrics struct {
	// API call counters by op class (incremented in do()).
	CallsCreate  atomic.Int64
	CallsQuery   atomic.Int64
	CallsControl atomic.Int64
	CallsTotal   atomic.Int64

	// CDN/passthrough byte counters (incremented by Server stream paths).
	BytesCDN         atomic.Int64 // bytes relayed from TorBox CDN to client
	BytesPassthrough atomic.Int64 // bytes from rangecache/disk to client

	// RangeProbe call counter (incremented by probe.Prober).
	ProbeRangeCount atomic.Int64

	startedAt time.Time
}

func newMetrics() *Metrics {
	return &Metrics{startedAt: time.Now()}
}

// AddCDNBytes credits n bytes to the CDN egress counter (satisfies api.EgressMeter).
func (m *Metrics) AddCDNBytes(n int64) { m.BytesCDN.Add(n) }

// AddPassthroughBytes credits n bytes to the passthrough egress counter (satisfies api.EgressMeter).
func (m *Metrics) AddPassthroughBytes(n int64) { m.BytesPassthrough.Add(n) }

// Snapshot returns a point-in-time copy of all counters.
func (m *Metrics) Snapshot() MetricsSnapshot {
	return MetricsSnapshot{
		CallsCreate:      m.CallsCreate.Load(),
		CallsQuery:       m.CallsQuery.Load(),
		CallsControl:     m.CallsControl.Load(),
		CallsTotal:       m.CallsTotal.Load(),
		BytesCDN:         m.BytesCDN.Load(),
		BytesPassthrough: m.BytesPassthrough.Load(),
		ProbeRangeCount:  m.ProbeRangeCount.Load(),
		UptimeSeconds:    int64(time.Since(m.startedAt).Seconds()),
	}
}

// SnapshotMap returns a map[string]any suitable for JSON embedding in /healthz.
// Satisfies the anonymous snapshotter interface in api.handleHealth.
func (m *Metrics) SnapshotMap() map[string]any {
	s := m.Snapshot()
	return map[string]any{
		"calls_create":      s.CallsCreate,
		"calls_query":       s.CallsQuery,
		"calls_control":     s.CallsControl,
		"calls_total":       s.CallsTotal,
		"bytes_cdn":         s.BytesCDN,
		"bytes_passthrough": s.BytesPassthrough,
		"probe_range_count": s.ProbeRangeCount,
		"uptime_seconds":    s.UptimeSeconds,
	}
}

// MetricsSnapshot is a copyable point-in-time view (no atomics).
type MetricsSnapshot struct {
	CallsCreate      int64 `json:"calls_create"`
	CallsQuery       int64 `json:"calls_query"`
	CallsControl     int64 `json:"calls_control"`
	CallsTotal       int64 `json:"calls_total"`
	BytesCDN         int64 `json:"bytes_cdn"`
	BytesPassthrough int64 `json:"bytes_passthrough"`
	ProbeRangeCount  int64 `json:"probe_range_count"`
	UptimeSeconds    int64 `json:"uptime_seconds"`
}
