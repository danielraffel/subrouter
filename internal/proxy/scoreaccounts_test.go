package proxy

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
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

func TestScoreAccountsTreatsAuthOnlyOAuthRefreshAsAuthEvidenceOnly(t *testing.T) {
	account := accounts.Account{
		ID: "antigravity", Provider: accounts.ProviderAntigravity,
		AuthMode: accounts.AuthModeOAuth, Token: "access",
	}
	source := &stubOAuthSource{
		provider:  accounts.ProviderAntigravity,
		listed:    []accounts.Account{account},
		refreshed: accounts.Account{ID: account.ID, Provider: account.Provider, AuthMode: account.AuthMode, Token: "fresh"},
	}
	networkCalls := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		networkCalls++
		return nil, errors.New("unexpected quota request")
	})}
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{account}, client)
	ref.oauthSources = []OAuthAccountSource{source}
	server := Server{
		AccountRef: ref,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{{
			AccountID: account.ID, Provider: account.Provider, Headroom: 0.63, ShortHeadroom: 0.61,
		}})),
	}

	scores, scored := server.scoreAccounts(t.Context(), []accounts.Account{account})
	if source.refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want one auth check", source.refreshCalls)
	}
	if networkCalls != 0 {
		t.Fatalf("unsupported quota endpoint was polled %d time(s)", networkCalls)
	}
	if scored != 0 || len(scores) != 1 || scores[0].Headroom != 0.63 || scores[0].ShortHeadroom != 0.61 || scores[0].Fresh {
		t.Fatalf("auth-only routing score = %+v, scored=%d", scores, scored)
	}
}

func TestAuthOnlyOAuthRefreshPreservesRequestTimeExhaustion(t *testing.T) {
	account := accounts.Account{
		ID: "antigravity", Provider: accounts.ProviderAntigravity,
		AuthMode: accounts.AuthModeOAuth, Token: "access",
	}
	source := &stubOAuthSource{
		provider:  accounts.ProviderAntigravity,
		listed:    []accounts.Account{account},
		refreshed: accounts.Account{ID: account.ID, Provider: account.Provider, AuthMode: account.AuthMode, Token: "fresh"},
	}
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{account}, nil)
	ref.oauthSources = []OAuthAccountSource{source}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{{
		AccountID: account.ID, Provider: account.Provider, Headroom: 1, ShortHeadroom: 1,
	}}))
	_, generation, revision := ref.CredentialSnapshot()
	schedulerRef.AdvanceAccountGenerationWithAccounts(generation, revision, SchedulerAccounts([]accounts.Account{account}))
	schedulerRef.MarkExhaustedUntil(account.Provider, account.ID, "", time.Now().Add(time.Hour))
	server := Server{AccountRef: ref, SchedulerRef: schedulerRef, UsageScoreTTL: time.Nanosecond}

	server.refreshUsageScoresIfStale(t.Context())
	if source.refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want one auth-only refresh", source.refreshCalls)
	}
	if !schedulerRef.Get().Exhausted(account.Provider, account.ID) {
		t.Fatal("auth-only token refresh cleared request-time quota exhaustion")
	}
}

// Regression: request-time exhaustion is an expiring overlay, not measured
// usage. If a refresh seeds its carried-forward score from that overlay and the
// mark expires before FinishRefresh, the zero is stranded in the base scheduler
// with no expiry. Simulate that ordering deterministically: score while marked,
// prune the mark, then publish the stale refresh result.
func TestScoreAccountsDoesNotBakeExpiringExhaustionOverlay(t *testing.T) {
	transport := &usageRoundTripper{responses: []*http.Response{usage429Response()}}
	accountRef := cacheTestAccountRef(t, transport)
	acct := accounts.Account{ID: "claude@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok"}
	key := acct.ID + "\x00" + string(acct.Provider)
	accountRef.usageWindowsMu.Lock()
	accountRef.usageWindows = map[string]usageWindowsEntry{
		key: {
			windows: []accounts.UsageWindow{{Name: "5h", UsedPercent: 100, LimitWindowSeconds: 5 * 60 * 60}},
			at:      time.Now().Add(-usageWindowsTTL - time.Minute),
		},
	}
	accountRef.usageWindowsMu.Unlock()

	ref := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{{
		AccountID: acct.ID, Provider: acct.Provider, Headroom: 0.75, ShortHeadroom: 0.75,
	}}))
	ref.MarkExhaustedUntil(acct.Provider, acct.ID, "", time.Now().Add(time.Hour))
	server := Server{AccountRef: accountRef, SchedulerRef: ref}

	scores, scored := server.scoreAccounts(context.Background(), []accounts.Account{acct})
	if scored != 0 {
		t.Fatalf("scored = %d, want stale carried-forward score", scored)
	}
	if scores[0].Headroom != 0.75 || scores[0].ShortHeadroom != 0.75 {
		t.Fatalf("refresh seed included temporary exhaustion overlay: %+v", scores[0])
	}

	// Deterministically model the original mark expiring while scoring was in
	// flight, before the resulting scheduler is published.
	ref.MarkExhaustedUntil(acct.Provider, acct.ID, "", time.Now().Add(-time.Second))
	_ = ref.Get() // prune the lapsed mark
	ref.FinishRefresh(selectacct.NewScheduler(scores), true)
	got := ref.Get().ScoreFor(acct.Provider, acct.ID)
	if got.Headroom != 0.75 || got.ShortHeadroom != 0.75 || ref.Get().Exhausted(acct.Provider, acct.ID) {
		t.Fatalf("expired overlay was baked into scheduler base: %+v", got)
	}
}

