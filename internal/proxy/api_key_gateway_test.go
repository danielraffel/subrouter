package proxy

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/selectacct"
	"github.com/manaflow-ai/subrouter/internal/session"
)

func TestAPIKeyGatewaysPassThroughRequestsAndStreams(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		path              string
		wantPath          string
		wantQuery         string
		gatewayToken      string
		providerKey       string
		setClientAuth     func(http.Header)
		wantAuthorization string
		wantAPIKey        string
		configure         func(*Server, *url.URL, string, string)
	}{
		{
			name:         "anthropic",
			path:         "/anthropic/v1/messages?beta=true&trace=a%2Fb&api_key=remove-me",
			wantPath:     "/v1/messages",
			wantQuery:    "beta=true&trace=a%2Fb",
			gatewayToken: "anthropic-team-token",
			providerKey:  "anthropic-provider-key",
			setClientAuth: func(headers http.Header) {
				headers.Set("X-Api-Key", "anthropic-team-token")
				headers.Set("Authorization", "Bearer remove-me")
			},
			wantAPIKey: "anthropic-provider-key",
			configure: func(server *Server, upstream *url.URL, providerKey, gatewayToken string) {
				server.AnthropicGateway = &APIKeyGatewayConfig{Upstream: upstream, APIKey: providerKey, GatewayToken: gatewayToken}
			},
		},
		{
			name:         "openai",
			path:         "/api/v1/responses?include=usage&trace=a%2Fb&key=remove-me",
			wantPath:     "/v1/responses",
			wantQuery:    "include=usage&trace=a%2Fb",
			gatewayToken: "openai-team-token",
			providerKey:  "openai-provider-key",
			setClientAuth: func(headers http.Header) {
				headers.Set("Authorization", "Bearer openai-team-token")
				headers.Set("X-Api-Key", "remove-me")
			},
			wantAuthorization: "Bearer openai-provider-key",
			configure: func(server *Server, upstream *url.URL, providerKey, gatewayToken string) {
				server.OpenAIGateway = &APIKeyGatewayConfig{Upstream: upstream, APIKey: providerKey, GatewayToken: gatewayToken}
			},
		},
		{
			name:         "anthropic bearer token",
			path:         "/anthropic/v1/messages?trace=bearer",
			wantPath:     "/v1/messages",
			wantQuery:    "trace=bearer",
			gatewayToken: "anthropic-team-token",
			providerKey:  "anthropic-provider-key",
			setClientAuth: func(headers http.Header) {
				headers.Set("Authorization", "Bearer anthropic-team-token")
			},
			wantAPIKey: "anthropic-provider-key",
			configure: func(server *Server, upstream *url.URL, providerKey, gatewayToken string) {
				server.AnthropicGateway = &APIKeyGatewayConfig{Upstream: upstream, APIKey: providerKey, GatewayToken: gatewayToken}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			type capturedRequest struct {
				path             string
				rawQuery         string
				body             string
				authorization    string
				apiKey           string
				userEmail        string
				sessionID        string
				organization     string
				project          string
				anthropicVersion string
				anthropicBeta    string
				openAIBeta       string
				idempotencyKey   string
				requestID        string
				adminToken       string
			}
			captured := make(chan capturedRequest, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				captured <- capturedRequest{
					path:             r.URL.Path,
					rawQuery:         r.URL.RawQuery,
					body:             string(body),
					authorization:    r.Header.Get("Authorization"),
					apiKey:           r.Header.Get("X-Api-Key"),
					userEmail:        r.Header.Get("X-Subrouter-User-Email"),
					sessionID:        r.Header.Get("X-Subrouter-Session"),
					organization:     r.Header.Get("OpenAI-Organization"),
					project:          r.Header.Get("OpenAI-Project"),
					anthropicVersion: r.Header.Get("Anthropic-Version"),
					anthropicBeta:    r.Header.Get("Anthropic-Beta"),
					openAIBeta:       r.Header.Get("OpenAI-Beta"),
					idempotencyKey:   r.Header.Get("Idempotency-Key"),
					requestID:        r.Header.Get("X-Request-ID"),
					adminToken:       r.Header.Get("X-Subrouter-Admin-Token"),
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("X-Request-ID", "request-123")
				_, _ = io.WriteString(w, "data: first\n\ndata: second\n\n")
			}))
			defer upstream.Close()
			upstreamURL, err := url.Parse(upstream.URL)
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			server := Server{Logger: slog.New(slog.NewTextHandler(&logs, nil))}
			test.configure(&server, upstreamURL, test.providerKey, test.gatewayToken)

			body := `{"input":"private prompt","stream":true}`
			req := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(body))
			test.setClientAuth(req.Header)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Subrouter-User-Email", "alice@example.com")
			req.Header.Set("X-Subrouter-Session", "private-session")
			req.Header.Set("OpenAI-Organization", "client-org")
			req.Header.Set("OpenAI-Project", "client-project")
			req.Header.Set("Anthropic-Version", "2023-06-01")
			req.Header.Set("Anthropic-Beta", "prompt-caching-2024-07-31")
			req.Header.Set("OpenAI-Beta", "realtime=v1")
			req.Header.Set("Idempotency-Key", "idem-123")
			req.Header.Set("X-Request-ID", "client-request-123")
			req.Header.Set("X-Subrouter-Admin-Token", "admin-secret")
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
			}
			got := <-captured
			if got.path != test.wantPath {
				t.Fatalf("upstream path = %q, want %q", got.path, test.wantPath)
			}
			if got.rawQuery != test.wantQuery {
				t.Fatalf("upstream query = %q, want %q", got.rawQuery, test.wantQuery)
			}
			if got.body != body {
				t.Fatalf("upstream body = %q, want %q", got.body, body)
			}
			if got.authorization != test.wantAuthorization || got.apiKey != test.wantAPIKey {
				t.Fatalf("upstream auth = Authorization %q, X-Api-Key %q", got.authorization, got.apiKey)
			}
			if got.userEmail != "" || got.sessionID != "" {
				t.Fatalf("internal headers leaked upstream: email=%q session=%q", got.userEmail, got.sessionID)
			}
			if got.adminToken != "" {
				t.Fatalf("Subrouter admin token leaked upstream")
			}
			if test.name == "openai" && (got.organization != "" || got.project != "") {
				t.Fatalf("OpenAI tenant headers leaked upstream: organization=%q project=%q", got.organization, got.project)
			}
			if got.anthropicVersion != "2023-06-01" || got.anthropicBeta != "prompt-caching-2024-07-31" ||
				got.openAIBeta != "realtime=v1" || got.idempotencyKey != "idem-123" || got.requestID != "client-request-123" {
				t.Fatalf("non-auth API headers were not preserved: %+v", got)
			}
			if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
				t.Fatalf("content type = %q", got)
			}
			if got := rec.Header().Get("X-Request-ID"); got != "request-123" {
				t.Fatalf("request ID = %q", got)
			}
			if got := rec.Body.String(); got != "data: first\n\ndata: second\n\n" {
				t.Fatalf("stream body = %q", got)
			}
			for _, secret := range []string{test.gatewayToken, test.providerKey, "private prompt", "private-session"} {
				if strings.Contains(logs.String(), secret) {
					t.Fatalf("logs contain secret %q: %s", secret, logs.String())
				}
			}
		})
	}
}

