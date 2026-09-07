package api

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// stubPersister is a fault-injection stub for itemPersister.
// UpdateItem fails for the first `failCount` calls, then succeeds.
// UpdateItemState records whether it was called and with which state.
type stubPersister struct {
	failCount        int
	calls            int
	updateStateCalls []store.ItemState
	updateStateErr   error
}

func (s *stubPersister) UpdateItem(_ context.Context, _ *store.Item) error {
	s.calls++
	if s.calls <= s.failCount {
		return errors.New("injected db error")
	}
	return nil
}

func (s *stubPersister) UpdateItemState(_ context.Context, _ *store.Item, next store.ItemState, _ string) error {
	s.updateStateCalls = append(s.updateStateCalls, next)
	return s.updateStateErr
}

var discardLog = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 10}))

func testResp() *provider.CreateTaskResponse {
	return &provider.CreateTaskResponse{
		RemoteID:    "remote-123",
		QueuedID:    "queued-456",
		RemoteHash:  "abc123hash",
		DisplayName: "Test.Show.S01E01",
	}
}

// TestPersistSubmitResult_HappyPath: first UpdateItem call succeeds → returns true,
// no StateFailed transition, item fields populated.
func TestPersistSubmitResult_HappyPath(t *testing.T) {
	st := &stubPersister{}
	item := &store.Item{ID: "item-1"}
	resp := testResp()

	ok := persistSubmitResult(context.Background(), discardLog, st, item, resp, true)
	if !ok {
		t.Fatal("expected true on success")
	}
	if st.calls != 1 {
		t.Fatalf("UpdateItem calls = %d, want 1", st.calls)
	}
	if len(st.updateStateCalls) != 0 {
		t.Fatalf("UpdateItemState should not be called on success, got %v", st.updateStateCalls)
	}
	if item.RemoteID == nil || *item.RemoteID != "remote-123" {
		t.Fatalf("item.RemoteID = %v, want remote-123", item.RemoteID)
	}
	if !item.Cached {
		t.Fatal("item.Cached should be true")
	}
}

// TestPersistSubmitResult_RetrySucceeds: first N-1 calls fail, last one succeeds
// → returns true, no StateFailed.
func TestPersistSubmitResult_RetrySucceeds(t *testing.T) {
	// submitPersistAttempts == 3; fail 2, succeed on 3rd
	st := &stubPersister{failCount: submitPersistAttempts - 1}
	item := &store.Item{ID: "item-2"}

	ok := persistSubmitResult(context.Background(), discardLog, st, item, testResp(), false)
	if !ok {
		t.Fatal("expected true when final retry succeeds")
	}
	if st.calls != submitPersistAttempts {
		t.Fatalf("UpdateItem calls = %d, want %d", st.calls, submitPersistAttempts)
	}
	if len(st.updateStateCalls) != 0 {
		t.Fatalf("UpdateItemState should not be called, got %v", st.updateStateCalls)
	}
}

// TestPersistSubmitResult_AllRetriesExhausted: all submitPersistAttempts fail
// → returns false, StateFailed transition called, leaked IDs appear in error message.
func TestPersistSubmitResult_AllRetriesExhausted(t *testing.T) {
	st := &stubPersister{failCount: submitPersistAttempts} // fail every attempt
	item := &store.Item{ID: "item-3"}
	resp := testResp()

	ok := persistSubmitResult(context.Background(), discardLog, st, item, resp, false)
	if ok {
		t.Fatal("expected false when all retries exhausted")
	}
	if st.calls != submitPersistAttempts {
		t.Fatalf("UpdateItem calls = %d, want %d", st.calls, submitPersistAttempts)
	}
	if len(st.updateStateCalls) != 1 || st.updateStateCalls[0] != store.StateFailed {
		t.Fatalf("UpdateItemState calls = %v, want [StateFailed]", st.updateStateCalls)
	}
	// Error message must contain the leaked IDs so ops can recover
	if item.ErrorMessage == nil {
		t.Fatal("item.ErrorMessage must be set on failure")
	}
	if !strings.Contains(*item.ErrorMessage, "remote-123") {
		t.Fatalf("error message missing remote_id: %q", *item.ErrorMessage)
	}
	if !strings.Contains(*item.ErrorMessage, "queued-456") {
		t.Fatalf("error message missing queued_id: %q", *item.ErrorMessage)
	}
}

// TestPersistSubmitResult_ContextCancelled: context cancelled mid-retry
// → loop terminates early, StateFailed transition called.
func TestPersistSubmitResult_ContextCancelled(t *testing.T) {
	st := &stubPersister{failCount: submitPersistAttempts} // always fail
	item := &store.Item{ID: "item-4"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	ok := persistSubmitResult(ctx, discardLog, st, item, testResp(), false)
	if ok {
		t.Fatal("expected false on cancelled context")
	}
	if len(st.updateStateCalls) != 1 || st.updateStateCalls[0] != store.StateFailed {
		t.Fatalf("UpdateItemState calls = %v, want [StateFailed]", st.updateStateCalls)
	}
}

// TestPersistSubmitResult_NoClobberExistingInfoHash: InfoHash already set on item
// should not be overwritten by resp.RemoteHash.
func TestPersistSubmitResult_NoClobberExistingInfoHash(t *testing.T) {
	st := &stubPersister{}
	existing := "existinghash"
	item := &store.Item{ID: "item-5", InfoHash: &existing}
	resp := testResp() // has RemoteHash "abc123hash"

	persistSubmitResult(context.Background(), discardLog, st, item, resp, false)

	if item.InfoHash == nil || *item.InfoHash != existing {
		t.Fatalf("InfoHash clobbered: got %v, want %q", item.InfoHash, existing)
	}
}
