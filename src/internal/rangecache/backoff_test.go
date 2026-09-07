package rangecache

import "testing"

// Pure-function test for the backoff schedule introduced by the
// readaheadGoroutine bugfix (2026-07-01): this is deterministic arithmetic,
// not a simulation of network/timing behavior, so it's exercised directly
// rather than via a mocked ChunkSource + goroutine timing harness.
func TestReadaheadPrefetchBackoff(t *testing.T) {
	cases := []struct {
		fails int
		want  string
	}{
		{0, "50ms"}, // clamped to 1
		{1, "50ms"},
		{2, "100ms"},
		{3, "200ms"},
		{4, "400ms"},
		{8, "6.4s"},
		{9, "10s"}, // would be 12.8s uncapped; must clamp to the ceiling
		{20, "10s"},
		{1000, "10s"}, // must never panic on shift overflow
	}
	for _, c := range cases {
		got := readaheadPrefetchBackoff(c.fails)
		if got.String() != c.want {
			t.Errorf("readaheadPrefetchBackoff(%d) = %s, want %s", c.fails, got, c.want)
		}
	}
}

// Monotonic: the schedule must never make the wait shorter as failures
// accumulate, or a persistently-dead chunk could oscillate instead of
// converging to the ceiling.
func TestReadaheadPrefetchBackoffMonotonic(t *testing.T) {
	prev := readaheadPrefetchBackoff(1)
	for i := 2; i <= 30; i++ {
		cur := readaheadPrefetchBackoff(i)
		if cur < prev {
			t.Fatalf("backoff decreased at fails=%d: %s -> %s", i, prev, cur)
		}
		prev = cur
	}
}
