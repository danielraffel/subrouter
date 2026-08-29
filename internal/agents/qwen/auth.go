package qwen

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

const previousAccessTokenKey = "subrouter_previous_access_token"

// PrepareConsoleLogin temporarily gives Bailian CLI the selected account's
// model key. Alibaba uses it to associate the browser login with the purchased
// Token Plan; FinishConsoleLogin removes it again after OAuth completes.
func PrepareConsoleLogin(accountID, apiKey, baseURL string) error {
	return PrepareConsoleLoginIn(DefaultConsoleRoot(), accountID, apiKey, baseURL)
}

func PrepareConsoleLoginIn(root, accountID, apiKey, baseURL string) error {
	if strings.TrimSpace(apiKey) == "" {
		return fmt.Errorf("Qwen Token Plan API key is missing")
	}
	config, err := readRawConsoleConfigIn(root, accountID)
	if err != nil {
		return err
	}
	// A successful CLI exit is not proof of a new browser authorization. Stage
	// the previous token separately so Finish cannot mistake it for a completed
	// callback, while a cancelled/failed flow can still restore working auth.
	if token, _ := config["access_token"].(string); strings.TrimSpace(token) != "" {
		config[previousAccessTokenKey] = token
	}
	delete(config, "access_token")
	config["api_key"] = strings.TrimSpace(apiKey)
	if strings.TrimSpace(baseURL) != "" {
		config["base_url"] = strings.TrimSpace(baseURL)
	}
	return writeRawConsoleConfigIn(root, accountID, config)
}

// FinishConsoleLogin removes the temporary model credential and verifies that
// the browser flow left a reusable console access token behind.
func FinishConsoleLogin(accountID string) error {
	return FinishConsoleLoginIn(DefaultConsoleRoot(), accountID)
}

func FinishConsoleLoginIn(root, accountID string) error {
	config, err := readRawConsoleConfigIn(root, accountID)
	if err != nil {
		return err
	}
	token, _ := config["access_token"].(string)
	if strings.TrimSpace(token) == "" {
		if err := StripTemporaryLoginKeyIn(root, accountID); err != nil {
			return err
		}
		return fmt.Errorf("Alibaba browser authorization did not save a console access token")
	}
	delete(config, "api_key")
	delete(config, "base_url")
	delete(config, previousAccessTokenKey)
	if err := writeRawConsoleConfigIn(root, accountID, config); err != nil {
		return err
	}
	return nil
}

// StripTemporaryLoginKey is safe to call after a failed or interrupted login.
func StripTemporaryLoginKey(accountID string) error {
	return StripTemporaryLoginKeyIn(DefaultConsoleRoot(), accountID)
}

