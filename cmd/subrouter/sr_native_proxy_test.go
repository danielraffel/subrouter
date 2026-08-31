package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

	relay, err := startNativeProxyRelay(upstream.URL+"/t/srt_test", kimiNativeProxy, "sr-native-test-session", "local-proxy-token")
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
	request.Header.Set("Cookie", "vendor_session=local-secret")
	request.Header.Set("X-Subrouter-Account-ID", "untrusted-pin")
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
	if authorization := got.Header.Get("Authorization"); authorization != "Bearer local-proxy-token" {
		t.Fatalf("Authorization = %q, want local proxy token", authorization)
	}
	if key := got.Header.Get("X-Api-Key"); key != "" {
		t.Fatalf("X-Api-Key leaked through relay: %q", key)
	}
	if got.Header.Get("Cookie") != "" || got.Header.Get("X-Subrouter-Account-ID") != "" {
		t.Fatalf("client credential/routing metadata leaked: cookie=%q account=%q", got.Header.Get("Cookie"), got.Header.Get("X-Subrouter-Account-ID"))
	}
	if got.Header.Get("X-Subrouter-Agent") != "kimi" || got.Header.Get("X-Subrouter-Session") != "sr-native-test-session" {
		t.Fatalf("routing headers = agent %q session %q", got.Header.Get("X-Subrouter-Agent"), got.Header.Get("X-Subrouter-Session"))
	}
	if got.Host != strings.TrimPrefix(upstream.URL, "http://") {
		t.Fatalf("upstream Host = %q, want target host", got.Host)
	}

	for _, forbidden := range []string{
		"http://" + relay.listener.Addr().String() + "/kimi/v1/messages",
		relay.URL() + "/_subrouter/accounts",
		relay.URL() + "/qwen-token/v1/chat/completions",
	} {
		response, err := http.Get(forbidden)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("forbidden relay path %q returned %d", forbidden, response.StatusCode)
		}
		select {
		case leaked := <-seen:
			t.Fatalf("forbidden relay path reached upstream: %s", leaked.URL)
		default:
		}
	}
}

