package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
)

func (s *Store) MergePlaybackMediaCoverage(ctx context.Context, representationID string, incoming playbackcoverage.Interval, updatedAt time.Time, maxIntervals int) (playbackcoverage.MediaSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return playbackcoverage.MediaSnapshot{}, fmt.Errorf("store: begin playback media coverage: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	merged := incoming
	rows, err := tx.QueryContext(ctx, `
        SELECT media_start_ns, media_end_ns FROM playback_media_coverage
        WHERE representation_id=? AND media_start_ns<=? AND media_end_ns>=?
        ORDER BY media_start_ns`, representationID, incoming.End, incoming.Start)
	if err != nil {
		return playbackcoverage.MediaSnapshot{}, fmt.Errorf("store: list overlapping playback media coverage: %w", err)
	}
	for rows.Next() {
		var interval playbackcoverage.Interval
		if err := rows.Scan(&interval.Start, &interval.End); err != nil {
			_ = rows.Close()
			return playbackcoverage.MediaSnapshot{}, fmt.Errorf("store: scan overlapping playback media coverage: %w", err)
		}
		if interval.Start < merged.Start {
			merged.Start = interval.Start
		}
		if interval.End > merged.End {
			merged.End = interval.End
		}
	}
	if err := rows.Close(); err != nil {
		return playbackcoverage.MediaSnapshot{}, fmt.Errorf("store: close overlapping playback media coverage: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
        DELETE FROM playback_media_coverage
        WHERE representation_id=? AND media_start_ns<=? AND media_end_ns>=?`,
		representationID, incoming.End, incoming.Start); err != nil {
		return playbackcoverage.MediaSnapshot{}, fmt.Errorf("store: merge playback media coverage: %w", err)
	}
	var remaining int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM playback_media_coverage WHERE representation_id=?`, representationID).Scan(&remaining); err != nil {
		return playbackcoverage.MediaSnapshot{}, fmt.Errorf("store: count playback media coverage: %w", err)
	}
	if remaining >= maxIntervals {
		return playbackcoverage.MediaSnapshot{}, errors.New("store: playback media coverage interval limit exceeded")
	}
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO playback_media_coverage (representation_id, media_start_ns, media_end_ns, updated_at)
        VALUES (?, ?, ?, ?)`, representationID, merged.Start, merged.End, formatTime(updatedAt)); err != nil {
		return playbackcoverage.MediaSnapshot{}, fmt.Errorf("store: insert playback media coverage: %w", err)
	}
	snapshot, found, err := getPlaybackMediaCoverage(ctx, tx, representationID, maxIntervals)
	if err != nil || !found {
		return playbackcoverage.MediaSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return playbackcoverage.MediaSnapshot{}, fmt.Errorf("store: commit playback media coverage: %w", err)
	}
	return snapshot, nil
}

func (s *Store) GetPlaybackMediaCoverage(ctx context.Context, representationID string, maxIntervals int) (playbackcoverage.MediaSnapshot, bool, error) {
	return getPlaybackMediaCoverage(ctx, s.db, representationID, maxIntervals)
}

func getPlaybackMediaCoverage(ctx context.Context, db coverageQuerier, representationID string, maxIntervals int) (playbackcoverage.MediaSnapshot, bool, error) {
	rows, err := db.QueryContext(ctx, `
        SELECT media_start_ns, media_end_ns, updated_at FROM playback_media_coverage
        WHERE representation_id=? ORDER BY media_start_ns LIMIT ?`, representationID, maxIntervals+1)
	if err != nil {
		return playbackcoverage.MediaSnapshot{}, false, fmt.Errorf("store: get playback media coverage: %w", err)
	}
	defer rows.Close()
	snapshot := playbackcoverage.MediaSnapshot{RepresentationID: representationID}
	for rows.Next() {
		var interval playbackcoverage.Interval
		var updated string
		if err := rows.Scan(&interval.Start, &interval.End, &updated); err != nil {
			return playbackcoverage.MediaSnapshot{}, false, fmt.Errorf("store: scan playback media coverage: %w", err)
		}
		if len(snapshot.Intervals) == maxIntervals {
			return playbackcoverage.MediaSnapshot{}, false, errors.New("store: playback media coverage interval limit exceeded")
		}
		at, err := parseTime(updated)
		if err != nil {
			return playbackcoverage.MediaSnapshot{}, false, fmt.Errorf("store: parse playback media coverage time: %w", err)
		}
		snapshot.Intervals = append(snapshot.Intervals, interval)
		if interval.End-interval.Start > int64(^uint64(0)>>1)-snapshot.DeliveredNS {
			return playbackcoverage.MediaSnapshot{}, false, errors.New("store: playback media coverage total overflow")
		}
		snapshot.DeliveredNS += interval.End - interval.Start
		if at.After(snapshot.UpdatedAt) {
			snapshot.UpdatedAt = at
		}
	}
	if err := rows.Err(); err != nil {
		return playbackcoverage.MediaSnapshot{}, false, fmt.Errorf("store: read playback media coverage: %w", err)
	}
	return snapshot, len(snapshot.Intervals) > 0, nil
}
