package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/bytesource"
	"github.com/darkharrbor/darkharrbor/internal/contentproof"
	"github.com/darkharrbor/darkharrbor/internal/crosslane"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/nntp"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

const (
	maxNNTPCrossLaneFileListBytes = 4 << 20
	maxNNTPCrossLaneFilesPerItem  = contentproof.MaxLaneCandidates
)

// ResolveNNTPCrossLaneCandidates returns bounded, URL-free handles for exact
// normalized-release-name and file-size matches. It deliberately returns no
// byte source: RecoverNNTPCrossLaneBlock owns range fetching and IFSC verification.
func (s *Server) ResolveNNTPCrossLaneCandidates(ctx context.Context, releaseName string, size int64) ([]contentproof.LaneCandidate, error) {
	if s == nil || s.cfg == nil || s.store == nil || !s.cfg.NNTP.CrossLaneSplice {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	targetKey, err := contentproof.ReleaseKey(releaseName, size)
	if err != nil {
		return nil, err
	}

	torrents, err := s.store.ListReadyTorrentItemsAfter(ctx, "", contentproof.MaxLaneCandidates)
	if err != nil {
		return nil, err
	}
	httpItems, err := s.store.ListReadyHTTPItems(ctx, contentproof.MaxLaneCandidates)
	if err != nil {
		return nil, err
	}

	matches := make([]contentproof.LaneCandidate, 0)
	appendMatch := func(lane contentproof.Lane, item *store.Item, fileID string, fileSize int64, representationID string) error {
		if item == nil || fileSize != size {
			return nil
		}
		candidate, candidateErr := contentproof.NewLaneCandidate(lane, item.ID, fileID, representationID, item.DisplayName, fileSize)
		if candidateErr != nil || candidate.ReleaseKey != targetKey {
			return nil
		}
		if len(matches) == contentproof.MaxLaneCandidates {
			return errors.New("api: too many NNTP cross-lane candidates")
		}
		matches = append(matches, candidate)
		return nil
	}

	for i, item := range torrents {
		if i%64 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if item == nil || item.FileList == nil || len(*item.FileList) > maxNNTPCrossLaneFileListBytes {
			continue
		}
		files := parseFileList(item.FileList)
		if len(files) > maxNNTPCrossLaneFilesPerItem {
			continue
		}
		for j, file := range files {
			if j%64 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			if err := appendMatch(contentproof.LaneTorrent, item, file.FileID, file.Size, torrentCrossLaneRepresentationID(item.ID, file.FileID)); err != nil {
				return nil, err
			}
		}
	}
	for i, item := range httpItems {
		if i%64 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if item == nil || item.FileList == nil || len(*item.FileList) > maxNNTPCrossLaneFileListBytes {
			continue
		}
		files, parseErr := httpstream.ParseFiles(*item.FileList)
		if parseErr != nil || len(files) > maxNNTPCrossLaneFilesPerItem {
			continue
		}
		for j, file := range files {
			if j%64 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			if !file.RangeVerified {
				continue
			}
			if err := appendMatch(contentproof.LaneHTTP, item, file.FileID, file.Size, httpRepresentationID(item.ID, file.FileID)); err != nil {
				return nil, err
			}
		}
	}
	return contentproof.ResolveLaneCandidates(ctx, releaseName, size, matches)
}

func torrentCrossLaneRepresentationID(itemID, fileID string) string {
	digest := sha256.Sum256([]byte("torrent\x00" + itemID + "\x00" + fileID))
	return "torrent:" + hex.EncodeToString(digest[:])
}

func nntpCrossLaneRepresentationID(itemID string, fileIndex int) string {
	digest := sha256.Sum256([]byte("nntp\x00" + itemID + "\x00" + strconv.Itoa(fileIndex)))
	return "nntp:" + hex.EncodeToString(digest[:])
}

// RecoverNNTPCrossLaneBlock records the target's existing IFSC assertion in
// the shared graph, materializes URL-free candidates lazily, and delegates the
// only byte-emission decision to XR-01's coordinator.
func (s *Server) RecoverNNTPCrossLaneBlock(ctx context.Context, itemID string, fileIndex int, fileSize int64, proof nntp.PAR2Proof) (nntp.CrossLaneBlock, error) {
	if s == nil || s.cfg == nil || s.store == nil || s.httpProofGraph == nil || !s.cfg.NNTP.CrossLaneSplice {
		return nntp.CrossLaneBlock{}, nntp.ErrCrossLaneAbstain
	}
	if err := ctx.Err(); err != nil {
		return nntp.CrossLaneBlock{}, err
	}
	item, err := s.store.GetItemByID(ctx, itemID)
	if err != nil || item == nil || item.State != store.StateReady || item.SourceType != store.SourceTypeNZB || fileIndex < 0 || fileSize <= 0 {
		return nntp.CrossLaneBlock{}, nntp.ErrCrossLaneAbstain
	}
	representationID := nntpCrossLaneRepresentationID(item.ID, fileIndex)
	target, err := contentproof.NewLaneCandidate(contentproof.LaneNNTP, item.ID, strconv.Itoa(fileIndex), representationID, item.DisplayName, fileSize)
	if err != nil {
		return nntp.CrossLaneBlock{}, nntp.ErrCrossLaneAbstain
	}
	evidence := proof.Evidence(representationID, s.nowUTC())
	if evidence.Offset < 0 || evidence.Length <= 0 || evidence.Offset > fileSize || evidence.Length > fileSize-evidence.Offset {
		return nntp.CrossLaneBlock{}, nntp.ErrCrossLaneAbstain
	}
	handles, err := s.ResolveNNTPCrossLaneCandidates(ctx, item.DisplayName, fileSize)
	if err != nil || len(handles) == 0 {
		handles = nil
	}
	handles = s.appendConvergedRoutes(ctx, target, handles, contentproof.LaneTorrent, contentproof.LaneHTTP)
	if len(handles) == 0 {
		return nntp.CrossLaneBlock{}, nntp.ErrCrossLaneAbstain
	}
	decision, err := s.httpProofGraph.VerifyDigest(ctx, evidence.RepresentationID, evidence.Scope, evidence.Offset, evidence.Length, evidence.Algorithm, evidence.Digest)
	if err != nil || decision.Relation == contentproof.RelationConflict {
		return nntp.CrossLaneBlock{}, nntp.ErrCrossLaneAbstain
	}
	if decision.Relation == contentproof.RelationNoProof {
		if err := s.httpProofGraph.Record(ctx, evidence); err != nil {
			return nntp.CrossLaneBlock{}, err
		}
	}
	candidates := make([]crosslane.Candidate, 0, len(handles))
	for _, handle := range handles {
		candidates = append(candidates, crosslane.Candidate{
			Handle: handle,
			Source: &deferredCrossLaneSource{server: s, handle: handle},
		})
	}
	block, err := crosslane.NewCoordinator(s.httpProofGraph, s.torrentGov, s.representationConvergence).RecoverBlock(ctx, crosslane.Request{
		Target: target, Proof: evidence, Candidates: candidates,
	})
	s.logRepresentationConvergence(block)
	if err != nil || block.Decision.Relation != contentproof.RelationProven || len(block.Data) == 0 {
		if ctx.Err() != nil {
			return nntp.CrossLaneBlock{}, ctx.Err()
		}
		return nntp.CrossLaneBlock{}, nntp.ErrCrossLaneAbstain
	}
	return nntp.CrossLaneBlock{Data: block.Data, Lane: block.Candidate.Lane}, nil
}

type deferredCrossLaneSource struct {
	server *Server
	handle contentproof.LaneCandidate
}

func (s *deferredCrossLaneSource) Size() int64 { return s.handle.Size }
func (s *deferredCrossLaneSource) Key() string { return s.handle.RepresentationID }
func (s *deferredCrossLaneSource) Caps() bytesource.Capabilities {
	return bytesource.Capabilities{RangeSupport: true, ExactSize: true, TailCost: bytesource.TailCostCheap}
}
func (s *deferredCrossLaneSource) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	src, err := s.server.openNNTPCrossLaneSource(ctx, s.handle)
	if err != nil || src == nil || src.Size() != s.handle.Size || !src.Caps().RangeSupport || !src.Caps().ExactSize {
		return 0, errors.New("api: alternate source unavailable")
	}
	return src.ReadAt(ctx, p, off)
}

