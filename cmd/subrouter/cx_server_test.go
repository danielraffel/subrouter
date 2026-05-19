package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
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

func TestCXServerAddStoresAdminTokenForRemoteAdminEndpoints(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()

	var out bytes.Buffer
	runner := cxRunner{store: store, out: &out, errOut: &out}
	err := runner.run(context.Background(), []string{
		"server", "add", "team",
		"--url", "http://100.64.0.1:31415",
		"--admin-token", "secret-token",
	})
	if err != nil {
		t.Fatal(err)
	}

	server, ok, err := defaultCXServerStore(store).find("team")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing configured server")
	}
	if server.AdminToken != "secret-token" {
		t.Fatalf("admin token = %q", server.AdminToken)
	}
}

func TestCXServerStatusSendsAdminToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	server := cxServerConfig{
		Name:       "team",
		URL:        "http://100.64.0.1:31415",
		AdminToken: "secret-token",
	}
	if err := defaultCXServerStore(store).save(cxServerFile{Servers: []cxServerConfig{server}}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := cxRunner{
		store:  store,
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: cxRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if got := req.Header.Get("Authorization"); got != "Bearer secret-token" {
				t.Fatalf("Authorization = %q", got)
			}
			body, _ := json.Marshal([]remoteServerAccount{{ID: "acct", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth}})
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewReader(body)),
			}, nil
		})},
	}
	if err := runner.run(context.Background(), []string{"server", "status", "team"}); err != nil {
		t.Fatal(err)
	}
}

func TestCXServerAddPreservesExistingAdminTokenWhenUpdatingMetadata(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()

	var out bytes.Buffer
	runner := cxRunner{store: store, out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{
		"server", "add", "team",
		"--url", "http://100.64.0.1:31415",
		"--admin-token", "secret-token",
	}); err != nil {
		t.Fatal(err)
	}
	if err := runner.run(context.Background(), []string{
		"server", "add", "team",
		"--url", "http://100.64.0.2:31415",
		"--gcp-instance", "subrouter-team",
		"--gcp-zone", "us-south1-a",
	}); err != nil {
		t.Fatal(err)
	}

	server, ok, err := defaultCXServerStore(store).find("team")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing configured server")
	}
	if server.URL != "http://100.64.0.2:31415" {
		t.Fatalf("url = %q", server.URL)
	}
	if server.AdminToken != "secret-token" {
		t.Fatalf("admin token = %q", server.AdminToken)
	}
}

func TestCXServerAddAllowsURLOnlyServer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()

	var out bytes.Buffer
	runner := cxRunner{store: store, out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{
		"server", "add", "team",
		"--url", "http://100.64.0.1:31415",
		"--default",
	}); err != nil {
		t.Fatal(err)
	}

	file, err := defaultCXServerStore(store).load()
	if err != nil {
		t.Fatal(err)
	}
	if file.Default != "team" {
		t.Fatalf("default = %q, want team", file.Default)
	}
	server, ok := file.find("team")
	if !ok {
		t.Fatal("missing team server")
	}
	if server.GCPInstance != "" || server.GCPZone != "" {
		t.Fatalf("unexpected GCP metadata: %+v", server)
	}
}

func TestCXServerUseSetsExplicitDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()

	var out bytes.Buffer
	runner := cxRunner{store: store, out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{
		"server", "add", "community",
		"--url", "http://100.64.0.1:31415",
		"--gcp-instance", "subrouter-community",
		"--gcp-zone", "us-central1-a",
	}); err != nil {
		t.Fatal(err)
	}
	if err := runner.run(context.Background(), []string{"server", "use", "community"}); err != nil {
		t.Fatal(err)
	}

	file, err := defaultCXServerStore(store).load()
	if err != nil {
		t.Fatal(err)
	}
	if file.Default != "community" {
		t.Fatalf("default = %q, want community", file.Default)
	}
	out.Reset()
	if err := runner.run(context.Background(), []string{"server", "list"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "(default)") {
		t.Fatalf("list did not mark default:\n%s", out.String())
	}
}

func TestCXServerRenameUpdatesDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()

	var out bytes.Buffer
	runner := cxRunner{program: "sr", store: store, out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{
		"server", "add", "community",
		"--url", "http://100.64.0.1:31415",
		"--default",
	}); err != nil {
		t.Fatal(err)
	}
	if err := runner.run(context.Background(), []string{"server", "rename", "community", "team"}); err != nil {
		t.Fatal(err)
	}

	file, err := defaultCXServerStore(store).load()
	if err != nil {
		t.Fatal(err)
	}
	if file.Default != "team" {
		t.Fatalf("default = %q, want team", file.Default)
	}
	if _, ok := file.find("community"); ok {
		t.Fatal("old server name still exists")
	}
	if _, ok := file.find("team"); !ok {
		t.Fatal("new server name missing")
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
	if !fake.hasCommandPrefix("ssh", "-o", "BatchMode=yes") {
		t.Fatalf("missing direct ssh upload/install command: %#v", fake.commands)
	}
	if fake.hasCommandPrefix("gcloud", "compute", "scp") {
		t.Fatalf("unexpected gcloud scp for tailnet server: %#v", fake.commands)
	}
	uploadCommand := strings.Join(fake.commands[len(fake.commands)-1], " ")
	if strings.Contains(uploadCommand, "systemctl restart subrouter") {
		t.Fatalf("upload should hot-reload instead of restarting:\n%s", uploadCommand)
	}
	if !strings.Contains(uploadCommand, "reload_status=$(curl") {
		t.Fatalf("upload should preflight hot-reload support before writing files:\n%s", uploadCommand)
	}
	if !strings.Contains(uploadCommand, "POST http://127.0.0.1:31415/_subrouter/reload-accounts") {
		t.Fatalf("upload should hot-reload accounts:\n%s", uploadCommand)
	}
	if !strings.Contains(uploadCommand, "/var/lib/subrouter/codex/accounts") {
		t.Fatalf("upload should install accounts into subrouter state dir:\n%s", uploadCommand)
	}
	if strings.Contains(uploadCommand, "/var/lib/subrouter/.codex-accounts") {
		t.Fatalf("upload should not use legacy account path:\n%s", uploadCommand)
	}
	if !strings.Contains(out.String(), "server owns the new refresh-token chain") {
		t.Fatalf("missing ownership message:\n%s", out.String())
	}
}

func TestCXServerLoginRejectsUnexpectedEmailWithoutUpload(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()

	oldLocal := testCodexAuth("alice@example.com", "acct_old_local")
	wrongLogin := testCodexAuth("wrong@example.com", "acct_wrong")
	if err := accounts.WriteActiveCodexAuth(oldLocal); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	fake := &recordingCXCommandRunner{loginAuth: wrongLogin}
	runner := cxRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out, cmd: fake}
	server := cxServerConfig{
		Name:        "community",
		URL:         "http://100.64.0.1:31415",
		GCPInstance: "subrouter-community",
		GCPZone:     "us-central1-a",
	}

	err := runner.serverLoginOne(context.Background(), server, true, "alice@example.com")
	if err == nil || !strings.Contains(err.Error(), "expected alice@example.com") {
		t.Fatalf("error = %v", err)
	}
	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok || active.Tokens.RefreshToken != oldLocal.Tokens.RefreshToken {
		t.Fatalf("active auth was not restored")
	}
	if fake.hasCommandPrefix("gcloud") {
		t.Fatalf("unexpected upload command after wrong login: %#v", fake.commands)
	}
}

