package api

import (
	"testing"
	"time"
)

// PERF-4: the skip is only safe when the passthrough would have re-requested
// the very URL the capability probe just failed to reach. These pin the exact
// mirroring of the passthrough's own "capability probing may have reresolved
// to a fresh node" lookup, so the skip can never discard an attempt that
// would have gone somewhere new.
func TestPassthroughWouldReuseURL(t *testing.T) {
	const (
		itemID = "item-1"
		fileID = "file-1"
		probed = "https://cdn.example/a?token=1"
		fresh  = "https://cdn2.example/b?token=2"
	)

	t.Run("no cached entry reuses the probed URL", func(t *testing.T) {
		s := &Server{}
		if !s.passthroughWouldReuseURL(itemID, fileID, probed) {
			t.Fatal("with nothing cached the passthrough keeps the probed URL, so the skip is safe")
		}
	})

	t.Run("cached entry identical to the probed URL", func(t *testing.T) {
		s := &Server{}
		s.cacheResolveEntry(itemID, fileID, probed, "video/mp4", 100, nil)
		if !s.passthroughWouldReuseURL(itemID, fileID, probed) {
			t.Fatal("identical cached URL means the passthrough would re-dial the same dead origin")
		}
	})

	t.Run("cached entry re-resolved to a different URL", func(t *testing.T) {
		s := &Server{}
		s.cacheResolveEntry(itemID, fileID, fresh, "video/mp4", 100, nil)
		if s.passthroughWouldReuseURL(itemID, fileID, probed) {
			t.Fatal("a fresher cached URL is a genuinely different attempt and must NOT be skipped")
		}
	})

	t.Run("expired cached entry falls back to the probed URL", func(t *testing.T) {
		s := &Server{}
		s.resolveCache.Store(resolveKey(itemID, fileID), &resolveCacheEntry{
			cdnURL:     fresh,
			resolvedAt: time.Now().Add(-resolveTTL - time.Second),
		})
		// resolveEntryFor drops the expired entry, so the passthrough sees
		// no override and reuses the probed URL -- same conclusion as the
		// empty-cache case, reached by a different route.
		if !s.passthroughWouldReuseURL(itemID, fileID, probed) {
			t.Fatal("an expired entry cannot make the passthrough a different attempt")
		}
	})

	t.Run("a different file's entry does not leak across", func(t *testing.T) {
		s := &Server{}
		s.cacheResolveEntry(itemID, "other-file", fresh, "video/mp4", 100, nil)
		if !s.passthroughWouldReuseURL(itemID, fileID, probed) {
			t.Fatal("another file's resolve must not influence this file's skip decision")
		}
	})
}
