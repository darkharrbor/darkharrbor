package api

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/darkharrbor/darkharrbor/internal/accountgov"
	"github.com/darkharrbor/darkharrbor/internal/bytesource"
	"github.com/darkharrbor/darkharrbor/internal/contentproof"
	"github.com/darkharrbor/darkharrbor/internal/crosslane"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/nntp"
	"github.com/darkharrbor/darkharrbor/internal/store"
	"github.com/darkharrbor/darkharrbor/internal/torrentmeta"
)

const (
	maxTorrentCrossLaneSourceBytes     = 4 << 20
	maxTorrentCrossLaneWindowBytes     = 64 << 20
	maxTorrentCrossLaneBlocksPerWindow = 4096
)

type torrentCrossLaneResult struct {
	data       []byte
	blocks     int
	nntpBlocks int
	httpBlocks int
	bytes      int64
}

// ResolveTorrentCrossLaneCandidates returns bounded URL-free handles for Ready
// NNTP and range-verified HTTP files with the exact normalized release and size.
func (s *Server) ResolveTorrentCrossLaneCandidates(ctx context.Context, releaseName string, size int64) ([]contentproof.LaneCandidate, error) {
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
	nntpItems, err := s.store.ListReadyNNTPItems(ctx, contentproof.MaxLaneCandidates)
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
			return errors.New("api: too many torrent cross-lane candidates")
		}
		matches = append(matches, candidate)
		return nil
	}

	for i, item := range nntpItems {
		if i%64 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if item == nil || item.SourceURI == nil || len(*item.SourceURI) > maxTorrentCrossLaneSourceBytes {
			continue
		}
		_, up, ok := s.pickUsenetProvider(item)
		np, concrete := up.(*nntp.NNTPProvider)
		if !ok || !concrete {
			continue
		}
		files, fileErr := np.CrossLaneFiles(ctx, item.ID, []byte(*item.SourceURI), contentproof.MaxLaneCandidates)
		if fileErr != nil {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			continue
		}
		for _, file := range files {
			if err := appendMatch(contentproof.LaneNNTP, item, strconv.Itoa(file.Index), file.Size, nntpCrossLaneRepresentationID(item.ID, file.Index)); err != nil {
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
		if item == nil || item.FileList == nil || len(*item.FileList) > maxTorrentCrossLaneSourceBytes {
			continue
		}
		files, parseErr := httpstream.ParseFiles(*item.FileList)
		if parseErr != nil || len(files) > contentproof.MaxLaneCandidates {
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

func (s *Server) recoverTorrentCrossLaneWindow(ctx context.Context, item *store.Item, fileID string, meta *torrentmeta.TorrentMeta, file *torrentmeta.FileEntry, start, end int64) (torrentCrossLaneResult, error) {
	if s == nil || s.cfg == nil || s.store == nil || s.httpProofGraph == nil || !s.cfg.NNTP.CrossLaneSplice ||
		item == nil || item.State != store.StateReady || item.SourceType != store.SourceTypeTorrent || start < 0 || end < start ||
		end-start >= maxTorrentCrossLaneWindowBytes || file == nil || end >= file.Size {
		return torrentCrossLaneResult{}, errors.New("api: torrent cross-lane recovery unavailable")
	}
	representationID := torrentCrossLaneRepresentationID(item.ID, fileID)
	target, err := torrentmeta.NewCrossLaneCandidate(meta, file, item.ID, fileID, representationID, item.DisplayName, file.Size)
	if err != nil {
		return torrentCrossLaneResult{}, err
	}
	handles, err := s.ResolveTorrentCrossLaneCandidates(ctx, item.DisplayName, file.Size)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return torrentCrossLaneResult{}, ctxErr
		}
		handles = nil
	}
	handles = s.appendConvergedRoutes(ctx, target.Candidate, handles, contentproof.LaneNNTP, contentproof.LaneHTTP)
	if len(handles) == 0 {
		return torrentCrossLaneResult{}, errors.New("api: torrent cross-lane candidates unavailable")
	}
	candidates := make([]crosslane.Candidate, 0, len(handles))
	for _, handle := range handles {
		candidates = append(candidates, crosslane.Candidate{
			Handle: handle,
			Source: &lazyTorrentCrossLaneSource{server: s, handle: handle},
		})
	}

	result := torrentCrossLaneResult{data: make([]byte, end-start+1)}
	coordinator := crosslane.NewCoordinator(s.httpProofGraph, s.torrentGov, s.representationConvergence)
	for offset := start; offset <= end; {
		if result.blocks == maxTorrentCrossLaneBlocksPerWindow {
			return torrentCrossLaneResult{}, errors.New("api: torrent cross-lane block limit exceeded")
		}
		proof, ok := target.Proofs.ProofAt(offset)
		if !ok || proof.Offset > offset || proof.Length <= 0 || proof.Offset+proof.Length <= offset {
			return torrentCrossLaneResult{}, errors.New("api: torrent proof does not cover window")
		}
		evidence := proof.Evidence(representationID, s.nowUTC())
		decision, verifyErr := s.httpProofGraph.VerifyDigest(ctx, evidence.RepresentationID, evidence.Scope, evidence.Offset, evidence.Length, evidence.Algorithm, evidence.Digest)
		if verifyErr != nil || decision.Relation == contentproof.RelationConflict {
			return torrentCrossLaneResult{}, errors.New("api: torrent proof conflicts")
		}
		if decision.Relation == contentproof.RelationNoProof {
			if err := s.httpProofGraph.Record(ctx, evidence); err != nil {
				return torrentCrossLaneResult{}, err
			}
		}
		block, recoverErr := coordinator.RecoverBlock(ctx, crosslane.Request{
			Target: target.Candidate, Proof: evidence, Candidates: candidates, CandidateDigest: proof.CandidateDigest,
		})
		s.logRepresentationConvergence(block)
		if recoverErr != nil || block.Decision.Relation != contentproof.RelationProven || int64(len(block.Data)) != proof.Length {
			if err := ctx.Err(); err != nil {
				return torrentCrossLaneResult{}, err
			}
			return torrentCrossLaneResult{}, errors.New("api: torrent cross-lane block abstained")
		}
		copyStart := max64(start, proof.Offset)
		copyEnd := min64(end+1, proof.Offset+proof.Length)
		if copyEnd <= copyStart {
			return torrentCrossLaneResult{}, errors.New("api: torrent cross-lane proof overlap absent")
		}
		copy(result.data[copyStart-start:copyEnd-start], block.Data[copyStart-proof.Offset:copyEnd-proof.Offset])
		result.blocks++
		result.bytes += proof.Length
		switch block.Candidate.Lane {
		case contentproof.LaneNNTP:
			result.nntpBlocks++
		case contentproof.LaneHTTP:
			result.httpBlocks++
		default:
			return torrentCrossLaneResult{}, errors.New("api: torrent cross-lane source lane disagrees")
		}
		offset = proof.Offset + proof.Length
	}
	return result, nil
}

type lazyTorrentCrossLaneSource struct {
	server *Server
	handle contentproof.LaneCandidate
	mu     sync.Mutex
	source bytesource.ByteSource
}

func (s *lazyTorrentCrossLaneSource) Size() int64 { return s.handle.Size }
func (s *lazyTorrentCrossLaneSource) Key() string { return s.handle.RepresentationID }
func (s *lazyTorrentCrossLaneSource) Caps() bytesource.Capabilities {
	return bytesource.Capabilities{RangeSupport: true, ExactSize: true}
}
func (s *lazyTorrentCrossLaneSource) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	s.mu.Lock()
	src := s.source
	if src == nil {
		var err error
		src, err = s.server.openTorrentCrossLaneSource(ctx, s.handle)
		if err != nil {
			s.mu.Unlock()
			return 0, errors.New("api: alternate source unavailable")
		}
		s.source = src
	}
	s.mu.Unlock()
	if src.Size() != s.handle.Size || !src.Caps().RangeSupport || !src.Caps().ExactSize {
		return 0, errors.New("api: alternate source geometry disagrees")
	}
	return src.ReadAt(ctx, p, off)
}

func (s *Server) openTorrentCrossLaneSource(ctx context.Context, candidate contentproof.LaneCandidate) (bytesource.ByteSource, error) {
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
	case contentproof.LaneNNTP:
		if item.SourceType != store.SourceTypeNZB || item.SourceURI == nil || len(*item.SourceURI) > maxTorrentCrossLaneSourceBytes {
			return nil, errors.New("api: NNTP candidate identity disagrees")
		}
		fileIndex, err := strconv.Atoi(candidate.FileID)
		if err != nil || fileIndex < 0 || strconv.Itoa(fileIndex) != candidate.FileID ||
			candidate.RepresentationID != nntpCrossLaneRepresentationID(item.ID, fileIndex) {
			return nil, errors.New("api: NNTP candidate file disagrees")
		}
		_, up, ok := s.pickUsenetProvider(item)
		np, concrete := up.(*nntp.NNTPProvider)
		if !ok || !concrete {
			return nil, errors.New("api: NNTP candidate provider unavailable")
		}
		return np.CrossLaneFileSource(ctx, item.ID, []byte(*item.SourceURI), fileIndex, candidate.Size)
	case contentproof.LaneHTTP:
		return s.openHTTPCrossLaneSource(ctx, item, candidate, "ts-7.2")
	default:
		return nil, fmt.Errorf("api: unsupported torrent alternate lane %q", candidate.Lane)
	}
}