func TestCXServerSyncUploadsMissingLocalOAuthOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()

	active := testCodexAuth("active@example.com", "acct_active")
	alice := testCodexAuth("alice@example.com", "acct_alice")
	bob := testCodexAuth("bob@example.com", "acct_bob")
	if err := accounts.WriteActiveCodexAuth(active); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		email string
		auth  accounts.CodexAuthFile
	}{
		{"alice@example.com", alice},
		{"bob@example.com", bob},
	} {
		if err := store.SaveStored(accounts.StoredCodexAccount{
			Email:   item.email,
			AddedAt: time.Now().UTC().Format(time.RFC3339),
			Auth:    item.auth,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := store.AddAPIKey("paid", "sk-test-paid"); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	fake := &recordingCXCommandRunner{loginAuths: []accounts.CodexAuthFile{alice}}
	runner := cxRunner{
		store:  store,
		in:     strings.NewReader(""),
		out:    &out,
		errOut: &out,
		cmd:    fake,
		client: &http.Client{Transport: cxRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != "/_subrouter/account-status" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}
			if req.Method != http.MethodPost {
				t.Fatalf("method = %s, want POST", req.Method)
			}
			body, _ := json.Marshal([]remoteServerAccountStatus{
				{ID: "bob@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Email: "bob@example.com", AuthChecked: true, AuthValid: true},
				{ID: "old@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Email: "old@example.com", AuthChecked: true, AuthValid: true},
				{ID: "apikey:paid", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeAPIKey, Email: "apikey:paid"},
			})
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewReader(body)),
			}, nil
		})},
	}
	if err := runner.run(context.Background(), []string{
		"server", "add", "community",
		"--url", "http://100.64.0.1:31415",
		"--gcp-instance", "subrouter-community",
		"--gcp-zone", "us-central1-a",
		"--gcp-project", "example-project",
	}); err != nil {
		t.Fatal(err)
	}

	if err := runner.run(context.Background(), []string{"server", "sync", "community", "--device-auth", "--yes"}); err != nil {
		t.Fatal(err)
	}

	output := out.String()
	for _, want := range []string{
		"Missing on server:\n  alice@example.com",
		"Already on server:\n  bob@example.com",
		"Invalid on server: none",
		"Server-only OAuth accounts:\n  old@example.com",
		"Synced 1 server-owned OAuth account(s) to community",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
	if fake.countCommand("codex", "login", "--device-auth") != 1 {
		t.Fatalf("login command count mismatch: %#v", fake.commands)
	}
	if !fake.hasCommandPrefix("ssh", "-o", "BatchMode=yes") {
		t.Fatalf("missing direct ssh upload/install command: %#v", fake.commands)
	}
	if fake.hasCommandPrefix("gcloud", "compute", "scp") {
		t.Fatalf("unexpected gcloud scp for tailnet server: %#v", fake.commands)
	}
	restored, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok || restored.Tokens.RefreshToken != active.Tokens.RefreshToken {
		t.Fatalf("active auth was not restored")
	}
}

func TestCXServerSyncURLOnlyServerUsesDirectSSHUpload(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	active := testCodexAuth("active@example.com", "acct_active")
	fresh := testCodexAuth("alice@example.com", "acct_alice_fresh")
	if err := accounts.WriteActiveCodexAuth(active); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "alice@example.com",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    testCodexAuth("alice@example.com", "acct_alice"),
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	fake := &recordingCXCommandRunner{loginAuth: fresh}
	runner := cxRunner{
		store:  store,
		out:    &out,
		errOut: &out,
		cmd:    fake,
		client: &http.Client{Transport: cxRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			body, _ := json.Marshal([]remoteServerAccountStatus{})
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewReader(body)),
			}, nil
		})},
	}
	if err := runner.run(context.Background(), []string{
		"server", "add", "team",
		"--url", "http://100.64.0.1:31415",
	}); err != nil {
		t.Fatal(err)
	}

	if err := runner.run(context.Background(), []string{"server", "sync", "team", "--yes"}); err != nil {
		t.Fatal(err)
	}
	if !fake.hasCommandPrefix("ssh", "-o", "BatchMode=yes") {
		t.Fatalf("missing direct ssh upload/install command: %#v", fake.commands)
	}
	if fake.hasCommandPrefix("gcloud") {
		t.Fatalf("URL-only server used gcloud: %#v", fake.commands)
	}
	restored, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok || restored.Tokens.RefreshToken != active.Tokens.RefreshToken {
		t.Fatalf("active auth was not restored")
	}
}

