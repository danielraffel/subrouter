package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/fsutil"
	"github.com/manaflow-ai/subrouter/wake"
)

// AutoResumeSetting is the pool-wide auto-resume switch for each agent. It
// lives on the proxy so one change applies to every machine using the pool.
type AutoResumeSetting struct {
	Claude bool `json:"claude"`
	Codex  bool `json:"codex"`
}

// Enabled reports the switch for an agent ("claude" or "codex").
func (s AutoResumeSetting) Enabled(agent string) bool {
	switch agent {
	case "claude":
		return s.Claude
	case "codex":
		return s.Codex
	}
	return false
}

// autoResumeSettingMu serializes writes to the setting file.
var autoResumeSettingMu sync.Mutex

// ReadAutoResumeSetting returns the saved setting; a missing file is off.
func ReadAutoResumeSetting(path string) (AutoResumeSetting, error) {
	var setting AutoResumeSetting
	if strings.TrimSpace(path) == "" {
		return setting, nil
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return setting, nil
	}
	if err != nil {
		return setting, err
	}
	err = json.Unmarshal(body, &setting)
	return setting, err
}

func writeAutoResumeSetting(path, agent string, enabled bool) (AutoResumeSetting, error) {
	autoResumeSettingMu.Lock()
	defer autoResumeSettingMu.Unlock()
	setting, err := ReadAutoResumeSetting(path)
	if err != nil {
		return setting, err
	}
	switch agent {
	case "claude":
		setting.Claude = enabled
	case "codex":
		setting.Codex = enabled
	default:
		return setting, errors.New("agent must be claude or codex")
	}
	body, err := json.MarshalIndent(setting, "", "  ")
	if err != nil {
		return setting, err
	}
	return setting, fsutil.WriteFileAtomic(path, append(body, '\n'), 0o600)
}

// handleAutoResume serves the pool-wide switch. GET returns it. POST with a
// JSON body {"agent":"claude","enabled":true} changes one agent and returns
// the result; a form POST from the dashboard redirects back to it.
func (s Server) handleAutoResume(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(s.AutoResumeSettingPath) == "" {
		http.Error(w, "auto-resume is not configured on this server", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		setting, err := ReadAutoResumeSetting(s.AutoResumeSettingPath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, setting)
	case http.MethodPost:
		agent, enabled, form := "", false, false
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			if err := r.ParseForm(); err != nil {
				http.Error(w, "invalid form", http.StatusBadRequest)
				return
			}
			agent, enabled, form = r.PostForm.Get("agent"), r.PostForm.Get("enabled") == "true", true
		} else {
			var body struct {
				Agent   string `json:"agent"`
				Enabled bool   `json:"enabled"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
				http.Error(w, "invalid auto-resume update", http.StatusBadRequest)
				return
			}
			agent, enabled = body.Agent, body.Enabled
		}
		setting, err := writeAutoResumeSetting(s.AutoResumeSettingPath, agent, enabled)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if s.Logger != nil {
			s.Logger.Info("auto-resume setting changed", "agent", agent, "enabled", enabled)
		}
		if form {
			http.Redirect(w, r, "/_subrouter/dashboard#auto-resume", http.StatusSeeOther)
			return
		}
		writeJSON(w, setting)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleAutoResumeTest records a quota failure, already past its reset, for
// a test session. `sr auto-resume test` binds that session to its own tab and
// checks that the resumer types into it, without any model request.
func (s Server) handleAutoResumeTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Agent     string `json:"agent"`
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil ||
		(body.Agent != "claude" && body.Agent != "codex") ||
		!strings.HasPrefix(body.SessionID, "sr-auto-resume-test-") {
		http.Error(w, "agent must be claude or codex and session_id must start with sr-auto-resume-test-", http.StatusBadRequest)
		return
	}
	kind := wake.KindClaudeQuota
	if body.Agent == "codex" {
		kind = wake.KindCodexQuota
	}
	now := time.Now().UTC()
	// Failed two minutes ago and already reset, so the resume is due now.
	s.Recovery.RecordQuotaFailure(body.Agent, body.SessionID, kind, "test", now.Add(-2*time.Minute), now.Add(-3*time.Minute))
	writeJSON(w, map[string]string{"agent": body.Agent, "session_id": body.SessionID, "kind": kind})
}
