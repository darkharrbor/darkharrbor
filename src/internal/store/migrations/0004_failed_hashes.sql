-- 0004_failed_hashes.sql — hash-level blacklist for re-search loop prevention (D54)
-- When a hash repeatedly evicts from TorBox cache, increment fail_count.
-- At threshold (3), set blacklisted_until = now + 7 days.
-- Dark Harrbor checks IsBlacklisted before notifying Sonarr to re-search.

-- +goose Up

CREATE TABLE IF NOT EXISTS failed_hashes (
    hash              TEXT NOT NULL PRIMARY KEY,   -- torrent infohash (lowercase hex)
    fail_count        INTEGER NOT NULL DEFAULT 0,
    last_failed_at    TEXT NOT NULL,               -- RFC3339Nano UTC
    blacklisted_until TEXT                         -- NULL = not currently blacklisted
);

CREATE INDEX IF NOT EXISTS idx_failed_hashes_blacklisted_until ON failed_hashes(blacklisted_until);

-- +goose Down

DROP INDEX IF EXISTS idx_failed_hashes_blacklisted_until;
DROP TABLE IF EXISTS failed_hashes;
