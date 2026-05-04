package accounts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRefreshStoredIfExpiredUsesFreshTokenWrittenByWinner(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir()}
	stale := storedOAuthAccount("founders@example.com", "old", time.Now().Add(-time.Hour))
	fresh := storedOAuthAccount("founders@example.com", "new", time.Now().Add(time.Hour))
	if err := store.SaveStored(fresh); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	client := &http.Client{Transport: codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResponse(http.StatusInternalServerError, `{}`), nil
	})}

	got, refreshed, err := store.RefreshStoredIfExpired(context.Background(), client, stale)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed {
		t.Fatal("expected refresh to be skipped")
	}
	if calls.Load() != 0 {
		t.Fatalf("refresh calls = %d, want 0", calls.Load())
	}
	if got.Auth.Tokens.RefreshToken != "new-refresh" {
		t.Fatalf("refresh token = %q, want new-refresh", got.Auth.Tokens.RefreshToken)
	}
}

func TestRefreshStoredIfExpiredSerializesConcurrentRefresh(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir()}
	stale := storedOAuthAccount("founders@example.com", "old", time.Now().Add(-time.Hour))
	if err := store.SaveStored(stale); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	client := &http.Client{Transport: codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return refreshResponse("new", "founders@example.com", time.Now().Add(time.Hour)), nil
	})}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	results := make(chan StoredCodexAccount, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, _, err := store.RefreshStoredIfExpired(context.Background(), client, stale)
			if err != nil {
				errs <- err
				return
			}
			results <- got
		}()
	}
	wg.Wait()
	close(errs)
	close(results)
	for err := range errs {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls.Load())
	}
	for got := range results {
		if got.Auth.Tokens.RefreshToken != "new-refresh" {
			t.Fatalf("refresh token = %q, want new-refresh", got.Auth.Tokens.RefreshToken)
		}
	}
}

func TestRefreshStoredIfExpiredRecoversFromRefreshTokenReuseAfterExternalWrite(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir()}
	stale := storedOAuthAccount("founders@example.com", "old", time.Now().Add(-time.Hour))
	if err := store.SaveStored(stale); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Transport: codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
		fresh := storedOAuthAccount("founders@example.com", "new", time.Now().Add(time.Hour))
		writeStoredCodexAccountFile(t, store, fresh)
		return jsonResponse(http.StatusUnauthorized, `{"error":{"code":"refresh_token_reused"}}`), nil
	})}

	got, refreshed, err := store.RefreshStoredIfExpired(context.Background(), client, stale)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed {
		t.Fatal("expected recovery without reporting this process as refreshed")
	}
	if got.Auth.Tokens.RefreshToken != "new-refresh" {
		t.Fatalf("refresh token = %q, want new-refresh", got.Auth.Tokens.RefreshToken)
	}
}

func TestRefreshStoredIfExpiredSyncsActiveAuthWhenActiveAccountRefreshes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := CodexStore{Dir: t.TempDir()}
	stale := storedOAuthAccount("founders@example.com", "old", time.Now().Add(-time.Hour))
	if err := store.SaveStored(stale); err != nil {
		t.Fatal(err)
	}
	if err := WriteActiveCodexAuth(stale.Auth); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Transport: codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return refreshResponse("new", "founders@example.com", time.Now().Add(time.Hour)), nil
	})}

	if _, _, err := store.RefreshStoredIfExpired(context.Background(), client, stale); err != nil {
		t.Fatal(err)
	}
	active, ok, err := ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing active auth")
	}
	if active.Tokens.RefreshToken != "new-refresh" {
		t.Fatalf("active refresh token = %q, want new-refresh", active.Tokens.RefreshToken)
	}
}

func TestSyncActiveToStoreDoesNotOverwriteNewerStoredToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir()}
	stale := storedOAuthAccount("founders@example.com", "old", time.Now().Add(-time.Hour))
	fresh := storedOAuthAccount("founders@example.com", "new", time.Now().Add(time.Hour))
	if err := store.SaveStored(fresh); err != nil {
		t.Fatal(err)
	}
	if err := WriteActiveCodexAuth(stale.Auth); err != nil {
		t.Fatal(err)
	}

	if err := store.SyncActiveToStore(); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.FindStored("founders@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("missing stored account")
	}
	if got.Auth.Tokens.RefreshToken != "new-refresh" {
		t.Fatalf("stored refresh token = %q, want new-refresh", got.Auth.Tokens.RefreshToken)
	}
}

type codexRoundTripFunc func(*http.Request) (*http.Response, error)

func (f codexRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func storedOAuthAccount(email, tokenPrefix string, exp time.Time) StoredCodexAccount {
	return StoredCodexAccount{
		Email:   email,
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth: CodexAuthFile{AuthMode: "chatgpt", Tokens: &CodexTokens{
			AccessToken:  testCodexJWT(email, tokenPrefix+"-access", exp),
			RefreshToken: tokenPrefix + "-refresh",
			IDToken:      testCodexJWT(email, tokenPrefix+"-id", exp),
		}},
	}
}

func writeStoredCodexAccountFile(t *testing.T, store CodexStore, account StoredCodexAccount) {
	t.Helper()
	if err := os.MkdirAll(store.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(account, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, '\n')
	if err := os.WriteFile(account.SourcePath(store), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func refreshResponse(tokenPrefix, email string, exp time.Time) *http.Response {
	body, _ := json.Marshal(map[string]string{
		"access_token":  testCodexJWT(email, tokenPrefix+"-access", exp),
		"refresh_token": tokenPrefix + "-refresh",
		"id_token":      testCodexJWT(email, tokenPrefix+"-id", exp),
	})
	return jsonResponse(http.StatusOK, string(body))
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func testCodexJWT(email, jwtID string, exp time.Time) string {
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	payload, _ := json.Marshal(map[string]any{
		"exp": exp.Unix(),
		"iat": time.Now().Add(-time.Minute).Unix(),
		"jti": jwtID,
		"https://api.openai.com/profile": map[string]any{
			"email": email,
		},
	})
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}
