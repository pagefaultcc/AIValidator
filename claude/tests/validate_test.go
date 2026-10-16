package tests

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"claude-validator/pkg/cookie"
	"claude-validator/pkg/proxy"
	"claude-validator/pkg/validator"
)

// hitCountingServer serves canned responses per path and counts hits per path.
type hitCountingServer struct {
	*httptest.Server
	hits map[string]*atomic.Int64
}

func newTestServer(t *testing.T, handler func(path string, w http.ResponseWriter, r *http.Request)) *hitCountingServer {
	t.Helper()
	hs := &hitCountingServer{hits: map[string]*atomic.Int64{}}
	hs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hs.hits[r.URL.Path].Add(1)
		handler(r.URL.Path, w, r)
	}))
	for _, p := range []string{"/api/organizations", "/api/account"} {
		hs.hits[p] = &atomic.Int64{}
	}
	t.Cleanup(hs.Close)
	return hs
}

func testFile(t *testing.T, sessionKey string, expiry int64) *cookie.CookieFile {
	t.Helper()
	cf := &cookie.CookieFile{Path: "test.txt", Cookies: map[string]string{"cf_clearance": "x"}}
	if sessionKey != "" {
		cf.Cookies["sessionKey"] = sessionKey
		cf.SessionKeyExpires = time.Unix(expiry, 0)
	}
	return cf
}

func check(t *testing.T, cf *cookie.CookieFile, endpoint string) validator.Result {
	t.Helper()
	return validator.CheckCookieFile(context.Background(), cf, func() *proxy.Proxy { return nil }, 5*time.Second, endpoint)
}

func orgsHandler(orgsJSON, accountJSON string) func(path string, w http.ResponseWriter, r *http.Request) {
	return func(path string, w http.ResponseWriter, r *http.Request) {
		switch path {
		case "/api/organizations":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(orgsJSON))
		case "/api/account":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(accountJSON))
		}
	}
}

const validAccountJSON = `{"email_address":"a@b.c","full_name":"A B","memberships":[{"seat_tier":"","organization":{"name":"n","plan_display_name":"","rate_limit_tier":"default_claude_ai"}}]}`

func TestCheckValid(t *testing.T) {
	srv := newTestServer(t, orgsHandler(`[{"uuid":"u1","name":"Org One","extra":true},{"uuid":"u2","name":"Org Two"}]`, validAccountJSON))
	res := check(t, testFile(t, "sk", time.Now().Unix()+3600), srv.URL)
	if res.Status != validator.StatusValid {
		t.Fatalf("status = %v, want VALID (detail %q)", res.Status, res.Detail)
	}
	if res.Email != "a@b.c" {
		t.Errorf("email = %q", res.Email)
	}
	if res.Plan != "default_claude_ai" {
		t.Errorf("plan = %q", res.Plan)
	}
	if res.Orgs != "Org One, Org Two" {
		t.Errorf("orgs = %q", res.Orgs)
	}
	if res.Elapsed <= 0 {
		t.Error("elapsed not recorded")
	}
}

func TestCheckPlanDisplayNamePreferred(t *testing.T) {
	srv := newTestServer(t, orgsHandler(`[{"uuid":"u","name":"O"}]`,
		`{"email_address":"a@b.c","memberships":[{"organization":{"plan_display_name":"Pro","rate_limit_tier":"tier2"}}]}`))
	res := check(t, testFile(t, "sk", time.Now().Unix()+3600), srv.URL)
	if res.Plan != "Pro" {
		t.Errorf("plan = %q, want Pro", res.Plan)
	}
}

func TestPrechecksMakeNoHTTPRequests(t *testing.T) {
	srv := newTestServer(t, orgsHandler(`[]`, `{}`))

	res := check(t, testFile(t, "", 0), srv.URL) // no sessionKey
	if res.Status != validator.StatusInvalid || res.Detail != "missing sessionKey" {
		t.Errorf("status = %v detail = %q", res.Status, res.Detail)
	}

	res = check(t, testFile(t, "sk", 1), srv.URL) // expired 1970
	if res.Status != validator.StatusExpired || !strings.Contains(res.Detail, "1970-01-01") {
		t.Errorf("status = %v detail = %q", res.Status, res.Detail)
	}

	for _, p := range srv.hits {
		if n := p.Load(); n != 0 {
			t.Errorf("pre-check paths made %d HTTP requests, want 0", n)
		}
	}
}

