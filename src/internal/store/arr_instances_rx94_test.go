package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newArrInstanceTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "arr-instances.db"), 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrationsFS(db, EmbeddedMigrations); err != nil {
		t.Fatalf("RunMigrationsFS: %v", err)
	}
	return New(db)
}

// RD-34 D8: the 0045 migration must leave EXISTING instances not-addable, so an
// upgrade can never by itself cause content to be added to any arr.
func TestArrInstanceDestinationDefaultsAreNotAddable(t *testing.T) {
	ctx := context.Background()
	st := newArrInstanceTestStore(t)

	if err := st.UpsertArrInstance(ctx, ArrInstance{
		Name: "radarr", URL: "http://radarr:7878", AppType: "radarr",
		APIKeyRef: "HARRBOR_ARR_RADARR_APIKEY",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	instances, err := st.ListArrInstances(ctx)
	if err != nil || len(instances) != 1 {
		t.Fatalf("list: %v %+v", err, instances)
	}
	if instances[0].CanAdd() || instances[0].RootFolder != "" || instances[0].QualityProfile != 0 {
		t.Fatalf("a freshly discovered instance must not be addable: %+v", instances[0])
	}

	if err := st.SetArrInstanceDestination(ctx, "radarr", "/movies", 14); err != nil {
		t.Fatalf("set destination: %v", err)
	}
	instances, _ = st.ListArrInstances(ctx)
	if !instances[0].CanAdd() || instances[0].QualityProfile != 14 {
		t.Fatalf("captured destination not applied: %+v", instances[0])
	}

	// Rediscovery must NOT clear an operator-captured destination.
	if err := st.UpsertArrInstance(ctx, ArrInstance{
		Name: "radarr", URL: "http://radarr:7878", AppType: "radarr",
		APIKeyRef: "HARRBOR_ARR_RADARR_APIKEY",
	}); err != nil {
		t.Fatalf("rediscover: %v", err)
	}
	instances, _ = st.ListArrInstances(ctx)
	if !instances[0].CanAdd() || instances[0].RootFolder != "/movies" {
		t.Fatalf("rediscovery cleared the captured destination: %+v", instances[0])
	}

	if err := st.SetArrInstanceDestination(ctx, "nope", "/x", 1); err == nil {
		t.Fatal("setting a destination on an unknown instance must fail")
	}
}
