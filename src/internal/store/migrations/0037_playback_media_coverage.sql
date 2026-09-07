-- 0037_playback_media_coverage.sql — RX-3.1 native VOD HLS media-time union.
-- Stores only opaque representation IDs, nanosecond coordinates, and timestamps.

-- +goose Up

CREATE TABLE playback_media_coverage (
    representation_id TEXT NOT NULL,
    media_start_ns    INTEGER NOT NULL CHECK (media_start_ns >= 0),
    media_end_ns      INTEGER NOT NULL CHECK (media_end_ns > media_start_ns),
    updated_at        TEXT NOT NULL,
    PRIMARY KEY (representation_id, media_start_ns)
);
CREATE INDEX idx_playback_media_coverage_updated
    ON playback_media_coverage(updated_at, representation_id);

-- +goose Down

DROP INDEX IF EXISTS idx_playback_media_coverage_updated;
DROP TABLE IF EXISTS playback_media_coverage;
