package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/contentproof"
)

func (s *Store) ObserveRepresentationConvergence(ctx context.Context, left, right contentproof.LaneCandidate, span contentproof.VerifiedSpan, proposedCanonical string, verifiedAt time.Time, maxRoutes, maxSpans int) (string, error) {
	if right.RepresentationID < left.RepresentationID {
		left, right = right, left
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin representation convergence: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	exists, err := verificationPairExists(ctx, tx, left.RepresentationID, right.RepresentationID)
	if err != nil {
		return "", err
	}
	if !exists {
		for _, id := range []string{left.RepresentationID, right.RepresentationID} {
			var pairs int
			if err := tx.QueryRowContext(ctx, `
                SELECT COUNT(*) FROM (
                    SELECT left_id, right_id FROM representation_verification_spans
                    WHERE left_id=? OR right_id=? GROUP BY left_id, right_id
                )`, id, id).Scan(&pairs); err != nil {
				return "", fmt.Errorf("count representation verification pairs: %w", err)
			}
			if pairs >= maxRoutes {
				return "", errors.New("store: representation verification pair limit exceeded")
			}
		}
	}

	spans, err := loadVerificationSpans(ctx, tx, left.RepresentationID, right.RepresentationID)
	if err != nil {
		return "", err
	}
	spans = mergeVerifiedSpans(append(spans, span))
	if len(spans) > maxSpans {
		return "", errors.New("store: representation verification span limit exceeded")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM representation_verification_spans WHERE left_id=? AND right_id=?`, left.RepresentationID, right.RepresentationID); err != nil {
		return "", fmt.Errorf("replace representation verification spans: %w", err)
	}
	for _, verified := range spans {
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO representation_verification_spans
                (left_id, right_id, byte_offset, byte_length, verified_at)
            VALUES (?, ?, ?, ?, ?)`, left.RepresentationID, right.RepresentationID,
			verified.Offset, verified.Length, formatTime(verifiedAt)); err != nil {
			return "", fmt.Errorf("insert representation verification span: %w", err)
		}
	}
	if len(spans) != 1 || spans[0].Offset != 0 || spans[0].Length != left.Size {
		if err := tx.Commit(); err != nil {
			return "", fmt.Errorf("commit partial representation convergence: %w", err)
		}
		return "", nil
	}

	canonical, err := convergeGroups(ctx, tx, left, right, proposedCanonical, verifiedAt, maxRoutes)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM representation_verification_spans WHERE left_id=? AND right_id=?`, left.RepresentationID, right.RepresentationID); err != nil {
		return "", fmt.Errorf("clear completed representation verification: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit representation convergence: %w", err)
	}
	return canonical, nil
}

func verificationPairExists(ctx context.Context, tx *sql.Tx, left, right string) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM representation_verification_spans WHERE left_id=? AND right_id=? LIMIT 1`, left, right).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read representation verification pair: %w", err)
	}
	return true, nil
}

func loadVerificationSpans(ctx context.Context, tx *sql.Tx, left, right string) ([]contentproof.VerifiedSpan, error) {
	rows, err := tx.QueryContext(ctx, `
        SELECT byte_offset, byte_length FROM representation_verification_spans
        WHERE left_id=? AND right_id=? ORDER BY byte_offset, byte_length`, left, right)
	if err != nil {
		return nil, fmt.Errorf("list representation verification spans: %w", err)
	}
	defer rows.Close()
	var spans []contentproof.VerifiedSpan
	for rows.Next() {
		var span contentproof.VerifiedSpan
		if err := rows.Scan(&span.Offset, &span.Length); err != nil {
			return nil, fmt.Errorf("scan representation verification span: %w", err)
		}
		spans = append(spans, span)
	}
	return spans, rows.Err()
}

