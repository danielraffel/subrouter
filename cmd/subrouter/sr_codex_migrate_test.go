package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func saveLegacyCodexAccount(t *testing.T, store accounts.CodexStore, email, accountID, refresh string) {
	t.Helper()
	auth := testCodexAuth(email, accountID)
	auth.Tokens.RefreshToken = refresh
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   email,
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    auth,
	}); err != nil {
		t.Fatal(err)
	}
}

func (r *recordingSRCommandRunner) commandCount(parts ...string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, command := range r.commands {
		if commandHasPrefix(command, parts) {
			count++
		}
	}
	return count
}

func TestCodexMigrateIsolationIsReservedWithoutHijackingCodexLauncher(t *testing.T) {
	for _, test := range []struct {
		args []string
		want bool
	}{
		{[]string{"codex", "migrate-isolation"}, true},
		{[]string{"codex", "migrate-isolation", "--device-auth"}, true},
		{[]string{"codex"}, false},
		{[]string{"codex", "resume", "thread-id"}, false},
		{[]string{"status"}, false},
	} {
		if got := isCodexIsolationCommand(test.args); got != test.want {
			t.Fatalf("isCodexIsolationCommand(%q) = %v, want %v", test.args, got, test.want)
		}
	}
}

func TestCodexMigrateIsolationRemainsAvailableWithMalformedRoutingConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	configPath := t.TempDir() + "/cloud.json"
	t.Setenv("SUBROUTER_CLOUD_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	runner := srRunner{
		store: accounts.DefaultCodexStore(), in: strings.NewReader(""), out: &out, errOut: &out,
	}
	if err := runner.run(context.Background(), []string{"codex", "migrate-isolation"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No migration needed") {
		t.Fatalf("migration command did not run: %q", out.String())
	}
}

func TestCodexMigrateIsolationReenrollsExpectedAccountsAndPreservesActiveAuth(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	active := testCodexAuth("interactive@example.com", "acct-interactive")
	active.Tokens.RefreshToken = "interactive-refresh"
	if err := accounts.WriteActiveCodexAuth(active); err != nil {
		t.Fatal(err)
	}
	activeBefore, err := os.ReadFile(accounts.DefaultCodexAuthPath())
	if err != nil {
		t.Fatal(err)
	}
	saveLegacyCodexAccount(t, store, "alpha@example.com", "acct-alpha", "legacy-alpha")
	saveLegacyCodexAccount(t, store, "beta@example.com", "acct-beta", "legacy-beta")

	alpha := testCodexAuth("alpha@example.com", "acct-alpha")
	alpha.Tokens.RefreshToken = "isolated-alpha"
	beta := testCodexAuth("beta@example.com", "acct-beta")
	beta.Tokens.RefreshToken = "isolated-beta"
	fake := &recordingSRCommandRunner{loginAuths: []accounts.CodexAuthFile{alpha, beta}}
	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out, cmd: fake}
	if err := runner.codexAccount(context.Background(), []string{"migrate-isolation", "--device-auth"}); err != nil {
		t.Fatal(err)
	}

	activeAfter, err := os.ReadFile(accounts.DefaultCodexAuthPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(activeBefore, activeAfter) {
		t.Fatal("migration changed interactive Codex auth")
	}
	for _, want := range []struct{ email, refresh string }{
		{"alpha@example.com", "isolated-alpha"},
		{"beta@example.com", "isolated-beta"},
	} {
		stored, ok, err := store.FindStored(want.email)
		if err != nil || !ok {
			t.Fatalf("find %s: ok=%v err=%v", want.email, ok, err)
		}
		if stored.OAuthCredentialOrigin != accounts.CodexOAuthOriginIsolatedServerLogin {
			t.Fatalf("%s origin = %q", want.email, stored.OAuthCredentialOrigin)
		}
		if stored.Auth.Tokens == nil || stored.Auth.Tokens.RefreshToken != want.refresh {
			t.Fatalf("%s did not receive isolated credential", want.email)
		}
		if len(stored.Breadcrumbs) == 0 || stored.Breadcrumbs[len(stored.Breadcrumbs)-1].Event != "credential_reenrolled_isolated" {
			t.Fatalf("%s missing migration breadcrumb", want.email)
		}
	}
	if got := fake.commandCount("codex", "login", "--device-auth"); got != 2 {
		t.Fatalf("device login count = %d, want 2", got)
	}
	if targets, err := codexIsolationTargets(store); err != nil || len(targets) != 0 {
		t.Fatalf("remaining targets = %d, err=%v", len(targets), err)
	}
	for _, want := range []string{"Found 2", "Migrated 2 Codex OAuth account(s)", "Local Codex auth was left unchanged"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestCodexMigrateIsolationWrongIdentityDoesNotReplaceStoredAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	active := testCodexAuth("interactive@example.com", "acct-interactive")
	if err := accounts.WriteActiveCodexAuth(active); err != nil {
		t.Fatal(err)
	}
	saveLegacyCodexAccount(t, store, "expected@example.com", "acct-expected", "legacy-refresh")

	wrong := testCodexAuth("wrong@example.com", "acct-wrong")
	fake := &recordingSRCommandRunner{loginAuth: wrong}
	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out, cmd: fake}
	err := runner.codexAccount(context.Background(), []string{"migrate-isolation"})
	if err == nil || !strings.Contains(err.Error(), "expected expected@example.com") {
		t.Fatalf("error = %v", err)
	}
	stored, ok, findErr := store.FindStored("expected@example.com")
	if findErr != nil || !ok {
		t.Fatalf("find: ok=%v err=%v", ok, findErr)
	}
	if stored.OAuthCredentialOrigin != "" || stored.Auth.Tokens.RefreshToken != "legacy-refresh" {
		t.Fatal("wrong identity replaced stored credential")
	}
	if !strings.Contains(out.String(), "No stored credential was changed") || !strings.Contains(out.String(), "1 account(s) still need migration") {
		t.Fatalf("missing remaining-account report:\n%s", out.String())
	}
}

