package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// magnetTitleTTL bounds the durable magnet/title cache (C0.4) so it holds
// recently seen releases rather than growing without bound. Titles are tiny
// strings; this is a hygiene bound, not a correctness requirement, so a
// simple opportunistic delete-on-write is sufficient (no separate sweep
// goroutine).
const magnetTitleTTL = 30 * 24 * time.Hour

// UpsertMagnetTitle durably records infoHash -> (magnet, title) so a
// DarkHarrbor restart between a Sonarr search (which is the only time DH
// ever learns a release's real title for a tracker-only magnet) and a later
// grab (which carries only the hash) does not reintroduce hash-named
// releases. This is the restart-survival companion to the in-memory
// magnetCache in internal/api: that map remains the fast path; this table is
// the durable fallback consulted on a cache miss.
func (s *Store) UpsertMagnetTitle(ctx context.Context, infoHash, magnet, title string) error {
	if infoHash == "" {
		return nil
	}
	now := formatTime(s.now())
	if _, err := s.execWrite(ctx, `
		INSERT INTO magnet_titles (info_hash, magnet, title, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(info_hash) DO UPDATE SET
			magnet=excluded.magnet,
			title=excluded.title,
			updated_at=excluded.updated_at`,
		infoHash, magnet, title, now); err != nil {
		return fmt.Errorf("upsert magnet title %s: %w", infoHash, err)
	}
	cutoff := formatTime(s.now().Add(-magnetTitleTTL))
	if _, err := s.execWrite(ctx, `DELETE FROM magnet_titles WHERE updated_at < ?`, cutoff); err != nil {
		return fmt.Errorf("prune magnet titles: %w", err)
	}
	return nil
}

// GetMagnetTitle returns the durably recorded (magnet, title) pair for
// infoHash, if any row exists.
func (s *Store) GetMagnetTitle(ctx context.Context, infoHash string) (magnet, title string, ok bool, err error) {
	if infoHash == "" {
		return "", "", false, nil
	}
	row := s.db.QueryRowContext(ctx, `SELECT magnet, title FROM magnet_titles WHERE info_hash=?`, infoHash)
	if scanErr := row.Scan(&magnet, &title); scanErr != nil {
		if scanErr == sql.ErrNoRows {
			return "", "", false, nil
		}
		return "", "", false, fmt.Errorf("get magnet title %s: %w", infoHash, scanErr)
	}
	return magnet, title, true, nil
}
