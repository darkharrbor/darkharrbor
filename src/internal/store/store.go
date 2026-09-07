package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/util"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// gooseMu serialises RunMigrationsFS calls so that the global
// goose.SetBaseFS / goose.SetBaseFS(nil) pair cannot race across
// parallel test packages.
var gooseMu sync.Mutex

// Store wraps a SQLite database and provides all persistence operations
// for Dark Harrbor items, events, sessions, and the governor log.
type Store struct {
	db  *sql.DB
	now func() time.Time
	// credentialSealKey seals provider_credentials at rest (SEC-01). Empty
	// means sealing is off and rows are written as historical plaintext.
	// Set via SetCredentialSealKey, never at construction, because the
	// sealed-secrets store is loaded by config and a Store is also built in
	// contexts that have no config at all.
	credentialSealKey util.Redacted
}

const (
	sqlitePrimaryCodeMask    = 0xff
	sqliteBusyCode           = 5
	sqliteLockedCode         = 6
	sqliteBusyRetryAttempts  = 6
	sqliteBusyRetryBaseDelay = 25 * time.Millisecond
	sqliteBusyRetryMaxDelay  = 250 * time.Millisecond
)

const itemColumns = `
    id, public_id, source_type, client_kind, category, state, submission_key,
    remote_id, queued_id, info_hash, display_name, source_uri,
    cached, strm_path, sidecar_path, probe_json, file_list,
    total_size, last_played_at,
    error_message, retry_count, next_run_at,
    metadata_json, claimed_by, claimed_at, created_at, updated_at,
    rar_manifest, provider, resolve_key, download_consumed
`

// Open opens (or creates) the SQLite database at path, applies WAL mode and
// the given busy timeout, and returns a ready *sql.DB.
func Open(ctx context.Context, path string, busyTimeout time.Duration) (*sql.DB, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, fmt.Errorf("ensure sqlite parent dir: %w", err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		fmt.Sprintf("PRAGMA busy_timeout = %d;", busyTimeout.Milliseconds()),
		"PRAGMA foreign_keys = ON;",
		"PRAGMA synchronous = NORMAL;",
	}
	for _, pragma := range pragmas {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("apply pragma %q: %w", pragma, err)
		}
	}

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	return db, nil
}

// OpenReadOnly opens an existing SQLite database without changing journal
// mode, creating parent paths, or permitting writes. It is for audit commands,
// not daemon startup.
func OpenReadOnly(ctx context.Context, path string, busyTimeout time.Duration) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve sqlite path: %w", err)
	}
	dsn := (&url.URL{Scheme: "file", Path: absolute, RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, pragma := range []string{
		fmt.Sprintf("PRAGMA busy_timeout = %d;", busyTimeout.Milliseconds()),
		"PRAGMA foreign_keys = ON;",
		"PRAGMA query_only = ON;",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("apply read-only pragma %q: %w", pragma, err)
		}
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite read-only: %w", err)
	}
	return db, nil
}

// RunMigrationsFS runs goose migrations from an embedded FS.
func RunMigrationsFS(db *sql.DB, migrationFS fs.FS) error {
	gooseMu.Lock()
	defer gooseMu.Unlock()

	goose.SetBaseFS(migrationFS)
	defer goose.SetBaseFS(nil)
	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}
	if err := goose.Up(db, "migrations"); err != nil {
		return fmt.Errorf("run embedded migrations: %w", err)
	}
	return nil
}

// New wraps an open *sql.DB in a Store.
func New(db *sql.DB) *Store {
	return &Store{db: db, now: func() time.Time { return time.Now().UTC() }}
}

// SetClock overrides the clock used by the store. For testing only.
func (s *Store) SetClock(fn func() time.Time) { s.now = fn }

// DB returns the underlying *sql.DB for use by packages that need raw access.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) execWrite(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return retrySQLiteBusy(ctx, func() (sql.Result, error) {
		return s.db.ExecContext(ctx, query, args...)
	})
}

