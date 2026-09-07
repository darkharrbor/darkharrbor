package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

const (
	rx93MaxConcurrent   = 16
	rx93CaptureTimeout  = 90 * time.Second
	rx93ReadyPollPeriod = 100 * time.Millisecond
)

func (s *Server) rx93CaptureEnabled() bool {
	if s == nil || s.cfg == nil || s.store == nil || s.writer == nil || s.httpHandlers == nil ||
		s.playbackCoverage == nil || s.shutdownCtx == nil || s.rx93CaptureSem == nil ||
		!s.cfg.Reactive.Enabled || !s.cfg.HTTPStreamEnabled() {
		return false
	}
	threshold := s.cfg.Reactive.Threshold
	return threshold > 0 && threshold <= 1 && !math.IsNaN(threshold) && !math.IsInf(threshold, 0)
}

func (s *Server) rx93ThresholdReached(delivered, total int64) bool {
	return s.rx93CaptureEnabled() && delivered > 0 && total > 0 &&
		float64(delivered)/float64(total) >= s.cfg.Reactive.Threshold
}

func (s *Server) scheduleRX93Capture(representationID string, identity rx95Identity, filename string, total, offset, length int64, selected *httpstream.SearchResult) {
	filename, filenameOK := rx93Filename(filename)
	if !s.rx93CaptureEnabled() || playbackcoverage.ValidateRepresentationID(representationID) != nil ||
		!identity.valid() || !filenameOK || total <= 0 || offset < 0 || length <= 0 || offset > math.MaxInt64-length {
		return
	}
	select {
	case <-s.shutdownCtx.Done():
		return
	case s.rx93CaptureSem <- struct{}{}:
	default:
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() { <-s.rx93CaptureSem }()
		_, _, _ = s.rx93CaptureGroup.Do(representationID, func() (any, error) {
			ctx, cancel := context.WithTimeout(s.shutdownCtx, rx93CaptureTimeout)
			defer cancel()
			itemID, err := s.captureRX93(ctx, representationID, identity, filename, total, offset, length, selected)
			if err != nil {
				if ctx.Err() == nil && s.log != nil {
					s.log.Warn("rx93: aggregator capture failed (non-fatal)",
						"representation_id", representationID, "error", httpstream.Sanitize(err))
				}
			} else if itemID != "" && s.log != nil {
				s.log.Info("rx93: aggregator representation captured",
					"representation_id", representationID, "item_id", itemID)
			}
			return nil, nil
		})
	}()
}

func (s *Server) captureRX93(ctx context.Context, representationID string, identity rx95Identity, filename string, total, offset, length int64, selected *httpstream.SearchResult) (string, error) {
	if _, found, err := s.store.GetPlaybackProposal(ctx, representationID); err != nil || found {
		return "", err
	}
	var result httpstream.SearchResult
	if selected != nil {
		result = *selected
		candidateFilename, ok := rx93Filename(result.Filename)
		if !ok || candidateFilename != filename || result.Size != total ||
			!rx93KeyMatchesIdentity(result.Key, result.Key.BackendID, identity) {
			return "", errors.New("rx93: retained selector no longer matches capture target")
		}
	} else {
		var found bool
		var err error
		result, found, err = s.searchRX93(ctx, identity, filename, total)
		if err != nil {
			return "", err
		}
		if !found {
			return "", errors.New("rx93: unique filename-size selector unavailable at threshold")
		}
	}
	canonical, err := result.Key.Canonical()
	if err != nil {
		return "", err
	}
	digest, err := result.Key.Digest()
	if err != nil {
		return "", err
	}
	item, _, err := s.enqueueSubmission(ctx, SubmissionRequest{
		SourceType:  store.SourceTypeHTTP,
		ClientKind:  store.ClientKindQBit,
		Category:    rx93Category(identity),
		DisplayName: firstNonEmpty(result.Title, filename, digest),
		InfoHash:    digest,
		ResolveKey:  canonical,
		Metadata: store.SubmissionMetadata{
			ProviderIdentity: rx93ProviderIdentity(identity),
		},
	})
	if err != nil {
		return "", err
	}
	ready, err := s.waitRX93Ready(ctx, item.ID)
	if err != nil {
		return "", err
	}
	target, ok, err := s.rx93ReadyTarget(ctx, ready, identity, filename, canonical, total)
	if err != nil || !ok {
		return "", err
	}
	observeCtx := playbackcoverage.WithRepresentation(ctx, representationID)
	observeCtx = playbackcoverage.WithDeclaredSize(observeCtx, total)
	observeCtx = playbackcoverage.WithTarget(observeCtx, target)
	if _, err := s.playbackCoverage.Observe(observeCtx, representationID, offset, length); err != nil {
		return "", err
	}
	proposal, found, err := s.store.GetPlaybackProposal(ctx, representationID)
	if err != nil {
		return "", err
	}
	if !found || proposal.ItemID != target.ItemID || proposal.FileID != target.FileID || proposal.Total != target.Total {
		return "", errors.New("rx93: proposal was not created for captured target")
	}
	if selectorKey, ok := rx93SelectorCacheKey(representationID, filename, total); ok {
		s.rx93Selectors.Delete(selectorKey)
	}
	return ready.ID, nil
}

