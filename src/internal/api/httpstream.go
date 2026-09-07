package api

// HTTP stream provider — arr-facing surfaces (HTTP-STREAM-MASTER-PLAN.md H1).
//
//   /torznab/http-stream        — dedicated caps/search/feed route (HS-1.6)
//   /grab/http/{token}          — signed grab envelope → synthetic-magnet
//                                 302, torrent-compatible marker (D1, HS-1.3)
//   qBit add marker validation  — creates SourceTypeHTTP items (D1)
//   resolveHTTPItem             — synchronous one-pass resolve (HS-1.4)
//   streamHTTPItem              — live byte relay + HEAD + status matrix
//                                 (HS-1.7, HS-1.8, D16)
//
// Invariants enforced here (§0): no source/CDN URL is ever persisted or
// logged; .strm files carry only the signed DH /stream URL; Removed HTTP
// items keep streaming (D13). HR3.1 routes progressive playback through the
// shared rangecache/current/re-resolve ladder.
//
// HR5.3 exception, scoped narrowly to resolve time only: the D9 grab-time
// preflight (preflightHTTPSource) and a bounded startup prewarm
// (prewarmHTTPFile) DO lease from the HTTP lane's own accountgov.Governor
// and DO pin bytes into the shared rangecache, per LC-01's explicit
// allowance for "governed grab-time/startup ... prewarm into existing cache
// tiers." This is bounded preparation, not a playback bypass; HR3.1 consumes
// the same cache identity during live playback.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/bytesource"
	"github.com/darkharrbor/darkharrbor/internal/contentproof"
	"github.com/darkharrbor/darkharrbor/internal/hls"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/identitycheck"
	"github.com/darkharrbor/darkharrbor/internal/ladder"
	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
	"github.com/darkharrbor/darkharrbor/internal/sidecar"
	"github.com/darkharrbor/darkharrbor/internal/store"
	"github.com/darkharrbor/darkharrbor/internal/suppress"
)

// httpMagnetTokenParam is the magnet query parameter that carries the signed
// grab envelope through the arr → qBit round trip (x.* is the magnet
// extension namespace). The arr treats the magnet as opaque and posts it
// verbatim to the qBit emulator — same mechanism the /download precedent
// relies on for dn/tr survival.
const httpMagnetTokenParam = "x.dh"

// maxHTTPResolveAttempts bounds the transient-failure retry loop for
// resolveHTTPItem; exhaustion is StateFailed (HS-1.2: every path ends Ready,
// bounded retry, or Failed).
const maxHTTPResolveAttempts = 5

// httpStreamCopyBufPool provides small reused relay copy buffers (A21: never
// allocate the legacy 10 MiB-per-request buffer for HTTP).
var httpStreamCopyBufPool = sync.Pool{
	New: func() any { b := make([]byte, 256<<10); return &b },
}

// SetHTTPStreamRegistry installs the protocol handler registry. Called from
// main during startup wiring; nil-safe (H1 ships it empty).
func (s *Server) SetHTTPStreamRegistry(r *httpstream.Registry) {
	s.httpHandlers = r
}

// registerHTTPStream wires the HTTP stream routes. Exact routes — never the
// /torznab/ catch-all — so HTTP results cannot fall through the shared
// torrent/usenet parsing or populate magnetCache (HS-1.6).
func (s *Server) registerHTTPStream(mux *http.ServeMux) {
	mux.HandleFunc("/torznab/http-stream", s.handleTorznabHTTPStream)
	mux.HandleFunc("/torznab/http-stream/", s.handleTorznabHTTPStream)
	mux.HandleFunc("/grab/http/", s.handleGrabHTTP)
}

// ── Grab envelope endpoint (D1, HS-1.3) ──────────────────────────────────────

// handleGrabHTTP serves GET/HEAD /grab/http/{token}. It validates the signed
// envelope and 302-redirects to a synthetic magnet carrying the token — the
// torrent-compatible marker form the arr forwards to the qBit emulator
// (per the /download/ precedent). Stateless: replay after restart is safe;
// duplicates collapse in enqueueSubmission's submission-key dedup.
func (s *Server) handleGrabHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.cfg.HTTPStreamEnabled() {
		http.Error(w, "http stream provider disabled", http.StatusNotFound)
		return
	}
	token := strings.TrimPrefix(r.URL.Path, "/grab/http/")
	token = strings.TrimSpace(strings.TrimSuffix(token, "/"))
	env, err := httpstream.DecodeGrabToken(s.cfg.Server.StreamSecret, token)
	if err != nil {
		s.log.Warn("grab/http: token rejected", "error", httpstream.Sanitize(err), "remote_addr", r.RemoteAddr)
		http.Error(w, "invalid grab token", http.StatusBadRequest)
		return
	}
	title := env.Title
	if title == "" {
		title = env.Hash
	}
	// Trackers appended for the same reason as /download/: Sonarr rejects
	// tracker-less magnets client-side before posting to the qBit shim.
	magnet := "magnet:?xt=urn:btih:" + env.Hash +
		"&dn=" + url.QueryEscape(title) +
		"&" + httpMagnetTokenParam + "=" + url.QueryEscape(token) +
		"&tr=udp%3A%2F%2Ftracker.opentrackr.org%3A1337%2Fannounce"
	http.Redirect(w, r, magnet, http.StatusFound)
}

// httpGrabTokenFromMagnet extracts the grab envelope token from a magnet URI
// produced by handleGrabHTTP. Empty string means "not an HTTP-marker magnet".
func httpGrabTokenFromMagnet(line string) string {
	parsed, err := url.Parse(strings.TrimSpace(line))
	if err != nil || !strings.EqualFold(parsed.Scheme, "magnet") {
		return ""
	}
	return strings.TrimSpace(parsed.Query().Get(httpMagnetTokenParam))
}

// enqueueHTTPGrab validates a marker magnet's envelope and enqueues the
// SourceTypeHTTP item (qBit add path, D1). Returns an error the caller logs
// as a rejected submission — bad tokens are never retried.
func (s *Server) enqueueHTTPGrab(r *http.Request, line, category string, metadata store.SubmissionMetadata) error {
	token := httpGrabTokenFromMagnet(line)
	if token == "" {
		return httpstream.NewError(httpstream.ClassInvalidKey, "missing grab token")
	}
	env, err := httpstream.DecodeGrabToken(s.cfg.Server.StreamSecret, token)
	if err != nil {
		return err
	}
	canonical, err := env.Key.Canonical()
	if err != nil {
		return err
	}
	displayName := firstNonEmpty(metadata.Rename, env.Title, env.Hash)
	if sidecar.ValidateProviderIdentity(env.ProviderIdentity) == nil {
		metadata.ProviderIdentity = env.ProviderIdentity
	}
	_, _, err = s.enqueueSubmission(r.Context(), SubmissionRequest{
		SourceType:  store.SourceTypeHTTP,
		ClientKind:  store.ClientKindQBit,
		Category:    category,
		DisplayName: displayName,
		InfoHash:    env.Hash,
		ResolveKey:  canonical,
		Metadata:    metadata,
	})
	return err
}

// ── One-pass resolve (HS-1.4) ────────────────────────────────────────────────

// ResolveHTTPItem is the shared one-pass HTTP resolver, called from the
// immediate per-grab goroutine AND the recovery resolver cycle — a crash at
// any point re-enters the identical pass (HS-1.2). Exported so main.go's
// resolve cycle can dispatch to it.
func (s *Server) ResolveHTTPItem(ctx context.Context, item *store.Item) {
	s.resolveHTTPItem(ctx, item)
}

// httpFailItem transitions an HTTP item to StateFailed with a sanitized
// message (HS-1.9: error_message is a persisted column).
func (s *Server) httpFailItem(ctx context.Context, item *store.Item, err error) {
	msg := httpstream.Sanitize(err)
	item.ErrorMessage = &msg
	if uerr := s.store.UpdateItemState(ctx, item, store.StateFailed, msg); uerr != nil {
		s.log.Error("http resolve: persist failed state", "item_id", item.ID, "error", uerr)
	}
}

// httpScheduleRetry mirrors the resolver-cycle retry contract: bounded
// attempts with backoff, exhaustion → StateFailed (HS-1.2).
func (s *Server) httpScheduleRetry(ctx context.Context, item *store.Item, delay time.Duration, cause error) {
	item.RetryCount++
	if item.RetryCount >= maxHTTPResolveAttempts {
		item.NextRunAt = nil
		s.httpFailItem(ctx, item, httpstream.Wrapf(httpstream.ClassBackendUnavailable,
			"http resolve attempts exhausted after %d attempts: %s", item.RetryCount, httpstream.Sanitize(cause)))
		return
	}
	next := time.Now().Add(delay)
	item.NextRunAt = &next
	item.UpdatedAt = time.Now().UTC()
	if err := s.store.UpdateItem(ctx, item); err != nil {
		s.log.Error("http resolve: schedule retry", "item_id", item.ID, "error", err)
	}
}

func (s *Server) httpHandleResolveError(ctx context.Context, log *slog.Logger, item *store.Item, handler string, err error) {
	if ctx.Err() != nil {
		return
	}
	if httpstream.ClassOf(err).Transient() {
		log.Warn("http resolve: transient backend failure (will retry)",
			"handler", handler, "error", httpstream.Sanitize(err))
		s.httpScheduleRetry(ctx, item, 30*time.Second, err)
		return
	}
	s.httpFailItem(ctx, item, err)
}

