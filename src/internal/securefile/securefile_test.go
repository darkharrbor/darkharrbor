package securefile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateFileBoundary(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := PrepareDir(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "secret")
	if err := AtomicWrite(path, []byte("sealed")); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path, 64)
	if err != nil || string(got) != "sealed" {
		t.Fatalf("read=%q err=%v", got, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v err=%v", info.Mode().Perm(), err)
	}
}

func TestPrivateFileRejectsSymlinkAndLooseMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := PrepareDir(dir); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(dir, "real")
	if err := os.WriteFile(real, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(link, 64); err == nil {
		t.Fatal("symlink read was accepted")
	}
	if err := AtomicWrite(link, []byte("replacement")); err == nil {
		t.Fatal("symlink destination was accepted")
	}
	if err := os.Chmod(real, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(real, 64); err == nil {
		t.Fatal("group-readable file was accepted")
	}
}

func TestPrivateFileRejectsWritableParentAndOversizeInput(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := PrepareDir(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path, 4); err == nil {
		t.Fatal("oversized private file was accepted")
	}
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path, 64); err == nil {
		t.Fatal("group-writable parent was accepted")
	}
}

func TestPrepareDirRejectsSymlinkParent(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := PrepareDir(filepath.Join(link, "child")); err == nil {
		t.Fatal("symlink parent was accepted")
	}
}

func TestOpenLockRejectsSymlinkAndLooseExistingFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := PrepareDir(dir); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "redirect.lock")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if lock, err := OpenLock(link); err == nil {
		_ = lock.Close()
		t.Fatal("symlink lock was accepted")
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "unchanged" {
		t.Fatalf("symlink target changed: %q, err=%v", got, err)
	}

	loose := filepath.Join(dir, "loose.lock")
	if err := os.WriteFile(loose, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	if lock, err := OpenLock(loose); err == nil {
		_ = lock.Close()
		t.Fatal("group-readable lock was accepted")
	}
}
