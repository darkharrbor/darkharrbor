package torrentmeta

import (
	"bytes"
	"crypto/sha1" // #nosec G505 -- building v1 known-answer piece hashes for the test fixture, mirroring the package's own definition.
	"encoding/hex"
	"testing"
)

// ---- known-answer v2 piece-hash vectors, computed independently in Python
// (hashlib.sha256, BEP52 16KiB-leaf merkle-tree-of-blocks algorithm) and
// cross-checked against the "Note that the last piece is padded... The last
// 16kiB block is not padded, but the piece is padded with padding blocks"
// wording of BEP52 and the libtorrent maintainer's clarification of it. ----

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex fixture: %v", err)
	}
	return b
}

func TestComputeV2PieceHashKnownAnswers(t *testing.T) {
	cases := []struct {
		name        string
		data        []byte
		pieceLength int64
		want        string
	}{
		{
			name:        "full two-block piece of 'A'",
			data:        bytes.Repeat([]byte("A"), 32768),
			pieceLength: 32768,
			want:        "2ee03c283c20cb98581b30c7a79990fb44241f8a79e2d36eff85409d64534f01",
		},
		{
			name:        "partial last piece, piece length 65536, 20000 real bytes",
			data:        partialBytes(20000),
			pieceLength: 65536,
			want:        "d630f462f9bc5b2493cdbd9cb9ea01064ee59be5dbed23ccd7454e7c934c3e30",
		},
		{
			name:        "single full block, piece length 16384",
			data:        bytes.Repeat([]byte("B"), 16384),
			pieceLength: 16384,
			want:        "db03474b1b90657f9fe742b4eed775e8b9000196bf262d1bd8521f8f7f3edd3f",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := computeV2PieceHash(c.data, c.pieceLength)
			want := mustHex(t, c.want)
			if !bytes.Equal(got[:], want) {
				t.Fatalf("computeV2PieceHash(%s) = %x, want %s", c.name, got, c.want)
			}
		})
	}
}

func TestComputeV2PieceHashDetectsCorruption(t *testing.T) {
	original := bytes.Repeat([]byte("A"), 32768)
	corrupt := append([]byte(nil), original...)
	corrupt[0] ^= 0xFF
	h1 := computeV2PieceHash(original, 32768)
	h2 := computeV2PieceHash(corrupt, 32768)
	if bytes.Equal(h1[:], h2[:]) {
		t.Fatal("corrupted piece produced the same hash as the original -- verification would never catch this")
	}
	wantCorrupt := mustHex(t, "ef51733aae094861407cbfd89ffc7e4c027946c50919294555e5b7f7c13f3db1")
	if !bytes.Equal(h2[:], wantCorrupt) {
		t.Fatalf("corrupt piece hash = %x, want %s (independent Python reference)", h2, "ef51733aae094861407cbfd89ffc7e4c027946c50919294555e5b7f7c13f3db1")
	}
}

func partialBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

// ---- VerifyWindow: v1 ----

func TestVerifyWindowV1MatchesAlignedWindow(t *testing.T) {
	pieceLength := int64(16384)
	piece0 := bytes.Repeat([]byte("x"), int(pieceLength))
	piece1 := bytes.Repeat([]byte("y"), int(pieceLength))
	h0 := sha1.Sum(piece0)
	h1 := sha1.Sum(piece1)
	meta := &TorrentMeta{
		PieceLength:   pieceLength,
		PieceHashesV1: append(append([]byte{}, h0[:]...), h1[:]...),
	}
	file := &FileEntry{Path: "movie.mkv", Size: pieceLength * 2, StartOffset: 0, EndOffset: pieceLength * 2}
	window := append(append([]byte{}, piece0...), piece1...)

	checked, ok, _ := VerifyWindow(meta, file, 0, window)
	if checked != 2 || !ok {
		t.Fatalf("VerifyWindow = checked=%d ok=%v, want checked=2 ok=true", checked, ok)
	}
}

func TestVerifyWindowV1DetectsCorruption(t *testing.T) {
	pieceLength := int64(16384)
	piece0 := bytes.Repeat([]byte("x"), int(pieceLength))
	h0 := sha1.Sum(piece0)
	meta := &TorrentMeta{PieceLength: pieceLength, PieceHashesV1: h0[:]}
	file := &FileEntry{Path: "movie.mkv", Size: pieceLength, StartOffset: 0, EndOffset: pieceLength}

	corrupt := append([]byte(nil), piece0...)
	corrupt[100] ^= 0xFF

	checked, ok, badIndex := VerifyWindow(meta, file, 0, corrupt)
	if checked != 1 || ok {
		t.Fatalf("VerifyWindow(corrupt) = checked=%d ok=%v, want checked=1 ok=false", checked, ok)
	}
	if badIndex != 0 {
		t.Fatalf("badIndex = %d, want 0", badIndex)
	}
}

