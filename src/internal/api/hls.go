package api

// HR4.2-HR4.6: the first real consumer of HR4.1's internal/hlssession graph.
// GET /hls/{resource_id} resolves an opaque DH resource ID, re-derives its
// current upstream URL, fetches it, and — for Master/Media resources —
// rewrites it via internal/hls, minting child resources (Media/Subtitle
// under a master; Segment/Map/Key/Subtitle under a media playlist) so a
// player only ever sees further opaque DH URLs (HR-D6, LC-02, DG-04).
//
// Nothing here is cached across requests: every GET re-derives the live
// URL and re-fetches/re-parses the manifest. This is deliberate — the
// persisted Session/Resource graph (itemID/fileID, and each resource's
// Reference: parent resource ID + ordinal + byte range) is already enough
// to reproduce the identical rewrite deterministically, which is exactly
// what makes a resource ID "restart-resumable" (HR4.1) without DarkHarrbor
// ever retaining an upstream URL or manifest body itself.
//
// Segment/Map/Key resources stream through the same TrustSource transport.
// Subtitle playlists remain in the HLS graph; direct subtitle payloads are
// registered with HR5.5's shared sidecar owner before delivery. One
// pre-header expiry/failure may force a coalesced same-selector refresh and
// re-derive the identical opaque resource through its persisted ordinal.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/hls"
	"github.com/darkharrbor/darkharrbor/internal/hlssession"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
	"github.com/darkharrbor/darkharrbor/internal/sidecar"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// hlsMaxWalkDepth bounds recursive parent re-derivation (DG-07: a corrupt
// or (impossibly, since mint always points strictly at an existing parent)
// cyclic chain can never cause unbounded recursion). Real HLS nests at most
// two levels (master -> media -> segment), so this is generous headroom,
// not a real-world ceiling.
const hlsMaxWalkDepth = 8

// hlsFetchTimeout bounds how long a single manifest fetch may take. Reuses
// the same order of magnitude as the rest of the HTTP lane's metadata/probe
// calls (httpstream.TransportProbe), not a new timeout class.
const hlsFetchTimeout = 30 * time.Second

var errHLSRangeUnsatisfiable = errors.New("hls: range unsatisfiable")

// registerHLS wires the HLS resource-serving route.
func (s *Server) registerHLS(mux *http.ServeMux) {
	mux.HandleFunc("/hls/{resource_id}", s.handleHLSResource)
}

// CreateHLSPlaybackSession mints a new hlssession.Session plus its root
// resource for one resolved (item, file) representation, and returns the
// opaque path a player should be given as its master/media playlist URL.
// It performs exactly one manifest fetch (to classify the root as a master
// or media playlist so the root resource's persisted Kind matches reality
// from the start) — every subsequent GET of that resource re-fetches
// independently; nothing from this call is cached.
func (s *Server) CreateHLSPlaybackSession(ctx context.Context, itemID, fileID string) (playURL string, err error) {
	if s.hlsSessions == nil {
		return "", errors.New("hls: session graph not wired")
	}
	sess, err := s.hlsSessions.CreateSession(ctx, itemID, fileID)
	if err != nil {
		return "", err
	}
	body, err := hlsFetchWithRefresh(ctx, s, sess, hlssession.Resource{},
		func(source hlsLiveSource) (string, error) {
			return s.fetchHLSPlaylistSource(ctx, source)
		})
	if err != nil {
		return "", err
	}
	var kind hlssession.ResourceKind
	switch {
	case hls.IsMaster(body):
		kind = hlssession.KindMaster
	case hls.IsMedia(body):
		kind = hlssession.KindMedia
	default:
		return "", httpstream.NewError(httpstream.ClassUpstreamMalformed, "not a recognizable HLS playlist")
	}
	root, err := s.hlsSessions.RegisterResource(ctx, sess.SessionID, kind, hlssession.Reference{
		Handler: rootHandlerTag,
	})
	if err != nil {
		return "", err
	}
	return hlsResourcePath(root.ResourceID, root.Kind, ""), nil
}

// rootHandlerTag is a fixed, opaque, non-empty placeholder satisfying
// hlssession.Reference's required Handler field. The root resource's real
// re-derivation path (deriveHLSRootURL) goes through the owning item's own
// persisted ResolveKey, never through this field — it exists only to
// satisfy validateReference's non-empty-opaque-token requirement.
const rootHandlerTag = "hls-root"

