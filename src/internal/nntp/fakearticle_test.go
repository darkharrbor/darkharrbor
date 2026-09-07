package nntp

import (
	"bufio"
	"net"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeArticleServer is a minimal NNTP server that speaks enough of the
// protocol for Pool.dial plus real BODY/STAT exchanges: greeting, AUTHINFO
// USER/PASS, and a per-message-id scripted BODY/STAT response.
//
// It exists for NS-1.1's DG-05 cases, which need to distinguish a
// definitive 430 "no such article" answer (permanent-here) from an
// ordinary transient failure, and to count how many BODY/STAT commands and
// how many TCP connections a fetch actually costs. NS-6.1 reuses it
// unmodified for its own STAT-only health-audit DG-05 cases via the
// additive statReply map below.
type fakeArticleServer struct {
	// bodyReply maps a message-id (without angle brackets) to the raw
	// status line the server answers a BODY command with. A "222" reply is
	// followed by a one-line dot-terminated body. A message-id absent from
	// the map is answered "430 no such article".
	bodyReply map[string]string

	// statReply maps a message-id to the raw status line the server
	// answers a STAT command with (e.g. "223 0 <id> article exists"). A
	// message-id absent from the map is answered "430 no such article".
	// Nil (the zero value) makes every STAT answer 430, matching bodyReply's
	// existing default-missing convention.
	statReply map[string]string

	// bodyCommands/statCommands count every BODY/STAT command received.
	bodyCommands int32
	statCommands int32
	// connections counts every TCP connection the server accepted.
	connections int32

	ln net.Listener
}

func startFakeArticleServer(t *testing.T, bodyReply map[string]string) *fakeArticleServer {
	t.Helper()
	return startFakeArticleServerWithStat(t, bodyReply, nil)
}

func startFakeArticleServerWithStat(t *testing.T, bodyReply, statReply map[string]string) *fakeArticleServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeArticleServer{bodyReply: bodyReply, statReply: statReply, ln: ln}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			atomic.AddInt32(&s.connections, 1)
			go s.serve(conn)
		}
	}()
	return s
}

func (s *fakeArticleServer) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte("200 ready\r\n")); err != nil {
		return
	}
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(cmd)

		switch {
		case strings.HasPrefix(upper, "AUTHINFO USER"):
			_, _ = c.Write([]byte("381 need password\r\n"))
		case strings.HasPrefix(upper, "AUTHINFO PASS"):
			_, _ = c.Write([]byte("281 authenticated\r\n"))
		case strings.HasPrefix(upper, "BODY"):
			atomic.AddInt32(&s.bodyCommands, 1)
			id := strings.TrimSpace(strings.TrimPrefix(cmd, "BODY"))
			id = strings.TrimPrefix(id, "<")
			id = strings.TrimSuffix(id, ">")
			reply, ok := s.bodyReply[id]
			if !ok {
				reply = "430 no such article"
			}
			_, _ = c.Write([]byte(reply + "\r\n"))
			if strings.HasPrefix(reply, "222") {
				_, _ = c.Write([]byte("=ybegin line=128 size=3 name=test.bin\r\nklm\r\n=yend size=3 pcrc32=a3830348\r\n.\r\n"))
			}
		case strings.HasPrefix(upper, "STAT"):
			atomic.AddInt32(&s.statCommands, 1)
			id := strings.TrimSpace(strings.TrimPrefix(cmd, "STAT"))
			id = strings.TrimPrefix(id, "<")
			id = strings.TrimSuffix(id, ">")
			reply, ok := s.statReply[id]
			if !ok {
				reply = "430 no such article"
			}
			_, _ = c.Write([]byte(reply + "\r\n"))
		case strings.HasPrefix(upper, "QUIT"):
			_, _ = c.Write([]byte("205 bye\r\n"))
			return
		default:
			_, _ = c.Write([]byte("500 unknown command\r\n"))
		}
	}
}

func (s *fakeArticleServer) bodyCount() int  { return int(atomic.LoadInt32(&s.bodyCommands)) }
func (s *fakeArticleServer) statCount() int  { return int(atomic.LoadInt32(&s.statCommands)) }
func (s *fakeArticleServer) connCount() int  { return int(atomic.LoadInt32(&s.connections)) }
func (s *fakeArticleServer) address() string { return s.ln.Addr().String() }
