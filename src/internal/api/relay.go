package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/mediatruth"
)

// relayRequest is the D-RELAY endpoint's request body (REDESIGN S10 Gate 5).
// The arr-side shim (Gate 6) extracts the .strm URL and its own original
// ffprobe args (minus -i/input, which the shim already located while
// deciding to reroute in the first place), then POSTs both here.
type relayRequest struct {
	// URL is the .strm sidecar's target -- always a DH /stream URL. Validated
	// same-origin against Server.BaseURL before use (see validateRelayURL):
	// this endpoint must never become an arbitrary-URL fetch proxy.
	URL string `json:"url"`
	// Args are the arr's original ffprobe arguments with the input token
	// already removed by the shim. A small denylist (see relayArgDenylist)
	// blocks flags that could let a request override what gets probed or
	// which protocols are reachable, regardless of what URL was validated.
	Args []string `json:"args"`
	// Source is an optional caller-supplied identifier (e.g. the arr's
	// hostname) folded into the audit log line. Never trusted for anything
	// beyond logging.
	Source string `json:"source,omitempty"`
}

// relayResponse carries back exactly what a local ffprobe invocation would
// have produced: real stdout and the real exit code, so the shim can relay
// both to the arr byte-for-byte and exit with the same code.
type relayResponse struct {
	Stdout   string `json:"stdout"`
	ExitCode int    `json:"exit_code"`
}

// relayArgDenylist blocks client-supplied args that could change what gets
// probed or which protocols ffprobe-full is willing to reach, independent of
// the URL same-origin check -- defense in depth, not the only guard.
var relayArgDenylist = map[string]bool{
	"-i":                  true, // input is supplied exactly once, by this handler, from the validated URL
	"-protocol_whitelist": true, // this handler sets its own, deliberately narrower than the arr-side wrapper's (no "file")
}

// relayNetworkArgs are prepended ahead of the caller's args on every
// invocation -- same tuning values as the interim wrapper
// (strm-mode/ffprobe-wrapper.sh), minus "file" from the protocol whitelist:
// the relay only ever fetches a validated http(s) DH URL, so there is no
// legitimate case for ffprobe-full to touch a local path here.
var relayNetworkArgs = []string{
	"-protocol_whitelist", "http,https,tcp,tls",
	"-analyzeduration", "5M",
	"-probesize", "5M",
	"-rw_timeout", "30000000",
}

// registerRelay wires the D-RELAY endpoint into mux. Called from Router().
func (s *Server) registerRelay(mux *http.ServeMux) {
	mux.Handle("/api/v1/probe", postOnly(http.HandlerFunc(s.handleProbeRelay)))
}

