package proxy

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func TestPublishAccountDiskMutationDoesNotMutateWhenPublicationFails(t *testing.T) {
	called := false
	want := errors.New("generation unavailable")
	err := publishAccountDiskMutation(
		context.Background(),
		t.TempDir(),
		func(string) error { return want },
		func() (bool, error) {
			called = true
			return true, nil
		},
	)
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want publication failure", err)
	}
	if called {
		t.Fatal("credential mutation ran after generation publication failed")
	}
}

func TestPublishAccountDiskMutationPublishesBeforeMutation(t *testing.T) {
	published := false
	err := publishAccountDiskMutation(
		context.Background(),
		t.TempDir(),
		func(string) error {
			published = true
			return nil
		},
		func() (bool, error) {
			if !published {
				t.Fatal("credential mutation ran before generation publication")
			}
			return true, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestWithAccountDiskMutationPublicationSkipsNoOpGeneration(t *testing.T) {
	published := 0
	err := withAccountDiskMutationPublication(
		context.Background(),
		t.TempDir(),
		func(string) error {
			published++
			return nil
		},
		func(func() error) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if published != 0 {
		t.Fatalf("no-op refresh published %d account generations, want 0", published)
	}
}

func TestWithAccountDiskMutationPublicationRunsHookOnceBeforeMutation(t *testing.T) {
	published := 0
	mutated := false
	err := withAccountDiskMutationPublication(
		context.Background(),
		t.TempDir(),
		func(string) error {
			if mutated {
				t.Fatal("account generation published after credential mutation")
			}
			published++
			return nil
		},
		func(publish func() error) error {
			if err := publish(); err != nil {
				return err
			}
			if err := publish(); err != nil {
				return err
			}
			mutated = true
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if published != 1 || !mutated {
		t.Fatalf("publication/mutation = %d/%v, want 1/true", published, mutated)
	}
}

func TestRollbackUnpublishedAccountDiskMutationRemovesBeforePublication(t *testing.T) {
	removed := false
	err := rollbackUnpublishedAccountDiskMutation(
		context.Background(),
		t.TempDir(),
		func(string) error {
			if !removed {
				t.Fatal("unpublished credential was not removed before rollback publication")
			}
			return nil
		},
		func() error {
			removed = true
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRollbackUnpublishedAccountDiskMutationKeepsRemovalWhenPublicationFails(t *testing.T) {
	removed := false
	wantErr := errors.New("generation unavailable")
	err := rollbackUnpublishedAccountDiskMutation(
		context.Background(),
		t.TempDir(),
		func(string) error { return wantErr },
		func() error {
			removed = true
			return nil
		},
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want publication failure", err)
	}
	if !removed {
		t.Fatal("publication failure prevented unpublished credential removal")
	}
}

func TestAccountRefKeepsServingCompleteSnapshotDuringPublishedMutation(t *testing.T) {
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	seed := accounts.StoredCodexAccount{
		Email: "apikey:seed", Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "sk-seed"},
	}
	if err := store.SaveStored(seed); err != nil {
		t.Fatal(err)
	}
	ref, err := OpenAccountRef(store, agentclaude.Store{Dir: t.TempDir()}, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	held, err := lockAccountImportTransaction(context.Background(), store.StoreDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := advanceAccountDiskGeneration(store.StoreDir()); err != nil {
		_ = held.Close()
		t.Fatal(err)
	}
	added := accounts.StoredCodexAccount{
		Email: "apikey:added", Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "sk-added"},
	}
	if err := store.SaveStored(added); err != nil {
		_ = held.Close()
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	reloaded, _, err := ref.reloadIfDiskGenerationChanged(ctx)
	if err != nil || reloaded {
		_ = held.Close()
		t.Fatalf("in-flight mutation reload = %v, %v, want false/nil", reloaded, err)
	}
	if len(ref.All()) != 1 {
		_ = held.Close()
		t.Fatalf("in-flight mutation exposed partial snapshot: %+v", ref.All())
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	reloaded, _, err = ref.reloadIfDiskGenerationChanged(context.Background())
	if err != nil || !reloaded {
		t.Fatalf("completed mutation reload = %v, %v, want true/nil", reloaded, err)
	}
	if len(ref.All()) != 2 {
		t.Fatalf("completed mutation snapshot = %+v", ref.All())
	}
}

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
