-- 0008_synthetic_release.sql — Phase 16 feature_multiseason_grab.
-- Maps a synthetic infohash (one per real-torrent+season, advertised to Sonarr
-- as a per-season release of a cached multi-season torrent) back to the real
-- torrent reference and the season it represents, so a grab of the synthetic
-- hash resolves to the right torrent and materialises only that season's files.
-- Persisted so a grab that lands after a restart still resolves.

-- +goose Up
CREATE TABLE synthetic_release (
    synth_hash    TEXT    NOT NULL PRIMARY KEY,
    real_infohash TEXT    NOT NULL,
    magnet        TEXT    NOT NULL,
    season        INTEGER NOT NULL,
    media_id      TEXT    NOT NULL DEFAULT '',
    file_ids      TEXT    NOT NULL DEFAULT '',
    created_at    TEXT    NOT NULL
);
CREATE INDEX idx_synthetic_release_real ON synthetic_release (real_infohash);

-- +goose Down
DROP INDEX idx_synthetic_release_real;
DROP TABLE synthetic_release;
