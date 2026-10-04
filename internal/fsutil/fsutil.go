// Package fsutil contains small file system helpers shared by the builder and
// the launcher.
package fsutil

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// Exists reports whether p exists (without following a final symlink).
func Exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// IsFile reports whether p is a regular file (following symlinks).
func IsFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// IsDir reports whether p is a directory (following symlinks).
func IsDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// WriteFileAtomic writes data to a temporary file and renames it over path, so
// readers never observe a half-written file.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return ReplaceFile(tmp, path)
}

// ReplaceFile moves src over dst. Windows cannot overwrite a running
// executable but can rename it, so there the old file is first moved aside to
// dst+".old" (removed on a later start).
func ReplaceFile(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil || runtime.GOOS != "windows" {
		return err
	}
	old := dst + ".old"
	_ = os.Remove(old)
	if os.Rename(dst, old) != nil {
		return err
	}
	return os.Rename(src, dst)
}

// ReplaceDir swaps directory src into place at dst and removes the previous
// dst. If dst cannot be moved (files in use on Windows) nothing is changed.
func ReplaceDir(src, dst string) error {
	old := dst + ".old"
	_ = os.RemoveAll(old)
	if Exists(dst) {
		if err := os.Rename(dst, old); err != nil {
			return err
		}
	}
	if err := os.Rename(src, dst); err != nil {
		_ = os.Rename(old, dst)
		return err
	}
	_ = os.RemoveAll(old)
	return nil
}

// CopyFile copies src to dst, creating parent directories.
func CopyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// DirSize returns the total size of the regular files below dir.
func DirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}
