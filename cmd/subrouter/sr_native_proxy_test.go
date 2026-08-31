package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/broker"
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

	relay, err := startNativeProxyRelay(upstream.URL+"/t/srt_test", kimiNativeProxy, "sr-native-test-session", "local-proxy-token", "")
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
	request.Header.Set("OpenAI-Organization", "direct-org-secret")
	request.Header.Set("OpenAI-Project", "direct-project-secret")
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
	if got.Header.Get("Cookie") != "" || got.Header.Get("X-Subrouter-Account-ID") != "" ||
		got.Header.Get("OpenAI-Organization") != "" || got.Header.Get("OpenAI-Project") != "" {
		t.Fatalf("client credential/routing metadata leaked: cookie=%q account=%q org=%q project=%q",
			got.Header.Get("Cookie"), got.Header.Get("X-Subrouter-Account-ID"),
			got.Header.Get("OpenAI-Organization"), got.Header.Get("OpenAI-Project"))
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

func TestNativeProxyRelayInjectsOnlyValidatedPinnedAccount(t *testing.T) {
	seen := make(chan *http.Request, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		seen <- request.Clone(request.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	relay, err := startNativeProxyRelay(upstream.URL, qwenNativeProxy, "pinned-session", "router-token", "qwen-token:work")
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	request, err := http.NewRequest(http.MethodPost, relay.URL()+"/qwen-token/v1/chat/completions", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Subrouter-Account-ID", "untrusted-child-account")
	request.Header.Set("Connection", "Authorization, X-Subrouter-Agent, X-Subrouter-Session, X-Subrouter-Account-ID")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	got := <-seen
	if accountID := got.Header.Get("X-Subrouter-Account-ID"); accountID != "qwen-token:work" {
		t.Fatalf("pinned account header = %q", accountID)
	}
	if authorization := got.Header.Get("Authorization"); authorization != "Bearer router-token" {
		t.Fatalf("router authorization = %q", authorization)
	}
	if got.Header.Get("X-Subrouter-Agent") != "qwen-token" || got.Header.Get("X-Subrouter-Session") != "pinned-session" {
		t.Fatalf("routing headers were removed by client hop metadata: agent=%q session=%q", got.Header.Get("X-Subrouter-Agent"), got.Header.Get("X-Subrouter-Session"))
	}
	if got.Header.Get("Connection") != "" {
		t.Fatalf("client hop metadata leaked upstream: %q", got.Header.Get("Connection"))
	}
	if got.Header.Get("X-Subrouter-Account") != "" {
		t.Fatalf("untrusted account alias leaked: %q", got.Header.Get("X-Subrouter-Account"))
	}
	if _, err := startNativeProxyRelay(upstream.URL, qwenNativeProxy, "pinned-session", "router-token", "bad\r\nX-Injected: yes"); err == nil {
		t.Fatal("header-injecting pinned account was accepted")
	}
}

func TestParseNativeProxyLaunchArgsOwnsOnlyLeadingAccountOption(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		selector   string
		picker     bool
		vendorArgs []string
		wantErr    string
	}{
		{name: "pooled", args: []string{"--model", "x"}, vendorArgs: []string{"--model", "x"}},
		{name: "pin separate", args: []string{"--account", "work", "--model", "x"}, selector: "work", vendorArgs: []string{"--model", "x"}},
		{name: "pin equals with delimiter", args: []string{"--account=work", "--", "--account", "vendor"}, selector: "work", vendorArgs: []string{"--account", "vendor"}},
		{name: "picker", args: []string{"--account"}, picker: true},
		{name: "picker with delimiter", args: []string{"--account", "--", "prompt"}, picker: true, vendorArgs: []string{"prompt"}},
		{name: "delimiter makes account vendor owned", args: []string{"--", "--account", "vendor"}, vendorArgs: []string{"--account", "vendor"}},
		{name: "first vendor arg ends parsing", args: []string{"--model", "x", "--account", "vendor"}, vendorArgs: []string{"--model", "x", "--account", "vendor"}},
		{name: "empty equals", args: []string{"--account="}, wantErr: "non-empty"},
		{name: "missing selector", args: []string{"--account", "--model"}, wantErr: "requires an account selector"},
		{name: "duplicate", args: []string{"--account", "work", "--account=other"}, wantErr: "only once"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options, vendorArgs, err := parseNativeProxyLaunchArgs(test.args)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if options.accountSelector != test.selector || options.pickPinnedAccount != test.picker || !reflect.DeepEqual(vendorArgs, test.vendorArgs) {
				t.Fatalf("parsed = %+v, %q; want selector=%q picker=%t args=%q", options, vendorArgs, test.selector, test.picker, test.vendorArgs)
			}
		})
	}
}

func TestNativeProxyAccountSelectionIsProviderScopedAndFailsClosed(t *testing.T) {
	inventory := []remoteServerAccount{
		{ID: "kimi-subscription:work", Label: "Work", Email: "member@example.test", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth},
		{ID: "kimi:metered", Label: "Metered", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeAPIKey},
		{ID: "kimi-subscription:collision", Label: "kimi:metered", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth},
		{ID: "kimi-code", Label: "Direct CLI", Source: "kimi-code credentials file", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth},
		{ID: "qwen-token:large", Label: "Large", Provider: accounts.ProviderQwenToken, AuthMode: accounts.AuthModeAPIKey},
		{ID: "qwen-token:larger", Label: "Larger", Provider: accounts.ProviderQwenToken, AuthMode: accounts.AuthModeAPIKey},
	}
	for selector, want := range map[string]string{
		"WORK":                "kimi-subscription:work",
		"member@example.test": "kimi-subscription:work",
		"metered":             "kimi:metered",
	} {
		got, err := resolveNativeProxyAccountSelector(kimiNativeProxy, inventory, selector)
		if err != nil || got != want {
			t.Fatalf("resolve Kimi %q = %q, %v; want %q", selector, got, err, want)
		}
	}
	if got, err := resolveNativeProxyAccountSelector(qwenNativeProxy, inventory, "large"); err != nil || got != "qwen-token:large" {
		t.Fatalf("resolve Qwen prefix-stripped ID = %q, %v", got, err)
	}
	if got, err := resolveNativeProxyAccountSelector(kimiNativeProxy, inventory, "kimi:metered"); err != nil || got != "kimi:metered" {
		t.Fatalf("canonical ID did not outrank colliding label: %q, %v", got, err)
	}
	for _, test := range []struct {
		spec     nativeProxySpec
		selector string
		wantErr  string
	}{
		{spec: kimiNativeProxy, selector: "Direct CLI", wantErr: "not an eligible routed Kimi account"},
		{spec: kimiNativeProxy, selector: "Large", wantErr: "not an eligible routed Kimi account"},
		{spec: qwenNativeProxy, selector: "larg", wantErr: "ambiguous"},
		{spec: qwenNativeProxy, selector: "missing", wantErr: "was not found"},
		{spec: qwenNativeProxy, selector: "bad\rselector", wantErr: "control character"},
	} {
		if _, err := resolveNativeProxyAccountSelector(test.spec, inventory, test.selector); err == nil || !strings.Contains(err.Error(), test.wantErr) {
			t.Fatalf("resolve %s %q error = %v, want %q", test.spec.display, test.selector, err, test.wantErr)
		}
	}
	injected := append([]remoteServerAccount(nil), inventory...)
	injected[4].ID = "qwen-token:good\r\nX-Injected: yes"
	if _, err := resolveNativeProxyAccountSelector(qwenNativeProxy, injected, "Large"); err == nil || !strings.Contains(err.Error(), "invalid server routing ID") {
		t.Fatalf("header-injecting account ID error = %v", err)
	}
}

func TestPooledTeamNativeProxyLaunchDefersAccountSelectionToBroker(t *testing.T) {
	var accountRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/_subrouter/health":
			w.WriteHeader(http.StatusOK)
		case "/":
			if request.Method != http.MethodHead {
				t.Errorf("data-plane preflight method = %s", request.Method)
			}
			w.WriteHeader(http.StatusNoContent)
		case "/_subrouter/accounts":
			accountRequests.Add(1)
			_, _ = io.WriteString(w, `[]`)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	cloudPath := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudPath)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", server.URL)
	if err := broker.SaveConfig(cloudPath, broker.Config{
		CredentialSource: broker.CredentialSourceTeam,
		BaseURL:          "https://cmux.com",
		AccessToken:      "test-access",
		RefreshToken:     "test-refresh",
		TeamID:           "test-team",
		LocalProxyToken:  "test-local-proxy-token",
	}); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "qwen"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	runner := srRunner{client: server.Client(), in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
	if err := runner.launchNativeProxy(t.Context(), qwenNativeProxy, nil, nativeProxyLaunchOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := accountRequests.Load(); got != 0 {
		t.Fatalf("pooled team launch made %d local account inventory request(s), want broker selection at request time", got)
	}

	err := runner.launchNativeProxy(t.Context(), qwenNativeProxy, nil, nativeProxyLaunchOptions{accountSelector: "work"})
	if err == nil || !strings.Contains(err.Error(), "no routed Qwen apikey account") {
		t.Fatalf("pinned team launch error = %v, want authoritative inventory failure", err)
	}
	if got := accountRequests.Load(); got != 1 {
		t.Fatalf("pinned team launch made %d account inventory request(s), want 1", got)
	}
}

func TestNativeProxyPinnedPickerIsSortedAndBlankCancels(t *testing.T) {
	inventory := []remoteServerAccount{
		{ID: "qwen-token:z", Label: "Shared", Provider: accounts.ProviderQwenToken, AuthMode: accounts.AuthModeAPIKey},
		{ID: "qwen-token:a", Label: "Shared", Provider: accounts.ProviderQwenToken, AuthMode: accounts.AuthModeAPIKey},
	}
	var out bytes.Buffer
	runner := srRunner{in: strings.NewReader("2\n"), out: &out}
	got, chosen, err := runner.pickNativeProxyAccount(qwenNativeProxy, inventory)
	if err != nil || !chosen || got != "qwen-token:z" {
		t.Fatalf("picked = %q chosen=%t err=%v", got, chosen, err)
	}
	if text := out.String(); !strings.Contains(text, "PINNED process") || !strings.Contains(text, "No account failover") ||
		!strings.Contains(text, "Shared (qwen-token:a; apikey)") || !strings.Contains(text, "Shared (qwen-token:z; apikey)") ||
		strings.Index(text, "qwen-token:a") > strings.Index(text, "qwen-token:z") {
		t.Fatalf("picker output = %q", text)
	}
	runner.in = strings.NewReader("\n")
	if got, chosen, err := runner.pickNativeProxyAccount(qwenNativeProxy, inventory); err != nil || chosen || got != "" {
		t.Fatalf("blank picker = %q chosen=%t err=%v", got, chosen, err)
	}
}

func TestNativeProxyDispatchSeparatesManagementFromDefaultLaunch(t *testing.T) {
	for _, args := range [][]string{nil, {"--model", "x"}, {"proxy"}, {"--account", "work"}} {
		if isKimiManagementCommand(args) || isQwenManagementCommand(args) {
			t.Fatalf("launch args %q classified as management", args)
		}
	}
	for _, args := range [][]string{{"login"}, {"help"}, {"--help"}} {
		if !isKimiManagementCommand(args) || !isQwenManagementCommand(args) {
			t.Fatalf("management args %q classified as launch", args)
		}
	}
	if !isKimiManagementCommand([]string{"list"}) || isQwenManagementCommand([]string{"list"}) {
		t.Fatal("provider-specific management verbs were not preserved")
	}
	if err := (srRunner{}).antigravityCommand(t.Context(), []string{"--account", "unused"}); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("Antigravity pin error = %v", err)
	}
	for _, provider := range []string{"kimi", "qwen"} {
		var out bytes.Buffer
		runner := srRunner{out: &out}
		if err := runner.run(t.Context(), []string{provider, "--help"}); err != nil {
			t.Fatalf("sr %s --help: %v", provider, err)
		}
		if !strings.Contains(out.String(), "Plain '") && !strings.Contains(out.String(), "plain ") {
			t.Fatalf("sr %s --help did not describe the direct bypass: %q", provider, out.String())
		}
	}
}

func TestNativeProxyRelayTransportNeverUsesAmbientProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://credential-sink.invalid")
	t.Setenv("HTTPS_PROXY", "http://credential-sink.invalid")
	transport, err := nativeProxyRelayTransport("https://router.example.test/t/opaque")
	if err != nil {
		t.Fatal(err)
	}
	if transport.Proxy != nil {
		t.Fatal("native relay transport retained an ambient proxy function")
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
	if otherLoopbackToken, err := nativeProxyServerToken("http://127.0.0.2:43213"); err != nil || otherLoopbackToken != "subrouter" {
		t.Fatalf("other loopback token = %q err=%v, want remote placeholder", otherLoopbackToken, err)
	}
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", "https://router.example.test")
	if sameLocalProxyEndpoint("https://router.example.test/t/opaque", "https://router.example.test") {
		t.Fatal("matching non-loopback endpoints were treated as the local daemon")
	}
	if remoteOverrideToken, err := nativeProxyServerToken("https://router.example.test/t/opaque"); err != nil || remoteOverrideToken != "subrouter" {
		t.Fatalf("non-loopback local override token = %q err=%v", remoteOverrideToken, err)
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

func TestNativeProxyServerIgnoresStaleRemoteDefaultForLocalStorage(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/_subrouter/health" {
			http.NotFound(w, request)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer local.Close()
	var staleRequests atomic.Int32
	stale := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		staleRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer stale.Close()

	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL)
	cloudPath := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudPath)
	if err := os.WriteFile(cloudPath, []byte(`{"version":1,"baseUrl":"https://cmux.com","credentialSource":"local"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	serverStore := defaultSRServerStore(store)
	if err := serverStore.update(func(file *srServerFile) error {
		file.Default = "stale"
		file.Servers = []srServerConfig{{Name: "stale", URL: stale.URL}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	server, remote, err := (srRunner{store: store, errOut: io.Discard}).nativeProxyServer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if remote || !sameEndpoint(server.URL, local.URL) {
		t.Fatalf("native proxy server = %+v remote=%t, want active local storage", server, remote)
	}
	if staleRequests.Load() != 0 {
		t.Fatalf("stale remote received %d request(s)", staleRequests.Load())
	}
}

func TestNativeProxyServerHonorsExplicitLocalOverLegacyStorage(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/_subrouter/health" {
			http.NotFound(w, request)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer local.Close()
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL)
	t.Setenv("SUBROUTER_SERVER", "local")
	cloudPath := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudPath)
	if err := os.WriteFile(cloudPath, []byte(`{"version":1,"baseUrl":"https://cmux.com","credentialSource":"legacy"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	server, remote, err := (srRunner{store: accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}, errOut: io.Discard}).nativeProxyServer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if remote || !sameEndpoint(server.URL, local.URL) {
		t.Fatalf("native proxy server = %+v remote=%t, want explicit local", server, remote)
	}
}

func TestNativeProxyServerRejectsUnselectedLegacyAuthority(t *testing.T) {
	var localRequests atomic.Int32
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		localRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer local.Close()
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL)
	cloudPath := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudPath)
	if err := os.WriteFile(cloudPath, []byte(`{"version":1,"baseUrl":"https://cmux.com","credentialSource":"legacy"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := (srRunner{store: accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}, errOut: io.Discard}).nativeProxyServer(t.Context())
	if err == nil || !strings.Contains(err.Error(), "no selected server") {
		t.Fatalf("unselected legacy authority error = %v", err)
	}
	if localRequests.Load() != 0 {
		t.Fatalf("unselected legacy authority silently probed local %d time(s)", localRequests.Load())
	}
}

func TestNativeProxyServerUsesHostedAuthorityWithoutLegacyRegistry(t *testing.T) {
	cloudPath := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudPath)
	if err := broker.SaveConfig(cloudPath, broker.Config{
		CredentialSource: broker.CredentialSourceHosted,
		AccessToken:      "hosted-access",
		RefreshToken:     "hosted-refresh",
		TeamID:           "hosted-team",
		HostedURL:        "https://hosted.example.test",
		TenantKey:        testTenantKey,
	}); err != nil {
		t.Fatal(err)
	}
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	if err := os.MkdirAll(store.StoreDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(defaultSRServerStore(store).Path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", "http://127.0.0.1:1")

	server, remote, err := (srRunner{store: store, errOut: io.Discard}).nativeProxyServer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !remote || server.Name != "cmux" || server.URL != "https://hosted.example.test" || server.TenantKey != testTenantKey {
		t.Fatalf("native proxy server = %+v remote=%t, want hosted cloud authority", server, remote)
	}
}

func TestNativeProxyEnvironmentsReplaceRoutingCredentialsWithoutExposingScope(t *testing.T) {
	original := []string{
		"PATH=/usr/bin", "KEEP_ME=yes", "KIMI_CODE_HOME=/custom/kimi-home", "QWEN_HOME=/custom/qwen-home",
		"OPENAI_API_KEY=real-openai-secret", "OPENAI_BASE_URL=https://vendor.invalid/v1",
		"OPENAI_ORG_ID=direct-org-secret", "OPENAI_PROJECT_ID=direct-project-secret",
		"BAILIAN_TOKEN_PLAN_API_KEY=real-bailian-secret", "KIMI_MODEL_API_KEY=real-kimi-secret",
		"KIMI_CODE_CUSTOM_HEADERS=X-Direct-Gateway-Secret: custom-header-secret",
		"HTTP_PROXY=http://credential-sink.invalid", "https_proxy=http://credential-sink.invalid",
		"ALL_PROXY=socks5://credential-sink.invalid", "NO_PROXY=vendor.invalid",
	}
	qwenRelay := "http://127.0.0.1:43210/private-relay-capability"
	qwenProviderURL := qwenRelay + "/qwen-token/v1"
	qwenEnv, qwenCleanup, err := nativeProxyEnvironment(qwenNativeProxy, qwenRelay, original, []string{"--model", "qwen-test-model"})
	if err != nil {
		t.Fatal(err)
	}
	defer qwenCleanup()
	joined := strings.Join(qwenEnv, "\n")
	for _, secret := range []string{"real-openai-secret", "real-bailian-secret", "real-kimi-secret", "custom-header-secret", "direct-org-secret", "direct-project-secret", "vendor.invalid", "credential-sink.invalid"} {
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
		Proxy          *string `json:"proxy"`
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
	if overlay.Proxy == nil || *overlay.Proxy != "" {
		t.Fatalf("Qwen overlay proxy = %v, want an explicit process-only disable", overlay.Proxy)
	}
	if got := testEnvValue(qwenEnv, "NO_PROXY"); got != "127.0.0.1,localhost,::1" {
		t.Fatalf("Qwen NO_PROXY = %q", got)
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
	joinedKimi := strings.Join(kimiEnv, "\n")
	for _, secret := range []string{"real-kimi-secret", "custom-header-secret"} {
		if strings.Contains(joinedKimi, secret) {
			t.Fatalf("Kimi child environment retained direct credential %q", secret)
		}
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

func TestQwenProxyRejectsClientProxyOverrideBeforeLaunch(t *testing.T) {
	for _, args := range [][]string{{"--proxy", "http://proxy.invalid"}, {"--proxy=http://proxy.invalid"}, {"--", "--proxy", "http://proxy.invalid"}} {
		err := (srRunner{}).launchQwenProxy(t.Context(), args)
		if err == nil || !strings.Contains(err.Error(), "--proxy controls Qwen routing") {
			t.Fatalf("proxy override %q error = %v", args, err)
		}
		if strings.Contains(err.Error(), "proxy.invalid") {
			t.Fatalf("proxy override error exposed the supplied URL: %v", err)
		}
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
	for _, args := range [][]string{{"--session"}, {"-S"}, {"--resume"}, {"-r"}, {"--session="}, {"--resume="}, {"--session", ""}, {"-r", "   "}, {"--session", "--model", "kimi-test"}} {
		if !nativeProxyResumePickerRequested(kimiNativeProxy, args) {
			t.Fatalf("Kimi picker resume %q was not detected", args)
		}
	}
	for _, args := range [][]string{{"--session", "session-id"}, {"-S", "session-id"}, {"--resume", "session-id"}, {"-r", "session-id"}, {"--resume=session-id"}} {
		if nativeProxyResumePickerRequested(kimiNativeProxy, args) {
			t.Fatalf("explicit Kimi resume %q was rejected", args)
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
	qwenWork := nativeProxyPinnedSessionID(qwenID, "qwen-token:work")
	qwenPersonal := nativeProxyPinnedSessionID(qwenID, "qwen-token:personal")
	if qwenWork == qwenID || qwenPersonal == qwenID || qwenWork == qwenPersonal {
		t.Fatalf("pinned session identities collide: pooled=%q work=%q personal=%q", qwenID, qwenWork, qwenPersonal)
	}
	if again := nativeProxyPinnedSessionID(qwenID, "qwen-token:work"); again != qwenWork {
		t.Fatalf("pinned session identity is not stable: %q != %q", again, qwenWork)
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
		{"HOME=" + t.TempDir(), "GOOGLE_API_KEY=direct-secret"},
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
			{"id":"qwen-token:work","provider":"qwen-token","auth_mode":"apikey"},
			{"id":"kimi-code","provider":"kimi","auth_mode":"oauth","source":"kimi-code credentials file"}
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