// handleProbeRelay implements POST /api/v1/probe (Gate 5 AC):
//   - malformed/oversized bodies rejected before touching the concurrency
//     semaphore
//   - concurrency bounded by s.relaySem (default 4); excess queues behind
//     Relay.QueueTimeout, never an unbounded ffprobe-full fan-out
//   - every call audit-logged without URLs, tokens, headers, or raw arguments
func (s *Server) handleProbeRelay(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.Relay.MaxBodyBytes)
	var req relayRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.log.Warn("relay: malformed request body", "error", err, "remote_addr", r.RemoteAddr)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed or oversized request body"})
		return
	}

	if err := validateRelayURL(req.URL, s.cfg.Server.BaseURL); err != nil {
		s.log.Warn("relay: rejected url", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := validateRelayArgs(req.Args); err != nil {
		s.log.Warn("relay: rejected args", "arg_count", len(req.Args), "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	// Passive cache check: DH never probes proactively (tried that
	// 2026-07-02, reverted the same day -- see HANDOFF: Sonarr already
	// probes each file exactly once at import and never again, so there
	// was no redundant work to eliminate, and having DH probe too just
	// raced Sonarr for no benefit and, once, caused a real panic). This
	// cache exists purely to skip a REPEAT live probe -- e.g. a rescan, or
	// a MediaInfo-version-triggered mass refresh -- for a file whose exact
	// (item, file, args) combination was already answered once before.
	// SF-05: one stable cache key for both URL shapes. /stream URLs keep
	// their historical key exactly; /dav (NZB) URLs — previously uncacheable,
	// which let a broken multi-file pack cost a full live NNTP re-probe of
	// every file on every arr import retry — now resolve to the owning item.
	cacheItemID, cacheFileID, cacheKeyOK := s.resolveRelayCacheKey(r.Context(), req.URL)

	if s.store != nil {
		if itemID, fileID, ok := cacheItemID, cacheFileID, cacheKeyOK; ok {
			argsKey := relayArgsKey(req.Args)
			if cached, found, err := s.store.GetFileProbe(r.Context(), itemID, fileID, argsKey); err != nil {
				s.log.Warn("relay: cache lookup failed (non-fatal, falling back to live probe)",
					"item_id", itemID, "file_id", fileID, "error", err)
			} else if found {
				s.log.Info("relay: probe served from cache",
					"item_id", itemID, "file_id", fileID)
				writeJSON(w, http.StatusOK, relayResponse{Stdout: cached, ExitCode: 0})
				return
			}
			// Content-identity fallback: a re-grab of the same torrent gets a
			// fresh item id, so the direct lookup above can never hit — but a
			// probe stored under ANY prior item with the same info_hash is
			// byte-identical content. Serve it and promote it under the new
			// item id so the next lookup is direct. (Live failure 2026-07-07:
			// a re-grabbed 24-episode season pack sat in "importing" for ~15
			// minutes re-probing episodes DH had already probed cold.)
			if item, ierr := s.store.GetItemByID(r.Context(), itemID); ierr == nil &&
				item != nil && item.InfoHash != nil && *item.InfoHash != "" {
				if cached, found, cerr := s.store.GetFileProbeByContent(r.Context(), *item.InfoHash, fileID, argsKey); cerr != nil {
					s.log.Warn("relay: content-cache lookup failed (non-fatal)",
						"info_hash", *item.InfoHash, "file_id", fileID, "error", cerr)
				} else if found {
					if perr := s.store.SetFileProbe(r.Context(), itemID, fileID, argsKey, cached); perr != nil {
						s.log.Warn("relay: content-cache promote failed (non-fatal)",
							"item_id", itemID, "file_id", fileID, "error", perr)
					}
					s.log.Info("relay: probe served from content cache",
						"item_id", itemID, "file_id", fileID, "info_hash", *item.InfoHash)
					writeJSON(w, http.StatusOK, relayResponse{Stdout: cached, ExitCode: 0})
					return
				}
			}
		}
	}

	// SF-05: negative cache — a live (unexpired) deterministic prior failure
	// is replayed without any upstream byte movement, bounding the provider
	// cost of an arr's infinite import-retry of a broken release. Disabled
	// with HARRBOR_RELAY_NEGATIVE_TTL_MIN=0.
	if s.store != nil && s.cfg.Relay.NegativeTTLMin > 0 && cacheKeyOK {
		if stdout, exitCode, hits, found, err := s.store.GetFileProbeFailure(r.Context(), cacheItemID, cacheFileID, relayArgsKey(req.Args)); err != nil {
			s.log.Warn("relay: negative cache lookup failed (non-fatal, falling back to live probe)",
				"item_id", cacheItemID, "file_id", cacheFileID, "error", err)
		} else if found {
			s.log.Info("relay: probe failure served from negative cache",
				"item_id", cacheItemID, "file_id", cacheFileID, "exit_code", exitCode, "hits", hits)
			writeJSON(w, http.StatusOK, relayResponse{Stdout: stdout, ExitCode: exitCode})
			return
		}
	}

	// SF-04: serve from already-persisted HR5.2 media truth when this exact
	// file has it, before ever touching the live-probe path below. This is
	// checked after both positive-probe caches (a real prior probe is
	// always preferred over a synthesized one) and the negative cache (a
	// known deterministic failure must not be masked by stale facts), so it
	// only ever fires for a file that has never been probed live or
	// negatively cached yet -- exactly the case that otherwise pays for a
	// live network ffprobe-full exec (and its timeout/error noise) on
	// every cold Arr import.
	if s.tryServeFromMediaTruth(w, r, cacheItemID, cacheFileID, cacheKeyOK, req.Args) {
		return
	}

	select {
	case s.relaySem <- struct{}{}:
		defer func() { <-s.relaySem }()
	case <-time.After(s.cfg.Relay.QueueTimeout):
		itemID, fileID, _ := parseStreamPath(req.URL)
		s.log.Warn("relay: queue timeout, concurrency exhausted",
			"item_id", itemID, "file_id", fileID, "concurrency", s.cfg.Relay.Concurrency)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "relay busy, try again"})
		return
	case <-r.Context().Done():
		return
	}

	stdout, exitCode, stderr, runErr := s.runFFprobeFull(r.Context(), req.URL, req.Args, relayMaxProbeBytes)

	// One automatic, bounded retry when the capped first attempt's own
	// result looks incomplete (see probeResultMissingAudio and
	// relayEscalatedProbeBytes) -- transparent to the caller, so a file
	// whose audio track sits past the 10M window still imports
	// automatically instead of needing the arr's manual-import fallback.
	if runErr == nil && exitCode == 0 {
		if missing, parseable := probeResultMissingAudio(stdout); parseable && missing {
			itemID, fileID, _ := parseStreamPath(req.URL)
			s.log.Info("relay: capped probe found no audio, retrying with a larger window",
				"item_id", itemID, "file_id", fileID, "cap_bytes", relayEscalatedProbeBytes)
			if rStdout, rExitCode, rStderr, rErr := s.runFFprobeFull(r.Context(), req.URL, req.Args, relayEscalatedProbeBytes); rErr == nil {
				stdout, exitCode, stderr = rStdout, rExitCode, rStderr
			} else {
				s.log.Warn("relay: escalated probe retry failed (non-fatal, using capped result)",
					"item_id", itemID, "file_id", fileID, "error", rErr)
			}
		}
	}

	duration := time.Since(started)

	itemID, fileID, _ := parseStreamPath(req.URL)
	logArgs := []any{
		"item_id", itemID,
		"file_id", fileID,
		"arg_count", len(req.Args),
		"exit_code", exitCode,
		"duration", duration.String(),
		"error", errString(runErr),
	}
	if exitCode != 0 || runErr != nil {
		// ffprobe stderr may echo its signed input URL. Preserve only its size.
		logArgs = append(logArgs, "stderr_bytes", len(stderr))
	}
	s.log.Info("relay: probe", logArgs...)

	if runErr != nil && exitCode == 0 {
		// A non-exec error (couldn't even start the process, or a hard
		// timeout with no exit code recovered) -- this is a relay-level
		// failure, not "ffprobe ran and reported a problem", so it gets an
		// HTTP error status rather than a 200 with a synthetic exit code.
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "ffprobe-full invocation failed: " + runErr.Error()})
		return
	}

	// SF-05: cache a deterministic failure (ffprobe genuinely ran and judged
	// the bytes; identical bytes give an identical verdict) so the next
	// identical request costs zero upstream bytes until the TTL lapses.
	// Relay-level failures were already returned above and are never cached.
	if exitCode != 0 && runErr == nil && s.store != nil && s.cfg.Relay.NegativeTTLMin > 0 && cacheKeyOK {
		ttl := time.Duration(s.cfg.Relay.NegativeTTLMin) * time.Minute
		if setErr := s.store.SetFileProbeFailure(r.Context(), cacheItemID, cacheFileID, relayArgsKey(req.Args), exitCode, stdout, ttl); setErr != nil {
			s.log.Warn("relay: negative cache populate failed (non-fatal)",
				"item_id", cacheItemID, "file_id", cacheFileID, "error", setErr)
		}
	}

	// Populate the passive cache from this real (Sonarr-triggered) result
	// so an exact repeat request -- not a proactive DH probe, just the
	// same request happening twice -- hits cache next time.
	if exitCode == 0 && runErr == nil && s.store != nil {
		if itemID, fileID, ok := cacheItemID, cacheFileID, cacheKeyOK; ok {
			argsKey := relayArgsKey(req.Args)
			if setErr := s.store.SetFileProbe(r.Context(), itemID, fileID, argsKey, stdout); setErr != nil {
				s.log.Warn("relay: cache populate failed (non-fatal)", "item_id", itemID, "file_id", fileID, "error", setErr)
			}
		}
	}

	writeJSON(w, http.StatusOK, relayResponse{Stdout: stdout, ExitCode: exitCode})
}

