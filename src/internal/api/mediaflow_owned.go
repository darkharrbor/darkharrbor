package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/contentproof"
	"github.com/darkharrbor/darkharrbor/internal/crosslane"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/mediaidentity"
	"github.com/darkharrbor/darkharrbor/internal/playbackcoverage"
	"github.com/darkharrbor/darkharrbor/internal/sidecar"
	"github.com/darkharrbor/darkharrbor/internal/store"
	"github.com/darkharrbor/darkharrbor/internal/torrentmeta"
)

const (
	mediaFlowAIOPlaybackPrefix      = "/api/v1/debrid/playback/"
	mediaFlowMaxProofWindowBytes    = 8 << 20
	mediaFlowMaxProofCandidates     = 32
	mediaFlowMaxProofsPerRequest    = 64
	mediaFlowFallbackProbeBytes     = 64 << 10
	mediaFlowOwnedRouteFileID       = "owned"
	mediaFlowMaxDescriptorSources   = 256
	mediaFlowMaxDescriptorHashBytes = 128
)

type mediaFlowOwnedDescriptor struct {
	Kind     string
	Title    string
	Hash     string
	Identity mediaFlowStremioIdentity
}

type mediaFlowStremioIdentity struct {
	IMDb    string
	Season  int
	Episode int
	Series  bool
}

type mediaFlowAIOFileInfo struct {
	Type    string          `json:"type"`
	Title   string          `json:"title"`
	Hash    *string         `json:"hash"`
	Sources json.RawMessage `json:"sources"`
	NZB     *string         `json:"nzb"`
}

type mediaFlowGeometry struct {
	Offset int64
	Length int64
	Total  int64
}

// mediaFlowOwnedAIORoute recognizes a capability, not a product version. Only
// an exact owned route on a configured Stremio backend origin with a complete
// inline descriptor qualifies. Cache keys and arbitrary URLs remain ephemeral.
func mediaFlowOwnedAIORoute(raw string, backends []config.HTTPBackend) (mediaFlowOwnedDescriptor, error) {
	if len(raw) == 0 || len(raw) > mediaFlowMaxURLBytes {
		return mediaFlowOwnedDescriptor{}, errors.New("mediaflow: owned route length")
	}
	u, err := httpstream.ValidateURL(raw)
	if err != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || !mediaFlowMatchesStremioOrigin(u, backends) {
		return mediaFlowOwnedDescriptor{}, errors.New("mediaflow: route is not on a configured owned origin")
	}
	escaped := u.EscapedPath()
	if !strings.HasPrefix(escaped, mediaFlowAIOPlaybackPrefix) {
		return mediaFlowOwnedDescriptor{}, errors.New("mediaflow: route path is not owned")
	}
	segments := strings.Split(strings.TrimPrefix(escaped, mediaFlowAIOPlaybackPrefix), "/")
	if len(segments) != 5 {
		return mediaFlowOwnedDescriptor{}, errors.New("mediaflow: owned route shape")
	}
	for i, segment := range segments {
		decoded, decodeErr := url.PathUnescape(segment)
		if decodeErr != nil || decoded == "" || containsMediaFlowControl(decoded) || strings.ContainsAny(decoded, "/\\") {
			return mediaFlowOwnedDescriptor{}, errors.New("mediaflow: invalid owned route segment")
		}
		segments[i] = decoded
	}

	plain, err := base64.RawURLEncoding.DecodeString(segments[2])
	if err != nil || len(plain) == 0 || len(plain) > mediaFlowMaxURLBytes {
		return mediaFlowOwnedDescriptor{}, errors.New("mediaflow: descriptor is not inline")
	}
	var info mediaFlowAIOFileInfo
	dec := json.NewDecoder(bytes.NewReader(plain))
	if err := dec.Decode(&info); err != nil || ensureJSONEOF(dec) != nil || info.Hash == nil || len(*info.Hash) > mediaFlowMaxDescriptorHashBytes ||
		containsMediaFlowControl(*info.Hash) || len(info.Title) > contentproof.MaxReleaseNameBytes || containsMediaFlowControl(info.Title) {
		return mediaFlowOwnedDescriptor{}, errors.New("mediaflow: malformed inline descriptor")
	}
	switch info.Type {
	case "torrent":
		var sources []string
		if len(info.Sources) == 0 || json.Unmarshal(info.Sources, &sources) != nil || sources == nil || len(sources) > mediaFlowMaxDescriptorSources {
			return mediaFlowOwnedDescriptor{}, errors.New("mediaflow: incomplete torrent descriptor")
		}
	case "usenet":
		if info.NZB == nil {
			return mediaFlowOwnedDescriptor{}, errors.New("mediaflow: incomplete usenet descriptor")
		}
	default:
		return mediaFlowOwnedDescriptor{}, errors.New("mediaflow: unsupported descriptor type")
	}
	identity, err := parseMediaFlowStremioIdentity(segments[3])
	if err != nil {
		return mediaFlowOwnedDescriptor{}, err
	}
	return mediaFlowOwnedDescriptor{Kind: info.Type, Title: strings.TrimSpace(info.Title), Hash: strings.TrimSpace(*info.Hash), Identity: identity}, nil
}

