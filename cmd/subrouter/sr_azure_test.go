package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/azureopenai"
)

type azureTestCommandRunner struct {
	output      []byte
	outputCalls [][]string
	runName     string
	runArgs     []string
	runEnv      []string
}

func (r *azureTestCommandRunner) Run(ctx context.Context, name string, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	return r.RunWithEnv(ctx, name, args, nil, stdin, stdout, stderr)
}

func (r *azureTestCommandRunner) RunWithEnv(_ context.Context, name string, args []string, env []string, _ io.Reader, _ io.Writer, _ io.Writer) error {
	r.runName = name
	r.runArgs = append([]string(nil), args...)
	r.runEnv = append([]string(nil), env...)
	return nil
}

func (r *azureTestCommandRunner) Output(_ context.Context, name string, args []string) ([]byte, error) {
	r.outputCalls = append(r.outputCalls, append([]string{name}, args...))
	return append([]byte(nil), r.output...), nil
}

func TestSRAzureAddValidatesCLIThenPersistsMetadata(t *testing.T) {
	dir := t.TempDir()
	cliPath := filepath.Join(dir, "az")
	if err := os.WriteFile(cliPath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	store := azureopenai.Store{Path: filepath.Join(dir, "azure-openai.json")}
	commands := &azureTestCommandRunner{output: []byte(`{"accessToken":"azure-cli-secret","expires_on":4070908800}`)}
	var restarts int
	var out bytes.Buffer
	runner := srRunner{
		program:    "sr",
		in:         strings.NewReader(""),
		out:        &out,
		errOut:     &out,
		cmd:        commands,
		azureStore: store,
		restartDaemon: func() error {
			restarts++
			return nil
		},
	}

	err := runner.run(context.Background(), []string{
		"add", "azure", "work",
		"--endpoint", "https://example.openai.azure.com",
		"--deployment", "codex-deployment",
		"--azure-cli", cliPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantCommand := []string{
		cliPath,
		"account", "get-access-token",
		"--resource", azureopenai.FoundryTokenResource,
		"--output", "json",
	}
	if len(commands.outputCalls) != 1 || !reflect.DeepEqual(commands.outputCalls[0], wantCommand) {
		t.Fatalf("Azure CLI calls = %#v, want %#v", commands.outputCalls, wantCommand)
	}
	if restarts != 1 {
		t.Fatalf("daemon restarts = %d, want 1", restarts)
	}
	profile, ok, err := store.Find("work")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || profile.Deployment != "codex-deployment" || profile.AzureCLI != cliPath {
		t.Fatalf("profile = %#v, found = %t", profile, ok)
	}
	body, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "azure-cli-secret") {
		t.Fatalf("profile store contains access token: %s", body)
	}
	if !strings.Contains(out.String(), "Access tokens are renewed through Azure CLI and are not stored") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestSRAzureCodexBindsCustomProviderToDeployment(t *testing.T) {
	local := healthServer(t, 200)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL+"/v1")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", filepath.Join(t.TempDir(), "cloud.json"))
	t.Setenv("SUBROUTER_CODEX_BIN", "codex-test")

	store := azureopenai.Store{Path: filepath.Join(t.TempDir(), "azure-openai.json")}
	if _, err := store.Save(azureopenai.Profile{
		Name:       "work",
		Endpoint:   "https://example.openai.azure.com",
		Deployment: "codex-deployment",
		AzureCLI:   "/opt/homebrew/bin/az",
	}); err != nil {
		t.Fatal(err)
	}
	commands := &azureTestCommandRunner{output: []byte(`{"accessToken":"azure-cli-secret","expires_on":4070908800}`)}
	runner := srRunner{
		program:    "sr",
		in:         strings.NewReader(""),
		out:        io.Discard,
		errOut:     io.Discard,
		client:     local.Client(),
		cmd:        commands,
		azureStore: store,
	}

	if err := runner.run(context.Background(), []string{"azure", "codex", "work", "exec", "hello"}); err != nil {
		t.Fatal(err)
	}
	if commands.runName != "codex-test" {
		t.Fatalf("command = %q, want codex-test", commands.runName)
	}
	joined := strings.Join(commands.runArgs, "\n")
	for _, want := range []string{
		"exec",
		`model="codex-deployment"`,
		`model_provider="subrouter_azure"`,
		`model_providers.subrouter_azure.base_url="` + local.URL + `/azure/work/v1"`,
		`model_providers.subrouter_azure.experimental_bearer_token="subrouter"`,
		`model_providers.subrouter_azure.wire_api="responses"`,
		`model_providers.subrouter_azure.supports_websockets=false`,
		"hello",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Codex args missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "azure-cli-secret") || strings.Contains(strings.Join(commands.runEnv, "\n"), "azure-cli-secret") {
		t.Fatal("Azure access token was passed to the Codex child")
	}
}

func TestSRAzureCodexRejectsDeploymentOverride(t *testing.T) {
	store := azureopenai.Store{Path: filepath.Join(t.TempDir(), "azure-openai.json")}
	if _, err := store.Save(azureopenai.Profile{
		Name:       "work",
		Endpoint:   "https://example.openai.azure.com",
		Deployment: "codex-deployment",
		AzureCLI:   "/opt/homebrew/bin/az",
	}); err != nil {
		t.Fatal(err)
	}
	runner := srRunner{
		program:    "sr",
		in:         strings.NewReader(""),
		out:        io.Discard,
		errOut:     io.Discard,
		cmd:        &azureTestCommandRunner{},
		azureStore: store,
	}
	err := runner.run(context.Background(), []string{"azure", "codex", "work", "-m", "other-deployment"})
	if err == nil || !strings.Contains(err.Error(), `bound to deployment "codex-deployment"`) {
		t.Fatalf("error = %v", err)
	}
}
