package platform

import (
	"net"
	"net/url"
	"path"
	"strings"
)

// Proxy is the operating system's proxy configuration: proxy addresses for
// http and https URLs and the hosts that are reached directly. Automatic
// configuration scripts (PAC/WPAD) are not supported.
type Proxy struct {
	HTTP, HTTPS string
	Bypass      []string
}

// For returns the proxy for a request URL, or nil for a direct connection.
func (p Proxy) For(u *url.URL) (*url.URL, error) {
	server := p.HTTP
	if u.Scheme == "https" {
		server = p.HTTPS
	}
	if server == "" || p.bypassed(u.Hostname()) {
		return nil, nil
	}
	if !strings.Contains(server, "://") {
		server = "http://" + server
	}
	return url.Parse(server)
}

func (p Proxy) bypassed(host string) bool {
	host = strings.ToLower(host)
	ip := net.ParseIP(host)
	if host == "localhost" || (ip != nil && ip.IsLoopback()) {
		return true
	}
	for _, e := range p.Bypass {
		e = strings.ToLower(strings.TrimSpace(e))
		switch {
		case e == "":
		case e == "<local>": // Windows: names without a dot
			if ip == nil && !strings.Contains(host, ".") {
				return true
			}
		case strings.Contains(e, "/"): // macOS: CIDR, also abbreviated such as 169.254/16
			addr, bits, _ := strings.Cut(e, "/")
			for !strings.Contains(addr, ":") && strings.Count(addr, ".") < 3 {
				addr += ".0"
			}
			if _, n, err := net.ParseCIDR(addr + "/" + bits); err == nil && ip != nil && n.Contains(ip) {
				return true
			}
		case strings.HasPrefix(e, "."):
			if strings.HasSuffix(host, e) || host == e[1:] {
				return true
			}
		default:
			if ok, _ := path.Match(e, host); ok {
				return true
			}
		}
	}
	return false
}

// parseWinProxy parses the ProxyServer ("host:port" or
// "http=host:port;https=host:port") and ProxyOverride values of Windows'
// Internet Options.
func parseWinProxy(server, override string) Proxy {
	var p Proxy
	for _, part := range strings.Split(server, ";") {
		part = strings.TrimSpace(part)
		if k, v, ok := strings.Cut(part, "="); ok {
			switch strings.ToLower(strings.TrimSpace(k)) {
			case "http":
				p.HTTP = strings.TrimSpace(v)
			case "https":
				p.HTTPS = strings.TrimSpace(v)
			}
		} else if part != "" {
			p.HTTP, p.HTTPS = part, part
		}
	}
	for _, e := range strings.Split(override, ";") {
		if e = strings.TrimSpace(e); e != "" {
			p.Bypass = append(p.Bypass, e)
		}
	}
	return p
}

// parseScutil parses the output of macOS' "scutil --proxy".
func parseScutil(out string) Proxy {
	var p Proxy
	kv := map[string]string{}
	inExceptions := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "ExceptionsList"):
			inExceptions = true
			continue
		case line == "}":
			inExceptions = false
			continue
		}
		k, v, ok := strings.Cut(line, " : ")
		if !ok {
			continue
		}
		if inExceptions {
			p.Bypass = append(p.Bypass, strings.TrimSpace(v))
		} else {
			kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	server := func(prefix string) string {
		if kv[prefix+"Enable"] != "1" || kv[prefix+"Proxy"] == "" {
			return ""
		}
		if port := kv[prefix+"Port"]; port != "" {
			return net.JoinHostPort(kv[prefix+"Proxy"], port)
		}
		return kv[prefix+"Proxy"]
	}
	p.HTTP, p.HTTPS = server("HTTP"), server("HTTPS")
	return p
}
