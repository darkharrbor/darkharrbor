package store

import (
	"context"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestMigration0019UpDown(t *testing.T) {
	s, closeDB := newSegmentOffsetTestStore(t)
	defer closeDB()

	assertTable := func(name string, want int) {
		t.Helper()
		var count int
		if err := s.db.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != want {
			t.Fatalf("table %s count=%d, want %d", name, count, want)
		}
	}
	assertTable("nntp_artifacts", 1)
	assertTable("nntp_segment_offsets", 1)

	gooseMu.Lock()
	goose.SetBaseFS(EmbeddedMigrations)
	defer func() {
		goose.SetBaseFS(nil)
		gooseMu.Unlock()
	}()
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownTo(s.db, "migrations", 18); err != nil {
		t.Fatal(err)
	}
	assertTable("nntp_artifacts", 0)
	assertTable("nntp_segment_offsets", 0)
	if err := goose.UpTo(s.db, "migrations", 19); err != nil {
		t.Fatal(err)
	}
	assertTable("nntp_artifacts", 1)
	assertTable("nntp_segment_offsets", 1)
	if _, err := s.db.Exec(`INSERT INTO nntp_segment_offsets VALUES
		('nzb:unsafe',0,0,100),('nzb:safe:file:abc',0,0,100)`); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(s.db, "migrations", 20); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM nntp_segment_offsets`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("safe offset rows after migration 0020=%d, want 1", rows)
	}
}
