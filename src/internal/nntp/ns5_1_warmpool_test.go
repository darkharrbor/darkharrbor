package nntp

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// warmTestServer is a small scripted NNTP fake used only by the NS-5.1 tests
// below: it greets every connection normally, then answers DATE with a
// configurable response (111 success by default, or a forced failure) and
// keeps the connection open for further commands until the client closes it.
// This is a canned deterministic fixture per WORKFLOW's own convention
// (real-provider acceptance is the separate CG-01 live gate, not this test).
type warmTestServer struct {
	t        *testing.T
	ln       net.Listener
	mu       sync.Mutex
	accepts  int
	failDate bool // when true, DATE gets a bogus non-111 response
}

func startWarmTestServer(t *testing.T) *warmTestServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &warmTestServer{t: t, ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go s.serve()
	return s
}

func (s *warmTestServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.accepts++
		s.mu.Unlock()
		go s.handle(conn)
	}
}

func (s *warmTestServer) setFailDate(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failDate = v
}

func (s *warmTestServer) acceptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accepts
}

func (s *warmTestServer) handle(conn net.Conn) {
	defer conn.Close()
	_, _ = conn.Write([]byte("200 ready\r\n"))
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case cmd == "DATE":
			s.mu.Lock()
			fail := s.failDate
			s.mu.Unlock()
			if fail {
				_, _ = conn.Write([]byte("503 no facility\r\n"))
				return // simulate a broken session: close after the bad answer
			}
			_, _ = conn.Write([]byte("111 20260101000000\r\n"))
		default:
			_, _ = conn.Write([]byte("500 unknown command\r\n"))
		}
	}
}

func (s *warmTestServer) hostPort() (string, int) {
	return hostPort(s.t, s.ln.Addr().String())
}

func TestWarmTick_Disabled_NoOp(t *testing.T) {
	srv := startWarmTestServer(t)
	host, port := srv.hostPort()
	p := NewPool(PoolConfig{Host: host, Port: port, MaxConns: 4, DialTimeout: 2 * time.Second, WarmConns: 0}, nil)
	t.Cleanup(p.Close)

	p.WarmTick(nil)

	if got := srv.acceptCount(); got != 0 {
		t.Fatalf("warm-keeping disabled: expected zero dials, got %d", got)
	}
	open, target, cold, warm := p.WarmSnapshot()
	if open != 0 || target != 0 || cold != 0 || warm != 0 {
		t.Fatalf("disabled pool snapshot = (%d,%d,%d,%d), want all zero", open, target, cold, warm)
	}
}

func TestWarmTick_DialsUpToTarget(t *testing.T) {
	srv := startWarmTestServer(t)
	host, port := srv.hostPort()
	p := NewPool(PoolConfig{Host: host, Port: port, MaxConns: 4, DialTimeout: 2 * time.Second, WarmConns: 2}, nil)
	t.Cleanup(p.Close)

	p.WarmTick(nil)

	if got := srv.acceptCount(); got != 2 {
		t.Fatalf("expected exactly 2 real dials to reach the warm target, got %d", got)
	}
	open, target, _, _ := p.WarmSnapshot()
	if open != 2 || target != 2 {
		t.Fatalf("WarmSnapshot open/target = %d/%d, want 2/2", open, target)
	}

	// A second tick with the target already met must not dial again.
	p.WarmTick(nil)
	if got := srv.acceptCount(); got != 2 {
		t.Fatalf("second tick at target should not dial again, got %d total accepts", got)
	}
}

func TestWarmTick_RefreshesWithPing(t *testing.T) {
	srv := startWarmTestServer(t)
	host, port := srv.hostPort()
	p := NewPool(PoolConfig{Host: host, Port: port, MaxConns: 2, DialTimeout: 2 * time.Second, WarmConns: 1}, nil)
	t.Cleanup(p.Close)

	p.WarmTick(nil) // dials 1 warm connection
	if got := srv.acceptCount(); got != 1 {
		t.Fatalf("expected 1 dial, got %d", got)
	}

	// Repeated ticks must keep pinging the SAME connection (DATE succeeds),
	// never redialing, as long as the server keeps answering 111.
	for i := 0; i < 3; i++ {
		p.WarmTick(nil)
	}
	if got := srv.acceptCount(); got != 1 {
		t.Fatalf("healthy warm connection should never be redialed by refresh ticks, got %d total accepts", got)
	}
	open, target, _, _ := p.WarmSnapshot()
	if open != 1 || target != 1 {
		t.Fatalf("WarmSnapshot open/target = %d/%d, want 1/1", open, target)
	}
}

