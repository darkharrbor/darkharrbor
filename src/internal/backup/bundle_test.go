package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestEncryptedBundleRoundTripAndAuthentication(t *testing.T) {
	svc, snapshots, _ := testService(t, 2)
	created, err := svc.Create(context.Background())
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	password := "test-recovery-key"
	exports := filepath.Join(t.TempDir(), "exports")
	bundleName, err := Export(context.Background(), snapshots, exports, password)
	if err != nil {
		t.Fatalf("export snapshot: %v", err)
	}
	if bundleName != created+bundleSuffix {
		t.Fatalf("bundle name = %q, want %q", bundleName, created+bundleSuffix)
	}
	bundlePath := filepath.Join(exports, bundleName)
	info, err := os.Stat(bundlePath)
	if err != nil {
		t.Fatalf("stat bundle: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("bundle mode = %o, want 600", info.Mode().Perm())
	}

	imports := filepath.Join(t.TempDir(), "imports")
	imported, err := Import(context.Background(), bundlePath, imports, password)
	if err != nil {
		t.Fatalf("import bundle: %v", err)
	}
	if !completeSnapshot(filepath.Join(imports, imported)) {
		t.Fatal("import did not publish a complete snapshot")
	}
	if err := checkDatabase(context.Background(), filepath.Join(imports, imported, databaseName)); err != nil {
		t.Fatalf("imported database: %v", err)
	}

	badTarget := filepath.Join(t.TempDir(), "wrong-key")
	if _, err := Import(context.Background(), bundlePath, badTarget, "wrong-key"); err == nil {
		t.Fatal("wrong recovery key accepted")
	}
	entries, err := os.ReadDir(badTarget)
	if err != nil {
		t.Fatalf("read failed import target: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed import left %d artifact(s)", len(entries))
	}

	blob, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatalf("read bundle for tamper test: %v", err)
	}
	blob[len(blob)-1] ^= 0x01
	if err := os.WriteFile(bundlePath, blob, 0o600); err != nil {
		t.Fatalf("tamper bundle: %v", err)
	}
	if _, err := Import(context.Background(), bundlePath, filepath.Join(t.TempDir(), "tampered"), password); err == nil {
		t.Fatal("tampered bundle accepted")
	}
}
