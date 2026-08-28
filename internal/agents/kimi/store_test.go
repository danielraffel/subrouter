package kimi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
	"github.com/manaflow-ai/subrouter/internal/oauthdevice"
)

var reference = time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

func TestParseCredentialReadsTheCLIFileShape(t *testing.T) {
	credential, err := ParseCredential([]byte(
		`{"access_token":"at","refresh_token":"rt","expires_at":1787144400,"scope":"kimi-code","token_type":"Bearer","expires_in":900}`),
		"test", reference)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if credential.AccessToken != "at" || credential.RefreshToken != "rt" {
		t.Fatal("credential tokens did not round-trip")
	}
	if want := time.Unix(1787144400, 0).UTC(); !credential.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %s, want %s", credential.ExpiresAt, want)
	}
	if credential.Scope != "kimi-code" || credential.TokenType != "Bearer" {
		t.Fatalf("scope/token_type = %q/%q", credential.Scope, credential.TokenType)
	}
}

// expires_in is relative, so it only means anything against a clock.
func TestParseCredentialFallsBackToRelativeExpiry(t *testing.T) {
	credential, err := ParseCredential([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":900}`), "test", reference)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if want := reference.Add(900 * time.Second); !credential.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %s, want %s", credential.ExpiresAt, want)
	}
}

// An unparseable expiry must fail loudly. Falling back to a zero value would
// silently mark every credential expired and refresh on every single request.
func TestParseCredentialRejectsAnUnreadableExpiry(t *testing.T) {
	_, err := ParseCredential([]byte(`{"access_token":"at","refresh_token":"rt","expires_at":"tomorrow"}`), "test", reference)
	if err == nil {
		t.Fatal("a non-numeric expires_at must be an error, not a silent zero")
	}
	if !strings.Contains(err.Error(), unreadableCredentialPhrase) {
		t.Fatalf("error %q should carry the unreadable-credential phrase so the proxy classifies it terminal", err)
	}
}

func TestUnreadableExpiryDoesNotLeakItsValue(t *testing.T) {
	secret := "token-like-secret-value"
	_, err := ParseCredential([]byte(`{"access_token":"at","expires_at":"`+secret+`"}`), "test", reference)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("invalid expiry error must be redacted")
	}
}

func TestParseCredentialRejectsABlobWithNoTokens(t *testing.T) {
	if _, err := ParseCredential([]byte(`{"token_type":"Bearer"}`), "test", reference); err == nil {
		t.Fatal("a credential with neither token must be rejected")
	}
}

// The decode error must name its source and shape, and must not echo the blob.
func TestParseCredentialReportsShapeWithoutLeaking(t *testing.T) {
	body := []byte(`{"access_token":"eyJ.secret-value","refresh_token":"eyJ.secret-refresh"}` + "bplist00")
	_, err := ParseCredential(body, "kimi-code.json", reference)
	if err == nil {
		t.Fatal("trailing bytes must not decode")
	}
	message := err.Error()
	for _, want := range []string{unreadableCredentialPhrase, "from kimi-code.json", "trailing_kind=binary-plist"} {
		if !strings.Contains(message, want) {
			t.Fatalf("error %q is missing %q", message, want)
		}
	}
	for _, secret := range []string{"eyJ.secret-value", "eyJ.secret-refresh"} {
		if strings.Contains(message, secret) {
			t.Fatalf("error leaked a secret: %q", message)
		}
	}
}

func TestNeedsRefresh(t *testing.T) {
	live := CredentialInfo{AccessToken: "at", ExpiresAt: reference.Add(10 * time.Minute)}
	if live.NeedsRefresh(reference) {
		t.Fatal("a token with 10 minutes left does not need a refresh")
	}
	// The access token lives 900 seconds, so "inside the lead" is a third of
	// its life.
	soon := CredentialInfo{AccessToken: "at", ExpiresAt: reference.Add(4 * time.Minute)}
	if !soon.NeedsRefresh(reference) {
		t.Fatal("a token inside the five-minute lead must be refreshed before use")
	}
	unknown := CredentialInfo{AccessToken: "at"}
	if !unknown.NeedsRefresh(reference) {
		t.Fatal("a credential with no stated expiry must be refreshed rather than trusted")
	}
}

// writeCredentialFile seeds a CLI credential file for a test store.
func writeCredentialFile(t *testing.T, payload string) Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kimi-code.json")
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	return Store{Path: path}
}

func TestListAccountsReportsSignedOutWhenTheFileIsAbsent(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "kimi-code.json")}
	accounts, err := store.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(accounts) != 0 {
		t.Fatalf("got %d accounts, want none", len(accounts))
	}
}

func TestListAccountsSurfacesTheCLICredential(t *testing.T) {
	store := writeCredentialFile(t, credentialFileJSON("at", "rt", time.Now().Add(time.Hour)))
	accounts, err := store.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("got %d accounts, want 1", len(accounts))
	}
	acct := accounts[0]
	if acct.Provider != account.ProviderKimi || acct.AuthMode != account.AuthModeOAuth {
		t.Fatalf("account identity = %s/%s, want kimi/oauth", acct.Provider, acct.AuthMode)
	}
	if acct.Token != "at" {
		t.Fatal("account did not carry the expected access token")
	}
}

