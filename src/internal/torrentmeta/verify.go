// verify.go — TS-3.4 (T12): opportunistic piece/merkle verification of
// already-fetched chunk-path bytes against the torrent's own harvested
// TorrentMeta (TS-0.1). This package performs no I/O and owns no cache,
// ladder, or scoring state of its own -- it is a pure function over bytes
// the caller already has in hand, matching every other verification/proof
// owner's boundary in this codebase (contentproof, par2, archive readers).
//
// Design (frozen T12 spec): "When cached chunk boundaries align with piece
// boundaries (v1) or per-file merkle leaves (v2), verify; mismatch = treat
// as fetch failure ... Never block streaming on unaligned reads;
// verification is opportunistic by design." VerifyWindow therefore ALWAYS
// returns a safe abstention (checked=0, ok=true) rather than an error when
// the window is not exactly piece-aligned or no hash material exists --
// abstention is never distinguishable from "nothing to check" by the
// caller, by design, so a caller can treat checked==0 as "proceed exactly
// as before this row" unconditionally.
package torrentmeta

import (
	"bytes"
	"context"
	"crypto/sha1" // #nosec G505 -- BitTorrent v1 piece hashes are defined as SHA-1; not used for any security property here.
	"crypto/sha256"
)

// v2BlockSize is BEP52's fixed merkle-tree leaf block size, independent of
// the torrent's own configured piece length.
const v2BlockSize = 16384

// zeroBlockHash is the fixed BEP52 padding leaf: the SHA-256 hash of one
// (imaginary) all-zero v2BlockSize block, used to pad a partial-piece
// subtree up to piece_length/v2BlockSize leaves (a power of two).
var zeroBlockHash = func() [32]byte {
	return sha256.Sum256(make([]byte, v2BlockSize))
}()

// VerifyWindow opportunistically checks every whole-piece verification
// opportunity contained in [windowFileOffset, windowFileOffset+len(data))
// of file, using whatever hash material meta carries. data must be exactly
// the bytes DarkHarrbor already fetched for that exact file-relative byte
// range (the chunk-path window).
//
// checked reports how many pieces were actually checked; ok is true only
// when every checked piece matched. When !ok, badIndex names the first
// mismatching piece (v1: global torrent-wide piece index; v2: file-relative
// piece index within file) for logging -- never any content, path, or byte
// value.
//
// checked==0 is always a safe abstention: an unaligned window, a nil/empty
// file or meta, or a torrent with no matching hash material for this file
// all abstain identically. A caller must never treat checked==0 as either
// success or failure -- only "no opinion", exactly as the frozen T12 design
// requires ("never block streaming on unaligned reads").
func VerifyWindow(meta *TorrentMeta, file *FileEntry, windowFileOffset int64, data []byte) (checked int, ok bool, badIndex int) {
	if meta == nil || file == nil || len(data) == 0 || meta.PieceLength <= 0 || windowFileOffset < 0 {
		return 0, true, 0
	}
	// Strict whole-window alignment per the frozen spec's own wording
	// ("cached chunk boundaries align with piece boundaries") -- not merely
	// "contains some aligned interior pieces". V1 alignment is torrent-global;
	// v2 alignment is file-relative because BEP52 hashes each file separately.
	pieceLength := meta.PieceLength
	if int64(len(data))%pieceLength != 0 {
		return 0, true, 0
	}

	if len(meta.PieceHashesV1) > 0 {
		// V1 pieces span the whole torrent, so alignment is torrent-global.
		// A later file commonly starts inside a piece; a file-relative cache
		// window at offset zero must abstain instead of hashing the wrong bytes
		// against the containing global piece.
		absolute := file.StartOffset + windowFileOffset
		if file.StartOffset >= 0 && absolute >= file.StartOffset && absolute%pieceLength == 0 {
			return verifyWindowV1(meta, file, windowFileOffset, data, pieceLength)
		}
	}
	if len(meta.V2MerkleRoots) > 0 && len(meta.PieceLayers) > 0 {
		if windowFileOffset%pieceLength != 0 {
			return 0, true, 0
		}
		return verifyWindowV2(meta, file, windowFileOffset, data, pieceLength)
	}
	return 0, true, 0
}

