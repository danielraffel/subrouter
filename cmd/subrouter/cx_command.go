package main

import (
	"context"
	"io"
	"os/exec"
)

type cxCommandRunner interface {
	Run(ctx context.Context, name string, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error
	Output(ctx context.Context, name string, args []string) ([]byte, error)
}

type execCXCommandRunner struct{}

func (execCXCommandRunner) Run(ctx context.Context, name string, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func (execCXCommandRunner) Output(ctx context.Context, name string, args []string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

func (r cxRunner) commandRunner() cxCommandRunner {
	if r.cmd != nil {
		return r.cmd
	}
	return execCXCommandRunner{}
}
