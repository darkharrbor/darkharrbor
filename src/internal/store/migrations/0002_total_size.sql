-- +goose Up
ALTER TABLE items ADD COLUMN total_size INTEGER NOT NULL DEFAULT 0;

-- +goose Down
-- SQLite does not support DROP COLUMN before 3.35; left as no-op for compatibility.
