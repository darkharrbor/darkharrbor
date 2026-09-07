package nntp

import (
	"testing"

	archiveparser "github.com/darkharrbor/darkharrbor/internal/archive"
)

func TestMatchPAR2FileIdentitiesPreservesExactNZBIndex(t *testing.T) {
	info := archiveparser.PAR2Info{Files: []archiveparser.PAR2FileDesc{{
		FileID: "a", Hash16K: "00112233445566778899aabbccddeeff",
		Name: "Show.S01E01.Real.Name.mkv", Length: 1_000,
	}}}
	identities := []par2CandidateIdentity{
		{NZBFileIdx: 0, File: NZBFile{Subject: `"wrong"`, TotalBytes: 1_010}, Hash16K: "ffeeddccbbaa99887766554433221100"},
		{NZBFileIdx: 3, File: NZBFile{Subject: `"obfuscated"`, TotalBytes: 1_020}, Hash16K: "00112233445566778899aabbccddeeff"},
	}
	matches := matchPAR2FileIdentities(info, identities)
	if len(matches) != 1 || matches[0].NZBFileIdx != 3 || matches[0].File.Subject != `"obfuscated"` {
		t.Fatalf("matches = %+v, want exact parsed NZB index 3", matches)
	}
}

func TestMatchPAR2FileIdentitiesUsesHashBeforeEqualSize(t *testing.T) {
	info := archiveparser.PAR2Info{Files: []archiveparser.PAR2FileDesc{{
		Hash16K: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Name: "release.part01.rar", Length: 1_000,
	}}}
	identities := []par2CandidateIdentity{
		{NZBFileIdx: 1, File: NZBFile{TotalBytes: 1_000}, Hash16K: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		{NZBFileIdx: 2, File: NZBFile{TotalBytes: 1_000}, Hash16K: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}
	matches := matchPAR2FileIdentities(info, identities)
	if len(matches) != 1 || matches[0].NZBFileIdx != 2 {
		t.Fatalf("matches = %+v, want matching hash at index 2", matches)
	}
}

func TestMatchPAR2FileIdentitiesAbstainsOnNearTie(t *testing.T) {
	info := archiveparser.PAR2Info{Files: []archiveparser.PAR2FileDesc{{
		Hash16K: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Name: "movie.mkv", Length: 1_000_000,
	}}}
	identities := []par2CandidateIdentity{
		{NZBFileIdx: 1, File: NZBFile{TotalBytes: 1_010_000}, Hash16K: info.Files[0].Hash16K},
		{NZBFileIdx: 2, File: NZBFile{TotalBytes: 1_010_500}, Hash16K: info.Files[0].Hash16K},
	}
	if matches := matchPAR2FileIdentities(info, identities); len(matches) != 0 {
		t.Fatalf("matches = %+v, want safe abstention on near tie", matches)
	}
}

func TestMatchPAR2FileIdentitiesRejectsOutOfTolerance(t *testing.T) {
	info := archiveparser.PAR2Info{Files: []archiveparser.PAR2FileDesc{{
		Hash16K: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Name: "movie.mkv", Length: 1_000,
	}}}
	identities := []par2CandidateIdentity{{
		NZBFileIdx: 1, File: NZBFile{TotalBytes: 1_500}, Hash16K: info.Files[0].Hash16K,
	}}
	if matches := matchPAR2FileIdentities(info, identities); len(matches) != 0 {
		t.Fatalf("matches = %+v, want out-of-tolerance abstention", matches)
	}
}

func TestPAR2IdentityCandidateIndexesAreBounded(t *testing.T) {
	info := archiveparser.PAR2Info{Files: []archiveparser.PAR2FileDesc{{Length: 1_000}}}
	files := make([]NZBFile, maxPAR2IdentityCandidates+20)
	for i := range files {
		files[i] = NZBFile{TotalBytes: 1_000, Segments: []NZBSegment{{Bytes: 1_000}}}
	}
	indexes, truncated := par2IdentityCandidateIndexes(info, files, -1)
	if len(indexes) != maxPAR2IdentityCandidates || !truncated {
		t.Fatalf("indexes=%d truncated=%v, want bounded count %d and truncation", len(indexes), truncated, maxPAR2IdentityCandidates)
	}
}