// resolveHTTPItem runs the synchronous one-pass HTTP resolve: parse the
// persisted resolve key, re-resolve through the protocol handler, assign DH
// opaque file IDs, write .strm files (signed DH proxy URLs only), persist the
// URL-free file DTO, transition to StateReady. No submit, no polling, no
// remote IDs, no governor (D1/D13, A30).
func (s *Server) resolveHTTPItem(ctx context.Context, item *store.Item) {
	log := s.log.With("item_id", item.ID, "display_name", item.DisplayName)

	if item.ResolveKey == nil || strings.TrimSpace(*item.ResolveKey) == "" {
		s.httpFailItem(ctx, item, httpstream.NewError(httpstream.ClassInvalidKey, "http item has no resolve key"))
		return
	}
	key, err := httpstream.ParseResolveKey(*item.ResolveKey)
	if err != nil {
		s.httpFailItem(ctx, item, err)
		return
	}
	handler, ok := s.httpHandlers.Lookup(key.BackendID, key.Handler)
	if !ok {
		// Permanent for this deployment: the backend/handler is not
		// configured. Fail fast so the arr blocklists and re-grabs elsewhere.
		s.httpFailItem(ctx, item, httpstream.Wrapf(httpstream.ClassNoHandler,
			"protocol handler %q not configured", key.Handler))
		return
	}

	var chosen []httpstream.ResolvedFile
	var archiveSize int64
	var archiveFileID string
	if httpstream.IsRemoteArchiveSelector(key.Selector) {
		archiveHandler, archiveOK := s.httpHandlers.LookupRemoteArchive(key.BackendID, key.Handler)
		if !archiveOK {
			s.httpFailItem(ctx, item, httpstream.NewError(httpstream.ClassNoHandler,
				"protocol handler has no remote archive arm"))
			return
		}
		archiveFileID, err = httpstream.NewFileID()
		if err != nil {
			s.httpScheduleRetry(ctx, item, 10*time.Second, err)
			return
		}
		descriptors, resolveErr := archiveHandler.ResolveRemoteArchives(ctx, httpstream.ResolveRequest{Key: key, Operation: httpstream.ResolveNormal})
		if resolveErr != nil {
			s.httpHandleResolveError(ctx, log, item, key.Handler, resolveErr)
			return
		}
		descriptor, matchErr := httpstream.MatchRemoteArchiveDescriptor(descriptors, key.Selector)
		if matchErr != nil {
			s.httpFailItem(ctx, item, matchErr)
			return
		}
		if descriptor.Format != httpstream.RemoteArchiveZIP || len(descriptor.Parts) != 1 {
			s.httpFailItem(ctx, item, httpstream.NewError(httpstream.ClassNoSource,
				"remote archive has no enabled single-part reader"))
			return
		}
		archiveSize, _, err = s.preflightHTTPSource(ctx, descriptor.Parts[0], item.ID,
			httpRepresentationID(item.ID, archiveFileID+"|archive"), key.BackendID, key.Handler)
		if err != nil {
			s.httpHandleResolveError(ctx, log, item, key.Handler, err)
			return
		}
		member, memberSource, openErr := s.openHTTPRemoteArchive(ctx, item.ID, archiveFileID,
			key, archiveSize, archiveHandler, descriptor)
		if openErr != nil {
			s.httpHandleResolveError(ctx, log, item, key.Handler, openErr)
			return
		}
		var first [1]byte
		if n, readErr := memberSource.ReadAt(ctx, first[:], 0); n != 1 || readErr != nil {
			if readErr == nil {
				readErr = io.ErrUnexpectedEOF
			}
			s.httpHandleResolveError(ctx, log, item, key.Handler,
				httpstream.Wrapf(httpstream.ClassUpstreamMalformed, "remote archive member probe failed: %s", httpstream.Sanitize(readErr)))
			return
		}
		chosen = []httpstream.ResolvedFile{{
			Selector: key.Selector, Name: member.Name, Size: member.Size,
			ContentType: fileExtContentType(member.Name), SupportsRange: true,
		}}
	} else {
		files, resolveErr := handler.Resolve(ctx, httpstream.ResolveRequest{Key: key, Operation: httpstream.ResolveNormal})
		if resolveErr != nil {
			s.httpHandleResolveError(ctx, log, item, key.Handler, resolveErr)
			return
		}
		files, err = httpstream.ValidateResolvedFiles(files, time.Now())
		if err != nil {
			s.httpFailItem(ctx, item, err)
			return
		}

		// Select the grabbed representation. A key with a selector pins exactly
		// one representation (D10); without one, exactly one resolved file is
		// required — anything else is ambiguous and rejected.
		if key.Selector != "" {
			f, matchErr := httpstream.MatchSelector(files, key.Selector)
			if matchErr != nil {
				s.httpFailItem(ctx, item, matchErr)
				return
			}
			chosen = []httpstream.ResolvedFile{f}
		} else {
			if len(files) != 1 {
				s.httpFailItem(ctx, item, httpstream.NewError(httpstream.ClassRepresentationLost,
					"resolve returned zero or ambiguous representations for selector-less key"))
				return
			}
			chosen = files
		}
	}

	// Assign DH opaque file IDs and build the URL-free persisted DTO plus
	// the writer input. provider.CachedFile.RequestDLURL is deliberately
	// left empty — the writer only ever embeds the signed DH proxy URL.
	persisted := make([]httpstream.PersistedFile, 0, len(chosen))
	writerFiles := make([]provider.CachedFile, 0, len(chosen))
	var totalSize int64
	for _, f := range chosen {
		if archiveSize == 0 && strings.EqualFold(key.Handler, "stremio") {
			if _, unwrapErr := s.unwrapMediaFlowResolvedFile(&f); unwrapErr != nil {
				s.httpFailItem(ctx, item, httpstream.NewError(httpstream.ClassUpstreamMalformed,
					"invalid self-origin playback source"))
				return
			}
		}
		var size int64
		contentType := f.ContentType
		rangeVerified := false
		var perr error

		// HR2.7: the file ID is generated before preflight (rather than
		// after, as previously) purely so the D9 preflight call below can
		// record proof-graph evidence under this file's own stable
		// representation ID -- FileID assignment itself is unconditional
		// and never depended on preflight succeeding first.
		fid := archiveFileID
		if fid == "" {
			var iderr error
			fid, iderr = httpstream.NewFileID()
			if iderr != nil {
				s.httpScheduleRetry(ctx, item, 10*time.Second, iderr)
				return
			}
		}

		if archiveSize > 0 {
			size = f.Size
			rangeVerified = true
		} else if isHLSResolvedFile(f) {
			// HLS owns range semantics below the manifest. Validate the bounded
			// root playlist through the existing HLS parser instead of applying
			// the progressive file's bytes=0-0 contract to it.
			perr = s.preflightHLSSource(ctx, f)
		} else {
			// D9 progressive grab-time verification: bytes=0-0 from DH's egress.
			var verifiedType string
			size, verifiedType, perr = s.preflightHTTPSource(ctx, f, item.ID, httpRepresentationID(item.ID, fid), key.BackendID, key.Handler)
			if verifiedType != "" {
				contentType = verifiedType
			}
			rangeVerified = perr == nil
		}
		if perr != nil {
			if httpstream.ClassOf(perr).Transient() {
				log.Warn("http resolve: preflight transient failure (will retry)",
					"error", httpstream.Sanitize(perr))
				s.httpScheduleRetry(ctx, item, 30*time.Second, perr)
				return
			}
			s.httpFailItem(ctx, item, perr)
			return
		}

		persisted = append(persisted, httpstream.PersistedFile{
			FileID:      fid,
			Selector:    f.Selector,
			Name:        f.Name,
			Size:        size,
			ContentType: contentType,
			ArchiveSize: archiveSize,
			// Progressive preflight alone verifies file-range semantics; HLS
			// manifests delegate ranges to their opaque leaf resources.
			RangeVerified: rangeVerified,
		})
		writerName := f.Name
		if strings.EqualFold(key.Handler, "stremio") {
			writerName = stremioEpisodeImportName(item, f.Name)
		}
		writerFiles = append(writerFiles, provider.CachedFile{
			FileID: fid,
			Name:   writerName,
			Size:   size,
		})
		totalSize += size
	}

	// TS-0.3 (T11) is torrent-lane scoped only -- the HTTP/OMSS/Stremio lane
	// has no TorrentMeta concept, so this always passes nil and Write's
	// existing provider-name/heuristic behavior is unchanged here.
	strmPath, werr := s.writer.Write(ctx, item, writerFiles, s.cfg.Server.BaseURL, nil)
	if werr != nil {
		log.Warn("http resolve: write strm failed (will retry)", "error", werr)
		s.httpScheduleRetry(ctx, item, 30*time.Second, werr)
		return
	}

	fileListJSON, merr := httpstream.MarshalFiles(persisted)
	if merr != nil {
		s.httpScheduleRetry(ctx, item, 10*time.Second, merr)
		return
	}
	item.StrmPath = &strmPath
	item.FileList = &fileListJSON
	item.TotalSize = totalSize
	item.ErrorMessage = nil
	item.NextRunAt = nil
	if err := s.store.UpdateItemState(ctx, item, store.StateReady, "http: resolved"); err != nil {
		log.Error("http resolve: persist ready state", "error", err)
		return
	}
	log.Info("http resolve: item ready",
		"handler", key.Handler,
		"backend_id", key.BackendID,
		"files", len(persisted),
		"total_size", totalSize,
	)

	// HR5.3: bounded startup prewarm of the just-resolved file, under a
	// PriorityPrewarm lease on the HTTP lane's own governor. Best-effort
	// and never blocks/reopens the StateReady transition just persisted
	// above — a failure here is logged and dropped, exactly like the
	// torrent lane's own grab-time prewarm (TS-1.4) and the D9 preflight
	// above it. `chosen`/`persisted` always have exactly one entry at this
	// point (MatchSelector or the selector-less single-file branch above),
	// so there is no "which file" ambiguity to resolve here.
	if archiveSize == 0 && len(chosen) == 1 && len(persisted) == 1 {
		s.prewarmHTTPFile(ctx, item, persisted[0], chosen[0].Selector, handler, key)
	}
}

// stremioEpisodeImportName keeps the native Stremio representation name as
// source metadata, but prevents a pack-level stream label from becoming the
// filename Sonarr parses for an exact episode grab. AIOStreams commonly
// returns one episode URL selected from a season-pack stream whose display
// name still describes the whole pack. The grab already carries the Arr's
// authoritative, episode-scoped ProviderIdentity and an Arr-parseable
// DisplayName. Reuse that display name only when it contains the exact single
// SxxEyy identity; otherwise abstain and preserve the backend name.
//
// If the backend name itself contains any exact SxxEyy token, keep it even
// when it disagrees with the grab identity so a real identity mismatch stays
// visible to Sonarr/ID-01 rather than being hidden by a rename.
func stremioEpisodeImportName(item *store.Item, resolvedName string) string {
	if item == nil || item.Metadata.ProviderIdentity == nil {
		return resolvedName
	}
	identity := item.Metadata.ProviderIdentity
	if !strings.EqualFold(identity.Kind, "series") || len(identity.Episodes) != 1 {
		return resolvedName
	}
	episode := identity.Episodes[0]
	if episode.Season < 1 || episode.Episode < 1 {
		return resolvedName
	}
	if _, _, ok := identitycheck.ParseSeasonEpisode(resolvedName); ok {
		return resolvedName
	}
	season, ep, ok := identitycheck.ParseSeasonEpisode(item.DisplayName)
	if !ok || season != episode.Season || ep != episode.Episode {
		return resolvedName
	}
	return item.DisplayName
}

// prewarmHTTPFile performs HR5.3's bounded, governed startup prewarm for a
// freshly-resolved HTTP item: it pins pf's leading hot-head bytes (plus
// whatever mediatruth.Analyze reads while locating the container's seek
// index) into the shared rangecache, through the shared experience.Service
// bound to the HTTP lane's own governor (httpExpSvc/httpGov,
// HTTPSourceGovOp), so live playback demand elsewhere on this same governed
// operation class is never starved (LC-06). It reuses the existing
// per-(item,file) resolve coordinator as its Resolver — a prewarm call
// immediately after this exact resolve pass is very likely an httpResolveCoord
// cache hit, not a fresh backend call (DG-07: no wasted work). Nil-safe and
// bounded: a nil httpExpSvc (feature disabled or unwired) is a no-op, and the
// whole call is wrapped in the configured prewarm timeout regardless of ctx's
// own deadline.
func (s *Server) prewarmHTTPFile(ctx context.Context, item *store.Item, pf httpstream.PersistedFile, selector string, handler httpstream.Handler, key httpstream.ResolveKey) {
	if s.httpExpSvc == nil || !pf.RangeVerified || pf.Size <= 0 {
		return
	}
	fid := pf.FileID
	resolver := func(rctx context.Context, forced bool) (httpstream.ResolvedFile, error) {
		return s.resolveHTTPPlayback(rctx, item.ID, fid, handler, key, selector, forced, "")
	}
	chunkSrc, err := s.httpChunkSource(item, pf, resolver)
	if err != nil {
		s.log.Warn("http resolve: prewarm source build failed (non-fatal)", "item_id", item.ID, "error", httpstream.Sanitize(err))
		return
	}
	timeoutSec := s.cfg.Prewarm.TimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = 45
	}
	pctx, pcancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer pcancel()
	presult, perr := s.httpExpSvc.PrewarmHotHead(pctx, chunkSrc)
	if perr != nil {
		s.log.Warn("http resolve: prewarm hot-head failed (non-fatal)", "item_id", item.ID, "error", httpstream.Sanitize(perr))
		return
	}
	s.log.Info("http resolve: prewarm hot-head complete", "item_id", item.ID, "file_id", fid)
	// SF-04: persist any facts this same bounded analysis already computed,
	// so a later Arr ffprobe import can be answered instantly instead of a
	// live network probe. Best-effort and non-fatal, mirroring every other
	// step in this prewarm path.
	if presult != nil && !presult.Facts.Empty() && s.store != nil {
		if merr := s.store.SetMediaFacts(ctx, item.ID, fid, presult.SourceKey, presult.Facts); merr != nil {
			s.log.Warn("http resolve: media facts persist failed (non-fatal)", "item_id", item.ID, "error", merr)
		}
	}
	// ID-01: grab-time identity verification against the same already-
	// computed presult.Facts, right alongside SF-04's persist above --
	// this is the HTTP lane's equivalent of the torrent lane's own
	// TS-3.3/ID-01 call site in cmd/darkharrbor's resolveItem.
	if presult != nil {
		season, episode, _ := identitycheck.ParseSeasonEpisode(item.DisplayName)
		s.CheckIdentity(ctx, item, season, episode, &presult.Facts)
	}
}