func TestNativeProxyUsesConfiguredLocalDaemonTokenWithoutExposingItToChild(t *testing.T) {
	root := "http://127.0.0.1:43213"
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", root)
	configPath := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte(`{"version":1,"baseUrl":"https://cmux.com","credentialSource":"local","localProxyToken":"local-daemon-secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := nativeProxyServerToken(root + "/tenantless")
	if err != nil {
		t.Fatal(err)
	}
	if token != "local-daemon-secret" {
		t.Fatalf("local daemon token selected = %q", token)
	}
	if remoteToken, err := nativeProxyServerToken("https://router.example.test"); err != nil || remoteToken != "subrouter" {
		t.Fatalf("remote placeholder = %q err=%v", remoteToken, err)
	}
	if selectedLoopbackToken, err := nativeProxyServerToken(root + "/selected-server"); err != nil || selectedLoopbackToken != "local-daemon-secret" {
		t.Fatalf("selected loopback token = %q err=%v", selectedLoopbackToken, err)
	}
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", "http://localhost:43213")
	if pinnedLoopbackToken, err := nativeProxyServerToken("http://127.0.0.1:43213"); err != nil || pinnedLoopbackToken != "local-daemon-secret" {
		t.Fatalf("pinned loopback token = %q err=%v", pinnedLoopbackToken, err)
	}
	env, cleanup, err := nativeProxyEnvironment(kimiNativeProxy, "http://127.0.0.1:43214/capability", os.Environ(), nil)
	cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(env, "\n"), "local-daemon-secret") {
		t.Fatal("local daemon token leaked into the vendor child environment")
	}
}

func TestNativeProxyEnvironmentsReplaceRoutingCredentialsWithoutExposingScope(t *testing.T) {
	original := []string{
		"PATH=/usr/bin", "KEEP_ME=yes", "KIMI_CODE_HOME=/custom/kimi-home", "QWEN_HOME=/custom/qwen-home",
		"OPENAI_API_KEY=real-openai-secret", "OPENAI_BASE_URL=https://vendor.invalid/v1",
		"BAILIAN_TOKEN_PLAN_API_KEY=real-bailian-secret", "KIMI_MODEL_API_KEY=real-kimi-secret",
	}
	qwenRelay := "http://127.0.0.1:43210/private-relay-capability"
	qwenProviderURL := qwenRelay + "/qwen-token/v1"
	qwenEnv, qwenCleanup, err := nativeProxyEnvironment(qwenNativeProxy, qwenRelay, original, []string{"--model", "qwen-test-model"})
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
	if got := testEnvValue(qwenEnv, "OPENAI_BASE_URL"); got != qwenProviderURL {
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
	if len(providers) != 1 || providers[0].ID != "qwen-test-model" || providers[0].BaseURL != qwenProviderURL {
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
	baseURL := "http://127.0.0.1:43210/private-relay-capability/qwen-token/v1"
	for _, input := range [][]string{
		{"--continue"},
		{"--resume", "session-id", "--model", "qwen-custom"},
		{"-p", "hello", "--model=qwen-equals"},
	} {
		model := qwenProxyModel(input)
		got := qwenNativeProxyArgs(input, model)
		joined := strings.Join(got, " ")
		for _, want := range []string{"--auth-type openai", "--openai-api-key subrouter", "--model " + model} {
			if !strings.Contains(joined, want) {
				t.Fatalf("qwen proxy args %q do not contain %q", got, want)
			}
		}
		if strings.Contains(joined, baseURL) {
			t.Fatalf("qwen proxy args exposed the private relay capability: %q", got)
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
	if got := qwenSystemPolicyConflict([]string{"qwen_code_system_settings_path=C:\\managed\\settings.json"}, "windows"); got != "QWEN_CODE_SYSTEM_SETTINGS_PATH" {
		t.Fatalf("case-insensitive Windows policy conflict = %q", got)
	}
	policyPath := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(policyPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := qwenSystemPolicyConflictAtPaths(nil, []string{policyPath}); got != policyPath {
		t.Fatalf("system policy conflict = %q, want %q", got, policyPath)
	}
}

func TestNativeProxyRejectsResumePickerWithoutStickySessionID(t *testing.T) {
	for _, args := range [][]string{{"--resume"}, {"-r"}, {"--resume="}, {"--resume", ""}, {"-r", "   "}, {"--resume", "--model", "qwen-test"}} {
		if !nativeProxyResumePickerRequested(qwenNativeProxy, args) {
			t.Fatalf("picker resume %q was not detected", args)
		}
	}
	for _, args := range [][]string{{"--resume", "session-id"}, {"-r", "session-id"}, {"--resume=session-id"}, {"--", "--resume"}} {
		if nativeProxyResumePickerRequested(qwenNativeProxy, args) {
			t.Fatalf("explicit/non-option resume %q was rejected", args)
		}
	}
	if err := (srRunner{}).launchQwenProxy(t.Context(), []string{"--resume"}); err == nil || !strings.Contains(err.Error(), "explicit session ID") {
		t.Fatalf("picker launch error = %v", err)
	}
	for _, args := range [][]string{{"--session"}, {"-S"}, {"--session="}, {"--session", ""}, {"-S", "   "}, {"--session", "--model", "kimi-test"}} {
		if !nativeProxyResumePickerRequested(kimiNativeProxy, args) {
			t.Fatalf("Kimi picker resume %q was not detected", args)
		}
	}
	if err := (srRunner{}).launchKimiProxy(t.Context(), []string{"--session"}); err == nil || !strings.Contains(err.Error(), "explicit session ID") {
		t.Fatalf("Kimi picker launch error = %v", err)
	}
}

func TestNativeProxyDataPlanePreflightRejectsLeaseRequiredRouter(t *testing.T) {
	for _, test := range []struct {
		status  int
		wantErr bool
	}{{status: http.StatusNoContent}, {status: http.StatusUnauthorized, wantErr: true}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			if request.Method != http.MethodHead || request.URL.Path != "/" || request.Header.Get("Authorization") != "Bearer relay-token" {
				http.Error(w, "bad preflight", http.StatusBadRequest)
				return
			}
			w.WriteHeader(test.status)
		}))
		runner := srRunner{client: server.Client()}
		err := runner.requireNativeProxyDataPlane(t.Context(), server.URL, "relay-token")
		server.Close()
		if (err != nil) != test.wantErr {
			t.Fatalf("status %d preflight error = %v, wantErr=%t", test.status, err, test.wantErr)
		}
		if err != nil && !strings.Contains(err.Error(), "no vendor CLI was started") {
			t.Fatalf("preflight error did not fail before launch: %v", err)
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

func TestNativeProxySessionIdentitySurvivesInitialLaunchAndResume(t *testing.T) {
	for _, spec := range []nativeProxySpec{kimiNativeProxy, qwenNativeProxy, antigravityNativeProxy} {
		first, err := nativeProxySessionID(spec, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"--resume", "native-id"}, {"--session", "native-id"}, {"--conversation", "native-id"}, {"--continue"}} {
			resumed, resumeErr := nativeProxySessionID(spec, args)
			if resumeErr != nil {
				t.Fatal(resumeErr)
			}
			if resumed != first {
				t.Fatalf("%s initial session %q changed on resume %q to %q", spec.provider, first, args, resumed)
			}
		}
		if !strings.HasPrefix(first, "sr-native-") {
			t.Fatalf("workspace session identity = %q", first)
		}
	}
	qwenID, _ := nativeProxySessionID(qwenNativeProxy, nil)
	kimiID, _ := nativeProxySessionID(kimiNativeProxy, nil)
	if qwenID == kimiID {
		t.Fatal("provider namespaces share a native proxy session ID")
	}
}

func TestAntigravityProxyFailsClosedForDirectGeminiConfiguration(t *testing.T) {
	home := t.TempDir()
	settingsDir := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(settingsDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"modelProvider":"gemini"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, environ := range [][]string{
		{"HOME=" + home},
		{"HOME=" + t.TempDir(), "GEMINI_API_KEY=direct-secret"},
		{"HOME=" + t.TempDir(), "GOOGLE_GEMINI_BASE_URL=https://direct.invalid"},
		{"HOME=" + t.TempDir(), "AGY_ADC_AUTH=1"},
	} {
		_, cleanup, err := nativeProxyEnvironment(antigravityNativeProxy, "http://127.0.0.1:43212", environ, nil)
		cleanup()
		if err == nil || !strings.Contains(err.Error(), "direct provider") {
			t.Fatalf("direct Gemini configuration error = %v", err)
		}
		if strings.Contains(err.Error(), "direct-secret") {
			t.Fatal("direct Gemini error exposed the credential")
		}
	}
	if err := os.WriteFile(settingsPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env, cleanup, err := nativeProxyEnvironment(antigravityNativeProxy, "http://127.0.0.1:43212", []string{"HOME=" + home}, nil)
	cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if got := testEnvValue(env, "CLOUD_CODE_URL"); got != "http://127.0.0.1:43212/antigravity" {
		t.Fatalf("CLOUD_CODE_URL = %q", got)
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
