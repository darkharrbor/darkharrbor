-- 0040_reactive_observability.sql — RX-8.1 bounded reactive telemetry.

-- +goose Up

CREATE TABLE reactive_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    kind        TEXT NOT NULL CHECK (kind IN ('commit','park','undo','promotion')),
    mode        TEXT NOT NULL DEFAULT '' CHECK (mode IN ('','auto','supervised')),
    outcome     TEXT NOT NULL DEFAULT '' CHECK (outcome IN ('','strong_evidence','weak_evidence','positive_mismatch')),
    bytes       INTEGER NOT NULL DEFAULT 0 CHECK (bytes >= 0),
    occurred_at TEXT NOT NULL
);
CREATE INDEX idx_reactive_events_time ON reactive_events(occurred_at, id);

INSERT INTO reactive_events (kind, mode, outcome, occurred_at)
SELECT 'park', mode, 'positive_mismatch', updated_at
FROM reactive_commits WHERE state='parked';
INSERT INTO reactive_events (kind, mode, outcome, occurred_at)
SELECT 'commit', mode,
       CASE review_required WHEN 1 THEN 'weak_evidence' ELSE 'strong_evidence' END,
       updated_at
FROM reactive_commits WHERE state IN ('committed','undoing','undone');
INSERT INTO reactive_events (kind, occurred_at)
SELECT 'undo', updated_at FROM reactive_commits WHERE state='undone';

CREATE TABLE reactive_promotion_headroom (
    singleton      INTEGER PRIMARY KEY CHECK (singleton=1),
    headroom_bytes INTEGER NOT NULL,
    observed_at    TEXT NOT NULL
);

-- +goose Down

DROP TABLE IF EXISTS reactive_promotion_headroom;
DROP INDEX IF EXISTS idx_reactive_events_time;
DROP TABLE IF EXISTS reactive_events;
