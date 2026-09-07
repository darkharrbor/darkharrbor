package contentproof

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"unicode"
)

const (
	// MaxLaneCandidates bounds one in-memory cross-lane lookup.
	MaxLaneCandidates = 2048
	// MaxReleaseNameBytes bounds normalization input before allocation.
	MaxReleaseNameBytes = 1024
)

const releaseKeyDomain = "darkharrbor/crosslane-release/v1\x00"

// Lane identifies a candidate's acquisition lane without carrying a source
// locator.
type Lane string

const (
	LaneTorrent Lane = "torrent"
	LaneNNTP    Lane = "nntp"
	LaneHTTP    Lane = "http"
)

// LaneCandidate is an ephemeral, URL-free handle to bytes another lane can
// obtain. ReleaseKey is a domain-separated digest of normalized release name
// plus exact size; the raw release name is deliberately not retained.
type LaneCandidate struct {
	Lane             Lane
	ItemID           string
	FileID           string
	RepresentationID string
	ReleaseKey       string
	Size             int64
}

// NewLaneCandidate constructs a candidate only from bounded, opaque
// identifiers. A later coordinator still has to prove candidate bytes; a name
// and size match alone never authorizes them.
func NewLaneCandidate(lane Lane, itemID, fileID, representationID, releaseName string, size int64) (LaneCandidate, error) {
	key, err := ReleaseKey(releaseName, size)
	if err != nil {
		return LaneCandidate{}, err
	}
	candidate := LaneCandidate{
		Lane:             lane,
		ItemID:           itemID,
		FileID:           fileID,
		RepresentationID: representationID,
		ReleaseKey:       key,
		Size:             size,
	}
	if err := validateLaneCandidate(candidate); err != nil {
		return LaneCandidate{}, err
	}
	return candidate, nil
}

// ReleaseKey returns the stable lookup key for normalized release name plus
// exact size. It never returns the name itself.
func ReleaseKey(releaseName string, size int64) (string, error) {
	if size <= 0 {
		return "", errors.New("contentproof: candidate size must be positive")
	}
	if len(releaseName) == 0 || len(releaseName) > MaxReleaseNameBytes {
		return "", errors.New("contentproof: invalid candidate release name length")
	}
	if strings.Contains(releaseName, "://") || strings.ContainsAny(releaseName, "\x00\r\n") {
		return "", errors.New("contentproof: candidate release name is not URL-free")
	}
	normalized := normalizeReleaseName(releaseName)
	if normalized == "" {
		return "", errors.New("contentproof: candidate release name is empty after normalization")
	}
	sum := sha256.Sum256([]byte(releaseKeyDomain + normalized + "\x00" + strconv.FormatInt(size, 10)))
	return hex.EncodeToString(sum[:]), nil
}

// ResolveLaneCandidates returns every valid exact name+size match in input
// order. Multiple matches remain multiple: choosing among them without proof
// belongs to the later recovery coordinator and is never guessed here.
func ResolveLaneCandidates(ctx context.Context, releaseName string, size int64, candidates []LaneCandidate) ([]LaneCandidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(candidates) > MaxLaneCandidates {
		return nil, errors.New("contentproof: too many lane candidates")
	}
	key, err := ReleaseKey(releaseName, size)
	if err != nil {
		return nil, err
	}
	matches := make([]LaneCandidate, 0, len(candidates))
	for i, candidate := range candidates {
		if i%64 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if err := validateLaneCandidate(candidate); err != nil {
			return nil, err
		}
		if candidate.ReleaseKey == key && candidate.Size == size {
			matches = append(matches, candidate)
		}
	}
	return matches, nil
}

func normalizeReleaseName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	separator := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if separator && b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteRune(r)
			separator = false
			continue
		}
		separator = true
	}
	return b.String()
}

// ValidateLaneCandidate verifies that a transient candidate contains only bounded,
// URL-free owner fields.
func ValidateLaneCandidate(candidate LaneCandidate) error {
	return validateLaneCandidate(candidate)
}

func validateLaneCandidate(candidate LaneCandidate) error {
	switch candidate.Lane {
	case LaneTorrent, LaneNNTP, LaneHTTP:
	default:
		return errors.New("contentproof: unknown candidate lane")
	}
	if err := validateOpaqueID("candidate item", candidate.ItemID); err != nil {
		return err
	}
	if err := validateOpaqueID("candidate file", candidate.FileID); err != nil {
		return err
	}
	if err := validateOpaqueID("candidate representation", candidate.RepresentationID); err != nil {
		return err
	}
	if candidate.Size <= 0 {
		return errors.New("contentproof: candidate size must be positive")
	}
	decoded, err := hex.DecodeString(candidate.ReleaseKey)
	if err != nil || len(decoded) != sha256.Size || candidate.ReleaseKey != strings.ToLower(candidate.ReleaseKey) {
		return errors.New("contentproof: invalid candidate release key")
	}
	return nil
}
