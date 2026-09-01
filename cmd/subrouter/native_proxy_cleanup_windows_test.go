//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRemovePrivateProxyHomeDoesNotFollowNestedJunction(t *testing.T) {
	home := filepath.Join(t.TempDir(), "private-home")
	if err := os.MkdirAll(filepath.Join(home, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	marker := filepath.Join(external, "must-survive")
	if err := os.WriteFile(marker, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	createWindowsTestJunction(t, filepath.Join(home, "child", "outside"), external)

	if err := removePrivateProxyHome(home); err != nil {
		t.Fatalf("remove private home with nested junction: %v", err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "safe" {
		t.Fatalf("cleanup followed nested junction: %q, %v", got, err)
	}
	if _, err := os.Lstat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private home survived cleanup: %v", err)
	}
}

func TestRemovePrivateProxyHomeDoesNotFollowReplacedRootJunction(t *testing.T) {
	home := filepath.Join(t.TempDir(), "private-home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	detached := home + "-detached"
	if err := os.Rename(home, detached); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(detached) })
	external := t.TempDir()
	marker := filepath.Join(external, "must-survive")
	if err := os.WriteFile(marker, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	createWindowsTestJunction(t, home, external)

	if err := removePrivateProxyHome(home); err != nil {
		t.Fatalf("remove replaced root junction: %v", err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "safe" {
		t.Fatalf("cleanup followed replaced root junction: %q, %v", got, err)
	}
	if _, err := os.Lstat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement junction survived cleanup: %v", err)
	}
	if _, err := os.Lstat(detached); err != nil {
		t.Fatalf("detached original home was removed: %v", err)
	}
}

func createWindowsTestJunction(t *testing.T, link, target string) {
	t.Helper()
	output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Fatalf("create test junction: %v: %s", err, output)
	}
}