// execWriteAffected executes a write query and returns RowsAffected.
func (s *Store) execWriteAffected(ctx context.Context, query string, args ...any) (int64, error) {
	result, err := s.execWrite(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return n, nil
}

// ─── Item CRUD ───────────────────────────────────────────────────────────────

func (s *Store) CreateItem(ctx context.Context, item *Item) error {
	metaJSON, err := json.Marshal(item.Metadata)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	_, err = s.execWrite(ctx, `
        INSERT INTO items (
            id, public_id, source_type, client_kind, category, state, submission_key,
            remote_id, queued_id, info_hash, display_name, source_uri,
            cached, strm_path, sidecar_path, probe_json, file_list,
            total_size, last_played_at,
            error_message, retry_count, next_run_at,
            metadata_json, created_at, updated_at,
            rar_manifest, provider, resolve_key, download_consumed
        ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		item.ID, item.PublicID,
		string(item.SourceType), string(item.ClientKind),
		item.Category, string(item.State), item.SubmissionKey,
		nullableString(item.RemoteID),
		nullableString(item.QueuedID),
		nullableString(item.InfoHash),
		item.DisplayName,
		nullableString(item.SourceURI),
		boolToInt(item.Cached),
		nullableString(item.StrmPath),
		nullableString(item.SidecarPath),
		nullableString(item.ProbeJSON),
		nullableString(item.FileList),
		item.TotalSize,
		nullableTime(item.LastPlayedAt),
		nullableString(item.ErrorMessage),
		item.RetryCount,
		nullableTime(item.NextRunAt),
		string(metaJSON),
		formatTime(item.CreatedAt),
		formatTime(item.UpdatedAt),
		nullableString(item.RarManifest),
		nullableString(item.Provider),
		nullableString(item.ResolveKey),
		boolToInt(item.DownloadConsumed),
	)
	if err != nil {
		return fmt.Errorf("insert item: %w", err)
	}
	return s.AppendEvent(ctx, item.ID, nil, &item.State, "item created")
}

func (s *Store) UpdateItem(ctx context.Context, item *Item) error {
	metaJSON, err := json.Marshal(item.Metadata)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	res, err := s.execWrite(ctx, `
		UPDATE items SET
		    source_type=?, client_kind=?, category=?, state=?, submission_key=?,
		    remote_id=?, queued_id=?, info_hash=?, display_name=?, source_uri=?,
		    cached=?, strm_path=?, sidecar_path=?, probe_json=?, file_list=?,
		    total_size=?, last_played_at=?,
		    error_message=?, retry_count=?, next_run_at=?,
		    metadata_json=?,
		    claimed_by=NULL, claimed_at=NULL,
		    updated_at=?,
		    rar_manifest=?, provider=?, resolve_key=?, download_consumed=?
		WHERE id=? AND state=?`,
		string(item.SourceType), string(item.ClientKind),
		item.Category, string(item.State), item.SubmissionKey,
		nullableString(item.RemoteID),
		nullableString(item.QueuedID),
		nullableString(item.InfoHash),
		item.DisplayName,
		nullableString(item.SourceURI),
		boolToInt(item.Cached),
		nullableString(item.StrmPath),
		nullableString(item.SidecarPath),
		nullableString(item.ProbeJSON),
		nullableString(item.FileList),
		item.TotalSize,
		nullableTime(item.LastPlayedAt),
		nullableString(item.ErrorMessage),
		item.RetryCount,
		nullableTime(item.NextRunAt),
		string(metaJSON),
		formatTime(item.UpdatedAt),
		nullableString(item.RarManifest),
		nullableString(item.Provider),
		nullableString(item.ResolveKey),
		boolToInt(item.DownloadConsumed),
		item.ID,
		string(item.State),
	)
	if err != nil {
		return fmt.Errorf("update item: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update item rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("state update conflict: item %s was not in expected state %q", item.ID, item.State)
	}
	return nil
}

// UpdateLastPlayedAt updates only playback timestamps, preserving all other
// item fields and the same optimistic state guard used by UpdateItem.
func (s *Store) UpdateLastPlayedAt(ctx context.Context, itemID string, expectedState ItemState, at time.Time) error {
	n, err := s.execWriteAffected(ctx, `
		UPDATE items SET last_played_at=?, updated_at=?
		WHERE id=? AND state=?`,
		formatTime(at), formatTime(at), itemID, string(expectedState))
	if err != nil {
		return fmt.Errorf("update last_played_at for item %s: %w", itemID, err)
	}
	if n == 0 {
		return fmt.Errorf("last_played_at update conflict: item %s was not in expected state %q", itemID, expectedState)
	}
	return nil
}

func (s *Store) UpdateItemState(ctx context.Context, item *Item, next ItemState, message string) error {
	prev := item.State
	if prev == StateRemoved && next != prev {
		return fmt.Errorf("state transition conflict: item %s is already terminal in state %q", item.ID, prev)
	}
	if prev == StateFailed && next != prev && next != StateRemoved {
		return fmt.Errorf("state transition conflict: item %s is already terminal in state %q", item.ID, prev)
	}

	metaJSON, err := json.Marshal(item.Metadata)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	previousUpdatedAt := item.UpdatedAt
	item.State = next
	item.UpdatedAt = s.now()
	_, err = retrySQLiteBusy(ctx, func() (struct{}, error) {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return struct{}{}, fmt.Errorf("begin item state tx: %w", err)
		}
		committed := false
		defer func() {
			if !committed {
				_ = tx.Rollback()
			}
		}()

		res, err := tx.ExecContext(ctx, `
			UPDATE items SET
				source_type=?, client_kind=?, category=?, state=?, submission_key=?,
				remote_id=?, queued_id=?, info_hash=?, display_name=?, source_uri=?,
				cached=?, strm_path=?, sidecar_path=?, probe_json=?, file_list=?,
				total_size=?, last_played_at=?,
				error_message=?, retry_count=?, next_run_at=?,
				metadata_json=?,
				claimed_by=NULL, claimed_at=NULL,
				updated_at=?,
				rar_manifest=?, provider=?, resolve_key=?, download_consumed=?
			WHERE id=? AND state=?`,
			string(item.SourceType), string(item.ClientKind),
			item.Category, string(item.State), item.SubmissionKey,
			nullableString(item.RemoteID),
			nullableString(item.QueuedID),
			nullableString(item.InfoHash),
			item.DisplayName,
			nullableString(item.SourceURI),
			boolToInt(item.Cached),
			nullableString(item.StrmPath),
			nullableString(item.SidecarPath),
			nullableString(item.ProbeJSON),
			nullableString(item.FileList),
			item.TotalSize,
			nullableTime(item.LastPlayedAt),
			nullableString(item.ErrorMessage),
			item.RetryCount,
			nullableTime(item.NextRunAt),
			string(metaJSON),
			formatTime(item.UpdatedAt),
			nullableString(item.RarManifest),
			nullableString(item.Provider),
			nullableString(item.ResolveKey),
			boolToInt(item.DownloadConsumed),
			item.ID,
			string(prev),
		)
		if err != nil {
			return struct{}{}, fmt.Errorf("update item state: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return struct{}{}, fmt.Errorf("update item state rows affected: %w", err)
		}
		if n == 0 {
			return struct{}{}, fmt.Errorf("state transition conflict: item %s was not in expected state %q", item.ID, prev)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO item_events (item_id, from_state, to_state, message, created_at)
			VALUES (?,?,?,?,?)`,
			item.ID,
			nullableItemState(&prev),
			nullableItemState(&next),
			message,
			formatTime(item.UpdatedAt),
		); err != nil {
			return struct{}{}, fmt.Errorf("append event: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return struct{}{}, fmt.Errorf("commit item state tx: %w", err)
		}
		committed = true
		return struct{}{}, nil
	})
	if err != nil {
		item.State = prev
		item.UpdatedAt = previousUpdatedAt
		return err
	}
	return nil
}

func (s *Store) AppendEvent(ctx context.Context, itemID string, from, to *ItemState, message string) error {
	_, err := s.execWrite(ctx, `
        INSERT INTO item_events (item_id, from_state, to_state, message, created_at)
        VALUES (?,?,?,?,?)`,
		itemID,
		nullableItemState(from),
		nullableItemState(to),
		message,
		formatTime(s.now()),
	)
	if err != nil {
		return fmt.Errorf("append event: %w", err)
	}
	return nil
}

func (s *Store) GetItemByID(ctx context.Context, id string) (*Item, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+itemColumns+` FROM items WHERE id=? LIMIT 1`, id)
	item, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return item, err
}

func (s *Store) GetItemByPublicID(ctx context.Context, publicID string) (*Item, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+itemColumns+` FROM items WHERE public_id=? LIMIT 1`, publicID)
	item, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return item, err
}