func StripTemporaryLoginKeyIn(root, accountID string) error {
	config, err := readExistingRawConsoleConfigIn(root, accountID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	delete(config, "api_key")
	delete(config, "base_url")
	if previous, _ := config[previousAccessTokenKey].(string); strings.TrimSpace(previous) != "" {
		config["access_token"] = previous
	}
	delete(config, previousAccessTokenKey)
	return writeRawConsoleConfigIn(root, accountID, config)
}

type consoleMetadata struct {
	Account string `json:"account,omitempty"`
}

// ConsoleCredential is the minimal account-isolated state that may be copied
// to a selected Subrouter server. It intentionally excludes the model API key.
type ConsoleCredential struct {
	AccessToken        string `json:"access_token"`
	ConsoleRegion      string `json:"console_region,omitempty"`
	ConsoleSite        string `json:"console_site,omitempty"`
	ConsoleSwitchAgent *int64 `json:"console_switch_agent,omitempty"`
	Account            string `json:"account,omitempty"`
}

func ExportConsoleCredential(accountID string) (ConsoleCredential, error) {
	return ExportConsoleCredentialIn(DefaultConsoleRoot(), accountID)
}

func ExportConsoleCredentialIn(root, accountID string) (ConsoleCredential, error) {
	config, err := readConsoleConfigIn(root, accountID)
	if err != nil {
		return ConsoleCredential{}, err
	}
	if strings.TrimSpace(config.AccessToken) == "" {
		return ConsoleCredential{}, fmt.Errorf("Qwen Token Plan console credential is missing")
	}
	return ConsoleCredential{
		AccessToken:        config.AccessToken,
		ConsoleRegion:      config.ConsoleRegion,
		ConsoleSite:        config.ConsoleSite,
		ConsoleSwitchAgent: config.ConsoleSwitchAgent,
		Account:            ConsoleAccountIn(root, accountID),
	}, nil
}

func SaveConsoleCredential(accountID string, credential ConsoleCredential) error {
	return SaveConsoleCredentialIn(DefaultConsoleRoot(), accountID, credential)
}

func SaveConsoleCredentialIn(root, accountID string, credential ConsoleCredential) error {
	if strings.TrimSpace(credential.AccessToken) == "" {
		return fmt.Errorf("Qwen Token Plan console credential is missing")
	}
	config := map[string]any{
		"access_token": credential.AccessToken,
	}
	if credential.ConsoleRegion != "" {
		config["console_region"] = credential.ConsoleRegion
	}
	if credential.ConsoleSite != "" {
		config["console_site"] = credential.ConsoleSite
	}
	if credential.ConsoleSwitchAgent != nil {
		config["console_switch_agent"] = *credential.ConsoleSwitchAgent
	}
	if err := writeRawConsoleConfigIn(root, accountID, config); err != nil {
		return err
	}
	return SetConsoleAccountIn(root, accountID, credential.Account)
}

// SetConsoleAccount stores a user-facing sign-in label separately from the
// Bailian-managed credential file, which does not expose the login email.
func SetConsoleAccount(accountID, label string) error {
	return SetConsoleAccountIn(DefaultConsoleRoot(), accountID, label)
}

func SetConsoleAccountIn(root, accountID, label string) error {
	label = strings.TrimSpace(label)
	if label == "" {
		err := os.Remove(filepath.Join(ConsoleConfigDirIn(root, accountID), "metadata.json"))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if len(label) > 320 || strings.IndexFunc(label, unicode.IsControl) >= 0 {
		return fmt.Errorf("Qwen console account label contains invalid terminal text")
	}
	dir := ConsoleConfigDirIn(root, accountID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(consoleMetadata{Account: label}, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	path := filepath.Join(dir, "metadata.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func ConsoleAccount(accountID string) string {
	return ConsoleAccountIn(DefaultConsoleRoot(), accountID)
}

func ConsoleAccountIn(root, accountID string) string {
	body, err := os.ReadFile(filepath.Join(ConsoleConfigDirIn(root, accountID), "metadata.json"))
	if err != nil {
		return ""
	}
	var metadata consoleMetadata
	if json.Unmarshal(body, &metadata) != nil {
		return ""
	}
	return strings.TrimSpace(metadata.Account)
}

func readRawConsoleConfig(accountID string) (map[string]any, error) {
	return readRawConsoleConfigIn(DefaultConsoleRoot(), accountID)
}

func readRawConsoleConfigIn(root, accountID string) (map[string]any, error) {
	config, err := readExistingRawConsoleConfigIn(root, accountID)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	return config, err
}

func readExistingRawConsoleConfigIn(root, accountID string) (map[string]any, error) {
	body, err := os.ReadFile(ConsoleConfigPathIn(root, accountID))
	if err != nil {
		return nil, err
	}
	var config map[string]any
	if err := json.Unmarshal(body, &config); err != nil {
		return nil, fmt.Errorf("parse Qwen console credential: %w", err)
	}
	return config, nil
}

func writeRawConsoleConfig(accountID string, config map[string]any) error {
	return writeRawConsoleConfigIn(DefaultConsoleRoot(), accountID, config)
}

func writeRawConsoleConfigIn(root, accountID string, config map[string]any) error {
	dir := ConsoleConfigDirIn(root, accountID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, filepath.Join(dir, "config.json"))
}

func RemoveConsoleCredential(accountID string) error {
	return RemoveConsoleCredentialIn(DefaultConsoleRoot(), accountID)
}

func RemoveConsoleCredentialIn(root, accountID string) error {
	dir := ConsoleConfigDirIn(root, accountID)
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("Qwen console profile is not a safe directory")
	}
	return os.RemoveAll(dir)
}
