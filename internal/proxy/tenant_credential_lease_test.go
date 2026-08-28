package proxy

import (
	"net/http"
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

func TestQwenAnthropicLeaseFailuresCooldownTheSharedTokenPlanAccount(t *testing.T) {
	const accountID = "qwen-token:shared"
	const model = "qwen3.7-plus"
	account := accounts.Account{
		ID: accountID, Provider: accounts.ProviderQwenToken,
		AuthMode: accounts.AuthModeAPIKey, Token: "key", CredentialVersion: "credential-v1",
	}
	newServer := func() *Server {
		ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{account}, nil)
		return &Server{
			AccountRef: ref,
			SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{{
				AccountID: accountID, Provider: accounts.ProviderQwenToken,
				Headroom: 1, ShortHeadroom: 1,
			}})),
		}
	}
	lease := tenantCredentialLease{
		accountID: accountID, provider: accounts.ProviderQwenAnthropic,
		credentialIdentity: account.CredentialIdentity(), model: model,
	}

	tests := []struct {
		name    string
		report  tenantCredentialLeaseReport
		isQuota bool
	}{
		{name: "unauthorized", report: tenantCredentialLeaseReport{Outcome: broker.LeaseUnauthorized}},
		{name: "forbidden account", report: tenantCredentialLeaseReport{Outcome: broker.LeaseForbidden, Scope: broker.LeaseCooldownAccount}},
		{name: "forbidden quota", report: tenantCredentialLeaseReport{Outcome: broker.LeaseForbidden, Scope: broker.LeaseCooldownQuota}, isQuota: true},
		{name: "rate limited quota", report: tenantCredentialLeaseReport{Outcome: broker.LeaseRateLimited, Scope: broker.LeaseCooldownQuota, RetryAt: time.Now().Add(time.Minute).UnixMilli()}, isQuota: true},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			server := newServer()
			applyTenantCredentialLeaseReport(server, lease, testCase.report)
			scheduler := server.SchedulerRef.Get()
			if testCase.isQuota {
				scheduler = scheduler.ForModel(model)
			}
			if !scheduler.Exhausted(accounts.ProviderQwenToken, accountID) {
				t.Fatalf("shared Token Plan account remained usable after %s", testCase.name)
			}
			if server.SchedulerRef.Get().Exhausted(accounts.ProviderQwenAnthropic, accountID) {
				t.Fatalf("failure was recorded under the transport alias for %s", testCase.name)
			}
		})
	}
}

func TestQwenAnthropicTransportFailuresMarkSharedTokenPlanAccount(t *testing.T) {
	const accountID = "qwen-token:shared"
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			account := accounts.Account{
				ID: accountID, Provider: accounts.ProviderQwenAnthropic,
				AuthMode: accounts.AuthModeAPIKey, Token: "key", CredentialVersion: "credential-v1",
			}
			ref := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{{
				AccountID: accountID, Provider: accounts.ProviderQwenToken, Headroom: 1, ShortHeadroom: 1,
			}}))
			server := Server{
				AccountRef:   NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{account}, nil),
				SchedulerRef: ref,
			}
			server.markAccountExhaustedFromResponseForAccount(accounts.Account{
				ID: accountID, Provider: accounts.ProviderQwenAnthropic, CredentialVersion: "credential-v1",
			}, "", status, nil)
			if !ref.Get().Exhausted(accounts.ProviderQwenToken, accountID) {
				t.Fatalf("status %d did not mark canonical Token Plan account", status)
			}
			if ref.Get().Exhausted(accounts.ProviderQwenAnthropic, accountID) {
				t.Fatalf("status %d left a mark under the transport alias", status)
			}
		})
	}
}

func TestQwenAnthropicCredentialLookupUsesSharedTokenPlanOwner(t *testing.T) {
	const accountID = "qwen-token:shared"
	account := accounts.Account{
		ID: accountID, Provider: accounts.ProviderQwenAnthropic,
		AuthMode: accounts.AuthModeAPIKey, Token: "key", CredentialVersion: "credential-v1",
	}
	ref := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{{
		AccountID: accountID, Provider: accounts.ProviderQwenToken, Headroom: 1, ShortHeadroom: 1,
	}}))
	server := Server{
		AccountRef:   NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{account}, nil),
		SchedulerRef: ref,
	}
	server.markAccountExhaustedFromResponse(accounts.ProviderQwenAnthropic, accountID, "", http.StatusUnauthorized, nil)
	if !ref.Get().Exhausted(accounts.ProviderQwenToken, accountID) {
		t.Fatal("transport alias lookup did not mark the canonical Token Plan credential")
	}
}
