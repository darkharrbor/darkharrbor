package archive

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

// ErrPAR2NoRecoverySlices reports that a volume yielded none of the requested
// recovery slices. It is a normal abstain condition, not a malformed input:
// the wrong recovery set, the wrong slice size, and a volume whose framing
// stops early all reach it, and every caller treats it as "this volume cannot
// help" rather than as an error to surface.
var ErrPAR2NoRecoverySlices = errors.New("par2: no matching recovery slices")

const (
	// maxPAR2RecoveryPackets bounds one volume walk against a pathological
	// packet count.
	maxPAR2RecoveryPackets = 100_000
	// maxPAR2RecoveryWalkBytes bounds how far into a volume the framing walk
	// will advance, independent of the caller's payload budget.
	maxPAR2RecoveryWalkBytes = 512 << 20

	par2TypeRecoverySlice = "RecvSlic"
)

// PAR2RecoverySlice is one Recovery Slice packet: an exponent and exactly one
// slice of recovery payload. It carries no file identity, no URL, and no
// message identifier.
type PAR2RecoverySlice struct {
	Exponent uint32
	Data     []byte
}

// PAR2RecoveryExponents discovers which recovery exponents a volume holds,
// without reading any recovery payload.
//
// Discovery is separated from collection because exponent selection decides
// the solver's coefficient matrix, and that decision must be made across
// candidate volumes before committing a single byte of the caller's payload
// budget to any of them.
func PAR2RecoveryExponents(ctx context.Context, src bytesource.ByteSource, recoverySetID string, sliceSize int64, need int) ([]uint32, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	if need <= 0 {
		return nil, errors.New("par2: exponent discovery needs a positive count")
	}
	if err := validateRecoveryScan(src, sliceSize); err != nil {
		return nil, err
	}

	found := make([]uint32, 0, need)
	seen := make(map[uint32]struct{}, need)
	err := walkPAR2RecoveryPackets(ctx, src, recoverySetID, sliceSize,
		func(exponent uint32, bodyOffset int64) (bool, error) {
			if _, dup := seen[exponent]; dup {
				return true, nil
			}
			seen[exponent] = struct{}{}
			found = append(found, exponent)
			return len(found) < need, nil
		})
	if err != nil {
		return nil, err
	}
	if len(found) < need {
		return nil, fmt.Errorf("%w: found %d of %d requested exponents", ErrPAR2NoRecoverySlices, len(found), need)
	}
	return found, nil
}

// ScanPAR2RecoverySlices collects exactly the requested exponents' payloads
// from one recovery volume, spending no more than byteBudget bytes.
//
// Anything ambiguous fails closed. A slice whose body is not exactly
// 4+sliceSize bytes is skipped, a slice from a foreign recovery set is never
// mixed in, and a duplicate exponent whose payload disagrees with the one
// already held aborts the scan rather than letting an arbitrary winner decide
// what gets reconstructed.
func ScanPAR2RecoverySlices(
	ctx context.Context,
	src bytesource.ByteSource,
	recoverySetID string,
	sliceSize int64,
	want map[uint32]struct{},
	byteBudget int64,
) ([]PAR2RecoverySlice, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	if len(want) == 0 {
		return nil, errors.New("par2: no recovery exponents requested")
	}
	if err := validateRecoveryScan(src, sliceSize); err != nil {
		return nil, err
	}
	if byteBudget < sliceSize {
		return nil, fmt.Errorf("par2: %d byte budget cannot hold one %d byte slice", byteBudget, sliceSize)
	}

	out := make([]PAR2RecoverySlice, 0, len(want))
	byExponent := make(map[uint32]int, len(want))
	var spent int64

	err := walkPAR2RecoveryPackets(ctx, src, recoverySetID, sliceSize,
		func(exponent uint32, bodyOffset int64) (bool, error) {
			if _, requested := want[exponent]; !requested {
				return true, nil
			}
			payload := make([]byte, sliceSize)
			if _, err := src.ReadAt(ctx, payload, bodyOffset+4); err != nil {
				if cErr := ctxErr(ctx); cErr != nil {
					return false, cErr
				}
				// A volume that cannot deliver a slice it framed is simply
				// not usable for that exponent; keep walking.
				return true, nil
			}
			if prior, dup := byExponent[exponent]; dup {
				if !equalBytes(out[prior].Data, payload) {
					return false, fmt.Errorf("par2: conflicting recovery slices for exponent %d", exponent)
				}
				return true, nil
			}
			if spent > byteBudget-sliceSize {
				return false, nil
			}
			spent += sliceSize
			byExponent[exponent] = len(out)
			out = append(out, PAR2RecoverySlice{Exponent: exponent, Data: payload})
			return len(out) < len(want), nil
		})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: none of the %d requested exponents are present", ErrPAR2NoRecoverySlices, len(want))
	}
	return out, nil
}

