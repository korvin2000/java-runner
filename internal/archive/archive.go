// Package archive safely unpacks .zip and .tar.gz archives (no paths or links
// escaping the destination).
package archive

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Extract unpacks a zip or gzip-compressed tar archive into dest. The format
// is detected from the content, not the file name.
func Extract(src, dest string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	var magic [2]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return fmt.Errorf("%s: not an archive", src)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	switch {
	case magic == [2]byte{'P', 'K'}:
		st, err := f.Stat()
		if err != nil {
			return err
		}
		zr, err := zip.NewReader(f, st.Size())
		if err != nil {
			return err
		}
		return ExtractZip(zr, "", dest)
	case magic == [2]byte{0x1f, 0x8b}:
		gz, err := gzip.NewReader(bufio.NewReader(f))
		if err != nil {
			return err
		}
		defer gz.Close()
		return extractTar(gz, dest)
	}
	return fmt.Errorf("%s: unsupported archive format (expected .zip or .tar.gz)", src)
}

// ExtractZip extracts the entries of zr whose names start with prefix into
// dest, with the prefix removed.
func ExtractZip(zr *zip.Reader, prefix, dest string) error {
	dest = filepath.Clean(dest)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	for _, zf := range zr.File {
		rel, ok := strings.CutPrefix(zf.Name, prefix)
		if !ok || rel == "" {
			continue
		}
		target, err := safeJoin(dest, rel)
		if err == nil {
			err = checkNoLinks(dest, target)
		}
		if err != nil {
			return err
		}
		mode := zf.Mode()
		switch {
		case zf.FileInfo().IsDir():
			err = os.MkdirAll(target, 0o755)
		case mode&fs.ModeSymlink != 0:
			var link []byte
			if link, err = readZip(zf); err == nil {
				err = symlink(dest, target, string(link))
			}
		default:
			var rc io.ReadCloser
			if rc, err = zf.Open(); err == nil {
				err = writeFile(target, rc, mode.Perm())
				rc.Close()
			}
		}
		if err != nil {
			return fmt.Errorf("extracting %s: %w", zf.Name, err)
		}
	}
	return nil
}

func readZip(zf *zip.File) ([]byte, error) {
	rc, err := zf.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, 4096))
}

func extractTar(r io.Reader, dest string) error {
	dest = filepath.Clean(dest)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := safeJoin(dest, h.Name)
		if err == nil {
			err = checkNoLinks(dest, target)
		}
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(target, 0o755)
		case tar.TypeReg, '\x00':
			err = writeFile(target, tr, fs.FileMode(h.Mode).Perm())
		case tar.TypeSymlink:
			err = symlink(dest, target, h.Linkname)
		case tar.TypeLink:
			var src string
			if src, err = safeJoin(dest, h.Linkname); err == nil {
				err = checkNoLinks(dest, src)
			}
			if err == nil {
				_ = os.Remove(target)
				if err = os.Link(src, target); err != nil {
					err = copyFile(src, target)
				}
			}
		}
		if err != nil {
			return fmt.Errorf("extracting %s: %w", h.Name, err)
		}
	}
}

// safeJoin joins an archive entry name to dest, rejecting entries that would
// end up outside dest ("zip slip").
func safeJoin(dest, name string) (string, error) {
	name = filepath.FromSlash(strings.TrimPrefix(name, "./"))
	if filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return "", fmt.Errorf("unsafe path in archive: %s", name)
	}
	target := filepath.Join(dest, name)
	if rel, err := filepath.Rel(dest, target); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe path in archive: %s", name)
	}
	return target, nil
}

// checkNoLinks rejects a target whose parent directories include a symbolic
// link extracted earlier: writing through it could end up outside dest.
func checkNoLinks(dest, target string) error {
	if target == dest { // "./" entry
		return nil
	}
	rel, err := filepath.Rel(dest, filepath.Dir(target))
	if err != nil || rel == "." {
		return err
	}
	p := dest
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		p = filepath.Join(p, part)
		st, err := os.Lstat(p)
		if err != nil {
			return nil // not created yet: it becomes a real directory
		}
		if st.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("unsafe path in archive: %s is below a symbolic link", target)
		}
	}
	return nil
}

func symlink(dest, target, link string) error {
	if filepath.IsAbs(link) || filepath.VolumeName(link) != "" || !linkInside(dest, filepath.Dir(target), link) {
		return fmt.Errorf("unsafe link %s -> %s", target, link)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	_ = os.Remove(target)
	return os.Symlink(link, target)
}

// linkInside resolves link relative to dir one component at a time and
// reports whether it stays inside root. Passing through an existing symbolic
// link is refused, because a ".." after it would not resolve lexically.
func linkInside(root, dir, link string) bool {
	p := dir
	parts := strings.Split(filepath.ToSlash(link), "/")
	for i, part := range parts {
		switch part {
		case "", ".":
			continue
		case "..":
			if p == root {
				return false
			}
			p = filepath.Dir(p)
			continue
		}
		p = filepath.Join(p, part)
		if i < len(parts)-1 {
			if st, err := os.Lstat(p); err == nil && st.Mode()&fs.ModeSymlink != 0 {
				return false
			}
		}
	}
	return true
}

func writeFile(target string, r io.Reader, perm fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	_ = os.Remove(target)
	f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm|0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func copyFile(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	return writeFile(dst, f, st.Mode().Perm())
}