func TestOpenAIGatewayAlias(t *testing.T) {
	t.Parallel()

	paths := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{OpenAIGateway: &APIKeyGatewayConfig{
		Upstream: upstreamURL, APIKey: "provider-key", GatewayToken: "team-token",
	}}.Handler()
	req := httptest.NewRequest(http.MethodGet, "/openai/v1/models", nil)
	req.Header.Set("Authorization", "Bearer team-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if got := <-paths; got != "/v1/models" {
		t.Fatalf("upstream path = %q", got)
	}
}

func TestOpenAIGatewayProxiesRealtimeWebSocket(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer provider-key" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("OpenAI-Organization"); got != "" {
			t.Errorf("OpenAI-Organization leaked upstream: %q", got)
		}
		if r.URL.Path != "/v1/realtime" || r.URL.Query().Get("model") != "gpt-realtime" {
			t.Errorf("upstream URL = %s", r.URL.String())
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read message: %v", err)
			return
		}
		if err := conn.WriteMessage(messageType, append([]byte("echo:"), message...)); err != nil {
			t.Errorf("write message: %v", err)
		}
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{OpenAIGateway: &APIKeyGatewayConfig{
		Upstream: upstreamURL, APIKey: "provider-key", GatewayToken: "team-token",
	}}.Handler()
	gateway := httptest.NewServer(handler)
	defer gateway.Close()
	wsURL := "ws" + strings.TrimPrefix(gateway.URL, "http") + "/api/v1/realtime?model=gpt-realtime"
	headers := http.Header{
		"Authorization":       []string{"Bearer team-token"},
		"OpenAI-Organization": []string{"client-org"},
		"OpenAI-Beta":         []string{"realtime=v1"},
	}
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		if response != nil && response.Body != nil {
			defer response.Body.Close()
		}
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	_, message, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if got := string(message); got != "echo:hello" {
		t.Fatalf("message = %q", got)
	}
}