func TestCXServerSyncDryRunDoesNotLogin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "alice@example.com",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    testCodexAuth("alice@example.com", "acct_alice"),
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	fake := &recordingCXCommandRunner{}
	runner := cxRunner{
		store:  store,
		out:    &out,
		errOut: &out,
		cmd:    fake,
		client: &http.Client{Transport: cxRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != "/_subrouter/account-status" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`[]`)),
			}, nil
		})},
	}
	if err := runner.run(context.Background(), []string{
		"server", "add", "community",
		"--url", "http://100.64.0.1:31415",
		"--gcp-instance", "subrouter-community",
		"--gcp-zone", "us-central1-a",
	}); err != nil {
		t.Fatal(err)
	}

	if err := runner.run(context.Background(), []string{"server", "sync", "community", "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	if fake.hasCommandPrefix("codex") || fake.hasCommandPrefix("gcloud") {
		t.Fatalf("dry-run ran commands: %#v", fake.commands)
	}
	if !strings.Contains(out.String(), "Would reauth on server:\n  alice@example.com") {
		t.Fatalf("unexpected output:\n%s", out.String())
	}
}

func TestCXServerSyncPromptsForInvalidServerAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("FORCE_COLOR", "1")
	store := accounts.DefaultCodexStore()
	active := testCodexAuth("active@example.com", "acct_active")
	invalidFresh := testCodexAuth("old@example.com", "acct_old_fresh")
	if err := accounts.WriteActiveCodexAuth(active); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	fake := &recordingCXCommandRunner{loginAuths: []accounts.CodexAuthFile{invalidFresh}}
	runner := cxRunner{
		store:  store,
		in:     strings.NewReader("yes\n"),
		out:    &out,
		errOut: &out,
		cmd:    fake,
		client: &http.Client{Transport: cxRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != "/_subrouter/account-status" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}
			body, _ := json.Marshal([]remoteServerAccountStatus{
				{ID: "old@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Email: "old@example.com", AuthChecked: true, AuthValid: false, Error: "token refresh failed (401): refresh_token_reused"},
			})
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewReader(body)),
			}, nil
		})},
	}
	if err := runner.run(context.Background(), []string{
		"server", "add", "community",
		"--url", "http://100.64.0.1:31415",
		"--gcp-instance", "subrouter-community",
		"--gcp-zone", "us-central1-a",
	}); err != nil {
		t.Fatal(err)
	}

	if err := runner.run(context.Background(), []string{"server", "sync", "community", "--device-auth"}); err != nil {
		t.Fatal(err)
	}
	output := out.String()
	for _, want := range []string{
		"Invalid on server:\n  old@example.com: token refresh failed",
		"Reauth 1 account(s) on server community?",
		"Sign in as " + ansiBold + ansiMagenta + "old@example.com" + ansiReset + " for server community.",
		"Synced 1 server-owned OAuth account(s) to community",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
	if fake.countCommand("codex", "login", "--device-auth") != 1 {
		t.Fatalf("login command count mismatch: %#v", fake.commands)
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
		">/dev/null 2>&1",
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
	loginAuth  accounts.CodexAuthFile
	loginAuths []accounts.CodexAuthFile
	commands   [][]string
}

func (r *recordingCXCommandRunner) Run(_ context.Context, name string, args []string, _ io.Reader, _ io.Writer, _ io.Writer) error {
	command := append([]string{name}, args...)
	r.commands = append(r.commands, command)
	if name == "codex" && len(args) > 0 && args[0] == "login" {
		loginAuth := r.loginAuth
		if len(r.loginAuths) > 0 {
			loginAuth = r.loginAuths[0]
			r.loginAuths = r.loginAuths[1:]
		}
		body, err := jsonMarshalIndent(loginAuth)
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

func (r *recordingCXCommandRunner) countCommand(parts ...string) int {
	count := 0
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
			count++
		}
	}
	return count
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
