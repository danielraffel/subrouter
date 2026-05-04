package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestCXServerAddStoresGCPServer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()

	var out bytes.Buffer
	runner := cxRunner{store: store, out: &out, errOut: &out}
	err := runner.run(context.Background(), []string{
		"server", "add", "community",
		"--url", "http://100.64.0.1:31415",
		"--gcp-instance", "subrouter-community",
		"--gcp-zone", "us-central1-a",
		"--gcp-project", "example-project",
	})
	if err != nil {
		t.Fatal(err)
	}

	server, ok, err := defaultCXServerStore(store).find("community")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing configured server")
	}
	if server.URL != "http://100.64.0.1:31415" || server.GCPInstance != "subrouter-community" || server.GCPZone != "us-central1-a" || server.GCPProject != "example-project" {
		t.Fatalf("unexpected server config: %+v", server)
	}
}

func TestCXServerLoginUploadsFreshAuthAndRestoresLocalChain(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()

	oldLocal := testCodexAuth("bob@example.com", "acct_old_local")
	freshServer := testCodexAuth("bob@example.com", "acct_fresh_server")
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "bob@example.com",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    oldLocal,
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.WriteActiveCodexAuth(oldLocal); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	fake := &recordingCXCommandRunner{loginAuth: freshServer}
	runner := cxRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out, cmd: fake}
	if err := runner.run(context.Background(), []string{
		"server", "add", "community",
		"--url", "http://100.64.0.1:31415",
		"--gcp-instance", "subrouter-community",
		"--gcp-zone", "us-central1-a",
		"--gcp-project", "example-project",
	}); err != nil {
		t.Fatal(err)
	}

	if err := runner.run(context.Background(), []string{"server", "login", "community", "--device-auth"}); err != nil {
		t.Fatal(err)
	}

	stored, ok, err := store.FindStored("bob@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("local account should have been restored")
	}
	if stored.Auth.Tokens.RefreshToken != oldLocal.Tokens.RefreshToken {
		t.Fatalf("local refresh token = %q, want old chain", stored.Auth.Tokens.RefreshToken)
	}
	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok || active.Tokens.RefreshToken != oldLocal.Tokens.RefreshToken {
		t.Fatalf("active auth was not restored")
	}
	if !fake.hasCommand("codex", "login", "--device-auth") {
		t.Fatalf("missing device-auth login command: %#v", fake.commands)
	}
	if !fake.hasCommandPrefix("gcloud", "compute", "scp") || !fake.hasCommandPrefix("gcloud", "compute", "ssh") {
		t.Fatalf("missing gcloud upload/install commands: %#v", fake.commands)
	}
	if !strings.Contains(out.String(), "server owns the new refresh-token chain") {
		t.Fatalf("missing ownership message:\n%s", out.String())
	}
}

func TestCXServerInstallUsesPublicInstallerAndSystemdCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TAILSCALE_AUTH_KEY", "tailscale-auth-test-secret")
	store := accounts.DefaultCodexStore()

	var out bytes.Buffer
	fake := &recordingCXCommandRunner{}
	runner := cxRunner{store: store, out: &out, errOut: &out, cmd: fake}
	if err := runner.run(context.Background(), []string{
		"server", "add", "community",
		"--url", "http://100.64.0.1:31415",
		"--gcp-instance", "subrouter-community",
		"--gcp-zone", "us-central1-a",
		"--gcp-project", "example-project",
	}); err != nil {
		t.Fatal(err)
	}

	if err := runner.run(context.Background(), []string{"server", "install", "community", "--version", "0.1.2"}); err != nil {
		t.Fatal(err)
	}

	if !fake.hasCommandPrefix("gcloud", "compute", "ssh", "subrouter-community") {
		t.Fatalf("missing gcloud ssh install command: %#v", fake.commands)
	}
	joined := strings.Join(fake.commands[0], " ")
	if strings.Contains(joined, "tailscale-auth-test-secret") {
		t.Fatalf("tailscale auth key leaked into command: %s", joined)
	}
	installCommand := strings.Join(fake.commands[len(fake.commands)-1], " ")
	for _, want := range []string{
		publicInstallScriptURL,
		"SUBROUTER_VERSION='0.1.2'",
		"/usr/local/bin/sr install-systemd",
		"until curl -fsS http://127.0.0.1:31415/_subrouter/health",
		"tailscale up",
	} {
		if !strings.Contains(installCommand, want) {
			t.Fatalf("install command missing %q:\n%s", want, installCommand)
		}
	}
	if !strings.Contains(out.String(), "Installed Subrouter server: community") {
		t.Fatalf("missing install message:\n%s", out.String())
	}
}

type recordingCXCommandRunner struct {
	loginAuth accounts.CodexAuthFile
	commands  [][]string
}

func (r *recordingCXCommandRunner) Run(_ context.Context, name string, args []string, _ io.Reader, _ io.Writer, _ io.Writer) error {
	command := append([]string{name}, args...)
	r.commands = append(r.commands, command)
	if name == "codex" && len(args) > 0 && args[0] == "login" {
		body, err := jsonMarshalIndent(r.loginAuth)
		if err != nil {
			return err
		}
		path := accounts.DefaultCodexAuthPath()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		return os.WriteFile(path, body, 0o600)
	}
	return nil
}

func (r *recordingCXCommandRunner) Output(context.Context, string, []string) ([]byte, error) {
	return nil, nil
}

func (r *recordingCXCommandRunner) hasCommand(parts ...string) bool {
	for _, command := range r.commands {
		if len(command) != len(parts) {
			continue
		}
		matches := true
		for i := range parts {
			if command[i] != parts[i] {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

func (r *recordingCXCommandRunner) hasCommandPrefix(parts ...string) bool {
	for _, command := range r.commands {
		if len(command) < len(parts) {
			continue
		}
		matches := true
		for i := range parts {
			if command[i] != parts[i] {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

func jsonMarshalIndent(value any) ([]byte, error) {
	return json.MarshalIndent(value, "", "  ")
}
