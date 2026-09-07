-- 0005_rar_manifest.sql — per-item RAR part manifest for RAR-split NZB streaming (D123/D124)

-- +goose Up
ALTER TABLE items ADD COLUMN rar_manifest TEXT;

-- +goose Down
-- SQLite ALTER TABLE DROP COLUMN requires SQLite 3.35+. If unavailable,
-- this migration is irreversible without recreating the table.
-- ALTER TABLE items DROP COLUMN rar_manifest;
