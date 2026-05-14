package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/selectacct"
	"github.com/manaflow-ai/subrouter/internal/session"
	"github.com/manaflow-ai/subrouter/internal/transcript"
)

type Server struct {
	Upstream       *url.URL
	CodexUpstream  *url.URL
	APIUpstream    *url.URL
	ClaudeUpstream *url.URL
	Accounts       []accounts.Account
	AccountRef     *AccountRef
	Sessions       *session.Store
	Scheduler      selectacct.Scheduler
	SchedulerRef   *selectacct.SchedulerRef
	UsageScoreTTL  time.Duration
	ScoreAccounts  func(context.Context, []accounts.Account) ([]selectacct.Score, int)
	Transport      http.RoundTripper
	Logger         *slog.Logger
	MaxBodyBytes   int64
	Transcripts    *transcript.Recorder
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

func (r *AccountRef) Reload() ([]accounts.Account, error) {
	if r == nil {
		return nil, nil
	}
	loaded, err := r.store.List()
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.accounts = append([]accounts.Account(nil), loaded...)
	return append([]accounts.Account(nil), loaded...), nil
}

func (r *AccountRef) Refresh(ctx context.Context, account accounts.Account) (accounts.Account, error) {
	if r == nil || account.AuthMode != accounts.AuthModeOAuth || (account.Provider != "" && account.Provider != accounts.ProviderCodex) {
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
	mux.HandleFunc("/_subrouter/reload-accounts", s.handleReloadAccounts)
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

func (s Server) handleReloadAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !isLoopbackRemote(r.RemoteAddr) {
		http.Error(w, "reload-accounts is only available from loopback", http.StatusForbidden)
		return
	}
	loaded, scored, err := s.reloadAccounts(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"ok":              true,
		"accounts":        loaded,
		"usage_refreshed": scored,
	})
}

func (s Server) reloadAccounts(ctx context.Context) (int, int, error) {
	if s.AccountRef == nil {
		return 0, 0, fmt.Errorf("account reload is not configured")
	}
	loaded, err := s.AccountRef.Reload()
	if err != nil {
		return 0, 0, err
	}
	if s.SchedulerRef == nil {
		return len(loaded), 0, nil
	}
	scores, scored := s.scoreAccounts(ctx, loaded)
	if scored == 0 {
		if s.Logger != nil {
			s.Logger.Warn("account reload usage score update skipped", "reason", "no fresh OAuth usage scores")
		}
		return len(loaded), scored, nil
	}
	scheduler := selectacct.NewScheduler(scores)
	if s.Sessions != nil {
		scheduler = scheduler.WithSessionCounts(s.Sessions.CountByAccount())
	}
	s.SchedulerRef.Set(scheduler)
	return len(loaded), scored, nil
}

func (s Server) scoreAccounts(ctx context.Context, available []accounts.Account) ([]selectacct.Score, int) {
	scores := make([]selectacct.Score, 0, len(available))
	scoreByID := make(map[string]int, len(available))
	for _, account := range available {
		headroom := 1.0
		if account.AuthMode == accounts.AuthModeAPIKey {
			headroom = 0.01
		}
		scoreByID[account.ID] = len(scores)
		scores = append(scores, selectacct.Score{
			AccountID:     account.ID,
			Headroom:      headroom,
			ShortHeadroom: headroom,
		})
	}

	client := (*http.Client)(nil)
	if s.AccountRef != nil {
		client = s.AccountRef.client
	}
	if client == nil {
		client = &http.Client{Timeout: defaultUsageFetchTimeout}
	}
	scored := 0
	for _, account := range available {
		if account.AuthMode != accounts.AuthModeOAuth {
			continue
		}
		refreshed, err := s.refreshAccount(ctx, account)
		if err != nil {
			if s.Logger != nil {
				s.Logger.Warn("account reload refresh failed", "account", account.ID, "error", err)
			}
			setZeroScore(scores, scoreByID, account.ID)
			continue
		}
		windows, err := accounts.FetchCodexUsage(ctx, client, refreshed)
		if err != nil {
			if s.Logger != nil {
				s.Logger.Warn("account reload usage fetch failed", "account", account.ID, "error", err)
			}
			setZeroScore(scores, scoreByID, account.ID)
			continue
		}
		limitWindows := make([]selectacct.LimitWindow, 0, len(windows))
		for _, window := range windows {
			limitWindows = append(limitWindows, selectacct.LimitWindow{
				Name:               window.Name,
				UsedPercent:        window.UsedPercent,
				LimitWindowSeconds: window.LimitWindowSeconds,
				ResetAfterSeconds:  window.ResetAfterSeconds,
			})
		}
		if idx, ok := scoreByID[account.ID]; ok {
			scores[idx] = selectacct.ScoreFromLimitWindows(account.ID, 0, limitWindows)
			scored++
		}
	}
	return scores, scored
}

const defaultUsageFetchTimeout = 10 * time.Second

func setZeroScore(scores []selectacct.Score, scoreByID map[string]int, accountID string) {
	if idx, ok := scoreByID[accountID]; ok {
		scores[idx] = selectacct.Score{AccountID: accountID, Headroom: 0, ShortHeadroom: 0}
	}
}

func isLoopbackRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s Server) handleSessions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.Sessions.All())
}

