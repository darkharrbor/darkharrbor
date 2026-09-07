package backup

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/securefile"
)

func testDatabase(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(2)
	if _, err := db.Exec("PRAGMA journal_mode=WAL; CREATE TABLE records(id INTEGER PRIMARY KEY, value TEXT); INSERT INTO records(value) VALUES ('before')"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func testService(t *testing.T, keep int) (*Service, string, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	secrets := filepath.Join(root, "secrets.sealed")
	if err := os.WriteFile(secrets, []byte("sealed-ciphertext"), 0o600); err != nil {
		t.Fatal(err)
	}
	db := testDatabase(t, filepath.Join(root, "source.db"))
	target := filepath.Join(root, "backups")
	svc := New(db, Config{Target: target, SecretsPath: secrets, Interval: 24 * time.Hour, Keep: keep}, nil)
	return svc, target, secrets
}

func TestCreateOnlineSnapshotAndRetention(t *testing.T) {
	svc, target, secrets := testService(t, 2)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return now }, nil)

	for i := 0; i < 3; i++ {
		if _, err := svc.db.Exec("INSERT INTO records(value) VALUES (?)", i); err != nil {
			t.Fatal(err)
		}
		name, err := svc.Create(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !isSnapshotName(name) {
			t.Fatalf("invalid snapshot name %q", name)
		}
		now = now.Add(time.Hour)
	}

	snapshots, err := listSnapshots(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 2 {
		t.Fatalf("retained snapshots = %d, want 2", len(snapshots))
	}
	latest := snapshots[len(snapshots)-1]
	snapshotDB, err := sql.Open("sqlite", filepath.Join(latest, databaseName))
	if err != nil {
		t.Fatal(err)
	}
	defer snapshotDB.Close()
	var count int
	if err := snapshotDB.QueryRow("SELECT COUNT(*) FROM records").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 4 {
		t.Fatalf("snapshot row count = %d, want 4", count)
	}
	gotSecret, err := os.ReadFile(filepath.Join(latest, secretsName))
	if err != nil {
		t.Fatal(err)
	}
	wantSecret, err := os.ReadFile(secrets)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotSecret) != string(wantSecret) {
		t.Fatal("sealed secrets copy differs")
	}
	for _, name := range []string{databaseName, secretsName} {
		info, err := os.Stat(filepath.Join(latest, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o, want 600", name, info.Mode().Perm())
		}
	}
}

func TestCreateCancellationPublishesNothing(t *testing.T) {
	svc, target, _ := testService(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := svc.Create(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Create error = %v, want context canceled", err)
	}
	entries, err := os.ReadDir(target)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("published entries after cancellation: %v", entries)
	}
}

func TestCreateWaitsForTargetLockAndHonorsCancellation(t *testing.T) {
	svc, target, _ := testService(t, 1)
	if err := securefile.PrepareDir(target); err != nil {
		t.Fatal(err)
	}
	release, err := acquireSnapshotLock(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := svc.Create(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Create error = %v, want deadline exceeded", err)
	}
	snapshots, err := listSnapshots(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 0 {
		t.Fatalf("published snapshots while target was locked: %v", snapshots)
	}
}

func TestRunUsesInjectedCadenceAndStops(t *testing.T) {
	svc, target, _ := testService(t, 3)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	ticks := make(chan time.Time, 1)
	svc.SetClock(func() time.Time {
		current := now
		now = now.Add(time.Hour)
		return current
	}, func(time.Duration) <-chan time.Time { return ticks })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		svc.Run(ctx)
		close(done)
	}()

	waitForSnapshots(t, target, 1)
	ticks <- time.Now()
	waitForSnapshots(t, target, 2)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler goroutine leaked after cancellation")
	}
}

func TestRestoreLatestAndRefuseRunningDaemon(t *testing.T) {
	svc, target, _ := testService(t, 2)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return now }, nil)
	if _, err := svc.Create(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec("INSERT INTO records(value) VALUES ('after')"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if _, err := svc.Create(context.Background()); err != nil {
		t.Fatal(err)
	}

	configDir := filepath.Join(t.TempDir(), "config")
	if err := os.MkdirAll(configDir, 0o750); err != nil {
		t.Fatal(err)
	}
	oldDB := testDatabase(t, filepath.Join(configDir, databaseName))
	if err := oldDB.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, secretsName), []byte("old-secrets"), 0o600); err != nil {
		t.Fatal(err)
	}

	lock, err := AcquireLock(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(context.Background(), target, configDir); !errors.Is(err, ErrDaemonRunning) {
		t.Fatalf("Restore while locked error = %v, want ErrDaemonRunning", err)
	}
	ReleaseLock(lock)

	name, err := Restore(context.Background(), target, configDir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(name, "130000") {
		t.Fatalf("restored snapshot %q, want newest", name)
	}
	restored, err := sql.Open("sqlite", filepath.Join(configDir, databaseName))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var count int
	if err := restored.QueryRow("SELECT COUNT(*) FROM records").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("restored row count = %d, want 2", count)
	}
	secret, err := os.ReadFile(filepath.Join(configDir, secretsName))
	if err != nil {
		t.Fatal(err)
	}
	if string(secret) != "sealed-ciphertext" {
		t.Fatalf("restored sealed secrets = %q", secret)
	}
}

func TestVerifySnapshotIsReadOnly(t *testing.T) {
	svc, target, _ := testService(t, 1)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return now }, nil)
	name, err := svc.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(target, name)
	before, err := os.ReadFile(filepath.Join(snapshot, databaseName))
	if err != nil {
		t.Fatal(err)
	}
	got, err := VerifySnapshot(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if got != snapshot {
		t.Fatalf("VerifySnapshot() = %q, want %q", got, snapshot)
	}
	if _, err := VerifySnapshot(context.Background(), target); err == nil {
		t.Fatal("VerifySnapshot accepted a snapshot parent instead of one exact snapshot")
	}
	after, err := os.ReadFile(filepath.Join(snapshot, databaseName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("VerifySnapshot changed the snapshot database")
	}
	if _, err := os.Stat(filepath.Join(snapshot, databaseName+"-wal")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only verify created WAL sidecar: %v", err)
	}
}

func TestRestoreReadsImmutableSnapshotWithoutSidecarWrites(t *testing.T) {
	svc, target, _ := testService(t, 1)
	svc.SetClock(func() time.Time {
		return time.Date(2026, 7, 28, 14, 0, 0, 0, time.UTC)
	}, nil)
	name, err := svc.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(target, name)
	if err := os.Chmod(snapshot, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(snapshot, 0o700) })

	configDir := filepath.Join(t.TempDir(), "config")
	if _, err := Restore(context.Background(), snapshot, configDir); err != nil {
		t.Fatalf("restore immutable snapshot: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Lstat(filepath.Join(snapshot, databaseName+suffix)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("snapshot sidecar %s was created", suffix)
		}
	}
}

