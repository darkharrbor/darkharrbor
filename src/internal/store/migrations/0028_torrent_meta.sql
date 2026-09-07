-- 0028_torrent_meta.sql — TS-0.1: compact bencode-parsed TorrentMeta cache,
-- keyed by v1 infohash (the same identity item.InfoHash uses everywhere else
-- in DarkHarrbor). No source/CDN URL, tracker, or content bytes are
-- persisted here -- only structural metadata harvested from an
-- arr-uploaded .torrent file (T1: file list, piece length, piece/merkle
-- hash index). A missing row is always non-fatal to any consumer (T1's
-- own "absence is non-fatal" rule).

-- +goose Up

CREATE TABLE torrent_meta (
    info_hash       TEXT NOT NULL PRIMARY KEY,
    info_hash_v2    TEXT,
    name            TEXT NOT NULL,
    piece_length    INTEGER NOT NULL,
    meta_version    INTEGER NOT NULL,
    files_json      TEXT NOT NULL,
    piece_hashes_v1 BLOB,
    v2_merkle_roots TEXT,
    created_at      TEXT NOT NULL
);

-- +goose Down

DROP TABLE torrent_meta;
