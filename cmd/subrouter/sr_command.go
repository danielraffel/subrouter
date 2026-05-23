package main

import (
	"context"
	"io"
	"os/exec"
)

type srCommandRunner interface {
	Run(ctx context.Context, name string, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error
	Output(ctx context.Context, name string, args []string) ([]byte, error)
}

type execSRCommandRunner struct{}

func (execSRCommandRunner) Run(ctx context.Context, name string, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func (execSRCommandRunner) Output(ctx context.Context, name string, args []string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

func (r srRunner) commandRunner() srCommandRunner {
	if r.cmd != nil {
		return r.cmd
	}
	return execSRCommandRunner{}
}