// parseMediaFlowStremioIdentity accepts only Stremio's stable IMDb forms:
// ttNNNNNNN for a movie and ttNNNNNNN:season:episode for one series episode.
// It intentionally does not derive an identity from a title, filename, or hash.
func parseMediaFlowStremioIdentity(raw string) (mediaFlowStremioIdentity, error) {
	parts := strings.Split(raw, ":")
	if len(parts) != 1 && len(parts) != 3 {
		return mediaFlowStremioIdentity{}, errors.New("mediaflow: unsupported Stremio identity")
	}
	imdb := mediaidentity.NormalizedIDs(mediaidentity.ProviderIDs{IMDB: parts[0]}).IMDB
	if imdb == "" || imdb != parts[0] {
		return mediaFlowStremioIdentity{}, errors.New("mediaflow: invalid Stremio IMDb identity")
	}
	identity := mediaFlowStremioIdentity{IMDb: imdb}
	if len(parts) == 1 {
		return identity, nil
	}
	if len(parts[1]) == 0 || len(parts[1]) > 5 || len(parts[2]) == 0 || len(parts[2]) > 5 {
		return mediaFlowStremioIdentity{}, errors.New("mediaflow: invalid Stremio episode identity")
	}
	season, seasonErr := strconv.Atoi(parts[1])
	episode, episodeErr := strconv.Atoi(parts[2])
	if seasonErr != nil || episodeErr != nil || season < 0 || episode < 1 {
		return mediaFlowStremioIdentity{}, errors.New("mediaflow: invalid Stremio episode identity")
	}
	identity.Season, identity.Episode, identity.Series = season, episode, true
	return identity, nil
}

func mediaFlowIdentityMatches(identity *store.ProviderIdentity, want mediaFlowStremioIdentity) bool {
	if identity == nil || sidecar.ValidateProviderIdentity(identity) != nil || identity.IDs.IMDB != want.IMDb {
		return false
	}
	if !want.Series {
		return identity.Kind == "movie"
	}
	if identity.Kind != "series" {
		return false
	}
	for _, episode := range identity.Episodes {
		if episode.Season == want.Season && episode.Episode == want.Episode {
			return true
		}
	}
	return false
}

// mediaFlowOwnedProposalTarget converts already-proven canonical route state
// into RX-3.2's existing target. It is deliberately a read-only, unique match:
// every non-native, stale, malformed, or ambiguous candidate abstains.
func (s *Server) mediaFlowOwnedProposalTarget(ctx context.Context, representationID string, descriptor mediaFlowOwnedDescriptor) (playbackcoverage.Target, bool, error) {
	if s == nil || s.store == nil || representationID == "" {
		return playbackcoverage.Target{}, false, nil
	}
	routes, err := s.store.ListRepresentationRoutes(ctx, representationID, contentproof.DefaultMaxRoutesPerRepresentation)
	if err != nil {
		return playbackcoverage.Target{}, false, err
	}
	var target playbackcoverage.Target
	for _, route := range routes {
		if route.ItemID == "" || route.FileID == "" || route.Size <= 0 || route.ItemID == representationID {
			continue
		}
		item, itemErr := s.store.GetItemByID(ctx, route.ItemID)
		if itemErr != nil {
			return playbackcoverage.Target{}, false, itemErr
		}
		if item == nil || item.State != store.StateReady || !mediaFlowIdentityMatches(item.Metadata.ProviderIdentity, descriptor.Identity) {
			continue
		}
		candidate, ok := playbackProposalTarget(item, route.FileID, route.Size)
		if !ok {
			continue
		}
		if target.ItemID != "" && (target.ItemID != candidate.ItemID || target.FileID != candidate.FileID) {
			return playbackcoverage.Target{}, false, nil
		}
		target = candidate
	}
	return target, target.ItemID != "", nil
}