// tryServeFromMediaTruth answers a probe request instantly from an item's
// already-persisted HR5.2 media facts (SF-04), writing exactly the same
// response shape a live probe would produce and populating the existing
// positive probe cache so a repeat request behaves identically either way.
// Returns false (writing nothing) whenever facts are absent, belong to a
// different file, or the store is unavailable -- the caller falls through
// to the live probe path unchanged; this method never blocks correctness,
// only skips unnecessary live network work.
func (s *Server) tryServeFromMediaTruth(w http.ResponseWriter, r *http.Request, itemID, fileID string, cacheKeyOK bool, args []string) bool {
	if s.store == nil || !cacheKeyOK || itemID == "" || fileID == "" {
		return false
	}
	item, err := s.store.GetItemByID(r.Context(), itemID)
	if err != nil || item == nil {
		return false
	}
	facts := item.Metadata.MediaFacts
	if facts == nil || facts.Empty() {
		return false
	}
	// Exact-file binding, not "the item has some facts": a multi-file
	// release must never serve one file's facts under another file's probe
	// request (the same mistake class TS-1.4's cachedFiles[0] bug made
	// once). item.Metadata.MediaFactsFileID is set atomically alongside
	// MediaFacts by store.SetMediaFacts, so a mismatch here means the facts
	// on record describe a different file than the one being probed.
	if item.Metadata.MediaFactsFileID != fileID {
		return false
	}

	stdout := mediatruth.SynthesizeFFprobeJSON(*facts)
	argsKey := relayArgsKey(args)
	if setErr := s.store.SetFileProbe(r.Context(), itemID, fileID, argsKey, stdout); setErr != nil {
		s.log.Warn("relay: media-truth cache populate failed (non-fatal)",
			"item_id", itemID, "file_id", fileID, "error", setErr)
	}
	s.log.Info("relay: probe served from media truth", "item_id", itemID, "file_id", fileID)
	writeJSON(w, http.StatusOK, relayResponse{Stdout: stdout, ExitCode: 0})
	return true
}

