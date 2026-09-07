-- 0045_arr_instance_destinations.sql — RX-9.4 (RD-34 D8) destination capture.
--
-- The reactive router built its target list from `cfg.Arrs`, which is populated
-- ONLY when `HARRBOR_ARR_NAMES` is set. A deployment that discovered its arrs
-- through `darkharrbor setup` has them in `arr_instances` instead and typically
-- leaves that key unset, so the router resolved ZERO targets and parked every
-- entry no matter what was configured. RD-34 D8 rules ONE source of truth: the
-- router sources `arr_instances`.
--
-- To ADD content an instance needs a root folder and a quality profile, and
-- those are per-instance operator choices that belong beside the instance
-- rather than in a second configuration surface. Re-declaring name, URL and
-- API key in configuration is refused: it duplicates discovery and would put a
-- credential where `api_key_ref` deliberately keeps only a sealed-secrets key
-- NAME.
--
-- Existing rows take '' and 0, which read as NOT CAPTURED. An instance in that
-- state can still be routed to manually through the pending queue and the CLI;
-- automatic dispatch parks it. That preserves the shipping default OFF, so this
-- migration cannot by itself cause anything to be added to any arr.

-- +goose Up
ALTER TABLE arr_instances ADD COLUMN root_folder TEXT NOT NULL DEFAULT '';
ALTER TABLE arr_instances ADD COLUMN quality_profile INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE arr_instances DROP COLUMN quality_profile;
ALTER TABLE arr_instances DROP COLUMN root_folder;
