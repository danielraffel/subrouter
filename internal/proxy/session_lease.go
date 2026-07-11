package proxy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

const (
	defaultSessionLeaseTTL      = 15 * time.Minute
	maxSessionLeaseRequestBytes = 64 << 10
	sessionLeaseTokenType       = "SRLEASE"
	syntheticChatGPTAccountID   = "cloudmux-broker"
)

var errInvalidSessionLease = errors.New("invalid or expired session lease")

// sessionLeaseStore keeps short-lived broker credentials in memory. The
// underlying provider credentials remain in Subrouter's account store and are
// never returned to the caller.
type sessionLeaseStore struct {
	mu      sync.Mutex
	byID    map[string]sessionLease
	byScope map[string]string
	byToken map[[32]byte]string
	now     func() time.Time
	ttl     time.Duration
}

// sessionLease is the server-side binding for one Cloudmux invocation. Token
// is only returned once through the authenticated lease response and is never
// logged.
type sessionLease struct {
	ID             string
	Token          string
	ScopeKey       string
	OrganizationID string
	WorkspaceID    string
	ConversationID string
	InvocationID   string
	SessionKey     string
	Agent          string
	Provider       accounts.Provider
	AccountID      string
	AuthMode       accounts.AuthMode
	Model          string
	CreatedAt      time.Time
	ExpiresAt      time.Time
}

type sessionLeaseRequest struct {
	OrganizationID string `json:"organizationId"`
	WorkspaceID    string `json:"workspaceId"`
	ConversationID string `json:"conversationId"`
	InvocationID   string `json:"invocationId"`
	AgentSessionID string `json:"agentSessionId"`
	Agent          string `json:"agent"`
	Provider       string `json:"provider,omitempty"`
	Model          string `json:"model,omitempty"`
	ProxyBaseURL   string `json:"proxyBaseUrl,omitempty"`
}

type sessionLeaseResponse struct {
	LeaseID     string                 `json:"leaseId"`
	SessionKey  string                 `json:"sessionKey"`
	ExpiresAt   string                 `json:"expiresAt"`
	Environment map[string]string      `json:"environment"`
	Assignment  sessionLeaseAssignment `json:"assignment"`
	Pi          sessionLeasePiConfig   `json:"pi"`
}

type sessionLeaseAssignment struct {
	AccountID string `json:"accountId"`
	Provider  string `json:"provider"`
	AuthMode  string `json:"authMode"`
	Model     string `json:"model,omitempty"`
	Reason    string `json:"reason"`
}

// sessionLeasePiConfig is enough for the caller to create an isolated Pi
// models.json provider without embedding a provider credential in that file.
// apiKeyEnvironmentVariable resolves to the ephemeral broker token.
type sessionLeasePiConfig struct {
	Provider                  string `json:"provider"`
	API                       string `json:"api"`
	BaseURL                   string `json:"baseUrl"`
	APIKeyEnvironmentVariable string `json:"apiKeyEnvironmentVariable"`
	Model                     string `json:"model,omitempty"`
}

type sessionLeaseTokenHeader struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
}

type sessionLeaseTokenPayload struct {
	Issuer               string                      `json:"iss"`
	Audience             string                      `json:"aud"`
	IssuedAt             int64                       `json:"iat"`
	ExpiresAt            int64                       `json:"exp"`
	Nonce                string                      `json:"jti"`
	CloudmuxSessionLease bool                        `json:"cloudmux_session_lease"`
	OpenAIAuthentication sessionLeaseOpenAIAuthClaim `json:"https://api.openai.com/auth"`
}

type sessionLeaseOpenAIAuthClaim struct {
	ChatGPTAccountID string `json:"chatgpt_account_id"`
}

func newSessionLeaseStore() *sessionLeaseStore {
	return &sessionLeaseStore{
		byID:    make(map[string]sessionLease),
		byScope: make(map[string]string),
		byToken: make(map[[32]byte]string),
		now:     time.Now,
		ttl:     defaultSessionLeaseTTL,
	}
}