func mergeVerifiedSpans(spans []contentproof.VerifiedSpan) []contentproof.VerifiedSpan {
	sort.Slice(spans, func(i, j int) bool { return spans[i].Offset < spans[j].Offset })
	merged := make([]contentproof.VerifiedSpan, 0, len(spans))
	for _, span := range spans {
		if len(merged) == 0 {
			merged = append(merged, span)
			continue
		}
		last := &merged[len(merged)-1]
		lastEnd := last.Offset + last.Length
		spanEnd := span.Offset + span.Length
		if span.Offset > lastEnd {
			merged = append(merged, span)
			continue
		}
		if spanEnd > lastEnd {
			last.Length = spanEnd - last.Offset
		}
	}
	return merged
}

func convergeGroups(ctx context.Context, tx *sql.Tx, left, right contentproof.LaneCandidate, proposed string, at time.Time, maxRoutes int) (string, error) {
	leftCanonical, err := aliasCanonical(ctx, tx, left.RepresentationID)
	if err != nil {
		return "", err
	}
	rightCanonical, err := aliasCanonical(ctx, tx, right.RepresentationID)
	if err != nil {
		return "", err
	}
	canonical := leftCanonical
	if canonical == "" {
		canonical = rightCanonical
	}
	if canonical == "" {
		canonical = proposed
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO canonical_representations (id, byte_size, created_at, updated_at)
            VALUES (?, ?, ?, ?)`, canonical, left.Size, formatTime(at), formatTime(at)); err != nil {
			return "", fmt.Errorf("create canonical representation: %w", err)
		}
	}
	if leftCanonical != "" && rightCanonical != "" && leftCanonical != rightCanonical {
		winner, loser := leftCanonical, rightCanonical
		if loser < winner {
			winner, loser = loser, winner
		}
		var total int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM representation_routes WHERE canonical_id IN (?, ?)`, winner, loser).Scan(&total); err != nil {
			return "", fmt.Errorf("count merged representation routes: %w", err)
		}
		if total > maxRoutes {
			return "", errors.New("store: canonical representation route limit exceeded")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE representation_aliases SET canonical_id=? WHERE canonical_id=?`, winner, loser); err != nil {
			return "", fmt.Errorf("merge representation aliases: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE representation_routes SET canonical_id=? WHERE canonical_id=?`, winner, loser); err != nil {
			return "", fmt.Errorf("merge representation routes: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM canonical_representations WHERE id=?`, loser); err != nil {
			return "", fmt.Errorf("delete merged canonical representation: %w", err)
		}
		canonical = winner
	}
	for _, route := range []contentproof.LaneCandidate{left, right} {
		if err := ensureCanonicalSize(ctx, tx, canonical, route.Size); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO representation_aliases (representation_id, canonical_id, verified_at)
            VALUES (?, ?, ?)
            ON CONFLICT(representation_id) DO UPDATE SET
                canonical_id=excluded.canonical_id, verified_at=excluded.verified_at`,
			route.RepresentationID, canonical, formatTime(at)); err != nil {
			return "", fmt.Errorf("upsert representation alias: %w", err)
		}
		if err := upsertRepresentationRoute(ctx, tx, canonical, route, at); err != nil {
			return "", err
		}
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM representation_routes WHERE canonical_id=?`, canonical).Scan(&count); err != nil {
		return "", fmt.Errorf("count canonical representation routes: %w", err)
	}
	if count > maxRoutes {
		return "", errors.New("store: canonical representation route limit exceeded")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE canonical_representations SET updated_at=? WHERE id=?`, formatTime(at), canonical); err != nil {
		return "", fmt.Errorf("update canonical representation: %w", err)
	}
	return canonical, nil
}

func aliasCanonical(ctx context.Context, tx *sql.Tx, representationID string) (string, error) {
	var canonical string
	err := tx.QueryRowContext(ctx, `SELECT canonical_id FROM representation_aliases WHERE representation_id=?`, representationID).Scan(&canonical)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read representation alias: %w", err)
	}
	return canonical, nil
}

func ensureCanonicalSize(ctx context.Context, tx *sql.Tx, canonical string, size int64) error {
	var existing int64
	if err := tx.QueryRowContext(ctx, `SELECT byte_size FROM canonical_representations WHERE id=?`, canonical).Scan(&existing); err != nil {
		return fmt.Errorf("read canonical representation size: %w", err)
	}
	if existing != size {
		return errors.New("store: canonical representation size conflict")
	}
	return nil
}

func upsertRepresentationRoute(ctx context.Context, tx *sql.Tx, canonical string, route contentproof.LaneCandidate, at time.Time) error {
	var lane, itemID, fileID, releaseKey string
	var size int64
	err := tx.QueryRowContext(ctx, `
        SELECT lane, item_id, file_id, release_key, byte_size
        FROM representation_routes WHERE representation_id=?`, route.RepresentationID).
		Scan(&lane, &itemID, &fileID, &releaseKey, &size)
	if err == nil && (lane != string(route.Lane) || itemID != route.ItemID || fileID != route.FileID || releaseKey != route.ReleaseKey || size != route.Size) {
		return errors.New("store: representation route identity conflict")
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read representation route: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
        INSERT INTO representation_routes
            (representation_id, canonical_id, lane, item_id, file_id, release_key, byte_size, verified_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(representation_id) DO UPDATE SET
            canonical_id=excluded.canonical_id, verified_at=excluded.verified_at`,
		route.RepresentationID, canonical, string(route.Lane), route.ItemID, route.FileID,
		route.ReleaseKey, route.Size, formatTime(at))
	if err != nil {
		return fmt.Errorf("upsert representation route: %w", err)
	}
	return nil
}