func (s *Server) openNNTPCrossLaneSource(ctx context.Context, candidate contentproof.LaneCandidate) (bytesource.ByteSource, error) {
	if err := contentproof.ValidateLaneCandidate(candidate); err != nil {
		return nil, err
	}
	item, err := s.store.GetItemByID(ctx, candidate.ItemID)
	if err != nil || item == nil || item.State != store.StateReady {
		return nil, errors.New("api: alternate item unavailable")
	}
	releaseKey, err := contentproof.ReleaseKey(item.DisplayName, candidate.Size)
	if err != nil || releaseKey != candidate.ReleaseKey {
		return nil, errors.New("api: alternate release identity disagrees")
	}
	switch candidate.Lane {
	case contentproof.LaneTorrent:
		if item.SourceType != store.SourceTypeTorrent || candidate.RepresentationID != torrentCrossLaneRepresentationID(item.ID, candidate.FileID) {
			return nil, errors.New("api: torrent candidate identity disagrees")
		}
		file, ok := fileListEntryForID(parseFileList(item.FileList), candidate.FileID)
		if !ok || file.Size != candidate.Size {
			return nil, errors.New("api: torrent candidate file disagrees")
		}
		prov := s.pickDebridProvider(item)
		if prov == nil {
			return nil, errors.New("api: torrent provider unavailable")
		}
		reresolve := func(rctx context.Context) (string, error) {
			return prov.RequestDownloadURL(rctx, item, candidate.FileID)
		}
		url, err := reresolve(ctx)
		if err != nil || url == "" {
			return nil, errors.New("api: torrent candidate resolve failed")
		}
		windowBytes := int64(s.cfg.Cache.StreamChunkSizeMB) << 20
		src, ranged, err := NewProbedCDNByteSource(ctx, candidate.RepresentationID, url, windowBytes, reresolve, nil, s.torrentGov, accountgov.PriorityRecovery, item.ID)
		if err != nil || !ranged {
			return nil, errors.New("api: torrent candidate range unavailable")
		}
		return src, nil
	case contentproof.LaneHTTP:
		return s.openHTTPCrossLaneSource(ctx, item, candidate, "ns-7.2")
	default:
		return nil, fmt.Errorf("api: unsupported alternate lane %q", candidate.Lane)
	}
}
