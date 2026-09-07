package nntp

import (
	"context"
	"testing"
)

func TestFindPAR2FilePrefersSmallIndexOverVolume(t *testing.T) {
	files := []NZBFile{
		{Subject: `"release.vol000+004.par2"`, TotalBytes: 400000},
		{Subject: `"release.par2"`, TotalBytes: 5000},
		{Subject: `"release.vol004+008.par2"`, TotalBytes: 800000},
		{Subject: `"release.mkv"`, TotalBytes: 900000000},
	}
	got, idx, ok := FindPAR2File(files)
	if !ok || idx != 1 || got.Subject != `"release.par2"` {
		t.Fatalf("FindPAR2File = (%+v, %d, %v), want index 1 (bare index)", got, idx, ok)
	}
}

func TestFindPAR2FileFallsBackToSmallestVolume(t *testing.T) {
	files := []NZBFile{
		{Subject: `"release.vol004+008.par2"`, TotalBytes: 800000},
		{Subject: `"release.vol000+004.par2"`, TotalBytes: 400000},
		{Subject: `"release.mkv"`, TotalBytes: 900000000},
	}
	got, idx, ok := FindPAR2File(files)
	if !ok || idx != 1 || got.Subject != `"release.vol000+004.par2"` {
		t.Fatalf("FindPAR2File = (%+v, %d, %v), want index 1 (smallest volume)", got, idx, ok)
	}
}

func TestFindPAR2FileNoneFound(t *testing.T) {
	files := []NZBFile{
		{Subject: `"release.mkv"`, TotalBytes: 900000000},
		{Subject: `"release.nfo"`, TotalBytes: 2000},
	}
	if _, _, ok := FindPAR2File(files); ok {
		t.Fatal("expected no PAR2 file found")
	}
}

func TestFetchPAR2ForItemNoFileInNZB(t *testing.T) {
	p := &NNTPProvider{}
	nzb := NZB{Files: []NZBFile{{Subject: `"release.mkv"`, TotalBytes: 900000000}}}
	info, idx, ok, err := p.FetchPAR2ForItem(context.Background(), "item-1", nzb)
	if err != nil || ok || idx != -1 || len(info.Files) != 0 {
		t.Fatalf("FetchPAR2ForItem = (%+v, %d, %v, %v), want (zero, -1, false, nil)", info, idx, ok, err)
	}
}

func TestPAR2MagicCandidateIndexesAreBoundedAndSmallestFirst(t *testing.T) {
	files := make([]NZBFile, 20)
	for i := range files {
		files[i] = NZBFile{
			TotalBytes: int64(20 - i),
			Segments:   []NZBSegment{{Bytes: 1000}},
		}
	}
	indexes := par2MagicCandidateIndexes(files)
	if len(indexes) != maxPAR2MagicCandidates {
		t.Fatalf("candidate count = %d, want %d", len(indexes), maxPAR2MagicCandidates)
	}
	if indexes[0] != 19 || indexes[len(indexes)-1] != 4 {
		t.Fatalf("indexes = %v, want smallest-first parsed indexes 19..4", indexes)
	}
}
