package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStripClaudeUnsupportedFields(t *testing.T) {
	in := []byte(`{"model":"claude-fable-5","context_management":{"edits":[]},"max_tokens":8,"messages":[]}`)
	out := stripClaudeUnsupportedFields(in)
	if strings.Contains(string(out), "context_management") {
		t.Fatalf("context_management not stripped: %s", out)
	}
	if !strings.Contains(string(out), "claude-fable-5") || !strings.Contains(string(out), "max_tokens") {
		t.Fatalf("other fields lost: %s", out)
	}
	// No context_management => unchanged.
	clean := []byte(`{"model":"claude-fable-5"}`)
	if string(stripClaudeUnsupportedFields(clean)) != string(clean) {
		t.Fatalf("clean body altered")
	}
}

func TestServeClaudeFableViaAPIKey(t *testing.T) {
	var captured *http.Request
	var capturedBody string
	rt := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		captured = req
		b, _ := io.ReadAll(req.Body)
		capturedBody = string(b)
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(`{"type":"message","usage":{"output_tokens":3}}`)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	})
	s := Server{ClaudeFableAPIKey: "sk-ant-fable-key", Transport: rt}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-fable-5","context_management":{"edits":[]},"max_tokens":8,"messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer oauth-should-be-dropped")
	rec := httptest.NewRecorder()

	if !s.maybeServeClaudeFable(rec, req) {
		t.Fatal("expected fable request to be handled")
	}
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if captured == nil {
		t.Fatal("no upstream request captured")
	}
	if captured.URL.String() != "https://api.anthropic.com/v1/messages" {
		t.Fatalf("upstream URL = %q", captured.URL.String())
	}
	if captured.Header.Get("X-Api-Key") != "sk-ant-fable-key" {
		t.Fatalf("x-api-key = %q, want the fable key", captured.Header.Get("X-Api-Key"))
	}
	if captured.Header.Get("Authorization") != "" {
		t.Fatalf("Authorization should be dropped, got %q", captured.Header.Get("Authorization"))
	}
	if strings.Contains(capturedBody, "context_management") {
		t.Fatalf("context_management not stripped from forwarded body: %s", capturedBody)
	}
}

func TestMaybeServeClaudeFableIgnoresNonFable(t *testing.T) {
	s := Server{ClaudeFableAPIKey: "sk-ant-fable-key"}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-4-8","messages":[]}`))
	rec := httptest.NewRecorder()
	if s.maybeServeClaudeFable(rec, req) {
		t.Fatal("opus must NOT be handled by the fable api-key path")
	}
	// body must be restored for the normal path
	b, _ := io.ReadAll(req.Body)
	if !strings.Contains(string(b), "claude-opus-4-8") {
		t.Fatalf("request body not restored for non-fable: %q", b)
	}
}
