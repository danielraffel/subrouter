package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func TestRetryableUpstreamPostRequestQwenGenerationEndpoints(t *testing.T) {
	post := func(path string) *http.Request {
		return httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
	}
	for _, tc := range []struct {
		provider accounts.Provider
		path     string
	}{
		{accounts.ProviderQwen, "/chat/completions"},
		{accounts.ProviderQwenToken, "/v1/chat/completions"},
		{accounts.ProviderQwenAnthropic, "/v1/messages"},
	} {
		if !retryableUpstreamPostRequest(tc.provider, post(tc.path)) {
			t.Fatalf("%s %s should be replayable for account failover", tc.provider, tc.path)
		}
	}
	if retryableUpstreamPostRequest(accounts.ProviderQwenToken, post("/models")) {
		t.Fatal("a non-generation Qwen request must not be replayed")
	}
}

func TestRetryableUpstreamPostRequestAdditionalKeyedProviders(t *testing.T) {
	post := func(path string) *http.Request {
		return httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
	}
	for _, provider := range []accounts.Provider{
		accounts.ProviderOpenRouter,
		accounts.ProviderQwen,
		accounts.ProviderDeepSeek,
		accounts.ProviderTogether,
		accounts.ProviderFireworks,
		accounts.ProviderOpenCodeZen,
	} {
		for _, path := range []string{"/chat/completions", "/v1/responses"} {
			if !retryableUpstreamPostRequest(provider, post(path)) {
				t.Fatalf("%s %s should be replayable for account failover", provider, path)
			}
		}
	}
}

func TestKeyedProviderQuotaExhaustionIsAccountWideWithRequestModel(t *testing.T) {
	const accountID = "qwen-token:limited"
	scheduler := selectacct.NewScheduler([]selectacct.Score{{
		AccountID: accountID, Provider: accounts.ProviderQwenToken, Headroom: 1, ShortHeadroom: 1,
	}})
	server := &Server{SchedulerRef: selectacct.NewSchedulerRef(scheduler)}
	transport := usageLimitRetryTransport{
		base: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusTooManyRequests,
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Body:   io.NopCloser(strings.NewReader(`{"error":{"message":"quota exhausted"}}`)), Request: request}, nil
		}),
		server: server, provider: accounts.ProviderQwenToken, account: accountID,
		accountCredential: "credential-v1", poolModel: "qwen3.7-plus", maxAttempts: 1,
	}
	request := httptest.NewRequest(http.MethodPost, "https://qwen.test/v1/chat/completions", strings.NewReader(`{"model":"qwen3.7-plus"}`))
	request.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(`{"model":"qwen3.7-plus"}`)), nil
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if !server.SchedulerRef.Get().Exhausted(accounts.ProviderQwenToken, accountID) {
		t.Fatal("plan-wide keyed-provider quota exhaustion remained scoped to the request model")
	}
}

func TestCodexFailoverRebuildsPathForReplacementAuthMode(t *testing.T) {
	const oauthID = "codex:oauth"
	const apiID = "codex:api"
	var paths []string
	base := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		paths = append(paths, request.URL.Path)
		if len(paths) == 1 {
			return &http.Response{StatusCode: http.StatusTooManyRequests,
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Body:   io.NopCloser(strings.NewReader(`{"error":{"type":"usage_limit_reached"}}`)), Request: request}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Request: request}, nil
	})
	server := &Server{
		Accounts: []accounts.Account{
			{ID: oauthID, Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "oauth-token"},
			{ID: apiID, Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeAPIKey, Token: "api-key"},
		},
		SchedulerRef:  selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		APIUpstream:   mustParseURL(t, "https://api.openai.test"),
		CodexUpstream: mustParseURL(t, "https://chatgpt.test/backend-api/codex"),
	}
	transport := usageLimitRetryTransport{base: base, server: server, provider: accounts.ProviderCodex,
		account: oauthID, path: "/responses", poolModel: "gpt-5", maxAttempts: 2}
	request := httptest.NewRequest(http.MethodPost, "https://chatgpt.test/responses", strings.NewReader(`{"model":"gpt-5"}`))
	request.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(`{"model":"gpt-5"}`)), nil
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if len(paths) != 2 || paths[1] != "/v1/responses" {
		t.Fatalf("attempt paths = %v, want replacement API-key attempt at /v1/responses", paths)
	}
}

func TestCodexFailoverRemovesAPIPathPrefixForOAuthReplacement(t *testing.T) {
	const apiID = "codex:api"
	const oauthID = "codex:oauth"
	var paths []string
	base := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		paths = append(paths, request.URL.Path)
		if len(paths) == 1 {
			return &http.Response{StatusCode: http.StatusTooManyRequests,
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Body:   io.NopCloser(strings.NewReader(`{"error":{"type":"usage_limit_reached"}}`)), Request: request}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Request: request}, nil
	})
	server := &Server{
		Accounts: []accounts.Account{
			{ID: apiID, Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeAPIKey, Token: "api-key"},
			{ID: oauthID, Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "oauth-token"},
		},
		SchedulerRef:  selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		APIUpstream:   mustParseURL(t, "https://api.openai.test"),
		CodexUpstream: mustParseURL(t, "https://chatgpt.test/backend-api/codex"),
	}
	transport := usageLimitRetryTransport{base: base, server: server, provider: accounts.ProviderCodex,
		account: apiID, path: "/v1/responses", poolModel: "gpt-5", maxAttempts: 2}
	request := httptest.NewRequest(http.MethodPost, "https://api.openai.test/v1/responses", strings.NewReader(`{"model":"gpt-5"}`))
	request.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(`{"model":"gpt-5"}`)), nil
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if len(paths) != 2 || paths[1] != "/backend-api/codex/responses" {
		t.Fatalf("attempt paths = %v, want replacement OAuth attempt at /backend-api/codex/responses", paths)
	}
}

