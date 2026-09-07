-- 0043_reactive_promotions.sql — RX-5.1 explicit materialization state.

-- +goose Up

CREATE TABLE reactive_promotions (
    representation_id TEXT PRIMARY KEY REFERENCES reactive_commits(representation_id) ON DELETE CASCADE,
    state              TEXT NOT NULL CHECK (state IN ('staging','promoted','failed','removed')),
    target_path        TEXT NOT NULL,
    size_bytes         INTEGER NOT NULL CHECK (size_bytes > 0),
    created_at         TEXT NOT NULL,
    updated_at         TEXT NOT NULL
);

-- +goose Down

DROP TABLE IF EXISTS reactive_promotions;
