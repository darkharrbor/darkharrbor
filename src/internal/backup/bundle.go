package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/securefile"
	"golang.org/x/crypto/argon2"
)

const (
	bundleMagic   = "DHBKUP"
	bundleVersion = 1
	bundleSuffix  = ".dhbackup"
	bundleChunk   = 1 << 20
	bundleMaxSize = 1 << 41

	bundleArgonTime      uint32 = 3
	bundleArgonMemoryKiB uint32 = 64 * 1024
	bundleArgonThreads   uint8  = 4
	bundleSaltLen               = 16
	bundleNoncePrefixLen        = 8
	bundleHeaderLen             = 6 + 1 + 4 + 4 + 1 + 4 + bundleSaltLen + bundleNoncePrefixLen
)

// Export encrypts the newest complete snapshot below source into targetDir.
// The existing sealed-store recovery key is intentionally reused: restoring
// secrets.sealed already requires it, and no second recovery secret is needed.
func Export(ctx context.Context, source, targetDir, password string) (string, error) {
	if strings.TrimSpace(password) == "" {
		return "", errors.New("backup export recovery key is empty")
	}
	snapshot, err := latestSnapshot(source)
	if err != nil {
		return "", err
	}
	if err := checkDatabase(ctx, filepath.Join(snapshot, databaseName)); err != nil {
		return "", err
	}
	if err := securefile.PrepareDir(targetDir); err != nil {
		return "", errors.New("create private export directory")
	}

	name := filepath.Base(snapshot) + bundleSuffix
	finalPath := filepath.Join(targetDir, name)
	if _, err := os.Lstat(finalPath); err == nil {
		return "", errors.New("backup export already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("inspect backup export destination")
	}
	tmp, err := os.CreateTemp(targetDir, ".darkharrbor-export-*")
	if err != nil {
		return "", errors.New("create backup export")
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return "", errors.New("protect backup export")
	}

	encrypted, err := newBundleWriter(tmp, password)
	if err != nil {
		return "", err
	}
	archive := tar.NewWriter(encrypted)
	for _, file := range []struct {
		name string
		max  int64
	}{{databaseName, 1 << 40}, {secretsName, maxSecretsSize}} {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		in, err := securefile.Open(filepath.Join(snapshot, file.name), file.max)
		if err != nil {
			return "", errors.New("open private snapshot file")
		}
		info, statErr := in.Stat()
		if statErr == nil {
			statErr = archive.WriteHeader(&tar.Header{Name: file.name, Mode: 0o600, Size: info.Size(), Typeflag: tar.TypeReg})
		}
		if statErr == nil {
			_, statErr = io.Copy(archive, &contextReader{ctx: ctx, reader: in})
		}
		_ = in.Close()
		if statErr != nil {
			return "", errors.New("archive snapshot file")
		}
	}
	if err := archive.Close(); err != nil {
		return "", errors.New("finish backup archive")
	}
	if err := encrypted.Close(); err != nil {
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", errors.New("sync backup export")
	}
	if err := tmp.Close(); err != nil {
		return "", errors.New("close backup export")
	}
	if err := os.Rename(tmpName, finalPath); err != nil {
		return "", errors.New("publish backup export")
	}
	if err := syncDir(targetDir); err != nil {
		return "", err
	}
	ok = true
	return name, nil
}

// Import decrypts and validates one bundle into a normal local snapshot.
func Import(ctx context.Context, bundlePath, targetDir, password string) (string, error) {
	if strings.TrimSpace(password) == "" {
		return "", errors.New("backup import recovery key is empty")
	}
	in, err := securefile.Open(bundlePath, bundleMaxSize)
	if err != nil {
		return "", errors.New("open private backup bundle")
	}
	defer func() { _ = in.Close() }()
	if err := securefile.PrepareDir(targetDir); err != nil {
		return "", errors.New("create private backup directory")
	}
	tmpDir, err := os.MkdirTemp(targetDir, ".darkharrbor-import-*")
	if err != nil {
		return "", errors.New("create backup import staging directory")
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	decrypted, err := newBundleReader(in, password)
	if err != nil {
		return "", err
	}
	archive := tar.NewReader(decrypted)
	seen := make(map[string]bool, 2)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", errors.New("read encrypted backup archive")
		}
		limit := int64(0)
		switch header.Name {
		case databaseName:
			limit = 1 << 40
		case secretsName:
			limit = maxSecretsSize
		default:
			return "", errors.New("encrypted backup contains an unexpected path")
		}
		if seen[header.Name] || header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > limit {
			return "", errors.New("encrypted backup contains an invalid file")
		}
		seen[header.Name] = true
		out, err := os.OpenFile(filepath.Join(tmpDir, header.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return "", errors.New("create imported snapshot file")
		}
		written, copyErr := io.Copy(out, &contextReader{ctx: ctx, reader: archive})
		if copyErr == nil && written != header.Size {
			copyErr = errors.New("imported snapshot size mismatch")
		}
		if copyErr == nil {
			copyErr = out.Sync()
		}
		closeErr := out.Close()
		if copyErr != nil || closeErr != nil {
			return "", errors.New("write imported snapshot file")
		}
	}
	if !seen[databaseName] || !seen[secretsName] {
		return "", errors.New("encrypted backup is incomplete")
	}
	if err := decrypted.Finish(); err != nil {
		return "", err
	}
	if err := checkDatabase(ctx, filepath.Join(tmpDir, databaseName)); err != nil {
		return "", err
	}
	if err := syncDir(tmpDir); err != nil {
		return "", err
	}

	name := snapshotPrefix + time.Now().UTC().Format(snapshotLayout)
	finalDir := filepath.Join(targetDir, name)
	if err := os.Rename(tmpDir, finalDir); err != nil {
		return "", errors.New("publish imported snapshot")
	}
	if err := syncDir(targetDir); err != nil {
		return "", err
	}
	return name, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

type bundleWriter struct {
	dst     io.Writer
	aead    cipher.AEAD
	header  []byte
	prefix  []byte
	counter uint32
	buffer  []byte
	closed  bool
}

func newBundleWriter(dst io.Writer, password string) (*bundleWriter, error) {
	salt := make([]byte, bundleSaltLen)
	prefix := make([]byte, bundleNoncePrefixLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, errors.New("generate backup salt")
	}
	if _, err := rand.Read(prefix); err != nil {
		return nil, errors.New("generate backup nonce")
	}
	header := bundleHeader(salt, prefix)
	if _, err := dst.Write(header); err != nil {
		return nil, errors.New("write backup header")
	}
	aead, err := bundleAEAD(password, salt)
	if err != nil {
		return nil, err
	}
	return &bundleWriter{dst: dst, aead: aead, header: header, prefix: prefix, buffer: make([]byte, 0, bundleChunk)}, nil
}

func (w *bundleWriter) Write(p []byte) (int, error) {
	if w.closed {
		return 0, errors.New("write closed backup bundle")
	}
	written := 0
	for len(p) > 0 {
		n := min(len(p), bundleChunk-len(w.buffer))
		w.buffer = append(w.buffer, p[:n]...)
		p = p[n:]
		written += n
		if len(w.buffer) == bundleChunk {
			if err := w.writeRecord(w.buffer); err != nil {
				return written, err
			}
			w.buffer = w.buffer[:0]
		}
	}
	return written, nil
}

func (w *bundleWriter) Close() error {
	if w.closed {
		return nil
	}
	if len(w.buffer) > 0 {
		if err := w.writeRecord(w.buffer); err != nil {
			return err
		}
	}
	if err := w.writeRecord(nil); err != nil {
		return err
	}
	w.closed = true
	return nil
}

func (w *bundleWriter) writeRecord(plain []byte) error {
	if w.counter == math.MaxUint32 {
		return errors.New("backup bundle is too large")
	}
	nonce := bundleNonce(w.prefix, w.counter)
	aad := bundleAAD(w.header, w.counter, uint32(len(plain)))
	ciphertext := w.aead.Seal(nil, nonce, plain, aad)
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(plain)))
	if _, err := w.dst.Write(size[:]); err != nil {
		return errors.New("write backup record")
	}
	if _, err := w.dst.Write(ciphertext); err != nil {
		return errors.New("write encrypted backup record")
	}
	w.counter++
	return nil
}

