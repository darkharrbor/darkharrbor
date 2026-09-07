package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func TestEnqueueTorrentWithoutDebridProviderFailsClosed(t *testing.T) {
	server := newTorrentBlacklistTestServer(newTorrentBlacklistTestStore(t))
	item, duplicate, err := server.enqueueSubmission(context.Background(), SubmissionRequest{
		SourceType: store.SourceTypeTorrent,
		ClientKind: store.ClientKindQBit,
		SourceURI:  "magnet:?xt=urn:btih:7777777777777777777777777777777777777777",
		InfoHash:   "7777777777777777777777777777777777777777",
	})
	if err == nil || !strings.Contains(err.Error(), "no debrid provider") {
		t.Fatalf("enqueue error = %v, want providerless rejection", err)
	}
	if item != nil || duplicate {
		t.Fatalf("rejected enqueue returned item=%v duplicate=%v", item, duplicate)
	}
}

func TestEnqueueSubmissionFallsBackWhenPrimaryLaneGateRejects(t *testing.T) {
	ctx := context.Background()
	st := newTorrentBlacklistTestStore(t)

	primary := &altFakeProvider{
		name:           "torbox",
		checkCachedErr: errors.New("primary capacity unavailable"),
	}
	secondary := &altFakeProvider{
		name:              "realdebrid",
		checkCachedResult: &provider.CheckCachedResult{Cached: true},
		submitResp:        &provider.CreateTaskResponse{RemoteID: "secondary-task"},
	}
	primaryLane := newRepairLane(t, "torbox", primary)
	server := newTorrentBlacklistTestServer(st)
	server.gate = primaryLane.Gate
	server.lanes = []ProviderLane{
		primaryLane,
		newRepairLane(t, "realdebrid", secondary),
	}
	server.shutdownCtx = context.Background()
	server.activeGoroutines = make(map[string]struct{})

	const hash = "6666666666666666666666666666666666666666"
	item, duplicate, err := server.enqueueSubmission(ctx, SubmissionRequest{
		SourceType:  store.SourceTypeTorrent,
		ClientKind:  store.ClientKindQBit,
		Category:    "tv",
		DisplayName: "Fallback.Release.S01",
		SourceURI:   "magnet:?xt=urn:btih:" + hash,
		InfoHash:    hash,
	})
	if err != nil {
		t.Fatalf("enqueueSubmission: %v", err)
	}
	if duplicate || item == nil {
		t.Fatalf("enqueue result item=%v duplicate=%v, want new item", item, duplicate)
	}
	server.wg.Wait()

	fresh, err := st.GetItemByID(ctx, item.ID)
	if err != nil {
		t.Fatalf("GetItemByID: %v", err)
	}
	if fresh.Provider == nil || *fresh.Provider != "realdebrid" {
		t.Fatalf("provider = %v, want realdebrid fallback", fresh.Provider)
	}
	if primary.checkCachedCalls != 1 || primary.submitCalls != 0 {
		t.Fatalf("primary calls check=%d submit=%d, want 1/0", primary.checkCachedCalls, primary.submitCalls)
	}
	if secondary.checkCachedCalls != 1 || secondary.submitCalls != 1 {
		t.Fatalf("secondary calls check=%d submit=%d, want 1/1", secondary.checkCachedCalls, secondary.submitCalls)
	}
}
