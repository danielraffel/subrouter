package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/selectacct"
	"github.com/manaflow-ai/subrouter/internal/session"
	"github.com/manaflow-ai/subrouter/internal/transcript"
)

func TestHandlerProxiesWebSocketWithSelectedAccountAuth(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			t.Fatalf("expected websocket upgrade")
		}
		if got := r.Header.Get("Authorization"); got != "Bearer selected-token" {
			t.Fatalf("Authorization = %q, want selected token", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "acct-1" {
			t.Fatalf("ChatGPT-Account-ID = %q, want acct-1", got)
		}
		if got := r.Header.Get("x-codex-window-id"); got != "window-1" {
			t.Fatalf("x-codex-window-id = %q, want window-1", got)
		}

		conn, err := upgrader.Upgrade(w, r, http.Header{"x-codex-turn-state": []string{"turn-1"}})
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()
		if err := conn.WriteMessage(websocket.TextMessage, []byte("ok")); err != nil {
			t.Fatalf("write message: %v", err)
		}
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{{
			ID:        "a@example.com",
			AuthMode:  accounts.AuthModeOAuth,
			Token:     "selected-token",
			AccountID: "acct-1",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	wsURL := "ws" + strings.TrimPrefix(subrouter.URL, "http") + "/v1/responses"
	header := http.Header{"x-codex-window-id": []string{"window-1"}}
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer response.Body.Close()
	defer conn.Close()
	if got := response.Header.Get("x-codex-turn-state"); got != "turn-1" {
		t.Fatalf("x-codex-turn-state = %q, want turn-1", got)
	}

	_, body, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("message = %q, want ok", string(body))
	}
}

func TestHandlerMapsV1RequestsToCodexBackendPaths(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/backend-api/codex/responses" {
			t.Fatalf("path = %q, want /backend-api/codex/responses", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer oauth-token" {
			t.Fatalf("Authorization = %q, want oauth token", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	codexUpstream, err := url.Parse(upstream.URL + "/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		CodexUpstream: codexUpstream,
		Accounts: []accounts.Account{{
			ID:       "codex-account",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "oauth-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	response, err := http.Post(subrouter.URL+"/v1/responses", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}
}

func TestHandlerDoesNotProxyUnknownInternalSubrouterPaths(t *testing.T) {
	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	codexUpstream, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		CodexUpstream: codexUpstream,
		Accounts: []accounts.Account{{
			ID:       "codex-account",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "oauth-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/_subrouter/newer-cli-endpoint", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	if upstreamCalled {
		t.Fatal("unknown internal path was proxied upstream")
	}
}

func TestHandlerRefreshesExpiredOAuthBeforeProxying(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir()}
	stale := proxyStoredOAuthAccount("codex@example.com", "old", time.Now().Add(-time.Hour))
	fresh := proxyStoredOAuthAccount("codex@example.com", "new", time.Now().Add(time.Hour))
	if err := store.SaveStored(stale); err != nil {
		t.Fatal(err)
	}
	staleAccount, ok := stale.Account(stale.SourcePath(store))
	if !ok {
		t.Fatal("stale account did not convert")
	}
	freshAccount, ok := fresh.Account(fresh.SourcePath(store))
	if !ok {
		t.Fatal("fresh account did not convert")
	}

	refreshClient := &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost {
			t.Fatalf("refresh method = %s", req.Method)
		}
		body, _ := json.Marshal(map[string]string{
			"access_token":  fresh.Auth.Tokens.AccessToken,
			"refresh_token": fresh.Auth.Tokens.RefreshToken,
			"id_token":      fresh.Auth.Tokens.IDToken,
		})
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(body)),
		}, nil
	})}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != freshAccount.AuthorizationHeader() {
			t.Fatalf("Authorization = %q, want refreshed token", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	codexUpstream, err := url.Parse(upstream.URL + "/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	sessionStore, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}

	handler := Server{
		CodexUpstream: codexUpstream,
		Accounts:      []accounts.Account{staleAccount},
		AccountRef:    NewAccountRef(store, []accounts.Account{staleAccount}, refreshClient),
		Sessions:      sessionStore,
		Scheduler:     selectacct.NewScheduler(nil),
		MaxBodyBytes:  1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	response, err := http.Post(subrouter.URL+"/v1/responses", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}

	stored, ok, err := store.FindStored("codex@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || stored.Auth.Tokens.RefreshToken != fresh.Auth.Tokens.RefreshToken {
		t.Fatalf("stored refresh token was not updated")
	}
}

func TestAccountStatusEndpointValidatesRefreshToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir()}
	stale := proxyStoredOAuthAccount("codex@example.com", "old", time.Now().Add(-time.Hour))
	fresh := proxyStoredOAuthAccount("codex@example.com", "new", time.Now().Add(time.Hour))
	if err := store.SaveStored(stale); err != nil {
		t.Fatal(err)
	}
	staleAccount, ok := stale.Account(stale.SourcePath(store))
	if !ok {
		t.Fatal("stale account did not convert")
	}
	refreshClient := &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, _ := json.Marshal(map[string]string{
			"access_token":  fresh.Auth.Tokens.AccessToken,
			"refresh_token": fresh.Auth.Tokens.RefreshToken,
			"id_token":      fresh.Auth.Tokens.IDToken,
		})
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(body)),
		}, nil
	})}
	sessionStore, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		Accounts:     []accounts.Account{staleAccount},
		AccountRef:   NewAccountRef(store, []accounts.Account{staleAccount}, refreshClient),
		Sessions:     sessionStore,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/_subrouter/account-status", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var statuses []AccountStatus
	if err := json.Unmarshal(recorder.Body.Bytes(), &statuses); err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || !statuses[0].AuthValid || !statuses[0].Refreshed {
		t.Fatalf("unexpected statuses: %+v", statuses)
	}
}

