-- 0032_hash_availability.sql — TS-5.1 (T6): own-traffic per-provider hash
-- availability observation store with TTL decay.
--
-- One row per (provider, info_hash): the most recent real observation of
-- whether that provider currently has that torrent hash cached, upserted on
-- every new real observation (submit-time CheckCached result, or a live
-- stream-time capability probe/genuine-failure outcome). This mirrors
-- migration 0023's item_file_probe_failures shape exactly (SF-05's
-- established negative-cache/TTL idiom, reused rather than duplicated):
-- upsert-on-observe, explicit expires_at, a hits counter for observability.
--
-- Never a crowd-sourced or imported signal (T6's own "measure, don't
-- gossip" rule) -- every row here traces back to this exact deployment's
-- own real provider traffic. No source/CDN URL, token, or content byte is
-- stored -- only the provider name, the torrent's own info hash, a cached
-- boolean, and timestamps.
--
-- Reading/consuming this table for search-time eligibility decisions is
-- TS-5.2's explicit scope, not this migration's -- this row only persists.

-- +goose Up

CREATE TABLE hash_availability (
    provider    TEXT NOT NULL,
    info_hash   TEXT NOT NULL,
    cached      INTEGER NOT NULL,
    source      TEXT NOT NULL,
    observed_at TEXT NOT NULL,
    expires_at  TEXT NOT NULL,
    hits        INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (provider, info_hash)
);

CREATE INDEX idx_hash_availability_expires_at ON hash_availability(expires_at);

-- +goose Down

DROP TABLE hash_availability;
