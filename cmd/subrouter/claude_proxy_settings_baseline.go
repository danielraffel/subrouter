package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/fsutil"
)

// A proxy config directory's own settings.json is what Claude reads as the
// user settings whenever it runs with CLAUDE_CONFIG_DIR pointing there, and
// sr's --settings overlay (withClaudeUserSettings) exists only for launches
// sr makes itself. So the directory also carries a baseline of the user's
// permission rules and hooks: a new directory starts with them, and an
// existing one gains any it is missing each time sr prepares it.
//
// The merge only adds. Rule lists and hook groups are unioned without
// duplicates, entries the directory already has are never removed, and every
// other key (theme, autoMode, a /config change made inside a pooled session)
// is left exactly as the directory has it. Only permissions and hooks are
// copied: they hold no credentials, and keys that could substitute a
// credential source never leave the user's file.
//
// SUBROUTER_CLAUDE_USER_SETTINGS=0 turns this off together with the launch
// overlay, because claudeProxyUserSettingsPath returns "" for it.
var claudeProxySettingsBaselineKeys = []string{"permissions", "hooks"}

// claudeProxyPermissionListKeys are the permissions lists that are unioned.
// Any other key under permissions, list or not, stays the directory's own.
var claudeProxyPermissionListKeys = []string{"allow", "deny", "ask", "additionalDirectories"}

// seedClaudeProxySettingsBaseline merges the baseline keys of the user's
// settings file into configDir/settings.json. A missing or unparsable user
// file, or an unparsable proxy file, is left alone; errors are for logging
// only and never block a launch.
func seedClaudeProxySettingsBaseline(userSettingsPath, configDir string) error {
	proxySettingsPath := claudeProxyOwnSettingsPath(configDir)
	if strings.TrimSpace(userSettingsPath) == "" || proxySettingsPath == "" {
		return nil
	}
	if sameClaudeConfigFile(userSettingsPath, proxySettingsPath) {
		return nil
	}
	user, ok := readClaudeSettingsObject(userSettingsPath)
	if !ok {
		return nil
	}
	baseline := map[string]any{}
	for _, key := range claudeProxySettingsBaselineKeys {
		if value, ok := user[key].(map[string]any); ok && len(value) > 0 {
			baseline[key] = value
		}
	}
	if len(baseline) == 0 {
		return nil
	}
	// A dotfile manager may link the file elsewhere; write through the link.
	if resolved, err := filepath.EvalSymlinks(proxySettingsPath); err == nil {
		proxySettingsPath = resolved
	}
	release, locked := lockClaudeConfig(proxySettingsPath)
	if !locked {
		return nil
	}
	defer release()
	info, statErr := os.Stat(proxySettingsPath)
	body, err := os.ReadFile(proxySettingsPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		body = []byte("{}")
	case err != nil:
		return err
	}
	var own map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber() // keep the directory's own numbers exactly as written
	if err := decoder.Decode(&own); err != nil {
		// Claude reports a broken settings file itself; rewriting it here
		// would destroy whatever the user was in the middle of editing.
		return nil
	}
	if own == nil {
		own = map[string]any{}
	}
	changed := false
	if user, ok := baseline["permissions"].(map[string]any); ok {
		if merged, ok := mergeClaudePermissionLists(own["permissions"], user); ok {
			own["permissions"] = merged
			changed = true
		}
	}
	if user, ok := baseline["hooks"].(map[string]any); ok {
		if merged, ok := mergeClaudeHookGroups(own["hooks"], user); ok {
			own["hooks"] = merged
			changed = true
		}
	}
	if !changed {
		return nil
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false) // hook commands keep their literal > and &
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(own); err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if statErr == nil {
		mode = info.Mode().Perm()
	}
	return fsutil.WriteFileAtomic(proxySettingsPath, out.Bytes(), mode)
}

// mergeClaudePermissionLists unions each list the user's permissions block
// has (allow, deny, ask, additionalDirectories) into the proxy's, keeping the
// proxy's order and appending only missing entries. Single values such as
// defaultMode stay the proxy's own. A proxy permissions value that is not an
// object is left untouched. It reports whether anything was added.
func mergeClaudePermissionLists(ownValue any, user map[string]any) (map[string]any, bool) {
	own, ok := ownValue.(map[string]any)
	if ownValue != nil && !ok {
		return nil, false
	}
	if own == nil {
		own = map[string]any{}
	}
	changed := false
	for _, key := range claudeProxyPermissionListKeys {
		userList, ok := user[key].([]any)
		if !ok {
			continue
		}
		existing, ok := own[key].([]any)
		if own[key] != nil && !ok {
			continue
		}
		if merged, added := unionJSONList(existing, userList); added {
			own[key] = merged
			changed = true
		}
	}
	return own, changed
}

// mergeClaudeHookGroups unions the user's hook groups into the proxy's per
// event. A group already present with identical content is not added again.
func mergeClaudeHookGroups(ownValue any, user map[string]any) (map[string]any, bool) {
	own, ok := ownValue.(map[string]any)
	if ownValue != nil && !ok {
		return nil, false
	}
	if own == nil {
		own = map[string]any{}
	}
	changed := false
	for event, value := range user {
		userGroups, ok := value.([]any)
		if !ok {
			continue
		}
		existing, ok := own[event].([]any)
		if own[event] != nil && !ok {
			continue
		}
		if merged, added := unionJSONList(existing, userGroups); added {
			own[event] = merged
			changed = true
		}
	}
	return own, changed
}

// unionJSONList appends each entry of extra that base does not already hold,
// comparing canonical JSON so object key order does not matter.
func unionJSONList(base, extra []any) ([]any, bool) {
	seen := make(map[string]bool, len(base)+len(extra))
	out := make([]any, 0, len(base)+len(extra))
	for _, entry := range base {
		key, err := json.Marshal(entry)
		if err == nil {
			seen[string(key)] = true
		}
		out = append(out, entry)
	}
	added := false
	for _, entry := range extra {
		key, err := json.Marshal(entry)
		if err != nil || seen[string(key)] {
			continue
		}
		seen[string(key)] = true
		out = append(out, entry)
		added = true
	}
	return out, added
}
