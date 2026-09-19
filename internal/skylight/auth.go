package skylight

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Skylight has no public API. These constants mirror the OAuth2 client used
// by the official web/mobile app (app.ourskylight.com).
// authBase is a var so tests can point at a fake server.
var authBase = "https://app.ourskylight.com"

const (
	clientID    = "skylight-mobile"
	scope       = "everything"
	redirectURI = "https://ourskylight.com/welcome"

	browserUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
)

// TokenResponse is the OAuth2 token endpoint payload.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

// Expiry converts ExpiresIn to an absolute time, shaved by a safety margin.
func (t TokenResponse) Expiry(now time.Time) time.Time {
	d := time.Duration(t.ExpiresIn) * time.Second
	if d <= 0 {
		d = time.Hour
	}
	return now.Add(d - time.Minute)
}

var csrfRe = []*regexp.Regexp{
	regexp.MustCompile(`name="csrf-token"\s+content="([^"]+)"`),
	regexp.MustCompile(`name="authenticity_token"[^>]*value="([^"]+)"`),
	regexp.MustCompile(`value="([^"]+)"[^>]*name="authenticity_token"`),
}

// Login performs a headless authorization-code login with email/password:
//
//  1. GET  /auth/session/new     -> Rails CSRF token + session cookie
//  2. POST /auth/session         -> authenticated session cookie
//  3. GET  /oauth/authorize      -> 302 to redirect_uri?code=...
//  4. POST /oauth/token          -> access + refresh tokens
//
// fingerprint is a stable per-installation UUID sent as the device identifier.
func Login(ctx context.Context, email, password, fingerprint string) (*TokenResponse, error) {
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// 1. CSRF token
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, authBase+"/auth/session/new", nil)
	setBrowserHeaders(req)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching login page: %w", err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var csrf string
	for _, re := range csrfRe {
		if m := re.FindSubmatch(page); len(m) > 1 {
			csrf = string(m[1])
			break
		}
	}
	if csrf == "" {
		return nil, fmt.Errorf("login page (status %d) did not contain a CSRF token; Skylight may have changed its login flow", resp.StatusCode)
	}

	// 2. Credentials
	form := url.Values{
		"authenticity_token": {csrf},
		"email":              {email},
		"password":           {password},
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodPost, authBase+"/auth/session", strings.NewReader(form.Encode()))
	setBrowserHeaders(req)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", authBase)
	req.Header.Set("Referer", authBase+"/auth/session/new")
	resp, err = hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("posting credentials: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("login rejected (status %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if resp.StatusCode == http.StatusOK && strings.Contains(string(page), "authenticity_token") && strings.Contains(string(body), "authenticity_token") {
		// A 200 that re-renders the form means bad credentials.
		return nil, fmt.Errorf("login rejected: invalid email or password")
	}

	// 3. Authorization code
	q := url.Values{
		"client_id":                              {clientID},
		"response_type":                          {"code"},
		"redirect_uri":                           {redirectURI},
		"scope":                                  {scope},
		"skylight_api_client_device_fingerprint": {fingerprint},
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, authBase+"/oauth/authorize?"+q.Encode(), nil)
	setBrowserHeaders(req)
	resp, err = hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("requesting authorization code: %w", err)
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || loc.Query().Get("code") == "" {
		return nil, fmt.Errorf("authorize did not redirect with a code (status %d, location %q)", resp.StatusCode, resp.Header.Get("Location"))
	}

	// 4. Token exchange
	return postToken(ctx, url.Values{
		"grant_type":                             {"authorization_code"},
		"code":                                   {loc.Query().Get("code")},
		"client_id":                              {clientID},
		"redirect_uri":                           {redirectURI},
		"scope":                                  {scope},
		"skylight_api_client_device_fingerprint": {fingerprint},
	})
}

// Refresh exchanges a refresh token for a new token pair. Skylight rotates
// refresh tokens; the caller must persist the returned one.
func Refresh(ctx context.Context, refreshToken, fingerprint string) (*TokenResponse, error) {
	return postToken(ctx, url.Values{
		"grant_type":                             {"refresh_token"},
		"refresh_token":                          {refreshToken},
		"client_id":                              {clientID},
		"skylight_api_client_device_fingerprint": {fingerprint},
	})
}

func postToken(ctx context.Context, form url.Values) (*TokenResponse, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, authBase+"/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", browserUA)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token request failed (status %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var t TokenResponse
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, fmt.Errorf("parsing token response: %w", err)
	}
	if t.AccessToken == "" {
		return nil, fmt.Errorf("token response missing access_token")
	}
	return &t, nil
}

func setBrowserHeaders(req *http.Request) {
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
}
