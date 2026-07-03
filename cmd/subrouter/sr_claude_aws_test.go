package main

import "testing"

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
