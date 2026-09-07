package sidecar

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

type memoryRepo struct {
	mu        sync.Mutex
	resources map[string]Resource
}

func TestSubtitleMediaTypeFailsClosed(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"show.srt": "application/x-subrip",
		"show.ASS": "text/x-ssa; charset=utf-8",
		"show.idx": "text/plain; charset=utf-8",
		"show.sub": "application/octet-stream",
	} {
		got, ok := SubtitleMediaType(name)
		if !ok || got != want {
			t.Fatalf("SubtitleMediaType(%q) = %q, %v", name, got, ok)
		}
	}
	for _, name := range []string{"", "../show.srt", "show.vtt", "show.srt\nheader"} {
		if mediaType, ok := SubtitleMediaType(name); ok || mediaType != "" {
			t.Fatalf("unsafe name %q accepted as %q", name, mediaType)
		}
	}
}

func FuzzSubtitleMediaType(f *testing.F) {
	f.Add("show.en.srt")
	f.Add("../escape.ass")
	f.Add("show.vtt")
	f.Fuzz(func(t *testing.T, name string) {
		mediaType, ok := SubtitleMediaType(name)
		if ok && mediaType == "" {
			t.Fatal("accepted subtitle has empty media type")
		}
	})
}

func TestSubtitleAssociationAndZIPRead(t *testing.T) {
	targets := []SubtitleTarget{
		{Filename: "Show.S01E01.mkv", FileID: "video-1"},
		{Filename: "Show.S01E02.mkv", FileID: "video-2"},
	}
	if name, ok := SubtitleFilename("Subs/Show.S01E02.en.srt"); !ok || name != "Show.S01E02.en.srt" {
		t.Fatalf("subtitle filename=%q ok=%t", name, ok)
	}
	if id, ok := MatchSubtitleTarget("Subs/Show.S01E02.en.srt", targets); !ok || id != "video-2" {
		t.Fatalf("target=%q ok=%t", id, ok)
	}
	for _, name := range []string{"../escape.srt", "safe/../escape.srt", "/absolute.srt", "dir\\escape.srt", "show.vtt"} {
		if _, ok := SubtitleFilename(name); ok {
			t.Fatalf("unsafe filename %q accepted", name)
		}
	}

	var raw bytes.Buffer
	zw := zip.NewWriter(&raw)
	for _, fixture := range []struct {
		name   string
		method uint16
		data   string
	}{
		{"Subs/Show.S01E01.srt", zip.Store, "stored"},
		{"Subs/Show.S01E02.en.ass", zip.Deflate, "deflated"},
		{"Subs/unrelated.srt", zip.Store, "ignored"},
	} {
		header := &zip.FileHeader{Name: fixture.name, Method: fixture.method}
		writer, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(fixture.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	payloads, err := ReadZIPSubtitles(context.Background(), bytesource.NewMemSource("zip", raw.Bytes()), targets, 64, 4, 64)
	if err != nil || len(payloads) != 2 || payloads[0].FileID != "video-1" || string(payloads[0].Bytes) != "stored" ||
		payloads[1].FileID != "video-2" || string(payloads[1].Bytes) != "deflated" {
		t.Fatalf("payloads=%+v err=%v", payloads, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadZIPSubtitles(ctx, bytesource.NewMemSource("zip", raw.Bytes()), targets, 64, 4, 64); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
}

func FuzzSubtitleFilename(f *testing.F) {
	f.Add("Subs/Show.S01E01.en.srt")
	f.Add("../escape.ass")
	f.Add("show.vtt")
	f.Fuzz(func(t *testing.T, name string) {
		filename, ok := SubtitleFilename(name)
		if ok {
			if filename == "" {
				t.Fatal("accepted empty filename")
			}
			if _, mediaOK := SubtitleMediaType(filename); !mediaOK {
				t.Fatalf("accepted filename %q has no media type", filename)
			}
		}
	})
}

func (m *memoryRepo) InsertSidecarResource(_ context.Context, resource Resource, maxResources int, maxBytes int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.resources[resource.ID]; ok {
		return nil
	}
	var count int
	var total int64
	for _, existing := range m.resources {
		if existing.ItemID == resource.ItemID {
			count++
			total += int64(len(existing.Bytes))
		}
	}
	if count >= maxResources || total+int64(len(resource.Bytes)) > maxBytes {
		return ErrCapacity
	}
	m.resources[resource.ID] = resource
	return nil
}

func (m *memoryRepo) GetSidecarResource(_ context.Context, id string) (*Resource, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	resource, ok := m.resources[id]
	if !ok {
		return nil, nil
	}
	return &resource, nil
}

func (m *memoryRepo) ListSidecarResources(_ context.Context, itemID, fileID string) ([]Resource, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var resources []Resource
	for _, resource := range m.resources {
		if resource.ItemID == itemID && resource.FileID == fileID {
			resources = append(resources, resource)
		}
	}
	return resources, nil
}

func newRegistry(t *testing.T, opts Options) *Registry {
	t.Helper()
	registry, err := New(&memoryRepo{resources: make(map[string]Resource)}, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return registry
}

func validInput() Input {
	return Input{
		ItemID: "item-1", FileID: "file-1", Kind: KindSubtitle,
		Filename: "episode.en.srt", MediaType: "application/x-subrip",
		Language: "en", Bytes: []byte("1\n00:00:00,000 --> 00:00:01,000\nHello\n"),
	}
}

func TestRegisterResolveAndList(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	registry := newRegistry(t, Options{Now: func() time.Time { return now }})
	resource, err := registry.Register(context.Background(), validInput())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !ValidResourceID(resource.ID) || resource.Path() != "/sidecar/"+resource.ID {
		t.Fatalf("invalid opaque identity: %#v", resource)
	}
	if !resource.CreatedAt.Equal(now) {
		t.Fatalf("CreatedAt = %v, want %v", resource.CreatedAt, now)
	}
	duplicate, err := registry.Register(context.Background(), validInput())
	if err != nil || duplicate.ID != resource.ID || !duplicate.CreatedAt.Equal(resource.CreatedAt) {
		t.Fatalf("duplicate registration = %#v, err=%v; want stable resource", duplicate, err)
	}
	resolved, err := registry.Resolve(context.Background(), resource.ID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	resolved.Bytes[0] = 'X'
	again, err := registry.Resolve(context.Background(), resource.ID)
	if err != nil || again.Bytes[0] != '1' {
		t.Fatalf("Resolve must return an isolated payload copy: err=%v bytes=%q", err, again.Bytes)
	}
	listed, err := registry.List(context.Background(), "item-1", "file-1")
	if err != nil || len(listed) != 1 {
		t.Fatalf("List: len=%d err=%v", len(listed), err)
	}
}

func TestRegisterRejectsMalformedAndOversized(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Input)
	}{
		{"unknown kind", func(in *Input) { in.Kind = "unknown" }},
		{"empty payload", func(in *Input) { in.Bytes = nil }},
		{"oversized payload", func(in *Input) { in.Bytes = make([]byte, 9) }},
		{"traversal filename", func(in *Input) { in.Filename = "../x.srt" }},
		{"backslash filename", func(in *Input) { in.Filename = `dir\x.srt` }},
		{"header filename", func(in *Input) { in.Filename = "x\r\nX-Evil: yes" }},
		{"malformed media type", func(in *Input) { in.MediaType = "text/plain\r\nX-Evil: yes" }},
		{"url item id", func(in *Input) { in.ItemID = "https://origin.invalid/item" }},
		{"url file id", func(in *Input) { in.FileID = "https://origin.invalid/file" }},
		{"invalid language", func(in *Input) { in.Language = "https://origin.invalid" }},
		{"url payload", func(in *Input) { in.Bytes = []byte("https://origin.invalid/subtitle") }},
		{"token payload", func(in *Input) { in.Bytes = []byte("opaque?tok=secret") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := newRegistry(t, Options{MaxResourceBytes: 8})
			in := validInput()
			in.Bytes = []byte("valid")
			tt.mutate(&in)
			if _, err := registry.Register(context.Background(), in); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestCapacityAndCancellation(t *testing.T) {
	registry := newRegistry(t, Options{
		MaxResourcesPerItem: 1,
		MaxResourceBytes:    16,
		MaxBytesPerItem:     8,
	})
	in := validInput()
	in.Bytes = []byte("12345678")
	if _, err := registry.Register(context.Background(), in); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	second := in
	second.Filename = "other.srt"
	if _, err := registry.Register(context.Background(), second); !errors.Is(err, ErrCapacity) {
		t.Fatalf("second Register error = %v, want ErrCapacity", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := registry.Register(ctx, in); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Register error = %v", err)
	}
	if _, err := registry.Resolve(ctx, "sc000000000000000000000000"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Resolve error = %v", err)
	}
}

func TestResolveRejectsUnknownID(t *testing.T) {
	registry := newRegistry(t, Options{})
	for _, id := range []string{"", "../x", "sc-not-hex", "sc000000000000000000000000"} {
		if _, err := registry.Resolve(context.Background(), id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Resolve(%q) error = %v, want ErrNotFound", id, err)
		}
	}
}
