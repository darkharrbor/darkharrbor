package httpstream

import (
	"context"
	"hash/fnv"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	backendConcurrency   = 4
	startupProbeTimeout  = 5 * time.Second
	startupWholeTimeout  = 12 * time.Second
	searchBackendTimeout = 10 * time.Second
	searchWholeTimeout   = 15 * time.Second
)

// HealthChecker is the optional protocol-specific health surface used by the
// backend lifecycle. A handler without it is treated as healthy for backward
// compatibility with deterministic test handlers.
type HealthChecker interface {
	Healthy(context.Context) error
}

type backendState uint8

const (
	backendConfigured backendState = iota
	backendHealthy
	backendUnhealthy
)

type backendEntry struct {
	id       string
	priority int
	handler  Handler
	managed  Handler
	sem      chan struct{}

	mu            sync.RWMutex
	state         backendState
	cooldownUntil time.Time
}

// SearchOutcome is one backend's bounded search result. Outcomes retain
// priority/stable-ID order even though the work runs concurrently.
type SearchOutcome struct {
	BackendID string
	Handler   string
	Results   []SearchResult
	Err       error
}

// BackendStatus is the URL-free health projection exposed by /healthz.
type BackendStatus struct {
	BackendID string `json:"backend_id"`
	Handler   string `json:"handler"`
	State     string `json:"state"`
	Cooldown  bool   `json:"cooldown"`
}

// Registry owns named backend instances. The backend ID, not protocol name,
// is the primary key so multiple OMSS/Stremio instances never overwrite one
// another (D5/HS-3.12).
type Registry struct {
	mu       sync.RWMutex
	backends map[string]*backendEntry
}

func NewRegistry() *Registry { return &Registry{backends: make(map[string]*backendEntry)} }

// Register adds or replaces one configured named backend instance.
func (r *Registry) Register(backendID string, priority int, h Handler) {
	if r == nil || h == nil {
		return
	}
	id := strings.ToLower(strings.TrimSpace(backendID))
	if id == "" {
		return
	}
	e := &backendEntry{id: id, priority: priority, handler: h, sem: make(chan struct{}, backendConcurrency)}
	e.managed = managedHandler{entry: e}
	r.mu.Lock()
	r.backends[id] = e
	r.mu.Unlock()
}

// Lookup returns the bounded handler for the exact persisted backend and
// protocol pair. Resolve remains available while unhealthy so an existing
// library item always gets one bounded live attempt (A27).
func (r *Registry) Lookup(backendID, protocol string) (Handler, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	e := r.backends[strings.ToLower(strings.TrimSpace(backendID))]
	r.mu.RUnlock()
	if e == nil || !strings.EqualFold(e.handler.Name(), strings.TrimSpace(protocol)) {
		return nil, false
	}
	return e.managed, true
}

// LookupRemoteArchive returns the bounded optional archive arm for an exact
// configured backend. Unsupported handlers abstain instead of being treated
// as archive-capable merely because they share the main Handler contract.
func (r *Registry) LookupRemoteArchive(backendID, protocol string) (RemoteArchiveHandler, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	e := r.backends[strings.ToLower(strings.TrimSpace(backendID))]
	r.mu.RUnlock()
	if e == nil || !strings.EqualFold(e.handler.Name(), strings.TrimSpace(protocol)) {
		return nil, false
	}
	h, ok := e.handler.(RemoteArchiveHandler)
	if !ok {
		return nil, false
	}
	return managedRemoteArchiveHandler{entry: e, handler: h}, true
}

// Handlers returns every configured handler in deterministic priority then
// stable-ID order. It is used for stable capability advertisement.
func (r *Registry) Handlers() []Handler {
	entries := r.entries()
	out := make([]Handler, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.managed)
	}
	return out
}

func (r *Registry) Empty() bool {
	if r == nil {
		return true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.backends) == 0
}

func (r *Registry) Statuses() []BackendStatus {
	entries := r.entries()
	out := make([]BackendStatus, 0, len(entries))
	for _, e := range entries {
		e.mu.RLock()
		state := e.state
		cooldown := time.Now().Before(e.cooldownUntil)
		e.mu.RUnlock()
		label := "configured"
		if state == backendHealthy {
			label = "healthy"
		} else if state == backendUnhealthy {
			label = "unhealthy"
		}
		out = append(out, BackendStatus{BackendID: e.id, Handler: e.handler.Name(), State: label, Cooldown: cooldown})
	}
	return out
}

func (r *Registry) entries() []*backendEntry {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	out := make([]*backendEntry, 0, len(r.backends))
	for _, e := range r.backends {
		out = append(out, e)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].priority != out[j].priority {
			return out[i].priority < out[j].priority
		}
		return out[i].id < out[j].id
	})
	return out
}

// Start performs concurrent, non-fatal startup probes and starts jittered
// periodic rechecks. It returns after the whole-startup budget even when a
// backend is stuck.
func (r *Registry) Start(ctx context.Context, log *slog.Logger) {
	entries := r.entries()
	if len(entries) == 0 {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	probeCtx, cancel := context.WithTimeout(ctx, startupWholeTimeout)
	defer cancel()
	var wg sync.WaitGroup
	for _, e := range entries {
		wg.Add(1)
		go func(e *backendEntry) {
			defer wg.Done()
			r.probe(probeCtx, log, e)
		}(e)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-probeCtx.Done():
		log.Warn("http-stream: startup health budget exhausted")
	}
	for _, e := range entries {
		go r.recheckLoop(ctx, log, e)
	}
}

func (r *Registry) recheckLoop(ctx context.Context, log *slog.Logger, e *backendEntry) {
	t := time.NewTicker(healthInterval(e.id))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.probe(ctx, log, e)
		}
	}
}

