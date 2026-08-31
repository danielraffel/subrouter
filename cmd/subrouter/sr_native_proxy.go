package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

const (
	antigravityProxyHelp = `Usage: sr antigravity proxy [agy args...]
       sr agy proxy [agy args...]

Launch agy through the selected Subrouter. Plain 'agy' remains a direct bypass.
The agy CLI must still have its own local login; Subrouter never copies or changes it.
`
	kimiProxyHelp = `Usage: sr kimi proxy [kimi args...]

Launch Kimi Code through the selected Subrouter pool. Plain 'kimi' remains direct.
The process-scoped model override leaves Kimi's normal login and config unchanged.
`
	qwenProxyHelp = `Usage: sr qwen proxy [qwen args...]

Launch Qwen Code through the selected Qwen Token Plan pool. Plain 'qwen' remains direct.
The process-only routing overlay preserves Qwen's normal sessions and configuration.
`
)

type nativeProxySpec struct {
	command  string
	display  string
	agent    string
	route    string
	provider accounts.Provider
	authMode accounts.AuthMode
}

var (
	antigravityNativeProxy = nativeProxySpec{
		command: "agy", display: "Antigravity", agent: "antigravity",
		route: "antigravity", provider: accounts.ProviderAntigravity, authMode: accounts.AuthModeOAuth,
	}
	kimiNativeProxy = nativeProxySpec{
		command: "kimi", display: "Kimi", agent: "kimi",
		route: "kimi", provider: accounts.ProviderKimi,
	}
	qwenNativeProxy = nativeProxySpec{
		command: "qwen", display: "Qwen", agent: "qwen-token",
		route: "qwen-token", provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey,
	}
)

func (r srRunner) antigravityCommand(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(r.out, antigravityProxyHelp)
		return nil
	}
	if args[0] != "proxy" {
		return fmt.Errorf("unknown Antigravity command %q; use 'sr agy proxy'", args[0])
	}
	return r.launchNativeProxy(ctx, antigravityNativeProxy, args[1:])
}

func (r srRunner) launchKimiProxy(ctx context.Context, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "login", "provider":
			return fmt.Errorf("%q changes Kimi's local credentials; use plain 'kimi %s' for the direct CLI or 'sr kimi login <label>' to manage the routed pool", args[0], args[0])
		}
	}
	return r.launchNativeProxy(ctx, kimiNativeProxy, args)
}

func (r srRunner) launchQwenProxy(ctx context.Context, args []string) error {
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			break
		}
		for _, option := range []string{"--auth-type", "--openai-api-key", "--openai-base-url"} {
			if args[i] == option || strings.HasPrefix(args[i], option+"=") {
				return fmt.Errorf("%s controls Qwen routing and cannot be used with 'sr qwen proxy'", option)
			}
		}
	}
	return r.launchNativeProxy(ctx, qwenNativeProxy, args)
}

func (r srRunner) launchNativeProxy(ctx context.Context, spec nativeProxySpec, args []string) error {
	server, remote, err := r.nativeProxyServer(ctx)
	if err != nil {
		return err
	}
	root, err := secureNativeProxyRoot(ctx, server)
	if err != nil {
		return fmt.Errorf("secure %s proxy transport: %w", spec.display, err)
	}
	if err := r.requireNativeProxyAccount(ctx, server, spec); err != nil {
		return err
	}
	sessionID, err := nativeProxySessionID(spec, args)
	if err != nil {
		return err
	}
	proxyToken, err := nativeProxyServerToken(root, remote)
	if err != nil {
		return err
	}
	relay, err := startNativeProxyRelay(root, spec, sessionID, proxyToken)
	if err != nil {
		return fmt.Errorf("start local %s proxy relay: %w", spec.display, err)
	}
	defer relay.Close()

	commandPath, err := exec.LookPath(spec.command)
	if err != nil {
		return fmt.Errorf("%s CLI %q was not found in PATH", spec.display, spec.command)
	}
	env, cleanup, err := nativeProxyEnvironment(spec, relay.URL(), os.Environ(), args)
	if err != nil {
		return err
	}
	defer cleanup()
	if spec.provider == accounts.ProviderKimi {
		args = kimiNativeProxyArgs(args)
	} else if spec.provider == accounts.ProviderQwenToken {
		args = qwenNativeProxyArgs(args, qwenProxyModel(args), strings.TrimRight(relay.URL(), "/")+"/"+spec.route+"/v1")
	}
	cmd := exec.CommandContext(ctx, commandPath, args...)
	cmd.Stdin = r.in
	cmd.Stdout = r.out
	cmd.Stderr = r.errOut
	cmd.Env = env
	return cmd.Run()
}

