package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/selectacct"
	"github.com/manaflow-ai/subrouter/internal/session"
	"github.com/manaflow-ai/subrouter/internal/transcript"
)

type Server struct {
	Upstream      *url.URL
	CodexUpstream *url.URL
	APIUpstream   *url.URL
	Accounts      []accounts.Account
	AccountRef    *AccountRef
	Sessions      *session.Store
	Scheduler     selectacct.Scheduler
	SchedulerRef  *selectacct.SchedulerRef
	Logger        *slog.Logger
	MaxBodyBytes  int64
	Transcripts   *transcript.Recorder
}

type AccountRef struct {
	mu       sync.RWMutex
	accounts []accounts.Account
	store    accounts.CodexStore
	client   *http.Client
}

type AccountStatus struct {
	ID          string            `json:"id"`
	Provider    accounts.Provider `json:"provider"`
	AuthMode    accounts.AuthMode `json:"auth_mode"`
	Email       string            `json:"email,omitempty"`
	Source      string            `json:"source"`
	AuthChecked bool              `json:"auth_checked"`
	AuthValid   bool              `json:"auth_valid"`
	Refreshed   bool              `json:"refreshed,omitempty"`
	Error       string            `json:"error,omitempty"`
}

func NewAccountRef(store accounts.CodexStore, initial []accounts.Account, client *http.Client) *AccountRef {
	return &AccountRef{
		accounts: append([]accounts.Account(nil), initial...),
		store:    store,
		client:   client,
	}
}

func (r *AccountRef) All() []accounts.Account {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]accounts.Account(nil), r.accounts...)
}

func (r *AccountRef) Refresh(ctx context.Context, account accounts.Account) (accounts.Account, error) {
	if r == nil || account.AuthMode != accounts.AuthModeOAuth {
		return account, nil
	}
	stored, ok, err := r.store.FindStored(account.ID)
	if err != nil || !ok {
		return account, err
	}
	refreshed, _, err := r.store.RefreshStoredIfExpired(ctx, r.client, stored)
	if err != nil {
		return account, err
	}
	next, ok := refreshed.Account(refreshed.SourcePath(r.store))
	if !ok {
		return account, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	replaced := false
	for i := range r.accounts {
		if accountMatches(r.accounts[i], account.ID) {
			r.accounts[i] = next
			replaced = true
			break
		}
	}
	if !replaced {
		r.accounts = append(r.accounts, next)
	}
	return next, nil
}

func (r *AccountRef) Statuses(ctx context.Context, forceRefresh bool) []AccountStatus {
	if r == nil {
		return nil
	}
	storedAccounts, err := r.store.ListStored()
	if err != nil {
		return []AccountStatus{{
			Provider:    accounts.ProviderCodex,
			AuthChecked: true,
			AuthValid:   false,
			Error:       err.Error(),
		}}
	}
	out := make([]AccountStatus, 0, len(storedAccounts))
	for _, stored := range storedAccounts {
		status := AccountStatus{
			ID:       stored.Email,
			Provider: accounts.ProviderCodex,
			Email:    stored.Email,
			Source:   stored.SourcePath(r.store),
		}
		if stored.IsAPIKey() {
			status.AuthMode = accounts.AuthModeAPIKey
			out = append(out, status)
			continue
		}
		status.AuthMode = accounts.AuthModeOAuth
		status.AuthChecked = true
		refreshed := stored
		didRefresh := false
		var refreshErr error
		if forceRefresh {
			refreshed, didRefresh, refreshErr = r.store.RefreshStored(ctx, r.client, stored)
		} else {
			refreshed, didRefresh, refreshErr = r.store.RefreshStoredIfExpired(ctx, r.client, stored)
		}
		if refreshErr != nil {
			status.AuthValid = false
			status.Error = refreshErr.Error()
			out = append(out, status)
			continue
		}
		status.AuthValid = true
		status.Refreshed = didRefresh
		if account, ok := refreshed.Account(refreshed.SourcePath(r.store)); ok {
			r.replace(account)
		}
		out = append(out, status)
	}
	return out
}

func (r *AccountRef) replace(account accounts.Account) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.accounts {
		if accountMatches(r.accounts[i], account.ID) {
			r.accounts[i] = account
			return
		}
	}
	r.accounts = append(r.accounts, account)
}

