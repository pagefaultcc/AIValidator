package tests

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"chatgpt-validator/pkg/cookie"
	"chatgpt-validator/pkg/proxy"
	"chatgpt-validator/pkg/validator"
)

func createFakeJWT(t *testing.T, accountID, planType string, exp int64) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims := map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
			"plan_type":          planType,
		},
		"exp": exp,
	}
	claimsBytes, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	payload := base64.RawURLEncoding.EncodeToString(claimsBytes)
	return fmt.Sprintf("%s.%s.signature", header, payload)
}

func testCookieFile(t *testing.T, cookies map[string]string) *cookie.CookieFile {
	t.Helper()
	return &cookie.CookieFile{
		Path:    "test_cookie.txt",
		Cookies: cookies,
	}
}

func checkCookie(t *testing.T, cf *cookie.CookieFile, endpoint string) validator.Result {
	t.Helper()
	return validator.CheckCookieFile(context.Background(), cf, func() *proxy.Proxy { return nil }, 5*time.Second, endpoint)
}

func TestCheckValidSession(t *testing.T) {
	expTime := time.Now().Add(24 * time.Hour).Unix()
	fakeToken := createFakeJWT(t, "acc_123", "plus", expTime)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/session":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(fmt.Sprintf(`{
				"accessToken": %q,
				"user": {"email": "user@example.com", "name": "Test User"},
				"account": {"planType": "plus"}
			}`, fakeToken)))
		case "/backend-api/me":
			auth := r.Header.Get("Authorization")
			if auth != "Bearer "+fakeToken {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id": "user-123"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cf := testCookieFile(t, map[string]string{"__Secure-next-auth.session-token": "abc"})
	res := checkCookie(t, cf, srv.URL)

	if res.Status != validator.StatusValid {
		t.Fatalf("status = %v, want VALID (detail: %q)", res.Status, res.Detail)
	}
	if res.Email != "user@example.com" {
		t.Errorf("email = %q, want user@example.com", res.Email)
	}
	if res.Plan != "Plus" {
		t.Errorf("plan = %q, want Plus", res.Plan)
	}
	if res.Expiry == "" || res.Expiry == "Unknown" {
		t.Errorf("expiry not populated: %q", res.Expiry)
	}
}

func TestCheckValidFallbackToModels(t *testing.T) {
	expTime := time.Now().Add(12 * time.Hour).Unix()
	fakeToken := createFakeJWT(t, "acc_456", "team", expTime)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/session":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(fmt.Sprintf(`{
				"accessToken": %q,
				"user": {"email": "team@example.com"},
				"account": {"planType": "team"}
			}`, fakeToken)))
		case "/backend-api/me":
			// Simulate /me failure
			w.WriteHeader(http.StatusInternalServerError)
		case "/backend-api/models":
			// Fallback /models succeeds
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"models": []}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cf := testCookieFile(t, map[string]string{"__Secure-next-auth.session-token": "abc"})
	res := checkCookie(t, cf, srv.URL)

	if res.Status != validator.StatusValid {
		t.Fatalf("status = %v, want VALID (detail: %q)", res.Status, res.Detail)
	}
	if res.Plan != "Team" {
		t.Errorf("plan = %q, want Team", res.Plan)
	}
}

func TestCheckBackendTokenRejected(t *testing.T) {
	fakeToken := createFakeJWT(t, "acc_789", "free", time.Now().Add(time.Hour).Unix())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/session":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(fmt.Sprintf(`{
				"accessToken": %q,
				"user": {"email": "free@example.com"}
			}`, fakeToken)))
		case "/backend-api/me", "/backend-api/models":
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer srv.Close()

	cf := testCookieFile(t, map[string]string{"session": "xyz"})
	res := checkCookie(t, cf, srv.URL)

	if res.Status != validator.StatusInvalid {
		t.Fatalf("status = %v, want INVALID", res.Status)
	}
	if !strings.Contains(res.Detail, "Backend Token Rejected") {
		t.Errorf("detail = %q, want Backend Token Rejected", res.Detail)
	}
}

func TestCheckUnauthorizedSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	cf := testCookieFile(t, map[string]string{"session": "xyz"})
	res := checkCookie(t, cf, srv.URL)

	if res.Status != validator.StatusInvalid {
		t.Fatalf("status = %v, want INVALID", res.Status)
	}
	if !strings.Contains(res.Detail, "401") {
		t.Errorf("detail = %q, want HTTP 401 Unauthorized", res.Detail)
	}
}

func TestCheckCloudflareChallenge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("cf-mitigated", "challenge")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("challenge"))
	}))
	defer srv.Close()

	cf := testCookieFile(t, map[string]string{"session": "xyz"})
	res := checkCookie(t, cf, srv.URL)

	if res.Status != validator.StatusBlocked {
		t.Fatalf("status = %v, want BLOCKED", res.Status)
	}
	if res.Detail != "cloudflare challenge" {
		t.Errorf("detail = %q, want cloudflare challenge", res.Detail)
	}
}

func TestCheckRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	cf := testCookieFile(t, map[string]string{"session": "xyz"})
	res := checkCookie(t, cf, srv.URL)

	if res.Status != validator.StatusRateLimited {
		t.Fatalf("status = %v, want RATE_LIMITED", res.Status)
	}
}

func TestCheckEmptyCookies(t *testing.T) {
	cf := testCookieFile(t, map[string]string{})
	res := checkCookie(t, cf, "http://127.0.0.1:9999")

	if res.Status != validator.StatusEmpty {
		t.Fatalf("status = %v, want EMPTY", res.Status)
	}
}

func TestCheckNoActiveSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cf := testCookieFile(t, map[string]string{"session": "xyz"})
	res := checkCookie(t, cf, srv.URL)

	if res.Status != validator.StatusInvalid {
		t.Fatalf("status = %v, want INVALID", res.Status)
	}
	if !strings.Contains(res.Detail, "No active user session") {
		t.Errorf("detail = %q", res.Detail)
	}
}

func TestJWTDecodingAndPlanCapitalization(t *testing.T) {
	tok := createFakeJWT(t, "acc_1", "pro", 1800000000)
	claims, err := validator.DecodeJWT(tok)
	if err != nil {
		t.Fatal(err)
	}
	authObj, ok := claims["https://api.openai.com/auth"].(map[string]any)
	if !ok {
		t.Fatal("missing auth obj")
	}
	if authObj["chatgpt_account_id"] != "acc_1" || authObj["plan_type"] != "pro" {
		t.Errorf("claims mismatch: %+v", authObj)
	}

	for _, tc := range []struct {
		in   string
		want string
	}{
		{"free", "Free"},
		{"plus", "Plus"},
		{"team", "Team"},
		{"pro", "Pro"},
		{"enterprise", "Enterprise"},
		{"", "Free"},
	} {
		if got := validator.CapitalizePlan(tc.in); got != tc.want {
			t.Errorf("CapitalizePlan(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