func (s *Server) httpChunkSource(item *store.Item, pf httpstream.PersistedFile, resolver httpstream.Resolver) (rangecache.ChunkSource, error) {
	bs, err := s.httpByteSource(item, pf, resolver)
	if err != nil {
		return nil, err
	}
	return rangecache.NewByteSourceBlocks(bs, 0)
}

func (s *Server) httpByteSource(item *store.Item, pf httpstream.PersistedFile, resolver httpstream.Resolver) (bytesource.ByteSource, error) {
	// Key is deliberately colon/URL-free (httpstream.NewByteSource rejects
	// "://" and query characters) — item.ID and fid are opaque DH-assigned
	// identifiers, never upstream-derived (DG-04).
	return s.newHTTPByteSource("httpitem|"+item.ID+"|"+pf.FileID, pf.Size, pf.RangeVerified, resolver)
}

func (s *Server) newHTTPByteSource(key string, size int64, rangeVerified bool, resolver httpstream.Resolver) (bytesource.ByteSource, error) {
	var predictLeadMin, predictLeadMax time.Duration
	if s.cfg.HTTPStream.PredictiveResolveEnabled {
		predictLeadMin = time.Duration(s.cfg.HTTPStream.PredictiveResolveMinLeadMS) * time.Millisecond
		predictLeadMax = time.Duration(s.cfg.HTTPStream.PredictiveResolveMaxLeadMS) * time.Millisecond
	}
	return httpstream.NewByteSource(httpstream.ByteSourceConfig{
		Key:               key,
		Size:              size,
		RangeVerified:     rangeVerified,
		Client:            s.httpStreamClient(),
		Resolve:           resolver,
		Now:               s.nowUTC,
		OriginScores:      s.httpOriginScores,
		PredictiveLeadMin: predictLeadMin,
		PredictiveLeadMax: predictLeadMax,
	})
}

func (s *Server) openHTTPRemoteArchive(
	ctx context.Context,
	itemID, fileID string,
	key httpstream.ResolveKey,
	archiveSize int64,
	handler httpstream.RemoteArchiveHandler,
	initial httpstream.RemoteArchiveDescriptor,
) (httpstream.RemoteArchiveMember, bytesource.ByteSource, error) {
	if handler == nil || archiveSize <= 0 {
		return httpstream.RemoteArchiveMember{}, nil, httpstream.NewError(httpstream.ClassNoSource,
			"remote archive source is unavailable")
	}
	var initialMu sync.Mutex
	initialAvailable := true
	resolver := func(resolveCtx context.Context, forced bool) (httpstream.ResolvedFile, error) {
		initialMu.Lock()
		if !forced && initialAvailable {
			initialAvailable = false
			initialMu.Unlock()
			if initial.Format != httpstream.RemoteArchiveZIP || len(initial.Parts) != 1 {
				return httpstream.ResolvedFile{}, httpstream.NewError(httpstream.ClassNoSource,
					"remote archive has no enabled single-part reader")
			}
			return initial.Parts[0], nil
		}
		initialMu.Unlock()
		op := httpstream.ResolveNormal
		if forced {
			op = httpstream.ResolveForceRefresh
		}
		descriptors, err := handler.ResolveRemoteArchives(resolveCtx, httpstream.ResolveRequest{Key: key, Operation: op})
		if err != nil {
			return httpstream.ResolvedFile{}, err
		}
		descriptor, err := httpstream.MatchRemoteArchiveDescriptor(descriptors, key.Selector)
		if err != nil {
			return httpstream.ResolvedFile{}, err
		}
		if descriptor.Format != httpstream.RemoteArchiveZIP || len(descriptor.Parts) != 1 {
			return httpstream.ResolvedFile{}, httpstream.NewError(httpstream.ClassNoSource,
				"remote archive has no enabled single-part reader")
		}
		return descriptor.Parts[0], nil
	}
	sourceKey := "httparchive|" + itemID + "|" + fileID
	archiveSource, err := s.newHTTPByteSource(sourceKey, archiveSize, true, resolver)
	if err != nil {
		return httpstream.RemoteArchiveMember{}, nil, err
	}
	opened := initial
	opened.Selector = sourceKey
	return httpstream.OpenRemoteArchiveMember(ctx, opened, []bytesource.ByteSource{archiveSource})
}

// ── Stream surface (HS-1.7, HS-1.8, D16) ─────────────────────────────────────

// streamHTTPItem serves GET/HEAD /stream/{item}/{file} for SourceTypeHTTP.
// GET playback uses HR3.1's shared cache/current/re-resolve ladder; HEAD stays
// metadata-only. probe=1 suppresses play accounting only. It never mutates
// item state — a play-time outage is 5xx, not StateFailed (D13).
func (s *Server) streamHTTPItem(w http.ResponseWriter, r *http.Request, item *store.Item) {
	ctx := r.Context()
	fileID := r.PathValue("file_id")

	files, err := httpstream.ParseFiles(strFromPtr(item.FileList))
	if err != nil {
		s.log.Warn("http stream: file list unreadable", "item_id", item.ID, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	f, ok := httpstream.LookupFile(files, fileID)
	if !ok {
		// Strict stored-ID lookup only — no positional fallback (D10).
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	if isHLSFile(f) {
		playURL, err := s.CreateHLSPlaybackSession(ctx, item.ID, f.FileID)
		if err != nil {
			s.httpStreamUnavailable(w, item, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, playURL, http.StatusTemporaryRedirect)
		return
	}

	// HEAD is answered entirely from URL-free persisted metadata (HS-1.7).
	if r.Method == http.MethodHead {
		ct := advertisedHTTPContentType(f.ContentType, f.Name)
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "no-store")
		if f.RangeVerified {
			w.Header().Set("Accept-Ranges", "bytes")
		}
		if f.Size > 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(f.Size, 10))
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	probe := r.URL.Query().Get("probe") == "1"
	if !probe {
		// Play accounting only — HTTP items have no janitor timers, no
		// provider lifecycle (A30). last_played_at keeps COR-13 rows warm.
		if err := s.persistLastPlayedAt(ctx, item); err != nil {
			s.log.Warn("http stream: update last_played_at failed", "item_id", item.ID, "error", err)
		}
	}

	// Client range decided against persisted size BEFORE any resolve work:
	// a valid 416 never triggers resolution or refresh (D16).
	rng, satisfiable := httpstream.ParseRange(r.Header.Get("Range"), f.Size)
	if !satisfiable {
		w.Header().Set("Content-Range", httpstream.UnsatisfiedContentRange(f.Size))
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "Requested Range Not Satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return
	}

	// Live re-resolve through the protocol handler. Resolution failure is a
	// stream-time 5xx — NEVER a state transition (D13).
	if item.ResolveKey == nil {
		s.httpStreamUnavailable(w, item, httpstream.NewError(httpstream.ClassInvalidKey, "item has no resolve key"))
		return
	}
	key, kerr := httpstream.ParseResolveKey(*item.ResolveKey)
	if kerr != nil {
		s.httpStreamUnavailable(w, item, kerr)
		return
	}
	handler, hok := s.httpHandlers.Lookup(key.BackendID, key.Handler)
	if !hok {
		s.httpStreamUnavailable(w, item, httpstream.Wrapf(httpstream.ClassNoHandler,
			"protocol handler %q not configured", key.Handler))
		return
	}
	if f.ArchiveSize > 0 {
		archiveHandler, archiveOK := s.httpHandlers.LookupRemoteArchive(key.BackendID, key.Handler)
		if !archiveOK {
			s.httpStreamUnavailable(w, item, httpstream.NewError(httpstream.ClassNoHandler,
				"protocol handler has no remote archive arm"))
			return
		}
		descriptors, resolveErr := archiveHandler.ResolveRemoteArchives(ctx, httpstream.ResolveRequest{
			Key: key, Operation: httpstream.ResolveNormal,
		})
		if resolveErr != nil {
			s.httpStreamUnavailable(w, item, resolveErr)
			return
		}
		descriptor, matchErr := httpstream.MatchRemoteArchiveDescriptor(descriptors, f.Selector)
		if matchErr != nil {
			s.httpStreamUnavailable(w, item, matchErr)
			return
		}
		member, memberSource, openErr := s.openHTTPRemoteArchive(ctx, item.ID, f.FileID,
			key, f.ArchiveSize, archiveHandler, descriptor)
		if openErr != nil {
			s.httpStreamUnavailable(w, item, openErr)
			return
		}
		if member.Name != f.Name || member.Size != f.Size {
			s.httpStreamUnavailable(w, item, httpstream.NewError(httpstream.ClassRepresentationLost,
				"remote archive member changed since grab"))
			return
		}
		s.log.Info("http stream: archive play started", "item_id", item.ID, "file_id", f.FileID,
			"handler", key.Handler, "probe", probe)
		s.streamHTTPArchiveViaCache(w, r, item, f, memberSource, rng)
		return
	}
	if httpstream.IsRemoteArchiveSelector(key.Selector) {
		s.httpStreamUnavailable(w, item, httpstream.NewError(httpstream.ClassRepresentationLost,
			"remote archive file metadata is incomplete"))
		return
	}
	// HS-3.8 (A19): coalesce concurrent play-time resolves for this exact
	// (item,file) — a Jellyfin probe burst shares one backend call instead
	// of fanning out N-for-N. HS-3.9 (A20) extends the same coordinator
	// entry with an expiry-aware cache, so a repeat play/seek within the
	// file's own effective expiry is a pure cache hit with no backend call
	// at all. The coordinator runs the actual handler call (and the
	// validate + selector-match that used to happen here) on its own
	// bounded, server-owned context; this request's context only governs
	// how long THIS caller waits for that shared result.
	rf, rerr := s.resolveHTTPPlayback(ctx, item.ID, f.FileID, handler, key, f.Selector, false, "")
	if rerr != nil {
		if s.streamHTTPCrossLaneOnly(w, r, item, f, rng) {
			return
		}
		s.httpStreamUnavailable(w, item, rerr)
		return
	}
	// A source may disclose its byte extent only during live resolution. Carry
	// that exact value into delivered-byte accounting without guessing or
	// waiting for a later request (A-1's lazy denominator rule).
	if f.Size <= 0 && rf.Size > 0 {
		r = r.WithContext(withPlaybackProposalTarget(r.Context(), item, f.FileID, rf.Size))
	}

	s.log.Info("http stream: play started",
		"item_id", item.ID,
		"file_id", f.FileID,
		"display_name", item.DisplayName,
		"handler", key.Handler,
		"probe", probe,
	)
	if s.streamHTTPViaCache(w, r, item, f, rf, rng, handler, key) && !probe {
		s.queueNextEpisodePrewarm(item, key, f, rng)
	}
}

func (s *Server) streamHTTPCrossLaneOnly(w http.ResponseWriter, r *http.Request, item *store.Item, file httpstream.PersistedFile, rng *httpstream.ByteRange) bool {
	if s == nil || s.cfg == nil || !s.cfg.NNTP.CrossLaneSplice || file.Size <= 0 {
		return false
	}
	chunkBytes := int64(s.cfg.Cache.StreamChunkSizeMB) << 20
	source, err := newHTTPCrossLaneSource(item.ID, file.FileID, file.Size, chunkBytes, func(ctx context.Context, start, end int64) ([]byte, error) {
		return s.recoverHTTPCrossLaneWindow(ctx, item, file, start, end)
	})
	if err != nil {
		return false
	}
	s.log.Info("http stream: origin unavailable; trying proof-verified cross-lane route",
		"item_id", item.ID, "file_id", file.FileID)
	s.streamHTTPArchiveViaCache(w, r, item, file, source, rng)
	return true
}

func newHTTPCrossLaneSource(itemID, fileID string, size, chunkBytes int64, recover func(context.Context, int64, int64) ([]byte, error)) (bytesource.ByteSource, error) {
	if size <= 0 || chunkBytes <= 0 || recover == nil {
		return nil, errors.New("api: invalid HTTP cross-lane source")
	}
	chunkBytes = min64(chunkBytes, contentproof.MaxMappingProofBytes)
	count := (size-1)/chunkBytes + 1
	if count > contentproof.MaxLaneCandidates {
		return nil, errors.New("api: HTTP cross-lane source exceeds chunk limit")
	}
	chunks := make([]bytesource.Chunk, 0, count)
	starts := make([]int64, 0, count+1)
	for start, index := int64(0), 0; start < size; index++ {
		remaining := size - start
		length := min64(chunkBytes, remaining)
		starts = append(starts, start)
		chunks = append(chunks, bytesource.Chunk{Index: index, Key: strconv.Itoa(index), DeclaredSize: length})
		start += length
	}
	starts = append(starts, size)
	return bytesource.NewChunked("http-crosslane|"+httpRepresentationID(itemID, fileID), chunks, starts, size,
		bytesource.Capabilities{RangeSupport: true, ExactSize: true, TailCost: bytesource.TailCostCheap, Alignment: chunkBytes},
		func(ctx context.Context, chunk bytesource.Chunk) ([]byte, error) {
			if chunk.Index < 0 || chunk.Index >= len(chunks) {
				return nil, errors.New("api: invalid HTTP cross-lane chunk")
			}
			start := starts[chunk.Index]
			return recover(ctx, start, start+chunk.DeclaredSize-1)
		})
}

func isHLSFile(f httpstream.PersistedFile) bool {
	return isHLSNameType(f.Name, f.ContentType)
}

func isHLSResolvedFile(f httpstream.ResolvedFile) bool {
	return isHLSNameType(f.Name, f.ContentType)
}

func isHLSNameType(name, contentType string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(contentType)), "mpegurl") ||
		strings.HasSuffix(strings.ToLower(strings.TrimSpace(name)), ".m3u8")
}

// httpLadderChunkSource adds HR3.1's playback lease and continuity validation
// to the existing HTTP ByteSource block adapter. The sparse cache remains the
// outer first rung; NewByteSource owns current-origin plus one same-selector
// forced re-resolve. Proven mirror/cross-lane rungs are absent until their
// scheduled producers supply a cryptographically verified candidate.
type httpLadderChunkSource struct {
	rangecache.ChunkSource
	gov              *accountgov.Governor
	ledger           *contentproof.Ledger
	representationID string
	session          string
	maxWorkers       int
	offsets          []int64
	recover          func(context.Context, int64, int64) ([]byte, error)
	primeMu          sync.Mutex
	primed           map[int][]byte
}

func newHTTPLadderChunkSource(src rangecache.ChunkSource, gov *accountgov.Governor, ledger *contentproof.Ledger, representationID, session string) *httpLadderChunkSource {
	refs := src.Chunks()
	offsets := make([]int64, len(refs))
	var offset int64
	for i, ref := range refs {
		offsets[i] = offset
		offset += ref.Size
	}
	return &httpLadderChunkSource{
		ChunkSource:      src,
		gov:              gov,
		ledger:           ledger,
		representationID: representationID,
		session:          session,
		offsets:          offsets,
		primed:           make(map[int][]byte),
	}
}

func (s *httpLadderChunkSource) PayloadKey(ref rangecache.ChunkRef) string {
	if keyer, ok := s.ChunkSource.(rangecache.PayloadKeyer); ok {
		return keyer.PayloadKey(ref)
	}
	return s.Key() + "|" + ref.Key
}

func (s *httpLadderChunkSource) MaxWorkers() int { return s.maxWorkers }

func (s *httpLadderChunkSource) Fetch(ctx context.Context, ref rangecache.ChunkRef, class rangecache.FetchClass) ([]byte, error) {
	s.primeMu.Lock()
	if data, ok := s.primed[ref.Index]; ok {
		delete(s.primed, ref.Index)
		s.primeMu.Unlock()
		return data, nil
	}
	s.primeMu.Unlock()

	var data []byte
	l := ladder.New(s.gov, ladder.Rung{
		Name:        "current_reresolve",
		Op:          HTTPSourceGovOp,
		Priority:    accountgov.PriorityPlayback,
		MaxAttempts: 1,
		Attempt: func(attemptCtx context.Context) error {
			var err error
			data, err = s.ChunkSource.Fetch(attemptCtx, ref, class)
			return err
		},
	})
	result, err := l.Run(ctx, s.session)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		originErr := err
		if last := lastHTTPRungError(result); last != nil {
			originErr = errors.Join(last, err)
		}
		if s.recover != nil && ref.Index >= 0 && ref.Index < len(s.offsets) && ref.Size > 0 {
			start := s.offsets[ref.Index]
			if recovered, recoverErr := s.recover(ctx, start, start+ref.Size-1); recoverErr == nil && int64(len(recovered)) == ref.Size {
				return recovered, nil
			}
		}
		return nil, originErr
	}
	return data, nil
}

