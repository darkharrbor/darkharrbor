package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
)

func TestClientGone(t *testing.T) {
	live := context.Background()
	done, cancel := context.WithCancel(context.Background())
	cancel()
	reset := fmt.Errorf("rangecache: write error: %w", &net.OpError{Op: "write", Err: os.NewSyscallError("write", syscall.ECONNRESET)})
	pipe := fmt.Errorf("rangecache: write error: %w", &net.OpError{Op: "write", Err: os.NewSyscallError("write", syscall.EPIPE)})
	for _, tt := range []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"nil", live, nil, false},
		{"reset", live, reset, true},
		{"broken pipe", live, pipe, true},
		{"request cancelled", done, errors.New("anything"), true},
		{"provider failure", live, errors.New("cdn source: status 502"), false},
	} {
		if got := clientGone(tt.ctx, tt.err); got != tt.want {
			t.Errorf("%s: got %v want %v", tt.name, got, tt.want)
		}
	}
}
