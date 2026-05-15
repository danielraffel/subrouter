package accounts

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

const codexOAuthTokenURL = "https://auth.openai.com/oauth/token"
const codexOAuthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

type codexRefreshReasonKey struct{}

func WithCodexRefreshReason(ctx context.Context, reason string) context.Context {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ctx
	}
	return context.WithValue(ctx, codexRefreshReasonKey{}, reason)
}

func CodexRefreshReason(ctx context.Context) string {
	reason, _ := ctx.Value(codexRefreshReasonKey{}).(string)
	return reason
}

func ReadActiveCodexAuth() (CodexAuthFile, bool, error) {
	body, err := os.ReadFile(DefaultCodexAuthPath())
	if os.IsNotExist(err) {
		return CodexAuthFile{}, false, nil
	}
	if err != nil {
		return CodexAuthFile{}, false, err
	}
	var auth CodexAuthFile
	if err := json.Unmarshal(body, &auth); err != nil {
		return CodexAuthFile{}, false, err
	}
	return auth, true, nil
}

func WriteActiveCodexAuth(auth CodexAuthFile) error {
	body, err := json.Marshal(auth)
	if err != nil {
		return err
	}
	return writeCodexActiveAuth(DefaultCodexAuthPath(), body)
}

func (s CodexStore) DetectActiveAccount() (string, error) {
	auth, ok, err := ReadActiveCodexAuth()
	if err != nil || !ok {
		return "", err
	}
	if auth.Tokens != nil && auth.Tokens.IDToken != "" {
		email, err := ExtractEmailFromJWT(auth.Tokens.IDToken)
		if err == nil && email != "" {
			if _, found, err := s.FindStored(email); err != nil {
				return "", err
			} else if found {
				return email, nil
			}
		}
	}
	if auth.OpenAIAPIKey != "" {
		all, err := s.ListStored()
		if err != nil {
			return "", err
		}
		for _, account := range all {
			if account.Auth.OpenAIAPIKey == auth.OpenAIAPIKey {
				return account.Email, nil
			}
		}
	}
	return "", nil
}

func (s CodexStore) SyncActiveToStore() error {
	auth, ok, err := ReadActiveCodexAuth()
	if err != nil || !ok {
		return err
	}
	if auth.Tokens == nil || auth.Tokens.IDToken == "" {
		return nil
	}
	email, err := ExtractEmailFromJWT(auth.Tokens.IDToken)
	if err != nil || email == "" {
		return nil
	}
	lock, err := s.lockStoredAccount(email)
	if err != nil {
		return err
	}
	defer lock.Close()
	account, found, err := s.FindStored(email)
	if err != nil || !found {
		return err
	}
	if accountAuthNewerThanIncoming(account.Auth, auth) {
		return nil
	}
	account.Auth = auth
	return s.saveStoredUnlocked(account)
}

func (s CodexStore) ImportActive() (StoredCodexAccount, bool, error) {
	auth, ok, err := ReadActiveCodexAuth()
	if err != nil || !ok {
		return StoredCodexAccount{}, false, err
	}
	if auth.Tokens == nil || auth.Tokens.IDToken == "" {
		return StoredCodexAccount{}, false, fmt.Errorf("no active Codex OAuth auth found in %s", DefaultCodexAuthPath())
	}
	email, err := ExtractEmailFromJWT(auth.Tokens.IDToken)
	if err != nil || email == "" {
		return StoredCodexAccount{}, false, fmt.Errorf("could not extract email from current auth token")
	}
	account, existed, err := s.FindStored(email)
	if err != nil {
		return StoredCodexAccount{}, false, err
	}
	if !existed {
		account = StoredCodexAccount{
			Email:   email,
			AddedAt: time.Now().UTC().Format(time.RFC3339),
		}
	}
	account.Auth = auth
	return account, existed, s.SaveStored(account)
}

func (s CodexStore) AddAPIKey(label, key string) (StoredCodexAccount, bool, error) {
	label = strings.TrimSpace(label)
	key = strings.TrimSpace(key)
	if label == "" {
		return StoredCodexAccount{}, false, fmt.Errorf("label is required")
	}
	if !strings.HasPrefix(key, "sk-") {
		return StoredCodexAccount{}, false, fmt.Errorf("invalid API key format, expected sk-...")
	}
	email := "apikey:" + label
	account, existed, err := s.FindStored(email)
	if err != nil {
		return StoredCodexAccount{}, false, err
	}
	if !existed {
		account = StoredCodexAccount{
			Email:   email,
			AddedAt: time.Now().UTC().Format(time.RFC3339),
		}
	}
	account.Auth = CodexAuthFile{
		AuthMode:     "apikey",
		OpenAIAPIKey: key,
	}
	return account, existed, s.SaveStored(account)
}

