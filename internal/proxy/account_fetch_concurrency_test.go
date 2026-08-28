package proxy

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
)

type concurrencyTrackingTransport struct {
	mu      sync.Mutex
	current int
	maximum int
	calls   int
}

func (t *concurrencyTrackingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.current++
	t.calls++
	if t.current > t.maximum {
		t.maximum = t.current
	}
	t.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	t.mu.Lock()
	t.current--
	t.mu.Unlock()
	return usageOKResponse(), nil
}

func (t *concurrencyTrackingTransport) counts() (int, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls, t.maximum
}

func accountFetchConcurrencyFixture(t *testing.T) (*AccountRef, []accounts.Account, *concurrencyTrackingTransport) {
	t.Helper()
	store := accounts.CodexStore{Dir: t.TempDir()}
	available := make([]accounts.Account, 0, 8)
	for i := 0; i < 8; i++ {
		id := "account-" + string(rune('a'+i)) + "@example.com"
		token := proxyTestCodexJWT(id, id, time.Now().Add(time.Hour))
		stored := accounts.StoredCodexAccount{
			Email: id, Provider: accounts.ProviderCodex,
			OAuthCredentialOrigin: accounts.CodexOAuthOriginIsolatedServerLogin,
			Auth: accounts.CodexAuthFile{AuthMode: "chatgpt", Tokens: &accounts.CodexTokens{
				AccessToken: token, RefreshToken: "refresh-" + id, IDToken: token,
			}},
		}
		if err := store.SaveStored(stored); err != nil {
			t.Fatal(err)
		}
		account, ok := stored.Account(stored.SourcePath(store))
		if !ok {
			t.Fatal("stored test account is unusable")
		}
		available = append(available, account)
	}
	transport := &concurrencyTrackingTransport{}
	ref := NewAccountRef(store, available, &http.Client{Transport: transport})
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	return ref, available, transport
}

func TestUsageStatusesLiveBoundsUpstreamConcurrency(t *testing.T) {
	ref, _, transport := accountFetchConcurrencyFixture(t)
	statuses := ref.usageStatusesLive(context.Background())
	if len(statuses) != 8 {
		t.Fatalf("statuses = %d, want 8", len(statuses))
	}
	calls, maximum := transport.counts()
	if calls != 8 || maximum != accountFetchConcurrency {
		t.Fatalf("usage fetch calls/max = %d/%d, want 8/%d", calls, maximum, accountFetchConcurrency)
	}
}

func TestScoreAccountsBoundsUpstreamConcurrency(t *testing.T) {
	ref, available, transport := accountFetchConcurrencyFixture(t)
	server := Server{
		AccountRef: ref,
		Scheduler:  selectacct.NewScheduler(nil),
	}
	_, scored := server.scoreAccounts(context.Background(), available)
	if scored != 8 {
		t.Fatalf("scored = %d, want 8", scored)
	}
	calls, maximum := transport.counts()
	if calls != 8 || maximum != accountFetchConcurrency {
		t.Fatalf("score fetch calls/max = %d/%d, want 8/%d", calls, maximum, accountFetchConcurrency)
	}
}
