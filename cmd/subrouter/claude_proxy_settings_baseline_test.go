package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/agents/claude"
)

const baselineUserSettings = `{
	"permissions": {
		"allow": ["Bash(ls:*)", "Read(//tmp/**)"],
		"deny": ["Bash(rm -rf:*)"],
		"ask": ["Bash(git push:*)"],
		"defaultMode": "acceptEdits"
	},
	"hooks": {
		"SessionStart": [{"hooks": [{"type": "command", "command": "start.sh 2>&1"}]}],
		"PostToolUse": [{"matcher": "Edit", "hooks": [{"type": "command", "command": "fmt.sh", "timeout": 30}]}]
	},
	"theme": "light",
	"model": "opus",
	"apiKeyHelper": "print-other-key",
	"env": {"SECRET_TOKEN": "do-not-copy"}
}`

func writeBaselineFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readBaselineFile(t *testing.T, path string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatalf("settings not JSON: %v\n%s", err, body)
	}
	return settings
}

func baselineStrings(t *testing.T, value any) []string {
	t.Helper()
	list, _ := value.([]any)
	out := make([]string, 0, len(list))
	for _, entry := range list {
		out = append(out, entry.(string))
	}
	return out
}

func TestSeedClaudeProxySettingsBaselineSeedsNewFolder(t *testing.T) {
	userSettings := filepath.Join(t.TempDir(), "settings.json")
	writeBaselineFile(t, userSettings, baselineUserSettings)
	configDir := filepath.Join(t.TempDir(), "claude-proxy", "abc")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := seedClaudeProxySettingsBaseline(userSettings, configDir); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(configDir, "settings.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	got := readBaselineFile(t, path)
	for _, key := range []string{"theme", "model", "apiKeyHelper", "env"} {
		if _, ok := got[key]; ok {
			t.Fatalf("%s copied into proxy settings; only permissions and hooks may be: %v", key, got)
		}
	}
	permissions := got["permissions"].(map[string]any)
	if want := []string{"Bash(ls:*)", "Read(//tmp/**)"}; !reflect.DeepEqual(baselineStrings(t, permissions["allow"]), want) {
		t.Fatalf("allow = %v, want %v", permissions["allow"], want)
	}
	if want := []string{"Bash(rm -rf:*)"}; !reflect.DeepEqual(baselineStrings(t, permissions["deny"]), want) {
		t.Fatalf("deny = %v", permissions["deny"])
	}
	if want := []string{"Bash(git push:*)"}; !reflect.DeepEqual(baselineStrings(t, permissions["ask"]), want) {
		t.Fatalf("ask = %v", permissions["ask"])
	}
	if _, ok := permissions["defaultMode"]; ok {
		t.Fatalf("single-valued permission setting copied: %v", permissions)
	}
	hooks := got["hooks"].(map[string]any)
	for _, event := range []string{"SessionStart", "PostToolUse"} {
		if groups, _ := hooks[event].([]any); len(groups) != 1 {
			t.Fatalf("%s hooks = %v, want one group", event, hooks[event])
		}
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "start.sh 2>&1") {
		t.Fatalf("hook command was rewritten with escapes:\n%s", raw)
	}
}

func TestSeedClaudeProxySettingsBaselineMergesExistingFolderWithoutLosingEntries(t *testing.T) {
	userSettings := filepath.Join(t.TempDir(), "settings.json")
	writeBaselineFile(t, userSettings, baselineUserSettings)
	configDir := t.TempDir()
	path := filepath.Join(configDir, "settings.json")
	writeBaselineFile(t, path, `{
		"theme": "dark",
		"autoMode": {"enabled": true},
		"cleanupPeriodDays": 9007199254740993,
		"permissions": {
			"allow": ["Bash(make:*)", "Bash(ls:*)"],
			"defaultMode": "plan"
		},
		"hooks": {
			"SessionStart": [
				{"hooks": [{"command": "start.sh 2>&1", "type": "command"}]},
				{"hooks": [{"type": "command", "command": "proxy-only.sh"}]}
			]
		}
	}`)

	if err := seedClaudeProxySettingsBaseline(userSettings, configDir); err != nil {
		t.Fatal(err)
	}

	got := readBaselineFile(t, path)
	if got["theme"] != "dark" {
		t.Fatalf("folder theme overwritten: %v", got["theme"])
	}
	if autoMode, _ := got["autoMode"].(map[string]any); autoMode["enabled"] != true {
		t.Fatalf("folder autoMode lost: %v", got["autoMode"])
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "9007199254740993") {
		t.Fatalf("folder number rewritten through float64:\n%s", raw)
	}
	permissions := got["permissions"].(map[string]any)
	if want := []string{"Bash(make:*)", "Bash(ls:*)", "Read(//tmp/**)"}; !reflect.DeepEqual(baselineStrings(t, permissions["allow"]), want) {
		t.Fatalf("allow = %v, want %v", permissions["allow"], want)
	}
	if permissions["defaultMode"] != "plan" {
		t.Fatalf("folder defaultMode overwritten: %v", permissions["defaultMode"])
	}
	if len(baselineStrings(t, permissions["deny"])) != 1 || len(baselineStrings(t, permissions["ask"])) != 1 {
		t.Fatalf("deny/ask not added: %v", permissions)
	}
	hooks := got["hooks"].(map[string]any)
	// The user's SessionStart group is already present (with different key
	// order), so only the folder's two groups remain.
	if groups, _ := hooks["SessionStart"].([]any); len(groups) != 2 {
		t.Fatalf("SessionStart groups = %v, want the folder's 2 with no duplicate", hooks["SessionStart"])
	}
	if groups, _ := hooks["PostToolUse"].([]any); len(groups) != 1 {
		t.Fatalf("PostToolUse groups = %v, want the user's group added", hooks["PostToolUse"])
	}

	// A second run finds nothing to add and leaves the file byte-identical.
	before, _ := os.ReadFile(path)
	if err := seedClaudeProxySettingsBaseline(userSettings, configDir); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatalf("idempotent run changed the file:\n%s\n---\n%s", before, after)
	}
}