// mediaFlowStremioIdentityFromRoute is RX-9.1's identity extractor. It applies
// the SAME origin, prefix, shape, and segment-decoding checks as
// mediaFlowOwnedAIORoute, but deliberately does NOT decode the inline
// descriptor, because the representation identity must be derivable for EVERY
// aggregator source -- http and omss included -- not only the torrent/usenet
// descriptor types that carry a proof-bearing inline blob.
//
// The origin check is retained and is load-bearing: it is what keeps an
// arbitrary caller-supplied URL from choosing another identity's
// representation ID and thereby merging coverage into it.
func mediaFlowStremioIdentityFromRoute(raw string, backends []config.HTTPBackend) (mediaFlowStremioIdentity, error) {
	if len(raw) == 0 || len(raw) > mediaFlowMaxURLBytes {
		return mediaFlowStremioIdentity{}, errors.New("mediaflow: owned route length")
	}
	u, err := httpstream.ValidateURL(raw)
	if err != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || !mediaFlowMatchesStremioOrigin(u, backends) {
		return mediaFlowStremioIdentity{}, errors.New("mediaflow: route is not on a configured owned origin")
	}
	escaped := u.EscapedPath()
	if !strings.HasPrefix(escaped, mediaFlowAIOPlaybackPrefix) {
		return mediaFlowStremioIdentity{}, errors.New("mediaflow: route path is not owned")
	}
	segments := strings.Split(strings.TrimPrefix(escaped, mediaFlowAIOPlaybackPrefix), "/")
	if len(segments) != 5 {
		return mediaFlowStremioIdentity{}, errors.New("mediaflow: owned route shape")
	}
	decoded, decodeErr := url.PathUnescape(segments[3])
	if decodeErr != nil || decoded == "" || containsMediaFlowControl(decoded) || strings.ContainsAny(decoded, "/\\") {
		return mediaFlowStremioIdentity{}, errors.New("mediaflow: invalid owned route segment")
	}
	return parseMediaFlowStremioIdentity(decoded)
}

// mediaFlowIdentityRepresentationID is RX-9.1 (RD-30 D5). Every other lane
// derives its representation ID deterministically -- http: is
// sha256(itemID\x00fileID), nntp: and torrent: likewise -- while MediaFlow
// alone used 24 random bytes. Randomness made aggregator playback structurally
// incapable of accumulating: playback_coverage is keyed by representation, so
// a movie watched across two sittings became two representations at partial
// coverage each and the threshold could never fire.
//
// The value is HASHED, never plaintext. Metrics are documented as carrying "no
// title, identity, URL, service name or per-item label" and contentproof
// documents the representation ID as opaque and secret-free; a plaintext
// identity here would put viewing history into logs and the database.
// Unguessability is not weakened, because the stream URL's capability is the
// AES-GCM seal over StreamSecret with a random nonce and this ID lives INSIDE
// that sealed ticket.
// rx95RepresentationID derives the representation ID from an identity bound at
// the addon boundary (RX-9.5). It shares mediaFlowIdentityRepresentationID's
// domain and construction so a route-derived and a batch-bound identity for the
// same title produce the SAME representation, which is the whole point: one
// episode, one representation, coverage that accumulates across sessions.
//
// It stays HASHED. Metrics are documented as carrying "no title, identity, URL,
// service name or per-item label", and contentproof documents the
// representation ID as opaque and secret-free.
func rx95RepresentationID(identity rx95Identity) string {
	return mediaFlowIdentityRepresentationID(mediaFlowStremioIdentity{
		IMDb:    identity.IMDb,
		Series:  identity.Kind == "series",
		Season:  identity.Season,
		Episode: identity.Episode,
	})
}

// mediaFlowBatchMetadataID returns the single metadata_id shared by the
// aggregator-owned URLs in a batch, or "" when there is none or they disagree.
// In the captured sample all 36 owned URLs carried one identical value while
// the other 56 carried none, so disagreement means the batch is not the single
// coherent resolution this binding assumes and must not corroborate anything.
func mediaFlowBatchMetadataID(urls []mediaFlowURLRequest) string {
	found := ""
	for _, candidate := range urls {
		u, err := httpstream.ValidateURL(candidate.DestinationURL)
		if err != nil {
			continue
		}
		escaped := u.EscapedPath()
		if !strings.HasPrefix(escaped, mediaFlowAIOPlaybackPrefix) {
			continue
		}
		segments := strings.Split(strings.TrimPrefix(escaped, mediaFlowAIOPlaybackPrefix), "/")
		if len(segments) != 5 || segments[3] == "" {
			continue
		}
		if found == "" {
			found = segments[3]
			continue
		}
		if found != segments[3] {
			return ""
		}
	}
	return found
}

