package store

import (
	"context"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
)

// TestRX96CoverageResetsOnDeclaredSizeChange is RD-32's regression, and the
// defect it guards was found in LIVE OPERATION rather than review.
//
// RD-30 D5 collapses every release of one title into a single identity-keyed
// representation, which is correct for identity. But coverage is measured in
// BYTES and byte offsets are release-specific: on 2026-08-18 one identity
// accumulated spans reaching byte 16,725,425,753 from a 4K remux and then
// shared that offset space with a much smaller release. Since the threshold is
// delivered/total against the declared size of the representation BEING
// PLAYED, retained coverage from the larger release makes the ratio exceed 1
// and commits immediately a file the viewer has barely started.
func TestRX96CoverageResetsOnDeclaredSizeChange(t *testing.T) {
	const rep = "mediaflow:b2270f7789bf42b776738645"
	const remux = int64(16_725_425_754)
	const smaller = int64(4_000_000_000)
	now := time.Date(2026, 8, 18, 14, 51, 19, 0, time.UTC)

	newStore := func(t *testing.T) *Store {
		t.Helper()
		db, err := Open(context.Background(), t.TempDir()+"/rx96.db", time.Second)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
			t.Fatalf("RunMigrationsFS: %v", err)
		}
		return New(db)
	}

	t.Run("same declared size accumulates", func(t *testing.T) {
		st := newStore(t)
		ctx := context.Background()
		if _, err := st.MergePlaybackCoverage(ctx, rep, playbackcoverage.Interval{Start: 0, End: 1_000_000}, now, 64, remux); err != nil {
			t.Fatal(err)
		}
		snap, err := st.MergePlaybackCoverage(ctx, rep, playbackcoverage.Interval{Start: 5_000_000, End: 6_000_000}, now, 64, remux)
		if err != nil {
			t.Fatal(err)
		}
		if snap.DeliveredBytes != 2_000_000 {
			t.Fatalf("a replay at the same size must accumulate, got %d", snap.DeliveredBytes)
		}
	})

	t.Run("changed declared size discards prior coverage", func(t *testing.T) {
		st := newStore(t)
		ctx := context.Background()
		// Half of the remux: the dangerous amount to retain.
		if _, err := st.MergePlaybackCoverage(ctx, rep, playbackcoverage.Interval{Start: 0, End: remux / 2}, now, 64, remux); err != nil {
			t.Fatal(err)
		}
		snap, err := st.MergePlaybackCoverage(ctx, rep, playbackcoverage.Interval{Start: 0, End: 1_000_000}, now, 64, smaller)
		if err != nil {
			t.Fatal(err)
		}
		if snap.DeliveredBytes != 1_000_000 {
			t.Fatalf("coverage from another release must be discarded, got %d", snap.DeliveredBytes)
		}
		if ratio := float64(snap.DeliveredBytes) / float64(smaller); ratio >= 1 {
			t.Fatalf("retained coverage would have crossed threshold instantly: ratio=%f", ratio)
		}
	})

	t.Run("unknown size is adopted, not treated as a mismatch", func(t *testing.T) {
		// Pre-migration rows carry 0. Upgrading must not discard live coverage.
		st := newStore(t)
		ctx := context.Background()
		if _, err := st.MergePlaybackCoverage(ctx, rep, playbackcoverage.Interval{Start: 0, End: 1_000_000}, now, 64, 0); err != nil {
			t.Fatal(err)
		}
		snap, err := st.MergePlaybackCoverage(ctx, rep, playbackcoverage.Interval{Start: 5_000_000, End: 6_000_000}, now, 64, remux)
		if err != nil {
			t.Fatal(err)
		}
		if snap.DeliveredBytes != 2_000_000 {
			t.Fatalf("unknown size must be adopted rather than discarded, got %d", snap.DeliveredBytes)
		}
	})

	t.Run("absent declared size never discards", func(t *testing.T) {
		st := newStore(t)
		ctx := context.Background()
		if _, err := st.MergePlaybackCoverage(ctx, rep, playbackcoverage.Interval{Start: 0, End: 1_000_000}, now, 64, remux); err != nil {
			t.Fatal(err)
		}
		snap, err := st.MergePlaybackCoverage(ctx, rep, playbackcoverage.Interval{Start: 5_000_000, End: 6_000_000}, now, 64, 0)
		if err != nil {
			t.Fatal(err)
		}
		if snap.DeliveredBytes != 2_000_000 {
			t.Fatalf("an observation without an extent must not reset, got %d", snap.DeliveredBytes)
		}
	})
}

// TestPlaybackCoverageWaitsForTransientStoreContention guards a live RX-9.4
// defect observed on 2026-08-20. DarkHarrbor intentionally uses one SQLite
// connection, and playback coverage used to abandon an already-delivered byte
// observation after only one second if another store operation held that
// connection. The durable streaming metadata path already allows five seconds;
// coverage must tolerate the same bounded transient contention.
func TestPlaybackCoverageWaitsForTransientStoreContention(t *testing.T) {
	db, err := Open(context.Background(), t.TempDir()+"/coverage-contention.db", time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}

	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("reserve only sqlite connection: %v", err)
	}

	released := make(chan struct{})
	go func() {
		time.Sleep(1200 * time.Millisecond)
		_ = conn.Close()
		close(released)
	}()

	tracker, err := playbackcoverage.New(New(db), playbackcoverage.Options{})
	if err != nil {
		t.Fatalf("playbackcoverage.New: %v", err)
	}
	started := time.Now()
	snap, err := tracker.Observe(context.Background(), "mediaflow:contention-regression", 0, 1_048_576)
	if err != nil {
		t.Fatalf("Observe must survive transient sqlite admission contention: %v", err)
	}
	<-released
	if elapsed := time.Since(started); elapsed < time.Second {
		t.Fatalf("test did not exercise the prior one-second failure window: elapsed=%s", elapsed)
	}
	if snap.DeliveredBytes != 1_048_576 {
		t.Fatalf("durable delivered bytes = %d, want 1048576", snap.DeliveredBytes)
	}
}
