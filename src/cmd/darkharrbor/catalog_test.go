package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportCatalogValidatesBeforeAtomicPublication(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "cloud.json")
	valid := `[{"kind":"webdav","title":"Example","url":"http://rclone-webdav:8080/movies/"}]`
	count, err := importCatalog(strings.NewReader(valid), path)
	if err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("private catalog mode=%v", info.Mode())
	}
	if _, err := importCatalog(strings.NewReader(`[{"kind":"bogus"}]`), path); err == nil {
		t.Fatal("invalid replacement was accepted")
	}
	if _, err := importCatalog(strings.NewReader(`[{"kind":"webdav","title":"Example","url":"http://safe.invalid/","url":"http://other.invalid/"}]`), path); err == nil {
		t.Fatal("duplicate-field replacement was accepted")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != valid {
		t.Fatal("invalid replacement changed the previous catalog")
	}
}
