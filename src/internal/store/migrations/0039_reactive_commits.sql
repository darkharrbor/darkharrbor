-- 0039_reactive_commits.sql — RX-4.1 durable commit/undo ownership.

-- +goose Up

CREATE TABLE reactive_commits (
    representation_id TEXT PRIMARY KEY,
    item_id            TEXT NOT NULL,
    file_id            TEXT NOT NULL,
    state              TEXT NOT NULL CHECK (state IN ('committing','parked','committed','undoing','undone')),
    mode               TEXT NOT NULL CHECK (mode IN ('auto','supervised')),
    review_required    INTEGER NOT NULL DEFAULT 0 CHECK (review_required IN (0,1)),
    reason             TEXT NOT NULL DEFAULT '',
    arr_name           TEXT NOT NULL DEFAULT '',
    arr_kind           TEXT NOT NULL DEFAULT '' CHECK (arr_kind IN ('','series','movie')),
    arr_item_id        INTEGER NOT NULL DEFAULT 0 CHECK (arr_item_id >= 0),
    prior_monitored    INTEGER CHECK (prior_monitored IN (0,1)),
    created_at         TEXT NOT NULL,
    updated_at         TEXT NOT NULL
);

CREATE TABLE reactive_commit_files (
    representation_id TEXT NOT NULL REFERENCES reactive_commits(representation_id) ON DELETE CASCADE,
    arr_file_id        INTEGER NOT NULL CHECK (arr_file_id > 0),
    PRIMARY KEY (representation_id, arr_file_id)
);

CREATE TABLE reactive_commit_episodes (
    representation_id TEXT NOT NULL REFERENCES reactive_commits(representation_id) ON DELETE CASCADE,
    arr_episode_id     INTEGER NOT NULL CHECK (arr_episode_id > 0),
    prior_monitored    INTEGER NOT NULL CHECK (prior_monitored IN (0,1)),
    PRIMARY KEY (representation_id, arr_episode_id)
);

CREATE INDEX idx_reactive_commits_state ON reactive_commits(state, updated_at, representation_id);

-- +goose Down

DROP INDEX IF EXISTS idx_reactive_commits_state;
DROP TABLE IF EXISTS reactive_commit_episodes;
DROP TABLE IF EXISTS reactive_commit_files;
DROP TABLE IF EXISTS reactive_commits;