func TestGrokFailoverRetargetsBetweenAPIAndSubscriptionUpstreams(t *testing.T) {
	apiCalls := 0
	apiUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls++
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer api-key" {
			t.Fatalf("API attempt path=%q authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"quota exceeded"}}`)
	}))
	defer apiUpstream.Close()

	subscriptionCalls := 0
	subscriptionUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subscriptionCalls++
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer oauth-token" || r.Header.Get("X-XAI-Token-Auth") != "xai-grok-cli" {
			t.Fatalf("subscription attempt path=%q authorization=%q marker=%q", r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-XAI-Token-Auth"))
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer subscriptionUpstream.Close()

	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("grok", "grok-mixed-auth", "grok:a-api", ""); err != nil {
		t.Fatal(err)
	}
	server := Server{
		Accounts: []accounts.Account{
			{ID: "grok:a-api", Provider: accounts.ProviderGrok, AuthMode: accounts.AuthModeAPIKey, Token: "api-key"},
			{ID: "grok:z-subscription", Provider: accounts.ProviderGrok, AuthMode: accounts.AuthModeOAuth, Token: "oauth-token"},
		},
		Sessions: store, SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		GrokUpstream:             mustParseURL(t, apiUpstream.URL+"/v1"),
		GrokSubscriptionUpstream: mustParseURL(t, subscriptionUpstream.URL+"/v1"),
		MaxBodyBytes:             1024,
	}
	req := httptest.NewRequest(http.MethodPost, "/grok/chat/completions", strings.NewReader(`{"model":"grok-4","messages":[]}`))
	req.Header.Set("X-Subrouter-Session", "grok-mixed-auth")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if apiCalls != 1 || subscriptionCalls != 1 {
		t.Fatalf("upstream calls api=%d subscription=%d, want one each", apiCalls, subscriptionCalls)
	}
	if !server.SchedulerRef.Get().Exhausted(accounts.ProviderGrok, "grok:a-api") {
		t.Fatal("quota-limited Grok API key was not exhausted")
	}
	assignment, ok := store.Get("grok", "grok-mixed-auth")
	if !ok || assignment.AccountID != "grok:z-subscription" {
		t.Fatalf("Grok sticky assignment = %+v, want subscription account", assignment)
	}
}

func TestGrokFailoverRetargetsFromSubscriptionToAPIUpstream(t *testing.T) {
	subscriptionCalls := 0
	subscriptionUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		subscriptionCalls++
		if request.Header.Get("Authorization") != "Bearer oauth-token" || request.Header.Get("X-XAI-Token-Auth") != "xai-grok-cli" {
			t.Fatalf("subscription authorization=%q marker=%q", request.Header.Get("Authorization"), request.Header.Get("X-XAI-Token-Auth"))
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"Rate limit exceeded"}}`)
	}))
	defer subscriptionUpstream.Close()

	apiCalls := 0
	apiUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		apiCalls++
		if request.Header.Get("Authorization") != "Bearer api-key" || request.Header.Get("X-XAI-Token-Auth") != "" {
			t.Fatalf("API authorization=%q marker=%q", request.Header.Get("Authorization"), request.Header.Get("X-XAI-Token-Auth"))
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer apiUpstream.Close()

	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("grok", "grok-subscription-first", "grok:a-subscription", ""); err != nil {
		t.Fatal(err)
	}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	server := Server{
		Accounts: []accounts.Account{
			{ID: "grok:a-subscription", Provider: accounts.ProviderGrok, AuthMode: accounts.AuthModeOAuth, Token: "oauth-token"},
			{ID: "grok:z-api", Provider: accounts.ProviderGrok, AuthMode: accounts.AuthModeAPIKey, Token: "api-key"},
		},
		Sessions: store, SchedulerRef: schedulerRef,
		GrokUpstream:             mustParseURL(t, apiUpstream.URL+"/v1"),
		GrokSubscriptionUpstream: mustParseURL(t, subscriptionUpstream.URL+"/v1"),
		MaxBodyBytes:             1024,
	}
	req := httptest.NewRequest(http.MethodPost, "/grok/chat/completions", strings.NewReader(`{"model":"grok-4","messages":[]}`))
	req.Header.Set("X-Subrouter-Session", "grok-subscription-first")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if subscriptionCalls != 1 || apiCalls != 1 {
		t.Fatalf("upstream calls subscription=%d api=%d, want one each", subscriptionCalls, apiCalls)
	}
	if !schedulerRef.Get().Exhausted(accounts.ProviderGrok, "grok:a-subscription") {
		t.Fatal("rate-limited Grok subscription was not exhausted")
	}
	assignment, ok := store.Get("grok", "grok-subscription-first")
	if !ok || assignment.AccountID != "grok:z-api" {
		t.Fatalf("Grok sticky assignment = %+v, want API account", assignment)
	}
}

func TestHandlerFailsOverBetweenDeepSeekAPIKeys(t *testing.T) {
	var authorizations []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		authorizations = append(authorizations, auth)
		if auth == "Bearer key-primary" {
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = io.WriteString(w, `{"error":{"message":"Insufficient Balance"}}`)
			return
		}
		if auth != "Bearer key-secondary" {
			t.Fatal("upstream received an unexpected Authorization header")
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer upstream.Close()

	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	server := Server{
		Accounts: []accounts.Account{
			{ID: "deepseek:a-primary", Provider: accounts.ProviderDeepSeek, AuthMode: accounts.AuthModeAPIKey, Token: "key-primary"},
			{ID: "deepseek:z-secondary", Provider: accounts.ProviderDeepSeek, AuthMode: accounts.AuthModeAPIKey, Token: "key-secondary"},
		},
		Sessions: store, SchedulerRef: schedulerRef,
		DeepSeekUpstream: mustParseURL(t, upstream.URL+"/v1"), MaxBodyBytes: 1024,
	}
	req := httptest.NewRequest(http.MethodPost, "/deepseek/chat/completions", strings.NewReader(`{"model":"deepseek-v4-flash","messages":[]}`))
	req.Header.Set("X-Subrouter-Session", "deepseek-failover")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if want := []string{"Bearer key-primary", "Bearer key-secondary"}; !reflect.DeepEqual(authorizations, want) {
		t.Fatal("DeepSeek failover used an unexpected Authorization sequence")
	}
	if !schedulerRef.Get().Exhausted(accounts.ProviderDeepSeek, "deepseek:a-primary") {
		t.Fatal("DeepSeek 402 did not exhaust the empty-balance key")
	}
	assignment, ok := store.Get("deepseek", "deepseek-failover")
	if !ok || assignment.AccountID != "deepseek:z-secondary" {
		t.Fatalf("sticky assignment = %+v, want secondary key", assignment)
	}
}

func TestDeepSeek429FailsOverWithoutCookingTheKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.Header.Get("Authorization") {
		case "Bearer key-primary":
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"Rate Limit Reached"}}`)
		case "Bearer key-secondary":
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
		default:
			t.Error("upstream received an unexpected DeepSeek credential")
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer upstream.Close()

	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	server := Server{
		Accounts: []accounts.Account{
			{ID: "deepseek:a-primary", Provider: accounts.ProviderDeepSeek, AuthMode: accounts.AuthModeAPIKey, Token: "key-primary"},
			{ID: "deepseek:z-secondary", Provider: accounts.ProviderDeepSeek, AuthMode: accounts.AuthModeAPIKey, Token: "key-secondary"},
		},
		Sessions: store, SchedulerRef: schedulerRef,
		DeepSeekUpstream: mustParseURL(t, upstream.URL+"/v1"), MaxBodyBytes: 1024,
	}
	req := httptest.NewRequest(http.MethodPost, "/deepseek/chat/completions", strings.NewReader(`{"model":"deepseek-v4-flash","messages":[]}`))
	req.Header.Set("X-Subrouter-Session", "deepseek-transient")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want successful alternate", rec.Code)
	}
	if schedulerRef.Get().Exhausted(accounts.ProviderDeepSeek, "deepseek:a-primary") {
		t.Fatal("DeepSeek transient 429 cooked the primary key")
	}
}

