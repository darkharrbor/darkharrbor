// Package dockerctl is DH's client for cmd/harrbor-dockerproxy -- the
// purpose-built allowlist proxy in front of the Docker Engine API (see that
// package's doc comment for why a third-party socket-proxy image isn't
// used). DH itself never touches /var/run/docker.sock directly; every call
// in this package goes through the proxy's allowlisted route set, so the
// proxy independently verifies the operator-approved container name, its
// inspected image, and the exact command before Docker sees an exec request.
//
// .strm mode is opt-in (D-DOCKER); a Client is only ever constructed when
// that mode is selected. VFS-mode and hardened deployments never import
// this package's constructor path at runtime.
package dockerctl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sync"
	"time"
)

// DefaultBaseURL is where the docker-proxy compose service listens on
// arr-net. Override via Client.BaseURL for tests or non-standard topologies.
const DefaultBaseURL = "http://docker-proxy:2375"

var dockerAPIVersionPattern = regexp.MustCompile(`^1\.[0-9]+$`)

// Client talks to a running harrbor-dockerproxy instance over plain HTTP.
// It never dials /var/run/docker.sock itself. The Docker API version is
// negotiated through the proxy's sanitized /version surface and cached per
// client so older supported engines are not rejected by a hardcoded version.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	versionMu  sync.Mutex
	apiVersion string
}

// New returns a Client pointed at the given base URL (e.g. DefaultBaseURL).
// A zero-value HTTPClient with a bounded timeout is used if httpClient is nil.
func New(baseURL string, httpClient *http.Client) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{BaseURL: baseURL, HTTPClient: httpClient}
}

