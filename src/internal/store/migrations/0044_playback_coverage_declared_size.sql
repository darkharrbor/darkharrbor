-- 0044_playback_coverage_declared_size.sql — RX-9.6 (RD-32) coverage reset.
--
-- RD-30 D5 collapses two releases of one title into a single identity-keyed
-- representation, which is correct: the operator cares about the episode, not
-- the encode. But COVERAGE IS MEASURED IN BYTES and byte offsets are
-- RELEASE-SPECIFIC. Found in live operation 2026-08-18: one identity
-- accumulated spans reaching byte 16,725,425,753 from a 4K remux and then
-- shared that offset space with a much smaller release.
--
-- The threshold is delivered/total against the declared size of the
-- representation BEING PLAYED, so retained coverage from a larger release can
-- present a ratio above 1 and commit immediately a file the viewer has barely
-- started. Recording the declared size lets the merge DISCARD coverage when it
-- changes, keeping exactly one coverage set per identity.
--
-- Existing rows take 0, which reads as "unknown" and is adopted by the first
-- observation carrying a real size rather than being treated as a mismatch.

-- +goose Up

ALTER TABLE playback_coverage ADD COLUMN declared_size INTEGER NOT NULL DEFAULT 0;

-- +goose Down

ALTER TABLE playback_coverage DROP COLUMN declared_size;