func (s CodexStore) RefreshStoredIfExpired(ctx context.Context, client *http.Client, account StoredCodexAccount) (StoredCodexAccount, bool, error) {
	return s.refreshStored(ctx, client, account, false)
}

func (s CodexStore) RefreshStored(ctx context.Context, client *http.Client, account StoredCodexAccount) (StoredCodexAccount, bool, error) {
	return s.refreshStored(ctx, client, account, true)
}

func (s CodexStore) refreshStored(ctx context.Context, client *http.Client, account StoredCodexAccount, force bool) (StoredCodexAccount, bool, error) {
	if account.Auth.Tokens == nil {
		logCodexRefreshSkipped(ctx, account, force, "missing_tokens")
		return account, false, nil
	}
	if !force && !IsJWTExpired(account.Auth.Tokens.AccessToken, 60*time.Second) {
		logCodexRefreshSkipped(ctx, account, force, "access_token_fresh")
		return account, false, nil
	}

	lock, err := s.lockStoredAccount(account.Email)
	if err != nil {
		logCodexRefreshFailed(ctx, account, force, err)
		return account, false, err
	}
	defer lock.Close()

	latest, found, err := s.FindStored(account.Email)
	if err != nil {
		logCodexRefreshFailed(ctx, account, force, err)
		return account, false, err
	}
	if found {
		account = latest
	}
	if account.Auth.Tokens == nil {
		logCodexRefreshSkipped(ctx, account, force, "missing_tokens_after_lock")
		return account, false, nil
	}
	if !force && !IsJWTExpired(account.Auth.Tokens.AccessToken, 60*time.Second) {
		logCodexRefreshSkipped(ctx, account, force, "access_token_fresh_after_lock")
		return account, false, nil
	}

	logCodexRefreshStart(ctx, account, force)
	auth, err := RefreshCodexAuth(ctx, client, account.Auth)
	if err != nil {
		if recovered, ok := s.recoverRefreshedAccount(account); ok {
			logCodexRefreshRecovered(ctx, account, recovered, force, err)
			return recovered, false, nil
		}
		logCodexRefreshFailed(ctx, account, force, err)
		return account, false, err
	}
	account.Auth = auth
	if err := s.saveStoredUnlocked(account); err != nil {
		logCodexRefreshFailed(ctx, account, force, err)
		return account, true, err
	}
	if err := syncActiveCodexAuthIfAccountActive(account); err != nil {
		logCodexRefreshFailed(ctx, account, force, err)
		return account, true, err
	}
	logCodexRefreshSucceeded(ctx, account, force)
	return account, true, nil
}

func (s CodexStore) recoverRefreshedAccount(previous StoredCodexAccount) (StoredCodexAccount, bool) {
	latest, found, err := s.FindStored(previous.Email)
	if err != nil || !found || latest.Auth.Tokens == nil {
		return StoredCodexAccount{}, false
	}
	if previous.Auth.Tokens != nil &&
		latest.Auth.Tokens.AccessToken == previous.Auth.Tokens.AccessToken &&
		latest.Auth.Tokens.RefreshToken == previous.Auth.Tokens.RefreshToken {
		return StoredCodexAccount{}, false
	}
	if IsJWTExpired(latest.Auth.Tokens.AccessToken, 60*time.Second) {
		return StoredCodexAccount{}, false
	}
	return latest, true
}

func accountAuthNewerThanIncoming(stored, incoming CodexAuthFile) bool {
	if stored.Tokens == nil || incoming.Tokens == nil {
		return false
	}
	if stored.Tokens.AccessToken == incoming.Tokens.AccessToken &&
		stored.Tokens.RefreshToken == incoming.Tokens.RefreshToken {
		return false
	}
	storedExpiry, storedOK := JWTExpiryMillis(stored.Tokens.AccessToken)
	incomingExpiry, incomingOK := JWTExpiryMillis(incoming.Tokens.AccessToken)
	if storedOK && incomingOK {
		return storedExpiry > incomingExpiry
	}
	return !IsJWTExpired(stored.Tokens.AccessToken, 60*time.Second) &&
		IsJWTExpired(incoming.Tokens.AccessToken, 60*time.Second)
}

