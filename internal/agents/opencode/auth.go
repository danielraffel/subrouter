package opencode

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

const providerOpenAI = "openai"

type Entry struct {
	Type      string            `json:"type"`
	Refresh   string            `json:"refresh,omitempty"`
	Access    string            `json:"access,omitempty"`
	Expires   int64             `json:"expires,omitempty"`
	AccountID string            `json:"accountId,omitempty"`
	Key       string            `json:"key,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

func DefaultAuthPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".local", "share", "opencode", "auth.json")
	}
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		dataHome = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataHome, "opencode", "auth.json")
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
			return "", Entry{}, fmt.Errorf("OpenCode sync requires a non-empty OpenAI API key")
		}
		return providerOpenAI, Entry{Type: "api", Key: account.Auth.OpenAIAPIKey}, nil
	}

	tokens := account.Auth.Tokens
	if tokens == nil || tokens.AccessToken == "" || tokens.RefreshToken == "" {
		return "", Entry{}, fmt.Errorf("OpenCode sync requires Codex OAuth tokens")
	}
	expires, ok := accounts.JWTExpiryMillis(tokens.AccessToken)
	if !ok {
		expires = time.Now().Add(time.Hour).UnixMilli()
	}
	return providerOpenAI, Entry{
		Type:      "oauth",
		Refresh:   tokens.RefreshToken,
		Access:    tokens.AccessToken,
		Expires:   expires,
		AccountID: accounts.ExtractChatGPTAccountID(account.Auth),
	}, nil
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
