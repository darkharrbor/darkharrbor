package util

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log/slog"

	"golang.org/x/crypto/argon2"
)

// RandomHex generates n random bytes and returns them as a hex-encoded string.
func RandomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate random bytes: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// SHA1Hex returns the SHA-1 hash of v as a hex-encoded string.
func SHA1Hex(v string) string {
	sum := sha1.Sum([]byte(v))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// ---------------------------------------------------------------------------

// Redacted is a secret-bearing string whose printed forms mask the value.
// New F-D plumbing carries secrets as Redacted; existing config fields stay
// plain string because they are consumed by packages outside this gate's
// authorization (api/torznab.go Bearer comparisons, router StreamSecret).
type Redacted string

// Value returns the underlying secret (the only deliberate unwrap point).
func (r Redacted) Value() string { return string(r) }

// String masks the secret for fmt/%s/%v.
func (r Redacted) String() string {
	if r == "" {
		return ""
	}
	return "[redacted]"
}

// GoString masks the secret for %#v.
func (r Redacted) GoString() string { return r.String() }

// LogValue masks the secret for slog.
func (r Redacted) LogValue() slog.Value { return slog.StringValue(r.String()) }

// Sealed-file format, version 1 (all integers big-endian):
//
//	offset  len  field
//	0       6    magic "OCSEAL"
//	6       1    version (0x01)
//	7       4    argon2id time parameter
//	11      4    argon2id memory parameter (KiB)
//	15      1    argon2id threads parameter
//	16      16   random salt
//	32      12   AES-GCM nonce
//	44      ..   AES-256-GCM ciphertext||tag over KEY=VALUE lines
//
// KDF parameters are pinned below (argon2id time=3, memory=64 MiB,
// threads=4 — RFC 9106 second recommended parameter choice scaled to a
// homelab container) and recorded in the header so future tuning leaves
// existing files decryptable.
const (
	sealMagic   = "OCSEAL"
	sealVersion = 1

	sealArgonTime      uint32 = 3
	sealArgonMemoryKiB uint32 = 64 * 1024
	sealArgonThreads   uint8  = 4

	sealSaltLen  = 16
	sealNonceLen = 12
	sealKeyLen   = 32
)

// SealSecrets encrypts plaintext (env-style KEY=VALUE lines) under password.
func SealSecrets(plaintext []byte, password string) ([]byte, error) {
	salt := make([]byte, sealSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("seal: generate salt: %w", err)
	}
	nonce := make([]byte, sealNonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("seal: generate nonce: %w", err)
	}
	gcm, err := sealAEAD(password, salt, sealArgonTime, sealArgonMemoryKiB, sealArgonThreads)
	if err != nil {
		return nil, err
	}

	var out bytes.Buffer
	out.WriteString(sealMagic)
	out.WriteByte(sealVersion)
	_ = binary.Write(&out, binary.BigEndian, sealArgonTime)
	_ = binary.Write(&out, binary.BigEndian, sealArgonMemoryKiB)
	out.WriteByte(sealArgonThreads)
	out.Write(salt)
	out.Write(nonce)
	out.Write(gcm.Seal(nil, nonce, plaintext, headerAAD(sealArgonTime, sealArgonMemoryKiB, sealArgonThreads, salt, nonce)))
	return out.Bytes(), nil
}

// UnsealSecrets decrypts a sealed-secrets blob. A wrong password and a
// corrupt file are indistinguishable by design (GCM authentication failure).
func UnsealSecrets(blob []byte, password string) ([]byte, error) {
	const headerLen = 6 + 1 + 4 + 4 + 1 + sealSaltLen + sealNonceLen
	if len(blob) < headerLen {
		return nil, fmt.Errorf("unseal: file too short to be a sealed secrets file")
	}
	if string(blob[:6]) != sealMagic {
		return nil, fmt.Errorf("unseal: bad magic (not an %s file)", sealMagic)
	}
	if blob[6] != sealVersion {
		return nil, fmt.Errorf("unseal: unsupported version %d", blob[6])
	}
	t := binary.BigEndian.Uint32(blob[7:11])
	mem := binary.BigEndian.Uint32(blob[11:15])
	threads := blob[15]
	// Bound the KDF parameters BEFORE deriving the key: the header is only
	// authenticated by GCM after the KDF runs, so a corrupt/hostile header
	// could otherwise demand absurd argon2 work (e.g. time=2^24) as a
	// denial-of-service. Anything outside these generous caps cannot have
	// been written by SealSecrets and is rejected immediately.
	if t == 0 || t > 64 || mem == 0 || mem > 1<<20 || threads == 0 || threads > 32 {
		return nil, fmt.Errorf("unseal: implausible KDF parameters (corrupt file)")
	}
	salt := blob[16 : 16+sealSaltLen]
	nonce := blob[16+sealSaltLen : 16+sealSaltLen+sealNonceLen]
	ct := blob[headerLen:]

	gcm, err := sealAEAD(password, salt, t, mem, threads)
	if err != nil {
		return nil, err
	}
	pt, err := gcm.Open(nil, nonce, ct, headerAAD(t, mem, threads, salt, nonce))
	if err != nil {
		return nil, fmt.Errorf("unseal: decrypt failed (wrong password or corrupt file)")
	}
	return pt, nil
}

func sealAEAD(password string, salt []byte, t, memKiB uint32, threads uint8) (cipher.AEAD, error) {
	key := argon2.IDKey([]byte(password), salt, t, memKiB, threads, sealKeyLen)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("seal: aes: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("seal: gcm: %w", err)
	}
	return gcm, nil
}

// headerAAD binds the KDF parameters, salt, and nonce into the GCM
// authentication so header tampering is detected.
func headerAAD(t, memKiB uint32, threads uint8, salt, nonce []byte) []byte {
	var b bytes.Buffer
	b.WriteString(sealMagic)
	b.WriteByte(sealVersion)
	_ = binary.Write(&b, binary.BigEndian, t)
	_ = binary.Write(&b, binary.BigEndian, memKiB)
	b.WriteByte(threads)
	b.Write(salt)
	b.Write(nonce)
	return b.Bytes()
}
