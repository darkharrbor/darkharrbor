-- 0009_item_stream_failures.sql — stream-time dead-letter for NZB/NNTP items (Surface 3)
--
-- The D54 failed_hashes breaker is keyed on a torrent infohash, which NZB items
-- do not have, so NNTP-streamed items had no failure accounting at /stream time:
-- a release whose Usenet articles are gone was re-fetched from the provider on
-- every play/probe/scan forever. This table mirrors failed_hashes but is keyed on
-- item_id. The /stream handler increments on a genuine content failure (NOT a
-- client abort); at threshold the item enters a dead window the handler
-- short-circuits, so a dead release stops hammering the NNTP provider.

-- +goose Up

CREATE TABLE IF NOT EXISTS item_stream_failures (
    item_id        TEXT NOT NULL PRIMARY KEY,   -- store item id (no infohash for NZBs)
    fail_count     INTEGER NOT NULL DEFAULT 0,
    last_failed_at TEXT NOT NULL,               -- RFC3339Nano UTC
    dead_until     TEXT                         -- NULL = not currently dead
);

CREATE INDEX IF NOT EXISTS idx_item_stream_failures_dead_until ON item_stream_failures(dead_until);

-- +goose Down

DROP INDEX IF EXISTS idx_item_stream_failures_dead_until;
DROP TABLE IF EXISTS item_stream_failures;
