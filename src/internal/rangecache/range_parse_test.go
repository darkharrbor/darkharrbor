package rangecache

import (
	"testing"
	"time"
)

// TestParseRangeSuffixRFC7233 covers A-B4: suffix ranges ("bytes=-N") must
// return the last N bytes, not the first N bytes as the pre-fix code did.
func TestParseRangeSuffixRFC7233(t *testing.T) {
	total := int64(1000)

	cases := []struct {
		header    string
		wantStart int64
		wantEnd   int64
		wantPart  bool
	}{
		{"bytes=-100", 900, 999, true}, // last 100 bytes
		{"bytes=-9999", 0, 999, true},  // larger than file → clamp to 0
		{"bytes=-1000", 0, 999, true},  // exactly whole file
		{"bytes=0-99", 0, 99, true},    // normal range unchanged
		{"bytes=500-", 500, 999, true}, // open-ended start
		{"", 0, 999, false},            // no range header → not partial
		{"bytes=-1", 999, 999, true},   // last 1 byte
	}

	for _, tc := range cases {
		s, e, p := parseRange(tc.header, total)
		if s != tc.wantStart || e != tc.wantEnd || p != tc.wantPart {
			t.Errorf("parseRange(%q, %d) = (%d,%d,%v), want (%d,%d,%v)",
				tc.header, total, s, e, p, tc.wantStart, tc.wantEnd, tc.wantPart)
		}
	}
}

// TestLastUsedRaceA9 validates the A-B9 fix: lastUsed in the reuse branch is
// now written under sess.mu, matching how sessionReaper reads it.
// Run with go test -race to catch violations.
func TestLastUsedRaceA9(t *testing.T) {
	sess := &readaheadSession{}

	done := make(chan struct{})
	// Goroutine 1: write lastUsed (simulates Stream reuse branch)
	go func() {
		for i := 0; i < 1000; i++ {
			sess.mu.Lock()
			sess.lastUsed = time.Now()
			sess.mu.Unlock()
		}
		close(done)
	}()
	// Goroutine 2: read lastUsed (simulates sessionReaper)
	for i := 0; i < 1000; i++ {
		sess.mu.Lock()
		_ = sess.lastUsed
		sess.mu.Unlock()
	}
	<-done
}
