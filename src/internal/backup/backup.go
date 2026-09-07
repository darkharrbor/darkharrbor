// Package backup owns DarkHarrbor database and sealed-secret snapshots.
package backup

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/securefile"
	sqlite "modernc.org/sqlite"
)

const (
	snapshotPrefix = "darkharrbor-"
	snapshotLayout = "20060102T150405.000000000Z"
	databaseName   = "darkharrbor.db"
	secretsName    = "secrets.sealed"
	maxSecretsSize = 16 << 20
)

var ErrDaemonRunning = errors.New("darkharrbor daemon is running")

// Config is the non-secret backup configuration.
type Config struct {
	Target      string
	SecretsPath string
	Interval    time.Duration
	Keep        int
}

// Service creates scheduled, atomically published snapshots.
type Service struct {
	db    *sql.DB
	cfg   Config
	log   *slog.Logger
	now   func() time.Time
	after func(time.Duration) <-chan time.Time
}

// New constructs a backup service.
func New(db *sql.DB, cfg Config, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		db:    db,
		cfg:   cfg,
		log:   log,
		now:   time.Now,
		after: time.After,
	}
}

// SetClock replaces time sources for deterministic cadence tests.
func (s *Service) SetClock(now func() time.Time, after func(time.Duration) <-chan time.Time) {
	if now != nil {
		s.now = now
	}
	if after != nil {
		s.after = after
	}
}

// Run creates one snapshot immediately and then at each configured interval.
func (s *Service) Run(ctx context.Context) {
	for {
		name, err := s.Create(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Error("backup failed")
		} else {
			s.log.Info("backup complete", "snapshot", name)
		}

		select {
		case <-ctx.Done():
			return
		case <-s.after(s.cfg.Interval):
		}
	}
}

// Create creates one complete snapshot and applies retention.
func (s *Service) Create(ctx context.Context) (string, error) {
	if s.db == nil {
		return "", errors.New("backup database is required")
	}
	if err := validateConfig(s.cfg); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := securefile.PrepareDir(s.cfg.Target); err != nil {
		return "", errors.New("create backup target")
	}
	release, err := acquireSnapshotLock(ctx, s.cfg.Target)
	if err != nil {
		return "", err
	}
	defer release()
	if err := cleanupPartials(s.cfg.Target); err != nil {
		return "", err
	}

	tmpDir, err := os.MkdirTemp(s.cfg.Target, ".darkharrbor-partial-")
	if err != nil {
		return "", errors.New("create partial snapshot")
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	dbPath := filepath.Join(tmpDir, databaseName)
	if err := onlineBackup(ctx, s.db, dbPath); err != nil {
		return "", err
	}
	if err := os.Chmod(dbPath, 0o600); err != nil {
		return "", errors.New("secure database snapshot")
	}
	if err := syncFile(dbPath); err != nil {
		return "", err
	}
	if err := copyRegularFile(s.cfg.SecretsPath, filepath.Join(tmpDir, secretsName), maxSecretsSize); err != nil {
		return "", err
	}
	if err := syncDir(tmpDir); err != nil {
		return "", err
	}

	name := snapshotPrefix + s.now().UTC().Format(snapshotLayout)
	finalDir := filepath.Join(s.cfg.Target, name)
	if err := os.Rename(tmpDir, finalDir); err != nil {
		return "", errors.New("publish snapshot")
	}
	if err := syncDir(s.cfg.Target); err != nil {
		return "", err
	}
	if err := enforceRetention(s.cfg.Target, s.cfg.Keep); err != nil {
		return "", err
	}
	return name, nil
}

func acquireSnapshotLock(ctx context.Context, target string) (func(), error) {
	path := filepath.Join(target, ".darkharrbor-backup.lock")
	lock, err := securefile.OpenLock(path)
	if err != nil {
		return nil, errors.New("open backup lock")
	}
	closeLock := func() { _ = lock.Close() }
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
				closeLock()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			closeLock()
			return nil, errors.New("lock backup target")
		}
		select {
		case <-ctx.Done():
			closeLock()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

type onlineBackupper interface {
	NewBackup(string) (*sqlite.Backup, error)
}

func onlineBackup(ctx context.Context, db *sql.DB, dst string) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return errors.New("open database backup connection")
	}
	defer func() { _ = conn.Close() }()

	return conn.Raw(func(driverConn any) error {
		source, ok := driverConn.(onlineBackupper)
		if !ok {
			return errors.New("sqlite online backup is unavailable")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		b, err := source.NewBackup(dst)
		if err != nil {
			return errors.New("start sqlite online backup")
		}
		finished := false
		defer func() {
			if !finished {
				_ = b.Finish()
			}
		}()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			more, err := b.Step(128)
			if err != nil {
				return errors.New("step sqlite online backup")
			}
			if !more {
				break
			}
		}
		finished = true
		if err := b.Finish(); err != nil {
			return errors.New("finish sqlite online backup")
		}
		return nil
	})
}

