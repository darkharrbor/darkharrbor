// Command harrbor-dockerproxy is a minimal, purpose-built allowlist proxy in
// front of the Docker Engine API (reached via /var/run/docker.sock). It exists
// because general-purpose socket proxies (e.g. tecnativa/docker-socket-proxy)
// gate ALL state-changing verbs behind one blanket "POST" toggle -- there is no
// way to allow exec-create (a POST) while denying stop/restart/kill/delete
// (also POSTs/DELETEs) using that model. See upstream tecnativa/docker-socket-
// proxy issue #101: ALLOW_STOP/ALLOW_RESTARTS do not restrict anything once
// POST=1 is set.
//
// This proxy instead default-denies everything, allowlists exact read routes,
// and independently validates every exec target and command. No
// third-party proxy image or its undocumented behavior is trusted for this
// security boundary; it's DH's own code, in DH's own repo, MIT-licensed.
//
// Allowlisted surface (the minimum .strm-mode installer needs):
//
//	GET  /v*/containers/json            - list containers (env vars redacted)
//	POST /v*/containers/{id}/exec       - approved fixed operation only
//	POST /v*/exec/{id}/start            - proxy-issued exec ID, once only
//	GET  /v*/exec/{id}/json             - proxy-issued exec ID, then forgotten
//	GET  /v*/events                     - container event stream
//	GET  /version or /v*/version        - sanitized API-version handshake
//
// Every other path/method -- including stop, restart, kill, DELETE on any
// resource, image pull, volumes, networks, swarm, info -- returns 403 without
// ever reaching the socket.
//
// Container inspect is not exposed. List responses discard labels, and event
// responses retain only ID, image, name, type, action, and timestamp.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/dockerpolicy"
)

const (
	envListenAddr            = "HARRBOR_DOCKERPROXY_LISTEN"
	envSocketPath            = "HARRBOR_DOCKERPROXY_SOCKET"
	envTargets               = "HARRBOR_DOCKERPROXY_TARGETS"
	defaultListen            = ":2375"
	defaultSocket            = "/var/run/docker.sock"
	maxContainerListResponse = 8 << 20
)

// allowRule is one (method, path-regex) pair permitted through untouched
// (modulo response redaction, applied separately below).
type allowRule struct {
	method string
	path   *regexp.Regexp
}

var allowlist = []allowRule{
	{"GET", regexp.MustCompile(`^/version$`)},
	{"GET", regexp.MustCompile(`^/v[\d.]*/containers/json$`)},
	{"GET", regexp.MustCompile(`^/v[\d.]*/events$`)},
	{"GET", regexp.MustCompile(`^/v[\d.]*/version$`)},
}

var (
	execCreatePath  = regexp.MustCompile(`^/(v[\d.]*)/containers/([a-zA-Z0-9_.-]+)/exec$`)
	execStartPath   = regexp.MustCompile(`^/v[\d.]*/exec/([a-zA-Z0-9_.-]+)/start$`)
	execInspectPath = regexp.MustCompile(`^/v[\d.]*/exec/([a-zA-Z0-9_.-]+)/json$`)
	containerList   = regexp.MustCompile(`^/v[\d.]*/containers/json$`)
	eventsPath      = regexp.MustCompile(`^/v[\d.]*/events$`)
	versionPath     = regexp.MustCompile(`^/(?:v[\d.]*/)?version$`)
)

func allowed(method, path string) bool {
	for _, r := range allowlist {
		if r.method == method && r.path.MatchString(path) {
			return true
		}
	}
	return false
}

func main() {
	socketPath := os.Getenv(envSocketPath)
	if socketPath == "" {
		socketPath = defaultSocket
	}
	listen := os.Getenv(envListenAddr)
	if listen == "" {
		listen = defaultListen
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{}
			return d.DialContext(ctx, "unix", socketPath)
		},
	}

	handler := newHandler(transport, parseTargets(os.Getenv(envTargets)))

	srv := &http.Server{
		Addr:              listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("harrbor-dockerproxy listening on %s -> %s (default-deny, fixed exec policy, %d approved targets)", listen, socketPath, len(parseTargets(os.Getenv(envTargets))))
	log.Fatal(srv.ListenAndServe())
}