// ListReadyTorrentsBefore returns torrent items in StateReady whose
// updated_at is before the given cutoff (for auto-removal after import).
func (s *Store) ListReadyTorrentsBefore(ctx context.Context, cutoff time.Time, limit int) ([]*Item, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+itemColumns+` FROM items
		 WHERE state = 'ready'
		   AND source_type = 'torrent'
		   AND updated_at < ?
		 ORDER BY updated_at ASC LIMIT ?`,
		cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("list ready torrents before: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanItems(rows)
}

// ListNeverPlayedUncachedBefore returns StateReady torrent items that were
// submitted as uncached (a logged 'uncached_add' governor event exists for
// the item), are still materialized on the provider (remote_id set), and
// have never been played (last_played_at IS NULL), where the earliest
// uncached_add event predates cutoff. R1: on TorBox Pro, a finished uncached
// torrent seeds for 30 days holding an Allowed Active Slot until removed;
// a never-played grab would otherwise hold that slot for the full 30 days.
// Only StateReady items are eligible — items still resolving (actively
// downloading) are left alone.
func (s *Store) ListNeverPlayedUncachedBefore(ctx context.Context, cutoff time.Time, limit int) ([]*Item, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+itemColumns+` FROM items
		 WHERE state = 'ready'
		   AND source_type = 'torrent'
		   AND remote_id IS NOT NULL
		   AND last_played_at IS NULL
		   AND id IN (
		       SELECT item_id FROM governor_log
		       WHERE event_type = 'uncached_add'
		       GROUP BY item_id
		       HAVING MIN(created_at) <= ?
		   )
		 ORDER BY created_at ASC LIMIT ?`,
		formatTime(cutoff), limit)
	if err != nil {
		return nil, fmt.Errorf("list never-played uncached before: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanItems(rows)
}

// ListOrphanedRemoteBefore returns terminal (removed/failed) items that still
// hold a provider remote_id — i.e. TorBox transfers nobody will ever reap:
// the post-play janitor only covers played items and the R1 loop only covers
// StateReady, so a burst of grab→import→remove cycles leaks Allowed Active
// Slots until the provider's own 30-day expiry (live failure 2026-07-08:
// 10/10 slots pinned by ~50 orphans, every new grab rejected by the
// governor). Items sharing a remote_id with any LIVE item are excluded —
// TorBox dedups createtorrent by hash, so a removed item's transfer can be
// the same transfer a ready item is streaming from. The cutoff is a grace
// window so a just-removed item isn't reaped mid-lifecycle; removed items
// remain streamable afterward regardless (idempotent re-materialize).
func (s *Store) ListOrphanedRemoteBefore(ctx context.Context, cutoff time.Time, limit int) ([]*Item, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+itemColumns+` FROM items
		 WHERE state IN ('removed','failed')
		   AND remote_id IS NOT NULL
		   AND updated_at < ?
		   AND NOT EXISTS (
		       SELECT 1 FROM items live
		       WHERE live.remote_id = items.remote_id
		         AND live.state NOT IN ('removed','failed')
		   )
		 ORDER BY updated_at ASC LIMIT ?`,
		formatTime(cutoff), limit)
	if err != nil {
		return nil, fmt.Errorf("list orphaned remote before: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanItems(rows)
}

// GetItemsByInfoHash returns ALL items with the given info_hash (there may be
// multiple when re-grabs create new items after a previous removal).
func (s *Store) GetItemsByInfoHash(ctx context.Context, infoHash string) ([]*Item, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+itemColumns+` FROM items WHERE info_hash=?`, infoHash)
	if err != nil {
		return nil, fmt.Errorf("get items by info_hash: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanItems(rows)
}

// FindAliasCandidateByInfoHash returns the best still-relevant item sharing
// the given real torrent identity hash that already has a live provider
// binding (remote_id or queued_id set). It matches either the top-level
// info_hash column (the common case: plain magnet/torrent submissions) or a
// synthetic per-season grab's persisted real infohash in metadata_json (the
// resolveSyntheticGrab case) -- a season's synthetic client-facing hash never
// collides across items, but its real infohash does when multiple episodes
// of the same season are grabbed separately.
//
// TS-8.1: a later submission of the same real hash aliases onto this item's
// provider object/cache footprint instead of a redundant CheckCached +
// createtorrent, while the new item keeps its own independent row and
// eventual .strm identity (LC-01). Ready items are preferred over
// still-resolving ones; ties break on most recently updated. Only
// resolving/ready items are considered -- failed/removed/accepted rows never
// carry a durable provider binding worth aliasing onto. Returns nil, nil
// when infoHash is empty or no suitable candidate exists (never an error for
// the "no candidate" case, matching FindActiveBySubmissionKey's convention).
func (s *Store) FindAliasCandidateByInfoHash(ctx context.Context, infoHash string) (*Item, error) {
	infoHash = strings.TrimSpace(infoHash)
	if infoHash == "" {
		return nil, nil
	}
	row := s.db.QueryRowContext(ctx, `
        SELECT `+itemColumns+` FROM items
        WHERE state IN ('resolving','ready')
          AND provider IS NOT NULL AND provider != ''
          AND (remote_id IS NOT NULL OR queued_id IS NOT NULL)
          AND (info_hash = ? OR json_extract(metadata_json,'$.real_infohash') = ?)
        ORDER BY (state='ready') DESC, updated_at DESC
        LIMIT 1`, infoHash, infoHash)
	item, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return item, err
}

func (s *Store) FindActiveBySubmissionKey(ctx context.Context, submissionKey string) (*Item, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT `+itemColumns+` FROM items
        WHERE submission_key=?
          AND state IN ('accepted','resolving','ready')
        LIMIT 1`, submissionKey)
	item, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return item, err
}

func (s *Store) ListSABHistoryItems(ctx context.Context, clientKind ClientKind, category string, limit int) ([]*Item, error) {
	// For SAB history: removed items stay visible for a short tail window so
	// Sonarr can read the storage path and finish its post-import handshake —
	// but NOT forever. Listing removed items indefinitely made Sonarr's
	// DownloadEventHub re-issue "remove from history" for the same entries on
	// EVERY poll, permanently (live 2026-07-08: yesterday's imported S01 NZB
	// entries deleted every ~90s all day). The history delete is a no-op on an
	// already-removed item, so the loop never converged; a 15-minute tail
	// covers the real handshake and lets zombies age out.
	query := `SELECT ` + itemColumns + ` FROM items WHERE client_kind=?
	  AND COALESCE(json_extract(metadata_json,'$.sab_history_hidden'),0) != 1
	  AND (state IN ('ready','failed')
	       OR (state = 'removed' AND updated_at > datetime('now','-15 minutes')))`
	args := []any{string(clientKind)}
	if category != "" {
		query += ` AND category=?`
		args = append(args, category)
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list sab history items: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanItems(rows)
}

// HideFromSABHistory marks an item as deleted-from-history by the arr. The
// item's state is untouched (a StateReady item keeps streaming); it is only
// excluded from ListSABHistoryItems from now on, which converges Sonarr's
// per-poll history re-delete loop. Targeted json_set (not a full UpdateItem
// read-modify-write) so it can never clobber a concurrent resolver update.
func (s *Store) HideFromSABHistory(ctx context.Context, publicID string) (bool, error) {
	n, err := s.execWriteAffected(ctx, `
		UPDATE items SET metadata_json = json_set(metadata_json, '$.sab_history_hidden', json('true')), download_consumed = 1
		WHERE public_id = ?`, publicID)
	if err != nil {
		return false, fmt.Errorf("hide from sab history: %w", err)
	}
	return n > 0, nil
}

// MarkDownloadConsumed sets download_consumed=1 for the given item ID,
// signalling that the Arr has imported (moved) the download-side .strm.
// The blob reconciler will skip re-materialization for this item (COR-3).
func (s *Store) MarkDownloadConsumed(ctx context.Context, itemID string) error {
	_, err := s.execWrite(ctx, `UPDATE items SET download_consumed = 1 WHERE id = ?`, itemID)
	if err != nil {
		return fmt.Errorf("mark download consumed: %w", err)
	}
	return nil
}

func (s *Store) ListVisibleClientItems(ctx context.Context, clientKind ClientKind, category string, limit int) ([]*Item, error) {
	query := `SELECT ` + itemColumns + ` FROM items WHERE client_kind=? AND state != 'removed'`
	args := []any{string(clientKind)}
	if category != "" {
		query += ` AND category=?`
		args = append(args, category)
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list visible items: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanItems(rows)
}
func (s *Store) ClaimItemsDue(ctx context.Context, workerID string, states []ItemState, now time.Time, limit int) ([]*Item, error) {
	if len(states) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(states)), ",")
	args := make([]any, 0, len(states)+2)
	for _, st := range states {
		args = append(args, string(st))
	}
	args = append(args, formatTime(now), limit)

	selectQuery := fmt.Sprintf(`
        SELECT id FROM items
        WHERE state IN (%s)
          AND (next_run_at IS NULL OR next_run_at <= ?)
          AND claimed_by IS NULL
        ORDER BY created_at ASC LIMIT ?`, placeholders)

	ids, err := retrySQLiteBusy(ctx, func() ([]string, error) {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, fmt.Errorf("begin tx: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		rows, err := tx.QueryContext(ctx, selectQuery, args...)
		if err != nil {
			return nil, fmt.Errorf("select due items: %w", err)
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, err
			}
			ids = append(ids, id)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			return nil, nil
		}

		idPlaceholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		claimArgs := make([]any, 0, len(ids)+2)
		claimArgs = append(claimArgs, workerID, formatTime(now))
		for _, id := range ids {
			claimArgs = append(claimArgs, id)
		}
		claimQuery := fmt.Sprintf(`UPDATE items SET claimed_by=?, claimed_at=? WHERE id IN (%s)`, idPlaceholders)
		if _, err := tx.ExecContext(ctx, claimQuery, claimArgs...); err != nil {
			return nil, fmt.Errorf("claim items: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit claim: %w", err)
		}
		return ids, nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetItemsByIDs(ctx, ids)
}

func (s *Store) ClaimItem(ctx context.Context, itemID, workerID string) (bool, error) {
	res, err := s.execWrite(ctx, `UPDATE items SET claimed_by=?, claimed_at=? WHERE id=? AND claimed_by IS NULL`, workerID, formatTime(s.now()), itemID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *Store) ReleaseItemClaim(ctx context.Context, itemID, workerID string) error {
	_, err := s.execWrite(ctx, `UPDATE items SET claimed_by=NULL, claimed_at=NULL WHERE id=? AND claimed_by=?`, itemID, workerID)
	return err
}

func (s *Store) ReleaseAllClaims(ctx context.Context) error {
	_, err := s.execWrite(ctx, `UPDATE items SET claimed_by=NULL, claimed_at=NULL WHERE claimed_by IS NOT NULL`)
	return err
}

func (s *Store) GetItemsByIDs(ctx context.Context, ids []string) ([]*Item, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	query := fmt.Sprintf(`SELECT %s FROM items WHERE id IN (%s) ORDER BY created_at ASC`, itemColumns, placeholders)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get items by ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanItems(rows)
}

// ListPrunableRemovedOlderThan returns StateRemoved items older than cutoff
// that never materialized output: no strm_path receipt AND no strm_blobs rows.
//
// COR-13: materialized items are permanent library records. Sonarr moves the
// imported .strm into the library; that file points at /stream/{item}/{file},
// and under the DH-URL-only rule the items row + strm_blobs are the ONLY
// playback identity (see the SAB invariant in api/router.go
// handleSABDeleteFromHistory). The old query listed every removed item, so
// the prune silently killed imported torrents ~30 days after import
// (arr delete / ready-auto-remove -> removed; 30d idle -> row+blobs deleted
// -> library .strm 404s forever). Removed-retention now applies only to items
// removed before ever producing output (dead grabs, cancelled resolves).
func (s *Store) ListPrunableRemovedOlderThan(ctx context.Context, cutoff time.Time, limit int) ([]*Item, error) {
	query := `SELECT ` + itemColumns + ` FROM items
        WHERE state='removed'
          AND updated_at<?
          AND strm_path IS NULL
          AND NOT EXISTS (SELECT 1 FROM strm_blobs b WHERE b.item_id = items.id)
        ORDER BY updated_at ASC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, formatTime(cutoff), limit)
	if err != nil {
		return nil, fmt.Errorf("list prunable removed items: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanItems(rows)
}

// RequeueStaleAccepted transitions stale StateAccepted items to StateResolving
// with next_run_at=now so the normal submit/resolve workers pick them up.
//
// COR-14: item creation (StateAccepted) and the Accepted->Resolving queue
// write are two separate statements (api/router.go enqueueSubmission). A crash
// between them strands the item: no recovery worker claims Accepted, while
// FindActiveBySubmissionKey still matches 'accepted' -- so every re-grab of
// the same release dedupes against the orphan forever. cutoff must trail now
// by far more than the create->queue window ever lasts (callers use minutes),
// so an in-flight creation is never raced; claimed rows are skipped, and the
// optimistic state guard in UpdateItemState makes a lost race a no-op.
func (s *Store) RequeueStaleAccepted(ctx context.Context, cutoff time.Time, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT id FROM items
        WHERE state='accepted' AND updated_at<? AND claimed_by IS NULL
        ORDER BY created_at ASC LIMIT ?`, formatTime(cutoff), limit)
	if err != nil {
		return nil, fmt.Errorf("list stale accepted items: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan stale accepted id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	requeued := make([]string, 0, len(ids))
	for _, id := range ids {
		item, err := s.GetItemByID(ctx, id)
		if err != nil || item == nil {
			continue
		}
		if item.State != StateAccepted {
			continue // raced the normal enqueue transition -- leave it alone
		}
		now := s.now()
		item.NextRunAt = &now
		if err := s.UpdateItemState(ctx, item, StateResolving, "accepted-recovery: requeued after create/queue crash window"); err != nil {
			continue // optimistic guard lost -- the item moved on without us
		}
		requeued = append(requeued, id)
	}
	return requeued, nil
}

func (s *Store) DeleteRemovedItemsByIDs(ctx context.Context, ids []string) (int64, error) {
	return s.deleteItemsByIDsInState(ctx, ids, "removed")
}

// ListFailedOlderThan returns StateFailed items whose updated_at is before
// the given cutoff. Failed items are otherwise retained forever (unlike
// StateRemoved, which the main prune cycle already ages out) — that meant a
// permanently-failed NZB (e.g. a dead/expired NNTP article) stayed in DH's
// SAB-emulator history indefinitely, and Sonarr rebuilds its queue view from
// the download client's reported history on every poll, so the same failed
// entry kept reappearing in Sonarr's queue forever even after being manually
// cleared there (found 2026-07-01: The Boys S01 RakuvArrow release). Pruning
// failed items on a short retention (HARRBOR_FAILED_RETENTION_HOURS, default
// 24h — deliberately much shorter than RemovedRetention's 30 days, since a
// failure only needs to stay visible long enough to be noticed) stops that.
func (s *Store) ListFailedOlderThan(ctx context.Context, cutoff time.Time, limit int) ([]*Item, error) {
	query := `SELECT ` + itemColumns + ` FROM items WHERE state='failed' AND updated_at<? ORDER BY updated_at ASC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, formatTime(cutoff), limit)
	if err != nil {
		return nil, fmt.Errorf("list failed items: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanItems(rows)
}

// DeleteFailedItemsByIDs deletes StateFailed items by ID. Mirrors
// DeleteRemovedItemsByIDs's state guard (hardcoded state='failed' in the
// WHERE clause, not just the ID list) so a caller with a stale/wrong ID
// never deletes an item that has moved to a different state since it was
// listed.
func (s *Store) DeleteFailedItemsByIDs(ctx context.Context, ids []string) (int64, error) {
	return s.deleteItemsByIDsInState(ctx, ids, "failed")
}

// deleteItemsByIDsInState deletes items by ID, scoped to the given state (so
// a stale/wrong ID never deletes an item that has since moved to a different
// state). The transaction deletes all satellite rows first so FK constraints
// (item_events, governor_log) and orphan-prevention (segment_offsets, strm_blobs,
// item_file_probes, item_stream_failures — no FK, but no auto-cascade either)
// are handled atomically. A-B6: satellite tables added 2026-07-04.
func (s *Store) deleteItemsByIDsInState(ctx context.Context, ids []string, state string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}

	return retrySQLiteBusy(ctx, func() (int64, error) {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return 0, fmt.Errorf("begin delete items tx: %w", err)
		}
		committed := false
		defer func() {
			if !committed {
				_ = tx.Rollback()
			}
		}()

		// FK-constrained tables (must precede items delete).
		for _, table := range []string{"item_events", "governor_log"} {
			if _, err := tx.ExecContext(ctx,
				fmt.Sprintf(`DELETE FROM %s WHERE item_id IN (%s)`, table, placeholders), args...); err != nil {
				return 0, fmt.Errorf("delete %s for items: %w", table, err)
			}
		}
		// Non-FK satellite tables — no cascade, orphan on delete without this.
		for _, table := range []string{"segment_offsets", "strm_blobs", "item_file_probes", "item_stream_failures"} {
			if _, err := tx.ExecContext(ctx,
				fmt.Sprintf(`DELETE FROM %s WHERE item_id IN (%s)`, table, placeholders), args...); err != nil {
				return 0, fmt.Errorf("delete %s for items: %w", table, err)
			}
		}

		itemsArgs := append([]any{}, args...)
		result, err := tx.ExecContext(ctx,
			fmt.Sprintf(`DELETE FROM items WHERE state=? AND id IN (%s)`, placeholders),
			append([]any{state}, itemsArgs...)...)
		if err != nil {
			return 0, fmt.Errorf("delete items (state=%s): %w", state, err)
		}
		affected, _ := result.RowsAffected()

		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("commit delete items tx: %w", err)
		}
		committed = true
		return affected, nil
	})
}

// PruneOrphanSatelliteRows removes rows in satellite tables (segment_offsets,
// strm_blobs, item_file_probes, item_stream_failures) that reference item IDs
// no longer present in the items table. Called once at startup (A-B6) to
// clean up any orphans that accumulated before this fix was deployed.
// Returns the total number of rows deleted.
func (s *Store) PruneOrphanSatelliteRows(ctx context.Context) (int64, error) {
	var total int64
	for _, table := range []string{"segment_offsets", "strm_blobs", "item_file_probes", "item_stream_failures"} {
		n, err := s.execWriteAffected(ctx,
			fmt.Sprintf(`DELETE FROM %s WHERE item_id NOT IN (SELECT id FROM items)`, table))
		if err != nil {
			return total, fmt.Errorf("prune orphan %s: %w", table, err)
		}
		total += n
	}
	return total, nil
}

// PruneOrphanSyntheticReleases bounds synthetic_release growth (no FK,
// accumulates on every season search of a multi-season pack); called at
// startup alongside PruneOrphanSatelliteRows (A-B6).
//
// BUGFIX 2026-07-08: the original predicate deleted rows whose REAL_INFOHASH
// had no item — but items created from synthetic grabs store the SYNTH hash
// in info_hash, never the real one, so every restart wiped ALL multi-season
// mappings, including ones with live items. It only went unnoticed because
// arr re-searches re-upsert the mapping right before most grabs. A row is
// live if EITHER hash side has an item; rows with no item on either side are
// search-time leftovers, kept for a 7-day grace so a mapping surfaced by a
// search stays resolvable until the arr acts on it (RSS/delayed grabs).
func (s *Store) PruneOrphanSyntheticReleases(ctx context.Context) (int64, error) {
	return s.execWriteAffected(ctx,
		`DELETE FROM synthetic_release
		 WHERE created_at < datetime('now','-7 days')
		   AND synth_hash NOT IN (
		     SELECT COALESCE(info_hash,'') FROM items WHERE info_hash IS NOT NULL
		   )
		   AND real_infohash NOT IN (
		     SELECT COALESCE(info_hash,'') FROM items WHERE info_hash IS NOT NULL
		   )`)
}

// ─── qBit session CRUD (verbatim from TorBoxarr) ────────────────────────────

func (s *Store) CreateQBitSession(ctx context.Context, sid, username string, expiresAt time.Time) error {
	_, err := s.execWrite(ctx, `
        INSERT INTO qbit_sessions (sid, username, expires_at, created_at)
        VALUES (?,?,?,?)`,
		sid, username, formatTime(expiresAt), formatTime(s.now()))
	if err != nil {
		return fmt.Errorf("create qbit session: %w", err)
	}
	return nil
}

func (s *Store) ValidateQBitSession(ctx context.Context, sid string) (bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT expires_at FROM qbit_sessions WHERE sid=? LIMIT 1`, sid)
	var expiresAt string
	if err := row.Scan(&expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("query session: %w", err)
	}
	parsed, err := parseTime(expiresAt)
	if err != nil {
		return false, err
	}
	return parsed.After(s.now()), nil
}

func (s *Store) DeleteQBitSession(ctx context.Context, sid string) error {
	_, err := s.execWrite(ctx, `DELETE FROM qbit_sessions WHERE sid=?`, sid)
	if err != nil {
		return fmt.Errorf("delete qbit session: %w", err)
	}
	return nil
}

func (s *Store) PruneExpiredQBitSessions(ctx context.Context) (int64, error) {
	result, err := s.execWrite(ctx, `DELETE FROM qbit_sessions WHERE expires_at<=?`, formatTime(s.now()))
	if err != nil {
		return 0, fmt.Errorf("prune qbit sessions: %w", err)
	}
	affected, _ := result.RowsAffected()
	return affected, nil
}

// ─── Governor log ────────────────────────────────────────────────────────────

// RecordUncachedAdd appends one uncached-transfer event to the governor log.
func (s *Store) RecordUncachedAdd(ctx context.Context, itemID string) error {
	_, err := s.execWrite(ctx, `
        INSERT INTO governor_log (item_id, event_type, created_at)
        VALUES (?, 'uncached_add', ?)`,
		itemID, formatTime(s.now()))
	if err != nil {
		return fmt.Errorf("record uncached add: %w", err)
	}
	return nil
}

// CountUncachedSince returns the number of uncached-transfer events recorded
// since the given cutoff time. Used by the governor to enforce the rolling
func (s *Store) CountUncachedSince(ctx context.Context, since time.Time) (int, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM governor_log WHERE event_type='uncached_add' AND created_at>=?`,
		formatTime(since))
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("count uncached: %w", err)
	}
	return n, nil
}

func (s *Store) PruneGovernorLog(ctx context.Context) (int64, error) {
	cutoff := s.now().Add(-(15 * 24 * time.Hour))
	result, err := s.execWrite(ctx, `DELETE FROM governor_log WHERE created_at<?`, formatTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("prune governor log: %w", err)
	}
	affected, _ := result.RowsAffected()
	return affected, nil
}

// ─── Scan helpers ────────────────────────────────────────────────────────────

type scanner interface {
	Scan(dest ...any) error
}

func scanItems(rows *sql.Rows) ([]*Item, error) {
	var items []*Item
	for rows.Next() {
		item, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate items: %w", err)
	}
	return items, nil
}

func scanItem(s scanner) (*Item, error) {
	var (
		item             Item
		sourceType       string
		clientKind       string
		state            string
		remoteID         sql.NullString
		queuedID         sql.NullString
		infoHash         sql.NullString
		sourceURI        sql.NullString
		cached           int
		strmPath         sql.NullString
		sidecarPath      sql.NullString
		probeJSON        sql.NullString
		fileList         sql.NullString
		totalSize        int64
		lastPlayedAt     sql.NullString
		errorMessage     sql.NullString
		nextRunAt        sql.NullString
		metaJSON         string
		claimedBy        sql.NullString
		claimedAt        sql.NullString
		createdAt        string
		updatedAt        string
		rarManifest      sql.NullString
		provider         sql.NullString
		resolveKey       sql.NullString
		downloadConsumed int
	)

	if err := s.Scan(
		&item.ID, &item.PublicID,
		&sourceType, &clientKind,
		&item.Category, &state, &item.SubmissionKey,
		&remoteID, &queuedID, &infoHash,
		&item.DisplayName, &sourceURI,
		&cached, &strmPath, &sidecarPath, &probeJSON, &fileList,
		&totalSize, &lastPlayedAt,
		&errorMessage, &item.RetryCount, &nextRunAt,
		&metaJSON, &claimedBy, &claimedAt,
		&createdAt, &updatedAt,
		&rarManifest,
		&provider,
		&resolveKey,
		&downloadConsumed,
	); err != nil {
		return nil, err
	}

	item.SourceType = SourceType(sourceType)
	item.ClientKind = ClientKind(clientKind)
	item.State = ItemState(state)
	item.RemoteID = fromNullString(remoteID)
	item.QueuedID = fromNullString(queuedID)
	item.InfoHash = fromNullString(infoHash)
	item.SourceURI = fromNullString(sourceURI)
	item.Cached = cached == 1
	item.StrmPath = fromNullString(strmPath)
	item.SidecarPath = fromNullString(sidecarPath)
	item.ProbeJSON = fromNullString(probeJSON)
	item.FileList = fromNullString(fileList)
	item.ResolveKey = fromNullString(resolveKey)
	item.TotalSize = totalSize
	item.ErrorMessage = fromNullString(errorMessage)
	if lastPlayedAt.Valid {
		t, err := parseTime(lastPlayedAt.String)
		if err != nil {
			return nil, err
		}
		item.LastPlayedAt = &t
	}
	item.ClaimedBy = fromNullString(claimedBy)

	if claimedAt.Valid {
		t, err := parseTime(claimedAt.String)
		if err != nil {
			return nil, err
		}
		item.ClaimedAt = &t
	}
	if nextRunAt.Valid {
		t, err := parseTime(nextRunAt.String)
		if err != nil {
			return nil, err
		}
		item.NextRunAt = &t
	}

	var err error
	item.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return nil, err
	}
	item.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return nil, err
	}

	if metaJSON != "" {
		if err := json.Unmarshal([]byte(metaJSON), &item.Metadata); err != nil {
			return nil, fmt.Errorf("unmarshal metadata: %w", err)
		}
	}
	item.RarManifest = fromNullString(rarManifest)
	item.Provider = fromNullString(provider)
	item.DownloadConsumed = downloadConsumed == 1
	return &item, nil
}

