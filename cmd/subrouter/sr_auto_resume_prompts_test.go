package main

import "testing"

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

func TestClaudeMemoryPromptAnswer(t *testing.T) {
	for name, tc := range map[string]struct {
		screen string
		want   string
	}{
		"memory create":           {memoryCreatePrompt, "1"},
		"MEMORY.md edit":          {"  diff lines\n Do you want to make this edit to MEMORY.md?\n ❯ 1. Yes\n   2. No\n Esc to cancel · Tab to amend\n\n", "1"},
		"md outside memory":       {"Create file\n /Users/me/repo/README.md\n Do you want to create README.md?\n ❯ 1. Yes\n   2. No\n Esc to cancel\n", ""},
		"not markdown":            {"Create file\n /Users/me/.claude/projects/p/memory/x.sh\n Do you want to create x.sh?\n ❯ 1. Yes\n   2. No\n Esc to cancel\n", ""},
		"bash command":            {" Bash command\n rm -rf /tmp/x\n Do you want to proceed?\n ❯ 1. Yes\n   2. No\n Esc to cancel\n", ""},
		"prompt already answered": {memoryCreatePrompt + "\n⏺ Wrote 9 lines\n\n❯ \n────\n  status line\n  more\n  more\n", ""},
	} {
		if got := claudeMemoryPromptAnswer(tc.screen); got != tc.want {
			t.Errorf("%s: answer = %q, want %q", name, got, tc.want)
		}
	}
}
