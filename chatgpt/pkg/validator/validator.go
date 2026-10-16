package validator

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"chatgpt-validator/pkg/cookie"
	"chatgpt-validator/pkg/proxy"
)

// Status classifies the outcome of validating a cookie file.
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
	StatusEmpty
	StatusRetry // Transient Network/DNS error: file is retained in place
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
	case StatusEmpty:
		return "EMPTY"
	case StatusRetry:
		return "RETRY"
	default:
		return "UNKNOWN"
	}
}

// Result holds the outcome of checking a cookie file.
type Result struct {
	Path      string
	MovedPath string
	Status    Status
	Email     string
	Plan      string
	Expiry    string
	Detail    string
	Proxy     string
	Elapsed   time.Duration
}

// GenerateUUID generates an RFC 4122 version 4 UUID.
func GenerateUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // Version 4
	b[8] = (b[8] & 0x3f) | 0x80 // Variant 10xx
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	)
}

// BrowserHeaders returns realistic Chrome browser headers.
func BrowserHeaders(endpoint, deviceID string) map[string]string {
	return map[string]string{
		"User-Agent":         "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
		"Accept-Language":    "en-US,en;q=0.9",
		"Origin":             endpoint,
		"Referer":            endpoint + "/",
		"Sec-Ch-Ua":          `"Chromium";v="124", "Google Chrome";v="124", "Not-A.Brand";v="99"`,
		"Sec-Ch-Ua-Mobile":   "?0",
		"Sec-Ch-Ua-Platform": `"Windows"`,
		"Sec-Fetch-Dest":     "empty",
		"Sec-Fetch-Mode":     "cors",
		"Sec-Fetch-Site":     "same-origin",
		"oai-device-id":      deviceID,
		"oai-language":       "en-US",
		"oai-client-version": "prod-f9b775c0e3ff13d0d7f21c9c6a1c1d3c",
	}
}

// IsNetworkOrDNSError checks if an error indicates a transient network or DNS issue.
func IsNetworkOrDNSError(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}

	msg := strings.ToLower(err.Error())
	indicators := []string{
		"could not resolve",
		"could not resolv",
		"resolve host",
		"no such host",
		"connection refused",
		"proxy error",
		"proxyconnect",
		"timed out",
		"timeout",
		"deadline exceeded",
		"network unreachable",
		"network is unreachable",
		"ssl_connect_error",
		"handshake failure",
		"connectex",
		"eof",
		"connection reset",
		"i/o timeout",
	}
	for _, ind := range indicators {
		if strings.Contains(msg, ind) {
			return true
		}
	}
	return false
}

// DecodeJWT extracts payload claims from a JWT token string.
func DecodeJWT(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil, errors.New("invalid jwt token")
	}
	payload := parts[1]

	// URL-safe base64 decoding with padding tolerance
	data, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		data, err = base64.URLEncoding.DecodeString(payload + strings.Repeat("=", (4-len(payload)%4)%4))
		if err != nil {
			return nil, err
		}
	}

	var claims map[string]any
	if err := json.Unmarshal(data, &claims); err != nil {
		return nil, err
	}
	return claims, nil
}

// FormatExpiry formats a unix timestamp into "YYYY-MM-DD HH:MM".
func FormatExpiry(exp any) string {
	var sec int64
	switch v := exp.(type) {
	case float64:
		sec = int64(v)
	case int64:
		sec = v
	case int:
		sec = int64(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			sec = n
		}
	default:
		return "Unknown"
	}
	if sec <= 0 {
		return "Unknown"
	}
	return time.Unix(sec, 0).Format("2006-01-02 15:04")
}

// CapitalizePlan standardizes plan strings (e.g. "plus" -> "Plus", "free" -> "Free").
func CapitalizePlan(plan string) string {
	plan = strings.TrimSpace(plan)
	if plan == "" {
		return "Free"
	}
	lower := strings.ToLower(plan)
	switch lower {
	case "free":
		return "Free"
	case "plus":
		return "Plus"
	case "team":
		return "Team"
	case "pro":
		return "Pro"
	case "enterprise":
		return "Enterprise"
	default:
		return strings.ToUpper(plan[:1]) + plan[1:]
	}
}