func TestAdditionalKeyedProvidersFailOverOn429AndCommitStickyAccount(t *testing.T) {
	for _, provider := range []accounts.Provider{
		accounts.ProviderOpenRouter,
		accounts.ProviderQwen,
		accounts.ProviderDeepSeek,
		accounts.ProviderTogether,
		accounts.ProviderFireworks,
		accounts.ProviderOpenCodeZen,
	} {
		t.Run(string(provider), func(t *testing.T) {
			var attempts []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				authorization := request.Header.Get("Authorization")
				switch authorization {
				case "Bearer primary-key":
					attempts = append(attempts, "primary")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
				case "Bearer secondary-key":
					attempts = append(attempts, "secondary")
					_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
				default:
					attempts = append(attempts, "unexpected")
					t.Error("upstream received an unexpected credential")
					w.WriteHeader(http.StatusUnauthorized)
				}
			}))
			defer upstream.Close()

			primaryID := string(provider) + ":a-primary"
			secondaryID := string(provider) + ":z-secondary"
			server := opencodeProviderTestServer(t, accounts.Account{
				ID: primaryID, Provider: provider, AuthMode: accounts.AuthModeAPIKey, Token: "primary-key",
			}, provider, upstream.URL+"/v1")
			server.Accounts = []accounts.Account{
				{ID: primaryID, Provider: provider, AuthMode: accounts.AuthModeAPIKey, Token: "primary-key"},
				{ID: secondaryID, Provider: provider, AuthMode: accounts.AuthModeAPIKey, Token: "secondary-key"},
			}
			server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
			sessionID := string(provider) + "-429"
			req := httptest.NewRequest(http.MethodPost, "/"+string(provider)+"/chat/completions", strings.NewReader(`{"model":"test","messages":[]}`))
			req.Header.Set("X-Subrouter-Session", sessionID)
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
			}
			if want := []string{"primary", "secondary"}; !reflect.DeepEqual(attempts, want) {
				t.Fatalf("account attempt sequence = %v, want %v", attempts, want)
			}
			assignment, ok := server.Sessions.Get(string(provider), sessionID)
			if !ok || assignment.AccountID != secondaryID {
				t.Fatalf("sticky assignment = %+v, want %s", assignment, secondaryID)
			}
			wantExhausted := provider != accounts.ProviderDeepSeek
			if got := server.SchedulerRef.Get().Exhausted(provider, primaryID); got != wantExhausted {
				t.Fatalf("primary exhausted = %v, want %v", got, wantExhausted)
			}
		})
	}
}

