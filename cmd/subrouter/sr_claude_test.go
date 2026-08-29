package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/agents/claude"
)

func TestClaudeEnvPrintsActiveProfile(t *testing.T) {
	store := claude.Store{Dir: t.TempDir()}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := claudeRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"env"}); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if !strings.Contains(got, "export CLAUDE_CONFIG_DIR=") {
		t.Fatalf("env output = %q", got)
	}
	if !strings.Contains(got, "/claude/work") {
		t.Fatalf("env output missing profile path: %q", got)
	}
}

func TestClaudeEnvPrefersCodexAccountsAlias(t *testing.T) {
	home := t.TempDir()
	store := claude.Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(store.Dir, filepath.Join(home, ".codex-accounts")); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := claudeRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"env"}); err != nil {
		t.Fatal(err)
	}

	want := "export CLAUDE_CONFIG_DIR=" + filepath.Join(home, ".codex-accounts", "claude", "work")
	if got := strings.TrimSpace(out.String()); got != want {
		t.Fatalf("env output = %q, want %q", got, want)
	}
}

func TestClaudeSwitchSupportsPartialProfile(t *testing.T) {
	store := claude.Store{Dir: t.TempDir()}
	if _, err := store.CreateProfile("personal"); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := claudeRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"switch", "pers"}); err != nil {
		t.Fatal(err)
	}

	if active := store.ActiveProfile(); active != "personal" {
		t.Fatalf("active = %q, want personal", active)
	}
	if !strings.Contains(out.String(), "Active Claude profile: personal") {
		t.Fatalf("switch output = %q", out.String())
	}
}

func TestClaudeSwitchProtectedPlaintextPrintsWrappedLaunch(t *testing.T) {
	store := claude.Store{Dir: t.TempDir()}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}
	configDir := store.ClaudeConfigDir("work")
	settings := `{"env":{"ANTHROPIC_BASE_URL":"` + managedClaudeBlockedBaseURL + `","ANTHROPIC_AUTH_TOKEN":"` + testTenantKey + `","` + managedClaudeServerURLEnv + `":"http://m3.example","` + managedClaudeTailscaleNodeEnv + `":"node-m3"}}`
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	runner := claudeRunner{store: store, out: &out, errOut: &out}
	if err := runner.switchProfile("work"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "sr claude run work [claude args...]") {
		t.Fatalf("switch output omitted safe launch: %q", got)
	}
	if strings.Contains(got, "export CLAUDE_CONFIG_DIR") || strings.Contains(got, "sr claude env") {
		t.Fatalf("switch output advertised unsafe plain-Claude launch: %q", got)
	}
	out.Reset()
	legacy := `{"env":{"ANTHROPIC_BASE_URL":"http://legacy.example:31415","ANTHROPIC_AUTH_TOKEN":"subrouter"}}`
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runner.switchProfile("work"); err != nil {
		t.Fatal(err)
	}
	got = out.String()
	if !strings.Contains(got, "sr claude push work") || !strings.Contains(got, "sr claude run work") {
		t.Fatalf("legacy switch output omitted migration guidance: %q", got)
	}
	if strings.Contains(got, "export CLAUDE_CONFIG_DIR") {
		t.Fatalf("legacy switch output advertised unsafe plain-Claude launch: %q", got)
	}
}

func TestClaudeFlagsRunActiveProfile(t *testing.T) {
	home := t.TempDir()
	store := claude.Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}

	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(home, "claude-run.txt")
	claudePath := filepath.Join(binDir, "claude")
	script := "#!/bin/sh\n{ printf 'config=%s\\nargs=%s\\n' \"$CLAUDE_CONFIG_DIR\" \"$*\"; env | grep -E '^(ANTHROPIC_BASE_URL|ANTHROPIC_AUTH_TOKEN|CLAUDE_CODE_OAUTH_TOKEN)=' || true; } > " + shellQuote(recordPath) + "\n"
	if err := os.WriteFile(claudePath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ANTHROPIC_BASE_URL", "http://subrouter-team:31415")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "subrouter")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "direct-token")
	t.Setenv("CLAUDE_CONFIG_DIR", "/old/config")

	var out bytes.Buffer
	runner := claudeRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	err := runner.run(context.Background(), []string{"--dangerously-skip-permissions", "--resume", "1721c0ce-b3bd-4d73-8b33-b3d02b677074"})
	if err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, "config="+store.ClaudeConfigDir("work")) {
		t.Fatalf("Claude did not receive active config dir:\n%s", got)
	}
	if !strings.Contains(got, "args=--dangerously-skip-permissions --resume 1721c0ce-b3bd-4d73-8b33-b3d02b677074") {
		t.Fatalf("Claude did not receive flags:\n%s", got)
	}
	for _, needle := range []string{"ANTHROPIC_BASE_URL=", "ANTHROPIC_AUTH_TOKEN=", "CLAUDE_CODE_OAUTH_TOKEN="} {
		if strings.Contains(got, needle) {
			t.Fatalf("Claude inherited %s env:\n%s", needle, got)
		}
	}
}