func TestAPIKeyGatewaysFailClosed(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		path      string
		configure func(*Server)
		auth      func(http.Header)
		want      int
	}{
		{"anthropic missing config", "/anthropic/v1/messages", func(s *Server) { s.AnthropicGateway = &APIKeyGatewayConfig{Upstream: upstreamURL, APIKey: "provider"} }, func(http.Header) {}, http.StatusServiceUnavailable},
		{"openai missing config", "/api/v1/responses", func(s *Server) { s.OpenAIGateway = &APIKeyGatewayConfig{Upstream: upstreamURL, APIKey: "provider"} }, func(http.Header) {}, http.StatusServiceUnavailable},
		{"anthropic missing auth", "/anthropic/v1/messages", func(s *Server) {
			s.AnthropicGateway = &APIKeyGatewayConfig{Upstream: upstreamURL, APIKey: "provider", GatewayToken: "team"}
		}, func(http.Header) {}, http.StatusUnauthorized},
		{"anthropic wrong auth", "/anthropic/v1/messages", func(s *Server) {
			s.AnthropicGateway = &APIKeyGatewayConfig{Upstream: upstreamURL, APIKey: "provider", GatewayToken: "team"}
		}, func(h http.Header) { h.Set("X-Api-Key", "wrong") }, http.StatusUnauthorized},
		{"openai missing auth", "/api/v1/responses", func(s *Server) {
			s.OpenAIGateway = &APIKeyGatewayConfig{Upstream: upstreamURL, APIKey: "provider", GatewayToken: "team"}
		}, func(http.Header) {}, http.StatusUnauthorized},
		{"openai wrong auth", "/api/v1/responses", func(s *Server) {
			s.OpenAIGateway = &APIKeyGatewayConfig{Upstream: upstreamURL, APIKey: "provider", GatewayToken: "team"}
		}, func(h http.Header) { h.Set("Authorization", "Bearer wrong") }, http.StatusUnauthorized},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := Server{}
			test.configure(&server)
			req := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader("{}"))
			test.auth(req.Header)
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, req)
			if rec.Code != test.want {
				t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
			}
		})
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("upstream requests = %d", got)
	}
}

func TestOpenAIGatewayRejectsAdministrativeAccess(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	adminKeyHandler := Server{OpenAIGateway: &APIKeyGatewayConfig{
		Upstream: upstreamURL, APIKey: "sk-admin-secret", GatewayToken: "team-token",
	}}.Handler()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	req.Header.Set("Authorization", "Bearer team-token")
	rec := httptest.NewRecorder()
	adminKeyHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("admin key status = %d body = %s", rec.Code, rec.Body.String())
	}

	organizationHandler := Server{OpenAIGateway: &APIKeyGatewayConfig{
		Upstream: upstreamURL, APIKey: "sk-project-secret", GatewayToken: "team-token",
	}}.Handler()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/organization/projects/example/service_accounts", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer team-token")
	rec = httptest.NewRecorder()
	organizationHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("organization route status = %d body = %s", rec.Code, rec.Body.String())
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("upstream requests = %d", got)
	}
}

func TestAnthropicGatewayRejectsPrivilegedAccess(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	for _, providerKey := range []string{"sk-ant-admin01-secret", "sk-ant-api01-secret"} {
		handler := Server{AnthropicGateway: &APIKeyGatewayConfig{
			Upstream: upstreamURL, APIKey: providerKey, GatewayToken: "team-token",
		}}.Handler()
		req := httptest.NewRequest(http.MethodGet, "/anthropic/v1/messages", nil)
		req.Header.Set("X-Api-Key", "team-token")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("key %q status = %d body = %s", providerKey, rec.Code, rec.Body.String())
		}
	}

	handler := Server{AnthropicGateway: &APIKeyGatewayConfig{
		Upstream: upstreamURL, APIKey: "sk-ant-api03-inference", GatewayToken: "team-token",
	}}.Handler()
	for _, path := range []string{"/anthropic/v1/organizations/example/members", "/anthropic/v1/compliance/activities"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Api-Key", "team-token")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s status = %d body = %s", path, rec.Code, rec.Body.String())
		}
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("upstream requests = %d", got)
	}
}

