package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexArgsUsesAuthenticatedSubrouterProviderByDefault(t *testing.T) {
	got := codexArgs([]string{"exec", "--cd", "/tmp", "prompt"}, "http://127.0.0.1:31415/v1", "", "")
	want := append([]string{"exec", "--cd", "/tmp", "prompt"}, defaultSubrouterCodexConfigArgs("http://127.0.0.1:31415/v1")...)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestDefaultCodexBaseURLDoesNotGuessSingleConfiguredServer(t *testing.T) {
	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	if err := store.save(srServerFile{Servers: []srServerConfig{{
		Name: "community",
		URL:  "http://100.99.8.37:31415",
	}}}); err != nil {
		t.Fatal(err)
	}

	got, err := defaultCodexBaseURLFor(store)
	if err != nil {
		t.Fatal(err)
	}
	if got != defaultCodexBaseURL {
		t.Fatalf("base URL = %q, want %q", got, defaultCodexBaseURL)
	}
}

func TestDefaultCodexBaseURLUsesExplicitDefaultServer(t *testing.T) {
	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	if err := store.save(srServerFile{
		Default: "community",
		Servers: []srServerConfig{
			{Name: "community", URL: "http://100.99.8.37:31415"},
			{Name: "other", URL: "http://100.99.8.38:31415"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := defaultCodexBaseURLFor(store)
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://100.99.8.37:31415/v1" {
		t.Fatalf("base URL = %q", got)
	}
}

func TestCodexBaseURLUsesServerEnvOverride(t *testing.T) {
	t.Setenv("SUBROUTER_CODEX_SERVER", "other")
	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	if err := store.save(srServerFile{
		Default: "community",
		Servers: []srServerConfig{
			{Name: "community", URL: "http://100.99.8.37:31415"},
			{Name: "other", URL: "http://100.99.8.38:31415"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := codexBaseURL(store)
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://100.99.8.38:31415/v1" {
		t.Fatalf("base URL = %q", got)
	}
}

func TestCodexBaseURLUsesExplicitURLBeforeServerDefault(t *testing.T) {
	t.Setenv("SUBROUTER_CODEX_BASE_URL", "http://explicit.example/v1")
	t.Setenv("SUBROUTER_CODEX_SERVER", "other")
	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}

	got, err := codexBaseURL(store)
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://explicit.example/v1" {
		t.Fatalf("base URL = %q", got)
	}
}

func TestCodexArgsWorksWithoutSubcommand(t *testing.T) {
	got := codexArgs(nil, "http://127.0.0.1:31415/v1", "", "")
	want := defaultSubrouterCodexConfigArgs("http://127.0.0.1:31415/v1")
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestCodexArgsPassesThroughCodexFlags(t *testing.T) {
	got := codexArgs([]string{"--version"}, "http://127.0.0.1:31415/v1", "", "")
	want := append([]string{"--version"}, defaultSubrouterCodexConfigArgs("http://127.0.0.1:31415/v1")...)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestCodexArgsDoesNotInjectIntoUtilitySubcommands(t *testing.T) {
	got := codexArgs([]string{"login", "--help"}, "http://127.0.0.1:31415/v1", "", "")
	want := []string{"login", "--help"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestCodexArgsPreservesRoutingConfigForUtilitySubcommands(t *testing.T) {
	for _, args := range [][]string{
		{"app", "-c", `openai_base_url="http://direct.example/v1"`, "/tmp/project"},
		{"login", `--config=model_provider="openai"`},
		{"-c", `model_provider="openai"`, "mcp-server"},
	} {
		got := codexArgs(args, "http://127.0.0.1:31415/v1", "", "")
		if strings.Join(got, "\x00") != strings.Join(args, "\x00") {
			t.Fatalf("args = %#v, want pass-through %#v", got, args)
		}
	}
}

func TestCodexArgsDoesNotMistakeGlobalOptionValueForUtilitySubcommand(t *testing.T) {
	got := codexArgs(
		[]string{"--model", "app", "write an implementation"},
		"http://127.0.0.1:31415/v1",
		"",
		"",
	)
	if !contains(got, `model_provider="subrouter"`) {
		t.Fatalf("interactive args were mistaken for a utility subcommand: %#v", got)
	}
}

func TestCodexArgsDoesNotMistakeVariadicImageValuesForUtilitySubcommands(t *testing.T) {
	got := codexArgs(
		[]string{"--image", "first.png", "login", "app"},
		"http://127.0.0.1:31415/v1",
		"",
		"",
	)
	if !contains(got, `model_provider="subrouter"`) {
		t.Fatalf("image filenames were mistaken for a utility subcommand: %#v", got)
	}
}

func TestCodexArgsFindsUtilitySubcommandAfterVariadicImagesEndAtOption(t *testing.T) {
	args := []string{"--image", "first.png", "second.png", "--model", "gpt-5.6-sol", "login"}
	got := codexArgs(args, "http://127.0.0.1:31415/v1", "", "")
	if strings.Join(got, "\x00") != strings.Join(args, "\x00") {
		t.Fatalf("args = %#v, want utility pass-through %#v", got, args)
	}
}

func TestCodexArgsInjectsSubrouterBaseURLIntoAppServer(t *testing.T) {
	got := codexArgs([]string{"app-server", "--listen", "off"}, "http://127.0.0.1:31415/v1", "", "")
	want := append([]string{"app-server", "--listen", "off"}, defaultSubrouterCodexConfigArgs("http://127.0.0.1:31415/v1")...)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestCodexArgsInjectsSubrouterProviderIntoRemoteControl(t *testing.T) {
	got := codexArgs([]string{"remote-control", "start"}, "http://127.0.0.1:31415/v1", "", "")
	want := append([]string{"remote-control", "start"}, defaultSubrouterCodexConfigArgs("http://127.0.0.1:31415/v1")...)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestCodexArgsDoesNotInjectIntoDesktopAppLauncher(t *testing.T) {
	got := codexArgs([]string{"app", "/tmp/project"}, "http://127.0.0.1:31415/v1", "", "")
	want := []string{"app", "/tmp/project"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestCodexArgsTreatsUnknownCommandAsInteractivePrompt(t *testing.T) {
	got := codexArgs([]string{"write", "tests"}, "http://127.0.0.1:31415/v1", "", "")
	want := append([]string{"write", "tests"}, defaultSubrouterCodexConfigArgs("http://127.0.0.1:31415/v1")...)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func defaultSubrouterCodexConfigArgs(baseURL string) []string {
	return []string{
		"-c", `model_provider="subrouter"`,
		"-c", `model_providers.subrouter.name="Subrouter"`,
		"-c", `model_providers.subrouter.base_url="` + baseURL + `"`,
		"-c", `model_providers.subrouter.experimental_bearer_token="subrouter"`,
		"-c", `model_providers.subrouter.wire_api="responses"`,
		"-c", `model_providers.subrouter.supports_websockets=true`,
		"-c", `model_providers.subrouter.http_headers={"X-Subrouter-Agent"="codex"}`,
	}
}

func TestCodexArgsInjectsUserEmailWithCustomSubrouterProvider(t *testing.T) {
	got := codexArgs([]string{"exec", "prompt"}, "http://127.0.0.1:31415/v1", "alice@example.com", "")
	want := []string{
		"exec",
		"prompt",
		"-c", `model_provider="subrouter"`,
		"-c", `model_providers.subrouter.name="Subrouter"`,
		"-c", `model_providers.subrouter.base_url="http://127.0.0.1:31415/v1"`,
		"-c", `model_providers.subrouter.experimental_bearer_token="subrouter"`,
		"-c", `model_providers.subrouter.wire_api="responses"`,
		"-c", `model_providers.subrouter.supports_websockets=true`,
		"-c", `model_providers.subrouter.http_headers={"X-Subrouter-Agent"="codex","X-Subrouter-User-Email"="alice@example.com"}`,
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestCodexArgsInjectsAccountIDWithCustomSubrouterProvider(t *testing.T) {
	got := codexArgs([]string{"exec", "prompt"}, "http://127.0.0.1:31415/v1", "", "team-codex-1")
	want := []string{
		"exec",
		"prompt",
		"-c", `model_provider="subrouter"`,
		"-c", `model_providers.subrouter.name="Subrouter"`,
		"-c", `model_providers.subrouter.base_url="http://127.0.0.1:31415/v1"`,
		"-c", `model_providers.subrouter.experimental_bearer_token="subrouter"`,
		"-c", `model_providers.subrouter.wire_api="responses"`,
		"-c", `model_providers.subrouter.supports_websockets=true`,
		"-c", `model_providers.subrouter.http_headers={"X-Subrouter-Agent"="codex","X-Subrouter-Account-ID"="team-codex-1"}`,
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestCodexArgsKeepsCustomProviderAuthInResumableArguments(t *testing.T) {
	got := codexArgs([]string{"exec", "prompt"}, "http://127.0.0.1:31415/v1", "", "team-codex-1")
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, `env_key="SUBROUTER_CODEX_DUMMY_API_KEY"`) {
		t.Fatalf("args depend on process-only environment and will fail when Codex is resumed directly:\n%s", joined)
	}
	if !strings.Contains(joined, `experimental_bearer_token="subrouter"`) {
		t.Fatalf("args lack self-contained provider authentication:\n%s", joined)
	}
}

func TestCodexArgsInjectsUserEmailAndAccountID(t *testing.T) {
	got := codexArgs([]string{"exec", "prompt"}, "http://127.0.0.1:31415/v1", "alice@example.com", "apikey:paid")
	headers := `model_providers.subrouter.http_headers={"X-Subrouter-Agent"="codex","X-Subrouter-User-Email"="alice@example.com","X-Subrouter-Account-ID"="apikey:paid"}`
	if !contains(got, headers) {
		t.Fatalf("args = %#v, want headers %q", got, headers)
	}
}

func TestCodexArgsInjectsModelHeader(t *testing.T) {
	got := codexArgs([]string{"exec", "-m", "GPT-5.3-Codex-Spark", "prompt"}, "http://127.0.0.1:31415/v1", "", "")
	headers := `model_providers.subrouter.http_headers={"X-Subrouter-Agent"="codex","X-Subrouter-Model"="GPT-5.3-Codex-Spark"}`
	if !contains(got, headers) {
		t.Fatalf("args = %#v, want headers %q", got, headers)
	}
}

func TestCodexModelArgSupportsEqualsForm(t *testing.T) {
	if got := codexModelArg([]string{"exec", "--model=GPT-5.3-Codex-Spark"}); got != "GPT-5.3-Codex-Spark" {
		t.Fatalf("model = %q", got)
	}
}

func TestCodexArgsDropsStaleSubrouterOverridesFromResume(t *testing.T) {
	parentOverride := `model_providers={subrouter={name="Parent",base_url="http://parent-table.example/v1",wire_api="responses"}}`
	got := codexArgs([]string{
		"resume",
		"-c", parentOverride,
		"-c", `model_provider="subrouter"`,
		"-c", `model_providers.subrouter={name="Retired",base_url="http://whole-table-retired.invalid/v1",wire_api="responses"}`,
		"--config", `model_providers.subrouter.base_url="http://retired.invalid/v1"`,
		`--config=model_providers.subrouter.supports_websockets=false`,
		`-c=chatgpt_base_url="http://retired.invalid/backend-api"`,
		`-cmodel_providers.subrouter.base_url="http://attached-retired.invalid/v1"`,
		"-c", `model="gpt-5.6-sol"`,
		"thread-id",
	}, "http://current.example/v1", "", "")
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "retired.invalid") || strings.Contains(joined, "supports_websockets=false") {
		t.Fatalf("stale routing override survived:\n%s", joined)
	}
	for _, want := range []string{
		`model_provider="subrouter"`,
		`model_providers.subrouter.base_url="http://current.example/v1"`,
		`model_providers.subrouter.supports_websockets=true`,
		`model="gpt-5.6-sol"`,
		"thread-id",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
	if strings.Count(joined, `model_provider="subrouter"`) != 1 {
		t.Fatalf("model provider was not canonicalized:\n%s", joined)
	}
	if strings.LastIndex(joined, `model_providers.subrouter.base_url="http://current.example/v1"`) <
		strings.Index(joined, parentOverride) {
		t.Fatalf("authoritative provider block does not follow parent-table override:\n%s", joined)
	}
}

func TestSanitizeCodexRoutingArgsPreservesUnrelatedConfigAndMalformedPair(t *testing.T) {
	args := []string{
		"resume",
		"-c", `model_reasoning_effort="high"`,
		"--config=features.foo=true",
		"-c",
	}
	got := sanitizeCodexRoutingArgs(args)
	if strings.Join(got, "\x00") != strings.Join(args, "\x00") {
		t.Fatalf("args = %#v, want %#v", got, args)
	}
}

func TestSanitizeCodexRoutingArgsPreservesArgumentsAfterTerminator(t *testing.T) {
	args := []string{
		"exec",
		"--",
		"-c", `model_provider="openai"`,
		`--config=model_providers.subrouter.base_url="http://prompt.example/v1"`,
	}
	got := sanitizeCodexRoutingArgs(args)
	if strings.Join(got, "\x00") != strings.Join(args, "\x00") {
		t.Fatalf("args = %#v, want positional tail %#v", got, args)
	}
}

func TestCodexArgsDropsLocalProviderOverridesBeforeTerminator(t *testing.T) {
	got := codexArgs(
		[]string{
			"--oss",
			"--local-provider", "ollama",
			"--local-provider=lmstudio",
			"--",
			"--oss", "--local-provider", "prompt-value",
		},
		"http://current.example/v1",
		"",
		"",
	)
	joined := strings.Join(got, "\n")
	terminator := strings.Index(joined, "\n--\n")
	if terminator < 0 {
		t.Fatalf("missing option terminator:\n%s", joined)
	}
	if strings.Contains(joined[:terminator], "--oss") || strings.Contains(joined[:terminator], "--local-provider") {
		t.Fatalf("local-provider override survived before terminator:\n%s", joined)
	}
	if !strings.Contains(joined[terminator:], "--oss\n--local-provider\nprompt-value") {
		t.Fatalf("positional tail was changed:\n%s", joined)
	}
}

func TestCodexArgsPlacesAuthoritativeConfigBeforeTerminator(t *testing.T) {
	got := codexArgs(
		[]string{"exec", "-c", `model_providers={subrouter={base_url="http://parent.example/v1"}}`, "--", "-c", `model_provider="openai"`},
		"http://current.example/v1",
		"",
		"",
	)
	joined := strings.Join(got, "\n")
	current := strings.LastIndex(joined, `model_providers.subrouter.base_url="http://current.example/v1"`)
	terminator := strings.Index(joined, "\n--\n")
	positional := strings.LastIndex(joined, `model_provider="openai"`)
	if current < 0 || terminator < current || positional < terminator {
		t.Fatalf("authoritative config/terminator ordering is unsafe:\n%s", joined)
	}
}

func TestCodexArgsPlacesAuthoritativeConfigAfterOptionsFirstAndInteractiveOverrides(t *testing.T) {
	parent := `model_providers={subrouter={base_url="http://parent.example/v1"}}`
	for _, args := range [][]string{
		{"-c", parent, "exec", "prompt"},
		{"-c", parent, "interactive prompt"},
	} {
		got := codexArgs(args, "http://current.example/v1", "", "")
		joined := strings.Join(got, "\n")
		if strings.LastIndex(joined, `model_providers.subrouter.base_url="http://current.example/v1"`) <
			strings.Index(joined, parent) {
			t.Fatalf("authoritative config does not follow preserved parent override:\n%s", joined)
		}
	}
}

func TestCodexChildEnvMarksSubrouterResumeCommand(t *testing.T) {
	got := codexChildEnv([]string{
		"A=1",
		subrouterCodexLauncherEnv + "=old",
		subrouterCodexResumeCommandEnv + "=old resume",
	}, "local-secret", "subrouter")
	joined := strings.Join(got, "\n")
	for _, want := range []string{
		subrouterCodexLauncherEnv + "=subrouter codex",
		subrouterCodexResumeCommandEnv + "=subrouter codex resume",
		"SUBROUTER_CODEX_DUMMY_API_KEY=local-secret",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
}

func TestUpsertEnvReplacesExistingValue(t *testing.T) {
	got := upsertEnv([]string{"A=1", "SUBROUTER_CODEX_DUMMY_API_KEY=old"}, "SUBROUTER_CODEX_DUMMY_API_KEY", "subrouter")
	want := []string{"A=1", "SUBROUTER_CODEX_DUMMY_API_KEY=subrouter"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("env = %#v, want %#v", got, want)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
