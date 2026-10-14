package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/proxy"
	"github.com/manaflow-ai/subrouter/wake"
)

const (
	sharedPrompt      = "Your session stopped at 2026-10-13 10:00:01 PDT (2026-10-13T17:00:01Z) when its account ran out of quota; quota is available again. Your last action may not have completed. Before doing anything else, check the real state..."
	sharedClaimToken  = "eyJmaW5nZXJwcmludCI6ImYifQ=="
	testCodexSession  = "01a10852-5eed-7993-bee8-c40693a36dc3"
	testClaudeSession = "64823d4f-2711-4ce5-8cf4-2842fa7d3358"
)

// fakeCMUX answers agent-continue-prompt like cmux: a dry run replies with
// dryOut and dryCode, a commit with commitCode. Every call is recorded.
type fakeCMUX struct {
	dryOut     string
	dryCode    int
	dryErr     error
	commitCode int
	calls      [][]string
}

func sharedPromptJSON() string {
	body, _ := json.Marshal(map[string]string{"prompt": sharedPrompt, "claim_token": sharedClaimToken})
	return string(body) + "\n"
}

func (f *fakeCMUX) run(cmuxPath string, args ...string) (string, int, error) {
	f.calls = append(f.calls, append([]string{cmuxPath}, args...))
	if slices.Contains(args, "--commit") {
		return "", f.commitCode, nil
	}
	return f.dryOut, f.dryCode, f.dryErr
}

func (f *fakeCMUX) commits() [][]string {
	var out [][]string
	for _, call := range f.calls {
		if slices.Contains(call, "--commit") {
			out = append(out, call)
		}
	}
	return out
}

func TestContinuePromptAsksForADryRunOfTheSharedCmuxPrompt(t *testing.T) {
	cmux := &fakeCMUX{dryOut: sharedPromptJSON()}
	got := continuePrompt(cmux.run, "/bin/cmux", "codex", testCodexSession, "quota")
	if got.Skip || got.Text != sharedPrompt || got.ClaimToken != sharedClaimToken {
		t.Fatalf("decision = %+v", got)
	}
	want := [][]string{{"/bin/cmux", "agent-continue-prompt", "--kind", "codex", "--session", testCodexSession, "--reason", "quota", "--dry-run", "--json"}}
	if !reflect.DeepEqual(cmux.calls, want) {
		t.Fatalf("cmux calls = %q, want %q", cmux.calls, want)
	}
}

func TestContinuePromptNeverContinuesAnIdleOrAlreadyContinuedSession(t *testing.T) {
	for _, code := range []int{3, 4, 5} {
		got := continuePrompt((&fakeCMUX{dryCode: code}).run, "/bin/cmux", "claude", testClaudeSession, "quota")
		if !got.Skip || got.Text != "" {
			t.Fatalf("exit %d: decision = %+v; want skip with no text", code, got)
		}
		if got.AlreadySent != (code == 4) {
			t.Fatalf("exit %d: AlreadySent = %v", code, got.AlreadySent)
		}
	}
}

func TestContinuePromptFallsBackForAnOlderCmux(t *testing.T) {
	for _, cmux := range []*fakeCMUX{
		{dryCode: 1},
		{dryCode: -1, dryErr: errors.New("exec: not found")},
		{dryCode: 0},
		{dryCode: 0, dryOut: sharedPrompt + "\n"},
	} {
		got := continuePrompt(cmux.run, "/bin/cmux", "codex", testCodexSession, "provider")
		if got.Skip || got.Text != legacyContinueAction || got.ClaimToken != "" {
			t.Fatalf("decision = %+v; want the legacy continue", got)
		}
	}
	cmux := &fakeCMUX{dryOut: sharedPromptJSON()}
	if got := continuePrompt(cmux.run, "/bin/cmux", "codex", "", "provider"); got.Text != legacyContinueAction || cmux.calls != nil {
		t.Fatalf("no session id: decision = %+v, cmux called with %q", got, cmux.calls)
	}
}

