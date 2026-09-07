-- 0036_playback_coverage.sql — RX-3.1 delivered-byte interval union.
-- Stores only opaque representation IDs, byte coordinates, and timestamps.

-- +goose Up

CREATE TABLE playback_coverage (
    representation_id TEXT NOT NULL,
    byte_start        INTEGER NOT NULL CHECK (byte_start >= 0),
    byte_end          INTEGER NOT NULL CHECK (byte_end > byte_start),
    updated_at        TEXT NOT NULL,
    PRIMARY KEY (representation_id, byte_start)
);
CREATE INDEX idx_playback_coverage_updated
    ON playback_coverage(updated_at, representation_id);

-- +goose Down

DROP INDEX IF EXISTS idx_playback_coverage_updated;
DROP TABLE IF EXISTS playback_coverage;
