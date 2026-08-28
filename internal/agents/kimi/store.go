package kimi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/account"
	"github.com/manaflow-ai/subrouter/internal/oauthdevice"
)

// OAuth endpoints for the Kimi Code CLI's installed-app client, matching what
// the CLI itself uses. There is no client secret: the device-grant client is a
// public client.
const (
	oauthClientID = "17e5f671-d194-4dfb-9706-5516cb48c098"
	oauthTokenURL = "https://auth.kimi.com/api/oauth/token"
)

// oauthConfig is a variable so tests can point the refresh at a stub server.
var oauthConfig = oauthdevice.Config{
	ClientID: oauthClientID,
	TokenURL: oauthTokenURL,
}

// accountID is the stable identifier of the one account the CLI credential
// file represents.
const accountID = "kimi-code"

// Store reads and refreshes the Kimi Code CLI credential file.
type Store struct {
	// Path is the credential file. Empty means the CLI's default location.
	Path string
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
	return filepath.Join(home, ".kimi-code", "credentials", "kimi-code.json")
}

// ReadLocalCredential returns the credential the Kimi Code CLI currently holds
// on this machine. It reports ok=false rather than an error when the CLI is
// simply not signed in, so callers can distinguish "nothing to import" from
// "the stored credential is broken".
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
	issuedAt := now
	if info, statErr := os.Stat(path); statErr == nil {
		issuedAt = info.ModTime()
	}
	credential, err = ParseCredential(body, path, issuedAt)
	if err != nil {
		return CredentialInfo{}, false, err
	}
	return credential, true, nil
}

// Provider implements the proxy's OAuth account source.
func (s Store) Provider() account.Provider {
	return account.ProviderKimi
}

// ListAccounts surfaces the CLI credential as one account, or none when the
// CLI is not signed in. An unreadable credential is an error: silently
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

// refreshMu serializes refreshes process-wide. Whether Kimi's refresh token is
// single-use is unproven, but Claude's is, and a concurrent double-redemption
// of a single-use token invalidates the credential for both the proxy and the
// CLI — so refresh takes the lock rather than finding out.
var refreshMu sync.Mutex

// RefreshAccount refreshes the credential when its access token is near expiry
// and writes the result back to the CLI's credential file. Writing back is
// what keeps the CLI itself signed in: if Kimi rotates the refresh token on
// use, a proxy that keeps the new token to itself would log the CLI out.
func (s Store) RefreshAccount(ctx context.Context, client *http.Client, acct account.Account) (account.Account, error) {
	refreshMu.Lock()
	defer refreshMu.Unlock()
	credential, ok, err := s.ReadLocalCredential(time.Now())
	if err != nil || !ok {
		return acct, err
	}
	if !credential.NeedsRefresh(time.Now()) {
		return credentialAccount(credential), nil
	}
	token, err := oauthdevice.Refresh(ctx, client, oauthConfig, credential.RefreshToken, time.Now())
	if err != nil {
		return acct, err
	}
	credential.AccessToken = token.AccessToken
	if strings.TrimSpace(token.RefreshToken) != "" {
		credential.RefreshToken = token.RefreshToken
	}
	if strings.TrimSpace(token.TokenType) != "" {
		credential.TokenType = token.TokenType
	}
	if strings.TrimSpace(token.Scope) != "" {
		credential.Scope = token.Scope
	}
	credential.ExpiresAt = token.ExpiresAt
	if err := s.writeCredential(credential); err != nil {
		return acct, fmt.Errorf("refreshed the Kimi credential but could not write it back: %w", err)
	}
	return credentialAccount(credential), nil
}

func credentialAccount(credential CredentialInfo) account.Account {
	return account.Account{
		ID:       accountID,
		Provider: account.ProviderKimi,
		AuthMode: account.AuthModeOAuth,
		Label:    "Kimi Code",
		Token:    credential.AccessToken,
		Source:   "kimi-code credentials file",
	}
}

// writeCredential persists the refreshed credential over the CLI's file,
// atomically and private, so a crash mid-write cannot truncate the only copy
// of the refresh token.
func (s Store) writeCredential(credential CredentialInfo) error {
	path := s.credentialPath()
	if path == "" {
		return fmt.Errorf("no Kimi credential path")
	}
	payload := map[string]any{
		"access_token":  credential.AccessToken,
		"refresh_token": credential.RefreshToken,
		"token_type":    credential.TokenType,
		"scope":         credential.Scope,
	}
	if !credential.ExpiresAt.IsZero() {
		payload["expires_at"] = credential.ExpiresAt.Unix()
		payload["expires_in"] = int64(time.Until(credential.ExpiresAt).Seconds())
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".kimi-code.json.*")
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
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
