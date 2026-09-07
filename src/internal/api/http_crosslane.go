package api

import (
	"bytes"
	"context"
	"errors"

	"github.com/darkharrbor/darkharrbor/internal/contentproof"
	"github.com/darkharrbor/darkharrbor/internal/crosslane"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/store"
	"github.com/darkharrbor/darkharrbor/internal/torrentmeta"
)

const maxHTTPCrossLaneProofsPerWindow = contentproof.DefaultMaxProofsPerRepresentation

// recoverHTTPCrossLaneWindow is HR6.2's final HTTP ladder rung. It can consume
// only native torrent/PAR2 evidence that HR6.1 already mapped onto this exact
// HTTP representation. Exact name and size nominate lazy candidates but never
// authorize a source read.
func (s *Server) recoverHTTPCrossLaneWindow(ctx context.Context, item *store.Item, file httpstream.PersistedFile, start, end int64) ([]byte, error) {
	if s == nil || s.cfg == nil || s.store == nil || s.httpProofGraph == nil || !s.cfg.NNTP.CrossLaneSplice ||
		item == nil || item.State != store.StateReady || item.SourceType != store.SourceTypeHTTP ||
		start < 0 || end < start || end >= file.Size || end-start >= contentproof.MaxMappingProofBytes {
		return nil, errors.New("api: HTTP cross-lane recovery unavailable")
	}
	representationID := httpRepresentationID(item.ID, file.FileID)
	target, err := contentproof.NewLaneCandidate(contentproof.LaneHTTP, item.ID, file.FileID, representationID, item.DisplayName, file.Size)
	if err != nil {
		return nil, errors.New("api: HTTP cross-lane target unavailable")
	}
	representationIDs := []string{representationID}
	aliases, canonicalSize, err := s.store.ListRepresentationAliases(ctx, representationID, contentproof.DefaultMaxRoutesPerRepresentation)
	if err != nil {
		return nil, errors.New("api: HTTP cross-lane aliases unavailable")
	}
	if len(aliases) > 0 {
		if canonicalSize != file.Size {
			return nil, errors.New("api: HTTP cross-lane canonical size disagrees")
		}
		representationIDs = aliases
	}
	proofs, err := s.store.ListContentProofs(ctx, representationIDs, s.nowUTC())
	if err != nil || len(proofs) == 0 || len(proofs) > maxHTTPCrossLaneProofsPerWindow {
		return nil, errors.New("api: HTTP cross-lane proof unavailable")
	}
	// A fully converged canonical group has already proven every byte of each
	// alias identical. Native proofs may therefore live on another durable
	// route after convergence; normalize only proofs loaded through that exact
	// bounded alias set into the HTTP target's verification domain.
	if len(aliases) > 0 {
		normalizeCanonicalHTTPProofs(proofs, representationID)
	}
	windowProofs := make([]contentproof.Evidence, 0)
	for offset := start; offset <= end; {
		if len(windowProofs) == maxHTTPCrossLaneProofsPerWindow {
			return nil, errors.New("api: HTTP cross-lane proof limit exceeded")
		}
		proof, ok := mappedHTTPProofAt(proofs, representationID, file.Size, offset)
		if !ok {
			return nil, errors.New("api: HTTP cross-lane range lacks proof")
		}
		windowProofs = append(windowProofs, proof)
		offset = proof.Offset + proof.Length
	}
	handles, err := s.resolveHTTPCrossLaneCandidates(ctx, item.DisplayName, file.Size)
	if err != nil || len(handles) == 0 {
		handles = nil
	}
	handles = s.appendConvergedRoutes(ctx, target, handles, contentproof.LaneTorrent, contentproof.LaneNNTP)
	if len(handles) == 0 {
		return nil, errors.New("api: HTTP cross-lane candidates unavailable")
	}
	candidates := make([]crosslane.Candidate, 0, len(handles))
	for _, handle := range handles {
		var source crosslane.Candidate
		source.Handle = handle
		switch handle.Lane {
		case contentproof.LaneTorrent:
			source.Source = &deferredCrossLaneSource{server: s, handle: handle}
		case contentproof.LaneNNTP:
			source.Source = &lazyTorrentCrossLaneSource{server: s, handle: handle}
		default:
			return nil, errors.New("api: HTTP cross-lane candidate lane disagrees")
		}
		candidates = append(candidates, source)
	}

	result := make([]byte, end-start+1)
	coordinator := crosslane.NewCoordinator(s.httpProofGraph, s.torrentGov, s.representationConvergence)
	for _, proof := range windowProofs {
		var digestFn func(context.Context, []byte) ([]byte, error)
		if proof.Provenance == contentproof.ProvenanceTorrentMerkle {
			digestFn, err = s.mappedTorrentDigest(ctx, handles, proof)
			if err != nil {
				return nil, errors.New("api: HTTP cross-lane Merkle geometry unavailable")
			}
		}
		block, recoverErr := coordinator.RecoverBlock(ctx, crosslane.Request{
			Target: target, Proof: proof, Candidates: candidates, CandidateDigest: digestFn,
		})
		s.logRepresentationConvergence(block)
		if recoverErr != nil || block.Decision.Relation != contentproof.RelationProven || int64(len(block.Data)) != proof.Length {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errors.New("api: HTTP cross-lane proof abstained")
		}
		copyStart := max64(start, proof.Offset)
		copyEnd := min64(end+1, proof.Offset+proof.Length)
		if copyEnd <= copyStart {
			return nil, errors.New("api: HTTP cross-lane proof overlap absent")
		}
		copy(result[copyStart-start:copyEnd-start], block.Data[copyStart-proof.Offset:copyEnd-proof.Offset])
	}
	return result, nil
}