func validateRecoveryScan(src bytesource.ByteSource, sliceSize int64) error {
	if src == nil {
		return errors.New("par2: nil recovery byte source")
	}
	if sliceSize <= 0 || sliceSize%4 != 0 || sliceSize > MaxPAR2SliceBytes {
		return fmt.Errorf("par2: unusable recovery slice size %d", sliceSize)
	}
	if src.Size() <= 0 {
		return errors.New("par2: empty recovery byte source")
	}
	return nil
}

// walkPAR2RecoveryPackets streams packet framing over src, invoking visit for
// every Recovery Slice packet of the requested recovery set whose body is
// exactly one slice plus its exponent. visit returns false to stop the walk.
//
// The walk reads headers only; payload reads are the visitor's choice, which
// is what keeps discovery cheap over a multi-megabyte volume fetched across
// real segments.
func walkPAR2RecoveryPackets(
	ctx context.Context,
	src bytesource.ByteSource,
	recoverySetID string,
	sliceSize int64,
	visit func(exponent uint32, bodyOffset int64) (bool, error),
) error {
	size := src.Size()
	if size > maxPAR2RecoveryWalkBytes {
		size = maxPAR2RecoveryWalkBytes
	}
	header := make([]byte, par2HeaderSize)
	expBuf := make([]byte, 4)
	var pos int64
	packets := 0

	for pos+par2HeaderSize <= size {
		if err := ctxErr(ctx); err != nil {
			return err
		}
		if _, err := src.ReadAt(ctx, header, pos); err != nil {
			if cErr := ctxErr(ctx); cErr != nil {
				return cErr
			}
			// A short read at the tail is the natural end of a source whose
			// declared size exceeds its decoded length; stop cleanly.
			break
		}
		if [8]byte(header[0:8]) != par2Magic {
			break
		}
		length := binary.LittleEndian.Uint64(header[8:16])
		if length < par2HeaderSize || length%4 != 0 || length > uint64(size-pos) {
			break
		}
		setID := hexEncode(header[32:48])
		typeField := string(header[48:64])
		bodyOffset := pos + par2HeaderSize
		bodyLen := int64(length) - par2HeaderSize

		if bodyLen == sliceSize+4 &&
			isPAR2PacketType(typeField, par2TypeRecoverySlice) &&
			(recoverySetID == "" || setID == recoverySetID) {
			if _, err := src.ReadAt(ctx, expBuf, bodyOffset); err != nil {
				if cErr := ctxErr(ctx); cErr != nil {
					return cErr
				}
				break
			}
			keepGoing, err := visit(binary.LittleEndian.Uint32(expBuf), bodyOffset)
			if err != nil {
				return err
			}
			if !keepGoing {
				return nil
			}
		}

		pos += int64(length)
		packets++
		if packets > maxPAR2RecoveryPackets {
			break
		}
	}
	return nil
}

func isPAR2PacketType(typeField, name string) bool {
	if !strings.HasPrefix(typeField, par2TypePrefix) {
		return false
	}
	return strings.TrimRight(typeField[len(par2TypePrefix):], "\x00") == name
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