func validateConfig(cfg Config) error {
	target := filepath.Clean(strings.TrimSpace(cfg.Target))
	if !filepath.IsAbs(target) || target == string(filepath.Separator) || strings.Contains(target, "://") {
		return errors.New("backup target must be a safe absolute directory")
	}
	if strings.TrimSpace(cfg.SecretsPath) == "" {
		return errors.New("sealed secrets path is required")
	}
	if cfg.Interval <= 0 {
		return errors.New("backup interval must be positive")
	}
	if cfg.Keep < 1 {
		return errors.New("backup retention must be at least one")
	}
	return nil
}

func cleanupPartials(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return errors.New("read backup target")
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".darkharrbor-partial-") && entry.IsDir() {
			if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
				return errors.New("remove partial snapshot")
			}
		}
	}
	return nil
}

func enforceRetention(root string, keep int) error {
	snapshots, err := listSnapshots(root)
	if err != nil {
		return err
	}
	for _, path := range snapshots[:max(0, len(snapshots)-keep)] {
		if err := os.RemoveAll(path); err != nil {
			return errors.New("remove expired snapshot")
		}
	}
	return syncDir(root)
}

func listSnapshots(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, errors.New("read backup target")
	}
	var snapshots []string
	for _, entry := range entries {
		if !entry.IsDir() || !isSnapshotName(entry.Name()) {
			continue
		}
		path := filepath.Join(root, entry.Name())
		if completeSnapshot(path) {
			snapshots = append(snapshots, path)
		}
	}
	sort.Strings(snapshots)
	return snapshots, nil
}

// ListSnapshotNames returns sorted canonical basenames for structurally
// complete restore candidates. Restore still owns permission, no-follow,
// identity, size, and SQLite-integrity validation before installing anything.
func ListSnapshotNames(root string) ([]string, error) {
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("backup source is not a directory")
	}
	paths, err := listSnapshots(root)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(paths))
	for i, path := range paths {
		names[i] = filepath.Base(path)
	}
	return names, nil
}

func isSnapshotName(name string) bool {
	if !strings.HasPrefix(name, snapshotPrefix) {
		return false
	}
	_, err := time.Parse(snapshotLayout, strings.TrimPrefix(name, snapshotPrefix))
	return err == nil
}

func completeSnapshot(path string) bool {
	for _, name := range []string{databaseName, secretsName} {
		info, err := os.Lstat(filepath.Join(path, name))
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return false
		}
	}
	return true
}

func copyRegularFile(src, dst string, maxBytes int64) error {
	data, err := securefile.Read(src, maxBytes)
	if err != nil {
		return errors.New("sealed secrets file is missing or invalid")
	}
	if err := securefile.AtomicWrite(dst, data); err != nil {
		return errors.New("create sealed secrets snapshot")
	}
	return nil
}

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return errors.New("open snapshot for sync")
	}
	defer func() { _ = f.Close() }()
	if err := f.Sync(); err != nil {
		return errors.New("sync snapshot")
	}
	return nil
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return errors.New("open snapshot directory")
	}
	defer func() { _ = dir.Close() }()
	if err := dir.Sync(); err != nil {
		return errors.New("sync snapshot directory")
	}
	return nil
}

// AcquireLock prevents restore while the daemon owns the same config directory.
func AcquireLock(configDir string) (*os.File, error) {
	if err := securefile.PrepareDir(configDir); err != nil {
		return nil, errors.New("create config directory")
	}
	lock, err := securefile.OpenLock(filepath.Join(configDir, ".darkharrbor.lock"))
	if err != nil {
		return nil, errors.New("open daemon lock")
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrDaemonRunning
		}
		return nil, errors.New("acquire daemon lock")
	}
	return lock, nil
}

// ReleaseLock releases a lock returned by AcquireLock.
func ReleaseLock(lock *os.File) {
	if lock == nil {
		return
	}
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	_ = lock.Close()
}

