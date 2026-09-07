package nntp

import (
	"context"
	"errors"
)

// errNNTPProviderUnavailable is returned by TestConnection when called on a
// nil provider or a provider with no pool (e.g. a zero-value NNTPProvider
// in a test) rather than panicking.
var errNNTPProviderUnavailable = errors.New("nntp: provider unavailable")

// TestConnection performs one real, bounded connect+AUTHINFO round trip
// through this provider's own shared connection pool -- the identical
// dial() path every real Pool.Acquire call uses -- then immediately
// releases the connection back to the pool as a clean success. It never
// borrows a connection for longer than the round trip itself, never
// touches the negative-article cache, failover health, or final-rung
// accounting, and moves no article bytes. Bounded by ctx; a caller should
// pass a context with a short deadline (the pool's own DialTimeout is a
// fallback ceiling, not a substitute).
func (p *NNTPProvider) TestConnection(ctx context.Context) error {
	if p == nil || p.pool == nil {
		return errNNTPProviderUnavailable
	}
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	p.pool.Release(conn, nil)
	return nil
}
