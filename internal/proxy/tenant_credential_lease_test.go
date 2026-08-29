package proxy

import (
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
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
