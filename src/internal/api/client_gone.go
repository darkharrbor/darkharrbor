package api

import (
	"context"
	"errors"
	"syscall"
)

// clientGone reports whether a stream error is the client hanging up (the
// request context ended, or the write hit EPIPE/ECONNRESET) rather than a
// failure on DarkHarrbor's or the provider's side. Players close and reopen
// connections on every seek, so these are routine and logged at DEBUG.
func clientGone(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx != nil && ctx.Err() != nil {
		return true
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET)
}
