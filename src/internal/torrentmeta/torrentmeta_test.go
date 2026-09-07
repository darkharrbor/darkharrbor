package torrentmeta

import (
	"bytes"
	"crypto/sha1" // #nosec G505 -- verifying the package's own v1 infohash computation against an independently computed reference.
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

// ---- minimal test-only bencode encoder, used only to build canned fixtures
// with byte-exact control (so the info-dict hash assertions below are
// self-verifying, not dependent on any external "real" .torrent file). ----

func bStr(s string) string   { return fmt.Sprintf("%d:%s", len(s), s) }
func bBytes(b []byte) string { return fmt.Sprintf("%d:%s", len(b), b) }
func bInt(n int64) string    { return fmt.Sprintf("i%de", n) }
func bDict(pairsInSortedKeyOrder ...string) string {
	return "d" + strings.Join(pairsInSortedKeyOrder, "") + "e"
}
func bList(items ...string) string { return "l" + strings.Join(items, "") + "e" }

func sha1Hex(s string) string {
	sum := sha1.Sum([]byte(s)) // #nosec G401 -- reference computation mirroring the BitTorrent v1 infohash definition.
	return hex.EncodeToString(sum[:])
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestParseSingleFileV1(t *testing.T) {
	pieceHash := bytes.Repeat([]byte{0xAB}, 20) // one fake 20-byte piece hash
	info := bDict(
		bStr("length")+bInt(123456),
		bStr("name")+bStr("Movie.2024.1080p.mkv"),
		bStr("piece length")+bInt(16384),
		bStr("pieces")+bBytes(pieceHash),
	)
	torrent := bDict(
		bStr("announce")+bStr("http://tracker.example/announce"),
		bStr("info")+info,
	)

	meta, err := Parse([]byte(torrent))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if meta.MetaVersion != 1 {
		t.Fatalf("MetaVersion = %d, want 1", meta.MetaVersion)
	}
	if meta.InfoHashV1 != sha1Hex(info) {
		t.Fatalf("InfoHashV1 = %s, want %s", meta.InfoHashV1, sha1Hex(info))
	}
	if meta.InfoHashV2 != "" {
		t.Fatalf("InfoHashV2 = %q, want empty for a pure v1 torrent", meta.InfoHashV2)
	}
	if meta.PieceLength != 16384 {
		t.Fatalf("PieceLength = %d, want 16384", meta.PieceLength)
	}
	if !bytes.Equal(meta.PieceHashesV1, pieceHash) {
		t.Fatalf("PieceHashesV1 mismatch")
	}
	want := []FileEntry{{Path: "Movie.2024.1080p.mkv", Size: 123456, StartOffset: 0, EndOffset: 123456}}
	if len(meta.Files) != 1 || meta.Files[0] != want[0] {
		t.Fatalf("Files = %+v, want %+v", meta.Files, want)
	}
}

func TestParseMultiFileV1SeasonPackPreMapsExtents(t *testing.T) {
	file1 := bDict(bStr("length")+bInt(1000), bStr("path")+bList(bStr("S01E01.mkv")))
	file2 := bDict(bStr("length")+bInt(2000), bStr("path")+bList(bStr("S01E02.mkv")))
	info := bDict(
		bStr("files")+bList(file1, file2),
		bStr("name")+bStr("Show.S01.Pack"),
		bStr("piece length")+bInt(16384),
		bStr("pieces")+bBytes(bytes.Repeat([]byte{0xCD}, 40)),
	)
	torrent := bDict(bStr("info") + info)

	meta, err := Parse([]byte(torrent))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []FileEntry{
		{Path: "Show.S01.Pack/S01E01.mkv", Size: 1000, StartOffset: 0, EndOffset: 1000},
		{Path: "Show.S01.Pack/S01E02.mkv", Size: 2000, StartOffset: 1000, EndOffset: 3000},
	}
	if len(meta.Files) != len(want) {
		t.Fatalf("Files = %+v, want %+v", meta.Files, want)
	}
	for i := range want {
		if meta.Files[i] != want[i] {
			t.Fatalf("Files[%d] = %+v, want %+v", i, meta.Files[i], want[i])
		}
	}
}

func TestParseHybridV1V2(t *testing.T) {
	piecesRoot := bytes.Repeat([]byte{0xEF}, 32)
	fileLeaf := bDict(bStr("") + bDict(bStr("length")+bInt(5000), bStr("pieces root")+bBytes(piecesRoot)))
	fileTree := bDict(bStr("Movie.hybrid.mkv") + fileLeaf)
	info := bDict(
		bStr("file tree")+fileTree,
		bStr("length")+bInt(5000),
		bStr("meta version")+bInt(2),
		bStr("name")+bStr("Movie.hybrid.mkv"),
		bStr("piece length")+bInt(16384),
		bStr("pieces")+bBytes(bytes.Repeat([]byte{0x11}, 20)),
	)
	torrent := bDict(bStr("info") + info)

	meta, err := Parse([]byte(torrent))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if meta.MetaVersion != 2 {
		t.Fatalf("MetaVersion = %d, want 2", meta.MetaVersion)
	}
	if meta.InfoHashV1 != sha1Hex(info) {
		t.Fatalf("InfoHashV1 = %s, want %s", meta.InfoHashV1, sha1Hex(info))
	}
	if meta.InfoHashV2 != sha256Hex(info) {
		t.Fatalf("InfoHashV2 = %s, want %s", meta.InfoHashV2, sha256Hex(info))
	}
	wantFiles := []FileEntry{{Path: "Movie.hybrid.mkv", Size: 5000, StartOffset: 0, EndOffset: 5000}}
	if len(meta.Files) != 1 || meta.Files[0] != wantFiles[0] {
		t.Fatalf("Files = %+v, want %+v", meta.Files, wantFiles)
	}
	wantRoot := hex.EncodeToString(piecesRoot)
	if got := meta.V2MerkleRoots["Movie.hybrid.mkv"]; got != wantRoot {
		t.Fatalf("V2MerkleRoots[%q] = %s, want %s", "Movie.hybrid.mkv", got, wantRoot)
	}
}

func TestParsePureV2FileTreeOnly(t *testing.T) {
	// A pure v2 torrent (no top-level "files"/"length", no v1 "pieces") --
	// file listing must come entirely from the file tree walk.
	// Per BEP52 the file tree's own key IS the file's path (self-contained,
	// no re-prefixing with info.name), so a realistic single-file v2
	// torrent has name == the file tree's sole leaf key.
	piecesRoot := bytes.Repeat([]byte{0x42}, 32)
	leaf := bDict(bStr("") + bDict(bStr("length")+bInt(777), bStr("pieces root")+bBytes(piecesRoot)))
	fileTree := bDict(bStr("only.mkv") + leaf)
	info := bDict(
		bStr("file tree")+fileTree,
		bStr("meta version")+bInt(2),
		bStr("name")+bStr("only.mkv"),
		bStr("piece length")+bInt(16384),
	)
	torrent := bDict(bStr("info") + info)

	meta, err := Parse([]byte(torrent))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if meta.InfoHashV1 != "" {
		t.Fatalf("InfoHashV1 = %q, want empty for a pure v2 torrent", meta.InfoHashV1)
	}
	if meta.InfoHashV2 == "" {
		t.Fatal("InfoHashV2 is empty, want a computed hash")
	}
	want := []FileEntry{{Path: "only.mkv", Size: 777, StartOffset: 0, EndOffset: 777}}
	if len(meta.Files) != 1 || meta.Files[0] != want[0] {
		t.Fatalf("Files = %+v, want %+v", meta.Files, want)
	}
}

func TestParseV2MultiFileNestedDirectoryPreMapsExtents(t *testing.T) {
	// A v2-only multi-file torrent with a nested directory -- the file
	// tree's own structure supplies the directory, matching the same
	// season-pack extent pre-mapping guarantee as the v1 multi-file case.
	leaf1 := bDict(bStr("") + bDict(bStr("length")+bInt(1500), bStr("pieces root")+bBytes(bytes.Repeat([]byte{0x01}, 32))))
	leaf2 := bDict(bStr("") + bDict(bStr("length")+bInt(2500), bStr("pieces root")+bBytes(bytes.Repeat([]byte{0x02}, 32))))
	season := bDict(bStr("S01E01.mkv")+leaf1, bStr("S01E02.mkv")+leaf2)
	fileTree := bDict(bStr("Show.S01.Pack") + season)
	info := bDict(
		bStr("file tree")+fileTree,
		bStr("meta version")+bInt(2),
		bStr("name")+bStr("Show.S01.Pack"),
		bStr("piece length")+bInt(16384),
	)
	torrent := bDict(bStr("info") + info)

	meta, err := Parse([]byte(torrent))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []FileEntry{
		{Path: "Show.S01.Pack/S01E01.mkv", Size: 1500, StartOffset: 0, EndOffset: 1500},
		{Path: "Show.S01.Pack/S01E02.mkv", Size: 2500, StartOffset: 1500, EndOffset: 4000},
	}
	if len(meta.Files) != len(want) {
		t.Fatalf("Files = %+v, want %+v", meta.Files, want)
	}
	for i := range want {
		if meta.Files[i] != want[i] {
			t.Fatalf("Files[%d] = %+v, want %+v", i, meta.Files[i], want[i])
		}
	}
	if got := meta.V2MerkleRoots["Show.S01.Pack/S01E01.mkv"]; got != hex.EncodeToString(bytes.Repeat([]byte{0x01}, 32)) {
		t.Fatalf("V2MerkleRoots[S01E01] = %s, wrong root", got)
	}
	if got := meta.V2MerkleRoots["Show.S01.Pack/S01E02.mkv"]; got != hex.EncodeToString(bytes.Repeat([]byte{0x02}, 32)) {
		t.Fatalf("V2MerkleRoots[S01E02] = %s, wrong root", got)
	}
}

func TestParseRejectsPathTraversal(t *testing.T) {
	evil := bDict(bStr("length")+bInt(10), bStr("path")+bList(bStr(".."), bStr("evil.mkv")))
	info := bDict(
		bStr("files")+bList(evil),
		bStr("name")+bStr("Pack"),
		bStr("piece length")+bInt(16384),
		bStr("pieces")+bBytes(bytes.Repeat([]byte{0x01}, 20)),
	)
	torrent := bDict(bStr("info") + info)

	if _, err := Parse([]byte(torrent)); err == nil {
		t.Fatal("Parse succeeded on a path-traversal file entry, want error")
	}
}

func TestParseRejectsMalformedPieces(t *testing.T) {
	info := bDict(
		bStr("length")+bInt(10),
		bStr("name")+bStr("f.mkv"),
		bStr("piece length")+bInt(16384),
		bStr("pieces")+bBytes(bytes.Repeat([]byte{0x01}, 25)), // not a multiple of 20
	)
	torrent := bDict(bStr("info") + info)

	if _, err := Parse([]byte(torrent)); err == nil {
		t.Fatal("Parse succeeded on malformed pieces field, want error")
	}
}

func TestParseRejectsNoHashMaterial(t *testing.T) {
	// v1-shaped file list but no "pieces" and no v2 file tree/meta version:
	// nothing to key an infohash on at all.
	info := bDict(
		bStr("length")+bInt(10),
		bStr("name")+bStr("f.mkv"),
		bStr("piece length")+bInt(16384),
	)
	torrent := bDict(bStr("info") + info)

	if _, err := Parse([]byte(torrent)); err == nil {
		t.Fatal("Parse succeeded with no v1/v2 hash material, want error")
	}
}

func TestParseRejectsEmptyAndOversizedInput(t *testing.T) {
	if _, err := Parse(nil); err == nil {
		t.Fatal("Parse succeeded on empty input, want error")
	}
	oversized := make([]byte, maxTorrentBytes+1)
	if _, err := Parse(oversized); err == nil {
		t.Fatal("Parse succeeded on oversized input, want error")
	}
}

func TestParseNeverPanicsOnGarbage(t *testing.T) {
	inputs := [][]byte{
		[]byte("not bencode at all"),
		[]byte("d"),
		[]byte("l"),
		[]byte("i"),
		[]byte("9999999999999999999999:x"),
		[]byte("d4:infoe"),
		[]byte("d4:infoli1ee e"),
		bytes.Repeat([]byte("l"), 1000), // deep nesting attempt
	}
	for _, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Parse panicked on %q: %v", in, r)
				}
			}()
			_, _ = Parse(in)
		}()
	}
}

