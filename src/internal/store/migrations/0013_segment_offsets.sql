-- 0013_segment_offsets.sql — decoded NNTP segment size cache for correct seeking
--
-- NZB files declare segment sizes in yEnc-encoded bytes. The actual decoded
-- payload is smaller by the yEnc overhead (~2% per segment). The cumulative
-- drift across a 2.4GB file (~3400 segments) can exceed 48MB, causing range
-- requests to map to the wrong segment index. Jellyfin receives data from the
-- wrong position in the file, ffmpeg cannot decode it, and the connection is
-- closed immediately — seek always fails.
--
-- This table records the actual decoded byte length for each segment as it is
-- fetched and yEnc-decoded during streaming. The corrected cumulative offset
-- array is used for subsequent range-to-segment mapping, enabling accurate seek.
--
-- Population is passive: segments are recorded as they stream during first play.
-- Seeks into already-played content use the corrected map (exact). Seeks into
-- unplayed content fall back to declared-size estimation (within ~2%) and the
-- mkv Cues element handles fine positioning from there.

-- +goose Up

CREATE TABLE IF NOT EXISTS segment_offsets (
    item_id     TEXT NOT NULL,
    file_index  INTEGER NOT NULL,
    seg_index   INTEGER NOT NULL,
    decoded_bytes INTEGER NOT NULL,
    PRIMARY KEY (item_id, file_index, seg_index)
);

CREATE INDEX IF NOT EXISTS idx_segment_offsets_item_file
    ON segment_offsets (item_id, file_index);

-- +goose Down

DROP INDEX IF EXISTS idx_segment_offsets_item_file;
DROP TABLE IF EXISTS segment_offsets;
