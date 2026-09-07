package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/api"
	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/store"
	"github.com/darkharrbor/darkharrbor/internal/suppress"
)

type fakeTerminalFailureStore struct {
	blacklistCalls int
	blacklistKey   string
	blacklistErr   error
	updateCalls    int
	updateState    store.ItemState
	updateMessage  string
	updateErr      error

	suppressCalls int
	suppressFP    string
	suppressLane  string
	suppressRsn   string
	suppressTTL   time.Duration
	suppressErr   error

	// grabIdentity is NS-6.2's ID-02 grab-time identity cache fallback
	// read side (store.GetGrabProviderIdentity). Keyed by the exact key
	// resolveDecayedItemIdentity computes (nntp.ContentKey(SourceURI)),
	// so tests can preload it the same way the real 30-day cache would
	// have been populated by a real grab.
	grabIdentity      map[string]*store.ProviderIdentity
	grabIdentityCalls int
	grabIdentityErr   error
}

func (f *fakeTerminalFailureStore) GetGrabProviderIdentity(_ context.Context, key string) (*store.ProviderIdentity, bool, error) {
	f.grabIdentityCalls++
	if f.grabIdentityErr != nil {
		return nil, false, f.grabIdentityErr
	}
	id, ok := f.grabIdentity[key]
	return id, ok, nil
}

func (f *fakeTerminalFailureStore) BlacklistImmediately(context.Context, string) (store.FailureRecordResult, error) {
	panic("use blacklistImmediately")
}

func (f *fakeTerminalFailureStore) blacklistImmediately(_ context.Context, key string) (store.FailureRecordResult, error) {
	f.blacklistCalls++
	f.blacklistKey = key
	if f.blacklistErr != nil {
		return store.FailureRecordResult{}, f.blacklistErr
	}
	return store.FailureRecordResult{FailCount: 1, Blacklisted: true}, nil
}

func (f *fakeTerminalFailureStore) UpdateItemState(_ context.Context, _ *store.Item, state store.ItemState, message string) error {
	f.updateCalls++
	f.updateState = state
	f.updateMessage = message
	return f.updateErr
}

func (f *fakeTerminalFailureStore) RecordSuppression(_ context.Context, fingerprint, lane, reason string, ttl time.Duration) error {
	f.suppressCalls++
	f.suppressFP = fingerprint
	f.suppressLane = lane
	f.suppressRsn = reason
	f.suppressTTL = ttl
	return f.suppressErr
}

type terminalFailureStoreAdapter struct {
	*fakeTerminalFailureStore
}

func (a terminalFailureStoreAdapter) BlacklistImmediately(ctx context.Context, key string) (store.FailureRecordResult, error) {
	return a.blacklistImmediately(ctx, key)
}

type fakeRefreshSignal struct {
	calls int
}

func (f *fakeRefreshSignal) Notify() {
	f.calls++
}

func terminalFailureTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestHandleAllProvidersDeadPersistsBlacklistFailureAndRefresh(t *testing.T) {
	source := "<nzb>sample</nzb>"
	item := &store.Item{SourceURI: &source}
	fakeStore := &fakeTerminalFailureStore{}
	refresh := &fakeRefreshSignal{}

	handled := handleAllProvidersDead(
		context.Background(),
		terminalFailureTestLogger(),
		terminalFailureStoreAdapter{fakeStore},
		item,
		refresh,
		"",
		2,
		2,
		time.Hour,
	)

	if !handled {
		t.Fatal("expected all-provider dead outcome to be handled")
	}
	if fakeStore.blacklistCalls != 1 {
		t.Fatalf("blacklist calls = %d, want 1", fakeStore.blacklistCalls)
	}
	if want := api.NZBBlacklistKey(source); fakeStore.blacklistKey != want {
		t.Fatalf("blacklist key = %q, want %q", fakeStore.blacklistKey, want)
	}
	// SF-03: the name+size+lane fingerprint suppression fires alongside the
	// existing exact-identity blacklist, not instead of it.
	if fakeStore.suppressCalls != 1 {
		t.Fatalf("suppression record calls = %d, want 1", fakeStore.suppressCalls)
	}
	if want := suppress.Fingerprint(item.DisplayName, item.TotalSize, suppress.LaneNZB); fakeStore.suppressFP != want {
		t.Fatalf("suppression fingerprint = %q, want %q", fakeStore.suppressFP, want)
	}
	if fakeStore.suppressLane != string(suppress.LaneNZB) {
		t.Fatalf("suppression lane = %q, want %q", fakeStore.suppressLane, suppress.LaneNZB)
	}
	if fakeStore.suppressRsn != string(suppress.ReasonDeadPost) {
		t.Fatalf("suppression reason = %q, want %q", fakeStore.suppressRsn, suppress.ReasonDeadPost)
	}
	if fakeStore.suppressTTL != time.Hour {
		t.Fatalf("suppression ttl = %v, want %v (the value passed into handleAllProvidersDead)", fakeStore.suppressTTL, time.Hour)
	}
	if fakeStore.updateCalls != 1 {
		t.Fatalf("state update calls = %d, want 1", fakeStore.updateCalls)
	}
	if fakeStore.updateState != store.StateFailed {
		t.Fatalf("state = %q, want %q", fakeStore.updateState, store.StateFailed)
	}
	if !strings.Contains(fakeStore.updateMessage, failureCodeNNTPDeadAllProviders) {
		t.Fatalf("failure message %q does not contain %q", fakeStore.updateMessage, failureCodeNNTPDeadAllProviders)
	}
	if refresh.calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refresh.calls)
	}
}

