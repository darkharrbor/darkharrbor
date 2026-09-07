-- 0038_playback_proposals.sql — RX-3.2 idempotent threshold proposals.
-- Stores only DH identities, extent evidence, threshold, and timestamp.

-- +goose Up

CREATE TABLE playback_proposals (
    representation_id TEXT PRIMARY KEY,
    item_id            TEXT NOT NULL,
    file_id            TEXT NOT NULL,
    extent_kind        TEXT NOT NULL CHECK (extent_kind IN ('declared_bytes', 'media_truth_bytes', 'vod_hls_duration')),
    delivered          INTEGER NOT NULL CHECK (delivered > 0),
    total              INTEGER NOT NULL CHECK (total > 0),
    threshold          REAL NOT NULL CHECK (threshold > 0 AND threshold <= 1),
    created_at         TEXT NOT NULL
);
CREATE INDEX idx_playback_proposals_created ON playback_proposals(created_at, representation_id);

-- +goose Down

DROP INDEX IF EXISTS idx_playback_proposals_created;
DROP TABLE IF EXISTS playback_proposals;
