package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/mediatruth"
)

const maxPersistedSeekEntries = 2048

// SetNNTPSeekIndex atomically stores the compact, typed index on an item
// without rewriting timestamps or unrelated metadata accumulated by playback.
func (s *Store) SetNNTPSeekIndex(ctx context.Context, itemID, sourceKey string, index mediatruth.Index) error {
	itemID = strings.TrimSpace(itemID)
	sourceKey = strings.TrimSpace(sourceKey)
	if itemID == "" || sourceKey == "" {
		return fmt.Errorf("persist NNTP seek index: empty item or source key")
	}
	if len(sourceKey) > 256 {
		return fmt.Errorf("persist NNTP seek index: invalid source key")
	}
	for _, r := range sourceKey {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') &&
			r != '-' && r != '_' && r != '.' && r != '|' && r != ':' {
			return fmt.Errorf("persist NNTP seek index: invalid source key")
		}
	}
	if len(index.Entries) == 0 || len(index.Entries) > maxPersistedSeekEntries {
		return fmt.Errorf("persist NNTP seek index: invalid entry count")
	}
	if index.Source != mediatruth.IndexSourceMatroskaCues && index.Source != mediatruth.IndexSourceMP4Moov {
		return fmt.Errorf("persist NNTP seek index: invalid source")
	}
	for i, entry := range index.Entries {
		if entry.TimeMS < 0 || entry.Offset < 0 ||
			(i > 0 && (entry.TimeMS <= index.Entries[i-1].TimeMS || entry.Offset <= index.Entries[i-1].Offset)) {
			return fmt.Errorf("persist NNTP seek index: non-monotonic entry")
		}
	}
	raw, err := json.Marshal(index)
	if err != nil {
		return fmt.Errorf("persist NNTP seek index: %w", err)
	}
	_, err = retrySQLiteBusy(ctx, func() (struct{}, error) {
		var ignored int
		scanErr := s.db.QueryRowContext(ctx, `
			UPDATE items
			SET metadata_json = json_set(
				COALESCE(NULLIF(metadata_json, ''), '{}'),
				'$.nntp_seek_index', json(?),
				'$.nntp_seek_source_key', ?
			)
			WHERE id=?
			RETURNING 1`,
			string(raw), sourceKey, itemID,
		).Scan(&ignored)
		if scanErr == sql.ErrNoRows {
			return struct{}{}, fmt.Errorf("item %s not found", itemID)
		}
		return struct{}{}, scanErr
	})
	if err != nil {
		return fmt.Errorf("persist NNTP seek index for %s: %w", itemID, err)
	}
	return nil
}
