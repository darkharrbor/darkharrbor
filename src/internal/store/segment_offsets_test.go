package store

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func newSegmentOffsetTestStore(t *testing.T) (*Store, func()) {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "offsets.db"), 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		_ = db.Close()
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	return New(db), func() { _ = db.Close() }
}

func TestGetSegmentOffsetsReturnsExactRecordedIndexes(t *testing.T) {
	s, closeDB := newSegmentOffsetTestStore(t)
	defer closeDB()
	ctx := context.Background()
	for _, record := range []segmentOffsetRecord{
		{itemID: "item", fileIndex: 4, segIndex: 0, decodedBytes: 100},
		{itemID: "item", fileIndex: 4, segIndex: 2, decodedBytes: 90},
		{itemID: "item", fileIndex: 4, segIndex: 9, decodedBytes: 1},
	} {
		if _, err := s.recordSegmentSizes(ctx, []segmentOffsetRecord{record}); err != nil {
			t.Fatalf("recordSegmentSizes: %v", err)
		}
	}

	offsets, recorded, err := s.GetSegmentOffsets(ctx, "item", "", 4, 3, []int64{110, 110, 110})
	if err != nil {
		t.Fatalf("GetSegmentOffsets: %v", err)
	}
	if want := []int64{0, 100, 210, 300}; !reflect.DeepEqual(offsets, want) {
		t.Fatalf("offsets = %v, want %v", offsets, want)
	}
	if len(recorded) != 2 {
		t.Fatalf("recorded indexes = %v, want exactly 0 and 2", recorded)
	}
	for _, index := range []int{0, 2} {
		if _, ok := recorded[index]; !ok {
			t.Errorf("recorded index %d missing", index)
		}
	}
	if _, ok := recorded[1]; ok {
		t.Error("declared-size fallback index 1 reported as recorded")
	}
}

func TestFileTotalPersistsSeparatelyFromSegmentOffsets(t *testing.T) {
	s, closeDB := newSegmentOffsetTestStore(t)
	defer closeDB()
	recorder := NewSegmentOffsetRecorder(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	const total int64 = 5_000_000_000
	if err := recorder.RecordFileTotal(context.Background(), "item", "content", 0, total); err != nil {
		t.Fatalf("RecordFileTotal: %v", err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, err := s.GetFileTotal(context.Background(), "item", "content", 0)
	if err != nil {
		t.Fatalf("GetFileTotal: %v", err)
	}
	if got != total {
		t.Fatalf("file total = %d, want %d", got, total)
	}
	offsets, recorded, err := s.GetSegmentOffsets(context.Background(), "item", "content", 0, 1, []int64{123})
	if err != nil {
		t.Fatalf("GetSegmentOffsets: %v", err)
	}
	if offsets != nil || recorded != nil {
		t.Fatalf("file-total sentinel leaked into segment offsets: offsets=%v recorded=%v", offsets, recorded)
	}
}

func TestSegmentOffsetRecorderBatchesAndDrainsOnClose(t *testing.T) {
	s, closeDB := newSegmentOffsetTestStore(t)
	defer closeDB()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	recorder := NewSegmentOffsetRecorder(s, log)
	for i := 0; i < segmentOffsetBatchSize+1; i++ {
		if err := recorder.RecordSegmentSize(context.Background(), "item", "content", 0, i, 100+i); err != nil {
			t.Fatalf("RecordSegmentSize(%d): %v", i, err)
		}
	}
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if recorder.stats.transactions != 2 || recorder.stats.rows != segmentOffsetBatchSize+1 {
		t.Fatalf("stats = %+v, want 257 rows in 2 transactions", recorder.stats)
	}
	if recorder.stats.maxBatch > segmentOffsetBatchSize {
		t.Fatalf("max batch = %d, want <= %d", recorder.stats.maxBatch, segmentOffsetBatchSize)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM segment_offsets WHERE item_id='item'`).Scan(&count); err != nil {
		t.Fatalf("count segment offsets: %v", err)
	}
	if count != segmentOffsetBatchSize+1 {
		t.Fatalf("stored rows = %d, want %d", count, segmentOffsetBatchSize+1)
	}
	if err := recorder.RecordSegmentSize(context.Background(), "item", "content", 0, 999, 1); !errors.Is(err, ErrSegmentOffsetRecorderClosed) {
		t.Fatalf("enqueue after Close = %v, want ErrSegmentOffsetRecorderClosed", err)
	}
}

func TestSegmentOffsetRecorderReportsFlushFailure(t *testing.T) {
	s, closeDB := newSegmentOffsetTestStore(t)
	recorder := NewSegmentOffsetRecorder(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := recorder.RecordSegmentSize(context.Background(), "item", "content", 0, 0, 100); err != nil {
		t.Fatalf("RecordSegmentSize: %v", err)
	}
	closeDB()
	if err := recorder.Close(); err == nil {
		t.Fatal("Close succeeded after database was closed; want flush error")
	}
	if recorder.stats.failures != 1 {
		t.Fatalf("flush failures = %d, want 1", recorder.stats.failures)
	}
}
