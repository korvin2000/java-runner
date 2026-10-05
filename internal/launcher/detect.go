package launcher

import (
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
)

// URL detection for web applications without a configured browser.url: the
// application's output is watched for the address it listens on, e.g.
//
//	Tomcat started on port 8080 (http) with context path '/'        (Spring Boot)
//	Netty started on port 8080                                      (WebFlux)
//	ASTROLABE Studio is ready: http://127.0.0.1:8740/launch?t=...   (custom)
//	➜  started on Local:   http://localhost:4200/                   (Vite, Angular)
var (
	ansiRe    = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	urlRe     = regexp.MustCompile(`https?://[^\s'"<>()\[\]]+`)
	portRe    = regexp.MustCompile(`(?i)\b(?:started|listening|running|available) on (?:port(?:\(s\))?|address)s?:?\s*(?:[\w.:\[\]]*:)?(\d{2,5})\b(?:\s*\(([\w/.]+)\))?`)
	contextRe = regexp.MustCompile(`context path ['"]([^'"]*)['"]`)
)

// detectURL returns the local URL announced in one line of output, or "".
func detectURL(line string) string {
	line = ansiRe.ReplaceAllString(line, "")
	for _, m := range urlRe.FindAllString(line, -1) {
		m = strings.TrimRight(m, ".,;:!?*")
		u, err := url.Parse(m)
		if err != nil || !isLocalHost(u.Hostname()) {
			continue
		}
		return localURL(u)
	}
	if m := portRe.FindStringSubmatch(line); m != nil {
		scheme := "http"
		if strings.HasPrefix(strings.ToLower(m[2]), "https") {
			scheme = "https"
		}
		path := "/"
		if c := contextRe.FindStringSubmatch(line); c != nil && c[1] != "" && c[1] != "/" {
			path = "/" + strings.Trim(c[1], "/") + "/"
		}
		return scheme + "://localhost:" + m[1] + path
	}
	return ""
}

func isLocalHost(h string) bool {
	h = strings.ToLower(h)
	switch {
	case h == "localhost", h == "0.0.0.0", h == "::", h == "::1", h == "0:0:0:0:0:0:0:1",
		strings.HasPrefix(h, "127."), strings.HasSuffix(h, ".localhost"), strings.HasSuffix(h, ".local"):
		return true
	}
	if name, err := os.Hostname(); err == nil && strings.EqualFold(name, h) {
		return true
	}
	return false
}

// localURL rewrites wildcard addresses to localhost.
func localURL(u *url.URL) string {
	switch h := u.Hostname(); h {
	case "0.0.0.0", "::", "":
		port := u.Port()
		u.Host = "localhost"
		if port != "" {
			u.Host += ":" + port
		}
	}
	return u.String()
}

// watchOutput copies r to w and calls found with the first local URL seen in
// the stream. Output is forwarded immediately, so nothing is delayed or lost
// even for very long lines (only the first 8 KB of a line are inspected).
func watchOutput(r io.Reader, w io.Writer, found func(string), wg *sync.WaitGroup) {
	defer wg.Done()
	buf := make([]byte, 32*1024)
	var line []byte
	done := false
	for {
		n, err := r.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
			if !done {
				for _, b := range buf[:n] {
					if b == '\n' {
						if u := detectURL(string(line)); u != "" {
							found(u)
							done = true
							break
						}
						line = line[:0]
					} else if len(line) < 8192 {
						line = append(line, b)
					}
				}
			}
		}
		if err != nil {
			return
		}
	}
}
