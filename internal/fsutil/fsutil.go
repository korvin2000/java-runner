// Package fsutil contains small file system helpers shared by the builder and
// the launcher.
package fsutil

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
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
// readers never observe a half-written file. The temporary name is unique, so
// concurrent writers (two running launchers) cannot mix their data.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, perm)
	}
	if err == nil {
		err = ReplaceFile(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// Rename is os.Rename that retries for a moment on Windows, where virus
// scanners and the search indexer often keep a new file open briefly
// ("Access is denied", sharing violation).
func Rename(src, dst string) error {
	err := os.Rename(src, dst)
	for i := 1; err != nil && i <= 10 && runtime.GOOS == "windows" && lockedByOther(err); i++ {
		time.Sleep(time.Duration(50*i) * time.Millisecond) // about 2.75 s in total
		err = os.Rename(src, dst)
	}
	return err
}

// lockedByOther reports a Windows error that typically means another process
// has the file open: ERROR_ACCESS_DENIED, ERROR_SHARING_VIOLATION or
// ERROR_LOCK_VIOLATION.
func lockedByOther(err error) bool {
	var errno syscall.Errno
	return errors.As(err, &errno) && (errno == 5 || errno == 32 || errno == 33)
}

// NotWritable reports whether err means that a location cannot be written
// (missing permission or a read-only file system).
func NotWritable(err error) bool {
	return errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EROFS)
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
	if Exists(dst) && Rename(dst, old) != nil {
		return Rename(src, dst) // not a running executable: a scanner held it
	}
	return Rename(src, dst)
}

// ReplaceDir swaps directory src into place at dst and removes the previous
// dst. If dst cannot be moved (files in use on Windows) nothing is changed.
// A crash between the two renames leaves only dst+".old", which the launcher
// moves back on its next start.
func ReplaceDir(src, dst string) error {
	old := dst + ".old"
	_ = os.RemoveAll(old)
	if Exists(dst) {
		if err := Rename(dst, old); err != nil {
			return err
		}
	}
	if err := Rename(src, dst); err != nil {
		_ = Rename(old, dst)
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

// CopyFileAtomic copies src to dst through a temporary file and ReplaceFile,
// so dst is never left half-written and may be a running executable.
func CopyFileAtomic(src, dst string, perm fs.FileMode) error {
	tmp := dst + ".tmp"
	err := CopyFile(src, tmp, perm)
	if err == nil {
		err = os.Chmod(tmp, perm)
	}
	if err == nil {
		err = ReplaceFile(tmp, dst)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
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
