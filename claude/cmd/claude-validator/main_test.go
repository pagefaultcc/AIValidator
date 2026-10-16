package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"claude-validator/pkg/validator"
	"os"
)

// newTestServer serves canned claude.ai-shaped responses and counts nothing:
// the scan test only asserts statuses and channel close.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/organizations":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `[{"uuid":"u1","name":"Org One"}]`)
		case "/api/account":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"email_address":"a@b.c","memberships":[{"organization":{"plan_display_name":"","rate_limit_tier":"default_claude_ai"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestScannerWiring covers the scan closure shared by the flag flow and the
// -tui flow: arbitrary-name discovery, parse-error rows through the channel,
// worker pool, and channel close.
func TestScannerWiring(t *testing.T) {
	srv := newTestServer(t)
	dir := t.TempDir()
	names := []string{"weird name.txt", "no-format.txt", "notes.txt"}
	for i, name := range names[:2] {
		content := fmt.Sprintf("# Netscape HTTP Cookie File\n.claude.ai\tTRUE\t/\tTRUE\t1900000000\tsessionKey\tkey-%d\n", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	notesPath := filepath.Join(dir, names[2])
	if err := os.WriteFile(notesPath, []byte("just some notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	scan := newScanner(nil, "sticky", 3, 5*time.Second, srv.URL, "", "")
	paths, parsed, results, err := scan(dir, "*.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 3 || len(parsed) != 3 {
		t.Fatalf("discovered %d paths, want 3", len(paths))
	}

	var valid, invalidRows int
	for r := range results {
		switch r.Status {
		case validator.StatusValid:
			valid++
			if r.Email != "a@b.c" || r.Orgs != "Org One" {
				t.Errorf("%s: email = %q orgs = %q", r.Path, r.Email, r.Orgs)
			}
		case validator.StatusInvalid:
			invalidRows++ // notes.txt: no sessionKey
		case validator.StatusParseError:
			t.Errorf("%s: unexpected parse error: %v", r.Path, r.Detail)
		default:
			t.Errorf("%s: unexpected status %v", r.Path, r.Status)
		}
	}
	if valid != 2 || invalidRows != 1 {
		t.Errorf("valid = %d invalid = %d, want 2 and 1", valid, invalidRows)
	}
}

func TestScannerNoMatch(t *testing.T) {
	scan := newScanner(nil, "sticky", 1, time.Second, "http://127.0.0.1:1", "", "")
	paths, parsed, results, err := scan(t.TempDir(), "*.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 || len(parsed) != 0 {
		t.Fatalf("expected zero matches, got %d", len(paths))
	}
	for range results { // must close immediately, no deadlock
		t.Error("unexpected result on empty scan")
	}
}

func TestScannerMissingDir(t *testing.T) {
	scan := newScanner(nil, "sticky", 1, time.Second, "http://127.0.0.1:1", "", "")
	if _, _, _, err := scan(filepath.Join(t.TempDir(), "nope"), "*.txt"); err == nil {
		t.Error("expected error for missing dir")
	}
}
