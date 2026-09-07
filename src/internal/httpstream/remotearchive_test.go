package httpstream

import (
	stdzip "archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

func remoteZIP(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := stdzip.NewWriter(&buf)
	for name, body := range files {
		h := &stdzip.FileHeader{Name: name, Method: stdzip.Store}
		f, err := w.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSelectRemoteArchiveMember(t *testing.T) {
	data := remoteZIP(t, map[string]string{"movie.mkv": "video", "readme.txt": "text"})
	desc := RemoteArchiveDescriptor{Selector: "stable", Format: RemoteArchiveZIP, Parts: []ResolvedFile{{Selector: "part"}}}
	member, err := SelectRemoteArchiveMember(context.Background(), desc, []bytesource.ByteSource{bytesource.NewMemSource("zip", data)})
	if err != nil {
		t.Fatal(err)
	}
	if member.Name != "movie.mkv" || member.Size != 5 || member.Selector == "" {
		t.Fatalf("unexpected member: %+v", member)
	}
}

func TestMatchRemoteArchiveDescriptorExactOne(t *testing.T) {
	descriptors := []RemoteArchiveDescriptor{{Selector: "archive:zip:one"}, {Selector: "archive:zip:two"}}
	if got, err := MatchRemoteArchiveDescriptor(descriptors, "archive:zip:two"); err != nil || got.Selector != "archive:zip:two" {
		t.Fatalf("match=%+v err=%v", got, err)
	}
	if _, err := MatchRemoteArchiveDescriptor(descriptors, "archive:zip:missing"); ClassOf(err) != ClassRepresentationLost {
		t.Fatalf("missing class=%s err=%v", ClassOf(err), err)
	}
	duplicate := append(descriptors, RemoteArchiveDescriptor{Selector: "archive:zip:two"})
	if _, err := MatchRemoteArchiveDescriptor(duplicate, "archive:zip:two"); ClassOf(err) != ClassRepresentationLost {
		t.Fatalf("duplicate class=%s err=%v", ClassOf(err), err)
	}
	for selector, want := range map[string]bool{
		"archive:zip:one": true, "archive:rar:one": true, "archive:unknown:one": false, "video": false,
	} {
		if got := IsRemoteArchiveSelector(selector); got != want {
			t.Fatalf("IsRemoteArchiveSelector(%q)=%t want %t", selector, got, want)
		}
	}
}

func TestSelectRemoteArchiveMemberFailsClosed(t *testing.T) {
	data := remoteZIP(t, map[string]string{"a.mkv": "a", "b.mkv": "b"})
	desc := RemoteArchiveDescriptor{Selector: "stable", Format: RemoteArchiveZIP, Parts: []ResolvedFile{{Selector: "part"}}}
	src := []bytesource.ByteSource{bytesource.NewMemSource("zip", data)}
	if _, err := SelectRemoteArchiveMember(context.Background(), desc, src); ClassOf(err) != ClassRepresentationLost {
		t.Fatalf("ambiguous selection error = %v", err)
	}
	desc.MemberHint = "b.mkv"
	member, err := SelectRemoteArchiveMember(context.Background(), desc, src)
	if err != nil || member.Name != "b.mkv" {
		t.Fatalf("hint selection member=%+v err=%v", member, err)
	}
	desc.Format = RemoteArchiveRAR
	if _, err := SelectRemoteArchiveMember(context.Background(), desc, src); ClassOf(err) != ClassNoSource {
		t.Fatalf("unsupported format error = %v", err)
	}
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := SelectRemoteArchiveMember(cctx, desc, src); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
}

func TestRemoteArchiveDescriptorMarshalOmitsSources(t *testing.T) {
	desc := RemoteArchiveDescriptor{
		Selector: "stable", Format: RemoteArchiveZIP, MemberHint: "movie.mkv",
		Parts: []ResolvedFile{{Selector: "part", URL: "https://source.invalid/archive.zip", RequestHeaders: map[string]string{"X-Test": "private"}}},
	}
	encoded, err := json.Marshal(desc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "source.invalid") || strings.Contains(string(encoded), "private") {
		t.Fatal("transient archive source escaped JSON protection")
	}
}

func TestOpenRemoteArchiveMemberStoredZIP(t *testing.T) {
	data := remoteZIP(t, map[string]string{"movie.mkv": "the-real-video-bytes"})
	desc := RemoteArchiveDescriptor{Selector: "stable", Format: RemoteArchiveZIP, Parts: []ResolvedFile{{Selector: "part"}}}
	src := bytesource.NewMemSource("zip", data)
	member, memberSrc, err := OpenRemoteArchiveMember(context.Background(), desc, []bytesource.ByteSource{src})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if member.CompressionMethod != 0 {
		t.Fatalf("CompressionMethod = %d, want 0 (stored)", member.CompressionMethod)
	}
	if memberSrc.Size() != member.Size {
		t.Fatalf("memberSrc.Size() = %d, want member.Size = %d", memberSrc.Size(), member.Size)
	}
	buf := make([]byte, memberSrc.Size())
	n, err := memberSrc.ReadAt(context.Background(), buf, 0)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if got, want := string(buf[:n]), "the-real-video-bytes"; got != want {
		t.Fatalf("member bytes = %q, want %q", got, want)
	}
	// The member source never downloaded or buffered the whole archive: it
	// reads directly from the same underlying source at the member's own
	// data offset, so a partial mid-member read is also byte-exact.
	partial := make([]byte, 4)
	n, err = memberSrc.ReadAt(context.Background(), partial, 4)
	if err != nil {
		t.Fatalf("ReadAt partial: %v", err)
	}
	if got, want := string(partial[:n]), "real"; got != want {
		t.Fatalf("partial member bytes = %q, want %q", got, want)
	}
}

func TestOpenRemoteArchiveMemberCompressedFailsClosed(t *testing.T) {
	var buf bytes.Buffer
	w := stdzip.NewWriter(&buf)
	f, err := w.CreateHeader(&stdzip.FileHeader{Name: "movie.mkv", Method: stdzip.Deflate})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(strings.Repeat("compressible-video-bytes ", 50))); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	desc := RemoteArchiveDescriptor{Selector: "stable", Format: RemoteArchiveZIP, Parts: []ResolvedFile{{Selector: "part"}}}
	src := bytesource.NewMemSource("zip", buf.Bytes())
	_, memberSrc, err := OpenRemoteArchiveMember(context.Background(), desc, []bytesource.ByteSource{src})
	if err == nil {
		t.Fatalf("expected error for compressed member, got nil (memberSrc=%v)", memberSrc)
	}
	if ClassOf(err) != ClassNoSource {
		t.Fatalf("ClassOf(err) = %q, want %q", ClassOf(err), ClassNoSource)
	}
	if memberSrc != nil {
		t.Fatalf("memberSrc = %v, want nil on failure", memberSrc)
	}
}

func TestOpenRemoteArchiveMemberPropagatesSelectionFailure(t *testing.T) {
	desc := RemoteArchiveDescriptor{Selector: "stable", Format: RemoteArchiveRAR, Parts: []ResolvedFile{{Selector: "part"}}}
	_, memberSrc, err := OpenRemoteArchiveMember(context.Background(), desc, []bytesource.ByteSource{bytesource.NewMemSource("rar", []byte("not a zip"))})
	if err == nil {
		t.Fatalf("expected error for unsupported format, got nil")
	}
	if memberSrc != nil {
		t.Fatalf("memberSrc = %v, want nil on failure", memberSrc)
	}
}

func TestOpenRemoteArchiveMemberCancellation(t *testing.T) {
	data := remoteZIP(t, map[string]string{"movie.mkv": "video"})
	desc := RemoteArchiveDescriptor{Selector: "stable", Format: RemoteArchiveZIP, Parts: []ResolvedFile{{Selector: "part"}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := OpenRemoteArchiveMember(ctx, desc, []bytesource.ByteSource{bytesource.NewMemSource("zip", data)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
