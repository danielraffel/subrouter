package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// claudeFableEnabled reports whether a Fable-specific route is configured (a
// dedicated Anthropic API key, or the Bedrock gateway).
func (s Server) claudeFableEnabled() bool {
	if s.ClaudeFableAPIKey != "" {
		return true
	}
	return s.Bedrock != nil && s.Bedrock.Credentials != nil
}

// maybeServeClaudeFable intercepts Claude Messages requests for a Fable model
// and serves them off the subscription pool (which Anthropic rate-limits). Fable
// goes to the dedicated Anthropic API key when set, otherwise Bedrock. Any
// non-Fable request restores the body and returns false so the normal pool path
// runs unchanged, so Opus/Sonnet never use the Fable API key.
func (s Server) maybeServeClaudeFable(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/v1/messages") {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, replayablePostMaxBodyBytes))
	if err != nil {
		return false
	}
	restore := func() {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	}
	var probe struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &probe) != nil || !strings.Contains(strings.ToLower(probe.Model), "fable") {
		restore()
		return false
	}
	if s.ClaudeFableAPIKey != "" {
		s.serveClaudeFableViaAPIKey(w, r, body)
		return true
	}
	if s.Bedrock != nil && s.Bedrock.Credentials != nil {
		s.serveClaudeFableViaBedrock(w, r, body)
		return true
	}
	restore()
	return false
}

// serveClaudeFableViaAPIKey forwards a Fable request to Anthropic using the
// dedicated Fable API key (x-api-key). It strips the context_management field
// that the Messages API rejects with "Extra inputs are not permitted", and drops
// the OAuth-only auth so the api key is authoritative. The response is native
// Anthropic (SSE or JSON), so it streams straight back with no transcoding.
func (s Server) serveClaudeFableViaAPIKey(w http.ResponseWriter, r *http.Request, body []byte) {
	body = stripClaudeUnsupportedFields(body)

	upstream := s.ClaudeUpstream
	if upstream == nil {
		upstream = &url.URL{Scheme: "https", Host: "api.anthropic.com"}
	}
	target := *upstream
	target.Path = strings.TrimRight(upstream.Path, "/") + r.URL.Path
	target.RawQuery = r.URL.RawQuery

	outReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, "failed to build fable request", http.StatusInternalServerError)
		return
	}
	for key, values := range r.Header {
		lower := strings.ToLower(key)
		if isHopByHopHeader(key) || lower == "authorization" || lower == "x-api-key" || lower == "host" || lower == "content-length" {
			continue
		}
		for _, value := range values {
			outReq.Header.Add(key, value)
		}
	}
	outReq.Header.Set("X-Api-Key", s.ClaudeFableAPIKey)
	removeCommaHeaderValue(outReq.Header, "Anthropic-Beta", claudeOAuthBetaHeader)
	if outReq.Header.Get("Anthropic-Version") == "" {
		outReq.Header.Set("Anthropic-Version", "2023-06-01")
	}
	outReq.Host = target.Host
	outReq.ContentLength = int64(len(body))

	transport := s.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	resp, err := transport.RoundTrip(outReq)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Error("claude-fable api-key request failed", "error", err)
		}
		http.Error(w, "fable upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		if isHopByHopHeader(key) {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flushingCopy(w, resp.Body, nil)
}

// stripClaudeUnsupportedFields removes request fields the Anthropic Messages API
// rejects for direct (non-OAuth) calls, notably context_management, which Claude
// Code sends but the endpoint refuses with "Extra inputs are not permitted".
func stripClaudeUnsupportedFields(body []byte) []byte {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return body
	}
	changed := false
	for _, field := range []string{"context_management"} {
		if _, ok := payload[field]; ok {
			delete(payload, field)
			changed = true
		}
	}
	if !changed {
		return body
	}
	if rebuilt, err := json.Marshal(payload); err == nil {
		return rebuilt
	}
	return body
}
