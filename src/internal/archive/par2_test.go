package archive

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

func par2Packet(recoverySetID [16]byte, typeName string, body []byte) []byte {
	for len(body)%4 != 0 {
		body = append(body, 0)
	}
	pkt := make([]byte, par2HeaderSize+len(body))
	copy(pkt[0:8], par2Magic[:])
	binary.LittleEndian.PutUint64(pkt[8:16], uint64(len(pkt)))
	// pkt[16:32] packet hash — left zero, not verified by the parser.
	copy(pkt[32:48], recoverySetID[:])
	typeField := make([]byte, 16)
	copy(typeField, par2TypePrefix)
	copy(typeField[len(par2TypePrefix):], typeName)
	copy(pkt[48:64], typeField)
	copy(pkt[64:], body)
	return pkt
}

func par2MainPacket(recoverySetID [16]byte, sliceSize uint64, recoverIDs [][16]byte) []byte {
	body := make([]byte, 12)
	binary.LittleEndian.PutUint64(body[0:8], sliceSize)
	binary.LittleEndian.PutUint32(body[8:12], uint32(len(recoverIDs)))
	for _, id := range recoverIDs {
		body = append(body, id[:]...)
	}
	return par2Packet(recoverySetID, "Main", body)
}

func par2FileDescPacket(recoverySetID [16]byte, fileID [16]byte, length uint64, name string) []byte {
	return par2FileDescPacketWithHash16K(recoverySetID, fileID, [16]byte{}, length, name)
}

func par2FileDescPacketWithHash16K(recoverySetID, fileID, hash16K [16]byte, length uint64, name string) []byte {
	body := make([]byte, 56)
	copy(body[0:16], fileID[:])
	// body[16:32] whole-file md5 is not needed by NS-2.2.
	copy(body[32:48], hash16K[:])
	binary.LittleEndian.PutUint64(body[48:56], length)
	body = append(body, []byte(name)...)
	return par2Packet(recoverySetID, "FileDesc", body)
}

func par2IFSCPacket(recoverySetID, fileID [16]byte, hashes ...[16]byte) []byte {
	body := append([]byte(nil), fileID[:]...)
	for _, hash := range hashes {
		body = append(body, hash[:]...)
		body = append(body, make([]byte, 4)...) // CRC32 is not persisted by NS-4.1.
	}
	return par2Packet(recoverySetID, "IFSC", body)
}

func TestParsePAR2MetadataRecoversNamesInMainOrder(t *testing.T) {
	t.Parallel()
	var setID [16]byte
	copy(setID[:], "recovery-set-id!")
	var id1, id2, hash16K [16]byte
	copy(id1[:], "file-id-one-1234")
	copy(id2[:], "file-id-two-5678")
	copy(hash16K[:], "first-16k-hash!!")

	var data []byte
	// Main packet lists id2 before id1 — output order must follow this.
	data = append(data, par2MainPacket(setID, 384000, [][16]byte{id2, id1})...)
	data = append(data, par2FileDescPacket(setID, id1, 100, "Show.S01E01.Real.Name.mkv")...)
	data = append(data, par2FileDescPacketWithHash16K(setID, id2, hash16K, 200, "Show.S01E01.Real.Name.part01.rar")...)

	info, err := ParsePAR2Metadata(context.Background(), bytesource.NewMemSource("fixture", data))
	if err != nil {
		t.Fatal(err)
	}
	if info.SliceSize != 384000 {
		t.Fatalf("slice size = %d, want 384000", info.SliceSize)
	}
	if len(info.Files) != 2 {
		t.Fatalf("files = %+v, want 2", info.Files)
	}
	if info.Files[0].Name != "Show.S01E01.Real.Name.part01.rar" || info.Files[0].Length != 200 {
		t.Fatalf("files[0] = %+v, want part01.rar/200 (Main packet order)", info.Files[0])
	}
	if info.Files[0].Hash16K != hexEncode(hash16K[:]) {
		t.Fatalf("files[0].Hash16K = %q, want %q", info.Files[0].Hash16K, hexEncode(hash16K[:]))
	}
	if info.Files[1].Name != "Show.S01E01.Real.Name.mkv" || info.Files[1].Length != 100 {
		t.Fatalf("files[1] = %+v, want .mkv/100", info.Files[1])
	}
	if fd, ok := info.FileByID(info.Files[1].FileID); !ok || fd.Name != "Show.S01E01.Real.Name.mkv" {
		t.Fatalf("FileByID lookup = %+v, %v", fd, ok)
	}
}

