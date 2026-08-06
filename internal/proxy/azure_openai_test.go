package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/azureopenai"
	"github.com/manaflow-ai/subrouter/session"
)

type azureTokenSourceFunc func(context.Context) (string, error)

func (f azureTokenSourceFunc) Token(ctx context.Context) (string, error) {
	return f(ctx)
}

func TestAzureOpenAIResponsesRouteUsesCLITokenAndProfileEndpoint(t *testing.T) {
	const requestBody = `{"model":"codex-deployment","input":"hello"}`
	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		upstreamHits.Add(1)
		if request.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", request.Method)
		}
		if request.URL.Path != "/openai/v1/responses" {
			t.Errorf("path = %q, want /openai/v1/responses", request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer azure-cli-secret" {
			t.Errorf("Authorization = %q, want Azure CLI bearer token", got)
		}
		for _, header := range []string{"Api-Key", "X-Api-Key", "OpenAI-Organization", "OpenAI-Project", "X-Subrouter-Account-ID"} {
			if got := request.Header.Get(header); got != "" {
				t.Errorf("%s leaked upstream: %q", header, got)
			}
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
		}
		if string(body) != requestBody {
			t.Errorf("body = %s, want %s", body, requestBody)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
	}))
	defer upstream.Close()

	var tokenCalls atomic.Int32
	registry, err := azureopenai.NewRegistryWithTokenFactory([]azureopenai.Profile{{
		Name:       "work",
		Endpoint:   upstream.URL,
		Deployment: "codex-deployment",
		AzureCLI:   "/opt/homebrew/bin/az",
	}}, func(azureopenai.Profile) azureopenai.TokenSource {
		return azureTokenSourceFunc(func(context.Context) (string, error) {
			tokenCalls.Add(1)
			return "azure-cli-secret", nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		AzureOpenAI:  registry,
		Accounts:     registry.Accounts("test"),
		Sessions:     store,
		MaxBodyBytes: 1 << 20,
	}.Handler()

	request := httptest.NewRequest(http.MethodPost, "/azure/work/v1/responses", strings.NewReader(requestBody))
	request.Header.Set("Authorization", "Bearer client-placeholder")
	request.Header.Set("Api-Key", "client-api-key")
	request.Header.Set("X-Api-Key", "client-x-api-key")
	request.Header.Set("OpenAI-Organization", "org-client")
	request.Header.Set("OpenAI-Project", "project-client")
	request.Header.Set("X-Subrouter-Account-ID", "azure:other")
	request.Header.Set("X-Subrouter-Agent", "codex")
	request.Header.Set("X-Subrouter-Session", "azure-test-session")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "response.completed") {
		t.Fatalf("response body = %q", response.Body.String())
	}
	if got := upstreamHits.Load(); got != 1 {
		t.Fatalf("upstream hits = %d, want 1", got)
	}
	if got := tokenCalls.Load(); got != 1 {
		t.Fatalf("token calls = %d, want 1", got)
	}
	assignment, ok := store.Get("codex", "azure-test-session")
	if !ok || assignment.AccountID != azureopenai.AccountID("work") {
		t.Fatalf("session assignment = %#v, found = %t", assignment, ok)
	}
}

func TestAzureOpenAIProfileProbeAndUnknownProfile(t *testing.T) {
	registry, err := azureopenai.NewRegistryWithTokenFactory([]azureopenai.Profile{{
		Name:       "work",
		Endpoint:   "https://example.openai.azure.com",
		Deployment: "codex-deployment",
		AzureCLI:   "/opt/homebrew/bin/az",
	}}, func(azureopenai.Profile) azureopenai.TokenSource {
		return azureTokenSourceFunc(func(context.Context) (string, error) { return "token", nil })
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{AzureOpenAI: registry, Accounts: registry.Accounts("test"), Sessions: store}.Handler()

	for _, test := range []struct {
		path string
		want int
	}{
		{path: "/azure/work/v1", want: http.StatusNoContent},
		{path: "/azure/missing/v1", want: http.StatusNotFound},
	} {
		request := httptest.NewRequest(http.MethodHead, test.path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.want {
			t.Errorf("HEAD %s status = %d, want %d", test.path, response.Code, test.want)
		}
	}
}
