//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSafariClaudeSessionKeys(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "Library/Containers/com.apple.Safari/Data/Library/Cookies")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	fixture := buildBinaryCookiesFixture(
		binaryCookieRecord(".claude.ai", "sessionKey", "/", "sk-ant-safari-key"),
	)
	if err := os.WriteFile(filepath.Join(dir, "Cookies.binarycookies"), fixture, 0600); err != nil {
		t.Fatal(err)
	}
	got := safariClaudeSessionKeys(home)
	if len(got) != 1 || got[0].SessionKey != "sk-ant-safari-key" || got[0].Source != "Safari" {
		t.Fatalf("safariClaudeSessionKeys = %+v", got)
	}
}

func TestSafariClaudeSessionKeysSoftFail(t *testing.T) {
	home := t.TempDir()
	// Missing files and unreadable/corrupt files must not fail the chain.
	if got := safariClaudeSessionKeys(home); len(got) != 0 {
		t.Fatalf("missing file: %+v", got)
	}
	dir := filepath.Join(home, "Library/Cookies")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Cookies.binarycookies"), []byte("junk"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := safariClaudeSessionKeys(home); len(got) != 0 {
		t.Fatalf("corrupt file: %+v", got)
	}
}