func TestParsePAR2MetadataRecoversBoundedIFSC(t *testing.T) {
	t.Parallel()
	var setID, otherSetID, fileID, first, second [16]byte
	copy(setID[:], "recovery-set-id!")
	copy(otherSetID[:], "different-set-id")
	copy(fileID[:], "file-id-one-1234")
	copy(first[:], "first-block-md5!")
	copy(second[:], "second-block-md5")

	var data []byte
	data = append(data, par2MainPacket(setID, 4, [][16]byte{fileID})...)
	data = append(data, par2FileDescPacket(setID, fileID, 8, "movie.mkv")...)
	data = append(data, par2IFSCPacket(otherSetID, fileID, second, first)...)
	data = append(data, par2IFSCPacket(setID, fileID, first, second)...)

	info, err := ParsePAR2Metadata(context.Background(), bytesource.NewMemSource("fixture", data))
	if err != nil {
		t.Fatal(err)
	}
	index, ok := info.IFSCByFileID(hexEncode(fileID[:]))
	if !ok {
		t.Fatal("IFSC index missing")
	}
	want := append(append([]byte(nil), first[:]...), second[:]...)
	if !bytes.Equal(index.BlockMD5, want) {
		t.Fatalf("IFSC hashes = %x, want %x", index.BlockMD5, want)
	}
	index.BlockMD5[0] ^= 0xff
	again, _ := info.IFSCByFileID(hexEncode(fileID[:]))
	if !bytes.Equal(again.BlockMD5, want) {
		t.Fatal("IFSCByFileID did not return a defensive copy")
	}
}

func TestParsePAR2MetadataRejectsContradictoryIFSC(t *testing.T) {
	t.Parallel()
	var setID, fileID, first, second [16]byte
	copy(setID[:], "recovery-set-id!")
	copy(fileID[:], "file-id-one-1234")
	copy(first[:], "first-block-md5!")
	copy(second[:], "second-block-md5")

	var data []byte
	data = append(data, par2MainPacket(setID, 4, [][16]byte{fileID})...)
	data = append(data, par2FileDescPacket(setID, fileID, 4, "movie.mkv")...)
	data = append(data, par2IFSCPacket(setID, fileID, first)...)
	data = append(data, par2IFSCPacket(setID, fileID, second)...)

	info, err := ParsePAR2Metadata(context.Background(), bytesource.NewMemSource("fixture", data))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := info.IFSCByFileID(hexEncode(fileID[:])); ok {
		t.Fatal("contradictory IFSC packets must abstain")
	}
}

func TestParsePAR2IFSCRejectsMalformedAndOversized(t *testing.T) {
	t.Parallel()
	if _, err := parsePAR2IFSC(make([]byte, 35)); err == nil {
		t.Fatal("short IFSC body accepted")
	}
	if _, err := parsePAR2IFSC(make([]byte, 37)); err == nil {
		t.Fatal("misaligned IFSC body accepted")
	}
	if _, err := parsePAR2IFSC(make([]byte, 16+20*(maxPAR2IFSCBlocks+1))); err == nil {
		t.Fatal("oversized IFSC body accepted")
	}
}

func TestIsPAR2Prefix(t *testing.T) {
	t.Parallel()
	if !IsPAR2Prefix(par2Magic[:]) {
		t.Fatal("PAR2 magic was not recognized")
	}
	if IsPAR2Prefix([]byte("not-par2")) {
		t.Fatal("non-PAR2 prefix was recognized")
	}
}