func (s Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/_subrouter/health", s.handleHealth)
	mux.HandleFunc("/_subrouter/accounts", s.handleAccounts)
	mux.HandleFunc("/_subrouter/account-status", s.handleAccountStatus)
	mux.HandleFunc("/_subrouter/sessions", s.handleSessions)
	mux.HandleFunc("/_subrouter/dashboard", s.handleDashboard)
	mux.HandleFunc("/_subrouter/transcripts", s.handleTranscriptList)
	mux.HandleFunc("/_subrouter/transcripts/", s.handleTranscriptDetail)
	mux.HandleFunc("/_subrouter/", http.NotFound)
	mux.Handle("/", s.proxyHandler())
	return mux
}

func (s Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"ok": true})
}

func (s Server) handleAccounts(w http.ResponseWriter, _ *http.Request) {
	type safeAccount struct {
		ID       string            `json:"id"`
		Provider accounts.Provider `json:"provider"`
		AuthMode accounts.AuthMode `json:"auth_mode"`
		Email    string            `json:"email,omitempty"`
		Source   string            `json:"source"`
	}
	availableAccounts := s.accountList()
	out := make([]safeAccount, 0, len(availableAccounts))
	for _, account := range availableAccounts {
		out = append(out, safeAccount{
			ID:       account.ID,
			Provider: account.Provider,
			AuthMode: account.AuthMode,
			Email:    account.Email,
			Source:   account.Source,
		})
	}
	writeJSON(w, out)
}

func (s Server) handleAccountStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	forceRefresh := r.Method == http.MethodPost
	if s.AccountRef != nil {
		writeJSON(w, s.AccountRef.Statuses(r.Context(), forceRefresh))
		return
	}
	accounts := s.accountList()
	out := make([]AccountStatus, 0, len(accounts))
	for _, account := range accounts {
		out = append(out, AccountStatus{
			ID:       account.ID,
			Provider: account.Provider,
			AuthMode: account.AuthMode,
			Email:    account.Email,
			Source:   account.Source,
		})
	}
	writeJSON(w, out)
}

func (s Server) handleSessions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.Sessions.All())
}

func (s Server) proxyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agentType := session.ExtractAgentType(r)
		account, sessionID, userEmail, err := s.accountFor(agentType, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		account, err = s.refreshAccount(r.Context(), account)
		if err != nil {
			http.Error(w, "refresh selected account: "+err.Error(), http.StatusServiceUnavailable)
			return
		}

		auth := account.AuthorizationHeader()
		if auth == "" {
			http.Error(w, "selected account has no usable credential", http.StatusServiceUnavailable)
			return
		}

		r.Header.Set("Authorization", auth)
		if account.AccountID != "" {
			r.Header.Set("ChatGPT-Account-ID", account.AccountID)
		}

		upstream := s.upstreamFor(account)
		if upstream == nil {
			http.Error(w, "no upstream configured", http.StatusServiceUnavailable)
			return
		}
		if websocket.IsWebSocketUpgrade(r) {
			s.proxyWebSocket(w, r, account, agentType, sessionID, userEmail, upstream)
			return
		}
		proxyRequest := r.Clone(r.Context())
		proxyRequest.URL = cloneURL(r.URL)
		proxyRequest.URL.Path = s.pathForUpstream(proxyRequest.URL.Path, account)
		proxyRequest.URL.RawPath = ""
		session.StripSubrouterHeaders(proxyRequest.Header)
		s.recordHTTPMeta(proxyRequest, agentType, sessionID, userEmail, account, upstream)
		s.captureRequestBody(proxyRequest, agentType, sessionID)

		rp := httputil.NewSingleHostReverseProxy(upstream)
		originalDirector := rp.Director
		rp.Director = func(r *http.Request) {
			originalDirector(r)
			r.Host = upstream.Host
		}
		rp.ModifyResponse = func(response *http.Response) error {
			s.captureResponseBody(response, agentType, sessionID)
			return nil
		}

		if s.Logger != nil {
			s.Logger.Info("proxy request", "agent", agentType, "session", sessionID, "user", userEmail, "account", account.ID, "method", r.Method, "path", r.URL.Path, "upstream", upstream.Host)
		}
		rp.ServeHTTP(w, proxyRequest)
	})
}

