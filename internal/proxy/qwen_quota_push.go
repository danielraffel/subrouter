package proxy

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// qwenQuotaPushTTL bounds how long a pushed cookie-derived Qwen quota reading
// is served. Display-only data, so staleness is tolerated generously.
const qwenQuotaPushTTL = 24 * time.Hour

// qwenQuotaPushRecord is one pushed Qwen token-plan quota reading.
type qwenQuotaPushRecord struct {
	Windows   []accounts.UsageWindow `json:"windows"`
	FetchedAt time.Time              `json:"fetched_at"`
	Source    string                 `json:"source,omitempty"`
}

type qwenQuotaPushFile struct {
	Accounts map[string]qwenQuotaPushRecord `json:"accounts"`
}

// qwenQuotaPushStore is the server's JSON-backed store of Qwen quota readings
// pushed by CLI machines whose local browser holds a Qwen Cloud session, so
// clients without one still see quota in usage-status. The console credential
// is telemetry-only, so this overlay never feeds routing decisions.
type qwenQuotaPushStore struct {
	path string
	mu   sync.Mutex
	now  func() time.Time
}

func newQwenQuotaPushStore(path string) *qwenQuotaPushStore {
	return &qwenQuotaPushStore{path: path, now: time.Now}
}

func (s *qwenQuotaPushStore) load() qwenQuotaPushFile {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return qwenQuotaPushFile{Accounts: map[string]qwenQuotaPushRecord{}}
	}
	var file qwenQuotaPushFile
	if err := json.Unmarshal(data, &file); err != nil || file.Accounts == nil {
		return qwenQuotaPushFile{Accounts: map[string]qwenQuotaPushRecord{}}
	}
	return file
}

func (s *qwenQuotaPushStore) record(email string) (qwenQuotaPushRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.load().Accounts[email]
	if !ok || len(record.Windows) == 0 || s.now().Sub(record.FetchedAt) >= qwenQuotaPushTTL {
		return qwenQuotaPushRecord{}, false
	}
	return record, true
}

func (s *qwenQuotaPushStore) set(email string, windows []accounts.UsageWindow, source string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	file := s.load()
	file.Accounts[email] = qwenQuotaPushRecord{Windows: windows, FetchedAt: s.now(), Source: source}
	data, err := json.Marshal(file)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// validQwenQuotaWindow keeps pushed windows to the quota shapes the console
// path produces, so a client cannot inject arbitrary window metadata.
func validQwenQuotaWindow(window accounts.UsageWindow) bool {
	if strings.TrimSpace(window.Name) == "" || window.Feature != "" || window.ExtraUsage != nil {
		return false
	}
	if math.IsNaN(window.UsedPercent) || math.IsInf(window.UsedPercent, 0) ||
		window.UsedPercent < 0 || window.UsedPercent > 100 {
		return false
	}
	return window.LimitWindowSeconds >= 0 && window.ResetAfterSeconds >= 0
}

// handleQwenQuotaPush accepts a pushed cookie-derived Qwen quota reading. It
// is an admin-gated mutating endpoint, same as the other mutating
// /_subrouter APIs.
func (s Server) handleQwenQuotaPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.qwenQuotaPush == nil {
		http.Error(w, "quota store unavailable", http.StatusServiceUnavailable)
		return
	}
	var payload struct {
		Email   string                 `json:"email"`
		Windows []accounts.UsageWindow `json:"windows"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	email := strings.ToLower(strings.TrimSpace(payload.Email))
	if !strings.Contains(email, "@") || len(email) > 254 {
		http.Error(w, "invalid email", http.StatusBadRequest)
		return
	}
	if len(payload.Windows) == 0 || len(payload.Windows) > 8 {
		http.Error(w, "invalid windows", http.StatusBadRequest)
		return
	}
	for _, window := range payload.Windows {
		if !validQwenQuotaWindow(window) {
			http.Error(w, "invalid windows", http.StatusBadRequest)
			return
		}
	}
	if err := s.qwenQuotaPush.set(email, payload.Windows, "push"); err != nil {
		http.Error(w, "quota store write failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// withQwenQuotaPush overlays pushed cookie-derived quota onto Qwen token-plan
// statuses whose console-credential quota is unknown. It sets the quota
// windows, marks quota known, and clears the "login needed" state for
// display; live console readings always win. Routing never reads this store.
func (s Server) withQwenQuotaPush(statuses []AccountUsageStatus) []AccountUsageStatus {
	if s.qwenQuotaPush == nil {
		return statuses
	}
	for i := range statuses {
		if statuses[i].Provider != accounts.ProviderQwenToken || statuses[i].QuotaUsageKnown {
			continue
		}
		email := qwenStatusAccountKey(statuses[i])
		if email == "" {
			continue
		}
		record, ok := s.qwenQuotaPush.record(email)
		if !ok {
			continue
		}
		statuses[i].Windows = append([]accounts.UsageWindow(nil), record.Windows...)
		statuses[i].QuotaUsageKnown = true
		if statuses[i].QuotaStatus == "" || statuses[i].QuotaStatus == "login needed" || statuses[i].QuotaStatus == "error" {
			statuses[i].QuotaStatus = "live"
		}
		if strings.Contains(strings.ToLower(statuses[i].Error), "login") {
			statuses[i].Error = ""
		}
	}
	return statuses
}

// qwenStatusAccountKey returns the status's matchable account identity: the
// console account email when known, else Email when it is itself an email.
func qwenStatusAccountKey(status AccountUsageStatus) string {
	if identity := strings.ToLower(strings.TrimSpace(status.AccountIdentity)); strings.Contains(identity, "@") {
		return identity
	}
	if email := strings.ToLower(strings.TrimSpace(status.Email)); strings.Contains(email, "@") {
		return email
	}
	return ""
}