func TestKimiAPIKeysFailOverOnAuthoritativeQuotaAndCommitStickyAccount(t *testing.T) {
	var attempts []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.Header.Get("Authorization") {
		case "Bearer primary-key":
			attempts = append(attempts, "primary")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"message":"You've reached your 5-hour usage limit. Your quota will reset when the current 5-hour window ends."}}`)
		case "Bearer secondary-key":
			attempts = append(attempts, "secondary")
			_, _ = io.WriteString(w, `{"id":"msg_ok","content":[]}`)
		default:
			t.Error("upstream received an unexpected Kimi credential")
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer upstream.Close()

	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	server := Server{
		Accounts: []accounts.Account{
			{ID: "kimi:a-primary", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeAPIKey, Token: "primary-key"},
			{ID: "kimi:z-secondary", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeAPIKey, Token: "secondary-key"},
		},
		Sessions: store, SchedulerRef: schedulerRef,
		KimiUpstream: mustParseURL(t, upstream.URL+"/coding/v1"), MaxBodyBytes: 1024,
	}
	req := httptest.NewRequest(http.MethodPost, "/kimi/v1/messages", strings.NewReader(`{"model":"kimi-for-coding","messages":[]}`))
	req.Header.Set("X-Subrouter-Session", "kimi-api-key-failover")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if want := []string{"primary", "secondary"}; !reflect.DeepEqual(attempts, want) {
		t.Fatalf("Kimi API-key attempts = %v, want %v", attempts, want)
	}
	if !schedulerRef.Get().Exhausted(accounts.ProviderKimi, "kimi:a-primary") {
		t.Fatal("quota-limited Kimi API key was not exhausted")
	}
	assignment, ok := store.Get("kimi", "kimi-api-key-failover")
	if !ok || assignment.AccountID != "kimi:z-secondary" {
		t.Fatalf("Kimi API-key sticky assignment = %+v, want secondary key", assignment)
	}
}

func TestKimiSelectionRefreshesOAuthUsageScoresBeforePicking(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	accountsList := []accounts.Account{
		{ID: "kimi:a-low", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "low"},
		{ID: "kimi:z-high", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "high"},
	}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "kimi:a-low", Provider: accounts.ProviderKimi, Headroom: 1, ShortHeadroom: 1},
		{AccountID: "kimi:z-high", Provider: accounts.ProviderKimi, Headroom: 0.1, ShortHeadroom: 0.1},
	}))
	schedulerRef.SetUpdatedAt(time.Time{})
	scoreCalls := 0
	server := Server{
		Accounts: accountsList, Sessions: store, SchedulerRef: schedulerRef,
		UsageScoreTTL: time.Minute,
		ScoreAccounts: func(_ context.Context, got []accounts.Account) ([]selectacct.Score, int) {
			scoreCalls++
			if len(got) != 2 {
				t.Fatalf("score accounts = %+v, want both Kimi OAuth profiles", got)
			}
			return []selectacct.Score{
				{AccountID: "kimi:a-low", Provider: accounts.ProviderKimi, Headroom: 0.1, ShortHeadroom: 0.1, Fresh: true},
				{AccountID: "kimi:z-high", Provider: accounts.ProviderKimi, Headroom: 0.9, ShortHeadroom: 0.9, Fresh: true},
			}, 2
		},
	}
	request := httptest.NewRequest(http.MethodPost, "/kimi/v1/messages", strings.NewReader(`{"model":"kimi-for-coding","messages":[]}`))
	account, _, _, err := server.accountForSessionProvider(accounts.ProviderKimi, "kimi", "kimi-score-refresh", request)
	if err != nil {
		t.Fatal(err)
	}
	if scoreCalls != 1 {
		t.Fatalf("score calls = %d, want one pre-selection refresh", scoreCalls)
	}
	if account.ID != "kimi:z-high" {
		t.Fatalf("selected account = %q, want high-quota Kimi profile", account.ID)
	}
}

// Two separately purchased Token Plan keys are two schedulable accounts. A
// quota response from the account holding the sticky session must be consumed,
// replayed once with the other key, and leave the session on that healthy key.
func TestHandlerFailsOverBetweenQwenTokenPlanAccounts(t *testing.T) {
	var authorizations []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		authorizations = append(authorizations, auth)
		switch auth {
		case "Bearer token-primary":
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"code":"Throttling.AllocationQuota","message":"Allocated quota exceeded"}}`)
		case "Bearer token-secondary":
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
		default:
			t.Error("upstream received an unexpected Authorization header")
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer upstream.Close()

	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	server := Server{
		Accounts: []accounts.Account{
			{ID: "qwen-token:a-primary", Provider: accounts.ProviderQwenToken, AuthMode: accounts.AuthModeAPIKey, Token: "token-primary"},
			{ID: "qwen-token:z-secondary", Provider: accounts.ProviderQwenToken, AuthMode: accounts.AuthModeAPIKey, Token: "token-secondary"},
		},
		Sessions:          store,
		SchedulerRef:      schedulerRef,
		QwenTokenUpstream: mustParseURL(t, upstream.URL+"/compatible-mode/v1"),
		MaxBodyBytes:      1024,
	}

	req := httptest.NewRequest(http.MethodPost, "/qwen-token/v1/chat/completions", strings.NewReader(`{"model":"qwen3.8-max","messages":[]}`))
	req.Header.Set("X-Subrouter-Session", "qwen-failover")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	wantAuth := []string{"Bearer token-primary", "Bearer token-secondary"}
	if !reflect.DeepEqual(authorizations, wantAuth) {
		t.Fatal("Qwen failover used an unexpected Authorization sequence")
	}
	if !schedulerRef.Get().Exhausted(accounts.ProviderQwenToken, "qwen-token:a-primary") {
		t.Fatal("the quota-limited Qwen account should be held out of subsequent routing")
	}
	assignment, ok := store.Get("qwen-token", "qwen-failover")
	if !ok || assignment.AccountID != "qwen-token:z-secondary" {
		t.Fatalf("sticky assignment = %+v, want the successful alternate account", assignment)
	}
}

