package nntp

import (
	"context"
	"crypto/md5" // #nosec G501 -- PAR2 IFSC block hashes are defined as MD5.
	"crypto/subtle"
	"errors"
	"time"

	archiveparser "github.com/darkharrbor/darkharrbor/internal/archive"
	"github.com/darkharrbor/darkharrbor/internal/contentproof"
)

const (
	MaxPAR2ProofBlockBytes  int64 = 64 << 20
	maxPAR2ProofDigestBytes       = 4 << 20
)

// PAR2Proof is one exact native IFSC block proof.
type PAR2Proof struct {
	Offset int64
	Length int64
	digest []byte
}

// PAR2ProofDomain is an immutable, lazy view over one persisted IFSC file index.
type PAR2ProofDomain struct {
	sliceSize int64
	file      archiveparser.PAR2ProofFile
}

// BuildPAR2ProofIndex binds IFSC packets only to exact FileDesc/NZB matches.
// Missing or malformed proof material is omitted; an oversized aggregate is
// rejected rather than persisted partially.
func BuildPAR2ProofIndex(info archiveparser.PAR2Info, matches []PAR2FileMatch) (*archiveparser.PAR2ProofIndex, error) {
	if info.SliceSize <= 0 || info.SliceSize > MaxPAR2ProofBlockBytes {
		return nil, nil
	}
	seenIndexes := make(map[int]struct{}, len(matches))
	seenFiles := make(map[string]struct{}, len(matches))
	index := &archiveparser.PAR2ProofIndex{SliceSize: info.SliceSize}
	totalDigestBytes := 0
	for _, match := range matches {
		canonical, ok := info.FileByID(match.Desc.FileID)
		if match.NZBFileIdx < 0 || match.Desc.Length <= 0 || !ok || canonical != match.Desc {
			continue
		}
		if _, duplicate := seenIndexes[match.NZBFileIdx]; duplicate {
			return nil, errors.New("nntp: duplicate PAR2 proof file index")
		}
		if _, duplicate := seenFiles[match.Desc.FileID]; duplicate {
			return nil, errors.New("nntp: duplicate PAR2 proof file identity")
		}
		ifsc, ok := info.IFSCByFileID(match.Desc.FileID)
		if !ok {
			continue
		}
		blocks := (match.Desc.Length-1)/info.SliceSize + 1
		if blocks <= 0 || blocks > maxPAR2ProofDigestBytes/md5.Size ||
			len(ifsc.BlockMD5) != int(blocks)*md5.Size {
			continue
		}
		if totalDigestBytes > maxPAR2ProofDigestBytes-len(ifsc.BlockMD5) {
			return nil, errors.New("nntp: PAR2 proof index exceeds persisted byte bound")
		}
		totalDigestBytes += len(ifsc.BlockMD5)
		seenIndexes[match.NZBFileIdx] = struct{}{}
		seenFiles[match.Desc.FileID] = struct{}{}
		index.Files = append(index.Files, archiveparser.PAR2ProofFile{
			NZBFileIndex: match.NZBFileIdx,
			Length:       match.Desc.Length,
			BlockMD5:     append([]byte(nil), ifsc.BlockMD5...),
		})
	}
	if len(index.Files) == 0 {
		return nil, nil
	}
	return index, nil
}

// NewPAR2ProofDomain selects exactly one persisted file proof domain.
func NewPAR2ProofDomain(index *archiveparser.PAR2ProofIndex, nzbFileIndex int) (PAR2ProofDomain, bool) {
	if index == nil || index.SliceSize <= 0 || index.SliceSize > MaxPAR2ProofBlockBytes {
		return PAR2ProofDomain{}, false
	}
	var selected *archiveparser.PAR2ProofFile
	for i := range index.Files {
		file := &index.Files[i]
		if file.NZBFileIndex != nzbFileIndex {
			continue
		}
		if selected != nil {
			return PAR2ProofDomain{}, false
		}
		selected = file
	}
	if selected == nil || selected.Length <= 0 {
		return PAR2ProofDomain{}, false
	}
	blocks := (selected.Length-1)/index.SliceSize + 1
	if blocks <= 0 || blocks > maxPAR2ProofDigestBytes/md5.Size ||
		len(selected.BlockMD5) != int(blocks)*md5.Size {
		return PAR2ProofDomain{}, false
	}
	file := *selected
	file.BlockMD5 = append([]byte(nil), selected.BlockMD5...)
	return PAR2ProofDomain{sliceSize: index.SliceSize, file: file}, true
}

// Length is the exact decoded length of the file this domain describes.
func (d PAR2ProofDomain) Length() int64 { return d.file.Length }

// SliceSize is the recovery set's PAR2 slice size.
func (d PAR2ProofDomain) SliceSize() int64 { return d.sliceSize }

// Blocks is the number of IFSC-proofed blocks in this domain.
func (d PAR2ProofDomain) Blocks() int64 {
	if d.sliceSize <= 0 || d.file.Length <= 0 {
		return 0
	}
	return (d.file.Length-1)/d.sliceSize + 1
}

// ProofAt returns the exact IFSC block covering file-relative offset.
func (d PAR2ProofDomain) ProofAt(offset int64) (PAR2Proof, bool) {
	if offset < 0 || offset >= d.file.Length || d.sliceSize <= 0 {
		return PAR2Proof{}, false
	}
	block := offset / d.sliceSize
	start := block * d.sliceSize
	length := d.sliceSize
	if remaining := d.file.Length - start; remaining < length {
		length = remaining
	}
	digestStart := block * md5.Size
	if length <= 0 || digestStart < 0 || digestStart+md5.Size > int64(len(d.file.BlockMD5)) {
		return PAR2Proof{}, false
	}
	return PAR2Proof{
		Offset: start,
		Length: length,
		digest: append([]byte(nil), d.file.BlockMD5[digestStart:digestStart+md5.Size]...),
	}, true
}

// Evidence adapts this native proof to the existing shared Content Proof Graph.
func (p PAR2Proof) Evidence(representationID string, observedAt time.Time) contentproof.Evidence {
	return contentproof.Evidence{
		RepresentationID: representationID,
		Scope:            contentproof.ScopeBlock,
		Offset:           p.Offset,
		Length:           p.Length,
		Kind:             contentproof.KindAuthoritative,
		Algorithm:        contentproof.AlgorithmMD5,
		Digest:           append([]byte(nil), p.digest...),
		Provenance:       contentproof.ProvenancePAR2IFSC,
		ObservedAt:       observedAt,
	}
}

// DigestEquals reports whether a candidate digest matches this block's native
// IFSC hash in constant time with respect to content, without ever exposing
// the expected digest to a caller.
func (p PAR2Proof) DigestEquals(candidate []byte) bool {
	return len(candidate) == len(p.digest) && subtle.ConstantTimeCompare(candidate, p.digest) == 1
}

// CandidateDigest hashes exactly one bounded candidate block.
func (p PAR2Proof) CandidateDigest(ctx context.Context, data []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.Length <= 0 || p.Length > MaxPAR2ProofBlockBytes || int64(len(data)) != p.Length {
		return nil, errors.New("nntp: candidate bytes do not match PAR2 proof span")
	}
	h := md5.New() // #nosec G401 -- PAR2 IFSC verification requires MD5.
	const chunk = 64 << 10
	for offset := 0; offset < len(data); offset += chunk {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := offset + chunk
		if end > len(data) {
			end = len(data)
		}
		_, _ = h.Write(data[offset:end])
	}
	return h.Sum(nil), nil
}
