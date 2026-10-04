package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// Drain-first sends Codex traffic to the first listed account with quota,
// skips an exhausted one, and stops once its time has passed.
func TestDrainFirstAccountOrderAndExhaustion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "drain-first.json")
	write := func(d DrainFirst) {
		body, _ := json.Marshal(d)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	available := []accounts.Account{
		{ID: "a", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
		{ID: "steven", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
		{ID: "djr", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
	}
	scheduler := selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "a", Provider: accounts.ProviderCodex, Headroom: 0.9, ShortHeadroom: 1, Fresh: true},
		{AccountID: "steven", Provider: accounts.ProviderCodex, Headroom: 0.04, ShortHeadroom: 1, Fresh: true},
		{AccountID: "djr", Provider: accounts.ProviderCodex, Headroom: 0.5, ShortHeadroom: 1, Fresh: true},
	})
	server := Server{DrainFirstPath: path}
	write(DrainFirst{Accounts: []string{"steven", "djr"}})
	if got, ok := server.drainFirstAccount(accounts.ProviderCodex, available, scheduler); !ok || got.ID != "steven" {
		t.Fatalf("picked %q, %v; want steven, even at 4%% left", got.ID, ok)
	}
	exhausted := selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "steven", Provider: accounts.ProviderCodex, Headroom: 0, ShortHeadroom: 1, Fresh: true},
		{AccountID: "djr", Provider: accounts.ProviderCodex, Headroom: 0.5, ShortHeadroom: 1, Fresh: true},
	})
	if got, ok := server.drainFirstAccount(accounts.ProviderCodex, available, exhausted); !ok || got.ID != "djr" {
		t.Fatalf("with steven exhausted picked %q, %v; want djr", got.ID, ok)
	}
	if _, ok := server.drainFirstAccount(accounts.ProviderClaude, available, scheduler); ok {
		t.Fatal("drain-first must not move Claude traffic")
	}
	write(DrainFirst{Accounts: []string{"steven"}, Until: time.Now().Add(-time.Minute)})
	if _, ok := server.drainFirstAccount(accounts.ProviderCodex, available, scheduler); ok {
		t.Fatal("an expired drain-first list still applied")
	}
}
