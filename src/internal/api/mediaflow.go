package api

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
)

const (
	mediaFlowMaxBody = 1 << 20
	// Aggregators post an entire result set in ONE call: real AIOStreams
	// batches of 539 and 631 URLs were observed on 2026-08-16, and the
	// former cap of 200 rejected them wholesale, so every stream for a
	// title was discarded and playback presented as "media cannot be
	// played". The binding bound is mediaFlowMaxBody (1 MiB), which a
	// minimal ~90-byte entry caps near 11k URLs regardless of this value;
	// this limit stays finite so the slice pre-allocation below is bounded.
	mediaFlowMaxURLs          = 4096
	mediaFlowMaxURLBytes      = 16 << 10
	mediaFlowMaxFilenameBytes = 1 << 10
	mediaFlowMaxHeaders       = 64
	mediaFlowMaxHeaderBytes   = 32 << 10
	mediaFlowMaxTicketBytes   = 96 << 10
	mediaFlowMaxConcurrent    = 16
	mediaFlowTicketVersion    = 1
)

var mediaFlowAAD = []byte("darkharrbor-mediaflow-v1")

type mediaFlowGenerateRequest struct {
	MediaFlowProxyURL string                `json:"mediaflow_proxy_url"`
	APIPassword       string                `json:"api_password"`
	URLs              []mediaFlowURLRequest `json:"urls"`
}

type mediaFlowURLRequest struct {
	Endpoint        string            `json:"endpoint"`
	Filename        string            `json:"filename"`
	QueryParams     map[string]string `json:"query_params"`
	DestinationURL  string            `json:"destination_url"`
	RequestHeaders  map[string]string `json:"request_headers"`
	ResponseHeaders map[string]string `json:"response_headers"`
}

type mediaFlowGenerateResponse struct {
	URLs []string `json:"urls"`
}

type mediaFlowTicket struct {
	Version          int               `json:"v"`
	RepresentationID string            `json:"r,omitempty"`
	Identity         *rx95Identity     `json:"i,omitempty"`
	DestinationURL   string            `json:"u"`
	Filename         string            `json:"f,omitempty"`
	RequestHeaders   map[string]string `json:"q,omitempty"`
	ResponseHeaders  map[string]string `json:"s,omitempty"`
}

type mediaFlowRange struct {
	start  int64
	end    int64
	suffix int64
	hasEnd bool
}

func (s *Server) registerMediaFlow(mux *http.ServeMux) {
	mux.Handle("/proxy/ip", getOnly(http.HandlerFunc(s.handleMediaFlowIP)))
	mux.Handle("/generate_urls", postOnly(http.HandlerFunc(s.handleMediaFlowGenerate)))
	s.registerMediaFlowPlayback(mux)
}

// registerMediaFlowPlayback registers ONLY the client-fetched playback route.
// It is deliberately separable from /proxy/ip and /generate_urls, which are
// called by the aggregator over the container network and never by a viewing
// device: exposing playback must not require exposing the primary API, which
// carries qBit, SAB, WebDAV, metrics, debug and administration surfaces and
// reaches sealed credentials. The route authenticates from the AEAD-sealed
// ticket in its own query string, so it is self-contained on any listener.
func (s *Server) registerMediaFlowPlayback(mux *http.ServeMux) {
	mux.Handle("/proxy/stream", http.HandlerFunc(s.handleMediaFlowStream))
}

func (s *Server) mediaFlowEnabled() bool {
	return s != nil && s.cfg != nil && strings.TrimSpace(s.cfg.MediaFlow.Password) != "" && strings.TrimSpace(s.cfg.Server.StreamSecret) != ""
}

func mediaFlowSecretEqual(got, want string) bool {
	gotHash := sha256.Sum256([]byte(got))
	wantHash := sha256.Sum256([]byte(want))
	return hmac.Equal(gotHash[:], wantHash[:])
}