func nativeProxyServerToken(root string, remote bool) (string, error) {
	if remote {
		return "subrouter", nil
	}
	config, err := cloudModeConfig()
	if err != nil {
		return "", fmt.Errorf("load local Subrouter client credential: %w", err)
	}
	if token := cloudClientProxyToken(config, root); token != "" {
		return token, nil
	}
	return "subrouter", nil
}

func (r srRunner) nativeProxyServer(ctx context.Context) (srServerConfig, bool, error) {
	server, remote, err := r.selectedRemoteServer()
	if err != nil {
		return srServerConfig{}, false, err
	}
	if remote {
		return server, true, nil
	}
	if !ensureLocalHealthy(ctx, fallbackHTTPClient(), localBaseURL(), defaultDaemonStarter(), r.errOut) {
		return srServerConfig{}, false, fmt.Errorf("local proxy is unavailable; run '%s doctor'", r.programOrSubrouter())
	}
	return srServerConfig{Name: "local", URL: localBaseURL()}, false, nil
}

func secureNativeProxyRoot(ctx context.Context, server srServerConfig) (string, error) {
	root := canonicalServerProxyRootURL(server)
	protected := server
	parsed, _ := url.Parse(root)
	tenantInURL := parsed != nil && tenantKeyFromURL(parsed) != ""
	if strings.TrimSpace(protected.TenantKey) == "" && !tenantInURL {
		// Prompts and responses are private even on a single-tenant server. Force
		// the same HTTPS/loopback/Tailscale validation used by the Claude launcher.
		protected.TenantKey = "protected-native-proxy"
	}
	return secureTenantServerURL(ctx, root, protected)
}

func (r srRunner) requireNativeProxyAccount(ctx context.Context, server srServerConfig, spec nativeProxySpec) error {
	inventory, err := r.fetchServerAccounts(ctx, server)
	if err != nil {
		return fmt.Errorf("load %s accounts from server %s: %w", spec.display, server.Name, err)
	}
	for _, account := range inventory {
		if account.Provider != spec.provider {
			continue
		}
		if spec.authMode == "" || account.AuthMode == spec.authMode {
			return nil
		}
	}
	mode := ""
	if spec.authMode != "" {
		mode = " " + string(spec.authMode)
	}
	return fmt.Errorf("no routed %s%s account is available on server %s", spec.display, mode, server.Name)
}

type nativeProxyRelay struct {
	listener net.Listener
	server   *http.Server
	baseURL  string
}

func startNativeProxyRelay(targetRoot string, spec nativeProxySpec, sessionID, proxyToken string) (*nativeProxyRelay, error) {
	target, err := url.Parse(strings.TrimRight(targetRoot, "/"))
	if err != nil || target.Scheme == "" || target.Host == "" || target.User != nil || target.Fragment != "" {
		return nil, errors.New("proxy target must be an absolute URL")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(sessionID) == "" {
		sessionID, err = newNativeProxySessionID()
		if err != nil {
			_ = listener.Close()
			return nil, err
		}
	}
	relayToken, err := newNativeProxyToken()
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	proxyToken = strings.TrimSpace(proxyToken)
	if proxyToken == "" {
		proxyToken = "subrouter"
	}
	relayPrefix := "/" + relayToken
	providerPrefix := relayPrefix + "/" + spec.route
	reverse := httputil.NewSingleHostReverseProxy(target)
	originalDirector := reverse.Director
	reverse.Director = func(request *http.Request) {
		originalDirector(request)
		for _, header := range []string{
			"Authorization", "Proxy-Authorization", "Cookie", "X-Api-Key", "X-Goog-Api-Key", "X-Auth-Token",
			"X-Subrouter-Lease", "X-Subrouter-Session", "X-Subrouter-Agent",
			"X-Subrouter-User-Email", "X-Subrouter-User", "X-User-Email",
			"X-Subrouter-Account-ID", "X-Subrouter-Account", "X-Subrouter-Preferred-Account-ID",
			"X-Subrouter-Model", "X-Model", "X-Subrouter-Azure", "X-Subrouter-No-Retry",
		} {
			request.Header.Del(header)
		}
		request.Host = target.Host
		request.Header.Set("Authorization", "Bearer "+proxyToken)
		request.Header.Set("X-Subrouter-Agent", spec.agent)
		request.Header.Set("X-Subrouter-Session", sessionID)
	}
	reverse.ErrorLog = log.New(io.Discard, "", 0)
	reverse.ErrorHandler = func(response http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(response, "Subrouter relay could not reach the selected server", http.StatusBadGateway)
	}
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		// The random path is a process-local capability: other local processes
		// cannot turn this short-lived relay into a tenant-wide proxy merely by
		// discovering its port. Restrict it to the one advertised provider too.
		requestPath := request.URL.Path
		if request.URL.RawPath != "" || pathpkg.Clean(requestPath) != requestPath ||
			(requestPath != providerPrefix && !strings.HasPrefix(requestPath, providerPrefix+"/")) {
			http.NotFound(response, request)
			return
		}
		request.URL.Path = strings.TrimPrefix(requestPath, relayPrefix)
		request.URL.RawPath = ""
		reverse.ServeHTTP(response, request)
	})
	relay := &nativeProxyRelay{
		listener: listener,
		server:   &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second},
		baseURL:  "http://" + listener.Addr().String() + relayPrefix,
	}
	go func() { _ = relay.server.Serve(listener) }()
	return relay, nil
}

