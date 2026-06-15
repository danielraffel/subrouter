package proxy

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/selectacct"
	"github.com/manaflow-ai/subrouter/internal/session"
)

// TestAccountForSessionCodexNotExhaustedByCollidingClaudeScore guards against
// codex and claude accounts that share an email colliding in the single shared
// scheduler. A claude profile and a codex account for the same person both have
// Account.ID == "user@example.com"; before the fix the scheduler map was keyed
// by the bare ID, so an exhausted claude score marked the healthy codex account
// exhausted and codex requests 503'd with "no usable OAuth codex accounts
// available". The scheduler entry below simulates that collided/poisoned bare
// key. A codex request must not be blocked by it.
func TestAccountForSessionCodexNotExhaustedByCollidingClaudeScore(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	const email = "user@example.com"
	server := Server{
		Accounts: []accounts.Account{
			{ID: email, Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "tok-codex"},
			{ID: email, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-claude"},
		},
		Sessions: store,
		// Poisoned bare-email entry: the claude side exhausted the shared key.
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			{AccountID: email, Headroom: 0, ShortHeadroom: 0},
		})),
	}

	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader("{}"))
	account, _, _, err := server.accountForSession("codex", "session-new", req)
	if err != nil {
		t.Fatalf("codex request was blocked by colliding claude score: %v", err)
	}
	if account.Provider != accounts.ProviderCodex || account.ID != email {
		t.Fatalf("selected account = %s/%s, want codex/%s", account.Provider, account.ID, email)
	}
}
