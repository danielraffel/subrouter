package proxy

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestAntigravityRequestsAreReplayable(t *testing.T) {
	for _, path := range []string{"/antigravity/v1internal:generateContent", "/antigravity/v1internal/loadCodeAssist"} {
		r := &http.Request{Method: http.MethodPost, URL: &url.URL{Path: path}}
		if !retryableUpstreamPostRequest(accounts.ProviderAntigravity, r) {
			t.Errorf("retryableUpstreamPostRequest(%q) = false", path)
		}
	}
	for _, path := range []string{"/antigravity/v1/models", "/antigravity/health"} {
		r := &http.Request{Method: http.MethodPost, URL: &url.URL{Path: path}}
		if retryableUpstreamPostRequest(accounts.ProviderAntigravity, r) {
			t.Errorf("retryableUpstreamPostRequest(%q) = true", path)
		}
	}
}

func TestAntigravity429TriggersAccountFailover(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(strings.NewReader(`{"error":{"status":"RESOURCE_EXHAUSTED"}}`)),
	}
	transport := usageLimitRetryTransport{provider: accounts.ProviderAntigravity}
	limited, exhausted, credentialFailure, err := transport.responseUsageLimited(response)
	if err != nil || !limited || !exhausted || credentialFailure {
		t.Fatalf("AGY 429 classification = limited %v exhausted %v credential %v err %v", limited, exhausted, credentialFailure, err)
	}
}

func TestAntigravity401TriggersCredentialFailover(t *testing.T) {
	response := &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(`{"error":"invalid_token"}`))}
	transport := usageLimitRetryTransport{provider: accounts.ProviderAntigravity}
	limited, exhausted, credentialFailure, err := transport.responseUsageLimited(response)
	if err != nil || !limited || !exhausted || !credentialFailure {
		t.Fatalf("AGY 401 classification = limited %v exhausted %v credential %v err %v", limited, exhausted, credentialFailure, err)
	}
}

func TestCodexHeaderless429FailsOverWithoutExhaustingAccount(t *testing.T) {
	response := &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error"}}`))}
	transport := usageLimitRetryTransport{provider: accounts.ProviderCodex}
	limited, exhausted, credentialFailure, err := transport.responseUsageLimited(response)
	if err != nil || !limited || exhausted || credentialFailure {
		t.Fatalf("Codex headerless 429 classification = limited %v exhausted %v credential %v err %v", limited, exhausted, credentialFailure, err)
	}
}
