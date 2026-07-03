package main

import (
	"strings"
	"testing"
)

func TestEnvWithoutStripsRoutingVars(t *testing.T) {
	environ := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_BASE_URL=http://subrouter-team:31415",
		"ANTHROPIC_AUTH_TOKEN=secret",
		"ANTHROPIC_API_KEY=sk-ant-xyz",
		"CLAUDE_CODE_USE_BEDROCK=1",
		"ANTHROPIC_MODEL=claude-fable-5",
		"HOME=/home/x",
	}
	got := envWithout(environ, claudeRoutingEnvKeys)
	joined := strings.Join(got, "\n")
	for _, banned := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_CODE_USE_BEDROCK"} {
		if strings.Contains(joined, banned) {
			t.Fatalf("%s should have been stripped: %v", banned, got)
		}
	}
	for _, kept := range []string{"PATH=/usr/bin", "HOME=/home/x", "ANTHROPIC_MODEL=claude-fable-5"} {
		if !strings.Contains(joined, kept) {
			t.Fatalf("%s should have been kept: %v", kept, got)
		}
	}
}

func TestBedrockModelID(t *testing.T) {
	cases := map[string]string{
		"":                                 "us.anthropic.claude-fable-5",
		"fable":                            "us.anthropic.claude-fable-5",
		"claude-fable-5":                   "us.anthropic.claude-fable-5",
		"opus":                             "us.anthropic.claude-opus-4-8",
		"sonnet":                           "us.anthropic.claude-sonnet-5",
		"haiku":                            bedrockSmallFastModelID,
		"us.anthropic.claude-fable-5":      "us.anthropic.claude-fable-5",
		"global.anthropic.claude-opus-4-8": "global.anthropic.claude-opus-4-8",
		"some-unknown-id":                  "some-unknown-id",
	}
	for in, want := range cases {
		if got := bedrockModelID(in); got != want {
			t.Errorf("bedrockModelID(%q) = %q, want %q", in, got, want)
		}
	}
}
