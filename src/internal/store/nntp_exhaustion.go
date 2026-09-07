package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const nntpZeroFillMessagePrefix = "NNTP final-rung exhaustion events: "

// IncrementNNTPZeroFill atomically records one item-associated NNTP final-rung
// exhaustion and exposes the cumulative count through the existing persisted
// item error/state-message surface. It deliberately does not change item state
// or updated_at: a Ready item's completion timestamp and SAB history ordering
// must remain stable while playback diagnostics accumulate (NS-1.3).
func (s *Store) IncrementNNTPZeroFill(ctx context.Context, itemID string) (int64, error) {
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return 0, fmt.Errorf("increment NNTP final-rung exhaustion: empty item id")
	}

	var count int64
	_, err := retrySQLiteBusy(ctx, func() (struct{}, error) {
		row := s.db.QueryRowContext(ctx, `
			UPDATE items
			SET metadata_json = json_set(
					COALESCE(NULLIF(metadata_json, ''), '{}'),
					'$.nntp_zero_fill_count',
					COALESCE(CAST(json_extract(metadata_json, '$.nntp_zero_fill_count') AS INTEGER), 0) + 1
				),
				error_message = ? || CAST(
					COALESCE(CAST(json_extract(metadata_json, '$.nntp_zero_fill_count') AS INTEGER), 0) + 1
					AS TEXT
				)
			WHERE id = ?
			RETURNING CAST(json_extract(metadata_json, '$.nntp_zero_fill_count') AS INTEGER)`,
			nntpZeroFillMessagePrefix,
			itemID,
		)
		if scanErr := row.Scan(&count); scanErr != nil {
			if scanErr == sql.ErrNoRows {
				return struct{}{}, fmt.Errorf("item %s not found", itemID)
			}
			return struct{}{}, scanErr
		}
		return struct{}{}, nil
	})
	if err != nil {
		return 0, fmt.Errorf("increment NNTP final-rung exhaustion for %s: %w", itemID, err)
	}
	return count, nil
}
