// Package fetch downloads JSON documents and files over HTTP(S) with sane
// timeouts, a stall watchdog, progress output, retries, resumable downloads
// and checksum verification. Proxies are taken from HTTPS_PROXY/HTTP_PROXY/
// NO_PROXY or, when none of these is set, from the operating system.
package fetch

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/korvin2000/java-runner/internal/fsutil"
	"github.com/korvin2000/java-runner/internal/platform"
	"github.com/korvin2000/java-runner/internal/ui"
	"github.com/korvin2000/java-runner/internal/version"
)

const (
	stallTimeout = 60 * time.Second // abort a download that receives no data for this long
	fileAttempts = 4                // tries per file download (each one resumes the previous)
)

// ErrChecksum is returned when a downloaded file does not match its checksum.
var ErrChecksum = errors.New("checksum mismatch")

// errRestart: a partial download could not be resumed; the next attempt
// starts from the beginning.
var errRestart = errors.New("the partial download cannot be resumed, starting again")

var client = &http.Client{Transport: &http.Transport{
	Proxy:                 proxy,
	DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	TLSHandshakeTimeout:   15 * time.Second,
	ResponseHeaderTimeout: 30 * time.Second,
	IdleConnTimeout:       30 * time.Second,
	ForceAttemptHTTP2:     true,
}}

var (
	envProxy = sync.OnceValue(func() bool {
		for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy"} {
			if os.Getenv(k) != "" {
				return true
			}
		}
		return false
	})
	systemProxy = sync.OnceValue(platform.SystemProxy)
)

// proxy uses the proxy environment variables when any is set, otherwise the
// operating system's settings (Windows Internet Options, macOS network
// settings).
func proxy(req *http.Request) (*url.URL, error) {
	if envProxy() {
		return http.ProxyFromEnvironment(req)
	}
	return systemProxy().For(req.URL)
}

type statusError struct {
	url        string
	status     int
	text       string
	retryAfter time.Duration
}

func (e *statusError) Error() string { return fmt.Sprintf("GET %s: %s", e.url, e.text) }

// get sends a GET request. Responses other than 200 (or 206 for a range
// request) are returned as *statusError.
func get(ctx context.Context, url string, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("User-Agent", "jrunner/"+version.Tool)
	resp, err := client.Do(req)
	if err != nil {
		return nil, explain(err)
	}
	if resp.StatusCode == http.StatusOK || (resp.StatusCode == http.StatusPartialContent && req.Header.Get("Range") != "") {
		return resp, nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) // lets the connection be reused
	resp.Body.Close()
	return nil, &statusError{url: url, status: resp.StatusCode, text: resp.Status, retryAfter: retryAfter(resp.Header.Get("Retry-After"))}
}

// explain adds a hint to errors whose usual cause is not obvious.
func explain(err error) error {
	var dns *net.DNSError
	var ca x509.UnknownAuthorityError
	switch {
	case errors.As(err, &dns) && dns.IsNotFound:
		return fmt.Errorf("%w (no internet connection, or a proxy is required: set HTTPS_PROXY)", err)
	case errors.As(err, &ca):
		return fmt.Errorf("%w (is a proxy inspecting HTTPS? its certificate must be trusted by this computer)", err)
	}
	return err
}

// transient reports whether a failed request may succeed when repeated.
func transient(err error) bool {
	var se *statusError
	var dns *net.DNSError
	var ca x509.UnknownAuthorityError
	var pe *fs.PathError
	switch {
	case errors.As(err, &se):
		return se.status >= 500 || se.status == http.StatusRequestTimeout || se.status == http.StatusTooManyRequests
	case errors.As(err, &dns):
		return !dns.IsNotFound
	case errors.As(err, &ca), errors.Is(err, ErrChecksum):
		return false
	case errors.As(err, &pe):
		return false // local file problem: disk full, no permission
	}
	return true // network errors, timeouts, truncated responses, stalls
}

func retryAfter(v string) time.Duration {
	var d time.Duration
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
		d = time.Duration(n) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = time.Until(t)
	}
	return min(max(d, 0), time.Minute)
}

