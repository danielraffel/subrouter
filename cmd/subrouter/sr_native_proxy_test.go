package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestNativeProxyRelayComposesProviderPathAndScrubsClientCredentials(t *testing.T) {
	t.Helper()
	seen := make(chan *http.Request, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		seen <- request.Clone(request.Context())
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	relay, err := startNativeProxyRelay(upstream.URL+"/t/srt_test", kimiNativeProxy)
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	request, err := http.NewRequest(http.MethodPost, relay.URL()+"/kimi/v1/messages?stream=1", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer local-vendor-secret")
	request.Header.Set("X-Api-Key", "local-api-secret")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}

	got := <-seen
	if got.URL.Path != "/t/srt_test/kimi/v1/messages" || got.URL.RawQuery != "stream=1" {
		t.Fatalf("upstream URL = %s, want tenant + provider path", got.URL.String())
	}
	if authorization := got.Header.Get("Authorization"); authorization != "Bearer subrouter" {
		t.Fatalf("Authorization = %q, want placeholder", authorization)
	}
	if key := got.Header.Get("X-Api-Key"); key != "" {
		t.Fatalf("X-Api-Key leaked through relay: %q", key)
	}
	if got.Header.Get("X-Subrouter-Agent") != "kimi" || !strings.HasPrefix(got.Header.Get("X-Subrouter-Session"), "sr-native-") {
		t.Fatalf("routing headers = agent %q session %q", got.Header.Get("X-Subrouter-Agent"), got.Header.Get("X-Subrouter-Session"))
	}
}

func TestNativeProxyEnvironmentsReplaceRoutingCredentialsWithoutExposingScope(t *testing.T) {
	original := []string{
		"PATH=/usr/bin", "KEEP_ME=yes", "KIMI_CODE_HOME=/custom/kimi-home", "QWEN_HOME=/custom/qwen-home",
		"OPENAI_API_KEY=real-openai-secret", "OPENAI_BASE_URL=https://vendor.invalid/v1",
		"BAILIAN_TOKEN_PLAN_API_KEY=real-bailian-secret", "KIMI_MODEL_API_KEY=real-kimi-secret",
	}
	qwenEnv, qwenCleanup, err := nativeProxyEnvironment(qwenNativeProxy, "http://127.0.0.1:43210", original, []string{"--model", "qwen-test-model"})
	if err != nil {
		t.Fatal(err)
	}
	defer qwenCleanup()
	joined := strings.Join(qwenEnv, "\n")
	for _, secret := range []string{"real-openai-secret", "real-bailian-secret", "real-kimi-secret", "vendor.invalid"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("Qwen child environment leaked %q:\n%s", secret, joined)
		}
	}
	if got := testEnvValue(qwenEnv, "OPENAI_BASE_URL"); got != "http://127.0.0.1:43210/qwen-token/v1" {
		t.Fatalf("OPENAI_BASE_URL = %q", got)
	}
	if got := testEnvValue(qwenEnv, "QWEN_HOME"); got != "/custom/qwen-home" {
		t.Fatalf("QWEN_HOME changed from its normal value: %q", got)
	}
	settings := testEnvValue(qwenEnv, "QWEN_CODE_SYSTEM_SETTINGS_PATH")
	body, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	var overlay struct {
		ModelProviders map[string][]struct {
			ID      string `json:"id"`
			BaseURL string `json:"baseUrl"`
		} `json:"modelProviders"`
	}
	if err := json.Unmarshal(body, &overlay); err != nil {
		t.Fatal(err)
	}
	providers := overlay.ModelProviders["openai"]
	if len(providers) != 1 || providers[0].ID != "qwen-test-model" || providers[0].BaseURL != "http://127.0.0.1:43210/qwen-token/v1" {
		t.Fatalf("Qwen overlay = %+v", overlay)
	}

	kimiEnv, kimiCleanup, err := nativeProxyEnvironment(kimiNativeProxy, "http://127.0.0.1:43211", original, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer kimiCleanup()
	if got := testEnvValue(kimiEnv, "KIMI_CODE_HOME"); got != "/custom/kimi-home" {
		t.Fatalf("KIMI_CODE_HOME = %q, want the user's unchanged session home", got)
	}
	if got := testEnvValue(kimiEnv, "KIMI_MODEL_API_KEY"); got != "subrouter" {
		t.Fatalf("KIMI_MODEL_API_KEY = %q", got)
	}
	if strings.Contains(strings.Join(kimiEnv, "\n"), "real-kimi-secret") {
		t.Fatal("Kimi child environment retained the direct credential")
	}
}

func TestQwenNativeProxyArgsForceRoutingAndPreserveChosenModel(t *testing.T) {
	baseURL := "http://127.0.0.1:43210/qwen-token/v1"
	for _, input := range [][]string{
		{"--continue"},
		{"--resume", "session-id", "--model", "qwen-custom"},
		{"-p", "hello", "--model=qwen-equals"},
	} {
		model := qwenProxyModel(input)
		got := qwenNativeProxyArgs(input, model, baseURL)
		joined := strings.Join(got, " ")
		for _, want := range []string{"--auth-type openai", "--openai-api-key subrouter", "--openai-base-url " + baseURL, "--model " + model} {
			if !strings.Contains(joined, want) {
				t.Fatalf("qwen proxy args %q do not contain %q", got, want)
			}
		}
		if strings.Count(joined, "--model") != 1 {
			t.Fatalf("qwen proxy args retained a competing model: %q", got)
		}
	}
}

func TestQwenProxyOverlayRefusesExistingSystemPolicy(t *testing.T) {
	for _, environ := range [][]string{
		{"QWEN_CODE_SYSTEM_SETTINGS_PATH=/managed/settings.json"},
		{"QWEN_CODE_SYSTEM_DEFAULTS_PATH=/managed/defaults.json"},
	} {
		_, cleanup, err := prepareQwenProxyOverlay("http://127.0.0.1/qwen-token/v1", "qwen-test", environ)
		cleanup()
		if err == nil || !strings.Contains(err.Error(), "refusing a proxy overlay") {
			t.Fatalf("policy environment error = %v", err)
		}
	}
}

func TestQwenDefaultSystemPolicyPathsCoverSupportedPlatforms(t *testing.T) {
	for _, test := range []struct {
		goos string
		want string
	}{
		{goos: "darwin", want: "/Library/Application Support/QwenCode/settings.json"},
		{goos: "linux", want: "/etc/qwen-code/settings.json"},
		{goos: "windows", want: `C:\ProgramData`},
	} {
		paths := qwenDefaultSystemPolicyPaths(nil, test.goos)
		if len(paths) != 2 || !strings.Contains(paths[0], test.want) {
			t.Fatalf("%s Qwen policy paths = %q", test.goos, paths)
		}
	}
}

func TestKimiNativeProxyArgsForceEphemeralModelOnNewAndResumedSessions(t *testing.T) {
	for _, input := range [][]string{
		{"--continue"},
		{"--session", "session-id", "--model", "direct/model"},
		{"-p", "hello", "-m", "direct-model"},
	} {
		got := kimiNativeProxyArgs(input)
		joined := strings.Join(got, " ")
		if !strings.Contains(joined, "--model __kimi_env_model__") {
			t.Fatalf("kimi proxy args = %q", got)
		}
		if strings.Contains(joined, "direct-model") || strings.Contains(joined, "direct/model") {
			t.Fatalf("direct model survived proxy args: %q", got)
		}
	}
}

func TestRequireNativeProxyAccountChecksProviderAndAuthMode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/_subrouter/accounts" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		_, _ = io.WriteString(w, `[
			{"id":"antigravity","provider":"antigravity","auth_mode":"oauth"},
			{"id":"qwen-token:work","provider":"qwen-token","auth_mode":"apikey"}
		]`)
	}))
	defer server.Close()
	runner := srRunner{client: server.Client()}
	config := srServerConfig{Name: "test", URL: server.URL}
	if err := runner.requireNativeProxyAccount(context.Background(), config, antigravityNativeProxy); err != nil {
		t.Fatal(err)
	}
	if err := runner.requireNativeProxyAccount(context.Background(), config, qwenNativeProxy); err != nil {
		t.Fatal(err)
	}
	wrong := kimiNativeProxy
	wrong.authMode = accounts.AuthModeOAuth
	if err := runner.requireNativeProxyAccount(context.Background(), config, wrong); err == nil || !strings.Contains(err.Error(), "no routed Kimi oauth account") {
		t.Fatalf("wrong-mode error = %v", err)
	}
}

func testEnvValue(environ []string, key string) string {
	prefix := key + "="
	for _, item := range environ {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimPrefix(item, prefix)
		}
	}
	return ""
}