func (s *sessionLeaseStore) put(template sessionLease) (sessionLease, error) {
	if s == nil {
		return sessionLease{}, errors.New("session lease store is not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	s.removeExpiredLocked(now)
	if existingID := s.byScope[template.ScopeKey]; existingID != "" {
		if existing, ok := s.byID[existingID]; ok {
			return existing, nil
		}
	}
	id, err := randomLeaseValue("lease_", 18)
	if err != nil {
		return sessionLease{}, err
	}
	expiresAt := now.Add(s.ttl)
	token, err := newSessionLeaseToken(now, expiresAt)
	if err != nil {
		return sessionLease{}, err
	}
	template.ID = id
	template.Token = token
	template.CreatedAt = now
	template.ExpiresAt = expiresAt
	s.byID[id] = template
	s.byScope[template.ScopeKey] = id
	s.byToken[sha256.Sum256([]byte(token))] = id
	return template, nil
}

func (s *sessionLeaseStore) resolve(token string) (sessionLease, error) {
	if s == nil || token == "" {
		return sessionLease{}, errInvalidSessionLease
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	s.removeExpiredLocked(now)
	id := s.byToken[sha256.Sum256([]byte(token))]
	lease, ok := s.byID[id]
	if !ok {
		return sessionLease{}, errInvalidSessionLease
	}
	return lease, nil
}

func (s *sessionLeaseStore) release(id string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeExpiredLocked(s.now().UTC())
	lease, ok := s.byID[id]
	if !ok {
		return false
	}
	s.removeLocked(lease)
	return true
}

func (s *sessionLeaseStore) removeExpiredLocked(now time.Time) {
	for _, lease := range s.byID {
		if !now.Before(lease.ExpiresAt) {
			s.removeLocked(lease)
		}
	}
}

func (s *sessionLeaseStore) removeLocked(lease sessionLease) {
	delete(s.byID, lease.ID)
	delete(s.byToken, sha256.Sum256([]byte(lease.Token)))
	if s.byScope[lease.ScopeKey] == lease.ID {
		delete(s.byScope, lease.ScopeKey)
	}
}

func randomLeaseValue(prefix string, size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate session lease: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(value), nil
}

// newSessionLeaseToken returns a JWT-shaped opaque capability. Pi's
// openai-codex-responses adapter decodes the ChatGPT account claim before it
// sends a request, so the public payload contains a constant synthetic account
// ID. Authorization still relies only on exact server-side token-hash lookup.
// The random signature is an opaque capability segment, not a self-verifying
// signature.
func newSessionLeaseToken(issuedAt, expiresAt time.Time) (string, error) {
	nonce, err := randomLeaseValue("", 18)
	if err != nil {
		return "", err
	}
	signature, err := randomLeaseValue("", 32)
	if err != nil {
		return "", err
	}
	header, err := json.Marshal(sessionLeaseTokenHeader{
		Algorithm: "opaque",
		Type:      sessionLeaseTokenType,
	})
	if err != nil {
		return "", fmt.Errorf("encode session lease header: %w", err)
	}
	payload, err := json.Marshal(sessionLeaseTokenPayload{
		Issuer:               "subrouter",
		Audience:             "cloudmux-pi",
		IssuedAt:             issuedAt.Unix(),
		ExpiresAt:            expiresAt.Unix(),
		Nonce:                nonce,
		CloudmuxSessionLease: true,
		OpenAIAuthentication: sessionLeaseOpenAIAuthClaim{
			ChatGPTAccountID: syntheticChatGPTAccountID,
		},
	})
	if err != nil {
		return "", fmt.Errorf("encode session lease payload: %w", err)
	}
	// Pi currently decodes the payload with atob rather than a base64url
	// decoder. Standard unpadded base64 remains JWT-shaped and works with both
	// atob and tolerant server/actor decoders. The signature is never decoded by
	// Pi and can use the URL-safe alphabet.
	return strings.Join([]string{
		base64.RawStdEncoding.EncodeToString(header),
		base64.RawStdEncoding.EncodeToString(payload),
		signature,
	}, "."), nil
}

func (s Server) requireSessionLeaseAdmin(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Loopback remains usable for local self-hosting. Every network caller
		// must present a configured admin token, even when other legacy admin
		// endpoints are running in permissive mode.
		if isLoopbackRemote(r.RemoteAddr) || (strings.TrimSpace(s.AdminToken) != "" && s.authorizeAdmin(r)) {
			next(w, r)
			return
		}
		http.Error(w, "admin token required", http.StatusUnauthorized)
	}
}

func (s Server) handleSessionLeases(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSessionLeaseRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request sessionLeaseRequest
	if err := decoder.Decode(&request); err != nil {
		http.Error(w, "invalid session lease request", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		http.Error(w, "invalid session lease request", http.StatusBadRequest)
		return
	}
	request.normalize()
	if err := validateSessionLeaseRequest(request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	provider, model, err := sessionLeaseProvider(request.Provider, request.Model)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	proxyBaseURL, err := sessionLeaseProxyBaseURL(r, request.ProxyBaseURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	r.Header.Set("X-Subrouter-Agent", request.Agent)
	r.Header.Set("X-Subrouter-Session", request.AgentSessionID)
	if model != "" {
		r.Header.Set("X-Subrouter-Model", model)
	}
	account, sessionKey, _, err := s.accountForSessionProvider(
		provider,
		agentTypeForProviderSession(request.Agent, provider),
		request.AgentSessionID,
		r,
	)
	if err != nil {
		http.Error(w, "no account is available for the requested lease", http.StatusServiceUnavailable)
		return
	}
	lease, err := s.sessionLeases.put(sessionLease{
		ScopeKey:       sessionLeaseScopeKey(request, provider, model),
		OrganizationID: request.OrganizationID,
		WorkspaceID:    request.WorkspaceID,
		ConversationID: request.ConversationID,
		InvocationID:   request.InvocationID,
		SessionKey:     sessionKey,
		Agent:          request.Agent,
		Provider:       provider,
		AccountID:      account.ID,
		AuthMode:       account.AuthMode,
		Model:          model,
	})
	if err != nil {
		http.Error(w, "create session lease", http.StatusInternalServerError)
		return
	}
	writeJSON(w, sessionLeaseResponseFor(lease, proxyBaseURL))
}

func (s Server) handleSessionLease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/internal/v1/session-leases/"))
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	// Deletion is idempotent so actor cleanup can retry after a timeout.
	s.sessionLeases.release(id)
	w.WriteHeader(http.StatusNoContent)
}

func (s Server) resolveSessionLease(r *http.Request) (sessionLease, bool, error) {
	token, presented := presentedSessionLeaseToken(r)
	if !presented {
		return sessionLease{}, false, nil
	}
	lease, err := s.sessionLeases.resolve(token)
	if err != nil {
		return sessionLease{}, true, err
	}
	return lease, true, nil
}

func (lease sessionLease) allowsRequest(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	switch lease.Provider {
	case accounts.ProviderCodex:
		return codexResponsePath(r.URL.Path)
	case accounts.ProviderClaude:
		return r.URL.Path == "/v1/messages"
	case accounts.ProviderKimi:
		return r.URL.Path == "/kimi/v1/messages"
	case accounts.ProviderZAI:
		return r.URL.Path == "/zai/v1/messages"
	default:
		return false
	}
}

func presentedSessionLeaseToken(r *http.Request) (string, bool) {
	if explicit := strings.TrimSpace(r.Header.Get("X-Subrouter-Lease")); explicit != "" {
		return explicit, true
	}
	for _, value := range []string{
		bearerToken(r.Header.Get("Authorization")),
		strings.TrimSpace(r.Header.Get("X-Api-Key")),
	} {
		if looksLikeSessionLeaseToken(value) {
			return value, true
		}
	}
	return "", false
}

func looksLikeSessionLeaseToken(value string) bool {
	if value == "" || len(value) > 4096 {
		return false
	}
	parts := strings.Split(value, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return false
	}
	headerBody, err := decodeSessionLeaseTokenSegment(parts[0])
	if err != nil {
		return false
	}
	var header sessionLeaseTokenHeader
	if err := json.Unmarshal(headerBody, &header); err != nil || header.Type != sessionLeaseTokenType {
		return false
	}
	payloadBody, err := decodeSessionLeaseTokenSegment(parts[1])
	if err != nil {
		return false
	}
	var payload sessionLeaseTokenPayload
	return json.Unmarshal(payloadBody, &payload) == nil && payload.CloudmuxSessionLease
}

func decodeSessionLeaseTokenSegment(value string) ([]byte, error) {
	for _, encoding := range []*base64.Encoding{
		base64.RawStdEncoding,
		base64.StdEncoding,
		base64.RawURLEncoding,
		base64.URLEncoding,
	} {
		decoded, err := encoding.DecodeString(value)
		if err == nil {
			return decoded, nil
		}
	}
	return nil, errors.New("invalid session lease token encoding")
}

func bearerToken(value string) string {
	before, after, ok := strings.Cut(strings.TrimSpace(value), " ")
	if !ok || !strings.EqualFold(before, "Bearer") {
		return ""
	}
	return strings.TrimSpace(after)
}

func validateSessionLeaseRequest(request sessionLeaseRequest) error {
	fields := []struct {
		name  string
		value string
	}{
		{"organizationId", request.OrganizationID},
		{"workspaceId", request.WorkspaceID},
		{"conversationId", request.ConversationID},
		{"invocationId", request.InvocationID},
		{"agentSessionId", request.AgentSessionID},
	}
	for _, field := range fields {
		value := strings.TrimSpace(field.value)
		if value == "" || len(value) > 256 {
			return fmt.Errorf("%s is required and must be at most 256 bytes", field.name)
		}
	}
	if request.Agent != "pi" {
		return errors.New("agent must be pi")
	}
	if len(request.Model) > 256 {
		return errors.New("model must be at most 256 bytes")
	}
	return nil
}

func (request *sessionLeaseRequest) normalize() {
	request.OrganizationID = strings.TrimSpace(request.OrganizationID)
	request.WorkspaceID = strings.TrimSpace(request.WorkspaceID)
	request.ConversationID = strings.TrimSpace(request.ConversationID)
	request.InvocationID = strings.TrimSpace(request.InvocationID)
	request.AgentSessionID = strings.TrimSpace(request.AgentSessionID)
	request.Agent = strings.ToLower(strings.TrimSpace(request.Agent))
	request.Provider = strings.ToLower(strings.TrimSpace(request.Provider))
	request.Model = strings.TrimSpace(request.Model)
	request.ProxyBaseURL = strings.TrimSpace(request.ProxyBaseURL)
}

func sessionLeaseProvider(providerValue, modelValue string) (accounts.Provider, string, error) {
	providerName := strings.ToLower(strings.TrimSpace(providerValue))
	model := strings.TrimSpace(modelValue)
	if providerName == "" {
		modelProvider, modelID, hasProvider := strings.Cut(model, "/")
		if hasProvider {
			providerName = strings.ToLower(strings.TrimSpace(modelProvider))
			model = strings.TrimSpace(modelID)
		} else {
			lowerModel := strings.ToLower(model)
			switch {
			case strings.HasPrefix(lowerModel, "claude-"):
				providerName = "claude"
			case strings.HasPrefix(lowerModel, "kimi-"):
				providerName = "kimi"
			case strings.HasPrefix(lowerModel, "glm-"):
				providerName = "zai"
			default:
				providerName = "codex"
			}
		}
	}
	provider, err := parseSessionLeaseProvider(providerName)
	if err != nil {
		return "", "", err
	}
	if modelProvider, modelID, hasProvider := strings.Cut(model, "/"); hasProvider {
		if modelProviderValue, modelProviderErr := parseSessionLeaseProvider(strings.ToLower(strings.TrimSpace(modelProvider))); modelProviderErr == nil {
			if modelProviderValue != provider {
				return "", "", errors.New("provider and model provider do not match")
			}
			model = strings.TrimSpace(modelID)
		}
	}
	return provider, model, nil
}

func parseSessionLeaseProvider(providerName string) (accounts.Provider, error) {
	switch providerName {
	case "codex", "openai", "openai-codex":
		return accounts.ProviderCodex, nil
	case "claude", "anthropic":
		return accounts.ProviderClaude, nil
	case "kimi", "kimi-for-coding":
		return accounts.ProviderKimi, nil
	case "zai", "glm":
		return accounts.ProviderZAI, nil
	default:
		return "", fmt.Errorf("unsupported provider %q", providerName)
	}
}

func sessionLeaseProxyBaseURL(r *http.Request, override string) (string, error) {
	value := strings.TrimRight(strings.TrimSpace(override), "/")
	if value == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		value = scheme + "://" + r.Host
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("proxyBaseUrl must be an http or https base URL")
	}
	return strings.TrimRight(value, "/"), nil
}

func sessionLeaseScopeKey(request sessionLeaseRequest, provider accounts.Provider, model string) string {
	return strings.Join([]string{
		request.OrganizationID,
		request.WorkspaceID,
		request.ConversationID,
		request.InvocationID,
		request.AgentSessionID,
		string(provider),
		model,
	}, "\x00")
}

func sessionLeaseResponseFor(lease sessionLease, proxyBaseURL string) sessionLeaseResponse {
	baseURL := strings.TrimRight(proxyBaseURL, "/")
	piBaseURL := baseURL
	api := "openai-codex-responses"
	switch lease.Provider {
	case accounts.ProviderCodex:
		baseURL += "/v1"
		// Pi's openai-codex-responses adapter appends /codex/responses.
		// Point it at the ChatGPT-compatible prefix instead of the generic
		// OpenAI-compatible /v1 prefix.
		piBaseURL += "/backend-api"
	case accounts.ProviderClaude:
		api = "anthropic-messages"
	case accounts.ProviderKimi:
		api = "anthropic-messages"
		baseURL += "/kimi"
		piBaseURL += "/kimi"
	case accounts.ProviderZAI:
		api = "anthropic-messages"
		baseURL += "/zai"
		piBaseURL += "/zai"
	}
	environment := map[string]string{
		"CLOUDMUX_SUBROUTER_LEASE_TOKEN": lease.Token,
	}
	if lease.Provider == accounts.ProviderCodex {
		environment["OPENAI_API_KEY"] = lease.Token
		environment["OPENAI_BASE_URL"] = baseURL
	} else {
		environment["ANTHROPIC_API_KEY"] = lease.Token
		environment["ANTHROPIC_AUTH_TOKEN"] = lease.Token
		environment["ANTHROPIC_BASE_URL"] = baseURL
	}
	return sessionLeaseResponse{
		LeaseID:     lease.ID,
		SessionKey:  lease.SessionKey,
		ExpiresAt:   lease.ExpiresAt.Format(time.RFC3339Nano),
		Environment: environment,
		Assignment: sessionLeaseAssignment{
			AccountID: lease.AccountID,
			Provider:  string(lease.Provider),
			AuthMode:  string(lease.AuthMode),
			Model:     lease.Model,
			Reason:    "subrouter_scheduler",
		},
		Pi: sessionLeasePiConfig{
			Provider:                  "cloudmux-subrouter",
			API:                       api,
			BaseURL:                   piBaseURL,
			APIKeyEnvironmentVariable: "CLOUDMUX_SUBROUTER_LEASE_TOKEN",
			Model:                     lease.Model,
		},
	}
}