func TestWarmTick_DiscardsDeadConnectionAndRedialsNextTick(t *testing.T) {
	srv := startWarmTestServer(t)
	host, port := srv.hostPort()
	// MaxConns=1 (== cap(ch)) so one WarmTick pass, which visits at most
	// cap(ch) channel items, can only either notice-and-evict the dead
	// connection OR redial in a given tick -- not both in the same pass --
	// making the two-tick recovery sequence below deterministic. With a
	// larger pool a same-tick top-up could immediately refill the freed
	// slot (also correct, just not what this test isolates).
	p := NewPool(PoolConfig{Host: host, Port: port, MaxConns: 1, DialTimeout: 2 * time.Second, WarmConns: 1}, nil)
	t.Cleanup(p.Close)

	p.WarmTick(nil)
	if got := srv.acceptCount(); got != 1 {
		t.Fatalf("expected 1 initial dial, got %d", got)
	}

	// Force the next ping to fail (server answers non-111 and closes).
	srv.setFailDate(true)
	p.WarmTick(nil)
	open, _, _, _ := p.WarmSnapshot()
	if open != 0 {
		t.Fatalf("after a failed ping the dead connection must be discarded: open=%d, want 0", open)
	}

	// Server recovers; the NEXT tick must redial to restore the target.
	srv.setFailDate(false)
	p.WarmTick(nil)
	open, target, _, _ := p.WarmSnapshot()
	if open != 1 || target != 1 {
		t.Fatalf("after recovery tick: open/target = %d/%d, want 1/1", open, target)
	}
	if got := srv.acceptCount(); got != 2 {
		t.Fatalf("expected exactly 2 total dials (initial + redial after failure), got %d", got)
	}
}

func TestConnPing_SuccessAndFailure(t *testing.T) {
	srv := startWarmTestServer(t)
	host, port := srv.hostPort()
	p := NewPool(PoolConfig{Host: host, Port: port, MaxConns: 1, DialTimeout: 2 * time.Second}, nil)
	t.Cleanup(p.Close)

	c, err := p.dial()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	if err := c.Ping(); err != nil {
		t.Fatalf("Ping on a healthy connection: %v", err)
	}
	if c.Err() != nil {
		t.Fatalf("a successful Ping must not poison the connection: %v", c.Err())
	}

	// Second connection, this time scripted to fail DATE.
	srv.setFailDate(true)
	c2, err := p.dial()
	if err != nil {
		t.Fatalf("dial #2: %v", err)
	}
	defer c2.Close()
	if err := c2.Ping(); err == nil {
		t.Fatal("Ping expected to fail against a non-111 DATE response")
	}
	if c2.Err() == nil {
		t.Fatal("a failed Ping must mark the connection broken (c.Err() != nil)")
	}
}

func TestAcquireStats_ColdThenWarm(t *testing.T) {
	srv := startWarmTestServer(t)
	host, port := srv.hostPort()
	// MaxConns=1 makes the released connection the only channel item left
	// for the next Acquire to find, deterministically exercising the warm
	// (reuse) path rather than racing against the pool's other pre-filled
	// free slots for FIFO order.
	p := NewPool(PoolConfig{Host: host, Port: port, MaxConns: 1, DialTimeout: 2 * time.Second}, nil)
	t.Cleanup(p.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c1, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire #1: %v", err)
	}
	cold, warm := p.AcquireStats()
	if cold != 1 || warm != 0 {
		t.Fatalf("after first (cold) acquire: cold=%d warm=%d, want 1/0", cold, warm)
	}
	p.Release(c1, nil)

	c2, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire #2: %v", err)
	}
	cold, warm = p.AcquireStats()
	if cold != 1 || warm != 1 {
		t.Fatalf("after second (warm-reuse) acquire: cold=%d warm=%d, want 1/1", cold, warm)
	}
	p.Release(c2, nil)
}
