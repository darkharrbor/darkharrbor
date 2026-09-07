package provider

import (
	"context"
	"sync"
	"time"
)

// Rate-limiting mechanism. Relocated verbatim from internal/torbox/limiter.go
// Policy is now provider-neutral; internal/torbox re-exports aliases.

type Waiter interface {
	Wait(ctx context.Context) error
}

type TokenBucket struct {
	ch       chan struct{}
	stop     chan struct{}
	stopOnce sync.Once
}

func NewTokenBucket(capacity int, refillEvery time.Duration) *TokenBucket {
	if capacity <= 0 || refillEvery <= 0 {
		return &TokenBucket{ch: nil}
	}
	interval := refillEvery / time.Duration(capacity)
	if interval <= 0 {
		return &TokenBucket{ch: nil}
	}
	bucket := &TokenBucket{
		ch:   make(chan struct{}, capacity),
		stop: make(chan struct{}),
	}
	for range capacity {
		bucket.ch <- struct{}{}
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				select {
				case bucket.ch <- struct{}{}:
				default:
				}
			case <-bucket.stop:
				return
			}
		}
	}()
	return bucket
}

// Stop terminates the refill goroutine. Safe to call multiple times or on a nil receiver.
func (b *TokenBucket) Stop() {
	if b != nil && b.stop != nil {
		b.stopOnce.Do(func() { close(b.stop) })
	}
}

func (b *TokenBucket) Wait(ctx context.Context) error {
	if b == nil || b.ch == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.ch:
		return nil
	}
}
