package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/contentproof"
)

func (s *Store) UpsertContentProof(ctx context.Context, e contentproof.Evidence, maxPerRepresentation int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin content proof write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var expires any
	if e.ExpiresAt != nil {
		expires = e.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO content_proofs
		    (representation_id, scope, byte_offset, byte_length, kind, algorithm,
		     digest, provenance, origin_id, expires_at, observed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(representation_id, scope, byte_offset, byte_length, kind, algorithm, provenance, origin_id)
		DO UPDATE SET digest=excluded.digest, expires_at=excluded.expires_at, observed_at=excluded.observed_at`,
		e.RepresentationID, string(e.Scope), e.Offset, e.Length, string(e.Kind), string(e.Algorithm),
		e.Digest, string(e.Provenance), e.OriginID, expires, e.ObservedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("upsert content proof: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		DELETE FROM content_proofs
		WHERE representation_id=? AND id NOT IN (
			SELECT id FROM content_proofs WHERE representation_id=?
			ORDER BY observed_at DESC, id DESC LIMIT ?
		)`, e.RepresentationID, e.RepresentationID, maxPerRepresentation)
	if err != nil {
		return fmt.Errorf("bound content proofs: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit content proof write: %w", err)
	}
	return nil
}

func (s *Store) ListContentProofs(ctx context.Context, representationIDs []string, now time.Time) ([]contentproof.Evidence, error) {
	return s.listContentProofs(ctx, representationIDs, now, 0, 0, 0, false)
}

// ListContentProofsForWindow returns at most limit authoritative proofs wholly
// contained in one exact byte window. The limit is enforced in SQLite before
// rows are materialized so playback-triggered proof admission stays bounded.
func (s *Store) ListContentProofsForWindow(ctx context.Context, representationIDs []string, now time.Time, offset, length int64, limit int) ([]contentproof.Evidence, error) {
	if offset < 0 || length <= 0 || offset > int64(^uint64(0)>>1)-length || limit <= 0 {
		return nil, errors.New("store: invalid content proof window")
	}
	return s.listContentProofs(ctx, representationIDs, now, offset, length, limit, false)
}

// ListContentProofsIntersectingWindow is the bounded read used when a client
// range starts or ends inside an authoritative proof block.
func (s *Store) ListContentProofsIntersectingWindow(ctx context.Context, representationIDs []string, now time.Time, offset, length int64, limit int) ([]contentproof.Evidence, error) {
	if offset < 0 || length <= 0 || offset > int64(^uint64(0)>>1)-length || limit <= 0 {
		return nil, errors.New("store: invalid content proof window")
	}
	return s.listContentProofs(ctx, representationIDs, now, offset, length, limit, true)
}

func (s *Store) listContentProofs(ctx context.Context, representationIDs []string, now time.Time, offset, length int64, limit int, intersect bool) ([]contentproof.Evidence, error) {
	if len(representationIDs) == 0 {
		return nil, nil
	}
	query := `
		SELECT representation_id, scope, byte_offset, byte_length, kind, algorithm,
		       digest, provenance, origin_id, expires_at, observed_at
		FROM content_proofs
		WHERE representation_id IN (`
	args := make([]any, 0, len(representationIDs)+1)
	for i, id := range representationIDs {
		if i > 0 {
			query += ","
		}
		query += "?"
		args = append(args, id)
	}
	query += `) AND (expires_at IS NULL OR expires_at > ?)`
	args = append(args, now.UTC().Format(time.RFC3339Nano))
	if limit > 0 {
		end := offset + length
		if intersect {
			query += ` AND kind='authoritative' AND byte_length>0
				AND byte_offset<? AND byte_length>? - byte_offset`
			args = append(args, end, offset)
		} else {
			query += ` AND kind='authoritative' AND byte_offset>=? AND byte_length>0
				AND byte_offset<=? AND byte_length<=?-byte_offset`
			args = append(args, offset, end, end)
		}
	}
	query += ` ORDER BY observed_at, id`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit+1)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list content proofs: %w", err)
	}
	defer rows.Close()

	var result []contentproof.Evidence
	for rows.Next() {
		var e contentproof.Evidence
		var scope, kind, algorithm, provenance string
		var expires sql.NullString
		var observed string
		if err := rows.Scan(&e.RepresentationID, &scope, &e.Offset, &e.Length, &kind, &algorithm,
			&e.Digest, &provenance, &e.OriginID, &expires, &observed); err != nil {
			return nil, fmt.Errorf("scan content proof: %w", err)
		}
		e.Scope = contentproof.Scope(scope)
		e.Kind = contentproof.Kind(kind)
		e.Algorithm = contentproof.Algorithm(algorithm)
		e.Provenance = contentproof.Provenance(provenance)
		e.ObservedAt, err = time.Parse(time.RFC3339Nano, observed)
		if err != nil {
			return nil, fmt.Errorf("parse content proof observed_at: %w", err)
		}
		if expires.Valid {
			t, parseErr := time.Parse(time.RFC3339Nano, expires.String)
			if parseErr != nil {
				return nil, fmt.Errorf("parse content proof expires_at: %w", parseErr)
			}
			e.ExpiresAt = &t
		}
		result = append(result, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate content proofs: %w", err)
	}
	if limit > 0 && len(result) > limit {
		return nil, errors.New("store: content proof window limit exceeded")
	}
	return result, nil
}

func (s *Store) DeleteContentProof(ctx context.Context, key contentproof.Key) error {
	_, err := s.execWrite(ctx, `
		DELETE FROM content_proofs
		WHERE representation_id=? AND scope=? AND byte_offset=? AND byte_length=?
		  AND kind=? AND algorithm=? AND provenance=? AND origin_id=?`,
		key.RepresentationID, string(key.Scope), key.Offset, key.Length,
		string(key.Kind), string(key.Algorithm), string(key.Provenance), key.OriginID)
	if err != nil {
		return fmt.Errorf("delete content proof: %w", err)
	}
	return nil
}