func syncActiveCodexAuthIfAccountActive(account StoredCodexAccount) error {
	activeEmail, ok, err := activeCodexAuthEmail()
	if err != nil || !ok || activeEmail != account.Email {
		return err
	}
	return WriteActiveCodexAuth(account.Auth)
}

func activeCodexAuthEmail() (string, bool, error) {
	auth, ok, err := ReadActiveCodexAuth()
	if err != nil || !ok || auth.Tokens == nil || auth.Tokens.IDToken == "" {
		return "", false, err
	}
	email, err := ExtractEmailFromJWT(auth.Tokens.IDToken)
	if err != nil || email == "" {
		return "", false, err
	}
	return email, true, nil
}

func RefreshCodexAuthIfExpired(ctx context.Context, client *http.Client, auth CodexAuthFile) (CodexAuthFile, bool, error) {
	if auth.Tokens == nil {
		return auth, false, nil
	}
	if !IsJWTExpired(auth.Tokens.AccessToken, 60*time.Second) {
		return auth, false, nil
	}
	refreshed, err := RefreshCodexAuth(ctx, client, auth)
	return refreshed, err == nil, err
}

func RefreshCodexAuth(ctx context.Context, client *http.Client, auth CodexAuthFile) (CodexAuthFile, error) {
	if auth.Tokens == nil {
		return auth, nil
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	payload := map[string]string{
		"client_id":     codexOAuthClientID,
		"grant_type":    "refresh_token",
		"refresh_token": auth.Tokens.RefreshToken,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return auth, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexOAuthTokenURL, bytes.NewReader(body))
	if err != nil {
		return auth, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return auth, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(res.Body)
		return auth, newCodexAuthRefreshError(res.StatusCode, strings.TrimSpace(buf.String()))
	}
	var refreshed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}
	if err := json.NewDecoder(res.Body).Decode(&refreshed); err != nil {
		return auth, err
	}
	if refreshed.AccessToken == "" || refreshed.RefreshToken == "" || refreshed.IDToken == "" {
		return auth, fmt.Errorf("token refresh response missing required fields")
	}
	auth.Tokens.AccessToken = refreshed.AccessToken
	auth.Tokens.RefreshToken = refreshed.RefreshToken
	auth.Tokens.IDToken = refreshed.IDToken
	auth.LastRefresh = time.Now().UTC().Format(time.RFC3339)
	return auth, nil
}

type CodexAuthRefreshError struct {
	StatusCode      int
	Body            string
	ProviderType    string
	ProviderCode    string
	ProviderMessage string
}

func newCodexAuthRefreshError(statusCode int, body string) *CodexAuthRefreshError {
	err := &CodexAuthRefreshError{
		StatusCode: statusCode,
		Body:       body,
	}
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &payload) == nil {
		err.ProviderMessage = payload.Error.Message
		err.ProviderType = payload.Error.Type
		err.ProviderCode = payload.Error.Code
	}
	return err
}

func (e *CodexAuthRefreshError) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.Body) == "" {
		return fmt.Sprintf("token refresh failed (%d)", e.StatusCode)
	}
	return fmt.Sprintf("token refresh failed (%d): %s", e.StatusCode, e.Body)
}

func logCodexRefreshStart(ctx context.Context, account StoredCodexAccount, force bool) {
	slog.Info("codex oauth refresh start", codexRefreshLogAttrs(ctx, account, force)...)
}

func logCodexRefreshSucceeded(ctx context.Context, account StoredCodexAccount, force bool) {
	slog.Info("codex oauth refresh succeeded", codexRefreshLogAttrs(ctx, account, force)...)
}

func logCodexRefreshRecovered(ctx context.Context, previous, recovered StoredCodexAccount, force bool, err error) {
	attrs := codexRefreshLogAttrs(ctx, previous, force)
	attrs = append(attrs,
		"recovered_access_exp", codexAccessExpiryForLog(recovered),
	)
	appendCodexRefreshErrorAttrs(&attrs, err)
	slog.Warn("codex oauth refresh recovered from concurrent update", attrs...)
}

func logCodexRefreshFailed(ctx context.Context, account StoredCodexAccount, force bool, err error) {
	attrs := codexRefreshLogAttrs(ctx, account, force)
	appendCodexRefreshErrorAttrs(&attrs, err)
	slog.Warn("codex oauth refresh failed", attrs...)
}

