package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// Prompt rules answer a permission prompt an agent tab is waiting on. The
// rules are data on the pool's proxy (proxy.PromptRule; see
// `sr auto-resume rules`), so adding one needs no new sr. The resumer only
// reads the tab, finds the pending prompt, applies the first matching rule,
// and reports what it saw, answered or not, to the proxy's dashboard. A
// prompt no rule matches keeps waiting for a person.

// pendingPrompt is a permission prompt a tab is waiting on.
type pendingPrompt struct {
	Question string
	File     string
	// Header holds the lines above the question, where Claude names the
	// file's full path.
	Header []string
}

var (
	promptOptionLine = regexp.MustCompile(`^[❯›>]?\s*1\.\s+\S`)
	promptFileToken  = regexp.MustCompile(`(\S+\.[A-Za-z0-9]+)\?$`)
)

// findPendingPrompt returns the prompt the screen ends on, if any: a question
// line followed by numbered options and a cancel hint, at the bottom.
func findPendingPrompt(screen string) (pendingPrompt, bool) {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	end := len(lines)
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	tail := strings.Join(lines[max(0, end-8):end], "\n")
	if !strings.Contains(tail, "Esc to cancel") && !strings.Contains(tail, "esc to cancel") {
		return pendingPrompt{}, false
	}
	option := -1
	for i := end - 1; i >= 0 && i >= end-10; i-- {
		if promptOptionLine.MatchString(strings.TrimSpace(lines[i])) {
			option = i
			break
		}
	}
	if option < 0 {
		return pendingPrompt{}, false
	}
	for i := option - 1; i >= 0 && i >= option-4; i-- {
		question := strings.TrimSpace(lines[i])
		if strings.HasSuffix(question, "?") {
			prompt := pendingPrompt{Question: question, Header: lines[max(0, i-60):i]}
			if m := promptFileToken.FindStringSubmatch(question); m != nil {
				prompt.File = filepath.Base(m[1])
			}
			return prompt, true
		}
	}
	return pendingPrompt{}, false
}

// promptRuleMatches reports whether rule answers prompt in an agent's tab.
func promptRuleMatches(rule proxy.PromptRule, agent string, prompt pendingPrompt) bool {
	if rule.Agent != agent {
		return false
	}
	question, err := regexp.Compile(rule.Question)
	if err != nil || !question.MatchString(prompt.Question) {
		return false
	}
	switch rule.Files {
	case "":
		return true
	case "claude-memory":
		return isClaudeMemoryFile(prompt)
	default:
		for _, line := range prompt.Header {
			for _, field := range strings.Fields(line) {
				if ok, _ := filepath.Match(rule.Files, field); ok && strings.HasSuffix(field, prompt.File) {
					return true
				}
			}
		}
		return false
	}
}

// isClaudeMemoryFile reports whether the prompt's file is in Claude's own
// memory folder, from the path Claude shows or, when a long diff pushed that
// off the screen, from the file existing in a memory folder.
func isClaudeMemoryFile(prompt pendingPrompt) bool {
	if prompt.File == "" || !strings.HasSuffix(prompt.File, ".md") {
		return false
	}
	if prompt.File == "MEMORY.md" {
		return true
	}
	for _, line := range prompt.Header {
		if strings.Contains(line, "/.claude/projects/") && strings.Contains(line, "/memory/") && strings.Contains(line, prompt.File) {
			return true
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	matches, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", "memory", prompt.File))
	return len(matches) > 0
}

// promptQuietFor is how long a tab must be unchanged before it is read: a
// tab waiting on a person is quiet, and a working one is skipped cheaply.
const promptQuietFor = 5 * time.Second

// answerPrompts applies the pool's prompt rules to every quiet agent tab.
func answerPrompts(serverURL, cmuxPath string, setting proxy.AutoResumeSetting, out io.Writer) {
	sessions, err := readCMUXSessions(cmuxPath)
	if err != nil {
		return
	}
	host, _ := os.Hostname()
	host = strings.Split(host, ".")[0]
	rules := setting.EffectivePromptRules()
	now := time.Now()
	seen := map[string]bool{}
	for _, session := range sessions {
		if (session.Agent != "claude" && session.Agent != "codex") || !setting.Enabled(session.Agent) ||
			session.SurfaceID == "" || seen[session.SurfaceID] {
			continue
		}
		seen[session.SurfaceID] = true
		if updated, err := time.Parse(time.RFC3339Nano, session.UpdatedAt); err == nil && now.Sub(updated) < promptQuietFor {
			continue
		}
		screen, err := readSurface(cmuxPath, session.SurfaceID)
		if err != nil {
			continue
		}
		prompt, ok := findPendingPrompt(screen)
		if !ok {
			continue
		}
		report := proxy.PromptReport{Host: host, Agent: session.Agent, SurfaceID: session.SurfaceID, Question: prompt.Question, File: prompt.File}
		for _, rule := range rules {
			if !promptRuleMatches(rule, session.Agent, prompt) {
				continue
			}
			if exec.Command(cmuxPath, "send", "--surface", session.SurfaceID, rule.Answer).Run() == nil {
				report.AnsweredBy = rule.Note
				if report.AnsweredBy == "" {
					report.AnsweredBy = rule.Question
				}
				io.WriteString(out, now.Format("15:04:05")+" answered "+session.Agent+" prompt: "+prompt.Question+"\n")
			}
			break
		}
		if report.AnsweredBy == "" {
			io.WriteString(out, now.Format("15:04:05")+" waiting on you: "+session.Agent+" in "+session.SurfaceID+": "+prompt.Question+"\n")
		}
		_ = postAutoResume(serverURL, "/_subrouter/auto-resume/prompts", report, nil)
	}
}