func lastHTTPRungError(result ladder.Result) error {
	if len(result.Rungs) == 0 {
		return nil
	}
	return result.Rungs[len(result.Rungs)-1].Err
}

func (s *httpLadderChunkSource) ValidateChunk(ctx context.Context, ref rangecache.ChunkRef, data []byte) error {
	if s.ledger == nil || ref.Index < 0 || ref.Index >= len(s.offsets) {
		return nil
	}
	digest := sha256.Sum256(data)
	entry, err := s.ledger.Observe(ctx, contentproof.Observation{
		RepresentationID: s.representationID,
		Offset:           s.offsets[ref.Index],
		Length:           int64(len(data)),
		Digest:           digest[:],
	})
	if err != nil {
		return err
	}
	if entry.Mutation || entry.Relation == contentproof.ContinuityConflict {
		return httpstream.NewError(httpstream.ClassUpstreamMalformed, "source block failed continuity validation")
	}
	return nil
}

func (s *httpLadderChunkSource) prime(ref rangecache.ChunkRef, data []byte) {
	s.primeMu.Lock()
	s.primed[ref.Index] = data
	s.primeMu.Unlock()
}

func httpRepresentationID(itemID, fileID string) string {
	digest := sha256.Sum256([]byte(itemID + "\x00" + fileID))
	return "http:" + hex.EncodeToString(digest[:])
}

func firstHTTPChunk(src rangecache.ChunkSource, rng *httpstream.ByteRange) (rangecache.ChunkRef, bool) {
	target := int64(0)
	if rng != nil {
		target = rng.Start
	}
	var offset int64
	for _, ref := range src.Chunks() {
		if target < offset+ref.Size {
			return ref, true
		}
		offset += ref.Size
	}
	return rangecache.ChunkRef{}, false
}

func (s *Server) streamHTTPViaCache(w http.ResponseWriter, r *http.Request, item *store.Item, pf httpstream.PersistedFile, initial httpstream.ResolvedFile, rng *httpstream.ByteRange, handler httpstream.Handler, key httpstream.ResolveKey) bool {
	if s.streamCache == nil {
		s.httpStreamUnavailable(w, item, httpstream.NewError(httpstream.ClassBackendUnavailable, "shared stream cache unavailable"))
		return false
	}
	fid := pf.FileID
	var initialMu sync.Mutex
	initialAvailable := true
	resolver := func(ctx context.Context, forced bool) (httpstream.ResolvedFile, error) {
		initialMu.Lock()
		if !forced && initialAvailable {
			initialAvailable = false
			initialMu.Unlock()
			return initial, nil
		}
		initialMu.Unlock()
		return s.resolveHTTPPlayback(ctx, item.ID, fid, handler, key, pf.Selector, forced, "hr3.1-reresolve")
	}
	blocks, err := s.httpChunkSource(item, pf, resolver)
	if err != nil {
		s.httpStreamUnavailable(w, item, err)
		return false
	}
	return s.streamHTTPBlocksViaCache(w, r, item, pf, blocks, rng)
}

func (s *Server) streamHTTPArchiveViaCache(w http.ResponseWriter, r *http.Request, item *store.Item, pf httpstream.PersistedFile, source bytesource.ByteSource, rng *httpstream.ByteRange) bool {
	if s.streamCache == nil {
		s.httpStreamUnavailable(w, item, httpstream.NewError(httpstream.ClassBackendUnavailable, "shared stream cache unavailable"))
		return false
	}
	blocks, err := rangecache.NewByteSourceBlocks(source, 0)
	if err != nil {
		s.httpStreamUnavailable(w, item, err)
		return false
	}
	return s.streamHTTPBlocksViaCache(w, r, item, pf, blocks, rng)
}

func (s *Server) streamHTTPBlocksViaCache(w http.ResponseWriter, r *http.Request, item *store.Item, pf httpstream.PersistedFile, blocks rangecache.ChunkSource, rng *httpstream.ByteRange) bool {
	fid := pf.FileID
	src := newHTTPLadderChunkSource(blocks, s.httpGov, s.httpContinuity, httpRepresentationID(item.ID, fid), item.ID)
	src.recover = func(ctx context.Context, start, end int64) ([]byte, error) {
		return s.recoverHTTPCrossLaneWindow(ctx, item, pf, start, end)
	}
	workers := s.cfg.Cache.StreamReadaheadWorkers
	if s.httpExpSvc != nil {
		workers = s.httpExpSvc.Workers(blocks, workers)
	}
	src.maxWorkers = workers

	first, ok := firstHTTPChunk(src, rng)
	if !ok {
		s.httpStreamUnavailable(w, item, httpstream.NewError(httpstream.ClassUpstreamMalformed, "requested source has no cache block"))
		return false
	}
	mode := s.streamMode()
	firstData, err := s.streamCache.GetChunk(r.Context(), src, mode, first)
	if err != nil {
		s.httpStreamUnavailable(w, item, err)
		return false
	}
	if mode == rangecache.ModeNone || mode == rangecache.ModeReadahead {
		// Those modes do not retain demand reads. Keep only this bounded first
		// block so Stream can commit headers without issuing it twice.
		src.prime(first, firstData)
	}

	ct := advertisedHTTPContentType(pf.ContentType, pf.Name)
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-store")
	minBuf := s.cfg.Cache.StreamMinBufferSegments
	if minBuf <= 0 {
		minBuf = 2
	}
	if err := s.streamCache.Stream(r.Context(), src, mode, pf.Size, w, r.Header.Get("Range"), minBuf); err != nil {
		if r.Context().Err() == nil {
			s.log.Warn("http stream: cache ladder aborted", "item_id", item.ID, "file_id", fid, "error", httpstream.Sanitize(err))
		}
		return false
	}
	succeeded := r.Context().Err() == nil
	if succeeded {
		// HR3.4: a genuine successful play start clears any pending
		// single-strike self-heal suspicion for this item -- a real
		// success between two failures means the earlier one was
		// transient origin trouble, not evidence of decay.
		s.observeHTTPStreamSuccess(item)
	}
	return succeeded
}