func mediaFlowIdentityRepresentationID(identity mediaFlowStremioIdentity) string {
	season, episode := -1, -1
	if identity.Series {
		season, episode = identity.Season, identity.Episode
	}
	digest := sha256.Sum256([]byte("mediaflow\x00" + identity.IMDb + "\x00" +
		strconv.Itoa(season) + "\x00" + strconv.Itoa(episode)))
	return "mediaflow:" + hex.EncodeToString(digest[:])
}

func mediaFlowMatchesStremioOrigin(target *url.URL, backends []config.HTTPBackend) bool {
	for _, backend := range backends {
		if backend.Type != "" && backend.Type != "stremio" {
			continue
		}
		configured, err := httpstream.ValidateURL(backend.URL)
		if err == nil && mediaFlowSameOrigin(target, configured) {
			return true
		}
		if backend.PublicURL != "" {
			public, publicErr := httpstream.ValidateURL(backend.PublicURL)
			if publicErr == nil && mediaFlowSameOrigin(target, public) {
				return true
			}
		}
	}
	return false
}

func mediaFlowSameOrigin(left, right *url.URL) bool {
	return left != nil && right != nil && strings.EqualFold(left.Scheme, right.Scheme) &&
		strings.EqualFold(left.Hostname(), right.Hostname()) && mediaFlowEffectivePort(left) == mediaFlowEffectivePort(right)
}

func mediaFlowEffectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	if strings.EqualFold(u.Scheme, "http") {
		return "80"
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return ""
}

func containsMediaFlowControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func exactMediaFlowGeometry(resp *http.Response) (mediaFlowGeometry, bool) {
	if resp == nil {
		return mediaFlowGeometry{}, false
	}
	switch resp.StatusCode {
	case http.StatusOK:
		length, err := strconv.ParseInt(strings.TrimSpace(resp.Header.Get("Content-Length")), 10, 64)
		if err != nil || length <= 0 {
			return mediaFlowGeometry{}, false
		}
		return mediaFlowGeometry{Length: length, Total: length}, true
	case http.StatusPartialContent:
		raw := strings.TrimSpace(resp.Header.Get("Content-Range"))
		if !strings.HasPrefix(strings.ToLower(raw), "bytes ") {
			return mediaFlowGeometry{}, false
		}
		spanTotal := strings.SplitN(strings.TrimSpace(raw[len("bytes "):]), "/", 2)
		if len(spanTotal) != 2 || spanTotal[1] == "*" {
			return mediaFlowGeometry{}, false
		}
		span := strings.SplitN(spanTotal[0], "-", 2)
		if len(span) != 2 {
			return mediaFlowGeometry{}, false
		}
		start, startErr := strconv.ParseInt(strings.TrimSpace(span[0]), 10, 64)
		end, endErr := strconv.ParseInt(strings.TrimSpace(span[1]), 10, 64)
		total, totalErr := strconv.ParseInt(strings.TrimSpace(spanTotal[1]), 10, 64)
		length, lengthErr := strconv.ParseInt(strings.TrimSpace(resp.Header.Get("Content-Length")), 10, 64)
		if startErr != nil || endErr != nil || totalErr != nil || lengthErr != nil || start < 0 || end < start || total <= end || length != end-start+1 {
			return mediaFlowGeometry{}, false
		}
		return mediaFlowGeometry{Offset: start, Length: length, Total: total}, true
	default:
		return mediaFlowGeometry{}, false
	}
}

type mediaFlowProofWindow struct {
	representationID string
	data             []byte
	offset           int64
	total            int64
}

func (s *mediaFlowProofWindow) Size() int64 { return s.total }
func (s *mediaFlowProofWindow) Key() string { return s.representationID }
func (s *mediaFlowProofWindow) Caps() bytesource.Capabilities {
	return bytesource.Capabilities{RangeSupport: true, ExactSize: true, TailCost: bytesource.TailCostCheap}
}
func (s *mediaFlowProofWindow) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off < s.offset || off > s.offset+int64(len(s.data)) || int64(len(p)) > s.offset+int64(len(s.data))-off {
		return 0, errors.New("mediaflow: proof read outside delivered window")
	}
	return copy(p, s.data[off-s.offset:]), nil
}

func (s *Server) mediaFlowCanonical(ctx context.Context, representationID string) (string, int64, bool, error) {
	if s == nil || s.store == nil || s.representationConvergence == nil || representationID == "" {
		return "", 0, false, nil
	}
	canonical, found, err := s.representationConvergence.CanonicalRepresentationID(ctx, representationID)
	if err != nil || !found {
		return "", 0, false, err
	}
	aliases, size, err := s.store.ListRepresentationAliases(ctx, representationID, contentproof.DefaultMaxRoutesPerRepresentation)
	if err != nil || len(aliases) == 0 || size <= 0 {
		return "", 0, false, err
	}
	return canonical, size, true, nil
}