func TestCheckUnauthorized(t *testing.T) {
	srv := newTestServer(t, func(path string, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	res := check(t, testFile(t, "sk", time.Now().Unix()+3600), srv.URL)
	if res.Status != validator.StatusInvalid || res.Detail != "unauthorized" {
		t.Errorf("status = %v detail = %q", res.Status, res.Detail)
	}
}

func TestCheckBlockedHTMLChallenge(t *testing.T) {
	srv := newTestServer(t, func(path string, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("<!DOCTYPE html><title>Just a moment...</title>"))
	})
	res := check(t, testFile(t, "sk", time.Now().Unix()+3600), srv.URL)
	if res.Status != validator.StatusBlocked || res.Detail != "cloudflare challenge" {
		t.Errorf("status = %v detail = %q", res.Status, res.Detail)
	}
}

func TestCheckBlockedCfMitigatedHeader(t *testing.T) {
	srv := newTestServer(t, func(path string, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("cf-mitigated", "challenge")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"challenge"}`))
	})
	res := check(t, testFile(t, "sk", time.Now().Unix()+3600), srv.URL)
	if res.Status != validator.StatusBlocked || res.Detail != "cloudflare challenge" {
		t.Errorf("status = %v detail = %q", res.Status, res.Detail)
	}
}

func TestCheckForbiddenJSONSessionInvalid(t *testing.T) {
	// Live claude.ai shape: invalid session => 403 JSON permission_error,
	// not 401. Must classify as INVALID, not ERROR.
	srv := newTestServer(t, func(path string, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"type":"error","error":{"type":"permission_error","message":"Invalid authorization","details":{"error_code":"account_session_invalid"}}}`))
	})
	res := check(t, testFile(t, "sk", time.Now().Unix()+3600), srv.URL)
	if res.Status != validator.StatusInvalid {
		t.Errorf("status = %v, want INVALID", res.Status)
	}
	if !strings.Contains(res.Detail, "invalid authorization") || !strings.Contains(res.Detail, "account_session_invalid") {
		t.Errorf("detail = %q", res.Detail)
	}
}

