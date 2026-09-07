package main

import (
	"context"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/store"
)

func TestFailNestedArchivePersistsDistinctFailure(t *testing.T) {
	st := newDispatchStore(t)
	item := httpItem(t, st, "nested-archive", store.StateResolving)

	failNestedArchive(context.Background(), testLogger(), st, item, "resolveZIP")

	got, err := st.GetItemByID(context.Background(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.StateFailed {
		t.Fatalf("state = %q, want %q", got.State, store.StateFailed)
	}
	if got.ErrorMessage == nil || *got.ErrorMessage != nestedArchiveFailure {
		t.Fatalf("error message = %v, want distinct nested-archive failure", got.ErrorMessage)
	}
}