// admitMediaFlowOwnedWindow maps only bytes already written successfully to
// the client. Name and size nominate existing native routes; their stored
// authoritative proof is the sole authority for convergence.
func (s *Server) admitMediaFlowOwnedWindow(ctx context.Context, ticket mediaFlowTicket, descriptor mediaFlowOwnedDescriptor, geometry mediaFlowGeometry, delivered []byte) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if s == nil || s.cfg == nil || s.store == nil || s.httpProofGraph == nil || s.representationConvergence == nil ||
		!s.cfg.NNTP.CrossLaneSplice || ticket.RepresentationID == "" || geometry.Offset < 0 || geometry.Length <= 0 ||
		geometry.Total <= 0 || geometry.Offset > geometry.Total || geometry.Length > geometry.Total-geometry.Offset ||
		geometry.Length > mediaFlowMaxProofWindowBytes || int64(len(delivered)) != geometry.Length {
		return false, nil
	}
	if _, _, found, err := s.mediaFlowCanonical(ctx, ticket.RepresentationID); err != nil || found {
		return found, err
	}

	names := []string{descriptor.Title, ticket.Filename}
	var handles []contentproof.LaneCandidate
	for _, releaseName := range names {
		releaseName = strings.TrimSpace(releaseName)
		if releaseName == "" {
			continue
		}
		if _, err := contentproof.ReleaseKey(releaseName, geometry.Total); err != nil {
			continue
		}
		resolved, err := s.resolveMediaFlowProofCandidates(ctx, releaseName, geometry.Total)
		if err != nil {
			return false, err
		}
		if len(resolved) > 0 {
			handles = resolved
			break
		}
	}
	if len(handles) == 0 {
		return false, nil
	}
	if len(handles) > mediaFlowMaxProofCandidates {
		handles = handles[:mediaFlowMaxProofCandidates]
	}
	if err := s.ensureMediaFlowTorrentProofs(ctx, descriptor, handles, geometry); err != nil {
		return false, err
	}

	byRepresentation := make(map[string]contentproof.LaneCandidate, len(handles))
	representationIDs := make([]string, 0, len(handles))
	for _, handle := range handles {
		if _, exists := byRepresentation[handle.RepresentationID]; exists {
			continue
		}
		byRepresentation[handle.RepresentationID] = handle
		representationIDs = append(representationIDs, handle.RepresentationID)
	}
	proofs, err := s.store.ListContentProofsForWindow(ctx, representationIDs, s.nowUTC(), geometry.Offset, geometry.Length, mediaFlowMaxProofsPerRequest)
	if err != nil || len(proofs) == 0 {
		return false, err
	}
	source := &mediaFlowProofWindow{
		representationID: ticket.RepresentationID,
		data:             delivered,
		offset:           geometry.Offset,
		total:            geometry.Total,
	}
	for _, proof := range proofs {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		proofCandidate, ok := byRepresentation[proof.RepresentationID]
		if !ok || !contentproof.ProofMatchesLane(proofCandidate.Lane, proof.Provenance) {
			continue
		}
		aggregate := contentproof.LaneCandidate{
			Lane:             contentproof.LaneHTTP,
			ItemID:           ticket.RepresentationID,
			FileID:           mediaFlowOwnedRouteFileID,
			RepresentationID: ticket.RepresentationID,
			ReleaseKey:       proofCandidate.ReleaseKey,
			Size:             geometry.Total,
		}
		if err := contentproof.ValidateLaneCandidate(aggregate); err != nil {
			continue
		}
		var decision contentproof.Decision
		if proof.Provenance == contentproof.ProvenanceTorrentMerkle {
			digestFn, digestErr := s.mappedTorrentDigest(ctx, handles, proof)
			if digestErr != nil {
				continue
			}
			decision, err = s.httpProofGraph.MapHTTPRepresentationWithDigest(ctx, source, aggregate, proofCandidate, proof, digestFn)
		} else {
			decision, err = s.httpProofGraph.MapHTTPRepresentation(ctx, source, aggregate, proofCandidate, proof)
		}
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return false, ctxErr
			}
			continue
		}
		if decision.Relation != contentproof.RelationProven {
			continue
		}
		canonical, convergeErr := s.representationConvergence.ObserveVerified(ctx, aggregate, proofCandidate,
			contentproof.VerifiedSpan{Offset: proof.Offset, Length: proof.Length}, decision)
		if convergeErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return false, ctxErr
			}
			continue
		}
		if canonical != "" {
			return true, nil
		}
	}
	_, _, found, err := s.mediaFlowCanonical(ctx, ticket.RepresentationID)
	return found, err
}