func healthInterval(id string) time.Duration {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return time.Duration(45+int(h.Sum32()%31)) * time.Second
}

func (r *Registry) probe(parent context.Context, log *slog.Logger, e *backendEntry) {
	ctx, cancel := context.WithTimeout(parent, startupProbeTimeout)
	defer cancel()
	var err error
	if h, ok := e.handler.(HealthChecker); ok {
		if aerr := e.acquire(ctx); aerr != nil {
			err = aerr
		} else {
			err = h.Healthy(ctx)
			e.observe(err)
			e.release()
		}
	}
	old := e.setHealthy(err == nil)
	if err != nil {
		if old != backendUnhealthy {
			log.Warn("http-stream: backend unhealthy", "backend_id", e.id, "type", e.handler.Name(), "error", Sanitize(err))
		}
		return
	}
	if old != backendHealthy {
		log.Info("http-stream: backend healthy", "backend_id", e.id, "type", e.handler.Name())
	}
}

func (e *backendEntry) setHealthy(ok bool) backendState {
	e.mu.Lock()
	old := e.state
	if ok {
		e.state = backendHealthy
	} else {
		e.state = backendUnhealthy
	}
	e.mu.Unlock()
	return old
}

func (e *backendEntry) healthy() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.state == backendHealthy
}

func (e *backendEntry) acquire(ctx context.Context) error {
	e.mu.RLock()
	until := e.cooldownUntil
	e.mu.RUnlock()
	if time.Now().Before(until) {
		return NewError(ClassRateLimited, "backend cooldown active")
	}
	select {
	case e.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return NewError(ClassBackendUnavailable, "backend request budget exhausted")
	}
}

func (e *backendEntry) release() { <-e.sem }

func (e *backendEntry) observe(err error) {
	if ClassOf(err) != ClassRateLimited {
		return
	}
	e.mu.Lock()
	e.cooldownUntil = time.Now().Add(RetryAfter(err))
	e.mu.Unlock()
}

type managedHandler struct{ entry *backendEntry }

type managedRemoteArchiveHandler struct {
	entry   *backendEntry
	handler RemoteArchiveHandler
}

func (h managedHandler) Name() string { return h.entry.handler.Name() }

func (h managedHandler) IDNative() bool {
	idn, ok := h.entry.handler.(interface{ IDNative() bool })
	return ok && idn.IDNative()
}

func (h managedHandler) Search(ctx context.Context, q StreamQuery) ([]SearchResult, error) {
	if err := h.entry.acquire(ctx); err != nil {
		return nil, err
	}
	defer h.entry.release()
	res, err := h.entry.handler.Search(ctx, q)
	h.entry.observe(err)
	return res, err
}

func (h managedHandler) Resolve(ctx context.Context, req ResolveRequest) ([]ResolvedFile, error) {
	if err := h.entry.acquire(ctx); err != nil {
		return nil, err
	}
	defer h.entry.release()
	res, err := h.entry.handler.Resolve(ctx, req)
	h.entry.observe(err)
	return res, err
}

func (h managedRemoteArchiveHandler) ResolveRemoteArchives(ctx context.Context, req ResolveRequest) ([]RemoteArchiveDescriptor, error) {
	if err := h.entry.acquire(ctx); err != nil {
		return nil, err
	}
	defer h.entry.release()
	res, err := h.handler.ResolveRemoteArchives(ctx, req)
	h.entry.observe(err)
	return res, err
}

// Search fans out only to currently healthy backends under per-backend and
// whole-request budgets. It returns partial outcomes in deterministic order;
// authoritative is false whenever any configured backend is unhealthy,
// cooling down, times out, or reports a transient failure.
func (r *Registry) Search(parent context.Context, q StreamQuery) (out []SearchOutcome, authoritative bool) {
	entries := r.entries()
	authoritative = true
	type indexed struct {
		index int
		out   SearchOutcome
	}
	healthy := make([]*backendEntry, 0, len(entries))
	for _, e := range entries {
		if e.healthy() {
			healthy = append(healthy, e)
		} else {
			authoritative = false
		}
	}
	if len(healthy) == 0 {
		return nil, authoritative
	}
	ctx, cancel := context.WithTimeout(parent, searchWholeTimeout)
	defer cancel()
	ch := make(chan indexed, len(healthy))
	for i, e := range healthy {
		go func(i int, e *backendEntry) {
			callCtx, callCancel := context.WithTimeout(ctx, searchBackendTimeout)
			defer callCancel()
			res, err := e.managed.Search(callCtx, q)
			ch <- indexed{i, SearchOutcome{BackendID: e.id, Handler: e.handler.Name(), Results: res, Err: err}}
		}(i, e)
	}
	ordered := make([]SearchOutcome, len(healthy))
	for range healthy {
		select {
		case got := <-ch:
			ordered[got.index] = got.out
			if got.out.Err != nil && ClassOf(got.out.Err).Transient() {
				authoritative = false
			}
		case <-ctx.Done():
			authoritative = false
			return completedOutcomes(ordered), authoritative
		}
	}
	return ordered, authoritative
}

func completedOutcomes(in []SearchOutcome) []SearchOutcome {
	out := in[:0]
	for _, item := range in {
		if item.BackendID != "" {
			out = append(out, item)
		}
	}
	return out
}
