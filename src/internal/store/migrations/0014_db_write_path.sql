-- 0014_db_write_path.sql — P8 write-path cleanup (PERF-4, DC-9)

-- +goose Up

-- The primary key (item_id, file_index, seg_index) already serves queries on
-- its (item_id, file_index) prefix, so this duplicate index only amplifies
-- segment-size writes.
DROP INDEX IF EXISTS idx_segment_offsets_item_file;

-- Stub generation was retired in P3 and all rows were removed. .strm content
-- remains authoritative in strm_blobs.
DROP TABLE IF EXISTS stub_blobs;

-- +goose Down

CREATE TABLE IF NOT EXISTS stub_blobs (
    item_id    TEXT    NOT NULL,
    file_index INTEGER NOT NULL,
    rel_path   TEXT    NOT NULL,
    sha256     TEXT    NOT NULL,
    bytes      BLOB    NOT NULL,
    created_at TEXT    NOT NULL,
    PRIMARY KEY (item_id, file_index)
);

CREATE INDEX IF NOT EXISTS idx_segment_offsets_item_file
    ON segment_offsets (item_id, file_index);
