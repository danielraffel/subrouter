package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/storepath"
	"github.com/manaflow-ai/subrouter/wake"
)

func (r srRunner) wake(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(r.out, "usage: sr wake list|show <id>|schedule [options]|now [codex|claude|all]|cancel <id>|cancel --agent <agent>|cancel --all|enable|disable <agent>")
		return nil
	}
	store := wake.NewStore(storepath.StateDir() + "/wake.json")
	now := time.Now().UTC()
	switch args[0] {
	case "list":
		alarms, err := store.List(now)
		if err != nil {
			return err
		}
		if len(alarms) == 0 {
			fmt.Fprintln(r.out, "no wake alarms")
			return nil
		}
		for _, a := range alarms {
			fmt.Fprintf(r.out, "%s\t%s\t%s\t%s\t%s\t%s\n", a.ID, a.Status, a.Agent, a.Action, a.WakeAt.Local().Format(time.RFC3339), a.SurfaceID)
		}
		return nil
	case "show":
		if len(args) != 2 {
			return fmt.Errorf("usage: sr wake show <id>")
		}
		a, ok, err := store.Get(args[1], now)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("wake alarm %q not found", args[1])
		}
		fmt.Fprintf(r.out, "id=%s status=%s agent=%s action=%s wake_at=%s expires_at=%s session=%s surface=%s machine=%s attempt=%d jitter=%ds\n", a.ID, a.Status, a.Agent, a.Action, a.WakeAt.Format(time.RFC3339), a.ExpiresAt.Format(time.RFC3339), a.SessionID, a.SurfaceID, a.Machine, a.Attempt, a.JitterSeconds)
		return nil
	case "schedule":
		return scheduleWake(store, args[1:], now, r.out)
	case "update":
		return updateWake(store, args[1:], now, r.out)
	case "worker":
		return runWakeWorker(args[1:], store, r.out)
	case "install":
		return installWakeLaunchd(r.out)
	case "uninstall":
		return uninstallWakeLaunchd(r.out)
	case "now":
		return wakeNow(store, args[1:], now, r.out)
	case "cancel":
		return cancelWake(store, args[1:], now, r.out)
	case "enable", "disable":
		if len(args) != 2 || (args[1] != "codex" && args[1] != "claude") {
			return fmt.Errorf("usage: sr wake %s <codex|claude>", args[0])
		}
		// Configuration wiring is intentionally separate from the durable alarm
		// queue; this command currently records the requested policy in the same
		// state root for the shared watcher to consume.
		cfg := wake.NewConfig(storepath.StateDir() + "/wake-config.json")
		if err := cfg.SetEnabled(args[1], args[0] == "enable"); err != nil {
			return err
		}
		fmt.Fprintf(r.out, "automatic %s recovery %s\n", args[1], map[bool]string{true: "enabled", false: "disabled"}[args[0] == "enable"])
		return nil
	default:
		return fmt.Errorf("unknown wake command %q", args[0])
	}
}

const wakeLaunchdLabel = "ai.manaflow.subrouter.wake"

func wakeLaunchdPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", wakeLaunchdLabel+".plist"), nil
}

