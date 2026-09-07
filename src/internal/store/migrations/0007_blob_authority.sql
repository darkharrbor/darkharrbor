-- 0007_blob_authority.sql — SQLite-authoritative stub/.strm content (F-E, plan §4.2).
-- The DB is authoritative metadata+content; Item.SidecarPath/StrmPath become
-- materialization receipts. rel_path (relative to HARRBOR_DATA_ROOT) records
-- the materialization target so the reconciler can restore deleted or
-- corrupted on-disk copies (F3/X.9 guard).

-- +goose Up
CREATE TABLE stub_blobs (
    item_id    TEXT    NOT NULL,
    file_index INTEGER NOT NULL,
    rel_path   TEXT    NOT NULL,
    sha256     TEXT    NOT NULL,
    bytes      BLOB    NOT NULL,
    created_at TEXT    NOT NULL,
    PRIMARY KEY (item_id, file_index)
);

CREATE TABLE strm_blobs (
    item_id    TEXT    NOT NULL,
    file_index INTEGER NOT NULL,
    rel_path   TEXT    NOT NULL,
    sha256     TEXT    NOT NULL,
    url        TEXT    NOT NULL,
    created_at TEXT    NOT NULL,
    PRIMARY KEY (item_id, file_index)
);

-- +goose Down
DROP TABLE strm_blobs;
DROP TABLE stub_blobs;
