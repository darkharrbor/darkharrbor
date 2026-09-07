-- 0020_nntp_offset_file_keys.sql — discard unsafe pre-file-key NS-0.3 rows

-- +goose Up

DELETE FROM nntp_segment_offsets WHERE instr(content_key, ':file:') = 0;

-- +goose Down

-- Cache rows are reproducible from item-scoped offsets or NNTP payloads.
