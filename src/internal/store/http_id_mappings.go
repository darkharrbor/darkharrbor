package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// HTTPIDMapping is a provenance-bearing identifier mapping used by HTTP
// backend handlers. AuthoritativeEmpty rows are finite negative-cache entries.
type HTTPIDMapping struct {
	SourceNamespace    string
	SourceID           string
	TMDBID             string
	IMDBID             string
	AuthoritativeEmpty bool
	Provenance         string
	ExpiresAt          *time.Time
	UpdatedAt          time.Time
}

func (s *Store) GetHTTPIDMapping(ctx context.Context, namespace, sourceID string) (*HTTPIDMapping, error) {
	var m HTTPIDMapping
	var empty int
	var expires sql.NullString
	var updated string
	err := s.db.QueryRowContext(ctx, `
		SELECT source_namespace, source_id, COALESCE(tmdb_id,''), COALESCE(imdb_id,''),
		       authoritative_empty, provenance, expires_at, updated_at
		FROM http_id_mappings WHERE source_namespace=? AND source_id=?`, namespace, sourceID).
		Scan(&m.SourceNamespace, &m.SourceID, &m.TMDBID, &m.IMDBID, &empty, &m.Provenance, &expires, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get http id mapping: %w", err)
	}
	m.AuthoritativeEmpty = empty != 0
	m.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	if err != nil {
		return nil, fmt.Errorf("parse http id mapping updated_at: %w", err)
	}
	if expires.Valid {
		t, parseErr := time.Parse(time.RFC3339Nano, expires.String)
		if parseErr != nil {
			return nil, fmt.Errorf("parse http id mapping expires_at: %w", parseErr)
		}
		m.ExpiresAt = &t
	}
	return &m, nil
}

func (s *Store) PutHTTPIDMapping(ctx context.Context, m HTTPIDMapping) error {
	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = s.now()
	}
	var expires any
	if m.ExpiresAt != nil {
		expires = m.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	_, err := s.execWrite(ctx, `
		INSERT INTO http_id_mappings
		    (source_namespace, source_id, tmdb_id, imdb_id, authoritative_empty, provenance, expires_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_namespace, source_id) DO UPDATE SET
		    tmdb_id=excluded.tmdb_id,
		    imdb_id=excluded.imdb_id,
		    authoritative_empty=excluded.authoritative_empty,
		    provenance=excluded.provenance,
		    expires_at=excluded.expires_at,
		    updated_at=excluded.updated_at`,
		m.SourceNamespace, m.SourceID, nullableText(m.TMDBID), nullableText(m.IMDBID),
		boolToInt(m.AuthoritativeEmpty), m.Provenance, expires, m.UpdatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("put http id mapping: %w", err)
	}
	return nil
}

func nullableText(v string) any {
	if v == "" {
		return nil
	}
	return v
}
