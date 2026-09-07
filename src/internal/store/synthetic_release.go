package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SyntheticRelease maps a synthetic infohash — advertised to Sonarr as one
// per-season release of a cached multi-season torrent — back to the real
// torrent reference and the season it represents. The resolver dereferences
// this on grab to add the real torrent once and materialise only that season's
type SyntheticRelease struct {
	SynthHash    string
	RealInfohash string
	Magnet       string
	Season       int
	MediaID      string
	FileIDs      string // JSON array of provider file ids/indices for this season
	CreatedAt    time.Time
}

// UpsertSyntheticRelease inserts or replaces a synthetic-release mapping.
func (s *Store) UpsertSyntheticRelease(ctx context.Context, sr SyntheticRelease) error {
	createdAt := sr.CreatedAt
	if createdAt.IsZero() {
		createdAt = s.now()
	}
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO synthetic_release
            (synth_hash, real_infohash, magnet, season, media_id, file_ids, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(synth_hash) DO UPDATE SET
            real_infohash = excluded.real_infohash,
            magnet        = excluded.magnet,
            season        = excluded.season,
            media_id      = excluded.media_id,
            file_ids      = excluded.file_ids`,
		sr.SynthHash, sr.RealInfohash, sr.Magnet, sr.Season, sr.MediaID, sr.FileIDs,
		createdAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("upsert synthetic release: %w", err)
	}
	return nil
}

// GetSyntheticRelease returns the mapping for a synthetic infohash, or
// (nil, nil) if none exists.
func (s *Store) GetSyntheticRelease(ctx context.Context, synthHash string) (*SyntheticRelease, error) {
	var sr SyntheticRelease
	var createdAt string
	err := s.db.QueryRowContext(ctx, `
        SELECT synth_hash, real_infohash, magnet, season, media_id, file_ids, created_at
        FROM synthetic_release WHERE synth_hash = ?`, synthHash).
		Scan(&sr.SynthHash, &sr.RealInfohash, &sr.Magnet, &sr.Season, &sr.MediaID, &sr.FileIDs, &createdAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get synthetic release: %w", err)
	}
	if t, perr := time.Parse(time.RFC3339Nano, createdAt); perr == nil {
		sr.CreatedAt = t
	}
	return &sr, nil
}
