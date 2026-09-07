package api

import (
	"math"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/mediatruth"
	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func TestPlaybackProposalTargetDenominatorOrder(t *testing.T) {
	files, err := httpstream.MarshalFiles([]httpstream.PersistedFile{{FileID: "file", Name: "movie.mkv", Size: 200}})
	if err != nil {
		t.Fatal(err)
	}
	item := &store.Item{ID: "item", FileList: &files, TotalSize: 300}
	item.Metadata.MediaFactsFileID = "file"
	item.Metadata.MediaFacts = &mediatruth.Facts{DurationMS: 1000, BitRate: 8000}

	target, ok := playbackProposalTarget(item, "file", 123)
	if !ok || target.Total != 123 || target.Kind != playbackcoverage.ExtentDeclaredBytes {
		t.Fatalf("declared target=%+v ok=%v", target, ok)
	}
	target, ok = playbackProposalTarget(item, "file", 0)
	if !ok || target.Total != 200 || target.Kind != playbackcoverage.ExtentDeclaredBytes {
		t.Fatalf("file-list target=%+v ok=%v", target, ok)
	}

	item.FileList = nil
	item.TotalSize = 0
	target, ok = playbackProposalTarget(item, "file", 0)
	if !ok || target.Total != 1000 || target.Kind != playbackcoverage.ExtentMediaTruth {
		t.Fatalf("media-truth target=%+v ok=%v", target, ok)
	}
}

func TestPlaybackProposalTargetAbstainsOnAmbiguity(t *testing.T) {
	item := &store.Item{ID: "item"}
	cases := []struct {
		name   string
		item   *store.Item
		fileID string
	}{
		{"nil item", nil, "file"},
		{"missing file", item, ""},
		{"no extent", item, "file"},
	}
	for _, test := range cases {
		if target, ok := playbackProposalTarget(test.item, test.fileID, 0); ok {
			t.Fatalf("%s produced target %+v", test.name, target)
		}
	}

	item.Metadata.MediaFactsFileID = "other"
	item.Metadata.MediaFacts = &mediatruth.Facts{DurationMS: 1000, BitRate: 8000}
	if target, ok := playbackProposalTarget(item, "file", 0); ok {
		t.Fatalf("mismatched media truth produced target %+v", target)
	}
	item.Metadata.MediaFactsFileID = "file"
	item.Metadata.MediaFacts = &mediatruth.Facts{DurationMS: math.MaxInt64, BitRate: 2}
	if target, ok := playbackProposalTarget(item, "file", 0); ok {
		t.Fatalf("overflowing media truth produced target %+v", target)
	}
}
