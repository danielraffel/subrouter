package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexArgsInjectsSubrouterBaseURLAsGlobalConfig(t *testing.T) {
	got := codexArgs([]string{"exec", "--cd", "/tmp", "prompt"}, "http://127.0.0.1:31415/v1", "", "")
	want := []string{"exec", "-c", `openai_base_url="http://127.0.0.1:31415/v1"`, "--cd", "/tmp", "prompt"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestDefaultCodexBaseURLUsesConfiguredServerWhenLocalHealthFails(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "old gateway", http.StatusForbidden)
	}))
	t.Cleanup(local.Close)
	store := cxServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	if err := store.save(cxServerFile{Servers: []cxServerConfig{{
		Name: "community",
		URL:  "http://100.99.8.37:31415",
	}}}); err != nil {
		t.Fatal(err)
	}

	got := defaultCodexBaseURLFor(local.URL+"/_subrouter/health", store, local.Client())
	if got != "http://100.99.8.37:31415/v1" {
		t.Fatalf("base URL = %q", got)
	}
}

func TestDefaultCodexBaseURLKeepsLocalWhenLocalHealthWorks(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(local.Close)
	store := cxServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	if err := store.save(cxServerFile{Servers: []cxServerConfig{{
		Name: "community",
		URL:  "http://100.99.8.37:31415",
	}}}); err != nil {
		t.Fatal(err)
	}

	got := defaultCodexBaseURLFor(local.URL+"/_subrouter/health", store, local.Client())
	if got != defaultCodexBaseURL {
		t.Fatalf("base URL = %q, want %q", got, defaultCodexBaseURL)
	}
}

func TestDefaultCodexBaseURLDoesNotGuessWhenMultipleServersAreConfigured(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "old gateway", http.StatusForbidden)
	}))
	t.Cleanup(local.Close)
	store := cxServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	if err := store.save(cxServerFile{Servers: []cxServerConfig{
		{Name: "community", URL: "http://100.99.8.37:31415"},
		{Name: "other", URL: "http://100.99.8.38:31415"},
	}}); err != nil {
		t.Fatal(err)
	}

	got := defaultCodexBaseURLFor(local.URL+"/_subrouter/health", store, local.Client())
	if got != defaultCodexBaseURL {
		t.Fatalf("base URL = %q, want %q", got, defaultCodexBaseURL)
	}
}

func TestCodexArgsWorksWithoutSubcommand(t *testing.T) {
	got := codexArgs(nil, "http://127.0.0.1:31415/v1", "", "")
	want := []string{"-c", `openai_base_url="http://127.0.0.1:31415/v1"`}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestCodexArgsPassesThroughCodexFlags(t *testing.T) {
	got := codexArgs([]string{"--version"}, "http://127.0.0.1:31415/v1", "", "")
	want := []string{"-c", `openai_base_url="http://127.0.0.1:31415/v1"`, "--version"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestCodexArgsDoesNotInjectIntoUtilitySubcommands(t *testing.T) {
	got := codexArgs([]string{"login", "--help"}, "http://127.0.0.1:31415/v1", "", "")
	want := []string{"login", "--help"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestCodexArgsInjectsSubrouterBaseURLIntoAppServer(t *testing.T) {
	got := codexArgs([]string{"app-server", "--listen", "off"}, "http://127.0.0.1:31415/v1", "", "")
	want := []string{"app-server", "-c", `openai_base_url="http://127.0.0.1:31415/v1"`, "--listen", "off"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestCodexArgsTreatsUnknownCommandAsInteractivePrompt(t *testing.T) {
	got := codexArgs([]string{"write", "tests"}, "http://127.0.0.1:31415/v1", "", "")
	want := []string{"-c", `openai_base_url="http://127.0.0.1:31415/v1"`, "write", "tests"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestCodexArgsInjectsUserEmailWithCustomSubrouterProvider(t *testing.T) {
	got := codexArgs([]string{"exec", "prompt"}, "http://127.0.0.1:31415/v1", "alice@example.com", "")
	want := []string{
		"exec",
		"-c", `model_provider="subrouter"`,
		"-c", `model_providers.subrouter.name="Subrouter"`,
		"-c", `model_providers.subrouter.base_url="http://127.0.0.1:31415/v1"`,
		"-c", `model_providers.subrouter.env_key="SUBROUTER_CODEX_DUMMY_API_KEY"`,
		"-c", `model_providers.subrouter.wire_api="responses"`,
		"-c", `model_providers.subrouter.supports_websockets=true`,
		"-c", `model_providers.subrouter.http_headers={"X-Subrouter-Agent"="codex","X-Subrouter-User-Email"="alice@example.com"}`,
		"prompt",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestCodexArgsInjectsAccountIDWithCustomSubrouterProvider(t *testing.T) {
	got := codexArgs([]string{"exec", "prompt"}, "http://127.0.0.1:31415/v1", "", "team-codex-1")
	want := []string{
		"exec",
		"-c", `model_provider="subrouter"`,
		"-c", `model_providers.subrouter.name="Subrouter"`,
		"-c", `model_providers.subrouter.base_url="http://127.0.0.1:31415/v1"`,
		"-c", `model_providers.subrouter.env_key="SUBROUTER_CODEX_DUMMY_API_KEY"`,
		"-c", `model_providers.subrouter.wire_api="responses"`,
		"-c", `model_providers.subrouter.supports_websockets=true`,
		"-c", `model_providers.subrouter.http_headers={"X-Subrouter-Agent"="codex","X-Subrouter-Account-ID"="team-codex-1"}`,
		"prompt",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestCodexArgsInjectsUserEmailAndAccountID(t *testing.T) {
	got := codexArgs([]string{"exec", "prompt"}, "http://127.0.0.1:31415/v1", "alice@example.com", "apikey:paid")
	headers := `model_providers.subrouter.http_headers={"X-Subrouter-Agent"="codex","X-Subrouter-User-Email"="alice@example.com","X-Subrouter-Account-ID"="apikey:paid"}`
	if !contains(got, headers) {
		t.Fatalf("args = %#v, want headers %q", got, headers)
	}
}

func TestUpsertEnvReplacesExistingValue(t *testing.T) {
	got := upsertEnv([]string{"A=1", "SUBROUTER_CODEX_DUMMY_API_KEY=old"}, "SUBROUTER_CODEX_DUMMY_API_KEY", "subrouter")
	want := []string{"A=1", "SUBROUTER_CODEX_DUMMY_API_KEY=subrouter"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("env = %#v, want %#v", got, want)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
