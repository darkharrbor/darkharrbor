package api

import (
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/httpstream"
)

func TestRX93SelectorStoreScopesByReleaseAndExpires(t *testing.T) {
	store := newRX93SelectorStore()
	now := time.Unix(1000, 0).UTC()
	key, ok := rx93SelectorCacheKey("mediaflow:abc", "dir/episode.mkv", 100)
	if !ok {
		t.Fatal("valid selector key rejected")
	}
	result := httpstream.SearchResult{Filename: "episode.mkv", Size: 100}
	store.Put(key, result, now)

	if got, found := store.Get(key, now.Add(time.Hour)); !found || got.Filename != result.Filename {
		t.Fatalf("stored selector missing: found=%v got=%+v", found, got)
	}
	wrongSize, _ := rx93SelectorCacheKey("mediaflow:abc", "episode.mkv", 101)
	if _, found := store.Get(wrongSize, now.Add(time.Hour)); found {
		t.Fatal("selector crossed declared-size boundary")
	}
	wrongName, _ := rx93SelectorCacheKey("mediaflow:abc", "other.mkv", 100)
	if _, found := store.Get(wrongName, now.Add(time.Hour)); found {
		t.Fatal("selector crossed filename boundary")
	}
	if _, found := store.Get(key, now.Add(rx93SelectorTTL+time.Second)); found {
		t.Fatal("expired selector retained")
	}
}

func TestRX93SelectorStoreBounded(t *testing.T) {
	store := newRX93SelectorStore()
	now := time.Unix(2000, 0).UTC()
	for i := 0; i < rx93MaxSelectors+1; i++ {
		key := rx93SelectorKey{representationID: "rep", filename: string(rune(i + 1)), total: int64(i + 1)}
		store.Put(key, httpstream.SearchResult{Filename: key.filename, Size: key.total}, now)
	}
	store.mu.Lock()
	got := len(store.entries)
	store.mu.Unlock()
	if got != rx93MaxSelectors {
		t.Fatalf("selector entries=%d want=%d", got, rx93MaxSelectors)
	}
}
