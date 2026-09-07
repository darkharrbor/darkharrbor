-- 0019_nntp_content_cache.sql — NS-0.3 cross-item NNTP artifact reuse

-- +goose Up

CREATE TABLE nntp_artifacts (
    content_key  TEXT NOT NULL PRIMARY KEY,
    rar_manifest TEXT,
    zip_manifest TEXT,
    updated_at   TEXT NOT NULL
);

CREATE TABLE nntp_segment_offsets (
    content_key   TEXT NOT NULL,
    file_index    INTEGER NOT NULL,
    seg_index     INTEGER NOT NULL,
    decoded_bytes INTEGER NOT NULL,
    PRIMARY KEY (content_key, file_index, seg_index)
);

-- +goose Down

DROP TABLE nntp_segment_offsets;
DROP TABLE nntp_artifacts;