func (s Server) proxyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if baseURLProbeRequest(r) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
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

		setAccountAuthHeaders(r.Header, account)

		upstream := s.upstreamForRequest(r.URL.Path, account)
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
		retryPost := retryableResponsesPostRequest(proxyRequest)
		postReplayable := false
		if retryPost {
			var replayErr error
			postReplayable, replayErr = makeRequestBodyReplayable(proxyRequest, replayablePostMaxBodyBytes)
			if replayErr != nil {
				http.Error(w, "buffer retryable request body: "+replayErr.Error(), http.StatusBadGateway)
				return
			}
			if !postReplayable && s.Logger != nil {
				s.Logger.Warn("retryable request body exceeds retry buffer", "agent", agentType, "session", sessionID, "account", account.ID, "method", r.Method, "path", proxyRequest.URL.Path, "content_length", r.ContentLength, "max_bytes", replayablePostMaxBodyBytes)
			}
		}
		s.recordHTTPMeta(proxyRequest, agentType, sessionID, userEmail, account, upstream)
		if retryPost && postReplayable {
			s.recordReplayableRequestBody(proxyRequest, agentType, sessionID)
		} else {
			s.captureRequestBody(proxyRequest, agentType, sessionID)
		}

		rp := httputil.NewSingleHostReverseProxy(upstream)
		transport := s.transport()
		if retryPost && postReplayable {
			transport = replayablePostRetryTransport{
				base:        transport,
				logger:      s.Logger,
				agent:       agentType,
				session:     sessionID,
				account:     account.ID,
				method:      r.Method,
				path:        proxyRequest.URL.Path,
				upstream:    upstream.Host,
				maxAttempts: replayablePostMaxAttempts,
				limiter:     replayablePostUploadLimiter,
			}
		}
		rp.Transport = transport
		originalDirector := rp.Director
		rp.Director = func(r *http.Request) {
			originalDirector(r)
			r.Host = upstream.Host
		}
		rp.ModifyResponse = func(response *http.Response) error {
			s.captureResponseBody(response, agentType, sessionID, account.ID, proxyRequest.URL.Path)
			return nil
		}
		if s.Logger != nil {
			rp.ErrorLog = log.New(proxyErrorWriter{
				logger:   s.Logger,
				agent:    agentType,
				session:  sessionID,
				account:  account.ID,
				method:   r.Method,
				path:     proxyRequest.URL.Path,
				upstream: upstream.Host,
			}, "", 0)
			rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
				s.Logger.Error("proxy request failed", "agent", agentType, "session", sessionID, "account", account.ID, "method", r.Method, "path", proxyRequest.URL.Path, "upstream", upstream.Host, "error", err)
				http.Error(w, "upstream request failed", http.StatusBadGateway)
			}
		}

		if s.Logger != nil {
			s.Logger.Info("proxy request", "agent", agentType, "session", sessionID, "user", userEmail, "account", account.ID, "method", r.Method, "path", r.URL.Path, "upstream", upstream.Host)
		}
		rp.ServeHTTP(w, proxyRequest)
	})
}

