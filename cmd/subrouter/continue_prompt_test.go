package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/proxy"
	"github.com/manaflow-ai/subrouter/wake"
)

const sharedPrompt = "Your session stopped at 2026-10-13 10:00:01 PDT (2026-10-13T17:00:01Z) when its account ran out of quota; quota is available again. Your last action may not have completed. Before doing anything else, check the real state..."

func fakePromptRunner(out string, code int, err error, seen *[]string) continuePromptRunner {
	return func(cmuxPath string, args ...string) (string, int, error) {
		if seen != nil {
			*seen = append([]string{cmuxPath}, args...)
		}
		return out, code, err
	}
}

func TestContinuePromptUsesTheSharedCmuxPrompt(t *testing.T) {
	var seen []string
	got := continuePrompt(fakePromptRunner(sharedPrompt+"\n", 0, nil, &seen), "/bin/cmux", "codex", "01a10852-5eed-7993-bee8-c40693a36dc3", "quota")
	if got.Skip || got.Text != sharedPrompt {
		t.Fatalf("decision = %+v", got)
	}
	want := []string{"/bin/cmux", "agent-continue-prompt", "--kind", "codex", "--session", "01a10852-5eed-7993-bee8-c40693a36dc3", "--reason", "quota"}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("cmux args = %q, want %q", seen, want)
	}
}

func TestContinuePromptNeverContinuesAnIdleOrAlreadyContinuedSession(t *testing.T) {
	for _, code := range []int{3, 4} {
		got := continuePrompt(fakePromptRunner("", code, nil, nil), "/bin/cmux", "claude", "64823d4f-2711-4ce5-8cf4-2842fa7d3358", "quota")
		if !got.Skip || got.Text != "" {
			t.Fatalf("exit %d: decision = %+v; want skip with no text", code, got)
		}
	}
}

func TestContinuePromptFallsBackForAnOlderCmux(t *testing.T) {
	for _, runner := range []continuePromptRunner{
		fakePromptRunner("", 1, nil, nil),
		fakePromptRunner("", -1, errors.New("exec: not found"), nil),
		fakePromptRunner("", 0, nil, nil),
	} {
		got := continuePrompt(runner, "/bin/cmux", "codex", "01a10852-5eed-7993-bee8-c40693a36dc3", "provider")
		if got.Skip || got.Text != legacyContinueAction {
			t.Fatalf("decision = %+v; want the legacy continue", got)
		}
	}
	var seen []string
	if got := continuePrompt(fakePromptRunner(sharedPrompt, 0, nil, &seen), "/bin/cmux", "codex", "", "provider"); got.Text != legacyContinueAction || seen != nil {
		t.Fatalf("no session id: decision = %+v, cmux called with %q", got, seen)
	}
}

func TestContinueReasonForAlarm(t *testing.T) {
	if got := continueReasonForAlarm(wake.KindCodexProvider); got != "provider" {
		t.Fatalf("provider alarm reason = %q", got)
	}
	if got := continueReasonForAlarm("quota"); got != "quota" {
		t.Fatalf("quota alarm reason = %q", got)
	}
}

func TestResumeStalledTabTypesTheSharedPromptAndSkipsAnIdleSession(t *testing.T) {
	sent := filepath.Join(t.TempDir(), "sent")
	cmux := filepath.Join(t.TempDir(), "cmux")
	if err := os.WriteFile(cmux, []byte("#!/bin/sh\n[ \"$1\" = send ] && printf '%s' \"$4\" >> \""+sent+"\"\n[ \"$1\" = send-key ] && [ \"$4\" = enter ] && printf '<enter>' >> \""+sent+"\"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var codexRule proxy.StallRule
	for _, rule := range proxy.DefaultStallRules() {
		if rule.Agent == "codex" {
			codexRule = rule
		}
	}
	line, ok := findStall(codexCapacityStall, codexRule)
	if !ok {
		t.Fatal("fixture screen is not a stall")
	}
	start := time.Unix(1_800_000_000, 0)

	stalls := newStallTracker()
	stalls.runPrompt = fakePromptRunner(sharedPrompt, 0, nil, nil)
	stalls.resumeStalledTab(cmux, "codex", "01a10852-5eed-7993-bee8-c40693a36dc3", "s1", line, codexCapacityStall, start)
	if action, did := stalls.resumeStalledTab(cmux, "codex", "01a10852-5eed-7993-bee8-c40693a36dc3", "s1", line, codexCapacityStall, start.Add(31*time.Second)); !did || action != sharedPrompt {
		t.Fatalf("resume = %q, %v", action, did)
	}
	if body, _ := os.ReadFile(sent); string(body) != sharedPrompt+"<enter>" {
		t.Fatalf("typed %q", body)
	}

	idle := newStallTracker()
	idle.runPrompt = fakePromptRunner("", 3, nil, nil)
	idle.resumeStalledTab(cmux, "codex", "01a10852-5eed-7993-bee8-c40693a36dc3", "s2", line, codexCapacityStall, start)
	for _, at := range []time.Duration{31 * time.Second, 5 * time.Minute, time.Hour} {
		if _, did := idle.resumeStalledTab(cmux, "codex", "01a10852-5eed-7993-bee8-c40693a36dc3", "s2", line, codexCapacityStall, start.Add(at)); did {
			t.Fatalf("an idle session was continued at +%s", at)
		}
	}
	if body, _ := os.ReadFile(sent); string(body) != sharedPrompt+"<enter>" {
		t.Fatalf("typed into an idle session: %q", body)
	}
}

func TestAlarmInputReplacesContinueAndKeepsOtherActions(t *testing.T) {
	alarm := wake.Alarm{Agent: "codex", SessionID: "01a10852-5eed-7993-bee8-c40693a36dc3", Kind: wake.KindCodexProvider, Action: "continue"}
	var seen []string
	if got := alarmInput(fakePromptRunner(sharedPrompt, 0, nil, &seen), "/bin/cmux", alarm); got.Skip || got.Text != sharedPrompt {
		t.Fatalf("continue alarm = %+v", got)
	}
	if seen[len(seen)-1] != "provider" {
		t.Fatalf("provider alarm asked for reason %q", seen[len(seen)-1])
	}
	if got := alarmInput(fakePromptRunner("", 3, nil, nil), "/bin/cmux", alarm); !got.Skip {
		t.Fatalf("idle session alarm = %+v; want skip", got)
	}
	alarm.Action = "/goal resume"
	seen = nil
	if got := alarmInput(fakePromptRunner(sharedPrompt, 0, nil, &seen), "/bin/cmux", alarm); got.Text != "/goal resume" || seen != nil {
		t.Fatalf("goal alarm = %+v, cmux called with %q", got, seen)
	}
}
