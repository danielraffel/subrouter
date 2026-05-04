package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLaunchAgentPlistUsesMacOSLocalDefaults(t *testing.T) {
	home := "/Users/alice"
	config := daemonConfig{
		Label:            defaultDaemonLabel,
		Addr:             "127.0.0.1:31415",
		InstallPath:      "/Users/alice/bin/subrouter",
		TranscriptsDir:   "/Users/alice/.subrouter/transcripts",
		LogDir:           "/Users/alice/Library/Logs",
		WorkingDirectory: "/Users/alice/fun/subrouter",
		CXSwitchInterval: "10m",
		Path:             defaultDaemonPath("/Users/alice/bin/subrouter"),
	}
	plist, err := launchAgentPlist(config, home)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<string>ai.manaflow.subrouter</string>",
		"<string>/Users/alice/bin/subrouter</string>",
		"<string>serve</string>",
		"<string>--addr</string>",
		"<string>127.0.0.1:31415</string>",
		"<string>--transcripts</string>",
		"<string>/Users/alice/.subrouter/transcripts</string>",
		"<string>--cx-switch-interval</string>",
		"<string>10m</string>",
		"<string>/Users/alice/fun/subrouter</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("plist missing %q:\n%s", want, plist)
		}
	}
}

func TestInstallDaemonWritesPlistAndInstallsExecutableWithoutStarting(t *testing.T) {
	home := t.TempDir()
	config := daemonConfig{
		Label:            defaultDaemonLabel,
		Addr:             "127.0.0.1:31415",
		InstallPath:      filepath.Join(home, "bin", "subrouter"),
		TranscriptsDir:   filepath.Join(home, ".subrouter", "transcripts"),
		LogDir:           filepath.Join(home, "Library", "Logs"),
		WorkingDirectory: home,
		CXSwitchInterval: "10m",
		Path:             defaultDaemonPath(filepath.Join(home, "bin", "subrouter")),
		InstallCXShim:    true,
		CXShimPath:       filepath.Join(home, "bin", "cx"),
		Start:            false,
	}
	if err := installDaemonWithConfig(config, home, commandRunner{}); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		config.InstallPath,
		config.CXShimPath,
		config.TranscriptsDir,
		filepath.Join(home, "Library", "LaunchAgents", "ai.manaflow.subrouter.plist"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected %s: %v", path, err)
		}
	}

	body, err := os.ReadFile(filepath.Join(home, "Library", "LaunchAgents", "ai.manaflow.subrouter.plist"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "<string>10m</string>") {
		t.Fatalf("plist did not preserve auto-switch interval:\n%s", body)
	}
	target, err := os.Readlink(config.CXShimPath)
	if err != nil {
		t.Fatal(err)
	}
	if target != config.InstallPath {
		t.Fatalf("cx shim target = %q, want %q", target, config.InstallPath)
	}
}
