-- 0029_grab_provider_identities.sql — ID-02 restart-safe search-to-grab context.
-- +goose Up
CREATE TABLE grab_provider_identities (
    transport_key TEXT PRIMARY KEY,
    identity_json TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE INDEX idx_grab_provider_identities_updated
    ON grab_provider_identities (updated_at);

-- +goose Down
DROP INDEX IF EXISTS idx_grab_provider_identities_updated;
DROP TABLE IF EXISTS grab_provider_identities;
