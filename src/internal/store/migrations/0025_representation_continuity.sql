-- 0025_representation_continuity.sql — HR1.5: bounded Representation Continuity Ledger

-- +goose Up

CREATE TABLE representation_continuity (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    representation_id TEXT NOT NULL,
    byte_offset       INTEGER NOT NULL CHECK (byte_offset >= 0),
    byte_length       INTEGER NOT NULL CHECK (byte_length > 0),
    digest            BLOB NOT NULL CHECK (length(digest) = 32),
    relation          TEXT NOT NULL CHECK (relation IN ('tofu', 'authoritative', 'conflict')),
    proof_provenance  TEXT NOT NULL DEFAULT '',
    mutation          INTEGER NOT NULL CHECK (mutation IN (0, 1)),
    observed_at       TEXT NOT NULL
);

CREATE INDEX idx_representation_continuity_history
    ON representation_continuity (representation_id, byte_offset, byte_length, observed_at DESC);

-- +goose Down

DROP INDEX IF EXISTS idx_representation_continuity_history;
DROP TABLE IF EXISTS representation_continuity;
