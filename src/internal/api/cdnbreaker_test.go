package api

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
)

func TestCDNBreakerOpensOnDistinctHostsAndRecovers(t *testing.T) {
	now := time.Date(2026, 10, 17, 15, 21, 0, 0, time.UTC)
	b := newCDNBreaker(func() time.Time { return now })

	// Repeated failures on ONE host are PERF-4's per-URL job, not a
	// provider-wide outage.
	for i := 0; i < 5; i++ {
		if b.NoteConnectFailure("https://nexus-212.snam.tb-cdn.io/dld/x") {
			t.Fatal("one host must not open the breaker")
		}
	}
	if b.Open() {
		t.Fatal("open after single-host failures")
	}
	b.NoteConnectFailure("https://nexus-191.snam.tb-cdn.io/dld/y")
	if !b.NoteConnectFailure("https://store-7.tb-cdn.io/dld/z") {
		t.Fatal("third distinct host should open the breaker")
	}
	if !b.Open() {
		t.Fatal("breaker should be open")
	}
	now = now.Add(cdnBreakerOpenFor + time.Second)
	if b.Open() {
		t.Fatal("breaker should let traffic through after the pause")
	}
	b.NoteSuccess()
	if b.NoteConnectFailure("https://a.tb-cdn.io/") || b.Open() {
		t.Fatal("a success must reset history")
	}
}

func TestCDNBreakerForgetsFailuresOutsideWindow(t *testing.T) {
	now := time.Date(2026, 10, 17, 15, 0, 0, 0, time.UTC)
	b := newCDNBreaker(func() time.Time { return now })
	b.NoteConnectFailure("https://a.tb-cdn.io/")
	b.NoteConnectFailure("https://b.tb-cdn.io/")
	now = now.Add(cdnBreakerWindow + time.Second)
	if b.NoteConnectFailure("https://c.tb-cdn.io/") || b.Open() {
		t.Fatal("stale failures must not count toward opening")
	}
}

func TestCDNBreakerNilSafe(t *testing.T) {
	var b *cdnBreaker
	if b.Open() || b.NoteConnectFailure("https://a/") {
		t.Fatal("nil breaker must be inert")
	}
	b.NoteSuccess()
}

func TestStreamPriorityRanksProbesBelowPlayback(t *testing.T) {
	if p := streamPriority(httptest.NewRequest("GET", "/stream/i/f?tok=x", nil)); p != accountgov.PriorityPlayback {
		t.Fatalf("playback priority = %v", p)
	}
	if p := streamPriority(httptest.NewRequest("GET", "/stream/i/f?tok=x&probe=1", nil)); p != accountgov.PriorityGrab {
		t.Fatalf("probe priority = %v", p)
	}
	if accountgov.PriorityGrab <= accountgov.PriorityPlayback {
		t.Fatal("probe priority must rank below playback")
	}
}