func normalizeCanonicalHTTPProofs(proofs []contentproof.Evidence, representationID string) {
	for i := range proofs {
		proofs[i].RepresentationID = representationID
	}
}

func (s *Server) logRepresentationConvergence(block crosslane.Block) {
	if block.ConvergenceErr != nil {
		s.log.Warn("cross-lane representation convergence failed (non-fatal)",
			"error", httpstream.Sanitize(block.ConvergenceErr))
	}
}

func (s *Server) appendConvergedRoutes(ctx context.Context, target contentproof.LaneCandidate, handles []contentproof.LaneCandidate, allowed ...contentproof.Lane) []contentproof.LaneCandidate {
	if s.representationConvergence == nil {
		return handles
	}
	routes, err := s.representationConvergence.Routes(ctx, target.RepresentationID)
	if err != nil {
		s.log.Warn("cross-lane representation routes unavailable (non-fatal)", "error", httpstream.Sanitize(err))
		return handles
	}
	lanes := make(map[contentproof.Lane]bool, len(allowed))
	for _, lane := range allowed {
		lanes[lane] = true
	}
	seen := make(map[string]bool, len(handles)+1)
	seen[target.RepresentationID] = true
	for _, handle := range handles {
		seen[handle.RepresentationID] = true
	}
	for _, route := range routes {
		if len(handles) >= contentproof.MaxLaneCandidates {
			break
		}
		if seen[route.RepresentationID] || !lanes[route.Lane] || route.ReleaseKey != target.ReleaseKey || route.Size != target.Size {
			continue
		}
		seen[route.RepresentationID] = true
		handles = append(handles, route)
	}
	return handles
}

func (s *Server) resolveHTTPCrossLaneCandidates(ctx context.Context, releaseName string, size int64) ([]contentproof.LaneCandidate, error) {
	torrentHandles, err := s.ResolveNNTPCrossLaneCandidates(ctx, releaseName, size)
	if err != nil {
		return nil, err
	}
	nntpHandles, err := s.ResolveTorrentCrossLaneCandidates(ctx, releaseName, size)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	result := make([]contentproof.LaneCandidate, 0)
	appendLane := func(handles []contentproof.LaneCandidate, lane contentproof.Lane) error {
		for _, handle := range handles {
			if handle.Lane != lane {
				continue
			}
			if _, exists := seen[handle.RepresentationID]; exists {
				continue
			}
			if len(result) == contentproof.MaxLaneCandidates {
				return errors.New("api: too many HTTP cross-lane candidates")
			}
			seen[handle.RepresentationID] = struct{}{}
			result = append(result, handle)
		}
		return nil
	}
	if err := appendLane(torrentHandles, contentproof.LaneTorrent); err != nil {
		return nil, err
	}
	if err := appendLane(nntpHandles, contentproof.LaneNNTP); err != nil {
		return nil, err
	}
	return result, nil
}

func mappedHTTPProofAt(proofs []contentproof.Evidence, representationID string, size, offset int64) (contentproof.Evidence, bool) {
	var selected contentproof.Evidence
	for _, proof := range proofs {
		if proof.RepresentationID != representationID || proof.Kind != contentproof.KindAuthoritative ||
			proof.Scope != contentproof.ScopeBlock || proof.Offset < 0 || proof.Length <= 0 ||
			proof.Length > contentproof.MaxMappingProofBytes || proof.Offset > size || proof.Length > size-proof.Offset ||
			proof.Offset > offset || proof.Offset+proof.Length <= offset {
			continue
		}
		switch proof.Provenance {
		case contentproof.ProvenanceTorrentPiece, contentproof.ProvenanceTorrentMerkle, contentproof.ProvenancePAR2IFSC:
		default:
			continue
		}
		if selected.Length == 0 || proof.Offset+proof.Length < selected.Offset+selected.Length {
			selected = proof
		}
	}
	return selected, selected.Length > 0
}

func (s *Server) mappedTorrentDigest(ctx context.Context, handles []contentproof.LaneCandidate, evidence contentproof.Evidence) (func(context.Context, []byte) ([]byte, error), error) {
	for _, handle := range handles {
		if handle.Lane != contentproof.LaneTorrent {
			continue
		}
		item, err := s.store.GetItemByID(ctx, handle.ItemID)
		if err != nil || item == nil || item.SourceType != store.SourceTypeTorrent {
			continue
		}
		infoHash := item.Metadata.RealInfoHash
		if infoHash == "" && item.InfoHash != nil {
			infoHash = *item.InfoHash
		}
		if infoHash == "" {
			continue
		}
		meta, found, err := s.store.GetTorrentMeta(ctx, infoHash)
		if err != nil || !found {
			continue
		}
		var file *torrentmeta.FileEntry
		for i := range meta.Files {
			if meta.Files[i].Size != handle.Size {
				continue
			}
			if file != nil {
				file = nil
				break
			}
			file = &meta.Files[i]
		}
		if file == nil {
			continue
		}
		native, err := torrentmeta.NewCrossLaneCandidate(meta, file, handle.ItemID, handle.FileID, handle.RepresentationID, item.DisplayName, handle.Size)
		if err != nil {
			continue
		}
		proof, ok := native.Proofs.ProofAt(evidence.Offset)
		if !ok {
			continue
		}
		candidateEvidence := proof.Evidence(evidence.RepresentationID, evidence.ObservedAt)
		if candidateEvidence.Scope == evidence.Scope && candidateEvidence.Offset == evidence.Offset &&
			candidateEvidence.Length == evidence.Length && candidateEvidence.Algorithm == evidence.Algorithm &&
			candidateEvidence.Provenance == evidence.Provenance && bytes.Equal(candidateEvidence.Digest, evidence.Digest) {
			return proof.CandidateDigest, nil
		}
	}
	return nil, errors.New("api: mapped torrent proof unavailable")
}
