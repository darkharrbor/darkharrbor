package httpstream

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// PersistedFile is the URL-free file DTO stored in items.file_list for
// SourceTypeHTTP items (HS-1.9). This is the ONLY shape that may be
// persisted for HTTP files — never marshal a ResolvedFile. Adding a URL,
// query, or header field to this struct is a persistence-invariant
// violation; the redaction test enforces the field set.
type PersistedFile struct {
	// FileID is the DH-generated path-safe opaque file ID (D10). It is the
	// value embedded in /stream/{item}/{file} paths and stream tokens.
	FileID string `json:"file_id"`
	// Selector is the handler-stable representation selector this file was
	// grabbed as; play-time resolves must match it exactly (D10).
	Selector    string `json:"selector"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type,omitempty"`
	// ArchiveSize is the verified size of the transient parent archive when
	// this file is a stored remote-archive member. It is URL-free and lets
	// playback rebuild the parent ByteSource without persisting its location.
	// Zero retains the ordinary progressive/HLS file contract.
	ArchiveSize int64 `json:"archive_size,omitempty"`
	// RangeVerified is true once a grab-time bytes=0-0 preflight (D9)
	// confirmed coherent 206 semantics from DH's egress. HEAD advertises
	// Accept-Ranges only when set (HS-1.7).
	RangeVerified bool `json:"range_verified,omitempty"`
}

var fileIDRe = regexp.MustCompile(`^hf[0-9a-f]{12}$`)

// NewFileID returns a fresh path-safe opaque file ID ("hf" + 12 hex).
func NewFileID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate file id: %w", err)
	}
	return "hf" + hex.EncodeToString(b[:]), nil
}

// ValidFileID reports whether s is a DH-generated HTTP file ID.
func ValidFileID(s string) bool { return fileIDRe.MatchString(s) }

// MarshalFiles encodes the persisted file list for items.file_list.
func MarshalFiles(files []PersistedFile) (string, error) {
	b, err := json.Marshal(files)
	if err != nil {
		return "", fmt.Errorf("marshal http file list: %w", err)
	}
	return string(b), nil
}

// ParseFiles decodes items.file_list for an HTTP item.
func ParseFiles(s string) ([]PersistedFile, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var files []PersistedFile
	if err := json.Unmarshal([]byte(s), &files); err != nil {
		return nil, fmt.Errorf("parse http file list: %w", err)
	}
	return files, nil
}

// LookupFile finds a file strictly by stored FileID. There is NO positional
// fallback for HTTP items (D10) — an unknown ID is a 404, never an ordinal.
func LookupFile(files []PersistedFile, fileID string) (PersistedFile, bool) {
	for _, f := range files {
		if f.FileID == fileID {
			return f, true
		}
	}
	return PersistedFile{}, false
}