// ensureMediaFlowTorrentProofs lazily projects the existing, URL-free
// TorrentMeta proof domain into the shared graph for one exact AIO torrent.
// It never fetches metadata, guesses by title, or persists descriptor state.
func (s *Server) ensureMediaFlowTorrentProofs(ctx context.Context, descriptor mediaFlowOwnedDescriptor, handles []contentproof.LaneCandidate, geometry mediaFlowGeometry) error {
	if descriptor.Kind != "torrent" || s == nil || s.store == nil || s.httpProofGraph == nil {
		return nil
	}
	hash := normalizeInfoHash(descriptor.Hash)
	if len(hash) != 40 {
		return nil
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return nil
	}
	windowEnd := geometry.Offset + geometry.Length
	for _, handle := range handles {
		if err := ctx.Err(); err != nil {
			return err
		}
		if handle.Lane != contentproof.LaneTorrent || handle.Size != geometry.Total {
			continue
		}
		item, err := s.store.GetItemByID(ctx, handle.ItemID)
		if err != nil {
			return err
		}
		if item == nil || item.State != store.StateReady || item.SourceType != store.SourceTypeTorrent {
			continue
		}
		itemHash := item.Metadata.RealInfoHash
		if itemHash == "" && item.InfoHash != nil {
			itemHash = *item.InfoHash
		}
		if normalizeInfoHash(itemHash) != hash {
			continue
		}
		meta, found, err := s.store.GetTorrentMeta(ctx, hash)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		var file *torrentmeta.FileEntry
		for i := range meta.Files {
			if meta.Files[i].Size != handle.Size {
				continue
			}
			if file != nil {
				return nil
			}
			file = &meta.Files[i]
		}
		if file == nil {
			return nil
		}
		native, err := torrentmeta.NewCrossLaneCandidate(meta, file, handle.ItemID, handle.FileID,
			handle.RepresentationID, item.DisplayName, handle.Size)
		if err != nil || native.Candidate.ReleaseKey != handle.ReleaseKey {
			return nil
		}
		cursor := geometry.Offset
		for recorded := 0; cursor < windowEnd && recorded < mediaFlowMaxProofsPerRequest; {
			proof, ok := native.Proofs.ProofAt(cursor)
			if !ok || proof.Offset > cursor || proof.Length <= 0 || proof.Offset+proof.Length <= cursor {
				return nil
			}
			proofEnd := proof.Offset + proof.Length
			if proof.Offset < geometry.Offset {
				cursor = proofEnd
				continue
			}
			if proofEnd > windowEnd {
				return nil
			}
			evidence := proof.Evidence(handle.RepresentationID, s.nowUTC())
			decision, verifyErr := s.httpProofGraph.VerifyDigest(ctx, evidence.RepresentationID, evidence.Scope,
				evidence.Offset, evidence.Length, evidence.Algorithm, evidence.Digest)
			if verifyErr != nil {
				return verifyErr
			}
			if decision.Relation == contentproof.RelationConflict {
				return nil
			}
			if decision.Relation == contentproof.RelationNoProof {
				if err := s.httpProofGraph.Record(ctx, evidence); err != nil {
					return err
				}
			}
			recorded++
			cursor = proofEnd
		}
		return nil
	}
	return nil
}

func (s *Server) resolveMediaFlowProofCandidates(ctx context.Context, releaseName string, size int64) ([]contentproof.LaneCandidate, error) {
	fromNNTP, err := s.ResolveNNTPCrossLaneCandidates(ctx, releaseName, size)
	if err != nil {
		return nil, err
	}
	fromTorrent, err := s.ResolveTorrentCrossLaneCandidates(ctx, releaseName, size)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	result := make([]contentproof.LaneCandidate, 0, mediaFlowMaxProofCandidates)
	for _, group := range [][]contentproof.LaneCandidate{fromNNTP, fromTorrent} {
		for _, candidate := range group {
			if seen[candidate.RepresentationID] {
				continue
			}
			seen[candidate.RepresentationID] = true
			result = append(result, candidate)
			if len(result) == mediaFlowMaxProofCandidates {
				return result, nil
			}
		}
	}
	return result, nil
}

