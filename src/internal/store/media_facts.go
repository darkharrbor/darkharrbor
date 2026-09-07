package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/mediatruth"
)

const maxMediaFactsFileIDLen = 256

// SetMediaFacts atomically stores bounded, secret-free media truth (HR5.2)
// for one item's streamed file (SF-04), without rewriting timestamps or
// unrelated metadata accumulated by playback -- the same json_set-on-
// metadata_json pattern SetNNTPSeekIndex already established, so a
// concurrent playback-driven update elsewhere in metadata_json cannot be
// clobbered by a racing prewarm-time write (or vice versa).
//
// fileID/sourceKey identify the exact streamed representation the facts
// describe; a later probe request for a DIFFERENT file on the same item
// must never be answered from these facts (the relay's own read side
// enforces the fileID match -- this method only bounds/validates what it
// persists).
func (s *Store) SetMediaFacts(ctx context.Context, itemID, fileID, sourceKey string, facts mediatruth.Facts) error {
	itemID = strings.TrimSpace(itemID)
	fileID = strings.TrimSpace(fileID)
	sourceKey = strings.TrimSpace(sourceKey)
	if itemID == "" || fileID == "" || sourceKey == "" {
		return fmt.Errorf("persist media facts: empty item, file, or source key")
	}
	if len(fileID) > maxMediaFactsFileIDLen || len(sourceKey) > 256 {
		return fmt.Errorf("persist media facts: invalid file or source key")
	}
	// Torrent-lane source keys are "torrent|" + item.ID + "/" + fileID
	// (api.ResolveKey/resolveKey, the exact identity streamViaCache's own
	// ChunkSource.Key() uses); HTTP-lane keys are pipe-only
	// ("httpitem|"+item.ID+"|"+fid). Both are opaque DH-assigned identifiers,
	// never upstream-derived (DG-04). A bare "/" is a benign path-shaped
	// separator (found live: the first real uncached-grab live gate for the
	// torrent-lane call site failed validation with "invalid source key"
	// until this fix), so it is allowed alongside the NNTP-lane's existing
	// pipe/colon/dot/dash/underscore set -- but the actual URL marker "://"
	// is rejected explicitly first, matching the established convention
	// (internal/hlssession, internal/httpstream/bytesource, internal/sidecar
	// all reject on "://" specifically rather than banning "/" outright, so
	// a bare path-shaped identifier is never confused with a real URL).
	if strings.Contains(sourceKey, "://") {
		return fmt.Errorf("persist media facts: invalid source key")
	}
	for _, r := range sourceKey {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') &&
			r != '-' && r != '_' && r != '.' && r != '|' && r != ':' && r != '/' {
			return fmt.Errorf("persist media facts: invalid source key")
		}
	}
	if facts.Empty() {
		return fmt.Errorf("persist media facts: empty facts")
	}
	raw, err := json.Marshal(facts)
	if err != nil {
		return fmt.Errorf("persist media facts: %w", err)
	}
	_, err = retrySQLiteBusy(ctx, func() (struct{}, error) {
		var ignored int
		scanErr := s.db.QueryRowContext(ctx, `
			UPDATE items
			SET metadata_json = json_set(
				COALESCE(NULLIF(metadata_json, ''), '{}'),
				'$.media_facts', json(?),
				'$.media_facts_file_id', ?,
				'$.media_facts_source_key', ?
			)
			WHERE id=?
			RETURNING 1`,
			string(raw), fileID, sourceKey, itemID,
		).Scan(&ignored)
		if scanErr == sql.ErrNoRows {
			return struct{}{}, fmt.Errorf("item %s not found", itemID)
		}
		return struct{}{}, scanErr
	})
	if err != nil {
		return fmt.Errorf("persist media facts for %s: %w", itemID, err)
	}
	return nil
}
