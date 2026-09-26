package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/storepath"
	"github.com/manaflow-ai/subrouter/wake"
)

func TestSyncRecoveryAlarmsBindsRecentCMUXSession(t *testing.T) {
	stateRoot := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", stateRoot)
	now := time.Now().UTC().Truncate(time.Second)
	sessionID := "session-recent"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_subrouter/recovery-status" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode([]recoveryWireState{{Agent: "claude", SessionID: sessionID, Kind: wake.KindClaudeQuota, LastActivityAt: now, LastFailureAt: now, ResetAt: now.Add(time.Minute)}})
	}))
	defer server.Close()
	cmux := filepath.Join(stateRoot, "cmux-fake")
	sessions := `{"sessions":[{"agent":"claude","session_id":"session-recent","surface_id":"surface-1","updated_at":"` + now.Format(time.RFC3339) + `"}]}`
	script := "#!/bin/sh\nif [ \"$1\" = sessions ]; then printf '%s'; else printf 'prompt'; fi\n"
	script = strings.Replace(script, "%s", sessions, 1)
	if err := os.WriteFile(cmux, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	store := wake.NewStore(filepath.Join(storepath.StateDir(), "wake.json"))
	if err := wake.NewConfig(filepath.Join(storepath.StateDir(), "wake-config.json")).SetEnabled("claude", true); err != nil {
		t.Fatal(err)
	}
	if err := syncRecoveryAlarms(store, server.URL, cmux, now.Add(-time.Minute), true); err != nil {
		t.Fatal(err)
	}
	alarms, err := store.List(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(alarms) != 1 || alarms[0].SurfaceID != "surface-1" || alarms[0].Action != "continue" {
		t.Fatalf("alarms=%+v", alarms)
	}
}

func TestSyncRecoveryAlarmsDoesNotEnqueueWhenDisabled(t *testing.T) {
	stateRoot := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", stateRoot)
	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]recoveryWireState{{Agent: "claude", SessionID: "disabled", Kind: wake.KindClaudeQuota, LastActivityAt: now, LastFailureAt: now}})
	}))
	defer server.Close()
	cmux := filepath.Join(stateRoot, "cmux-fake")
	if err := os.WriteFile(cmux, []byte("#!/bin/sh\nprintf '{\"sessions\":[]}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	store := wake.NewStore(filepath.Join(storepath.StateDir(), "wake.json"))
	if err := syncRecoveryAlarms(store, server.URL, cmux, now, true); err != nil {
		t.Fatal(err)
	}
	alarms, err := store.List(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(alarms) != 0 {
		t.Fatalf("disabled automatic recovery enqueued alarms=%+v", alarms)
	}
}

func TestDispatchManualAlarmIgnoresAutomaticDisabledSetting(t *testing.T) {
	stateRoot := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", stateRoot)
	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/_subrouter/recovery-status" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	cmux := filepath.Join(stateRoot, "cmux-fake")
	if err := os.WriteFile(cmux, []byte("#!/bin/sh\nif [ \"$1\" = read-screen ]; then printf prompt; else exit 0; fi\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	store := wake.NewStore(filepath.Join(storepath.StateDir(), "wake.json"))
	if _, err := store.Put(wake.Alarm{Kind: wake.KindClaudeQuota, Agent: "claude", SessionID: "manual", SurfaceID: "surface", Action: "continue", WakeAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Hour), SessionLastActiveAt: now}, now); err != nil {
		t.Fatal(err)
	}
	if err := dispatchDueWakeAlarms(store, server.URL, cmux, 0, now.Add(-time.Minute), &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	alarms, err := store.List(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(alarms) != 1 || alarms[0].Status != wake.StatusCompleted {
		t.Fatalf("manual alarm was not dispatched while disabled: %+v", alarms)
	}
}

func TestSyncRecoveryAlarmsRejectsStaleInitialSession(t *testing.T) {
	stateRoot := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", stateRoot)
	now := time.Now().UTC()
	stale := now.Add(-9 * time.Hour)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]recoveryWireState{{Agent: "codex", SessionID: "stale", Kind: wake.KindCodexQuota, LastActivityAt: stale, LastFailureAt: stale}})
	}))
	defer server.Close()
	cmux := filepath.Join(stateRoot, "cmux-fake")
	if err := os.WriteFile(cmux, []byte("#!/bin/sh\nprintf '{\"sessions\":[]}\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	store := wake.NewStore(filepath.Join(storepath.StateDir(), "wake.json"))
	if err := syncRecoveryAlarms(store, server.URL, cmux, now, true); err != nil {
		t.Fatal(err)
	}
	alarms, err := store.List(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(alarms) != 0 {
		t.Fatalf("stale alarms=%+v", alarms)
	}
}

func TestAlarmDueAtUsesStableBoundedJitter(t *testing.T) {
	alarm := wake.Alarm{ID: "w_jitter", WakeAt: time.Unix(100, 0).UTC(), JitterSeconds: 30}
	first, second := alarmDueAt(alarm), alarmDueAt(alarm)
	if !first.Equal(second) {
		t.Fatalf("jitter is not deterministic: %v vs %v", first, second)
	}
	if first.Before(alarm.WakeAt) || first.After(alarm.WakeAt.Add(30*time.Second)) {
		t.Fatalf("jitter outside bound: %v", first)
	}
}
