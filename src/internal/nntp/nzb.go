package nntp

import (
	"encoding/xml"
	"fmt"
	"sort"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/util"
)

// NZBFile represents a single file entry within an NZB document.
type NZBFile struct {
	Subject    string
	Poster     string
	Date       int64
	Groups     []string
	Segments   []NZBSegment
	TotalBytes int64
}

// NZB is a parsed NZB document, including optional metadata from its head.
type NZB struct {
	Password string
	Title    string
	Category string
	Files    []NZBFile
}

// NZBSegment represents a single article segment within an NZB file.
type NZBSegment struct {
	Number    int    // segment number (1-based)
	Bytes     int64  // declared byte count
	MessageID string // article message-id, without angle brackets
	PostedAt  int64  `json:"posted_at,omitempty"` // parent NZB file Unix post time (NS-8.4)
}

// ContentKey returns a URL-free canonical identity for NZB article content.
// Equivalent XML wrappers and file orderings produce the same key.
func ContentKey(raw []byte) string {
	nzb, err := ParseNZB(raw)
	if err != nil {
		return "nzb:" + util.SHA1Hex(string(raw))
	}
	segments := make([]string, 0)
	for _, file := range nzb.Files {
		for _, segment := range file.Segments {
			if segment.MessageID != "" {
				segments = append(segments, fmt.Sprintf("%s\x00%d\x00%d", segment.MessageID, segment.Number, segment.Bytes))
			}
		}
	}
	if len(segments) == 0 {
		return "nzb:" + util.SHA1Hex(string(raw))
	}
	sort.Strings(segments)
	return "nzb:" + util.SHA1Hex(strings.Join(segments, "\n"))
}

// SegmentOffsetKey scopes one decoded-size map to a stable file identity
// within an NZB. File indexes are not stable when equivalent NZBs order
// equal-sized files differently.
func SegmentOffsetKey(contentKey string, segments []NZBSegment) string {
	parts := make([]string, 0, len(segments))
	for _, segment := range segments {
		parts = append(parts, fmt.Sprintf("%s\x00%d", segment.MessageID, segment.Number))
	}
	sort.Strings(parts)
	return contentKey + ":file:" + util.SHA1Hex(strings.Join(parts, "\n"))
}

// xmlNZB is the internal XML parse target. Handles the optional namespace.
type xmlNZB struct {
	Head  xmlHead   `xml:"head"`
	Files []xmlFile `xml:"file"`
}

type xmlHead struct {
	Meta []xmlMeta `xml:"meta"`
}

type xmlMeta struct {
	Type  string `xml:"type,attr"`
	Value string `xml:",chardata"`
}

type xmlFile struct {
	Poster   string   `xml:"poster,attr"`
	Date     int64    `xml:"date,attr"`
	Subject  string   `xml:"subject,attr"`
	Groups   []string `xml:"groups>group"`
	Segments []xmlSeg `xml:"segments>segment"`
}

type xmlSeg struct {
	Bytes     int64  `xml:"bytes,attr"`
	Number    int    `xml:"number,attr"`
	MessageID string `xml:",chardata"`
}

// ParseNZB parses an NZB XML byte slice. Files are sorted by TotalBytes
// descending; callers pick nzb.Files[0] as the main media file.
func ParseNZB(data []byte) (NZB, error) {
	// Strip the DOCTYPE and namespace that breaks stdlib xml.
	// Replace the namespace-qualified root with a plain <nzb> tag.
	s := string(data)
	// Remove DOCTYPE declaration
	for strings.Contains(s, "<!DOCTYPE") {
		start := strings.Index(s, "<!DOCTYPE")
		end := strings.Index(s[start:], ">")
		if end < 0 {
			break
		}
		s = s[:start] + s[start+end+1:]
	}
	// Strip xmlns attribute so stdlib xml doesn't require namespace-aware parsing
	s = strings.ReplaceAll(s, ` xmlns="http://www.newzbin.com/DTD/2003/nzb"`, "")

	var root xmlNZB
	if err := xml.Unmarshal([]byte(s), &root); err != nil {
		return NZB{}, fmt.Errorf("nntp: parse nzb: %w", err)
	}

	nzb := NZB{}
	for _, meta := range root.Head.Meta {
		value := strings.TrimSpace(meta.Value)
		switch strings.ToLower(strings.TrimSpace(meta.Type)) {
		case "password":
			if nzb.Password == "" {
				nzb.Password = value
			}
		case "title":
			if nzb.Title == "" {
				nzb.Title = value
			}
		case "category":
			if nzb.Category == "" {
				nzb.Category = value
			}
		}
	}

	files := make([]NZBFile, 0, len(root.Files))
	for _, xf := range root.Files {
		segs := make([]NZBSegment, 0, len(xf.Segments))
		var total int64
		for _, xs := range xf.Segments {
			msgID := strings.TrimSpace(xs.MessageID)
			msgID = strings.TrimPrefix(msgID, "<")
			msgID = strings.TrimSuffix(msgID, ">")
			segs = append(segs, NZBSegment{
				Number:    xs.Number,
				Bytes:     xs.Bytes,
				MessageID: msgID,
				PostedAt:  xf.Date,
			})
			total += xs.Bytes
		}
		sort.Slice(segs, func(i, j int) bool { return segs[i].Number < segs[j].Number })
		files = append(files, NZBFile{
			Subject:    xf.Subject,
			Poster:     xf.Poster,
			Date:       xf.Date,
			Groups:     xf.Groups,
			Segments:   segs,
			TotalBytes: total,
		})
	}
	// Largest file first
	sort.Slice(files, func(i, j int) bool { return files[i].TotalBytes > files[j].TotalBytes })
	nzb.Files = files
	return nzb, nil
}
