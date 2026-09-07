-- 0010_provider_credentials.sql — persisted, provider-discovered credentials
--
-- Some provider capabilities hand back credentials that DH itself discovers
-- rather than the operator configures (e.g. TorBox's NNTP News Server,
-- v9.0.0: GET /usenet/provider/account only reveals the real password on
-- first provisioning -- every call after that returns a masked "********"
-- placeholder, and the only way to see a real value again is to explicitly
-- reset it, which invalidates the previous one). Re-fetching at every boot
-- is therefore unsafe: it either breaks after the first restart (masked
-- value looks like a real password to naive code) or, if "fixed" by
-- resetting on every boot, silently rotates the credential out from under
-- any other consumer of that account every restart. This table is the fix:
-- fetch once, persist, reuse; only re-provision explicitly.
--
-- Same DB trust boundary as everything else DH stores (item source URIs,
-- etc.) -- not run through the sealed-secrets argon2id+AES-256-GCM store,
-- which is for operator-supplied static config unsealed at boot, not
-- DH-discovered dynamic credentials. credentials_json is a small JSON blob
-- (shape owned by the caller, e.g. host/port/tls/username/password) so this
-- table stays generic across providers rather than growing bespoke columns
-- per credential type.

-- +goose Up

CREATE TABLE IF NOT EXISTS provider_credentials (
    provider_name    TEXT NOT NULL PRIMARY KEY,
    credentials_json TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);

-- +goose Down

DROP TABLE IF EXISTS provider_credentials;
