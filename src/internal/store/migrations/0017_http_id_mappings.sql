-- 0017_http_id_mappings.sql — HS-3.6 identifier mapping cache
-- Stores media identifiers and provenance only. No source/CDN URL or secret.

-- +goose Up
CREATE TABLE http_id_mappings (
    source_namespace TEXT NOT NULL,
    source_id TEXT NOT NULL,
    tmdb_id TEXT,
    imdb_id TEXT,
    authoritative_empty INTEGER NOT NULL DEFAULT 0,
    provenance TEXT NOT NULL,
    expires_at TEXT,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (source_namespace, source_id)
);

-- +goose Down
DROP TABLE http_id_mappings;