// Restore replaces a stopped daemon's database and sealed secrets from the
// newest complete snapshot below source, or source itself when it is a snapshot.
func Restore(ctx context.Context, source, configDir string) (string, error) {
	lock, err := AcquireLock(configDir)
	if err != nil {
		return "", err
	}
	defer ReleaseLock(lock)

	snapshot, err := latestSnapshot(source)
	if err != nil {
		return "", err
	}

	dbTemp, err := copyRestoreFile(filepath.Join(snapshot, databaseName), configDir, ".darkharrbor-restore-db-", 0o600, 1<<40)
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(dbTemp) }()
	secretsTemp, err := copyRestoreFile(filepath.Join(snapshot, secretsName), configDir, ".darkharrbor-restore-secrets-", 0o600, maxSecretsSize)
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(secretsTemp) }()
	if err := checkDatabase(ctx, dbTemp); err != nil {
		return "", err
	}

	dbDst := filepath.Join(configDir, databaseName)
	secretsDst := filepath.Join(configDir, secretsName)
	dbOld := dbDst + ".pre-restore"
	secretsOld := secretsDst + ".pre-restore"
	_ = os.Remove(dbOld)
	_ = os.Remove(secretsOld)

	if err := moveAside(dbDst, dbOld); err != nil {
		return "", err
	}
	dbMoved := true
	defer func() {
		if dbMoved {
			_ = os.Rename(dbOld, dbDst)
		}
	}()
	if err := moveAside(secretsDst, secretsOld); err != nil {
		return "", err
	}
	secretsMoved := true
	defer func() {
		if secretsMoved {
			_ = os.Rename(secretsOld, secretsDst)
		}
	}()

	if err := os.Rename(dbTemp, dbDst); err != nil {
		return "", errors.New("install restored database")
	}
	if err := os.Rename(secretsTemp, secretsDst); err != nil {
		_ = os.Remove(dbDst)
		return "", errors.New("install restored sealed secrets")
	}
	if err := syncDir(configDir); err != nil {
		return "", err
	}
	dbMoved = false
	secretsMoved = false
	_ = os.Remove(dbOld)
	_ = os.Remove(secretsOld)
	_ = os.Remove(dbDst + "-wal")
	_ = os.Remove(dbDst + "-shm")
	return filepath.Base(snapshot), nil
}

// VerifySnapshot performs the read-only restore checks that do not require the
// daemon to stop. Restore repeats these checks before replacing live state.
func VerifySnapshot(ctx context.Context, source string) (string, error) {
	snapshot := filepath.Clean(source)
	info, err := os.Lstat(snapshot)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !completeSnapshot(snapshot) {
		return "", errors.New("backup source is not a complete snapshot")
	}
	if err := checkDatabase(ctx, filepath.Join(snapshot, databaseName)); err != nil {
		return "", err
	}
	return snapshot, nil
}

func latestSnapshot(source string) (string, error) {
	source = filepath.Clean(source)
	info, err := os.Lstat(source)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("backup source is not a directory")
	}
	if completeSnapshot(source) {
		return source, nil
	}
	snapshots, err := listSnapshots(source)
	if err != nil {
		return "", err
	}
	if len(snapshots) == 0 {
		return "", errors.New("no complete backup snapshot found")
	}
	return snapshots[len(snapshots)-1], nil
}

func checkDatabase(ctx context.Context, path string) error {
	guard, err := securefile.Open(path, 1<<40)
	if err != nil {
		return errors.New("snapshot database is missing or unsafe")
	}
	defer func() { _ = guard.Close() }()
	guardInfo, err := guard.Stat()
	if err != nil {
		return errors.New("inspect snapshot database")
	}
	// Published snapshots are immutable. Tell SQLite not to create WAL sidecars
	// so integrity checks also work when operators mount backup storage read-only.
	u := &url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&immutable=1"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return errors.New("open snapshot database")
	}
	defer func() { _ = db.Close() }()
	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil || result != "ok" {
		return errors.New("snapshot database integrity check failed")
	}
	pathInfo, err := os.Lstat(path)
	if err != nil || !pathInfo.Mode().IsRegular() || !os.SameFile(guardInfo, pathInfo) {
		return errors.New("snapshot database changed during validation")
	}
	return nil
}

func copyRestoreFile(src, dir, pattern string, mode os.FileMode, maxBytes int64) (string, error) {
	in, err := securefile.Open(src, maxBytes)
	if err != nil {
		return "", errors.New("snapshot file is missing or unsafe")
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil {
		return "", errors.New("inspect snapshot file")
	}
	out, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", errors.New("create restore staging file")
	}
	name := out.Name()
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if err := out.Chmod(mode); err != nil {
		return "", errors.New("secure restore staging file")
	}
	n, err := io.CopyN(out, in, maxBytes+1)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", errors.New("copy restore staging file")
	}
	if n != info.Size() || n > maxBytes {
		return "", errors.New("snapshot file changed or exceeded size limit")
	}
	if err := out.Sync(); err != nil {
		return "", errors.New("sync restore staging file")
	}
	if err := out.Close(); err != nil {
		return "", errors.New("close restore staging file")
	}
	ok = true
	return name, nil
}

func moveAside(path, rollback string) error {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		if err := os.Rename(path, rollback); err != nil {
			return errors.New("preserve pre-restore file")
		}
	case !errors.Is(err, os.ErrNotExist):
		return errors.New("inspect pre-restore file")
	}
	return nil
}