func (s *Server) handleHLSResource(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.hlsSessions == nil {
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}
	rawResourceID := r.PathValue("resource_id")
	resourceID, _, _ := strings.Cut(rawResourceID, ".")
	ctx := r.Context()

	res, err := s.hlsSessions.Resolve(ctx, resourceID)
	if err != nil {
		if errors.Is(err, hlssession.ErrResourceNotFound) {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		s.log.Warn("hls: resolve failed", "error", httpstream.Sanitize(err))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if strings.HasSuffix(rawResourceID, ".steering.json") {
		if res.Kind != hlssession.KindMaster {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		s.serveHLSContentSteering(w, r, res)
		return
	}

	switch res.Kind {
	case hlssession.KindMaster, hlssession.KindMedia:
		s.serveHLSPlaylistResource(w, r, res)
	case hlssession.KindSubtitle:
		s.serveHLSSubtitleResource(w, r, res)
	case hlssession.KindSegment, hlssession.KindMap, hlssession.KindKey:
		s.serveHLSLeafResource(w, r, res)
	default:
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (s *Server) serveHLSPlaylistResource(w http.ResponseWriter, r *http.Request, res hlssession.Resource) {
	ctx := r.Context()
	sess, err := s.hlsSessions.Session(ctx, res.SessionID)
	if err != nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	directives, err := parseHLSDeliveryDirectives(r.URL.Query())
	if err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	body, err := hlsFetchWithRefresh(ctx, s, sess, res,
		func(source hlsLiveSource) (string, error) {
			source, err = withHLSDeliveryDirectives(source, directives)
			if err != nil {
				return "", err
			}
			return s.fetchHLSPlaylistSource(ctx, source)
		})
	if err != nil {
		s.hlsUnavailable(w, err)
		return
	}

	detectedMaster := hls.IsMaster(body)
	detectedMedia := hls.IsMedia(body)
	if (res.Kind == hlssession.KindMaster && !detectedMaster) || (res.Kind == hlssession.KindMedia && !detectedMedia && detectedMaster) {
		s.hlsUnavailable(w, httpstream.NewError(httpstream.ClassUpstreamMalformed, "hls resource kind no longer matches upstream content"))
		return
	}

	var rewritten string
	switch {
	case detectedMaster:
		rewritten, err = s.rewriteHLSPlaylist(ctx, sess, res, body, true)
	case detectedMedia:
		rewritten, err = s.rewriteHLSPlaylist(ctx, sess, res, body, false)
	default:
		err = httpstream.NewError(httpstream.ClassUpstreamMalformed, "not a recognizable HLS playlist")
	}
	if err != nil {
		s.hlsUnavailable(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.Itoa(len(rewritten)))
		return
	}
	_, _ = io.WriteString(w, rewritten)
}

type hlsSteeringFetch struct {
	body       []byte
	status     int
	retryAfter string
}

func (s *Server) serveHLSContentSteering(w http.ResponseWriter, r *http.Request, res hlssession.Resource) {
	ctx := r.Context()
	sess, err := s.hlsSessions.Session(ctx, res.SessionID)
	if err != nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	fetched, err := hlsFetchWithRefresh(ctx, s, sess, res, func(masterSource hlsLiveSource) (hlsSteeringFetch, error) {
		master, fetchErr := s.fetchHLSPlaylistSource(ctx, masterSource)
		if fetchErr != nil {
			return hlsSteeringFetch{}, fetchErr
		}
		tag, found, parseErr := hls.ParseContentSteering(master)
		if parseErr != nil || !found {
			return hlsSteeringFetch{}, httpstream.NewError(httpstream.ClassUpstreamMalformed, "content steering tag unavailable")
		}
		steeringURL, resolveErr := hls.ResolveURIRef(masterSource.URL, tag.ServerURI)
		if resolveErr != nil {
			return hlsSteeringFetch{}, resolveErr
		}
		steeringSource := hlsLiveSource{
			URL:            steeringURL,
			Headers:        headersForHLSChild(masterSource, steeringURL),
			RefreshContext: masterSource.RefreshContext,
		}
		result, steeringErr := s.fetchHLSContentSteering(ctx, steeringSource)
		if steeringErr != nil || result.status != http.StatusOK {
			return result, steeringErr
		}
		manifest, parseErr := hls.ParseSteeringManifest(result.body)
		if parseErr != nil {
			return hlsSteeringFetch{}, httpstream.NewError(httpstream.ClassUpstreamMalformed, "invalid steering manifest")
		}
		pathways, parseErr := hls.MasterPathwayIDs(master)
		if parseErr != nil {
			return hlsSteeringFetch{}, httpstream.NewError(httpstream.ClassUpstreamMalformed, "invalid steering master")
		}
		candidates := make([]hls.PathwayCandidate, 0, len(pathways))
		for _, id := range pathways {
			candidates = append(candidates, hls.PathwayCandidate{ID: id, OriginSupplied: true})
		}
		result.body, parseErr = hls.SynthesizeSteeringManifest(manifest, candidates, func(ids []string) []string {
			return s.rankHLSPathways(sess.SessionID, ids)
		})
		if parseErr != nil {
			return hlsSteeringFetch{}, httpstream.NewError(httpstream.ClassUpstreamMalformed, "steering has no available pathway")
		}
		return result, nil
	})
	if err != nil {
		s.hlsUnavailable(w, err)
		return
	}
	if fetched.status == http.StatusGone {
		http.Error(w, "Gone", http.StatusGone)
		return
	}
	if fetched.status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", fetched.retryAfter)
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.steering-list")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(fetched.body)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(fetched.body)
}

func (s *Server) fetchHLSContentSteering(ctx context.Context, source hlsLiveSource) (hlsSteeringFetch, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, hlsFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, source.URL, nil)
	if err != nil {
		return hlsSteeringFetch{}, httpstream.NewError(httpstream.ClassUpstreamMalformed, "invalid steering url")
	}
	for name, value := range source.Headers {
		req.Header.Set(name, value)
	}
	resp, err := hlsHTTPClient(s.httpProbeHTTPClient(), source).Do(req)
	if err != nil {
		return hlsSteeringFetch{}, httpstream.NewError(httpstream.ClassBackendUnavailable, "steering fetch failed")
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusGone:
		return hlsSteeringFetch{status: http.StatusGone}, nil
	case http.StatusTooManyRequests:
		return hlsSteeringFetch{status: http.StatusTooManyRequests, retryAfter: boundedRetryAfter(resp.Header.Get("Retry-After"))}, nil
	case http.StatusOK:
	default:
		return hlsSteeringFetch{}, httpstream.NewError(httpstream.ClassBackendUnavailable, fmt.Sprintf("steering fetch status %d", resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, hls.MaxSteeringBytes+1))
	if err != nil || len(body) > hls.MaxSteeringBytes {
		return hlsSteeringFetch{}, httpstream.NewError(httpstream.ClassUpstreamMalformed, "steering body invalid")
	}
	return hlsSteeringFetch{body: body, status: http.StatusOK}, nil
}

func boundedRetryAfter(value string) string {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds < 1 {
		return "60"
	}
	if seconds > hls.MaxSteeringTTL {
		seconds = hls.MaxSteeringTTL
	}
	return strconv.Itoa(seconds)
}

// parseHLSDeliveryDirectives accepts only the three query parameters reserved
// by LL-HLS. Other client query parameters are ignored rather than forwarded;
// unknown _HLS_ directives and malformed values fail closed.
func parseHLSDeliveryDirectives(values url.Values) (url.Values, error) {
	var out url.Values
	for name, entries := range values {
		if !strings.HasPrefix(name, "_HLS_") {
			continue
		}
		if len(entries) != 1 {
			return nil, errors.New("hls: duplicate delivery directive")
		}
		value := entries[0]
		switch name {
		case "_HLS_msn", "_HLS_part":
			if value == "" {
				return nil, errors.New("hls: empty delivery directive")
			}
			for _, r := range value {
				if r < '0' || r > '9' {
					return nil, errors.New("hls: invalid delivery directive")
				}
			}
			if _, err := strconv.ParseInt(value, 10, 64); err != nil {
				return nil, errors.New("hls: delivery directive overflow")
			}
		case "_HLS_skip":
			if value != "YES" && value != "v2" {
				return nil, errors.New("hls: invalid skip directive")
			}
		default:
			return nil, errors.New("hls: unknown delivery directive")
		}
		if out == nil {
			out = make(url.Values)
		}
		out.Set(name, value)
	}
	if out.Get("_HLS_part") != "" && out.Get("_HLS_msn") == "" {
		return nil, errors.New("hls: part directive requires media sequence")
	}
	return out, nil
}

func withHLSDeliveryDirectives(source hlsLiveSource, directives url.Values) (hlsLiveSource, error) {
	if len(directives) == 0 {
		return source, nil
	}
	u, err := url.Parse(source.URL)
	if err != nil {
		return hlsLiveSource{}, httpstream.NewError(httpstream.ClassUpstreamMalformed, "invalid manifest url")
	}
	query := u.Query()
	for name, entries := range directives {
		query.Set(name, entries[0])
	}
	u.RawQuery = query.Encode()
	source.URL = u.String()
	return source, nil
}

func (s *Server) serveHLSSubtitleResource(w http.ResponseWriter, r *http.Request, res hlssession.Resource) {
	ctx := r.Context()
	sess, err := s.hlsSessions.Session(ctx, res.SessionID)
	if err != nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	if s.sidecars == nil {
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}
	directives, err := parseHLSDeliveryDirectives(r.URL.Query())
	if err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	filename := res.ResourceID + ".subtitle"
	existing, err := s.sidecars.List(ctx, sess.ItemID, sess.FileID)
	if err != nil {
		s.hlsUnavailable(w, err)
		return
	}
	for _, resource := range existing {
		if resource.Kind == sidecar.KindSubtitle && resource.Filename == filename {
			serveSidecarContent(w, r, resource)
			return
		}
	}

	fetched, err := hlsFetchWithRefresh(ctx, s, sess, res,
		func(source hlsLiveSource) (hlsBoundedResource, error) {
			source, err = withHLSDeliveryDirectives(source, directives)
			if err != nil {
				return hlsBoundedResource{}, err
			}
			body, mediaType, fetchErr := s.fetchHLSBoundedResource(ctx, source)
			return hlsBoundedResource{body: body, mediaType: mediaType}, fetchErr
		})
	if err != nil {
		s.hlsUnavailable(w, err)
		return
	}
	body, mediaType := fetched.body, fetched.mediaType
	text := string(body)
	if hls.IsMedia(text) {
		rewritten, rewriteErr := s.rewriteHLSPlaylist(ctx, sess, res, text, false)
		if rewriteErr != nil {
			s.hlsUnavailable(w, rewriteErr)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(rewritten)))
			return
		}
		_, _ = io.WriteString(w, rewritten)
		return
	}
	resource, err := s.sidecars.Register(ctx, sidecar.Input{
		ItemID:    sess.ItemID,
		FileID:    sess.FileID,
		Kind:      sidecar.KindSubtitle,
		Filename:  filename,
		MediaType: mediaType,
		Bytes:     body,
	})
	if err != nil {
		s.hlsUnavailable(w, err)
		return
	}
	serveSidecarContent(w, r, resource)
}

func (s *Server) serveHLSLeafResource(w http.ResponseWriter, r *http.Request, res hlssession.Resource) {
	ctx := r.Context()
	sess, err := s.hlsSessions.Session(ctx, res.SessionID)
	if err != nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	fixed := res.Reference.ByteStart >= 0 && res.Reference.ByteEnd >= res.Reference.ByteStart
	var logicalSize int64
	var clientRange *httpstream.ByteRange
	if fixed {
		logicalSize = res.Reference.ByteEnd - res.Reference.ByteStart + 1
		var satisfiable bool
		clientRange, satisfiable = httpstream.ParseRange(r.Header.Get("Range"), logicalSize)
		if !satisfiable {
			w.Header().Set("Content-Range", httpstream.UnsatisfiedContentRange(logicalSize))
			http.Error(w, "Requested Range Not Satisfiable", http.StatusRequestedRangeNotSatisfiable)
			return
		}
	}

	method := r.Method
	refreshContext := ""
	for attempt := 0; attempt < 2; attempt++ {
		source, attemptErr := s.deriveHLSLiveSourceAttempt(ctx, sess, res, 0, attempt > 0, refreshContext)
		if source.RefreshContext != "" {
			refreshContext = source.RefreshContext
		}
		if attemptErr != nil {
			if attempt == 0 && hlsShouldRefresh(ctx, attemptErr) {
				continue
			}
			if ctx.Err() == nil {
				s.hlsUnavailable(w, attemptErr)
			}
			return
		}
		if method == http.MethodHead && fixed {
			writeHLSLeafHeaders(w, res.Kind, source.URL, logicalSize)
			return
		}

		var upstreamRange *httpstream.ByteRange
		if fixed {
			upstreamRange = &httpstream.ByteRange{Start: res.Reference.ByteStart, End: res.Reference.ByteEnd}
			if clientRange != nil {
				upstreamRange.Start += clientRange.Start
				upstreamRange.End = res.Reference.ByteStart + clientRange.End
			}
		} else if rawRange := r.Header.Get("Range"); rawRange != "" {
			var total int64
			upstreamRange, total, attemptErr = s.parseOpenHLSRange(ctx, source, rawRange)
			if errors.Is(attemptErr, errHLSRangeUnsatisfiable) {
				if total >= 0 {
					w.Header().Set("Content-Range", httpstream.UnsatisfiedContentRange(total))
				}
				http.Error(w, "Requested Range Not Satisfiable", http.StatusRequestedRangeNotSatisfiable)
				return
			}
			if attemptErr != nil {
				if attempt == 0 && hlsShouldRefresh(ctx, attemptErr) {
					continue
				}
				if ctx.Err() == nil {
					s.hlsUnavailable(w, attemptErr)
				}
				return
			}
		}

		req, requestErr := http.NewRequestWithContext(ctx, method, source.URL, nil)
		if requestErr != nil {
			attemptErr = httpstream.NewError(httpstream.ClassUpstreamMalformed, "invalid hls resource url")
		} else {
			for name, value := range source.Headers {
				req.Header.Set(name, value)
			}
			if upstreamRange != nil {
				req.Header.Set("Range", httpstream.UpstreamRangeHeader(*upstreamRange))
			}
		}
		if attemptErr != nil {
			if attempt == 0 && hlsShouldRefresh(ctx, attemptErr) {
				continue
			}
			s.hlsUnavailable(w, attemptErr)
			return
		}

		started := s.nowUTC()
		resp, fetchErr := hlsHTTPClient(s.httpStreamClient(), source).Do(req)
		firstByte := s.nowUTC().Sub(started)
		if fetchErr != nil {
			attemptErr = httpstream.NewError(httpstream.ClassBackendUnavailable, "hls resource fetch failed")
			s.observeHLSPathway(res.SessionID, res.Reference.PathwayID, started, firstByte, 0, false, attemptErr)
			if attempt == 0 && hlsShouldRefresh(ctx, attemptErr) {
				continue
			}
			if ctx.Err() == nil {
				s.hlsUnavailable(w, attemptErr)
			}
			return
		}
		if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
			_ = resp.Body.Close()
			s.observeHLSPathway(res.SessionID, res.Reference.PathwayID, started, firstByte, 0, false,
				httpstream.NewError(httpstream.ClassRepresentationLost, "hls range rejected"))
			if logicalSize > 0 {
				w.Header().Set("Content-Range", httpstream.UnsatisfiedContentRange(logicalSize))
			}
			http.Error(w, "Requested Range Not Satisfiable", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		switch {
		case upstreamRange != nil &&
			resp.StatusCode == http.StatusOK &&
			upstreamRange.Start == 0 &&
			resp.ContentLength == upstreamRange.Length():
			// Some origins ignore Range for tiny whole-object resources such
			// as AES keys. Accept only an exact full-object response beginning
			// at byte zero; partial or ambiguous 200 responses still fail closed.
		case upstreamRange != nil && resp.StatusCode != http.StatusPartialContent:
			attemptErr = httpstream.NewError(httpstream.ClassUpstreamMalformed, "hls ranged resource did not return 206")
		case upstreamRange != nil:
			_, attemptErr = httpstream.ValidateUpstream206(resp.Header.Get("Content-Range"), *upstreamRange)
			if attemptErr == nil && resp.ContentLength >= 0 && resp.ContentLength != upstreamRange.Length() {
				attemptErr = httpstream.NewError(httpstream.ClassUpstreamMalformed, "hls ranged resource length mismatch")
			}
		case resp.StatusCode != http.StatusOK:
			attemptErr = httpstream.NewError(httpstream.ClassBackendUnavailable, fmt.Sprintf("hls resource fetch status %d", resp.StatusCode))
		}
		if attemptErr != nil {
			_ = resp.Body.Close()
			s.observeHLSPathway(res.SessionID, res.Reference.PathwayID, started, firstByte, 0, false, attemptErr)
			if attempt == 0 && hlsShouldRefresh(ctx, attemptErr) {
				continue
			}
			s.hlsUnavailable(w, attemptErr)
			return
		}

		defer resp.Body.Close()
		length := resp.ContentLength
		status := resp.StatusCode
		if fixed {
			length = logicalSize
			status = http.StatusOK
			if clientRange != nil {
				length = clientRange.Length()
				status = http.StatusPartialContent
				w.Header().Set("Content-Range", httpstream.ContentRange(*clientRange, logicalSize))
			}
		} else if upstreamRange != nil {
			w.Header().Set("Content-Range", resp.Header.Get("Content-Range"))
		}
		writeHLSLeafHeaders(w, res.Kind, source.URL, length)
		w.WriteHeader(status)
		if method == http.MethodHead {
			s.observeHLSPathway(res.SessionID, res.Reference.PathwayID, started, firstByte, 0, upstreamRange != nil, nil)
			return
		}
		if length >= 0 {
			n, copyErr := io.CopyN(w, resp.Body, length)
			if copyErr != nil {
				copyErr = httpstream.NewError(httpstream.ClassBackendUnavailable, "hls resource body truncated")
			}
			s.observeHLSPathway(res.SessionID, res.Reference.PathwayID, started, firstByte, n, upstreamRange != nil, copyErr)
			s.observeHLSMediaCoverage(ctx, sess, res, r.Header.Get("Range"), n, copyErr)
			return
		}
		n, copyErr := io.Copy(w, resp.Body)
		if copyErr != nil {
			copyErr = httpstream.NewError(httpstream.ClassBackendUnavailable, "hls resource body failed")
		}
		s.observeHLSPathway(res.SessionID, res.Reference.PathwayID, started, firstByte, n, upstreamRange != nil, copyErr)
		s.observeHLSMediaCoverage(ctx, sess, res, r.Header.Get("Range"), n, copyErr)
		return
	}
}

func (s *Server) observeHLSMediaCoverage(ctx context.Context, sess hlssession.Session, res hlssession.Resource, clientRange string, deliveredBytes int64, copyErr error) {
	ref := res.Reference
	if s.playbackCoverage == nil || res.Kind != hlssession.KindSegment || clientRange != "" || deliveredBytes <= 0 || copyErr != nil || !ref.HasMediaTime {
		return
	}
	identityCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	representationID := s.playbackRepresentationID(identityCtx, &store.Item{ID: sess.ItemID, SourceType: store.SourceTypeHTTP}, sess.FileID)
	if representationID == "" {
		return
	}
	if ref.HasMediaDuration && ref.MediaDurationNS > 0 {
		identityCtx = playbackcoverage.WithTarget(identityCtx, playbackcoverage.Target{
			ItemID: sess.ItemID, FileID: sess.FileID,
			Kind: playbackcoverage.ExtentVODHLS, Total: ref.MediaDurationNS,
		})
	}
	if _, err := s.playbackCoverage.ObserveMedia(identityCtx, representationID, ref.MediaStartNS, ref.MediaEndNS); err != nil {
		s.log.Warn("hls: playback media coverage unavailable (non-fatal)", "error", httpstream.Sanitize(err))
	}
}

func (s *Server) observeHLSPathway(
	sessionID string,
	pathwayID string,
	started time.Time,
	firstByte time.Duration,
	bytes int64,
	rangeSuccess bool,
	err error,
) {
	scoreID := hlsPathwayScoreID(sessionID, pathwayID)
	if s.httpOriginScores == nil || scoreID == "" {
		return
	}
	s.httpOriginScores.Observe(scoreID, httpstream.OriginObservation{
		FirstByte: firstByte, Duration: s.nowUTC().Sub(started), Bytes: bytes,
		RangeSuccess: rangeSuccess, Err: err,
	})
}

func (s *Server) rankHLSPathways(sessionID string, pathwayIDs []string) []string {
	if s.httpOriginScores == nil {
		return append([]string(nil), pathwayIDs...)
	}
	keys := make([]string, 0, len(pathwayIDs))
	byKey := make(map[string]string, len(pathwayIDs))
	for _, pathwayID := range pathwayIDs {
		key := hlsPathwayScoreID(sessionID, pathwayID)
		if key == "" {
			continue
		}
		keys = append(keys, key)
		byKey[key] = pathwayID
	}
	ranked := s.httpOriginScores.Rank(keys)
	out := make([]string, 0, len(ranked))
	for _, key := range ranked {
		out = append(out, byKey[key])
	}
	return out
}

func hlsPathwayScoreID(sessionID, pathwayID string) string {
	if !hlssession.ValidSessionID(sessionID) || !hls.ValidPathwayID(pathwayID) {
		return ""
	}
	return "hls:" + sessionID + ":" + pathwayID
}

func (s *Server) parseOpenHLSRange(ctx context.Context, source hlsLiveSource, value string) (*httpstream.ByteRange, int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, -1, errHLSRangeUnsatisfiable
	}
	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
		return nil, -1, errHLSRangeUnsatisfiable
	}
	parts := strings.SplitN(strings.TrimPrefix(value, "bytes="), "-", 2)
	if len(parts) != 2 || (parts[0] == "" && parts[1] == "") {
		return nil, -1, errHLSRangeUnsatisfiable
	}
	if parts[0] != "" && parts[1] != "" {
		start, err1 := strconv.ParseInt(parts[0], 10, 64)
		end, err2 := strconv.ParseInt(parts[1], 10, 64)
		if err1 != nil || err2 != nil || start < 0 || end < start {
			return nil, -1, errHLSRangeUnsatisfiable
		}
		return &httpstream.ByteRange{Start: start, End: end}, -1, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, source.URL, nil)
	if err != nil {
		return nil, -1, httpstream.NewError(httpstream.ClassUpstreamMalformed, "invalid hls resource url")
	}
	for name, headerValue := range source.Headers {
		req.Header.Set(name, headerValue)
	}
	resp, err := hlsHTTPClient(s.httpStreamClient(), source).Do(req)
	if err != nil {
		return nil, -1, httpstream.NewError(httpstream.ClassBackendUnavailable, "hls resource head failed")
	}
	resp.Body.Close()
	total := resp.ContentLength
	if resp.StatusCode != http.StatusOK {
		return nil, total, httpstream.NewError(httpstream.ClassBackendUnavailable, fmt.Sprintf("hls resource head status %d", resp.StatusCode))
	}
	if total < 0 {
		return nil, total, httpstream.NewError(httpstream.ClassUpstreamMalformed, "hls resource head omitted length")
	}
	if total == 0 {
		return nil, total, errHLSRangeUnsatisfiable
	}
	if parts[0] == "" {
		suffix, parseErr := strconv.ParseInt(parts[1], 10, 64)
		if parseErr != nil || suffix <= 0 {
			return nil, total, errHLSRangeUnsatisfiable
		}
		if suffix > total {
			suffix = total
		}
		return &httpstream.ByteRange{Start: total - suffix, End: total - 1}, total, nil
	}
	start, parseErr := strconv.ParseInt(parts[0], 10, 64)
	if parseErr != nil || start < 0 || start >= total {
		return nil, total, errHLSRangeUnsatisfiable
	}
	return &httpstream.ByteRange{Start: start, End: total - 1}, total, nil
}

func writeHLSLeafHeaders(w http.ResponseWriter, kind hlssession.ResourceKind, rawURL string, length int64) {
	mediaType := hlsLeafMediaType(kind, rawURL)
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Accept-Ranges", "bytes")
	if length >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	}
}

func hlsLeafMediaType(kind hlssession.ResourceKind, rawURL string) string {
	if kind == hlssession.KindKey {
		return "application/octet-stream"
	}
	u, err := url.Parse(rawURL)
	if err == nil {
		if mediaType := mime.TypeByExtension(path.Ext(u.Path)); mediaType != "" {
			return mediaType
		}
	}
	return "application/octet-stream"
}

func (s *Server) rewriteHLSPlaylist(
	ctx context.Context,
	sess hlssession.Session,
	parent hlssession.Resource,
	body string,
	master bool,
) (string, error) {
	s.hlsGraphMu.Lock()
	defer s.hlsGraphMu.Unlock()
	mint, cleanup, err := s.hlsMintFunc(ctx, sess, parent)
	if err != nil {
		return "", err
	}
	var rewritten string
	if master {
		body, err = hls.RewriteContentSteering(body, "/hls/"+parent.ResourceID+".steering.json")
		if err == nil {
			rewritten, err = hls.RewriteMaster(body, mint)
		}
	} else {
		rewritten, err = hls.RewriteMedia(body, mint)
	}
	if err != nil {
		return "", err
	}
	if err := cleanup(); err != nil {
		return "", err
	}
	return rewritten, nil
}

// hlsMintFunc returns the RewriteMaster/RewriteMedia callback for `parent`
// (the resource currently being served/rewritten). Existing children are
// loaded once up front so repeated fetches of the same resource — a
// reconnect, a restart, a player's own manifest re-fetch — reuse the SAME
// opaque child resource IDs rather than minting duplicates each time
// (bounded session state, DG-07).
func (s *Server) hlsMintFunc(ctx context.Context, sess hlssession.Session, parent hlssession.Resource) (hls.MintFunc, func() error, error) {
	existing, err := s.hlsSessions.ListResources(ctx, sess.SessionID)
	if err != nil {
		return nil, nil, err
	}
	type childKey struct {
		kind              hlssession.ResourceKind
		ordinal           int
		mediaSequence     int64
		hasMediaSequence  bool
		partIndex         int
		hasPartIndex      bool
		renditionIndex    int
		hasRenditionIndex bool
		mediaStartNS      int64
		mediaEndNS        int64
		hasMediaTime      bool
		mediaDurationNS   int64
		hasMediaDuration  bool
	}
	known := make(map[childKey]string, len(existing))
	var duplicates []string
	for _, r := range existing {
		if r.Reference.Selector == parent.ResourceID {
			key := childKey{
				kind: r.Kind, ordinal: r.Reference.Ordinal,
				mediaSequence: r.Reference.MediaSequence, hasMediaSequence: r.Reference.HasMediaSequence,
				partIndex: r.Reference.PartIndex, hasPartIndex: r.Reference.HasPartIndex,
				renditionIndex: r.Reference.RenditionIndex, hasRenditionIndex: r.Reference.HasRenditionIndex,
				mediaStartNS: r.Reference.MediaStartNS, mediaEndNS: r.Reference.MediaEndNS, hasMediaTime: r.Reference.HasMediaTime,
				mediaDurationNS: r.Reference.MediaDurationNS, hasMediaDuration: r.Reference.HasMediaDuration,
			}
			if r.Reference.HasMediaSequence || r.Reference.HasRenditionIndex {
				key.ordinal = 0
			}
			if _, ok := known[key]; ok {
				duplicates = append(duplicates, r.ResourceID)
				continue
			}
			known[key] = r.ResourceID
		}
	}
	minSequence := int64(-1)
	liveSequences := make(map[int64]struct{})
	mint := func(ordinal int, e hls.Entry) (string, error) {
		key := childKey{
			kind: e.Kind, ordinal: ordinal,
			mediaSequence: e.MediaSequence, hasMediaSequence: e.HasMediaSequence,
			partIndex: e.PartIndex, hasPartIndex: e.HasPartIndex,
			renditionIndex: e.RenditionIndex, hasRenditionIndex: e.HasRenditionIndex,
			mediaStartNS: e.MediaStartNS, mediaEndNS: e.MediaEndNS, hasMediaTime: e.HasMediaTime,
			mediaDurationNS: e.MediaDurationNS, hasMediaDuration: e.HasMediaDuration,
		}
		if e.HasMediaSequence || e.HasRenditionIndex {
			key.ordinal = 0
		}
		if e.HasMediaSequence {
			if minSequence < 0 || e.MediaSequence < minSequence {
				minSequence = e.MediaSequence
			}
			if e.Kind == hlssession.KindSegment {
				liveSequences[e.MediaSequence] = struct{}{}
			}
		}
		if id, ok := known[key]; ok {
			return hlsResourcePath(id, e.Kind, parent.Kind), nil
		}
		ref := hlssession.Reference{
			Handler:           parent.Reference.Handler,
			BackendID:         parent.Reference.BackendID,
			Selector:          parent.ResourceID,
			Ordinal:           ordinal,
			ByteStart:         e.ByteStart,
			ByteEnd:           e.ByteEnd,
			MediaSequence:     e.MediaSequence,
			HasMediaSequence:  e.HasMediaSequence,
			PartIndex:         e.PartIndex,
			HasPartIndex:      e.HasPartIndex,
			RenditionIndex:    e.RenditionIndex,
			HasRenditionIndex: e.HasRenditionIndex,
			MediaStartNS:      e.MediaStartNS,
			MediaEndNS:        e.MediaEndNS,
			HasMediaTime:      e.HasMediaTime,
			MediaDurationNS:   e.MediaDurationNS,
			HasMediaDuration:  e.HasMediaDuration,
		}
		ref.PathwayID = e.PathwayID
		if ref.PathwayID == "" {
			ref.PathwayID = parent.Reference.PathwayID
		}
		child, err := s.hlsSessions.RegisterResource(ctx, sess.SessionID, e.Kind, ref)
		if err != nil {
			return "", err
		}
		known[key] = child.ResourceID
		return hlsResourcePath(child.ResourceID, child.Kind, parent.Kind), nil
	}
	cleanup := func() error {
		if minSequence < 0 {
			return nil
		}
		cutoff := int64(0)
		retainWindow := int64(len(liveSequences))
		if minSequence > retainWindow {
			cutoff = minSequence - retainWindow
		}
		remove := append([]string(nil), duplicates...)
		for _, r := range existing {
			if r.Reference.Selector == parent.ResourceID &&
				r.Reference.HasMediaSequence &&
				r.Reference.MediaSequence < cutoff {
				remove = append(remove, r.ResourceID)
			}
		}
		_, err := s.hlsSessions.DeleteResources(ctx, sess.SessionID, remove)
		return err
	}
	return mint, cleanup, nil
}

func hlsResourcePath(resourceID string, kind hlssession.ResourceKind, parentKind hlssession.ResourceKind) string {
	suffix := ".bin"
	switch kind {
	case hlssession.KindMaster, hlssession.KindMedia, hlssession.KindSubtitle:
		suffix = ".m3u8"
	case hlssession.KindMap:
		suffix = ".mp4"
	case hlssession.KindKey:
		suffix = ".key"
	case hlssession.KindSegment:
		suffix = ".ts"
		if parentKind == hlssession.KindSubtitle {
			suffix = ".vtt"
		}
	}
	return "/hls/" + resourceID + suffix
}

// deriveHLSLiveURL reconstructs the current upstream URL a resource stands
// for. A root resource (Reference.Selector == "") re-resolves through the
// owning item's protocol handler, exactly the same coordinator the
// ordinary progressive-HTTP relay path uses. Every deeper resource
// re-fetches and re-parses its own parent's live playlist and locates
// itself by the Ordinal recorded at mint time — this is what lets the
// graph stay URL-free at rest (DG-04) while still being fully
// reproducible on demand.
type hlsLiveSource struct {
	URL            string
	Headers        map[string]string
	RefreshContext string
}

type hlsBoundedResource struct {
	body      []byte
	mediaType string
}

func hlsFetchWithRefresh[T any](
	ctx context.Context,
	s *Server,
	sess hlssession.Session,
	res hlssession.Resource,
	fetch func(hlsLiveSource) (T, error),
) (T, error) {
	var zero T
	refreshContext := ""
	for attempt := 0; attempt < 2; attempt++ {
		source, err := s.deriveHLSLiveSourceAttempt(ctx, sess, res, 0, attempt > 0, refreshContext)
		if source.RefreshContext != "" {
			refreshContext = source.RefreshContext
		}
		if err == nil {
			value, fetchErr := fetch(source)
			if fetchErr == nil {
				return value, nil
			}
			err = fetchErr
		}
		if attempt > 0 || !hlsShouldRefresh(ctx, err) {
			return zero, err
		}
	}
	return zero, httpstream.NewError(httpstream.ClassBackendUnavailable, "hls refresh exhausted")
}

func hlsShouldRefresh(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return false
	}
	switch httpstream.ClassOf(err) {
	case httpstream.ClassBackendUnavailable,
		httpstream.ClassUpstreamMalformed,
		httpstream.ClassRepresentationLost,
		httpstream.ClassNoSource:
		return true
	}
	return false
}