func logCodexRefreshSkipped(ctx context.Context, account StoredCodexAccount, force bool, reason string) {
	attrs := codexRefreshLogAttrs(ctx, account, force)
	attrs = append(attrs, "skip_reason", reason)
	slog.Debug("codex oauth refresh skipped", attrs...)
}

func codexRefreshLogAttrs(ctx context.Context, account StoredCodexAccount, force bool) []any {
	reason := CodexRefreshReason(ctx)
	if reason == "" {
		reason = "unspecified"
	}
	return []any{
		"account", account.Email,
		"reason", reason,
		"force", force,
		"last_refresh", account.Auth.LastRefresh,
		"access_exp", codexAccessExpiryForLog(account),
		"access_expired", codexAccessExpiredForLog(account),
	}
}

func codexAccessExpiryForLog(account StoredCodexAccount) string {
	if account.Auth.Tokens == nil {
		return ""
	}
	expMillis, ok := JWTExpiryMillis(account.Auth.Tokens.AccessToken)
	if !ok {
		return ""
	}
	return time.UnixMilli(expMillis).UTC().Format(time.RFC3339)
}

func codexAccessExpiredForLog(account StoredCodexAccount) bool {
	if account.Auth.Tokens == nil {
		return false
	}
	return IsJWTExpired(account.Auth.Tokens.AccessToken, 60*time.Second)
}

func appendCodexRefreshErrorAttrs(attrs *[]any, err error) {
	if err == nil {
		return
	}
	var refreshErr *CodexAuthRefreshError
	if errors.As(err, &refreshErr) {
		*attrs = append(*attrs,
			"status", refreshErr.StatusCode,
			"provider_type", refreshErr.ProviderType,
			"provider_code", refreshErr.ProviderCode,
			"provider_message", refreshErr.ProviderMessage,
		)
		return
	}
	*attrs = append(*attrs, "error", err)
}

func DecodeJWTClaims(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid JWT format")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, err
	}
	return claims, nil
}

func ExtractEmailFromJWT(token string) (string, error) {
	claims, err := DecodeJWTClaims(token)
	if err != nil {
		return "", err
	}
	if profile, ok := claims["https://api.openai.com/profile"].(map[string]any); ok {
		if email, ok := profile["email"].(string); ok && email != "" {
			return email, nil
		}
	}
	if email, ok := claims["email"].(string); ok {
		return email, nil
	}
	return "", nil
}

func ExtractChatGPTAccountID(auth CodexAuthFile) string {
	if auth.Tokens == nil {
		return ""
	}
	if auth.Tokens.AccountID != "" {
		return auth.Tokens.AccountID
	}
	for _, token := range []string{auth.Tokens.IDToken, auth.Tokens.AccessToken} {
		accountID := ExtractChatGPTAccountIDFromJWT(token)
		if accountID != "" {
			return accountID
		}
	}
	return ""
}

func ExtractChatGPTAccountIDFromJWT(token string) string {
	if token == "" {
		return ""
	}
	claims, err := DecodeJWTClaims(token)
	if err != nil {
		return ""
	}
	if accountID, ok := claims["chatgpt_account_id"].(string); ok && accountID != "" {
		return accountID
	}
	if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if accountID, ok := auth["chatgpt_account_id"].(string); ok && accountID != "" {
			return accountID
		}
	}
	if orgs, ok := claims["organizations"].([]any); ok && len(orgs) > 0 {
		if org, ok := orgs[0].(map[string]any); ok {
			if id, ok := org["id"].(string); ok && id != "" {
				return id
			}
		}
	}
	return ""
}

func JWTExpiryMillis(token string) (int64, bool) {
	claims, err := DecodeJWTClaims(token)
	if err != nil {
		return 0, false
	}
	exp, ok := numericClaim(claims["exp"])
	if !ok {
		return 0, false
	}
	return int64(exp * 1000), true
}

func IsJWTExpired(token string, grace time.Duration) bool {
	claims, err := DecodeJWTClaims(token)
	if err != nil {
		return false
	}
	exp, ok := numericClaim(claims["exp"])
	if !ok {
		return false
	}
	return time.Until(time.Unix(int64(exp), 0)) < grace
}

func numericClaim(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int64:
		return float64(v), true
	case json.Number:
		out, err := v.Float64()
		return out, err == nil
	default:
		return 0, false
	}
}