func TestQwenAnthropicFailoverSharesExhaustionWithTokenPlan(t *testing.T) {
	var attempts []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		authorization := request.Header.Get("Authorization")
		switch authorization {
		case "Bearer token-primary":
			attempts = append(attempts, "primary")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"quota exceeded"}}`)
		case "Bearer token-secondary":
			attempts = append(attempts, "secondary")
			_, _ = io.WriteString(w, `{"id":"msg_ok","content":[]}`)
		default:
			attempts = append(attempts, "unexpected")
			t.Error("upstream received an unexpected credential")
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer upstream.Close()

	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	server := Server{
		Accounts: []accounts.Account{
			{ID: "qwen-token:a-primary", Provider: accounts.ProviderQwenToken, AuthMode: accounts.AuthModeAPIKey, Token: "token-primary"},
			{ID: "qwen-token:z-secondary", Provider: accounts.ProviderQwenToken, AuthMode: accounts.AuthModeAPIKey, Token: "token-secondary"},
		},
		Sessions: store, SchedulerRef: schedulerRef,
		QwenAnthropicUpstream: mustParseURL(t, upstream.URL+"/apps/anthropic"), MaxBodyBytes: 1024,
	}
	req := httptest.NewRequest(http.MethodPost, "/qwen-anthropic/v1/messages", strings.NewReader(`{"model":"qwen3.7-plus","messages":[]}`))
	req.Header.Set("X-Subrouter-Session", "qwen-anthropic-failover")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if want := []string{"primary", "secondary"}; !reflect.DeepEqual(attempts, want) {
		t.Fatalf("account attempt sequence = %v, want %v", attempts, want)
	}
	if !schedulerRef.Get().Exhausted(accounts.ProviderQwenToken, "qwen-token:a-primary") {
		t.Fatal("Anthropic endpoint quota response did not exhaust the shared Token Plan account")
	}
	if schedulerRef.Get().Exhausted(accounts.ProviderQwenAnthropic, "qwen-token:a-primary") {
		t.Fatal("shared exhaustion was recorded under the transport endpoint instead of the account owner")
	}
	assignment, ok := store.Get("qwen-anthropic", "qwen-anthropic-failover")
	if !ok || assignment.AccountID != "qwen-token:z-secondary" {
		t.Fatalf("sticky assignment = %+v, want secondary Token Plan account", assignment)
	}

	// A later OpenAI-protocol request must observe the exhaustion recorded by
	// the Anthropic endpoint. The two endpoints spend the same purchased pool;
	// keeping separate scheduler namespaces would send this request back to the
	// already limited key.
	req = httptest.NewRequest(http.MethodPost, "/qwen-token/v1/chat/completions", strings.NewReader(`{"model":"qwen3.8-max","messages":[]}`))
	req.Header.Set("X-Subrouter-Session", "qwen-openai-after-anthropic-limit")
	rec = httptest.NewRecorder()
	server.QwenTokenUpstream = mustParseURL(t, upstream.URL+"/compatible-mode/v1")
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cross-protocol status = %d body = %s", rec.Code, rec.Body.String())
	}
	if want := []string{"primary", "secondary", "secondary"}; !reflect.DeepEqual(attempts, want) {
		t.Fatalf("cross-protocol account sequence = %v, want %v", attempts, want)
	}
}

func TestQwenProtocolsShareSchedulerLiveLoadThroughHandler(t *testing.T) {
	var attempts []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.Header.Get("Authorization") {
		case "Bearer busy-key":
			attempts = append(attempts, "busy")
		case "Bearer idle-key":
			attempts = append(attempts, "idle")
		default:
			attempts = append(attempts, "unexpected")
			t.Error("upstream received an unexpected credential")
		}
		if strings.Contains(request.URL.Path, "messages") {
			_, _ = io.WriteString(w, `{"id":"msg_ok","content":[]}`)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer upstream.Close()

	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	server := Server{
		Accounts: []accounts.Account{
			{ID: "qwen-token:a-busy", Provider: accounts.ProviderQwenToken, AuthMode: accounts.AuthModeAPIKey, Token: "busy-key"},
			{ID: "qwen-token:z-idle", Provider: accounts.ProviderQwenToken, AuthMode: accounts.AuthModeAPIKey, Token: "idle-key"},
		},
		Sessions: store,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "qwen-token:a-busy", Provider: accounts.ProviderQwenToken, Headroom: 0.41, ShortHeadroom: 0.41},
			{AccountID: "qwen-token:z-idle", Provider: accounts.ProviderQwenToken, Headroom: 0.41, ShortHeadroom: 0.41},
		})),
		QwenTokenUpstream:     mustParseURL(t, upstream.URL+"/compatible-mode/v1"),
		QwenAnthropicUpstream: mustParseURL(t, upstream.URL+"/apps/anthropic"), MaxBodyBytes: 1024,
	}

	// Route one Anthropic-protocol turn to the first key. Its live debit must be
	// recorded against the Token Plan owner namespace, reducing 41% headroom to
	// 39% and making the untouched key the deterministic next-session choice.
	req := httptest.NewRequest(http.MethodPost, "/qwen-anthropic/v1/messages", strings.NewReader(`{"model":"qwen3.7-plus","messages":[]}`))
	req.Header.Set("X-Subrouter-Session", "anthropic-live-debit")
	req.Header.Set("X-Subrouter-Account-ID", "qwen-token:a-busy")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Anthropic status = %d body = %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/qwen-token/v1/chat/completions", strings.NewReader(`{"model":"qwen3.8-max","messages":[]}`))
	req.Header.Set("X-Subrouter-Session", "new-openai-session")
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if want := []string{"busy", "idle"}; !reflect.DeepEqual(attempts, want) {
		t.Fatalf("cross-protocol live-load sequence = %v, want %v", attempts, want)
	}
	assignment, ok := store.Get("qwen-token", "new-openai-session")
	if !ok || assignment.AccountID != "qwen-token:z-idle" {
		t.Fatalf("sticky assignment = %+v, want idle Token Plan account", assignment)
	}
}

func TestFailedQwenAlternateKeepsOriginalStickyAssignment(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.Header.Get("Authorization") {
		case "Bearer token-primary":
			w.WriteHeader(http.StatusTooManyRequests)
		case "Bearer token-secondary":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			t.Error("upstream received an unexpected Qwen credential")
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer upstream.Close()

	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("qwen-token", "qwen-failed-alternate", "qwen-token:a-primary", ""); err != nil {
		t.Fatal(err)
	}
	server := Server{
		Accounts: []accounts.Account{
			{ID: "qwen-token:a-primary", Provider: accounts.ProviderQwenToken, AuthMode: accounts.AuthModeAPIKey, Token: "token-primary"},
			{ID: "qwen-token:z-secondary", Provider: accounts.ProviderQwenToken, AuthMode: accounts.AuthModeAPIKey, Token: "token-secondary"},
		},
		Sessions: store, SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		QwenTokenUpstream: mustParseURL(t, upstream.URL+"/compatible-mode/v1"), MaxBodyBytes: 1024,
	}
	req := httptest.NewRequest(http.MethodPost, "/qwen-token/v1/chat/completions", strings.NewReader(`{"model":"qwen3.8-max","messages":[]}`))
	req.Header.Set("X-Subrouter-Session", "qwen-failed-alternate")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want failed alternate response", rec.Code)
	}
	assignment, ok := store.Get("qwen-token", "qwen-failed-alternate")
	if !ok || assignment.AccountID != "qwen-token:a-primary" {
		t.Fatalf("failed alternate changed sticky assignment: %+v", assignment)
	}
}

func TestStreamingFailoverCommitsOnlyOnSuccessfulTerminalEvent(t *testing.T) {
	for _, test := range []struct {
		name        string
		stream      string
		wantCommit  bool
		breakStore  bool
		forceNewer  bool
		wantReadErr bool
	}{
		{name: "codex completed", stream: "data: {\"type\":\"response.completed\"}\n\n", wantCommit: true},
		{name: "anthropic message stop", stream: "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", wantCommit: true},
		{name: "openai done", stream: "data: [DONE]\n\n", wantCommit: true},
		{name: "event field completed", stream: "event: response.completed\ndata: {}\n\n", wantCommit: true},
		{name: "event field done", stream: "event: done\ndata: {}\n\n", wantCommit: true},
		{name: "event field error", stream: "event: error\ndata: {}\n\n"},
		{name: "codex failed", stream: "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_is_overloaded\"}}}\n\n"},
		{name: "anthropic error", stream: "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"overloaded\"}}\n\n"},
		{name: "unterminated", stream: "data: {\"type\":\"response.in_progress\"}\n\n"},
		{name: "successful event without delimiter", stream: "data: {\"type\":\"response.completed\"}\n"},
		{name: "newer assignment wins", stream: "data: {\"type\":\"response.completed\"}\n\n", forceNewer: true},
		{name: "persistence failure", stream: "data: {\"type\":\"response.done\"}\n\n", breakStore: true, wantReadErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			storePath := filepath.Join(t.TempDir(), "sessions.json")
			store, err := session.NewStore(storePath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Put("codex", "stream-session", "original", ""); err != nil {
				t.Fatal(err)
			}
			base := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream; charset=utf-8"}},
					Body:       io.NopCloser(iotest.OneByteReader(strings.NewReader(test.stream))),
					Request:    request,
				}, nil
			})
			transport := usageLimitRetryTransport{
				base: base, server: &Server{Sessions: store}, provider: accounts.ProviderCodex,
				agent: "codex", session: "stream-session", account: "alternate", expectedAccount: "original", maxAttempts: 1, commitFirstSuccess: true,
			}
			request := httptest.NewRequest(http.MethodPost, "https://codex.test/responses", strings.NewReader(`{}`))
			request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(`{}`)), nil }
			response, err := transport.RoundTrip(request)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := response.Body.(*sessionCommitSSEReadCloser); !ok {
				t.Fatalf("streaming response body = %T, want deferred session commit wrapper", response.Body)
			}
			assignment, _ := store.Get("codex", "stream-session")
			if assignment.AccountID != "original" {
				t.Fatalf("headers committed streaming alternate early: %+v", assignment)
			}
			if test.breakStore {
				if err := os.Remove(storePath); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(storePath, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if test.forceNewer {
				if _, err := store.Put("codex", "stream-session", "forced", ""); err != nil {
					t.Fatal(err)
				}
			}
			_, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if (readErr != nil) != test.wantReadErr {
				t.Fatalf("read error = %v, want error=%v", readErr, test.wantReadErr)
			}
			assignment, _ = store.Get("codex", "stream-session")
			want := "original"
			if test.wantCommit {
				want = "alternate"
			}
			if test.forceNewer {
				want = "forced"
			}
			if assignment.AccountID != want {
				t.Fatalf("sticky assignment = %q, want %q", assignment.AccountID, want)
			}
		})
	}
}

func TestNonStreamingFailoverCommitsOnlyAfterCleanBodyEOF(t *testing.T) {
	for _, test := range []struct {
		name        string
		body        func() io.ReadCloser
		breakStore  bool
		wantCommit  bool
		wantReadErr bool
	}{
		{
			name: "clean eof", wantCommit: true,
			body: func() io.ReadCloser { return io.NopCloser(iotest.OneByteReader(strings.NewReader(`{"ok":true}`))) },
		},
		{
			name: "truncated body", wantReadErr: true,
			body: func() io.ReadCloser {
				return io.NopCloser(io.MultiReader(strings.NewReader(`{"partial":`), iotest.ErrReader(errors.New("upstream reset"))))
			},
		},
		{
			name: "persistence failure", breakStore: true, wantReadErr: true,
			body: func() io.ReadCloser { return io.NopCloser(strings.NewReader(`{"ok":true}`)) },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			storePath := filepath.Join(t.TempDir(), "sessions.json")
			store, err := session.NewStore(storePath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Put("codex", "body-session", "original", ""); err != nil {
				t.Fatal(err)
			}
			base := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: test.body(), Request: request}, nil
			})
			transport := usageLimitRetryTransport{
				base: base, server: &Server{Sessions: store}, provider: accounts.ProviderCodex,
				agent: "codex", session: "body-session", account: "alternate", expectedAccount: "original", maxAttempts: 1, commitFirstSuccess: true,
			}
			request := httptest.NewRequest(http.MethodPost, "https://codex.test/responses", strings.NewReader(`{}`))
			request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(`{}`)), nil }
			response, err := transport.RoundTrip(request)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := response.Body.(*sessionCommitEOFReadCloser); !ok {
				t.Fatalf("response body = %T, want clean-EOF commit wrapper", response.Body)
			}
			assignment, _ := store.Get("codex", "body-session")
			if assignment.AccountID != "original" {
				t.Fatalf("headers committed alternate early: %+v", assignment)
			}
			if test.breakStore {
				if err := os.Remove(storePath); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(storePath, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			_, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if (readErr != nil) != test.wantReadErr {
				t.Fatalf("read error = %v, want error=%v", readErr, test.wantReadErr)
			}
			assignment, _ = store.Get("codex", "body-session")
			want := "original"
			if test.wantCommit {
				want = "alternate"
			}
			if assignment.AccountID != want {
				t.Fatalf("sticky assignment = %q, want %q", assignment.AccountID, want)
			}
		})
	}
}

func TestHandlerFailsOverBetweenKimiSubscriptionAccounts(t *testing.T) {
	var authorizations []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		authorizations = append(authorizations, auth)
		switch auth {
		case "Bearer kimi-primary":
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"message":"You've reached your 5-hour usage limit. Your quota will reset when the current 5-hour window ends. To continue now, purchase extra usage or upgrade your plan: https://www.kimi.com/membership/subscription?tab=quota"}}`)
		case "Bearer kimi-secondary":
			_, _ = io.WriteString(w, `{"id":"msg_ok","content":[]}`)
		default:
			t.Error("upstream received an unexpected Kimi credential")
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer upstream.Close()

	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("kimi", "kimi-failover", "kimi:a-primary", ""); err != nil {
		t.Fatal(err)
	}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	server := Server{
		Accounts: []accounts.Account{
			{ID: "kimi:a-primary", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "kimi-primary"},
			{ID: "kimi:z-secondary", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "kimi-secondary"},
		},
		Sessions: store, SchedulerRef: schedulerRef,
		KimiUpstream: mustParseURL(t, upstream.URL+"/coding/v1"), MaxBodyBytes: 1024,
	}
	req := httptest.NewRequest(http.MethodPost, "/kimi/v1/messages", strings.NewReader(`{"model":"kimi-for-coding","messages":[]}`))
	req.Header.Set("X-Subrouter-Agent", "kimi")
	req.Header.Set("X-Subrouter-Session", "kimi-failover")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if want := []string{"Bearer kimi-primary", "Bearer kimi-secondary"}; !reflect.DeepEqual(authorizations, want) {
		labels := make([]string, 0, len(authorizations))
		for _, authorization := range authorizations {
			switch authorization {
			case "Bearer kimi-primary":
				labels = append(labels, "primary")
			case "Bearer kimi-secondary":
				labels = append(labels, "secondary")
			default:
				labels = append(labels, "unexpected")
			}
		}
		t.Fatalf("Kimi failover sequence = %v, want [primary secondary]", labels)
	}
	if !schedulerRef.Get().Exhausted(accounts.ProviderKimi, "kimi:a-primary") {
		t.Fatal("the quota-limited Kimi account should be excluded from subsequent routing")
	}
	if counts := store.CountByAccount(); counts["kimi:z-secondary"] != 1 {
		t.Fatalf("Kimi sticky session counts = %v", counts)
	}
}