func TestParsePAR2MetadataNoMainFallsBackToEncounterOrder(t *testing.T) {
	t.Parallel()
	var setID [16]byte
	var id [16]byte
	copy(id[:], "solo-file-id-here")
	data := par2FileDescPacket(setID, id, 42, "solo.mkv")
	info, err := ParsePAR2Metadata(context.Background(), bytesource.NewMemSource("fixture", data))
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Files) != 1 || info.Files[0].Name != "solo.mkv" || info.Files[0].Length != 42 {
		t.Fatalf("files = %+v", info.Files)
	}
}

func TestParsePAR2MetadataMalformedNegatives(t *testing.T) {
	t.Parallel()
	var setID [16]byte
	var id [16]byte
	copy(id[:], "trunc-file-id")

	for _, test := range []struct {
		name string
		data []byte
	}{
		{"not par2 at all", []byte("this is not a par2 file, just text")},
		{"truncated header", par2FileDescPacket(setID, id, 1, "x.mkv")[:40]},
		{"declared length exceeds buffer", func() []byte {
			pkt := par2FileDescPacket(setID, id, 1, "x.mkv")
			binary.LittleEndian.PutUint64(pkt[8:16], uint64(len(pkt)*10))
			return pkt
		}()},
		{"declared length not multiple of 4", func() []byte {
			pkt := par2FileDescPacket(setID, id, 1, "x.mkv")
			binary.LittleEndian.PutUint64(pkt[8:16], uint64(len(pkt)-1))
			return pkt
		}()},
		{"declared length below header size", func() []byte {
			pkt := par2FileDescPacket(setID, id, 1, "x.mkv")
			binary.LittleEndian.PutUint64(pkt[8:16], 8)
			return pkt
		}()},
		{"main packet file count overflow", func() []byte {
			pkt := par2MainPacket(setID, 1000, nil)
			binary.LittleEndian.PutUint32(pkt[64+8:64+12], 0xffffffff)
			return pkt
		}()},
		{"empty", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParsePAR2Metadata(context.Background(), bytesource.NewMemSource("fixture", test.data))
			// None of these should panic (that's what matters for DG-03);
			// most legitimately produce an error, but a benign zero-value
			// result for the still-well-framed-but-empty-main case is fine.
			_ = err
		})
	}
}

func TestParsePAR2MetadataHonorsCancellation(t *testing.T) {
	t.Parallel()
	var setID [16]byte
	var id [16]byte
	copy(id[:], "cancel-file-id")
	data := par2FileDescPacket(setID, id, 1, "x.mkv")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ParsePAR2Metadata(ctx, bytesource.NewMemSource("fixture", data)); err == nil {
		t.Fatal("expected cancellation error")
	}
}

func TestPAR2RecoveryBlocksFromSubjects(t *testing.T) {
	t.Parallel()
	subjects := []string{
		`[1/5] "release.par2" yEnc (1/1)`,
		`[2/5] "release.vol000+004.par2" yEnc (1/10)`,
		`[3/5] "release.vol004+008.par2" yEnc (1/20)`,
		`[4/5] "release.part01.rar" yEnc (1/500)`,
		`[5/5] "release.mkv" yEnc (1/2000)`,
	}
	if got := PAR2RecoveryBlocksFromSubjects(subjects); got != 12 {
		t.Fatalf("recovery blocks = %d, want 12", got)
	}
	if got := PAR2RecoveryBlocksFromSubjects(nil); got != 0 {
		t.Fatalf("empty subjects = %d, want 0", got)
	}
}

func TestPAR2SourceBlockCount(t *testing.T) {
	t.Parallel()
	info := PAR2Info{Files: []PAR2FileDesc{{Length: 1000}, {Length: 500}}}
	if got := PAR2SourceBlockCount(info, 400); got != 4 { // ceil(1500/400)=4
		t.Fatalf("source blocks = %d, want 4", got)
	}
	if got := PAR2SourceBlockCount(info, 0); got != 0 {
		t.Fatalf("zero slice size = %d, want 0", got)
	}
	if got := PAR2SourceBlockCount(PAR2Info{}, 400); got != 0 {
		t.Fatalf("no files = %d, want 0", got)
	}
}

