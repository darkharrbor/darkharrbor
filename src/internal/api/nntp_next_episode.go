package api

// NS-5.4 (N12): idle-only one-episode look-ahead prewarm for the NNTP lane,
// mirroring HR5.4's next-episode prewarm for progressive HTTP. Scoped to
// RAR-split NZB items only in this v1: unlike HTTP's ResolveKey, an NZB item
// carries no persisted season/episode/series identity, and a direct-mkv or
// ZIP NZB item has no per-file size persisted, so a byte-Range-derived
// playback fraction cannot be mapped to "this one episode" reliably for
// them. A RAR-split item is the one NZB shape where item.TotalSize is
// exactly the single virtual video's size (see webdav.go's
// nzbEffectiveSize), so a Range position on it is an exact playback
// fraction. Direct/ZIP NZB next-episode prewarm is left for a future row,
// same shape as HR2.4 being parked pending HR2.1 (see HANDOFF 2026-07-25).
//
// Series/episode identity is derived from item.DisplayName (the torznab
// release title recorded at grab time) via an SxxEyy token, exactly like
// the existing episodeNameFromSubject/looksLikeEpisodeName heuristics in
// cmd/darkharrbor/main.go use for .strm naming. Matching is exact-only:
// same normalized series-name prefix, same season, episode = current+1.
// Ambiguity (more than one equally-qualifying Ready candidate) is never a
// guess -- it is treated exactly like "no candidate", the same confidence
// rule HR5.4 established for the HTTP lane.

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/nntp"
	"github.com/darkharrbor/darkharrbor/internal/provider"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// maxNextEpisodeNNTPCandidates bounds the candidate search (DG-07), mirroring
// maxNextEpisodeCandidates in httpstream.go.
const maxNextEpisodeNNTPCandidates = 2048

// nntpSeasonEpisodePattern extracts a leading series-name prefix plus an
// SxxEyy season/episode token from a torznab release title, e.g.
// "Show.Name.S03E05.1080p.WEB.x264-GROUP" -> ("Show.Name", 3, 5).
var nntpSeasonEpisodePattern = regexp.MustCompile(`(?i)^(.*?)[\s._-]+s0*([0-9]{1,3})e0*([0-9]{1,3})(?:[^0-9].*)?$`)

// nntpEpisodeIdentity is the exact-match-only identity NS-5.4 derives from a
// DisplayName. There is deliberately no fuzzy/title-similarity fallback.
type nntpEpisodeIdentity struct {
	series  string
	season  int
	episode int
}

// parseNNTPEpisodeIdentity extracts a series/season/episode identity from an
// NZB item's DisplayName. Returns ok=false for any title that doesn't carry
// a recognizable SxxEyy token -- never a partial or inferred guess.
func parseNNTPEpisodeIdentity(displayName string) (nntpEpisodeIdentity, bool) {
	m := nntpSeasonEpisodePattern.FindStringSubmatch(strings.TrimSpace(displayName))
	if m == nil {
		return nntpEpisodeIdentity{}, false
	}
	season, serr := strconv.Atoi(m[2])
	episode, eerr := strconv.Atoi(m[3])
	if serr != nil || eerr != nil {
		return nntpEpisodeIdentity{}, false
	}
	series := normalizeNNTPSeriesName(m[1])
	if series == "" {
		return nntpEpisodeIdentity{}, false
	}
	return nntpEpisodeIdentity{series: series, season: season, episode: episode}, true
}

// normalizeNNTPSeriesName lowercases and collapses any run of non-alphanumeric
// separators (dots, underscores, spaces, dashes) to a single space, so
// "Show.Name" and "Show Name" and "show_name" all compare equal.
func normalizeNNTPSeriesName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := true
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			space = false
		} else if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

// nntpPrewarmSourceProvider is the type-assertion seam used to reach a
// concrete *nntp.NNTPProvider's RAR prewarm chunk-source constructor from
// the interface-typed provider.UsenetProvider value returned by
// pickUsenetProvider, mirroring how prefetchNNTPSeek (NS-5.3) reaches
// Seek*Source via nntpSeekSourceProvider. No second classifier or provider
// abstraction is introduced.
type nntpPrewarmSourceProvider interface {
	PrewarmRARSource(ctx context.Context, itemID, contentKey string, manifest []nntp.RARPart) rangecache.ChunkSource
}