func (s Server) proxyWebSocket(w http.ResponseWriter, r *http.Request, account accounts.Account, agentType, sessionID, userEmail string, upstream *url.URL) {
	upstreamURL := cloneURL(r.URL)
	upstreamURL.Scheme = websocketScheme(upstream.Scheme)
	upstreamURL.Host = upstream.Host
	upstreamURL.Path = joinURLPath(upstream.Path, s.pathForUpstream(upstreamURL.Path, account))
	upstreamURL.RawPath = ""

	headers := r.Header.Clone()
	stripWebSocketDialHeaders(headers)
	session.StripSubrouterHeaders(headers)
	headers.Set("Authorization", account.AuthorizationHeader())
	if account.AccountID != "" {
		headers.Set("ChatGPT-Account-ID", account.AccountID)
	}
	s.recordWebSocketMeta(r, upstreamURL, headers, agentType, sessionID, userEmail, account, upstream)

	upstreamConn, response, err := websocket.DefaultDialer.Dial(upstreamURL.String(), headers)
	if err != nil {
		status := 502
		if response != nil {
			status = response.StatusCode
		}
		http.Error(w, err.Error(), status)
		return
	}
	defer upstreamConn.Close()
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}

	upgrader := websocket.Upgrader{
		CheckOrigin:       func(_ *http.Request) bool { return true },
		EnableCompression: true,
	}
	responseHeader := http.Header{}
	if response != nil {
		responseHeader = cloneWebSocketResponseHeaders(response.Header)
	}
	clientConn, err := upgrader.Upgrade(w, r, responseHeader)
	if err != nil {
		return
	}
	defer clientConn.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.copyWebSocketMessages(agentType, sessionID, "client_to_upstream", clientConn, upstreamConn)
		_ = upstreamConn.Close()
	}()
	go func() {
		defer wg.Done()
		s.copyWebSocketMessages(agentType, sessionID, "upstream_to_client", upstreamConn, clientConn)
		_ = clientConn.Close()
	}()
	wg.Wait()
}

func stripWebSocketDialHeaders(headers http.Header) {
	for _, key := range []string{
		"Connection",
		"Upgrade",
		"Sec-Websocket-Key",
		"Sec-WebSocket-Key",
		"Sec-Websocket-Version",
		"Sec-WebSocket-Version",
		"Sec-Websocket-Extensions",
		"Sec-WebSocket-Extensions",
		"Sec-Websocket-Accept",
		"Sec-WebSocket-Accept",
	} {
		headers.Del(key)
	}
}

func cloneWebSocketResponseHeaders(headers http.Header) http.Header {
	out := http.Header{}
	for key, values := range headers {
		lower := strings.ToLower(key)
		if lower == "connection" || lower == "upgrade" || strings.HasPrefix(lower, "sec-websocket-") {
			continue
		}
		out[key] = append([]string(nil), values...)
	}
	return out
}

