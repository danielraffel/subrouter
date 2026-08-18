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
			{AccountID: "spent@example.com", Provider: accounts.ProviderCodex, Headroom: 0.02, ShortHeadroom: 0.02},
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

// Every account in a busy pool sits well past the new-session threshold. An
// idle session must still stay where its upstream prompt cache lives.
func TestStickyCodexSessionSurvivesConstrainedAccount(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "session-1", "busy@example.com", ""); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	server := Server{
		Accounts: []accounts.Account{
			{ID: "busy@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "tok-busy"},
			{ID: "other@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "tok-other"},
		},
		Sessions: store,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			// 23% and 26% headroom: both are below MinNewSessionHeadroom, which
			// is the normal state of a pool under load.
			{AccountID: "busy@example.com", Provider: accounts.ProviderCodex, Headroom: 0.23, ShortHeadroom: 0.23},
			{AccountID: "other@example.com", Provider: accounts.ProviderCodex, Headroom: 0.26, ShortHeadroom: 0.26},
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
	if account.ID != "busy@example.com" {
		t.Fatalf("account = %q, want the session held on busy@example.com", account.ID)
	}
	if strings.Contains(logs.String(), "session moved to another account") {
		t.Fatalf("session was moved off a merely constrained account: %s", logs.String())
	}
}

// An account with almost nothing left cannot serve the session any more, so
// the move happens there instead.
func TestStickyCodexSessionLeavesNearlyEmptyAccount(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "session-1", "empty@example.com", ""); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	server := Server{
		Accounts: []accounts.Account{
			{ID: "empty@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "tok-empty"},
			{ID: "other@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "tok-other"},
		},
		Sessions: store,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "empty@example.com", Provider: accounts.ProviderCodex, Headroom: 0.01, ShortHeadroom: 0.01},
			{AccountID: "other@example.com", Provider: accounts.ProviderCodex, Headroom: 0.26, ShortHeadroom: 0.26},
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
	if account.ID != "other@example.com" {
		t.Fatalf("account = %q, want the session moved off the empty account", account.ID)
	}
	if !strings.Contains(logs.String(), "session moved to another account") {
		t.Fatalf("account move was not logged: %s", logs.String())
	}
}