func baseURLProbeRequest(r *http.Request) bool {
	return r.Method == http.MethodHead && (r.URL.Path == "" || r.URL.Path == "/")
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
	setAccountAuthHeaders(headers, account)
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
		s.copyWebSocketMessages(agentType, sessionID, account.ID, "client_to_upstream", clientConn, upstreamConn)
		_ = upstreamConn.Close()
	}()
	go func() {
		defer wg.Done()
		s.copyWebSocketMessages(agentType, sessionID, account.ID, "upstream_to_client", upstreamConn, clientConn)
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

func (s Server) copyWebSocketMessages(agentType, sessionID, accountID, direction string, src, dst *websocket.Conn) {
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
		if direction == "upstream_to_client" && messageType == websocket.TextMessage && usageLimitJSON(body) {
			s.markAccountExhausted(accountID)
		}
		if err := dst.WriteMessage(messageType, body); err != nil {
			return
		}
	}
}

func (s Server) markAccountExhausted(accountID string) {
	if s.SchedulerRef != nil {
		s.SchedulerRef.MarkExhausted(accountID)
	}
}

func usageLimitJSON(body []byte) bool {
	var event map[string]any
	if err := json.Unmarshal(body, &event); err != nil {
		return false
	}
	return usageLimitMap(event)
}

func usageLimitMap(event map[string]any) bool {
	if usageLimitCode(stringField(event, "type")) || usageLimitCode(stringField(event, "code")) {
		return true
	}
	if usageLimitMessage(stringField(event, "message")) {
		return true
	}
	switch value := event["error"].(type) {
	case map[string]any:
		return usageLimitMap(value)
	case string:
		return usageLimitMessage(value)
	default:
		return false
	}
}

func usageLimitCode(value string) bool {
	return strings.EqualFold(value, "usage_limit_reached")
}

func usageLimitMessage(value string) bool {
	lower := strings.ToLower(value)
	return strings.Contains(lower, "usage limit") &&
		(strings.Contains(lower, "reached") || strings.Contains(lower, "hit") || strings.Contains(lower, "exceeded"))
}

func stringField(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return value
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
	r.Body = newStreamingTranscriptReadCloser(streamingTranscriptConfig{
		ReadCloser: r.Body,
		Recorder:   s.Transcripts,
		AgentType:  agentType,
		SessionID:  sessionID,
		EventType:  "http_body",
		Direction:  "client_to_upstream",
		StreamID:   nextTranscriptStreamID(),
	})
}

func (s Server) recordReplayableRequestBody(r *http.Request, agentType, sessionID string) {
	if s.Transcripts == nil || r.GetBody == nil || !s.Transcripts.Enabled() {
		return
	}
	body, err := r.GetBody()
	if err != nil {
		return
	}
	defer body.Close()
	streamID := nextTranscriptStreamID()
	hasher := sha256.New()
	buffer := make([]byte, transcriptHTTPChunkBytes)
	var bytesRead int64
	chunks := 0
	for {
		n, readErr := body.Read(buffer)
		if n > 0 {
			chunk := buffer[:n]
			_, _ = hasher.Write(chunk)
			s.Transcripts.RecordPayloadChunk(agentType, sessionID, "http_body", "client_to_upstream", streamID, chunks, bytesRead, chunk, nil)
			bytesRead += int64(n)
			chunks++
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return
		}
	}
	s.Transcripts.RecordPayloadSummary(agentType, sessionID, "http_body", "client_to_upstream", streamID, bytesRead, hex.EncodeToString(hasher.Sum(nil)), chunks, nil)
}

