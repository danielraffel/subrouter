package wake

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type Config struct{ path string }
type configFile struct {
	Version int             `json:"version"`
	Enabled map[string]bool `json:"enabled"`
}

func NewConfig(path string) *Config { return &Config{path: path} }

func (c *Config) SetEnabled(agent string, enabled bool) error {
	if agent != "codex" && agent != "claude" {
		return fmt.Errorf("wake agent must be codex or claude")
	}
	state := configFile{Version: 1, Enabled: map[string]bool{"codex": false, "claude": false}}
	if body, err := os.ReadFile(c.path); err == nil && len(body) != 0 {
		if err := json.Unmarshal(body, &state); err != nil {
			return fmt.Errorf("decode wake config: %w", err)
		}
		if state.Enabled == nil {
			state.Enabled = map[string]bool{}
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	state.Enabled[agent] = enabled
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(c.path), ".wake-config-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
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
	return os.Rename(name, c.path)
}

func (c *Config) Enabled(agent string) (bool, error) {
	body, err := os.ReadFile(c.path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var state configFile
	if err := json.Unmarshal(body, &state); err != nil {
		return false, err
	}
	return state.Enabled[agent], nil
}
