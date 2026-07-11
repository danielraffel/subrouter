package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/selectacct"
	"github.com/manaflow-ai/subrouter/internal/session"
)

func TestSessionLeaseRequiresConfiguredAdminTokenForNetworkCaller(t *testing.T) {
	handler := Server{AdminToken: "expected-admin-token"}.Handler()
	for _, test := range []struct {
		name  string
		token string
	}{
		{name: "missing"},
		{name: "wrong", token: "wrong-admin-token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := newSessionLeaseRequest(t, "codex", "gpt-5.4")
			req.RemoteAddr = "100.64.0.2:12345"
			if test.token != "" {
				req.Header.Set("Authorization", "Bearer "+test.token)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}

	// A network listener with no configured token must stay closed rather than
	// inheriting the permissive legacy admin behavior.
	handler = Server{}.Handler()
	req := newSessionLeaseRequest(t, "codex", "gpt-5.4")
	req.RemoteAddr = "100.64.0.2:12345"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status without configured token = %d, want 401", recorder.Code)
	}
}

func TestCodexOAuthSessionLeaseIsIdempotentAndBrokersWithoutCredentialDisclosure(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("upstream path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer oauth-access-secret" {
			t.Fatalf("Authorization = %q, want selected OAuth access token", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "chatgpt-account-1" {
			t.Fatalf("ChatGPT-Account-ID = %q", got)
		}
		if got := r.Header.Get("X-Subrouter-Lease"); got != "" {
			t.Fatalf("lease header reached upstream: %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	codexUpstream := mustParseURL(t, upstream.URL+"/backend-api/codex")
	store := newSessionStore(t)
	leaseStore := newSessionLeaseStore()
	handler := Server{
		CodexUpstream: codexUpstream,
		Accounts: []accounts.Account{{
			ID:        "oauth@example.com",
			Provider:  accounts.ProviderCodex,
			AuthMode:  accounts.AuthModeOAuth,
			Token:     "oauth-access-secret",
			AccountID: "chatgpt-account-1",
		}},
		Sessions:      store,
		Scheduler:     selectacct.NewScheduler(nil),
		sessionLeases: leaseStore,
		AdminToken:    "service-admin-token",
		MaxBodyBytes:  1024,
	}.Handler()

	first, firstBody := issueSessionLease(t, handler, "codex", "openai/gpt-5.4")
	if strings.Contains(firstBody, "oauth-access-secret") {
		t.Fatal("lease response disclosed the underlying OAuth access token")
	}
	if first.Assignment.AuthMode != string(accounts.AuthModeOAuth) {
		t.Fatalf("auth mode = %q", first.Assignment.AuthMode)
	}
	if first.Assignment.Model != "gpt-5.4" {
		t.Fatalf("model = %q", first.Assignment.Model)
	}
	if first.Pi.API != "openai-codex-responses" || first.Pi.BaseURL != "http://subrouter:31415/backend-api" {
		t.Fatalf("unexpected Pi config: %+v", first.Pi)
	}
	if first.Environment["OPENAI_BASE_URL"] != "http://subrouter:31415/v1" {
		t.Fatalf("OpenAI compatibility base URL = %q", first.Environment["OPENAI_BASE_URL"])
	}
	leaseToken := first.Environment["CLOUDMUX_SUBROUTER_LEASE_TOKEN"]
	header, payload, tokenParts := decodeSessionLeaseToken(t, leaseToken)
	if header.Type != sessionLeaseTokenType {
		t.Fatalf("lease token typ = %q", header.Type)
	}
	if !payload.CloudmuxSessionLease {
		t.Fatal("lease token is missing its public Cloudmux marker")
	}
	if payload.OpenAIAuthentication.ChatGPTAccountID != syntheticChatGPTAccountID {
		t.Fatalf("synthetic account ID = %q", payload.OpenAIAuthentication.ChatGPTAccountID)
	}
	if payload.OpenAIAuthentication.ChatGPTAccountID == "chatgpt-account-1" || strings.Contains(leaseToken, "chatgpt-account-1") {
		t.Fatal("lease token disclosed the upstream ChatGPT account ID")
	}
	if payload.Nonce == "" || tokenParts[2] == "" {
		t.Fatal("lease token requires a random nonce and signature segment")
	}
	if !looksLikeSessionLeaseToken(leaseToken) {
		t.Fatal("server did not recognize its JWT-shaped lease token")
	}
	if first.Environment["OPENAI_API_KEY"] != leaseToken {
		t.Fatal("OPENAI_API_KEY must contain the ephemeral broker token")
	}
	forgedSignature, err := randomLeaseValue("", 32)
	if err != nil {
		t.Fatal(err)
	}
	forged := tokenParts[0] + "." + tokenParts[1] + "." + forgedSignature
	if !looksLikeSessionLeaseToken(forged) {
		t.Fatal("public marker should identify the forged token shape")
	}
	if _, err := leaseStore.resolve(forged); !errors.Is(err, errInvalidSessionLease) {
		t.Fatalf("forged lease resolved: %v", err)
	}

	second, _ := issueSessionLease(t, handler, "codex", "openai/gpt-5.4")
	if second.LeaseID != first.LeaseID || second.Environment["CLOUDMUX_SUBROUTER_LEASE_TOKEN"] != leaseToken {
		t.Fatalf("retry minted a different lease: first=%s second=%s", first.LeaseID, second.LeaseID)
	}

	adapterResolvedURL := strings.TrimRight(first.Pi.BaseURL, "/") + "/codex/responses"
	if adapterResolvedURL != "http://subrouter:31415/backend-api/codex/responses" {
		t.Fatalf("Pi adapter URL = %q", adapterResolvedURL)
	}
	proxyReq := httptest.NewRequest(http.MethodPost, adapterResolvedURL, strings.NewReader(`{"model":"gpt-5.4"}`))
	proxyReq.Header.Set("Authorization", "Bearer "+leaseToken)
	proxyReq.Header.Set("Content-Type", "application/json")
	proxyRecorder := httptest.NewRecorder()
	handler.ServeHTTP(proxyRecorder, proxyReq)
	if proxyRecorder.Code != http.StatusNoContent {
		t.Fatalf("proxy status = %d, body = %s", proxyRecorder.Code, proxyRecorder.Body.String())
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", upstreamCalls.Load())
	}
	wrongEndpointReq := httptest.NewRequest(http.MethodPost, "http://subrouter:31415/backend-api/accounts/check", nil)
	wrongEndpointReq.Header.Set("Authorization", "Bearer "+leaseToken)
	wrongEndpoint := httptest.NewRecorder()
	handler.ServeHTTP(wrongEndpoint, wrongEndpointReq)
	if wrongEndpoint.Code != http.StatusForbidden {
		t.Fatalf("wrong endpoint status = %d, want 403", wrongEndpoint.Code)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatal("lease reached an endpoint outside its model API")
	}

	releaseSessionLease(t, handler, first.LeaseID)
	rejectedReq := httptest.NewRequest(http.MethodPost, adapterResolvedURL, strings.NewReader(`{"model":"gpt-5.4"}`))
	rejectedReq.Header.Set("Authorization", "Bearer "+leaseToken)
	rejectedReq.Header.Set("Content-Type", "application/json")
	rejected := httptest.NewRecorder()
	handler.ServeHTTP(rejected, rejectedReq)
	if rejected.Code != http.StatusUnauthorized {
		t.Fatalf("released lease status = %d, want 401", rejected.Code)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("released lease reached upstream")
	}
}

func TestClaudeAPIKeySessionLeaseUsesEphemeralTokenAtSandboxBoundary(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Api-Key"); got != "sk-ant-underlying-secret" {
			t.Fatalf("X-Api-Key = %q, want selected API key", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization = %q, want empty for Anthropic API-key auth", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	handler := Server{
		ClaudeUpstream: mustParseURL(t, upstream.URL),
		Accounts: []accounts.Account{{
			ID:       "claude:team-key",
			Provider: accounts.ProviderClaude,
			AuthMode: accounts.AuthModeAPIKey,
			Token:    "sk-ant-underlying-secret",
		}},
		Sessions:      newSessionStore(t),
		Scheduler:     selectacct.NewScheduler(nil),
		sessionLeases: newSessionLeaseStore(),
		AdminToken:    "service-admin-token",
		MaxBodyBytes:  1024,
	}.Handler()

	lease, body := issueSessionLease(t, handler, "claude", "anthropic/claude-sonnet-4-5")
	if strings.Contains(body, "sk-ant-underlying-secret") {
		t.Fatal("lease response disclosed the underlying API key")
	}
	leaseToken := lease.Environment["CLOUDMUX_SUBROUTER_LEASE_TOKEN"]
	if lease.Environment["ANTHROPIC_API_KEY"] != leaseToken || lease.Environment["ANTHROPIC_AUTH_TOKEN"] != leaseToken {
		t.Fatal("Anthropic environment must use the ephemeral broker token")
	}
	if lease.Pi.API != "anthropic-messages" || lease.Pi.BaseURL != "http://subrouter:31415" {
		t.Fatalf("unexpected Pi config: %+v", lease.Pi)
	}

	proxyReq := httptest.NewRequest(http.MethodPost, "http://subrouter:31415/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5","max_tokens":8,"messages":[]}`))
	proxyReq.Header.Set("X-Api-Key", leaseToken)
	proxyReq.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, proxyReq)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("proxy status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestSessionLeaseExpiryRejectsBrokerToken(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	store := newSessionLeaseStore()
	store.now = func() time.Time { return now }
	store.ttl = time.Minute
	lease, err := store.put(sessionLease{ScopeKey: "scope", SessionKey: "session", Provider: accounts.ProviderCodex})
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.put(sessionLease{ScopeKey: "other-scope", SessionKey: "other-session", Provider: accounts.ProviderCodex})
	if err != nil {
		t.Fatal(err)
	}
	_, leasePayload, leaseParts := decodeSessionLeaseToken(t, lease.Token)
	_, otherPayload, otherParts := decodeSessionLeaseToken(t, other.Token)
	if leasePayload.Nonce == otherPayload.Nonce || leaseParts[2] == otherParts[2] {
		t.Fatal("independent leases reused a nonce or signature segment")
	}
	now = now.Add(time.Minute)
	if _, err := store.resolve(lease.Token); !errors.Is(err, errInvalidSessionLease) {
		t.Fatalf("resolve after expiry error = %v", err)
	}
}

func TestSessionLeaseProviderRejectsConflictingModelPrefix(t *testing.T) {
	if _, _, err := sessionLeaseProvider("codex", "anthropic/claude-sonnet-4-5"); err == nil {
		t.Fatal("expected conflicting provider and model prefix to fail")
	}
}

func TestPresentedSessionLeaseTokenIgnoresOrdinaryJWT(t *testing.T) {
	header := base64.RawStdEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload := base64.RawStdEncoding.EncodeToString([]byte(`{"cloudmux_session_lease":true}`))
	ordinaryJWT := header + "." + payload + ".signature"
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer "+ordinaryJWT)
	if _, presented := presentedSessionLeaseToken(req); presented {
		t.Fatal("ordinary provider JWT was mistaken for a session lease")
	}
}

func issueSessionLease(t *testing.T, handler http.Handler, provider, model string) (sessionLeaseResponse, string) {
	t.Helper()
	req := newSessionLeaseRequest(t, provider, model)
	req.RemoteAddr = "100.64.0.2:12345"
	req.Header.Set("Authorization", "Bearer service-admin-token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("lease status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response sessionLeaseResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response, recorder.Body.String()
}

func newSessionLeaseRequest(t *testing.T, provider, model string) *http.Request {
	t.Helper()
	body, err := json.Marshal(sessionLeaseRequest{
		OrganizationID: "organization-1",
		WorkspaceID:    "workspace-1",
		ConversationID: "conversation-1",
		InvocationID:   "invocation-1",
		AgentSessionID: "agent-session-1",
		Agent:          "pi",
		Provider:       provider,
		Model:          model,
		ProxyBaseURL:   "http://subrouter:31415",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://subrouter:31415/internal/v1/session-leases", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func releaseSessionLease(t *testing.T, handler http.Handler, leaseID string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "http://subrouter:31415/internal/v1/session-leases/"+url.PathEscape(leaseID), nil)
	req.RemoteAddr = "100.64.0.2:12345"
	req.Header.Set("Authorization", "Bearer service-admin-token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNoContent {
		body, _ := io.ReadAll(recorder.Result().Body)
		t.Fatalf("release status = %d, body = %s", recorder.Code, string(body))
	}
}

func newSessionStore(t *testing.T) *session.Store {
	t.Helper()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func mustParseURL(t *testing.T, value string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func decodeSessionLeaseToken(t *testing.T, token string) (sessionLeaseTokenHeader, sessionLeaseTokenPayload, []string) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("lease token has %d segments, want 3", len(parts))
	}
	headerBody, err := base64.RawStdEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("Pi-compatible header decode: %v", err)
	}
	payloadBody, err := base64.RawStdEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("Pi-compatible payload decode: %v", err)
	}
	var header sessionLeaseTokenHeader
	if err := json.Unmarshal(headerBody, &header); err != nil {
		t.Fatal(err)
	}
	var payload sessionLeaseTokenPayload
	if err := json.Unmarshal(payloadBody, &payload); err != nil {
		t.Fatal(err)
	}
	return header, payload, parts
}
