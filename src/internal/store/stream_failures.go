package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Stream-time dead-letter for NZB/NNTP items (Surface 3).
//
// The D54 failed_hashes breaker is keyed on a torrent infohash, which NZB items
// do not have, so NNTP-streamed items had no failure accounting at /stream time:
// a release whose Usenet articles are gone would be re-fetched from the provider
// on every play/probe/scan forever. These methods mirror the failed_hashes
// breaker but are keyed on item_id. The /stream handler increments on a genuine
// content failure (NOT a client abort) and resets on a clean serve; at threshold
// the item enters a dead window the handler short-circuits, so a dead release
// stops hammering the NNTP provider.

const (
	streamFailThreshold = 3                  // failures before an item is marked dead
	streamDeadDuration  = 7 * 24 * time.Hour // dead-window length (mirrors D54)
)

// IncrementStreamFailure records one stream-time content failure for itemID.
// Uses UPSERT so a new item is inserted and an existing one incremented
// atomically; at streamFailThreshold it sets dead_until = now + streamDeadDuration.
// Returns dead = whether the item is now within an active dead window.
func (s *Store) IncrementStreamFailure(ctx context.Context, itemID string) (bool, error) {
	now := formatTime(s.now())
	deadAt := formatTime(s.now().Add(streamDeadDuration))
	if _, err := s.execWrite(ctx, `
        INSERT INTO item_stream_failures (item_id, fail_count, last_failed_at, dead_until)
        VALUES (?, 1, ?, NULL)
        ON CONFLICT(item_id) DO UPDATE SET
            fail_count     = item_stream_failures.fail_count + 1,
            last_failed_at = excluded.last_failed_at,
            dead_until     = CASE
                WHEN item_stream_failures.fail_count + 1 >= ?
                THEN ?
                ELSE item_stream_failures.dead_until
            END`,
		itemID, now, streamFailThreshold, deadAt,
	); err != nil {
		return false, fmt.Errorf("increment stream failure for %s: %w", itemID, err)
	}
	return s.IsStreamDead(ctx, itemID)
}

// IsStreamDead returns true if itemID has an active dead window (dead_until > now).
// Returns false for unknown items and for items whose window has expired.
func (s *Store) IsStreamDead(ctx context.Context, itemID string) (bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT dead_until FROM item_stream_failures WHERE item_id=? LIMIT 1`, itemID)
	var du sql.NullString
	if err := row.Scan(&du); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("is stream dead %s: %w", itemID, err)
	}
	if !du.Valid {
		return false, nil
	}
	until, err := parseTime(du.String)
	if err != nil {
		return false, err
	}
	return until.After(s.now()), nil
}

// ResetStreamFailure clears any recorded stream failures for itemID. Called on a
// clean serve (so transient blips do not accumulate) and by `deadletter requeue`.
func (s *Store) ResetStreamFailure(ctx context.Context, itemID string) error {
	if _, err := s.execWrite(ctx,
		`DELETE FROM item_stream_failures WHERE item_id=?`, itemID); err != nil {
		return fmt.Errorf("reset stream failure for %s: %w", itemID, err)
	}
	return nil
}

// ExpireStreamDead clears dead_until for all items whose window has passed, so a
// dead release is retried once after the window (the content may have returned).
// Called at startup and periodically alongside ExpireBlacklists.
func (s *Store) ExpireStreamDead(ctx context.Context) (int64, error) {
	result, err := s.execWrite(ctx,
		`UPDATE item_stream_failures SET dead_until = NULL WHERE dead_until IS NOT NULL AND dead_until <= ?`,
		formatTime(s.now()),
	)
	if err != nil {
		return 0, fmt.Errorf("expire stream dead: %w", err)
	}
	affected, _ := result.RowsAffected()
	return affected, nil
}

// DeadStream is one dead-letter row for operator inspection (`deadletter list`).
type DeadStream struct {
	ItemID       string
	FailCount    int
	LastFailedAt time.Time
	DeadUntil    time.Time
}

// ListDeadStreams returns items currently within an active dead window, most
// recently failed first.
func (s *Store) ListDeadStreams(ctx context.Context) ([]DeadStream, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT item_id, fail_count, last_failed_at, dead_until
        FROM item_stream_failures
        WHERE dead_until IS NOT NULL AND dead_until > ?
        ORDER BY last_failed_at DESC`,
		formatTime(s.now()),
	)
	if err != nil {
		return nil, fmt.Errorf("list dead streams: %w", err)
	}
	defer rows.Close()

	var out []DeadStream
	for rows.Next() {
		var (
			d  DeadStream
			lf string
			du sql.NullString
		)
		if err := rows.Scan(&d.ItemID, &d.FailCount, &lf, &du); err != nil {
			return nil, fmt.Errorf("scan dead stream: %w", err)
		}
		if t, perr := parseTime(lf); perr == nil {
			d.LastFailedAt = t
		}
		if du.Valid {
			if t, perr := parseTime(du.String); perr == nil {
				d.DeadUntil = t
			}
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ClearStreamFailures removes every stream-failure row (used by `deadletter
// clear`). Returns the number of rows removed.
func (s *Store) ClearStreamFailures(ctx context.Context) (int64, error) {
	result, err := s.execWrite(ctx, `DELETE FROM item_stream_failures`)
	if err != nil {
		return 0, fmt.Errorf("clear stream failures: %w", err)
	}
	n, _ := result.RowsAffected()
	return n, nil
}
