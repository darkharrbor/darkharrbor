-- 0024_content_proofs.sql — HR1.2: bounded URL-free Content Proof Graph persistence

-- +goose Up

CREATE TABLE content_proofs (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    representation_id TEXT NOT NULL,
    scope             TEXT NOT NULL CHECK (scope IN ('whole', 'block')),
    byte_offset       INTEGER NOT NULL CHECK (byte_offset >= 0),
    byte_length       INTEGER NOT NULL CHECK (byte_length > 0),
    kind              TEXT NOT NULL CHECK (kind IN ('authoritative', 'same_origin', 'tofu', 'hint')),
    algorithm         TEXT NOT NULL,
    digest            BLOB NOT NULL CHECK (length(digest) BETWEEN 1 AND 64),
    provenance        TEXT NOT NULL,
    origin_id         TEXT NOT NULL DEFAULT '',
    expires_at        TEXT,
    observed_at       TEXT NOT NULL,
    UNIQUE (representation_id, scope, byte_offset, byte_length, kind, algorithm, provenance, origin_id)
);

CREATE INDEX idx_content_proofs_representation
    ON content_proofs (representation_id, observed_at DESC);

-- +goose Down

DROP INDEX IF EXISTS idx_content_proofs_representation;
DROP TABLE IF EXISTS content_proofs;
