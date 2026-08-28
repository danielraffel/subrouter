package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

const (
	// keychainService and keychainAccount address the generic-password item the
	// Antigravity CLI writes on macOS. The service is "gemini" for historical
	// reasons: the CLI was Gemini CLI before it was renamed.
	keychainService = "gemini"
	keychainAccount = "antigravity"

	// keychainReadTimeout bounds the `security` call, which can block on a
	// locked keychain or an access-control prompt.
	keychainReadTimeout = 5 * time.Second
)

// oauthTokenURL is a variable so tests can point the refresh at a stub server.
var oauthTokenURL = "https://oauth2.googleapis.com/token"

// oauthClient is a public installed-app OAuth client the Antigravity CLI was
// built with. Refreshing a credential the CLI issued requires presenting the
// client it was issued to. The values are not committed to source: they are
// read from the installed CLI binary, or from the
// SUBROUTER_ANTIGRAVITY_CLIENT_ID / SUBROUTER_ANTIGRAVITY_CLIENT_SECRET
// environment variables when both are set.
type oauthClient struct {
	id     string
	secret string
}

// oauthClientsForRefresh is a variable so tests can stub the candidate list.
var oauthClientsForRefresh = defaultOAuthClients

// workingClient caches the candidate that last refreshed successfully, so a
// binary carrying several clients pays the trial cost once per process.
var workingClient atomic.Pointer[oauthClient]

func defaultOAuthClients() []oauthClient {
	if client, ok := oauthClientFromEnv(); ok {
		return []oauthClient{client}
	}
	return oauthClientsFromBinary(agyBinaryPath())
}

// oauthClientFromEnv reports the explicitly configured client, if any. A
// half-set pair is ignored rather than used, because presenting a client id
// with the wrong secret is indistinguishable from a dead account upstream.
func oauthClientFromEnv() (oauthClient, bool) {
	client := oauthClient{
		id:     strings.TrimSpace(os.Getenv("SUBROUTER_ANTIGRAVITY_CLIENT_ID")),
		secret: strings.TrimSpace(os.Getenv("SUBROUTER_ANTIGRAVITY_CLIENT_SECRET")),
	}
	return client, client.id != "" && client.secret != ""
}

// agyBinaryPath locates the installed Antigravity CLI. PATH first, then the
// install location its installer uses, because launchd runs this daemon with a
// sparse PATH.
func agyBinaryPath() string {
	if path, err := exec.LookPath("agy"); err == nil {
		return path
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, ".local", "bin", "agy")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

var (
	clientIDPattern = regexp.MustCompile(`[0-9]+-[a-z0-9]+\.apps\.googleusercontent\.com`)
	// Google client secrets are GOCSPX- plus exactly 28 characters. The length
	// must be pinned: the binary packs its string constants with no
	// separators, so an unbounded match would swallow whatever string the
	// linker placed next.
	clientSecretPattern = regexp.MustCompile(`GOCSPX-[A-Za-z0-9_-]{28}`)
)

// oauthClientsFromBinary scans the CLI binary for the installed-app clients it
// carries. The binary packs its string constants with no separators and no
// recorded pairing between client ids and secrets — today's build holds two of
// each — so this returns the cross product and lets the refresh path discover
// the working pair by trying them.
func oauthClientsFromBinary(path string) []oauthClient {
	if path == "" {
		return nil
	}
	binary, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	ids := uniqueMatches(clientIDPattern, binary)
	secrets := uniqueMatches(clientSecretPattern, binary)
	clients := make([]oauthClient, 0, len(ids)*len(secrets))
	for _, id := range ids {
		for _, secret := range secrets {
			clients = append(clients, oauthClient{id: id, secret: secret})
		}
	}
	return clients
}

func uniqueMatches(pattern *regexp.Regexp, blob []byte) []string {
	seen := make(map[string]bool)
	var out []string
	for _, match := range pattern.FindAll(blob, -1) {
		text := string(match)
		if !seen[text] {
			seen[text] = true
			out = append(out, text)
		}
	}
	return out
}

// ReadLocalCredential returns the credential the Antigravity CLI currently
// holds on this machine. It reports ok=false rather than an error when the CLI
// is simply not signed in, so callers can distinguish "nothing to import" from
// "the stored credential is broken".
func ReadLocalCredential(ctx context.Context, now time.Time) (credential CredentialInfo, ok bool, err error) {
	if runtime.GOOS != "darwin" {
		return CredentialInfo{}, false, nil
	}
	current, err := user.Current()
	if err != nil {
		return CredentialInfo{}, false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, keychainReadTimeout)
	defer cancel()
	lookup := func(account string) ([]byte, bool, error) {
		cmd := exec.CommandContext(ctx, "security", "find-generic-password", "-s", keychainService, "-a", account, "-w")
		body, runErr := cmd.Output()
		if runErr == nil {
			return body, len(bytes.TrimSpace(body)) > 0, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, false, ctxErr
		}
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) && exitErr.ExitCode() == 44 {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read Antigravity keychain item: %w", runErr)
	}
	body, found, lookupErr := lookup(current.Username)
	if lookupErr != nil {
		return CredentialInfo{}, false, lookupErr
	}
	if !found {
		// The CLI stores the item under its own account name rather than the
		// unix user on some versions; try that before concluding it is absent.
		body, found, lookupErr = lookup(keychainAccount)
		if lookupErr != nil {
			return CredentialInfo{}, false, lookupErr
		}
		if !found {
			return CredentialInfo{}, false, nil
		}
	}
	credential, err = ParseCredential(bytes.TrimSpace(body), "antigravity keychain", now)
	if err != nil {
		return CredentialInfo{}, false, err
	}
	return credential, true, nil
}

// tokenResponse is Google's OAuth2 token-endpoint response.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	// RefreshToken is only returned when Google rotates it. Google does not
	// rotate on every refresh, so an empty value means keep the existing one
	// rather than that the credential lost its refresh token.
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	ExpiresIn    int64  `json:"expires_in"`
}