func TestRestoreRejectsUnsafeSnapshotBeforeReplacingState(t *testing.T) {
	svc, target, _ := testService(t, 1)
	svc.SetClock(func() time.Time {
		return time.Date(2026, 7, 28, 15, 0, 0, 0, time.UTC)
	}, nil)
	name, err := svc.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(target, name)
	if err := os.Chmod(filepath.Join(snapshot, secretsName), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(snapshot, secretsName), 0o600) })

	configDir := filepath.Join(t.TempDir(), "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(configDir, databaseName)
	secretsPath := filepath.Join(configDir, secretsName)
	if err := os.WriteFile(databasePath, []byte("existing-database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretsPath, []byte("existing-secrets"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(context.Background(), snapshot, configDir); err == nil {
		t.Fatal("restore accepted a group-readable snapshot secret")
	}
	for path, want := range map[string]string{
		databasePath: "existing-database",
		secretsPath:  "existing-secrets",
	} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("pre-restore state changed at %s: %q, err=%v", path, got, err)
		}
	}
}

func TestIncompleteAndForeignDirectoriesAreIgnored(t *testing.T) {
	svc, target, _ := testService(t, 1)
	if err := os.MkdirAll(filepath.Join(target, snapshotPrefix+"20260728T120000.000000000Z"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(target, "operator-files"), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 13, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return now }, nil)
	if _, err := svc.Create(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "operator-files")); err != nil {
		t.Fatal("retention removed foreign directory")
	}
	if _, err := os.Stat(filepath.Join(target, snapshotPrefix+"20260728T120000.000000000Z")); err != nil {
		t.Fatal("retention removed incomplete snapshot directory")
	}
}

func TestListSnapshotNamesReturnsOnlyCanonicalCompleteBasenames(t *testing.T) {
	svc, target, _ := testService(t, 2)
	svc.SetClock(func() time.Time {
		return time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	}, nil)
	want, err := svc.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(target, snapshotPrefix+"20260829T120000.000000000Z"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(target, "operator-files"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(target, want), filepath.Join(target, snapshotPrefix+"20260828T120000.000000000Z")); err != nil {
		t.Fatal(err)
	}

	names, err := ListSnapshotNames(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != want || strings.Contains(names[0], string(filepath.Separator)) {
		t.Fatalf("ListSnapshotNames() = %q, want basename %q", names, want)
	}

	symlinkRoot := filepath.Join(t.TempDir(), "backups")
	if err := os.Symlink(target, symlinkRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := ListSnapshotNames(symlinkRoot); err == nil {
		t.Fatal("ListSnapshotNames accepted a symlink root")
	}
}

func waitForSnapshots(t *testing.T, target string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snapshots, _ := listSnapshots(target)
		if len(snapshots) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d snapshots", want)
}

func FuzzSnapshotName(f *testing.F) {
	f.Add("darkharrbor-20260728T120000.000000000Z")
	f.Add("../darkharrbor-20260728T120000.000000000Z")
	f.Fuzz(func(t *testing.T, input string) {
		ok := isSnapshotName(input)
		if ok && (strings.Contains(input, "/") || strings.Contains(input, "\\") || len(input) != len(snapshotPrefix)+len(snapshotLayout)) {
			t.Fatalf("unsafe snapshot name accepted: %q", input)
		}
	})
}

func TestOnlineSnapshotIsConsistentDuringWrites(t *testing.T) {
	svc, target, _ := testService(t, 1)
	if _, err := svc.db.Exec("CREATE TABLE paired(id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 14, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return now }, nil)

	started := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		for i := 0; i < 250; i++ {
			tx, err := svc.db.Begin()
			if err != nil {
				writerDone <- err
				return
			}
			if _, err = tx.Exec("INSERT INTO records(value) VALUES ('concurrent')"); err == nil {
				_, err = tx.Exec("INSERT INTO paired DEFAULT VALUES")
			}
			if err != nil {
				_ = tx.Rollback()
				writerDone <- err
				return
			}
			if err := tx.Commit(); err != nil {
				writerDone <- err
				return
			}
			if i == 0 {
				close(started)
			}
		}
		writerDone <- nil
	}()
	<-started

	if _, err := svc.Create(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}

	snapshots, err := listSnapshots(target)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(snapshots[0], databaseName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var records, paired int
	if err := db.QueryRow("SELECT (SELECT COUNT(*) FROM records), (SELECT COUNT(*) FROM paired)").Scan(&records, &paired); err != nil {
		t.Fatal(err)
	}
	if records != paired+1 {
		t.Fatalf("inconsistent online snapshot: records=%d paired=%d", records, paired)
	}
}