func (s *Server) deriveHLSLiveSourceAttempt(
	ctx context.Context,
	sess hlssession.Session,
	res hlssession.Resource,
	depth int,
	forced bool,
	refreshContext string,
) (hlsLiveSource, error) {
	if depth > hlsMaxWalkDepth {
		return hlsLiveSource{}, httpstream.NewError(httpstream.ClassUpstreamMalformed, "hls resource chain too deep")
	}
	if res.Reference.Selector == "" {
		return s.deriveHLSRootSource(ctx, sess, forced, refreshContext)
	}
	if !hlssession.ValidResourceID(res.Reference.Selector) {
		return hlsLiveSource{}, httpstream.NewError(httpstream.ClassInvalidKey, "malformed hls parent reference")
	}
	parent, err := s.hlsSessions.Resolve(ctx, res.Reference.Selector)
	if err != nil {
		return hlsLiveSource{}, httpstream.NewError(httpstream.ClassRepresentationLost, "hls parent resource no longer present")
	}
	if parent.SessionID != sess.SessionID {
		return hlsLiveSource{}, httpstream.NewError(httpstream.ClassInvalidKey, "hls parent belongs to a different session")
	}
	parentSource, err := s.deriveHLSLiveSourceAttempt(ctx, sess, parent, depth+1, forced, refreshContext)
	if err != nil {
		return parentSource, err
	}
	failedSource := hlsLiveSource{RefreshContext: parentSource.RefreshContext}
	parentBody, err := s.fetchHLSPlaylistSource(ctx, parentSource)
	if err != nil {
		return failedSource, err
	}
	var entries []hls.Entry
	switch parent.Kind {
	case hlssession.KindMaster:
		entries, err = hls.ParseMaster(parentBody)
	case hlssession.KindMedia, hlssession.KindSubtitle:
		entries, err = hls.ParseMedia(parentBody)
	default:
		return failedSource, httpstream.NewError(httpstream.ClassUpstreamMalformed, "hls parent kind cannot own children")
	}
	if err != nil {
		return failedSource, httpstream.NewError(httpstream.ClassUpstreamMalformed, "hls parent playlist no longer parses")
	}
	var entry hls.Entry
	switch {
	case res.Reference.HasRenditionIndex:
		found := false
		for _, candidate := range entries {
			if candidate.Kind == res.Kind && candidate.HasRenditionIndex &&
				candidate.RenditionIndex == res.Reference.RenditionIndex {
				entry = candidate
				found = true
				break
			}
		}
		if !found {
			return failedSource, httpstream.NewError(httpstream.ClassRepresentationLost, "hls rendition report no longer present")
		}
	case res.Reference.HasMediaSequence:
		found := false
		for _, candidate := range entries {
			if candidate.Kind == res.Kind && candidate.HasMediaSequence &&
				candidate.MediaSequence == res.Reference.MediaSequence &&
				candidate.HasPartIndex == res.Reference.HasPartIndex &&
				(!candidate.HasPartIndex || candidate.PartIndex == res.Reference.PartIndex) {
				entry = candidate
				found = true
				break
			}
		}
		if !found {
			return failedSource, httpstream.NewError(httpstream.ClassRepresentationLost, "hls live entry no longer present")
		}
	default:
		if res.Reference.Ordinal < 0 || res.Reference.Ordinal >= len(entries) {
			return failedSource, httpstream.NewError(httpstream.ClassRepresentationLost, "hls entry ordinal no longer present")
		}
		entry = entries[res.Reference.Ordinal]
	}
	if entry.Kind != res.Kind {
		return failedSource, httpstream.NewError(httpstream.ClassRepresentationLost, "hls entry kind changed since mint")
	}
	childURL, err := hls.ResolveURIRef(parentSource.URL, entry.URI)
	if err != nil {
		return failedSource, err
	}
	return hlsLiveSource{
		URL:            childURL,
		Headers:        headersForHLSChild(parentSource, childURL),
		RefreshContext: parentSource.RefreshContext,
	}, nil
}