func (s Server) copyWebSocketMessages(agentType, sessionID, direction string, src, dst *websocket.Conn) {
	for {
		messageType, body, err := src.ReadMessage()
		if err != nil {
			return
		}
		if s.Transcripts != nil {
			s.Transcripts.RecordPayload(agentType, sessionID, "websocket_message", direction, body, map[string]any{
				"opcode": websocketMessageType(messageType),
			})
		}
		if err := dst.WriteMessage(messageType, body); err != nil {
			return
		}
	}
}

func websocketScheme(scheme string) string {
	if scheme == "https" {
		return "wss"
	}
	return "ws"
}

func joinURLPath(basePath, requestPath string) string {
	if basePath == "" || basePath == "/" {
		if requestPath == "" {
			return "/"
		}
		return requestPath
	}
	if requestPath == "" || requestPath == "/" {
		return basePath
	}
	return strings.TrimRight(basePath, "/") + "/" + strings.TrimLeft(requestPath, "/")
}

func websocketMessageType(messageType int) string {
	switch messageType {
	case websocket.TextMessage:
		return "text"
	case websocket.BinaryMessage:
		return "binary"
	case websocket.CloseMessage:
		return "close"
	case websocket.PingMessage:
		return "ping"
	case websocket.PongMessage:
		return "pong"
	default:
		return "unknown"
	}
}

func (s Server) recordHTTPMeta(r *http.Request, agentType, sessionID, userEmail string, account accounts.Account, upstream *url.URL) {
	if s.Transcripts == nil {
		return
	}
	s.Transcripts.RecordMeta(agentType, sessionID, map[string]any{
		"transport": "http",
		"user":      userEmail,
		"account":   account.ID,
		"method":    r.Method,
		"path":      r.URL.Path,
		"upstream":  upstream.String(),
		"headers":   transcript.RedactedHeaders(r.Header),
	})
}

func (s Server) recordWebSocketMeta(r *http.Request, upstreamURL *url.URL, headers http.Header, agentType, sessionID, userEmail string, account accounts.Account, upstream *url.URL) {
	if s.Transcripts == nil {
		return
	}
	s.Transcripts.RecordMeta(agentType, sessionID, map[string]any{
		"transport":    "websocket",
		"user":         userEmail,
		"account":      account.ID,
		"method":       r.Method,
		"path":         r.URL.Path,
		"upstream":     upstream.String(),
		"upstream_url": upstreamURL.String(),
		"headers":      transcript.RedactedHeaders(headers),
	})
}

func (s Server) captureRequestBody(r *http.Request, agentType, sessionID string) {
	if s.Transcripts == nil || r.Body == nil {
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		r.Body = io.NopCloser(bytes.NewReader(nil))
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	s.Transcripts.RecordPayload(agentType, sessionID, "http_body", "client_to_upstream", body, map[string]any{})
}

func (s Server) captureResponseBody(response *http.Response, agentType, sessionID string) {
	if s.Transcripts == nil || response.Body == nil {
		return
	}
	response.Body = &recordingReadCloser{
		ReadCloser: response.Body,
		onClose: func(body []byte) {
			s.Transcripts.RecordPayload(agentType, sessionID, "http_body", "upstream_to_client", body, map[string]any{
				"status": response.StatusCode,
			})
		},
	}
}

type recordingReadCloser struct {
	io.ReadCloser
	buf     bytes.Buffer
	onClose func([]byte)
	once    sync.Once
}

func (r *recordingReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		_, _ = r.buf.Write(p[:n])
	}
	return n, err
}

func (r *recordingReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(func() {
		r.onClose(r.buf.Bytes())
	})
	return err
}

func (s Server) upstreamFor(account accounts.Account) *url.URL {
	if s.Upstream != nil {
		return s.Upstream
	}
	if account.AuthMode == accounts.AuthModeAPIKey {
		return s.APIUpstream
	}
	return s.CodexUpstream
}

