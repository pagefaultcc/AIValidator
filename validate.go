package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
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
	Path    string
	Status  Status
	Detail  string
	Email   string
	Plan    string
	Orgs    string
	Proxy   string
	Elapsed time.Duration
}

// browserHeaders is the shared browser-like header set sent with every request;
// Referer/Origin are derived from the endpoint.
func browserHeaders(endpoint string) map[string]string {
	return map[string]string{
		"User-Agent":      "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
		"Accept":          "application/json",
		"Accept-Language": "en-US,en;q=0.9",
		"Referer":         endpoint + "/new",
		"Origin":          endpoint,
		"Sec-Fetch-Dest":  "empty",
		"Sec-Fetch-Mode":  "cors",
		"Sec-Fetch-Site":  "same-origin",
	}
}

// CheckCookieFile validates one parsed cookie file. pickProxy is consulted for
// every HTTP request: in sticky mode it always returns the file's pinned proxy
// (so the client is reused across the file's requests); in rotating mode it
// round-robins endpoints per request. No retries anywhere: one request per
// endpoint per file, deterministic.
func CheckCookieFile(ctx context.Context, file *CookieFile, pickProxy func() *Proxy, timeout time.Duration, endpoint string) (res Result) {
	start := time.Now()
	res = Result{Path: file.Path}
	defer func() { res.Elapsed = time.Since(start) }()

	if _, ok := file.Cookies["sessionKey"]; !ok {
		res.Status = StatusInvalid
		res.Detail = "missing sessionKey"
		return res
	}
	if !file.SessionKeyExpires.IsZero() && file.SessionKeyExpires.Before(time.Now()) {
		res.Status = StatusExpired
		res.Detail = "sessionKey expired " + file.SessionKeyExpires.Format("2006-01-02")
		return res
	}

	// Client comes from a shared pool keyed by proxy label: TCP/TLS connections
	// are reused across files. Sticky keeps one endpoint per file, rotating
	// still re-picks per request.
	getClient := func() (*http.Client, string) {
		p := pickProxy()
		return clientFor(p, timeout), proxyLabel(p)
	}
	cookieHeader := buildCookieHeader(file.Cookies)

	// Request 1: the org list alone decides the status.
	sc, body, isJSON, cfMit, label, err := httpGet(ctx, getClient, endpoint+"/api/organizations", cookieHeader, endpoint)
	res.Proxy = label
	if err != nil {
		res.Status = StatusError
		res.Detail = err.Error()
		return res
	}
	switch {
	case sc == http.StatusOK && isJSON:
		var orgs []struct {
			UUID string `json:"uuid"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(body, &orgs); err != nil {
			res.Status = StatusError
			res.Detail = "unexpected response shape"
			return res
		}
		names := make([]string, 0, len(orgs))
		for _, o := range orgs {
			names = append(names, o.Name)
		}
		res.Status = StatusValid
		res.Orgs = strings.Join(names, ", ")
	case sc == http.StatusUnauthorized:
		res.Status = StatusInvalid
		res.Detail = "unauthorized"
	case sc == http.StatusForbidden && cfMit != "":
		// cf-mitigated is authoritative for challenges, even over a JSON body.
		res.Status = StatusBlocked
		res.Detail = "cloudflare challenge"
	case sc == http.StatusForbidden && isJSON:
		// Live claude.ai reports an invalid session as 403 JSON
		// {"error":{"type":"permission_error","details":{"error_code":"account_session_invalid"}}},
		// not 401. Any parseable JSON error body counts as an auth failure.
		if detail := apiErrorDetail(body); detail != "" {
			res.Status = StatusInvalid
			res.Detail = detail
		} else {
			res.Status = StatusError
			res.Detail = fmt.Sprintf("HTTP %d", sc)
		}
	case sc == http.StatusForbidden:
		// Non-JSON 403 without cf-mitigated: classic HTML challenge page.
		res.Status = StatusBlocked
		res.Detail = "cloudflare challenge"
	case sc == http.StatusTooManyRequests:
		res.Status = StatusRateLimited
		res.Detail = "rate limited"
	default:
		res.Status = StatusError
		res.Detail = fmt.Sprintf("HTTP %d", sc)
		return res
	}
	if res.Status != StatusValid {
		return res
	}

	// Request 2 (valid only): account details from /api/account. Failure never
	// downgrades VALID; it is reported in Detail instead.
	sc, body, isJSON, _, _, err = httpGet(ctx, getClient, endpoint+"/api/account", cookieHeader, endpoint)
	if err != nil {
		res.Detail = "account lookup failed: " + err.Error()
		return res
	}
	if sc != http.StatusOK {
		// Rate limits and other secondary failures keep the VALID status but
		// must not be silent: the row shows why email/plan are empty.
		res.Detail = fmt.Sprintf("account lookup: HTTP %d", sc)
		return res
	}
	if !isJSON {
		res.Detail = "account lookup failed: non-JSON body"
		return res
	}
	var acct struct {
		EmailAddress string `json:"email_address"`
		FullName     string `json:"full_name"`
		Memberships  []struct {
			SeatTier     string `json:"seat_tier"`
			Organization struct {
				Name            string `json:"name"`
				PlanDisplayName string `json:"plan_display_name"`
				RateLimitTier   string `json:"rate_limit_tier"`
			} `json:"organization"`
		} `json:"memberships"`
	}
	if err := json.Unmarshal(body, &acct); err != nil {
		res.Detail = "account lookup failed: unexpected response shape"
		return res
	}
	res.Email = acct.EmailAddress
	if len(acct.Memberships) > 0 {
		org := acct.Memberships[0].Organization
		res.Plan = firstNonEmpty(org.PlanDisplayName, org.RateLimitTier, acct.Memberships[0].SeatTier)
	}
	return res
}

// apiErrorDetail extracts a human detail from a claude.ai JSON error body:
// "<message> (<error_code>)". Empty if the body is not a parseable error.
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
	client, label := getClient()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil, false, "", label, err
	}
	for k, v := range browserHeaders(endpoint) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Cookie", cookieHeader)
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, false, "", label, err
	}
	defer resp.Body.Close()
	cfMitigated = resp.Header.Get("cf-mitigated")
	body, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, false, cfMitigated, label, err
	}
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	isJSON = (len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[')) ||
		strings.Contains(resp.Header.Get("Content-Type"), "json")
	return resp.StatusCode, body, isJSON, cfMitigated, label, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
