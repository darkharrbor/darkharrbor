package main

// ts3_2_submitbudget_test.go — TS-3.2: focused coverage for the uncached
// torrent submit-progress budget (T4). Extends stall detection to the
// acceptance window: an uncached torrent with zero download progress within
// the configured budget is blacklisted, distinct from and stricter than
// COR-11's own never-blacklisting stall handling.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func TestTorrentSubmitBudgetExpiredMeasuresAgeFromCreatedAt(t *testing.T) {
	createdAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	budget := 15 * time.Minute

	age, expired := torrentSubmitBudgetExpired(createdAt, createdAt, 0, budget)
	if age != 0 || expired {
		t.Fatalf("t=0 = (%v, %t), want fresh non-expired observation", age, expired)
	}

	age, expired = torrentSubmitBudgetExpired(createdAt, createdAt.Add(14*time.Minute), 0, budget)
	if age != 14*time.Minute || expired {
		t.Fatalf("14-minute age = (%v, %t), want non-expired", age, expired)
	}

	age, expired = torrentSubmitBudgetExpired(createdAt, createdAt.Add(15*time.Minute), 0, budget)
	if age != 15*time.Minute || !expired {
		t.Fatalf("15-minute age = (%v, %t), want expired", age, expired)
	}
}

func TestTorrentSubmitBudgetExpiredAbstainsOnAnyProgress(t *testing.T) {
	createdAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	budget := 15 * time.Minute

	// Even a tiny amount of real progress permanently disqualifies this
	// check for the rest of the item's lifetime in this branch — a source
	// that started and later stalls mid-download is COR-11's job, not this
	// one's.
	age, expired := torrentSubmitBudgetExpired(createdAt, createdAt.Add(2*time.Hour), 0.001, budget)
	if age != 0 || expired {
		t.Fatalf("any-progress = (%v, %t), want never expired regardless of item age", age, expired)
	}
}

func TestTorrentSubmitBudgetExpiredNeverNegativeAge(t *testing.T) {
	createdAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	// A clock supplied earlier than createdAt (clock skew / malformed
	// input) must never produce a negative age or a spurious expiry.
	age, expired := torrentSubmitBudgetExpired(createdAt, createdAt.Add(-1*time.Hour), 0, 15*time.Minute)
	if age != 0 || expired {
		t.Fatalf("pre-creation clock = (%v, %t), want zero age, never expired", age, expired)
	}
}

// TestHandleTerminalTorrentFailureAcceptsSubmitBudgetExceededFailureCode
// proves the exact reuse shape resolveItem's TS-3.2 branch depends on:
// handleTerminalTorrentFailure (the same blacklist+fail+notify owner
// TerminalDeadSource/DeadSentinel already use) recognizes a status forced to
// TorrentOutcomeTerminalDeadSource carrying the new
// TorrentFailureSubmitBudgetExceeded code, and reports that code (not the
// generic default) in the persisted failure message.
func TestHandleTerminalTorrentFailureAcceptsSubmitBudgetExceededFailureCode(t *testing.T) {
	item := &store.Item{ID: "torrent-item-budget", SourceType: store.SourceTypeTorrent}
	status := &provider.TaskStatus{
		Outcome:     provider.TorrentOutcomeTerminalDeadSource,
		FailureCode: provider.TorrentFailureSubmitBudgetExceeded,
		Hash:        "submitbudgethash",
	}
	fakeStore := &fakeTerminalFailureStore{}
	refresh := &fakeRefreshSignal{}

	handled := handleTerminalTorrentFailure(
		context.Background(),
		terminalFailureTestLogger(),
		terminalFailureStoreAdapter{fakeStore},
		item,
		status,
		refresh,
	)

	if !handled {
		t.Fatal("expected submit-budget-exceeded status to be handled as terminal")
	}
	if fakeStore.blacklistCalls != 1 {
		t.Fatalf("blacklist calls = %d, want 1", fakeStore.blacklistCalls)
	}
	if fakeStore.updateCalls != 1 || fakeStore.updateState != store.StateFailed {
		t.Fatalf("state update = calls:%d state:%q, want one failed update", fakeStore.updateCalls, fakeStore.updateState)
	}
	if fakeStore.updateMessage != string(provider.TorrentFailureSubmitBudgetExceeded) {
		t.Fatalf("failure message = %q, want %q", fakeStore.updateMessage, provider.TorrentFailureSubmitBudgetExceeded)
	}
	if item.ErrorMessage == nil || *item.ErrorMessage != string(provider.TorrentFailureSubmitBudgetExceeded) {
		t.Fatalf("durable item failure = %v, want %q", item.ErrorMessage, provider.TorrentFailureSubmitBudgetExceeded)
	}
	if refresh.calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refresh.calls)
	}
}

type ts32Remover struct {
	called int
	err    error
}

func (r *ts32Remover) Name() string { return "test-provider" }
func (r *ts32Remover) Remove(ctx context.Context, _ *store.Item) error {
	r.called++
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.err
}

func TestCleanupSubmitBudgetRemoteIsBoundedAndFailClosed(t *testing.T) {
	tests := []struct {
		name string
		ctx  func() context.Context
		err  error
		want bool
	}{
		{"success", context.Background, nil, true},
		{"provider failure", context.Background, errors.New("provider failure"), false},
		{"cancelled", func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			remover := &ts32Remover{err: tt.err}
			if got := cleanupSubmitBudgetRemote(tt.ctx(), terminalFailureTestLogger(), remover.Name(), remover.Remove, &store.Item{}); got != tt.want {
				t.Fatalf("cleanup = %t, want %t", got, tt.want)
			}
			if remover.called != 1 {
				t.Fatalf("remove calls = %d, want 1", remover.called)
			}
		})
	}
}

func TestTriggerSubmitBudgetReSearchUsesExistingArrOwner(t *testing.T) {
	var command string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/movie":
			_, _ = w.Write([]byte("[{\"id\":42}]"))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/command":
			buf := make([]byte, 256)
			n, _ := r.Body.Read(buf)
			command = string(buf[:n])
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	item := &store.Item{Metadata: store.SubmissionMetadata{ProviderIdentity: &store.ProviderIdentity{
		Kind: "movie",
		IDs:  store.ProviderIDs{TMDB: "1234"},
	}}}
	arrs := []config.ArrTarget{{Name: "radarr", BaseURL: srv.URL, APIKey: "test-key"}}
	if !triggerSubmitBudgetReSearch(context.Background(), terminalFailureTestLogger(), &fakeTerminalFailureStore{}, item, arrs) {
		t.Fatal("expected exact Arr owner to receive a targeted re-search")
	}
	if !strings.Contains(command, "\"name\":\"MoviesSearch\"") || !strings.Contains(command, "\"movieIds\":[42]") {
		t.Fatalf("command = %q, want MoviesSearch for movie 42", command)
	}
}

func TestTriggerSubmitBudgetReSearchAbstainsWithoutIdentity(t *testing.T) {
	if triggerSubmitBudgetReSearch(context.Background(), terminalFailureTestLogger(), &fakeTerminalFailureStore{}, &store.Item{}, nil) {
		t.Fatal("expected missing identity to abstain")
	}
}
