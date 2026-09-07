-- 0022_release_suppressions.sql — SF-03: own-measurement failed-release suppression

-- +goose Up

CREATE TABLE release_suppressions (
    fingerprint TEXT NOT NULL PRIMARY KEY,
    lane        TEXT NOT NULL,
    reason      TEXT NOT NULL,
    first_seen  TEXT NOT NULL,
    last_seen   TEXT NOT NULL,
    count       INTEGER NOT NULL DEFAULT 1,
    expires_at  TEXT NOT NULL
);
CREATE INDEX idx_release_suppressions_expires ON release_suppressions(expires_at);

-- +goose Down

DROP TABLE release_suppressions;