// FuzzParse ships the required DG-03 fuzz target for this new parser: never
// panic, hang, or otherwise misbehave on arbitrary/malicious input.
func FuzzParse(f *testing.F) {
	pieceHash := bytes.Repeat([]byte{0xAB}, 20)
	singleFileInfo := bDict(
		bStr("length")+bInt(123456),
		bStr("name")+bStr("Movie.2024.1080p.mkv"),
		bStr("piece length")+bInt(16384),
		bStr("pieces")+bBytes(pieceHash),
	)
	f.Add([]byte(bDict(bStr("info") + singleFileInfo)))

	file1 := bDict(bStr("length")+bInt(1000), bStr("path")+bList(bStr("S01E01.mkv")))
	file2 := bDict(bStr("length")+bInt(2000), bStr("path")+bList(bStr("S01E02.mkv")))
	multiInfo := bDict(
		bStr("files")+bList(file1, file2),
		bStr("name")+bStr("Show.S01.Pack"),
		bStr("piece length")+bInt(16384),
		bStr("pieces")+bBytes(bytes.Repeat([]byte{0xCD}, 40)),
	)
	f.Add([]byte(bDict(bStr("info") + multiInfo)))

	piecesRoot := bytes.Repeat([]byte{0xEF}, 32)
	fileLeaf := bDict(bStr("") + bDict(bStr("length")+bInt(5000), bStr("pieces root")+bBytes(piecesRoot)))
	fileTree := bDict(bStr("Movie.hybrid.mkv") + fileLeaf)
	hybridInfo := bDict(
		bStr("file tree")+fileTree,
		bStr("length")+bInt(5000),
		bStr("meta version")+bInt(2),
		bStr("name")+bStr("Movie.hybrid.mkv"),
		bStr("piece length")+bInt(16384),
		bStr("pieces")+bBytes(bytes.Repeat([]byte{0x11}, 20)),
	)
	f.Add([]byte(bDict(bStr("info") + hybridInfo)))

	f.Add([]byte("not bencode at all"))
	f.Add([]byte("d4:infod4:name3:abee"))
	f.Add([]byte(""))
	f.Add(bytes.Repeat([]byte("l"), 200))

	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Parse panicked on input %q: %v", data, r)
			}
		}()
		_, _ = Parse(data) // an error is fine; a panic or hang is not
	})
}
