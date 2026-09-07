package store

// blobs.go — SQLite-authoritative .strm content DAO and filesystem
//
// The DB is the authoritative source of .strm URLs (migration 0007).
// Item.StrmPath is a materialization receipt: the materializer reconciles
// DB→disk on write and on startup, restoring deleted or corrupted on-disk
// copies sha-verified from the DB. Materialization-to-disk is mandatory because
// the arr imports the .strm from the filesystem.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/sidecar"
)

// StrmBlob is one authoritative .strm record: the content of a .strm file
// is exactly one URL — the URL is stored and the file is materialized.
type StrmBlob struct {
	ItemID    string
	FileIndex int
	RelPath   string
	SHA256    string // sha256 of the materialized file content (the URL bytes)
	URL       string
}

// SHA256Hex returns the hex sha256 of b (exported for write-path callers
// that persist alongside their own atomic disk write).
func SHA256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// UpsertStrmBlob stores or replaces the authoritative .strm URL for
// (itemID, fileIndex).
func (s *Store) UpsertStrmBlob(ctx context.Context, b StrmBlob) error {
	if b.SHA256 == "" {
		b.SHA256 = SHA256Hex([]byte(b.URL))
	}
	_, err := s.execWrite(ctx, `
        INSERT INTO strm_blobs (item_id, file_index, rel_path, sha256, url, created_at)
        VALUES (?,?,?,?,?,?)
        ON CONFLICT(item_id, file_index) DO UPDATE SET
            rel_path=excluded.rel_path, sha256=excluded.sha256,
            url=excluded.url, created_at=excluded.created_at`,
		b.ItemID, b.FileIndex, b.RelPath, b.SHA256, b.URL, formatTime(s.now()))
	if err != nil {
		return fmt.Errorf("upsert strm blob: %w", err)
	}
	return nil
}

// UpsertStrmBlobAt is the sink-shaped form (output.StrmBlobSink): it persists
// the .strm URL for (itemID, fileIndex) at relPath, computing the sha
// internally from the URL bytes.
func (s *Store) UpsertStrmBlobAt(ctx context.Context, itemID string, fileIndex int, relPath, url string) error {
	return s.UpsertStrmBlob(ctx, StrmBlob{ItemID: itemID, FileIndex: fileIndex, RelPath: relPath, URL: url})
}

