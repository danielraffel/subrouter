package main

import (
	"io"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Prompt rules answer a permission prompt an agent is waiting on, when the
// prompt is one the user has said should never need them. Each rule reads
// the tab's screen, so it only matches what Claude Code actually shows.
//
// The one rule today: Claude asking to create or edit a file in its own
// memory folder (~/.claude/projects/<project>/memory/, or MEMORY.md). Pooled
// sessions reach that folder through a symlink, so Claude treats it as
// outside the working directory and asks every time. New sessions get the
// folder in permissions.additionalDirectories and never ask; this rule keeps
// sessions started before that from waiting.

var claudePermissionQuestion = regexp.MustCompile(`Do you want to (create|make this edit to) ([^\s?]+)\?\s*$`)

// claudeMemoryPromptAnswer returns the key that accepts a Claude prompt to
// create or edit a memory file, or "" when the screen shows anything else.
func claudeMemoryPromptAnswer(screen string) string {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	// Ignore trailing blank lines; the prompt must be what the tab ends on.
	end := len(lines)
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	question := -1
	for i := end - 1; i >= 0 && i >= end-6; i-- {
		if claudePermissionQuestion.MatchString(strings.TrimSpace(lines[i])) {
			question = i
			break
		}
	}
	if question < 0 {
		return ""
	}
	rest := strings.Join(lines[question+1:end], "\n")
	if !strings.Contains(rest, "1. Yes") || !strings.Contains(rest, "Esc to cancel") {
		return ""
	}
	file := claudePermissionQuestion.FindStringSubmatch(strings.TrimSpace(lines[question]))[2]
	if file == "MEMORY.md" {
		return "1"
	}
	if !strings.HasSuffix(file, ".md") {
		return ""
	}
	// The prompt header names the full path; require it to be a memory file.
	start := question - 60
	if start < 0 {
		start = 0
	}
	for _, line := range lines[start:question] {
		if strings.Contains(line, "/.claude/projects/") && strings.Contains(line, "/memory/") && strings.Contains(line, file) {
			return "1"
		}
	}
	return ""
}

// answerClaudeMemoryPrompts runs the prompt rules over every Claude tab.
func answerClaudeMemoryPrompts(cmuxPath string, scope wakeScope, out io.Writer) {
	enabled, err := scope.enabled("claude")
	if err != nil || !enabled {
		return
	}
	sessions, err := readCMUXSessions(cmuxPath)
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, session := range sessions {
		if session.Agent != "claude" || session.SurfaceID == "" || seen[session.SurfaceID] {
			continue
		}
		seen[session.SurfaceID] = true
		screen, err := readSurface(cmuxPath, session.SurfaceID)
		if err != nil {
			continue
		}
		key := claudeMemoryPromptAnswer(screen)
		if key == "" {
			continue
		}
		if err := exec.Command(cmuxPath, "send", "--surface", session.SurfaceID, key).Run(); err == nil && out != nil {
			io.WriteString(out, time.Now().Format("15:04:05")+" approved a Claude memory-file prompt in "+session.SurfaceID+"\n")
		}
	}
}
