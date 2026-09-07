package httpstream

import (
	"context"
	"net"
	"net/http"
	"time"
)

// TransportRole selects the timeout and pooling budget for one outbound HTTP
// workload (HS-3.10/A21). Metadata and probe calls are bounded end to end;
// stream calls have no whole-body deadline and instead use an idle-read
// deadline refreshed on every socket read.
type TransportRole int

const (
	TransportMetadata TransportRole = iota
	TransportAggregateMetadata
	TransportProbe
	TransportStream
)

const (
	metadataRequestTimeout          = 30 * time.Second
	aggregateMetadataRequestTimeout = 2 * time.Minute
	probeRequestTimeout             = 30 * time.Second
	streamIdleReadTimeout           = 2 * time.Minute
)

// NewHTTPClient builds a pooled client on the shared D11 security policy.
// Callers select TrustBackend for configured API origins and TrustSource for
// backend-returned media URLs. The returned client is safe for concurrent use.
func NewHTTPClient(policy *SecurityPolicy, trust Trust, role TransportRole, allowedRedirectOrigins ...string) *http.Client {
	if policy == nil {
		policy = NewSecurityPolicy(false)
	}

	tr := policy.Transport(trust)
	tr.MaxIdleConns = 32
	tr.MaxIdleConnsPerHost = 4
	tr.MaxConnsPerHost = 8
	tr.IdleConnTimeout = 90 * time.Second
	tr.ResponseHeaderTimeout = 30 * time.Second
	tr.TLSHandshakeTimeout = 15 * time.Second
	tr.ExpectContinueTimeout = time.Second
	tr.DisableCompression = true

	client := &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return policy.checkRedirect(trust, req, via, allowedRedirectOrigins...)
		},
	}

	switch role {
	case TransportMetadata:
		client.Timeout = metadataRequestTimeout
	case TransportAggregateMetadata:
		client.Timeout = aggregateMetadataRequestTimeout
		tr.ResponseHeaderTimeout = aggregateMetadataRequestTimeout
	case TransportProbe:
		client.Timeout = probeRequestTimeout
	case TransportStream:
		// A movie-sized response must not have a whole-request deadline. Wrap
		// each accepted socket so a genuinely stalled upstream body cannot
		// hold a connection forever; the deadline is refreshed per Read.
		baseDial := tr.DialContext
		tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := baseDial(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return &idleReadConn{Conn: conn, timeout: streamIdleReadTimeout}, nil
		}
		tr.MaxConnsPerHost = 4
	default:
		client.Timeout = metadataRequestTimeout
	}

	return client
}

type idleReadConn struct {
	net.Conn
	timeout time.Duration
}

func (c *idleReadConn) Read(p []byte) (int, error) {
	if c.timeout > 0 {
		if err := c.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
			return 0, err
		}
	}
	return c.Conn.Read(p)
}
