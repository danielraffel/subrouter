package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var codexRoutingConfigKeys = []string{
	"model_provider",
	"openai_base_url",
	"chatgpt_base_url",
	"experimental_realtime_ws_base_url",
}

// codexSubrouterProviderID is the model_providers key written into a client's
// codex config so requests route through a provider with WebSockets disabled.
const codexSubrouterProviderID = "subrouter"

func defaultCodexConfigPath() (string, error) {
	if codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME")); codexHome != "" {
		return filepath.Join(codexHome, "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "config.toml"), nil
}

func writeCodexConfigForServer(server srServerConfig) (string, error) {
	return writeCodexConfigForBaseURL(codexBaseURLForServer(server))
}

func writeCodexConfigForLocal() (string, error) {
	return writeCodexConfigForBaseURL(defaultCodexBaseURL)
}

func writeCodexConfigForBaseURL(baseURL string) (string, error) {
	path, err := defaultCodexConfigPath()
	if err != nil {
		return "", err
	}
	root := codexProxyRootURL(baseURL)
	providerBaseURL := root + "/v1"
	values := map[string]string{
		"model_provider":                    codexSubrouterProviderID,
		"openai_base_url":                   providerBaseURL,
		"chatgpt_base_url":                  root + "/backend-api",
		"experimental_realtime_ws_base_url": providerBaseURL,
	}
	if err := writeCodexConfig(path, func(existing string) string {
		next := updateTopLevelTomlStrings(existing, values)
		return ensureCodexWebsocketDisabledProvider(next, providerBaseURL)
	}); err != nil {
		return "", err
	}
	return path, nil
}

func codexProxyRootURL(baseURL string) string {
	root := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	for _, suffix := range []string{"/v1", "/backend-api"} {
		if strings.HasSuffix(root, suffix) {
			root = strings.TrimSuffix(root, suffix)
			break
		}
	}
	return strings.TrimRight(root, "/")
}

func writeCodexConfigValues(path string, values map[string]string) error {
	return writeCodexConfig(path, func(existing string) string {
		return updateTopLevelTomlStrings(existing, values)
	})
}

// writeCodexConfig reads the codex config, applies transform, and writes the
// result back atomically (with a `.bak` backup) only when it changed.
func writeCodexConfig(path string, transform func(existing string) string) error {
	body, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	next := transform(string(body))
	if string(body) == next {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if len(body) > 0 {
		if err := os.WriteFile(path+".bak", body, 0o600); err != nil {
			return fmt.Errorf("write backup: %w", err)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(next), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ensureCodexWebsocketDisabledProvider rewrites the
// `[model_providers.subrouter]` table so codex routes through a provider with
// `supports_websockets = false`. codex 0.136+ otherwise derives a `ws://` URL
// from `openai_base_url` and opens the Responses transport over a WebSocket;
// the subrouter's upstream rejects that upgrade (403), and codex retries the
// connect without falling back to HTTP, surfacing the turn as a 429. Pinning a
// provider with WebSockets disabled keeps codex on the proven HTTP Responses
// path. `name = "OpenAI"` preserves codex's OpenAI-specific behavior (request
// compression, remote compaction); the subrouter still selects the upstream
// account, so client auth is unchanged.
func ensureCodexWebsocketDisabledProvider(text, baseURL string) string {
	text = removeTomlTable(text, "model_providers."+codexSubrouterProviderID)
	block := strings.Join([]string{
		"[model_providers." + codexSubrouterProviderID + "]",
		`name = "OpenAI"`,
		"base_url = " + strconv.Quote(baseURL),
		`wire_api = "responses"`,
		"requires_openai_auth = true",
		"supports_websockets = false",
		"",
	}, "\n")
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	if text != "" {
		text += "\n"
	}
	return text + block
}

// removeTomlTable strips a `[header]` table (its header line through the line
// before the next table header or EOF) from a TOML document, leaving other
// top-level keys and tables untouched.
func removeTomlTable(text, header string) string {
	lines := splitLinesKeepingEndings(text)
	out := make([]string, 0, len(lines))
	target := "[" + header + "]"
	skipping := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(strings.TrimRight(line, "\r\n"))
		if isTomlTableHeader(trimmed) {
			skipping = trimmed == target
		}
		if skipping {
			continue
		}
		out = append(out, line)
	}
	return strings.TrimRight(strings.Join(out, ""), "\n")
}

func updateTopLevelTomlStrings(text string, values map[string]string) string {
	remaining := map[string]string{}
	for key, value := range values {
		remaining[key] = value
	}
	lines := splitLinesKeepingEndings(text)
	out := make([]string, 0, len(lines)+len(values))
	inTopLevel := true
	insertedMissing := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(strings.TrimRight(line, "\r\n"))
		if inTopLevel && isTomlTableHeader(trimmed) {
			out = appendMissingTomlStrings(out, remaining)
			insertedMissing = true
			inTopLevel = false
		}
		if inTopLevel {
			key, ok := tomlBareAssignmentKey(line)
			if ok {
				if value, exists := remaining[key]; exists {
					out = append(out, leadingWhitespace(line)+key+" = "+strconv.Quote(value)+lineEnding(line))
					delete(remaining, key)
					continue
				}
			}
		}
		out = append(out, line)
	}
	if !insertedMissing {
		out = appendMissingTomlStrings(out, remaining)
	}
	result := strings.Join(out, "")
	if result == "" || !strings.HasSuffix(result, "\n") {
		result += "\n"
	}
	return result
}

func appendMissingTomlStrings(out []string, remaining map[string]string) []string {
	if len(remaining) == 0 {
		return out
	}
	if len(out) > 0 && !strings.HasSuffix(out[len(out)-1], "\n") {
		out[len(out)-1] += "\n"
	}
	keys := make([]string, 0, len(remaining))
	for _, key := range codexRoutingConfigKeys {
		if _, ok := remaining[key]; ok {
			keys = append(keys, key)
		}
	}
	for _, key := range keys {
		out = append(out, key+" = "+strconv.Quote(remaining[key])+"\n")
		delete(remaining, key)
	}
	return out
}

func splitLinesKeepingEndings(text string) []string {
	if text == "" {
		return nil
	}
	var lines []string
	start := 0
	for i, ch := range text {
		if ch == '\n' {
			lines = append(lines, text[start:i+1])
			start = i + 1
		}
	}
	if start < len(text) {
		lines = append(lines, text[start:])
	}
	return lines
}

func isTomlTableHeader(trimmed string) bool {
	return strings.HasPrefix(trimmed, "[")
}

func tomlBareAssignmentKey(line string) (string, bool) {
	idx := strings.IndexByte(line, '=')
	if idx < 0 {
		return "", false
	}
	key := strings.TrimSpace(line[:idx])
	if key == "" {
		return "", false
	}
	for _, ch := range key {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '-' {
			continue
		}
		return "", false
	}
	return key, true
}

func leadingWhitespace(line string) string {
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	return line[:i]
}

func lineEnding(line string) string {
	if strings.HasSuffix(line, "\r\n") {
		return "\r\n"
	}
	if strings.HasSuffix(line, "\n") {
		return "\n"
	}
	return "\n"
}