// RefreshCredential exchanges the refresh token for a fresh access token.
// Google refresh tokens are not single-use, so unlike the Claude path a
// concurrent refresh wastes a round trip rather than invalidating the
// credential; callers may still serialize to avoid the waste.
//
// The CLI binary can carry several OAuth clients and does not record which
// one a credential was issued to, so candidates are tried in order: the pair
// that last worked in this process, then the configured or discovered pairs.
// Only a client rejection advances to the next candidate — an invalid_grant is
// about the credential, not the client, and retrying it against another client
// would just multiply a terminal failure. (Google answers invalid_client for
// an unknown id but 401 unauthorized_client for a known id with the wrong
// secret, so both mark the candidate as wrong.)
func RefreshCredential(ctx context.Context, client *http.Client, credential CredentialInfo, now time.Time) (CredentialInfo, error) {
	if strings.TrimSpace(credential.RefreshToken) == "" {
		return credential, fmt.Errorf("Antigravity credential has no refresh token")
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	if cached := workingClient.Load(); cached != nil {
		refreshed, err := refreshWithClient(ctx, client, credential, now, *cached)
		if err == nil {
			return refreshed, nil
		}
		if !isInvalidClient(err) {
			return credential, err
		}
	}
	clients := oauthClientsForRefresh()
	if len(clients) == 0 {
		return credential, fmt.Errorf("no Antigravity OAuth client available: install the agy CLI or set SUBROUTER_ANTIGRAVITY_CLIENT_ID and SUBROUTER_ANTIGRAVITY_CLIENT_SECRET")
	}
	var lastErr error
	for _, candidate := range clients {
		refreshed, err := refreshWithClient(ctx, client, credential, now, candidate)
		if err == nil {
			workingClient.Store(&candidate)
			return refreshed, nil
		}
		if !isInvalidClient(err) {
			return credential, err
		}
		lastErr = err
	}
	return credential, lastErr
}

// invalidClientError marks a rejection of the presented client rather than of
// the credential, so the caller tries the next candidate.
type invalidClientError struct{ err error }

func (e invalidClientError) Error() string { return e.err.Error() }
func (e invalidClientError) Unwrap() error { return e.err }

func isInvalidClient(err error) bool {
	var target invalidClientError
	return errors.As(err, &target)
}

func refreshWithClient(ctx context.Context, client *http.Client, credential CredentialInfo, now time.Time, oauth oauthClient) (CredentialInfo, error) {
	form := url.Values{
		"client_id":     {oauth.id},
		"client_secret": {oauth.secret},
		"grant_type":    {"refresh_token"},
		"refresh_token": {credential.RefreshToken},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return credential, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := client.Do(req)
	if err != nil {
		return credential, err
	}
	defer func() { _ = res.Body.Close() }()
	body, copyErr := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if copyErr != nil {
		return credential, copyErr
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var rejection struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &rejection)
		err := fmt.Errorf("Antigravity OAuth refresh failed: %s (error=%s)", res.Status, rejection.Error)
		// A client rejection means the presented pair is wrong, not the
		// credential, so it is the one failure worth retrying with the next
		// candidate.
		if rejection.Error != "" &&
			(rejection.Error == "invalid_client" || rejection.Error == "unauthorized_client") {
			return credential, invalidClientError{err}
		}
		return credential, err
	}
	var parsed tokenResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return credential, fmt.Errorf("Antigravity OAuth refresh returned an undecodable body: %w", err)
	}
	if strings.TrimSpace(parsed.AccessToken) == "" {
		return credential, fmt.Errorf("Antigravity OAuth refresh returned no access token")
	}
	refreshed := credential
	refreshed.AccessToken = parsed.AccessToken
	if strings.TrimSpace(parsed.RefreshToken) != "" {
		refreshed.RefreshToken = parsed.RefreshToken
	}
	if strings.TrimSpace(parsed.IDToken) != "" {
		refreshed.IDToken = parsed.IDToken
	}
	if strings.TrimSpace(parsed.TokenType) != "" {
		refreshed.TokenType = parsed.TokenType
	}
	if strings.TrimSpace(parsed.Scope) != "" {
		refreshed.Scope = parsed.Scope
	}
	if parsed.ExpiresIn > 0 {
		refreshed.ExpiresAt = now.Add(time.Duration(parsed.ExpiresIn) * time.Second).UTC()
	} else {
		refreshed.ExpiresAt = time.Time{}
	}
	return refreshed, nil
}