func TestCodexMigrateIsolationRejectsLoginThatSharesActiveRefreshChain(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.DefaultCodexStore()
	active := testCodexAuth("expected@example.com", "acct-expected")
	active.Tokens.RefreshToken = "shared-refresh"
	if err := accounts.WriteActiveCodexAuth(active); err != nil {
		t.Fatal(err)
	}
	saveLegacyCodexAccount(t, store, "expected@example.com", "acct-expected", "legacy-refresh")

	fake := &recordingSRCommandRunner{loginAuth: active}
	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out, cmd: fake}
	err := runner.codexAccount(context.Background(), []string{"migrate-isolation"})
	if err == nil || !strings.Contains(err.Error(), "active Codex refresh-token chain") {
		t.Fatalf("error = %v", err)
	}
	stored, ok, findErr := store.FindStored("expected@example.com")
	if findErr != nil || !ok {
		t.Fatalf("find: ok=%v err=%v", ok, findErr)
	}
	if stored.OAuthCredentialOrigin != "" || stored.Auth.Tokens.RefreshToken != "legacy-refresh" {
		t.Fatal("shared active credential replaced stored account")
	}
}

func TestCodexIsolationTargetsIncludesTrustedCredentialSharedWithActiveAuth(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.DefaultCodexStore()
	active := testCodexAuth("shared@example.com", "acct-shared")
	active.Tokens.RefreshToken = "shared-refresh"
	if err := accounts.WriteActiveCodexAuth(active); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:                 "shared@example.com",
		OAuthCredentialOrigin: accounts.CodexOAuthOriginIsolatedServerLogin,
		Auth:                  active,
	}); err != nil {
		t.Fatal(err)
	}
	targets, err := codexIsolationTargets(store)
	if err != nil || len(targets) != 1 {
		t.Fatalf("targets = %d, err=%v", len(targets), err)
	}
}

func TestCodexIsolationSummaryIsAggregateAndNoOpWhenClean(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.DefaultCodexStore()
	saveLegacyCodexAccount(t, store, "private@example.com", "acct-private", "secret-refresh")
	check := codexIsolationDoctorCheck(store)
	if check.status != "fail" || !strings.Contains(check.detail, "1 account(s)") || !strings.Contains(check.detail, codexIsolationRemediation) {
		t.Fatalf("doctor check = %#v", check)
	}
	if strings.Contains(check.detail, "private@example.com") || strings.Contains(check.detail, "secret-refresh") {
		t.Fatalf("doctor leaked account data: %q", check.detail)
	}
	var status bytes.Buffer
	if err := printCodexIsolationStatus(&status, store); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(status.String(), "private@example.com") || !strings.Contains(status.String(), codexIsolationRemediation) {
		t.Fatalf("status summary = %q", status.String())
	}

	stored, _, err := store.FindStored("private@example.com")
	if err != nil {
		t.Fatal(err)
	}
	stored.OAuthCredentialOrigin = accounts.CodexOAuthOriginIsolatedServerLogin
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"codex", "migrate-isolation"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No migration needed") {
		t.Fatalf("no-op output = %q", out.String())
	}
}

func TestDoctorReportsAggregateCodexIsolationRemediation(t *testing.T) {
	isolateCloudConfig(t)
	local := healthServer(t, 200)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL+"/v1")
	t.Setenv("HOME", t.TempDir())
	store := accounts.DefaultCodexStore()
	saveLegacyCodexAccount(t, store, "private@example.com", "acct-private", "secret-refresh")

	var out bytes.Buffer
	err := runDoctorWith(context.Background(), &fakeController{present: true}, nil, store, &out)
	if err == nil {
		t.Fatal("doctor accepted unisolated Codex account")
	}
	got := out.String()
	if !strings.Contains(got, "FAIL  Codex isolation") || !strings.Contains(got, codexIsolationRemediation) {
		t.Fatalf("doctor output missing aggregate remediation:\n%s", got)
	}
	if strings.Contains(got, "private@example.com") || strings.Contains(got, "secret-refresh") {
		t.Fatalf("doctor leaked account data:\n%s", got)
	}
}

func TestLocalCodexStoreServingDoesNotAssumeUnconfiguredSelectedRemoteIsLocal(t *testing.T) {
	isolateCloudConfig(t)
	t.Setenv("HOME", t.TempDir())
	store := accounts.DefaultCodexStore()
	serverStore := defaultSRServerStore(store)
	if err := serverStore.update(func(file *srServerFile) error {
		file.Default = "remote"
		file.Servers = []srServerConfig{{Name: "remote", URL: "https://remote.example.test"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if localCodexStoreServesLegacy(store) {
		t.Fatal("selected remote server was treated as the local credential store")
	}

	if err := serverStore.update(func(file *srServerFile) error {
		file.Default = "local-loopback"
		file.Servers = []srServerConfig{{Name: "local-loopback", URL: localBaseURL()}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !localCodexStoreServesLegacy(store) {
		t.Fatal("selected loopback server was not treated as the local credential store")
	}
}