func (s Server) captureResponseBody(response *http.Response, agentType, sessionID, accountID, path string) {
	inspectUsageLimit := s.SchedulerRef != nil && accountID != "" && responseStatusCanExhaust(response.StatusCode)
	if response.Body == nil || (s.Transcripts == nil && s.Logger == nil && !inspectUsageLimit) {
		return
	}
	payload := map[string]any{"status": response.StatusCode}
	var inspect func([]byte)
	if inspectUsageLimit {
		inspect = func(body []byte) {
			if usageLimitJSON(body) {
				s.markAccountExhausted(accountID)
			}
		}
	}
	response.Body = newStreamingTranscriptReadCloser(streamingTranscriptConfig{
		ReadCloser: response.Body,
		Recorder:   s.Transcripts,
		AgentType:  agentType,
		SessionID:  sessionID,
		EventType:  "http_body",
		Direction:  "upstream_to_client",
		StreamID:   nextTranscriptStreamID(),
		Payload:    payload,
		InspectMax: usageLimitInspectMaxBytes,
		OnInspect:  inspect,
		onReadError: func(err error, bytesRead int) {
			if s.Logger != nil {
				s.Logger.Error("proxy response stream read failed", "agent", agentType, "session", sessionID, "account", accountID, "path", path, "status", response.StatusCode, "bytes", bytesRead, "error", err)
			}
		},
	})
}

func responseStatusCanExhaust(status int) bool {
	return status >= 400 && status < 500
}

const transcriptHTTPChunkBytes = 64 * 1024
const usageLimitInspectMaxBytes = 1 << 20

var transcriptStreamCounter atomic.Uint64

func nextTranscriptStreamID() string {
	return fmt.Sprintf("body-%d", transcriptStreamCounter.Add(1))
}

type streamingTranscriptConfig struct {
	ReadCloser  io.ReadCloser
	Recorder    *transcript.Recorder
	AgentType   string
	SessionID   string
	EventType   string
	Direction   string
	StreamID    string
	Payload     map[string]any
	InspectMax  int64
	OnInspect   func([]byte)
	onReadError func(error, int)
}

func newStreamingTranscriptReadCloser(config streamingTranscriptConfig) io.ReadCloser {
	return &streamingTranscriptReadCloser{
		ReadCloser:  config.ReadCloser,
		recorder:    config.Recorder,
		agentType:   config.AgentType,
		sessionID:   config.SessionID,
		eventType:   config.EventType,
		direction:   config.Direction,
		streamID:    config.StreamID,
		payload:     config.Payload,
		inspectMax:  config.InspectMax,
		onInspect:   config.OnInspect,
		onReadError: config.onReadError,
		hasher:      sha256.New(),
	}
}

type streamingTranscriptReadCloser struct {
	io.ReadCloser
	recorder    *transcript.Recorder
	agentType   string
	sessionID   string
	eventType   string
	direction   string
	streamID    string
	payload     map[string]any
	inspect     []byte
	inspectMax  int64
	onInspect   func([]byte)
	hasher      hash.Hash
	bytesRead   int
	chunks      int
	onReadError func(error, int)
	closeOnce   sync.Once
	readErrOnce sync.Once
}

func (r *streamingTranscriptReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.bytesRead += n
	}
	if n > 0 {
		r.recordChunk(p[:n])
	}
	if err != nil && err != io.EOF && r.onReadError != nil {
		r.readErrOnce.Do(func() {
			r.onReadError(err, r.bytesRead)
		})
	}
	return n, err
}

func (r *streamingTranscriptReadCloser) recordChunk(body []byte) {
	_, _ = r.hasher.Write(body)
	r.captureInspectBytes(body)
	if r.recorder == nil || !r.recorder.Enabled() {
		return
	}
	offset := int64(r.bytesRead - len(body))
	for len(body) > 0 {
		chunk := body
		if len(chunk) > transcriptHTTPChunkBytes {
			chunk = body[:transcriptHTTPChunkBytes]
		}
		r.recorder.RecordPayloadChunk(r.agentType, r.sessionID, r.eventType, r.direction, r.streamID, r.chunks, offset, chunk, r.payload)
		r.chunks++
		offset += int64(len(chunk))
		body = body[len(chunk):]
	}
}

