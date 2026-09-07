package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
)

func (s *Store) MergePlaybackCoverage(ctx context.Context, representationID string, incoming playbackcoverage.Interval, updatedAt time.Time, maxIntervals int, declaredSize int64) (playbackcoverage.Snapshot, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return playbackcoverage.Snapshot{}, fmt.Errorf("store: begin playback coverage: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// RX-9.6 (RD-32): an identity-keyed representation is shared by every
	// release of one title, but COVERAGE IS MEASURED IN BYTES and byte offsets
	// are release-specific. Retained coverage from a larger release would make
	// delivered/total exceed 1 against a smaller one and commit immediately a
	// file the viewer has barely started. Discard the prior set when the
	// declared size changes, so exactly one coverage set exists per identity
	// and no consumer must choose between rival sets.
	//
	// A stored 0 means "unknown" -- pre-migration rows, or an observation made
	// without a declared extent -- and is ADOPTED by the first real size rather
	// than counted as a mismatch, so upgrading does not discard live coverage.
	if declaredSize > 0 {
		var stored sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT MAX(declared_size) FROM playback_coverage WHERE representation_id=?`,
			representationID).Scan(&stored); err != nil {
			return playbackcoverage.Snapshot{}, fmt.Errorf("store: read coverage declared size: %w", err)
		}
		switch {
		case !stored.Valid || stored.Int64 == 0:
			if _, err := tx.ExecContext(ctx,
				`UPDATE playback_coverage SET declared_size=? WHERE representation_id=?`,
				declaredSize, representationID); err != nil {
				return playbackcoverage.Snapshot{}, fmt.Errorf("store: adopt coverage declared size: %w", err)
			}
		case stored.Int64 != declaredSize:
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM playback_coverage WHERE representation_id=?`,
				representationID); err != nil {
				return playbackcoverage.Snapshot{}, fmt.Errorf("store: reset coverage on size change: %w", err)
			}
		}
	}

	merged := incoming
	rows, err := tx.QueryContext(ctx, `
        SELECT byte_start, byte_end FROM playback_coverage
        WHERE representation_id=? AND byte_start<=? AND byte_end>=?
        ORDER BY byte_start`, representationID, incoming.End, incoming.Start)
	if err != nil {
		return playbackcoverage.Snapshot{}, fmt.Errorf("store: list overlapping playback coverage: %w", err)
	}
	for rows.Next() {
		var interval playbackcoverage.Interval
		if err := rows.Scan(&interval.Start, &interval.End); err != nil {
			_ = rows.Close()
			return playbackcoverage.Snapshot{}, fmt.Errorf("store: scan overlapping playback coverage: %w", err)
		}
		if interval.Start < merged.Start {
			merged.Start = interval.Start
		}
		if interval.End > merged.End {
			merged.End = interval.End
		}
	}
	if err := rows.Close(); err != nil {
		return playbackcoverage.Snapshot{}, fmt.Errorf("store: close overlapping playback coverage: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
        DELETE FROM playback_coverage
        WHERE representation_id=? AND byte_start<=? AND byte_end>=?`,
		representationID, incoming.End, incoming.Start); err != nil {
		return playbackcoverage.Snapshot{}, fmt.Errorf("store: merge playback coverage: %w", err)
	}
	var remaining int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM playback_coverage WHERE representation_id=?`, representationID).Scan(&remaining); err != nil {
		return playbackcoverage.Snapshot{}, fmt.Errorf("store: count playback coverage: %w", err)
	}
	if remaining >= maxIntervals {
		return playbackcoverage.Snapshot{}, errors.New("store: playback coverage interval limit exceeded")
	}
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO playback_coverage (representation_id, byte_start, byte_end, updated_at, declared_size)
        VALUES (?, ?, ?, ?, ?)`, representationID, merged.Start, merged.End, formatTime(updatedAt), declaredSize); err != nil {
		return playbackcoverage.Snapshot{}, fmt.Errorf("store: insert playback coverage: %w", err)
	}
	snapshot, found, err := getPlaybackCoverage(ctx, tx, representationID, maxIntervals)
	if err != nil || !found {
		return playbackcoverage.Snapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return playbackcoverage.Snapshot{}, fmt.Errorf("store: commit playback coverage: %w", err)
	}
	return snapshot, nil
}

func (s *Store) GetPlaybackCoverage(ctx context.Context, representationID string, maxIntervals int) (playbackcoverage.Snapshot, bool, error) {
	return getPlaybackCoverage(ctx, s.db, representationID, maxIntervals)
}

// LatestPlaybackCoverage returns the most recent durable observation in one
// fixed representation class. It deliberately returns no representation ID.
func (s *Store) LatestPlaybackCoverage(ctx context.Context, prefix string) (time.Time, bool, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `
        SELECT updated_at FROM playback_coverage
        WHERE representation_id LIKE ?
        ORDER BY updated_at DESC LIMIT 1`, prefix+"%").Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: latest playback coverage: %w", err)
	}
	at, err := parseTime(raw)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: parse latest playback coverage time: %w", err)
	}
	return at, true, nil
}

type coverageQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func getPlaybackCoverage(ctx context.Context, db coverageQuerier, representationID string, maxIntervals int) (playbackcoverage.Snapshot, bool, error) {
	rows, err := db.QueryContext(ctx, `
        SELECT byte_start, byte_end, updated_at FROM playback_coverage
        WHERE representation_id=? ORDER BY byte_start LIMIT ?`, representationID, maxIntervals+1)
	if err != nil {
		return playbackcoverage.Snapshot{}, false, fmt.Errorf("store: get playback coverage: %w", err)
	}
	defer rows.Close()
	snapshot := playbackcoverage.Snapshot{RepresentationID: representationID}
	for rows.Next() {
		var interval playbackcoverage.Interval
		var updated string
		if err := rows.Scan(&interval.Start, &interval.End, &updated); err != nil {
			return playbackcoverage.Snapshot{}, false, fmt.Errorf("store: scan playback coverage: %w", err)
		}
		if len(snapshot.Intervals) == maxIntervals {
			return playbackcoverage.Snapshot{}, false, errors.New("store: playback coverage interval limit exceeded")
		}
		at, err := parseTime(updated)
		if err != nil {
			return playbackcoverage.Snapshot{}, false, fmt.Errorf("store: parse playback coverage time: %w", err)
		}
		snapshot.Intervals = append(snapshot.Intervals, interval)
		snapshot.DeliveredBytes += interval.End - interval.Start
		if at.After(snapshot.UpdatedAt) {
			snapshot.UpdatedAt = at
		}
	}
	if err := rows.Err(); err != nil {
		return playbackcoverage.Snapshot{}, false, fmt.Errorf("store: read playback coverage: %w", err)
	}
	return snapshot, len(snapshot.Intervals) > 0, nil
}

// PlaybackRepresentationCounts is RX-8.2's durable delivery observation: how
// many playback representations have been observed in each FIXED class, and how
// recently the aggregator-proxied class was last delivered.
//
// It complements RX-8.1, which counts reactive-lane outcomes and therefore
// cannot distinguish "no playback occurred" from "playback bypassed
// DarkHarrbor entirely" -- the condition that went unnoticed from 2026-08-14 to
// 2026-08-17 while topology validation still reported healthy.
type PlaybackRepresentationCounts struct {
	// ByClass is keyed by fixed representation class only. Cardinality is
	// bounded by playbackRepresentationClasses plus "other"; no title,
	// identity, URL, service name or per-item label ever appears.
	ByClass map[string]int64
	// AggregatorAge is how long ago the most recent aggregator-proxied
	// delivery was observed. Valid only when AggregatorKnown is true, which
	// makes the ABSENCE case visible without database access.
	AggregatorAge   time.Duration
	AggregatorKnown bool
}

// playbackRepresentationClasses is the closed vocabulary RX-8.2 reports. A
// representation whose prefix is not listed is counted as "other" so that
// cardinality stays fixed no matter what a future lane introduces.
var playbackRepresentationClasses = []string{"mediaflow", "torrent", "http", "nntp", "archive"}

// aggregatorRepresentationClass is the class produced when playback transits
// DarkHarrbor as a universal playback proxy for an aggregator (RD-28 T2/T3).
const aggregatorRepresentationClass = "mediaflow"

// CurrentPlaybackRepresentationMetrics reports RX-8.2's fixed-cardinality
// delivery observation. Missing rows are not an error: a deployment that has
// never served playback reports zero counts and AggregatorKnown false.
func (s *Store) CurrentPlaybackRepresentationMetrics(ctx context.Context, now time.Time) (PlaybackRepresentationCounts, error) {
	out := PlaybackRepresentationCounts{ByClass: make(map[string]int64, len(playbackRepresentationClasses)+1)}
	for _, class := range playbackRepresentationClasses {
		out.ByClass[class] = 0
	}
	out.ByClass["other"] = 0

	rows, err := s.db.QueryContext(ctx, `
        SELECT representation_id, COUNT(*) FROM playback_coverage
        GROUP BY representation_id`)
	if err != nil {
		return PlaybackRepresentationCounts{}, fmt.Errorf("store: playback representation metrics: %w", err)
	}
	defer rows.Close()
	known := make(map[string]bool, len(playbackRepresentationClasses))
	for _, class := range playbackRepresentationClasses {
		known[class] = true
	}
	for rows.Next() {
		var representationID string
		var count int64
		if err := rows.Scan(&representationID, &count); err != nil {
			return PlaybackRepresentationCounts{}, fmt.Errorf("store: scan playback representation metrics: %w", err)
		}
		class := "other"
		if idx := strings.IndexByte(representationID, ':'); idx > 0 && known[representationID[:idx]] {
			class = representationID[:idx]
		}
		out.ByClass[class]++
		_ = count
	}
	if err := rows.Err(); err != nil {
		return PlaybackRepresentationCounts{}, fmt.Errorf("store: iterate playback representation metrics: %w", err)
	}

	at, ok, err := s.LatestPlaybackCoverage(ctx, aggregatorRepresentationClass+":")
	if err != nil {
		return PlaybackRepresentationCounts{}, err
	}
	if ok {
		out.AggregatorKnown = true
		if age := now.Sub(at); age > 0 {
			out.AggregatorAge = age
		}
	}
	return out, nil
}
