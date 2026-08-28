// Package oauthdevice implements the OAuth 2.0 device authorization grant
// (RFC 8628), which is how coding-CLI subscriptions are signed into on a
// machine with no browser callback. Providers differ only in their endpoints
// and client, so the flow lives here once.
package oauthdevice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Config identifies one provider's device-flow endpoints and client.
type Config struct {
	ClientID string
	// ClientSecret is optional. Installed-app clients often carry one, and it
	// is not confidential; a provider that omits it simply leaves this empty.
	ClientSecret string
	// DeviceAuthURL issues device and user codes.
	DeviceAuthURL string
	// TokenURL exchanges an authorized device code, and later refresh tokens.
	TokenURL string
	Scope    string
	// ExtraAuthParams are provider-specific fields on the device-code request.
	ExtraAuthParams map[string]string
}

// Code is the device-authorization response. The caller shows UserCode and
// VerificationURI to the person signing in.
type Code struct {
	DeviceCode      string
	UserCode        string
	VerificationURI string
	// VerificationURIComplete embeds the user code so the person can follow one
	// link instead of typing the code. Empty when the provider does not send it.
	VerificationURIComplete string
	Interval                time.Duration
	ExpiresAt               time.Time
}

// Token is a successful token response.
type Token struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	TokenType    string
	Scope        string
	ExpiresAt    time.Time
}

const (
	// deviceCodeGrantType is the RFC 8628 grant type.
	deviceCodeGrantType = "urn:ietf:params:oauth:grant-type:device_code"
	// defaultInterval applies when the provider omits one, per RFC 8628 §3.2.
	defaultInterval = 5 * time.Second
	// slowDownIncrement is the interval bump RFC 8628 §3.5 requires on
	// slow_down. Ignoring it gets the client rate-limited out of the flow.
	slowDownIncrement = 5 * time.Second
	// defaultExpiry applies when the provider omits expires_in on the device code.
	defaultExpiry = 15 * time.Minute
)

// ErrAuthorizationDeclined reports that the person refused the sign-in.
var ErrAuthorizationDeclined = errors.New("device authorization was declined")

// ErrAuthorizationExpired reports that the device code expired before the
// person completed the sign-in.
var ErrAuthorizationExpired = errors.New("device authorization expired before it was approved")

type deviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIAlt      string `json:"verification_url"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	Interval                int64  `json:"interval"`
	ExpiresIn               int64  `json:"expires_in"`
}

type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	IDToken          string `json:"id_token"`
	TokenType        string `json:"token_type"`
	Scope            string `json:"scope"`
	ExpiresIn        int64  `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// RequestCode starts a sign-in and returns the codes to show the person.
func RequestCode(ctx context.Context, client *http.Client, cfg Config, now time.Time) (Code, error) {
	form := url.Values{"client_id": {cfg.ClientID}}
	if cfg.Scope != "" {
		form.Set("scope", cfg.Scope)
	}
	if cfg.ClientSecret != "" {
		form.Set("client_secret", cfg.ClientSecret)
	}
	for key, value := range cfg.ExtraAuthParams {
		form.Set(key, value)
	}
	body, err := postForm(ctx, client, cfg.DeviceAuthURL, form)
	if err != nil {
		return Code{}, err
	}
	var parsed deviceCodeResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Code{}, fmt.Errorf("device authorization returned an undecodable body: %w", err)
	}
	if strings.TrimSpace(parsed.DeviceCode) == "" {
		return Code{}, errors.New("device authorization returned no device code")
	}
	if strings.TrimSpace(parsed.UserCode) == "" {
		return Code{}, errors.New("device authorization returned no user code")
	}
	verificationURI := firstNonEmpty(parsed.VerificationURI, parsed.VerificationURIAlt)
	if strings.TrimSpace(verificationURI) == "" {
		return Code{}, errors.New("device authorization returned no verification URI")
	}
	code := Code{
		DeviceCode:              parsed.DeviceCode,
		UserCode:                parsed.UserCode,
		VerificationURI:         verificationURI,
		VerificationURIComplete: parsed.VerificationURIComplete,
		Interval:                defaultInterval,
		ExpiresAt:               now.Add(defaultExpiry),
	}
	if parsed.Interval > 0 {
		code.Interval = time.Duration(parsed.Interval) * time.Second
	}
	if parsed.ExpiresIn > 0 {
		code.ExpiresAt = now.Add(time.Duration(parsed.ExpiresIn) * time.Second)
	}
	return code, nil
}

