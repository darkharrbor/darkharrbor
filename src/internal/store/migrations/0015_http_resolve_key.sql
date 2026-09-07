-- 0015_http_resolve_key.sql — HTTP stream provider (HS-1.1, G2)
--
-- Adds items.resolve_key: the canonical, URL-free resolve key JSON for
-- source_type='http' items (HTTP-STREAM-MASTER-PLAN.md D10). It is the item's
-- only re-resolution identity — no source/CDN URL is ever persisted.
-- source_type now admits 'http' alongside 'torrent' | 'nzb' (the 0001 column
-- is comment-constrained only; no CHECK exists, so no table rebuild needed).
--
-- Backfill: n/a — no pre-existing rows can be source_type='http'.
-- Downgrade posture: the Down drops the column (SQLite >= 3.35 via modernc);
-- any http items lose their resolve keys and become permanently
-- unresolvable — downgrading past this migration is only safe when no
-- source_type='http' rows exist.

-- +goose Up
ALTER TABLE items ADD COLUMN resolve_key TEXT;

-- +goose Down
ALTER TABLE items DROP COLUMN resolve_key;
