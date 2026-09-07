package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SetFileProbe upserts the stored ffprobe-full JSON result for one file
// within an item, keyed by the exact ffprobe args that produced it. See
// migrations/0011_item_file_probes.sql: this is a passive cache the relay
// endpoint (internal/api/relay.go) populates from Sonarr's own live probe
// results and checks before re-running a live probe for a repeat request
// (e.g. a rescan) -- DH never probes proactively. argsKey must be built
// identically by every writer/reader (a plain deterministic join of the
// args slice) so an exact-match lookup is meaningful.
func (s *Store) SetFileProbe(ctx context.Context, itemID, fileID, argsKey, probeJSON string) error {
	_, err := s.execWrite(ctx, `
        INSERT INTO item_file_probes (item_id, file_id, args_key, probe_json, created_at)
        VALUES (?, ?, ?, ?, ?)
        ON CONFLICT(item_id, file_id, args_key) DO UPDATE SET
            probe_json = excluded.probe_json,
            created_at = excluded.created_at`,
		itemID, fileID, argsKey, probeJSON, formatTime(s.now()))
	if err != nil {
		return fmt.Errorf("set file probe %s/%s/%s: %w", itemID, fileID, argsKey, err)
	}
	return nil
}

// GetFileProbe returns the stored ffprobe-full JSON for one file within an
// item for an exact args_key match, or ("", false, nil) if none is stored
// (never probed at grab time, probed with different args, or resolved
// before migration 0011).
func (s *Store) GetFileProbe(ctx context.Context, itemID, fileID, argsKey string) (string, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT probe_json FROM item_file_probes WHERE item_id=? AND file_id=? AND args_key=? LIMIT 1`,
		itemID, fileID, argsKey)
	var raw string
	if err := row.Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("get file probe %s/%s/%s: %w", itemID, fileID, argsKey, err)
	}
	return raw, true, nil
}

// GetFileProbeByContent is the content-identity fallback for the relay's
// probe cache: it returns the most recent stored probe for (info_hash,
// file_id, args_key) across ALL items — including removed ones (satellite
// rows persist until the 30-day prune). A re-grab of previously-probed
// content creates a fresh item id, so the direct (item_id, ...) lookup can
// never hit; the content of the torrent is identical though, and TorBox
// file ids are stable for the same torrent, so serving the prior item's
// probe is exact. A miss simply falls back to a live probe — this can skip
// work, never fabricate it.
func (s *Store) GetFileProbeByContent(ctx context.Context, infoHash, fileID, argsKey string) (string, bool, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT p.probe_json
        FROM item_file_probes p
        JOIN items i ON i.id = p.item_id
        WHERE i.info_hash = ? AND p.file_id = ? AND p.args_key = ?
        ORDER BY p.created_at DESC
        LIMIT 1`,
		infoHash, fileID, argsKey)
	var raw string
	if err := row.Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("get file probe by content %s/%s/%s: %w", infoHash, fileID, argsKey, err)
	}
	return raw, true, nil
}

// SetFileProbeFailure records a deterministic probe failure (ffprobe ran and
// exited nonzero) for negative caching (SF-05). ttl bounds how long the
// failure is replayed; the row is upserted so a repeat live failure refreshes
// the expiry. Relay-level failures (the process could not run) must NOT be
// recorded here — they are transient, not judgments about the bytes.
func (s *Store) SetFileProbeFailure(ctx context.Context, itemID, fileID, argsKey string, exitCode int, stdout string, ttl time.Duration) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO item_file_probe_failures (item_id, file_id, args_key, exit_code, stdout, created_at, expires_at, hits)
        VALUES (?, ?, ?, ?, ?, ?, ?, 0)
        ON CONFLICT(item_id, file_id, args_key) DO UPDATE SET
            exit_code = excluded.exit_code,
            stdout = excluded.stdout,
            created_at = excluded.created_at,
            expires_at = excluded.expires_at`,
		itemID, fileID, argsKey, exitCode, stdout,
		now.Format(time.RFC3339Nano), now.Add(ttl).Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("store: set file probe failure: %w", err)
	}
	return nil
}

// GetFileProbeFailure returns a live (unexpired) cached probe failure and
// increments its hit counter. Expired rows are deleted on sight and reported
// as not found, so a once-failing file gets a fresh live probe after the TTL.
func (s *Store) GetFileProbeFailure(ctx context.Context, itemID, fileID, argsKey string) (stdout string, exitCode int, hits int64, found bool, err error) {
	var expiresAt string
	row := s.db.QueryRowContext(ctx, `
        SELECT stdout, exit_code, hits, expires_at FROM item_file_probe_failures
        WHERE item_id = ? AND file_id = ? AND args_key = ?`, itemID, fileID, argsKey)
	if scanErr := row.Scan(&stdout, &exitCode, &hits, &expiresAt); scanErr != nil {
		if errors.Is(scanErr, sql.ErrNoRows) {
			return "", 0, 0, false, nil
		}
		return "", 0, 0, false, fmt.Errorf("store: get file probe failure: %w", scanErr)
	}
	exp, perr := time.Parse(time.RFC3339Nano, expiresAt)
	if perr != nil || !time.Now().UTC().Before(exp) {
		_, _ = s.db.ExecContext(ctx, `
            DELETE FROM item_file_probe_failures
            WHERE item_id = ? AND file_id = ? AND args_key = ?`, itemID, fileID, argsKey)
		return "", 0, 0, false, nil
	}
	_, _ = s.db.ExecContext(ctx, `
        UPDATE item_file_probe_failures SET hits = hits + 1
        WHERE item_id = ? AND file_id = ? AND args_key = ?`, itemID, fileID, argsKey)
	return stdout, exitCode, hits + 1, true, nil
}
