package proxy

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/selectacct"
)

// Regression: a usage-score refresh must never demote a healthy account to
// exhausted using stale data. The upstream usage endpoint rate-limits under
// load, so the per-request re-score read stale "last good" cooked windows and
// overwrote the scheduler's healthy scores with zeros, after which the router
// routed traffic to dead accounts. scoreAccounts must preserve the last known
// score when it cannot fetch FRESH usage.
func TestScoreAccountsPreservesHealthyScoreWhenUsageIsStale(t *testing.T) {
	transport := &usageRoundTripper{responses: []*http.Response{usage429Response()}}
	ref := cacheTestAccountRef(t, transport)
	account := accounts.Account{ID: "claude@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok"}

	// Seed a STALE, cooked last-known-good usage entry so the live (rate-limited
	// 429) fetch falls back to it and reports fresh=false.
	key := account.ID + "\x00" + string(account.Provider)
	ref.usageWindowsMu.Lock()
	if ref.usageWindows == nil {
		ref.usageWindows = map[string]usageWindowsEntry{}
	}
	ref.usageWindows[key] = usageWindowsEntry{
		windows: []accounts.UsageWindow{{Name: "5h", UsedPercent: 100, LimitWindowSeconds: 5 * 60 * 60}},
		at:      time.Now().Add(-usageWindowsTTL - time.Minute),
	}
	ref.usageWindowsMu.Unlock()

	server := Server{
		AccountRef: ref,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "claude@example.com", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
		})),
	}

	scores, scored := server.scoreAccounts(context.Background(), []accounts.Account{account})
	if scored != 0 {
		t.Fatalf("scored = %d, want 0 (stale usage must not count as a fresh score)", scored)
	}
	if len(scores) != 1 {
		t.Fatalf("scores = %d, want 1", len(scores))
	}
	if scores[0].Headroom <= 0 || scores[0].ShortHeadroom <= 0 {
		t.Fatalf("healthy account clobbered to exhausted by stale usage: %+v", scores[0])
	}
}
