-- 0011_item_file_probes.sql — per-file grab-time ffprobe-full results
--
-- item.ProbeJSON only ever stored one probe per item (the first file of a
-- multi-file torrent/season-pack item), and nothing consumed it except DH's
-- own deprecated local stub-generation path. The relay endpoint Sonarr's
-- shim calls (POST /api/v1/probe) never read it at all -- every import-time
-- probe for every episode was a fresh live ffprobe-full network fetch
-- against DH's own /stream endpoint (which itself proxies to the TorBox
-- CDN), regardless of whether DH already had the data.
--
-- Found live 2026-07-02: a 20-episode season pack (American Dad S05)
-- produced 20 near-simultaneous live CDN probes at Sonarr import time, all
-- individually triggered by the arr's own per-episode ffprobe-wrapper call,
-- none served from cache -- the intended design (probe once at grab time,
-- serve from storage until actual Jellyfin first-play) was only ever half
-- built.
--
-- args_key is part of the primary key rather than a single stored probe per
-- file: a cached result is only ever safe to serve back for the EXACT same
-- ffprobe args it was produced with (duration format, e.g. -sexagesimal,
-- and probesize/analyzeduration, differ by arg set and change the output
-- shape). A shim retry with different args correctly misses cache and falls
-- back to a live probe rather than risk serving output in the wrong shape.
-- See internal/probe/ffprobefull.go (CanonicalArgs, ArgsKey).

-- +goose Up

CREATE TABLE IF NOT EXISTS item_file_probes (
    item_id    TEXT NOT NULL,
    file_id    TEXT NOT NULL,
    args_key   TEXT NOT NULL,
    probe_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (item_id, file_id, args_key)
);

-- +goose Down

DROP TABLE IF EXISTS item_file_probes;
