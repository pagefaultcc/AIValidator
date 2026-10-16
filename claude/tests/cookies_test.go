package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claude-validator/pkg/cookie"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseNetscapeFile(t *testing.T) {
	path := writeFile(t, t.TempDir(), "c.txt", ""+
		"# Netscape HTTP Cookie File\n"+
		"\n"+
		"# a comment line\n"+
		"#HttpOnly_.claude.ai\tTRUE\t/\tTRUE\t1900000000\thtkey\thtvalue\n"+
		".claude.ai\tTRUE\t/\tTRUE\t1790000000\tsessionKey\tsessionvalue\n"+
		".claude.ai\tTRUE\t/\tTRUE\tNaN\tsessionKey\tbrokenkey\n"+
		"malformed line without tabs\n"+
		"only\tfive\ttab\tfields\there\n"+
		"a\tb\tc\td\te\tf\tg\th\ti\n"+ // 9 fields: ignored
		".claude.ai\tTRUE\t/\tTRUE\tnot-a-number\tbadexpiry\tx\n")
	cf, err := cookie.ParseNetscapeFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cf.Cookies["htkey"] != "htvalue" {
		t.Errorf("#HttpOnly_ line not parsed: %q", cf.Cookies["htkey"])
	}
	if cf.Cookies["sessionKey"] != "brokenkey" {
		t.Errorf("duplicate cookie name: last line must win, got %q", cf.Cookies["sessionKey"])
	}
	if cf.Cookies["badexpiry"] != "x" {
		t.Error("non-sessionKey cookie with non-numeric expiry should still be stored")
	}
	if _, ok := cf.Cookies["a"]; ok {
		t.Error("9-field line should be ignored")
	}
	want := time.Unix(1790000000, 0)
	if !cf.SessionKeyExpires.Equal(want) {
		t.Errorf("SessionKeyExpires = %v, want %v (broken expiry line must not overwrite a good one)", cf.SessionKeyExpires, want)
	}
}

func TestParseNetscapeFileNoSessionKey(t *testing.T) {
	path := writeFile(t, t.TempDir(), "c.txt", "example.com\tTRUE\t/\tTRUE\t1900000000\tfoo\tbar\n")
	cf, err := cookie.ParseNetscapeFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cf.SessionKeyExpires.IsZero() {
		t.Errorf("expected zero SessionKeyExpires, got %v", cf.SessionKeyExpires)
	}
}

func TestParseNetscapeFileUnreadable(t *testing.T) {
	if _, err := cookie.ParseNetscapeFile(filepath.Join(t.TempDir(), "nonexistent.txt")); err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestDiscoverSingleFile(t *testing.T) {
	path := writeFile(t, t.TempDir(), "single.txt", "x")
	got, err := cookie.DiscoverCookieFiles(path, "ignored")
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
	b := writeFile(t, sub, "cookie_2.txt", "2")
	_ = writeFile(t, sub, "other.txt", "ignore me")

	got, err := cookie.DiscoverCookieFiles(root, "cookie_*.txt")
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

func TestDiscoverNoMatch(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "mismatch.txt", "x")
	got, err := cookie.DiscoverCookieFiles(root, "cookie_*.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 files, got %v", got)
	}
}

func TestDiscoverMissingPath(t *testing.T) {
	if _, err := cookie.DiscoverCookieFiles(filepath.Join(t.TempDir(), "nope"), "*"); err == nil {
		t.Error("expected error for nonexistent path")
	}
}

func TestDiscoverAllFiles(t *testing.T) {
	root := t.TempDir()
	a := writeFile(t, root, "a.txt", "1")
	b := writeFile(t, root, "b.cookies", "2")
	c := writeFile(t, root, "random_name", "3")
	got, err := cookie.DiscoverCookieFiles(root, "*")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d files, want 3: %v", len(got), got)
	}
	want := map[string]bool{a: true, b: true, c: true}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected file: %s", p)
		}
	}
}

func TestDiscoverSkipsHiddenFiles(t *testing.T) {
	root := t.TempDir()
	_ = writeFile(t, root, ".DS_Store", "x")
	a := writeFile(t, root, "valid.txt", "x")
	got, err := cookie.DiscoverCookieFiles(root, "*")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != a {
		t.Fatalf("got %v, want [%s]", got, a)
	}
}

func TestDiscoverSkipsValidAndInvalidDirs(t *testing.T) {
	root := t.TempDir()
	vDir := filepath.Join(root, "valid")
	iDir := filepath.Join(root, "invalid")
	if err := os.MkdirAll(vDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(iDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = writeFile(t, vDir, "c1.txt", "v")
	_ = writeFile(t, iDir, "c2.txt", "i")
	target := writeFile(t, root, "normal.txt", "n")

	got, err := cookie.DiscoverCookieFiles(root, "*")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != target {
		t.Fatalf("got %v, want [%s]", got, target)
	}
}

func TestMoveFile(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, dir, "source.txt", "content")
	dst := filepath.Join(dir, "sub", "dest.txt")

	if err := cookie.MoveFile(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("src %s still exists", src)
	}
	data, err := os.ReadFile(dst)
	if err != nil || string(data) != "content" {
		t.Errorf("dst %s read failed: %v, content=%q", dst, err, string(data))
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

	_ = writeFile(t, dir, "cookie_1.txt", "2")
	p3 := cookie.UniqueDestPath(dir, "cookie.txt")
	want3 := filepath.Join(dir, "cookie_2.txt")
	if p3 != want3 {
		t.Errorf("got %q, want %q", p3, want3)
	}
}

func TestBuildCookieHeader(t *testing.T) {
	cases := []struct {
		cookies map[string]string
		want    []string // parts that must be present
	}{
		{map[string]string{"a": "1"}, []string{"a=1"}},
		{map[string]string{"a": "1", "b": "2"}, []string{"a=1", "b=2"}},
	}
	for _, tc := range cases {
		got := cookie.BuildCookieHeader(tc.cookies)
		for _, part := range tc.want {
			if !strings.Contains(got, part) {
				t.Errorf("cookies %+v: header %q missing %q", tc.cookies, got, part)
			}
		}
	}
}