// parseStreamPath extracts (item_id, file_id) from a DH /stream/{id}/{file}
// URL -- the shape every .strm this endpoint is ever asked to probe points
// at (already validated same-origin by validateRelayURL before this is
// called). Returns ok=false for anything else (e.g. a stale pre-rename
// pre-rename hostname URL, or a WebDAV NZB URL, which this cache does not
// cover) so the caller treats it as uncacheable rather than guessing.
func parseStreamPath(rawURL string) (itemID, fileID string, ok bool) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "stream" || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// resolveRelayCacheKey maps a validated relay URL to a stable probe-cache
// key (SF-05). /stream/{item}/{file} URLs keep their historical key shape
// exactly, so pre-existing cached rows continue to hit. /dav NZB URLs are
// resolved to the owning ready item via the same store lookup the WebDAV
// handler uses, keyed by the stable served file name.
func (s *Server) resolveRelayCacheKey(ctx context.Context, rawURL string) (itemID, fileID string, ok bool) {
	if itemID, fileID, ok = parseStreamPath(rawURL); ok {
		return itemID, fileID, true
	}
	category, relDir, fileName, davOK := parseDavRelayPath(rawURL)
	if !davOK || s.store == nil {
		return "", "", false
	}
	item, err := s.store.FindNZBItemByStrmDir(ctx, filepath.Join(s.cfg.Data.Root, category, relDir))
	if err != nil || item == nil {
		return "", "", false
	}
	return item.ID, "dav:" + fileName, true
}

