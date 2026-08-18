package proxy

import (
	"bytes"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// A Codex session that moves to another account loses the upstream prompt
// cache and re-bills its whole prefix, so the move has to be logged.
func TestAccountForSessionLogsAccountMove(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "session-1", "spent@example.com", ""); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	server := Server{
		Accounts: []accounts.Account{
			{ID: "spent@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "tok-spent"},
			{ID: "fresh@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "tok-fresh"},
		},
		Sessions: store,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "spent@example.com", Provider: accounts.ProviderCodex, Headroom: 0.2, ShortHeadroom: 0.2},
			{AccountID: "fresh@example.com", Provider: accounts.ProviderCodex, Headroom: 1, ShortHeadroom: 1},
		})),
		MaxBodyBytes: 1024,
		Logger:       slog.New(slog.NewTextHandler(&logs, nil)),
	}
	req, err := http.NewRequest(http.MethodPost, "https://subrouter.test/v1/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Agent", "codex")
	req.Header.Set("X-Subrouter-Session", "session-1")
	account, _, _, err := server.accountForSessionProvider(accounts.ProviderCodex, "codex", "session-1", req)
	if err != nil {
		t.Fatal(err)
	}
	if account.ID != "fresh@example.com" {
		t.Fatalf("account = %q, want the session rerouted to fresh@example.com", account.ID)
	}
	if !strings.Contains(logs.String(), "session moved to another account") {
		t.Fatalf("account move was not logged: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "from_account=spent@example.com") ||
		!strings.Contains(logs.String(), "to_account=fresh@example.com") {
		t.Fatalf("account move log is missing the from/to accounts: %s", logs.String())
	}
}

// A session that stays on its account must not report a cache-breaking move.
func TestAccountForSessionDoesNotLogStickyReuseAsMove(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "session-1", "healthy@example.com", ""); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	server := Server{
		Accounts: []accounts.Account{
			{ID: "healthy@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "tok-healthy"},
			{ID: "other@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "tok-other"},
		},
		Sessions: store,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "healthy@example.com", Provider: accounts.ProviderCodex, Headroom: 1, ShortHeadroom: 1},
			{AccountID: "other@example.com", Provider: accounts.ProviderCodex, Headroom: 1, ShortHeadroom: 1},
		})),
		MaxBodyBytes: 1024,
		Logger:       slog.New(slog.NewTextHandler(&logs, nil)),
	}
	req, err := http.NewRequest(http.MethodPost, "https://subrouter.test/v1/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Agent", "codex")
	req.Header.Set("X-Subrouter-Session", "session-1")
	account, _, _, err := server.accountForSessionProvider(accounts.ProviderCodex, "codex", "session-1", req)
	if err != nil {
		t.Fatal(err)
	}
	if account.ID != "healthy@example.com" {
		t.Fatalf("account = %q, want the sticky account reused", account.ID)
	}
	if strings.Contains(logs.String(), "session moved to another account") {
		t.Fatalf("sticky reuse was logged as a move: %s", logs.String())
	}
}
