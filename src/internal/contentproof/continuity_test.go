package contentproof_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/contentproof"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

func TestLedgerDetectsMutationAndAuthoritativeConflict(t *testing.T) {
	ledger, graph, st, now := newLedger(t, 8)
	ctx := context.Background()

	first := observation("representation", 4096, []byte("first bytes"))
	entry, err := ledger.Observe(ctx, first)
	if err != nil || entry.Relation != contentproof.ContinuityTOFU || entry.Mutation {
		t.Fatalf("first Observe = %+v, %v", entry, err)
	}
	*now = now.Add(time.Second)
	entry, err = ledger.Observe(ctx, first)
	if err != nil || entry.Relation != contentproof.ContinuityTOFU || entry.Mutation {
		t.Fatalf("matching Observe = %+v, %v", entry, err)
	}

	changed := observation("representation", 4096, []byte("other bytes"))
	*now = now.Add(time.Second)
	entry, err = ledger.Observe(ctx, changed)
	if err != nil || entry.Relation != contentproof.ContinuityTOFU || !entry.Mutation {
		t.Fatalf("mutated Observe = %+v, %v", entry, err)
	}

	proof := authoritativeProof(changed, contentproof.ProvenanceHTTPDigest, *now)
	if err := graph.Record(ctx, proof); err != nil {
		t.Fatalf("Record matching proof: %v", err)
	}
	*now = now.Add(time.Second)
	entry, err = ledger.Observe(ctx, changed)
	if err != nil || entry.Relation != contentproof.ContinuityAuthoritative || entry.Mutation {
		t.Fatalf("authoritative Observe = %+v, %v", entry, err)
	}

	conflict := authoritativeProof(changed, contentproof.ProvenanceTorrentMerkle, *now)
	conflict.Digest = digest([]byte("different authoritative bytes"))
	if err := graph.Record(ctx, conflict); err != nil {
		t.Fatalf("Record conflicting proof: %v", err)
	}
	*now = now.Add(time.Second)
	entry, err = ledger.Observe(ctx, changed)
	if err != nil || entry.Relation != contentproof.ContinuityConflict {
		t.Fatalf("conflicting Observe = %+v, %v", entry, err)
	}

	// A new ledger over the same store models a daemon restart.
	restarted, err := contentproof.NewLedger(st, contentproof.Options{
		MaxProofsPerRepresentation: 8,
		Now:                        func() time.Time { return *now },
	})
	if err != nil {
		t.Fatalf("NewLedger restart: %v", err)
	}
	history, err := restarted.History(ctx, "representation")
	if err != nil {
		t.Fatalf("History after restart: %v", err)
	}
	if len(history) != 5 || !history[2].Mutation ||
		history[3].Relation != contentproof.ContinuityAuthoritative ||
		history[4].Relation != contentproof.ContinuityConflict {
		t.Fatalf("restart history = %+v", history)
	}
}

func TestLedgerBoundsStateRejectsSecretsAndHonorsCancellation(t *testing.T) {
	ledger, _, st, now := newLedger(t, 4)
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		*now = now.Add(time.Second)
		if _, err := ledger.Observe(ctx, observation("bounded", int64(i), []byte{byte(i)})); err != nil {
			t.Fatalf("Observe %d: %v", i, err)
		}
	}
	history, err := ledger.History(ctx, "bounded")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(history) != 4 || history[0].Offset != 2 || history[3].Offset != 5 {
		t.Fatalf("bounded history = %+v", history)
	}

	bad := observation("https://origin.invalid/file?tok=secret", 0, []byte("bytes"))
	if _, err := ledger.Observe(ctx, bad); err == nil || !strings.Contains(err.Error(), "opaque secret-free ID") {
		t.Fatalf("URL-shaped observation error = %v", err)
	}
	bad = observation("safe-id", 0, []byte("bytes"))
	bad.Digest = bad.Digest[:31]
	if _, err := ledger.Observe(ctx, bad); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("short digest error = %v", err)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := ledger.Observe(canceled, observation("safe-id", 0, []byte("bytes"))); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Observe error = %v", err)
	}
	if _, err := ledger.History(canceled, "safe-id"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled History error = %v", err)
	}

	var leaked int
	if err := st.DB().QueryRow(`
		SELECT COUNT(*) FROM representation_continuity
		WHERE representation_id LIKE '%http%' OR representation_id LIKE '%tok=%'`).Scan(&leaked); err != nil {
		t.Fatalf("redaction scan: %v", err)
	}
	if leaked != 0 {
		t.Fatalf("URL/token-shaped durable rows = %d, want 0", leaked)
	}
}

func TestLedgerConcurrentObservationsStayBounded(t *testing.T) {
	ledger, _, _, _ := newLedger(t, 8)
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := ledger.Observe(context.Background(), observation("concurrent", int64(i%2), []byte{byte(i)}))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Observe: %v", err)
		}
	}
	history, err := ledger.History(context.Background(), "concurrent")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(history) != 8 {
		t.Fatalf("bounded concurrent history = %d, want 8", len(history))
	}
}

func newLedger(t *testing.T, max int) (*contentproof.Ledger, *contentproof.Graph, *store.Store, *time.Time) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "continuity.db"), time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	st := store.New(db)
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	opts := contentproof.Options{MaxProofsPerRepresentation: max, Now: func() time.Time { return now }}
	graph, err := contentproof.New(st, opts)
	if err != nil {
		t.Fatalf("New graph: %v", err)
	}
	ledger, err := contentproof.NewLedger(st, opts)
	if err != nil {
		t.Fatalf("NewLedger: %v", err)
	}
	return ledger, graph, st, &now
}

func observation(id string, offset int64, data []byte) contentproof.Observation {
	return contentproof.Observation{
		RepresentationID: id,
		Offset:           offset,
		Length:           int64(len(data)),
		Digest:           digest(data),
	}
}

func authoritativeProof(observation contentproof.Observation, provenance contentproof.Provenance, observed time.Time) contentproof.Evidence {
	scope := contentproof.ScopeBlock
	if observation.Offset == 0 {
		scope = contentproof.ScopeWhole
	}
	return contentproof.Evidence{
		RepresentationID: observation.RepresentationID,
		Scope:            scope,
		Offset:           observation.Offset,
		Length:           observation.Length,
		Kind:             contentproof.KindAuthoritative,
		Algorithm:        contentproof.AlgorithmSHA256,
		Digest:           append([]byte(nil), observation.Digest...),
		Provenance:       provenance,
		ObservedAt:       observed,
	}
}

func digest(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}
