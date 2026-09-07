package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const (
	segmentOffsetBatchSize     = 256
	segmentOffsetQueueSize     = 65_536 // holds a 40K-segment RAR plus concurrent burst headroom
	segmentOffsetFlushInterval = time.Second
	segmentOffsetWriteTimeout  = 5 * time.Second
)

var (
	ErrSegmentOffsetRecorderClosed = errors.New("segment offset recorder is closed")
	ErrSegmentOffsetRecorderFull   = errors.New("segment offset recorder queue is full")
)

type segmentOffsetRecord struct {
	itemID       string
	contentKey   string
	fileIndex    int
	segIndex     int
	decodedBytes int64
}

type segmentOffsetRecorderStats struct {
	rows         int64
	inserts      int64
	transactions int64
	failures     int64
	maxBatch     int
}

// SegmentOffsetRecorder batches decoded NNTP segment sizes onto one SQLite
// writer. RecordSegmentSize only queues work; Close drains the queue and flushes
// the final partial transaction before the database is closed.
type SegmentOffsetRecorder struct {
	store *Store
	log   *slog.Logger

	records chan segmentOffsetRecord
	sendMu  sync.RWMutex
	closed  bool

	closeOnce sync.Once
	done      chan struct{}
	closeErr  error
	stats     segmentOffsetRecorderStats
}

func NewSegmentOffsetRecorder(s *Store, log *slog.Logger) *SegmentOffsetRecorder {
	r := &SegmentOffsetRecorder{
		store:   s,
		log:     log,
		records: make(chan segmentOffsetRecord, segmentOffsetQueueSize),
		done:    make(chan struct{}),
	}
	go r.run()
	return r
}

// RecordSegmentSize queues one immutable decoded size without blocking the
// streaming path. A full queue is reported to the caller so it can retry the
// segment later instead of silently losing the record.
func (r *SegmentOffsetRecorder) RecordSegmentSize(ctx context.Context, itemID, contentKey string, fileIndex, segIndex int, decodedBytes int) error {
	r.sendMu.RLock()
	defer r.sendMu.RUnlock()
	if r.closed {
		return ErrSegmentOffsetRecorderClosed
	}
	select {
	case r.records <- segmentOffsetRecord{itemID: itemID, contentKey: contentKey, fileIndex: fileIndex, segIndex: segIndex, decodedBytes: int64(decodedBytes)}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return ErrSegmentOffsetRecorderFull
	}
}

func (r *SegmentOffsetRecorder) RecordFileTotal(ctx context.Context, itemID, contentKey string, fileIndex int, decodedBytes int64) error {
	if decodedBytes <= 0 {
		return fmt.Errorf("record file total: invalid decoded byte count")
	}
	r.sendMu.RLock()
	defer r.sendMu.RUnlock()
	if r.closed {
		return ErrSegmentOffsetRecorderClosed
	}
	select {
	case r.records <- segmentOffsetRecord{itemID: itemID, contentKey: contentKey, fileIndex: fileIndex, segIndex: -1, decodedBytes: decodedBytes}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return ErrSegmentOffsetRecorderFull
	}
}

// GetSegmentOffsets delegates reads to the underlying store so the recorder
// itself satisfies nntp.SegmentOffsetStore.
func (r *SegmentOffsetRecorder) GetSegmentOffsets(ctx context.Context, itemID, contentKey string, fileIndex, segCount int, declaredSizes []int64) ([]int64, map[int]struct{}, error) {
	return r.store.GetSegmentOffsets(ctx, itemID, contentKey, fileIndex, segCount, declaredSizes)
}

func (r *SegmentOffsetRecorder) GetFileTotal(ctx context.Context, itemID, contentKey string, fileIndex int) (int64, error) {
	return r.store.GetFileTotal(ctx, itemID, contentKey, fileIndex)
}

// Close is safe to call more than once. It prevents new sends, drains all
// already-accepted records, and waits for the final transaction.
func (r *SegmentOffsetRecorder) Close() error {
	r.closeOnce.Do(func() {
		r.sendMu.Lock()
		r.closed = true
		close(r.records)
		r.sendMu.Unlock()
		<-r.done
	})
	return r.closeErr
}

