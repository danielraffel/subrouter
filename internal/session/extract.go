package session

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
)

var headerCandidates = []string{
	"X-Subrouter-Session",
	"X-Codex-Window-ID",
	"X-Codex-Turn-State",
	"X-Codex-Parent-Thread-ID",
	"X-Session-ID",
	"X-Conversation-ID",
	"X-Codex-Session-ID",
	"X-Claude-Session-ID",
	"X-Gemini-Session-ID",
	"X-Gemini-Conversation-ID",
	"OpenAI-Conversation-ID",
	"Anthropic-Conversation-ID",
	"Google-Conversation-ID",
	"Idempotency-Key",
}

var userEmailHeaderCandidates = []string{
	"X-Subrouter-User-Email",
	"X-Subrouter-User",
	"X-User-Email",
}

var accountIDHeaderCandidates = []string{
	"X-Subrouter-Account-ID",
	"X-Subrouter-Account",
}

var codexAgentHeaderCandidates = []string{
	"X-Codex-Window-ID",
	"X-Codex-Turn-State",
	"X-Codex-Parent-Thread-ID",
	"X-Codex-Session-ID",
	"OpenAI-Conversation-ID",
}

var claudeAgentHeaderCandidates = []string{
	"X-Claude-Session-ID",
	"Anthropic-Conversation-ID",
}

var geminiAgentHeaderCandidates = []string{
	"X-Gemini-Session-ID",
	"X-Gemini-Conversation-ID",
	"Google-Conversation-ID",
}

var jsonCandidates = map[string]struct{}{
	"session_id":      {},
	"conversation_id": {},
	"thread_id":       {},
}

func ExtractAgentType(r *http.Request) string {
	if explicit := NormalizeAgentType(r.Header.Get("X-Subrouter-Agent")); explicit != "" {
		return explicit
	}
	if hasAnyHeader(r, claudeAgentHeaderCandidates) {
		return "claude"
	}
	if hasAnyHeader(r, geminiAgentHeaderCandidates) {
		return "gemini"
	}
	if hasAnyHeader(r, codexAgentHeaderCandidates) {
		return "codex"
	}
	return "codex"
}

func NormalizeAgentType(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" || len(normalized) > 64 {
		return ""
	}
	if !agentTypePattern.MatchString(normalized) {
		return ""
	}
	return normalized
}

func ExtractUserEmail(r *http.Request) string {
	for _, header := range userEmailHeaderCandidates {
		if email := NormalizeUserEmail(r.Header.Get(header)); email != "" {
			return email
		}
	}
	return ""
}

func ExtractAccountID(r *http.Request) string {
	for _, header := range accountIDHeaderCandidates {
		if accountID := NormalizeAccountID(r.Header.Get(header)); accountID != "" {
			return accountID
		}
	}
	return ""
}

func NormalizeUserEmail(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || len(trimmed) > 320 {
		return ""
	}
	address, err := mail.ParseAddress(trimmed)
	if err != nil {
		return ""
	}
	return strings.ToLower(address.Address)
}

func NormalizeAccountID(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || len(trimmed) > 256 {
		return ""
	}
	return trimmed
}

func StripSubrouterHeaders(headers http.Header) {
	headers.Del("X-Subrouter-Session")
	headers.Del("X-Subrouter-Agent")
	headers.Del("X-Subrouter-User-Email")
	headers.Del("X-Subrouter-User")
	headers.Del("X-User-Email")
	headers.Del("X-Subrouter-Account-ID")
	headers.Del("X-Subrouter-Account")
}

func ExtractID(r *http.Request, maxBodyBytes int64) string {
	for _, header := range headerCandidates {
		if value := strings.TrimSpace(r.Header.Get(header)); value != "" {
			return value
		}
	}

	for _, key := range []string{"session_id", "conversation_id", "thread_id"} {
		if value := strings.TrimSpace(r.URL.Query().Get(key)); value != "" {
			return value
		}
	}

	if id := extractJSONID(r, maxBodyBytes); id != "" {
		return id
	}

	return fallbackID(r)
}

func extractJSONID(r *http.Request, maxBodyBytes int64) string {
	if r.Body == nil || maxBodyBytes <= 0 {
		return ""
	}
	if r.ContentLength < 0 || r.ContentLength > maxBodyBytes {
		return ""
	}
	if contentType := r.Header.Get("Content-Type"); !strings.Contains(contentType, "json") {
		return ""
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return ""
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if int64(len(body)) > maxBodyBytes {
		return ""
	}

	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return ""
	}
	return findJSONID(value)
}

func findJSONID(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if _, ok := jsonCandidates[strings.ToLower(key)]; ok {
				if str, ok := child.(string); ok && strings.TrimSpace(str) != "" {
					return strings.TrimSpace(str)
				}
			}
		}
		for _, child := range typed {
			if id := findJSONID(child); id != "" {
				return id
			}
		}
	case []any:
		for _, child := range typed {
			if id := findJSONID(child); id != "" {
				return id
			}
		}
	}
	return ""
}

func fallbackID(r *http.Request) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(r.RemoteAddr))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(r.UserAgent()))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(r.Method))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(r.URL.Path))
	return "fallback:" + hex.EncodeToString(hash.Sum(nil))[:24]
}

func hasAnyHeader(r *http.Request, headers []string) bool {
	for _, header := range headers {
		if strings.TrimSpace(r.Header.Get(header)) != "" {
			return true
		}
	}
	return false
}

var agentTypePattern = regexp.MustCompile(`^[a-z0-9._-]+$`)
