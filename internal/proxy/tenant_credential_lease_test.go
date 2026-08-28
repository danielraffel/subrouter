package proxy

import (
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func TestForbiddenCredentialLeaseHonorsCooldownScope(t *testing.T) {
	const accountID = "claude@example.com"
	newServer := func() *Server {
		score := selectacct.Score{
			AccountID: accountID, Provider: accounts.ProviderClaude,
			Headroom: 1, ShortHeadroom: 1,
			ModelScores: map[string]selectacct.Score{
				agentclaude.OpusFeature:  {AccountID: accountID, Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
				agentclaude.FableFeature: {AccountID: accountID, Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
			},
		}
		return &Server{SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{score}))}
	}
	lease := tenantCredentialLease{
		accountID: accountID, provider: accounts.ProviderClaude, model: "claude-opus-4-8",
	}

	quotaServer := newServer()
	applyTenantCredentialLeaseReport(quotaServer, lease, tenantCredentialLeaseReport{
		Outcome: broker.LeaseForbidden, Scope: broker.LeaseCooldownQuota,
	})
	quotaScheduler := quotaServer.SchedulerRef.Get()
	if !quotaScheduler.ForModel(agentclaude.OpusFeature).Exhausted(accounts.ProviderClaude, accountID) {
		t.Fatal("quota-scoped forbidden report did not exhaust the reported model pool")
	}
	if quotaScheduler.ForModel(agentclaude.FableFeature).Exhausted(accounts.ProviderClaude, accountID) {
		t.Fatal("quota-scoped forbidden report exhausted an unrelated model pool")
	}
	if quotaScheduler.Exhausted(accounts.ProviderClaude, accountID) {
		t.Fatal("quota-scoped forbidden report exhausted the whole account")
	}

	accountServer := newServer()
	applyTenantCredentialLeaseReport(accountServer, lease, tenantCredentialLeaseReport{
		Outcome: broker.LeaseForbidden, Scope: broker.LeaseCooldownAccount,
	})
	accountScheduler := accountServer.SchedulerRef.Get()
	if !accountScheduler.Exhausted(accounts.ProviderClaude, accountID) ||
		!accountScheduler.ForModel(agentclaude.FableFeature).Exhausted(accounts.ProviderClaude, accountID) {
		t.Fatal("account-scoped forbidden report did not exhaust the account and all model pools")
	}
	accountServer.SchedulerRef.Set(selectacct.NewScheduler([]selectacct.Score{{
		AccountID: accountID, Provider: accounts.ProviderClaude,
		Headroom: 1, ShortHeadroom: 1, Fresh: true,
	}}))
	if !accountServer.SchedulerRef.Get().Exhausted(accounts.ProviderClaude, accountID) {
		t.Fatal("healthy quota refresh cleared account-scoped forbidden report")
	}
}

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