func TestVerifyWindowV1UsesFileStartOffsetForGlobalPieceIndex(t *testing.T) {
	// Two-file v1 torrent sharing one piece stream: file B starts mid-way
	// through the piece stream at a piece-aligned offset. A window read from
	// file B's own byte 0 must verify against the GLOBAL piece index
	// (file.StartOffset/pieceLength + 0), not piece index 0.
	pieceLength := int64(16384)
	pieceA := bytes.Repeat([]byte("a"), int(pieceLength)) // global piece 0 (file A)
	pieceB := bytes.Repeat([]byte("b"), int(pieceLength)) // global piece 1 (file B, starts here)
	hA := sha1.Sum(pieceA)
	hB := sha1.Sum(pieceB)
	meta := &TorrentMeta{
		PieceLength:   pieceLength,
		PieceHashesV1: append(append([]byte{}, hA[:]...), hB[:]...),
	}
	fileB := &FileEntry{Path: "b.mkv", Size: pieceLength, StartOffset: pieceLength, EndOffset: pieceLength * 2}

	checked, ok, _ := VerifyWindow(meta, fileB, 0, pieceB)
	if checked != 1 || !ok {
		t.Fatalf("VerifyWindow(correct pieceB) = checked=%d ok=%v, want checked=1 ok=true", checked, ok)
	}

	// Sanity: verifying file B's bytes against what would be piece-index-0's
	// hash (i.e. mislabeling) must fail -- proves StartOffset really is used.
	checked2, ok2, _ := VerifyWindow(meta, &FileEntry{Path: "b.mkv", Size: pieceLength, StartOffset: 0, EndOffset: pieceLength}, 0, pieceB)
	if checked2 != 1 || ok2 {
		t.Fatal("verifying file B's bytes at StartOffset=0 unexpectedly matched piece 0's hash -- fixture collision, strengthen the test")
	}
}

func TestVerifyWindowV1AbstainsWhenFileWindowIsNotTorrentAligned(t *testing.T) {
	pieceLength := int64(16384)
	prefix := bytes.Repeat([]byte("p"), 140)
	video := bytes.Repeat([]byte("v"), int(2*pieceLength))
	whole := append(append([]byte(nil), prefix...), video...)
	hashes := make([]byte, 0, 3*sha1.Size)
	for offset := int64(0); offset < int64(len(whole)); offset += pieceLength {
		end := offset + pieceLength
		if end > int64(len(whole)) {
			end = int64(len(whole))
		}
		sum := sha1.Sum(whole[offset:end])
		hashes = append(hashes, sum[:]...)
	}
	meta := &TorrentMeta{PieceLength: pieceLength, PieceHashesV1: hashes}
	file := &FileEntry{Path: "video.mp4", Size: int64(len(video)), StartOffset: int64(len(prefix)), EndOffset: int64(len(whole))}

	checked, ok, _ := VerifyWindow(meta, file, 0, video[:pieceLength])
	if checked != 0 || !ok {
		t.Fatalf("torrent-unaligned file window: checked=%d ok=%v, want safe abstention", checked, ok)
	}
}

func TestVerifyWindowAbstainsOnUnalignedWindow(t *testing.T) {
	pieceLength := int64(16384)
	piece0 := bytes.Repeat([]byte("x"), int(pieceLength))
	h0 := sha1.Sum(piece0)
	meta := &TorrentMeta{PieceLength: pieceLength, PieceHashesV1: h0[:]}
	file := &FileEntry{Path: "movie.mkv", Size: pieceLength, StartOffset: 0, EndOffset: pieceLength}

	// Unaligned start offset.
	checked, ok, _ := VerifyWindow(meta, file, 100, piece0[100:])
	if checked != 0 || !ok {
		t.Fatalf("unaligned-offset window: checked=%d ok=%v, want checked=0 ok=true (abstain)", checked, ok)
	}

	// Aligned start, but window length not a multiple of piece length.
	checked, ok, _ = VerifyWindow(meta, file, 0, piece0[:100])
	if checked != 0 || !ok {
		t.Fatalf("unaligned-length window: checked=%d ok=%v, want checked=0 ok=true (abstain)", checked, ok)
	}
}

