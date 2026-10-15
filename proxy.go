package main

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Proxy is one parsed proxy endpoint.
type Proxy struct {
	URL *url.URL
	Raw string // original line from proxies.txt
}

// LoadProxies reads proxies.txt: one proxy per line, # comments and blank lines
// ignored. Lines are auto-detected in this order:
//  1. contains "://"  -> parsed as-is (http, https, socks5)
//  2. host:port:user:pass   -> http://user:pass@host:port
//  3. user:pass@host:port   -> http://user:pass@host:port
//  4. host:port             -> http://host:port
//
// Malformed lines fail fast with the line number.
func LoadProxies(path string) ([]Proxy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Proxy
	for i, line := range strings.Split(string(data), "\n") {
		lineNo := i + 1
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p, err := parseProxyLine(line)
		if err != nil {
			return nil, fmt.Errorf("proxies.txt line %d: %w", lineNo, err)
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("proxies.txt contains no proxies")
	}
	return out, nil
}

func parseProxyLine(line string) (Proxy, error) {
	if strings.Contains(line, "://") {
		u, err := url.Parse(line)
		if err != nil {
			return Proxy{}, fmt.Errorf("bad URL %q: %w", line, err)
		}
		switch u.Scheme {
		case "http", "https", "socks5":
		default:
			return Proxy{}, fmt.Errorf("unsupported scheme %q in %q", u.Scheme, line)
		}
		if u.Port() == "" {
			return Proxy{}, fmt.Errorf("missing port in %q", line)
		}
		return Proxy{URL: u, Raw: line}, nil
	}

	if strings.Contains(line, "@") {
		// user:pass@host:port
		u, err := url.Parse("http://" + line)
		if err != nil {
			return Proxy{}, fmt.Errorf("bad proxy %q: %w", line, err)
		}
		if u.Port() == "" {
			return Proxy{}, fmt.Errorf("missing port in %q", line)
		}
		return Proxy{URL: u, Raw: line}, nil
	}

	parts := strings.Split(line, ":")
	switch len(parts) {
	case 2: // host:port
		if err := validHostPort(parts[0], parts[1]); err != nil {
			return Proxy{}, err
		}
		return Proxy{URL: &url.URL{Scheme: "http", Host: net.JoinHostPort(parts[0], parts[1])}, Raw: line}, nil
	case 4: // host:port:user:pass
		host, port, user, pass := parts[0], parts[1], parts[2], parts[3]
		if err := validHostPort(host, port); err != nil {
			return Proxy{}, err
		}
		u := &url.URL{Scheme: "http", Host: net.JoinHostPort(host, port), User: url.UserPassword(user, pass)}
		return Proxy{URL: u, Raw: line}, nil
	default:
		return Proxy{}, fmt.Errorf("malformed proxy %q (want host:port[:user:pass] or a URL)", line)
	}
}

func validHostPort(host, port string) error {
	if host == "" {
		return fmt.Errorf("missing host")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("bad port %q", port)
	}
	return nil
}

// proxyLabel is the display form of a proxy: host:port without credentials.
func proxyLabel(p *Proxy) string {
	if p == nil {
		return ""
	}
	return p.URL.Host
}

// NewClient builds an HTTP client for the given proxy (nil = direct). The
// transport natively supports the socks5 scheme.
func NewClient(p *Proxy, timeout time.Duration) *http.Client {
	tr := &http.Transport{
		ForceAttemptHTTP2: true,
	}
	if p != nil {
		tr.Proxy = http.ProxyURL(p.URL)
	}
	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
	}
}

// clientFor returns a shared HTTP client per proxy label so TCP/TLS
// connections are pooled across files instead of paying a fresh handshake per
// check. Selection semantics are unchanged: sticky keeps one endpoint per file,
// rotating still round-robins per request — each endpoint just reuses its
// pooled client.
var (
	clientCacheMu sync.Mutex
	clientCache   = map[string]*http.Client{}
)

func clientFor(p *Proxy, timeout time.Duration) *http.Client {
	key := proxyLabel(p) + "|" + timeout.String()
	clientCacheMu.Lock()
	defer clientCacheMu.Unlock()
	if c, ok := clientCache[key]; ok {
		return c
	}
	c := NewClient(p, timeout)
	clientCache[key] = c
	return c
}