func TestKimiUsageLimitClassificationMatchesOfficialErrors(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		limited   bool
		exhausted bool
	}{
		{name: "five hour", status: 403, body: `{"error":{"message":"You've reached your 5-hour usage limit. Your quota will reset when the current 5-hour window ends. To continue now, purchase extra usage or upgrade your plan: https://www.kimi.com/membership/subscription?tab=quota"}}`, limited: true, exhausted: true},
		{name: "weekly", status: 403, body: `{"message":"You've reached your weekly (7-day) usage limit. Your quota will reset when the current 7-day window ends. To continue now, purchase extra usage or upgrade your plan: https://www.kimi.com/membership/subscription?tab=quota"}`, limited: true, exhausted: true},
		{name: "monthly", status: 403, body: `{"error":{"message":"You've reached your monthly usage limit for this billing cycle. Your quota will be refreshed in the next cycle. To continue now, purchase extra usage or upgrade your plan: https://www.kimi.com/membership/subscription?tab=quota"}}`, limited: true, exhausted: true},
		{name: "credits", status: 403, body: `{"error":"Your credit balance is insufficient. To continue, purchase extra usage or upgrade your plan: https://www.kimi.com/membership/subscription?tab=quota"}`, limited: true, exhausted: true},
		{name: "billing cycle", status: 403, body: `{"error":{"message":"You've reached your usage limit for this billing cycle. Your quota will be refreshed in the next cycle. To continue now, purchase extra usage or upgrade your plan: https://www.kimi.com/membership/subscription?tab=quota"}}`, limited: true, exhausted: true},
		{name: "concurrency", status: 403, body: `{"error":{"message":"You've reached your concurrent request limit. Please wait for your ongoing requests to finish and try again."}}`, limited: true, exhausted: false},
		{name: "unrelated forbidden", status: 403, body: `{"error":{"message":"access denied"}}`},
		{name: "membership verification", status: 402, body: `{"error":{"message":"unable to verify your membership benefits"}}`},
		{name: "overload", status: 429, body: `{"error":{"message":"The engine is currently overloaded"}}`},
	}
	transport := usageLimitRetryTransport{provider: accounts.ProviderKimi}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := &http.Response{StatusCode: test.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(test.body))}
			limited, exhausted, err := transport.responseUsageLimited(response)
			if err != nil {
				t.Fatal(err)
			}
			if limited != test.limited || exhausted != test.exhausted {
				t.Fatalf("classification = limited:%v exhausted:%v", limited, exhausted)
			}
			preserved, err := io.ReadAll(response.Body)
			if err != nil || string(preserved) != test.body {
				t.Fatal("classification did not preserve the response body")
			}
		})
	}
}

