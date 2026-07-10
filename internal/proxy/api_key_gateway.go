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
	if c == nil || c.Upstream == nil {
		return false
	}
	apiKey := strings.TrimSpace(c.APIKey)
	gatewayToken := strings.TrimSpace(c.GatewayToken)
	return apiKey != "" && gatewayToken != "" && apiKey != gatewayToken
}

type apiKeyGatewayAuth int

const (
	apiKeyGatewayBearer apiKeyGatewayAuth = iota
	apiKeyGatewayXAPIKeyOrBearer
)

type apiKeyGatewaySpec struct {
	name                  string
	prefixes              []string
	auth                  apiKeyGatewayAuth
	stripHeaders          []string
	blockedPathPrefixes   []string
	blockedAPIKeyPrefixes []string
}

func (s Server) apiKeyGatewayHandler(config *APIKeyGatewayConfig, spec apiKeyGatewaySpec) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if config == nil || !config.configured() {
			http.Error(w, spec.name+" gateway not configured", http.StatusServiceUnavailable)
			return
		}
		for _, prefix := range spec.blockedAPIKeyPrefixes {
			if strings.HasPrefix(strings.TrimSpace(config.APIKey), prefix) {
				http.Error(w, spec.name+" gateway requires a non-admin provider key", http.StatusServiceUnavailable)
				return
			}
		}
		if !authorizeAPIKeyGateway(r, config.GatewayToken, spec.auth) {
			http.Error(w, spec.name+" gateway token required", http.StatusUnauthorized)
			return
		}
		if gatewayMethodCanReflectCredentials(r.Method) {
			http.Error(w, "gateway method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if gatewayPathIsUnsafe(r.URL) {
			http.Error(w, "invalid gateway path", http.StatusBadRequest)
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
		for _, prefix := range spec.blockedPathPrefixes {
			if proxyRequest.URL.Path == prefix || strings.HasPrefix(proxyRequest.URL.Path, prefix+"/") {
				http.Error(w, spec.name+" administrative route not allowed", http.StatusForbidden)
				return
			}
		}
		query := proxyRequest.URL.Query()
		query.Del("key")
		query.Del("api_key")
		proxyRequest.URL.RawQuery = query.Encode()
		proxyRequest.Header.Del("Authorization")
		proxyRequest.Header.Del("X-Api-Key")
		if spec.auth == apiKeyGatewayBearer {
			stripOpenAIWebSocketCredential(proxyRequest.Header)
		}
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

func gatewayMethodCanReflectCredentials(method string) bool {
	return strings.EqualFold(method, "TRACE") || strings.EqualFold(method, "TRACK")
}

func gatewayPathIsUnsafe(requestURL *url.URL) bool {
	if requestURL == nil {
		return true
	}
	for _, candidate := range []string{requestURL.Path, requestURL.RawPath} {
		if candidate == "" {
			continue
		}
		for range 8 {
			if !strings.HasPrefix(candidate, "/") || strings.Contains(candidate, "//") || strings.Contains(candidate, `\`) {
				return true
			}
			for _, segment := range strings.Split(candidate, "/") {
				if segment == "." || segment == ".." {
					return true
				}
			}
			decoded, err := url.PathUnescape(candidate)
			if err != nil {
				return true
			}
			if decoded == candidate {
				candidate = ""
				break
			}
			candidate = decoded
		}
		if candidate != "" {
			return true
		}
	}
	return false
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
	if got == "" && auth == apiKeyGatewayBearer {
		got = openAIWebSocketCredential(r.Header)
	}
	if len(got) != len(token) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

const openAIWebSocketCredentialPrefix = "openai-insecure-api-key."

var openAIWebSocketTenantPrefixes = []string{"openai-organization.", "openai-project."}

func openAIWebSocketCredential(headers http.Header) string {
	for _, value := range headers.Values("Sec-WebSocket-Protocol") {
		for _, protocol := range strings.Split(value, ",") {
			protocol = strings.TrimSpace(protocol)
			if strings.HasPrefix(protocol, openAIWebSocketCredentialPrefix) {
				return strings.TrimPrefix(protocol, openAIWebSocketCredentialPrefix)
			}
		}
	}
	return ""
}

func stripOpenAIWebSocketCredential(headers http.Header) {
	var kept []string
	for _, value := range headers.Values("Sec-WebSocket-Protocol") {
		for _, protocol := range strings.Split(value, ",") {
			protocol = strings.TrimSpace(protocol)
			lowerProtocol := strings.ToLower(protocol)
			isTenantSelector := false
			for _, prefix := range openAIWebSocketTenantPrefixes {
				if strings.HasPrefix(lowerProtocol, prefix) {
					isTenantSelector = true
					break
				}
			}
			if protocol != "" && !strings.HasPrefix(lowerProtocol, openAIWebSocketCredentialPrefix) && !isTenantSelector {
				kept = append(kept, protocol)
			}
		}
	}
	if len(kept) == 0 {
		headers.Del("Sec-WebSocket-Protocol")
		return
	}
	headers.Set("Sec-WebSocket-Protocol", strings.Join(kept, ", "))
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