func (s Server) pathForUpstream(path string, account accounts.Account) string {
	if s.Upstream != nil {
		return path
	}
	if path == "" {
		path = "/"
	}
	if account.AuthMode == accounts.AuthModeAPIKey {
		if path == "/v1" || strings.HasPrefix(path, "/v1/") {
			return path
		}
		return "/v1" + path
	}
	if path == "/v1" {
		return "/"
	}
	if strings.HasPrefix(path, "/v1/") {
		return strings.TrimPrefix(path, "/v1")
	}
	return path
}

func cloneURL(value *url.URL) *url.URL {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func (s Server) accountFor(agentType string, r *http.Request) (accounts.Account, string, string, error) {
	sessionID := session.ExtractID(r, s.MaxBodyBytes)
	userEmail := session.ExtractUserEmail(r)
	forcedAccountID := session.ExtractAccountID(r)
	availableAccounts := s.accountList()
	if forcedAccountID != "" {
		account, ok := findAccount(availableAccounts, forcedAccountID)
		if !ok {
			return accounts.Account{}, sessionID, userEmail, fmt.Errorf("requested account %q not found", forcedAccountID)
		}
		assignment, err := s.Sessions.Put(agentType, sessionID, account.ID, userEmail)
		if err != nil {
			return accounts.Account{}, sessionID, userEmail, err
		}
		return account, sessionID, assignment.UserEmail, nil
	}
	scheduler := s.scheduler().WithSessionCounts(s.Sessions.CountByAccount())
	if assignment, ok := s.Sessions.Get(agentType, sessionID); ok {
		if userEmail != "" && userEmail != assignment.UserEmail {
			updated, err := s.Sessions.Put(agentType, sessionID, assignment.AccountID, userEmail)
			if err != nil {
				return accounts.Account{}, sessionID, userEmail, err
			}
			assignment = updated
		}
		if userEmail == "" {
			userEmail = assignment.UserEmail
		}
		if account, ok := findAccount(availableAccounts, assignment.AccountID); ok {
			if !scheduler.Exhausted(account.ID) {
				return account, sessionID, userEmail, nil
			}
		}
	}

	account, err := scheduler.Pick(availableAccounts)
	if err != nil {
		return accounts.Account{}, sessionID, userEmail, err
	}
	if account.AuthMode == accounts.AuthModeOAuth && !scheduler.UsableForNewSession(account.ID) {
		return accounts.Account{}, sessionID, userEmail, fmt.Errorf("no usable OAuth Codex accounts available")
	}
	assignment, err := s.Sessions.Put(agentType, sessionID, account.ID, userEmail)
	if err != nil {
		return accounts.Account{}, sessionID, userEmail, err
	}
	return account, sessionID, assignment.UserEmail, nil
}

func (s Server) accountList() []accounts.Account {
	if s.AccountRef != nil {
		return s.AccountRef.All()
	}
	return append([]accounts.Account(nil), s.Accounts...)
}

func (s Server) refreshAccount(ctx context.Context, account accounts.Account) (accounts.Account, error) {
	if s.AccountRef == nil {
		return account, nil
	}
	return s.AccountRef.Refresh(ctx, account)
}

func (s Server) scheduler() selectacct.Scheduler {
	if s.SchedulerRef != nil {
		return s.SchedulerRef.Get()
	}
	return s.Scheduler
}

func findAccount(haystack []accounts.Account, id string) (accounts.Account, bool) {
	needle := strings.TrimSpace(id)
	for _, account := range haystack {
		if accountMatches(account, needle) {
			return account, true
		}
	}
	return accounts.Account{}, false
}

func accountMatches(account accounts.Account, id string) bool {
	if id == "" {
		return false
	}
	if strings.EqualFold(account.ID, id) || strings.EqualFold(account.Label, id) || strings.EqualFold(account.Email, id) {
		return true
	}
	if account.AuthMode == accounts.AuthModeAPIKey && strings.EqualFold(strings.TrimPrefix(account.ID, "apikey:"), id) {
		return true
	}
	return false
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}
