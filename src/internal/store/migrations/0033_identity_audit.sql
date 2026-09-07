-- 0033_identity_audit.sql — ID-03: retroactive library identity audit.
--
-- Re-runs ID-01's identitycheck.Verify() over already-Ready items (any
-- lane) on a slow background cadence and via an on-demand trigger, and
-- persists the most recent pass's verdicts so a legible possible-wrong-
-- content list can be reported. This row deliberately never enforces
-- (no suppression, no blacklist, no StateFailed) — measurement and
-- persistence only, mirroring migration 0030's nntp_health_audit shape
-- and its own "measurement-only" precedent, extended here to a
-- cross-lane (not NNTP-only) selection.
--
-- One row per item: verdicts_json is the JSON-encoded []identitycheck.Verdict
-- from the most recent pass ('[]' means the last pass found no mismatch —
-- itself a meaningful, reportable "clean" result, not "never audited",
-- which is instead the absence of any row here).

-- +goose Up

CREATE TABLE identity_audit (
    item_id         TEXT PRIMARY KEY REFERENCES items (id) ON DELETE CASCADE,
    last_audited_at TEXT NOT NULL,
    verdicts_json   TEXT NOT NULL DEFAULT '[]',
    updated_at      TEXT NOT NULL
);

-- Drives NextIdentityAuditItem's least-recently-audited selection (a Ready
-- item with no row here sorts first, then ascending last_audited_at),
-- giving the cadence-capped round-robin its full-library-period property
-- for free, mirroring idx_nntp_health_audit_last_audited exactly.
CREATE INDEX idx_identity_audit_last_audited
    ON identity_audit (last_audited_at);

-- +goose Down

DROP INDEX IF EXISTS idx_identity_audit_last_audited;
DROP TABLE IF EXISTS identity_audit;