// GetStrmBlobs returns all strm blobs for an item ordered by file index.
func (s *Store) GetStrmBlobs(ctx context.Context, itemID string) ([]StrmBlob, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT item_id, file_index, rel_path, sha256, url
        FROM strm_blobs WHERE item_id=? ORDER BY file_index ASC`, itemID)
	if err != nil {
		return nil, fmt.Errorf("get strm blobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []StrmBlob
	for rows.Next() {
		var b StrmBlob
		if err := rows.Scan(&b.ItemID, &b.FileIndex, &b.RelPath, &b.SHA256, &b.URL); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// strmBlobItemIDs returns the distinct item ids with .strm blobs whose item is
// not in a terminal state and whose download-side .strm has not been
// consumed by the Arr (COR-3 fix).
func (s *Store) strmBlobItemIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT DISTINCT b.item_id FROM strm_blobs b
        JOIN items i ON i.id = b.item_id
        WHERE i.state NOT IN ('failed','removed')
        AND i.download_consumed = 0
        ORDER BY b.item_id`)
	if err != nil {
		return nil, fmt.Errorf("strm blob item ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// MaterializeResult summarizes one reconcile pass.
type MaterializeResult struct {
	Checked  int
	Restored int
	Errors   int
}

// safeJoinUnder joins rel under root and rejects any result that escapes
// root (path traversal guard for DB-sourced rel_path values).
func safeJoinUnder(root, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", fmt.Errorf("blob rel_path invalid: %q", rel)
	}
	joined := filepath.Join(root, rel)
	cleanRoot := filepath.Clean(root)
	if joined != cleanRoot && !strings.HasPrefix(joined, cleanRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("blob rel_path escapes root: %q", rel)
	}
	return joined, nil
}

// MaterializeItem reconciles one item's .strm blobs DB→disk: a missing or
// sha-divergent on-disk copy is atomically rewritten from the DB.
func (s *Store) MaterializeItem(ctx context.Context, dataRoot, itemID string) (MaterializeResult, error) {
	var res MaterializeResult
	strms, err := s.GetStrmBlobs(ctx, itemID)
	if err != nil {
		return res, err
	}
	for _, b := range strms {
		res.Checked++
		dest, err := safeJoinUnder(dataRoot, b.RelPath)
		if err != nil {
			res.Errors++
			continue
		}
		current, rerr := os.ReadFile(dest)
		if rerr == nil && SHA256Hex(current) == b.SHA256 {
			continue // intact
		}
		if werr := atomicWriteBlob(dest, []byte(b.URL), 0o644); werr != nil {
			res.Errors++
			continue
		}
		res.Restored++
	}
	if len(strms) > 0 {
		resources, listErr := s.listItemSidecarResources(ctx, itemID)
		if listErr != nil {
			res.Errors++
		} else {
			baseDir, baseErr := safeJoinUnder(dataRoot, filepath.Dir(strms[0].RelPath))
			if baseErr != nil {
				res.Errors++
			} else {
				for _, resource := range resources {
					if resource.Kind != sidecar.KindSidecar ||
						!strings.HasSuffix(strings.ToLower(resource.Filename), ".nfo") ||
						strings.EqualFold(resource.Filename, "tvshow.nfo") ||
						filepath.Base(resource.Filename) != resource.Filename {
						continue
					}
					dest, joinErr := safeJoinUnder(baseDir, resource.Filename)
					if joinErr != nil {
						res.Errors++
						continue
					}
					current, readErr := os.ReadFile(dest)
					if readErr == nil && SHA256Hex(current) == SHA256Hex(resource.Bytes) {
						continue
					}
					if writeErr := atomicWriteBlob(dest, resource.Bytes, 0o644); writeErr != nil {
						res.Errors++
					} else {
						res.Restored++
					}
				}
			}
		}
	}
	return res, nil
}

// ReconcileAll reconciles every non-terminal item's .strm blobs DB→disk.
// Called at startup and from the periodic reconcile loop so a deleted or
// garbled on-disk .strm is restored while the daemon runs.
func (s *Store) ReconcileAll(ctx context.Context, dataRoot string) (MaterializeResult, error) {
	var total MaterializeResult
	ids, err := s.strmBlobItemIDs(ctx)
	if err != nil {
		return total, err
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		r, err := s.MaterializeItem(ctx, dataRoot, id)
		total.Checked += r.Checked
		total.Restored += r.Restored
		total.Errors += r.Errors
		if err != nil {
			total.Errors++
		}
	}
	return total, nil
}

// SweepFinding is one legible, read-only consistency-sweep observation:
// drift ConsistencySweep deliberately does not heal, either because healing
// it would require fabricating content that does not exist in the DB, or
// because acting on it would risk regressing COR-3's ownership rule.
type SweepFinding struct {
	ItemID string
	Kind   string // "no_output" | "consumed_present" | "orphan_blob"
	Detail string
}

// SweepResult is one ConsistencySweep pass: the exact COR-3-safe heal counts
// already owned by ReconcileAll/MaterializeItem, plus legible-only findings
// outside that safe boundary.
type SweepResult struct {
	MaterializeResult
	Findings []SweepFinding
}

// listReadyItemsMissingOutput returns Ready items with neither a strm_path
// receipt nor any strm_blobs row -- state claims playable output exists but
// the DB disagrees. This can only be reported: there is no authoritative
// content to (re)materialize from, so fabricating a .strm would violate the
// "DH URLs only ever come from real resolves" invariant.
func (s *Store) listReadyItemsMissingOutput(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT id FROM items
        WHERE state = 'ready'
          AND (strm_path IS NULL OR strm_path = '')
          AND NOT EXISTS (SELECT 1 FROM strm_blobs b WHERE b.item_id = items.id)
        ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list ready items missing output: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// listConsumedPresentItemIDs returns Ready, download-consumed items whose
// strm_blobs rows still exist. This is informational only -- COR-3
// deliberately excludes consumed items from disk healing (strmBlobItemIDs
// above), and their DB rows persisting after Arr's MOVE is expected, not
// drift. Reported so an operator can distinguish "expected retained
// metadata" from an actual gap; never acted upon.
func (s *Store) listConsumedPresentItemIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT DISTINCT i.id FROM items i
        JOIN strm_blobs b ON b.item_id = i.id
        WHERE i.state = 'ready' AND i.download_consumed = 1
        ORDER BY i.id`)
	if err != nil {
		return nil, fmt.Errorf("list consumed-present items: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// listOrphanBlobItemIDs returns distinct item_ids present in strm_blobs with
// no corresponding items row. strm_blobs has no FK (a SQLite non-FK satellite
// table, deliberately cleaned up alongside items in the delete-items
// transaction -- see store.go), so this is a defensive DB-resolves-cleanly
// check, not an expected finding under current shipped delete paths.
func (s *Store) listOrphanBlobItemIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT DISTINCT b.item_id FROM strm_blobs b
        WHERE NOT EXISTS (SELECT 1 FROM items i WHERE i.id = b.item_id)
        ORDER BY b.item_id`)
	if err != nil {
		return nil, fmt.Errorf("list orphan blob item ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ConsistencySweep is the TS-0.5 (T8c) janitor pass: "every Ready item's
// strm path exists and its DB row resolves; drift is healed or made
// legible." It does not add a second heal path -- the heal half delegates
// to the exact ReconcileAll/MaterializeItem COR-3-safe boundary (unconsumed,
// non-terminal items only) already established by C0.1. Everything outside
// that boundary (no persisted output at all, consumed items' retained rows,
// and DB rows that fail to resolve) is reported as a SweepFinding and never
// written, restored, or deleted -- reporting cannot regress COR-3 because it
// makes no disk or DB mutation of its own beyond the pre-existing heal call.
func (s *Store) ConsistencySweep(ctx context.Context, dataRoot string) (SweepResult, error) {
	var res SweepResult

	heal, err := s.ReconcileAll(ctx, dataRoot)
	if err != nil {
		return res, err
	}
	res.MaterializeResult = heal

	noOutput, err := s.listReadyItemsMissingOutput(ctx)
	if err != nil {
		return res, err
	}
	for _, id := range noOutput {
		res.Findings = append(res.Findings, SweepFinding{
			ItemID: id, Kind: "no_output",
			Detail: "state=ready but no strm_path and no strm_blobs rows",
		})
	}

	consumedPresent, err := s.listConsumedPresentItemIDs(ctx)
	if err != nil {
		return res, err
	}
	for _, id := range consumedPresent {
		res.Findings = append(res.Findings, SweepFinding{
			ItemID: id, Kind: "consumed_present",
			Detail: "download_consumed=1 but strm_blobs rows still present (informational, not drift)",
		})
	}

	orphans, err := s.listOrphanBlobItemIDs(ctx)
	if err != nil {
		return res, err
	}
	for _, id := range orphans {
		res.Findings = append(res.Findings, SweepFinding{
			ItemID: id, Kind: "orphan_blob",
			Detail: "strm_blobs row(s) with no corresponding items row",
		})
	}

	return res, nil
}

// atomicWriteBlob writes data via same-directory temp + rename so import-
// and scan-side readers never observe a partial .strm.
func atomicWriteBlob(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("store: blob mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("store: blob temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("store: blob write %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("store: blob chmod %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("store: blob close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("store: blob rename %s to %s: %w", tmpName, path, err)
	}
	keep = true
	return nil
}