// parseDavRelayPath extracts (category, release, file) from a DH
// /dav/{category}/{release}/{file} URL, applying the same path-part
// validation as the WebDAV handler. ok=false for any other shape.
func parseDavRelayPath(rawURL string) (category, relDir, fileName string, ok bool) {
	u, err := url.Parse(rawURL)
	if err != nil || !strings.HasPrefix(u.Path, "/dav/") {
		return "", "", "", false
	}
	parts := strings.SplitN(strings.TrimPrefix(u.Path, "/dav/"), "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	if !validateWebDAVPathParts(parts) {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// relayArgsKey builds a deterministic cache key from an ffprobe args slice.
// Order-sensitive and exact -- a strict-equality key, not normalized/sorted
// -- so a lookup only ever hits for a genuinely identical invocation (e.g.
// a retry with a bigger -probesize/-analyzeduration correctly misses and
// falls back to live rather than risk serving output in the wrong shape).
func relayArgsKey(args []string) string {
	return strings.Join(args, "\x1f")
}

// validateRelayURL enforces that url is http(s) and same-origin with
// baseURL (Server.BaseURL, i.e. DH's own configured address) -- the SSRF
// guard. This endpoint exists to probe DH's own /stream URLs on the shim's
// behalf, never as a general-purpose authenticated fetch-any-URL proxy for
// whatever's reachable from arr-net.
func validateRelayURL(rawURL, baseURL string) error {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return errors.New("url is required")
	}
	if !sameSchemeAndHost(rawURL, baseURL) {
		return errors.New("url must be same-origin with this server's configured base URL")
	}
	return nil
}

// validateRelayArgs rejects any arg on relayArgDenylist. Empty/nil args are
// fine -- the shim may have nothing beyond the standard probe flags to add.
func validateRelayArgs(args []string) error {
	for _, a := range args {
		if relayArgDenylist[a] {
			return errors.New("arg not permitted: " + a)
		}
	}
	return nil
}

// runFFprobeFull execs ffprobe-full with the network/tuning args, the
// caller's (validated) args capped to probeCapBytes, then -i <url> last.
// Returns stdout, the real exit code when the process ran to completion
// (successfully or not), and a non-nil error only for relay-level failures
// (process couldn't start, or a hard timeout the process didn't self-report
// an exit code for).
func (s *Server) runFFprobeFull(ctx context.Context, url string, args []string, probeCapBytes int64) (stdout string, exitCode int, stderr string, err error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Relay.Timeout)
	defer cancel()

	argv := make([]string, 0, len(relayNetworkArgs)+len(args)+2)
	argv = append(argv, relayNetworkArgs...)
	argv = append(argv, capProbeArgsTo(args, probeCapBytes)...)
	argv = append(argv, "-i", markProbeURL(url))

	cmd := exec.CommandContext(ctx, s.cfg.Relay.FFprobeFullPath, argv...) // #nosec G204 -- FFprobeFullPath is server config, not request input; argv is validated (denylist) and url is same-origin-validated.
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := cmd.Run()
	stdout = outBuf.String()
	stderr = errBuf.String()

	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		return stdout, 0, stderr, nil
	case ctx.Err() != nil:
		// BUGFIX 2026-07-01 (found live during a real bulk multi-season
		// grab: All in the Family S02/S03/S04/S09, ~98 episodes at once):
		// this branch MUST be checked before the errors.As(exitErr) branch
		// below, not after. exec.CommandContext kills the process with
		// SIGKILL on timeout; on Linux a signal-killed process's error
		// STILL satisfies errors.As(..., *exec.ExitError) (it wraps a real
		// os.ProcessState), and ExitError.ExitCode() returns -1 for a
		// signal-terminated process -- so with the branches in the other
		// order, every relay timeout was being misclassified as "ffprobe
		// ran and produced a real result: exit_code=-1, err=nil" instead of
		// a relay-level failure. handleProbeRelay only 502s when runErr !=
		// nil, so a nil err here meant every timeout was returned as HTTP
		// 200 with exit_code=-1 and empty/truncated stdout -- the shim
		// decoded that as "success" and passed exit_code=-1 (bash: 255) and
		// no valid ffprobe JSON straight to Sonarr, which is exactly what
		// left dozens of real episodes stuck in Sonarr's importPending/
		// importing state under concurrent load (each relay call sharing
		// Relay.Concurrency's small semaphore plus Relay.Timeout, both too
		// tight for a 98-episode burst -- see the config bump below this
		// commit). Now unconditionally treated as a real relay-level
		// timeout regardless of what exec reports for the killed process.
		return stdout, 0, stderr, errors.New("timed out after " + s.cfg.Relay.Timeout.String())
	case errors.As(runErr, &exitErr):
		// ffprobe-full ran to completion and exited non-zero on its own --
		// a real result, not a relay failure. Relay it exactly as a local
		// invocation would report. (ctx.Err() is nil here, so this is not
		// the timeout-kill case above.)
		return stdout, exitErr.ExitCode(), stderr, nil
	default:
		return stdout, 0, stderr, runErr
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// relayMaxProbeBytes caps the caller-supplied -probesize/-analyzeduration on
// the FIRST attempt at every cold probe. ffprobe applies the LAST occurrence
// of a repeated option, so the caller's args (appended after
// relayNetworkArgs) override the relay's own 5M — a Sonarr import sends
// -probesize 50000000, which forced every cold probe to pull up to 50MB
// through /stream (~37s each at proxy throughput; a 24-episode pack spent
// ~15 minutes "importing"). 10M matches the value the Jellyfin ffmpeg
// wrapper has used for the same reason since S10 and is ample for
// container/codec/duration detection on the overwhelming majority of
// strm-backed media.
const relayMaxProbeBytes = 10_000_000

// relayEscalatedProbeBytes is the ONE automatic retry ceiling used only when
// the capped first attempt's own result looks incomplete (see
// probeResultMissingAudio) — e.g. a container whose audio track index sits
// past the 10M window (found live via REL-08 against a genuine Internet
// Archive title: Radarr's own default args got capped to 10M, missed the
// audio track, and permanently rejected the release with "No audio tracks
// detected" even though the file has real AAC audio, confirmed by an
// uncapped probe). This still bounds the cost: it only fires for the rare
// file whose first probe found no audio at all, never on every cold import,
// so it cannot reproduce the bulk-import slowdown relayMaxProbeBytes exists
// to prevent. Set above Sonarr's own historical 50M default so this retry is
// never a smaller window than what triggered that original incident.
const relayEscalatedProbeBytes = 60_000_000

// capProbeArgs returns a copy of args with any -probesize/-analyzeduration
// value above relayMaxProbeBytes lowered to it -- the default, first-attempt
// cap. capProbeArgsTo generalizes this to an arbitrary ceiling, used by the
// escalated retry.
func capProbeArgs(args []string) []string {
	return capProbeArgsTo(args, relayMaxProbeBytes)
}

// capProbeArgsTo returns a copy of args with any -probesize/-analyzeduration
// value above limit lowered to it. The probe-cache args key is built from
// the ORIGINAL caller args (before capping), so cache identity is
// unaffected. Values that fail to parse are left untouched — ffprobe is the
// authority on its own syntax (suffixed forms like "5M" are already ≤ cap in
// practice and pass through).
func capProbeArgsTo(args []string, limit int64) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i := 0; i+1 < len(out); i++ {
		switch out[i] {
		case "-probesize", "-analyzeduration":
			if v, err := strconv.ParseInt(out[i+1], 10, 64); err == nil && v > limit {
				out[i+1] = strconv.FormatInt(limit, 10)
			}
			i++
		}
	}
	return out
}

