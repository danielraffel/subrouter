package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateTopLevelTomlStringsPreservesTables(t *testing.T) {
	input := strings.Join([]string{
		`model = "gpt-5"`,
		`openai_base_url = "http://old.example/v1"`,
		``,
		`[profiles.work]`,
		`openai_base_url = "http://profile.example/v1"`,
		``,
	}, "\n")
	got := updateTopLevelTomlStrings(input, map[string]string{
		"openai_base_url":                   "http://team.example/v1",
		"chatgpt_base_url":                  "http://team.example/backend-api",
		"experimental_realtime_ws_base_url": "http://team.example/v1",
	})

	for _, want := range []string{
		`openai_base_url = "http://team.example/v1"`,
		`chatgpt_base_url = "http://team.example/backend-api"`,
		`experimental_realtime_ws_base_url = "http://team.example/v1"`,
		`[profiles.work]`,
		`openai_base_url = "http://profile.example/v1"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Index(got, `experimental_realtime_ws_base_url`) > strings.Index(got, `[profiles.work]`) {
		t.Fatalf("routing keys were inserted inside a table:\n%s", got)
	}
}

func TestUpdateTopLevelTomlStringsSeparatesMissingKeysAfterUnterminatedLine(t *testing.T) {
	got := updateTopLevelTomlStrings(`model = "gpt-5"`, map[string]string{
		"openai_base_url": "http://team.example/v1",
	})
	want := "model = \"gpt-5\"\nopenai_base_url = \"http://team.example/v1\"\n"
	if got != want {
		t.Fatalf("config = %q, want %q", got, want)
	}
}

func TestWriteCodexConfigValuesCreatesBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("model = \"gpt-5\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeCodexConfigValues(path, map[string]string{
		"openai_base_url": "http://team.example/v1",
	}); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != "model = \"gpt-5\"\n" {
		t.Fatalf("backup = %q", string(backup))
	}
}

func TestWriteCodexConfigForBaseURLDisablesWebsockets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	path := filepath.Join(home, "config.toml")
	if err := os.WriteFile(path, []byte("model = \"gpt-5.5\"\n\n[projects.\"/\"]\ntrust_level = \"trusted\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := writeCodexConfigForBaseURL("http://100.64.0.1:31415")
	if err != nil {
		t.Fatal(err)
	}
	if got != path {
		t.Fatalf("path = %q, want %q", got, path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`model_provider = "subrouter"`,
		`openai_base_url = "http://100.64.0.1:31415/v1"`,
		`chatgpt_base_url = "http://100.64.0.1:31415/backend-api"`,
		"[model_providers.subrouter]",
		`name = "OpenAI"`,
		`base_url = "http://100.64.0.1:31415/v1"`,
		"supports_websockets = false",
		`requires_openai_auth = true`,
		// pre-existing tables are preserved
		`[projects."/"]`,
		`trust_level = "trusted"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("missing %q in config:\n%s", want, string(body))
		}
	}
	if strings.Count(string(body), "[model_providers.subrouter]") != 1 {
		t.Fatalf("expected exactly one subrouter provider table:\n%s", string(body))
	}

	// Running again is a no-op: idempotent regeneration must not duplicate the
	// table or rewrite the file.
	if _, err := writeCodexConfigForBaseURL("http://100.64.0.1:31415"); err != nil {
		t.Fatal(err)
	}
	body2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body2) != string(body) {
		t.Fatalf("second write changed config:\nfirst:\n%s\nsecond:\n%s", string(body), string(body2))
	}
	if strings.Count(string(body2), "[model_providers.subrouter]") != 1 {
		t.Fatalf("idempotent write duplicated provider table:\n%s", string(body2))
	}
}

func TestEnsureCodexWebsocketDisabledProviderReplacesStaleBlock(t *testing.T) {
	input := strings.Join([]string{
		`model = "gpt-5.5"`,
		``,
		`[model_providers.subrouter]`,
		`name = "OpenAI"`,
		`base_url = "http://old.example/v1"`,
		`supports_websockets = false`,
		``,
		`[projects."/"]`,
		`trust_level = "trusted"`,
		``,
	}, "\n")

	got := ensureCodexWebsocketDisabledProvider(input, "http://new.example/v1")

	if strings.Contains(got, "http://old.example/v1") {
		t.Fatalf("stale base_url not replaced:\n%s", got)
	}
	if !strings.Contains(got, `base_url = "http://new.example/v1"`) {
		t.Fatalf("new base_url missing:\n%s", got)
	}
	if strings.Count(got, "[model_providers.subrouter]") != 1 {
		t.Fatalf("expected one provider table:\n%s", got)
	}
	for _, want := range []string{`[projects."/"]`, `trust_level = "trusted"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing preserved table content %q:\n%s", want, got)
		}
	}
}
