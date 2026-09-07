package nntp

import (
	"context"
	"errors"
	"reflect"
	"testing"

	archiveparser "github.com/darkharrbor/darkharrbor/internal/archive"
)

func TestClassifyNZBMagicBoundsAndExclusions(t *testing.T) {
	t.Parallel()
	files := []NZBFile{
		magicTestFile("metadata.par2", 1024),
		magicTestFile("oversized.bin", magicMaxFirstSegmentSize+1),
		{Subject: "empty.bin"},
	}
	for i := 0; i < magicCandidateLimit+2; i++ {
		files = append(files, magicTestFile("opaque", 1024))
	}
	var fetched []int
	got, err := classifyNZBMagic(context.Background(), files, func(_ context.Context, index int, _ NZBFile) ([]byte, error) {
		fetched = append(fetched, index)
		if len(fetched) == magicCandidateLimit {
			return []byte("Rar!\x1a\x07\x00"), nil
		}
		return []byte("unknown"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Format != archiveparser.FormatRAR || got.FileIndex != 10 || got.Inspected != magicCandidateLimit {
		t.Fatalf("classification = %+v", got)
	}
	wantFetched := []int{3, 4, 5, 6, 7, 8, 9, 10}
	if !reflect.DeepEqual(fetched, wantFetched) {
		t.Fatalf("fetched = %v, want %v", fetched, wantFetched)
	}
}

func TestClassifyNZBMagicAmbiguousAbstains(t *testing.T) {
	t.Parallel()
	files := []NZBFile{magicTestFile("one", 1024), magicTestFile("two", 1024)}
	got, err := classifyNZBMagic(context.Background(), files, func(_ context.Context, index int, _ NZBFile) ([]byte, error) {
		if index == 0 {
			return []byte("PK\x03\x04"), nil
		}
		return []byte("\x1a\x45\xdf\xa3"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Format != archiveparser.FormatUnknown || !got.Ambiguous || got.FileIndex != -1 {
		t.Fatalf("classification = %+v, want ambiguous abstention", got)
	}
}

func TestClassifyNZBMagicCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	_, err := classifyNZBMagic(ctx, []NZBFile{magicTestFile("one", 1024)}, func(context.Context, int, NZBFile) ([]byte, error) {
		called = true
		return nil, errors.New("unexpected")
	})
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("err = %v, fetch called = %v", err, called)
	}
}

func TestClassifyNZBMagicFetchFailuresSafelyAbstain(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("unavailable")
	got, err := classifyNZBMagic(context.Background(), []NZBFile{magicTestFile("opaque.rar", 1024)}, func(context.Context, int, NZBFile) ([]byte, error) {
		return nil, wantErr
	})
	if !errors.Is(err, wantErr) || got.Format != archiveparser.FormatUnknown || got.Ambiguous {
		t.Fatalf("classification = %+v, err = %v", got, err)
	}
}

func TestClassifyNZBMagicPartialFetchFailureSafelyAbstains(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("unavailable")
	files := []NZBFile{magicTestFile("one", 1024), magicTestFile("two", 1024)}
	got, err := classifyNZBMagic(context.Background(), files, func(_ context.Context, index int, _ NZBFile) ([]byte, error) {
		if index == 0 {
			return []byte("\x1a\x45\xdf\xa3"), nil
		}
		return nil, wantErr
	})
	if !errors.Is(err, wantErr) || got.Format != archiveparser.FormatUnknown || got.FileIndex != -1 || got.Ambiguous {
		t.Fatalf("classification = %+v, err = %v", got, err)
	}
}

func magicTestFile(subject string, firstSegmentBytes int64) NZBFile {
	return NZBFile{Subject: subject, Segments: []NZBSegment{{Number: 1, Bytes: firstSegmentBytes, MessageID: "fixture"}}}
}
