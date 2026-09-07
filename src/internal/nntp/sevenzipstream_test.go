package nntp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

func sevenZipTestManifest() (SevenZipManifest, map[int][]byte) {
	parts := map[int][]byte{1: []byte("ABCDE"), 2: []byte("FGHIJ"), 3: []byte("KLMNO")}
	volumes := make([]SevenZipVolume, 3)
	for i := range volumes {
		volumes[i] = SevenZipVolume{
			Number: i + 1, NZBFileIdx: i, Size: 5, CumOffset: int64(i * 5),
			Segments: []NZBSegment{{Number: 1, Bytes: 5}},
		}
	}
	return SevenZipManifest{
		Volumes:  volumes,
		Entries:  []SevenZipEntry{{EntryIdx: 0, Filename: "video/movie.mkv", DataOff: 3, Size: 8}},
		Sidecars: []SevenZipEntry{{EntryIdx: 0, Filename: "video/movie.srt", DataOff: 0, Size: 3}},
	}, parts
}

func TestSevenZipManifestRoundTripAndValidation(t *testing.T) {
	t.Parallel()
	manifest, _ := sevenZipTestManifest()
	raw, err := MarshalSevenZipManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalSevenZipManifest(raw)
	if err != nil || len(got.Volumes) != 3 || len(got.Entries) != 1 || len(got.Sidecars) != 1 {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	bad := manifest
	bad.Volumes = append([]SevenZipVolume(nil), manifest.Volumes...)
	bad.Volumes[1].CumOffset++
	if _, err := MarshalSevenZipManifest(bad); err == nil {
		t.Fatal("non-contiguous manifest accepted")
	}
	if _, err := UnmarshalSevenZipManifest("{"); err == nil {
		t.Fatal("malformed manifest accepted")
	}
	duplicate := manifest
	duplicate.Volumes = append([]SevenZipVolume(nil), manifest.Volumes...)
	duplicate.Volumes[1].NZBFileIdx = duplicate.Volumes[0].NZBFileIdx
	if _, err := MarshalSevenZipManifest(duplicate); err == nil {
		t.Fatal("duplicate volume file index accepted")
	}
}

func TestStreamSevenZipEntryRangeMatrix(t *testing.T) {
	t.Parallel()
	manifest, parts := sevenZipTestManifest()
	streamer := func(ctx context.Context, volume SevenZipVolume, start, end int64, dst io.Writer, flush func()) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		data := parts[volume.Number]
		_, err := dst.Write(data[start : end+1])
		if flush != nil {
			flush()
		}
		return err
	}
	for _, test := range []struct {
		name   string
		header string
		status int
		body   string
	}{
		{name: "full", status: http.StatusOK, body: "DEFGHIJK"},
		{name: "head", header: "bytes=0-2", status: http.StatusPartialContent, body: "DEF"},
		{name: "cross-volume", header: "bytes=2-5", status: http.StatusPartialContent, body: "FGHI"},
		{name: "open-ended", header: "bytes=6-", status: http.StatusPartialContent, body: "JK"},
		{name: "suffix", header: "bytes=-3", status: http.StatusPartialContent, body: "IJK"},
		{name: "clamped", header: "bytes=6-99", status: http.StatusPartialContent, body: "JK"},
		{name: "unsatisfiable", header: "bytes=8-", status: http.StatusRequestedRangeNotSatisfiable},
		{name: "malformed", header: "bytes=a-b", status: http.StatusRequestedRangeNotSatisfiable},
		{name: "multi-range", header: "bytes=0-1,3-4", status: http.StatusRequestedRangeNotSatisfiable},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			err := streamSevenZipEntry(context.Background(), manifest, 0, rec, test.header, streamer)
			if err != nil {
				t.Fatal(err)
			}
			if rec.Code != test.status || rec.Body.String() != test.body {
				t.Fatalf("response = %d %q, want %d %q", rec.Code, rec.Body.String(), test.status, test.body)
			}
		})
	}
}

func TestStreamSevenZipEntryFailsClosedAndCancels(t *testing.T) {
	t.Parallel()
	manifest, _ := sevenZipTestManifest()
	if err := streamSevenZipEntry(context.Background(), manifest, 1, httptest.NewRecorder(), "", func(context.Context, SevenZipVolume, int64, int64, io.Writer, func()) error {
		return nil
	}); err == nil {
		t.Fatal("out-of-range entry accepted")
	}
	short := func(_ context.Context, _ SevenZipVolume, _, _ int64, dst io.Writer, _ func()) error {
		_, err := dst.Write([]byte("x"))
		return err
	}
	if err := streamSevenZipEntry(context.Background(), manifest, 0, httptest.NewRecorder(), "", short); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short stream error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := func(ctx context.Context, _ SevenZipVolume, _, _ int64, _ io.Writer, _ func()) error {
		return ctx.Err()
	}
	if err := streamSevenZipEntry(ctx, manifest, 0, httptest.NewRecorder(), "", cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
}

func TestSevenZipConcatSourceCrossesVolumes(t *testing.T) {
	t.Parallel()
	source := &sevenZipConcatSource{
		sources: []bytesource.ByteSource{
			bytesource.NewMemSource("a", []byte("abc")),
			bytesource.NewMemSource("b", []byte("def")),
			bytesource.NewMemSource("c", []byte("ghi")),
		},
		starts: []int64{0, 3, 6, 9},
		key:    "fixture",
	}
	buf := make([]byte, 7)
	if n, err := source.ReadAt(context.Background(), buf, 1); err != nil || n != len(buf) || string(buf) != "bcdefgh" {
		t.Fatalf("ReadAt = %d %q %v", n, buf, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := source.ReadAt(ctx, make([]byte, 1), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
}

func TestParseSevenZipVolumeSubject(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		subject string
		stem    string
		part    int
		ok      bool
	}{
		{subject: `"Release Name.7z.001" yEnc`, stem: "release name", part: 1, ok: true},
		{subject: `"Release.7Z.010"`, stem: "release", part: 10, ok: true},
		{subject: "release.7z", stem: "release", ok: true},
		{subject: "release.7z.000", ok: false},
		{subject: "release.7z.01", ok: false},
		{subject: "release.7z.001x", ok: false},
		{subject: "release.rar", ok: false},
	} {
		stem, part, ok := parseSevenZipVolumeSubject(test.subject)
		if stem != test.stem || part != test.part || ok != test.ok {
			t.Fatalf("parse %q = (%q,%d,%v), want (%q,%d,%v)", test.subject, stem, part, ok, test.stem, test.part, test.ok)
		}
	}
}

func FuzzParseSevenZipVolumeSubject(f *testing.F) {
	f.Add(`"release.7z.001" yEnc`)
	f.Add("release.7z")
	f.Add("release.7z.000")
	f.Add("")
	f.Fuzz(func(t *testing.T, subject string) {
		stem, part, ok := parseSevenZipVolumeSubject(subject)
		if ok && (stem == "" || part < 0 || part > 999) {
			t.Fatalf("invalid accepted result: %q %d", stem, part)
		}
	})
}
