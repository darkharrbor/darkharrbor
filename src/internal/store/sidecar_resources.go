package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/sidecar"
)

func (s *Store) InsertSidecarResource(ctx context.Context, r sidecar.Resource, maxResources int, maxBytes int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin sidecar resource write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM sidecar_resources WHERE id=?`, r.ID).Scan(&exists); err == nil {
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check existing sidecar resource: %w", err)
	}

	var count int
	var total int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(length(bytes)), 0)
		FROM sidecar_resources WHERE item_id=?`, r.ItemID).Scan(&count, &total); err != nil {
		return fmt.Errorf("measure sidecar resource capacity: %w", err)
	}
	if count >= maxResources || total+int64(len(r.Bytes)) > maxBytes {
		return sidecar.ErrCapacity
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sidecar_resources
		    (id, item_id, file_id, kind, filename, media_type, language, bytes, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.ItemID, r.FileID, string(r.Kind), r.Filename, r.MediaType,
		r.Language, r.Bytes, r.CreatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("insert sidecar resource: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sidecar resource write: %w", err)
	}
	return nil
}

func (s *Store) GetSidecarResource(ctx context.Context, id string) (*sidecar.Resource, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, item_id, file_id, kind, filename, media_type, language, bytes, created_at
		FROM sidecar_resources WHERE id=?`, id)
	resource, err := scanSidecarResource(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get sidecar resource: %w", err)
	}
	return &resource, nil
}

func (s *Store) ListSidecarResources(ctx context.Context, itemID, fileID string) ([]sidecar.Resource, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, item_id, file_id, kind, filename, media_type, language, bytes, created_at
		FROM sidecar_resources
		WHERE item_id=? AND file_id=?
		ORDER BY created_at, id`, itemID, fileID)
	if err != nil {
		return nil, fmt.Errorf("list sidecar resources: %w", err)
	}
	defer rows.Close()

	var resources []sidecar.Resource
	for rows.Next() {
		resource, err := scanSidecarResource(rows)
		if err != nil {
			return nil, fmt.Errorf("scan sidecar resource: %w", err)
		}
		resources = append(resources, resource)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sidecar resources: %w", err)
	}
	return resources, nil
}

func (s *Store) listItemSidecarResources(ctx context.Context, itemID string) ([]sidecar.Resource, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, item_id, file_id, kind, filename, media_type, language, bytes, created_at
		FROM sidecar_resources
		WHERE item_id=?
		ORDER BY created_at, id`, itemID)
	if err != nil {
		return nil, fmt.Errorf("list item sidecar resources: %w", err)
	}
	defer rows.Close()
	var resources []sidecar.Resource
	for rows.Next() {
		resource, err := scanSidecarResource(rows)
		if err != nil {
			return nil, fmt.Errorf("scan item sidecar resource: %w", err)
		}
		resources = append(resources, resource)
	}
	return resources, rows.Err()
}

type sidecarResourceScanner interface {
	Scan(...any) error
}

func scanSidecarResource(row sidecarResourceScanner) (sidecar.Resource, error) {
	var resource sidecar.Resource
	var kind, created string
	if err := row.Scan(&resource.ID, &resource.ItemID, &resource.FileID, &kind,
		&resource.Filename, &resource.MediaType, &resource.Language, &resource.Bytes, &created); err != nil {
		return resource, err
	}
	resource.Kind = sidecar.Kind(kind)
	var err error
	resource.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	return resource, err
}
