-- 0031_torrent_meta_piece_layers.sql — TS-3.4 (T12): persist BEP52 v2
-- per-piece hash material ("piece layers") harvested at the same TS-0.1
-- parse time as the existing v2_merkle_roots column, so a stream request
-- served long after grab time -- a separate process lifetime, potentially
-- after a restart -- can still opportunistically verify v2 pieces without
-- re-parsing the original .torrent (which DarkHarrbor never retains).
--
-- Nullable, additive-only column on the existing torrent_meta table (TS-0.1,
-- migration 0028). A NULL/absent value is always non-fatal per T1's
-- "absence is non-fatal" rule and TS-3.4's own opportunistic-abstention
-- design -- an old row from before this migration simply has no v2
-- piece-level verification material, exactly like a v1-only or magnet-only
-- torrent has today. No source/CDN URL, tracker, or content bytes are
-- stored here -- only structural per-piece SHA-256 hashes, same trust class
-- as the existing piece_hashes_v1/v2_merkle_roots columns.

-- +goose Up
ALTER TABLE torrent_meta ADD COLUMN piece_layers_json TEXT;

-- +goose Down
ALTER TABLE torrent_meta DROP COLUMN piece_layers_json;