const (
	maxNextEpisodeCandidates      = 2048
	exactNextEpisodeConfidence    = 1.0
	requiredNextEpisodeConfidence = 1.0
)

func nextEpisodeCandidate(current httpstream.ResolveKey, items []*store.Item) (*store.Item, httpstream.PersistedFile, httpstream.ResolveKey, bool) {
	// Exact backend, handler, authoritative IDs, season, and N+1 episode
	// identity is the only accepted prediction and carries confidence 1.0.
	// There is deliberately no fuzzy/title-based lower-confidence path.
	if exactNextEpisodeConfidence < requiredNextEpisodeConfidence {
		return nil, httpstream.PersistedFile{}, httpstream.ResolveKey{}, false
	}
	var match *store.Item
	var matchFile httpstream.PersistedFile
	var matchKey httpstream.ResolveKey
	for _, item := range items {
		if item.ResolveKey == nil {
			continue
		}
		key, err := httpstream.ParseResolveKey(*item.ResolveKey)
		if err != nil || key.Kind != "episode" ||
			key.BackendID != current.BackendID || key.Handler != current.Handler ||
			key.Season != current.Season || key.Episode != current.Episode+1 ||
			!maps.Equal(key.IDs, current.IDs) {
			continue
		}
		if item.FileList == nil {
			continue
		}
		files, err := httpstream.ParseFiles(*item.FileList)
		if err != nil || len(files) != 1 || !files[0].RangeVerified || files[0].Size <= 0 || files[0].ArchiveSize > 0 || isHLSFile(files[0]) {
			continue
		}
		if match != nil {
			return nil, httpstream.PersistedFile{}, httpstream.ResolveKey{}, false
		}
		match, matchFile, matchKey = item, files[0], key
	}
	return match, matchFile, matchKey, match != nil
}

func (s *Server) queueNextEpisodePrewarm(current *store.Item, currentKey httpstream.ResolveKey, currentFile httpstream.PersistedFile, rng *httpstream.ByteRange) {
	if s.httpExpSvc == nil || currentKey.Kind != "episode" || currentFile.Size <= 0 {
		return
	}
	fraction := 1.0
	if rng != nil {
		fraction = float64(rng.End+1) / float64(currentFile.Size)
	}
	if fraction < s.cfg.Prewarm.NextEpisodeThreshold {
		return
	}

	s.nextEpisodeMu.Lock()
	if _, exists := s.nextEpisodePrewarmed[current.ID]; exists {
		s.nextEpisodeMu.Unlock()
		return
	}
	s.nextEpisodePrewarmed[current.ID] = struct{}{}
	s.nextEpisodeOrder = append(s.nextEpisodeOrder, current.ID)
	trimInsertionCache(s.nextEpisodePrewarmed, &s.nextEpisodeOrder)
	s.nextEpisodeMu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		items, err := s.store.ListReadyHTTPItems(s.shutdownCtx, maxNextEpisodeCandidates+1)
		if err != nil || len(items) > maxNextEpisodeCandidates {
			return
		}
		next, file, key, ok := nextEpisodeCandidate(currentKey, items)
		if !ok {
			return
		}
		handler, ok := s.httpHandlers.Lookup(key.BackendID, key.Handler)
		if !ok {
			return
		}
		resolver := func(ctx context.Context, forced bool) (httpstream.ResolvedFile, error) {
			return s.resolveHTTPPlayback(ctx, next.ID, file.FileID, handler, key, file.Selector, forced, "")
		}
		src, err := s.httpChunkSource(next, file, resolver)
		if err != nil {
			return
		}
		timeoutSec := s.cfg.Prewarm.TimeoutSec
		if timeoutSec <= 0 {
			timeoutSec = 45
		}
		ctx, cancel := context.WithTimeout(s.shutdownCtx, time.Duration(timeoutSec)*time.Second)
		defer cancel()
		if _, err := s.httpExpSvc.NextEpisodePrewarm(ctx, src, fraction); err != nil && ctx.Err() == nil {
			s.log.Warn("http stream: next-episode prewarm failed (non-fatal)", "item_id", next.ID, "error", httpstream.Sanitize(err))
		} else if ctx.Err() == nil {
			s.log.Info("http stream: next-episode prewarm complete", "item_id", next.ID, "prediction_confidence", exactNextEpisodeConfidence)
		}
	}()
}

// httpStreamUnavailable maps a resolve-side failure to the client status
// matrix: exhausted/failed resolution is 503 + Retry-After; malformed
// upstream semantics are 502. Upstream bodies are never relayed (HS-1.7).
func (s *Server) httpStreamUnavailable(w http.ResponseWriter, item *store.Item, err error) {
	s.log.Warn("http stream: source unavailable", "item_id", item.ID, "error", httpstream.Sanitize(err))
	// HR3.4: a plain client-side disconnect/cancel (Jellyfin abandoning a
	// probe, a seek tearing down the old range request, a browser tab
	// closing) is never evidence of source decay -- the same reasoning
	// attemptHTTPRelay's own "client cancelled ... never a refresh
	// trigger" rule and HR3.3's OriginScorer ("cancellations abstain")
	// already establish elsewhere in this lane. Transient backend outages
	// and rate limits also abstain: they are not evidence that the selected
	// representation has permanently decayed, and must never trigger the
	// destructive Arr re-search path.
	if !errors.Is(err, context.Canceled) && !httpstream.ClassOf(err).Transient() {
		s.observeHTTPStreamFailure(item)
	}
	if httpstream.ClassOf(err) == httpstream.ClassUpstreamMalformed {
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	w.Header().Set("Retry-After", "300")
	http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
}

// initHTTPSourceClients builds separate probe and live-relay clients on the
// shared D11 source-origin policy. Probe work is bounded end to end; relay
// work has no whole-movie deadline and uses an idle-read policy (HS-3.10).
func (s *Server) initHTTPSourceClients() {
	s.httpClientOnce.Do(func() {
		// D11: private-CIDR TrustSource dials stay denied by default; the
		// operator must explicitly opt in (HARRBOR_HTTP_ALLOW_PRIVATE_SOURCES)
		// for a self-hosted backend that hands back further LAN-hosted
		// playback URLs (HR0.2 live-gate finding, 2026-07-18).
		policy := httpstream.NewSecurityPolicy(s.cfg.HTTPStream.AllowPrivateSourceCIDRs)
		s.httpProbeClient = httpstream.NewHTTPClient(policy, httpstream.TrustSource, httpstream.TransportProbe)
		s.httpRelayClient = httpstream.NewHTTPClient(policy, httpstream.TrustSource, httpstream.TransportStream)
	})
}

func (s *Server) httpStreamClient() *http.Client {
	s.initHTTPSourceClients()
	return s.httpRelayClient
}

func (s *Server) httpProbeHTTPClient() *http.Client {
	s.initHTTPSourceClients()
	return s.httpProbeClient
}

// httpRelayOutcome classifies what attemptHTTPRelay did so relayHTTPSource
// knows whether to return, retry once, or map a final status (HS-3.11, A18).
type httpRelayOutcome int

const (
	// httpRelayDone means attemptHTTPRelay already wrote a complete,
	// final response to w (success, or a terminal non-refreshable
	// response like a valid 416) — the caller just returns.
	httpRelayDone httpRelayOutcome = iota
	// httpRelayRefreshable means nothing was written to w yet and the
	// failure is one forced refresh away from possibly succeeding.
	httpRelayRefreshable
)

// httpRelayResult carries the outcome plus enough to map a final status if
// refresh is exhausted (or fails outright).
type httpRelayResult struct {
	outcome    httpRelayOutcome
	err        error
	retryAfter string // upstream Retry-After (429 case only), unbounded/raw
}

// relayHTTPSource performs the live byte relay for one request, driving the
// HS-3.11 status matrix (A18, GPT audit §4 promoted finding): before any
// header is committed to the client, a refreshable failure gets exactly one
// forced re-resolve (HS-3.8's forced path + HS-3.9's Invalidate) and one
// retry; a still-failing or newly-failing attempt after that maps to a
// final client-visible status. A valid 416 (decided in streamHTTPItem
// against persisted size, before any resolve) and client cancellation never
// reach this refresh cycle at all — this function only ever sees the
// post-resolve relay attempt. DH owns the upstream Range header;
// hop-by-hop and identity-bearing headers are never forwarded in either
// direction; upstream Location is never relayed (D11).
func (s *Server) relayHTTPSource(w http.ResponseWriter, r *http.Request, item *store.Item, pf httpstream.PersistedFile, rf httpstream.ResolvedFile, rng *httpstream.ByteRange, handler httpstream.Handler, key httpstream.ResolveKey) {
	current := rf
	refreshed := false
	for {
		res := s.attemptHTTPRelay(w, r, item, pf, current, rng)
		if res.outcome == httpRelayDone {
			return
		}
		// httpRelayRefreshable: nothing written to w yet.
		if refreshed {
			s.httpRelayExhausted(w, item, res.err, res.retryAfter)
			return
		}
		refreshed = true
		s.httpResolveCoord.Invalidate(item.ID, pf.FileID)
		nf, nerr := s.resolveHTTPPlayback(r.Context(), item.ID, pf.FileID, handler, key, pf.Selector, true, "status-matrix-refresh")
		if nerr != nil {
			s.httpStreamUnavailable(w, item, nerr)
			return
		}
		current = nf
	}
}

func (s *Server) resolveHTTPPlayback(ctx context.Context, itemID, fileID string, handler httpstream.Handler, key httpstream.ResolveKey, selector string, forced bool, refreshContext string) (httpstream.ResolvedFile, error) {
	file, err := s.httpResolveCoord.Resolve(ctx, itemID, fileID, handler, key, selector, forced, refreshContext)
	if err != nil || !strings.EqualFold(key.Handler, "stremio") {
		return file, err
	}
	if _, err := s.unwrapMediaFlowResolvedFile(&file); err != nil {
		return httpstream.ResolvedFile{}, httpstream.NewError(httpstream.ClassUpstreamMalformed, "invalid self-origin playback source")
	}
	return file, nil
}

// attemptHTTPRelay performs exactly one upstream fetch + response
// classification. On success (or a terminal non-refreshable outcome like a
// valid 416) it writes the complete response to w and returns
// httpRelayDone. On a refreshable failure it writes nothing to w — the
// caller decides whether to retry.
func (s *Server) attemptHTTPRelay(w http.ResponseWriter, r *http.Request, item *store.Item, pf httpstream.PersistedFile, rf httpstream.ResolvedFile, rng *httpstream.ByteRange) httpRelayResult {
	ctx := r.Context()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rf.URL, nil)
	if err != nil {
		s.httpStreamUnavailable(w, item, httpstream.NewError(httpstream.ClassUpstreamMalformed, "source URL not requestable"))
		return httpRelayResult{outcome: httpRelayDone}
	}
	// Transient source headers (D8/D11): applied to the upstream request
	// only, never echoed downstream, never logged.
	for k, v := range rf.RequestHeaders {
		if badRelayHeader(k) {
			continue
		}
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept-Encoding", "identity")
	if rng != nil {
		req.Header.Set("Range", httpstream.UpstreamRangeHeader(*rng))
	}

	resp, err := s.httpStreamClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			// Client cancelled/disconnected before headers — never a
			// refresh trigger (A18: "never refresh ... client cancel").
			return httpRelayResult{outcome: httpRelayDone}
		}
		return httpRelayResult{outcome: httpRelayRefreshable,
			err: httpstream.NewError(httpstream.ClassBackendUnavailable, "source fetch failed")}
	}

	ct := advertisedHTTPContentType(pf.ContentType, pf.Name)
	if respCT := resp.Header.Get("Content-Type"); respCT != "" && !strings.HasPrefix(respCT, "text/") && !strings.Contains(respCT, "json") {
		ct = advertisedHTTPContentType(respCT, pf.Name)
	}

	switch {
	case rng != nil && resp.StatusCode == http.StatusPartialContent:
		if _, verr := httpstream.ValidateUpstream206(resp.Header.Get("Content-Range"), *rng); verr != nil {
			_ = resp.Body.Close()
			// Malformed range response: refreshable once (A18), 502 if
			// still malformed after refresh.
			return httpRelayResult{outcome: httpRelayRefreshable, err: verr}
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Range", httpstream.ContentRange(*rng, pf.Size))
		w.Header().Set("Content-Length", strconv.FormatInt(rng.Length(), 10))
		w.WriteHeader(http.StatusPartialContent)
		s.copyHTTPRelayBody(w, r, item, pf, resp)
		return httpRelayResult{outcome: httpRelayDone}
	case resp.StatusCode == http.StatusOK:
		if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/") || strings.Contains(resp.Header.Get("Content-Type"), "json") {
			_ = resp.Body.Close()
			// 200 with an HTML/JSON body is an expired/erroring source, not
			// media — refreshable once (A18 matrix row "non-media 200").
			return httpRelayResult{outcome: httpRelayRefreshable,
				err: httpstream.NewError(httpstream.ClassUpstreamMalformed, "source returned non-media body")}
		}
		// Honest 200: the source ignored (or was not sent) a Range.
		// Jellyfin handles full-content responses.
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "no-store")
		if cl := resp.Header.Get("Content-Length"); cl != "" {
			w.Header().Set("Content-Length", cl)
		}
		w.WriteHeader(http.StatusOK)
		s.copyHTTPRelayBody(w, r, item, pf, resp)
		return httpRelayResult{outcome: httpRelayDone}
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		_ = resp.Body.Close()
		// Upstream's own 416 against a URL DH just resolved: treated as
		// terminal, never refreshed, same as the pre-resolve "valid 416"
		// path in streamHTTPItem (A18: "never refresh ... a valid 416").
		w.Header().Set("Content-Range", httpstream.UnsatisfiedContentRange(pf.Size))
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "Requested Range Not Satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return httpRelayResult{outcome: httpRelayDone}
	case resp.StatusCode == http.StatusTooManyRequests:
		ra := resp.Header.Get("Retry-After")
		_ = resp.Body.Close()
		return httpRelayResult{outcome: httpRelayRefreshable,
			err: httpstream.Wrapf(httpstream.ClassBackendUnavailable, "source responded 429"), retryAfter: ra}
	default:
		// 401/403/404/410/502/503/504/other: refreshable once (A18),
		// mapped to a final sanitized 503 if still failing after refresh.
		_ = resp.Body.Close()
		return httpRelayResult{outcome: httpRelayRefreshable,
			err: httpstream.Wrapf(httpstream.ClassBackendUnavailable, "source responded %d", resp.StatusCode)}
	}
}

