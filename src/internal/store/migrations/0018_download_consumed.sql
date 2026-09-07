-- 0018_download_consumed.sql — C0.1: download-path ownership for COR-3
--
-- Adds items.download_consumed: tracks whether the Arr has consumed
-- (imported/moved) the download-side .strm from DH's download directory.
-- When true, the blob reconciler skips re-materialization of the download
-- path, preventing the COR-3 infinite-recreate loop for Ready SAB/NZB items.
--
-- The permanent library .strm (placed by the Arr in its library directory)
-- is unaffected — it points to a stable DH URL and playback resolves by
-- item ID, not filesystem path.
--
-- Backfill: any item with sab_history_hidden=true has already been imported
-- by the Arr, so its download-side .strm is consumed.
--
-- Downgrade posture: the Down drops the column; reconciliation reverts to
-- the pre-COR-3-fix behavior (recreating consumed download-side .strms).
-- No data loss; playback is unaffected.

-- +goose Up
ALTER TABLE items ADD COLUMN download_consumed INTEGER NOT NULL DEFAULT 0;
UPDATE items SET download_consumed = 1
    WHERE COALESCE(json_extract(metadata_json, '$.sab_history_hidden'), 0) = 1;

-- +goose Down
ALTER TABLE items DROP COLUMN download_consumed;
