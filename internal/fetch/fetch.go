// Package fetch downloads JSON documents and files over HTTP(S) with sane
// timeouts, a stall watchdog, progress output, retries and SHA-256
// verification. Proxies are taken from HTTPS_PROXY/HTTP_PROXY/NO_PROXY.
package fetch

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/korvin2000/java-runner/internal/ui"
	"github.com/korvin2000/java-runner/internal/version"
)

// stallTimeout aborts a download that receives no data for this long.
const stallTimeout = 60 * time.Second

// ErrChecksum is returned when a downloaded file does not match its checksum.
var ErrChecksum = errors.New("checksum mismatch")

var client = &http.Client{Transport: &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	TLSHandshakeTimeout:   15 * time.Second,
	ResponseHeaderTimeout: 30 * time.Second,
	IdleConnTimeout:       30 * time.Second,
	ForceAttemptHTTP2:     true,
}}

type statusError struct {
	url    string
	status int
	text   string
}

func (e *statusError) Error() string { return fmt.Sprintf("GET %s: %s", e.url, e.text) }

func get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "jrunner/"+version.Tool)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, &statusError{url: url, status: resp.StatusCode, text: resp.Status}
	}
	return resp, nil
}

// JSON fetches url and decodes the JSON response into v.
func JSON(ctx context.Context, url string, v any) error {
	resp, err := get(ctx, url)
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
	resp, err := get(ctx, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(b), err
}

// File downloads url to dest with a progress bar. If want (a hex SHA-256 or,
// for vendors that publish nothing better, SHA-1) is not empty the content is
// verified. Transient failures are retried once.
func File(ctx context.Context, url, dest, want string) error {
	var err error
	for attempt := 1; attempt <= 2; attempt++ {
		if attempt > 1 {
			ui.Warn("download failed (%v), retrying", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
		err = download(ctx, url, dest, strings.TrimSpace(want))
		var se *statusError
		if err == nil || errors.Is(err, ErrChecksum) || ctx.Err() != nil ||
			(errors.As(err, &se) && se.status >= 400 && se.status < 500) {
			return err
		}
	}
	return err
}

func download(parent context.Context, url, dest, want string) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	resp, err := get(ctx, url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	var stalled atomic.Bool
	watchdog := time.AfterFunc(stallTimeout, func() { stalled.Store(true); cancel() })
	defer watchdog.Stop()

	h := sha256.New()
	if len(want) == 40 {
		h = sha1.New()
	}
	bar := ui.NewBar(resp.ContentLength)
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
		os.Remove(tmp)
		if stalled.Load() {
			return fmt.Errorf("download stalled: no data received for %s", stallTimeout)
		}
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); want != "" && !strings.EqualFold(got, want) {
		os.Remove(tmp)
		return fmt.Errorf("%w for %s: expected %s, got %s", ErrChecksum, url, want, got)
	}
	return os.Rename(tmp, dest)
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }
