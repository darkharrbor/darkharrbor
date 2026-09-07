-- 0003_last_played_at.sql — janitor timer persistence (D51)
--
-- last_played_at records the most recent proxy GET hit for a StateReady item.
-- On startup, the janitor reconciles in-memory AfterFunc timers from this column:
--   remaining = last_played_at + cleanup_hours - now
--   if <= 0: fire cleanup immediately
--   if  > 0: start AfterFunc with remaining duration
--
-- NULL means the item has never been played (no timer to restore).

-- +goose Up
ALTER TABLE items ADD COLUMN last_played_at TEXT;

-- +goose Down
-- SQLite does not support DROP COLUMN on older versions; this is a no-op.
-- In practice, last_played_at is additive and never needs rollback.
SELECT 1;
