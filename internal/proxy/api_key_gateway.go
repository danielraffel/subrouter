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

// APIKeyGatewayConfig exposes a provider API through a separate team
// credential while keeping the provider credential on the server.
type APIKeyGatewayConfig struct {
	Upstream     *url.URL
	APIKey       string
	GatewayToken string
	Transport    http.RoundTripper
}

func (c *APIKeyGatewayConfig) configured() bool {
	return c != nil &&
		c.Upstream != nil &&
		strings.TrimSpace(c.APIKey) != "" &&
		strings.TrimSpace(c.GatewayToken) != ""
}

type apiKeyGatewayAuth int

const (
	apiKeyGatewayBearer apiKeyGatewayAuth = iota
	apiKeyGatewayXAPIKeyOrBearer
)

type apiKeyGatewaySpec struct {
	name         string
	prefixes     []string
	auth         apiKeyGatewayAuth
	stripHeaders []string
}

func (s Server) apiKeyGatewayHandler(config *APIKeyGatewayConfig, spec apiKeyGatewaySpec) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if config == nil || !config.configured() {
			http.Error(w, spec.name+" gateway not configured", http.StatusServiceUnavailable)
			return
		}
		if !authorizeAPIKeyGateway(r, config.GatewayToken, spec.auth) {
			http.Error(w, spec.name+" gateway token required", http.StatusUnauthorized)
			return
		}
		if s.Lifecycle != nil && s.Lifecycle.Draining() {
			http.Error(w, "subrouter is draining", http.StatusServiceUnavailable)
			return
		}
		endProxyRequest := s.Lifecycle.BeginProxyRequest()
		defer endProxyRequest()

		upstream := cloneURL(config.Upstream)
		proxyRequest := r.Clone(r.Context())
		proxyRequest.URL = cloneURL(r.URL)
		proxyRequest.URL.Path = stripGatewayPathPrefix(proxyRequest.URL.Path, spec.prefixes)
		proxyRequest.URL.RawPath = ""
		query := proxyRequest.URL.Query()
		query.Del("key")
		query.Del("api_key")
		proxyRequest.URL.RawQuery = query.Encode()
		proxyRequest.Header.Del("Authorization")
		proxyRequest.Header.Del("X-Api-Key")
		for _, header := range spec.stripHeaders {
			proxyRequest.Header.Del(header)
		}
		if spec.auth == apiKeyGatewayXAPIKeyOrBearer {
			proxyRequest.Header.Set("X-Api-Key", strings.TrimSpace(config.APIKey))
		} else {
			proxyRequest.Header.Set("Authorization", "Bearer "+strings.TrimSpace(config.APIKey))
		}
		session.StripSubrouterHeaders(proxyRequest.Header)
		stripOutboundForwardingHeaders(proxyRequest.Header)
		if s.Logger != nil {
			s.Logger.Info(spec.name+" api proxy request", "method", r.Method, "path", proxyRequest.URL.Path, "upstream", upstream.Host, "remote_addr", clientRemoteIP(r))
		}

		rp := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(upstream)
				stripOutboundForwardingHeaders(pr.Out.Header)
			},
			Transport: config.Transport,
		}
		if rp.Transport == nil {
			rp.Transport = s.transport()
		}
		if s.Logger != nil {
			rp.ErrorLog = log.New(proxyErrorWriter{
				logger:   s.Logger,
				agent:    spec.name + "-api",
				method:   r.Method,
				path:     proxyRequest.URL.Path,
				upstream: upstream.Host,
			}, "", 0)
			rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
				s.Logger.Error(spec.name+" api proxy request failed", "method", r.Method, "path", proxyRequest.URL.Path, "upstream", upstream.Host, "error", err)
				http.Error(w, spec.name+" upstream request failed", http.StatusBadGateway)
			}
		}
		rp.ServeHTTP(w, proxyRequest)
	})
}

func authorizeAPIKeyGateway(r *http.Request, configuredToken string, auth apiKeyGatewayAuth) bool {
	token := strings.TrimSpace(configuredToken)
	if token == "" {
		return false
	}
	var got string
	if auth == apiKeyGatewayXAPIKeyOrBearer {
		got = strings.TrimSpace(r.Header.Get("X-Api-Key"))
	}
	if got == "" {
		header := strings.TrimSpace(r.Header.Get("Authorization"))
		if scheme, value, ok := strings.Cut(header, " "); ok && strings.EqualFold(scheme, "Bearer") {
			got = strings.TrimSpace(value)
		}
	}
	if len(got) != len(token) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

func stripGatewayPathPrefix(path string, prefixes []string) string {
	for _, prefix := range prefixes {
		stripped := stripProviderPathPrefix(path, prefix)
		if stripped != path {
			return stripped
		}
	}
	return path
}
