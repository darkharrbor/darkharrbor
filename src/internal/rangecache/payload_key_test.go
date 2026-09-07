package rangecache

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
)

type payloadTestSource struct {
	key     string
	payload []byte
	fetches atomic.Int64
}

func (s *payloadTestSource) Key() string        { return s.key }
func (s *payloadTestSource) Chunks() []ChunkRef { return nil }
func (s *payloadTestSource) ExactSizes() bool   { return true }
func (s *payloadTestSource) Fetch(context.Context, ChunkRef, FetchClass) ([]byte, error) {
	s.fetches.Add(1)
	return append([]byte(nil), s.payload...), nil
}

type globallyKeyedSource struct{ *payloadTestSource }

func (s *globallyKeyedSource) PayloadKey(ref ChunkRef) string { return "global|" + ref.Key }

type transformedSource struct {
	*globallyKeyedSource
	trim     int
	observed atomic.Int64
}

func (s *transformedSource) TransformChunk(_ ChunkRef, data []byte) []byte {
	return data[s.trim:]
}
func (s *transformedSource) ObserveChunk(_ ChunkRef, data []byte) {
	s.observed.Store(int64(len(data)))
}

func TestPayloadKeyDeduplicatesAcrossSourcesAndSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	newCache := func() *Cache {
		return New(Config{DiskCachePath: dir, DiskCacheSizeMB: 1, DiskCacheTTLMin: 60}, log)
	}
	ref := ChunkRef{Index: 0, Key: "shared", Size: 7}

	first := &globallyKeyedSource{&payloadTestSource{key: "item-a", payload: []byte("payload")}}
	cache := newCache()
	if _, err := cache.GetChunk(context.Background(), first, ModeDisk, ref); err != nil {
		t.Fatal(err)
	}
	second := &globallyKeyedSource{&payloadTestSource{key: "item-b", payload: []byte("wrong")}}
	got, err := cache.GetChunk(context.Background(), second, ModeDisk, ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "payload" || second.fetches.Load() != 0 {
		t.Fatalf("cross-source result=%q fetches=%d", got, second.fetches.Load())
	}
	if got := cache.SnapshotMap()["cross_item_hits"]; got != int64(1) {
		t.Fatalf("cross_item_hits=%v, want 1", got)
	}
	cache.Close()

	restarted := newCache()
	defer restarted.Close()
	third := &globallyKeyedSource{&payloadTestSource{key: "item-c", payload: []byte("wrong")}}
	got, err = restarted.GetChunk(context.Background(), third, ModeDisk, ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "payload" || third.fetches.Load() != 0 {
		t.Fatalf("restart result=%q fetches=%d", got, third.fetches.Load())
	}
}

func TestLegacyPayloadIdentityRemainsSourceScoped(t *testing.T) {
	cache := New(Config{DiskCachePath: t.TempDir(), DiskCacheSizeMB: 1}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer cache.Close()
	ref := ChunkRef{Index: 0, Key: "ordinal", Size: 1}
	first := &payloadTestSource{key: "cdn-a", payload: []byte("a")}
	second := &payloadTestSource{key: "cdn-b", payload: []byte("b")}
	if _, err := cache.GetChunk(context.Background(), first, ModeDisk, ref); err != nil {
		t.Fatal(err)
	}
	got, err := cache.GetChunk(context.Background(), second, ModeDisk, ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "b" || second.fetches.Load() != 1 {
		t.Fatalf("legacy source result=%q fetches=%d", got, second.fetches.Load())
	}
}

func TestPayloadTransformDoesNotMutateSharedBytes(t *testing.T) {
	cache := New(Config{DiskCachePath: t.TempDir(), DiskCacheSizeMB: 1}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer cache.Close()
	ref := ChunkRef{Index: 0, Key: "article", Size: 6}
	archive := &transformedSource{
		globallyKeyedSource: &globallyKeyedSource{&payloadTestSource{key: "archive", payload: []byte("HEADERdata")}},
		trim:                6,
	}
	got, err := cache.GetChunk(context.Background(), archive, ModeDisk, ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "data" || archive.observed.Load() != 4 {
		t.Fatalf("archive result=%q observed=%d", got, archive.observed.Load())
	}
	raw := &globallyKeyedSource{&payloadTestSource{key: "raw", payload: []byte("wrong")}}
	got, err = cache.GetChunk(context.Background(), raw, ModeDisk, ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "HEADERdata" || raw.fetches.Load() != 0 {
		t.Fatalf("raw result=%q fetches=%d", got, raw.fetches.Load())
	}
}

func TestPayloadKeyConcurrentCrossSourceHits(t *testing.T) {
	cache := New(Config{DiskCachePath: t.TempDir(), DiskCacheSizeMB: 1}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer cache.Close()
	ref := ChunkRef{Index: 0, Key: "shared", Size: 7}
	seed := &globallyKeyedSource{&payloadTestSource{key: "seed", payload: []byte("payload")}}
	if _, err := cache.GetChunk(context.Background(), seed, ModeDisk, ref); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			src := &globallyKeyedSource{&payloadTestSource{key: "other", payload: []byte("wrong")}}
			got, err := cache.GetChunk(context.Background(), src, ModeDisk, ref)
			if err != nil || string(got) != "payload" || src.fetches.Load() != 0 {
				t.Errorf("result=%q fetches=%d err=%v", got, src.fetches.Load(), err)
			}
		}()
	}
	wg.Wait()
	if got := cache.SnapshotMap()["cross_item_hits"]; got != int64(64) {
		t.Fatalf("cross_item_hits=%v, want 64", got)
	}
}
