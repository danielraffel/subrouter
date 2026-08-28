package oauthdevice

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

var reference = time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

func testConfig(server *httptest.Server) Config {
	return Config{
		ClientID:      "client",
		ClientSecret:  "secret",
		DeviceAuthURL: server.URL + "/device",
		TokenURL:      server.URL + "/token",
		Scope:         "openid offline_access",
	}
}

func TestRequestCodeParsesTheDeviceAuthorization(t *testing.T) {
	var gotForm string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.Form.Encode()
		_, _ = w.Write([]byte(`{"device_code":"dc","user_code":"WXYZ-1234","verification_uri":"https://example.test/device","verification_uri_complete":"https://example.test/device?user_code=WXYZ-1234","interval":7,"expires_in":600}`))
	}))
	defer server.Close()

	code, err := RequestCode(context.Background(), server.Client(), testConfig(server), reference)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if code.DeviceCode != "dc" || code.UserCode != "WXYZ-1234" {
		t.Fatalf("codes did not round-trip: %+v", code)
	}
	if code.VerificationURI != "https://example.test/device" {
		t.Fatalf("VerificationURI = %q", code.VerificationURI)
	}
	if code.Interval != 7*time.Second {
		t.Fatalf("Interval = %s, want the provider's 7s", code.Interval)
	}
	if want := reference.Add(10 * time.Minute); !code.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %s, want %s", code.ExpiresAt, want)
	}
	for _, want := range []string{"client_id=client", "scope=openid", "client_secret=secret"} {
		if !contains(gotForm, want) {
			t.Fatalf("device request form %q is missing %q", gotForm, want)
		}
	}
}

// Some providers spell it verification_url. Falling back matters because an
// empty URI means the person has nowhere to go.
func TestRequestCodeAcceptsTheAlternateVerificationField(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"device_code":"dc","user_code":"U","verification_url":"https://alt.test/device"}`))
	}))
	defer server.Close()

	code, err := RequestCode(context.Background(), server.Client(), testConfig(server), reference)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if code.VerificationURI != "https://alt.test/device" {
		t.Fatalf("VerificationURI = %q, want the verification_url fallback", code.VerificationURI)
	}
	// Defaults apply when the provider omits them.
	if code.Interval != defaultInterval {
		t.Fatalf("Interval = %s, want the RFC default", code.Interval)
	}
	if want := reference.Add(defaultExpiry); !code.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %s, want the default expiry", code.ExpiresAt)
	}
}

func TestRequestCodeRejectsIncompleteResponses(t *testing.T) {
	for _, body := range []string{
		`{"user_code":"U","verification_uri":"https://example.test"}`,
		`{"device_code":"D","verification_uri":"https://example.test"}`,
		`{"device_code":"D","user_code":"U"}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
		_, err := RequestCode(context.Background(), server.Client(), testConfig(server), reference)
		server.Close()
		if err == nil {
			t.Fatalf("incomplete response %s should fail", body)
		}
	}
}

// authorization_pending is the ordinary case: keep polling until approval.
func TestPollWaitsThroughAuthorizationPending(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer server.Close()

	token, err := Poll(context.Background(), server.Client(), testConfig(server),
		Code{DeviceCode: "dc", Interval: time.Millisecond, ExpiresAt: reference.Add(time.Hour)},
		noSleep, func() time.Time { return reference })
	if err != nil {
		t.Fatalf("poll failed: %v", err)
	}
	if token.AccessToken != "at" || token.RefreshToken != "rt" {
		t.Fatalf("token did not round-trip: %+v", token)
	}
	if want := reference.Add(time.Hour); !token.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %s, want %s", token.ExpiresAt, want)
	}
	if calls != 3 {
		t.Fatalf("polled %d times, want 3", calls)
	}
}