// Regression: an account whose refresh token is dead (invalid_grant) only
// recovers via human re-auth, so probing it again costs a doomed round trip on
// a path that fronts proxy requests. With a fully expired Claude pool that made
// every scoring sweep pay N dead round trips and `sr` stopped responding.
// scoreAccounts must zero a known-dead account without any network call.
func TestScoreAccountsSkipsKnownDeadCredentialWithoutNetwork(t *testing.T) {
	transport := &usageRoundTripper{responses: []*http.Response{usageOKResponse()}}
	ref := cacheTestAccountRef(t, transport)
	account := accounts.Account{ID: "claude@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok"}

	ref.noteCredResult(account, errors.New(`Claude OAuth refresh failed: 400 Bad Request: {"error": "invalid_grant"}`))

	server := Server{
		AccountRef: ref,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "claude@example.com", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
		})),
	}

	scores, scored := server.scoreAccounts(context.Background(), []accounts.Account{account})
	if transport.calls != 0 {
		t.Fatalf("transport.calls = %d, want 0 (a dead credential must not be re-probed)", transport.calls)
	}
	if scored != 0 {
		t.Fatalf("scored = %d, want 0", scored)
	}
	if len(scores) != 1 {
		t.Fatalf("scores = %d, want 1", len(scores))
	}
	if scores[0].Headroom != 0 || scores[0].ShortHeadroom != 0 {
		t.Fatalf("dead credential must be zeroed out of routing: %+v", scores[0])
	}
}

// A re-authed account must rejoin routing immediately rather than waiting out
// the fast-fail TTL, so any non-terminal result clears the remembered failure.
func TestNoteCredResultClearsRememberedFailure(t *testing.T) {
	ref := &AccountRef{}
	account := accounts.Account{Provider: accounts.ProviderClaude, ID: "claude@example.com", Token: "credential"}

	ref.noteCredResult(account, errors.New("invalid_grant"))
	if _, dead := ref.terminalCredFailure(account); !dead {
		t.Fatal("terminal credential error was not remembered")
	}

	ref.noteCredResult(account, nil)
	if _, dead := ref.terminalCredFailure(account); dead {
		t.Fatal("a successful refresh must clear the remembered failure")
	}

	// A transient error is not a credential verdict and must not re-arm it.
	ref.noteCredResult(account, context.DeadlineExceeded)
	if _, dead := ref.terminalCredFailure(account); dead {
		t.Fatal("a timeout must not be treated as a dead credential")
	}
}

// Regression: a Codex account and a Claude account routinely share the same ID
// (a Codex email equals a Claude profile name). Mutating the account list
// (refresh/replace) must match provider too, or one provider's update silently
// overwrites the other's entry, dropping it from selection. This is exactly
// what hid the best Codex accounts (e.g. aziz@cmux.com) behind their Claude
// namesakes so Codex never routed to them.
func TestAccountRefReplaceDoesNotClobberAcrossProviders(t *testing.T) {
	ref := &AccountRef{
		accounts: []accounts.Account{
			{ID: "shared@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "codex-tok"},
			{ID: "shared@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "claude-tok"},
		},
	}

	// A Claude-side refresh must update only the Claude entry.
	ref.replace(accounts.Account{ID: "shared@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "claude-tok-2"})

	all := ref.All()
	var codex, claude *accounts.Account
	for i := range all {
		switch all[i].Provider {
		case accounts.ProviderCodex:
			codex = &all[i]
		case accounts.ProviderClaude:
			claude = &all[i]
		}
	}
	if codex == nil {
		t.Fatal("Codex account was clobbered by a same-ID Claude replace")
	}
	if codex.Token != "codex-tok" {
		t.Fatalf("Codex token = %q, want untouched codex-tok", codex.Token)
	}
	if claude == nil || claude.Token != "claude-tok-2" {
		t.Fatalf("Claude entry not updated: %+v", claude)
	}
}