func TestCommitContinuePromptClaimsWithTheDryRunToken(t *testing.T) {
	cmux := &fakeCMUX{}
	if !commitContinuePrompt(cmux.run, "/bin/cmux", "codex", testCodexSession, continuePromptDecision{Text: sharedPrompt, ClaimToken: sharedClaimToken}) {
		t.Fatal("commit did not report the claim")
	}
	want := [][]string{{"/bin/cmux", "agent-continue-prompt", "--kind", "codex", "--session", testCodexSession, "--commit", sharedClaimToken}}
	if !reflect.DeepEqual(cmux.calls, want) {
		t.Fatalf("cmux calls = %q, want %q", cmux.calls, want)
	}
	cmux = &fakeCMUX{}
	if commitContinuePrompt(cmux.run, "/bin/cmux", "codex", testCodexSession, continuePromptDecision{Text: legacyContinueAction}) || cmux.calls != nil {
		t.Fatalf("a legacy continue was committed: %q", cmux.calls)
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

func stallFixture(t *testing.T, sendExit int) (cmuxPath, sentPath, line string) {
	t.Helper()
	sentPath = filepath.Join(t.TempDir(), "sent")
	cmuxPath = filepath.Join(t.TempDir(), "cmux")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = send ]; then printf '%s' \"$4\" >> \"" + sentPath + "\"; exit " + string(rune('0'+sendExit)) + "; fi\n" +
		"[ \"$1\" = send-key ] && [ \"$4\" = enter ] && printf '<enter>' >> \"" + sentPath + "\"\nexit 0\n"
	if err := os.WriteFile(cmuxPath, []byte(script), 0o755); err != nil {
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
	return cmuxPath, sentPath, line
}

func TestResumeStalledTabTypesTheSharedPromptThenClaimsIt(t *testing.T) {
	cmuxPath, sent, line := stallFixture(t, 0)
	start := time.Unix(1_800_000_000, 0)
	cmux := &fakeCMUX{dryOut: sharedPromptJSON()}
	stalls := newStallTracker()
	stalls.runPrompt = cmux.run
	stalls.resumeStalledTab(cmuxPath, "codex", testCodexSession, "s1", line, codexCapacityStall, start)
	if action, did := stalls.resumeStalledTab(cmuxPath, "codex", testCodexSession, "s1", line, codexCapacityStall, start.Add(31*time.Second)); !did || action != sharedPrompt {
		t.Fatalf("resume = %q, %v", action, did)
	}
	if body, _ := os.ReadFile(sent); string(body) != sharedPrompt+"<enter>" {
		t.Fatalf("typed %q", body)
	}
	if commits := cmux.commits(); len(commits) != 1 || commits[0][len(commits[0])-1] != sharedClaimToken {
		t.Fatalf("commits = %q; want one with the dry-run token", commits)
	}
}

func TestResumeStalledTabDoesNotClaimWhenTypingFails(t *testing.T) {
	cmuxPath, _, line := stallFixture(t, 1)
	start := time.Unix(1_800_000_000, 0)
	cmux := &fakeCMUX{dryOut: sharedPromptJSON()}
	stalls := newStallTracker()
	stalls.runPrompt = cmux.run
	stalls.resumeStalledTab(cmuxPath, "codex", testCodexSession, "s1", line, codexCapacityStall, start)
	if _, did := stalls.resumeStalledTab(cmuxPath, "codex", testCodexSession, "s1", line, codexCapacityStall, start.Add(31*time.Second)); did {
		t.Fatal("a failed send was reported as a resume")
	}
	if commits := cmux.commits(); len(commits) != 0 {
		t.Fatalf("claimed after a failed send: %q", commits)
	}
}

func TestResumeStalledTabAlreadyContinuedDoesNotUseAnAttempt(t *testing.T) {
	cmuxPath, sent, line := stallFixture(t, 0)
	start := time.Unix(1_800_000_000, 0)
	cmux := &fakeCMUX{dryCode: 4}
	stalls := newStallTracker()
	stalls.runPrompt = cmux.run
	stalls.resumeStalledTab(cmuxPath, "codex", testCodexSession, "s1", line, codexCapacityStall, start)
	for i := 1; i <= maxStallAttempts+2; i++ {
		if _, did := stalls.resumeStalledTab(cmuxPath, "codex", testCodexSession, "s1", line, codexCapacityStall, start.Add(time.Duration(i)*time.Hour)); did {
			t.Fatal("typed into a tab whose interruption was already continued")
		}
	}
	if got := stalls.tabs["s1"].attempts; got != 0 {
		t.Fatalf("already-continued skips used %d attempts", got)
	}
	// A later, different interruption on the same tab is still resumed.
	cmux.dryCode, cmux.dryOut = 0, sharedPromptJSON()
	if action, did := stalls.resumeStalledTab(cmuxPath, "codex", testCodexSession, "s1", line, codexCapacityStall, start.Add(10*time.Hour)); !did || action != sharedPrompt {
		t.Fatalf("resume after the skips = %q, %v", action, did)
	}
	if body, _ := os.ReadFile(sent); string(body) != sharedPrompt+"<enter>" {
		t.Fatalf("typed %q", body)
	}
}

func TestResumeStalledTabNeverContinuesAnIdleSession(t *testing.T) {
	cmuxPath, sent, line := stallFixture(t, 0)
	start := time.Unix(1_800_000_000, 0)
	idle := newStallTracker()
	idle.runPrompt = (&fakeCMUX{dryCode: 3}).run
	idle.resumeStalledTab(cmuxPath, "codex", testCodexSession, "s2", line, codexCapacityStall, start)
	for _, at := range []time.Duration{31 * time.Second, 5 * time.Minute, time.Hour} {
		if _, did := idle.resumeStalledTab(cmuxPath, "codex", testCodexSession, "s2", line, codexCapacityStall, start.Add(at)); did {
			t.Fatalf("an idle session was continued at +%s", at)
		}
	}
	if body, _ := os.ReadFile(sent); len(body) != 0 {
		t.Fatalf("typed into an idle session: %q", body)
	}
}

func TestAlarmInputReplacesContinueAndKeepsOtherActions(t *testing.T) {
	alarm := wake.Alarm{Agent: "codex", SessionID: testCodexSession, Kind: wake.KindCodexProvider, Action: "continue"}
	cmux := &fakeCMUX{dryOut: sharedPromptJSON()}
	if got := alarmInput(cmux.run, "/bin/cmux", alarm); got.Skip || got.Text != sharedPrompt || got.ClaimToken != sharedClaimToken {
		t.Fatalf("continue alarm = %+v", got)
	}
	if !slices.Contains(cmux.calls[0], "provider") {
		t.Fatalf("provider alarm asked with %q", cmux.calls[0])
	}
	if got := alarmInput((&fakeCMUX{dryCode: 3}).run, "/bin/cmux", alarm); !got.Skip {
		t.Fatalf("idle session alarm = %+v; want skip", got)
	}
	alarm.Action = "/goal resume"
	cmux = &fakeCMUX{dryOut: sharedPromptJSON()}
	if got := alarmInput(cmux.run, "/bin/cmux", alarm); got.Text != "/goal resume" || cmux.calls != nil {
		t.Fatalf("goal alarm = %+v, cmux called with %q", got, cmux.calls)
	}
}

func TestResumeStalledTabDoesNotClaimAnUnsubmittedPrompt(t *testing.T) {
	root := t.TempDir()
	screen := filepath.Join(root, "screen")
	if err := os.WriteFile(screen, []byte("› "+sharedPrompt+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmuxPath := filepath.Join(root, "cmux")
	script := "#!/bin/sh\nif [ \"$1\" = read-screen ]; then cat \"" + screen + "\"; fi\nexit 0\n"
	if err := os.WriteFile(cmuxPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, line := stallFixture(t, 0)
	previous := submitConfirmDelay
	submitConfirmDelay = time.Millisecond
	t.Cleanup(func() { submitConfirmDelay = previous })
	start := time.Unix(1_800_000_000, 0)
	cmux := &fakeCMUX{dryOut: sharedPromptJSON()}
	stalls := newStallTracker()
	stalls.runPrompt = cmux.run
	stalls.resumeStalledTab(cmuxPath, "codex", testCodexSession, "s1", line, codexCapacityStall, start)
	if _, did := stalls.resumeStalledTab(cmuxPath, "codex", testCodexSession, "s1", line, codexCapacityStall, start.Add(31*time.Second)); !did {
		t.Fatal("an unsubmitted send was not counted as an attempt")
	}
	if !stalls.tabs["s1"].failed {
		t.Fatal("the unsubmitted send was not recorded as failed")
	}
	if commits := cmux.commits(); len(commits) != 0 {
		t.Fatalf("claimed a prompt still sitting in the input box: %q", commits)
	}
}

func TestWakeAlarmCommitsTheClaimAfterASuccessfulSubmit(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", root)
	now := time.Now().UTC()
	store := wake.NewStore(filepath.Join(root, "wake.json"))
	if _, err := store.Put(wake.Alarm{Agent: "codex", Kind: wake.KindCodexProvider, SessionID: testCodexSession, SurfaceID: "surface-1", Action: "continue", WakeAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Hour)}, now); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	sent := filepath.Join(root, "sent")
	cmuxPath := filepath.Join(root, "cmux-fake")
	script := "#!/bin/sh\nif [ \"$1\" = read-screen ]; then printf prompt; elif [ \"$1\" = send ]; then printf '%s' \"$4\" >> \"" + sent + "\"; fi\nexit 0\n"
	if err := os.WriteFile(cmuxPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	previousDelay := submitConfirmDelay
	submitConfirmDelay = time.Millisecond
	t.Cleanup(func() { submitConfirmDelay = previousDelay })
	cmux := &fakeCMUX{dryOut: sharedPromptJSON()}
	previousRun := wakeContinuePromptRun
	wakeContinuePromptRun = cmux.run
	t.Cleanup(func() { wakeContinuePromptRun = previousRun })

	if err := dispatchDueWakeAlarms(store, server.URL, cmuxPath, 0, now, &strings.Builder{}, testConfigScope()); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(sent); string(body) != sharedPrompt {
		t.Fatalf("typed %q", body)
	}
	commits := cmux.commits()
	if len(commits) != 1 || commits[0][len(commits[0])-1] != sharedClaimToken {
		t.Fatalf("commits = %q; want one with the dry-run token", commits)
	}
	alarms, err := store.List(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(alarms) != 1 || alarms[0].Status != wake.StatusCompleted {
		t.Fatalf("alarm = %+v", alarms)
	}
}