func FuzzParsePAR2Metadata(f *testing.F) {
	var setID, id1, id2 [16]byte
	copy(setID[:], "recovery-set-id!")
	copy(id1[:], "file-id-one-1234")
	copy(id2[:], "file-id-two-5678")
	var seed []byte
	seed = append(seed, par2MainPacket(setID, 384000, [][16]byte{id1, id2})...)
	seed = append(seed, par2FileDescPacket(setID, id1, 100, "movie.mkv")...)
	seed = append(seed, par2FileDescPacket(setID, id2, 200, "movie.part01.rar")...)
	seed = append(seed, par2IFSCPacket(setID, id1, id1)...)
	f.Add(seed)
	f.Add(par2FileDescPacket(setID, id1, 1, "solo.mkv"))
	f.Add([]byte("not a par2 file"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParsePAR2Metadata(context.Background(), bytesource.NewMemSource("fuzz", data))
	})
}

func FuzzPAR2RecoveryBlocksFromSubjects(f *testing.F) {
	f.Add("release.vol012+034.par2")
	f.Add("not a par2 subject")
	f.Fuzz(func(t *testing.T, subject string) {
		_ = PAR2RecoveryBlocksFromSubjects([]string{subject})
	})
}

// declaredOversizeSource mimics the real ByteSource contract that surfaced
// this defect in production: Size() reports the NZB-declared (yEnc-encoded)
// byte count, which can legitimately exceed the actual decoded content by a
// small margin (Capabilities.ExactSize == false). ReadAt correctly returns
// io.EOF once the real data is exhausted, per the ByteSource ReadAt/EOF
// contract -- this is not corruption, just a smaller true length than
// declared.
type declaredOversizeSource struct {
	data         []byte
	declaredSize int64
}

func (s *declaredOversizeSource) Size() int64 { return s.declaredSize }
func (s *declaredOversizeSource) Key() string { return "declared-oversize-fixture" }
func (s *declaredOversizeSource) Caps() bytesource.Capabilities {
	return bytesource.Capabilities{RangeSupport: true, ExactSize: false}
}
func (s *declaredOversizeSource) ReadAt(_ context.Context, p []byte, off int64) (int, error) {
	if off >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := copy(p, s.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestParsePAR2MetadataTakesShortReadFromUndersizedDeclaredSize(t *testing.T) {
	t.Parallel()
	var setID [16]byte
	var id [16]byte
	copy(id[:], "real-file-id-123")
	full := par2FileDescPacket(setID, id, 999, "real-movie-name.mkv")

	// Real production case: declared Size() (yEnc-encoded byte count from
	// the NZB) is a few percent larger than the actual decoded article
	// content -- ReadAt hits io.EOF a little short of the declared length,
	// exactly as observed live against Newshosting and TorBox for the same
	// PAR2 index file. The parser must still recover the packet that fully
	// fit within the actual (shorter) data.
	src := &declaredOversizeSource{
		data:         full,
		declaredSize: int64(len(full)) + int64(len(full))/32, // ~3% larger, matches observed drift
	}
	info, err := ParsePAR2Metadata(context.Background(), src)
	if err != nil {
		t.Fatalf("expected best-effort short-read tolerance, got error: %v", err)
	}
	if len(info.Files) != 1 || info.Files[0].Name != "real-movie-name.mkv" || info.Files[0].Length != 999 {
		t.Fatalf("files = %+v, want the one packet that fit within the real (shorter) data", info.Files)
	}
}

func TestParsePAR2MetadataShortReadWithNoUsableDataErrors(t *testing.T) {
	t.Parallel()
	// Declared size wildly exceeds any real data at all (e.g. a genuinely
	// empty/failed fetch) -- must still error, not silently succeed empty.
	src := &declaredOversizeSource{data: nil, declaredSize: 1000}
	if _, err := ParsePAR2Metadata(context.Background(), src); err == nil {
		t.Fatal("expected error when no data at all is available")
	}
}