func TestHandleAllProvidersDeadLogsSafeIdentityWithoutRawNZB(t *testing.T) {
	rawNZB := "https://indexer.example/api?t=get&id=release-1&apikey=super-secret-token"
	item := &store.Item{
		ID:          "item-123",
		PublicID:    "public-456",
		SourceType:  store.SourceTypeNZB,
		DisplayName: "Safe.Release.S01E01",
		Category:    "tv",
		SourceURI:   &rawNZB,
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	handled := handleAllProvidersDead(
		context.Background(),
		logger,
		terminalFailureStoreAdapter{&fakeTerminalFailureStore{}},
		item,
		&fakeRefreshSignal{},
		"",
		2,
		2,
		time.Hour,
	)
	if !handled {
		t.Fatal("expected terminal outcome to be handled")
	}

	got := logs.String()
	for _, want := range []string{
		"event=source_failure_recorded",
		"event=source_blacklisted",
		"event=item_state_transition",
		"event=item_failure_persisted",
		"item_id=item-123",
		"public_id=public-456",
		"nzb_key=" + api.NZBBlacklistKey(rawNZB),
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("logs missing %q: %s", want, got)
		}
	}
	for _, forbidden := range []string{rawNZB, "super-secret-token"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("logs exposed sensitive NZB source %q: %s", forbidden, got)
		}
	}
}

func TestHandleAllProvidersDeadPreservesNonTerminalOutcomes(t *testing.T) {
	source := "<nzb>sample</nzb>"
	tests := []struct {
		name              string
		item              *store.Item
		probeProviderUsed string
		providersTried    int
		deadPostCount     int
	}{
		{name: "successful fallback", item: &store.Item{SourceURI: &source}, probeProviderUsed: "newshosting", providersTried: 2, deadPostCount: 1},
		{name: "dead plus transient", item: &store.Item{SourceURI: &source}, providersTried: 2, deadPostCount: 1},
		{name: "no providers attempted", item: &store.Item{SourceURI: &source}, providersTried: 0, deadPostCount: 0},
		{name: "missing source", item: &store.Item{}, providersTried: 1, deadPostCount: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeStore := &fakeTerminalFailureStore{}
			refresh := &fakeRefreshSignal{}
			handled := handleAllProvidersDead(
				context.Background(),
				terminalFailureTestLogger(),
				terminalFailureStoreAdapter{fakeStore},
				tt.item,
				refresh,
				tt.probeProviderUsed,
				tt.providersTried,
				tt.deadPostCount,
				time.Hour,
			)
			if handled {
				t.Fatal("unexpected terminal handling")
			}
			if fakeStore.blacklistCalls != 0 || fakeStore.updateCalls != 0 || refresh.calls != 0 {
				t.Fatalf("unexpected side effects: blacklist=%d update=%d refresh=%d", fakeStore.blacklistCalls, fakeStore.updateCalls, refresh.calls)
			}
		})
	}
}

func TestHandleAllProvidersDeadDoesNotRefreshWhenStatePersistenceFails(t *testing.T) {
	source := "<nzb>sample</nzb>"
	fakeStore := &fakeTerminalFailureStore{updateErr: errors.New("state persistence failed")}
	refresh := &fakeRefreshSignal{}

	handled := handleAllProvidersDead(
		context.Background(),
		terminalFailureTestLogger(),
		terminalFailureStoreAdapter{fakeStore},
		&store.Item{SourceURI: &source},
		refresh,
		"",
		2,
		2,
		time.Hour,
	)

	if !handled {
		t.Fatal("expected all-provider dead outcome to be handled")
	}
	if fakeStore.blacklistCalls != 1 || fakeStore.updateCalls != 1 {
		t.Fatalf("calls: blacklist=%d update=%d, want 1 each", fakeStore.blacklistCalls, fakeStore.updateCalls)
	}
	if refresh.calls != 0 {
		t.Fatalf("refresh calls = %d, want 0", refresh.calls)
	}
}

