package proxy

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
}

// LoadProxies reads proxies.txt: one proxy per line, # comments and blank lines
// ignored. Lines are auto-detected in this order:
//  1. contains "://"        -> parsed as-is (http, https, socks5)
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
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p, err := parseProxyLine(line)
		if err != nil {
			return nil, fmt.Errorf("%s: line %d: %w", path, i+1, err)
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no proxies found", path)
	}
	return out, nil
}

func parseProxyLine(line string) (Proxy, error) {
	if strings.Contains(line, "://") {
		u, err := url.Parse(line)
		if err != nil {
			return Proxy{}, err
		}
		switch u.Scheme {
		case "http", "https", "socks5":
		default:
			return Proxy{}, fmt.Errorf("unsupported scheme %q (want http, https, socks5)", u.Scheme)
		}
		if u.Port() == "" {
			return Proxy{}, fmt.Errorf("missing port in %q", line)
		}
		return Proxy{URL: u}, nil
	}

	parts := strings.Split(line, ":")
	switch len(parts) {
	case 2:
		// host:port
		host, port := parts[0], parts[1]
		if err := validHostPort(host, port); err != nil {
			return Proxy{}, err
		}
		u, _ := url.Parse("http://" + net.JoinHostPort(host, port))
		return Proxy{URL: u}, nil

	case 4:
		// host:port:user:pass
		host, port, user, pass := parts[0], parts[1], parts[2], parts[3]
		if err := validHostPort(host, port); err != nil {
			return Proxy{}, err
		}
		u := &url.URL{
			Scheme: "http",
			User:   url.UserPassword(user, pass),
			Host:   net.JoinHostPort(host, port),
		}
		return Proxy{URL: u}, nil

	default:
		// user:pass@host:port (colon-split has len 3 because user:pass has one, host:port has one)
		if strings.Contains(line, "@") {
			u, err := url.Parse("http://" + line)
			if err != nil {
				return Proxy{}, err
			}
			if u.Port() == "" {
				return Proxy{}, fmt.Errorf("missing port in %q", line)
			}
			return Proxy{URL: u}, nil
		}
		return Proxy{}, fmt.Errorf("invalid proxy format %q", line)
	}
}

func validHostPort(host, port string) error {
	if host == "" {
		return fmt.Errorf("empty host")
	}
	p, err := strconv.Atoi(port)
	if err != nil || p <= 0 || p > 65535 {
		return fmt.Errorf("invalid port %q", port)
	}
	return nil
}

// ProxyLabel is the display form of a proxy: host:port without credentials.
func ProxyLabel(p *Proxy) string {
	if p == nil || p.URL == nil {
		return ""
	}
	return p.URL.Host
}

// NewClient builds an HTTP client for the given proxy (nil = direct). The
// transport natively supports the socks5 scheme.
func NewClient(p *Proxy, timeout time.Duration) *http.Client {
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	if p != nil && p.URL != nil {
		tr.Proxy = http.ProxyURL(p.URL)
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

// ClientFor returns a shared HTTP client per proxy label so TCP/TLS
// connections are pooled across files instead of paying a fresh handshake per
// check. Selection semantics are unchanged: sticky keeps one endpoint per file,
// rotating still round-robins per request — each endpoint just reuses its
// pooled client.
var (
	poolMu  sync.Mutex
	clients = map[string]*http.Client{}
)

func ClientFor(p *Proxy, timeout time.Duration) *http.Client {
	key := fmt.Sprintf("%s|%v", ProxyLabel(p), timeout)
	poolMu.Lock()
	defer poolMu.Unlock()
	if c, ok := clients[key]; ok {
		return c
	}
	c := NewClient(p, timeout)
	clients[key] = c
	return c
}
