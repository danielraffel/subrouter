package pi

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

const (
	providerOpenAI      = "openai"
	providerOpenAICodex = "openai-codex"
)

type Entry struct {
	Type      string `json:"type"`
	Key       string `json:"key,omitempty"`
	Refresh   string `json:"refresh,omitempty"`
	Access    string `json:"access,omitempty"`
	Expires   int64  `json:"expires,omitempty"`
	AccountID string `json:"accountId,omitempty"`
}

func DefaultAuthPath() string {
	dir := os.Getenv("PI_CODING_AGENT_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(".pi", "agent", "auth.json")
		}
		dir = filepath.Join(home, ".pi", "agent")
	}
	return filepath.Join(expandTilde(dir), "auth.json")
}

func SyncCodexAccount(account accounts.StoredCodexAccount) (string, error) {
	provider, entry, err := EntryForCodexAccount(account)
	if err != nil {
		return "", err
	}
	path := DefaultAuthPath()
	return path, writeAuthEntry(path, provider, entry)
}

func EntryForCodexAccount(account accounts.StoredCodexAccount) (string, Entry, error) {
	if account.IsAPIKey() {
		if account.Auth.OpenAIAPIKey == "" {
			return "", Entry{}, fmt.Errorf("pi sync requires a non-empty OpenAI API key")
		}
		return providerOpenAI, Entry{Type: "api_key", Key: account.Auth.OpenAIAPIKey}, nil
	}

	tokens := account.Auth.Tokens
	if tokens == nil || tokens.AccessToken == "" || tokens.RefreshToken == "" {
		return "", Entry{}, fmt.Errorf("pi sync requires Codex OAuth tokens")
	}
	expires, ok := accounts.JWTExpiryMillis(tokens.AccessToken)
	if !ok {
		expires = time.Now().Add(time.Hour).UnixMilli()
	}
	return providerOpenAICodex, Entry{
		Type:      "oauth",
		Refresh:   tokens.RefreshToken,
		Access:    tokens.AccessToken,
		Expires:   expires,
		AccountID: accounts.ExtractChatGPTAccountID(account.Auth),
	}, nil
}

func expandTilde(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	if len(path) > 2 && path[:2] == "~/" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func writeAuthEntry(path, provider string, entry Entry) error {
	data := map[string]json.RawMessage{}
	body, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(body, &data); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	entryBody, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	data[provider] = entryBody

	formatted, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	formatted = append(formatted, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, formatted, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