func TestVerifyWindowAbstainsWithNoHashMaterial(t *testing.T) {
	meta := &TorrentMeta{PieceLength: 16384}
	file := &FileEntry{Path: "movie.mkv", Size: 16384, StartOffset: 0, EndOffset: 16384}
	data := bytes.Repeat([]byte("z"), 16384)

	checked, ok, _ := VerifyWindow(meta, file, 0, data)
	if checked != 0 || !ok {
		t.Fatalf("no-hash-material window: checked=%d ok=%v, want checked=0 ok=true (abstain)", checked, ok)
	}
	if checked, ok, _ := VerifyWindow(nil, file, 0, data); checked != 0 || !ok {
		t.Fatal("nil meta must abstain safely, never panic or fail")
	}
	if checked, ok, _ := VerifyWindow(meta, nil, 0, data); checked != 0 || !ok {
		t.Fatal("nil file must abstain safely, never panic or fail")
	}
}

// ---- VerifyWindow: v2 ----

func TestVerifyWindowV2MatchesAlignedTwoPieceFile(t *testing.T) {
	pieceLength := int64(32768)
	piece0 := bytes.Repeat([]byte("A"), int(pieceLength)) // full piece
	piece1 := bytes.Repeat([]byte("C"), 10000)            // short final piece of the file

	h0 := computeV2PieceHash(piece0, pieceLength)
	h1 := computeV2PieceHash(piece1, pieceLength)
	root := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef" // opaque test root id
	meta := &TorrentMeta{
		PieceLength:   pieceLength,
		V2MerkleRoots: map[string]string{"movie.mkv": root},
		PieceLayers:   map[string][]byte{root: append(append([]byte{}, h0[:]...), h1[:]...)},
	}
	file := &FileEntry{Path: "movie.mkv", Size: pieceLength + 10000, StartOffset: 0, EndOffset: pieceLength + 10000}

	// Only piece0 forms an aligned, full-pieceLength window (piece1 is short
	// and, per VerifyWindow's strict-alignment abstention, is never checked
	// this way -- that's an intentional, disclosed limitation of the
	// opportunistic design, exercised directly below via a hash-level test).
	checked, ok, _ := VerifyWindow(meta, file, 0, piece0)
	if checked != 1 || !ok {
		t.Fatalf("VerifyWindow(piece0) = checked=%d ok=%v, want checked=1 ok=true", checked, ok)
	}
}

func TestVerifyWindowV2DetectsCorruption(t *testing.T) {
	pieceLength := int64(32768)
	piece0 := bytes.Repeat([]byte("A"), int(pieceLength))
	h0 := computeV2PieceHash(piece0, pieceLength)
	root := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	meta := &TorrentMeta{
		PieceLength:   pieceLength,
		V2MerkleRoots: map[string]string{"movie.mkv": root},
		PieceLayers:   map[string][]byte{root: h0[:]},
	}
	file := &FileEntry{Path: "movie.mkv", Size: pieceLength, StartOffset: 0, EndOffset: pieceLength}

	corrupt := append([]byte(nil), piece0...)
	corrupt[5] ^= 0xFF
	checked, ok, badIndex := VerifyWindow(meta, file, 0, corrupt)
	if checked != 1 || ok {
		t.Fatalf("VerifyWindow(corrupt) = checked=%d ok=%v, want checked=1 ok=false", checked, ok)
	}
	if badIndex != 0 {
		t.Fatalf("badIndex = %d, want 0", badIndex)
	}
}

func TestVerifyWindowV2AbstainsWithoutRootOrLayer(t *testing.T) {
	pieceLength := int64(32768)
	data := bytes.Repeat([]byte("A"), int(pieceLength))

	// No V2MerkleRoots entry for this file at all.
	meta1 := &TorrentMeta{PieceLength: pieceLength}
	file := &FileEntry{Path: "movie.mkv", Size: pieceLength, StartOffset: 0, EndOffset: pieceLength}
	if checked, ok, _ := VerifyWindow(meta1, file, 0, data); checked != 0 || !ok {
		t.Fatalf("no-root: checked=%d ok=%v, want abstain", checked, ok)
	}

	// Root present but no matching piece-layers entry (e.g. file <= one
	// piece long, BEP52's own "only for files larger than piece size" rule).
	root := "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe"
	meta2 := &TorrentMeta{PieceLength: pieceLength, V2MerkleRoots: map[string]string{"movie.mkv": root}}
	if checked, ok, _ := VerifyWindow(meta2, file, 0, data); checked != 0 || !ok {
		t.Fatalf("no-layer: checked=%d ok=%v, want abstain", checked, ok)
	}
}