// nextEpisodeNNTPCandidate finds the unique Ready, RAR-split NNTP-lane item
// whose DisplayName identifies the same normalized series and season, one
// episode ahead of current. Any ambiguity -- more than one equally-qualifying
// candidate -- fails closed (nil, false), never guesses.
func nextEpisodeNNTPCandidate(current nntpEpisodeIdentity, items []*store.Item) (*store.Item, bool) {
	var match *store.Item
	for _, item := range items {
		if item == nil || !item.IsRARSplit() || item.RarManifest == nil || item.SourceURI == nil {
			continue
		}
		cand, ok := parseNNTPEpisodeIdentity(item.DisplayName)
		if !ok || cand.series != current.series || cand.season != current.season || cand.episode != current.episode+1 {
			continue
		}
		if match != nil {
			return nil, false
		}
		match = item
	}
	return match, match != nil
}

// queueNextEpisodeNNTPPrewarm mirrors queueNextEpisodePrewarm (HR5.4) for the
// RAR-split NNTP lane. Once playback of the current RAR-split item crosses
// the configured threshold, it looks for the unique next-episode Ready item
// (same series/season, episode+1) and runs the shared experience.Service's
// NextEpisodePrewarm against it. Best-effort, bounded, and non-blocking: it
// is called only after a successful stream, from its own goroutine, and
// reuses the existing process-local dedup set HR5.4 introduced
// (s.nextEpisodePrewarmed/nextEpisodeOrder) -- item IDs are unique across
// source types, so sharing that set introduces no collision risk and no
// second bookkeeping structure (DG-07).
func (s *Server) queueNextEpisodeNNTPPrewarm(item *store.Item, usenetProv provider.UsenetProvider, rangeHeader string) {
	if s.nntpExpSvc == nil || item == nil || !item.IsRARSplit() || item.TotalSize <= 0 {
		return
	}
	current, ok := parseNNTPEpisodeIdentity(item.DisplayName)
	if !ok {
		return
	}
	rng, satisfiable := httpstream.ParseRange(rangeHeader, item.TotalSize)
	if !satisfiable {
		return
	}
	fraction := 1.0
	if rng != nil {
		fraction = float64(rng.End+1) / float64(item.TotalSize)
	}
	if fraction < s.cfg.Prewarm.NextEpisodeThreshold {
		return
	}

	s.nextEpisodeMu.Lock()
	if _, exists := s.nextEpisodePrewarmed[item.ID]; exists {
		s.nextEpisodeMu.Unlock()
		return
	}
	s.nextEpisodePrewarmed[item.ID] = struct{}{}
	s.nextEpisodeOrder = append(s.nextEpisodeOrder, item.ID)
	trimInsertionCache(s.nextEpisodePrewarmed, &s.nextEpisodeOrder)
	s.nextEpisodeMu.Unlock()

	prewarmProv, ok := usenetProv.(nntpPrewarmSourceProvider)
	if !ok {
		return
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		items, err := s.store.ListReadyNNTPItems(s.shutdownCtx, maxNextEpisodeNNTPCandidates+1)
		if err != nil || len(items) > maxNextEpisodeNNTPCandidates {
			return
		}
		next, ok := nextEpisodeNNTPCandidate(current, items)
		if !ok {
			return
		}
		manifest, merr := nntp.UnmarshalRARManifest(*next.RarManifest)
		if merr != nil || len(manifest) == 0 || manifest[0].Encryption != nil {
			return
		}
		contentKey := nntp.ContentKey([]byte(*next.SourceURI))
		src := prewarmProv.PrewarmRARSource(s.shutdownCtx, next.ID, contentKey, manifest)
		if src == nil {
			return
		}
		timeoutSec := s.cfg.Prewarm.TimeoutSec
		if timeoutSec <= 0 {
			timeoutSec = 45
		}
		ctx, cancel := context.WithTimeout(s.shutdownCtx, time.Duration(timeoutSec)*time.Second)
		defer cancel()
		if _, perr := s.nntpExpSvc.NextEpisodePrewarm(ctx, src, fraction); perr != nil && ctx.Err() == nil {
			s.log.Warn("stream: nntp next-episode prewarm failed (non-fatal)", "item_id", next.ID, "error", perr)
		} else if ctx.Err() == nil {
			s.log.Info("stream: nntp next-episode prewarm complete", "item_id", next.ID, "current_item_id", item.ID)
		}
	}()
}
