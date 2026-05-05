package storepath

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestCodexDirUsesSubrouterStateDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SUBROUTER_STATE_DIR", "")

	got := CodexDir()
	want := filepath.Join(home, ".subrouter", "codex")
	if got != want {
		t.Fatalf("CodexDir = %q, want %q", got, want)
	}
}

func TestCodexDirHonorsStateDirOverride(t *testing.T) {
	home := t.TempDir()
	stateDir := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SUBROUTER_STATE_DIR", stateDir)

	got := CodexDir()
	want := filepath.Join(stateDir, "codex")
	if got != want {
		t.Fatalf("CodexDir = %q, want %q", got, want)
	}
}

func TestCodexDirMigratesLegacyCodexAccounts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SUBROUTER_STATE_DIR", "")

	legacy := filepath.Join(home, ".codex-accounts")
	if err := os.MkdirAll(filepath.Join(legacy, "accounts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "accounts", "alice@example.com.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, ".alice@example.com.json.lock"), []byte("lock"), 0o600); err != nil {
		t.Fatal(err)
	}

	target := CodexDir()
	if _, err := os.Stat(filepath.Join(target, "accounts", "alice@example.com.json")); err != nil {
		t.Fatalf("migrated account missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, ".alice@example.com.json.lock")); !os.IsNotExist(err) {
		t.Fatalf("lock file should not migrate, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacy, "accounts", "alice@example.com.json")); err != nil {
		t.Fatalf("legacy source should be preserved: %v", err)
	}
}

func TestMigrateCodexDirAllowsConcurrentFirstRun(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "legacy")
	target := filepath.Join(root, "state", "codex")
	if err := os.MkdirAll(filepath.Join(legacy, "accounts"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.json", "b.json", "c.json"} {
		if err := os.WriteFile(filepath.Join(legacy, "accounts", name), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- MigrateCodexDir(target, legacy)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"a.json", "b.json", "c.json"} {
		if _, err := os.Stat(filepath.Join(target, "accounts", name)); err != nil {
			t.Fatalf("missing migrated %s: %v", name, err)
		}
	}
}