func mediaFlowRangeBounds(requested *mediaFlowRange, total int64) (start, length int64, status int, ok bool) {
	if total <= 0 {
		return 0, 0, 0, false
	}
	if requested == nil {
		return 0, total, http.StatusOK, true
	}
	if requested.suffix > 0 {
		length = requested.suffix
		if length > total {
			length = total
		}
		return total - length, length, http.StatusPartialContent, true
	}
	if requested.start >= total {
		return 0, 0, http.StatusRequestedRangeNotSatisfiable, true
	}
	end := total - 1
	if requested.hasEnd && requested.end < end {
		end = requested.end
	}
	return requested.start, end - requested.start + 1, http.StatusPartialContent, true
}

func (s *Server) openMediaFlowCanonicalSource(ctx context.Context, route contentproof.LaneCandidate) (bytesource.ByteSource, error) {
	switch route.Lane {
	case contentproof.LaneTorrent:
		return s.openNNTPCrossLaneSource(ctx, route)
	case contentproof.LaneNNTP:
		return s.openTorrentCrossLaneSource(ctx, route)
	case contentproof.LaneHTTP:
		item, err := s.store.GetItemByID(ctx, route.ItemID)
		if err != nil || item == nil {
			return nil, errors.New("mediaflow: canonical HTTP route unavailable")
		}
		source, err := s.openHTTPCrossLaneSource(ctx, item, route, "rx-2.3-fallback")
		if governed, ok := source.(*governedHTTPRecoverySource); ok {
			// The MediaFlow request already owns the HTTP playback lease.
			return governed.ByteSource, err
		}
		return source, err
	default:
		return nil, errors.New("mediaflow: unsupported canonical route")
	}
}

type mediaFlowCanonicalReader struct {
	ctx            context.Context
	server         *Server
	target         contentproof.LaneCandidate
	routes         []contentproof.LaneCandidate
	candidates     []crosslane.Candidate
	cursor         int64
	end            int64
	proofWindowEnd int64
	proofs         []contentproof.Evidence
	buffer         []byte
}