type bundleReader struct {
	src      io.Reader
	aead     cipher.AEAD
	header   []byte
	prefix   []byte
	counter  uint32
	current  *bytes.Reader
	finished bool
}

func newBundleReader(src io.Reader, password string) (*bundleReader, error) {
	header := make([]byte, bundleHeaderLen)
	if _, err := io.ReadFull(src, header); err != nil {
		return nil, errors.New("read encrypted backup header")
	}
	if string(header[:6]) != bundleMagic || header[6] != bundleVersion ||
		binary.BigEndian.Uint32(header[7:11]) != bundleArgonTime ||
		binary.BigEndian.Uint32(header[11:15]) != bundleArgonMemoryKiB ||
		header[15] != bundleArgonThreads || binary.BigEndian.Uint32(header[16:20]) != bundleChunk {
		return nil, errors.New("unsupported or corrupt encrypted backup header")
	}
	salt := header[20 : 20+bundleSaltLen]
	prefix := header[20+bundleSaltLen:]
	aead, err := bundleAEAD(password, salt)
	if err != nil {
		return nil, err
	}
	return &bundleReader{src: src, aead: aead, header: header, prefix: prefix}, nil
}

func (r *bundleReader) Read(p []byte) (int, error) {
	for r.current == nil || r.current.Len() == 0 {
		if r.finished {
			return 0, io.EOF
		}
		plain, terminal, err := r.readRecord()
		if err != nil {
			return 0, err
		}
		if terminal {
			r.finished = true
			var extra [1]byte
			if n, err := r.src.Read(extra[:]); n != 0 || !errors.Is(err, io.EOF) {
				return 0, errors.New("encrypted backup has trailing data")
			}
			return 0, io.EOF
		}
		r.current = bytes.NewReader(plain)
	}
	return r.current.Read(p)
}

