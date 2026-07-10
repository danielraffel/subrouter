package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
)

func TestGeminiGatewayReplacesClientCredentialAndPreservesAPIPaths(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if got := r.Header.Get("X-Goog-Api-Key"); got != "provider-secret" {
			t.Fatalf("X-Goog-Api-Key = %q, want provider credential", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization leaked upstream: %q", got)
		}
		if r.URL.Query().Get("key") != "" {
			t.Fatalf("query key leaked upstream")
		}
		if got := r.Header.Get("X-Subrouter-User-Email"); got != "" {
			t.Fatalf("X-Subrouter-User-Email leaked upstream: %q", got)
		}
		if got := r.Header.Get("X-Subrouter-Session"); got != "" {
			t.Fatalf("X-Subrouter-Session leaked upstream: %q", got)
		}
		w.Header().Set("X-Goog-Upload-Url", "http://"+r.Host+"/upload/v1beta/files?upload_id=abc")
		_, _ = io.WriteString(w, r.URL.Path)
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{Gemini: &GeminiConfig{
		Upstream:     upstreamURL,
		APIKey:       "provider-secret",
		GatewayToken: "team-token",
	}}.Handler()

	for _, path := range []string{
		"/gemini/upload/v1beta/files",
		"/gemini/v1beta/interactions",
		"/gemini/v1beta/files/example",
		"/upload/v1beta/files",
		"/v1beta/interactions",
	} {
		req := httptest.NewRequest(http.MethodPost, path+"?key=client-secret", strings.NewReader("{}"))
		req.Header.Set("X-Goog-Api-Key", "team-token")
		req.Header.Set("Authorization", "Bearer client-secret")
		req.Header.Set("X-Subrouter-User-Email", "alice@example.com")
		req.Header.Set("X-Subrouter-Session", "session-secret")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d body = %s", path, rec.Code, rec.Body.String())
		}
		if got := strings.TrimSpace(rec.Body.String()); got != strings.TrimPrefix(path, "/gemini") {
			t.Fatalf("%s upstream path = %q", path, got)
		}
		if got := rec.Header().Get("X-Goog-Upload-Url"); got != "http://example.com/upload/v1beta/files?upload_id=abc" {
			t.Fatalf("upload URL = %q", got)
		}
	}
	if got := requests.Load(); got != 5 {
		t.Fatalf("upstream requests = %d", got)
	}
}

func TestGeminiGatewayRejectsWrongGatewayToken(t *testing.T) {
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
	handler := Server{Gemini: &GeminiConfig{
		Upstream:     upstreamURL,
		APIKey:       "provider-secret",
		GatewayToken: "team-token",
	}}.Handler()

	req := httptest.NewRequest(http.MethodPost, "/gemini/v1beta/interactions", strings.NewReader("{}"))
	req.Header.Set("X-Goog-Api-Key", "wrong-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("upstream requests = %d", got)
	}
}

func TestGeminiGatewayReportsMissingProviderCredential(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodPost, "/gemini/v1beta/interactions", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	Server{}.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestGeminiGatewayReportsMissingGatewayToken(t *testing.T) {
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
	handler := Server{Gemini: &GeminiConfig{
		Upstream: upstreamURL,
		APIKey:   "provider-secret",
	}}.Handler()

	req := httptest.NewRequest(http.MethodPost, "/gemini/v1beta/interactions", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("upstream requests = %d", got)
	}
}

func TestGeminiGatewayRejectsRequestsWhileDraining(t *testing.T) {
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
		Lifecycle: lifecycle,
		Gemini: &GeminiConfig{
			Upstream:     upstreamURL,
			APIKey:       "provider-secret",
			GatewayToken: "team-token",
		},
	}.Handler()

	req := httptest.NewRequest(http.MethodPost, "/gemini/v1beta/interactions", strings.NewReader("{}"))
	req.Header.Set("X-Goog-Api-Key", "team-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("upstream requests = %d", got)
	}
	if got := lifecycle.ActiveProxyRequests(); got != 0 {
		t.Fatalf("active proxy requests = %d", got)
	}
}

func TestGeminiGatewayTracksActiveProxyRequests(t *testing.T) {
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
		Lifecycle: lifecycle,
		Gemini: &GeminiConfig{
			Upstream:     upstreamURL,
			APIKey:       "provider-secret",
			GatewayToken: "team-token",
		},
	}.Handler()

	req := httptest.NewRequest(http.MethodPost, "/gemini/v1beta/interactions", strings.NewReader("{}"))
	req.Header.Set("X-Goog-Api-Key", "team-token")
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
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestRewriteGeminiUploadURLUsesForwardedHTTPS(t *testing.T) {
	t.Parallel()

	upstream, err := url.Parse("https://generativelanguage.googleapis.com")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/gemini/upload/v1beta/files", nil)
	request.Host = "gateway.example.com"
	request.Header.Set("X-Forwarded-Proto", "https, http")
	headers := http.Header{
		"X-Goog-Upload-Url": []string{"https://generativelanguage.googleapis.com/upload/v1beta/files?upload_id=abc"},
	}
	rewriteGeminiUploadURL(headers, upstream, request)
	if got := headers.Get("X-Goog-Upload-Url"); got != "https://gateway.example.com/upload/v1beta/files?upload_id=abc" {
		t.Fatalf("upload URL = %q", got)
	}
}

func TestGeminiGatewayProxiesLiveWebSocket(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("key") != "" {
			t.Errorf("query key leaked upstream")
		}
		if got := r.Header.Get("X-Goog-Api-Key"); got != "provider-secret" {
			t.Errorf("X-Goog-Api-Key = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization leaked upstream: %q", got)
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
	handler := Server{Gemini: &GeminiConfig{
		Upstream: upstreamURL, APIKey: "provider-secret", GatewayToken: "team-token",
	}}.Handler()
	gateway := httptest.NewServer(handler)
	defer gateway.Close()
	wsURL := "ws" + strings.TrimPrefix(gateway.URL, "http") +
		"/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent?key=client-secret"
	headers := http.Header{
		"X-Goog-Api-Key": []string{"team-token"},
		"Authorization":  []string{"Bearer client-secret"},
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
