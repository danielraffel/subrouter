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
func answerPrompts(serverURL, cmuxPath string, setting proxy.AutoResumeSetting, stalls *stallTracker, out io.Writer) {
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
			resumeIfStalled(serverURL, cmuxPath, host, session, screen, setting, stalls, now, out)
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

// stallTracker remembers, per tab, how often a stall rule has resumed it, so
// a tab that keeps stopping is retried with backoff rather than hammered.
type stallTracker struct {
	tabs map[string]*stalledTab
}

type stalledTab struct {
	firstSeen time.Time
	lastSent  time.Time
	attempts  int
	line      string
}

func newStallTracker() *stallTracker { return &stallTracker{tabs: map[string]*stalledTab{}} }

// stallBackoff is how long to wait before each resume of a stalled tab.
var stallBackoff = []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute}

// maxStallAttempts stops retrying a tab that never recovers (about 2 hours).
const maxStallAttempts = 14

// findStall returns the screen line a stall rule matches, when it is among
// the last lines of the tab, so an error that has scrolled up is ignored.
func findStall(screen string, rule proxy.StallRule) (string, bool) {
	pattern, err := regexp.Compile(rule.Screen)
	if err != nil {
		return "", false
	}
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	var recent []string
	for i := len(lines) - 1; i >= 0 && len(recent) < 8; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			recent = append(recent, line)
		}
	}
	for _, line := range recent {
		if pattern.MatchString(line) {
			return line, true
		}
	}
	return "", false
}

// resumeStalledTab types continue or /goal resume into a tab stopped on a
// temporary failure, once its backoff has passed. It reports whether it sent.
func (t *stallTracker) resumeStalledTab(cmuxPath, surface, line, screen string, now time.Time) (string, bool) {
	tab := t.tabs[surface]
	if tab == nil || tab.line != line {
		tab = &stalledTab{firstSeen: now, line: line}
		t.tabs[surface] = tab
	}
	if tab.attempts >= maxStallAttempts {
		return "", false
	}
	wait := stallBackoff[min(tab.attempts, len(stallBackoff)-1)]
	since := tab.firstSeen
	if !tab.lastSent.IsZero() {
		since = tab.lastSent
	}
	if now.Sub(since) < wait {
		return "", false
	}
	action := resumeActionForSurface(screen)
	if exec.Command(cmuxPath, "send", "--surface", surface, action+"\n").Run() != nil {
		return "", false
	}
	tab.attempts++
	tab.lastSent = now
	return action, true
}

// forget drops tabs that are no longer stalled.
func (t *stallTracker) forget(surface string) { delete(t.tabs, surface) }

// resumeIfStalled applies the stall rules to a quiet tab with no prompt.
func resumeIfStalled(serverURL, cmuxPath, host string, session cmuxSessionWire, screen string, setting proxy.AutoResumeSetting, stalls *stallTracker, now time.Time, out io.Writer) {
	if stalls == nil {
		return
	}
	for _, rule := range setting.EffectiveStallRules() {
		if rule.Agent != session.Agent {
			continue
		}
		line, stalled := findStall(screen, rule)
		if !stalled {
			continue
		}
		action, sent := stalls.resumeStalledTab(cmuxPath, session.SurfaceID, line, screen, now)
		if !sent {
			return
		}
		io.WriteString(out, now.Format("15:04:05")+" typed "+action+" into "+session.Agent+" tab "+session.SurfaceID+" after: "+line+"\n")
		_ = postAutoResume(serverURL, "/_subrouter/auto-resume/prompts", proxy.PromptReport{
			Host: host, Agent: session.Agent, SurfaceID: session.SurfaceID,
			Question: "stalled on: " + line, AnsweredBy: "typed " + action + " — " + rule.Note,
		}, nil)
		return
	}
	stalls.forget(session.SurfaceID)
}
