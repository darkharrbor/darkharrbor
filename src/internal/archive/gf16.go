package archive

import (
	"errors"
	"fmt"
	"sync"
)

// GF(2^16) arithmetic and the Reed-Solomon solver PAR2 2.0 defines: generator
// polynomial 0x1100B, primitive element 2, and 16-bit little-endian word
// ordering over slice payloads.
//
// This is the repository's only Galois-field implementation. It lives beside
// the PAR2 packet parser because PAR2 recovery is its sole consumer; no other
// lane performs erasure coding.
const (
	gf16Poly = 0x1100B
	// gf16Limit is the multiplicative order of the field, 2^16 - 1.
	gf16Limit = 65535

	// MaxPAR2TotalBlocks is the number of distinct input-block constants the
	// field admits. 65535 = 3 x 5 x 17 x 257, so exactly phi(65535) = 32768
	// discrete logs are relatively prime to it, and PAR2 assigns one per
	// input block.
	MaxPAR2TotalBlocks = 32768

	// MaxPAR2SliceBytes is the largest recovery slice this package will
	// accept. It matches nntp.MaxPAR2ProofBlockBytes so proof and recovery
	// geometry cannot disagree about what an acceptable block is.
	MaxPAR2SliceBytes = 64 << 20

	// MaxPAR2UnknownBlocks bounds the linear system's dimension, and with it
	// both the solve cost and the solver's resident working set.
	MaxPAR2UnknownBlocks = 256

	// maxPAR2SolverBytes bounds unknown-count x slice-size, the solver's
	// entire allocation.
	maxPAR2SolverBytes = 256 << 20
)

var (
	gf16ALog  [gf16Limit]uint16 // gf16ALog[i] == 2^i
	gf16Log   [65536]uint16     // gf16Log[gf16ALog[i]] == i; index 0 is never read
	gf16Once  sync.Once
	par2Const struct {
		once   sync.Once
		values []uint16
	}
)

func init() { gf16Init() }

// gf16Init builds the field tables exactly once. It is safe to call from any
// entry point and from any goroutine.
func gf16Init() {
	gf16Once.Do(func() {
		x := uint32(1)
		for i := 0; i < gf16Limit; i++ {
			gf16ALog[i] = uint16(x)
			gf16Log[uint16(x)] = uint16(i)
			x <<= 1
			if x&0x10000 != 0 {
				x ^= gf16Poly
			}
		}
	})
}

func gf16Mul(a, b uint16) uint16 {
	if a == 0 || b == 0 {
		return 0
	}
	sum := uint32(gf16Log[a]) + uint32(gf16Log[b])
	if sum >= gf16Limit {
		sum -= gf16Limit
	}
	return gf16ALog[sum]
}

// gf16Inv returns the multiplicative inverse of a. Zero has none, and that is
// reported rather than silently yielding a wrong reconstruction.
func gf16Inv(a uint16) (uint16, bool) {
	if a == 0 {
		return 0, false
	}
	l := uint32(gf16Log[a])
	if l == 0 {
		return 1, true
	}
	return gf16ALog[gf16Limit-l], true
}

// gf16Pow raises a to exponent e. The exponent is reduced modulo the field's
// multiplicative order, which is what makes a PAR2 constant's powers cycle
// without ever repeating within one recovery set.
func gf16Pow(a uint16, e uint32) uint16 {
	if e == 0 {
		return 1
	}
	if a == 0 {
		return 0
	}
	l := (uint64(gf16Log[a]) * uint64(e)) % gf16Limit
	return gf16ALog[l]
}

func gcdU32(a, b uint32) uint32 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// PAR2InputConstants returns the constants assigned to the first count input
// blocks of a recovery set, in PAR2's own order.
//
// par2cmdline walks candidate discrete logs upward and keeps only those
// relatively prime to 65535, assigning 2^log to each successive input block.
// Reproducing that ordering exactly is mandatory: an off-by-one in this
// sequence produces a solver that inverts cleanly and returns confident,
// entirely wrong bytes.
func PAR2InputConstants(count int) ([]uint16, bool) {
	if count <= 0 || count > MaxPAR2TotalBlocks {
		return nil, false
	}
	gf16Init()
	par2Const.once.Do(func() {
		values := make([]uint16, 0, MaxPAR2TotalBlocks)
		for l := uint32(1); l < gf16Limit; l++ {
			if gcdU32(gf16Limit, l) == 1 {
				values = append(values, gf16ALog[l])
			}
		}
		par2Const.values = values
	})
	if count > len(par2Const.values) {
		return nil, false
	}
	return par2Const.values[:count], true
}

