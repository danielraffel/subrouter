package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/fsutil"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// DrainFirst lists Codex accounts to use before any other, in order: every
// Codex request, from a new or a running session, goes to the first listed
// account that still has quota. It exists to spend an account down before a
// reset credit on it expires. An exhausted account falls through to the
// next by the usual failover; when none is left, routing is as usual.
type DrainFirst struct {
	Accounts []string `json:"accounts"`
	// Until, when set, ends the preference at that time.
	Until time.Time `json:"until,omitempty"`
	Note  string    `json:"note,omitempty"`
}

var drainFirstMu sync.Mutex

// ReadDrainFirst returns the saved list; a missing file means none.
func ReadDrainFirst(path string) (DrainFirst, error) {
	var drain DrainFirst
	if strings.TrimSpace(path) == "" {
		return drain, nil
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return drain, nil
	}
	if err != nil {
		return drain, err
	}
	return drain, json.Unmarshal(body, &drain)
}

// active reports whether the list applies at now.
func (d DrainFirst) active(now time.Time) bool {
	return len(d.Accounts) > 0 && (d.Until.IsZero() || now.Before(d.Until))
}

// drainFirstAccount returns the first listed account with quota left, if
// the preference is active. Explicit account pins are handled before this.
func (s Server) drainFirstAccount(provider accounts.Provider, available []accounts.Account, scheduler selectacct.Scheduler) (accounts.Account, bool) {
	if provider != accounts.ProviderCodex || s.DrainFirstPath == "" {
		return accounts.Account{}, false
	}
	drain, err := ReadDrainFirst(s.DrainFirstPath)
	if err != nil || !drain.active(time.Now()) {
		return accounts.Account{}, false
	}
	for _, id := range drain.Accounts {
		account, ok := findAccount(available, id)
		if !ok || scheduler.Exhausted(schedulerAccountProvider(account.Provider), account.ID) {
			continue
		}
		return account, true
	}
	return accounts.Account{}, false
}

// handleDrainFirst reads (GET), replaces (PUT, JSON DrainFirst) or clears
// (DELETE) the drain-first list.
func (s Server) handleDrainFirst(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(s.DrainFirstPath) == "" {
		http.Error(w, "drain-first is not configured on this server", http.StatusNotFound)
		return
	}
	drainFirstMu.Lock()
	defer drainFirstMu.Unlock()
	switch r.Method {
	case http.MethodGet:
		drain, err := ReadDrainFirst(s.DrainFirstPath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, drain)
	case http.MethodPut:
		var drain DrainFirst
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&drain); err != nil {
			http.Error(w, "invalid drain-first list", http.StatusBadRequest)
			return
		}
		body, err := json.MarshalIndent(drain, "", "  ")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := fsutil.WriteFileAtomic(s.DrainFirstPath, append(body, '\n'), 0o600); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if s.Logger != nil {
			s.Logger.Info("drain-first list set", "accounts", strings.Join(drain.Accounts, ","), "until", drain.Until)
		}
		writeJSON(w, drain)
	case http.MethodDelete:
		if err := os.Remove(s.DrainFirstPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if s.Logger != nil {
			s.Logger.Info("drain-first list cleared")
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