func (r *nativeProxyRelay) URL() string {
	if r == nil {
		return ""
	}
	return r.baseURL
}

func (r *nativeProxyRelay) Close() {
	if r == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = r.server.Shutdown(ctx)
	_ = r.listener.Close()
}

func newNativeProxySessionID() (string, error) {
	var body [16]byte
	if _, err := rand.Read(body[:]); err != nil {
		return "", fmt.Errorf("create proxy session ID: %w", err)
	}
	return "sr-native-" + hex.EncodeToString(body[:]), nil
}

func newNativeProxyToken() (string, error) {
	var body [32]byte
	if _, err := rand.Read(body[:]); err != nil {
		return "", fmt.Errorf("create local proxy capability: %w", err)
	}
	return hex.EncodeToString(body[:]), nil
}

func nativeProxySessionID(spec nativeProxySpec, args []string) (string, error) {
	if identity := nativeProxyResumeIdentity(spec, args); identity != "" {
		digest := sha256.Sum256([]byte(string(spec.provider) + "\x00" + identity))
		return "sr-native-" + hex.EncodeToString(digest[:16]), nil
	}
	return newNativeProxySessionID()
}

func nativeProxyResumeIdentity(spec nativeProxySpec, args []string) string {
	var idFlags []string
	var continueFlags []string
	switch spec.provider {
	case accounts.ProviderAntigravity:
		idFlags = []string{"--conversation"}
		continueFlags = []string{"-c", "--continue"}
	case accounts.ProviderKimi:
		idFlags = []string{"-S", "--session"}
		continueFlags = []string{"-c", "--continue"}
	case accounts.ProviderQwenToken:
		idFlags = []string{"-r", "--resume", "--session-id"}
		continueFlags = []string{"-c", "--continue"}
	default:
		return ""
	}
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			break
		}
		for _, flag := range idFlags {
			if strings.HasPrefix(args[i], flag+"=") {
				if value := strings.TrimSpace(strings.TrimPrefix(args[i], flag+"=")); value != "" {
					return "resume:" + value
				}
			}
			if args[i] == flag && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				return "resume:" + strings.TrimSpace(args[i+1])
			}
		}
		for _, flag := range continueFlags {
			// Kimi has used -c both as a boolean continue alias and, in newer
			// releases, as a prompt alias. Only treat a short flag as continue
			// when it does not consume a following value; the long form is stable.
			shortWithValue := len(flag) == 2 && args[i] == flag && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-")
			if !shortWithValue && (args[i] == flag || args[i] == flag+"=true") {
				cwd, err := os.Getwd()
				if err != nil {
					cwd = "."
				}
				return "continue:" + filepath.Clean(cwd)
			}
		}
	}
	return ""
}

var nativeProxyRoutingEnvKeys = []string{
	"CLOUD_CODE_URL",
	"GEMINI_API_KEY", "GOOGLE_GEMINI_BASE_URL", "AGY_ADC_AUTH",
	"KIMI_CODE_BASE_URL", "KIMI_API_KEY", "KIMI_BASE_URL",
	"KIMI_MODEL_NAME", "KIMI_MODEL_API_KEY", "KIMI_MODEL_BASE_URL", "KIMI_MODEL_PROVIDER_TYPE",
	"KIMI_MODEL_MAX_CONTEXT_SIZE", "KIMI_WEB_SEARCH_BASE_URL", "KIMI_WEB_SEARCH_API_KEY",
	"KIMI_WEB_FETCH_BASE_URL", "KIMI_WEB_FETCH_API_KEY",
	"QWEN_OAUTH", "QWEN_MODEL", "OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_MODEL",
	"BAILIAN_TOKEN_PLAN_API_KEY", "DASHSCOPE_API_KEY",
}