func (s *Server) searchRX93(ctx context.Context, identity rx95Identity, filename string, total int64) (httpstream.SearchResult, bool, error) {
	query := httpstream.StreamQuery{Kind: "movie", IMDBID: identity.IMDb}
	if identity.Kind == "series" {
		query.Kind, query.Season, query.Episode = "episode", identity.Season, identity.Episode
	}
	var match httpstream.SearchResult
	matches := 0
	filenameMatches := 0
	sizeMatches := 0
	selectorMatches := 0
	for _, backend := range s.httpHandlers.Statuses() {
		if !strings.EqualFold(backend.Handler, "stremio") {
			continue
		}
		handler, ok := s.httpHandlers.Lookup(backend.BackendID, "stremio")
		if !ok {
			return httpstream.SearchResult{}, false, errors.New("rx93: configured Stremio backend unavailable")
		}
		results, err := handler.Search(ctx, query)
		if err != nil {
			return httpstream.SearchResult{}, false, err
		}
		for _, candidate := range results {
			candidateFilename, ok := rx93Filename(candidate.Filename)
			if !ok || candidateFilename != filename {
				continue
			}
			filenameMatches++
			if candidate.Size != total {
				continue
			}
			sizeMatches++
			if !rx93KeyMatchesIdentity(candidate.Key, backend.BackendID, identity) {
				continue
			}
			selectorMatches++
			match = candidate
			matches++
		}
	}
	if matches != 1 && s.log != nil {
		s.log.Info("rx93: selector match diagnostic",
			"kind", identity.Kind,
			"imdb", identity.IMDb,
			"season", identity.Season,
			"episode", identity.Episode,
			"filename", filename,
			"declared_size", total,
			"filename_matches", filenameMatches,
			"size_matches", sizeMatches,
			"selector_matches", selectorMatches,
			"exact_matches", matches)
	}
	return match, matches == 1, nil
}

func rx93KeyMatchesIdentity(key httpstream.ResolveKey, backendID string, identity rx95Identity) bool {
	if key.Validate() != nil || !strings.EqualFold(key.BackendID, backendID) ||
		!strings.EqualFold(key.Handler, "stremio") || key.IDs["imdb"] != identity.IMDb || key.Selector == "" {
		return false
	}
	if identity.Kind == "movie" {
		return key.Kind == "movie" && key.Season == 0 && key.Episode == 0
	}
	return key.Kind == "episode" && key.Season == identity.Season && key.Episode == identity.Episode
}

