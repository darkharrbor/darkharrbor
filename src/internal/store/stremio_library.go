package store

import (
	"context"
	"errors"
	"fmt"
)

const MaxStremioLibraryStreams = 100

// StremioLibraryStream is one Ready, DB-authoritative library stream.
// URL remains a DarkHarrbor URL; callers must still enforce same-origin before
// returning it to a client.
type StremioLibraryStream struct {
	ItemID       string
	Title        string
	RelPath      string
	URL          string
	BlobCount    int
	EpisodeCount int
}

// ListStremioLibraryStreams returns Ready authoritative streams whose persisted
// identity exactly matches one Stremio IMDb video identity. A reactive commit is
// not required, but a positive mismatch or explicit undo lifecycle excludes the
// item.
func (s *Store) ListStremioLibraryStreams(ctx context.Context, kind, imdbID string, season, episode, limit int) ([]StremioLibraryStream, error) {
	if (kind != "movie" && kind != "series") || imdbID == "" || limit <= 0 || limit > MaxStremioLibraryStreams {
		return nil, errors.New("store: invalid Stremio library query")
	}
	if (kind == "movie" && (season != 0 || episode != 0)) || (kind == "series" && (season < 0 || episode < 1)) {
		return nil, errors.New("store: invalid Stremio video coordinates")
	}

	query := `
		SELECT i.id, json_extract(i.metadata_json,'$.provider_identity.title'),
		       b.rel_path, b.url,
		       (SELECT COUNT(*) FROM strm_blobs bx WHERE bx.item_id=i.id),
		       COALESCE(json_array_length(i.metadata_json,'$.provider_identity.episodes'),0)
		FROM items i
		JOIN strm_blobs b ON b.item_id=i.id
		WHERE i.state='ready'
		  AND json_extract(i.metadata_json,'$.provider_identity.kind')=?
		  AND lower(json_extract(i.metadata_json,'$.provider_identity.ids.imdb'))=?
		  AND length(json_extract(i.metadata_json,'$.provider_identity.title')) BETWEEN 1 AND 4096
		  AND length(b.rel_path) BETWEEN 1 AND 4096
		  AND length(b.url) BETWEEN 1 AND 16384
		  AND NOT EXISTS (
			SELECT 1 FROM reactive_commits c
			WHERE c.item_id=i.id AND c.state IN ('parked','undoing','undone')
		  )`
	args := []any{kind, imdbID}
	if kind == "series" {
		query += ` AND (
			SELECT COUNT(*)
			FROM json_each(i.metadata_json,'$.provider_identity.episodes') ep
			WHERE json_extract(ep.value,'$.season')=?
			  AND json_extract(ep.value,'$.episode')=?
		)=1`
		args = append(args, season, episode)
	}
	query += ` ORDER BY i.updated_at DESC, i.id ASC, b.file_index ASC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list Stremio library streams: %w", err)
	}
	defer rows.Close()
	streams := make([]StremioLibraryStream, 0)
	for rows.Next() {
		var stream StremioLibraryStream
		if err := rows.Scan(&stream.ItemID, &stream.Title, &stream.RelPath, &stream.URL, &stream.BlobCount, &stream.EpisodeCount); err != nil {
			return nil, fmt.Errorf("store: scan Stremio library stream: %w", err)
		}
		streams = append(streams, stream)
	}
	return streams, rows.Err()
}