func headersForHLSChild(parent hlsLiveSource, childURL string) map[string]string {
	parentURL, parentErr := url.Parse(parent.URL)
	child, childErr := url.Parse(childURL)
	if parentErr != nil || childErr != nil || !strings.EqualFold(parentURL.Host, child.Host) {
		return nil
	}
	return parent.Headers
}

func (s *Server) deriveHLSRootSource(
	ctx context.Context,
	sess hlssession.Session,
	forced bool,
	refreshContext string,
) (hlsLiveSource, error) {
	item, err := s.store.GetItemByID(ctx, sess.ItemID)
	if err != nil || item == nil {
		return hlsLiveSource{}, httpstream.NewError(httpstream.ClassRepresentationLost, "hls session item no longer present")
	}
	if item.ResolveKey == nil {
		return hlsLiveSource{}, httpstream.NewError(httpstream.ClassInvalidKey, "item has no resolve key")
	}
	key, kerr := httpstream.ParseResolveKey(*item.ResolveKey)
	if kerr != nil {
		return hlsLiveSource{}, kerr
	}
	files, ferr := httpstream.ParseFiles(strFromPtr(item.FileList))
	if ferr != nil {
		return hlsLiveSource{}, httpstream.NewError(httpstream.ClassInvalidKey, "item file list unreadable")
	}
	pf, ok := httpstream.LookupFile(files, sess.FileID)
	if !ok {
		return hlsLiveSource{}, httpstream.NewError(httpstream.ClassRepresentationLost, "hls session file no longer present")
	}
	handler, hok := s.httpHandlers.Lookup(key.BackendID, key.Handler)
	if !hok {
		return hlsLiveSource{}, httpstream.NewError(httpstream.ClassNoHandler, "protocol handler not configured")
	}
	rf, rerr := s.resolveHTTPPlayback(ctx, item.ID, pf.FileID, handler, key, pf.Selector, forced, refreshContext)
	if rerr != nil {
		return hlsLiveSource{}, rerr
	}
	return hlsLiveSource{URL: rf.URL, Headers: rf.RequestHeaders, RefreshContext: rf.RefreshContext}, nil
}

