package tests

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"chatgpt-validator/pkg/proxy"
)

func loadProxyFile(t *testing.T, content string) ([]proxy.Proxy, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "proxies.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return proxy.LoadProxies(path)
}

func TestLoadProxiesFormats(t *testing.T) {
	cases := []struct {
		line string
		want string
		user string
	}{
		{line: "127.0.0.1:8080", want: "http://127.0.0.1:8080"},
		{line: "proxy.local:3128", want: "http://proxy.local:3128"},
		{line: "host:8080:usr:pwd", want: "http://usr:pwd@host:8080", user: "usr"},
		{line: "usr:pwd@host:8080", want: "http://usr:pwd@host:8080", user: "usr"},
		{line: "http://host:8080", want: "http://host:8080"},
		{line: "https://host:8443", want: "https://host:8443"},
		{line: "socks5://host:1080", want: "socks5://host:1080"},
	}

	for _, tc := range cases {
		proxies, err := loadProxyFile(t, "# header\n\n"+tc.line+"\n")
		if err != nil {
			t.Errorf("line %q: unexpected error: %v", tc.line, err)
			continue
		}
		if len(proxies) != 1 {
			t.Errorf("line %q: got %d proxies, want 1", tc.line, len(proxies))
			continue
		}
		if got := proxies[0].URL.String(); got != tc.want {
			t.Errorf("line %q: URL = %q, want %q", tc.line, got, tc.want)
		}
		if tc.user != "" && (proxies[0].URL.User == nil || proxies[0].URL.User.Username() != tc.user) {
			t.Errorf("line %q: missing username", tc.line)
		}
	}
}

func TestLoadProxiesDirectString(t *testing.T) {
	proxies, err := proxy.LoadProxies("user:pass@127.0.0.1:9090")
	if err != nil {
		t.Fatal(err)
	}
	if len(proxies) != 1 {
		t.Fatalf("expected 1 proxy, got %d", len(proxies))
	}
	if proxies[0].URL.Host != "127.0.0.1:9090" {
		t.Errorf("expected host 127.0.0.1:9090, got %q", proxies[0].URL.Host)
	}
}

func TestLoadProxiesMalformed(t *testing.T) {
	cases := []string{
		"ftp://host:21",
		"host",
		"host:badport",
		"host:0",
		"host:70000",
		"a:b:c",
	}

	for _, line := range cases {
		_, err := loadProxyFile(t, line+"\n")
		if err == nil {
			t.Errorf("line %q: expected error, got nil", line)
		}
	}
}

func TestProxyLabel(t *testing.T) {
	u, _ := url.Parse("http://usr:pwd@myproxy.com:8080")
	p := &proxy.Proxy{URL: u}
	label := proxy.ProxyLabel(p)
	if label != "myproxy.com:8080" {
		t.Errorf("expected myproxy.com:8080, got %q", label)
	}

	if proxy.ProxyLabel(nil) != "" {
		t.Errorf("expected empty string for nil proxy")
	}
}

func TestVerifyProxies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"origin": "127.0.0.1"}`))
	}))
	defer srv.Close()

	// Direct proxy (URL points to mock server)
	u, _ := url.Parse(srv.URL)
	p := proxy.Proxy{URL: u}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ok := proxy.CheckSingleProxy(ctx, &p, 2*time.Second, srv.URL)
	if !ok {
		t.Errorf("expected CheckSingleProxy to return true")
	}

	alive := proxy.VerifyProxies(ctx, []proxy.Proxy{p}, 2, 2*time.Second, srv.URL)
	if len(alive) != 1 {
		t.Errorf("expected 1 alive proxy, got %d", len(alive))
	}
}
