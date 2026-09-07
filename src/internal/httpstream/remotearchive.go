package httpstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strings"

	archiveparser "github.com/darkharrbor/darkharrbor/internal/archive"
	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

// IsRemoteArchiveSelector reports whether selector belongs to the stable
// descriptor namespace emitted by the shared Stremio archive normalizer.
func IsRemoteArchiveSelector(selector string) bool {
	return strings.HasPrefix(selector, "archive:zip:") || strings.HasPrefix(selector, "archive:rar:") ||
		strings.HasPrefix(selector, "archive:7z:") || strings.HasPrefix(selector, "archive:tgz:") ||
		strings.HasPrefix(selector, "archive:tar:")
}

// MatchRemoteArchiveDescriptor applies the same exact-one selector rule used
// by MatchSelector. Descriptor order never chooses a source implicitly.
func MatchRemoteArchiveDescriptor(descriptors []RemoteArchiveDescriptor, selector string) (RemoteArchiveDescriptor, error) {
	var matched RemoteArchiveDescriptor
	count := 0
	for _, descriptor := range descriptors {
		if descriptor.Selector == selector {
			matched = descriptor
			count++
		}
	}
	if count != 1 {
		return RemoteArchiveDescriptor{}, NewError(ClassRepresentationLost, "remote archive descriptor is missing or ambiguous")
	}
	return matched, nil
}

// SelectRemoteArchiveMember inspects a descriptor through the shared archive
// reader and returns exactly one safe member. ZIP is the only format enabled
// by HR2.6; other formats abstain until a ByteSource-safe reader is proven.
func SelectRemoteArchiveMember(ctx context.Context, descriptor RemoteArchiveDescriptor, sources []bytesource.ByteSource) (RemoteArchiveMember, error) {
	if err := ctx.Err(); err != nil {
		return RemoteArchiveMember{}, err
	}
	if descriptor.Format != RemoteArchiveZIP {
		return RemoteArchiveMember{}, NewError(ClassNoSource, "remote archive format has no enabled shared reader")
	}
	if len(descriptor.Parts) != 1 || len(sources) != 1 || sources[0] == nil {
		return RemoteArchiveMember{}, NewError(ClassUpstreamMalformed, "remote ZIP requires exactly one source part")
	}
	entries, _, err := archiveparser.ParseZIPVideoEntries(ctx, sources[0])
	if err != nil {
		return RemoteArchiveMember{}, Wrapf(ClassUpstreamMalformed, "remote ZIP inspection failed: %s", Sanitize(err))
	}
	entry, err := selectZIPEntry(entries, descriptor.MemberHint)
	if err != nil {
		return RemoteArchiveMember{}, err
	}
	sum := sha256.Sum256([]byte(descriptor.Selector + "\x00" + entry.Filename))
	return RemoteArchiveMember{
		Selector:          "archive:" + hex.EncodeToString(sum[:16]),
		Name:              path.Base(strings.ReplaceAll(entry.Filename, "\\", "/")),
		Size:              entry.Uncompressed,
		Format:            RemoteArchiveZIP,
		DataOffset:        entry.DataOff,
		CompressedSize:    entry.CompressedSize,
		CompressionMethod: entry.Method,
	}, nil
}

func selectZIPEntry(entries []archiveparser.ZIPEntry, hint string) (archiveparser.ZIPEntry, error) {
	hint = strings.TrimSpace(strings.ReplaceAll(hint, "\\", "/"))
	if hint == "" {
		if len(entries) != 1 {
			return archiveparser.ZIPEntry{}, NewError(ClassRepresentationLost, "remote ZIP member selection is ambiguous")
		}
		return entries[0], nil
	}
	var matches []archiveparser.ZIPEntry
	for _, entry := range entries {
		name := strings.ReplaceAll(entry.Filename, "\\", "/")
		if name == hint || path.Base(name) == path.Base(hint) {
			matches = append(matches, entry)
		}
	}
	if len(matches) != 1 {
		return archiveparser.ZIPEntry{}, NewError(ClassRepresentationLost, "remote ZIP member hint is missing or ambiguous")
	}
	return matches[0], nil
}

// OpenRemoteArchiveMember selects one safe member from a remote archive
// descriptor (via SelectRemoteArchiveMember) and, when that member is
// stored/uncompressed, exposes it as its own logical ByteSource using the
// shared archive.NewArchiveMemberByteSource adapter (HR5.1) -- no archive
// bytes are downloaded in full or extracted to disk; reads are served by
// random access against the already-open whole-archive source. A compressed
// member fails closed with ClassNoSource: compressed members cannot be
// exposed as random-access reads without full extraction, and multipart
// archives already abstain earlier inside SelectRemoteArchiveMember (ZIP is
// presently the only enabled format; RAR/7z/TGZ/TAR remain unimplemented
// here pending their own proven adapters, matching HR2.6's own scope note).
func OpenRemoteArchiveMember(ctx context.Context, descriptor RemoteArchiveDescriptor, sources []bytesource.ByteSource) (RemoteArchiveMember, bytesource.ByteSource, error) {
	member, err := SelectRemoteArchiveMember(ctx, descriptor, sources)
	if err != nil {
		return RemoteArchiveMember{}, nil, err
	}
	// SelectRemoteArchiveMember currently only returns ZIP members with
	// exactly one source part (Part 0); this stays defensive rather than
	// assuming that shape survives future format additions unchanged.
	if member.Part < 0 || member.Part >= len(sources) || sources[member.Part] == nil {
		return RemoteArchiveMember{}, nil, NewError(ClassUpstreamMalformed, "remote archive member part out of range")
	}
	if member.CompressionMethod != 0 {
		return RemoteArchiveMember{}, nil, NewError(ClassNoSource, "remote archive member is compressed; only stored members stream without full extraction")
	}
	memberSrc, err := archiveparser.NewArchiveMemberByteSource(member.Selector, sources[member.Part], member.DataOffset, member.CompressedSize)
	if err != nil {
		return RemoteArchiveMember{}, nil, Wrapf(ClassUpstreamMalformed, "remote archive member span invalid: %s", Sanitize(err))
	}
	return member, memberSrc, nil
}