func (s *Server) fetchHLSPlaylistSource(ctx context.Context, source hlsLiveSource) (string, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, hlsFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, source.URL, nil)
	if err != nil {
		return "", httpstream.NewError(httpstream.ClassUpstreamMalformed, "invalid manifest url")
	}
	for name, value := range source.Headers {
		req.Header.Set(name, value)
	}
	resp, err := hlsHTTPClient(s.httpProbeHTTPClient(), source).Do(req)
	if err != nil {
		return "", httpstream.NewError(httpstream.ClassBackendUnavailable, "manifest fetch failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", httpstream.NewError(httpstream.ClassBackendUnavailable, fmt.Sprintf("manifest fetch status %d", resp.StatusCode))
	}
	limited := io.LimitReader(resp.Body, hls.MaxPlaylistBytes+1)
	b, err := io.ReadAll(limited)
	if err != nil {
		return "", httpstream.NewError(httpstream.ClassUpstreamMalformed, "manifest read failed")
	}
	if int64(len(b)) > hls.MaxPlaylistBytes {
		return "", httpstream.NewError(httpstream.ClassUpstreamMalformed, "manifest exceeds size bound")
	}
	return string(b), nil
}

func (s *Server) fetchHLSBoundedResource(ctx context.Context, source hlsLiveSource) ([]byte, string, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, hlsFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, source.URL, nil)
	if err != nil {
		return nil, "", httpstream.NewError(httpstream.ClassUpstreamMalformed, "invalid hls resource url")
	}
	for name, value := range source.Headers {
		req.Header.Set(name, value)
	}
	resp, err := hlsHTTPClient(s.httpProbeHTTPClient(), source).Do(req)
	if err != nil {
		return nil, "", httpstream.NewError(httpstream.ClassBackendUnavailable, "hls resource fetch failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", httpstream.NewError(httpstream.ClassBackendUnavailable, fmt.Sprintf("hls resource fetch status %d", resp.StatusCode))
	}
	limited := io.LimitReader(resp.Body, hls.MaxPlaylistBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, "", httpstream.NewError(httpstream.ClassUpstreamMalformed, "hls resource read failed")
	}
	if int64(len(body)) > hls.MaxPlaylistBytes {
		return nil, "", httpstream.NewError(httpstream.ClassUpstreamMalformed, "hls resource exceeds size bound")
	}
	mediaType := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	if mediaType == "" {
		mediaType = hlsLeafMediaType(hlssession.KindSubtitle, source.URL)
	}
	return body, mediaType, nil
}

func hlsHTTPClient(base *http.Client, source hlsLiveSource) *http.Client {
	if len(source.Headers) == 0 {
		return base
	}
	client := *base
	checkRedirect := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if checkRedirect != nil {
			if err := checkRedirect(req, via); err != nil {
				return err
			}
		}
		if len(via) == 0 || strings.EqualFold(req.URL.Host, via[0].URL.Host) {
			return nil
		}
		for name := range source.Headers {
			req.Header.Del(name)
		}
		return nil
	}
	return &client
}

func (s *Server) hlsUnavailable(w http.ResponseWriter, err error) {
	s.log.Warn("hls: resource unavailable", "error", httpstream.Sanitize(err))
	switch httpstream.ClassOf(err) {
	case httpstream.ClassUpstreamMalformed:
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	case httpstream.ClassRepresentationLost, httpstream.ClassInvalidKey, httpstream.ClassNoHandler:
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	if errors.Is(err, hlssession.ErrResourceNotFound) || errors.Is(err, hlssession.ErrSessionNotFound) {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	w.Header().Set("Retry-After", "60")
	http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
}
