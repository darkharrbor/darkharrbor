package httpstream

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"syscall"
	"testing"
)

// timeoutErr is net/http's shape for a TLS-handshake or dial timeout: a
// net.Error reporting Timeout() true, reached through a *url.Error wrapper.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "net/http: TLS handshake timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

type plainNetErr struct{}

func (plainNetErr) Error() string   { return "some non-timeout net error" }
func (plainNetErr) Timeout() bool   { return false }
func (plainNetErr) Temporary() bool { return false }

func TestIsConnectLevel(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},

		// The case this exists for: the measured 10s wall, as net/http
		// actually delivers it -- wrapped in *url.Error by the client.
		{"tls handshake timeout via url.Error",
			&url.Error{Op: "Get", URL: "https://cdn.example/x", Err: timeoutErr{}}, true},
		{"bare timeout", timeoutErr{}, true},

		{"dns failure", &net.DNSError{Err: "no such host", Name: "cdn.example"}, true},
		{"dns failure wrapped",
			&url.Error{Op: "Get", URL: "https://cdn.example/x",
				Err: &net.DNSError{Err: "no such host", Name: "cdn.example"}}, true},
		{"connection refused", fmt.Errorf("dial: %w", syscall.ECONNREFUSED), true},
		{"host unreachable", fmt.Errorf("dial: %w", syscall.EHOSTUNREACH), true},
		{"network unreachable", fmt.Errorf("dial: %w", syscall.ENETUNREACH), true},

		// A response the origin actually sent, or an error about the
		// request rather than reachability: the passthrough may still
		// differ, so these must NOT be treated as connect-level.
		{"plain error", errors.New("cdn source: unexpected capability probe status 403"), false},
		{"non-timeout net error", plainNetErr{}, false},
		{"http class error", &Error{Class: ClassBackendUnavailable, Detail: "502"}, false},

		// Client went away: says nothing about the origin (HR3.4).
		{"context canceled", context.Canceled, false},
		{"context canceled wrapped",
			&url.Error{Op: "Get", URL: "https://cdn.example/x", Err: context.Canceled}, false},
		{"context deadline exceeded", context.DeadlineExceeded, false},
		{"context deadline wrapped",
			&url.Error{Op: "Get", URL: "https://cdn.example/x", Err: context.DeadlineExceeded}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsConnectLevel(tc.err); got != tc.want {
				t.Fatalf("IsConnectLevel(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// A context deadline that surfaces as a net.Error timeout must still lose to
// the context check: the ordering inside IsConnectLevel is what guarantees a
// client-side abort is never mistaken for an unreachable origin.
func TestIsConnectLevelContextBeatsTimeout(t *testing.T) {
	err := fmt.Errorf("dial tcp: %w", context.DeadlineExceeded)
	if IsConnectLevel(err) {
		t.Fatal("a wrapped context deadline must not be reported as connect-level")
	}
}
