package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

func TestClaudeUsageStatusesRoundTripCredentialPlanTypes(t *testing.T) {
	store := agentclaude.Store{Dir: t.TempDir()}
	credentials := map[string]agentclaude.CredentialInfo{
		"max-account":     {AccessToken: "max-token", RefreshToken: "max-refresh", SubscriptionType: "MAX", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()},
		"pro-account":     {AccessToken: "pro-token", RefreshToken: "pro-refresh", RateLimitTier: "Pro", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()},
		"free-account":    {AccessToken: "free-token", RefreshToken: "free-refresh", SubscriptionType: "free", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()},
		"unknown-account": {AccessToken: "unknown-token", RefreshToken: "unknown-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()},
	}
	for name, credential := range credentials {
		if err := store.ImportProfileCredential(name, credential); err != nil {
			t.Fatalf("ImportProfileCredential(%q): %v", name, err)
		}
	}
	client := &http.Client{Transport: proxyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewBufferString(`{"five_hour":{"utilization":0,"resets_at":"2099-01-01T00:00:00Z"}}`)),
		}, nil
	})}
	ref := &AccountRef{
		store:       accounts.CodexStore{Dir: t.TempDir()},
		claudeStore: store,
		client:      client,
	}
	statuses := ref.UsageStatuses(context.Background())
	want := map[string]string{
		"max-account": "max", "pro-account": "pro", "free-account": "free", "unknown-account": "unknown",
	}
	if len(statuses) != len(want) {
		t.Fatalf("statuses = %+v, want %d Claude rows", statuses, len(want))
	}
	for _, status := range statuses {
		if status.Provider != accounts.ProviderClaude {
			continue
		}
		if !status.AuthValid || !status.UsageFresh {
			t.Errorf("credential-derived status for %q is not live: %+v", status.ID, status)
		}
		if got := status.PlanType; got != want[status.ID] {
			t.Errorf("plan for %q = %q, want %q", status.ID, got, want[status.ID])
		}
		if status.PlanType == "claude" {
			t.Errorf("plan for %q retained the provider placeholder", status.ID)
		}
	}
}