func nativeProxyEnvironment(spec nativeProxySpec, relayRoot string, environ, args []string) ([]string, func(), error) {
	env := envWithout(environ, nativeProxyRoutingEnvKeys)
	env = directPlainHTTPEnvironment(env, relayRoot)
	providerURL := strings.TrimRight(relayRoot, "/") + "/" + spec.route
	switch spec.provider {
	case accounts.ProviderAntigravity:
		if conflict, err := antigravityDirectProviderConflict(environ); err != nil {
			return nil, func() {}, err
		} else if conflict != "" {
			return nil, func() {}, fmt.Errorf("Antigravity direct provider %s is configured; remove it before using 'sr agy proxy'", conflict)
		}
		return upsertEnv(env, "CLOUD_CODE_URL", providerURL), func() {}, nil
	case accounts.ProviderKimi:
		for key, value := range map[string]string{
			"KIMI_MODEL_NAME":             "k3",
			"KIMI_MODEL_API_KEY":          "subrouter",
			"KIMI_MODEL_BASE_URL":         providerURL + "/v1",
			"KIMI_MODEL_PROVIDER_TYPE":    "kimi",
			"KIMI_MODEL_MAX_CONTEXT_SIZE": "1048576",
			"KIMI_WEB_SEARCH_BASE_URL":    providerURL + "/v1/search",
			"KIMI_WEB_SEARCH_API_KEY":     "subrouter",
			"KIMI_WEB_FETCH_BASE_URL":     providerURL + "/v1/fetch",
			"KIMI_WEB_FETCH_API_KEY":      "subrouter",
		} {
			env = upsertEnv(env, key, value)
		}
		return env, func() {}, nil
	case accounts.ProviderQwenToken:
		model := qwenProxyModel(args)
		overlay, cleanup, err := prepareQwenProxyOverlay(providerURL+"/v1", model, environ)
		if err != nil {
			return nil, func() {}, err
		}
		for key, value := range map[string]string{
			"QWEN_CODE_SYSTEM_SETTINGS_PATH": overlay.settings,
			"QWEN_CODE_SYSTEM_DEFAULTS_PATH": overlay.defaults,
			"OPENAI_API_KEY":                 "subrouter",
			"OPENAI_BASE_URL":                providerURL + "/v1",
			"OPENAI_MODEL":                   model,
		} {
			env = upsertEnv(env, key, value)
		}
		return env, cleanup, nil
	default:
		return nil, func() {}, fmt.Errorf("unsupported native proxy provider %s", spec.provider)
	}
}

func antigravityDirectProviderConflict(environ []string) (string, error) {
	for _, key := range []string{"GEMINI_API_KEY", "GOOGLE_GEMINI_BASE_URL", "AGY_ADC_AUTH"} {
		if strings.TrimSpace(envValue(environ, key)) != "" {
			return key, nil
		}
	}
	home := strings.TrimSpace(envValue(environ, "HOME"))
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate Antigravity settings: %w", err)
		}
	}
	settingsPath := filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	body, err := os.ReadFile(settingsPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read Antigravity settings: %w", err)
	}
	if len(body) > 1<<20 {
		return "", errors.New("Antigravity settings are too large to validate safely")
	}
	var settings struct {
		ModelProvider json.RawMessage `json:"modelProvider"`
	}
	if err := json.Unmarshal(body, &settings); err != nil {
		return "", fmt.Errorf("parse Antigravity settings: %w", err)
	}
	if len(settings.ModelProvider) == 0 || string(settings.ModelProvider) == "null" {
		return "", nil
	}
	var provider string
	if err := json.Unmarshal(settings.ModelProvider, &provider); err != nil {
		return "", errors.New("Antigravity settings contain an unsupported modelProvider value")
	}
	if strings.TrimSpace(provider) != "" {
		return "modelProvider", nil
	}
	return "", nil
}

func kimiNativeProxyArgs(args []string) []string {
	out := make([]string, 0, len(args)+2)
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			out = append(out, "--model", "__kimi_env_model__")
			return append(out, args[i:]...)
		}
		switch {
		case args[i] == "-m" || args[i] == "--model":
			if i+1 < len(args) {
				i++
			}
			continue
		case strings.HasPrefix(args[i], "--model="):
			continue
		default:
			out = append(out, args[i])
		}
	}
	return append(out, "--model", "__kimi_env_model__")
}

const defaultQwenProxyModel = "qwen3.7-plus"

