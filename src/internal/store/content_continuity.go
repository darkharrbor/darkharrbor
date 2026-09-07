package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/contentproof"
)

func (s *Store) LatestContinuity(ctx context.Context, representationID string, offset, length int64) (*contentproof.ContinuityEntry, error) {
	entry, err := scanContinuity(s.db.QueryRowContext(ctx, `
		SELECT representation_id, byte_offset, byte_length, digest, relation,
		       proof_provenance, mutation, observed_at
		FROM representation_continuity
		WHERE representation_id=? AND byte_offset=? AND byte_length=?
		ORDER BY observed_at DESC, id DESC LIMIT 1`,
		representationID, offset, length))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest representation continuity: %w", err)
	}
	return &entry, nil
}

func (s *Store) AppendContinuity(ctx context.Context, entry contentproof.ContinuityEntry, maxPerRepresentation int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin representation continuity write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO representation_continuity
		    (representation_id, byte_offset, byte_length, digest, relation,
		     proof_provenance, mutation, observed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.RepresentationID, entry.Offset, entry.Length, entry.Digest, string(entry.Relation),
		string(entry.Provenance), boolToInt(entry.Mutation), entry.ObservedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("append representation continuity: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM representation_continuity
		WHERE representation_id=? AND id NOT IN (
			SELECT id FROM representation_continuity WHERE representation_id=?
			ORDER BY observed_at DESC, id DESC LIMIT ?
		)`, entry.RepresentationID, entry.RepresentationID, maxPerRepresentation); err != nil {
		return fmt.Errorf("bound representation continuity: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit representation continuity write: %w", err)
	}
	return nil
}

func (s *Store) ListContinuity(ctx context.Context, representationID string) ([]contentproof.ContinuityEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT representation_id, byte_offset, byte_length, digest, relation,
		       proof_provenance, mutation, observed_at
		FROM representation_continuity
		WHERE representation_id=?
		ORDER BY observed_at, id`, representationID)
	if err != nil {
		return nil, fmt.Errorf("list representation continuity: %w", err)
	}
	defer rows.Close()

	var entries []contentproof.ContinuityEntry
	for rows.Next() {
		entry, err := scanContinuity(rows)
		if err != nil {
			return nil, fmt.Errorf("scan representation continuity: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate representation continuity: %w", err)
	}
	return entries, nil
}

type continuityScanner interface {
	Scan(...any) error
}

func scanContinuity(scanner continuityScanner) (contentproof.ContinuityEntry, error) {
	var entry contentproof.ContinuityEntry
	var relation, provenance, observed string
	var mutation int
	err := scanner.Scan(&entry.RepresentationID, &entry.Offset, &entry.Length, &entry.Digest,
		&relation, &provenance, &mutation, &observed)
	if err != nil {
		return entry, err
	}
	entry.Relation = contentproof.ContinuityRelation(relation)
	entry.Provenance = contentproof.Provenance(provenance)
	entry.Mutation = mutation != 0
	entry.ObservedAt, err = time.Parse(time.RFC3339Nano, observed)
	return entry, err
}
