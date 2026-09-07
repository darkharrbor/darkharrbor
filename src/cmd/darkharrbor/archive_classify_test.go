package main

import (
	"context"
	"io"
	"log/slog"
	"testing"

	archiveparser "github.com/darkharrbor/darkharrbor/internal/archive"
	"github.com/darkharrbor/darkharrbor/internal/nntp"
)

func TestClassifyNZBContentWithoutConclusiveProviderAbstains(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	got, allowSubjectFallback := classifyNZBContent(context.Background(), log, nil, nil, "fixture", nntp.NZB{})
	if got.Format != archiveparser.FormatUnknown || got.FileIndex != -1 || allowSubjectFallback {
		t.Fatalf("classification = %+v, allow subject fallback = %t", got, allowSubjectFallback)
	}
}

func TestHintNZBArchiveTypePreservesPrecedence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		files []nntp.NZBFile
		want  archiveparser.Format
	}{
		{"rar", []nntp.NZBFile{{Subject: "opaque.RAR"}}, archiveparser.FormatRAR},
		{"zip", []nntp.NZBFile{{Subject: "opaque.zip"}}, archiveparser.FormatZIP},
		{"7z", []nntp.NZBFile{{Subject: "opaque.7z.001"}}, archiveparser.Format7z},
		{"rar wins", []nntp.NZBFile{{Subject: "one.zip"}, {Subject: "two.rar"}}, archiveparser.FormatRAR},
		{"unknown", []nntp.NZBFile{{Subject: "opaque.bin"}}, archiveparser.FormatUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := hintNZBArchiveType(test.files); got != test.want {
				t.Fatalf("hintNZBArchiveType() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestSelectNZBVideoFilesByEvidencePrecedence(t *testing.T) {
	t.Parallel()
	files := []nntp.NZBFile{
		{Subject: "misleading.mkv"},
		{Subject: "opaque"},
		{Subject: "Recovered.Show.S01E01.mkv"},
	}
	tests := []struct {
		name         string
		exact        map[int]bool
		magic        nntp.MagicClassification
		allowSubject bool
		wantIndex    int
		wantCount    int
		wantExact    bool
	}{
		{
			name:         "par2 exact identity wins",
			exact:        map[int]bool{2: true},
			magic:        nntp.MagicClassification{Format: archiveparser.FormatMKV, FileIndex: 1},
			allowSubject: true,
			wantIndex:    2,
			wantExact:    true,
		},
		{
			name:         "magic wins over subject",
			magic:        nntp.MagicClassification{Format: archiveparser.FormatMKV, FileIndex: 1},
			allowSubject: true,
			wantIndex:    1,
			wantExact:    true,
		},
		{
			name:         "subject is final fallback",
			magic:        nntp.MagicClassification{Format: archiveparser.FormatUnknown, FileIndex: -1},
			allowSubject: true,
			wantIndex:    0,
			wantCount:    2,
		},
		{
			name:      "ambiguous content abstains",
			magic:     nntp.MagicClassification{Format: archiveparser.FormatUnknown, FileIndex: -1, Ambiguous: true},
			wantIndex: -1,
		},
		{
			name:      "cancelled evidence abstains",
			magic:     nntp.MagicClassification{Format: archiveparser.FormatUnknown, FileIndex: -1},
			wantIndex: -1,
		},
		{
			name:         "archive magic refuses video subject",
			magic:        nntp.MagicClassification{Format: archiveparser.FormatRAR, FileIndex: 1},
			allowSubject: true,
			wantIndex:    -1,
		},
		{
			name:      "invalid direct magic index abstains",
			magic:     nntp.MagicClassification{Format: archiveparser.FormatMP4, FileIndex: len(files)},
			wantIndex: -1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, exact := selectNZBVideoFilesByEvidence(files, test.exact, test.magic, test.allowSubject)
			if test.wantIndex < 0 {
				if len(got) != 0 {
					t.Fatalf("selection = %+v, want abstention", got)
				}
				return
			}
			wantCount := test.wantCount
			if wantCount == 0 {
				wantCount = 1
			}
			if len(got) != wantCount || got[0].NZBFileIdx != test.wantIndex {
				t.Fatalf("selection = %+v, want count %d starting at index %d", got, wantCount, test.wantIndex)
			}
			if exact[got[0].NZBFileIdx] != test.wantExact {
				t.Fatalf("exact[%d] = %t, want %t", got[0].NZBFileIdx, exact[got[0].NZBFileIdx], test.wantExact)
			}
		})
	}
}
