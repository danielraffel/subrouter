package proxy

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func TestQwenAnthropicLeaseSelectionUsesSharedTokenPlanScores(t *testing.T) {
	const model = "qwen3.7-plus"
	available := []accounts.Account{
		{ID: "qwen-token:a-cooked", Provider: accounts.ProviderQwenAnthropic, AuthMode: accounts.AuthModeAPIKey, Token: "cooked"},
		{ID: "qwen-token:z-healthy", Provider: accounts.ProviderQwenAnthropic, AuthMode: accounts.AuthModeAPIKey, Token: "healthy"},
	}
	scheduler := selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "qwen-token:a-cooked", Provider: accounts.ProviderQwenToken, Headroom: 1, ShortHeadroom: 1},
		{AccountID: "qwen-token:z-healthy", Provider: accounts.ProviderQwenToken, Headroom: 1, ShortHeadroom: 1},
	})
	schedulerRef := selectacct.NewSchedulerRef(scheduler)
	schedulerRef.MarkExhaustedUntil(accounts.ProviderQwenToken, "qwen-token:a-cooked", model, time.Now().Add(time.Hour))
	server := &Server{SchedulerRef: schedulerRef}

	picked, err := pickTenantCredentialLeaseAccount(server, available, nil, tenantCredentialLeaseRequest{
		Provider: string(accounts.ProviderQwenAnthropic), Model: model,
	})
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "qwen-token:z-healthy" {
		t.Fatalf("picked %q, want healthy shared Token Plan account", picked.ID)
	}
}

func TestTenantCredentialLeaseRoutingOrder(t *testing.T) {
	available := []accounts.Account{
		{ID: "forced", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "forced"},
		{ID: "sticky", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "sticky"},
		{ID: "preferred", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "preferred"},
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("claude", "session-a", "sticky", ""); err != nil {
		t.Fatal(err)
	}
	server := &Server{Sessions: store, Scheduler: selectacct.NewScheduler(nil)}

	forced, err := pickTenantCredentialLeaseAccount(server, available, nil, tenantCredentialLeaseRequest{
		Provider: string(accounts.ProviderClaude), AgentType: "claude", SessionID: "session-a",
		PreferAccountID: "preferred", ForceAccountID: "forced",
	})
	if err != nil || forced.ID != "forced" {
		t.Fatalf("forced selection = %+v, %v", forced, err)
	}
	sticky, err := pickTenantCredentialLeaseAccount(server, available, nil, tenantCredentialLeaseRequest{
		Provider: string(accounts.ProviderClaude), AgentType: "claude", SessionID: "session-a",
		PreferAccountID: "preferred",
	})
	if err != nil || sticky.ID != "sticky" {
		t.Fatalf("sticky-before-preferred selection = %+v, %v", sticky, err)
	}
	_, err = pickTenantCredentialLeaseAccount(server, available, nil, tenantCredentialLeaseRequest{
		Provider: string(accounts.ProviderClaude), AgentType: "claude", SessionID: "new-session",
		ForceAccountID: "missing",
	})
	if err == nil || !strings.Contains(err.Error(), "forced account") {
		t.Fatalf("missing forced selection error = %v", err)
	}
}
