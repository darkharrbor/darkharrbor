package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/torbox"
)

func TestDiscoverTorBoxPlanRetriesTransient(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	calls := 0
	get := func(context.Context) (*torbox.UserInfo, error) {
		calls++
		if calls < 3 {
			return nil, provider.MarkRetryable(errors.New("torbox status 429: rate limit exceeded"))
		}
		return &torbox.UserInfo{Plan: 2}, nil
	}
	info, err := discoverTorBoxPlan(context.Background(), log, get, []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond})
	if err != nil || info == nil || info.Plan != 2 || calls != 3 {
		t.Fatalf("info=%v err=%v calls=%d, want plan 2 after 3 calls", info, err, calls)
	}
}

func TestDiscoverTorBoxPlanStopsOnPermanentAndBound(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	calls := 0
	perm := func(context.Context) (*torbox.UserInfo, error) {
		calls++
		return nil, errors.New("torbox status 403: AUTH_ERROR")
	}
	if _, err := discoverTorBoxPlan(context.Background(), log, perm, []time.Duration{time.Millisecond}); err == nil || calls != 1 {
		t.Fatalf("permanent: err=%v calls=%d, want error after 1 call", err, calls)
	}
	calls = 0
	always := func(context.Context) (*torbox.UserInfo, error) {
		calls++
		return nil, provider.MarkRetryable(errors.New("503"))
	}
	if _, err := discoverTorBoxPlan(context.Background(), log, always, []time.Duration{time.Millisecond, time.Millisecond}); err == nil || calls != 3 {
		t.Fatalf("bound: err=%v calls=%d, want error after 3 calls", err, calls)
	}
}

func TestDiscoverTorBoxPlanRetriesAttemptTimeout(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	calls := 0
	get := func(context.Context) (*torbox.UserInfo, error) {
		calls++
		if calls == 1 {
			// What the policy limiter returns when an attempt times out behind a 429 hold.
			return nil, fmt.Errorf("rate limiter wait: torbox policy: hold wait (query): %w", context.DeadlineExceeded)
		}
		return &torbox.UserInfo{Plan: 2}, nil
	}
	info, err := discoverTorBoxPlan(context.Background(), log, get, []time.Duration{time.Millisecond})
	if err != nil || info == nil || calls != 2 {
		t.Fatalf("info=%v err=%v calls=%d, want success on 2nd call", info, err, calls)
	}
}
