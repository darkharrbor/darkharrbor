-- 0027_sidecar_resources.sql — HR5.5 stable, bounded non-media resources.

-- +goose Up

CREATE TABLE sidecar_resources (
    id          TEXT PRIMARY KEY,
    item_id     TEXT NOT NULL REFERENCES items (id) ON DELETE CASCADE,
    file_id     TEXT NOT NULL,
    kind        TEXT NOT NULL CHECK (kind IN ('subtitle', 'chapters', 'attachment', 'sidecar')),
    filename    TEXT NOT NULL CHECK (length(filename) BETWEEN 1 AND 255),
    media_type  TEXT NOT NULL CHECK (length(media_type) BETWEEN 1 AND 255),
    language    TEXT NOT NULL CHECK (length(language) <= 35),
    bytes       BLOB NOT NULL CHECK (length(bytes) > 0),
    created_at  TEXT NOT NULL
);

CREATE INDEX idx_sidecar_resources_item_file
    ON sidecar_resources (item_id, file_id, created_at, id);

-- +goose Down

DROP INDEX IF EXISTS idx_sidecar_resources_item_file;
DROP TABLE IF EXISTS sidecar_resources;