func TestGatewaysRejectEncodedDotSegments(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL + "/provider")
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		AnthropicGateway: &APIKeyGatewayConfig{Upstream: upstreamURL, APIKey: "anthropic-provider", GatewayToken: "anthropic-team"},
		OpenAIGateway:    &APIKeyGatewayConfig{Upstream: upstreamURL, APIKey: "openai-provider", GatewayToken: "openai-team"},
		Gemini:           &GeminiConfig{Upstream: upstreamURL, APIKey: "gemini-provider", GatewayToken: "gemini-team"},
	}.Handler()
	tests := []struct {
		path string
		auth func(http.Header)
	}{
		{"/api/%2e%2e/debug", func(h http.Header) { h.Set("Authorization", "Bearer openai-team") }},
		{"/anthropic/%2e%2e/debug", func(h http.Header) { h.Set("X-Api-Key", "anthropic-team") }},
		{"/gemini/%2e%2e/debug", func(h http.Header) { h.Set("X-Goog-Api-Key", "gemini-team") }},
	}
	for _, test := range tests {
		req := httptest.NewRequest(http.MethodGet, test.path, nil)
		test.auth(req.Header)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d body = %s", test.path, rec.Code, rec.Body.String())
		}
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("upstream requests = %d", got)
	}
}

func TestAPIKeyGatewaysRejectRequestsWhileDraining(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := NewLifecycle()
	lifecycle.Drain()
	handler := Server{
		Lifecycle:        lifecycle,
		AnthropicGateway: &APIKeyGatewayConfig{Upstream: upstreamURL, APIKey: "anthropic-provider", GatewayToken: "anthropic-team"},
		OpenAIGateway:    &APIKeyGatewayConfig{Upstream: upstreamURL, APIKey: "openai-provider", GatewayToken: "openai-team"},
	}.Handler()
	tests := []struct {
		path string
		auth func(http.Header)
	}{
		{"/anthropic/v1/messages", func(h http.Header) { h.Set("X-Api-Key", "anthropic-team") }},
		{"/api/v1/responses", func(h http.Header) { h.Set("Authorization", "Bearer openai-team") }},
	}
	for _, test := range tests {
		req := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader("{}"))
		test.auth(req.Header)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s status = %d body = %s", test.path, rec.Code, rec.Body.String())
		}
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("upstream requests = %d", got)
	}
	if got := lifecycle.ActiveProxyRequests(); got != 0 {
		t.Fatalf("active proxy requests = %d", got)
	}
}

func TestAPIKeyGatewayTracksActiveProxyRequests(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := NewLifecycle()
	handler := Server{
		Lifecycle:     lifecycle,
		OpenAIGateway: &APIKeyGatewayConfig{Upstream: upstreamURL, APIKey: "provider", GatewayToken: "team"},
	}.Handler()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/responses", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer team")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(rec, req)
	}()
	<-started
	if got := lifecycle.ActiveProxyRequests(); got != 1 {
		t.Fatalf("active proxy requests = %d, want 1", got)
	}
	close(release)
	<-done
	if got := lifecycle.ActiveProxyRequests(); got != 0 {
		t.Fatalf("active proxy requests after response = %d", got)
	}
}

func TestAPIKeyGatewayRoutesDoNotCaptureRootProxyPaths(t *testing.T) {
	t.Parallel()

	var rootRequests atomic.Int32
	var gatewayRequests atomic.Int32
	root := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rootRequests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer root.Close()
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		gatewayRequests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer gateway.Close()
	rootURL, err := url.Parse(root.URL)
	if err != nil {
		t.Fatal(err)
	}
	gatewayURL, err := url.Parse(gateway.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		Upstream:      rootURL,
		Accounts:      []accounts.Account{{ID: "account@example.com", AuthMode: accounts.AuthModeOAuth, Token: "root-token"}},
		Sessions:      store,
		Scheduler:     selectacct.NewScheduler(nil),
		MaxBodyBytes:  1024,
		OpenAIGateway: &APIKeyGatewayConfig{Upstream: gatewayURL, APIKey: "provider", GatewayToken: "team"},
	}.Handler()
	for index, path := range []string{"/v1/responses", "/responses"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		req.Header.Set("X-Subrouter-Session", "root-session-"+string(rune('a'+index)))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s status = %d body = %s", path, rec.Code, rec.Body.String())
		}
	}
	if got := rootRequests.Load(); got != 2 {
		t.Fatalf("root upstream requests = %d", got)
	}
	if got := gatewayRequests.Load(); got != 0 {
		t.Fatalf("gateway upstream requests = %d", got)
	}
}