func (r *SegmentOffsetRecorder) run() {
	defer close(r.done)
	ticker := time.NewTicker(segmentOffsetFlushInterval)
	defer ticker.Stop()

	batch := make([]segmentOffsetRecord, 0, segmentOffsetBatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		rows := len(batch)
		ctx, cancel := context.WithTimeout(context.Background(), segmentOffsetWriteTimeout)
		inserted, err := r.store.recordSegmentSizes(ctx, batch)
		cancel()

		r.stats.rows += int64(rows)
		if rows > r.stats.maxBatch {
			r.stats.maxBatch = rows
		}
		if err != nil {
			r.stats.failures++
			if r.closeErr == nil {
				r.closeErr = err
			}
			if r.log != nil {
				r.log.Error("store: segment offset batch failed", "rows", rows, "error", err)
			}
		} else {
			r.stats.inserts += inserted
			r.stats.transactions++
			if r.log != nil {
				r.log.Info("store: segment offsets flushed",
					"rows", rows,
					"inserted", inserted,
					"transactions_total", r.stats.transactions)
			}
		}
		batch = batch[:0]
	}

	for {
		select {
		case record, ok := <-r.records:
			if !ok {
				flush()
				return
			}
			batch = append(batch, record)
			if len(batch) == segmentOffsetBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (s *Store) recordSegmentSizes(ctx context.Context, records []segmentOffsetRecord) (int64, error) {
	if len(records) == 0 {
		return 0, nil
	}
	return retrySQLiteBusy(ctx, func() (int64, error) {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return 0, fmt.Errorf("begin segment offset batch: %w", err)
		}
		committed := false
		defer func() {
			if !committed {
				_ = tx.Rollback()
			}
		}()

		stmt, err := tx.PrepareContext(ctx, `
			INSERT OR IGNORE INTO segment_offsets (item_id, file_index, seg_index, decoded_bytes)
			VALUES (?, ?, ?, ?)`)
		if err != nil {
			return 0, fmt.Errorf("prepare segment offset batch: %w", err)
		}
		defer stmt.Close()
		sharedStmt, err := tx.PrepareContext(ctx, `
			INSERT OR IGNORE INTO nntp_segment_offsets (content_key, file_index, seg_index, decoded_bytes)
			VALUES (?, ?, ?, ?)`)
		if err != nil {
			return 0, fmt.Errorf("prepare shared segment offset batch: %w", err)
		}
		defer sharedStmt.Close()

		var inserted int64
		for _, record := range records {
			result, err := stmt.ExecContext(ctx, record.itemID, record.fileIndex, record.segIndex, record.decodedBytes)
			if err != nil {
				return 0, fmt.Errorf("record segment size %s/%d/%d: %w", record.itemID, record.fileIndex, record.segIndex, err)
			}
			n, err := result.RowsAffected()
			if err != nil {
				return 0, fmt.Errorf("segment offset rows affected: %w", err)
			}
			inserted += n
			if record.contentKey != "" {
				if _, err := sharedStmt.ExecContext(ctx, record.contentKey, record.fileIndex, record.segIndex, record.decodedBytes); err != nil {
					return 0, fmt.Errorf("record shared segment size %s/%d/%d: %w", record.contentKey, record.fileIndex, record.segIndex, err)
				}
			}
		}
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("commit segment offset batch: %w", err)
		}
		committed = true
		return inserted, nil
	})
}

// GetSegmentOffsets returns a cumulative decoded-byte offset array and the
// exact set of segment indexes already present in SQLite. offsets[i] is the
// byte offset at which segment i starts; the final element is the total.
// Missing segments use the caller-supplied declared size.
//
// It returns nil, nil when no segments are recorded yet.
func (s *Store) GetSegmentOffsets(ctx context.Context, itemID, contentKey string, fileIndex, segCount int, declaredSizes []int64) ([]int64, map[int]struct{}, error) {
	legacyQuery := `SELECT seg_index, decoded_bytes FROM segment_offsets
		 WHERE item_id=? AND file_index=? AND seg_index>=0 AND seg_index<? ORDER BY seg_index`
	query := legacyQuery
	args := []any{itemID, fileIndex, segCount}
	if contentKey != "" {
		query = `SELECT seg_index, decoded_bytes FROM nntp_segment_offsets
			 WHERE content_key=? AND file_index=? AND seg_index>=0 AND seg_index<? ORDER BY seg_index`
		args = []any{contentKey, fileIndex, segCount}
	}
	read := func(query string, args ...any) (map[int]int64, error) {
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		sizes := make(map[int]int64)
		for rows.Next() {
			var idx int
			var decoded int64
			if err := rows.Scan(&idx, &decoded); err != nil {
				return nil, err
			}
			sizes[idx] = decoded
		}
		return sizes, rows.Err()
	}
	sizes, err := read(query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("get segment offsets %s/%d: %w", itemID, fileIndex, err)
	}
	if len(sizes) == 0 && contentKey != "" {
		sizes, err = read(legacyQuery, itemID, fileIndex, segCount)
		if err != nil {
			return nil, nil, fmt.Errorf("get legacy segment offsets %s/%d: %w", itemID, fileIndex, err)
		}
	}
	if len(sizes) == 0 {
		return nil, nil, nil
	}
	recorded := make(map[int]struct{}, len(sizes))
	for index := range sizes {
		recorded[index] = struct{}{}
	}

	offsets := make([]int64, segCount+1)
	var cumulative int64
	for i := 0; i < segCount; i++ {
		offsets[i] = cumulative
		if decoded, ok := sizes[i]; ok {
			cumulative += decoded
		} else if i < len(declaredSizes) {
			cumulative += declaredSizes[i]
		}
	}
	offsets[segCount] = cumulative
	return offsets, recorded, nil
}

func (s *Store) GetFileTotal(ctx context.Context, itemID, contentKey string, fileIndex int) (int64, error) {
	read := func(query string, args ...any) (int64, error) {
		var total int64
		err := s.db.QueryRowContext(ctx, query, args...).Scan(&total)
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return total, err
	}
	if contentKey != "" {
		total, err := read(`SELECT decoded_bytes FROM nntp_segment_offsets WHERE content_key=? AND file_index=? AND seg_index=-1`, contentKey, fileIndex)
		if err != nil {
			return 0, fmt.Errorf("get shared file total: %w", err)
		}
		if total > 0 {
			return total, nil
		}
	}
	total, err := read(`SELECT decoded_bytes FROM segment_offsets WHERE item_id=? AND file_index=? AND seg_index=-1`, itemID, fileIndex)
	if err != nil {
		return 0, fmt.Errorf("get legacy file total: %w", err)
	}
	return total, nil
}