// accountID is the stable identifier of the one account the CLI keychain
// credential represents.
const accountID = "antigravity"

// Store adapts the CLI's keychain credential to the proxy's OAuth account
// source.
type Store struct {
	mu                sync.Mutex
	cached            CredentialInfo
	readCredential    func(context.Context, time.Time) (CredentialInfo, bool, error)
	refreshCredential func(context.Context, *http.Client, CredentialInfo, time.Time) (CredentialInfo, error)
}

// Provider implements the proxy's OAuth account source.
func (*Store) Provider() account.Provider {
	return account.ProviderAntigravity
}

// ListAccounts surfaces the CLI credential as one account, or none when the
// CLI is not signed in.
func (s *Store) ListAccounts(ctx context.Context) ([]account.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.cached.AccessToken != "" && !s.cached.NeedsRefresh(now) {
		return []account.Account{credentialAccount(s.cached)}, nil
	}
	credential, ok, err := s.read(ctx, now)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	s.cached = credential
	return []account.Account{credentialAccount(credential)}, nil
}

// RefreshAccount refreshes the credential when its access token is near
// expiry. Unlike the Claude and Kimi stores it does not write the refreshed
// pair back: Google does not rotate the refresh token on exchange, so the CLI
// keeps its own credential valid and both sides refresh independently.
func (s *Store) RefreshAccount(ctx context.Context, client *http.Client, acct account.Account) (account.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.cached.AccessToken != "" && !s.cached.NeedsRefresh(now) {
		return credentialAccount(s.cached), nil
	}
	credential, ok, err := s.read(ctx, now)
	if err != nil || !ok {
		return acct, err
	}
	// A prior refresh may have rotated the refresh token even though the CLI's
	// keychain entry is unchanged. Continue from the in-process credential when
	// available; otherwise the stale keychain token can force a refresh on every
	// request or eventually fail after rotation.
	if s.cached.RefreshToken != "" {
		credential = s.cached
	}
	if !credential.NeedsRefresh(now) {
		s.cached = credential
		return credentialAccount(credential), nil
	}
	refreshed, err := s.refresh(ctx, client, credential, now)
	if err != nil {
		return acct, err
	}
	s.cached = refreshed
	return credentialAccount(refreshed), nil
}

func (s *Store) read(ctx context.Context, now time.Time) (CredentialInfo, bool, error) {
	if s.readCredential != nil {
		return s.readCredential(ctx, now)
	}
	return ReadLocalCredential(ctx, now)
}

func (s *Store) refresh(ctx context.Context, client *http.Client, credential CredentialInfo, now time.Time) (CredentialInfo, error) {
	if s.refreshCredential != nil {
		return s.refreshCredential(ctx, client, credential, now)
	}
	return RefreshCredential(ctx, client, credential, now)
}

func credentialAccount(credential CredentialInfo) account.Account {
	return account.Account{
		ID:       accountID,
		Provider: account.ProviderAntigravity,
		AuthMode: account.AuthModeOAuth,
		Label:    "Antigravity",
		Token:    credential.AccessToken,
		Source:   "antigravity keychain",
	}
}