// mulXorInto computes dst ^= src * factor over 16-bit little-endian words. A
// shorter src is treated as zero-extended, which is exactly how PAR2 pads a
// file's final block.
func mulXorInto(dst, src []byte, factor uint16) {
	if factor == 0 {
		return
	}
	n := len(dst)
	if len(src) < n {
		n = len(src)
	}
	n &^= 1
	if factor == 1 {
		for i := 0; i < n; i++ {
			dst[i] ^= src[i]
		}
		return
	}
	logFactor := uint32(gf16Log[factor])
	for i := 0; i < n; i += 2 {
		word := uint16(src[i]) | uint16(src[i+1])<<8
		if word == 0 {
			continue
		}
		sum := uint32(gf16Log[word]) + logFactor
		if sum >= gf16Limit {
			sum -= gf16Limit
		}
		product := gf16ALog[sum]
		dst[i] ^= byte(product)
		dst[i+1] ^= byte(product >> 8)
	}
}

// PAR2Solver recovers a bounded set of unknown input blocks from an equal
// number of recovery slices.
//
// Each recovery slice is a linear combination over *every* input block of the
// recovery set, so the unknowns can only be isolated once every surviving
// block has been folded out. Known blocks are therefore streamed in one at a
// time through AddKnownBlock and applied immediately: the resident working set
// stays at len(unknown) slices no matter how large the recovery set is.
// Callers bound the *fetch* volume separately, before constructing a solver.
type PAR2Solver struct {
	sliceSize int64
	total     int64
	unknown   []int64
	exponents []uint32

	coeff [][]uint16 // row r (exponent r) by column c (unknown c)
	rhs   [][]byte   // row r's running right-hand side

	unknownSet map[int64]struct{}
	haveSlice  []bool
	slicesSeen int
	known      map[int64]struct{}
	pad        []byte
	solved     bool
}

// NewPAR2Solver validates the geometry and builds the coefficient matrix.
// Every malformed or ambiguous construction is refused rather than repaired.
func NewPAR2Solver(sliceSize int64, total int64, unknown []int64, exponents []uint32) (*PAR2Solver, error) {
	if sliceSize <= 0 || sliceSize%4 != 0 || sliceSize > MaxPAR2SliceBytes {
		return nil, fmt.Errorf("par2: unusable slice size %d", sliceSize)
	}
	if total <= 0 || total > MaxPAR2TotalBlocks {
		return nil, fmt.Errorf("par2: unusable input block count %d", total)
	}
	if len(unknown) == 0 {
		return nil, errors.New("par2: no unknown blocks to reconstruct")
	}
	if len(unknown) != len(exponents) {
		return nil, fmt.Errorf("par2: %d unknown blocks against %d exponents", len(unknown), len(exponents))
	}
	if len(unknown) > MaxPAR2UnknownBlocks {
		return nil, fmt.Errorf("par2: unknown block count %d exceeds bound %d", len(unknown), MaxPAR2UnknownBlocks)
	}
	if int64(len(unknown)) > total {
		return nil, errors.New("par2: more unknown blocks than input blocks")
	}
	if int64(len(unknown))*sliceSize > maxPAR2SolverBytes {
		return nil, errors.New("par2: solver working set exceeds bound")
	}

	unknownSet := make(map[int64]struct{}, len(unknown))
	for _, index := range unknown {
		if index < 0 || index >= total {
			return nil, fmt.Errorf("par2: unknown block index %d out of range", index)
		}
		if _, dup := unknownSet[index]; dup {
			return nil, fmt.Errorf("par2: duplicate unknown block index %d", index)
		}
		unknownSet[index] = struct{}{}
	}
	seenExp := make(map[uint32]struct{}, len(exponents))
	for _, e := range exponents {
		if _, dup := seenExp[e]; dup {
			return nil, fmt.Errorf("par2: duplicate recovery exponent %d", e)
		}
		seenExp[e] = struct{}{}
	}

	constants, ok := PAR2InputConstants(int(total))
	if !ok {
		return nil, fmt.Errorf("par2: field admits no constants for %d input blocks", total)
	}

	dim := len(unknown)
	s := &PAR2Solver{
		sliceSize:  sliceSize,
		total:      total,
		unknown:    append([]int64(nil), unknown...),
		exponents:  append([]uint32(nil), exponents...),
		coeff:      make([][]uint16, dim),
		rhs:        make([][]byte, dim),
		unknownSet: unknownSet,
		haveSlice:  make([]bool, dim),
		known:      make(map[int64]struct{}, int(total)-dim),
		pad:        make([]byte, sliceSize),
	}
	for r := 0; r < dim; r++ {
		s.coeff[r] = make([]uint16, dim)
		for c := 0; c < dim; c++ {
			s.coeff[r][c] = gf16Pow(constants[unknown[c]], exponents[r])
		}
		s.rhs[r] = make([]byte, sliceSize)
	}
	return s, nil
}

// AddRecoverySlice supplies one declared exponent's recovery payload.
func (s *PAR2Solver) AddRecoverySlice(exponent uint32, data []byte) error {
	if s.solved {
		return errors.New("par2: solver already solved")
	}
	row := -1
	for i, e := range s.exponents {
		if e == exponent {
			row = i
			break
		}
	}
	if row < 0 {
		return fmt.Errorf("par2: recovery exponent %d was not declared", exponent)
	}
	if int64(len(data)) != s.sliceSize {
		return fmt.Errorf("par2: recovery slice is %d bytes, expected %d", len(data), s.sliceSize)
	}
	if s.haveSlice[row] {
		return fmt.Errorf("par2: recovery exponent %d supplied twice", exponent)
	}
	copy(s.rhs[row], data)
	s.haveSlice[row] = true
	s.slicesSeen++
	return nil
}

