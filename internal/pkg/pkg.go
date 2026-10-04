// Package pkg implements the application package: a zip archive with
//
//	jrunner.json   packaged configuration
//	app/...        application files (jar, extra files, icons)
//	runtime/...    optional bundled Java runtime (jlink image)
//
// A launcher is a jrunner executable ("stub") followed by a package and a
// 16-byte trailer: the package length (uint64, little endian) and a magic
// string. The same package format, as a standalone file, is the update
// package referenced by update.json.
package pkg

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"debug/pe"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/korvin2000/java-runner/internal/archive"
	"github.com/korvin2000/java-runner/internal/config"
	"github.com/korvin2000/java-runner/internal/fsutil"
)

const (
	magic      = "JRUNPKG1"
	trailerLen = 16

	ConfigName = "jrunner.json" // packaged configuration entry
	AppDir     = "app/"         // application files prefix
	RuntimeDir = "runtime/"     // bundled runtime prefix
)

// Package is an opened application package.
type Package struct {
	Config     *config.Config
	RawConfig  []byte
	Zip        *zip.Reader
	StubSize   int64 // bytes of executable code before the package (0 for a standalone file)
	HasApp     bool  // contains application files (an installer, not just a launcher)
	HasRuntime bool  // contains a bundled Java runtime
	f          *os.File
}

// OpenExecutable opens the package embedded in an executable. It returns
// (nil, nil) if the file carries no package.
func OpenExecutable(name string) (*Package, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	off, size, ok := findPayload(f, st.Size())
	if !ok {
		f.Close()
		return nil, nil
	}
	p, err := open(f, off, size)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: damaged application package: %w", name, err)
	}
	p.StubSize = off
	return p, nil
}

// OpenFile opens a standalone package file (an update package).
func OpenFile(name string) (*Package, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	p, err := open(f, 0, st.Size())
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: invalid package: %w", name, err)
	}
	return p, nil
}

func open(f *os.File, off, size int64) (*Package, error) {
	zr, err := zip.NewReader(io.NewSectionReader(f, off, size), size)
	if err != nil {
		return nil, err
	}
	p := &Package{Zip: zr, f: f}
	for _, zf := range zr.File {
		switch {
		case zf.Name == ConfigName:
			rc, err := zf.Open()
			if err != nil {
				return nil, err
			}
			p.RawConfig, err = io.ReadAll(io.LimitReader(rc, 1<<20))
			rc.Close()
			if err != nil {
				return nil, err
			}
		case strings.HasPrefix(zf.Name, AppDir):
			p.HasApp = true
		case strings.HasPrefix(zf.Name, RuntimeDir):
			p.HasRuntime = true
		}
	}
	if p.RawConfig == nil {
		return nil, fmt.Errorf("%s not found", ConfigName)
	}
	if p.Config, err = config.Parse(p.RawConfig, false); err != nil {
		return nil, fmt.Errorf("%s: %w", ConfigName, err)
	}
	return p, nil
}

// Close releases the underlying file.
func (p *Package) Close() error { return p.f.Close() }

// Extract unpacks all entries below prefix (AppDir or RuntimeDir) into dest.
func (p *Package) Extract(prefix, dest string) error {
	return archive.ExtractZip(p.Zip, prefix, dest)
}

// Stub returns the executable code that precedes the package.
func (p *Package) Stub() io.Reader { return io.NewSectionReader(p.f, 0, p.StubSize) }

// findPayload locates the trailer. Normally it is at the end of the file; on
// an Authenticode-signed Windows executable the certificate table follows it.
func findPayload(r io.ReaderAt, size int64) (off, n int64, ok bool) {
	ends := []int64{size}
	if cert := peCertTable(r, size); cert > 0 {
		ends = append(ends, cert)
	}
	for _, end := range ends {
		start := end - trailerLen - 8 // tolerate alignment padding
		if start < 0 {
			start = 0
		}
		buf := make([]byte, end-start)
		if _, err := r.ReadAt(buf, start); err != nil {
			continue
		}
		i := bytes.LastIndex(buf, []byte(magic))
		if i < 8 {
			continue
		}
		n = int64(binary.LittleEndian.Uint64(buf[i-8 : i]))
		off = start + int64(i) - 8 - n
		if n > 0 && off >= 0 {
			return off, n, true
		}
	}
	return 0, 0, false
}

