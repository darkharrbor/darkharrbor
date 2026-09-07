package rangecache_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
	"github.com/darkharrbor/darkharrbor/internal/rangecache"
)

func TestYieldForPromotionEvictsCacheBeforeFailingFloor(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	cache := rangecache.New(rangecache.Config{DiskCachePath: cacheDir, DiskCacheSizeMB: 1, PinnedBudgetMB: 1}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(cache.Close)
	source, err := rangecache.NewByteSourceBlocks(bytesource.NewMemSource("safe", []byte("cached bytes")), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.GetChunkPinned(t.Context(), source, source.Chunks()[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.YieldForPromotion(context.Background(), filepath.Join(dir, "media.mkv"), math.MaxInt64, 99); err == nil {
		t.Fatal("impossible floor succeeded")
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			t.Fatalf("cache payload did not yield: %s", entry.Name())
		}
	}
	if _, err := cache.YieldForPromotion(context.Background(), filepath.Join(dir, "media.mkv"), 1, 1); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("small promotion rejected: %v", err)
	}
}
