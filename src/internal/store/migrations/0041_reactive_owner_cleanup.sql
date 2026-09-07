-- 0041_reactive_owner_cleanup.sql — RX-4.1 exact compensation ownership.

-- +goose Up

ALTER TABLE reactive_commits ADD COLUMN owner_created INTEGER NOT NULL DEFAULT 0 CHECK (owner_created IN (0,1));

-- +goose Down

ALTER TABLE reactive_commits DROP COLUMN owner_created;
