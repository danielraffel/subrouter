package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/account"
	"github.com/manaflow-ai/subrouter/internal/oauthdevice"
)

// OAuth constants for the Grok CLI's installed-app client, matching what the
// CLI itself uses. There is no client secret: the device-grant client is a
// public client. The token endpoint is resolved through OIDC discovery rather
// than hardcoded, because xAI has moved it before.
const (
	oauthClientID     = "b1a00492-073a-47ea-816f-4c329264a828"
	oauthScope        = "openid profile email offline_access grok-cli:access api:access conversations:read conversations:write workspaces:read workspaces:write"
	oauthDiscoveryURL = "https://auth.x.ai/.well-known/openid-configuration"
)

// discoveryURL is a variable so tests can point discovery at a stub server.
var discoveryURL = oauthDiscoveryURL

// accountID is the stable identifier of the one subscription account the
// credential file represents.
const accountID = "grok-subscription"

// Store reads, refreshes, and persists the Grok subscription credential file.
type Store struct {
	// Path is the credential file. Empty means the default location.
	Path string
	// RefreshTransaction serializes a refresh write with account add/remove
	// mutations across Subrouter processes.
	RefreshTransaction func(context.Context, func() error) error
}

func DefaultStore() Store {
	return Store{}
}

func (s Store) credentialPath() string {
	if s.Path != "" {
		return s.Path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".subrouter", "grok", "oauth.json")
}

// ReadLocalCredential returns the credential currently on this machine. It
// reports ok=false rather than an error when nobody has signed in, so callers
// can distinguish "nothing to route" from "the stored credential is broken".
func (s Store) ReadLocalCredential(now time.Time) (credential CredentialInfo, ok bool, err error) {
	path := s.credentialPath()
	if path == "" {
		return CredentialInfo{}, false, nil
	}
	body, readErr := os.ReadFile(path)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return CredentialInfo{}, false, nil
		}
		return CredentialInfo{}, false, readErr
	}
	credential, err = ParseCredential(body, path, now)
	if err != nil {
		return CredentialInfo{}, false, err
	}
	return credential, true, nil
}

// Provider implements the proxy's OAuth account source.
func (s Store) Provider() account.Provider {
	return account.ProviderGrok
}

// ListAccounts surfaces the stored credential as one account, or none when
// nobody has signed in. An unreadable credential is an error: silently
// dropping it would look identical to being signed out.
func (s Store) ListAccounts(_ context.Context) ([]account.Account, error) {
	credential, ok, err := s.ReadLocalCredential(time.Now())
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return []account.Account{credentialAccount(credential)}, nil
}

// refreshMu serializes refreshes process-wide. Whether xAI's refresh token is
// single-use is unproven, but Claude's is, and a concurrent double-redemption
// of a single-use token invalidates the credential — so refresh takes the lock
// rather than finding out.
var refreshMu sync.Mutex

// RefreshAccount refreshes the credential when its access token is near expiry
// and writes the result back. Writing back matters because Subrouter owns this
// file: a refreshed token that stays in memory only is lost on restart, and if
// xAI rotates refresh tokens the stored one is already dead.
func (s Store) RefreshAccount(ctx context.Context, client *http.Client, acct account.Account) (account.Account, error) {
	refreshed, _, err := s.RefreshAccountIfNeeded(ctx, client, acct)
	return refreshed, err
}

// RefreshAccountIfNeeded is RefreshAccount plus whether the credential file
// changed, allowing CLI callers to publish the account generation only after a
// refresh actually committed new tokens.
func (s Store) RefreshAccountIfNeeded(ctx context.Context, client *http.Client, acct account.Account) (refreshed account.Account, didRefresh bool, err error) {
	refreshMu.Lock()
	defer refreshMu.Unlock()
	refresh := func() error {
		credential, ok, readErr := s.ReadLocalCredential(time.Now())
		if readErr != nil {
			return readErr
		}
		if !ok {
			return fmt.Errorf("Grok subscription credential was not found")
		}
		if !credential.NeedsRefresh(time.Now()) {
			refreshed = credentialAccount(credential)
			return nil
		}
		config, discoverErr := discoverOAuthConfig(ctx, client)
		if discoverErr != nil {
			return discoverErr
		}
		token, refreshErr := oauthdevice.Refresh(ctx, client, config, credential.RefreshToken, time.Now())
		if refreshErr != nil {
			return refreshErr
		}
		credential.AccessToken = token.AccessToken
		if strings.TrimSpace(token.RefreshToken) != "" {
			credential.RefreshToken = token.RefreshToken
		}
		if strings.TrimSpace(token.IDToken) != "" {
			credential.IDToken = token.IDToken
			if email := emailFromIDToken(token.IDToken); email != "" {
				credential.Email = email
			}
		}
		if strings.TrimSpace(token.TokenType) != "" {
			credential.TokenType = token.TokenType
		}
		if strings.TrimSpace(token.Scope) != "" {
			credential.Scope = token.Scope
		}
		credential.ExpiresAt = token.ExpiresAt
		if writeErr := s.writeCredential(credential); writeErr != nil {
			return fmt.Errorf("refreshed the Grok credential but could not write it back: %w", writeErr)
		}
		refreshed = credentialAccount(credential)
		didRefresh = true
		return nil
	}
	if s.RefreshTransaction != nil {
		err = s.RefreshTransaction(ctx, refresh)
	} else {
		err = refresh()
	}
	if err != nil {
		return acct, false, err
	}
	return refreshed, didRefresh, nil
}

