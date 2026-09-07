package contentproof_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/contentproof"
)

func TestResolveLaneCandidatesNormalizesAndKeepsAllExactMatches(t *testing.T) {
	first, err := contentproof.NewLaneCandidate(contentproof.LaneTorrent, "item-1", "file-1", "rep-1", "Example.Release_2026", 1234)
	if err != nil {
		t.Fatal(err)
	}
	second, err := contentproof.NewLaneCandidate(contentproof.LaneHTTP, "item-2", "file-2", "rep-2", "example release 2026", 1234)
	if err != nil {
		t.Fatal(err)
	}
	wrongSize, err := contentproof.NewLaneCandidate(contentproof.LaneNNTP, "item-3", "file-3", "rep-3", "example release 2026", 4321)
	if err != nil {
		t.Fatal(err)
	}

	matches, err := contentproof.ResolveLaneCandidates(context.Background(), " EXAMPLE-release.2026 ", 1234, []contentproof.LaneCandidate{first, wrongSize, second})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 || matches[0].ItemID != "item-1" || matches[1].ItemID != "item-2" {
		t.Fatalf("matches = %+v", matches)
	}
}

func TestLaneCandidateRejectsMalformedAndURLShapedInput(t *testing.T) {
	urlShaped := "https" + "://origin.invalid/x"
	tests := []struct {
		name        string
		lane        contentproof.Lane
		itemID      string
		releaseName string
		size        int64
	}{
		{name: "unknown lane", lane: "other", itemID: "item", releaseName: "release", size: 1},
		{name: "URL ID", lane: contentproof.LaneTorrent, itemID: urlShaped, releaseName: "release", size: 1},
		{name: "URL release", lane: contentproof.LaneTorrent, itemID: "item", releaseName: urlShaped, size: 1},
		{name: "empty normalized release", lane: contentproof.LaneTorrent, itemID: "item", releaseName: "---", size: 1},
		{name: "non-positive size", lane: contentproof.LaneTorrent, itemID: "item", releaseName: "release", size: 0},
		{name: "oversized release", lane: contentproof.LaneTorrent, itemID: "item", releaseName: strings.Repeat("a", contentproof.MaxReleaseNameBytes+1), size: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := contentproof.NewLaneCandidate(tc.lane, tc.itemID, "file", "representation", tc.releaseName, tc.size); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestResolveLaneCandidatesCancellationAndBounds(t *testing.T) {
	candidate, err := contentproof.NewLaneCandidate(contentproof.LaneTorrent, "item", "file", "representation", "release", 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := contentproof.ResolveLaneCandidates(ctx, "release", 1, []contentproof.LaneCandidate{candidate}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}

	tooMany := make([]contentproof.LaneCandidate, contentproof.MaxLaneCandidates+1)
	for i := range tooMany {
		tooMany[i] = candidate
	}
	if _, err := contentproof.ResolveLaneCandidates(context.Background(), "release", 1, tooMany); err == nil {
		t.Fatal("expected candidate bound rejection")
	}
}

func TestResolveLaneCandidatesRejectsContradictoryCandidate(t *testing.T) {
	candidate, err := contentproof.NewLaneCandidate(contentproof.LaneTorrent, "item", "file", "representation", "release", 1)
	if err != nil {
		t.Fatal(err)
	}
	candidate.ReleaseKey = strings.Repeat("z", 64)
	if _, err := contentproof.ResolveLaneCandidates(context.Background(), "release", 1, []contentproof.LaneCandidate{candidate}); err == nil {
		t.Fatal("expected malformed candidate rejection")
	}
}

func FuzzReleaseKey(f *testing.F) {
	for _, seed := range []struct {
		name string
		size int64
	}{
		{"Example.Release_2026", 1},
		{" example release 2026 ", 1234},
		{"---", 1},
		{"https" + "://origin.invalid/file", 1},
		{strings.Repeat("a", contentproof.MaxReleaseNameBytes+1), 1},
	} {
		f.Add(seed.name, seed.size)
	}
	f.Fuzz(func(t *testing.T, name string, size int64) {
		key, err := contentproof.ReleaseKey(name, size)
		if err != nil {
			return
		}
		again, err := contentproof.ReleaseKey(name, size)
		if err != nil || key != again || len(key) != 64 {
			t.Fatalf("unstable or non-opaque key: %q %q %v", key, again, err)
		}
	})
}