func TestReloadAccountsHotLoadsNewAccountWithoutRestart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	accountStore := accounts.CodexStore{Dir: t.TempDir()}
	initial := proxyStoredOAuthAccount("old@example.com", "old", time.Now().Add(time.Hour))
	added := proxyStoredOAuthAccount("new@example.com", "new", time.Now().Add(time.Hour))
	if err := accountStore.SaveStored(initial); err != nil {
		t.Fatal(err)
	}
	initialAccount, ok := initial.Account(initial.SourcePath(accountStore))
	if !ok {
		t.Fatal("initial account did not convert")
	}
	upstreamAuthorizations := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAuthorizations <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	codexUpstream, err := url.Parse(upstream.URL + "/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	sessionStore, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	accountRef := NewAccountRef(accountStore, []accounts.Account{initialAccount}, nil)
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "old@example.com", Headroom: 0.90, ShortHeadroom: 0.90},
	}))
	handler := Server{
		CodexUpstream: codexUpstream,
		AccountRef:    accountRef,
		Sessions:      sessionStore,
		SchedulerRef:  schedulerRef,
		MaxBodyBytes:  1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	if err := accountStore.SaveStored(added); err != nil {
		t.Fatal(err)
	}
	reloadReq, err := http.NewRequest(http.MethodPost, subrouter.URL+"/_subrouter/reload-accounts", nil)
	if err != nil {
		t.Fatal(err)
	}
	reloadResp, err := http.DefaultClient.Do(reloadReq)
	if err != nil {
		t.Fatal(err)
	}
	if reloadResp.StatusCode != http.StatusOK {
		defer reloadResp.Body.Close()
		body, _ := io.ReadAll(reloadResp.Body)
		t.Fatalf("reload status = %d, body = %s", reloadResp.StatusCode, string(body))
	}
	var payload struct {
		OK       bool `json:"ok"`
		Accounts int  `json:"accounts"`
	}
	if err := json.NewDecoder(reloadResp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if err := reloadResp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if !payload.OK || payload.Accounts != 2 {
		t.Fatalf("reload payload = %+v, want 2 accounts", payload)
	}

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Session", "new-session")
	req.Header.Set("X-Subrouter-Account-ID", "new@example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if got := <-upstreamAuthorizations; got != "Bearer "+added.Auth.Tokens.AccessToken {
		t.Fatalf("Authorization = %q, want new account token", got)
	}
}

func TestHandlerMapsV1WebSocketRequestsToCodexBackendPaths(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			t.Fatalf("expected websocket upgrade")
		}
		if got := r.URL.Path; got != "/backend-api/codex/responses" {
			t.Fatalf("path = %q, want /backend-api/codex/responses", got)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()
		if err := conn.WriteMessage(websocket.TextMessage, []byte("ok")); err != nil {
			t.Fatalf("write message: %v", err)
		}
	}))
	defer upstream.Close()

	codexUpstream, err := url.Parse(upstream.URL + "/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		CodexUpstream: codexUpstream,
		Accounts: []accounts.Account{{
			ID:       "codex-account",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "oauth-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	wsURL := "ws" + strings.TrimPrefix(subrouter.URL, "http") + "/v1/responses"
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer response.Body.Close()
	defer conn.Close()
	_, body, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("message = %q, want ok", string(body))
	}
}

type proxyRoundTripFunc func(*http.Request) (*http.Response, error)

func (f proxyRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func proxyStoredOAuthAccount(email, tokenPrefix string, exp time.Time) accounts.StoredCodexAccount {
	return accounts.StoredCodexAccount{
		Email:   email,
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth: accounts.CodexAuthFile{AuthMode: "chatgpt", Tokens: &accounts.CodexTokens{
			AccessToken:  proxyTestCodexJWT(email, tokenPrefix+"-access", exp),
			RefreshToken: tokenPrefix + "-refresh",
			IDToken:      proxyTestCodexJWT(email, tokenPrefix+"-id", exp),
		}},
	}
}

func proxyTestCodexJWT(email, jwtID string, exp time.Time) string {
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	payload, _ := json.Marshal(map[string]any{
		"exp": exp.Unix(),
		"iat": time.Now().Add(-time.Minute).Unix(),
		"jti": jwtID,
		"https://api.openai.com/profile": map[string]any{
			"email": email,
		},
	})
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestHandlerMapsBareRequestsToOpenAIV1Paths(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/responses" {
			t.Fatalf("path = %q, want /v1/responses", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer api-token" {
			t.Fatalf("Authorization = %q, want API token", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	apiUpstream, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		APIUpstream: apiUpstream,
		Accounts: []accounts.Account{{
			ID:       "api-account",
			AuthMode: accounts.AuthModeAPIKey,
			Token:    "api-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	response, err := http.Post(subrouter.URL+"/responses", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}
}

func TestHandlerUsesExplicitSubrouterAccountID(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/responses" {
			t.Fatalf("path = %q, want /v1/responses", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer api-token" {
			t.Fatalf("Authorization = %q, want API token", got)
		}
		if got := r.Header.Get("X-Subrouter-Account-ID"); got != "" {
			t.Fatalf("X-Subrouter-Account-ID leaked upstream: %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	apiUpstream, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		APIUpstream: apiUpstream,
		Accounts: []accounts.Account{
			{ID: "oauth@example.com", AuthMode: accounts.AuthModeOAuth, Token: "oauth-token"},
			{ID: "apikey:team-codex-1", AuthMode: accounts.AuthModeAPIKey, Token: "api-token"},
		},
		Sessions: store,
		Scheduler: selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "oauth@example.com", Headroom: 1, ShortHeadroom: 1},
			{AccountID: "apikey:team-codex-1", Headroom: 0.01, ShortHeadroom: 0.01},
		}),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Session", "session-1")
	req.Header.Set("X-Subrouter-Account-ID", "team-codex-1")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}

	assignment, ok := store.Get("codex", "session-1")
	if !ok {
		t.Fatal("missing assignment")
	}
	if assignment.AccountID != "apikey:team-codex-1" {
		t.Fatalf("AccountID = %q, want apikey:team-codex-1", assignment.AccountID)
	}
}

func TestHandlerPreservesRequestBodyBytesAfterJSONSessionExtraction(t *testing.T) {
	body := []byte("{\n  \"input\": \"keep bytes: \\u2603\",\n  \"metadata\": {\"session_id\": \"json-session\"},\n  \"array\": [1, true, null]\n}")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, body) {
			t.Fatalf("body changed:\n got: %q\nwant: %q", got, body)
		}
		if r.ContentLength != int64(len(body)) {
			t.Fatalf("ContentLength = %d, want %d", r.ContentLength, len(body))
		}
		if got := r.Header.Get("X-Client-Trace-ID"); got != "trace-preserved" {
			t.Fatalf("X-Client-Trace-ID = %q, want trace-preserved", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{{
			ID:       "a@example.com",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "a-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Client-Trace-ID", "trace-preserved")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}

	assignment, ok := store.Get("codex", "json-session")
	if !ok {
		t.Fatal("missing json-session assignment")
	}
	if assignment.AccountID != "a@example.com" {
		t.Fatalf("AccountID = %q, want a@example.com", assignment.AccountID)
	}
}

func TestHandlerPreservesResponseBodyBytes(t *testing.T) {
	body := []byte("event: response.output_text.delta\ndata: {\"delta\":\"hello\"}\n\nevent: response.completed\ndata: {}\n\n")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusAccepted)
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{{
			ID:       "a@example.com",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "a-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Session", "response-session")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	got, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.StatusCode)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("body changed:\n got: %q\nwant: %q", got, body)
	}
}

func TestHandlerPreservesWebSocketMessageBytes(t *testing.T) {
	payload := []byte{0x00, 0x01, 0x02, 0xfe, 0xff, 'c', 'm', 'u', 'x'}
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()
		messageType, got, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read message: %v", err)
		}
		if messageType != websocket.BinaryMessage {
			t.Fatalf("message type = %d, want binary", messageType)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("websocket payload changed:\n got: %x\nwant: %x", got, payload)
		}
		if got := r.Header.Get("X-Codex-Window-ID"); got != "ws-window" {
			t.Fatalf("X-Codex-Window-ID = %q, want ws-window", got)
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, got); err != nil {
			t.Fatalf("write message: %v", err)
		}
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{{
			ID:       "a@example.com",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "a-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	wsURL := "ws" + strings.TrimPrefix(subrouter.URL, "http") + "/v1/responses"
	header := http.Header{"X-Codex-Window-ID": []string{"ws-window"}}
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer response.Body.Close()
	defer conn.Close()
	if err := conn.WriteMessage(websocket.BinaryMessage, payload); err != nil {
		t.Fatalf("write message: %v", err)
	}
	messageType, got, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	if messageType != websocket.BinaryMessage {
		t.Fatalf("message type = %d, want binary", messageType)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("websocket echo changed:\n got: %x\nwant: %x", got, payload)
	}
}

func TestHandlerRecordsHTTPTranscriptBodies(t *testing.T) {
	requestBody := []byte(`{"session_id":"codex-session:0","input":"hello"}`)
	responseBody := []byte("event: done\ndata: {}\n\n")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := w.Write(responseBody); err != nil {
			t.Fatal(err)
		}
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	transcripts := transcript.NewRecorder(filepath.Join(t.TempDir(), "transcripts"))
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{{
			ID:       "a@example.com",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "a-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
		Transcripts:  transcripts,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-token")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}

	events := readTranscriptEventsEventually(t, transcripts.PathForSession("codex", "codex-session:0"), 3)
	assertTranscriptPayload(t, events, "http_body", "client_to_upstream", requestBody)
	assertTranscriptPayload(t, events, "http_body", "upstream_to_client", responseBody)
	if got := events[0]["payload"].(map[string]any)["agent_type"]; got != "codex" {
		t.Fatalf("agent_type = %v, want codex", got)
	}
	if got := events[0]["payload"].(map[string]any)["agent_session_id"]; got != "codex-session" {
		t.Fatalf("agent_session_id = %v, want codex-session", got)
	}
	if got := events[0]["payload"].(map[string]any)["codex_session_id"]; got != "codex-session" {
		t.Fatalf("codex_session_id = %v, want codex-session", got)
	}
	headers := events[0]["payload"].(map[string]any)["headers"].(map[string]any)
	if got := headers["Authorization"].([]any)[0]; got != "<redacted>" {
		t.Fatalf("Authorization header = %v, want redacted", got)
	}
}

func TestHandlerRecordsWebSocketTranscriptMessages(t *testing.T) {
	clientPayload := []byte(`{"encrypted_content":"client-ciphertext","prompt_cache_key":"cache-key"}`)
	upstreamPayload := []byte(`{"encrypted_content":"upstream-ciphertext"}`)
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()
		messageType, body, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read message: %v", err)
		}
		if messageType != websocket.TextMessage {
			t.Fatalf("message type = %d, want text", messageType)
		}
		if !bytes.Equal(body, clientPayload) {
			t.Fatalf("client payload = %q, want %q", body, clientPayload)
		}
		if err := conn.WriteMessage(websocket.TextMessage, upstreamPayload); err != nil {
			t.Fatalf("write message: %v", err)
		}
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	transcripts := transcript.NewRecorder(filepath.Join(t.TempDir(), "transcripts"))
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{{
			ID:       "a@example.com",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "a-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
		Transcripts:  transcripts,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	wsURL := "ws" + strings.TrimPrefix(subrouter.URL, "http") + "/v1/responses"
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"X-Codex-Window-ID": []string{"codex-ws:0"}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer response.Body.Close()
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, clientPayload); err != nil {
		t.Fatalf("write message: %v", err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read message: %v", err)
	}

	events := readTranscriptEventsEventually(t, transcripts.PathForSession("codex", "codex-ws:0"), 3)
	assertTranscriptPayload(t, events, "websocket_message", "client_to_upstream", clientPayload)
	assertTranscriptPayload(t, events, "websocket_message", "upstream_to_client", upstreamPayload)
}

func TestHandlerStoresUserEmailAndStripsSubrouterHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Subrouter-Session"); got != "" {
			t.Fatalf("X-Subrouter-Session = %q, want empty", got)
		}
		if got := r.Header.Get("X-Subrouter-Agent"); got != "" {
			t.Fatalf("X-Subrouter-Agent = %q, want empty", got)
		}
		if got := r.Header.Get("X-Subrouter-User-Email"); got != "" {
			t.Fatalf("X-Subrouter-User-Email = %q, want empty", got)
		}
		if got := r.Header.Get("X-Subrouter-User"); got != "" {
			t.Fatalf("X-Subrouter-User = %q, want empty", got)
		}
		if got := r.Header.Get("X-User-Email"); got != "" {
			t.Fatalf("X-User-Email = %q, want empty", got)
		}
		if got := r.Header.Get("X-Trace-ID"); got != "trace-1" {
			t.Fatalf("X-Trace-ID = %q, want trace-1", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{{
			ID:       "a@example.com",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "a-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Session", "session-1")
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-User-Email", "Alice <Alice@Example.COM>")
	req.Header.Set("X-Subrouter-User", "bob@example.com")
	req.Header.Set("X-User-Email", "carol@example.com")
	req.Header.Set("X-Trace-ID", "trace-1")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}

	assignments := store.All()
	if len(assignments) != 1 {
		t.Fatalf("len(assignments) = %d, want 1", len(assignments))
	}
	assignment := assignments[0]
	if assignment.AgentType != "claude" {
		t.Fatalf("AgentType = %q, want claude", assignment.AgentType)
	}
	if assignment.SessionID != "session-1" {
		t.Fatalf("SessionID = %q, want session-1", assignment.SessionID)
	}
	if assignment.AccountID != "a@example.com" {
		t.Fatalf("AccountID = %q, want a@example.com", assignment.AccountID)
	}
	if assignment.UserEmail != "alice@example.com" {
		t.Fatalf("UserEmail = %q, want alice@example.com", assignment.UserEmail)
	}
}

func TestHandlerReroutesStickySessionWhenAssignedAccountExhausted(t *testing.T) {
	var auths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "session-1", "empty@example.com", ""); err != nil {
		t.Fatal(err)
	}
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "empty@example.com", AuthMode: accounts.AuthModeOAuth, Token: "empty-token"},
			{ID: "healthy@example.com", AuthMode: accounts.AuthModeOAuth, Token: "healthy-token"},
		},
		Sessions: store,
		Scheduler: selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "empty@example.com", Headroom: 0, ShortHeadroom: 0},
			{AccountID: "healthy@example.com", Headroom: 0.90, ShortHeadroom: 0.90},
		}),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Session", "session-1")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}

	if strings.Join(auths, "\x00") != "Bearer healthy-token" {
		t.Fatalf("auths = %#v, want healthy account", auths)
	}
	assignment, ok := store.Get("codex", "session-1")
	if !ok {
		t.Fatal("missing session-1 assignment")
	}
	if assignment.AccountID != "healthy@example.com" {
		t.Fatalf("AccountID = %q, want healthy@example.com", assignment.AccountID)
	}
}

func TestHandlerDoesNotAssignNewSessionToExhaustedOAuthAccount(t *testing.T) {
	var auths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "short-empty@example.com", AuthMode: accounts.AuthModeOAuth, Token: "short-empty-token"},
			{ID: "weekly-empty@example.com", AuthMode: accounts.AuthModeOAuth, Token: "weekly-empty-token"},
		},
		Sessions: store,
		Scheduler: selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "short-empty@example.com", Headroom: 0, ShortHeadroom: 0},
			{AccountID: "weekly-empty@example.com", Headroom: 0, ShortHeadroom: 1},
		}),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Session", "session-1")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.StatusCode)
	}
	if len(auths) != 0 {
		t.Fatalf("upstream auths = %#v, want no upstream request", auths)
	}
	if _, ok := store.Get("codex", "session-1"); ok {
		t.Fatal("exhausted account should not be assigned to new session")
	}
}