func TestHandleAllProvidersDeadStillFailsWhenBlacklistPersistenceFails(t *testing.T) {
	source := "<nzb>sample</nzb>"
	fakeStore := &fakeTerminalFailureStore{blacklistErr: errors.New("blacklist persistence failed")}
	refresh := &fakeRefreshSignal{}

	handled := handleAllProvidersDead(
		context.Background(),
		terminalFailureTestLogger(),
		terminalFailureStoreAdapter{fakeStore},
		&store.Item{SourceURI: &source},
		refresh,
		"",
		1,
		1,
		time.Hour,
	)

	if !handled {
		t.Fatal("expected all-provider dead outcome to be handled")
	}
	if fakeStore.blacklistCalls != 1 || fakeStore.updateCalls != 1 {
		t.Fatalf("calls: blacklist=%d update=%d, want 1 each", fakeStore.blacklistCalls, fakeStore.updateCalls)
	}
	if fakeStore.updateState != store.StateFailed {
		t.Fatalf("state = %q, want %q", fakeStore.updateState, store.StateFailed)
	}
	if refresh.calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refresh.calls)
	}
}

func TestHandleAllProvidersDeadStillFailsWhenSuppressionRecordFails(t *testing.T) {
	source := "<nzb>sample</nzb>"
	fakeStore := &fakeTerminalFailureStore{suppressErr: errors.New("suppression persistence failed")}
	refresh := &fakeRefreshSignal{}

	handled := handleAllProvidersDead(
		context.Background(),
		terminalFailureTestLogger(),
		terminalFailureStoreAdapter{fakeStore},
		&store.Item{SourceURI: &source},
		refresh,
		"",
		1,
		1,
		time.Hour,
	)

	if !handled {
		t.Fatal("expected all-provider dead outcome to be handled even when the SF-03 suppression record fails")
	}
	if fakeStore.suppressCalls != 1 {
		t.Fatalf("suppression record calls = %d, want 1 (attempted even though it fails)", fakeStore.suppressCalls)
	}
	if fakeStore.blacklistCalls != 1 || fakeStore.updateCalls != 1 {
		t.Fatalf("calls: blacklist=%d update=%d, want 1 each -- a suppression persist failure must not skip the existing blacklist/state-transition path", fakeStore.blacklistCalls, fakeStore.updateCalls)
	}
	if fakeStore.updateState != store.StateFailed {
		t.Fatalf("state = %q, want %q", fakeStore.updateState, store.StateFailed)
	}
	if refresh.calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refresh.calls)
	}
}

func TestHandleTerminalTorrentFailureUsesCanonicalRealHashAndRefreshes(t *testing.T) {
	synthetic := "SYNTHETIC-HASH"
	item := &store.Item{
		ID:         "torrent-item-1",
		SourceType: store.SourceTypeTorrent,
		InfoHash:   &synthetic,
		Metadata: store.SubmissionMetadata{
			RealInfoHash: "ABCDEF1234567890",
		},
	}
	status := &provider.TaskStatus{
		Outcome:     provider.TorrentOutcomeTerminalDeadSource,
		FailureCode: provider.TorrentFailureTerminalDeadSource,
		Hash:        "provider-hash-must-not-win",
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
		t.Fatal("expected terminal torrent outcome to be handled")
	}
	if fakeStore.blacklistCalls != 1 {
		t.Fatalf("blacklist calls = %d, want 1", fakeStore.blacklistCalls)
	}
	if fakeStore.blacklistKey != "abcdef1234567890" {
		t.Fatalf("blacklist key = %q, want canonical real hash", fakeStore.blacklistKey)
	}
	if fakeStore.updateCalls != 1 || fakeStore.updateState != store.StateFailed {
		t.Fatalf("state update = calls:%d state:%q, want one failed update", fakeStore.updateCalls, fakeStore.updateState)
	}
	if fakeStore.updateMessage != string(provider.TorrentFailureTerminalDeadSource) {
		t.Fatalf("failure message = %q, want %q", fakeStore.updateMessage, provider.TorrentFailureTerminalDeadSource)
	}
	if refresh.calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refresh.calls)
	}
}