func qwenProxyModel(args []string) string {
	model := defaultQwenProxyModel
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			break
		}
		switch {
		case (args[i] == "-m" || args[i] == "--model") && i+1 < len(args):
			if candidate := strings.TrimSpace(args[i+1]); candidate != "" {
				model = candidate
			}
			i++
		case strings.HasPrefix(args[i], "--model="):
			if candidate := strings.TrimSpace(strings.TrimPrefix(args[i], "--model=")); candidate != "" {
				model = candidate
			}
		}
	}
	return model
}

func qwenNativeProxyArgs(args []string, model, baseURL string) []string {
	out := make([]string, 0, len(args)+8)
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			out = append(out,
				"--auth-type", "openai",
				"--model", model,
				"--openai-api-key", "subrouter",
				"--openai-base-url", baseURL,
			)
			return append(out, args[i:]...)
		}
		switch {
		case args[i] == "-m" || args[i] == "--model":
			if i+1 < len(args) {
				i++
			}
		case strings.HasPrefix(args[i], "--model="):
		default:
			out = append(out, args[i])
		}
	}
	return append(out,
		"--auth-type", "openai",
		"--model", model,
		"--openai-api-key", "subrouter",
		"--openai-base-url", baseURL,
	)
}

type qwenProxyOverlay struct {
	settings string
	defaults string
}

func prepareQwenProxyOverlay(baseURL, model string, environ []string) (qwenProxyOverlay, func(), error) {
	if conflict := qwenSystemPolicyConflict(environ, runtime.GOOS); conflict != "" {
		return qwenProxyOverlay{}, func() {}, fmt.Errorf("Qwen system policy %s is configured; refusing a proxy overlay that could bypass it", conflict)
	}
	dir, err := os.MkdirTemp("", "subrouter-qwen-proxy-")
	if err != nil {
		return qwenProxyOverlay{}, func() {}, fmt.Errorf("create temporary Qwen proxy overlay: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	payload := map[string]any{
		"modelProviders": map[string]any{
			"openai": []any{map[string]any{
				"id":      model,
				"name":    "Qwen Token Plan via Subrouter",
				"baseUrl": baseURL,
				"generationConfig": map[string]any{
					"customHeaders": map[string]string{"X-Subrouter-Agent": "qwen-token"},
				},
			}},
		},
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		cleanup()
		return qwenProxyOverlay{}, func() {}, fmt.Errorf("encode Qwen proxy overlay: %w", err)
	}
	body = append(body, '\n')
	settings := filepath.Join(dir, "settings.json")
	defaults := filepath.Join(dir, "system-defaults.json")
	if err := writeFileAtomic(settings, body, 0o600); err != nil {
		cleanup()
		return qwenProxyOverlay{}, func() {}, fmt.Errorf("write Qwen proxy overlay: %w", err)
	}
	if err := writeFileAtomic(defaults, []byte("{}\n"), 0o600); err != nil {
		cleanup()
		return qwenProxyOverlay{}, func() {}, fmt.Errorf("write Qwen proxy defaults: %w", err)
	}
	return qwenProxyOverlay{settings: settings, defaults: defaults}, cleanup, nil
}

func qwenSystemPolicyConflict(environ []string, goos string) string {
	return qwenSystemPolicyConflictAtPaths(environ, qwenDefaultSystemPolicyPaths(environ, goos))
}

func qwenSystemPolicyConflictAtPaths(environ, paths []string) string {
	for _, key := range []string{"QWEN_CODE_SYSTEM_SETTINGS_PATH", "QWEN_CODE_SYSTEM_DEFAULTS_PATH"} {
		if strings.TrimSpace(envValue(environ, key)) != "" {
			return key
		}
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
			return path
		}
	}
	return ""
}

func qwenDefaultSystemPolicyPaths(environ []string, goos string) []string {
	switch goos {
	case "darwin":
		return []string{
			"/Library/Application Support/QwenCode/settings.json",
			"/Library/Application Support/QwenCode/system-defaults.json",
		}
	case "windows":
		root := envValue(environ, "ProgramData")
		if root == "" {
			root = `C:\ProgramData`
		}
		return []string{filepath.Join(root, "qwen-code", "settings.json"), filepath.Join(root, "qwen-code", "system-defaults.json")}
	default:
		return []string{"/etc/qwen-code/settings.json", "/etc/qwen-code/system-defaults.json"}
	}
}

func envValue(environ []string, key string) string {
	prefix := key + "="
	for i := len(environ) - 1; i >= 0; i-- {
		if strings.HasPrefix(environ[i], prefix) {
			return strings.TrimPrefix(environ[i], prefix)
		}
	}
	return ""
}
