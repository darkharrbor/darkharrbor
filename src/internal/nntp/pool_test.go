package nntp

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
)

// startFakeGreetingServer accepts connections and greets the first
// allowGreetings of them with a normal "200" NNTP greeting (held open,
// otherwise idle — enough for Pool.dial to succeed and return a live Conn).
// Every connection after that is rejected with a "502 too many connections"
// greeting and closed, modeling a provider's real concurrent-connection
// ceiling rejection for HR1.4's AIMD ConnCeiling.
func startFakeGreetingServer(t *testing.T, allowGreetings int32) (addr string, accepted *int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	var count int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			n := atomic.AddInt32(&count, 1)
			go func(c net.Conn, n int32) {
				if n <= allowGreetings {
					_, _ = c.Write([]byte("200 ready\r\n"))
					// Keep the connection open and idle; the test only
					// exercises dial()/greeting behavior, not BODY fetches.
					buf := make([]byte, 1)
					_, _ = bufio.NewReader(c).Read(buf)
					_ = c.Close()
					return
				}
				_, _ = c.Write([]byte("502 too many connections\r\n"))
				_ = c.Close()
			}(conn, n)
		}
	}()
	return ln.Addr().String(), &count
}

func hostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return host, port
}

func TestPool_NoCeilingWired_BehavesAsBefore(t *testing.T) {
	addr, _ := startFakeGreetingServer(t, 10)
	host, port := hostPort(t, addr)

	p := NewPool(PoolConfig{Host: host, Port: port, MaxConns: 2, DialTimeout: 2 * time.Second}, nil)
	t.Cleanup(p.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c1, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire #1: %v", err)
	}
	c2, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire #2: %v", err)
	}
	p.Release(c1, nil)
	p.Release(c2, nil)
}

func TestPool_ConnCapRejection_ShrinksCeilingAndParksFutureSlots(t *testing.T) {
	// The fake server only greets the first 2 connections; the 3rd is a
	// hard "too many connections" rejection.
	addr, _ := startFakeGreetingServer(t, 2)
	host, port := hostPort(t, addr)

	p := NewPool(PoolConfig{Host: host, Port: port, MaxConns: 4, DialTimeout: 2 * time.Second}, nil)
	t.Cleanup(p.Close)

	ceiling := accountgov.NewConnCeiling(4)
	fixedNow := time.Now()
	ceiling.SetClock(func() time.Time { return fixedNow }) // freeze growth for this test
	p.SetCeiling(ceiling)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c1, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire #1: %v", err)
	}
	c2, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire #2: %v", err)
	}

	// Third attempt hits the server's real cap and must fail, and must
	// classify as a conn-cap rejection that shrinks the ceiling.
	if _, err := p.Acquire(ctx); err == nil {
		t.Fatal("Acquire #3: expected conn-cap rejection error, got nil")
	}
	if got := ceiling.Current(); got != 2 {
		t.Fatalf("after rejection: ceiling.Current() = %d, want 2 (halved from 4)", got)
	}

	// A 4th attempt must NOT dial again (which would hit the server's cap a
	// second time and needlessly halve the ceiling further, i.e. the
	// "per-segment retry churn" HR1.4 forbids) — it must park and time out
	// waiting for capacity instead, since 2 connections are already open
	// and the ceiling is now 2.
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer shortCancel()
	if _, err := p.Acquire(shortCtx); err == nil {
		t.Fatal("Acquire #4: expected the attempt to park and time out, got a connection")
	}
	if got := ceiling.Current(); got != 2 {
		t.Fatalf("ceiling should be unchanged by a parked (non-dialing) attempt: got %d, want 2", got)
	}

	// Releasing an existing connection must satisfy a fresh Acquire without
	// any further dial against the server.
	p.Release(c1, nil)
	c3, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	p.Release(c2, nil)
	p.Release(c3, nil)
}

func TestPool_CeilingGrowsBack_UnparksSlots(t *testing.T) {
	addr, _ := startFakeGreetingServer(t, 10)
	host, port := hostPort(t, addr)

	p := NewPool(PoolConfig{Host: host, Port: port, MaxConns: 2, DialTimeout: 2 * time.Second}, nil)
	t.Cleanup(p.Close)

	ceiling := accountgov.NewConnCeiling(2)
	now := time.Now()
	ceiling.SetClock(func() time.Time { return now })
	ceiling.SetGrowInterval(10 * time.Second)
	ceiling.OnRejected() // 2 -> 1, simulating an earlier real rejection
	p.SetCeiling(ceiling)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c1, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire #1: %v", err)
	}

	// Ceiling is 1 and one connection is already open: a second attempt
	// must park rather than dial.
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	if _, err := p.Acquire(shortCtx); err == nil {
		t.Fatal("Acquire #2: expected park/timeout while ceiling=1 and one conn open")
	}
	shortCancel()

	// Advance the clock past the grow interval: the ceiling probes back up
	// to 2, and the parked slot must become usable.
	now = now.Add(11 * time.Second)
	c2, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire after ceiling growth: %v", err)
	}

	p.Release(c1, nil)
	p.Release(c2, nil)
}