// ─── Null / format helpers (verbatim from TorBoxarr) ─────────────────────────

func nullableString(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullableItemState(v *ItemState) any {
	if v == nil {
		return nil
	}
	return string(*v)
}

func nullableTime(v *time.Time) any {
	if v == nil {
		return nil
	}
	return formatTime(*v)
}

func fromNullString(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	out := v.String
	return &out
}

// parseTime parses a timestamp stored in SQLite. The canonical format is
// RFC3339Nano (written by formatTime). As a fallback it also accepts SQLite's
// space-separated datetime format ("2006-01-02 15:04:05") so that timestamps
// written by direct SQL tools (e.g. sqlite3 CLI with datetime('now')) never
// bring the service down. Any successfully parsed time is returned as UTC.
func parseTime(v string) (time.Time, error) {
	// Primary: canonical RFC3339Nano written by formatTime.
	if parsed, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return parsed.UTC(), nil
	}
	// Fallback: RFC3339 without sub-seconds.
	if parsed, err := time.Parse(time.RFC3339, v); err == nil {
		return parsed.UTC(), nil
	}
	// Fallback: SQLite datetime('now') space-separated, assumed UTC.
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if parsed, err := time.Parse(layout, v); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("parse time %q: unrecognised format", v)
}

func formatTime(v time.Time) string {
	return v.UTC().Format(time.RFC3339Nano)
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// ─── SQLite busy retry (verbatim from TorBoxarr) ─────────────────────────────

func retrySQLiteBusy[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	var zero T
	delay := sqliteBusyRetryBaseDelay
	for attempt := 0; ; attempt++ {
		value, err := fn()
		if err == nil {
			return value, nil
		}
		if !isSQLiteBusy(err) || attempt >= sqliteBusyRetryAttempts-1 {
			return zero, err
		}
		if err := sleepContext(ctx, delay); err != nil {
			return zero, err
		}
		if delay < sqliteBusyRetryMaxDelay {
			delay *= 2
			if delay > sqliteBusyRetryMaxDelay {
				delay = sqliteBusyRetryMaxDelay
			}
		}
	}
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func isSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	type sqliteCodeError interface{ Code() int }
	var codeErr sqliteCodeError
	if errors.As(err, &codeErr) {
		switch codeErr.Code() & sqlitePrimaryCodeMask {
		case sqliteBusyCode, sqliteLockedCode:
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "sqlite_busy") ||
		strings.Contains(msg, "sqlite_locked") ||
		strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked")
}

const (
	failHashThreshold = 3
	blacklistDuration = 7 * 24 * time.Hour
)

// FailureRecordResult describes the persisted state after recording a source failure.
type FailureRecordResult struct {
	FailCount        int
	Blacklisted      bool
	BlacklistedUntil *time.Time
}

// RecordFailure records one threshold-based source failure and returns the
// persisted failure-memory state. A source is blacklisted when its fail count
// reaches failHashThreshold; earlier strikes remain eligible.
func (s *Store) RecordFailure(ctx context.Context, hash string) (FailureRecordResult, error) {
	now := s.now()
	blacklistAt := now.Add(blacklistDuration)
	result, err := retrySQLiteBusy(ctx, func() (FailureRecordResult, error) {
		row := s.db.QueryRowContext(ctx, `
        INSERT INTO failed_hashes (hash, fail_count, last_failed_at, blacklisted_until)
        VALUES (?, 1, ?, NULL)
        ON CONFLICT(hash) DO UPDATE SET
            fail_count     = failed_hashes.fail_count + 1,
            last_failed_at = excluded.last_failed_at,
            blacklisted_until = CASE
                WHEN failed_hashes.fail_count + 1 >= ?
                THEN ?
                ELSE failed_hashes.blacklisted_until
            END
        RETURNING fail_count, blacklisted_until`,
			hash, formatTime(now), failHashThreshold, formatTime(blacklistAt),
		)
		return scanFailureRecordResult(row, now)
	})
	if err != nil {
		return FailureRecordResult{}, fmt.Errorf("record failure for %s: %w", hash, err)
	}
	return result, nil
}

// IncrementFailCount records one threshold-based source failure while
// preserving the legacy error-only API used by existing callers.
func (s *Store) IncrementFailCount(ctx context.Context, hash string) error {
	_, err := s.RecordFailure(ctx, hash)
	return err
}

// BlacklistImmediately records one conclusive terminal failure and opens a
// blacklist window without waiting for the threshold-strike count.
func (s *Store) BlacklistImmediately(ctx context.Context, hash string) (FailureRecordResult, error) {
	now := s.now()
	blacklistAt := now.Add(blacklistDuration)
	result, err := retrySQLiteBusy(ctx, func() (FailureRecordResult, error) {
		row := s.db.QueryRowContext(ctx, `
        INSERT INTO failed_hashes (hash, fail_count, last_failed_at, blacklisted_until)
        VALUES (?, 1, ?, ?)
        ON CONFLICT(hash) DO UPDATE SET
            fail_count = failed_hashes.fail_count + 1,
            last_failed_at = excluded.last_failed_at,
            blacklisted_until = excluded.blacklisted_until
        RETURNING fail_count, blacklisted_until`,
			hash, formatTime(now), formatTime(blacklistAt),
		)
		return scanFailureRecordResult(row, now)
	})
	if err != nil {
		return FailureRecordResult{}, fmt.Errorf("blacklist immediately %s: %w", hash, err)
	}
	return result, nil
}

func scanFailureRecordResult(row *sql.Row, now time.Time) (FailureRecordResult, error) {
	var result FailureRecordResult
	var blacklistedUntil sql.NullString
	if err := row.Scan(&result.FailCount, &blacklistedUntil); err != nil {
		return FailureRecordResult{}, err
	}
	if !blacklistedUntil.Valid {
		return result, nil
	}
	until, err := parseTime(blacklistedUntil.String)
	if err != nil {
		return FailureRecordResult{}, err
	}
	result.BlacklistedUntil = &until
	result.Blacklisted = until.After(now)
	return result, nil
}

// IsBlacklisted returns true if hash has an active blacklist window (blacklisted_until > now).
// Returns false for unknown hashes and for hashes whose blacklist has expired.
func (s *Store) IsBlacklisted(ctx context.Context, hash string) (bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT blacklisted_until FROM failed_hashes WHERE hash=? LIMIT 1`, hash)
	var bu sql.NullString
	if err := row.Scan(&bu); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("is blacklisted %s: %w", hash, err)
	}
	if !bu.Valid {
		return false, nil
	}
	until, err := parseTime(bu.String)
	if err != nil {
		return false, err
	}
	return until.After(s.now()), nil
}

// ExpireBlacklists clears blacklisted_until for all hashes whose window has passed.
// Called at startup and periodically to allow stale blacklists to fall off.
func (s *Store) ExpireBlacklists(ctx context.Context) (int64, error) {
	result, err := s.execWrite(ctx,
		`UPDATE failed_hashes SET blacklisted_until = NULL WHERE blacklisted_until IS NOT NULL AND blacklisted_until <= ?`,
		formatTime(s.now()),
	)
	if err != nil {
		return 0, fmt.Errorf("expire blacklists: %w", err)
	}
	affected, _ := result.RowsAffected()
	return affected, nil
}

// ListMaterializedItems returns every played item still materialized on TorBox
// (remote_id set), regardless of state, so in-memory janitor AfterFunc timers
// can be restored after a container restart. Played-then-imported items are
// state='removed' yet still hold a TorBox entry that must be cleaned — the old
// state='ready' filter skipped them, leaking entries until TorBox's 30-day expiry.
func (s *Store) ListMaterializedItems(ctx context.Context) ([]*Item, error) {
	query := `SELECT ` + itemColumns + ` FROM items
		WHERE remote_id IS NOT NULL
		  AND last_played_at IS NOT NULL`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list materialized items: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanItems(rows)
}

// ListStubsPending returns StateReady items where the stub .mkv has not yet
// been successfully written (sidecar_path IS NULL). These items have their
// .strm files on disk but Jellyfin cannot read local codec metadata yet.
// Limited to items where next_run_at is NULL or <= now so callers can apply
// exponential backoff via UpdateItem(next_run_at = future).
func (s *Store) ListStubsPending(ctx context.Context, now time.Time, limit int) ([]*Item, error) {
	query := `SELECT ` + itemColumns + ` FROM items
        WHERE state = 'ready'
          -- Stub .mkv writing is the retired NNTP-only path. Torrent (debrid)
          -- items import via .strm + the arr-side probe wrapper, so they must
          -- never be stub-wired. Gate the retry cycle to nzb items only.
          AND source_type = 'nzb'
          AND sidecar_path IS NULL
          AND (next_run_at IS NULL OR next_run_at <= ?)
        ORDER BY created_at ASC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, formatTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("list stubs pending: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanItems(rows)
}

// FindNZBItemsByCategory returns all StateReady NZB items in a given category.
// If category is empty, returns all StateReady NZB items across all categories.
func (s *Store) FindNZBItemsByCategory(ctx context.Context, category string) ([]*Item, error) {
	var (
		query string
		args  []any
	)
	if category == "" {
		query = `SELECT ` + itemColumns + ` FROM items
        WHERE source_type = 'nzb'
          AND state = 'ready'
        ORDER BY created_at DESC`
	} else {
		query = `SELECT ` + itemColumns + ` FROM items
        WHERE source_type = 'nzb'
          AND state = 'ready'
          AND category = ?
        ORDER BY created_at DESC`
		args = append(args, category)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("find nzb items by category: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanItems(rows)
}

func escapeSQLiteLike(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '%', '_', 92:
			b.WriteRune(92)
		}
		b.WriteRune(r)
	}
	return b.String()
}

// FindNZBItemByStrmDir returns the first StateReady NZB item whose strm_path
// is within the given directory prefix (e.g. /data/tv-modern/ReleaseName/).
// Used by the WebDAV handler to resolve /dav/{category}/{release}/{file}.mkv
func (s *Store) FindNZBItemByStrmDir(ctx context.Context, strmDirPrefix string) (*Item, error) {
	// Ensure prefix ends with / so LIKE doesn't match sibling directories.
	prefix := strmDirPrefix
	if len(prefix) > 0 && prefix[len(prefix)-1] != '/' {
		prefix += "/"
	}
	query := `SELECT ` + itemColumns + ` FROM items
        WHERE source_type = 'nzb'
          AND state = 'ready'
          AND strm_path LIKE ? ESCAPE '\'
        ORDER BY created_at DESC LIMIT 1`
	rows, err := s.db.QueryContext(ctx, query, escapeSQLiteLike(prefix)+"%")
	if err != nil {
		return nil, fmt.Errorf("find nzb item by strm dir: %w", err)
	}
	defer func() { _ = rows.Close() }()
	items, err := scanItems(rows)
	if err != nil || len(items) == 0 {
		return nil, err
	}
	return items[0], nil
}

// ScrubFileListRequestDLURLs removes the "requestdl_url" field from every
// file_list JSON row that still contains it (B-N3 one-time maintenance).
// The field carries a time-limited TorBox API token and must not rest in the
// DB. Each row is decoded and re-encoded without the field; rows that fail to
// decode (e.g. non-JSON legacy values) are left untouched.
//
// Returns the number of rows updated. Safe to call on every startup — once
// all rows are clean the SELECT returns zero rows and the function is a no-op.
func (s *Store) ScrubFileListRequestDLURLs(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, file_list FROM items WHERE file_list LIKE '%requestdl_url%'`)
	if err != nil {
		return 0, fmt.Errorf("scrub file_list: query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type entry map[string]any
	type rowUpdate struct {
		id      string
		newJSON string
	}
	var updates []rowUpdate
	for rows.Next() {
		var id string
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			continue
		}
		var entries []entry
		if err := json.Unmarshal([]byte(raw), &entries); err != nil {
			continue // non-JSON or unexpected shape — leave untouched
		}
		for i := range entries {
			delete(entries[i], "requestdl_url")
		}
		cleaned, err := json.Marshal(entries)
		if err != nil {
			continue
		}
		updates = append(updates, rowUpdate{id: id, newJSON: string(cleaned)})
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("scrub file_list: scan: %w", err)
	}

	n := 0
	for _, u := range updates {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE items SET file_list = ? WHERE id = ?`, u.newJSON, u.id); err != nil {
			return n, fmt.Errorf("scrub file_list: update %s: %w", u.id, err)
		}
		n++
	}
	return n, nil
}

// RekeyFileProbesByFileID updates item_file_probes rows whose file_id is a
// small integer (positional index from pre-B-N3 writes) to use the real
// provider file_id from the item's current file_list JSON. This is a one-time
// migration: once all rows use real file IDs the SELECT returns zero rows.
//
// A probe-cache hit for a file only occurs when the exact (item_id, file_id,
// args_key) triple matches; positional-index keys never match post-B-N3 URLs
// (which embed the real file_id), so they would simply never be used again.
// This rekey makes them usable without requiring a re-probe of every file.
func (s *Store) RekeyFileProbesByFileID(ctx context.Context) (int, error) {
	// Find all probe rows whose file_id looks like a small integer index.
	rows, err := s.db.QueryContext(ctx,
		`SELECT p.item_id, p.file_id, p.args_key, p.probe_json, i.file_list
         FROM item_file_probes p
         JOIN items i ON i.id = p.item_id
         WHERE p.file_id GLOB '[0-9]' OR p.file_id GLOB '[0-9][0-9]'`)
	if err != nil {
		return 0, fmt.Errorf("rekey probes: query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type rekeyRow struct {
		itemID    string
		oldFileID string
		newFileID string
		argsKey   string
		probeJSON string
	}
	var updates []rekeyRow
	for rows.Next() {
		var itemID, oldFileID, argsKey, probeJSON string
		var fileListRaw sql.NullString
		if err := rows.Scan(&itemID, &oldFileID, &argsKey, &probeJSON, &fileListRaw); err != nil {
			continue
		}
		if !fileListRaw.Valid || fileListRaw.String == "" {
			continue
		}
		var entries []struct {
			FileID string `json:"file_id"`
		}
		if err := json.Unmarshal([]byte(fileListRaw.String), &entries); err != nil {
			continue
		}
		idx, err := strconv.Atoi(oldFileID)
		if err != nil {
			return 0, fmt.Errorf("rekey probes: parse file_id %q for item %s: %w", oldFileID, itemID, err)
		}
		if idx < 0 || idx >= len(entries) || entries[idx].FileID == "" {
			continue
		}
		newFileID := entries[idx].FileID
		if newFileID == oldFileID {
			continue // already correct
		}
		updates = append(updates, rekeyRow{
			itemID:    itemID,
			oldFileID: oldFileID,
			newFileID: newFileID,
			argsKey:   argsKey,
			probeJSON: probeJSON,
		})
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("rekey probes: scan: %w", err)
	}

	n := 0
	for _, u := range updates {
		// Upsert with the new file_id; delete the old row to avoid orphans.
		_, err := s.execWrite(ctx,
			`INSERT INTO item_file_probes (item_id, file_id, args_key, probe_json, created_at)
             VALUES (?, ?, ?, ?, ?)
             ON CONFLICT(item_id, file_id, args_key) DO UPDATE SET
                 probe_json = excluded.probe_json,
                 created_at = excluded.created_at`,
			u.itemID, u.newFileID, u.argsKey, u.probeJSON, formatTime(s.now()))
		if err != nil {
			return n, fmt.Errorf("rekey probes: upsert %s/%s: %w", u.itemID, u.newFileID, err)
		}
		if _, err := s.execWrite(ctx,
			`DELETE FROM item_file_probes WHERE item_id=? AND file_id=? AND args_key=?`,
			u.itemID, u.oldFileID, u.argsKey); err != nil {
			return n, fmt.Errorf("rekey probes: delete old key %s/%s: %w", u.itemID, u.oldFileID, err)
		}
		n++
	}
	return n, nil
}

// ListReadyItemsWithStrmPath returns all items in StateReady that have a
// strm_path set, for the rescan-reconcile startup pass.
func (s *Store) ListReadyItemsWithStrmPath(ctx context.Context) ([]*Item, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+itemColumns+` FROM items
         WHERE state = 'ready' AND strm_path IS NOT NULL AND strm_path != ''
         ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list ready items with strm_path: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanItems(rows)
}

// ListReadyNNTPItems returns at most limit Ready NZB items fulfilled via the
// NNTP lane (no TorBox remote/queued id -- a TorBox-usenet-cache-lane NZB
// item never reaches this list), in stable order. NS-5.4 uses this to search
// for a next-episode look-ahead candidate; the explicit bound keeps that
// search finite (DG-07), mirroring ListReadyHTTPItems.
func (s *Store) ListReadyNNTPItems(ctx context.Context, limit int) ([]*Item, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+itemColumns+` FROM items
         WHERE state = 'ready' AND source_type = 'nzb'
           AND remote_id IS NULL AND queued_id IS NULL
         ORDER BY created_at DESC, id DESC
         LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list ready NNTP items: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanItems(rows)
}

// ListKeepWarmCandidates returns at most limit Ready torrent items that
// were played at or after playedSince (TS-4.2's "recent play activity"
// qualifier), ordered oldest-grab-first. Oldest CreatedAt is the natural
// closest-to-expiry-first ordering under the row's fixed per-provider
// retention assumption (internal/api's assumedProviderExpiryDays), giving
// the same eventually-covers-the-whole-eligible-set rotation property
// NS-6.1's least-recently-audited ordering gives, without a separate
// cursor. The explicit bound (mirrors ListReadyNNTPItems/ListReadyHTTPItems)
// keeps one janitor tick finite regardless of library size.
func (s *Store) ListKeepWarmCandidates(ctx context.Context, playedSince time.Time, limit int) ([]*Item, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+itemColumns+` FROM items
         WHERE state = 'ready' AND source_type = 'torrent'
           AND last_played_at IS NOT NULL AND last_played_at >= ?
         ORDER BY created_at ASC, id ASC
         LIMIT ?`, formatTime(playedSince), limit)
	if err != nil {
		return nil, fmt.Errorf("list keep-warm candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanItems(rows)
}

// ListReadyTorrentItemsAfter returns at most limit Ready torrent items with
// id > afterID in ascending id order, for TS-4.3's in-memory decay-audit
// rotation cursor. This row lists no DG-02 (no migration, no persisted
// last-audited timestamp), unlike NS-6.1's DB-backed NextHealthAuditItem, so
// eventual full-library coverage is achieved via an id-keyset cursor held in
// memory by the caller instead of a persisted least-recently-audited
// ordering. Passing afterID="" starts from the beginning; an empty result
// means "no more items after afterID" -- the caller's signal to reset its
// cursor to "" and query again for the next candidate, wrapping the
// rotation. The explicit bound (mirrors ListReadyNNTPItems/ListReadyHTTPItems)
// keeps one call finite regardless of library size.
func (s *Store) ListReadyTorrentItemsAfter(ctx context.Context, afterID string, limit int) ([]*Item, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+itemColumns+` FROM items
         WHERE state = 'ready' AND source_type = 'torrent' AND id > ?
         ORDER BY id ASC
         LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list ready torrent items after: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanItems(rows)
}

// ListReadyHTTPItems returns at most limit ready HTTP items in stable order.
// Callers use the explicit bound to keep predictive work finite.
func (s *Store) ListReadyHTTPItems(ctx context.Context, limit int) ([]*Item, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+itemColumns+` FROM items
         WHERE state = 'ready' AND source_type = 'http'
         ORDER BY created_at DESC, id DESC
         LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list ready HTTP items: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanItems(rows)
}