func (s *Store) ListRepresentationRoutes(ctx context.Context, representationID string, limit int) ([]contentproof.LaneCandidate, error) {
	if limit < 1 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
        SELECT r.lane, r.item_id, r.file_id, r.representation_id, r.release_key, r.byte_size
        FROM representation_routes r
        JOIN representation_aliases a ON a.canonical_id=r.canonical_id
        WHERE a.representation_id=?
        ORDER BY r.verified_at, r.representation_id LIMIT ?`, representationID, limit+1)
	if err != nil {
		return nil, fmt.Errorf("list representation routes: %w", err)
	}
	defer rows.Close()
	var routes []contentproof.LaneCandidate
	for rows.Next() {
		var route contentproof.LaneCandidate
		if err := rows.Scan(&route.Lane, &route.ItemID, &route.FileID, &route.RepresentationID, &route.ReleaseKey, &route.Size); err != nil {
			return nil, fmt.Errorf("scan representation route: %w", err)
		}
		routes = append(routes, route)
	}
	if len(routes) > limit {
		return nil, errors.New("store: representation route limit exceeded")
	}
	return routes, rows.Err()
}

func (s *Store) ListRepresentationAliases(ctx context.Context, representationID string, limit int) ([]string, int64, error) {
	if limit < 1 {
		return nil, 0, nil
	}
	var canonical string
	var size int64
	err := s.db.QueryRowContext(ctx, `
        SELECT a.canonical_id, c.byte_size
        FROM representation_aliases a JOIN canonical_representations c ON c.id=a.canonical_id
        WHERE a.representation_id=?`, representationID).Scan(&canonical, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("resolve representation alias: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `
        SELECT representation_id FROM representation_aliases
        WHERE canonical_id=? ORDER BY representation_id LIMIT ?`, canonical, limit+1)
	if err != nil {
		return nil, 0, fmt.Errorf("list representation aliases: %w", err)
	}
	defer rows.Close()
	var aliases []string
	for rows.Next() {
		var alias string
		if err := rows.Scan(&alias); err != nil {
			return nil, 0, fmt.Errorf("scan representation alias: %w", err)
		}
		aliases = append(aliases, alias)
	}
	if len(aliases) > limit {
		return nil, 0, errors.New("store: representation alias limit exceeded")
	}
	return aliases, size, rows.Err()
}

func (s *Store) ResolveRepresentationCanonical(ctx context.Context, representationID string) (string, bool, error) {
	var canonical string
	err := s.db.QueryRowContext(ctx, `SELECT canonical_id FROM representation_aliases WHERE representation_id=?`, representationID).Scan(&canonical)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolve canonical representation: %w", err)
	}
	return canonical, true, nil
}