func TestKimi429RetriesSameAccountWithoutCookingIt(t *testing.T) {
	calls := 0
	base := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		status := http.StatusTooManyRequests
		body := `{"error":{"message":"The engine is currently overloaded"}}`
		if calls > providerOverloadMaxRetries {
			status = http.StatusOK
			body = `{"id":"msg_ok","content":[]}`
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	server := Server{SchedulerRef: schedulerRef}
	var waits []time.Duration
	transport := usageLimitRetryTransport{
		base: base, server: &server, provider: accounts.ProviderKimi,
		account: "kimi:primary", maxAttempts: 1,
		sleep: func(_ context.Context, wait time.Duration) error {
			waits = append(waits, wait)
			return nil
		},
	}
	request := httptest.NewRequest(http.MethodPost, "https://kimi.test/v1/messages", strings.NewReader(`{}`))
	request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(`{}`)), nil }
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || calls != 1+providerOverloadMaxRetries || len(waits) != providerOverloadMaxRetries {
		t.Fatalf("Kimi overload retry status=%d calls=%d waits=%d", response.StatusCode, calls, len(waits))
	}
	if schedulerRef.Get().Exhausted(accounts.ProviderKimi, "kimi:primary") {
		t.Fatal("Kimi overload cooked the account")
	}
}

