package httpstream

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
)

type lifecycleHandler struct {
	id        string
	healthErr error
	searchErr error

	mu           sync.Mutex
	searchCalls  int
	resolveCalls int
}

type archiveLifecycleHandler struct{ lifecycleHandler }

func (h *archiveLifecycleHandler) ResolveRemoteArchives(context.Context, ResolveRequest) ([]RemoteArchiveDescriptor, error) {
	return []RemoteArchiveDescriptor{{Selector: "archive:zip:test"}}, nil
}

func (h *lifecycleHandler) Name() string { return "omss" }
func (h *lifecycleHandler) Healthy(context.Context) error {
	return h.healthErr
}
func (h *lifecycleHandler) Search(context.Context, StreamQuery) ([]SearchResult, error) {
	h.mu.Lock()
	h.searchCalls++
	err := h.searchErr
	h.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return []SearchResult{{Title: h.id}}, nil
}
func (h *lifecycleHandler) Resolve(context.Context, ResolveRequest) ([]ResolvedFile, error) {
	h.mu.Lock()
	h.resolveCalls++
	h.mu.Unlock()
	return []ResolvedFile{{Selector: h.id, URL: "https://example.invalid/file"}}, nil
}

func testLifecycleLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestRegistryKeepsNamedInstancesAndDeterministicOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := NewRegistry()
	r.Register("beta", 1, &lifecycleHandler{id: "beta"})
	r.Register("zeta", 0, &lifecycleHandler{id: "zeta"})
	r.Register("alpha", 1, &lifecycleHandler{id: "alpha"})
	r.Start(ctx, testLifecycleLog())

	for _, id := range []string{"alpha", "beta", "zeta"} {
		if _, ok := r.Lookup(id, "omss"); !ok {
			t.Fatalf("named backend %q was overwritten or lost", id)
		}
	}
	out, authoritative := r.Search(ctx, StreamQuery{Kind: "movie"})
	if !authoritative || len(out) != 3 {
		t.Fatalf("outcomes=%d authoritative=%v", len(out), authoritative)
	}
	if out[0].BackendID != "zeta" || out[1].BackendID != "alpha" || out[2].BackendID != "beta" {
		t.Fatalf("unexpected priority/stable-ID order: %+v", out)
	}
}

func TestRegistryBoundsOptionalRemoteArchiveArm(t *testing.T) {
	r := NewRegistry()
	r.Register("plain", 0, &lifecycleHandler{id: "plain"})
	r.Register("archive", 0, &archiveLifecycleHandler{lifecycleHandler{id: "archive"}})
	if _, ok := r.LookupRemoteArchive("plain", "omss"); ok {
		t.Fatal("plain handler exposed archive arm")
	}
	h, ok := r.LookupRemoteArchive("archive", "omss")
	if !ok {
		t.Fatal("archive arm missing")
	}
	descriptors, err := h.ResolveRemoteArchives(context.Background(), ResolveRequest{})
	if err != nil || len(descriptors) != 1 {
		t.Fatalf("descriptors=%+v err=%v", descriptors, err)
	}
}

func TestRegistryExcludesUnhealthySearchButAllowsPlaybackAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bad := &lifecycleHandler{id: "bad", healthErr: errors.New("down")}
	good := &lifecycleHandler{id: "good"}
	r := NewRegistry()
	r.Register("bad", 0, bad)
	r.Register("good", 1, good)
	r.Start(ctx, testLifecycleLog())

	out, authoritative := r.Search(ctx, StreamQuery{Kind: "movie"})
	if authoritative || len(out) != 1 || out[0].BackendID != "good" {
		t.Fatalf("outcomes=%+v authoritative=%v", out, authoritative)
	}
	h, ok := r.Lookup("bad", "omss")
	if !ok {
		t.Fatal("unhealthy backend blocked existing-library lookup")
	}
	if _, err := h.Resolve(ctx, ResolveRequest{}); err != nil {
		t.Fatalf("bounded playback attempt failed before handler call: %v", err)
	}
	bad.mu.Lock()
	calls := bad.resolveCalls
	bad.mu.Unlock()
	if calls != 1 {
		t.Fatalf("resolve calls=%d, want 1", calls)
	}
}

func TestRegistryRateCooldownStopsBackendBurst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &lifecycleHandler{id: "limited", searchErr: NewRateLimitError("60")}
	r := NewRegistry()
	r.Register("limited", 0, h)
	r.Start(ctx, testLifecycleLog())

	for i := 0; i < 2; i++ {
		out, authoritative := r.Search(ctx, StreamQuery{Kind: "movie"})
		if authoritative || len(out) != 1 || ClassOf(out[0].Err) != ClassRateLimited {
			t.Fatalf("pass %d: outcomes=%+v authoritative=%v", i, out, authoritative)
		}
	}
	h.mu.Lock()
	calls := h.searchCalls
	h.mu.Unlock()
	if calls != 1 {
		t.Fatalf("handler calls=%d, want 1 before cooldown", calls)
	}
}

func TestRateLimitRetryAfterBounded(t *testing.T) {
	for raw, want := range map[string]int{"": 30, "0": 30, "12": 12, "999": 300} {
		if got := int(RetryAfter(NewRateLimitError(raw)).Seconds()); got != want {
			t.Fatalf("RetryAfter(%q)=%d, want %d", raw, got, want)
		}
	}
}
