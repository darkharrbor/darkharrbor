package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SuppressionCounts is the reason-broken-out count of currently ACTIVE
// (non-expired) release suppressions, exposed for SF-03's "counters
// exposed" requirement.
type SuppressionCounts struct {
	Total    int
	ByReason map[string]int
}

// RecordSuppression durably records that a release fingerprint (SF-03:
// normalized name+size+lane, see internal/suppress.Fingerprint) failed with
// a deterministic, distinct reason, suppressing it from DarkHarrbor's own
// feeds until now+ttl. A repeated call for the same fingerprint before
// expiry extends the suppression window from now and increments count
// (own-measurement evidence accumulating, never crowd-shared or imported).
func (s *Store) RecordSuppression(ctx context.Context, fingerprint, lane, reason string, ttl time.Duration) error {
	if fingerprint == "" {
		return nil
	}
	if ttl == 0 {
		// Only an unspecified (zero-value) TTL gets the default. A negative
		// TTL is a deliberate caller choice (used by tests to construct an
		// already-expired row) and must produce a genuinely past expiry, not
		// silently become 72h.
		ttl = 72 * time.Hour
	}
	now := s.now()
	expiresAt := now.Add(ttl)
	if _, err := s.execWrite(ctx, `
		INSERT INTO release_suppressions (fingerprint, lane, reason, first_seen, last_seen, count, expires_at)
		VALUES (?, ?, ?, ?, ?, 1, ?)
		ON CONFLICT(fingerprint) DO UPDATE SET
			lane=excluded.lane,
			reason=excluded.reason,
			last_seen=excluded.last_seen,
			count=release_suppressions.count + 1,
			expires_at=excluded.expires_at`,
		fingerprint, lane, reason, formatTime(now), formatTime(now), formatTime(expiresAt),
	); err != nil {
		return fmt.Errorf("record suppression %s: %w", fingerprint, err)
	}
	return nil
}

// IsSuppressed reports whether fingerprint currently has an unexpired
// suppression recorded against it. A caller (feed rendering) uses this to
// exclude a release from what DarkHarrbor offers an Arr, without touching
// grab/resolve behavior for anything already in flight.
func (s *Store) IsSuppressed(ctx context.Context, fingerprint string) (bool, error) {
	if fingerprint == "" {
		return false, nil
	}
	row := s.db.QueryRowContext(ctx, `SELECT expires_at FROM release_suppressions WHERE fingerprint=?`, fingerprint)
	var expiresAtStr string
	if err := row.Scan(&expiresAtStr); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("check suppression %s: %w", fingerprint, err)
	}
	expiresAt, perr := parseTime(expiresAtStr)
	if perr != nil {
		return false, fmt.Errorf("check suppression %s: parse expiry: %w", fingerprint, perr)
	}
	return s.now().Before(expiresAt), nil
}

// SuppressionCounters returns the count of currently active (unexpired)
// suppressions, broken out by reason, for SF-03's "counters exposed"
// requirement.
func (s *Store) SuppressionCounters(ctx context.Context) (SuppressionCounts, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT reason, COUNT(*) FROM release_suppressions
		WHERE expires_at > ?
		GROUP BY reason`,
		formatTime(s.now()),
	)
	if err != nil {
		return SuppressionCounts{}, fmt.Errorf("suppression counters: %w", err)
	}
	defer rows.Close()
	out := SuppressionCounts{ByReason: map[string]int{}}
	for rows.Next() {
		var reason string
		var n int
		if err := rows.Scan(&reason, &n); err != nil {
			return SuppressionCounts{}, fmt.Errorf("suppression counters: scan: %w", err)
		}
		out.ByReason[reason] = n
		out.Total += n
	}
	if err := rows.Err(); err != nil {
		return SuppressionCounts{}, fmt.Errorf("suppression counters: rows: %w", err)
	}
	return out, nil
}

// PruneExpiredSuppressions deletes every suppression row whose expiry has
// already passed and returns the number removed. Hygiene only — an expired
// row is already inert to IsSuppressed; this just bounds table growth.
func (s *Store) PruneExpiredSuppressions(ctx context.Context) (int64, error) {
	res, err := s.execWrite(ctx, `DELETE FROM release_suppressions WHERE expires_at <= ?`, formatTime(s.now()))
	if err != nil {
		return 0, fmt.Errorf("prune expired suppressions: %w", err)
	}
	return res.RowsAffected()
}
