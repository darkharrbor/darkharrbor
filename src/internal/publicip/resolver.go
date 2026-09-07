// Package publicip resolves and caches the host's public egress address.
package publicip

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	defaultTimeout         = 5 * time.Second
	defaultRefreshInterval = 15 * time.Minute
	defaultStaleAfter      = 24 * time.Hour
	maxResponseBytes       = 128
)

var defaultEndpoints = []string{
	"https://api.ipify.org",
	"https://checkip.amazonaws.com",
	"https://icanhazip.com",
}

var ErrUnavailable = errors.New("public egress address unavailable")

type Status struct {
	Address         string
	UpdatedAt       time.Time
	LastAttemptAt   time.Time
	LastAttemptFail bool
	Refreshing      bool
	Stale           bool
}

type Options struct {
	Client          *http.Client
	Endpoints       []string
	Timeout         time.Duration
	RefreshInterval time.Duration
	StaleAfter      time.Duration
	Now             func() time.Time
}

type Resolver struct {
	client          *http.Client
	endpoints       []string
	timeout         time.Duration
	refreshInterval time.Duration
	staleAfter      time.Duration
	now             func() time.Time
	ctx             context.Context
	cancel          context.CancelFunc

	mu              sync.Mutex
	address         string
	updatedAt       time.Time
	lastAttemptAt   time.Time
	lastAttemptFail bool
	inflight        chan struct{}
	wg              sync.WaitGroup
}

func New(parent context.Context, opts Options) *Resolver {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	if opts.Client == nil {
		opts.Client = &http.Client{}
	}
	if len(opts.Endpoints) == 0 {
		opts.Endpoints = defaultEndpoints
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}
	if opts.RefreshInterval <= 0 {
		opts.RefreshInterval = defaultRefreshInterval
	}
	if opts.StaleAfter <= 0 {
		opts.StaleAfter = defaultStaleAfter
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Resolver{
		client: opts.Client, endpoints: append([]string(nil), opts.Endpoints...),
		timeout: opts.Timeout, refreshInterval: opts.RefreshInterval,
		staleAfter: opts.StaleAfter, now: opts.Now, ctx: ctx, cancel: cancel,
	}
}

// Resolve returns a fresh cached address, performs a bounded lookup when the
// cache is empty, and serves the last known good address while refreshing it.
func (r *Resolver) Resolve(ctx context.Context) (string, error) {
	now := r.now()
	r.mu.Lock()
	if r.address != "" {
		address := r.address
		if now.Sub(r.updatedAt) >= r.refreshInterval && r.inflight == nil {
			r.inflight = make(chan struct{})
			r.wg.Add(1)
			go func() {
				defer r.wg.Done()
				r.finishLookup(r.lookup(r.ctx))
			}()
		}
		r.mu.Unlock()
		return address, nil
	}
	if wait := r.inflight; wait != nil {
		r.mu.Unlock()
		select {
		case <-wait:
			return r.Resolve(ctx)
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	r.inflight = make(chan struct{})
	r.mu.Unlock()
	address, err := r.lookup(ctx)
	r.finishLookup(address, err)
	return address, err
}

func (r *Resolver) lookup(parent context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(parent, r.timeout)
	defer cancel()
	attemptTimeout := r.timeout / time.Duration(len(r.endpoints))
	if attemptTimeout <= 0 {
		attemptTimeout = r.timeout
	}
	for _, endpoint := range r.endpoints {
		attemptCtx, attemptCancel := context.WithTimeout(ctx, attemptTimeout)
		req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, endpoint, nil)
		if err != nil {
			attemptCancel()
			continue
		}
		resp, err := r.client.Do(req)
		if err != nil {
			attemptCancel()
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
		_ = resp.Body.Close()
		attemptCancel()
		if readErr != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 || len(body) > maxResponseBytes {
			continue
		}
		ip := net.ParseIP(strings.TrimSpace(string(body)))
		if ip != nil {
			return ip.String(), nil
		}
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return "", ErrUnavailable
}

func (r *Resolver) finishLookup(address string, err error) {
	now := r.now()
	r.mu.Lock()
	if err == nil {
		r.address = address
		r.updatedAt = now
	}
	r.lastAttemptAt = now
	r.lastAttemptFail = err != nil
	wait := r.inflight
	r.inflight = nil
	if wait != nil {
		close(wait)
	}
	r.mu.Unlock()
}

func (r *Resolver) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Status{
		Address: r.address, UpdatedAt: r.updatedAt, LastAttemptAt: r.lastAttemptAt,
		LastAttemptFail: r.lastAttemptFail, Refreshing: r.inflight != nil,
		Stale: r.address != "" && r.now().Sub(r.updatedAt) >= r.staleAfter,
	}
}

func (r *Resolver) Close() {
	r.cancel()
	r.wg.Wait()
}
