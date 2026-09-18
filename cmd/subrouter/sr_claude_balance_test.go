package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// Chromium's macOS cookie key: PBKDF2-SHA1("password", "saltysalt", 1003, 16).
// Expected value produced by hashlib.pbkdf2_hmac.
func TestPBKDF2SHA1ChromiumKey(t *testing.T) {
	key := pbkdf2SHA1([]byte("password"), []byte("saltysalt"), 1003, 16)
	if got := hex.EncodeToString(key); got != "9395139d5abdba8b749042ad882c0937" {
		t.Fatalf("pbkdf2SHA1 key = %s", got)
	}
}

// v10 fixtures were encrypted with AES-128-CBC, key 9395139d..., IV of 16
// space bytes, over "sk-ant-test-session-key-value". The second has the
// Chrome 80+ SHA256(host_key) plaintext prefix.
func TestDecryptChromiumCookieValue(t *testing.T) {
	key, err := hex.DecodeString("9395139d5abdba8b749042ad882c0937")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		hex  string
	}{
		{"plain", "76313052b43e8a288bf88bf8e59cd64db23ebbe428c35fc15b7983e146b80c7d550085"},
		{"host-prefixed", "7631304a6838bc052d1ab8220899577356683a670f623ff0cff0cba8ffd2839339ae37f406738045c432621fc8dbdbc45913e5d268294ae96b6867bdbfc411ae14ad7b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encrypted, err := hex.DecodeString(tc.hex)
			if err != nil {
				t.Fatal(err)
			}
			value, err := decryptChromiumCookieValue(encrypted, key)
			if err != nil {
				t.Fatal(err)
			}
			if value != "sk-ant-test-session-key-value" {
				t.Fatalf("decrypted %q", value)
			}
		})
	}
	if _, err := decryptChromiumCookieValue([]byte("v11aaaaaaaaaaaaaaaa"), key); err == nil {
		t.Fatal("expected error for non-v10 value")
	}
}

// claudeWebTestServer serves the account -> organizations -> prepaid credits
// chain, rotating sessionKey via Set-Cookie on the credits response.
func claudeWebTestServer(t *testing.T, email, orgID string, balanceCents float64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("sessionKey")
		if err != nil || !strings.HasPrefix(cookie.Value, "sk-ant-") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/account":
			fmt.Fprintf(w, `{"email_address": %q}`, email)
		case "/organizations":
			fmt.Fprintf(w, `[{"uuid": "other", "capabilities": ["api"]}, {"uuid": %q, "capabilities": ["chat"]}]`, orgID)
		case "/organizations/" + orgID + "/prepaid/credits":
			http.SetCookie(w, &http.Cookie{Name: "sessionKey", Value: "sk-ant-rotated"})
			fmt.Fprintf(w, `{"amount": %v, "currency": "USD"}`, balanceCents)
		default:
			http.NotFound(w, r)
		}
	}))
}

func claudeWebTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SUBROUTER_STATE_DIR", t.TempDir())
	// CodexDir() runs a legacy-migration check against ~/.codex-accounts; give
	// tests a temp HOME so they never read the real one.
	t.Setenv("HOME", t.TempDir())
	oldBase := claudeWebBaseURL
	oldDiscover := claudeWebDiscoverSessionKeys
	t.Cleanup(func() {
		claudeWebBaseURL = oldBase
		claudeWebDiscoverSessionKeys = oldDiscover
	})
}

func TestClaudeWebFetchBalanceChain(t *testing.T) {
	claudeWebTestEnv(t)
	server := claudeWebTestServer(t, "user@example.com", "org-1", 374)
	defer server.Close()
	claudeWebBaseURL = server.URL

	session := &claudeWebSession{SessionKey: "sk-ant-initial", Source: "test"}
	email, balance, err := newClaudeWebClient().fetchBalance(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	if email != "user@example.com" {
		t.Fatalf("email = %q", email)
	}
	if balance != 374 {
		t.Fatalf("balance = %v", balance)
	}
	if session.OrgID != "org-1" {
		t.Fatalf("org = %q", session.OrgID)
	}
	if session.SessionKey != "sk-ant-rotated" {
		t.Fatalf("session key was not renewed from Set-Cookie")
	}
}

func TestClaudeWebBalances401DropsSession(t *testing.T) {
	claudeWebTestEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()
	claudeWebBaseURL = server.URL
	claudeWebDiscoverSessionKeys = func(context.Context) []claudeWebSessionKeyCandidate { return nil }

	saveClaudeWebSessions([]claudeWebSession{{SessionKey: "sk-ant-dead", Email: "user@example.com"}})
	balances := claudeWebBalances(context.Background(), map[string]bool{"user@example.com": true})
	if len(balances) != 0 {
		t.Fatalf("balances = %v", balances)
	}
	if sessions := loadClaudeWebSessions(); len(sessions) != 0 {
		t.Fatalf("dead session was not dropped: %+v", sessions)
	}
}

func TestClaudeWebBalancesUsesFreshCache(t *testing.T) {
	claudeWebTestEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("network must not be touched when the cache is fresh")
	}))
	defer server.Close()
	claudeWebBaseURL = server.URL

	saveClaudeWebBalanceCache(claudeWebBalanceCacheFile{Balances: map[string]claudeWebBalanceCacheEntry{
		"user@example.com": {BalanceCents: 374, FetchedAt: time.Now()},
	}})
	balances := claudeWebBalances(context.Background(), map[string]bool{"user@example.com": true})
	if balances["user@example.com"] != 374 {
		t.Fatalf("balances = %v", balances)
	}
}