func TestReadLocalCredentialAnchorsRelativeExpiryToFileTime(t *testing.T) {
	store := writeCredentialFile(t, `{"access_token":"at","refresh_token":"rt","expires_in":900}`)
	issuedAt := reference.Add(-time.Hour)
	if err := os.Chtimes(store.Path, issuedAt, issuedAt); err != nil {
		t.Fatal(err)
	}
	credential, ok, err := store.ReadLocalCredential(reference)
	if err != nil || !ok {
		t.Fatalf("read failed: ok=%v err=%v", ok, err)
	}
	if !credential.ExpiresAt.Equal(issuedAt.Add(15*time.Minute)) || !credential.NeedsRefresh(reference) {
		t.Fatalf("relative expiry was not anchored to the stable file time")
	}
}

// credentialFileJSON renders a CLI credential file expiring at the given time.
func credentialFileJSON(accessToken, refreshToken string, expiresAt time.Time) string {
	return `{"access_token":"` + accessToken + `","refresh_token":"` + refreshToken +
		`","expires_at":` + strconv.FormatInt(expiresAt.Unix(), 10) + `,"token_type":"Bearer"}`
}

func stubOAuthConfig(t *testing.T, tokenURL string) {
	t.Helper()
	restore := oauthConfig
	oauthConfig = oauthdevice.Config{ClientID: "test-client", TokenURL: tokenURL}
	t.Cleanup(func() { oauthConfig = restore })
}

func TestRefreshAccountKeepsAFreshCredential(t *testing.T) {
	store := writeCredentialFile(t, credentialFileJSON("at", "rt", time.Now().Add(time.Hour)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("a credential with life left must not be refreshed")
	}))
	defer server.Close()
	stubOAuthConfig(t, server.URL)

	refreshed, err := store.RefreshAccount(context.Background(), server.Client(), account.Account{ID: accountID, Provider: account.ProviderKimi})
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if refreshed.Token != "at" {
		t.Fatal("refresh changed the still-valid access token")
	}
}

// The refresh must write the rotated tokens back over the CLI's file, or the
// CLI keeps a dead refresh token and gets logged out.
func TestRefreshAccountRefreshesAndWritesBack(t *testing.T) {
	store := writeCredentialFile(t,
		strings.TrimSuffix(credentialFileJSON("stale", "rt", time.Now().Add(-time.Hour)), "}")+`,"scope":"kimi-code"}`)
	var gotForm string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		gotForm = r.Form.Encode()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fresh-access",
			"refresh_token": "rotated-refresh",
			"expires_in":    900,
			"token_type":    "Bearer",
		})
	}))
	defer server.Close()
	stubOAuthConfig(t, server.URL)

	expired := account.Account{ID: accountID, Provider: account.ProviderKimi, Token: "stale"}
	refreshed, err := store.RefreshAccount(context.Background(), server.Client(), expired)
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if refreshed.Token != "fresh-access" {
		t.Fatal("refresh did not return the new access token")
	}
	for _, want := range []string{"grant_type=refresh_token", "refresh_token=rt", "client_id=test-client"} {
		if !strings.Contains(gotForm, want) {
			t.Fatalf("request form %q is missing %q", gotForm, want)
		}
	}

	// The CLI's file must now hold the rotated pair.
	credential, ok, err := store.ReadLocalCredential(time.Now())
	if err != nil || !ok {
		t.Fatalf("re-read failed: ok=%v err=%v", ok, err)
	}
	if credential.AccessToken != "fresh-access" || credential.RefreshToken != "rotated-refresh" {
		t.Fatal("written credential did not contain the rotated pair")
	}
	if credential.ExpiresAt.IsZero() {
		t.Fatal("written credential lost its expiry")
	}
}

// A refresh response that omits the refresh token must not blank the stored
// one.
func TestRefreshAccountKeepsAnUnrotatedRefreshToken(t *testing.T) {
	store := writeCredentialFile(t, credentialFileJSON("stale", "rt", time.Now().Add(-time.Hour)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh-access", "expires_in": 900})
	}))
	defer server.Close()
	stubOAuthConfig(t, server.URL)

	if _, err := store.RefreshAccount(context.Background(), server.Client(), account.Account{ID: accountID}); err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	credential, _, err := store.ReadLocalCredential(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if credential.RefreshToken != "rt" {
		t.Fatal("existing refresh token was not preserved")
	}
}

// A rejected refresh token comes back as a 401/403, which the proxy classifies
// as terminal for the account.
func TestRefreshAccountSurfacesARejectedRefreshToken(t *testing.T) {
	store := writeCredentialFile(t, credentialFileJSON("stale", "dead", time.Now().Add(-time.Hour)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer server.Close()
	stubOAuthConfig(t, server.URL)

	_, err := store.RefreshAccount(context.Background(), server.Client(), account.Account{ID: accountID})
	if err == nil {
		t.Fatal("a 401 on refresh must be an error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error %q must carry the status so the proxy marks the account for re-auth", err)
	}
}
