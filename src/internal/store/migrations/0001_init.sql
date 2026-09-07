-- 0001_init.sql — Dark Harrbor v0 schema
-- 5-state item lifecycle (D27). No transfer_parts table (no local download).

-- +goose Up

CREATE TABLE IF NOT EXISTS items (
    id              TEXT NOT NULL PRIMARY KEY,
    public_id       TEXT NOT NULL UNIQUE,
    source_type     TEXT NOT NULL,   -- torrent | nzb
    client_kind     TEXT NOT NULL,   -- qbit | sab
    category        TEXT NOT NULL DEFAULT '',
    state           TEXT NOT NULL,   -- accepted | resolving | ready | failed | removed
    submission_key  TEXT NOT NULL,

    remote_id       TEXT,
    queued_id       TEXT,
    info_hash       TEXT,
    display_name    TEXT NOT NULL DEFAULT '',
    source_uri      TEXT,

    cached          INTEGER NOT NULL DEFAULT 0,
    strm_path       TEXT,
    sidecar_path    TEXT,
    probe_json      TEXT,
    file_list       TEXT,

    error_message   TEXT,
    retry_count     INTEGER NOT NULL DEFAULT 0,
    next_run_at     TEXT,

    metadata_json   TEXT NOT NULL DEFAULT '{}',

    claimed_by      TEXT,
    claimed_at      TEXT,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_items_state        ON items(state);
CREATE INDEX IF NOT EXISTS idx_items_submission_key ON items(submission_key);
CREATE INDEX IF NOT EXISTS idx_items_public_id    ON items(public_id);
CREATE INDEX IF NOT EXISTS idx_items_next_run_at  ON items(next_run_at);

CREATE TABLE IF NOT EXISTS item_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    item_id     TEXT NOT NULL REFERENCES items(id),
    from_state  TEXT,
    to_state    TEXT,
    message     TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_item_events_item_id ON item_events(item_id);

CREATE TABLE IF NOT EXISTS qbit_sessions (
    sid        TEXT NOT NULL PRIMARY KEY,
    username   TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL
);

-- governor_log tracks uncached transfer events for the rolling 15-day window (D3).
CREATE TABLE IF NOT EXISTS governor_log (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    item_id     TEXT NOT NULL REFERENCES items(id),
    event_type  TEXT NOT NULL DEFAULT 'uncached_add',
    created_at  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_governor_log_created_at ON governor_log(created_at);

-- +goose Down

DROP INDEX IF EXISTS idx_governor_log_created_at;
DROP TABLE IF EXISTS governor_log;
DROP TABLE IF EXISTS qbit_sessions;
DROP INDEX IF EXISTS idx_item_events_item_id;
DROP TABLE IF EXISTS item_events;
DROP INDEX IF EXISTS idx_items_next_run_at;
DROP INDEX IF EXISTS idx_items_public_id;
DROP INDEX IF EXISTS idx_items_submission_key;
DROP INDEX IF EXISTS idx_items_state;
DROP TABLE IF EXISTS items;