func TestClaudeWebBalancesDiscoversAndPersists(t *testing.T) {
	claudeWebTestEnv(t)
	server := claudeWebTestServer(t, "user@example.com", "org-1", 374)
	defer server.Close()
	claudeWebBaseURL = server.URL
	claudeWebDiscoverSessionKeys = func(context.Context) []claudeWebSessionKeyCandidate {
		return []claudeWebSessionKeyCandidate{
			{SessionKey: "not-a-claude-key", Source: "test"},
			{SessionKey: "sk-ant-discovered", Source: "Chrome"},
		}
	}

	balances := claudeWebBalances(context.Background(), map[string]bool{"user@example.com": true})
	if balances["user@example.com"] != 374 {
		t.Fatalf("balances = %v", balances)
	}

	sessions := loadClaudeWebSessions()
	if len(sessions) != 1 {
		t.Fatalf("sessions = %+v", sessions)
	}
	session := sessions[0]
	if session.SessionKey != "sk-ant-rotated" || session.Email != "user@example.com" || session.OrgID != "org-1" || session.Source != "Chrome" {
		t.Fatalf("persisted session = %+v", session)
	}
	info, err := os.Stat(claudeWebSessionsPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("session file mode = %o", info.Mode().Perm())
	}

	// The fresh cache entry must serve the next run without network or
	// discovery.
	server.Close()
	claudeWebDiscoverSessionKeys = func(context.Context) []claudeWebSessionKeyCandidate {
		t.Error("discovery must not run when the cache is fresh")
		return nil
	}
	balances = claudeWebBalances(context.Background(), map[string]bool{"user@example.com": true})
	if balances["user@example.com"] != 374 {
		t.Fatalf("cached balances = %v", balances)
	}
}

func TestEnrichClaudeRowsWithWebBalances(t *testing.T) {
	claudeWebTestEnv(t)
	server := claudeWebTestServer(t, "user@example.com", "org-1", 374)
	defer server.Close()
	claudeWebBaseURL = server.URL
	claudeWebDiscoverSessionKeys = func(context.Context) []claudeWebSessionKeyCandidate {
		return []claudeWebSessionKeyCandidate{{SessionKey: "sk-ant-discovered", Source: "test"}}
	}

	limit := 5000.0
	used := 4626.0
	rows := []srUsageRow{
		{
			// Case-insensitive email match; extra usage lives on the
			// synthetic window the way local rows carry it.
			email:    "User@Example.com",
			provider: accounts.ProviderClaude,
			authMode: accounts.AuthModeOAuth,
			windows: []accounts.UsageWindow{{
				Name:       "Extra usage",
				ExtraUsage: &accounts.ExtraUsageInfo{IsEnabled: true, MonthlyLimit: &limit, UsedCredits: &used},
			}},
		},
		{
			// No server extra-usage data at all.
			email:    "user@example.com",
			provider: accounts.ProviderClaude,
			authMode: accounts.AuthModeOAuth,
		},
		{
			email:    "other@example.com",
			provider: accounts.ProviderClaude,
			authMode: accounts.AuthModeOAuth,
		},
		{
			email:    "user@example.com",
			provider: accounts.ProviderCodex,
			authMode: accounts.AuthModeOAuth,
		},
	}
	enrichClaudeRowsWithWebBalances(context.Background(), rows)

	extra := claudeExtraUsageForRow(rows[0])
	if extra == nil || extra.CreditsBalance == nil || *extra.CreditsBalance != 374 {
		t.Fatalf("row 0 extra = %+v", extra)
	}
	if extra.MonthlyLimit == nil || *extra.MonthlyLimit != 5000 {
		t.Fatalf("row 0 lost its monthly limit: %+v", extra)
	}
	if rows[1].extraUsage == nil || rows[1].extraUsage.CreditsBalance == nil || *rows[1].extraUsage.CreditsBalance != 374 {
		t.Fatalf("row 1 extra = %+v", rows[1].extraUsage)
	}
	if rows[2].extraUsage != nil {
		t.Fatalf("unmatched row was enriched: %+v", rows[2].extraUsage)
	}
	if rows[3].extraUsage != nil {
		t.Fatalf("non-Claude row was enriched: %+v", rows[3].extraUsage)
	}
}