func TestCheckForbiddenJSONWithoutChallenge(t *testing.T) {
	// JSON 403 without a parseable claude.ai error body: not a challenge,
	// not an auth failure => generic ERROR.
	srv := newTestServer(t, func(path string, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"type":"error"}`))
	})
	res := check(t, testFile(t, "sk", time.Now().Unix()+3600), srv.URL)
	if res.Status != validator.StatusError || res.Detail != "HTTP 403" {
		t.Errorf("status = %v detail = %q", res.Status, res.Detail)
	}
}

func TestCheckRateLimited(t *testing.T) {
	srv := newTestServer(t, func(path string, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	res := check(t, testFile(t, "sk", time.Now().Unix()+3600), srv.URL)
	if res.Status != validator.StatusRateLimited {
		t.Errorf("status = %v", res.Status)
	}
}

func TestCheckServerErrors(t *testing.T) {
	for _, code := range []int{500, 503} {
		srv := newTestServer(t, func(path string, w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		})
		res := check(t, testFile(t, "sk", time.Now().Unix()+3600), srv.URL)
		if res.Status != validator.StatusError || res.Detail != fmt.Sprintf("HTTP %d", code) {
			t.Errorf("code %d: status = %v detail = %q", code, res.Status, res.Detail)
		}
	}
}

func TestCheckTransportError(t *testing.T) {
	// Endpoint points at a closed port: transport error, no panic.
	res := check(t, testFile(t, "sk", time.Now().Unix()+3600), "http://127.0.0.1:1")
	if res.Status != validator.StatusError || res.Detail == "" {
		t.Errorf("status = %v detail = %q", res.Status, res.Detail)
	}
}

func TestCheckOrgsNotArray(t *testing.T) {
	// 200 with a JSON object instead of an array: must not be VALID.
	srv := newTestServer(t, orgsHandler(`{"unexpected":"shape"}`, validAccountJSON))
	res := check(t, testFile(t, "sk", time.Now().Unix()+3600), srv.URL)
	if res.Status != validator.StatusError || res.Detail != "unexpected response shape" {
		t.Errorf("status = %v detail = %q", res.Status, res.Detail)
	}
}

func TestCheckAccountFailureDoesNotDowngrade(t *testing.T) {
	srv := newTestServer(t, func(path string, w http.ResponseWriter, r *http.Request) {
		if path == "/api/organizations" {
			w.Write([]byte(`[{"uuid":"u","name":"Org One"}]`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	res := check(t, testFile(t, "sk", time.Now().Unix()+3600), srv.URL)
	if res.Status != validator.StatusValid {
		t.Fatalf("status = %v, want VALID", res.Status)
	}
	if res.Orgs != "Org One" {
		t.Errorf("orgs = %q", res.Orgs)
	}
	if res.Detail != "account lookup: HTTP 500" {
		t.Errorf("detail = %q, want account lookup: HTTP 500", res.Detail)
	}
}

func TestCheckAccountRateLimitedKeepsValid(t *testing.T) {
	// Live-observed burst behavior: /api/account can 429 while the org call
	// succeeds. VALID must hold and the row must say why email/plan are empty.
	srv := newTestServer(t, func(path string, w http.ResponseWriter, r *http.Request) {
		if path == "/api/organizations" {
			w.Write([]byte(`[{"uuid":"u","name":"Org One"}]`))
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
	})
	res := check(t, testFile(t, "sk", time.Now().Unix()+3600), srv.URL)
	if res.Status != validator.StatusValid {
		t.Fatalf("status = %v, want VALID", res.Status)
	}
	if res.Detail != "account lookup: HTTP 429" {
		t.Errorf("detail = %q, want account lookup: HTTP 429", res.Detail)
	}
}

func TestCheckAccountMissingFieldsStayEmpty(t *testing.T) {
	srv := newTestServer(t, orgsHandler(`[{"uuid":"u","name":"O"}]`, `{"unrelated":true}`))
	res := check(t, testFile(t, "sk", time.Now().Unix()+3600), srv.URL)
	if res.Status != validator.StatusValid {
		t.Fatalf("status = %v", res.Status)
	}
	if res.Email != "" || res.Plan != "" {
		t.Errorf("email = %q plan = %q, want empty", res.Email, res.Plan)
	}
}

func TestCheckSendsBrowserHeadersAndCookies(t *testing.T) {
	var gotCookie, gotUA, gotReferer string
	srv := newTestServer(t, func(path string, w http.ResponseWriter, r *http.Request) {
		gotCookie, gotUA, gotReferer = r.Header.Get("Cookie"), r.Header.Get("User-Agent"), r.Header.Get("Referer")
		w.Write([]byte(`[{"uuid":"u","name":"O"}]`))
	})
	cf := testFile(t, "sk", time.Now().Unix()+3600)
	cf.Cookies["cf_clearance"] = "cl"
	check(t, cf, srv.URL)
	if !strings.Contains(gotCookie, "sessionKey=sk") || !strings.Contains(gotCookie, "cf_clearance=cl") {
		t.Errorf("cookie header = %q", gotCookie)
	}
	if gotUA == "" || !strings.Contains(gotUA, "Chrome") {
		t.Errorf("user agent = %q", gotUA)
	}
	if gotReferer != srv.URL+"/new" {
		t.Errorf("referer = %q", gotReferer)
	}
}

func TestStatusStringAll(t *testing.T) {
	for _, s := range []validator.Status{
		validator.StatusPending, validator.StatusValid, validator.StatusInvalid, validator.StatusExpired,
		validator.StatusRateLimited, validator.StatusBlocked, validator.StatusError, validator.StatusParseError,
	} {
		if s.String() == "" || s.String() == "UNKNOWN" {
			t.Errorf("status %d has no string", s)
		}
	}
}
