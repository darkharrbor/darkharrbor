package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/hlssession"
)

func (s *Store) InsertSession(ctx context.Context, sess hlssession.Session) error {
	_, err := s.execWrite(ctx, `
		INSERT INTO hls_sessions (id, item_id, file_id, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)`,
		sess.SessionID, sess.ItemID, sess.FileID,
		sess.CreatedAt.UTC().Format(time.RFC3339Nano), sess.ExpiresAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("insert hls session: %w", err)
	}
	return nil
}

func (s *Store) GetSession(ctx context.Context, sessionID string, now time.Time) (*hlssession.Session, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, item_id, file_id, created_at, expires_at
		FROM hls_sessions WHERE id=? AND expires_at > ?`,
		sessionID, now.UTC().Format(time.RFC3339Nano))
	sess, err := scanHLSSession(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get hls session: %w", err)
	}
	return &sess, nil
}

func (s *Store) TouchSession(ctx context.Context, sessionID string, expiresAt time.Time) (bool, error) {
	res, err := s.execWrite(ctx, `UPDATE hls_sessions SET expires_at=? WHERE id=?`,
		expiresAt.UTC().Format(time.RFC3339Nano), sessionID)
	if err != nil {
		return false, fmt.Errorf("touch hls session: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("touch hls session rows affected: %w", err)
	}
	return n > 0, nil
}

func (s *Store) InsertResource(ctx context.Context, r hlssession.Resource, maxPerSession int, now time.Time) error {
	refJSON, err := hlssession.MarshalReference(r.Reference)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin hls resource write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM hls_sessions WHERE id=? AND expires_at > ?`,
		r.SessionID, now.UTC().Format(time.RFC3339Nano)).Scan(&exists); err == sql.ErrNoRows {
		return hlssession.ErrSessionNotFound
	} else if err != nil {
		return fmt.Errorf("check hls session for resource insert: %w", err)
	}

	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM hls_resources WHERE session_id=?`, r.SessionID).Scan(&count); err != nil {
		return fmt.Errorf("count hls resources: %w", err)
	}
	if count >= maxPerSession {
		return hlssession.ErrSessionResourceCapacity
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO hls_resources (id, session_id, kind, ref_json, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		r.ResourceID, r.SessionID, string(r.Kind), refJSON, r.CreatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("insert hls resource: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit hls resource write: %w", err)
	}
	return nil
}

func (s *Store) GetResource(ctx context.Context, resourceID string, now time.Time) (*hlssession.Resource, *hlssession.Session, error) {
	nowStr := now.UTC().Format(time.RFC3339Nano)
	row := s.db.QueryRowContext(ctx, `
		SELECT r.id, r.session_id, r.kind, r.ref_json, r.created_at,
		       sess.id, sess.item_id, sess.file_id, sess.created_at, sess.expires_at
		FROM hls_resources r
		JOIN hls_sessions sess ON sess.id = r.session_id
		WHERE r.id=? AND sess.expires_at > ?`, resourceID, nowStr)

	var (
		resID, sessID, kind, refJSON, resCreated      string
		sID, itemID, fileID, sessCreated, sessExpires string
	)
	err := row.Scan(&resID, &sessID, &kind, &refJSON, &resCreated,
		&sID, &itemID, &fileID, &sessCreated, &sessExpires)
	if err == sql.ErrNoRows {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("get hls resource: %w", err)
	}

	ref, err := hlssession.UnmarshalReference(refJSON)
	if err != nil {
		return nil, nil, fmt.Errorf("unmarshal hls resource reference: %w", err)
	}
	resCreatedAt, err := time.Parse(time.RFC3339Nano, resCreated)
	if err != nil {
		return nil, nil, fmt.Errorf("parse hls resource created_at: %w", err)
	}
	sessCreatedAt, err := time.Parse(time.RFC3339Nano, sessCreated)
	if err != nil {
		return nil, nil, fmt.Errorf("parse hls session created_at: %w", err)
	}
	sessExpiresAt, err := time.Parse(time.RFC3339Nano, sessExpires)
	if err != nil {
		return nil, nil, fmt.Errorf("parse hls session expires_at: %w", err)
	}

	resource := &hlssession.Resource{
		ResourceID: resID,
		SessionID:  sessID,
		Kind:       hlssession.ResourceKind(kind),
		Reference:  ref,
		CreatedAt:  resCreatedAt,
	}
	session := &hlssession.Session{
		SessionID: sID,
		ItemID:    itemID,
		FileID:    fileID,
		CreatedAt: sessCreatedAt,
		ExpiresAt: sessExpiresAt,
	}
	return resource, session, nil
}

func (s *Store) ListResources(ctx context.Context, sessionID string, now time.Time) ([]hlssession.Resource, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.session_id, r.kind, r.ref_json, r.created_at
		FROM hls_resources r
		JOIN hls_sessions sess ON sess.id = r.session_id
		WHERE r.session_id=? AND sess.expires_at > ?
		ORDER BY r.created_at, r.id`, sessionID, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("list hls resources: %w", err)
	}
	defer rows.Close()

	var resources []hlssession.Resource
	for rows.Next() {
		var id, sessID, kind, refJSON, created string
		if err := rows.Scan(&id, &sessID, &kind, &refJSON, &created); err != nil {
			return nil, fmt.Errorf("scan hls resource: %w", err)
		}
		ref, err := hlssession.UnmarshalReference(refJSON)
		if err != nil {
			return nil, fmt.Errorf("unmarshal hls resource reference: %w", err)
		}
		createdAt, err := time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, fmt.Errorf("parse hls resource created_at: %w", err)
		}
		resources = append(resources, hlssession.Resource{
			ResourceID: id, SessionID: sessID, Kind: hlssession.ResourceKind(kind),
			Reference: ref, CreatedAt: createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate hls resources: %w", err)
	}
	return resources, nil
}

func (s *Store) DeleteResources(ctx context.Context, sessionID string, resourceIDs []string) (int, error) {
	if len(resourceIDs) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin hls resource delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	deleted := int64(0)
	for _, id := range resourceIDs {
		res, err := tx.ExecContext(ctx, `DELETE FROM hls_resources WHERE session_id=? AND id=?`, sessionID, id)
		if err != nil {
			return 0, fmt.Errorf("delete hls resource: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("delete hls resource rows affected: %w", err)
		}
		deleted += n
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit hls resource delete: %w", err)
	}
	return int(deleted), nil
}

func (s *Store) PruneExpired(ctx context.Context, now time.Time) (int, error) {
	res, err := s.execWrite(ctx, `DELETE FROM hls_sessions WHERE expires_at <= ?`, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("prune expired hls sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune expired hls sessions rows affected: %w", err)
	}
	return int(n), nil
}

type hlsSessionScanner interface {
	Scan(...any) error
}

func scanHLSSession(row hlsSessionScanner) (hlssession.Session, error) {
	var sess hlssession.Session
	var created, expires string
	if err := row.Scan(&sess.SessionID, &sess.ItemID, &sess.FileID, &created, &expires); err != nil {
		return sess, err
	}
	var err error
	sess.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return sess, err
	}
	sess.ExpiresAt, err = time.Parse(time.RFC3339Nano, expires)
	return sess, err
}
