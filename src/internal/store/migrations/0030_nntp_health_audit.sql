-- 0030_nntp_health_audit.sql — NS-6.1: rate-capped STAT health audit,
-- persisted per-item completeness/decayed state and a bounded dead-region
-- map (message-ID-scoped) for a future NS-4.2 idle-priority repair
-- consumer. This row itself never repairs and never triggers Arr
-- re-search (NS-6.2's job); it only measures and persists.

-- +goose Up

CREATE TABLE nntp_health_audit (
    item_id          TEXT PRIMARY KEY REFERENCES items (id) ON DELETE CASCADE,
    last_audited_at  TEXT NOT NULL,
    segments_sampled INTEGER NOT NULL,
    segments_present INTEGER NOT NULL,
    completeness     REAL NOT NULL,
    decayed          INTEGER NOT NULL DEFAULT 0,
    dead_regions     TEXT NOT NULL DEFAULT '[]',
    updated_at       TEXT NOT NULL
);

-- Drives NextHealthAuditItem's least-recently-audited selection (a Ready
-- NNTP item with no row here sorts first via the LEFT JOIN's NULL, then
-- ascending last_audited_at), giving the cadence-capped round-robin its
-- full-library-period property for free without a separate cursor table.
CREATE INDEX idx_nntp_health_audit_last_audited
    ON nntp_health_audit (last_audited_at);

-- +goose Down

DROP INDEX IF EXISTS idx_nntp_health_audit_last_audited;
DROP TABLE IF EXISTS nntp_health_audit;