func (r *streamingTranscriptReadCloser) captureInspectBytes(body []byte) {
	if r.onInspect == nil || r.inspectMax <= 0 || int64(len(r.inspect)) >= r.inspectMax {
		return
	}
	remaining := int(r.inspectMax) - len(r.inspect)
	if remaining > len(body) {
		remaining = len(body)
	}
	r.inspect = append(r.inspect, body[:remaining]...)
}

func (r *streamingTranscriptReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.closeOnce.Do(func() {
		sum := hex.EncodeToString(r.hasher.Sum(nil))
		if r.recorder != nil && r.recorder.Enabled() {
			r.recorder.RecordPayloadSummary(r.agentType, r.sessionID, r.eventType, r.direction, r.streamID, int64(r.bytesRead), sum, r.chunks, r.payload)
		}
		if r.onInspect != nil {
			r.onInspect(r.inspect)
		}
	})
	return err
}

type proxyErrorWriter struct {
	logger   *slog.Logger
	agent    string
	session  string
	account  string
	method   string
	path     string
	upstream string
}

func (w proxyErrorWriter) Write(p []byte) (int, error) {
	message := strings.TrimSpace(string(p))
	if message != "" && w.logger != nil {
		w.logger.Error("reverse proxy error", "agent", w.agent, "session", w.session, "account", w.account, "method", w.method, "path", w.path, "upstream", w.upstream, "message", message)
	}
	return len(p), nil
}

const claudeOAuthBetaHeader = "oauth-2025-04-20"

func setAccountAuthHeaders(headers http.Header, account accounts.Account) {
	headers.Set("Authorization", account.AuthorizationHeader())
	switch account.Provider {
	case accounts.ProviderClaude:
		headers.Del("X-Api-Key")
		ensureCommaHeaderValue(headers, "Anthropic-Beta", claudeOAuthBetaHeader)
	case accounts.ProviderCodex, "":
		if account.AccountID != "" {
			headers.Set("ChatGPT-Account-ID", account.AccountID)
		}
	}
}

func ensureCommaHeaderValue(headers http.Header, key, value string) {
	existing := headers.Get(key)
	if existing == "" {
		headers.Set(key, value)
		return
	}
	for _, part := range strings.Split(existing, ",") {
		if strings.TrimSpace(part) == value {
			return
		}
	}
	headers.Set(key, existing+","+value)
}