func installWakeLaunchd(out interface{ Write([]byte) (int, error) }) error {
	path, err := wakeLaunchdPath()
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	logDir := storepath.StateDir()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return err
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>wake</string><string>worker</string></array>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>ThrottleInterval</key><integer>10</integer>
<key>StandardOutPath</key><string>%s</string><key>StandardErrorPath</key><string>%s</string>
</dict></plist>
`, wakeLaunchdLabel, executable, filepath.Join(logDir, "wake-worker.log"), filepath.Join(logDir, "wake-worker.err.log"))
	if err := os.WriteFile(path, []byte(plist), 0o600); err != nil {
		return err
	}
	uid := strconv.Itoa(os.Getuid())
	if err := exec.Command("launchctl", "bootstrap", "gui/"+uid, path).Run(); err != nil {
		return fmt.Errorf("wrote %s but launchctl bootstrap failed: %w", path, err)
	}
	fmt.Fprintf(out, "installed %s\n", path)
	return nil
}

func uninstallWakeLaunchd(out interface{ Write([]byte) (int, error) }) error {
	path, err := wakeLaunchdPath()
	if err != nil {
		return err
	}
	uid := strconv.Itoa(os.Getuid())
	_ = exec.Command("launchctl", "bootout", "gui/"+uid, wakeLaunchdLabel).Run()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Fprintf(out, "uninstalled %s\n", path)
	return nil
}

func runWakeWorker(args []string, store *wake.Store, out interface{ Write([]byte) (int, error) }) error {
	once, interval, spacing, cmuxPath, serverURL := false, 15*time.Second, 5*time.Second, "cmux", "http://127.0.0.1:31415"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--once":
			once = true
		case "--interval", "--spacing":
			if i+1 >= len(args) {
				return fmt.Errorf("%s requires a duration", args[i])
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil || d <= 0 {
				return fmt.Errorf("invalid %s", args[i])
			}
			if args[i] == "--interval" {
				interval = d
			} else {
				spacing = d
			}
			i++
		case "--cmux":
			if i+1 >= len(args) {
				return fmt.Errorf("--cmux requires a path")
			}
			cmuxPath = args[i+1]
			i++
		case "--server":
			if i+1 >= len(args) {
				return fmt.Errorf("--server requires a URL")
			}
			serverURL = strings.TrimRight(args[i+1], "/")
			i++
		default:
			return fmt.Errorf("unknown worker option %q", args[i])
		}
	}
	lock, err := wake.AcquireWorkerLock(storePath(store) + ".worker.lock")
	if err != nil {
		return err
	}
	defer lock()
	startedAt := time.Now().UTC()
	initial := true
	pass := func() error {
		if err := syncRecoveryAlarms(store, serverURL, cmuxPath, startedAt, initial); err != nil {
			return err
		}
		initial = false
		return dispatchDueWakeAlarms(store, serverURL, cmuxPath, spacing, startedAt, out)
	}
	if once {
		return pass()
	}
	if err := pass(); err != nil {
		fmt.Fprintf(out, "wake worker: %v\n", err)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		if err := pass(); err != nil {
			fmt.Fprintf(out, "wake worker: %v\n", err)
		}
	}
	return nil
}

type recoveryWireState struct {
	Agent             string    `json:"agent"`
	SessionID         string    `json:"session_id"`
	Kind              string    `json:"kind"`
	Pool              string    `json:"pool"`
	LastActivityAt    time.Time `json:"last_activity_at"`
	LastFailureAt     time.Time `json:"last_failure_at"`
	ResetAt           time.Time `json:"reset_at"`
	ProviderHealthyAt time.Time `json:"provider_healthy_at"`
	Failures          int       `json:"failures"`
	GoalAttempts      int       `json:"goal_attempts"`
	ContinueSent      bool      `json:"continue_sent"`
}
type cmuxSessionWire struct {
	Agent     string `json:"agent"`
	SessionID string `json:"session_id"`
	SurfaceID string `json:"surface_id"`
	UpdatedAt string `json:"updated_at"`
}
type cmuxSessionsWire struct {
	Sessions []cmuxSessionWire `json:"sessions"`
}

func syncRecoveryAlarms(store *wake.Store, serverURL, cmuxPath string, startedAt time.Time, initial bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(serverURL, "/")+"/_subrouter/recovery-status", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("recovery-status returned %s", resp.Status)
	}
	var states []recoveryWireState
	if err := json.NewDecoder(resp.Body).Decode(&states); err != nil {
		return err
	}
	sessions, err := readCMUXSessions(cmuxPath)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	existing, err := store.List(now)
	if err != nil {
		return err
	}
	for _, state := range states {
		if (state.Agent != "codex" && state.Agent != "claude") || state.LastFailureAt.IsZero() || (!initial && state.LastFailureAt.Before(startedAt)) {
			continue
		}
		var matched cmuxSessionWire
		found := false
		for _, candidate := range sessions {
			if candidate.Agent == state.Agent && candidate.SessionID == state.SessionID {
				matched, found = candidate, true
				break
			}
		}
		if !found || matched.SurfaceID == "" {
			continue
		}
		lastActive := state.LastActivityAt
		if parsed, err := time.Parse(time.RFC3339, matched.UpdatedAt); err == nil && parsed.After(lastActive) {
			lastActive = parsed
		}
		if !wake.EligibleForAutomatic(lastActive, now, initial, !initial) {
			continue
		}
		already := false
		for _, alarm := range existing {
			if alarm.Agent == state.Agent && alarm.SessionID == state.SessionID && alarm.Kind == state.Kind && alarm.ObservedAt.Equal(state.LastFailureAt) {
				already = true
				break
			}
		}
		if already {
			continue
		}
		wakeAt := state.LastFailureAt.Add(time.Minute)
		if state.ResetAt.After(wakeAt) {
			wakeAt = state.ResetAt.Add(2 * time.Minute)
		}
		action := "continue"
		if state.Agent == "codex" && state.Kind == wake.KindCodexProvider {
			action = "/goal resume"
			policy := wake.DefaultGoalResumePolicy()
			next := policy.Next(wake.ResumeState{Failures: state.Failures, GoalAttempts: state.GoalAttempts, ContinueSent: state.ContinueSent, LastFailureAt: state.LastFailureAt, GenerationBegan: false}, now, !state.ProviderHealthyAt.IsZero() && state.ProviderHealthyAt.After(state.LastFailureAt))
			switch next {
			case wake.ResumeWait, wake.ResumeStop:
				continue
			case wake.ResumeProbe:
				// A probe is deliberately terminally cheap: validate the exact
				// surface before allowing the expensive replay. The provider's
				// next response records whether generation actually began.
				if err := validateSurface(cmuxPath, matched.SurfaceID); err != nil {
					continue
				}
			case wake.ResumeContinue:
				action = "continue"
			}
		}
		_, err = store.Put(wake.Alarm{Kind: state.Kind, Agent: state.Agent, SessionID: state.SessionID, SurfaceID: matched.SurfaceID, Machine: "local", Pool: state.Pool, Action: action, WakeAt: wakeAt, ExpiresAt: wakeAt.Add(7 * 24 * time.Hour), JitterSeconds: 30, SessionLastActiveAt: lastActive, ObservedAt: state.LastFailureAt}, now)
		if err != nil {
			return err
		}
		existing, _ = store.List(now)
	}
	return nil
}

func readCMUXSessions(cmuxPath string) ([]cmuxSessionWire, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body, err := exec.CommandContext(ctx, cmuxPath, "sessions", "--json", "--all").Output()
	if err != nil {
		return nil, fmt.Errorf("cmux sessions: %w", err)
	}
	var payload cmuxSessionsWire
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode cmux sessions: %w", err)
	}
	return payload.Sessions, nil
}

func storePath(store *wake.Store) string {
	// Store intentionally keeps its path private; the worker lock is colocated
	// with the configured state root through this stable default path.
	return storepath.StateDir() + "/wake.json"
}

func dispatchDueWakeAlarms(store *wake.Store, serverURL, cmuxPath string, spacing time.Duration, workerStartedAt time.Time, out interface{ Write([]byte) (int, error) }) error {
	now := time.Now().UTC()
	alarms, err := store.List(now)
	if err != nil {
		return err
	}
	sent := 0
	for _, alarm := range alarms {
		if alarm.Status != wake.StatusScheduled || alarmDueAt(alarm).After(now) {
			continue
		}
		enabled, err := wake.NewConfig(storepath.StateDir() + "/wake-config.json").Enabled(alarm.Agent)
		if err != nil {
			return err
		}
		if !enabled {
			continue
		}
		initialAlarm := alarm.ObservedAt.IsZero() || alarm.ObservedAt.Before(workerStartedAt)
		if alarm.SessionLastActiveAt.IsZero() || !wake.EligibleForAutomatic(alarm.SessionLastActiveAt, now, initialAlarm, !initialAlarm) {
			_, _ = store.Update(alarm.ID, now, func(a *wake.Alarm) error {
				a.Status = wake.StatusStale
				a.LastError = "session is older than initial 8h freshness window"
				return nil
			})
			continue
		}
		if err := validateSurface(cmuxPath, alarm.SurfaceID); err != nil {
			_, _ = store.Update(alarm.ID, now, func(a *wake.Alarm) error { a.Status = wake.StatusStale; a.LastError = err.Error(); return nil })
			continue
		}
		_, _ = store.Update(alarm.ID, now, func(a *wake.Alarm) error { a.Status = wake.StatusFired; a.Attempt++; return nil })
		text := alarm.Action
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		if err := exec.Command(cmuxPath, "send", "--surface", alarm.SurfaceID, text).Run(); err != nil {
			_, _ = store.Update(alarm.ID, now, func(a *wake.Alarm) error { a.Status = wake.StatusFailed; a.LastError = err.Error(); return nil })
			continue
		}
		_ = postRecoveryGeneration(serverURL, alarm)
		_, _ = store.Update(alarm.ID, now, func(a *wake.Alarm) error { a.Status = wake.StatusCompleted; return nil })
		fmt.Fprintf(out, "woke %s on %s\n", alarm.ID, alarm.Agent)
		sent++
		if sent > 0 {
			time.Sleep(spacing)
		}
	}
	return nil
}

func postRecoveryGeneration(serverURL string, alarm wake.Alarm) error {
	body := strings.NewReader(fmt.Sprintf(`{"agent":%q,"session_id":%q,"generation_began":false}`, alarm.Agent, alarm.SessionID))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(serverURL, "/")+"/_subrouter/recovery-status", body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("recovery update returned %s", resp.Status)
	}
	return nil
}

func alarmDueAt(alarm wake.Alarm) time.Time {
	if alarm.JitterSeconds <= 0 {
		return alarm.WakeAt
	}
	digest := sha256.Sum256([]byte(alarm.ID))
	var n uint64
	for _, b := range digest[:8] {
		n = (n << 8) | uint64(b)
	}
	return alarm.WakeAt.Add(time.Duration(n%uint64(alarm.JitterSeconds+1)) * time.Second)
}

func validateSurface(cmuxPath, surface string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cmuxPath, "read-screen", "--surface", surface, "--lines", "80")
	body, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("surface validation failed: %w", err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return fmt.Errorf("surface validation returned an empty screen")
	}
	return nil
}

func updateWake(store *wake.Store, args []string, now time.Time, out interface{ Write([]byte) (int, error) }) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: sr wake update <id> [--delay DURATION] [--expires-in DURATION] [--action ACTION] [--jitter SECONDS]")
	}
	id := args[0]
	vals := map[string]string{}
	for i := 1; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "--") || i+1 >= len(args) {
			return fmt.Errorf("update options must be --name value")
		}
		vals[strings.TrimPrefix(args[i], "--")] = args[i+1]
		i++
	}
	a, err := store.Update(id, now, func(a *wake.Alarm) error {
		if v := vals["delay"]; v != "" {
			d, err := wake.ParseDuration(v)
			if err != nil {
				return err
			}
			a.WakeAt = now.Add(d)
		}
		if v := vals["expires-in"]; v != "" {
			d, err := wake.ParseDuration(v)
			if err != nil {
				return err
			}
			a.ExpiresAt = now.Add(d)
		}
		if v := vals["action"]; v != "" {
			a.Action = v
		}
		if v := vals["jitter"]; v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return fmt.Errorf("jitter must be a non-negative number of seconds")
			}
			a.JitterSeconds = n
		}
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "updated %s for %s at %s\n", a.ID, a.Agent, a.WakeAt.Format(time.RFC3339))
	return nil
}

func scheduleWake(store *wake.Store, args []string, now time.Time, out interface{ Write([]byte) (int, error) }) error {
	vals := map[string]string{"kind": "quota", "action": "continue", "after": "0s", "expires-in": "7d", "jitter": "0"}
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "--") || i+1 >= len(args) {
			return fmt.Errorf("schedule options must be --name value")
		}
		vals[strings.TrimPrefix(args[i], "--")] = args[i+1]
		i++
	}
	for _, key := range []string{"agent", "session", "surface"} {
		if vals[key] == "" {
			return fmt.Errorf("schedule requires --%s", key)
		}
	}
	if vals["kind"] == "quota" {
		if vals["agent"] == "claude" {
			vals["kind"] = wake.KindClaudeQuota
		} else {
			vals["kind"] = wake.KindCodexQuota
		}
	}
	after, err := wake.ParseDuration(vals["after"])
	if err != nil {
		return err
	}
	expiry, err := wake.ParseDuration(vals["expires-in"])
	if err != nil {
		return err
	}
	jitter, err := strconv.Atoi(vals["jitter"])
	if err != nil || jitter < 0 {
		return fmt.Errorf("jitter must be a non-negative number of seconds")
	}
	lastActive := now
	if vals["last-active"] != "" {
		lastActive, err = time.Parse(time.RFC3339, vals["last-active"])
		if err != nil {
			return fmt.Errorf("last-active must be RFC3339: %w", err)
		}
	}
	a, err := store.Put(wake.Alarm{Kind: vals["kind"], Agent: vals["agent"], SessionID: vals["session"], SurfaceID: vals["surface"], Machine: vals["machine"], Pool: vals["pool"], Action: vals["action"], WakeAt: now.Add(after), ExpiresAt: now.Add(expiry), JitterSeconds: jitter, SessionLastActiveAt: lastActive}, now)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "scheduled %s for %s at %s\n", a.ID, a.Agent, a.WakeAt.Format(time.RFC3339))
	return nil
}

func wakeNow(store *wake.Store, args []string, now time.Time, out interface{ Write([]byte) (int, error) }) error {
	agent := ""
	if len(args) > 1 {
		return fmt.Errorf("usage: sr wake now [codex|claude|all]")
	}
	if len(args) == 1 && args[0] != "all" {
		agent = args[0]
	}
	if agent != "" && agent != "codex" && agent != "claude" {
		return fmt.Errorf("usage: sr wake now [codex|claude|all]")
	}
	alarms, err := store.List(now)
	if err != nil {
		return err
	}
	count := 0
	for _, a := range alarms {
		if a.Status != wake.StatusScheduled || (agent != "" && a.Agent != agent) {
			continue
		}
		if _, err := store.Update(a.ID, now, func(x *wake.Alarm) error { x.WakeAt = now; return nil }); err != nil {
			return err
		}
		count++
	}
	fmt.Fprintf(out, "made %d wake alarm(s) eligible now\n", count)
	return nil
}

func cancelWake(store *wake.Store, args []string, now time.Time, out interface{ Write([]byte) (int, error) }) error {
	if len(args) == 1 && args[0] == "--all" {
		n, err := store.CancelAll(now)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "cancelled %d wake alarm(s)\n", n)
		return nil
	}
	if len(args) == 2 && args[0] == "--agent" {
		n, err := store.CancelAgent(args[1], now)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "cancelled %d %s wake alarm(s)\n", n, args[1])
		return nil
	}
	if len(args) != 1 {
		return fmt.Errorf("usage: sr wake cancel <id>|--agent <agent>|--all")
	}
	if _, err := store.Cancel(args[0], now); err != nil {
		return err
	}
	fmt.Fprintf(out, "cancelled %s\n", args[0])
	return nil
}