// probeResultMissingAudio reports whether a successful ffprobe JSON result
// (-print_format/-of json, what every current Sonarr/Radarr sends) declares
// zero audio streams -- the specific, narrow signal that a capped probe may
// have been truncated before reaching a real audio track, not proof of it
// (a genuinely silent/video-only source also matches). parseable=false for
// anything that doesn't decode as the expected shape (a different -of, or a
// non-JSON/empty body), so callers never retry based on a format they can't
// actually read -- the existing capped result is used as-is in that case,
// same as before this function existed.
func probeResultMissingAudio(stdout string) (missing, parseable bool) {
	var parsed struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
		} `json:"streams"`
	}
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil || parsed.Streams == nil {
		return false, false
	}
	for _, st := range parsed.Streams {
		if st.CodecType == "audio" {
			return false, true
		}
	}
	return true, true
}

// markProbeURL appends probe=1 to a validated DH stream URL so the /stream
// handler can distinguish an import-time ffprobe from real playback (skip
// play-marking + serve via CDN redirect instead of a rangecache session).
// Applied after same-origin validation; on any parse failure the URL is
// passed through unmarked — the probe still works, just as a normal stream.
func markProbeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	q.Set("probe", "1")
	u.RawQuery = q.Encode()
	return u.String()
}
