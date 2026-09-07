package api

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

// newAvailabilityTestServer builds a minimal *Server sufficient for
// exercising the two TS-5.1 writer entry points, reusing repair_test.go's
// established newRepairTestStore helper for the underlying store.
func newAvailabilityTestServer(t *testing.T, st *store.Store) *Server {
	t.Helper()
	cfg := &config.Config{}
	cfg.Availability.TTLHours = 1 // AvailabilityTTL() = 1h, well within test bounds
	return &Server{
		cfg:   cfg,
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		store: st,
	}
}

func TestRecordSubmitAvailability_PersistsObservation(t *testing.T) {
	ctx := context.Background()
	st := newRepairTestStore(t)
	s := newAvailabilityTestServer(t, st)

	hash := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	item := &store.Item{InfoHash: &hash}
	s.recordSubmitAvailability(ctx, item, "TorBox", true)

	ha, found, err := st.GetHashAvailability(ctx, "torbox", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil || !found {
		t.Fatalf("expected a persisted observation: found=%v err=%v", found, err)
	}
	if !ha.Cached || ha.Source != "submit" {
		t.Fatalf("unexpected observation: %+v", ha)
	}
}

func TestRecordSubmitAvailability_AbstainsWithoutKnownHash(t *testing.T) {
	ctx := context.Background()
	st := newRepairTestStore(t)
	s := newAvailabilityTestServer(t, st)

	// No InfoHash and no RealInfoHash — torrentRepairIdentity returns "".
	s.recordSubmitAvailability(ctx, &store.Item{}, "torbox", true)

	var n int
	if err := st.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM hash_availability").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected no row written when hash is unknown, got %d", n)
	}
}

func TestRecordStreamAvailability_PersistsObservationUsingItemProvider(t *testing.T) {
	ctx := context.Background()
	st := newRepairTestStore(t)
	s := newAvailabilityTestServer(t, st)

	hash := "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	provName := "realdebrid"
	item := &store.Item{InfoHash: &hash, Provider: &provName}
	s.recordStreamAvailability(ctx, item, false)

	ha, found, err := st.GetHashAvailability(ctx, "realdebrid", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if err != nil || !found {
		t.Fatalf("expected a persisted observation: found=%v err=%v", found, err)
	}
	if ha.Cached || ha.Source != "stream" {
		t.Fatalf("unexpected observation: %+v", ha)
	}
}

func TestRecordStreamAvailability_AbstainsWhenProviderUnknown(t *testing.T) {
	ctx := context.Background()
	st := newRepairTestStore(t)
	s := newAvailabilityTestServer(t, st)

	hash := "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
	// item.Provider is nil — must never guess a provider name.
	s.recordStreamAvailability(ctx, &store.Item{InfoHash: &hash}, true)

	var n int
	if err := st.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM hash_availability").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected no row written when item.Provider is nil, got %d", n)
	}
}

func TestRecordStreamAvailability_AbstainsOnNilItem(t *testing.T) {
	ctx := context.Background()
	st := newRepairTestStore(t)
	s := newAvailabilityTestServer(t, st)

	// Must not panic on a nil item — mirrors TriggerRepairAsync's own nil
	// guard convention.
	s.recordStreamAvailability(ctx, nil, true)

	var n int
	if err := st.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM hash_availability").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected no row written for a nil item, got %d", n)
	}
}

func TestRecordHashAvailability_MalformedInputAbstains(t *testing.T) {
	ctx := context.Background()
	st := newRepairTestStore(t)
	s := newAvailabilityTestServer(t, st)

	s.recordHashAvailability(ctx, "", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", true, "submit")
	s.recordHashAvailability(ctx, "torbox", "not-hex!!", true, "submit")
	s.recordHashAvailability(ctx, "torbox", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", true, "bogus-source")

	var n int
	if err := st.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM hash_availability").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected malformed provider/hash/source to abstain (never guess), got %d rows", n)
	}
}

// Compile-time sanity: this test file never blocks — bound the whole suite.
func TestMain_AvailabilityWritersDoNotHang(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx := context.Background()
		st := newRepairTestStore(t)
		s := newAvailabilityTestServer(t, st)
		hash := "DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"
		s.recordSubmitAvailability(ctx, &store.Item{InfoHash: &hash}, "torbox", true)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("availability writer call did not return within bound")
	}
}
