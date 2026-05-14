package transcript

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Recorder struct {
	dir string
	mu  sync.Mutex
}

type Event struct {
	Timestamp string         `json:"timestamp"`
	Type      string         `json:"type"`
	Payload   map[string]any `json:"payload"`
}

func NewRecorder(dir string) *Recorder {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	return &Recorder{dir: dir}
}

func (r *Recorder) Enabled() bool {
	return r != nil && r.dir != ""
}

func (r *Recorder) RecordMeta(agentType, sessionID string, payload map[string]any) {
	if !r.Enabled() {
		return
	}
	r.write(agentType, sessionID, Event{
		Timestamp: now(),
		Type:      "subrouter_meta",
		Payload:   withSession(agentType, sessionID, payload),
	})
}

func (r *Recorder) RecordPayload(agentType, sessionID, eventType, direction string, body []byte, payload map[string]any) {
	if !r.Enabled() {
		return
	}
	sum := sha256.Sum256(body)
	enriched := withSession(agentType, sessionID, payload)
	enriched["direction"] = direction
	enriched["bytes"] = len(body)
	enriched["sha256"] = hex.EncodeToString(sum[:])
	enriched["body_base64"] = base64.StdEncoding.EncodeToString(body)
	r.write(agentType, sessionID, Event{
		Timestamp: now(),
		Type:      eventType,
		Payload:   enriched,
	})
}

func (r *Recorder) RecordPayloadChunk(agentType, sessionID, eventType, direction, streamID string, chunkIndex int, offset int64, body []byte, payload map[string]any) {
	if !r.Enabled() {
		return
	}
	sum := sha256.Sum256(body)
	enriched := withSession(agentType, sessionID, payload)
	enriched["direction"] = direction
	enriched["stream_id"] = streamID
	enriched["body_chunk"] = true
	enriched["chunk_index"] = chunkIndex
	enriched["offset"] = offset
	enriched["chunk_bytes"] = len(body)
	enriched["chunk_sha256"] = hex.EncodeToString(sum[:])
	enriched["body_base64"] = base64.StdEncoding.EncodeToString(body)
	r.write(agentType, sessionID, Event{
		Timestamp: now(),
		Type:      eventType + "_chunk",
		Payload:   enriched,
	})
}

func (r *Recorder) RecordPayloadSummary(agentType, sessionID, eventType, direction, streamID string, bytesRead int64, sha256Hex string, chunks int, payload map[string]any) {
	if !r.Enabled() {
		return
	}
	enriched := withSession(agentType, sessionID, payload)
	enriched["direction"] = direction
	enriched["stream_id"] = streamID
	enriched["body_chunked"] = true
	enriched["bytes"] = bytesRead
	enriched["sha256"] = sha256Hex
	enriched["chunks"] = chunks
	r.write(agentType, sessionID, Event{
		Timestamp: now(),
		Type:      eventType,
		Payload:   enriched,
	})
}

func (r *Recorder) PathForSession(agentType, sessionID string) string {
	agent := safeFilename(normalizeAgentType(agentType))
	base := BaseSessionID(sessionID)
	return filepath.Join(r.dir, "by-agent", agent, "by-session", safeFilename(base)+".jsonl")
}

func BaseSessionID(sessionID string) string {
	if before, _, ok := strings.Cut(sessionID, ":"); ok {
		return before
	}
	return sessionID
}

func RedactedHeaders(headers http.Header) map[string][]string {
	out := make(map[string][]string, len(headers))
	for key, values := range headers {
		if isSensitiveHeader(key) {
			out[key] = []string{"<redacted>"}
			continue
		}
		out[key] = append([]string(nil), values...)
	}
	return out
}

func (r *Recorder) write(agentType, sessionID string, event Event) {
	body, err := json.Marshal(event)
	if err != nil {
		return
	}
	path := r.PathForSession(agentType, sessionID)

	r.mu.Lock()
	defer r.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(body, '\n'))
}

func withSession(agentType, sessionID string, payload map[string]any) map[string]any {
	out := make(map[string]any, len(payload)+4)
	for key, value := range payload {
		out[key] = value
	}
	normalizedAgent := normalizeAgentType(agentType)
	agentSessionID := BaseSessionID(sessionID)
	out["agent_type"] = normalizedAgent
	out["session_id"] = sessionID
	out["agent_session_id"] = agentSessionID
	if normalizedAgent == "codex" {
		out["codex_session_id"] = agentSessionID
	}
	return out
}

func normalizeAgentType(agentType string) string {
	normalized := strings.ToLower(strings.TrimSpace(agentType))
	if normalized == "" {
		return "codex"
	}
	return normalized
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func isSensitiveHeader(key string) bool {
	switch strings.ToLower(key) {
	case "authorization", "cookie", "set-cookie", "proxy-authorization", "x-api-key", "openai-api-key":
		return true
	default:
		return false
	}
}

var unsafeFilenameChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeFilename(value string) string {
	if value == "" {
		return "unknown"
	}
	safe := unsafeFilenameChars.ReplaceAllString(value, "_")
	trimmed := strings.Trim(safe, "._-")
	if trimmed == "" {
		return "unknown"
	}
	return trimmed
}
