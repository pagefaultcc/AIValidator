package validator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"claude-validator/pkg/cookie"
	"claude-validator/pkg/proxy"
)

// Status classifies the outcome of validating one cookie file.
type Status int

const (
	StatusPending Status = iota
	StatusValid
	StatusInvalid
	StatusExpired
	StatusRateLimited
	StatusBlocked
	StatusError
	StatusParseError
)

func (s Status) String() string {
	switch s {
	case StatusPending:
		return "PENDING"
	case StatusValid:
		return "VALID"
	case StatusInvalid:
		return "INVALID"
	case StatusExpired:
		return "EXPIRED"
	case StatusRateLimited:
		return "RATE_LIMITED"
	case StatusBlocked:
		return "BLOCKED"
	case StatusError:
		return "ERROR"
	case StatusParseError:
		return "PARSE_ERROR"
	default:
		return "UNKNOWN"
	}
}

// Result is one finished check.
type Result struct {
	Path      string
	MovedPath string
	Status    Status
	Email     string
	Plan      string
	Orgs      string
	Detail    string
	Proxy     string
	Elapsed   time.Duration
}

// browserHeaders is the shared browser-like header set sent with every request;
// Referer/Origin are derived from the endpoint.
func browserHeaders(endpoint string) map[string]string {
	return map[string]string{
		"User-Agent":                "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
		"Accept":                    "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
		"Accept-Language":           "en-US,en;q=0.9",
		"Sec-Ch-Ua":                 `"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`,
		"Sec-Ch-Ua-Mobile":          "?0",
		"Sec-Ch-Ua-Platform":        `"Windows"`,
		"Sec-Fetch-Dest":            "document",
		"Sec-Fetch-Mode":            "navigate",
		"Sec-Fetch-Site":            "same-origin",
		"Sec-Fetch-User":            "?1",
		"Upgrade-Insecure-Requests": "1",
		"Referer":                   endpoint + "/new",
		"Origin":                    endpoint,
	}
}

