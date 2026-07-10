package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
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
		w.Header().Set("X-Goog-Upload-Url", "https://upload.example/session")
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
		if got := rec.Header().Get("X-Goog-Upload-Url"); got != "https://upload.example/session" {
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
