-- 0016_http_backend_detection.sql — HS-3.4 detector persistence
-- Stores configured backend identity and protocol classification only.
-- Source/CDN URLs and transient headers never enter this table.

-- +goose Up
CREATE TABLE http_backend_detection (
    backend_id TEXT PRIMARY KEY,
    base_url TEXT NOT NULL,
    detector_version INTEGER NOT NULL,
    backend_type TEXT NOT NULL CHECK (backend_type IN ('omss', 'stremio')),
    detected_at TEXT NOT NULL
);

-- +goose Down
DROP TABLE http_backend_detection;