func (r *bundleReader) Finish() error {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		for _, b := range buf[:n] {
			if b != 0 {
				return errors.New("encrypted backup contains trailing archive data")
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (r *bundleReader) readRecord() ([]byte, bool, error) {
	if r.counter == math.MaxUint32 {
		return nil, false, errors.New("encrypted backup has too many records")
	}
	var size [4]byte
	if _, err := io.ReadFull(r.src, size[:]); err != nil {
		return nil, false, errors.New("encrypted backup is truncated")
	}
	plainLen := binary.BigEndian.Uint32(size[:])
	if plainLen > bundleChunk {
		return nil, false, errors.New("encrypted backup record is too large")
	}
	ciphertext := make([]byte, int(plainLen)+r.aead.Overhead())
	if _, err := io.ReadFull(r.src, ciphertext); err != nil {
		return nil, false, errors.New("encrypted backup is truncated")
	}
	plain, err := r.aead.Open(nil, bundleNonce(r.prefix, r.counter), ciphertext, bundleAAD(r.header, r.counter, plainLen))
	if err != nil {
		return nil, false, errors.New("encrypted backup authentication failed")
	}
	r.counter++
	return plain, plainLen == 0, nil
}

func bundleHeader(salt, prefix []byte) []byte {
	var out bytes.Buffer
	out.WriteString(bundleMagic)
	out.WriteByte(bundleVersion)
	_ = binary.Write(&out, binary.BigEndian, bundleArgonTime)
	_ = binary.Write(&out, binary.BigEndian, bundleArgonMemoryKiB)
	out.WriteByte(bundleArgonThreads)
	_ = binary.Write(&out, binary.BigEndian, uint32(bundleChunk))
	out.Write(salt)
	out.Write(prefix)
	return out.Bytes()
}

func bundleAEAD(password string, salt []byte) (cipher.AEAD, error) {
	key := argon2.IDKey([]byte(password), salt, bundleArgonTime, bundleArgonMemoryKiB, bundleArgonThreads, 32)
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create backup cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create backup authenticator: %w", err)
	}
	return aead, nil
}

func bundleNonce(prefix []byte, counter uint32) []byte {
	nonce := make([]byte, 12)
	copy(nonce, prefix)
	binary.BigEndian.PutUint32(nonce[8:], counter)
	return nonce
}

func bundleAAD(header []byte, counter, plainLen uint32) []byte {
	aad := make([]byte, len(header)+8)
	copy(aad, header)
	binary.BigEndian.PutUint32(aad[len(header):], counter)
	binary.BigEndian.PutUint32(aad[len(header)+4:], plainLen)
	return aad
}
