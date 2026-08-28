package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func TestOlderUsageRefreshCannotOverwriteNewerAccountReload(t *testing.T) {
	accountStore := accounts.CodexStore{Dir: t.TempDir()}
	initialStored := proxyStoredOAuthAccount("old@example.com", "old", time.Now().Add(time.Hour))
	addedStored := proxyStoredOAuthAccount("new@example.com", "new", time.Now().Add(time.Hour))
	if err := accountStore.SaveStored(initialStored); err != nil {
		t.Fatal(err)
	}
	initial, err := accountStore.List()
	if err != nil {
		t.Fatal(err)
	}
	ref := NewAccountRef(accountStore, initial, nil)
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{{
		AccountID: "old@example.com", Headroom: 0.9, ShortHeadroom: 0.9,
	}}))
	schedulerRef.SetUpdatedAt(time.Time{})
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	refreshDone := make(chan struct{})
	server := Server{
		AccountRef:    ref,
		SchedulerRef:  schedulerRef,
		UsageScoreTTL: time.Nanosecond,
		ScoreAccounts: func(_ context.Context, available []accounts.Account) ([]selectacct.Score, int) {
			if len(available) == 1 {
				close(refreshStarted)
				<-releaseRefresh
				return []selectacct.Score{{
					AccountID: "old@example.com", Headroom: 0.1, ShortHeadroom: 0.1,
				}}, 1
			}
			return []selectacct.Score{
				{AccountID: "old@example.com", Headroom: 0.8, ShortHeadroom: 0.8},
				{AccountID: "new@example.com", Headroom: 0.42, ShortHeadroom: 0.42},
			}, 2
		},
	}
	go func() {
		server.refreshUsageScoresIfStale(context.Background())
		close(refreshDone)
	}()
	select {
	case <-refreshStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("usage refresh did not start")
	}

	if err := accountStore.SaveStored(addedStored); err != nil {
		t.Fatal(err)
	}
	if _, _, err := server.reloadAccounts(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(releaseRefresh)
	select {
	case <-refreshDone:
	case <-time.After(5 * time.Second):
		t.Fatal("usage refresh did not finish")
	}

	got := schedulerRef.Get().ScoreFor(accounts.ProviderCodex, "new@example.com")
	if got.Headroom != 0.42 {
		t.Fatalf("older refresh overwrote reloaded scheduler: new-account headroom = %v, want 0.42", got.Headroom)
	}
}

func TestGenericReloadPreservesUnchangedTerminalCredentialState(t *testing.T) {
	store := accounts.CodexStore{Dir: t.TempDir()}
	stored := proxyStoredOAuthAccount("dead@example.com", "dead", time.Now().Add(time.Hour))
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	account := loaded[0]
	ref := NewAccountRef(store, loaded, nil)
	scheduler := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	ref.noteCredResult(account, errors.New("invalid_grant"))
	scheduler.MarkExhaustedUntil(account.Provider, account.ID, "", time.Now().Add(time.Hour))
	server := Server{AccountRef: ref, SchedulerRef: scheduler, CredentialBroker: &fakeCredentialBroker{}}

	if _, _, err := server.reloadAccounts(context.Background()); err != nil {
		t.Fatal(err)
	}

	if _, dead := ref.terminalCredFailure(account); !dead {
		t.Fatal("generic reload cleared an unchanged terminal credential failure")
	}
	if _, exhausted := scheduler.ExhaustedUntilFor(account.Provider, account.ID, ""); !exhausted {
		t.Fatal("generic reload cleared an unchanged credential exclusion")
	}
}

func TestTerminalCredentialFailureIsScopedToCredentialToken(t *testing.T) {
	old := accounts.Account{
		ID: "repaired@example.com", Provider: accounts.ProviderCodex, Token: "same-access-token",
		CredentialVersion: "old-refresh-chain",
	}
	replacement := old
	replacement.CredentialVersion = "replacement-refresh-chain"
	ref := &AccountRef{}

	ref.noteCredResult(old, errors.New("invalid_grant"))
	if _, dead := ref.terminalCredFailure(replacement); dead {
		t.Fatal("replacement inherited the old credential's terminal failure")
	}

	// A late result from work that captured the old credential stays attached
	// to that credential and cannot poison the replacement.
	ref.noteCredResult(old, errors.New("invalid_grant"))
	if _, dead := ref.terminalCredFailure(replacement); dead {
		t.Fatal("late old-credential failure poisoned the replacement")
	}

	ref.noteCredResult(replacement, errors.New("invalid_grant"))
	if _, dead := ref.terminalCredFailure(replacement); !dead {
		t.Fatal("replacement's own terminal failure was not remembered")
	}
}
