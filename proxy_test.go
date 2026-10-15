package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func load(t *testing.T, content string) ([]Proxy, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "proxies.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return LoadProxies(path)
}

func TestLoadProxiesFormats(t *testing.T) {
	cases := []struct {
		line string
		want string // URL.String() of the parsed proxy
		user string // username, "" if none
	}{
		{line: "127.0.0.1:8080", want: "http://127.0.0.1:8080"},
		{line: "host.example:3128", want: "http://host.example:3128"},
		{line: "host:8080:user:pass", want: "http://user:pass@host:8080", user: "user"},
		{line: "user:pass@host:8080", want: "http://user:pass@host:8080", user: "user"},
		{line: "http://host:8080", want: "http://host:8080"},
		{line: "https://host:8443", want: "https://host:8443"},
		{line: "socks5://host:1080", want: "socks5://host:1080"},
	}
	for _, tc := range cases {
		proxies, err := load(t, "# comment\n\n"+tc.line+"\n")
		if err != nil {
			t.Errorf("line %q: %v", tc.line, err)
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

func TestLoadProxiesMultiple(t *testing.T) {
	proxies, err := load(t, "a:1\n\n# c\nb:2\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(proxies) != 2 || proxyLabel(&proxies[0]) != "a:1" || proxyLabel(&proxies[1]) != "b:2" {
		t.Errorf("got %+v", proxies)
	}
}

func TestLoadProxiesMalformed(t *testing.T) {
	cases := []string{
		"ftp://host:21", // unsupported scheme
		"http://host",   // no port
		"host",          // no port
		"host:notaport", // bad port
		"host:0",        // port out of range
		"host:70000",    // port out of range
		"a:b:c",         // wrong field count
	}
	for _, line := range cases {
		_, err := load(t, line+"\n")
		if err == nil {
			t.Errorf("line %q: expected error", line)
			continue
		}
		if !strings.Contains(err.Error(), "line 1") {
			t.Errorf("line %q: error missing line number: %v", line, err)
		}
	}
}

func TestLoadProxiesEmptyFile(t *testing.T) {
	if _, err := load(t, "# only comments\n"); err == nil {
		t.Error("expected error for empty proxy file")
	}
}

func TestLoadProxiesMissingFile(t *testing.T) {
	if _, err := LoadProxies(filepath.Join(t.TempDir(), "nope.txt")); err == nil {
		t.Error("expected error for missing file")
	}
}

func TestProxyLabelNil(t *testing.T) {
	if proxyLabel(nil) != "" {
		t.Error("nil proxy label should be empty")
	}
}
