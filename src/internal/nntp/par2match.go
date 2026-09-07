package nntp

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"math"
	"strings"

	archiveparser "github.com/darkharrbor/darkharrbor/internal/archive"
)

const (
	par2IdentityPrefixBytes        = 16 << 10
	maxPAR2IdentityCandidates      = 256
	maxPAR2IdentityDeclaredBytes   = 64 << 20
	par2IdentitySizeTolerance      = 0.08
	par2IdentityAmbiguityTolerance = 0.001
)

// PAR2FileMatch binds one recovered PAR2 FileDesc identity to the exact file
// index in ParseNZB's stable size-sorted list. The index, not a copied/renamed
// subject, is the playback identity used by the resolver and WebDAV path.
type PAR2FileMatch struct {
	NZBFileIdx int
	File       NZBFile
	Desc       archiveparser.PAR2FileDesc
}

type par2CandidateIdentity struct {
	NZBFileIdx int
	File       NZBFile
	Hash16K    string
}

func par2RelativeSizeDiff(declared, recovered int64) float64 {
	if declared <= 0 || recovered <= 0 {
		return math.Inf(1)
	}
	diff := float64(declared-recovered) / float64(recovered)
	if diff < 0 {
		diff = -diff
	}
	return diff
}

func par2SizePlausible(declared, recovered int64) bool {
	return par2RelativeSizeDiff(declared, recovered) <= par2IdentitySizeTolerance
}

func par2IdentityCandidateIndexes(info archiveparser.PAR2Info, files []NZBFile, par2FileIndex int) (indexes []int, truncated bool) {
	var declared int64
	for idx, file := range files {
		if idx == par2FileIndex || len(file.Segments) == 0 || file.TotalBytes <= 0 {
			continue
		}
		if strings.Contains(strings.ToLower(file.Subject), ".par2") {
			continue
		}
		plausible := false
		for _, desc := range info.Files {
			if par2SizePlausible(file.TotalBytes, desc.Length) {
				plausible = true
				break
			}
		}
		if !plausible {
			continue
		}
		if len(indexes) >= maxPAR2IdentityCandidates {
			truncated = true
			continue
		}
		first := file.Segments[0].Bytes
		if first <= 0 {
			first = file.TotalBytes
		}
		first = min(first, int64(par2IdentityPrefixBytes))
		if first > maxPAR2IdentityDeclaredBytes-declared {
			truncated = true
			continue
		}
		indexes = append(indexes, idx)
		declared += first
	}
	return indexes, truncated
}

// MatchPAR2FilesForItem computes the exact PAR2 MD5-16k identity for each
// bounded size-plausible NZB candidate and returns only unambiguous matches.
// A partial result may accompany a non-nil error when some candidate reads
// failed or the fixed scan budget was reached; callers may use direct-video
// matches but must require complete RAR coverage before applying archive names.
func (p *NNTPProvider) MatchPAR2FilesForItem(
	ctx context.Context,
	itemID string,
	nzb NZB,
	info archiveparser.PAR2Info,
	par2FileIndex int,
) ([]PAR2FileMatch, error) {
	indexes, truncated := par2IdentityCandidateIndexes(info, nzb.Files, par2FileIndex)
	identities := make([]par2CandidateIdentity, 0, len(indexes))
	readFailures := 0
	for _, idx := range indexes {
		prefix, err := p.readNZBFilePrefix(ctx, itemID, idx, nzb.Files[idx], par2IdentityPrefixBytes)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			readFailures++
			continue
		}
		sum := md5.Sum(prefix)
		identities = append(identities, par2CandidateIdentity{
			NZBFileIdx: idx,
			File:       nzb.Files[idx],
			Hash16K:    hex.EncodeToString(sum[:]),
		})
	}

	matches := matchPAR2FileIdentities(info, identities)
	if truncated || readFailures > 0 {
		return matches, fmt.Errorf("par2: identity scan incomplete (budget_truncated=%t read_failures=%d)", truncated, readFailures)
	}
	return matches, nil
}

// matchPAR2FileIdentities is the deterministic assignment core. Exact
// MD5-16k equality is primary; the existing 8% declared-vs-decoded size band
// is only a secondary guard and tie-break. Two same-hash candidates whose
// size distances are within 0.1 percentage points remain ambiguous.
func matchPAR2FileIdentities(info archiveparser.PAR2Info, identities []par2CandidateIdentity) []PAR2FileMatch {
	used := make(map[int]bool)
	matches := make([]PAR2FileMatch, 0, len(info.Files))
	for _, desc := range info.Files {
		if desc.Hash16K == "" || desc.Length <= 0 {
			continue
		}
		bestIdx := -1
		bestDiff := math.Inf(1)
		secondDiff := math.Inf(1)
		for idx, identity := range identities {
			if used[identity.NZBFileIdx] || !strings.EqualFold(identity.Hash16K, desc.Hash16K) {
				continue
			}
			diff := par2RelativeSizeDiff(identity.File.TotalBytes, desc.Length)
			if diff > par2IdentitySizeTolerance {
				continue
			}
			if diff < bestDiff {
				secondDiff = bestDiff
				bestDiff = diff
				bestIdx = idx
			} else if diff < secondDiff {
				secondDiff = diff
			}
		}
		if bestIdx < 0 || secondDiff-bestDiff <= par2IdentityAmbiguityTolerance {
			continue
		}
		identity := identities[bestIdx]
		used[identity.NZBFileIdx] = true
		matches = append(matches, PAR2FileMatch{
			NZBFileIdx: identity.NZBFileIdx,
			File:       identity.File,
			Desc:       desc,
		})
	}
	return matches
}