// copyHTTPRelayBody streams the already-committed response body. Once
// headers are written the response is final — a mid-body failure is never
// a refresh trigger (A21/A18): abort and let Jellyfin retry with a new
// Range, never mutate item state.
func (s *Server) copyHTTPRelayBody(w http.ResponseWriter, r *http.Request, item *store.Item, pf httpstream.PersistedFile, resp *http.Response) {
	defer func() { _ = resp.Body.Close() }()
	if r.Method == http.MethodHead {
		return
	}
	bufp := httpStreamCopyBufPool.Get().(*[]byte)
	defer httpStreamCopyBufPool.Put(bufp)
	if _, cerr := io.CopyBuffer(w, resp.Body, *bufp); cerr != nil {
		// After first bytes: abort and let Jellyfin retry with a new Range
		// (A21). Client cancels are normal seek/stop behavior.
		if r.Context().Err() == nil {
			s.log.Warn("http stream: relay aborted mid-body", "item_id", item.ID, "file_id", pf.FileID, "error", httpstream.Sanitize(cerr))
		}
	}
}

// httpRelayExhausted maps a refreshable failure that persisted through the
// one allowed forced refresh to its final client-visible status (A18):
// malformed upstream semantics (bad Content-Range, non-media 200 body) are
// always 502, never relayed raw; everything else (401/403/404/410/429/5xx/
// transport) is a sanitized 503 with a bounded Retry-After — honoring the
// upstream 429's own Retry-After when present, capped, never trusted
// unbounded.
func (s *Server) httpRelayExhausted(w http.ResponseWriter, item *store.Item, err error, upstreamRetryAfter string) {
	s.log.Warn("http stream: source unavailable after forced refresh", "item_id", item.ID, "error", httpstream.Sanitize(err))
	if httpstream.ClassOf(err) == httpstream.ClassUpstreamMalformed {
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	w.Header().Set("Retry-After", boundedRetryAfterSeconds(upstreamRetryAfter))
	http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
}

// boundedRetryAfterSeconds parses an upstream Retry-After (delta-seconds
// form only — HTTP-date is not honored, it is not worth trusting an
// upstream-supplied absolute clock) and bounds it to a sane client-visible
// range; missing/unparseable/non-positive falls back to the existing
// default. Never passes an unbounded upstream value straight through.
func boundedRetryAfterSeconds(raw string) string {
	const (
		defaultRetryAfter = 300
		maxRetryAfter     = 300
	)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return strconv.Itoa(defaultRetryAfter)
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs <= 0 {
		return strconv.Itoa(defaultRetryAfter)
	}
	if secs > maxRetryAfter {
		secs = maxRetryAfter
	}
	return strconv.Itoa(secs)
}

// preflightHLSSource validates one bounded root manifest through the shared
// HLS fetch/parser owner. It retains no manifest body or upstream address.
func (s *Server) preflightHLSSource(ctx context.Context, rf httpstream.ResolvedFile) error {
	body, err := s.fetchHLSPlaylistSource(ctx, hlsLiveSource{URL: rf.URL, Headers: rf.RequestHeaders})
	if err != nil {
		return err
	}
	if !hls.IsMaster(body) && !hls.IsMedia(body) {
		return httpstream.NewError(httpstream.ClassUpstreamMalformed, "not a recognizable HLS playlist")
	}
	return nil
}

// preflightHTTPSource performs the D9 grab-time verification: one bounded
// GET Range: bytes=0-0 from DH's real egress with final headers and capped
// redirects. It must observe a coherent 206 (span 0-0, parseable total) with
// a media-like content type. StateReady means "verified from DH now"; the
// preflight supplies size/type/range truth WITHOUT persisting the URL.
//
// HR5.3: when the HTTP lane's governor is configured, this real network
// fetch acquires a PriorityGrab lease on HTTPSourceGovOp first, so it
// genuinely contends for the same governed capacity as prewarmHTTPFile's
// PriorityPrewarm lease (LC-06) — sessionID is the item ID, for fair-share
// bookkeeping. nil-safe: an unconfigured httpGov leaves this fetch exactly
// as unleased as before this row.
func (s *Server) preflightHTTPSource(ctx context.Context, rf httpstream.ResolvedFile, sessionID string, representationID string, backendID string, handlerName string) (size int64, contentType string, err error) {
	if s.httpGov != nil {
		lease, lerr := s.httpGov.Acquire(ctx, HTTPSourceGovOp, accountgov.PriorityGrab, sessionID)
		if lerr != nil {
			return 0, "", httpstream.Wrapf(httpstream.ClassBackendUnavailable, "preflight governor lease: %v", lerr)
		}
		defer lease.Release()
	}
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, rf.URL, nil)
	if err != nil {
		return 0, "", httpstream.NewError(httpstream.ClassUpstreamMalformed, "source URL not requestable")
	}
	for k, v := range rf.RequestHeaders {
		if badRelayHeader(k) {
			continue
		}
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Range", "bytes=0-0")

	resp, err := s.httpProbeHTTPClient().Do(req)
	if err != nil {
		return 0, "", httpstream.NewError(httpstream.ClassBackendUnavailable, "preflight fetch failed")
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
		_ = resp.Body.Close()
	}()

	ct := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	if badPreflightContentType(ct) {
		// HTML/JSON bodies and HLS/DASH manifests are rejections (D9).
		return 0, "", httpstream.Wrapf(httpstream.ClassNoSource, "preflight returned non-media content type")
	}
	if resp.StatusCode != http.StatusPartialContent {
		// 200 means non-seekable (no range support); 401/403/404/5xx mean the
		// source is not usable from DH's egress right now. All are rejections
		// at grab time — the arr falls through to another release.
		return 0, "", httpstream.Wrapf(httpstream.ClassNoSource, "preflight responded %d (need 206)", resp.StatusCode)
	}
	total, verr := httpstream.ValidateUpstream206(resp.Header.Get("Content-Range"), httpstream.ByteRange{Start: 0, End: 0})
	if verr != nil {
		return 0, "", verr
	}
	if total <= 0 {
		return 0, "", httpstream.NewError(httpstream.ClassUpstreamMalformed, "preflight 206 without total length")
	}
	if rf.Size > 0 && rf.Size != total {
		return 0, "", httpstream.NewError(httpstream.ClassRepresentationLost, "preflight size does not match selected representation")
	}
	// HR2.7: feed authoritative IA/Metalink digests the handler already
	// supplied, plus this real live response's own Digest/ETag headers,
	// into the shared content proof graph. Best-effort and strictly
	// non-fatal -- this is proof-graph bookkeeping, never a playback gate,
	// and never reopens the verified StateReady path above it.
	s.recordHTTPSourceProofs(ctx, representationID, total, rf.Digests, resp.Header, backendID, handlerName)
	return total, advertisedHTTPContentType(resp.Header.Get("Content-Type"), rf.Name), nil
}

// advertisedHTTPContentType keeps valid origin types byte-for-byte but never
// advertises an absent, malformed, or generic binary type to an Arr/player.
func advertisedHTTPContentType(originType, name string) string {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(originType))
	if err != nil || mediaType == "" || strings.EqualFold(mediaType, "application/octet-stream") {
		return fileExtContentType(name)
	}
	return originType
}

// httpProofOriginID identifies the operator-configured backend instance an
// HTTP source was resolved through, purely as a stable, opaque, secret-free
// OriginID for same-origin ETag evidence (HR2.7). It is never the source
// host or URL (DG-04) -- only the resolve key's own backend/handler
// identity, already non-secret operator config -- so two different
// backends' ETags for coincidentally-equal representation IDs are never
// compared against each other as "same origin".
func httpProofOriginID(backendID, handlerName string) string {
	sum := sha256.Sum256([]byte("darkharrbor/http-origin/v1\x00" + backendID + "\x00" + handlerName))
	return "origin:" + hex.EncodeToString(sum[:])[:32]
}

