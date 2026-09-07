package api

import (
	"context"
	"math"

	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func playbackProposalTarget(item *store.Item, fileID string, declared int64) (playbackcoverage.Target, bool) {
	if item == nil || item.ID == "" || fileID == "" {
		return playbackcoverage.Target{}, false
	}
	kind := playbackcoverage.ExtentDeclaredBytes
	total := declared
	if total <= 0 {
		entries := parseFileList(item.FileList)
		if entry, ok := fileListEntryForID(entries, fileID); ok {
			total = entry.Size
		}
		if total <= 0 && len(entries) == 1 {
			total = item.TotalSize
		}
	}
	if total <= 0 && item.Metadata.MediaFacts != nil && item.Metadata.MediaFactsFileID == fileID {
		facts := item.Metadata.MediaFacts
		if facts.DurationMS > 0 && facts.BitRate > 0 && facts.DurationMS <= math.MaxInt64/facts.BitRate {
			total = facts.DurationMS * facts.BitRate / 8000
			kind = playbackcoverage.ExtentMediaTruth
		}
	}
	if total <= 0 {
		return playbackcoverage.Target{}, false
	}
	return playbackcoverage.Target{ItemID: item.ID, FileID: fileID, Kind: kind, Total: total}, true
}

func withPlaybackProposalTarget(ctx context.Context, item *store.Item, fileID string, declared int64) context.Context {
	if target, ok := playbackProposalTarget(item, fileID, declared); ok {
		return playbackcoverage.WithTarget(ctx, target)
	}
	return ctx
}