// CheckCookieFile validates one cookie file against the ChatGPT backend.
// It matches Python's retry loop with automatic fallback to direct machine
// connection if proxy fails due to network/DNS glitches.
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

	if len(file.Cookies) == 0 {
		res.Status = StatusEmpty
		res.Detail = "No cookies found"
		return
	}

	deviceID := file.Cookies["oai-did"]
	if deviceID == "" {
		deviceID = GenerateUUID()
	}

	cookieHeader := cookie.BuildCookieHeader(file.Cookies)
	baseHeaders := BrowserHeaders(endpoint, deviceID)

	const maxProxyAttempts = 3
	hasProxies := pickProxy() != nil
	maxTotalAttempts := 3
	if hasProxies {
		maxTotalAttempts = maxProxyAttempts + 2
	}

	useDirectFallback := false
	var lastNetworkErr error
	var lastProxyLabel string

	for attempt := 1; attempt <= maxTotalAttempts; attempt++ {
		var activeProxy *proxy.Proxy
		if hasProxies && !useDirectFallback && attempt <= maxProxyAttempts {
			activeProxy = pickProxy()
			lastProxyLabel = proxy.ProxyLabel(activeProxy)
		} else {
			useDirectFallback = true
			activeProxy = nil
			if hasProxies {
				lastProxyLabel = "direct"
			}
		}

		client := proxy.ClientFor(activeProxy, timeout)

		// -------------------------------------------------------------
		// Step 1: Query NextAuth session endpoint: GET /api/auth/session
		// -------------------------------------------------------------
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/auth/session", nil)
		if err != nil {
			res.Status = StatusError
			res.Detail = err.Error()
			return
		}
		for k, v := range baseHeaders {
			req.Header.Set(k, v)
		}
		req.Header.Set("Accept", "application/json")
		if cookieHeader != "" {
			req.Header.Set("Cookie", cookieHeader)
		}

		resp, err := client.Do(req)
		if err != nil {
			if IsNetworkOrDNSError(err) {
				lastNetworkErr = err
				if !useDirectFallback && attempt >= maxProxyAttempts {
					useDirectFallback = true
				}
				time.Sleep(300 * time.Millisecond)
				continue
			}
			res.Status = StatusError
			res.Detail = err.Error()
			return
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			// Check for Cloudflare challenge
			cfMitigated := resp.Header.Get("cf-mitigated")
			ct := resp.Header.Get("Content-Type")
			if strings.EqualFold(cfMitigated, "challenge") || (resp.StatusCode == http.StatusForbidden && strings.Contains(ct, "text/html")) {
				res.Status = StatusBlocked
				res.Detail = "cloudflare challenge"
				res.Proxy = lastProxyLabel
				return
			}

			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				res.Status = StatusInvalid
				res.Detail = fmt.Sprintf("HTTP %d Unauthorized", resp.StatusCode)
				res.Proxy = lastProxyLabel
				return
			}
			if resp.StatusCode == http.StatusTooManyRequests {
				res.Status = StatusRateLimited
				res.Detail = "rate limited"
				res.Proxy = lastProxyLabel
				return
			}

			if attempt < maxTotalAttempts {
				if attempt >= maxProxyAttempts {
					useDirectFallback = true
				}
				time.Sleep(300 * time.Millisecond)
				continue
			}

			res.Status = StatusError
			res.Detail = fmt.Sprintf("HTTP %d", resp.StatusCode)
			res.Proxy = lastProxyLabel
			return
		}

		// Parse session JSON
		var sessionData struct {
			AccessToken string `json:"accessToken"`
			User        *struct {
				Email string `json:"email"`
				Name  string `json:"name"`
			} `json:"user"`
			Account struct {
				PlanType string `json:"planType"`
				Plan_type string `json:"plan_type"`
			} `json:"account"`
		}

		trimmed := bytes.TrimSpace(body)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			res.Status = StatusInvalid
			res.Detail = "No active user session (Anonymous / Expired)"
			res.Proxy = lastProxyLabel
			return
		}

		if err := json.Unmarshal(body, &sessionData); err != nil {
			res.Status = StatusInvalid
			res.Detail = "No active user session (Anonymous / Expired)"
			res.Proxy = lastProxyLabel
			return
		}

		if sessionData.AccessToken == "" || sessionData.User == nil || sessionData.User.Email == "" {
			res.Status = StatusInvalid
			res.Detail = "No active user session (Anonymous / Expired)"
			res.Proxy = lastProxyLabel
			return
		}

		email := sessionData.User.Email
		accessToken := sessionData.AccessToken

		// Extract JWT claims
		var accountID string
		var jwtPlan string
		var expTS any
		claims, _ := DecodeJWT(accessToken)
		if claims != nil {
			if authObj, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
				if id, ok := authObj["chatgpt_account_id"].(string); ok {
					accountID = id
				}
				if p, ok := authObj["plan_type"].(string); ok {
					jwtPlan = p
				}
			}
			expTS = claims["exp"]
		}

		expiryStr := FormatExpiry(expTS)

		rawPlan := sessionData.Account.PlanType
		if rawPlan == "" {
			rawPlan = sessionData.Account.Plan_type
		}
		if rawPlan == "" {
			rawPlan = jwtPlan
		}
		planType := CapitalizePlan(rawPlan)

		// -------------------------------------------------------------
		// Step 2: Backend-API verification probe: /backend-api/me or /models
		// -------------------------------------------------------------
		backendOK := false
		authHeaders := make(map[string]string)
		for k, v := range baseHeaders {
			authHeaders[k] = v
		}
		authHeaders["Authorization"] = "Bearer " + accessToken
		authHeaders["Accept"] = "application/json"
		if accountID != "" {
			authHeaders["chatgpt-account-id"] = accountID
		}

		// Try /backend-api/me
		bReq, bErr := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/backend-api/me", nil)
		if bErr == nil {
			for k, v := range authHeaders {
				bReq.Header.Set(k, v)
			}
			if cookieHeader != "" {
				bReq.Header.Set("Cookie", cookieHeader)
			}
			bResp, err := client.Do(bReq)
			if err == nil {
				if bResp.StatusCode == http.StatusOK {
					backendOK = true
				}
				bResp.Body.Close()
			}
		}

		// Fallback to /backend-api/models if /me did not return 200
		if !backendOK {
			mReq, mErr := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/backend-api/models", nil)
			if mErr == nil {
				for k, v := range authHeaders {
					mReq.Header.Set(k, v)
				}
				if cookieHeader != "" {
					mReq.Header.Set("Cookie", cookieHeader)
				}
				mResp, err := client.Do(mReq)
				if err == nil {
					if mResp.StatusCode == http.StatusOK {
						backendOK = true
					}
					mResp.Body.Close()
				}
			}
		}

		if !backendOK {
			res.Status = StatusInvalid
			res.Detail = "Backend Token Rejected"
			res.Proxy = lastProxyLabel
			return
		}

		// Validation successful!
		res.Status = StatusValid
		res.Email = email
		res.Plan = planType
		res.Expiry = expiryStr
		res.Proxy = lastProxyLabel
		return
	}

	// All attempts exhausted due to network/DNS resolution
	if lastNetworkErr != nil {
		res.Status = StatusRetry
		errMsg := lastNetworkErr.Error()
		if len(errMsg) > 45 {
			errMsg = errMsg[:45]
		}
		res.Detail = fmt.Sprintf("Network/DNS Error: %s (Retained)", errMsg)
		res.Proxy = lastProxyLabel
		return
	}

	res.Status = StatusError
	res.Detail = "Connection failed"
	res.Proxy = lastProxyLabel
	return
}