func TestHandlerScopesStickySessionsByAgentType(t *testing.T) {
	var auths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "a@example.com", AuthMode: accounts.AuthModeOAuth, Token: "a-token"},
			{ID: "b@example.com", AuthMode: accounts.AuthModeOAuth, Token: "b-token"},
		},
		Sessions: store,
		Scheduler: selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "a@example.com", Headroom: 0.80, ShortHeadroom: 0.80},
			{AccountID: "b@example.com", Headroom: 0.80, ShortHeadroom: 0.80},
		}),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	for _, agentType := range []string{"codex", "claude"} {
		req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Subrouter-Session", "same-session")
		req.Header.Set("X-Subrouter-Agent", agentType)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{"Bearer a-token", "Bearer b-token"}
	if strings.Join(auths, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("auths = %#v, want %#v", auths, want)
	}
	if _, ok := store.Get("codex", "same-session"); !ok {
		t.Fatal("missing codex assignment")
	}
	if _, ok := store.Get("claude", "same-session"); !ok {
		t.Fatal("missing claude assignment")
	}
}

func readTranscriptEvents(t *testing.T, path string) []map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	events := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

func readTranscriptEventsEventually(t *testing.T, path string, wantAtLeast int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var lastEvents []map[string]any
	for {
		body, err := os.ReadFile(path)
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(body)), "\n")
			events := make([]map[string]any, 0, len(lines))
			parseOK := true
			for _, line := range lines {
				if strings.TrimSpace(line) == "" {
					continue
				}
				var event map[string]any
				if err := json.Unmarshal([]byte(line), &event); err != nil {
					parseOK = false
					break
				}
				events = append(events, event)
			}
			if parseOK {
				lastEvents = events
				if len(events) >= wantAtLeast {
					return events
				}
			}
		}
		if time.Now().After(deadline) {
			if len(lastEvents) > 0 {
				return lastEvents
			}
			t.Fatalf("transcript %s did not reach %d events", path, wantAtLeast)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertTranscriptPayload(t *testing.T, events []map[string]any, eventType, direction string, want []byte) {
	t.Helper()
	for _, event := range events {
		if event["type"] != eventType {
			continue
		}
		payload := event["payload"].(map[string]any)
		if payload["direction"] != direction {
			continue
		}
		encoded := payload["body_base64"].(string)
		got, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s %s payload = %q, want %q", eventType, direction, got, want)
		}
		return
	}
	t.Fatalf("missing %s %s transcript event", eventType, direction)
}

func TestHandlerBalancesEquivalentNewSessionsByStoredCounts(t *testing.T) {
	var auths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "a@example.com", AuthMode: accounts.AuthModeOAuth, Token: "a-token"},
			{ID: "b@example.com", AuthMode: accounts.AuthModeOAuth, Token: "b-token"},
		},
		Sessions: store,
		Scheduler: selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "a@example.com", Headroom: 0.80, ShortHeadroom: 0.80},
			{AccountID: "b@example.com", Headroom: 0.80, ShortHeadroom: 0.80},
		}),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	for _, sessionID := range []string{"session-1", "session-2"} {
		req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Subrouter-Session", sessionID)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{"Bearer a-token", "Bearer b-token"}
	if strings.Join(auths, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("auths = %#v, want %#v", auths, want)
	}
}