// AddKnownBlock folds one surviving input block out of every right-hand side.
// data shorter than one slice is zero-extended, exactly as PAR2 pads a file's
// final block; data longer than one slice is refused rather than truncated.
// The slice is not retained, so callers may reuse the buffer immediately.
func (s *PAR2Solver) AddKnownBlock(index int64, data []byte) error {
	if s.solved {
		return errors.New("par2: solver already solved")
	}
	if index < 0 || index >= s.total {
		return fmt.Errorf("par2: known block index %d out of range", index)
	}
	if _, unknown := s.unknownSet[index]; unknown {
		return fmt.Errorf("par2: block %d was declared unknown", index)
	}
	if _, dup := s.known[index]; dup {
		return fmt.Errorf("par2: block %d supplied twice", index)
	}
	if int64(len(data)) > s.sliceSize {
		return fmt.Errorf("par2: known block %d is %d bytes, exceeding the %d byte slice", index, len(data), s.sliceSize)
	}
	block := data
	if int64(len(data)) < s.sliceSize {
		for i := range s.pad {
			s.pad[i] = 0
		}
		copy(s.pad, data)
		block = s.pad
	}
	constants, ok := PAR2InputConstants(int(s.total))
	if !ok {
		return errors.New("par2: input constants unavailable")
	}
	for r := range s.rhs {
		mulXorInto(s.rhs[r], block, gf16Pow(constants[index], s.exponents[r]))
	}
	s.known[index] = struct{}{}
	return nil
}

// Solve inverts the coefficient matrix and applies it to the accumulated
// right-hand sides, returning the reconstructed blocks in the order the
// unknowns were declared.
//
// Completeness is enforced before any bytes are produced: a missing recovery
// slice or a missing surviving block would otherwise yield a confident,
// wrong result rather than an error.
func (s *PAR2Solver) Solve() ([][]byte, error) {
	if s.solved {
		return nil, errors.New("par2: solver already solved")
	}
	dim := len(s.unknown)
	if s.slicesSeen != dim {
		return nil, fmt.Errorf("par2: %d of %d recovery slices supplied", s.slicesSeen, dim)
	}
	inv, err := invertGF16Matrix(s.coeff)
	if err != nil {
		return nil, err
	}
	if wantKnown := int(s.total) - dim; len(s.known) != wantKnown {
		return nil, fmt.Errorf("par2: %d of %d surviving blocks supplied", len(s.known), wantKnown)
	}

	out := make([][]byte, dim)
	for c := 0; c < dim; c++ {
		block := make([]byte, s.sliceSize)
		for r := 0; r < dim; r++ {
			mulXorInto(block, s.rhs[r], inv[c][r])
		}
		out[c] = block
	}
	s.solved = true
	return out, nil
}

// invertGF16Matrix inverts a square GF(2^16) matrix by Gauss-Jordan
// elimination against an identity augmentation. A singular matrix means the
// chosen recovery slices cannot separate the unknowns, which is an abstain,
// never an approximation.
func invertGF16Matrix(m [][]uint16) ([][]uint16, error) {
	dim := len(m)
	if dim == 0 {
		return nil, errors.New("par2: empty coefficient matrix")
	}
	work := make([][]uint16, dim)
	inv := make([][]uint16, dim)
	for r := 0; r < dim; r++ {
		if len(m[r]) != dim {
			return nil, errors.New("par2: coefficient matrix is not square")
		}
		work[r] = append([]uint16(nil), m[r]...)
		inv[r] = make([]uint16, dim)
		inv[r][r] = 1
	}
	for col := 0; col < dim; col++ {
		pivot := -1
		for r := col; r < dim; r++ {
			if work[r][col] != 0 {
				pivot = r
				break
			}
		}
		if pivot < 0 {
			return nil, errors.New("par2: recovery slices are linearly dependent")
		}
		if pivot != col {
			work[pivot], work[col] = work[col], work[pivot]
			inv[pivot], inv[col] = inv[col], inv[pivot]
		}
		scale, ok := gf16Inv(work[col][col])
		if !ok {
			return nil, errors.New("par2: singular coefficient matrix")
		}
		for c := 0; c < dim; c++ {
			work[col][c] = gf16Mul(work[col][c], scale)
			inv[col][c] = gf16Mul(inv[col][c], scale)
		}
		for r := 0; r < dim; r++ {
			if r == col {
				continue
			}
			factor := work[r][col]
			if factor == 0 {
				continue
			}
			for c := 0; c < dim; c++ {
				work[r][c] ^= gf16Mul(work[col][c], factor)
				inv[r][c] ^= gf16Mul(inv[col][c], factor)
			}
		}
	}
	return inv, nil
}