func (c *Client) version(ctx context.Context) (string, error) {
	c.versionMu.Lock()
	defer c.versionMu.Unlock()
	if c.apiVersion != "" {
		return c.apiVersion, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/version", nil)
	if err != nil {
		return "", fmt.Errorf("dockerctl: build version request: %w", err)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("dockerctl: version: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("dockerctl: version: status %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	var payload struct {
		APIVersion string `json:"ApiVersion"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&payload); err != nil {
		return "", fmt.Errorf("dockerctl: decode version response: %w", err)
	}
	if !dockerAPIVersionPattern.MatchString(payload.APIVersion) {
		return "", fmt.Errorf("dockerctl: invalid Docker API version %q", payload.APIVersion)
	}
	c.apiVersion = "v" + payload.APIVersion
	return c.apiVersion, nil
}

// Container is the subset of the Docker /containers/json list response DH
// needs for arr discovery (Gate 2): identity + enough label/image data to
// match against the arr image/label patterns.
type Container struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	Labels map[string]string `json:"Labels"`
	State  string            `json:"State"`
}

// ExecResult is the outcome of a Client.Exec call: combined stdout+stderr
// (demuxed from the Docker stream-protocol framing) and the exit code
// reported by the subsequent exec-inspect call.
type ExecResult struct {
	Output   string
	ExitCode int
}

// Event is one entry from the Docker Engine API's /events stream. Only the
// fields the events watcher (Gate 4) needs are modeled.
type Event struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
	Time int64 `json:"time"`
}

// EventStream is an open connection to the proxy's /events endpoint. The
// Docker Engine API streams events as back-to-back JSON objects over a
// chunked HTTP response (not a JSON array, not newline-delimited framing --
// just concatenated documents), so Next() decodes one at a time directly off
// the response body via json.Decoder, which handles that framing natively.
type EventStream struct {
	body io.ReadCloser
	dec  *json.Decoder
}

// Next blocks until the next event arrives, the stream ends, or ctx passed
// to Client.Events is canceled (which unblocks the underlying read with a
// context error). Callers should treat any non-nil error as "reconnect" --
// EventStream does not reconnect itself; see internal/eventwatcher for the
// reconnect-with-backoff loop built on top of this.
func (s *EventStream) Next() (Event, error) {
	var e Event
	err := s.dec.Decode(&e)
	return e, err
}

// Close releases the underlying HTTP connection. Safe to call once; callers
// should always call it when done with a stream (including on error from
// Next) to avoid leaking the connection.
func (s *EventStream) Close() error {
	return s.body.Close()
}

// Events opens a streaming connection to the proxy's /events endpoint,
// filtered to container-type events only (the proxy allowlists GET
// /v*/events unfiltered; the filter is applied here as a query param, which
// the Docker Engine API itself honors). The returned EventStream must be
// closed by the caller. ctx governs the lifetime of the whole connection --
// canceling it unblocks any in-flight Next() call.
func (c *Client) Events(ctx context.Context) (*EventStream, error) {
	apiVersion, err := c.version(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/"+apiVersion+"/events?filters="+eventsContainerFilter, nil)
	if err != nil {
		return nil, fmt.Errorf("dockerctl: build events request: %w", err)
	}

	// BUGFIX 2026-07-01: found live, in production, within the first ~15s of
	// actually wiring eventwatcher.Run into cmd/darkharrbor/main.go's real
	// boot sequence for the first time (every prior verification used a
	// short-lived context well under any client Timeout, so this never
	// surfaced). go's http.Client.Timeout bounds the ENTIRE request
	// including reading the response body -- for a long-lived streaming
	// GET like /events, that silently kills the connection after Timeout
	// elapses regardless of ctx, which is supposed to be the sole lifetime
	// control here (see this method's doc comment above, and
	// TestEventsContextCancelUnblocksNext). New()'s default client sets
	// Timeout: 30s, and any caller-supplied client for the rest of Client's
	// (bounded, request/response) methods reasonably has one too -- so
	// Events() uses a shallow copy with Timeout cleared instead of
	// c.HTTPClient directly, preserving the same Transport (connection
	// pooling, proxy settings) for everything else.
	streamClient := *c.HTTPClient
	streamClient.Timeout = 0
	resp, err := streamClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dockerctl: events: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("dockerctl: events: status %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}

	return &EventStream{body: resp.Body, dec: json.NewDecoder(resp.Body)}, nil
}

// eventsContainerFilter is a URL-encoded Docker events filter restricting
// the stream to container-type events (drops image/network/volume/etc.
// noise before it ever reaches DH). Equivalent to filters={"type":["container"]}.
const eventsContainerFilter = `%7B%22type%22%3A%5B%22container%22%5D%7D`

// doJSON issues an HTTP request against the proxy and decodes a JSON
// response into out (if out is non-nil). A non-2xx status is returned as an
// error carrying the response body, since the proxy's own 403 responses
// ("path not on harrbor-dockerproxy allowlist") are plain text, not JSON.
func (c *Client) doJSON(ctx context.Context, method, path string, body any, out any) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("dockerctl: marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	apiVersion, err := c.version(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+"/"+apiVersion+path, reqBody)
	if err != nil {
		return fmt.Errorf("dockerctl: build request: %w", err)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("dockerctl: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("dockerctl: read response body for %s %s: %w", method, path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("dockerctl: %s %s: status %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(respBody))
	}

	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("dockerctl: decode response for %s %s: %w", method, path, err)
		}
	}
	return nil
}

// ListContainers returns the proxy's sanitized inventory: ID, names, image,
// and state only. Labels, mounts, environment, and inspect data are excluded.
func (c *Client) ListContainers(ctx context.Context) ([]Container, error) {
	var out []Container
	if err := c.doJSON(ctx, http.MethodGet, "/containers/json", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// execCreateRequest / execCreateResponse mirror the Docker Engine API's
// /containers/{id}/exec request and response shapes (minimal fields only).
type execCreateRequest struct {
	Cmd          []string `json:"Cmd"`
	User         string   `json:"User,omitempty"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	Tty          bool     `json:"Tty"`
}

type execCreateResponse struct {
	ID string `json:"Id"`
}

type execStartRequest struct {
	Detach bool `json:"Detach"`
	Tty    bool `json:"Tty"`
}

type execInspectResponse struct {
	ExitCode int  `json:"ExitCode"`
	Running  bool `json:"Running"`
}

// Exec transports an operation to the proxy. The proxy rejects unapproved
// container names, images, users, and commands; callers must use the fixed
// builders in internal/dockerpolicy. It blocks until the approved command
// exits, then returns its combined stdout+stderr and exit code.
//
// Tty is always false: the exec is created non-interactively, so Docker
// multiplexes stdout/stderr using the standard 8-byte-header stream framing
// (see demuxStream), not raw bytes.
func (c *Client) Exec(ctx context.Context, containerID, user string, cmd []string) (*ExecResult, error) {
	var created execCreateResponse
	createReq := execCreateRequest{
		Cmd:          cmd,
		User:         user,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          false,
	}
	if err := c.doJSON(ctx, http.MethodPost, "/containers/"+containerID+"/exec", createReq, &created); err != nil {
		return nil, fmt.Errorf("dockerctl: exec create in %s: %w", containerID, err)
	}
	if created.ID == "" {
		return nil, fmt.Errorf("dockerctl: exec create in %s: empty exec ID in response", containerID)
	}

	startReq := execStartRequest{Detach: false, Tty: false}
	b, err := json.Marshal(startReq)
	if err != nil {
		return nil, fmt.Errorf("dockerctl: marshal exec start request: %w", err)
	}
	apiVersion, err := c.version(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/"+apiVersion+"/exec/"+created.ID+"/start", bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("dockerctl: build exec start request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dockerctl: exec start %s: %w", created.ID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("dockerctl: exec start %s: status %d: %s", created.ID, resp.StatusCode, bytes.TrimSpace(body))
	}

	output, err := demuxStream(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("dockerctl: demux exec output for %s: %w", created.ID, err)
	}

	var inspected execInspectResponse
	if err := c.doJSON(ctx, http.MethodGet, "/exec/"+created.ID+"/json", nil, &inspected); err != nil {
		return nil, fmt.Errorf("dockerctl: exec inspect %s: %w", created.ID, err)
	}

	return &ExecResult{Output: output, ExitCode: inspected.ExitCode}, nil
}

// demuxStream decodes the Docker Engine API's multiplexed exec-attach
// stream: a sequence of frames, each an 8-byte header (1 byte stream type --
// 0 stdin / 1 stdout / 2 stderr -- 3 bytes unused, 4-byte big-endian payload
// size) followed by that many bytes of payload. Stdout and stderr are both
// folded into one combined string in frame order; stdin-typed frames (not
// expected on a start response) are skipped defensively rather than erroring.
func demuxStream(r io.Reader) (string, error) {
	br := bufio.NewReader(r)
	var out bytes.Buffer
	header := make([]byte, 8)

	for {
		_, err := io.ReadFull(br, header)
		if err == io.EOF {
			break
		}
		if err != nil {
			return out.String(), fmt.Errorf("read frame header: %w", err)
		}
		size := binary.BigEndian.Uint32(header[4:8])
		if size == 0 {
			continue
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(br, payload); err != nil {
			return out.String(), fmt.Errorf("read frame payload: %w", err)
		}
		streamType := header[0]
		if streamType == 1 || streamType == 2 { // stdout or stderr
			out.Write(payload)
		}
	}
	return out.String(), nil
}
