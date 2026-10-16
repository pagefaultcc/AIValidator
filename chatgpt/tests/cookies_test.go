package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chatgpt-validator/pkg/cookie"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseNetscapeCookies(t *testing.T) {
	path := writeFile(t, t.TempDir(), "cookies.txt", ""+
		"# Netscape HTTP Cookie File\n"+
		"# a comment\n"+
		"#HttpOnly_.chatgpt.com\tTRUE\t/\tTRUE\t1900000000\t__Secure-next-auth.session-token\tsess_token_val\n"+
		".chatgpt.com\tTRUE\t/\tTRUE\t1900000000\toai-did\tdevice_123\n"+
		"example.com TRUE / TRUE 1900000000 space_key space_val\n"+
		"malformed line without enough columns\n")

	cf, err := cookie.ParseCookieFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cf.Cookies["__Secure-next-auth.session-token"] != "sess_token_val" {
		t.Errorf("HttpOnly cookie failed: got %q", cf.Cookies["__Secure-next-auth.session-token"])
	}
	if cf.Cookies["oai-did"] != "device_123" {
		t.Errorf("oai-did cookie failed: got %q", cf.Cookies["oai-did"])
	}
	if cf.Cookies["space_key"] != "space_val" {
		t.Errorf("space separated cookie failed: got %q", cf.Cookies["space_key"])
	}
}

func TestParseJSONCookiesList(t *testing.T) {
	jsonContent := `[
		{"name": "__Secure-next-auth.session-token", "value": "jwt_token_here"},
		{"Name": "oai-did", "Value": "device_xyz"}
	]`
	path := writeFile(t, t.TempDir(), "cookies.json", jsonContent)

	cf, err := cookie.ParseCookieFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cf.Cookies["__Secure-next-auth.session-token"] != "jwt_token_here" {
		t.Errorf("got %q", cf.Cookies["__Secure-next-auth.session-token"])
	}
	if cf.Cookies["oai-did"] != "device_xyz" {
		t.Errorf("got %q", cf.Cookies["oai-did"])
	}
}

func TestParseJSONCookiesMap(t *testing.T) {
	jsonContent := `{"session-token": "abc", "oai-did": "123"}`
	path := writeFile(t, t.TempDir(), "cookies.json", jsonContent)

	cf, err := cookie.ParseCookieFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cf.Cookies["session-token"] != "abc" || cf.Cookies["oai-did"] != "123" {
		t.Errorf("unexpected cookies: %+v", cf.Cookies)
	}
}

func TestDiscoverSingleFile(t *testing.T) {
	path := writeFile(t, t.TempDir(), "single.txt", "content")
	got, err := cookie.DiscoverCookieFiles(path, "*")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != path {
		t.Errorf("got %v, want [%s]", got, path)
	}
}

func TestDiscoverFolderRecursive(t *testing.T) {
	root := t.TempDir()
	a := writeFile(t, root, "cookie_1.txt", "1")
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	b := writeFile(t, sub, "cookie_2.cookies", "2")
	_ = writeFile(t, sub, "script.py", "python script")

	validDir := filepath.Join(root, "valid")
	_ = os.MkdirAll(validDir, 0o755)
	_ = writeFile(t, validDir, "valid_cookie.txt", "valid")

	got, err := cookie.DiscoverCookieFiles(root, "*")
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{a: true, b: true}
	if len(got) != len(want) {
		t.Fatalf("got %d files, want %d: %v", len(got), len(want), got)
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected file %s", p)
		}
	}
}

func TestUniqueDestPath(t *testing.T) {
	dir := t.TempDir()
	p1 := cookie.UniqueDestPath(dir, "cookie.txt")
	want1 := filepath.Join(dir, "cookie.txt")
	if p1 != want1 {
		t.Errorf("got %q, want %q", p1, want1)
	}

	_ = writeFile(t, dir, "cookie.txt", "1")
	p2 := cookie.UniqueDestPath(dir, "cookie.txt")
	want2 := filepath.Join(dir, "cookie_1.txt")
	if p2 != want2 {
		t.Errorf("got %q, want %q", p2, want2)
	}
}

func TestMoveFile(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, dir, "source.txt", "data")
	dst := filepath.Join(dir, "sub", "dest.txt")

	if err := cookie.MoveFile(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source was not removed: %s", src)
	}
	data, err := os.ReadFile(dst)
	if err != nil || string(data) != "data" {
		t.Errorf("dest read error or wrong data: %v, %q", err, string(data))
	}
}

func TestBuildCookieHeader(t *testing.T) {
	cookies := map[string]string{"foo": "1", "bar": "2"}
	h := cookie.BuildCookieHeader(cookies)
	if !strings.Contains(h, "foo=1") || !strings.Contains(h, "bar=2") {
		t.Errorf("unexpected cookie header: %q", h)
	}
}
