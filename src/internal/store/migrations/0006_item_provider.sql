-- 0006_item_provider.sql — per-item provider attribution (gate 11.feature_cache_unification,
-- deferred from 11.feature_provider_plugin §6). NULL means the default provider;
-- the single-provider default path is unaffected.

-- +goose Up
ALTER TABLE items ADD COLUMN provider TEXT;

-- +goose Down
-- SQLite ALTER TABLE DROP COLUMN requires SQLite 3.35+. If unavailable,
-- this migration is irreversible without recreating the table.
ALTER TABLE items DROP COLUMN provider;
