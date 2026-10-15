package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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
	cf, err := ParseNetscapeFile(path)
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
	path := writeFile(t, t.TempDir(), "c.txt", "# only comments\n")
	cf, err := ParseNetscapeFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cf.SessionKeyExpires.IsZero() {
		t.Errorf("expected zero expiry, got %v", cf.SessionKeyExpires)
	}
}

func TestParseNetscapeFileUnreadable(t *testing.T) {
	if _, err := ParseNetscapeFile(filepath.Join(t.TempDir(), "missing.txt")); err == nil {
		t.Error("expected error for unreadable file")
	}
}

func TestDiscoverSingleFile(t *testing.T) {
	path := writeFile(t, t.TempDir(), "whatever.txt", "# x\n")
	got, err := DiscoverCookieFiles(path, "cookie_*.txt") // pattern must NOT apply to a direct file
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != path {
		t.Errorf("single-file discovery = %v, want [%s]", got, path)
	}
}

func TestDiscoverFolderRecursive(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "cookie_1.txt", "# x\n")
	sub := filepath.Join(dir, "nested", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, sub, "cookie_2.txt", "# x\n")
	writeFile(t, sub, "other.txt", "# x\n")

	got, err := DiscoverCookieFiles(dir, "cookie_*.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("discovered %d files, want 2: %v", len(got), got)
	}
	for _, p := range got {
		if filepath.Base(p) != "cookie_1.txt" && filepath.Base(p) != "cookie_2.txt" {
			t.Errorf("unexpected match: %s", p)
		}
	}
}

func TestDiscoverNoMatch(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "other.txt", "# x\n")
	got, err := DiscoverCookieFiles(dir, "cookie_*.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected no matches, got %v", got)
	}
}

func TestDiscoverMissingPath(t *testing.T) {
	if _, err := DiscoverCookieFiles(filepath.Join(t.TempDir(), "nope"), "*"); err == nil {
		t.Error("expected error for missing path")
	}
}

func TestBuildCookieHeader(t *testing.T) {
	h := buildCookieHeader(map[string]string{"a": "1", "b": "2"})
	if h != "a=1; b=2" && h != "b=2; a=1" {
		t.Errorf("unexpected header %q", h)
	}
}