type execCreateRequest struct {
	Cmd          []string `json:"Cmd"`
	User         string   `json:"User,omitempty"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	Tty          bool     `json:"Tty"`
}

type execStartRequest struct {
	Detach bool `json:"Detach"`
	Tty    bool `json:"Tty"`
}

type execRegistry struct {
	mu      sync.Mutex
	started map[string]bool
}

func newHandler(transport http.RoundTripper, targets map[string]struct{}) http.Handler {
	registry := &execRegistry{started: make(map[string]bool)}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(req *httputil.ProxyRequest) {
			req.Out.URL.Scheme = "http"
			req.Out.URL.Host = "docker" // unused by the unix dialer, just needs to be non-empty
		},
		Transport: transport,
		ModifyResponse: func(resp *http.Response) error {
			if resp.Request == nil {
				return nil
			}
			switch {
			case versionPath.MatchString(resp.Request.URL.Path):
				return sanitizeVersion(resp)
			case containerList.MatchString(resp.Request.URL.Path):
				return sanitizeContainerList(resp)
			case eventsPath.MatchString(resp.Request.URL.Path):
				return sanitizeEvents(resp)
			default:
				return nil
			}
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if match := execCreatePath.FindStringSubmatch(r.URL.Path); r.Method == http.MethodPost && match != nil {
			handleExecCreate(w, r, transport, registry, targets, match[1], match[2])
			return
		}
		if match := execStartPath.FindStringSubmatch(r.URL.Path); r.Method == http.MethodPost && match != nil {
			start, ok := decodeExecStart(w, r)
			if !ok {
				return
			}
			if !registry.start(match[1]) {
				deny(w, r, "exec ID was not issued by this proxy or was already started")
				return
			}
			body, _ := json.Marshal(start)
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			proxy.ServeHTTP(w, r)
			return
		}
		if match := execInspectPath.FindStringSubmatch(r.URL.Path); r.Method == http.MethodGet && match != nil {
			if !registry.finish(match[1]) {
				deny(w, r, "exec ID was not issued and started by this proxy")
				return
			}
			proxy.ServeHTTP(w, r)
			return
		}
		if !allowed(r.Method, r.URL.Path) {
			deny(w, r, "path not on allowlist")
			return
		}
		log.Printf("ALLOW %s %s", r.Method, r.URL.Path)
		proxy.ServeHTTP(w, r)
	})
	return mux
}

func decodeExecStart(w http.ResponseWriter, r *http.Request) (execStartRequest, bool) {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	var start execStartRequest
	if err := dec.Decode(&start); err != nil || start.Detach || start.Tty {
		deny(w, r, "invalid exec start request")
		return execStartRequest{}, false
	}
	if err := ensureEOF(dec); err != nil {
		deny(w, r, "invalid exec start request")
		return execStartRequest{}, false
	}
	return start, true
}

func (r *execRegistry) issue(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started[id] = false
}

func (r *execRegistry) start(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	started, ok := r.started[id]
	if !ok || started {
		return false
	}
	r.started[id] = true
	return true
}

func (r *execRegistry) finish(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	started, ok := r.started[id]
	if ok && started {
		delete(r.started, id)
		return true
	}
	return false
}

func deny(w http.ResponseWriter, r *http.Request, reason string) {
	log.Printf("DENY %s %s: %s", r.Method, r.URL.Path, reason)
	http.Error(w, "403 Forbidden -- "+reason, http.StatusForbidden)
}

func handleExecCreate(w http.ResponseWriter, r *http.Request, transport http.RoundTripper, registry *execRegistry, targets map[string]struct{}, version, containerID string) {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	var create execCreateRequest
	if err := dec.Decode(&create); err != nil || create.Tty || !create.AttachStdout || !create.AttachStderr {
		deny(w, r, "invalid exec request")
		return
	}
	if err := ensureEOF(dec); err != nil {
		deny(w, r, "invalid exec request")
		return
	}
	name, image, running, err := inspectTarget(r.Context(), transport, version, containerID)
	if err != nil {
		http.Error(w, "502 Bad Gateway -- target inspection failed", http.StatusBadGateway)
		return
	}
	if _, approved := targets[name]; !approved || !running || !dockerpolicy.AllowedExec(image, create.User, create.Cmd) {
		deny(w, r, "exec target or operation is not approved")
		return
	}
	body, _ := json.Marshal(create)
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	respBody, status, headers, err := roundTrip(r, transport, 1<<20)
	if err != nil {
		http.Error(w, "502 Bad Gateway -- exec create failed", http.StatusBadGateway)
		return
	}
	if status >= 200 && status < 300 {
		var created struct {
			ID string `json:"Id"`
		}
		if json.Unmarshal(respBody, &created) != nil || !regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`).MatchString(created.ID) {
			http.Error(w, "502 Bad Gateway -- invalid exec ID", http.StatusBadGateway)
			return
		}
		registry.issue(created.ID)
	}
	copyResponse(w, status, headers, respBody)
}

