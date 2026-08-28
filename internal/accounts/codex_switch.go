package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type rawCodexStoredAccount struct {
	Auth json.RawMessage `json:"auth"`
}

func DefaultCodexAuthPath() string {
	if codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME")); codexHome != "" {
		return filepath.Join(codexHome, "auth.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".codex", "auth.json")
	}
	return filepath.Join(home, ".codex", "auth.json")
}

func (s CodexStore) SwitchActive(accountID string) error {
	stored, ok, err := s.FindStored(accountID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("account %q not found", accountID)
	}
	lock, err := s.lockStoredAccount(stored.Email)
	if err != nil {
		return err
	}
	defer lock.Close()
	stored, ok, err = s.findStoredExact(stored.Email)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("account %q not found", accountID)
	}
	_, usable := stored.toAccount(stored.SourcePath(s))
	if !usable {
		return fmt.Errorf("account %q is not usable", accountID)
	}
	rawAuth, err := readRawAuth(stored.SourcePath(s))
	if err != nil {
		return err
	}

	previous := stored
	downgraded := !stored.IsAPIKey() && stored.OAuthCredentialOrigin == CodexOAuthOriginIsolatedServerLogin
	if downgraded {
		stored.OAuthCredentialOrigin = CodexOAuthOriginInteractiveImport
		appendCodexAuthBreadcrumb(
			context.Background(), s, &stored,
			"credential_exported_to_active", "account_manager", false,
			&previous, &stored, nil, nil,
		)
		if err := s.saveStoredUnlocked(stored); err != nil {
			return err
		}
	}
	if err := writeCodexActiveAuth(DefaultCodexAuthPath(), rawAuth); err != nil {
		if downgraded {
			if rollbackErr := s.saveStoredUnlocked(previous); rollbackErr != nil {
				return errors.Join(err, fmt.Errorf("restore isolated credential provenance: %w", rollbackErr))
			}
		}
		return err
	}
	return nil
}

func (s CodexStore) rawAuthFor(accountID string) (Account, json.RawMessage, error) {
	needle := strings.TrimSpace(accountID)
	if needle == "" {
		return Account{}, nil, errors.New("account id is required")
	}
	stored, ok, err := s.FindStored(needle)
	if err != nil {
		return Account{}, nil, err
	}
	if !ok {
		return Account{}, nil, fmt.Errorf("account %q not found", accountID)
	}
	source := stored.SourcePath(s)
	account, ok := stored.toAccount(source)
	if !ok {
		return Account{}, nil, fmt.Errorf("account %q is not usable", accountID)
	}
	rawAuth, err := readRawAuth(source)
	if err != nil {
		return Account{}, nil, err
	}
	return account, rawAuth, nil
}

func readRawAuth(path string) (json.RawMessage, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var stored rawCodexStoredAccount
	if err := json.Unmarshal(body, &stored); err != nil {
		return nil, err
	}
	if len(stored.Auth) == 0 {
		return nil, fmt.Errorf("%s has no auth", path)
	}
	return stored.Auth, nil
}

func writeCodexActiveAuth(path string, rawAuth json.RawMessage) error {
	var auth CodexAuthFile
	if err := json.Unmarshal(rawAuth, &auth); err != nil {
		return err
	}
	payload := rawAuth
	if auth.AuthMode == "apikey" || auth.OpenAIAPIKey != "" {
		apiKeyPayload := map[string]string{
			"auth_mode":      "apikey",
			"OPENAI_API_KEY": auth.OpenAIAPIKey,
		}
		body, err := json.Marshal(apiKeyPayload)
		if err != nil {
			return err
		}
		payload = body
	}

	var formatted bytes.Buffer
	if err := json.Indent(&formatted, payload, "", "  "); err != nil {
		return err
	}
	formatted.WriteByte('\n')

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		_ = os.Rename(path, path+".bak")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, formatted.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
