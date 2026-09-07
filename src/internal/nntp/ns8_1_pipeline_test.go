package nntp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func listenNS81(t *testing.T) (net.Listener, string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	host, portText, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	return ln, host, port
}

func TestPoolPipelinesBODYOnOneConnection(t *testing.T) {
	ln, host, port := listenNS81(t)
	serverErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		w := bufio.NewWriter(conn)
		if _, err := w.WriteString("200 ready\r\n"); err != nil {
			serverErr <- err
			return
		}
		if err := w.Flush(); err != nil {
			serverErr <- err
			return
		}
		ids := make([]string, 4)
		for i := range ids {
			line, err := r.ReadString('\n')
			if err != nil {
				serverErr <- err
				return
			}
			ids[i] = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "BODY "))
		}
		for _, id := range ids {
			if _, err := fmt.Fprintf(w, "222 0 %s\r\npayload-%s\r\n.\r\n", id, id); err != nil {
				serverErr <- err
				return
			}
		}
		serverErr <- w.Flush()
	}()

	pool := NewPool(PoolConfig{
		Host: host, Port: port, MaxConns: 2, PipelineDepth: 4,
		DialTimeout: 2 * time.Second, IdleTimeout: time.Minute,
	}, nil)
	t.Cleanup(pool.Close)

	start := make(chan struct{})
	errs := make(chan error, 4)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			conn, err := pool.Acquire(context.Background())
			if err != nil {
				errs <- err
				return
			}
			lines, err := conn.Body(fmt.Sprintf("synthetic-%d", i))
			pool.Release(conn, err)
			if err == nil && (len(lines) != 1 || !strings.HasPrefix(lines[0], "payload-<synthetic-")) {
				err = fmt.Errorf("unexpected body %q", lines)
			}
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	configured, effective, commands := pool.PipelineSnapshot()
	if configured != 4 || effective != 4 || commands < 3 {
		t.Fatalf("pipeline snapshot = (%d,%d,%d), want (4,4,>=3)", configured, effective, commands)
	}
}

func TestPoolPipelineMissingArticleKeepsConnection(t *testing.T) {
	ln, host, port := listenNS81(t)
	var accepts atomic.Int32
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		accepts.Add(1)
		defer conn.Close()
		r := bufio.NewReader(conn)
		w := bufio.NewWriter(conn)
		_, _ = w.WriteString("200 ready\r\n")
		_ = w.Flush()
		commands := make([]string, 2)
		for i := range commands {
			commands[i], _ = r.ReadString('\n')
		}
		for _, command := range commands {
			if strings.Contains(command, "missing") {
				_, _ = w.WriteString("430 absent\r\n")
			} else {
				_, _ = w.WriteString("222 0 present\r\nok\r\n.\r\n")
			}
		}
		_ = w.Flush()
		if _, err := r.ReadString('\n'); err == nil {
			_, _ = w.WriteString("222 0 reused\r\nreused\r\n.\r\n")
			_ = w.Flush()
		}
	}()

	pool := NewPool(PoolConfig{
		Host: host, Port: port, MaxConns: 1, PipelineDepth: 2,
		DialTimeout: 2 * time.Second, IdleTimeout: time.Minute,
	}, nil)
	t.Cleanup(pool.Close)
	c1, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c2, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	go func() {
		_, err := c1.Body("missing")
		pool.Release(c1, nil)
		errs <- err
	}()
	go func() {
		_, err := c2.Body("present")
		pool.Release(c2, err)
		errs <- err
	}()
	var missing, success bool
	for range 2 {
		err := <-errs
		switch {
		case err == nil:
			success = true
		case errors.Is(err, ErrArticleMissing):
			missing = true
		default:
			t.Fatalf("unexpected BODY error: %v", err)
		}
	}
	if !missing || !success {
		t.Fatalf("pipeline results missing=%v success=%v", missing, success)
	}
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lines, err := conn.Body("reuse")
	pool.Release(conn, err)
	if err != nil || len(lines) != 1 || lines[0] != "reused" {
		t.Fatalf("reused body=%q err=%v", lines, err)
	}
	if accepts.Load() != 1 {
		t.Fatalf("physical connections=%d, want 1", accepts.Load())
	}
	_, effective, _ := pool.PipelineSnapshot()
	if effective != 2 {
		t.Fatalf("missing article disabled pipeline: effective=%d", effective)
	}
}

func TestPipelineFallbackStateIsSharedPerProvider(t *testing.T) {
	state := &pipelineState{}
	first := NewPool(PoolConfig{MaxConns: 1, PipelineDepth: 4, pipelineState: state}, nil)
	second := NewPool(PoolConfig{MaxConns: 1, PipelineDepth: 4, pipelineState: state}, nil)
	first.disablePipelining()
	_, effective, _ := second.PipelineSnapshot()
	if effective != 1 {
		t.Fatalf("sibling pool effective depth = %d, want shared serial fallback", effective)
	}
}

func TestPoolPipelineWireFailureFallsBackToSerial(t *testing.T) {
	ln, host, port := listenNS81(t)
	var accepts atomic.Int32
	serverErr := make(chan error, 2)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			n := accepts.Add(1)
			go func() {
				defer conn.Close()
				r := bufio.NewReader(conn)
				w := bufio.NewWriter(conn)
				if _, err := w.WriteString("200 ready\r\n"); err != nil {
					serverErr <- err
					return
				}
				if err := w.Flush(); err != nil {
					serverErr <- err
					return
				}
				if n == 1 {
					for i := 0; i < 2; i++ {
						if _, err := r.ReadString('\n'); err != nil {
							serverErr <- err
							return
						}
					}
					_, _ = w.WriteString("222 0 partial\r\ntruncated")
					_ = w.Flush()
					return // partial/disconnected dot body.
				}
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					id := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "BODY "))
					if _, err := fmt.Fprintf(w, "222 0 %s\r\nok\r\n.\r\n", id); err != nil {
						serverErr <- err
						return
					}
					if err := w.Flush(); err != nil {
						serverErr <- err
						return
					}
				}
			}()
		}
	}()

	pool := NewPool(PoolConfig{
		Host: host, Port: port, MaxConns: 1, PipelineDepth: 4,
		DialTimeout: 2 * time.Second, IdleTimeout: time.Minute,
	}, nil)
	t.Cleanup(pool.Close)

	c1, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c2, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	for i, conn := range []*Conn{c1, c2} {
		go func(i int, conn *Conn) {
			defer wg.Done()
			_, bodyErr := conn.Body(fmt.Sprintf("fail-%d", i))
			if bodyErr == nil {
				t.Errorf("BODY %d unexpectedly succeeded", i)
			}
			pool.Release(conn, bodyErr)
		}(i, conn)
	}
	wg.Wait()
	_, effective, _ := pool.PipelineSnapshot()
	if effective != 1 {
		t.Fatalf("effective pipeline depth = %d, want serial 1", effective)
	}

	serial, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if extra, err := pool.Acquire(ctx); err == nil {
		pool.Release(extra, nil)
		t.Fatal("serial fallback allowed a second concurrent lease")
	}
	lines, err := serial.Body("serial-ok")
	pool.Release(serial, err)
	if err != nil || len(lines) != 1 || lines[0] != "ok" {
		t.Fatalf("serial fallback body = %q, err=%v", lines, err)
	}
	if accepts.Load() != 2 {
		t.Fatalf("physical connections = %d, want 2", accepts.Load())
	}
}