func credentialAccount(credential CredentialInfo) account.Account {
	label := "Grok"
	if credential.Email != "" {
		label = "Grok (" + credential.Email + ")"
	}
	return account.Account{
		ID:       accountID,
		Provider: account.ProviderGrok,
		AuthMode: account.AuthModeOAuth,
		Label:    label,
		Email:    credential.Email,
		Token:    credential.AccessToken,
		Source:   "subrouter grok credential file",
	}
}

// SaveCredential validates and atomically stores a freshly authorized Grok
// subscription credential. Keeping this operation separate from Authorize lets
// command callers publish it through the shared account-disk transaction.
func (s Store) SaveCredential(credential CredentialInfo) (account.Account, error) {
	if strings.TrimSpace(credential.AccessToken) == "" || strings.TrimSpace(credential.RefreshToken) == "" {
		return account.Account{}, fmt.Errorf("Grok OAuth response is incomplete")
	}
	if credential.ExpiresAt.IsZero() || !credential.ExpiresAt.After(time.Now()) {
		return account.Account{}, fmt.Errorf("Grok OAuth access token is not fresh")
	}
	if err := s.writeCredential(credential); err != nil {
		return account.Account{}, err
	}
	return credentialAccount(credential), nil
}

// RemoveCredential removes the one Subrouter-owned Grok subscription
// credential. The caller coordinates this mutation with other processes.
func (s Store) RemoveCredential() (account.Account, bool, error) {
	path := s.credentialPath()
	if path == "" {
		return account.Account{}, false, nil
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return account.Account{}, false, nil
		}
		return account.Account{}, false, err
	}
	removed := account.Account{
		ID: accountID, Provider: account.ProviderGrok, AuthMode: account.AuthModeOAuth,
		Label: "Grok", Source: "subrouter grok credential file",
	}
	if credential, ok, err := s.ReadLocalCredential(time.Now()); err == nil && ok {
		removed = credentialAccount(credential)
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return account.Account{}, false, nil
		}
		return account.Account{}, false, err
	}
	return removed, true, nil
}

// writeCredential persists the credential atomically and private, so a crash
// mid-write cannot truncate the only copy of the refresh token.
func (s Store) writeCredential(credential CredentialInfo) error {
	path := s.credentialPath()
	if path == "" {
		return fmt.Errorf("no Grok credential path")
	}
	payload := map[string]any{
		"access_token":  credential.AccessToken,
		"refresh_token": credential.RefreshToken,
		"token_type":    credential.TokenType,
	}
	if strings.TrimSpace(credential.IDToken) != "" {
		payload["id_token"] = credential.IDToken
	}
	if strings.TrimSpace(credential.Scope) != "" {
		payload["scope"] = credential.Scope
	}
	if strings.TrimSpace(credential.Email) != "" {
		payload["email"] = credential.Email
	}
	if !credential.ExpiresAt.IsZero() {
		payload["expires_at"] = credential.ExpiresAt.Unix()
		payload["expires_in"] = int64(time.Until(credential.ExpiresAt).Seconds())
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".oauth.json.*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// discovery holds the endpoints OIDC discovery returned, cached process-wide
// so a refresh on every account check does not re-fetch the document.
type discovery struct {
	DeviceAuthURL string
	TokenURL      string
}

var (
	discoveryMu    sync.Mutex
	discoveryCache *discovery
)

// discoverOAuthConfig resolves the device-flow endpoints through xAI's OIDC
// discovery document. The endpoints are validated to stay on x.ai over https:
// the credential file's owner trusts this document to say where refresh tokens
// get posted, and a poisoned or corrupted response must not redirect them
// elsewhere.
func discoverOAuthConfig(ctx context.Context, client *http.Client) (oauthdevice.Config, error) {
	found, err := discover(ctx, client)
	if err != nil {
		return oauthdevice.Config{}, err
	}
	return oauthdevice.Config{
		ClientID:      oauthClientID,
		DeviceAuthURL: found.DeviceAuthURL,
		TokenURL:      found.TokenURL,
		Scope:         oauthScope,
	}, nil
}

func discover(ctx context.Context, client *http.Client) (discovery, error) {
	discoveryMu.Lock()
	defer discoveryMu.Unlock()
	if discoveryCache != nil {
		return *discoveryCache, nil
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return discovery{}, err
	}
	req.Header.Set("Accept", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return discovery{}, fmt.Errorf("xAI OIDC discovery failed: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return discovery{}, fmt.Errorf("xAI OIDC discovery failed: %s", res.Status)
	}
	var payload struct {
		DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
		TokenEndpoint               string `json:"token_endpoint"`
	}
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		return discovery{}, fmt.Errorf("xAI OIDC discovery returned an undecodable body: %w", err)
	}
	found := discovery{}
	if found.DeviceAuthURL, err = validateOAuthEndpoint(payload.DeviceAuthorizationEndpoint, "device_authorization_endpoint"); err != nil {
		return discovery{}, err
	}
	if found.TokenURL, err = validateOAuthEndpoint(payload.TokenEndpoint, "token_endpoint"); err != nil {
		return discovery{}, err
	}
	discoveryCache = &found
	return found, nil
}

// endpointHostSuffix is the host suffix discovered endpoints must stay on.
// Tests point discoveryURL at a stub, so the suffix is a variable too.
var endpointHostSuffix = ".x.ai"

func validateOAuthEndpoint(rawURL, field string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", fmt.Errorf("xAI OIDC discovery: %s is empty", field)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("xAI OIDC discovery: %s is invalid: %w", field, err)
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	suffix := strings.TrimPrefix(endpointHostSuffix, ".")
	if parsed.Scheme != "https" || (host != suffix && !strings.HasSuffix(host, endpointHostSuffix)) {
		return "", fmt.Errorf("xAI OIDC discovery: %s %q is not an https endpoint on %s", field, rawURL, suffix)
	}
	return rawURL, nil
}