// CheckCookieFile validates one parsed cookie file. pickProxy is consulted for
// every HTTP request: in sticky mode it always returns the file's pinned proxy
// (so the client is reused across the file's requests); in rotating mode it
// round-robins endpoints per request. No retries anywhere: one request per
// endpoint per file, deterministic.
func CheckCookieFile(ctx context.Context, file *cookie.CookieFile, pickProxy func() *proxy.Proxy, timeout time.Duration, endpoint string) (res Result) {
	start := time.Now()
	res.Path = file.Path
	defer func() {
		res.Elapsed = time.Since(start)
		if res.Elapsed <= 0 {
			res.Elapsed = time.Microsecond
		}
	}()

	if file.Err != nil {
		res.Status = StatusParseError
		res.Detail = file.Err.Error()
		return
	}

	sk, hasSK := file.Cookies["sessionKey"]
	if !hasSK || sk == "" {
		res.Status = StatusInvalid
		res.Detail = "missing sessionKey"
		return
	}

	if !file.SessionKeyExpires.IsZero() && time.Now().After(file.SessionKeyExpires) {
		res.Status = StatusExpired
		res.Detail = "sessionKey expired " + file.SessionKeyExpires.UTC().Format("2006-01-02")
		return
	}

	cookieHeader := cookie.BuildCookieHeader(file.Cookies)
	getClient := func() (*http.Client, string) {
		p := pickProxy()
		return proxy.ClientFor(p, timeout), proxy.ProxyLabel(p)
	}

	// 1. GET /api/organizations — authoritative for VALID status.
	status, body, isJSON, cfMitigated, lastProxy, err := httpGet(ctx, getClient, endpoint+"/api/organizations", cookieHeader, endpoint)
	res.Proxy = lastProxy
	if err != nil {
		res.Status = StatusError
		res.Detail = err.Error()
		return
	}

	switch {
	case status == http.StatusOK:
		// Expect a JSON array of org objects: [{"uuid":"...","name":"..."}, ...]
		var orgs []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(body, &orgs); err != nil {
			res.Status = StatusError
			res.Detail = "unexpected response shape"
			return
		}
		names := make([]string, 0, len(orgs))
		for _, o := range orgs {
			if o.Name != "" {
				names = append(names, o.Name)
			}
		}
		res.Orgs = strings.Join(names, ", ")
		res.Status = StatusValid

	case status == http.StatusUnauthorized:
		res.Status = StatusInvalid
		res.Detail = "unauthorized"
		return

	case status == http.StatusForbidden:
		// Live claude.ai behavior:
		//  - cf-mitigated: challenge header => BLOCKED
		//  - JSON with parseable error => INVALID (e.g. account_session_invalid)
		//  - HTML => BLOCKED (Cloudflare challenge page)
		//  - other => ERROR
		if strings.EqualFold(cfMitigated, "challenge") {
			res.Status = StatusBlocked
			res.Detail = "cloudflare challenge"
			return
		}
		if isJSON {
			if d := apiErrorDetail(body); d != "" {
				res.Status = StatusInvalid
				res.Detail = d
				return
			}
			res.Status = StatusError
			res.Detail = "HTTP 403"
			return
		}
		res.Status = StatusBlocked
		res.Detail = "cloudflare challenge"
		return

	case status == http.StatusTooManyRequests:
		res.Status = StatusRateLimited
		res.Detail = "rate limited"
		return

	case status >= 500:
		res.Status = StatusError
		res.Detail = fmt.Sprintf("HTTP %d", status)
		return

	default:
		res.Status = StatusError
		res.Detail = fmt.Sprintf("HTTP %d", status)
		return
	}

	// 2. GET /api/account — enrich valid session with email and plan.
	// NOTE: this endpoint rate-limits heavily under concurrency. Failure here
	// MUST NOT downgrade the VALID verdict; report it in Detail instead.
	status, body, _, _, lastProxy, err = httpGet(ctx, getClient, endpoint+"/api/account", cookieHeader, endpoint)
	if lastProxy != "" {
		res.Proxy = lastProxy
	}
	if err != nil {
		res.Detail = "account lookup failed: " + err.Error()
		return
	}
	if status != http.StatusOK {
		res.Detail = fmt.Sprintf("account lookup: HTTP %d", status)
		return
	}

	var acc struct {
		EmailAddress string `json:"email_address"`
		FullName     string `json:"full_name"`
		Memberships  []struct {
			SeatTier     string `json:"seat_tier"`
			Organization struct {
				PlanDisplayName string `json:"plan_display_name"`
				RateLimitTier   string `json:"rate_limit_tier"`
			} `json:"organization"`
		} `json:"memberships"`
	}
	if err := json.Unmarshal(body, &acc); err != nil {
		res.Detail = "account lookup: malformed JSON"
		return
	}

	res.Email = acc.EmailAddress
	if len(acc.Memberships) > 0 {
		m := acc.Memberships[0]
		res.Plan = firstNonEmpty(m.Organization.PlanDisplayName, m.Organization.RateLimitTier, m.SeatTier)
	}
	return
}

// APIErrorDetail extracts a human detail from a claude.ai JSON error body:
// "<message> (<error_code>)". Empty if the body is not a parseable error.
func APIErrorDetail(body []byte) string {
	return apiErrorDetail(body)
}

func apiErrorDetail(body []byte) string {
	var e struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Details struct {
				ErrorCode string `json:"error_code"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil || e.Error.Type == "" {
		return ""
	}
	msg := strings.ToLower(e.Error.Message)
	if code := e.Error.Details.ErrorCode; code != "" {
		if msg != "" {
			return msg + " (" + code + ")"
		}
		return code
	}
	if msg != "" {
		return msg
	}
	return e.Error.Type
}

// httpGet performs one GET with the browser headers and the cookie header.
// isJSON is decided by the body's first byte or the Content-Type.
func httpGet(ctx context.Context, getClient func() (*http.Client, string), rawURL, cookieHeader, endpoint string) (statusCode int, body []byte, isJSON bool, cfMitigated string, proxyLabel string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil, false, "", "", err
	}
	for k, v := range browserHeaders(endpoint) {
		req.Header.Set(k, v)
	}
	if cookieHeader != "" {
		req.Header.Set("Cookie", cookieHeader)
	}

	client, label := getClient()
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, false, "", label, err
	}
	defer resp.Body.Close()

	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, false, resp.Header.Get("cf-mitigated"), label, err
	}

	ct := resp.Header.Get("Content-Type")
	trimmed := bytes.TrimSpace(body)
	jsonLike := strings.Contains(ct, "application/json") ||
		(len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '['))

	return resp.StatusCode, body, jsonLike, resp.Header.Get("cf-mitigated"), label, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
