// Package securefile owns the small local-file boundary used for credentials.
package securefile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// PrepareDir creates or normalizes an application-owned private directory.
func PrepareDir(path string) error {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) || path == string(filepath.Separator) {
		return errors.New("private directory must be a safe absolute path")
	}
	if err := rejectSymlinkComponents(path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create private directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("private directory is not a real directory")
	}
	if err := requireOwner(info); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("set private directory mode: %w", err)
	}
	return nil
}

// Read reads a bounded, owner-only regular file without following symlinks.
func Read(path string, maxBytes int64) ([]byte, error) {
	f, err := Open(path, maxBytes)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, errors.New("inspect opened private file")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read private file: %w", err)
	}
	if int64(len(data)) > maxBytes || int64(len(data)) != info.Size() {
		return nil, errors.New("private file changed or exceeded size limit")
	}
	return data, nil
}

// Open returns a bounded, owner-only regular file without following symlinks.
// The caller must close the returned file.
func Open(path string, maxBytes int64) (*os.File, error) {
	info, err := validateFile(path, maxBytes)
	if err != nil {
		return nil, err
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open private file: %w", err)
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = syscall.Close(fd)
		return nil, errors.New("open private file")
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = f.Close()
		return nil, errors.New("private file changed during open")
	}
	return f, nil
}

// OpenLock opens or creates an owner-only regular lock file without following
// symlinks. Callers still own the advisory-lock operation and must close it.
func OpenLock(path string) (*os.File, error) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return nil, errors.New("private lock path must be absolute")
	}
	if err := validateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, errors.New("open private lock")
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = syscall.Close(fd)
		return nil, errors.New("open private lock")
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || requireOwner(info) != nil || info.Mode().Perm()&0o077 != 0 {
		_ = f.Close()
		return nil, errors.New("private lock is unsafe")
	}
	pathInfo, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, pathInfo) {
		_ = f.Close()
		return nil, errors.New("private lock changed while opening")
	}
	return f, nil
}

// AtomicWrite durably replaces an owner-only regular file without following
// an existing symlink or other special file at the destination.
func AtomicWrite(path string, data []byte) error {
	dir := filepath.Dir(filepath.Clean(path))
	if err := validateDir(dir); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("private destination is not a regular file")
		}
		if err := requireOwner(info); err != nil {
			return err
		}
		if info.Mode().Perm()&0o077 != 0 {
			return errors.New("private destination is group/world accessible")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect private destination: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".darkharrbor-private-*")
	if err != nil {
		return fmt.Errorf("create private temporary file: %w", err)
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
		return fmt.Errorf("protect private temporary file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write private temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync private temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close private temporary file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace private file: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	ok = true
	return nil
}

func validateFile(path string, maxBytes int64) (os.FileInfo, error) {
	if maxBytes < 1 || !filepath.IsAbs(filepath.Clean(path)) {
		return nil, errors.New("private file path or size limit is invalid")
	}
	if err := validateDir(filepath.Dir(filepath.Clean(path))); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect private file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("private file is not a regular file")
	}
	if err := requireOwner(info); err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("private file is group/world accessible")
	}
	if info.Size() <= 0 || info.Size() > maxBytes {
		return nil, errors.New("private file is empty or too large")
	}
	return info, nil
}

func validateDir(path string) error {
	if err := rejectSymlinkComponents(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("private parent is not a real directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (int(stat.Uid) != os.Geteuid() && stat.Uid != 0) {
		return errors.New("private parent has unexpected owner")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New("private parent is group/world writable")
	}
	return nil
}

func requireOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return errors.New("private path has unexpected owner")
	}
	return nil
}

func rejectSymlinkComponents(path string) error {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return errors.New("private path must be absolute")
	}
	current := string(filepath.Separator)
	for _, part := range splitPath(path) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect private path: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("private path contains a symlink")
		}
	}
	return nil
}

func splitPath(path string) []string {
	var parts []string
	for path != string(filepath.Separator) && path != "." {
		dir, base := filepath.Split(path)
		if base != "" {
			parts = append([]string{base}, parts...)
		}
		path = filepath.Clean(dir)
	}
	return parts
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open private directory: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync private directory: %w", err)
	}
	return nil
}