// RFC 8628 §3.5 requires the interval to grow on slow_down. Ignoring it gets
// the client rate-limited out of its own sign-in.
func TestPollBacksOffOnSlowDown(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"slow_down"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"at","expires_in":60}`))
	}))
	defer server.Close()

	var waited []time.Duration
	record := func(_ context.Context, d time.Duration) error {
		waited = append(waited, d)
		return nil
	}
	if _, err := Poll(context.Background(), server.Client(), testConfig(server),
		Code{DeviceCode: "dc", Interval: 5 * time.Second, ExpiresAt: reference.Add(time.Hour)},
		record, func() time.Time { return reference }); err != nil {
		t.Fatalf("poll failed: %v", err)
	}
	if len(waited) != 2 {
		t.Fatalf("waited %v, want two intervals", waited)
	}
	if waited[0] != 5*time.Second {
		t.Fatalf("first interval = %s, want the provider's 5s", waited[0])
	}
	if waited[1] != 10*time.Second {
		t.Fatalf("second interval = %s, want 5s + the RFC's 5s increment", waited[1])
	}
}

func TestPollStopsOnDeclinedAndExpired(t *testing.T) {
	cases := []struct {
		name string
		body string
		want error
	}{
		{name: "declined", body: `{"error":"access_denied"}`, want: ErrAuthorizationDeclined},
		{name: "expired", body: `{"error":"expired_token"}`, want: ErrAuthorizationExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			_, err := Poll(context.Background(), server.Client(), testConfig(server),
				Code{DeviceCode: "dc", Interval: time.Millisecond, ExpiresAt: reference.Add(time.Hour)},
				noSleep, func() time.Time { return reference })
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if calls != 1 {
				t.Fatalf("polled %d times, want to stop after the first refusal", calls)
			}
		})
	}
}

// A device code that has run out must stop the loop locally rather than poll a
// dead code forever.
func TestPollGivesUpOnceTheDeviceCodeExpires(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
	}))
	defer server.Close()

	_, err := Poll(context.Background(), server.Client(), testConfig(server),
		Code{DeviceCode: "dc", Interval: time.Millisecond, ExpiresAt: reference.Add(-time.Second)},
		noSleep, func() time.Time { return reference })
	if !errors.Is(err, ErrAuthorizationExpired) {
		t.Fatalf("err = %v, want ErrAuthorizationExpired", err)
	}
	if calls != 0 {
		t.Fatalf("polled %d times, want none once the code has expired", calls)
	}
}

func TestPollRechecksExpiryAfterWaiting(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()
	now := reference
	advance := func(context.Context, time.Duration) error {
		now = reference.Add(time.Second)
		return nil
	}
	_, err := Poll(context.Background(), server.Client(), testConfig(server),
		Code{DeviceCode: "dc", Interval: time.Second, ExpiresAt: reference.Add(time.Second)},
		advance, func() time.Time { return now })
	if !errors.Is(err, ErrAuthorizationExpired) || calls != 0 {
		t.Fatalf("err = %v, calls = %d; want local expiry before polling", err, calls)
	}
}

func TestPollDoesNotCapSlowDownBackoff(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls <= 2 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"slow_down"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"at"}`))
	}))
	defer server.Close()
	var waited []time.Duration
	_, err := Poll(context.Background(), server.Client(), testConfig(server),
		Code{DeviceCode: "dc", Interval: 58 * time.Second, ExpiresAt: reference.Add(time.Hour)},
		func(_ context.Context, d time.Duration) error { waited = append(waited, d); return nil },
		func() time.Time { return reference })
	if err != nil || len(waited) != 3 || waited[2] != 68*time.Second {
		t.Fatalf("err = %v, waits = %v; want uncapped 58s, 63s, 68s", err, waited)
	}
}

func TestPollHonoursContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Poll(ctx, server.Client(), testConfig(server),
		Code{DeviceCode: "dc", Interval: time.Millisecond, ExpiresAt: reference.Add(time.Hour)},
		sleepContext, func() time.Time { return reference })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestRefreshPreservesANonRotatedRefreshToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"fresh","expires_in":3600}`))
	}))
	defer server.Close()

	token, err := Refresh(context.Background(), server.Client(), testConfig(server), "original", reference)
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if token.RefreshToken != "original" {
		t.Fatalf("RefreshToken = %q, want the caller's token preserved", token.RefreshToken)
	}
	if token.AccessToken != "fresh" {
		t.Fatalf("AccessToken = %q", token.AccessToken)
	}
}

func TestRefreshAdoptsARotatedRefreshToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"fresh","refresh_token":"rotated","expires_in":3600}`))
	}))
	defer server.Close()

	token, err := Refresh(context.Background(), server.Client(), testConfig(server), "original", reference)
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if token.RefreshToken != "rotated" {
		t.Fatalf("RefreshToken = %q, want the rotated token", token.RefreshToken)
	}
}

func TestRefreshSurfacesInvalidGrant(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"expired or revoked"}`))
	}))
	defer server.Close()

	_, err := Refresh(context.Background(), server.Client(), testConfig(server), "dead", reference)
	if err == nil {
		t.Fatal("a revoked refresh token must be an error")
	}
	if !contains(err.Error(), "invalid_grant") {
		t.Fatalf("error %q must carry invalid_grant so the proxy marks the account for re-auth", err)
	}
}

func TestRefreshRejectsAnEmptyToken(t *testing.T) {
	if _, err := Refresh(context.Background(), nil, Config{}, "  ", reference); err == nil {
		t.Fatal("an empty refresh token must be rejected before any request")
	}
}

func noSleep(context.Context, time.Duration) error { return nil }

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || len(needle) == 0 ||
		func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}())
}
