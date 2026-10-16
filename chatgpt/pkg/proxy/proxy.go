package proxy

import (
	"context"
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

// DefaultHealthCheckURL is used to verify candidate proxies.
const DefaultHealthCheckURL = "https://httpbin.org/ip"

// Proxy is one parsed proxy endpoint.
type Proxy struct {
	URL *url.URL
}

// LoadProxies loads proxies from a file path or parses a single proxy string.
// Supported line formats:
//  1. Contains "://"      -> parsed directly (http, https, socks5)
//  2. host:port:user:pass -> http://user:pass@host:port
//  3. user:pass@host:port -> http://user:pass@host:port
//  4. host:port           -> http://host:port
func LoadProxies(pathOrInput string) ([]Proxy, error) {
	pathOrInput = strings.TrimSpace(pathOrInput)
	if pathOrInput == "" {
		return nil, fmt.Errorf("empty proxy input")
	}

	// Check if path exists as a regular file
	if fi, err := os.Stat(pathOrInput); err == nil && !fi.IsDir() {
		data, err := os.ReadFile(pathOrInput)
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
				return nil, fmt.Errorf("%s: line %d: %w", pathOrInput, i+1, err)
			}
			out = append(out, p)
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("%s: no proxies found", pathOrInput)
		}
		return out, nil
	}

	// Treat as a direct proxy candidate string
	p, err := parseProxyLine(pathOrInput)
	if err != nil {
		return nil, err
	}
	return []Proxy{p}, nil
}

func parseProxyLine(line string) (Proxy, error) {
	line = strings.TrimSpace(line)
	if strings.Contains(line, "://") {
		u, err := url.Parse(line)
		if err != nil {
			return Proxy{}, err
		}
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h":
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
		// user:pass@host:port
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

// ProxyLabel returns host:port without credentials for display.
func ProxyLabel(p *Proxy) string {
	if p == nil || p.URL == nil {
		return ""
	}
	return p.URL.Host
}

// NewClient builds an HTTP client for the given proxy (nil = direct connection).
func NewClient(p *Proxy, timeout time.Duration) *http.Client {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSHandshakeTimeout:   10 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	if p != nil && p.URL != nil {
		tr.Proxy = http.ProxyURL(p.URL)
	}
	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
	}
}

var (
	poolMu  sync.Mutex
	clients = map[string]*http.Client{}
)

// ClientFor returns a pooled HTTP client per proxy and timeout, reusing connections.
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

// CheckSingleProxy sends a lightweight probe to verify proxy connectivity.
func CheckSingleProxy(ctx context.Context, p *Proxy, timeout time.Duration, healthURL string) bool {
	if healthURL == "" {
		healthURL = DefaultHealthCheckURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	client := ClientFor(p, timeout)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// VerifyProxies checks candidate proxies concurrently and returns alive ones.
func VerifyProxies(ctx context.Context, proxies []Proxy, maxWorkers int, timeout time.Duration, healthURL string) []Proxy {
	if len(proxies) == 0 {
		return nil
	}
	if maxWorkers < 1 {
		maxWorkers = 16
	}
	if maxWorkers > len(proxies) {
		maxWorkers = len(proxies)
	}

	jobs := make(chan Proxy, len(proxies))
	results := make(chan Proxy, len(proxies))

	var wg sync.WaitGroup
	for range maxWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				if CheckSingleProxy(ctx, &p, timeout, healthURL) {
					results <- p
				}
			}
		}()
	}

	for _, p := range proxies {
		jobs <- p
	}
	close(jobs)

	wg.Wait()
	close(results)

	var alive []Proxy
	for p := range results {
		alive = append(alive, p)
	}
	return alive
}