func rx93Filename(raw string) (string, bool) {
	if raw == "" || len(raw) > mediaFlowMaxFilenameBytes {
		return "", false
	}
	name := path.Base(strings.ReplaceAll(raw, "\\", "/"))
	if name == "." || name == "" || strings.ContainsAny(name, "\r\n\x00") {
		return "", false
	}
	return name, true
}

// rx93Category names the folder a captured item is materialized under. It is a
// HOLDING category per RD-11: capture happens at playback, routing happens later
// at dispatch, so capture CANNOT know the destination instance. The previous
// values were "tv" and "movies", which silently assumed the operator's own
// category names -- "tv" matched NO configured instance here (one such item
// against 1,188 across the three real tv categories) while "movies" matched only
// by coincidence. A dedicated holding prefix is honest about that and keeps
// captured content separable from grabbed content, which also makes RD-34 D3's
// "lands here unless you move it" workflow tractable. The Arr import path is
// passed explicitly through arrVisiblePath, so this never gates a commit.
func rx93Category(identity rx95Identity) string {
	if identity.Kind == "series" {
		return "reactive-tv"
	}
	return "reactive-movies"
}

func rx93ProviderIdentity(identity rx95Identity) *store.ProviderIdentity {
	providerIdentity := &store.ProviderIdentity{
		Kind: "movie",
		IDs:  store.ProviderIDs{IMDB: identity.IMDb},
	}
	if identity.Kind == "series" {
		providerIdentity.Kind = "series"
		providerIdentity.Episodes = []store.EpisodeProviderIDs{{
			Season: identity.Season, Episode: identity.Episode,
		}}
	}
	return providerIdentity
}

func (s *Server) waitRX93Ready(ctx context.Context, itemID string) (*store.Item, error) {
	ticker := time.NewTicker(rx93ReadyPollPeriod)
	defer ticker.Stop()
	for {
		item, err := s.store.GetItemByID(ctx, itemID)
		if err != nil {
			return nil, err
		}
		if item == nil {
			return nil, errors.New("rx93: captured item disappeared")
		}
		switch item.State {
		case store.StateReady:
			return item, nil
		case store.StateFailed, store.StateRemoved:
			return nil, fmt.Errorf("rx93: captured item ended in state %s", item.State)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Server) rx93ReadyTarget(ctx context.Context, item *store.Item, identity rx95Identity, filename, canonical string, total int64) (playbackcoverage.Target, bool, error) {
	if item == nil || item.State != store.StateReady || item.SourceType != store.SourceTypeHTTP ||
		item.ResolveKey == nil || *item.ResolveKey != canonical ||
		!mediaFlowIdentityMatches(item.Metadata.ProviderIdentity, mediaFlowStremioIdentity{
			IMDb: identity.IMDb, Series: identity.Kind == "series", Season: identity.Season, Episode: identity.Episode,
		}) {
		return playbackcoverage.Target{}, false, nil
	}
	files := parseFileList(item.FileList)
	if len(files) != 1 || files[0].FileID == "" || files[0].Size != total {
		return playbackcoverage.Target{}, false, nil
	}
	resolvedFilename, ok := rx93Filename(files[0].Name)
	if !ok || resolvedFilename != filename {
		return playbackcoverage.Target{}, false, nil
	}
	blobs, err := s.store.GetStrmBlobs(ctx, item.ID)
	if err != nil || len(blobs) != 1 {
		return playbackcoverage.Target{}, false, err
	}
	blobItemID, blobFileID, ok := parseStreamPath(blobs[0].URL)
	if !ok || blobItemID != item.ID || blobFileID != files[0].FileID ||
		!validStremioLibraryURL(blobs[0].URL, s.cfg.Server.BaseURL, s.cfg.Server.StreamSecret, item.ID, files[0].FileID) {
		return playbackcoverage.Target{}, false, nil
	}
	target, ok := playbackProposalTarget(item, files[0].FileID, total)
	return target, ok && target.Kind == playbackcoverage.ExtentDeclaredBytes && target.Total == total, nil
}
