-- 0026_hls_sessions.sql — HR4.1: bounded transient playback-session graph
-- with stable opaque DH resource IDs (HR-D6). Durable so a resource ID a
-- player already holds still resolves after a DarkHarrbor restart without a
-- full manifest re-resolve (master plan §4.4, HR4.1).

-- +goose Up

CREATE TABLE hls_sessions (
    id          TEXT PRIMARY KEY,
    item_id     TEXT NOT NULL,
    file_id     TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    expires_at  TEXT NOT NULL
);

CREATE INDEX idx_hls_sessions_expires ON hls_sessions (expires_at);

CREATE TABLE hls_resources (
    id          TEXT PRIMARY KEY,
    session_id  TEXT NOT NULL REFERENCES hls_sessions (id) ON DELETE CASCADE,
    kind        TEXT NOT NULL CHECK (kind IN ('master', 'media', 'segment', 'map', 'key', 'subtitle')),
    ref_json    TEXT NOT NULL CHECK (length(ref_json) <= 4096),
    created_at  TEXT NOT NULL
);

CREATE INDEX idx_hls_resources_session ON hls_resources (session_id, created_at);

-- +goose Down

DROP INDEX IF EXISTS idx_hls_resources_session;
DROP TABLE IF EXISTS hls_resources;
DROP INDEX IF EXISTS idx_hls_sessions_expires;
DROP TABLE IF EXISTS hls_sessions;