func verifyWindowV1(meta *TorrentMeta, file *FileEntry, windowFileOffset int64, data []byte, pieceLength int64) (checked int, ok bool, badIndex int) {
	numPieces := len(meta.PieceHashesV1) / 20
	absStart := file.StartOffset + windowFileOffset
	numWindowPieces := int64(len(data)) / pieceLength
	for i := int64(0); i < numWindowPieces; i++ {
		idx := int(absStart/pieceLength + i)
		if idx < 0 || idx >= numPieces {
			// Out of range: either the very last (short) piece of the whole
			// torrent, which strict alignment above cannot have produced a
			// full pieceLength slice for anyway, or a malformed/short pieces
			// field. Abstain on this piece rather than guess.
			continue
		}
		expected := meta.PieceHashesV1[idx*20 : idx*20+20]
		pieceData := data[i*pieceLength : (i+1)*pieceLength]
		sum := sha1.Sum(pieceData)
		checked++
		if !bytes.Equal(sum[:], expected) {
			return checked, false, idx
		}
	}
	return checked, true, 0
}

func verifyWindowV2(meta *TorrentMeta, file *FileEntry, windowFileOffset int64, data []byte, pieceLength int64) (checked int, ok bool, badIndex int) {
	rootHex, hasRoot := meta.V2MerkleRoots[file.Path]
	if !hasRoot {
		return 0, true, 0
	}
	layer, hasLayer := meta.PieceLayers[rootHex]
	if !hasLayer {
		// File is at most one piece long (BEP52 omits piece layers for such
		// files) -- no per-piece material to check against, opportunistic
		// abstention.
		return 0, true, 0
	}
	numPieces := len(layer) / 32
	numWindowPieces := int64(len(data)) / pieceLength
	for i := int64(0); i < numWindowPieces; i++ {
		idx := int(windowFileOffset/pieceLength + i)
		if idx < 0 || idx >= numPieces {
			continue
		}
		expected := layer[idx*32 : idx*32+32]
		pieceData := data[i*pieceLength : (i+1)*pieceLength]
		got := computeV2PieceHash(pieceData, pieceLength)
		checked++
		if !bytes.Equal(got[:], expected) {
			return checked, false, idx
		}
	}
	return checked, true, 0
}

// computeV2PieceHash computes the BEP52 piece-layer hash for one piece's
// raw bytes: SHA-256 leaves over v2BlockSize blocks (the true last block of
// the file may be shorter than v2BlockSize and is hashed as-is, never
// zero-padded itself), padded with zeroBlockHash up to pieceLength/
// v2BlockSize leaves (always a power of two, since BEP52 requires piece
// length to be a power of two multiple of v2BlockSize), then reduced
// pairwise (branching factor 2) to a single root. This is the exact
// algorithm BEP52 defines for both a full interior piece (all real leaves,
// already power-of-two count, no padding needed) and a short final piece of
// a file (some trailing leaves padded).
func computeV2PieceHash(pieceData []byte, pieceLength int64) [32]byte {
	root, _ := computeV2PieceHashContext(context.Background(), pieceData, pieceLength)
	return root
}

func computeV2PieceHashContext(ctx context.Context, pieceData []byte, pieceLength int64) ([32]byte, error) {
	leavesNeeded := int(pieceLength / v2BlockSize)
	if leavesNeeded < 1 {
		leavesNeeded = 1
	}
	leaves := make([][32]byte, leavesNeeded)
	off := 0
	li := 0
	for off < len(pieceData) && li < leavesNeeded {
		if err := ctx.Err(); err != nil {
			return [32]byte{}, err
		}
		end := off + v2BlockSize
		if end > len(pieceData) {
			end = len(pieceData)
		}
		leaves[li] = sha256.Sum256(pieceData[off:end])
		off = end
		li++
	}
	for ; li < leavesNeeded; li++ {
		leaves[li] = zeroBlockHash
	}
	return merkleRootContext(ctx, leaves)
}

func merkleRootContext(ctx context.Context, leaves [][32]byte) ([32]byte, error) {
	level := leaves
	for len(level) > 1 {
		if err := ctx.Err(); err != nil {
			return [32]byte{}, err
		}
		next := make([][32]byte, len(level)/2)
		for i := 0; i < len(next); i++ {
			var buf [64]byte
			copy(buf[:32], level[2*i][:])
			copy(buf[32:], level[2*i+1][:])
			next[i] = sha256.Sum256(buf[:])
		}
		level = next
	}
	return level[0], nil
}