func TestSeedClaudeProxySettingsBaselineSkipsUnparsableFiles(t *testing.T) {
	userSettings := filepath.Join(t.TempDir(), "settings.json")
	writeBaselineFile(t, userSettings, baselineUserSettings)
	configDir := t.TempDir()
	path := filepath.Join(configDir, "settings.json")
	broken := `{"theme": "dark", "permissions": {"allow": [` // mid-edit
	writeBaselineFile(t, path, broken)
	if err := seedClaudeProxySettingsBaseline(userSettings, configDir); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(path); string(body) != broken {
		t.Fatalf("unparsable proxy settings rewritten: %s", body)
	}

	brokenUser := filepath.Join(t.TempDir(), "settings.json")
	writeBaselineFile(t, brokenUser, `{not json`)
	fresh := t.TempDir()
	if err := seedClaudeProxySettingsBaseline(brokenUser, fresh); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fresh, "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("settings created from an unparsable user file: %v", err)
	}
	if err := seedClaudeProxySettingsBaseline(filepath.Join(t.TempDir(), "missing.json"), fresh); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fresh, "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("settings created without a user file: %v", err)
	}
}

func TestPrepareClaudeProxySharedStateSeedsBaselineAndHonoursOptOut(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeBaselineFile(t, filepath.Join(home, ".claude", "settings.json"), baselineUserSettings)
	storeDir := claude.DefaultStore().Dir

	optedOut := claudeProxyConfigDir(storeDir, "opt-out", "")
	if err := os.MkdirAll(optedOut, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(claudeProxyUserSettingsEnv, "0")
	if err := prepareClaudeProxySharedState(optedOut, storeDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(optedOut, "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("baseline seeded despite %s=0: %v", claudeProxyUserSettingsEnv, err)
	}

	t.Setenv(claudeProxyUserSettingsEnv, "")
	configDir := claudeProxyConfigDir(storeDir, "seeded", "")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := prepareClaudeProxySharedState(configDir, storeDir); err != nil {
		t.Fatal(err)
	}
	got := readBaselineFile(t, filepath.Join(configDir, "settings.json"))
	if _, ok := got["hooks"].(map[string]any)["SessionStart"]; !ok {
		t.Fatalf("new proxy folder missing the user's hooks: %v", got)
	}

	// A hermetic store never reads the user's real Claude home.
	hermetic := filepath.Join(t.TempDir(), "claude-proxy", "x")
	if err := os.MkdirAll(hermetic, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := prepareClaudeProxySharedState(hermetic, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(hermetic, "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("hermetic store seeded from the user's home: %v", err)
	}
}
