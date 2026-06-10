package main

import (
	"os/exec"
	"testing"
	"time"
)

func TestCloseInteractiveProcessEndsRunningProcess(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	start := time.Now()
	_ = closeInteractiveProcess(cmd, done)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("close took %v, want fast termination", elapsed)
	}
	if cmd.ProcessState == nil {
		t.Fatal("process state missing after close")
	}
	if cmd.ProcessState.Success() {
		t.Fatal("process should have been signaled, not exited cleanly")
	}
}
