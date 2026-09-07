package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	archiveparser "github.com/darkharrbor/darkharrbor/internal/archive"
	"github.com/darkharrbor/darkharrbor/internal/bytesource"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func (s *Server) torrentRARByteSource(ctx context.Context, item *store.Item) (bytesource.ByteSource, error) {
	manifest := item.Metadata.TorrentRARManifest
	if manifest == nil || len(manifest.Parts) == 0 {
		return nil, fmt.Errorf("cdnrar: missing manifest")
	}
	windowBytes := int64(s.cfg.Cache.StreamChunkSizeMB) * 1024 * 1024
	if windowBytes <= 0 {
		windowBytes = 16 << 20
	}
	sources := make([]bytesource.ByteSource, len(manifest.Parts))
	for i, part := range manifest.Parts {
		part := part
		value, err, _ := s.resolveGroup.Do("cdnrar|"+resolveKey(item.ID, part.FileID), func() (interface{}, error) {
			reresolve := func(rctx context.Context) (string, error) {
				s.invalidateResolveEntry(item.ID, part.FileID)
				return s.pickDebridProvider(item).RequestDownloadURL(rctx, item, part.FileID)
			}
			if cached := s.resolveEntryFor(item.ID, part.FileID); cached != nil &&
				cached.rangeCapable != nil && *cached.rangeCapable && cached.total == part.ArchiveSize {
				return newCDNByteSource(
					"cdnrar|"+resolveKey(item.ID, part.FileID),
					cached.cdnURL,
					part.ArchiveSize,
					windowBytes,
					"application/vnd.rar",
					reresolve,
					func() { s.invalidateResolveEntry(item.ID, part.FileID) },
					s.torrentGov,
					accountgov.PriorityPlayback,
					item.ID,
				)
			}
			dlURL, err := s.pickDebridProvider(item).RequestDownloadURL(ctx, item, part.FileID)
			if err != nil || dlURL == "" {
				return nil, fmt.Errorf("cdnrar: request volume: %s", httpstream.Sanitize(err))
			}
			src, ranged, err := NewProbedCDNByteSource(
				ctx,
				"cdnrar|"+resolveKey(item.ID, part.FileID),
				dlURL,
				windowBytes,
				reresolve,
				func() { s.invalidateResolveEntry(item.ID, part.FileID) },
				s.torrentGov,
				accountgov.PriorityPlayback,
				item.ID,
			)
			if err != nil {
				return nil, fmt.Errorf("cdnrar: range probe: %s", httpstream.Sanitize(err))
			}
			if !ranged {
				return nil, fmt.Errorf("cdnrar: provider volume does not support byte ranges")
			}
			capable := true
			s.cacheResolveEntry(item.ID, part.FileID, dlURL, "application/vnd.rar", part.ArchiveSize, &capable)
			return src, nil
		})
		if err != nil {
			return nil, err
		}
		sources[i] = value.(bytesource.ByteSource)
	}
	return archiveparser.NewStoredRARByteSource("torrent-rar|"+item.ID, *manifest, sources)
}

func (s *Server) handleTorrentRAR(w http.ResponseWriter, r *http.Request, item *store.Item, fileID string) {
	manifest := item.Metadata.TorrentRARManifest
	if manifest == nil || fileID != "rar:0" {
		http.NotFound(w, r)
		return
	}
	contentType := fileExtContentType(manifest.MemberName)
	if contentType == "" {
		contentType = "video/x-matroska"
	}
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", manifest.TotalSize))
		w.WriteHeader(http.StatusOK)
		return
	}
	if s.streamCache == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "archive_cache_unavailable"})
		return
	}
	src, err := s.torrentRARByteSource(r.Context(), item)
	if err != nil {
		s.log.Warn("stream: cdn RAR resolve failed", "item_id", item.ID, "error", httpstream.Sanitize(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": httpstream.Sanitize(err)})
		return
	}
	blockBytes := int64(s.cfg.Cache.StreamChunkSizeMB) * 1024 * 1024
	if blockBytes <= 0 {
		blockBytes = 16 << 20
	}
	chunks, err := rangecache.NewByteSourceBlocks(src, blockBytes)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "archive_source_unavailable"})
		return
	}
	if uerr := s.persistLastPlayedAt(r.Context(), item); uerr != nil {
		s.log.Warn("stream: update last_played_at", "item_id", item.ID, "error", uerr)
	}
	s.ResetJanitorTimer(item.ID, time.Duration(s.cfg.Lifecycle.CleanupHours)*time.Hour)
	s.log.Info("stream: cdn RAR play started", "item_id", item.ID, "parts", len(manifest.Parts))

	w.Header().Set("Content-Type", contentType)
	minBuf := s.cfg.Cache.StreamMinBufferSegments
	if minBuf <= 0 {
		minBuf = 2
	}
	cw := &countingWriter{ResponseWriter: w}
	if err := s.streamCache.Stream(r.Context(), chunks, s.streamMode(), manifest.TotalSize, cw, r.Header.Get("Range"), minBuf); err != nil {
		s.log.Warn("stream: cdn RAR cache stream error", "item_id", item.ID, "error", httpstream.Sanitize(err))
	}
	if cw.n > 0 && s.egressMeter != nil {
		s.egressMeter.AddCDNBytes(cw.n)
	}
}

func isTorrentRARFileID(fileID string) bool {
	return strings.HasPrefix(fileID, "rar:")
}
