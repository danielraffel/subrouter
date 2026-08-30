package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMacOSAutoUpdaterDefersDuringDeploymentTransaction(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "upgrade-inhibited")
	if err := os.WriteFile(marker, []byte("transaction\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	command := exec.Command("bash", filepath.Join(repoRoot, "deploy", "macos", "subrouter-autoupdate.sh"))
	command.Env = append(os.Environ(),
		"SUBROUTER_UPGRADE_INHIBIT_FILE="+marker,
		"SUBROUTER_MUTATION_LOCK_FILE="+filepath.Join(t.TempDir(), "mutation.lock"),
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("autoupdater with active transaction marker: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "worker update deferred") {
		t.Fatalf("autoupdater did not report a deferred update: %s", output)
	}
}

func TestMacOSAutoUpdaterDefersBeforeAnyMutationWhenLeaseIsHeld(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "supervisor-mutation.lock")
	holder := exec.Command("/usr/bin/lockf", "-k", lock, "/bin/sleep", "30")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if holder.ProcessState == nil {
			_ = holder.Process.Kill()
			_, _ = holder.Process.Wait()
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		probe := exec.Command("/usr/bin/lockf", "-s", "-k", "-t", "0", lock, "/usr/bin/true")
		if err := probe.Run(); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("mutation lease holder did not acquire its lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	command := exec.Command("bash", filepath.Join(repoRoot, "deploy", "macos", "subrouter-autoupdate.sh"))
	command.Env = append(os.Environ(), "SUBROUTER_MUTATION_LOCK_FILE="+lock)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("autoupdater with held mutation lease: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "mutation lease; update deferred") {
		t.Fatalf("autoupdater did not defer before mutation: %s", output)
	}
}