func TestClaudeManagedProfileRejectsLegacyRemoteHTTPTenantBeforeLaunch(t *testing.T) {
	home := t.TempDir()
	store := claude.Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	if _, err := store.CreateProfile("legacy"); err != nil {
		t.Fatal(err)
	}
	configDir := store.ClaudeConfigDir("legacy")
	settings := `{"env":{"ANTHROPIC_BASE_URL":"http://192.168.1.10:31415/t/` + testTenantKey + `","ANTHROPIC_AUTH_TOKEN":"` + testTenantKey + `"}}`
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(home, "launched")
	script := "#!/bin/sh\ntouch " + shellQuote(marker) + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	runner := claudeRunner{store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
	err := runner.runClaude(t.Context(), "legacy", []string{"--print", "hello"})
	if err == nil || !strings.Contains(err.Error(), "unsafe proxy transport") {
		t.Fatalf("managed Claude launch error = %v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Claude launched before transport rejection: %v", statErr)
	}

	apiKeySettings := `{"env":{"ANTHROPIC_BASE_URL":"http://192.168.1.10:31415","ANTHROPIC_API_KEY":"api-secret"}}`
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(apiKeySettings), 0o600); err != nil {
		t.Fatal(err)
	}
	err = runner.runClaude(t.Context(), "legacy", []string{"--print", "hello"})
	if err == nil || !strings.Contains(err.Error(), "unsafe proxy transport") {
		t.Fatalf("managed Claude API-key launch error = %v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Claude launched with an API key over unsafe transport: %v", statErr)
	}

	customHeaderSettings := `{"env":{"ANTHROPIC_BASE_URL":"http://192.168.1.10:31415","ANTHROPIC_AUTH_TOKEN":"subrouter","ANTHROPIC_CUSTOM_HEADERS":"Authorization: Bearer secret"}}`
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(customHeaderSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	err = runner.runClaude(t.Context(), "legacy", []string{"--print", "hello"})
	if err == nil || !strings.Contains(err.Error(), "unsafe proxy transport") {
		t.Fatalf("managed Claude custom-header launch error = %v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Claude launched with a custom header over unsafe transport: %v", statErr)
	}

	tokenlessSettings := `{"env":{"ANTHROPIC_BASE_URL":"http://legacy.example:31415","ANTHROPIC_AUTH_TOKEN":"subrouter"}}`
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(tokenlessSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	err = runner.runClaude(t.Context(), "legacy", []string{"--resume", "session-a"})
	if err == nil || !strings.Contains(err.Error(), "missing an exact durable identity") {
		t.Fatalf("managed Claude tokenless legacy launch error = %v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Claude launched tokenless legacy plaintext before migration: %v", statErr)
	}
}

func TestSecureManagedClaudeTransportDoesNotRewriteCredentialSettings(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	body := []byte(`{"theme":"dark","env":{"ANTHROPIC_BASE_URL":"http://localhost:` + port + `","ANTHROPIC_AUTH_TOKEN":"custom-auth","ANTHROPIC_CUSTOM_HEADERS":"Authorization: Bearer custom-header","FOO":"bar"}}`)
	if err := os.WriteFile(settingsPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	secureBaseURL, err := secureManagedClaudeProfileTransport(dir)
	if err != nil {
		t.Fatal(err)
	}
	if secureBaseURL != "http://127.0.0.1:"+port {
		t.Fatalf("secure base URL = %q", secureBaseURL)
	}
	after, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(body) {
		t.Fatalf("transport validation rewrote managed credentials:\nbefore: %s\nafter: %s", body, after)
	}
}

func TestSecureManagedClaudeTransportNeverRoutesCredentialsAsTenantKeys(t *testing.T) {
	load := func(context.Context) ([]byte, error) {
		return []byte(`{"Self":{"ID":"self","Online":true},"Peer":{"node-m3":{"ID":"node-m3","DNSName":"m3.example.ts.net.","TailscaleIPs":["100.88.0.9"],"Online":true}}}`), nil
	}
	lookup := func(context.Context, string) ([]net.IPAddr, error) {
		t.Fatal("exact node identity must avoid DNS")
		return nil, nil
	}
	for _, tc := range []struct {
		name string
		env  string
	}{
		{name: "tokenless", env: `"ANTHROPIC_AUTH_TOKEN":"subrouter"`},
		{name: "custom auth", env: `"ANTHROPIC_AUTH_TOKEN":"custom-secret"`},
		{name: "api key", env: `"ANTHROPIC_API_KEY":"api-secret"`},
		{name: "custom header", env: `"ANTHROPIC_AUTH_TOKEN":"subrouter","ANTHROPIC_CUSTOM_HEADERS":"Authorization: Bearer header-secret"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			settings := `{"env":{"ANTHROPIC_BASE_URL":"` + managedClaudeBlockedBaseURL + `",` + tc.env + `,"` + managedClaudeServerURLEnv + `":"http://m3.example.ts.net.:31415","` + managedClaudeTailscaleNodeEnv + `":"node-m3"}}`
			if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settings), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := secureManagedClaudeProfileTransportWithResolvers(dir, lookup, load)
			if err != nil {
				t.Fatal(err)
			}
			if got != "http://100.88.0.9:31415" {
				t.Fatalf("secured base URL = %q, credential became a tenant path", got)
			}
			for _, secret := range []string{"custom-secret", "api-secret", "header-secret", "protected-managed-credential"} {
				if strings.Contains(got, secret) {
					t.Fatalf("secured base URL leaked %q: %q", secret, got)
				}
			}
		})
	}
}

func TestRunClaudeUsesAuthoritativeSettingsOverrideAndPreservesResumeArgs(t *testing.T) {
	home := t.TempDir()
	store := claude.Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}
	configDir := store.ClaudeConfigDir("work")
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	settings := `{"theme":"dark","env":{"ANTHROPIC_BASE_URL":"http://localhost:` + port + `","ANTHROPIC_AUTH_TOKEN":"secret"}}`
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	argsPath := filepath.Join(home, "args")
	overridePath := filepath.Join(home, "override")
	modePath := filepath.Join(home, "mode")
	attackerSettingsPath := filepath.Join(home, "attacker-settings.json")
	if err := os.WriteFile(attackerSettingsPath, []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://attacker.invalid"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellQuote(argsPath) + "\nprev=''\nfor arg in \"$@\"; do\n  if [ \"$prev\" = '--settings' ]; then settings=$arg; break; fi\n  prev=$arg\ndone\ncat \"$settings\" > " + shellQuote(overridePath) + "\nstat -f '%Lp' \"$settings\" > " + shellQuote(modePath) + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	runner := claudeRunner{store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
	if err := runner.runClaude(t.Context(), "work", []string{"--managed-settings", attackerSettingsPath, "--resume", "session-a"}); err != nil {
		t.Fatal(err)
	}
	argsBody, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(argsBody)), "\n")
	if len(args) != 4 || args[0] != "--settings" || args[2] != "--resume" || args[3] != "session-a" {
		t.Fatalf("Claude args = %#v", args)
	}
	if strings.Contains(string(argsBody), "secret") {
		t.Fatalf("credential leaked through Claude argv: %q", argsBody)
	}
	if strings.Contains(string(argsBody), attackerSettingsPath) || strings.Contains(string(argsBody), "attacker.invalid") {
		t.Fatalf("attacker managed settings survived Claude argv sanitization: %q", argsBody)
	}
	var override struct {
		Env map[string]string `json:"env"`
	}
	overrideBody, err := os.ReadFile(overridePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(overrideBody, &override); err != nil {
		t.Fatalf("settings override = %q: %v", overrideBody, err)
	}
	if _, ok := override.Env["ANTHROPIC_AUTH_TOKEN"]; ok {
		t.Fatalf("settings override duplicated a credential: %+v", override)
	}
	if got := override.Env["ANTHROPIC_BASE_URL"]; got != "http://127.0.0.1:"+port {
		t.Fatalf("settings override base URL = %q", got)
	}
	modeBody, err := os.ReadFile(modePath)
	if err != nil || strings.TrimSpace(string(modeBody)) != "600" {
		t.Fatalf("override mode = %q, %v", modeBody, err)
	}
	if _, err := os.Stat(args[1]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary settings file survived launch: %v", err)
	}
}

func TestManagedClaudeLaunchArgsMakesVerifiedSettingsFinal(t *testing.T) {
	got, err := managedClaudeLaunchArgs([]string{
		"--settings", "user-a.json",
		"--managed-settings", "policy-a.json",
		"--resume", "session-a",
		"--settings=user-b.json",
		"--managed-settings=policy-b.json",
		"--", "--settings", "literal-prompt-text",
	}, "/tmp/verified.json")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--settings", "/tmp/verified.json",
		"--resume", "session-a",
		"--", "--settings", "literal-prompt-text",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("launch args = %#v, want %#v", got, want)
	}
	if _, err := managedClaudeLaunchArgs([]string{"--settings"}, "/tmp/verified.json"); err == nil {
		t.Fatal("missing user --settings value was silently accepted")
	}
	if _, err := managedClaudeLaunchArgs([]string{"--settings", "--resume", "session-a"}, "/tmp/verified.json"); err == nil {
		t.Fatal("option-looking --settings value consumed --resume")
	}
	if _, err := managedClaudeLaunchArgs([]string{"--settings="}, "/tmp/verified.json"); err == nil {
		t.Fatal("empty --settings=value was silently accepted")
	}
	if _, err := managedClaudeLaunchArgs([]string{"--managed-settings"}, "/tmp/verified.json"); err == nil {
		t.Fatal("missing --managed-settings value was silently accepted")
	}
	if _, err := managedClaudeLaunchArgs([]string{"--managed-settings", "--resume", "session-a"}, "/tmp/verified.json"); err == nil {
		t.Fatal("option-looking --managed-settings value consumed --resume")
	}
	if _, err := managedClaudeLaunchArgs([]string{"--managed-settings="}, "/tmp/verified.json"); err == nil {
		t.Fatal("empty --managed-settings=value was silently accepted")
	}
	subcommand, err := managedClaudeLaunchArgs([]string{"mcp", "list"}, "/tmp/verified.json")
	if err != nil || !slices.Equal(subcommand, []string{"--settings", "/tmp/verified.json", "mcp", "list"}) {
		t.Fatalf("subcommand launch args = %#v, %v", subcommand, err)
	}
}

func TestClaudeEnvRejectsProtectedPlaintextManagedServer(t *testing.T) {
	store := claude.Store{Dir: t.TempDir()}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetActiveProfile("work"); err != nil {
		t.Fatal(err)
	}
	configDir := store.ClaudeConfigDir("work")
	settings := `{"env":{"ANTHROPIC_BASE_URL":"` + managedClaudeBlockedBaseURL + `","ANTHROPIC_AUTH_TOKEN":"` + testTenantKey + `","` + managedClaudeServerURLEnv + `":"http://m3.example","` + managedClaudeTailscaleNodeEnv + `":"node-m3"}}`
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	runner := claudeRunner{store: store, out: &out, errOut: &out}
	err := runner.env()
	if err == nil || !strings.Contains(err.Error(), "sr claude run work") {
		t.Fatalf("env error = %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("unsafe shell exports were printed: %q", out.String())
	}
	legacy := `{"env":{"ANTHROPIC_BASE_URL":"http://legacy.example:31415","ANTHROPIC_AUTH_TOKEN":"subrouter"}}`
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	err = runner.env()
	if err == nil || !strings.Contains(err.Error(), "sr claude push work") || !strings.Contains(err.Error(), "sr claude run work") {
		t.Fatalf("legacy env error = %v", err)
	}
}

func TestProxyClaudeRunKeepsNamedProfileSemantics(t *testing.T) {
	store := claude.Store{Dir: t.TempDir()}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}

	configDir, args, err := proxyClaudeInvocation(
		store,
		[]string{"run", "work", "--resume", "session-a"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if configDir != store.ClaudeConfigDir("work") {
		t.Fatalf("config dir = %q, want work profile", configDir)
	}
	if got := strings.Join(args, " "); got != "--resume session-a" {
		t.Fatalf("Claude args = %q", got)
	}
}

func TestProxyClaudeProfileShorthandKeepsNamedProfileSemantics(t *testing.T) {
	store := claude.Store{Dir: t.TempDir()}
	if _, err := store.CreateProfile("personal"); err != nil {
		t.Fatal(err)
	}

	configDir, args, err := proxyClaudeInvocation(
		store,
		[]string{"personal", "--verbose"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if configDir != store.ClaudeConfigDir("personal") {
		t.Fatalf("config dir = %q, want personal profile", configDir)
	}
	if got := strings.Join(args, " "); got != "--verbose" {
		t.Fatalf("Claude args = %q", got)
	}
}

func TestProxyClaudeAllowsProfilelessFlagAndRunInvocations(t *testing.T) {
	store := claude.Store{Dir: t.TempDir()}
	for _, input := range [][]string{
		{"--print", "hello"},
		{"run", "--print", "hello"},
	} {
		configDir, args, err := proxyClaudeInvocation(store, input)
		if err != nil {
			t.Fatalf("proxyClaudeInvocation(%v): %v", input, err)
		}
		if configDir != "" {
			t.Fatalf("config dir for %v = %q, want default", input, configDir)
		}
		want := input
		if input[0] == "run" {
			want = input[1:]
		}
		if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("args for %v = %v, want %v", input, args, want)
		}
	}
}

func TestSRClaudeProxyUsesSelectedRemoteWithoutLocalProfile(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tenantKey string
		wantBase  string
		wantToken string
	}{
		{name: "trusted legacy", wantBase: "https://m3.example", wantToken: "subrouter"},
		{name: "tenant", tenantKey: "srt_team", wantBase: "https://m3.example/t/srt_team", wantToken: "srt_team"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			store := accounts.CodexStore{Dir: filepath.Join(home, "state", "codex", "accounts")}
			serverStore := defaultSRServerStore(store)
			if err := serverStore.save(srServerFile{
				Default: "m3-pilot",
				Servers: []srServerConfig{{
					Name:      "m3-pilot",
					URL:       "https://m3.example/v1",
					TenantKey: tc.tenantKey,
				}},
			}); err != nil {
				t.Fatal(err)
			}

			binDir := filepath.Join(home, "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatal(err)
			}
			recordPath := filepath.Join(home, "claude-proxy.txt")
			claudePath := filepath.Join(binDir, "claude")
			script := "#!/bin/sh\n{ printf 'config=%s\\nargs=%s\\nbase=%s\\ntoken=%s\\nheaders=%s\\n' \"$CLAUDE_CONFIG_DIR\" \"$*\" \"$ANTHROPIC_BASE_URL\" \"$ANTHROPIC_AUTH_TOKEN\" \"$ANTHROPIC_CUSTOM_HEADERS\"; env | grep -E '^(CLAUDE_CODE_OAUTH_TOKEN|CLAUDE_CODE_API_KEY|CLAUDE_CODE_AUTH_TOKEN|CLAUDE_CODE_BASE_URL|ANTHROPIC_API_KEY)=' || true; } > " + shellQuote(recordPath) + "\n"
			if err := os.WriteFile(claudePath, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CLAUDE_CONFIG_DIR", "/must-not-leak")
			t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "X-Subrouter-Agent: stale")
			t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "personal-oauth-must-not-leak")
			t.Setenv("CLAUDE_CODE_API_KEY", "personal-api-must-not-leak")
			t.Setenv("CLAUDE_CODE_AUTH_TOKEN", "personal-auth-must-not-leak")
			t.Setenv("CLAUDE_CODE_BASE_URL", "https://personal-route.invalid")
			t.Setenv("ANTHROPIC_API_KEY", "personal-anthropic-key-must-not-leak")

			runner := srRunner{program: "sr", store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
			if err := runner.claude(context.Background(), []string{"proxy", "--resume", "session-a", "--model", "opus"}); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(recordPath)
			if err != nil {
				t.Fatal(err)
			}
			got := string(body)
			for _, want := range []string{
				"args=--resume session-a --model opus\n",
				"base=" + tc.wantBase + "\n",
				"token=" + tc.wantToken + "\n",
				"headers=X-Subrouter-Agent: claude\n",
			} {
				if !strings.Contains(got, want) {
					t.Fatalf("proxy invocation missing %q:\n%s", want, got)
				}
			}
			for _, forbidden := range []string{
				"CLAUDE_CODE_OAUTH_TOKEN=", "CLAUDE_CODE_API_KEY=",
				"CLAUDE_CODE_AUTH_TOKEN=", "CLAUDE_CODE_BASE_URL=",
				"ANTHROPIC_API_KEY=", "personal-",
			} {
				if strings.Contains(got, forbidden) {
					t.Fatalf("proxy invocation inherited %q:\n%s", forbidden, got)
				}
			}
			configLine := strings.SplitN(got, "\n", 2)[0]
			configDir := strings.TrimPrefix(configLine, "config=")
			if configDir == "" || configDir == "/must-not-leak" || filepath.Dir(configDir) != filepath.Join(store.StoreDir(), "claude-proxy") {
				t.Fatalf("proxy did not use an isolated config directory: %q", configLine)
			}
			if tc.tenantKey != "" && strings.Contains(configDir, tc.tenantKey) {
				t.Fatalf("proxy config path exposed tenant credential: %q", configDir)
			}
			if info, err := os.Stat(configDir); err != nil || !info.IsDir() {
				t.Fatalf("durable proxy config is unavailable after launch: info=%v err=%v", info, err)
			}
			sessionMarker := filepath.Join(configDir, "session-marker")
			if err := os.WriteFile(sessionMarker, []byte("resume-me"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := runner.claude(context.Background(), []string{"proxy", "--resume", "session-a"}); err != nil {
				t.Fatal(err)
			}
			if marker, err := os.ReadFile(sessionMarker); err != nil || string(marker) != "resume-me" {
				t.Fatalf("proxy launch did not preserve resumable session state: marker=%q err=%v", marker, err)
			}
			secondBody, err := os.ReadFile(recordPath)
			if err != nil {
				t.Fatal(err)
			}
			if secondConfig := strings.TrimPrefix(strings.SplitN(string(secondBody), "\n", 2)[0], "config="); secondConfig != configDir {
				t.Fatalf("proxy config changed across resume: first=%q second=%q", configDir, secondConfig)
			}
		})
	}
}

func TestTenantProxyTransportRequiresHTTPSOffLoopback(t *testing.T) {
	for _, tc := range []struct {
		name      string
		baseURL   string
		tenantKey string
		wantErr   bool
	}{
		{name: "remote HTTPS", baseURL: "https://router.example/t/srt_team", tenantKey: "srt_team"},
		{name: "loopback IPv4 HTTP", baseURL: "http://127.0.0.1:31415/t/srt_team", tenantKey: "srt_team"},
		{name: "loopback localhost HTTP", baseURL: "http://localhost:31415/t/srt_team", tenantKey: "srt_team"},
		{name: "remote HTTP", baseURL: "http://router.example/t/srt_team", tenantKey: "srt_team", wantErr: true},
		{name: "remote HTTP without secret", baseURL: "http://router.example", tenantKey: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := secureTenantProxyURL(t.Context(), tc.baseURL, tc.tenantKey)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateTenantProxyTransport() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestSRClaudeProxyRejectsPreviouslyStoredRemoteHTTPTenant(t *testing.T) {
	home := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(home, "state", "codex", "accounts")}
	if err := defaultSRServerStore(store).save(srServerFile{
		Default: "insecure",
		Servers: []srServerConfig{{
			Name: "insecure", URL: "http://router.example:31415", TenantKey: testTenantKey,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	runner := srRunner{program: "sr", store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
	err := runner.claude(context.Background(), []string{"proxy", "--print", "hello"})
	if err == nil || !strings.Contains(err.Error(), "must use HTTPS") {
		t.Fatalf("Claude proxy error = %v, want HTTPS requirement", err)
	}
}

func TestSRClaudeProxyUsesHealthySelectedLocalRoute(t *testing.T) {
	home := t.TempDir()
	local := healthServer(t, http.StatusOK)
	t.Setenv("HOME", home)
	t.Setenv("SUBROUTER_CLOUD_CONFIG", filepath.Join(home, "missing-cloud.json"))
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL+"/v1")

	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(home, "claude-local.txt")
	claudePath := filepath.Join(binDir, "claude")
	script := "#!/bin/sh\nprintf 'args=%s\\nbase=%s\\ntoken=%s\\nheaders=%s\\n' \"$*\" \"$ANTHROPIC_BASE_URL\" \"$ANTHROPIC_AUTH_TOKEN\" \"$ANTHROPIC_CUSTOM_HEADERS\" > " + shellQuote(recordPath) + "\n"
	if err := os.WriteFile(claudePath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	runner := srRunner{
		program: "sr",
		store:   accounts.CodexStore{Dir: filepath.Join(home, "state", "codex", "accounts")},
		in:      strings.NewReader(""),
		out:     io.Discard,
		errOut:  io.Discard,
	}
	if err := runner.claude(context.Background(), []string{"proxy", "-p", "hello"}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	for _, want := range []string{
		"args=-p hello\n",
		"base=" + local.URL + "\n",
		"token=subrouter\n",
		"headers=X-Subrouter-Agent: claude\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("local proxy invocation missing %q:\n%s", want, got)
		}
	}
}

func TestSRClaudeBareRemainsProfileManager(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out bytes.Buffer
	runner := srRunner{
		store:  accounts.CodexStore{Dir: filepath.Join(home, "state", "codex", "accounts")},
		in:     strings.NewReader(""),
		out:    &out,
		errOut: &out,
		client: http.DefaultClient,
	}
	if err := runner.claude(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No Claude profiles") {
		t.Fatalf("bare command did not show profile manager output: %q", out.String())
	}
	if strings.Contains(out.String(), "daemon") {
		t.Fatalf("bare command attempted daemon management: %q", out.String())
	}
}

func TestSRClaudeHelpDocumentsProfilelessProxy(t *testing.T) {
	var out bytes.Buffer
	runner := srRunner{out: &out, errOut: &out}
	if err := runner.claude(context.Background(), []string{"help"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "sr claude proxy [args...]") {
		t.Fatalf("help missing proxy command:\n%s", out.String())
	}
}

func TestPrepareClaudeLoginFastPathSeedsFreshDir(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "profile")
	if err := prepareClaudeLoginFastPath(configDir); err != nil {
		t.Fatalf("prepareClaudeLoginFastPath: %v", err)
	}
	state, err := os.ReadFile(filepath.Join(configDir, ".claude.json"))
	if err != nil {
		t.Fatalf("read .claude.json: %v", err)
	}
	if !strings.Contains(string(state), `"hasCompletedOnboarding": true`) {
		t.Fatalf("onboarding not seeded:\n%s", state)
	}
	settings, err := os.ReadFile(filepath.Join(configDir, "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	if !strings.Contains(string(settings), `"forceLoginMethod": "claudeai"`) {
		t.Fatalf("login method not seeded:\n%s", settings)
	}
}

func TestPrepareClaudeLoginFastPathPreservesExistingChoices(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{"theme":"light"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"forceLoginMethod":"console"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareClaudeLoginFastPath(dir); err != nil {
		t.Fatalf("prepareClaudeLoginFastPath: %v", err)
	}
	state, _ := os.ReadFile(filepath.Join(dir, ".claude.json"))
	if !strings.Contains(string(state), `"theme": "light"`) || !strings.Contains(string(state), `"hasCompletedOnboarding": true`) {
		t.Fatalf("existing state not preserved:\n%s", state)
	}
	settings, _ := os.ReadFile(filepath.Join(dir, "settings.json"))
	if !strings.Contains(string(settings), `"forceLoginMethod":"console"`) {
		t.Fatalf("existing login method overwritten:\n%s", settings)
	}
}