func inspectTarget(ctx context.Context, transport http.RoundTripper, version, containerID string) (string, string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/"+version+"/containers/"+containerID+"/json", nil)
	if err != nil {
		return "", "", false, err
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return "", "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", false, fmt.Errorf("inspect status %d", resp.StatusCode)
	}
	var inspected struct {
		Name   string `json:"Name"`
		Config struct {
			Image string `json:"Image"`
		} `json:"Config"`
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&inspected); err != nil {
		return "", "", false, err
	}
	return strings.TrimPrefix(inspected.Name, "/"), inspected.Config.Image, inspected.State.Running, nil
}

func parseTargets(raw string) map[string]struct{} {
	targets := make(map[string]struct{})
	valid := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	for _, value := range strings.Split(raw, ",") {
		value = strings.TrimSpace(value)
		if valid.MatchString(value) {
			targets[value] = struct{}{}
		}
	}
	return targets
}

func ensureEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("trailing data")
	}
	return nil
}

func roundTrip(r *http.Request, transport http.RoundTripper, limit int64) ([]byte, int, http.Header, error) {
	upstream := r.Clone(r.Context())
	upstream.URL.Scheme, upstream.URL.Host = "http", "docker"
	upstream.RequestURI = ""
	resp, err := transport.RoundTrip(upstream)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || int64(len(body)) > limit {
		return nil, 0, nil, fmt.Errorf("upstream response too large")
	}
	return body, resp.StatusCode, resp.Header.Clone(), nil
}

func copyResponse(w http.ResponseWriter, status int, headers http.Header, body []byte) {
	for key, values := range headers {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func sanitizeVersion(resp *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	resp.Body.Close()
	if err != nil {
		return err
	}
	if len(body) > 4096 {
		return fmt.Errorf("sanitize version: response too large")
	}
	var raw struct {
		APIVersion string `json:"ApiVersion"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return fmt.Errorf("sanitize version: %w", err)
	}
	if !regexp.MustCompile(`^1\.[0-9]+$`).MatchString(raw.APIVersion) {
		return fmt.Errorf("sanitize version: invalid API version")
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Del("Content-Length")
	return nil
}

func sanitizeContainerList(resp *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxContainerListResponse+1))
	resp.Body.Close()
	if err != nil {
		return err
	}
	if len(body) > maxContainerListResponse {
		return fmt.Errorf("sanitize container list: response too large")
	}
	var containers []struct {
		ID    string   `json:"Id"`
		Names []string `json:"Names"`
		Image string   `json:"Image"`
		State string   `json:"State"`
	}
	if err := json.Unmarshal(body, &containers); err != nil {
		return fmt.Errorf("sanitize container list: %w", err)
	}
	out, err := json.Marshal(containers)
	if err != nil {
		return err
	}
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Del("Content-Length") // stale from original body; Go sets it from ContentLength
	return nil
}

func sanitizeEvents(resp *http.Response) error {
	upstream := resp.Body
	reader, writer := io.Pipe()
	resp.Body = reader
	resp.ContentLength = -1
	resp.Header.Del("Content-Length")
	go func() {
		defer upstream.Close()
		dec, enc := json.NewDecoder(upstream), json.NewEncoder(writer)
		for {
			var raw struct {
				Type, Action string
				Actor        struct {
					ID         string
					Attributes map[string]string
				}
				Time int64 `json:"time"`
			}
			if err := dec.Decode(&raw); err != nil {
				if err == io.EOF {
					_ = writer.Close()
				} else {
					_ = writer.CloseWithError(err)
				}
				return
			}
			raw.Actor.Attributes = map[string]string{
				"image": raw.Actor.Attributes["image"],
				"name":  raw.Actor.Attributes["name"],
			}
			if err := enc.Encode(raw); err != nil {
				_ = writer.CloseWithError(err)
				return
			}
		}
	}()
	return nil
}
