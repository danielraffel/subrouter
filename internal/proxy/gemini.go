package proxy

import (
	"crypto/subtle"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/session"
)

// GeminiConfig enables a transparent Gemini Developer API gateway. Clients
// present the gateway token as x-goog-api-key; the gateway replaces it
// with the provider key before forwarding the request.
type GeminiConfig struct {
	Upstream     *url.URL
	APIKey       string
	GatewayToken string
	Transport    http.RoundTripper
}

func (c *GeminiConfig) configured() bool {
	return c != nil &&
		c.Upstream != nil &&
		strings.TrimSpace(c.APIKey) != "" &&
		strings.TrimSpace(c.GatewayToken) != ""
}

func (s Server) geminiHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Gemini == nil || !s.Gemini.configured() {
			http.Error(w, "gemini gateway not configured", http.StatusServiceUnavailable)
			return
		}
		if !authorizeGeminiGateway(r, s.Gemini.GatewayToken) {
			http.Error(w, "gemini gateway token required", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodHead && (r.URL.Path == "/gemini" || r.URL.Path == "/gemini/") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if s.Lifecycle != nil && s.Lifecycle.Draining() {
			http.Error(w, "subrouter is draining", http.StatusServiceUnavailable)
			return
		}
		endProxyRequest := s.Lifecycle.BeginProxyRequest()
		defer endProxyRequest()

		upstream := cloneURL(s.Gemini.Upstream)
		proxyRequest := r.Clone(r.Context())
		proxyRequest.URL = cloneURL(r.URL)
		proxyRequest.URL.Path = stripProviderPathPrefix(proxyRequest.URL.Path, "gemini")
		proxyRequest.URL.RawPath = ""
		query := proxyRequest.URL.Query()
		query.Del("key")
		proxyRequest.URL.RawQuery = query.Encode()
		proxyRequest.Header.Del("Authorization")
		proxyRequest.Header.Set("X-Goog-Api-Key", strings.TrimSpace(s.Gemini.APIKey))
		session.StripSubrouterHeaders(proxyRequest.Header)
		stripOutboundForwardingHeaders(proxyRequest.Header)
		if s.Logger != nil {
			s.Logger.Info("gemini proxy request", "method", r.Method, "path", proxyRequest.URL.Path, "upstream", upstream.Host, "remote_addr", clientRemoteIP(r))
		}

		rp := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(upstream)
				stripOutboundForwardingHeaders(pr.Out.Header)
			},
			Transport: s.Gemini.Transport,
		}
		rp.ModifyResponse = func(response *http.Response) error {
			rewriteGeminiUploadURL(response.Header, upstream, r)
			return nil
		}
		if rp.Transport == nil {
			rp.Transport = s.transport()
		}
		if s.Logger != nil {
			rp.ErrorLog = log.New(proxyErrorWriter{
				logger:   s.Logger,
				agent:    "gemini",
				method:   r.Method,
				path:     proxyRequest.URL.Path,
				upstream: upstream.Host,
			}, "", 0)
			rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
				s.Logger.Error("gemini proxy request failed", "method", r.Method, "path", proxyRequest.URL.Path, "upstream", upstream.Host, "error", err)
				http.Error(w, "gemini upstream request failed", http.StatusBadGateway)
			}
		}
		rp.ServeHTTP(w, proxyRequest)
	})
}

func rewriteGeminiUploadURL(headers http.Header, upstream *url.URL, request *http.Request) {
	raw := strings.TrimSpace(headers.Get("X-Goog-Upload-Url"))
	if raw == "" || upstream == nil || request == nil || request.Host == "" {
		return
	}
	uploadURL, err := url.Parse(raw)
	if err != nil || !uploadURL.IsAbs() || !strings.EqualFold(uploadURL.Host, upstream.Host) {
		return
	}
	scheme := "http"
	if request.TLS != nil {
		scheme = "https"
	} else {
		forwardedProto, _, _ := strings.Cut(request.Header.Get("X-Forwarded-Proto"), ",")
		if strings.EqualFold(strings.TrimSpace(forwardedProto), "https") {
			scheme = "https"
		}
	}
	uploadURL.Scheme = scheme
	uploadURL.Host = request.Host
	headers.Set("X-Goog-Upload-Url", uploadURL.String())
}

func authorizeGeminiGateway(r *http.Request, configuredToken string) bool {
	token := strings.TrimSpace(configuredToken)
	if token == "" {
		return false
	}
	got := strings.TrimSpace(r.Header.Get("X-Goog-Api-Key"))
	if got == "" {
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		if scheme, value, ok := strings.Cut(auth, " "); ok && strings.EqualFold(scheme, "Bearer") {
			got = strings.TrimSpace(value)
		}
	}
	if len(got) != len(token) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}