func TestKimiConcurrencyLimitFailsOverWithoutCookingAccount(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.Header.Get("Authorization") {
		case "Bearer kimi-primary":
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"message":"You've reached your concurrent request limit. Please wait for your ongoing requests to finish and try again."}}`)
		case "Bearer kimi-secondary":
			_, _ = io.WriteString(w, `{"id":"msg_ok","content":[]}`)
		default:
			t.Error("upstream received an unexpected Kimi credential")
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer upstream.Close()

	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	server := Server{
		Accounts: []accounts.Account{
			{ID: "kimi:a-primary", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "kimi-primary"},
			{ID: "kimi:z-secondary", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "kimi-secondary"},
		},
		Sessions: store, SchedulerRef: schedulerRef,
		KimiUpstream: mustParseURL(t, upstream.URL+"/coding/v1"), MaxBodyBytes: 1024,
	}
	req := httptest.NewRequest(http.MethodPost, "/kimi/v1/messages", strings.NewReader(`{"model":"kimi-for-coding","messages":[]}`))
	req.Header.Set("X-Subrouter-Agent", "kimi")
	req.Header.Set("X-Subrouter-Session", "kimi-concurrency")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want successful alternate", rec.Code)
	}
	if schedulerRef.Get().Exhausted(accounts.ProviderKimi, "kimi:a-primary") {
		t.Fatal("Kimi concurrency limit cooked the primary account")
	}
}

func TestFailedKimiAlternateKeepsOriginalStickyAssignment(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.Header.Get("Authorization") {
		case "Bearer kimi-primary":
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"message":"You've reached your weekly (7-day) usage limit. Your quota will reset when the current 7-day window ends. To continue now, purchase extra usage or upgrade your plan: https://www.kimi.com/membership/subscription?tab=quota"}}`)
		case "Bearer kimi-secondary":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			t.Error("upstream received an unexpected Kimi credential")
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer upstream.Close()

	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("kimi", "kimi-failed-alternate", "kimi:a-primary", ""); err != nil {
		t.Fatal(err)
	}
	server := Server{
		Accounts: []accounts.Account{
			{ID: "kimi:a-primary", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "kimi-primary"},
			{ID: "kimi:z-secondary", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "kimi-secondary"},
		},
		Sessions: store, SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		KimiUpstream: mustParseURL(t, upstream.URL+"/coding/v1"), MaxBodyBytes: 1024,
	}
	req := httptest.NewRequest(http.MethodPost, "/kimi/v1/messages", strings.NewReader(`{"model":"kimi-for-coding","messages":[]}`))
	req.Header.Set("X-Subrouter-Agent", "kimi")
	req.Header.Set("X-Subrouter-Session", "kimi-failed-alternate")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want failed alternate response", rec.Code)
	}
	assignment, ok := store.Get("kimi", "kimi-failed-alternate")
	if !ok || assignment.AccountID != "kimi:a-primary" {
		t.Fatalf("failed alternate changed sticky assignment: %+v", assignment)
	}
}
