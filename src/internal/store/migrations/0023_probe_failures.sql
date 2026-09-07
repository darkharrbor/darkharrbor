-- 0023_probe_failures.sql — SF-05: bounded negative probe cache

-- +goose Up

CREATE TABLE item_file_probe_failures (
    item_id    TEXT NOT NULL,
    file_id    TEXT NOT NULL,
    args_key   TEXT NOT NULL,
    exit_code  INTEGER NOT NULL,
    stdout     TEXT NOT NULL,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    hits       INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (item_id, file_id, args_key)
);

-- +goose Down

DROP TABLE item_file_probe_failures;
