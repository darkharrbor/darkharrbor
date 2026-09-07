package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
	"github.com/darkharrbor/darkharrbor/internal/contentproof"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

const maxPromotionFormBytes = 4096

func (s *Server) handleReactivePromote(w http.ResponseWriter, r *http.Request) {
	if s.promotion == nil {
		http.Error(w, "promotion unavailable", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPromotionFormBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	var err error
	if r.Form.Get("action") == "remove" {
		err = s.promotion.Remove(r.Context(), r.Form.Get("representation"))
	} else if r.Form.Get("action") == "" || r.Form.Get("action") == "promote" {
		err = s.promotion.Promote(r.Context(), r.Form.Get("representation"), r.Form.Get("target"))
	} else {
		http.Error(w, "invalid action", http.StatusBadRequest)
		return
	}
	if err != nil {
		s.log.Warn("reactive promotion failed", "error", errors.New("promotion request failed"))
		http.Error(w, "promotion failed", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// OpenPromotionSource resolves only the committed file's existing lane. It
// never searches for a replacement representation.
func (s *Server) OpenPromotionSource(ctx context.Context, item *store.Item, fileID string) (bytesource.ByteSource, error) {
	if item == nil || item.State != store.StateReady || fileID == "" {
		return nil, errors.New("api: promotion item unavailable")
	}
	switch item.SourceType {
	case store.SourceTypeTorrent:
		file, ok := fileListEntryForID(parseFileList(item.FileList), fileID)
		if !ok {
			return nil, errors.New("api: promotion torrent file unavailable")
		}
		candidate, err := contentproof.NewLaneCandidate(contentproof.LaneTorrent, item.ID, fileID, torrentCrossLaneRepresentationID(item.ID, fileID), item.DisplayName, file.Size)
		if err != nil {
			return nil, err
		}
		return s.openNNTPCrossLaneSource(ctx, candidate)
	case store.SourceTypeNZB:
		index, err := strconv.Atoi(fileID)
		file, ok := fileListEntryForID(parseFileList(item.FileList), fileID)
		if err != nil || index < 0 || !ok {
			return nil, errors.New("api: promotion NNTP file unavailable")
		}
		candidate, err := contentproof.NewLaneCandidate(contentproof.LaneNNTP, item.ID, fileID, nntpCrossLaneRepresentationID(item.ID, index), item.DisplayName, file.Size)
		if err != nil {
			return nil, err
		}
		return s.openTorrentCrossLaneSource(ctx, candidate)
	case store.SourceTypeHTTP:
		files, err := httpstream.ParseFiles(strFromPtr(item.FileList))
		file, ok := httpstream.LookupFile(files, fileID)
		if err != nil || !ok {
			return nil, errors.New("api: promotion HTTP file unavailable")
		}
		candidate, err := contentproof.NewLaneCandidate(contentproof.LaneHTTP, item.ID, fileID, httpRepresentationID(item.ID, fileID), item.DisplayName, file.Size)
		if err != nil {
			return nil, err
		}
		return s.openTorrentCrossLaneSource(ctx, candidate)
	default:
		return nil, errors.New("api: promotion lane unsupported")
	}
}
