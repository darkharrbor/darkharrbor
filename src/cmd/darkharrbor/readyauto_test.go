package main

import (
	"testing"
	"time"
)

func strPtr(s string) *string { return &s }

func mkFileListJSON(n int) string {
	s := "["
	for i := 0; i < n; i++ {
		if i > 0 {
			s += ","
		}
		s += `{"name":"episode.mkv"}`
	}
	return s + "]"
}

// Pure-function test for the 2026-07-01 bugfix: a season pack must get
// scaled extra grace before ready-auto-remove touches it, or a large grab
// gets yanked mid-import (the real, live-observed failure: the arr's
// download vanishes from the client queue, the arr sends a DELETE for it,
// and DH immediately destroys the fully-fetched files with zero episodes
// actually imported).
func TestReadyAutoRemoveEffectiveTTL(t *testing.T) {
	base := 60 * time.Second

	cases := []struct {
		name     string
		fileList *string
		want     time.Duration
	}{
		{"nil file list -- fails safe to base", nil, base},
		{"empty file list -- fails safe to base", strPtr(""), base},
		{"unparseable JSON -- fails safe to base", strPtr("not json"), base},
		{"single file -- no extra grace", strPtr(`[{"name":"a.mkv"}]`), base},
		{"empty array -- no extra grace", strPtr(`[]`), base},
		{
			"25-episode season pack -- scaled grace (24 * 5s = 120s)",
			strPtr(mkFileListJSON(25)),
			base + 120*time.Second,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := readyAutoRemoveEffectiveTTL(base, c.fileList)
			if got != c.want {
				t.Errorf("readyAutoRemoveEffectiveTTL(%s, %v) = %s, want %s", base, c.fileList, got, c.want)
			}
		})
	}
}

func TestReadyAutoRemoveEffectiveTTLCapsGraceForVeryLargePacks(t *testing.T) {
	base := 60 * time.Second
	// 1000 "episodes" (e.g. a whole-series grab) would be 999*5s = 4995s
	// (~83min) uncapped -- must clamp to the 20-minute ceiling.
	got := readyAutoRemoveEffectiveTTL(base, strPtr(mkFileListJSON(1000)))
	want := base + 20*time.Minute
	if got != want {
		t.Errorf("effective TTL for a 1000-file pack = %s, want %s (capped)", got, want)
	}
}