// peCertTable returns the offset of the Authenticode certificate table if the
// file is a PE image with a table at its very end.
func peCertTable(r io.ReaderAt, size int64) int64 {
	var mz [2]byte
	if _, err := r.ReadAt(mz[:], 0); err != nil || string(mz[:]) != "MZ" {
		return 0
	}
	f, err := pe.NewFile(io.NewSectionReader(r, 0, size))
	if err != nil {
		return 0
	}
	var dd pe.DataDirectory
	switch oh := f.OptionalHeader.(type) {
	case *pe.OptionalHeader64:
		if oh.NumberOfRvaAndSizes > pe.IMAGE_DIRECTORY_ENTRY_SECURITY {
			dd = oh.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_SECURITY]
		}
	case *pe.OptionalHeader32:
		if oh.NumberOfRvaAndSizes > pe.IMAGE_DIRECTORY_ENTRY_SECURITY {
			dd = oh.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_SECURITY]
		}
	}
	if dd.VirtualAddress == 0 || int64(dd.VirtualAddress)+int64(dd.Size) != size {
		return 0
	}
	return int64(dd.VirtualAddress)
}

// Writer creates a package. It hashes everything added so that the builder
// can derive a content-based build id.
type Writer struct {
	zw   *zip.Writer
	hash hash.Hash
}

// NewWriter starts a package on w.
func NewWriter(w io.Writer) *Writer {
	return &Writer{zw: zip.NewWriter(w), hash: sha256.New()}
}

// AddFile adds the file src as entry name. Archives are stored, everything
// else is deflated.
func (w *Writer) AddFile(name, src string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	hdr := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: st.ModTime()}
	switch strings.ToLower(path.Ext(name)) {
	case ".jar", ".war", ".zip", ".gz", ".png", ".jpg", ".icns", ".jmod":
		hdr.Method = zip.Store
	}
	hdr.SetMode(st.Mode().Perm())
	zf, err := w.zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	io.WriteString(w.hash, name+"\x00")
	_, err = io.Copy(io.MultiWriter(zf, w.hash), f)
	return err
}

// AddBytes adds an in-memory file.
func (w *Writer) AddBytes(name string, data []byte) error {
	hdr := &zip.FileHeader{Name: name, Method: zip.Deflate}
	hdr.SetMode(0o644)
	zf, err := w.zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	_, err = zf.Write(data)
	return err
}

// AddTree adds all files below dir with the given entry prefix. Symbolic
// links are preserved.
func (w *Writer) AddTree(prefix, dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		name := path.Join(strings.TrimSuffix(prefix, "/"), filepath.ToSlash(rel))
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			hdr := &zip.FileHeader{Name: name}
			hdr.SetMode(fs.ModeSymlink | 0o777)
			zf, err := w.zw.CreateHeader(hdr)
			if err != nil {
				return err
			}
			io.WriteString(w.hash, name+"\x00"+link)
			_, err = io.WriteString(zf, filepath.ToSlash(link))
			return err
		case d.IsDir():
			return nil
		}
		return w.AddFile(name, p)
	})
}

// Digest returns the hash of everything added with AddFile/AddTree so far.
func (w *Writer) Digest() []byte { return w.hash.Sum(nil) }

// Close finishes the archive.
func (w *Writer) Close() error { return w.zw.Close() }

// WriteExecutable writes stub followed by the package produced by
// writePackage and the trailer to dst (atomically, mode 0755).
func WriteExecutable(dst string, stub io.Reader, writePackage func(io.Writer) error) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	err = func() error {
		if _, err := io.Copy(f, stub); err != nil {
			return err
		}
		cw := &countWriter{w: f}
		if err := writePackage(cw); err != nil {
			return err
		}
		var trailer [trailerLen]byte
		binary.LittleEndian.PutUint64(trailer[:8], uint64(cw.n))
		copy(trailer[8:], magic)
		_, err := f.Write(trailer[:])
		return err
	}()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o755)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return fsutil.ReplaceFile(tmp, dst)
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// Manifest is update.json, published next to the update package(s).
type Manifest struct {
	Version   string           `json:"version"`
	URL       string           `json:"url,omitempty"`       // package for all platforms (relative to update.json)
	SHA256    string           `json:"sha256,omitempty"`    // its SHA-256
	Notes     string           `json:"notes,omitempty"`     // release notes shown to the user
	Platforms map[string]Asset `json:"platforms,omitempty"` // per-platform packages (bundled runtimes)
}

// Asset is a downloadable package.
type Asset struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256,omitempty"`
}

// HashFile returns the hex SHA-256 of a file.
func HashFile(name string) (string, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