// TestHandleTerminalTorrentFailureAcceptsDeadSentinelOutcome is the
// regression for the 2026-07-18 soak finding: TorrentOutcomeDeadSentinel
// (TorBox's own ambiguous checking+zero-seed+100-day-ETA signal) must still
// be treated as terminal by handleTerminalTorrentFailure once the caller
// (resolveItem's dedicated grace-period gate) has decided to call it --
// this function itself does not re-apply any grace period, it only needs
// to recognize the outcome as failable, same as the explicit
// TerminalDeadSource case.
func TestHandleTerminalTorrentFailureAcceptsDeadSentinelOutcome(t *testing.T) {
	item := &store.Item{ID: "torrent-item-sentinel", SourceType: store.SourceTypeTorrent}
	status := &provider.TaskStatus{
		Outcome:     provider.TorrentOutcomeDeadSentinel,
		FailureCode: provider.TorrentFailureTerminalDeadSource,
		Hash:        "deadsentinelhash",
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
		t.Fatal("expected dead-sentinel outcome to be handled as terminal")
	}
	if fakeStore.blacklistCalls != 1 {
		t.Fatalf("blacklist calls = %d, want 1", fakeStore.blacklistCalls)
	}
	if fakeStore.updateCalls != 1 || fakeStore.updateState != store.StateFailed {
		t.Fatalf("state update = calls:%d state:%q, want one failed update", fakeStore.updateCalls, fakeStore.updateState)
	}
}

func TestTorrentLifecycleLogsUseSafeProviderNeutralIdentity(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	synthetic := "SYNTHETIC-HASH"
	magnet := "magnet:?xt=urn:btih:SYNTHETIC-HASH&tr=https://tracker.example/announce?token=tracker-secret&x.pe=credential-secret"
	item := &store.Item{
		ID:         "torrent-item-log",
		SourceType: store.SourceTypeTorrent,
		State:      store.StateResolving,
		InfoHash:   &synthetic,
		SourceURI:  &magnet,
		Metadata: store.SubmissionMetadata{
			RealInfoHash: "ABCDEF1234567890",
		},
	}
	status := &provider.TaskStatus{
		Outcome:     provider.TorrentOutcomeTerminalDeadSource,
		FailureCode: provider.TorrentFailureTerminalDeadSource,
		State:       "proprietary-provider-state",
		Error:       "https://provider.example/task?token=provider-secret",
	}
	fakeStore := &fakeTerminalFailureStore{}

	logTorrentOutcome(log, item, status)
	if !handleTerminalTorrentFailure(
		context.Background(),
		log,
		terminalFailureStoreAdapter{fakeStore},
		item,
		status,
		&fakeRefreshSignal{},
	) {
		t.Fatal("expected terminal torrent outcome to be handled")
	}

	got := buf.String()
	for _, required := range []string{
		"event=torrent_outcome_observed",
		"event=source_failure_recorded",
		"event=torrent_terminal_decision",
		"event=source_blacklisted",
		"event=item_state_transition",
		"event=item_failure_persisted",
		"info_hash=abcdef1234567890",
		"synthetic_alias=synthetic-hash",
		"failure_code=torrent_terminal_dead_source",
	} {
		if !strings.Contains(got, required) {
			t.Fatalf("log missing %q: %s", required, got)
		}
	}
	for _, forbidden := range []string{
		magnet,
		"tracker-secret",
		"credential-secret",
		"provider-secret",
		"proprietary-provider-state",
		"provider.example",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("log exposed forbidden value %q: %s", forbidden, got)
		}
	}
}

func TestHandleTerminalTorrentFailureDoesNotRefreshWhenPersistenceFails(t *testing.T) {
	hash := "ABC123"
	item := &store.Item{SourceType: store.SourceTypeTorrent, InfoHash: &hash}
	status := &provider.TaskStatus{Outcome: provider.TorrentOutcomeTerminalDeadSource}
	fakeStore := &fakeTerminalFailureStore{updateErr: errors.New("write failed")}
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
		t.Fatal("expected terminal torrent outcome to be handled")
	}
	if refresh.calls != 0 {
		t.Fatalf("refresh calls = %d, want 0 when failed-state persistence fails", refresh.calls)
	}
}

func TestHandleTerminalTorrentFailureIgnoresRecoverableOutcomes(t *testing.T) {
	tests := []provider.TorrentOutcome{
		provider.TorrentOutcomeUnknown,
		provider.TorrentOutcomeTransientError,
		provider.TorrentOutcomeStalled,
		provider.TorrentOutcomeCapacityLimited,
		provider.TorrentOutcomeCancelled,
		provider.TorrentOutcomeRemoteRemoved,
		provider.TorrentOutcomeReady,
	}
	for _, outcome := range tests {
		t.Run(string(outcome), func(t *testing.T) {
			fakeStore := &fakeTerminalFailureStore{}
			refresh := &fakeRefreshSignal{}
			handled := handleTerminalTorrentFailure(
				context.Background(),
				terminalFailureTestLogger(),
				terminalFailureStoreAdapter{fakeStore},
				&store.Item{},
				&provider.TaskStatus{Outcome: outcome},
				refresh,
			)
			if handled {
				t.Fatalf("outcome %q was handled as terminal", outcome)
			}
			if fakeStore.blacklistCalls != 0 || fakeStore.updateCalls != 0 || refresh.calls != 0 {
				t.Fatalf("outcome %q produced side effects", outcome)
			}
		})
	}
}
