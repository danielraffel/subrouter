package proxy

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

// stubOAuthSource records refresh calls and returns a fixed account.
type stubOAuthSource struct {
	provider     accounts.Provider
	refreshCalls int
	refreshed    accounts.Account
	err          error
	listed       []accounts.Account
	listErr      error
}

func (s *stubOAuthSource) Provider() accounts.Provider { return s.provider }

func (s *stubOAuthSource) ListAccounts(_ context.Context) ([]accounts.Account, error) {
	return s.listed, s.listErr
}

func (s *stubOAuthSource) RefreshAccount(_ context.Context, _ *http.Client, account accounts.Account) (accounts.Account, error) {
	s.refreshCalls++
	if s.err != nil {
		return account, s.err
	}
	return s.refreshed, nil
}

// An OAuth account of a registered provider must refresh through its source
// and the refreshed account must replace the snapshot entry.
func TestAccountRefRefreshDispatchesToTheOAuthSource(t *testing.T) {
	kimi := accounts.Account{ID: "kimi-code", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "stale"}
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{kimi}, nil)
	source := &stubOAuthSource{
		provider:  accounts.ProviderKimi,
		refreshed: accounts.Account{ID: "kimi-code", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "fresh"},
	}
	ref.oauthSources = []OAuthAccountSource{source}

	refreshed, err := ref.Refresh(context.Background(), kimi)
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if source.refreshCalls != 1 {
		t.Fatalf("source saw %d refreshes, want 1", source.refreshCalls)
	}
	if refreshed.Token != "fresh" {
		t.Fatal("refresh did not return the expected token")
	}
	stored, _ := ref.Snapshot()
	if len(stored) != 1 || stored[0].Token != "fresh" {
		t.Fatal("snapshot does not contain the refreshed account")
	}
}

// A refresh failure must leave the snapshot untouched so the account is not
// swapped for a zero value mid-flight.
func TestAccountRefRefreshKeepsTheAccountWhenTheSourceFails(t *testing.T) {
	kimi := accounts.Account{ID: "kimi-code", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "stale"}
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{kimi}, nil)
	source := &stubOAuthSource{provider: accounts.ProviderKimi, err: errors.New("401 Unauthorized")}
	ref.oauthSources = []OAuthAccountSource{source}

	refreshed, err := ref.Refresh(context.Background(), kimi)
	if err == nil {
		t.Fatal("a failing source must surface its error")
	}
	if refreshed.Token != "stale" {
		t.Fatal("failed refresh changed the returned account token")
	}
	stored, _ := ref.Snapshot()
	if len(stored) != 1 || stored[0].Token != "stale" {
		t.Fatal("failed refresh changed the stored account")
	}
}

// Providers without a registered source pass through unchanged, and API-key
// accounts are never refreshed.
func TestAccountRefRefreshLeavesUnregisteredProvidersAlone(t *testing.T) {
	apiKey := accounts.Account{ID: "apikey:kimi", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeAPIKey, Token: "key"}
	oauthOther := accounts.Account{ID: "other", Provider: accounts.ProviderZAI, AuthMode: accounts.AuthModeOAuth, Token: "tok"}
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{apiKey, oauthOther}, nil)
	source := &stubOAuthSource{provider: accounts.ProviderKimi}
	ref.oauthSources = []OAuthAccountSource{source}

	for _, acct := range []accounts.Account{apiKey, oauthOther} {
		refreshed, err := ref.Refresh(context.Background(), acct)
		if err != nil {
			t.Fatalf("refresh of %s failed: %v", acct.ID, err)
		}
		if refreshed != acct {
			t.Fatalf("account %s changed", acct.ID)
		}
	}
	if source.refreshCalls != 0 {
		t.Fatalf("source saw %d refreshes, want none", source.refreshCalls)
	}
}

// Account listing aggregates the registered sources alongside the Codex and
// Claude stores.
func TestLoadAccountRefAccountsAggregatesOAuthSources(t *testing.T) {
	source := &stubOAuthSource{
		provider: accounts.ProviderKimi,
		listed:   []accounts.Account{{ID: "kimi-code", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "tok"}},
	}
	loaded, err := loadAccountRefAccounts(accounts.CodexStore{Dir: t.TempDir()}, agentclaude.Store{Dir: t.TempDir()}, []OAuthAccountSource{source})
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	var found bool
	for _, acct := range loaded {
		if acct.Provider == accounts.ProviderKimi && acct.AuthMode == accounts.AuthModeOAuth {
			found = true
		}
	}
	if !found {
		t.Fatalf("%d loaded accounts missing the source account", len(loaded))
	}
}
