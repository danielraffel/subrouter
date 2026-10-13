package main

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/wake"
)

// The continue prompt is built by cmux, which owns restore and the shared
// "verify what finished before continuing" text, and keeps the once-only
// ledger that cmux's own restore and auto-resume also use. Subrouter asks
// `cmux agent-continue-prompt` for it instead of typing a bare "continue".

// legacyContinueAction is what an older cmux without agent-continue-prompt gets.
const legacyContinueAction = "continue"

// continuePromptDecision is the text to type, or why nothing is typed.
type continuePromptDecision struct {
	Text string
	Skip bool
	Note string
}

// continuePromptRunner runs cmux and returns its stdout and exit code.
type continuePromptRunner func(cmuxPath string, args ...string) (string, int, error)

func runCMUXContinuePrompt(cmuxPath string, args ...string) (string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, cmuxPath, args...).Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return string(out), exitErr.ExitCode(), nil
	}
	if err != nil {
		return "", -1, err
	}
	return string(out), 0, nil
}

// continueReasonForAlarm maps a wake alarm to the prompt's stated cause.
func continueReasonForAlarm(kind string) string {
	if kind == wake.KindCodexProvider {
		return "provider"
	}
	return "quota"
}

// continuePrompt asks cmux for the shared continue prompt for one session.
// cmux exits 3 when the session is idle, 4 when this interruption was
// already continued, and 5 when it cannot read the transcript; all three
// mean type nothing. Any other failure means an
// older cmux, which keeps the legacy "continue".
func continuePrompt(run continuePromptRunner, cmuxPath, agent, sessionID, reason string) continuePromptDecision {
	if strings.TrimSpace(sessionID) == "" {
		return continuePromptDecision{Text: legacyContinueAction, Note: "no session id; typed the legacy continue"}
	}
	out, code, err := run(cmuxPath, "agent-continue-prompt", "--kind", agent, "--session", sessionID, "--reason", reason)
	switch {
	case err == nil && code == 0 && strings.TrimSpace(out) != "":
		return continuePromptDecision{Text: strings.TrimSpace(out)}
	case err == nil && code == 3:
		return continuePromptDecision{Skip: true, Note: "session is idle at its prompt; not continued"}
	case err == nil && code == 4:
		return continuePromptDecision{Skip: true, Note: "this interruption was already continued"}
	case err == nil && code == 5:
		return continuePromptDecision{Skip: true, Note: "cmux cannot read this session's transcript; not continued"}
	default:
		return continuePromptDecision{Text: legacyContinueAction, Note: "cmux has no agent-continue-prompt; typed the legacy continue"}
	}
}

// alarmInput is what a due wake alarm types: its action, with "continue"
// replaced by the shared prompt, or a skip for an idle or already continued
// session.
func alarmInput(run continuePromptRunner, cmuxPath string, alarm wake.Alarm) continuePromptDecision {
	if alarm.Action != legacyContinueAction {
		return continuePromptDecision{Text: alarm.Action}
	}
	return continuePrompt(run, cmuxPath, alarm.Agent, alarm.SessionID, continueReasonForAlarm(alarm.Kind))
}
