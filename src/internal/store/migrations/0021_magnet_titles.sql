-- 0021_magnet_titles.sql — C0.4: durable magnet/title cache surviving restarts

-- +goose Up

CREATE TABLE magnet_titles (
    info_hash  TEXT NOT NULL PRIMARY KEY,
    magnet     TEXT NOT NULL,
    title      TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- +goose Down

DROP TABLE magnet_titles;
