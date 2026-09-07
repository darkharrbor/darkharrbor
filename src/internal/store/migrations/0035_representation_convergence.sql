-- 0035_representation_convergence.sql — RX-2.2 proof-gated native routes.
-- All fields are opaque IDs, fixed lane vocabulary, sizes, and timestamps.
-- No URL, header, credential, manifest, release name, or payload is stored.

-- +goose Up

CREATE TABLE canonical_representations (
    id         TEXT PRIMARY KEY,
    byte_size  INTEGER NOT NULL CHECK (byte_size > 0),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE representation_aliases (
    representation_id TEXT PRIMARY KEY,
    canonical_id      TEXT NOT NULL REFERENCES canonical_representations(id) ON DELETE CASCADE,
    verified_at       TEXT NOT NULL
);
CREATE INDEX idx_representation_aliases_canonical
    ON representation_aliases(canonical_id, representation_id);

CREATE TABLE representation_routes (
    representation_id TEXT PRIMARY KEY REFERENCES representation_aliases(representation_id) ON DELETE CASCADE,
    canonical_id      TEXT NOT NULL REFERENCES canonical_representations(id) ON DELETE CASCADE,
    lane              TEXT NOT NULL CHECK (lane IN ('torrent', 'nntp', 'http')),
    item_id           TEXT NOT NULL,
    file_id           TEXT NOT NULL,
    release_key       TEXT NOT NULL,
    byte_size         INTEGER NOT NULL CHECK (byte_size > 0),
    verified_at       TEXT NOT NULL
);
CREATE INDEX idx_representation_routes_canonical
    ON representation_routes(canonical_id, verified_at, representation_id);

CREATE TABLE representation_verification_spans (
    left_id     TEXT NOT NULL,
    right_id    TEXT NOT NULL,
    byte_offset INTEGER NOT NULL CHECK (byte_offset >= 0),
    byte_length INTEGER NOT NULL CHECK (byte_length > 0),
    verified_at TEXT NOT NULL,
    PRIMARY KEY (left_id, right_id, byte_offset, byte_length),
    CHECK (left_id < right_id)
);
CREATE INDEX idx_representation_verification_right
    ON representation_verification_spans(right_id, left_id);

-- +goose Down

DROP INDEX IF EXISTS idx_representation_verification_right;
DROP TABLE IF EXISTS representation_verification_spans;
DROP INDEX IF EXISTS idx_representation_routes_canonical;
DROP TABLE IF EXISTS representation_routes;
DROP INDEX IF EXISTS idx_representation_aliases_canonical;
DROP TABLE IF EXISTS representation_aliases;
DROP TABLE IF EXISTS canonical_representations;