// backoff is the delay after the n-th failed attempt: 1 s, 4 s, 9 s, or what
// the server asked for.
func backoff(n int, err error) time.Duration {
	var se *statusError
	if errors.As(err, &se) && se.retryAfter > 0 {
		return se.retryAfter
	}
	return time.Duration(n*n) * time.Second
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// getRetry is get with up to three attempts for transient failures, within
// the deadline of ctx. The pauses are short (0.5 s, 1 s): these small
// requests run before the application starts.
func getRetry(ctx context.Context, url string) (*http.Response, error) {
	var err error
	for n := 1; ; n++ {
		var resp *http.Response
		if resp, err = get(ctx, url, nil); err == nil || n == 3 || !transient(err) {
			return resp, err
		}
		d := time.Duration(n) * 500 * time.Millisecond
		var se *statusError
		if errors.As(err, &se) && se.retryAfter > 0 {
			d = se.retryAfter
		}
		if !sleep(ctx, d) {
			return resp, err
		}
		ui.Debug("GET %s failed (%v), retrying", url, err)
	}
}

// JSON fetches url and decodes the JSON response into v.
func JSON(ctx context.Context, url string, v any) error {
	resp, err := getRetry(ctx, url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(v); err != nil {
		return fmt.Errorf("%s: invalid JSON: %w", url, err)
	}
	return nil
}

// Text fetches a small text document.
func Text(ctx context.Context, url string) (string, error) {
	resp, err := getRetry(ctx, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(b), err
}

// File downloads url to dest with a progress bar. If want (a hex SHA-256 or,
// for vendors that publish nothing better, SHA-1) is not empty the content is
// verified, and an existing dest with that checksum is used as it is.
//
// The data is written to dest+".part". When a transfer breaks off, the next
// attempt (or a later process) continues it with an HTTP range request, as
// long as the server identifies the content (ETag/Last-Modified) or the
// checksum is known. Transient failures are retried with backoff; a checksum
// mismatch is retried once from scratch.
func File(ctx context.Context, url, dest, want string) error {
	want = strings.ToLower(strings.TrimSpace(want))
	if want != "" {
		if got, err := hashFile(dest, want); err == nil && got == want {
			ui.Info("already downloaded, checksum verified")
			return nil
		}
	}
	var err error
	fresh, recheck := false, true
	for n := 1; n <= fileAttempts; n++ {
		if n > 1 {
			d := backoff(n-1, err)
			ui.Warn("download failed (%v), retrying in %s", err, ui.Duration(d))
			if !sleep(ctx, d) {
				return ctx.Err()
			}
		}
		err = download(ctx, url, dest, want, fresh)
		fresh = false
		switch {
		case err == nil:
			return nil
		case ctx.Err() != nil:
			return err
		case errors.Is(err, ErrChecksum):
			if !recheck {
				return err
			}
			recheck, fresh = false, true // corrupted in transit or a bad partial file
		case !transient(err):
			return err
		}
	}
	return err
}

// partMeta identifies the content of a partial download, so that it is only
// resumed with the same file.
type partMeta struct {
	URL          string `json:"url"`
	Want         string `json:"sha,omitempty"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
}

func (m partMeta) resumable() bool { return m.Want != "" || m.ETag != "" || m.LastModified != "" }

// validator is the If-Range value: a strong ETag or the modification date.
func (m partMeta) validator() string {
	if m.ETag != "" {
		return m.ETag
	}
	return m.LastModified
}

func download(parent context.Context, url, dest, want string, fresh bool) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	part, metaFile := dest+".part", dest+".part.meta"
	var offset int64
	var meta partMeta
	if !fresh {
		offset, meta = resumePoint(part, metaFile, url, want)
	}
	if offset == 0 {
		removePart(dest)
	}
	// Byte offsets must refer to the stored bytes: no transparent compression.
	header := http.Header{"Accept-Encoding": {"identity"}}
	if offset > 0 {
		header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		if v := meta.validator(); v != "" {
			header.Set("If-Range", v)
		}
	}
	resp, err := get(ctx, url, header)
	var se *statusError
	if offset > 0 && errors.As(err, &se) && se.status == http.StatusRequestedRangeNotSatisfiable {
		// Nothing left to fetch: the part may already be complete.
		if want != "" {
			if got, err := hashFile(part, want); err == nil && got == want {
				os.Remove(metaFile)
				return fsutil.Rename(part, dest)
			}
		}
		removePart(dest)
		return errRestart
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	h := newHash(want)
	total := resp.ContentLength
	var f *os.File
	if resp.StatusCode == http.StatusPartialContent {
		if rangeStart(resp.Header.Get("Content-Range")) != offset {
			removePart(dest)
			return errRestart
		}
		// Hash what is already there, then append.
		if f, err = os.OpenFile(part, os.O_RDWR, 0); err == nil {
			if _, err = io.CopyN(h, f, offset); err == nil {
				_, err = f.Seek(offset, io.SeekStart)
			}
		}
		if total >= 0 {
			total += offset
		}
		ui.Info("resuming the download at %s", ui.Size(offset))
	} else {
		// A complete response: the server ignored the range or the content changed.
		offset = 0
		meta = partMeta{URL: url, Want: want, LastModified: resp.Header.Get("Last-Modified")}
		if etag := resp.Header.Get("ETag"); !strings.HasPrefix(etag, "W/") {
			meta.ETag = etag
		}
		if f, err = os.Create(part); err == nil && meta.resumable() {
			err = writeMeta(metaFile, meta)
		}
	}
	if err != nil {
		if f != nil {
			f.Close()
		}
		removePart(dest)
		return err
	}

	var stalled atomic.Bool
	watchdog := time.AfterFunc(stallTimeout, func() { stalled.Store(true); cancel() })
	defer watchdog.Stop()
	bar := ui.NewBar(total, offset)
	body := readerFunc(func(p []byte) (int, error) {
		n, err := resp.Body.Read(p)
		if n > 0 {
			watchdog.Reset(stallTimeout)
		}
		return n, err
	})
	_, err = io.Copy(io.MultiWriter(f, h, bar), body)
	bar.Finish(err == nil)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		if !meta.resumable() {
			removePart(dest)
		} // otherwise the next attempt continues where this one stopped
		if stalled.Load() && parent.Err() == nil {
			return fmt.Errorf("download stalled: no data received for %s", stallTimeout)
		}
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); want != "" && got != want {
		removePart(dest)
		return fmt.Errorf("%w for %s: expected %s, got %s", ErrChecksum, url, want, got)
	}
	os.Remove(metaFile)
	return fsutil.Rename(part, dest)
}

// resumePoint returns the size of a partial download that belongs to url and
// want and can be resumed, or 0.
func resumePoint(part, metaFile, url, want string) (int64, partMeta) {
	st, err := os.Stat(part)
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
		return 0, partMeta{}
	}
	var m partMeta
	data, err := os.ReadFile(metaFile)
	if err != nil || json.Unmarshal(data, &m) != nil || m.URL != url || m.Want != want || !m.resumable() {
		return 0, partMeta{}
	}
	return st.Size(), m
}

func writeMeta(name string, m partMeta) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(name, data, 0o644)
}

func removePart(dest string) {
	os.Remove(dest + ".part")
	os.Remove(dest + ".part.meta")
}

// rangeStart returns the first byte of a "bytes first-last/total"
// Content-Range header, or -1.
func rangeStart(v string) int64 {
	r, ok := strings.CutPrefix(strings.TrimSpace(v), "bytes ")
	if !ok {
		return -1
	}
	first, _, ok := strings.Cut(r, "-")
	n, err := strconv.ParseInt(first, 10, 64)
	if !ok || err != nil {
		return -1
	}
	return n
}

// newHash returns SHA-1 for 40-digit checksums, otherwise SHA-256.
func newHash(want string) hash.Hash {
	if len(want) == 40 {
		return sha1.New()
	}
	return sha256.New()
}

// hashFile returns the hex digest of a file, using the algorithm that
// matches want.
func hashFile(name, want string) (string, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := newHash(want)
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }
