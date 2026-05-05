package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestRunAcceptsDirectCXCommands(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := run([]string{"list"}); err != nil {
		t.Fatal(err)
	}
}

func TestSRKeepsSubrouterCommands(t *testing.T) {
	if err := runForProgram("sr", []string{"--help"}); err != nil {
		t.Fatal(err)
	}
}

func TestSRDefaultRunsAccountPicker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(home, ".pi", "agent"))

	store := accounts.DefaultCodexStore()
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "apikey:paid",
		AddedAt: "2026-05-04T00:00:00Z",
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "sk-test",
		},
	}); err != nil {
		t.Fatal(err)
	}

	if err := runForProgram("sr", nil); err != nil {
		t.Fatal(err)
	}
}

func TestDirectCXCommandNames(t *testing.T) {
	for _, command := range []string{
		"add",
		"add-admin-key",
		"add-api-key",
		"add-key",
		"admin-keys",
		"attach-project",
		"claude",
		"gemini",
		"gui-switch",
		"gui-use",
		"import",
		"list",
		"list-admin-keys",
		"login",
		"ls",
		"pick",
		"remove",
		"remove-admin-key",
		"rm",
		"server",
		"servers",
		"status",
		"switch",
		"usage",
		"use",
	} {
		if !isDirectCXCommand(command) {
			t.Fatalf("%s should be a direct cx command", command)
		}
	}
	for _, command := range []string{"serve", "codex", "install-daemon", "install-systemd"} {
		if isDirectCXCommand(command) {
			t.Fatalf("%s should stay a subrouter command", command)
		}
	}
}

func TestUsageShowsAccountCommandsAtTopLevel(t *testing.T) {
	got := usageText("sr")
	for _, want := range []string{
		"sr add",
		"sr add-key",
		"sr switch [email]",
		"sr pick",
		"sr server",
		"sr add-admin-key",
		"sr claude",
		"sr serve",
		"sr install-systemd",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("usage missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "sr cx [cx args...]") {
		t.Fatalf("usage should not present cx as the primary nested command:\n%s", got)
	}
}
