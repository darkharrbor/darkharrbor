package archive

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/darkharrbor/darkharrbor/internal/bytesource"
)

const (
	rarBlockSize       = aes.BlockSize
	rar3KDFRounds      = 0x40000
	rar5MaxKDFLog2     = 24
	rarKDFCancelStride = 4096
)

// RARKey is request-local derived key material. It is never serialized.
type RARKey struct {
	key []byte
	iv  [rarBlockSize]byte
}

// InitialIV returns a copy of the archive member's initial CBC vector.
func (k *RARKey) InitialIV() []byte {
	if k == nil {
		return nil
	}
	iv := make([]byte, len(k.iv))
	copy(iv, k.iv[:])
	return iv
}

// DecryptBlocks decrypts complete AES-CBC blocks using iv. Callers may supply
// the immediately preceding ciphertext block as iv for random access.
func (k *RARKey) DecryptBlocks(dst, src, iv []byte) error {
	if k == nil || len(iv) != rarBlockSize || len(src) == 0 || len(src)%rarBlockSize != 0 || len(dst) < len(src) {
		return fmt.Errorf("rar: invalid encrypted block span")
	}
	block, err := aes.NewCipher(k.key)
	if err != nil {
		return fmt.Errorf("rar: initialize cipher: %w", err)
	}
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(dst[:len(src)], src)
	return nil
}

// DecryptRARPrefix decrypts at most limit logical bytes from the start of one
// encrypted member. It is used for resolver-time media and password proof.
func DecryptRARPrefix(ctx context.Context, src bytesource.ByteSource, member StoredRARMember, password string, limit int64) ([]byte, error) {
	if src == nil || member.Encryption == nil || member.DataOffset < 0 || member.PackedSize <= 0 || limit <= 0 {
		return nil, fmt.Errorf("rar: invalid encrypted prefix span")
	}
	key, err := DeriveRARKey(ctx, password, member.Encryption)
	if err != nil {
		return nil, err
	}
	logical := min(limit, member.DataSize)
	cipherBytes := (logical + rarBlockSize - 1) &^ (rarBlockSize - 1)
	if cipherBytes > member.PackedSize {
		cipherBytes = member.PackedSize &^ (rarBlockSize - 1)
	}
	if cipherBytes <= 0 || cipherBytes > int64(maxRARHeaderBytes) {
		return nil, fmt.Errorf("rar: encrypted prefix exceeds bound")
	}
	ciphertext, err := readAt(ctx, src, member.DataOffset, cipherBytes)
	if err != nil {
		return nil, err
	}
	plaintext := make([]byte, len(ciphertext))
	if err := key.DecryptBlocks(plaintext, ciphertext, key.InitialIV()); err != nil {
		return nil, err
	}
	return plaintext[:min(int64(len(plaintext)), logical)], nil
}

// DeriveRARKey derives bounded RAR3/RAR5 AES key material and validates the
// RAR5 password check when one is present.
func DeriveRARKey(ctx context.Context, password string, encryption *RAREncryption) (*RARKey, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if encryption == nil {
		return nil, fmt.Errorf("rar: encryption metadata missing")
	}
	if password == "" {
		return nil, ErrRARPasswordRequired
	}
	switch encryption.Version {
	case 3:
		return deriveRAR3Key(ctx, password, encryption.Salt)
	case 5:
		return deriveRAR5Key(ctx, password, encryption)
	default:
		return nil, fmt.Errorf("rar: unsupported encryption version")
	}
}

func deriveRAR3Key(ctx context.Context, password string, salt []byte) (*RARKey, error) {
	if !utf8.ValidString(password) || (len(salt) != 0 && len(salt) != 8) {
		return nil, fmt.Errorf("rar3: invalid encryption parameters")
	}
	units := utf16.Encode([]rune(password))
	raw := make([]byte, len(units)*2, len(units)*2+len(salt))
	for i, unit := range units {
		binary.LittleEndian.PutUint16(raw[i*2:], unit)
	}
	raw = append(raw, salt...)
	// RAR3's historical SHA-1 routine mutates password buffers only when one
	// KDF input exceeds a SHA-1 block. Refuse that rare ambiguous case.
	if len(raw) > sha1.BlockSize {
		return nil, fmt.Errorf("rar3: password is too long")
	}

	hash := sha1.New()
	key := &RARKey{key: make([]byte, 16)}
	var counter [3]byte
	for i := 0; i < rar3KDFRounds; i++ {
		if i%rarKDFCancelStride == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		_, _ = hash.Write(raw)
		counter[0] = byte(i)
		counter[1] = byte(i >> 8)
		counter[2] = byte(i >> 16)
		_, _ = hash.Write(counter[:])
		if i%(rar3KDFRounds/16) == 0 {
			sum := hash.Sum(nil)
			key.iv[i/(rar3KDFRounds/16)] = sum[len(sum)-1]
		}
	}
	sum := hash.Sum(nil)
	for i := 0; i < 4; i++ {
		key.key[i*4+0] = sum[i*4+3]
		key.key[i*4+1] = sum[i*4+2]
		key.key[i*4+2] = sum[i*4+1]
		key.key[i*4+3] = sum[i*4+0]
	}
	return key, nil
}

func deriveRAR5Key(ctx context.Context, password string, encryption *RAREncryption) (*RARKey, error) {
	if !utf8.ValidString(password) || len(encryption.Salt) != 16 || len(encryption.IV) != 16 || encryption.KDFLog2 > rar5MaxKDFLog2 {
		return nil, fmt.Errorf("rar5: invalid encryption parameters")
	}
	rounds := uint64(1) << encryption.KDFLog2
	saltBlock := make([]byte, 20)
	copy(saltBlock, encryption.Salt)
	saltBlock[19] = 1

	mac := hmac.New(sha256.New, []byte(password))
	_, _ = mac.Write(saltBlock)
	u := mac.Sum(nil)
	acc := append([]byte(nil), u...)
	var key, passwordValue []byte
	if rounds == 1 {
		key = append([]byte(nil), acc...)
	}
	for count := uint64(2); count <= rounds+32; count++ {
		if count%rarKDFCancelStride == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		mac.Reset()
		_, _ = mac.Write(u)
		u = mac.Sum(u[:0])
		for i := range acc {
			acc[i] ^= u[i]
		}
		if count == rounds {
			key = append([]byte(nil), acc...)
		}
		if count == rounds+32 {
			passwordValue = append([]byte(nil), acc...)
		}
	}
	if len(encryption.PasswordCheck) != 0 {
		if len(encryption.PasswordCheck) != 8 {
			return nil, fmt.Errorf("rar5: invalid password check")
		}
		var check [8]byte
		for i, value := range passwordValue {
			check[i%len(check)] ^= value
		}
		if subtle.ConstantTimeCompare(check[:], encryption.PasswordCheck) != 1 {
			return nil, ErrRARBadPassword
		}
	}
	result := &RARKey{key: key}
	copy(result.iv[:], encryption.IV)
	return result, nil
}
