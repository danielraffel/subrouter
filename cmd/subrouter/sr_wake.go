package main

import (
	"fmt"
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
	a, err := store.Put(wake.Alarm{Kind: vals["kind"], Agent: vals["agent"], SessionID: vals["session"], SurfaceID: vals["surface"], Machine: vals["machine"], Pool: vals["pool"], Action: vals["action"], WakeAt: now.Add(after), ExpiresAt: now.Add(expiry), JitterSeconds: jitter}, now)
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