func (s *Server) handleMediaFlowIP(w http.ResponseWriter, r *http.Request) {
	if !s.mediaFlowEnabled() {
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}
	if !mediaFlowSecretEqual(r.URL.Query().Get("api_password"), s.cfg.MediaFlow.Password) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	ip := net.ParseIP(strings.TrimSpace(s.cfg.MediaFlow.PublicIP))
	if ip != nil {
		writeJSON(w, http.StatusOK, map[string]string{"ip": ip.String()})
		return
	}
	if strings.TrimSpace(s.cfg.MediaFlow.PublicIP) != "" || s.publicIPResolver == nil {
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}
	resolved, err := s.publicIPResolver.Resolve(r.Context())
	if err != nil {
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ip": resolved})
}

func (s *Server) handleMediaFlowGenerate(w http.ResponseWriter, r *http.Request) {
	if !s.mediaFlowEnabled() {
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, mediaFlowMaxBody)
	var req mediaFlowGenerateRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil || len(req.URLs) == 0 {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	// Distinguish an oversized batch from malformed JSON: collapsing both
	// into a bare 400 is what made the 2026-08-16 failure unreadable from
	// the aggregator side. No URL, credential or ticket is named.
	if len(req.URLs) > mediaFlowMaxURLs {
		http.Error(w, "Bad Request: too many urls in batch", http.StatusBadRequest)
		return
	}
	if err := ensureJSONEOF(dec); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	password := strings.TrimSpace(req.APIPassword)
	if password == "" {
		password = strings.TrimSpace(req.URLs[0].QueryParams["api_password"])
	}
	if !mediaFlowSecretEqual(password, s.cfg.MediaFlow.Password) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	base, err := mediaFlowStreamBase(s.mediaFlowClientBase())
	if err != nil {
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}
	// RX-9.5 (RD-31): the binding is PER BATCH. One /generate_urls request is
	// one Stremio identity, which is what covers the external addon links that
	// carry no identity marker at all -- 56 of 92 URLs in the captured sample.
	// metadata_id, present only on aggregator-owned URLs, corroborates it.
	batchIdentity, batchBound := s.rx95.Resolve(mediaFlowBatchMetadataID(req.URLs), s.nowUTC())
	urls := make([]string, 0, len(req.URLs))
	for _, candidate := range req.URLs {
		if candidate.Endpoint != "" && candidate.Endpoint != "/proxy/stream" {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		if p := strings.TrimSpace(candidate.QueryParams["api_password"]); p != "" && !mediaFlowSecretEqual(p, s.cfg.MediaFlow.Password) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		// RX-9.1 (RD-30 D5): derive the representation identity from the
		// Stremio coordinate carried by the route so that every ticket ever
		// issued for one episode shares one representation, letting coverage
		// accumulate across sessions and giving continuity a real chain.
		// An absent or unparseable identity MUST fall back to the previous
		// random ID rather than fail playback.
		var representationID string
		var captureIdentity *rx95Identity
		identity, identityErr := mediaFlowStremioIdentityFromRoute(candidate.DestinationURL, s.cfg.HTTPStream.Backends)
		// RD-31 falsified the route as an identity source: the segment read as
		// a coordinate is AIOStreams' metadata_id hash, so identityErr is
		// effectively always non-nil against a real aggregator. The batch
		// binding is the real source; the route path is retained only because
		// a future aggregator could legitimately carry a coordinate there.
		if batchBound && !rx95Contradicted(batchIdentity, candidate.Filename) {
			representationID = rx95RepresentationID(batchIdentity)
			identity := batchIdentity
			captureIdentity = &identity
		} else if identityErr == nil {
			representationID = mediaFlowIdentityRepresentationID(identity)
		}
		// RX-9.5 instrumentation: RD-31 records that the identity is NOT in
		// this payload, and that two prior mechanisms were built on assumed
		// shapes. Emit only the NON-SECRET shape needed to establish a real
		// correlation key -- never the destination URL itself, which carries
		// the aggregator's encrypted store auth, and never the ticket.
		s.logMediaFlowGenerateShape(candidate, representationID != "")
		if representationID == "" {
			var err error
			representationID, err = newMediaFlowRepresentationID()
			if err != nil {
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
		}
		ticket, err := normalizeMediaFlowTicket(mediaFlowTicket{
			Version:          mediaFlowTicketVersion,
			RepresentationID: representationID,
			Identity:         captureIdentity,
			DestinationURL:   candidate.DestinationURL,
			Filename:         candidate.Filename,
			RequestHeaders:   candidate.RequestHeaders,
			ResponseHeaders:  candidate.ResponseHeaders,
		})
		if err != nil {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		sealed, err := sealMediaFlowTicket(s.cfg.Server.StreamSecret, ticket)
		if err != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		u := *base
		q := u.Query()
		q.Set("d", sealed)
		u.RawQuery = q.Encode()
		urls = append(urls, u.String())
	}
	writeJSON(w, http.StatusOK, mediaFlowGenerateResponse{URLs: urls})
}

func (s *Server) handleMediaFlowStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.mediaFlowEnabled() {
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}
	ticket, err := openMediaFlowTicket(s.cfg.Server.StreamSecret, r.URL.Query().Get("d"))
	if err != nil {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	selfOrigin, err := s.unwrapMediaFlowSelfOrigin(&ticket)
	if err != nil {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	ownedDescriptor, ownedErr := mediaFlowOwnedAIORoute(ticket.DestinationURL, s.cfg.HTTPStream.Backends)
	ownedRoute := ownedErr == nil
	if s.mediaFlowSem == nil {
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}
	select {
	case s.mediaFlowSem <- struct{}{}:
		defer func() { <-s.mediaFlowSem }()
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}

	if s.httpGov != nil {
		granted, acquireErr := s.httpGov.Acquire(r.Context(), HTTPSourceGovOp, accountgov.PriorityPlayback, "mediaflow")
		if acquireErr != nil {
			return
		}
		defer granted.Release()
	}

	var requested *mediaFlowRange
	if raw := r.Header.Get("Range"); raw != "" {
		parsed, parseErr := parseMediaFlowRange(raw)
		if parseErr != nil {
			http.Error(w, "Requested Range Not Satisfiable", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		requested = &parsed
	}

	upstream, err := http.NewRequestWithContext(r.Context(), r.Method, ticket.DestinationURL, nil)
	if err != nil {
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	for name, value := range ticket.RequestHeaders {
		if !badRelayHeader(name) {
			upstream.Header.Set(name, value)
		}
	}
	upstream.Header.Set("Accept-Encoding", "identity")
	if requested != nil {
		upstream.Header.Set("Range", r.Header.Get("Range"))
	}

	resp, err := s.httpStreamClient().Do(upstream)
	if err != nil {
		if ownedRoute && r.Context().Err() == nil && s.serveMediaFlowCanonicalFallback(w, r, ticket, requested) {
			return
		}
		if r.Context().Err() == nil {
			// RX-0.6. This error was previously DISCARDED, so an upstream
			// that could not be reached at all was indistinguishable from
			// one that answered badly: on 2026-08-21 every shared-tier
			// stream returned a flat 10.004s 502 with nothing logged here
			// and no request arriving upstream, and the cause (a Docker
			// DNAT rule excluding the container bridge) had to be found by
			// reading the nat table by hand. The destination HOST is logged
			// -- never the full URL, which carries the signed ticket -- so
			// the log names what could not be reached without becoming a
			// credential leak.
			s.log.Warn("mediaflow upstream fetch failed",
				"destination", mediaFlowDestinationHost(ticket.DestinationURL),
				"cause", err)
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
		}
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusPartialContent {
		if err := validateMediaFlowContentRange(resp.Header.Get("Content-Range"), resp.Header.Get("Content-Length"), requested); err != nil {
			if ownedRoute && s.serveMediaFlowCanonicalFallback(w, r, ticket, requested) {
				return
			}
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
			return
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if ownedRoute && r.Context().Err() == nil && s.serveMediaFlowCanonicalFallback(w, r, ticket, requested) {
			return
		}
		if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
			if value := resp.Header.Get("Content-Range"); strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "bytes */") {
				w.Header().Set("Content-Range", value)
			}
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		http.Error(w, http.StatusText(resp.StatusCode), resp.StatusCode)
		return
	}
	geometry, exactGeometry := exactMediaFlowGeometry(resp)
	var canonicalID string
	var canonicalSize int64
	var canonical bool
	if ownedRoute {
		var canonicalErr error
		canonicalID, canonicalSize, canonical, canonicalErr = s.mediaFlowCanonical(r.Context(), ticket.RepresentationID)
		if canonicalErr != nil {
			canonical = false
		}
	}
	if ownedRoute && canonical && (!exactGeometry || geometry.Total != canonicalSize) {
		if s.serveMediaFlowCanonicalFallback(w, r, ticket, requested) {
			return
		}
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}

	copyMediaFlowResponseHeaders(w.Header(), resp.Header)
	for name, value := range ticket.ResponseHeaders {
		w.Header().Set(name, value)
	}
	if ticket.Filename != "" && w.Header().Get("Content-Disposition") == "" {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": ticket.Filename}))
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	bufp := httpStreamCopyBufPool.Get().(*[]byte)
	defer httpStreamCopyBufPool.Put(bufp)
	offset := int64(0)
	if exactGeometry {
		offset = geometry.Offset
	} else if resp.StatusCode == http.StatusPartialContent {
		offset, _ = mediaFlowContentRangeStart(resp.Header.Get("Content-Range"))
	}
	representationID := ticket.RepresentationID
	if canonical {
		representationID = canonicalID
	}
	ctx := r.Context()
	if !selfOrigin {
		ctx = playbackcoverage.WithRepresentation(ctx, representationID)
		if exactGeometry {
			ctx = playbackcoverage.WithDeclaredSize(ctx, geometry.Total)
		}
	}
	if ownedRoute && canonical {
		target, targetOK, targetErr := s.mediaFlowOwnedProposalTarget(r.Context(), ticket.RepresentationID, ownedDescriptor)
		if targetErr != nil {
			s.log.Warn("mediaflow canonical target lookup abstained (non-fatal)", "error", httpstream.Sanitize(targetErr))
		} else if targetOK {
			ctx = playbackcoverage.WithTarget(ctx, target)
		}
	}
	reader := io.Reader(resp.Body)
	var captured *bytes.Buffer
	if ownedRoute && !canonical && exactGeometry && geometry.Length <= mediaFlowMaxProofWindowBytes {
		captured = bytes.NewBuffer(make([]byte, 0, int(geometry.Length)))
		reader = io.TeeReader(resp.Body, captured)
	}
	var captureOnce sync.Once
	var observer playbackBodyObserver
	if !selfOrigin && !canonical && exactGeometry && ticket.Identity != nil && s.rx93CaptureEnabled() {
		identity := *ticket.Identity
		if s.log != nil {
			s.log.Info("rx93: playback target diagnostic",
				"kind", identity.Kind,
				"imdb", identity.IMDb,
				"season", identity.Season,
				"episode", identity.Episode,
				"filename", ticket.Filename,
				"declared_size", geometry.Total,
				"response_length", geometry.Length,
				"response_offset", geometry.Offset)
		}
		_, proposalExists, proposalErr := s.store.GetPlaybackProposal(r.Context(), representationID)
		if proposalErr != nil {
			if r.Context().Err() == nil && s.log != nil {
				s.log.Warn("rx93: proposal lookup failed (non-fatal)", "representation_id", representationID,
					"error", httpstream.Sanitize(proposalErr))
			}
		} else if !proposalExists {
			selectorKey, selectorKeyOK := rx93SelectorCacheKey(representationID, ticket.Filename, geometry.Total)
			if selectorKeyOK {
				if _, cached := s.rx93Selectors.Get(selectorKey, s.nowUTC()); !cached {
					if selected, found, selectErr := s.searchRX93(r.Context(), identity, ticket.Filename, geometry.Total); selectErr != nil {
						if r.Context().Err() == nil && s.log != nil {
							s.log.Warn("rx93: early selector lookup failed (non-fatal)",
								"representation_id", representationID, "error", httpstream.Sanitize(selectErr))
						}
					} else if found {
						s.rx93Selectors.Put(selectorKey, selected, s.nowUTC())
					}
				}
			}
			observer = func(snapshot playbackcoverage.Snapshot, spanOffset, spanLength int64) {
				if !s.rx93ThresholdReached(snapshot.DeliveredBytes, geometry.Total) {
					return
				}
				captureOnce.Do(func() {
					var retained *httpstream.SearchResult
					if selectorKeyOK {
						if selected, cached := s.rx93Selectors.Get(selectorKey, s.nowUTC()); cached {
							retained = &selected
						}
					}
					s.scheduleRX93Capture(representationID, identity, ticket.Filename, geometry.Total, spanOffset, spanLength, retained)
				})
			}
		}
	}
	written, copyErr := s.copyPlaybackBody(ctx, w, reader, *bufp, offset, "mediaflow", observer)
	if captured != nil && copyErr == nil && written == geometry.Length && captured.Len() == int(geometry.Length) {
		if _, err := s.admitMediaFlowOwnedWindow(r.Context(), ticket, ownedDescriptor, geometry, captured.Bytes()); err != nil && r.Context().Err() == nil && s.log != nil {
			s.log.Warn("mediaflow owned-route proof abstained (non-fatal)", "error", httpstream.Sanitize(err))
		}
	}
}

// logMediaFlowGenerateShape is RX-9.5's bounded observation of what the
// aggregator actually sends. It deliberately logs no credential, no ticket,
// and no full destination URL: only the host, the path shape, and the
// metadataId segment, which AIOStreams computes as a hash of its own metadata
// object and which is therefore not a secret. The correlation key for RX-9.5
// must be derived from THIS, not from an assumed URL shape.
func (s *Server) logMediaFlowGenerateShape(candidate mediaFlowURLRequest, identityParsed bool) {
	if s == nil || s.log == nil {
		return
	}
	host, segCount, metadataID, ownedPrefix := "", 0, "", false
	if u, err := httpstream.ValidateURL(candidate.DestinationURL); err == nil {
		host = u.Host
		escaped := u.EscapedPath()
		if strings.HasPrefix(escaped, mediaFlowAIOPlaybackPrefix) {
			ownedPrefix = true
			segments := strings.Split(strings.TrimPrefix(escaped, mediaFlowAIOPlaybackPrefix), "/")
			segCount = len(segments)
			if len(segments) == 5 {
				metadataID = segments[3]
			}
		}
	}
	s.log.Info("rx95: generate_urls shape",
		"dest_host", host,
		"owned_prefix", ownedPrefix,
		"path_segments", segCount,
		"metadata_id", metadataID,
		"filename", candidate.Filename,
		"identity_parsed", identityParsed,
		"request_header_keys", len(candidate.RequestHeaders),
		"query_param_keys", len(candidate.QueryParams))
}

func newMediaFlowRepresentationID() (string, error) {
	value := make([]byte, 24)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return "mediaflow:" + base64.RawURLEncoding.EncodeToString(value), nil
}

type playbackBodyObserver func(playbackcoverage.Snapshot, int64, int64)

const playbackCoverageRetryDelay = 250 * time.Millisecond

func (s *Server) observePlaybackCoverage(ctx context.Context, representationID string, offset, length int64) (playbackcoverage.Snapshot, error) {
	if s.playbackObserve != nil {
		return s.playbackObserve(ctx, representationID, offset, length)
	}
	return s.playbackCoverage.Observe(ctx, representationID, offset, length)
}

func (s *Server) copyPlaybackBody(ctx context.Context, dst io.Writer, src io.Reader, buf []byte, offset int64, operation string, observer playbackBodyObserver) (int64, error) {
	var written int64
	tracking := s.playbackCoverage != nil
	pendingStart := int64(-1)
	var retryAfter time.Time
	var representationID string

	flushCoverage := func(observeCtx context.Context, now time.Time, currentOffset, currentLength int64) {
		if !tracking || currentLength < 0 || (currentLength == 0 && pendingStart < 0) {
			return
		}
		if representationID == "" {
			var ok bool
			representationID, ok = playbackcoverage.Representation(ctx)
			if !ok {
				tracking = false
				return
			}
		}
		if pendingStart >= 0 && now.Before(retryAfter) {
			return
		}
		spanOffset := currentOffset
		spanLength := currentLength
		if pendingStart >= 0 {
			spanOffset = pendingStart
			spanLength = currentOffset + currentLength - pendingStart
		}
		snapshot, err := s.observePlaybackCoverage(observeCtx, representationID, spanOffset, spanLength)
		if err != nil {
			if pendingStart < 0 {
				pendingStart = currentOffset
			}
			retryAfter = now.Add(playbackCoverageRetryDelay)
			if s.log != nil {
				s.log.Warn("delivered-byte observation failed; coverage retry retained (non-fatal)", "operation", operation, "error", httpstream.Sanitize(err))
			}
			return
		}
		pendingStart = -1
		retryAfter = time.Time{}
		if observer != nil {
			observer(snapshot, spanOffset, spanLength)
		}
	}

	for {
		nr, readErr := src.Read(buf)
		if nr > 0 {
			nw, writeErr := dst.Write(buf[:nr])
			if nw > 0 && tracking {
				flushCoverage(ctx, s.nowUTC(), offset, int64(nw))
			}
			offset += int64(nw)
			written += int64(nw)
			if writeErr != nil || nw != nr {
				if writeErr != nil {
					return written, writeErr
				}
				return written, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if pendingStart >= 0 && tracking {
					flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
					retryAfter = time.Time{}
					flushCoverage(flushCtx, s.nowUTC(), offset, 0)
					cancel()
				}
				return written, nil
			}
			return written, readErr
		}
	}
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

// mediaFlowClientBase resolves the origin used to build playback URLs handed to
// VIEWING DEVICES. Server.BaseURL is intentionally private to the container
// network; a client cannot resolve a container name, and the resulting failure
// surfaces at the player as an unexplained "cannot be played" rather than at
// DarkHarrbor. MediaFlow.BaseURL overrides it when set; empty preserves the
// previous behavior exactly for single-network deployments.
// mediaFlowDestinationHost extracts scheme://host[:port] for diagnostics. The
// full destination URL carries signed ticket material and query credentials and
// must never reach a log; the host alone is what identifies an unreachable
// upstream.
func mediaFlowDestinationHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "<unparseable>"
	}
	return u.Scheme + "://" + u.Host
}

func (s *Server) mediaFlowClientBase() string {
	if base := strings.TrimSpace(s.cfg.MediaFlow.BaseURL); base != "" {
		return base
	}
	return s.cfg.Server.BaseURL
}

func mediaFlowStreamBase(raw string) (*url.URL, error) {
	u, err := httpstream.ValidateURL(raw)
	if err != nil {
		return nil, err
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/proxy/stream"
	u.RawQuery = ""
	u.Fragment = ""
	return u, nil
}

func normalizeMediaFlowTicket(ticket mediaFlowTicket) (mediaFlowTicket, error) {
	if ticket.RepresentationID != "" && playbackcoverage.ValidateRepresentationID(ticket.RepresentationID) != nil {
		return mediaFlowTicket{}, errors.New("invalid representation identity")
	}
	if ticket.Identity != nil && (!ticket.Identity.valid() || ticket.RepresentationID != rx95RepresentationID(*ticket.Identity)) {
		return mediaFlowTicket{}, errors.New("invalid Stremio identity")
	}
	ticket.DestinationURL = strings.TrimSpace(ticket.DestinationURL)
	if len(ticket.DestinationURL) == 0 || len(ticket.DestinationURL) > mediaFlowMaxURLBytes {
		return mediaFlowTicket{}, errors.New("destination URL length")
	}
	if _, err := httpstream.ValidateURL(ticket.DestinationURL); err != nil {
		return mediaFlowTicket{}, err
	}
	if len(ticket.Filename) > mediaFlowMaxFilenameBytes {
		return mediaFlowTicket{}, errors.New("filename too long")
	}
	if ticket.Filename != "" {
		ticket.Filename = path.Base(strings.ReplaceAll(ticket.Filename, "\\", "/"))
		if ticket.Filename == "." || strings.ContainsAny(ticket.Filename, "\r\n\x00") {
			return mediaFlowTicket{}, errors.New("invalid filename")
		}
	}
	validated, err := httpstream.ValidateResolvedFile(httpstream.ResolvedFile{
		Selector:       "mediaflow",
		URL:            ticket.DestinationURL,
		RequestHeaders: ticket.RequestHeaders,
	}, time.Time{})
	if err != nil {
		return mediaFlowTicket{}, err
	}
	ticket.RequestHeaders = validated.RequestHeaders
	if err := validateMediaFlowHeaderBudget(ticket.RequestHeaders); err != nil {
		return mediaFlowTicket{}, err
	}
	response := make(map[string]string, len(ticket.ResponseHeaders))
	for rawName, rawValue := range ticket.ResponseHeaders {
		name := http.CanonicalHeaderKey(strings.TrimSpace(rawName))
		if name != "Content-Type" && name != "Content-Disposition" {
			continue
		}
		value := strings.TrimSpace(rawValue)
		if strings.ContainsAny(rawName+value, "\r\n\x00") {
			return mediaFlowTicket{}, errors.New("invalid response header")
		}
		response[name] = value
	}
	if len(response) == 0 {
		ticket.ResponseHeaders = nil
	} else {
		ticket.ResponseHeaders = response
	}
	if err := validateMediaFlowHeaderBudget(ticket.ResponseHeaders); err != nil {
		return mediaFlowTicket{}, err
	}
	return ticket, nil
}

func mediaFlowContentRangeStart(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(strings.ToLower(raw), "bytes ") {
		return 0, errors.New("invalid content range")
	}
	span := strings.SplitN(strings.TrimSpace(raw[len("bytes "):]), "-", 2)
	if len(span) != 2 {
		return 0, errors.New("invalid content range")
	}
	return strconv.ParseInt(strings.TrimSpace(span[0]), 10, 64)
}

func validateMediaFlowHeaderBudget(headers map[string]string) error {
	if len(headers) > mediaFlowMaxHeaders {
		return errors.New("too many headers")
	}
	total := 0
	for name, value := range headers {
		total += len(name) + len(value)
		if total > mediaFlowMaxHeaderBytes {
			return errors.New("headers too large")
		}
	}
	return nil
}

func mediaFlowAEAD(secret string) (cipher.AEAD, error) {
	key := sha256.Sum256([]byte(secret + "\x00mediaflow-ticket"))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func sealMediaFlowTicket(secret string, ticket mediaFlowTicket) (string, error) {
	plain, err := json.Marshal(ticket)
	if err != nil || len(plain) > mediaFlowMaxTicketBytes {
		return "", errors.New("ticket too large")
	}
	aead, err := mediaFlowAEAD(secret)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nonce, nonce, plain, mediaFlowAAD)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func openMediaFlowTicket(secret, encoded string) (mediaFlowTicket, error) {
	if encoded == "" || len(encoded) > base64.RawURLEncoding.EncodedLen(mediaFlowMaxTicketBytes+64) {
		return mediaFlowTicket{}, errors.New("invalid ticket length")
	}
	sealed, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return mediaFlowTicket{}, errors.New("invalid ticket encoding")
	}
	aead, err := mediaFlowAEAD(secret)
	if err != nil || len(sealed) < aead.NonceSize()+aead.Overhead() {
		return mediaFlowTicket{}, errors.New("invalid ticket")
	}
	nonce := sealed[:aead.NonceSize()]
	plain, err := aead.Open(nil, nonce, sealed[aead.NonceSize():], mediaFlowAAD)
	if err != nil {
		return mediaFlowTicket{}, errors.New("invalid ticket")
	}
	var ticket mediaFlowTicket
	dec := json.NewDecoder(strings.NewReader(string(plain)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ticket); err != nil || ensureJSONEOF(dec) != nil || ticket.Version != mediaFlowTicketVersion {
		return mediaFlowTicket{}, errors.New("invalid ticket payload")
	}
	return normalizeMediaFlowTicket(ticket)
}

// unwrapMediaFlowSelfOrigin prevents DH's own Stremio discovery consumer from
// re-entering this proxy. Only the exact local MediaFlow stream shape qualifies;
// every other self-origin destination fails closed.
func (s *Server) unwrapMediaFlowSelfOrigin(ticket *mediaFlowTicket) (bool, error) {
	if s == nil || s.cfg == nil || ticket == nil {
		return false, errors.New("invalid MediaFlow self-origin state")
	}
	destination, err := httpstream.ValidateURL(ticket.DestinationURL)
	if err != nil {
		return false, err
	}
	base, err := mediaFlowStreamBase(s.mediaFlowClientBase())
	if err != nil || !mediaFlowSameOrigin(destination, base) {
		return false, err
	}
	if destination.EscapedPath() != base.EscapedPath() || destination.Fragment != "" || destination.ForceQuery {
		return true, errors.New("invalid MediaFlow self-origin route")
	}
	query, err := url.ParseQuery(destination.RawQuery)
	if err != nil || len(query) != 1 || len(query["d"]) != 1 || query.Get("d") == "" {
		return true, errors.New("invalid MediaFlow self-origin ticket")
	}
	inner, err := openMediaFlowTicket(s.cfg.Server.StreamSecret, query.Get("d"))
	if err != nil {
		return true, err
	}
	innerDestination, err := httpstream.ValidateURL(inner.DestinationURL)
	if err != nil || mediaFlowSameOrigin(innerDestination, base) {
		return true, errors.New("recursive MediaFlow self-origin ticket")
	}
	*ticket = inner
	return true, nil
}

// unwrapMediaFlowResolvedFile removes the MediaFlow envelope when a configured
// aggregator hands the HTTP playback lane one of DH's own proxy URLs. The
// native HTTP observer remains outside this hop and therefore stays the sole
// coverage owner.
func (s *Server) unwrapMediaFlowResolvedFile(file *httpstream.ResolvedFile) (bool, error) {
	if file == nil {
		return false, errors.New("invalid MediaFlow resolved file")
	}
	ticket := mediaFlowTicket{DestinationURL: file.URL}
	selfOrigin, err := s.unwrapMediaFlowSelfOrigin(&ticket)
	if err != nil || !selfOrigin {
		return selfOrigin, err
	}
	file.URL = ticket.DestinationURL
	file.RequestHeaders = ticket.RequestHeaders
	file.ResponseHeaders = ticket.ResponseHeaders
	return true, nil
}

func parseMediaFlowRange(raw string) (mediaFlowRange, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) == 0 || len(raw) > 128 || !strings.HasPrefix(strings.ToLower(raw), "bytes=") || strings.Contains(raw, ",") {
		return mediaFlowRange{}, errors.New("invalid range")
	}
	spec := strings.TrimSpace(raw[len("bytes="):])
	if strings.Count(spec, "-") != 1 {
		return mediaFlowRange{}, errors.New("invalid range")
	}
	parts := strings.SplitN(spec, "-", 2)
	if parts[0] == "" {
		suffix, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || suffix <= 0 {
			return mediaFlowRange{}, errors.New("invalid suffix range")
		}
		return mediaFlowRange{suffix: suffix}, nil
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 {
		return mediaFlowRange{}, errors.New("invalid range start")
	}
	result := mediaFlowRange{start: start}
	if parts[1] == "" {
		return result, nil
	}
	end, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || end < start {
		return mediaFlowRange{}, errors.New("invalid range end")
	}
	result.end, result.hasEnd = end, true
	return result, nil
}

func validateMediaFlowContentRange(raw, contentLength string, want *mediaFlowRange) error {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(strings.ToLower(raw), "bytes ") {
		return errors.New("invalid content range")
	}
	spanTotal := strings.SplitN(strings.TrimSpace(raw[len("bytes "):]), "/", 2)
	if len(spanTotal) != 2 {
		return errors.New("invalid content range")
	}
	span := strings.SplitN(spanTotal[0], "-", 2)
	if len(span) != 2 {
		return errors.New("invalid content range")
	}
	start, err1 := strconv.ParseInt(strings.TrimSpace(span[0]), 10, 64)
	end, err2 := strconv.ParseInt(strings.TrimSpace(span[1]), 10, 64)
	if err1 != nil || err2 != nil || start < 0 || end < start {
		return errors.New("invalid content range span")
	}
	total := int64(0)
	if strings.TrimSpace(spanTotal[1]) != "*" {
		var err error
		total, err = strconv.ParseInt(strings.TrimSpace(spanTotal[1]), 10, 64)
		if err != nil || total <= end {
			return errors.New("invalid content range total")
		}
	}
	if contentLength != "" {
		length, err := strconv.ParseInt(contentLength, 10, 64)
		if err != nil || length != end-start+1 {
			return errors.New("content length mismatch")
		}
	}
	if want == nil {
		return nil
	}
	switch {
	case want.suffix > 0:
		if total == 0 || end != total-1 || end-start+1 > want.suffix {
			return errors.New("suffix range mismatch")
		}
	case start != want.start:
		return errors.New("range start mismatch")
	case want.hasEnd && end > want.end:
		return errors.New("range end mismatch")
	}
	return nil
}

func copyMediaFlowResponseHeaders(dst, src http.Header) {
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
		if value := src.Get(name); value != "" {
			dst.Set(name, value)
		}
	}
}