type governedHTTPRecoverySource struct {
	bytesource.ByteSource
	gov     *accountgov.Governor
	session string
}

func (s *governedHTTPRecoverySource) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	if s.gov == nil {
		return s.ByteSource.ReadAt(ctx, p, off)
	}
	lease, err := s.gov.Acquire(ctx, HTTPSourceGovOp, accountgov.PriorityRecovery, s.session)
	if err != nil {
		return 0, err
	}
	defer lease.Release()
	return s.ByteSource.ReadAt(ctx, p, off)
}

func (s *Server) openHTTPCrossLaneSource(ctx context.Context, item *store.Item, candidate contentproof.LaneCandidate, stage string) (bytesource.ByteSource, error) {
	if item.SourceType != store.SourceTypeHTTP || candidate.RepresentationID != httpRepresentationID(item.ID, candidate.FileID) || item.ResolveKey == nil {
		return nil, errors.New("api: HTTP candidate identity disagrees")
	}
	files, err := httpstream.ParseFiles(strFromPtr(item.FileList))
	if err != nil {
		return nil, errors.New("api: HTTP candidate file list unreadable")
	}
	file, ok := httpstream.LookupFile(files, candidate.FileID)
	if !ok || !file.RangeVerified || file.Size != candidate.Size {
		return nil, errors.New("api: HTTP candidate file disagrees")
	}
	key, err := httpstream.ParseResolveKey(*item.ResolveKey)
	if err != nil || s.httpHandlers == nil {
		return nil, errors.New("api: HTTP candidate resolve key unavailable")
	}
	handler, ok := s.httpHandlers.Lookup(key.BackendID, key.Handler)
	if !ok {
		return nil, errors.New("api: HTTP candidate handler unavailable")
	}
	if file.ArchiveSize > 0 {
		archiveHandler, archiveOK := s.httpHandlers.LookupRemoteArchive(key.BackendID, key.Handler)
		if !archiveOK {
			return nil, errors.New("api: HTTP archive candidate handler unavailable")
		}
		descriptors, resolveErr := archiveHandler.ResolveRemoteArchives(ctx, httpstream.ResolveRequest{Key: key, Operation: httpstream.ResolveNormal})
		if resolveErr != nil {
			return nil, resolveErr
		}
		descriptor, matchErr := httpstream.MatchRemoteArchiveDescriptor(descriptors, file.Selector)
		if matchErr != nil {
			return nil, matchErr
		}
		member, source, openErr := s.openHTTPRemoteArchive(ctx, item.ID, file.FileID, key, file.ArchiveSize, archiveHandler, descriptor)
		if openErr != nil {
			return nil, openErr
		}
		if member.Name != file.Name || member.Size != file.Size {
			return nil, errors.New("api: HTTP archive candidate member disagrees")
		}
		return &governedHTTPRecoverySource{ByteSource: source, gov: s.httpGov, session: item.ID}, nil
	}
	resolver := func(rctx context.Context, forced bool) (httpstream.ResolvedFile, error) {
		return s.resolveHTTPPlayback(rctx, item.ID, file.FileID, handler, key, file.Selector, forced, stage)
	}
	source, err := s.httpByteSource(item, file, resolver)
	if err != nil {
		return nil, err
	}
	return &governedHTTPRecoverySource{ByteSource: source, gov: s.httpGov, session: item.ID}, nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
