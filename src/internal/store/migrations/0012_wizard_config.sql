-- 0012_wizard_config.sql — wizard/bootstrap persistent state
--
-- app_config: key-value store for wizard answers (non-secret), per-step
-- completion markers (wizard re-run idempotency), and cached provider_caps
-- with TTL. Values are plain text or small JSON blobs.
--
-- arr_instances: discovered arr instances. api_key_ref is the sealed-secrets
-- key name (e.g. "HARRBOR_ARR_SONARR_APIKEY") rather than the raw key so the
-- value remains in the sealed store — the wizard writes both the sealed file
-- and this reference. Populated by S8; read by S9/S10/S11 and at boot for
-- the failed-prune notify feature.

-- +goose Up

CREATE TABLE IF NOT EXISTS app_config (
    key        TEXT NOT NULL PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE IF NOT EXISTS arr_instances (
    name         TEXT NOT NULL PRIMARY KEY,  -- operator label, e.g. "sonarr-modern"
    url          TEXT NOT NULL,              -- base URL, e.g. "http://sonarr-modern:8989"
    app_type     TEXT NOT NULL,              -- "sonarr" | "radarr" | "prowlarr"
    api_key_ref  TEXT NOT NULL,              -- sealed-secrets key name for the API key
    discovered_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- +goose Down

DROP TABLE IF EXISTS arr_instances;
DROP TABLE IF EXISTS app_config;