func (s *Server) newMediaFlowCanonicalReader(ctx context.Context, representationID string, routes []contentproof.LaneCandidate, start, length, total int64) (*mediaFlowCanonicalReader, error) {
	var target contentproof.LaneCandidate
	for _, route := range routes {
		if route.RepresentationID == representationID && route.Lane == contentproof.LaneHTTP &&
			route.ItemID == representationID && route.FileID == mediaFlowOwnedRouteFileID && route.Size == total {
			target = route
			break
		}
	}
	if target.RepresentationID == "" || start < 0 || length <= 0 || start > total || length > total-start {
		return nil, errors.New("mediaflow: canonical target unavailable")
	}
	candidates := make([]crosslane.Candidate, 0, len(routes)-1)
	for _, route := range routes {
		if route.RepresentationID == representationID || route.Size != total || route.ReleaseKey != target.ReleaseKey {
			continue
		}
		source, err := s.openMediaFlowCanonicalSource(ctx, route)
		if err != nil || source == nil || source.Size() != total || !source.Caps().RangeSupport || !source.Caps().ExactSize {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		candidates = append(candidates, crosslane.Candidate{Handle: route, Source: source})
	}
	if len(candidates) == 0 {
		return nil, errors.New("mediaflow: canonical peer unavailable")
	}
	return &mediaFlowCanonicalReader{
		ctx: ctx, server: s, target: target, routes: routes, candidates: candidates, cursor: start, end: start + length,
	}, nil
}

func (r *mediaFlowCanonicalReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.cursor == r.end {
		return 0, io.EOF
	}
	if len(r.buffer) == 0 {
		if err := r.loadVerifiedBlock(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.buffer)
	r.buffer = r.buffer[n:]
	r.cursor += int64(n)
	return n, nil
}

func (r *mediaFlowCanonicalReader) loadVerifiedBlock() error {
	if r.cursor >= r.end {
		return io.EOF
	}
	if len(r.proofs) == 0 || r.cursor >= r.proofWindowEnd {
		windowLength := r.end - r.cursor
		if windowLength > mediaFlowMaxProofWindowBytes {
			windowLength = mediaFlowMaxProofWindowBytes
		}
		proofs, err := r.server.store.ListContentProofsIntersectingWindow(r.ctx, []string{r.target.RepresentationID},
			r.server.nowUTC(), r.cursor, windowLength, mediaFlowMaxProofsPerRequest)
		if err != nil || len(proofs) == 0 {
			if err != nil {
				return err
			}
			return errors.New("mediaflow: canonical range lacks proof")
		}
		r.proofs = proofs
		r.proofWindowEnd = r.cursor + windowLength
	}

	var selected contentproof.Evidence
	for _, proof := range r.proofs {
		if proof.RepresentationID != r.target.RepresentationID || proof.Kind != contentproof.KindAuthoritative ||
			proof.Offset < 0 || proof.Length <= 0 || proof.Length > mediaFlowMaxProofWindowBytes ||
			proof.Offset > r.target.Size || proof.Length > r.target.Size-proof.Offset ||
			proof.Offset > r.cursor || proof.Offset+proof.Length <= r.cursor || !mediaFlowProofMayAuthorizeAggregate(proof.Provenance) {
			continue
		}
		if selected.Length == 0 || proof.Offset+proof.Length < selected.Offset+selected.Length {
			selected = proof
		}
	}
	if selected.Length == 0 {
		return errors.New("mediaflow: canonical range has a proof gap")
	}
	var digestFn func(context.Context, []byte) ([]byte, error)
	if selected.Provenance == contentproof.ProvenanceTorrentMerkle {
		var err error
		digestFn, err = r.server.mappedTorrentDigest(r.ctx, r.routes, selected)
		if err != nil {
			return errors.New("mediaflow: canonical Merkle geometry unavailable")
		}
	}
	block, err := crosslane.NewCoordinator(r.server.httpProofGraph, r.server.torrentGov).RecoverBlock(r.ctx, crosslane.Request{
		Target: r.target, Proof: selected, Candidates: r.candidates, CandidateDigest: digestFn,
	})
	if err != nil || block.Decision.Relation != contentproof.RelationProven || int64(len(block.Data)) != selected.Length {
		if r.ctx.Err() != nil {
			return r.ctx.Err()
		}
		return errors.New("mediaflow: canonical peer proof abstained")
	}
	start := r.cursor - selected.Offset
	end := selected.Length
	if selected.Offset+end > r.end {
		end = r.end - selected.Offset
	}
	if start < 0 || end <= start || end > int64(len(block.Data)) {
		return errors.New("mediaflow: canonical proof overlap absent")
	}
	r.buffer = block.Data[start:end]
	return nil
}

func mediaFlowProofMayAuthorizeAggregate(provenance contentproof.Provenance) bool {
	return contentproof.ProofMatchesLane(contentproof.LaneHTTP, provenance) || provenance == contentproof.ProvenanceTorrentPiece ||
		provenance == contentproof.ProvenanceTorrentMerkle || provenance == contentproof.ProvenancePAR2IFSC
}

// serveMediaFlowCanonicalFallback emits only a route already admitted by the
// convergence owner. It returns false without touching w when no proven peer
// can serve the request.
func (s *Server) serveMediaFlowCanonicalFallback(w http.ResponseWriter, r *http.Request, ticket mediaFlowTicket, requested *mediaFlowRange) bool {
	canonical, total, found, err := s.mediaFlowCanonical(r.Context(), ticket.RepresentationID)
	if err != nil || !found {
		return false
	}
	start, length, status, ok := mediaFlowRangeBounds(requested, total)
	if !ok {
		return false
	}
	if status == http.StatusRequestedRangeNotSatisfiable {
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(total, 10))
		w.WriteHeader(status)
		return true
	}
	routes, err := s.representationConvergence.Routes(r.Context(), ticket.RepresentationID)
	if err != nil || len(routes) == 0 || len(routes) > contentproof.DefaultMaxRoutesPerRepresentation {
		return false
	}
	reader, err := s.newMediaFlowCanonicalReader(r.Context(), ticket.RepresentationID, routes, start, length, total)
	if err != nil {
		return false
	}
	var prefix []byte
	if r.Method != http.MethodHead {
		probeLength := length
		if probeLength > mediaFlowFallbackProbeBytes {
			probeLength = mediaFlowFallbackProbeBytes
		}
		prefix = make([]byte, int(probeLength))
		if _, err := io.ReadFull(reader, prefix); err != nil {
			return false
		}
	}

	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	if status == http.StatusPartialContent {
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(start+length-1, 10)+"/"+strconv.FormatInt(total, 10))
	}
	for name, value := range ticket.ResponseHeaders {
		w.Header().Set(name, value)
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", fileExtContentType(ticket.Filename))
	}
	if ticket.Filename != "" && w.Header().Get("Content-Disposition") == "" {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": ticket.Filename}))
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return true
	}

	bufp := httpStreamCopyBufPool.Get().(*[]byte)
	ctx := playbackcoverage.WithRepresentation(r.Context(), canonical)
	_, _ = s.copyPlaybackBody(ctx, w, io.MultiReader(bytes.NewReader(prefix), reader), *bufp, start, "mediaflow_canonical", nil)
	httpStreamCopyBufPool.Put(bufp)
	return true
}
