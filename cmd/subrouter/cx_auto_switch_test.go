package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/selectacct"
	"github.com/manaflow-ai/subrouter/internal/session"
)

func TestCXAutoSwitchPicksBestOAuthAccount(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	var switchedTo string

	picked, err := cxAutoSwitchOnce(context.Background(), cxAutoSwitchConfig{
		Accounts: []accounts.Account{
			{ID: "a@example.com", AuthMode: accounts.AuthModeOAuth},
			{ID: "b@example.com", AuthMode: accounts.AuthModeOAuth},
			{ID: "apikey:paid", AuthMode: accounts.AuthModeAPIKey},
		},
		Sessions:     store,
		SchedulerRef: schedulerRef,
		FetchScores: func(_ context.Context, candidates []accounts.Account) ([]selectacct.Score, int) {
			for _, candidate := range candidates {
				if candidate.AuthMode != accounts.AuthModeOAuth {
					t.Fatalf("auto-switch scored non-OAuth account: %#v", candidate)
				}
			}
			return []selectacct.Score{
				{AccountID: "a@example.com", Headroom: 0.50, ShortHeadroom: 0.50},
				{AccountID: "b@example.com", Headroom: 0.90, ShortHeadroom: 0.90},
				{AccountID: "apikey:paid", Headroom: 1.00, ShortHeadroom: 1.00},
			}, 2
		},
		SwitchActive: func(_ context.Context, accountID string) error {
			switchedTo = accountID
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if picked != "b@example.com" {
		t.Fatalf("picked = %q, want b@example.com", picked)
	}
	if switchedTo != "b@example.com" {
		t.Fatalf("switchedTo = %q, want b@example.com", switchedTo)
	}
	best, err := schedulerRef.Get().Pick([]accounts.Account{
		{ID: "a@example.com", AuthMode: accounts.AuthModeOAuth},
		{ID: "b@example.com", AuthMode: accounts.AuthModeOAuth},
	})
	if err != nil {
		t.Fatal(err)
	}
	if best.ID != "b@example.com" {
		t.Fatalf("scheduler ref pick = %q, want b@example.com", best.ID)
	}
}

func TestCXAutoSwitchSkipsWhenUsageUnavailable(t *testing.T) {
	ran := false
	_, err := cxAutoSwitchOnce(context.Background(), cxAutoSwitchConfig{
		Accounts: []accounts.Account{{ID: "a@example.com", AuthMode: accounts.AuthModeOAuth}},
		FetchScores: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			return []selectacct.Score{{AccountID: "a@example.com", Headroom: 0, ShortHeadroom: 0}}, 0
		},
		SwitchActive: func(context.Context, string) error {
			ran = true
			return nil
		},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if ran {
		t.Fatal("cx switch ran despite missing fresh usage")
	}
}

func TestCXAutoSwitchSkipsWhenOAuthAccountsExhausted(t *testing.T) {
	ran := false
	_, err := cxAutoSwitchOnce(context.Background(), cxAutoSwitchConfig{
		Accounts: []accounts.Account{
			{ID: "a@example.com", AuthMode: accounts.AuthModeOAuth},
			{ID: "b@example.com", AuthMode: accounts.AuthModeOAuth},
		},
		FetchScores: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			return []selectacct.Score{
				{AccountID: "a@example.com", Headroom: 0, ShortHeadroom: 0},
				{AccountID: "b@example.com", Headroom: 0.05, ShortHeadroom: 0.05},
			}, 2
		},
		SwitchActive: func(context.Context, string) error {
			ran = true
			return nil
		},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if ran {
		t.Fatal("cx switch ran despite exhausted OAuth accounts")
	}
}
