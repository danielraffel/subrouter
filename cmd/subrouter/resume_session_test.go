package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	testCodexSessionID  = "01a10852-5eed-7993-bee8-c40693a36dc3"
	testClaudeSessionID = "64823d4f-2711-4ce5-8cf4-2842fa7d3358"
)

func writeTestRollout(t *testing.T, codexHome, dir, sessionID, metaID, provider string) string {
	t.Helper()
	full := filepath.Join(codexHome, dir)
	if err := os.MkdirAll(full, 0o700); err != nil {
		t.Fatal(err)
	}
	meta, err := json.Marshal(map[string]any{
		"type": "session_meta",
		"payload": map[string]any{
			"id":                metaID,
			"model_provider":    provider,
			"base_instructions": map[string]any{"text": strings.Repeat("x", 200_000)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(full, "rollout-2026-10-04T12-09-38-"+sessionID+".jsonl")
	body := string(meta) + "\n" + `{"type":"response_item","payload":{}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeTestTranscript(t *testing.T, configDir, sessionID string) {
	t.Helper()
	dir := filepath.Join(configDir, "projects", "-tmp-project")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

type resumeCall struct {
	via  string
	args []string
}

func runTestResume(t *testing.T, roots sessionSearchRoots, args ...string) (resumeCall, string, error) {
	t.Helper()
	var call resumeCall
	var errOut bytes.Buffer
	err := resumeSession("sr", args, roots, &errOut,
		func(codexArgs []string) error {
			call = resumeCall{via: "codex", args: codexArgs}
			return nil
		},
		func(program string, srArgs []string) error {
			call = resumeCall{via: program, args: srArgs}
			return nil
		},
	)
	return call, errOut.String(), err
}

func TestResumeSessionRoutesSubrouterCodexSessionThroughSrCodex(t *testing.T) {
	home := t.TempDir()
	writeTestRollout(t, home, "sessions/2026/10/04", testCodexSessionID, testCodexSessionID, "subrouter")

	call, stderr, err := runTestResume(t, sessionSearchRoots{codexHome: home}, testCodexSessionID, "--yolo")
	if err != nil {
		t.Fatal(err)
	}
	want := resumeCall{via: "codex", args: []string{"resume", testCodexSessionID, "--yolo"}}
	if !reflect.DeepEqual(call, want) {
		t.Fatalf("call = %+v, want %+v", call, want)
	}
	if stderr != "" {
		t.Fatalf("unexpected stderr %q", stderr)
	}
}

func TestResumeSessionFindsArchivedCodexRollout(t *testing.T) {
	home := t.TempDir()
	writeTestRollout(t, home, "archived_sessions", testCodexSessionID, testCodexSessionID, "subrouter")

	call, _, err := runTestResume(t, sessionSearchRoots{codexHome: home}, testCodexSessionID)
	if err != nil || call.via != "codex" {
		t.Fatalf("call = %+v, err = %v", call, err)
	}
}

func TestResumeSessionNotesAPlainCodexSession(t *testing.T) {
	home := t.TempDir()
	writeTestRollout(t, home, "sessions/2026/10/04", testCodexSessionID, testCodexSessionID, "openai")

	call, stderr, err := runTestResume(t, sessionSearchRoots{codexHome: home}, testCodexSessionID)
	if err != nil || call.via != "codex" {
		t.Fatalf("call = %+v, err = %v", call, err)
	}
	if !strings.Contains(stderr, `model provider "openai"`) {
		t.Fatalf("stderr %q does not name the recorded provider", stderr)
	}
}

func TestResumeSessionRoutesClaudeSessionThroughClaudeProxy(t *testing.T) {
	codexHome := t.TempDir()
	claudeDir := t.TempDir()
	writeTestTranscript(t, claudeDir, testClaudeSessionID)

	call, _, err := runTestResume(t,
		sessionSearchRoots{codexHome: codexHome, claudeConfigDirs: []string{t.TempDir(), claudeDir}},
		testClaudeSessionID, "--permission-mode", "auto")
	if err != nil {
		t.Fatal(err)
	}
	want := resumeCall{via: "sr", args: []string{"claude", "proxy", "--resume", testClaudeSessionID, "--permission-mode", "auto"}}
	if !reflect.DeepEqual(call, want) {
		t.Fatalf("call = %+v, want %+v", call, want)
	}
}

func TestResumeSessionRejectsUnknownAndMalformedIDs(t *testing.T) {
	roots := sessionSearchRoots{codexHome: t.TempDir(), claudeConfigDirs: []string{t.TempDir()}}
	for _, args := range [][]string{
		{},
		{"--yolo"},
		{"../../etc/passwd"},
		{testCodexSessionID},
	} {
		call, _, err := runTestResume(t, roots, args...)
		if err == nil || call.via != "" {
			t.Fatalf("args %q: call = %+v, err = %v; want an error and no launch", args, call, err)
		}
	}
}

func TestResumeSessionNeverTreatsAnIDAsAPattern(t *testing.T) {
	claudeDir := t.TempDir()
	writeTestTranscript(t, claudeDir, testClaudeSessionID)
	roots := sessionSearchRoots{codexHome: t.TempDir(), claudeConfigDirs: []string{claudeDir}}
	for _, id := range []string{"*", "64823d4f-*"} {
		call, _, err := runTestResume(t, roots, id)
		if err == nil || call.via != "" {
			t.Fatalf("id %q: call = %+v, err = %v; a glob must never select a session", id, call, err)
		}
	}
}

func TestCodexRolloutModelProviderRequiresTheSessionsOwnMetadata(t *testing.T) {
	home := t.TempDir()
	path := writeTestRollout(t, home, "sessions/2026/10/04", testCodexSessionID, "01a10852-0000-7000-8000-000000000000", "subrouter")
	if got := codexRolloutModelProvider(path, testCodexSessionID); got != "" {
		t.Fatalf("provider = %q for a rollout whose session_meta names another session", got)
	}
	path = writeTestRollout(t, home, "sessions/2026/10/05", testCodexSessionID, testCodexSessionID, "subrouter")
	if got := codexRolloutModelProvider(path, testCodexSessionID); got != "subrouter" {
		t.Fatalf("provider = %q, want subrouter", got)
	}
}