func TestEnrichClaudeRowsWithWebBalancesNoClaudeRows(t *testing.T) {
	claudeWebTestEnv(t)
	claudeWebDiscoverSessionKeys = func(context.Context) []claudeWebSessionKeyCandidate {
		t.Error("discovery must not run without Claude rows")
		return nil
	}
	rows := []srUsageRow{{email: "user@example.com", provider: accounts.ProviderCodex}}
	enrichClaudeRowsWithWebBalances(context.Background(), rows)
}

func TestEnrichClaudeRowsWithWebBalancesNonDarwinNoop(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("non-darwin stub behavior")
	}
	claudeWebTestEnv(t)
	rows := []srUsageRow{{email: "user@example.com", provider: accounts.ProviderClaude}}
	enrichClaudeRowsWithWebBalances(context.Background(), rows)
	if rows[0].extraUsage != nil {
		t.Fatalf("non-darwin stub enriched a row: %+v", rows[0].extraUsage)
	}
}

func TestUsageGridClaudeExtraSpendCellPrefersBalance(t *testing.T) {
	limit := 5000.0
	used := 2204.0
	balance := 374.0
	zero := 0.0

	cell := usageGridClaudeExtraSpendCell(srUsageRow{extraUsage: &accounts.ExtraUsageInfo{
		IsEnabled: true, MonthlyLimit: &limit, UsedCredits: &used, CreditsBalance: &balance,
	}})
	if cell.Text != "$3.74/$50.00" || cell.Style != ansiGreen {
		t.Fatalf("balance cell = %+v", cell)
	}

	cell = usageGridClaudeExtraSpendCell(srUsageRow{extraUsage: &accounts.ExtraUsageInfo{
		IsEnabled: true, MonthlyLimit: &limit, CreditsBalance: &zero,
	}})
	if cell.Text != "$0.00/$50.00" || cell.Style != ansiYellow {
		t.Fatalf("empty balance cell = %+v", cell)
	}

	cell = usageGridClaudeExtraSpendCell(srUsageRow{extraUsage: &accounts.ExtraUsageInfo{
		IsEnabled: true, MonthlyLimit: &limit, UsedCredits: &used,
	}})
	if cell.Text != "$22.04/$50.00" || cell.Style != ansiGreen {
		t.Fatalf("used/limit fallback cell = %+v", cell)
	}

	cell = usageGridClaudeExtraSpendCell(srUsageRow{extraUsage: &accounts.ExtraUsageInfo{
		IsEnabled: true, UsedCredits: &used, CreditsBalance: &balance,
	}})
	if cell.Text != "?" {
		t.Fatalf("missing limit cell = %+v", cell)
	}

	cell = usageGridClaudeExtraSpendCell(srUsageRow{extraUsage: &accounts.ExtraUsageInfo{
		IsEnabled: false, MonthlyLimit: &limit, CreditsBalance: &balance,
	}})
	if cell.Text != "" {
		t.Fatalf("disabled cell = %+v", cell)
	}
}

// A stored session that is still valid must be reused instead of re-reading
// browser cookies, and the rotated key must be persisted back.
func TestClaudeWebBalancesReusesStoredSession(t *testing.T) {
	claudeWebTestEnv(t)
	server := claudeWebTestServer(t, "user@example.com", "org-1", 900)
	defer server.Close()
	claudeWebBaseURL = server.URL
	claudeWebDiscoverSessionKeys = func(context.Context) []claudeWebSessionKeyCandidate {
		t.Error("discovery must not run when a stored session works")
		return nil
	}

	saveClaudeWebSessions([]claudeWebSession{{SessionKey: "sk-ant-stored", Email: "user@example.com"}})
	balances := claudeWebBalances(context.Background(), map[string]bool{"user@example.com": true})
	if balances["user@example.com"] != 900 {
		t.Fatalf("balances = %v", balances)
	}
	sessions := loadClaudeWebSessions()
	if len(sessions) != 1 || sessions[0].SessionKey != "sk-ant-rotated" {
		data, _ := json.Marshal(sessions)
		t.Fatalf("sessions after renewal = %s", data)
	}
}