// ---- Parse() round-trip: "piece layers" wiring ----

func TestParsePieceLayersRoundTrip(t *testing.T) {
	pieceLength := int64(32768)
	piece0 := bytes.Repeat([]byte("A"), int(pieceLength))
	piece1 := bytes.Repeat([]byte("C"), 10000)
	h0 := computeV2PieceHash(piece0, pieceLength)
	h1 := computeV2PieceHash(piece1, pieceLength)
	rootBytes := bytes.Repeat([]byte{0x77}, 32)
	rootHex := hex.EncodeToString(rootBytes)
	layerConcat := append(append([]byte{}, h0[:]...), h1[:]...)

	fileLeaf := bDict(bStr("") + bDict(bStr("length")+bInt(pieceLength+10000), bStr("pieces root")+bBytes(rootBytes)))
	fileTree := bDict(bStr("movie.mkv") + fileLeaf)
	pieceLayers := bDict(bBytes(rootBytes) + bBytes(layerConcat))
	info := bDict(
		bStr("file tree")+fileTree,
		bStr("meta version")+bInt(2),
		bStr("name")+bStr("movie.mkv"),
		bStr("piece layers")+pieceLayers,
		bStr("piece length")+bInt(pieceLength),
	)
	torrent := bDict(bStr("info") + info)

	meta, err := Parse([]byte(torrent))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if meta.V2MerkleRoots["movie.mkv"] != rootHex {
		t.Fatalf("V2MerkleRoots[movie.mkv] = %s, want %s", meta.V2MerkleRoots["movie.mkv"], rootHex)
	}
	got, ok := meta.PieceLayers[rootHex]
	if !ok {
		t.Fatalf("PieceLayers[%s] missing, want present", rootHex)
	}
	if !bytes.Equal(got, layerConcat) {
		t.Fatalf("PieceLayers[%s] = %x, want %x", rootHex, got, layerConcat)
	}

	// And VerifyWindow actually closes the loop end-to-end through the
	// parsed structure, not just the hand-built fixture above.
	file := meta.Files[0]
	checked, okVerify, _ := VerifyWindow(meta, &file, 0, piece0)
	if checked != 1 || !okVerify {
		t.Fatalf("VerifyWindow via parsed meta = checked=%d ok=%v, want checked=1 ok=true", checked, okVerify)
	}
}

func TestParsePieceLayersRejectsMalformed(t *testing.T) {
	rootBytes := bytes.Repeat([]byte{0x77}, 32)
	fileLeaf := bDict(bStr("") + bDict(bStr("length")+bInt(70000), bStr("pieces root")+bBytes(rootBytes)))
	fileTree := bDict(bStr("movie.mkv") + fileLeaf)
	baseInfo := func(pieceLayers string) string {
		return bDict(
			bStr("file tree")+fileTree,
			bStr("meta version")+bInt(2),
			bStr("name")+bStr("movie.mkv"),
			bStr("piece layers")+pieceLayers,
			bStr("piece length")+bInt(32768),
		)
	}

	// Value not a multiple of 32 bytes.
	badValue := bDict(bBytes(rootBytes) + bBytes([]byte("not-32-bytes-or-a-multiple")))
	if _, err := Parse([]byte(bDict(bStr("info") + baseInfo(badValue)))); err == nil {
		t.Fatal("expected error for piece layers value not a multiple of 32 bytes")
	}

	// Value present but empty.
	badEmpty := bDict(bBytes(rootBytes) + bBytes(nil))
	if _, err := Parse([]byte(bDict(bStr("info") + baseInfo(badEmpty)))); err == nil {
		t.Fatal("expected error for empty piece layers value")
	}

	// Key not 32 bytes.
	badKey := bDict(bBytes([]byte("short")) + bBytes(bytes.Repeat([]byte{0x01}, 32)))
	if _, err := Parse([]byte(bDict(bStr("info") + baseInfo(badKey)))); err == nil {
		t.Fatal("expected error for piece layers key not 32 bytes")
	}

	// piece layers is not a dict at all.
	if _, err := Parse([]byte(bDict(bStr("info") + baseInfoWithBadPieceLayers(fileTree)))); err == nil {
		t.Fatal("expected error for piece layers not a dict")
	}
}

func baseInfoWithBadPieceLayers(fileTree string) string {
	return bDict(
		bStr("file tree")+fileTree,
		bStr("meta version")+bInt(2),
		bStr("name")+bStr("movie.mkv"),
		bStr("piece layers")+bInt(5), // wrong type: an int, not a dict
		bStr("piece length")+bInt(32768),
	)
}