// Poll waits for the person to approve the sign-in and returns the token.
// It honours the provider's polling interval and the slow_down backoff RFC 8628
// requires, and gives up when the device code expires.
func Poll(ctx context.Context, client *http.Client, cfg Config, code Code, sleep func(context.Context, time.Duration) error, now func() time.Time) (Token, error) {
	if sleep == nil {
		sleep = sleepContext
	}
	if now == nil {
		now = time.Now
	}
	interval := code.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	for {
		if !code.ExpiresAt.IsZero() && !now().Before(code.ExpiresAt) {
			return Token{}, ErrAuthorizationExpired
		}
		if err := sleep(ctx, interval); err != nil {
			return Token{}, err
		}
		if !code.ExpiresAt.IsZero() && !now().Before(code.ExpiresAt) {
			return Token{}, ErrAuthorizationExpired
		}
		form := url.Values{
			"client_id":   {cfg.ClientID},
			"device_code": {code.DeviceCode},
			"grant_type":  {deviceCodeGrantType},
		}
		if cfg.ClientSecret != "" {
			form.Set("client_secret", cfg.ClientSecret)
		}
		token, retry, err := exchange(ctx, client, cfg.TokenURL, form, now())
		if err == nil {
			return token, nil
		}
		if !retry {
			return Token{}, err
		}
		if errors.Is(err, errSlowDown) {
			interval += slowDownIncrement
		}
	}
}

// Refresh exchanges a refresh token for a new access token.
func Refresh(ctx context.Context, client *http.Client, cfg Config, refreshToken string, now time.Time) (Token, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return Token{}, errors.New("no refresh token")
	}
	form := url.Values{
		"client_id":     {cfg.ClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}
	if cfg.ClientSecret != "" {
		form.Set("client_secret", cfg.ClientSecret)
	}
	token, _, err := exchange(ctx, client, cfg.TokenURL, form, now)
	if err != nil {
		return Token{}, err
	}
	// A provider that does not rotate the refresh token omits it; keeping the
	// caller's avoids blanking a credential that is still valid.
	if token.RefreshToken == "" {
		token.RefreshToken = refreshToken
	}
	return token, nil
}

// errSlowDown marks the retryable slow_down response so Poll can back off.
var errSlowDown = errors.New("slow_down")

// errAuthorizationPending marks the ordinary "not approved yet" response.
var errAuthorizationPending = errors.New("authorization_pending")

// exchange posts a token request and classifies the response. retry reports
// whether Poll should keep waiting.
func exchange(ctx context.Context, client *http.Client, tokenURL string, form url.Values, now time.Time) (token Token, retry bool, err error) {
	body, httpErr := postForm(ctx, client, tokenURL, form)
	var parsed tokenResponse
	if len(body) > 0 {
		_ = json.Unmarshal(body, &parsed)
	}
	switch parsed.Error {
	case "authorization_pending":
		return Token{}, true, errAuthorizationPending
	case "slow_down":
		return Token{}, true, errSlowDown
	case "access_denied":
		return Token{}, false, ErrAuthorizationDeclined
	case "expired_token":
		return Token{}, false, ErrAuthorizationExpired
	}
	if parsed.Error != "" {
		if httpErr != nil {
			return Token{}, false, fmt.Errorf("device flow token error %s: %w", parsed.Error, httpErr)
		}
		return Token{}, false, fmt.Errorf("device flow token error %s", parsed.Error)
	}
	if httpErr != nil {
		return Token{}, false, httpErr
	}
	if strings.TrimSpace(parsed.AccessToken) == "" {
		return Token{}, false, errors.New("token response carried no access token")
	}
	token = Token{
		AccessToken:  parsed.AccessToken,
		RefreshToken: parsed.RefreshToken,
		IDToken:      parsed.IDToken,
		TokenType:    parsed.TokenType,
		Scope:        parsed.Scope,
	}
	if parsed.ExpiresIn > 0 {
		token.ExpiresAt = now.Add(time.Duration(parsed.ExpiresIn) * time.Second).UTC()
	}
	return token, false, nil
}

func postForm(ctx context.Context, client *http.Client, endpoint string, form url.Values) ([]byte, error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if readErr != nil {
		return nil, readErr
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// The body is returned alongside the error because RFC 8628 signals
		// authorization_pending and slow_down with a 400.
		return body, fmt.Errorf("device flow request to %s failed: %s", endpoint, res.Status)
	}
	return body, nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
