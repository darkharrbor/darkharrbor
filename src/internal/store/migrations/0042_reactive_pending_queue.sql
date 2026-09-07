-- 0042_reactive_pending_queue.sql — RX-4.2 single durable decision queue.

-- +goose Up

CREATE TABLE reactive_pending (
    representation_id TEXT PRIMARY KEY REFERENCES playback_proposals(representation_id) ON DELETE CASCADE,
    item_id            TEXT NOT NULL,
    reason             TEXT NOT NULL CHECK (reason IN ('supervised_review','weak_evidence','positive_mismatch','routing_unresolved','routing_ambiguous')),
    state              TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','resolved')),
    exit_action        TEXT NOT NULL DEFAULT '' CHECK (exit_action IN ('','approve','acknowledge','assign','jellyfin_only','remove','undo')),
    arr_name           TEXT NOT NULL DEFAULT '',
    created_at         TEXT NOT NULL,
    updated_at         TEXT NOT NULL
);
CREATE INDEX idx_reactive_pending_state
    ON reactive_pending(state, created_at, representation_id);

CREATE TABLE reactive_arr_assignments (
    identity_kind     TEXT NOT NULL CHECK (identity_kind IN ('series','movie')),
    identity_provider TEXT NOT NULL CHECK (identity_provider IN ('tvdb','imdb','tmdb')),
    identity_value    TEXT NOT NULL,
    arr_name          TEXT NOT NULL,
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL,
    PRIMARY KEY (identity_kind, identity_provider, identity_value)
);

-- +goose StatementBegin
CREATE TRIGGER reactive_pending_after_proposal
AFTER INSERT ON playback_proposals
BEGIN
    INSERT INTO reactive_pending
        (representation_id,item_id,reason,created_at,updated_at)
    VALUES (NEW.representation_id,NEW.item_id,'supervised_review',NEW.created_at,NEW.created_at);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER reactive_pending_after_park
AFTER UPDATE OF state ON reactive_commits
WHEN NEW.state='parked'
BEGIN
    INSERT INTO reactive_pending
        (representation_id,item_id,reason,created_at,updated_at)
    SELECT NEW.representation_id,NEW.item_id,'positive_mismatch',NEW.created_at,NEW.updated_at
    WHERE EXISTS (SELECT 1 FROM playback_proposals p WHERE p.representation_id=NEW.representation_id)
    ON CONFLICT(representation_id) DO UPDATE SET
        reason='positive_mismatch',state='pending',exit_action='',arr_name='',updated_at=excluded.updated_at;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER reactive_pending_after_weak_commit
AFTER UPDATE OF state ON reactive_commits
WHEN NEW.state='committed' AND NEW.review_required=1
BEGIN
    INSERT INTO reactive_pending
        (representation_id,item_id,reason,created_at,updated_at)
    SELECT NEW.representation_id,NEW.item_id,'weak_evidence',NEW.created_at,NEW.updated_at
    WHERE EXISTS (SELECT 1 FROM playback_proposals p WHERE p.representation_id=NEW.representation_id)
    ON CONFLICT(representation_id) DO UPDATE SET
        reason='weak_evidence',state='pending',exit_action='',arr_name='',updated_at=excluded.updated_at;
END;
-- +goose StatementEnd

INSERT INTO reactive_pending (representation_id,item_id,reason,created_at,updated_at)
SELECT p.representation_id,p.item_id,'supervised_review',p.created_at,p.created_at
FROM playback_proposals p
LEFT JOIN reactive_commits c ON c.representation_id=p.representation_id
WHERE c.representation_id IS NULL;

INSERT OR IGNORE INTO reactive_pending (representation_id,item_id,reason,created_at,updated_at)
SELECT c.representation_id,c.item_id,'positive_mismatch',c.created_at,c.updated_at
FROM reactive_commits c WHERE c.state='parked';

INSERT OR IGNORE INTO reactive_pending (representation_id,item_id,reason,created_at,updated_at)
SELECT c.representation_id,c.item_id,'weak_evidence',c.created_at,c.updated_at
FROM reactive_commits c WHERE c.state='committed' AND c.review_required=1;

-- +goose Down

DROP TRIGGER IF EXISTS reactive_pending_after_weak_commit;
DROP TRIGGER IF EXISTS reactive_pending_after_park;
DROP TRIGGER IF EXISTS reactive_pending_after_proposal;
DROP TABLE IF EXISTS reactive_arr_assignments;
DROP INDEX IF EXISTS idx_reactive_pending_state;
DROP TABLE IF EXISTS reactive_pending;
