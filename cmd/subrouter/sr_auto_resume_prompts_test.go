package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

const memoryCreatePrompt = `Create file · from the bs2-record agent
 …r/codex/claude-proxy/3fa7/projects/-Volumes-Workshop-Code-pulp/memory/whence-stamp.md
 This will modify /Users/me/.claude/projects/-Volumes-Workshop-Code-pulp/memory/whence-stamp.md
 (outside working directory) via a symlink
   1 ---
   2 name: whence-stamp
 Do you want to create whence-stamp.md?
 ❯ 1. Yes
   2. No
 Esc to cancel · Tab to amend
`

// The default rules answer Claude's memory-file prompts, whatever the verb,
// and nothing else.
func TestDefaultPromptRules(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	memory := filepath.Join(home, ".claude", "projects", "-p", "memory")
	if err := os.MkdirAll(memory, 0o700); err != nil {
		t.Fatal(err)
	}
	// A long diff can push the path off the screen; an existing memory file
	// still identifies it.
	if err := os.WriteFile(filepath.Join(memory, "notes.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	options := "\n ❯ 1. Yes\n   2. No\n Esc to cancel · Tab to amend\n"
	for name, tc := range map[string]struct {
		agent, screen string
		want          bool
	}{
		"memory create":           {"claude", memoryCreatePrompt, true},
		"memory overwrite":        {"claude", "  diff\n Do you want to overwrite notes.md?" + options, true},
		"MEMORY.md edit":          {"claude", "  diff\n Do you want to make this edit to MEMORY.md?" + options, true},
		"md outside memory":       {"claude", "Create file\n /Users/me/repo/README.md\n Do you want to create README.md?" + options, false},
		"not markdown":            {"claude", "Create file\n /Users/me/.claude/projects/p/memory/x.sh\n Do you want to create x.sh?" + options, false},
		"bash command":            {"claude", " Bash command\n rm -rf /tmp/x\n Do you want to proceed?" + options, false},
		"codex is not claude":     {"codex", "  diff\n Do you want to overwrite notes.md?" + options, false},
		"prompt already answered": {"claude", memoryCreatePrompt + "\n⏺ Wrote 9 lines\n\n❯ \n────\n  status\n  more\n  more\n  more\n", false},
	} {
		prompt, ok := findPendingPrompt(tc.screen)
		matched := false
		if ok {
			for _, rule := range proxy.DefaultPromptRules() {
				if promptRuleMatches(rule, tc.agent, prompt) {
					matched = true
				}
			}
		}
		if matched != tc.want {
			t.Errorf("%s: matched = %v, want %v (prompt %+v, found %v)", name, matched, tc.want, prompt.Question, ok)
		}
	}
}

// Every pending prompt is found, answered or not, so an unmatched one can be
// reported as waiting.
func TestFindPendingPromptReportsUnmatchedQuestions(t *testing.T) {
	prompt, ok := findPendingPrompt(" Bash command\n curl example.com\n Do you want to proceed?\n ❯ 1. Yes\n   2. Yes, and don't ask again\n   3. No\n Esc to cancel\n")
	if !ok || prompt.Question != "Do you want to proceed?" {
		t.Fatalf("prompt = %+v, %v", prompt, ok)
	}
}