// recordHTTPSourceProofs is HR2.7's single entry point into the shared
// content proof graph for the HTTP lane: authoritative whole-object
// digests (IA/Metalink from the handler, plus a live RFC 3230 Digest
// response header) and one same-origin ETag observation. A digest or ETag
// that conflicts with previously recorded evidence for the exact same
// representation/span/origin is rejected (logged, dropped) rather than
// silently overwritten -- an authoritative disagreement, or a same-origin
// ETag change with no corresponding new evidence, is exactly the ambiguous
// case this row must not paper over. Nil-safe and non-fatal throughout.
func (s *Server) recordHTTPSourceProofs(ctx context.Context, representationID string, size int64, digests []httpstream.SourceDigest, header http.Header, backendID, handlerName string) {
	if s.httpProofGraph == nil || representationID == "" || size <= 0 {
		return
	}
	now := s.nowUTC()

	// httpstream.SourceDigest is deliberately protocol-agnostic (the leaf
	// httpstream package never imports contentproof -- WORKFLOW rule 3: no
	// second classifier), so provenance is derived here from the resolve
	// key's own handler name, exactly like every other handler-specific
	// branch already in this file (e.g. isHLSResolvedFile).
	provenance := contentproof.ProvenanceHTTPDigest
	switch handlerName {
	case "ia":
		provenance = contentproof.ProvenanceIADigest
	case "generic":
		provenance = contentproof.ProvenanceMetalink
	}
	for _, d := range digests {
		s.recordAuthoritativeDigest(ctx, representationID, size, provenance, d, now)
	}

	for _, d := range httpstream.ParseDigestHeader(header.Get("Digest")) {
		s.recordAuthoritativeDigest(ctx, representationID, size, contentproof.ProvenanceHTTPDigest, d, now)
	}

	if etag := strings.TrimSpace(header.Get("ETag")); etag != "" {
		s.recordSameOriginETag(ctx, representationID, size, etag, backendID, handlerName, now)
	}
}

// recordAuthoritativeDigest validates and records one whole-object
// authoritative digest, refusing to overwrite a conflicting prior proof for
// the exact same representation/span/algorithm.
func (s *Server) recordAuthoritativeDigest(ctx context.Context, representationID string, size int64, provenance contentproof.Provenance, d httpstream.SourceDigest, now time.Time) {
	if !httpstream.ValidSourceDigest(d) {
		return
	}
	algorithm, digest, ok := decodeSourceAlgorithm(d)
	if !ok {
		return
	}
	decision, err := s.httpProofGraph.VerifyDigest(ctx, representationID, contentproof.ScopeWhole, 0, size, algorithm, digest)
	if err != nil {
		s.log.Warn("http proof: verify digest failed (non-fatal)", "representation_id", representationID, "error", httpstream.Sanitize(err))
		return
	}
	if decision.Relation == contentproof.RelationConflict {
		s.log.Warn("http proof: rejected conflicting authoritative digest",
			"representation_id", representationID, "provenance", provenance, "algorithm", algorithm)
		return
	}
	evidence := contentproof.Evidence{
		RepresentationID: representationID, Scope: contentproof.ScopeWhole, Offset: 0, Length: size,
		Kind: contentproof.KindAuthoritative, Algorithm: algorithm, Digest: digest,
		Provenance: provenance, ObservedAt: now,
	}
	if err := s.httpProofGraph.Record(ctx, evidence); err != nil {
		s.log.Warn("http proof: record authoritative digest failed (non-fatal)", "representation_id", representationID, "error", httpstream.Sanitize(err))
	}
}

// recordSameOriginETag records one same-origin ETag observation, rejecting
// (never overwriting) a differing ETag digest previously observed for the
// exact same representation/span/origin.
func (s *Server) recordSameOriginETag(ctx context.Context, representationID string, size int64, etag string, backendID, handlerName string, now time.Time) {
	if s.store == nil {
		return
	}
	originID := httpProofOriginID(backendID, handlerName)
	sum := sha256.Sum256([]byte(etag))
	digest := sum[:]

	existing, err := s.store.ListContentProofs(ctx, []string{representationID}, now)
	if err != nil {
		s.log.Warn("http proof: list existing proofs failed (non-fatal)", "representation_id", representationID, "error", err)
		return
	}
	for _, proof := range existing {
		if proof.Kind != contentproof.KindSameOrigin || proof.Provenance != contentproof.ProvenanceSameOriginETag ||
			proof.OriginID != originID || proof.Scope != contentproof.ScopeWhole || proof.Offset != 0 || proof.Length != size {
			continue
		}
		if !bytes.Equal(proof.Digest, digest) {
			s.log.Warn("http proof: rejected conflicting same-origin etag",
				"representation_id", representationID, "origin_id", originID)
			return
		}
		return
	}

	evidence := contentproof.Evidence{
		RepresentationID: representationID, Scope: contentproof.ScopeWhole, Offset: 0, Length: size,
		Kind: contentproof.KindSameOrigin, Algorithm: contentproof.AlgorithmETAGSHA256, Digest: digest,
		Provenance: contentproof.ProvenanceSameOriginETag, OriginID: originID, ObservedAt: now,
	}
	if err := s.httpProofGraph.Record(ctx, evidence); err != nil {
		s.log.Warn("http proof: record same-origin etag failed (non-fatal)", "representation_id", representationID, "error", err)
	}
}

func decodeSourceAlgorithm(d httpstream.SourceDigest) (contentproof.Algorithm, []byte, bool) {
	var algorithm contentproof.Algorithm
	switch d.Algorithm {
	case "md5":
		algorithm = contentproof.AlgorithmMD5
	case "sha1":
		algorithm = contentproof.AlgorithmSHA1
	case "sha256":
		algorithm = contentproof.AlgorithmSHA256
	default:
		return "", nil, false
	}
	digest, err := hex.DecodeString(d.Hex)
	if err != nil {
		return "", nil, false
	}
	return algorithm, digest, true
}

// badPreflightContentType rejects manifest/text/JSON responses (D9).
func badPreflightContentType(ct string) bool {
	switch {
	case ct == "":
		return false // absent header tolerated; range coherence is the gate
	case strings.HasPrefix(ct, "text/"),
		strings.Contains(ct, "json"),
		strings.Contains(ct, "mpegurl"),  // HLS
		strings.Contains(ct, "dash+xml"), // DASH
		strings.Contains(ct, "xml"):
		return true
	}
	return false
}

// badRelayHeader filters hop-by-hop and DH-owned headers from transient
// source header maps (D11).
func badRelayHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "host", "content-length", "transfer-encoding", "range", "connection",
		"keep-alive", "proxy-authenticate", "proxy-authorization", "te",
		"trailer", "upgrade", "accept-encoding":
		return true
	}
	return false
}

func strFromPtr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ── /torznab/http-stream (HS-1.6, HS-1.10) ───────────────────────────────────

// handleTorznabHTTPStream serves caps and searches for the dedicated HTTP
// stream indexer registration (registered per arr by the H5 wizard, 2+2+1
// topology). Fully isolated from the shared torznab paths: its own cache
// namespace, no magnetCache writes for HTTP results; infoHash results use
// the existing torrent /download lane. Auth posture: unauthenticated on arr-net like the other
// torznab routes; the grab envelope HMAC is the enqueue integrity boundary
// (D15).
func (s *Server) handleTorznabHTTPStream(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	switch strings.ToLower(strings.TrimSpace(q.Get("t"))) {
	case "tvsearch":
		s.httpStreamSearch(w, r, "episode")
	case "movie":
		s.httpStreamSearch(w, r, "movie")
	case "search":
		s.httpStreamSearch(w, r, searchTypeFromCats(q.Get("cat")))
	default:
		s.httpStreamCaps(w)
	}
}

// httpIDNative is the optional handler capability marker: handlers that can
// answer ID-only queries (OMSS/Stremio, H3) implement it and return true.
type httpIDNative interface{ IDNative() bool }

// httpStreamIDNative reports whether any registered handler answers ID-only
// queries.
func (s *Server) httpStreamIDNative() bool {
	if s.httpHandlers == nil {
		return false
	}
	for _, h := range s.httpHandlers.Handlers() {
		if idn, ok := h.(httpIDNative); ok && idn.IDNative() {
			return true
		}
	}
	return false
}

// httpStreamCaps advertises search params matched to the REGISTERED handler
// set (A16, live finding torznab.go:149): when an arr sees ID caps it sends
// ID-ONLY queries (no q). The ia handler requires a title (HS-2.1), so an
// ia-only deployment must advertise text search — otherwise every automatic
// search arrives titleless and returns empty. ID params are advertised only
// once an ID-native handler (OMSS/Stremio, H3) is registered. NOTE: arrs
// cache caps in memory — restart the arr or re-save the indexer after the
// registered handler set changes (live finding 2026-07-08).
func (s *Server) httpStreamCaps(w http.ResponseWriter) {
	type attrSearch struct {
		Available       string `xml:"available,attr"`
		SupportedParams string `xml:"supportedParams,attr"`
	}
	type capsSearching struct {
		Search      attrSearch `xml:"search"`
		TVSearch    attrSearch `xml:"tv-search"`
		MovieSearch attrSearch `xml:"movie-search"`
	}
	type capsCategory struct {
		ID   string `xml:"id,attr"`
		Name string `xml:"name,attr"`
	}
	type capsCategories struct {
		Category []capsCategory `xml:"category"`
	}
	type capsServer struct {
		Version string `xml:"version,attr"`
		Title   string `xml:"title,attr"`
		URL     string `xml:"url,attr"`
	}
	type caps struct {
		XMLName    xml.Name       `xml:"caps"`
		Server     capsServer     `xml:"server"`
		Searching  capsSearching  `xml:"searching"`
		Categories capsCategories `xml:"categories"`
	}
	idNative := s.httpStreamIDNative()
	out := caps{
		Server: capsServer{
			Version: "1.1",
			Title:   "Dark Harrbor – HTTP Stream",
			URL:     s.cfg.Server.BaseURL + "/torznab/http-stream",
		},
		Searching: capsSearching{
			Search:      attrSearch{Available: "yes", SupportedParams: "q"},
			TVSearch:    attrSearch{Available: "yes", SupportedParams: "q,season,ep"},
			MovieSearch: attrSearch{Available: "yes", SupportedParams: "q"},
		},
		Categories: capsCategories{
			Category: []capsCategory{
				{ID: "5000", Name: "TV"},
				{ID: "5040", Name: "TV/HD"},
				{ID: "5045", Name: "TV/UHD"},
				{ID: "2000", Name: "Movies"},
				{ID: "2040", Name: "Movies/HD"},
				{ID: "2045", Name: "Movies/UHD"},
			},
		},
	}
	if idNative {
		out.Searching.TVSearch.SupportedParams = "q,season,ep,tvdbid,tmdbid,imdbid"
		out.Searching.MovieSearch.SupportedParams = "q,tmdbid,imdbid"
	}
	writeTorznabXML(w, &out)
}

