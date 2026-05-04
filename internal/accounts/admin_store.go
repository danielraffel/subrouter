package accounts

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type AdminKeyEntry struct {
	Label   string `json:"label"`
	Key     string `json:"key"`
	OrgID   string `json:"orgId,omitempty"`
	OrgName string `json:"orgName,omitempty"`
	AddedAt string `json:"addedAt"`
}

type OpenAIProject struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status,omitempty"`
}

type DailyUsage struct {
	Date              string  `json:"date"`
	CostUSD           float64 `json:"costUsd"`
	CostEstimated     bool    `json:"costEstimated,omitempty"`
	InputTokens       int64   `json:"inputTokens"`
	CachedInputTokens int64   `json:"cachedInputTokens"`
	OutputTokens      int64   `json:"outputTokens"`
	Requests          int64   `json:"requests"`
}

type TopModel struct {
	Model  string `json:"model"`
	Tokens int64  `json:"tokens"`
}

type APIKeyUsageSnapshot struct {
	AdminKeyLabel      string       `json:"adminKeyLabel"`
	OrgID              string       `json:"orgId,omitempty"`
	ProjectID          string       `json:"projectId,omitempty"`
	ProjectName        string       `json:"projectName,omitempty"`
	FetchedAt          string       `json:"fetchedAt"`
	TodayUSD           float64      `json:"todayUsd"`
	TodayCostEstimated bool         `json:"todayCostEstimated"`
	WeekUSD            float64      `json:"weekUsd"`
	MonthUSD           float64      `json:"monthUsd"`
	TodayTokens        int64        `json:"todayTokens"`
	WeekTokens         int64        `json:"weekTokens"`
	MonthTokens        int64        `json:"monthTokens"`
	TopModel           *TopModel    `json:"topModel,omitempty"`
	Daily              []DailyUsage `json:"daily"`
}

type cachedUsageSnapshot struct {
	FetchedAt string              `json:"fetchedAt"`
	Snapshot  APIKeyUsageSnapshot `json:"snapshot"`
}

func (s CodexStore) adminKeysDir() string {
	return filepath.Join(s.StoreDir(), "admin-keys")
}

func (s CodexStore) usageCacheDir() string {
	return filepath.Join(s.StoreDir(), "usage-cache")
}

func (s CodexStore) ListAdminKeys() ([]AdminKeyEntry, error) {
	dir := s.adminKeysDir()
	files, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]AdminKeyEntry, 0, len(files))
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, file.Name()))
		if err != nil {
			return nil, err
		}
		var entry AdminKeyEntry
		if err := json.Unmarshal(body, &entry); err != nil {
			return nil, err
		}
		if entry.Label != "" {
			out = append(out, entry)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Label < out[j].Label
	})
	return out, nil
}

func (s CodexStore) FindAdminKey(label string) (AdminKeyEntry, bool, error) {
	path := filepath.Join(s.adminKeysDir(), safeFilename(label)+".json")
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return AdminKeyEntry{}, false, nil
	}
	if err != nil {
		return AdminKeyEntry{}, false, err
	}
	var entry AdminKeyEntry
	if err := json.Unmarshal(body, &entry); err != nil {
		return AdminKeyEntry{}, false, err
	}
	return entry, true, nil
}

func (s CodexStore) SaveAdminKey(entry AdminKeyEntry) error {
	if strings.TrimSpace(entry.Label) == "" {
		return errors.New("admin key label is required")
	}
	if err := os.MkdirAll(s.adminKeysDir(), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return os.WriteFile(filepath.Join(s.adminKeysDir(), safeFilename(entry.Label)+".json"), body, 0o600)
}

func (s CodexStore) RemoveAdminKey(label string) (bool, error) {
	err := os.Remove(filepath.Join(s.adminKeysDir(), safeFilename(label)+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (s CodexStore) PickAdminKeyFor(account StoredCodexAccount) (AdminKeyEntry, bool, error) {
	if account.AdminKeyLabel != "" {
		entry, ok, err := s.FindAdminKey(account.AdminKeyLabel)
		if err != nil || ok {
			return entry, ok, err
		}
	}
	all, err := s.ListAdminKeys()
	if err != nil || len(all) == 0 {
		return AdminKeyEntry{}, false, err
	}
	return all[0], true, nil
}

func (s CodexStore) ReadUsageCache(adminLabel, projectID string, maxAge time.Duration) (APIKeyUsageSnapshot, bool, error) {
	snapshot, ok, err := s.readUsageCache(adminLabel, projectID)
	if err != nil || !ok {
		return snapshot, ok, err
	}
	if maxAge > 0 {
		fetchedAt, err := parseTime(snapshot.FetchedAt)
		if err != nil || nowUTC().Sub(fetchedAt) > maxAge {
			return APIKeyUsageSnapshot{}, false, nil
		}
	}
	return snapshot, true, nil
}

func (s CodexStore) ReadUsageCacheStale(adminLabel, projectID string) (APIKeyUsageSnapshot, bool, error) {
	return s.readUsageCache(adminLabel, projectID)
}

func (s CodexStore) readUsageCache(adminLabel, projectID string) (APIKeyUsageSnapshot, bool, error) {
	path := s.usageCachePath(adminLabel, projectID)
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return APIKeyUsageSnapshot{}, false, nil
	}
	if err != nil {
		return APIKeyUsageSnapshot{}, false, err
	}
	var cached cachedUsageSnapshot
	if err := json.Unmarshal(body, &cached); err != nil {
		return APIKeyUsageSnapshot{}, false, err
	}
	return cached.Snapshot, true, nil
}

func (s CodexStore) WriteUsageCache(snapshot APIKeyUsageSnapshot) error {
	if err := os.MkdirAll(s.usageCacheDir(), 0o700); err != nil {
		return err
	}
	payload := cachedUsageSnapshot{FetchedAt: snapshot.FetchedAt, Snapshot: snapshot}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return os.WriteFile(s.usageCachePath(snapshot.AdminKeyLabel, snapshot.ProjectID), body, 0o600)
}

func (s CodexStore) usageCachePath(adminLabel, projectID string) string {
	if projectID == "" {
		projectID = "all"
	}
	return filepath.Join(s.usageCacheDir(), safeFilename(adminLabel)+"__"+safeFilename(projectID)+".json")
}

func safeFilename(value string) string {
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func parseTime(value string) (time.Time, error) {
	return time.Parse(time.RFC3339, value)
}

func nowUTC() time.Time {
	return time.Now().UTC()
}