func (s Server) upstreamForRequest(path string, account accounts.Account) *url.URL {
	if s.Upstream != nil {
		return s.Upstream
	}
	if account.Provider == accounts.ProviderClaude {
		return s.ClaudeUpstream
	}
	if account.AuthMode == accounts.AuthModeAPIKey {
		return s.APIUpstream
	}
	if chatGPTBackendPath(path) {
		return chatGPTBackendUpstream(s.CodexUpstream)
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
	if account.Provider == accounts.ProviderClaude {
		return path
	}
	if account.AuthMode == accounts.AuthModeOAuth {
		if stripped, ok := stripChatGPTBackendPath(path); ok {
			return stripped
		}
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

func chatGPTBackendUpstream(codexUpstream *url.URL) *url.URL {
	upstream := cloneURL(codexUpstream)
	if upstream == nil {
		return nil
	}
	path := strings.TrimRight(upstream.Path, "/")
	if strings.HasSuffix(path, "/backend-api/codex") {
		upstream.Path = strings.TrimSuffix(path, "/codex")
	}
	return upstream
}

func stripChatGPTBackendPath(path string) (string, bool) {
	const prefix = "/backend-api"
	if path == prefix {
		return "/", true
	}
	if strings.HasPrefix(path, prefix+"/") {
		return strings.TrimPrefix(path, prefix), true
	}
	return path, false
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
	provider := providerForAgent(agentType)
	availableAccounts := filterAccountsForProvider(s.accountList(), provider)
	if forcedAccountID != "" {
		account, ok := findAccount(availableAccounts, forcedAccountID)
		if !ok {
			return accounts.Account{}, sessionID, userEmail, fmt.Errorf("requested account %q not found", forcedAccountID)
		}
		if provider == accounts.ProviderCodex && chatGPTBackendPath(r.URL.Path) && account.AuthMode != accounts.AuthModeOAuth {
			return accounts.Account{}, sessionID, userEmail, fmt.Errorf("requested account %q cannot be used for ChatGPT backend paths", forcedAccountID)
		}
		assignment, err := s.Sessions.Put(agentType, sessionID, account.ID, userEmail)
		if err != nil {
			return accounts.Account{}, sessionID, userEmail, err
		}
		return account, sessionID, assignment.UserEmail, nil
	}
	if provider == accounts.ProviderCodex && chatGPTBackendPath(r.URL.Path) {
		availableAccounts = oauthAccounts(availableAccounts)
	}
	if provider == accounts.ProviderCodex {
		s.refreshUsageScoresIfStale(r.Context(), availableAccounts)
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
		if scheduler.Exhausted(account.ID) {
			return accounts.Account{}, sessionID, userEmail, fmt.Errorf("no usable OAuth %s accounts available", provider)
		}
		if provider == accounts.ProviderCodex && s.Logger != nil {
			s.Logger.Warn("selected OAuth Codex account below new-session headroom threshold", "account", account.ID, "threshold", selectacct.MinNewSessionHeadroom)
		}
	}
	assignment, err := s.Sessions.Put(agentType, sessionID, account.ID, userEmail)
	if err != nil {
		return accounts.Account{}, sessionID, userEmail, err
	}
	return account, sessionID, assignment.UserEmail, nil
}

func (s Server) refreshUsageScoresIfStale(ctx context.Context, availableAccounts []accounts.Account) {
	if s.SchedulerRef == nil || s.UsageScoreTTL <= 0 || !s.SchedulerRef.Stale(s.UsageScoreTTL) {
		return
	}
	scoreAccounts := s.ScoreAccounts
	if scoreAccounts == nil {
		scoreAccounts = s.scoreAccounts
	}
	scores, scored := scoreAccounts(ctx, availableAccounts)
	if scored == 0 {
		s.SchedulerRef.Touch()
		if s.Logger != nil {
			s.Logger.Warn("usage score refresh skipped", "reason", "no fresh OAuth usage scores")
		}
		return
	}
	scheduler := selectacct.NewScheduler(scores)
	if s.Sessions != nil {
		scheduler = scheduler.WithSessionCounts(s.Sessions.CountByAccount())
	}
	s.SchedulerRef.Set(scheduler)
	if s.Logger != nil {
		s.Logger.Debug("usage scores refreshed before account selection", "accounts", len(availableAccounts), "scored", scored)
	}
}

func chatGPTBackendPath(path string) bool {
	_, ok := stripChatGPTBackendPath(path)
	return ok
}

func oauthAccounts(all []accounts.Account) []accounts.Account {
	filtered := make([]accounts.Account, 0, len(all))
	for _, account := range all {
		if account.AuthMode == accounts.AuthModeOAuth {
			filtered = append(filtered, account)
		}
	}
	return filtered
}

func providerForAgent(agentType string) accounts.Provider {
	switch session.NormalizeAgentType(agentType) {
	case "claude":
		return accounts.ProviderClaude
	default:
		return accounts.ProviderCodex
	}
}

func filterAccountsForProvider(all []accounts.Account, provider accounts.Provider) []accounts.Account {
	filtered := make([]accounts.Account, 0, len(all))
	legacy := make([]accounts.Account, 0)
	for _, account := range all {
		if account.Provider == provider {
			filtered = append(filtered, account)
			continue
		}
		if account.Provider == "" {
			legacy = append(legacy, account)
		}
	}
	if len(filtered) > 0 {
		return filtered
	}
	return legacy
}

func (s Server) accountList() []accounts.Account {
	out := append([]accounts.Account(nil), s.Accounts...)
	if s.AccountRef != nil {
		out = append(out, s.AccountRef.All()...)
	}
	return out
}

func (s Server) refreshAccount(ctx context.Context, account accounts.Account) (accounts.Account, error) {
	if s.AccountRef == nil || (account.Provider != "" && account.Provider != accounts.ProviderCodex) {
		return account, nil
	}
	return s.AccountRef.Refresh(ctx, account)
}

const replayablePostMaxBodyBytes = 128 << 20
const replayablePostMaxAttempts = 6
const replayablePostMaxConcurrentUploads = 4

var replayablePostUploadLimiter = make(chan struct{}, replayablePostMaxConcurrentUploads)

func retryableResponsesPostRequest(r *http.Request) bool {
	if r == nil || r.Method != http.MethodPost {
		return false
	}
	path := r.URL.Path
	return path == "/responses" ||
		path == "/v1/responses" ||
		path == "/responses/compact" ||
		path == "/v1/responses/compact"
}

func makeRequestBodyReplayable(r *http.Request, maxBytes int64) (bool, error) {
	if r.Body == nil || r.GetBody != nil {
		return true, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		return false, err
	}
	if int64(len(body)) > maxBytes {
		r.Body = prefixReadCloser{
			Reader: io.MultiReader(bytes.NewReader(body), r.Body),
			Closer: r.Body,
		}
		return false, nil
	}
	if err := r.Body.Close(); err != nil {
		return false, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	r.ContentLength = int64(len(body))
	return true, nil
}

type prefixReadCloser struct {
	io.Reader
	io.Closer
}

type replayablePostRetryTransport struct {
	base        http.RoundTripper
	logger      *slog.Logger
	agent       string
	session     string
	account     string
	method      string
	path        string
	upstream    string
	maxAttempts int
	limiter     chan struct{}
}

func (t replayablePostRetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	maxAttempts := t.maxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	attemptReq := req
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		response, err := t.roundTrip(attemptReq)
		if err == nil || !retryablePostTransportError(err) || req.GetBody == nil || req.Context().Err() != nil || attempt == maxAttempts {
			return response, err
		}
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
		body, bodyErr := req.GetBody()
		if bodyErr != nil {
			return response, err
		}
		attemptReq = req.Clone(req.Context())
		attemptReq.Body = body
		attemptReq.GetBody = req.GetBody
		attemptReq.ContentLength = req.ContentLength
		if t.logger != nil {
			t.logger.Warn("retrying replayable upstream request after transport failure", "agent", t.agent, "session", t.session, "account", t.account, "method", t.method, "path", t.path, "upstream", t.upstream, "attempt", attempt+1, "max_attempts", maxAttempts, "error", err)
		}
	}
	return t.roundTrip(req)
}

func (t replayablePostRetryTransport) roundTrip(req *http.Request) (*http.Response, error) {
	if t.limiter == nil {
		return t.base.RoundTrip(req)
	}
	select {
	case t.limiter <- struct{}{}:
		defer func() { <-t.limiter }()
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	return t.base.RoundTrip(req)
}

func retryablePostTransportError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "broken pipe") ||
		strings.Contains(message, "use of closed network connection") ||
		strings.Contains(message, "tls: bad record MAC") ||
		strings.Contains(message, "connection reset by peer") ||
		strings.Contains(message, "unexpected EOF")
}

func (s Server) transport() http.RoundTripper {
	if s.Transport != nil {
		return s.Transport
	}
	return http.DefaultTransport
}

func NewOutboundTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Keep ChatGPT traffic on IPv4 and pooled HTTP/1.1 connections. HTTP/2
	// multiplexing lets one upstream TLS failure tear down unrelated streams.
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	transport.DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp4", addr)
	}
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	} else {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	}
	transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	return transport
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