// httpStreamSearch fans the arr query over the handler registry and renders
// the feed. Cache namespace is "http|..."; only successful/authoritative
// results are cached — an all-transient-failure pass is never frozen as a
// five-minute empty result (HS-1.6).
func (s *Server) httpStreamSearch(w http.ResponseWriter, r *http.Request, kind string) {
	q := r.URL.Query()

	// Arr RSS-window / save-time test fetch (HR0.3): answered with the
	// deterministic connectivity sentinel even while the feature is disabled
	// or no backend is registered, so the indexer registration itself is
	// always creatable and RSS windows complete with zero coverage warnings
	// (Sonarr blocks creation on a zero-result test — live 2026-07-17).
	// Targeted searches below keep the authoritative-empty dark-landing
	// behavior unchanged. The sentinel never touches magnetCache and cannot
	// be grabbed: it appears only on parameterless window queries.
	if isRSSWindowQuery(q) {
		if atoiClamp(q.Get("offset"), 0, 0, 1<<20) > 0 {
			s.writeHTTPStreamFeed(w, nil)
			return
		}
		category := "5000"
		if kind == "movie" {
			category = "2000"
		}
		s.writeHTTPStreamFeedItems(w, []torznabItem{{
			Title:    "DarkHarrbor.Feed.Sentinel.HTTP",
			InfoHash: "feedfacefeedfacefeedfacefeedface0000ffff",
			Size:     1 << 20,
			Category: category,
			PubDate:  sentinelPubDate,
			Protocol: "http",
			NZBToken: "feed-sentinel",
		}})
		return
	}

	if !s.cfg.HTTPStreamEnabled() || s.httpHandlers.Empty() {
		s.writeHTTPStreamFeed(w, nil)
		return
	}

	title, year := splitTrailingYear(strings.TrimSpace(q.Get("q")))
	query := httpstream.StreamQuery{
		Kind:    kind,
		Title:   title,
		Year:    year,
		Season:  parseIntParam(q.Get("season")),
		Episode: parseIntParam(q.Get("ep")),
		IMDBID:  strings.TrimSpace(q.Get("imdbid")),
		TVDBID:  strings.TrimSpace(q.Get("tvdbid")),
		TMDBID:  strings.TrimSpace(q.Get("tmdbid")),
	}
	if kind == "episode" && query.TVDBID != "" && query.Season > 0 && query.Episode > 0 {
		if title, episodes := s.episodeContext(r.Context(), query.TVDBID); len(episodes) > 0 {
			query.Episodes = episodes
			if query.Title == "" {
				query.Title = title
			}
		}
	}

	// Season-only tvsearch: exact-episode protocols cannot serve it and DH
	// never synthesizes fake season packs — authoritative empty (D12).
	if kind == "episode" && query.Season > 0 && query.Episode == 0 {
		s.writeHTTPStreamFeed(w, nil)
		return
	}

	cacheKey := "http|" + torznabCacheKey(kind, q)
	if cached, ok := s.torznabCacheGet(cacheKey); ok {
		s.writeHTTPStreamFeedItems(w, cached)
		return
	}

	var results []httpstream.SearchResult
	outcomes, authoritative := s.httpHandlers.Search(r.Context(), query)
	for _, outcome := range outcomes {
		if outcome.Err != nil {
			s.log.Warn("http-stream search: backend failed", "backend_id", outcome.BackendID,
				"handler", outcome.Handler, "error", httpstream.Sanitize(outcome.Err))
			continue
		}
		results = append(results, outcome.Results...)
	}

	items, err := s.renderHTTPStreamItems(r.Context(), results, query)
	if err != nil {
		s.log.Warn("http-stream search: render failed", "error", httpstream.Sanitize(err))
		s.writeHTTPStreamFeed(w, nil)
		return
	}
	if authoritative {
		s.torznabCacheSet(cacheKey, items)
	}
	s.writeHTTPStreamFeedItems(w, items)
}

// renderHTTPStreamItems converts handler results to feed items, deduping on
// the canonical resolve key (HS-1.6) and applying the D2/D17 presentation
// contract: DH-HTTP title token, WEBDL quality mapping, deterministic size
// estimates when the backend supplies none.
func (s *Server) renderHTTPStreamItems(ctx context.Context, results []httpstream.SearchResult, q httpstream.StreamQuery) ([]torznabItem, error) {
	seen := make(map[string]bool, len(results))
	items := make([]torznabItem, 0, len(results))
	identityKind := "series"
	if strings.EqualFold(q.Kind, "movie") {
		identityKind = "movie"
	}
	identity := s.providerIdentityForQuery(ctx, identityKind, q.Title, q.TVDBID, q.TMDBID, q.IMDBID)
	identity = scopeProviderIdentity(identity, q.Season, q.Episode)
	itemCategory := "5000"
	if strings.EqualFold(q.Kind, "movie") {
		itemCategory = "2000"
	}
	for _, res := range results {
		if res.Protocol == "torrent" {
			infoHash := extractInfoHash(res.InfoHash)
			if infoHash == "" || s.isReleaseSuppressed(ctx, res.Title, res.Size, suppress.LaneHTTP) {
				continue
			}
			canonical := "torrent:" + infoHash
			if seen[canonical] {
				continue
			}
			seen[canonical] = true
			items = append(items, torznabItem{
				Title: httpstream.TagTitle(res.Title), InfoHash: infoHash,
				Size: res.Size, Protocol: "torrent", Category: itemCategory,
				PubDate:          time.Now().UTC().Format(time.RFC1123Z),
				ProviderIdentity: identity,
				TVDBID:           strings.TrimSpace(q.TVDBID), IMDBID: strings.TrimSpace(q.IMDBID),
				TMDBID: strings.TrimSpace(q.TMDBID),
			})
			continue
		}
		canonical, err := res.Key.Canonical()
		if err != nil {
			s.log.Warn("http-stream: dropping result with invalid key", "error", httpstream.Sanitize(err))
			continue
		}
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		if s.isReleaseSuppressed(ctx, res.Title, res.Size, suppress.LaneHTTP) {
			continue
		}

		env := httpstream.GrabEnvelope{
			Key: res.Key, Title: httpstream.TagTitle(res.Title), ProviderIdentity: identity,
		}
		token, err := httpstream.EncodeGrabToken(s.cfg.Server.StreamSecret, env)
		if err != nil {
			return nil, err
		}
		digest := httpstream.DigestOfCanonical(canonical)

		size := res.Size
		category := "5000"
		if strings.EqualFold(res.Key.Kind, "movie") {
			category = "2000"
		}
		if size <= 0 {
			resLabel, _ := httpstream.QualityToRes(res.Quality)
			size = httpstream.SizeEstimate(res.Key.Kind, resLabel)
		}
		items = append(items, torznabItem{
			Title:    env.Title,
			InfoHash: digest,
			Size:     size,
			Category: category,
			PubDate:  time.Now().UTC().Format(time.RFC1123Z),
			Protocol: "http",
			NZBToken: token, // reused as the /grab/http token slot for http items
			TVDBID:   strings.TrimSpace(q.TVDBID),
			IMDBID:   strings.TrimSpace(q.IMDBID),
			TMDBID:   strings.TrimSpace(q.TMDBID),
		})
	}
	return items, nil
}

// writeHTTPStreamFeedItems renders the http-stream RSS. HTTP enclosures are
// torrent-typed (the arr registers this route as a Torznab indexer; D1's
// marker contract carries the truth) and point at /grab/http/{token}.
// magnetCache is NEVER touched for HTTP results (HS-1.6).
func (s *Server) writeHTTPStreamFeedItems(w http.ResponseWriter, items []torznabItem) {
	type tnAttr struct {
		XMLName xml.Name `xml:"torznab:attr"`
		Name    string   `xml:"name,attr"`
		Value   string   `xml:"value,attr"`
	}
	type tnEnclosure struct {
		XMLName xml.Name `xml:"enclosure"`
		URL     string   `xml:"url,attr"`
		Length  int64    `xml:"length,attr"`
		Type    string   `xml:"type,attr"`
	}
	type tnItem struct {
		XMLName   xml.Name `xml:"item"`
		Title     string   `xml:"title"`
		GUID      string   `xml:"guid"`
		PubDate   string   `xml:"pubDate"`
		Size      int64    `xml:"size"`
		Enclosure tnEnclosure
		Attrs     []tnAttr
	}
	type tnChannel struct {
		XMLName xml.Name `xml:"channel"`
		Title   string   `xml:"title"`
		Items   []tnItem
	}
	type tnRSS struct {
		XMLName xml.Name `xml:"rss"`
		Version string   `xml:"version,attr"`
		XMLNS   string   `xml:"xmlns:torznab,attr"`
		Channel tnChannel
	}
	rss := tnRSS{
		Version: "2.0",
		XMLNS:   "http://torznab.com/schemas/2015/feed",
		Channel: tnChannel{Title: "Dark Harrbor – HTTP Stream"},
	}
	base := strings.TrimRight(s.cfg.Server.BaseURL, "/")
	for _, it := range items {
		if it.Protocol == "torrent" {
			if it.Size <= 0 {
				it.Size = httpstream.SizeEstimate("movie", "")
				if it.Category == "5000" {
					it.Size = httpstream.SizeEstimate("episode", "")
				}
			}
			hash := strings.ToLower(it.InfoHash)
			s.magnetCacheSet(context.Background(), hash, "magnet:?xt=urn:btih:"+hash, it.Title)
			_ = s.persistGrabProviderIdentity(context.Background(), "torrent:"+hash, it.ProviderIdentity)
			downloadURL := base + "/download/" + hash
			if identityID := providerIdentityContextID(it.ProviderIdentity); identityID != "" &&
				s.persistGrabProviderIdentity(context.Background(), "torrent-context:"+identityID, it.ProviderIdentity) {
				downloadURL += "?idc=" + identityID
			}
			attrs := []tnAttr{
				{Name: "category", Value: it.Category},
				{Name: "size", Value: strconv.FormatInt(it.Size, 10)},
				{Name: "infohash", Value: it.InfoHash},
				{Name: "seeders", Value: "1"},
				{Name: "peers", Value: "1"},
				{Name: "downloadvolumefactor", Value: "0"},
				{Name: "uploadvolumefactor", Value: "1"},
			}
			for _, value := range []struct{ name, id string }{
				{"tvdbid", it.TVDBID}, {"imdbid", it.IMDBID}, {"tmdbid", it.TMDBID},
			} {
				if value.id != "" {
					attrs = append(attrs, tnAttr{Name: value.name, Value: value.id})
				}
			}
			rss.Channel.Items = append(rss.Channel.Items, tnItem{
				Title: it.Title, GUID: it.InfoHash, PubDate: it.PubDate, Size: it.Size,
				Enclosure: tnEnclosure{URL: downloadURL, Length: it.Size, Type: "application/x-bittorrent"},
				Attrs:     attrs,
			})
			continue
		}
		attrs := []tnAttr{
			{Name: "category", Value: it.Category},
			{Name: "size", Value: strconv.FormatInt(it.Size, 10)},
			{Name: "infohash", Value: it.InfoHash},
			{Name: "seeders", Value: "1"},
			{Name: "peers", Value: "1"},
			{Name: "downloadvolumefactor", Value: "0"},
			{Name: "uploadvolumefactor", Value: "1"},
		}
		// Echo the arr's own tvdbid/imdbid/tmdbid back on the item (never
		// invented here — see torznabItem doc comment) so the arr can match
		// this release to its library entry by ID even when the rendered
		// title text alone is not reliably parseable back to a known
		// series/movie (HR0.2 live-gate finding, 2026-07-18).
		if it.TVDBID != "" {
			attrs = append(attrs, tnAttr{Name: "tvdbid", Value: it.TVDBID})
		}
		if it.IMDBID != "" {
			attrs = append(attrs, tnAttr{Name: "imdbid", Value: it.IMDBID})
		}
		if it.TMDBID != "" {
			attrs = append(attrs, tnAttr{Name: "tmdbid", Value: it.TMDBID})
		}
		rss.Channel.Items = append(rss.Channel.Items, tnItem{
			Title:   it.Title,
			GUID:    it.InfoHash,
			PubDate: it.PubDate,
			Size:    it.Size,
			Enclosure: tnEnclosure{
				URL:    base + "/grab/http/" + it.NZBToken,
				Length: it.Size,
				Type:   "application/x-bittorrent",
			},
			Attrs: attrs,
		})
	}
	writeTorznabXML(w, &rss)
}

// splitTrailingYear extracts a trailing release year from a text query —
// Radarr sends q="{title} {year}" to text-only indexers, and the ia
// handler's conservative title match would otherwise reject its own query.
func splitTrailingYear(q string) (title string, year int) {
	m := trailingYearRe.FindStringSubmatch(q)
	if m == nil {
		return q, 0
	}
	y, _ := strconv.Atoi(m[2])
	return strings.TrimSpace(m[1]), y
}

var trailingYearRe = regexp.MustCompile(`^(.*\S)\s+((?:19|20)\d{2})$`)

// writeHTTPStreamFeed writes an empty (or nil) feed.
func (s *Server) writeHTTPStreamFeed(w http.ResponseWriter, items []torznabItem) {
	s.writeHTTPStreamFeedItems(w, items)
}
