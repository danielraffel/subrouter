package main

import (
	"bufio"
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
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/broker"
)

const (
	antigravityProxyHelp = `Usage: sr antigravity [agy args...]
       sr agy [agy args...]
       sr agy proxy [agy args...]

Launch agy through the selected Subrouter. Plain 'agy' remains a direct bypass.
The agy CLI must still have its own local login; Subrouter never copies or changes it.
Antigravity currently has one router-host login, so --account pinning is not supported.
`
	kimiProxyHelp = `Usage: sr kimi [--account [account]] [-- kimi args...]
       sr kimi proxy [--account [account]] [-- kimi args...]

Launch Kimi Code through the selected Subrouter pool. Plain 'kimi' remains direct.
Omit --account for pooled failover. A named account is pinned with no account failover;
bare --account opens a pinned-account picker.
The process-scoped model override leaves Kimi's normal login and config unchanged.
Account affinity is stable per working directory, including resumed sessions.
The session-picker form requires an explicit ID: sr kimi --session <session-id>.
`
	qwenProxyHelp = `Usage: sr qwen [--account [account]] [-- qwen args...]
       sr qwen proxy [--account [account]] [-- qwen args...]

Launch Qwen Code through the selected Qwen Token Plan pool. Plain 'qwen' remains direct.
Omit --account for pooled failover. A named account is pinned with no account failover;
bare --account opens a pinned-account picker.
The process-only routing overlay preserves Qwen's normal sessions and configuration.
Account affinity is stable per working directory, including resumed sessions.
The session-picker form requires an explicit ID: sr qwen --resume <session-id>.
Qwen serve/ACP can reload saved environment routing, so use plain 'qwen' for those modes.
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

type nativeProxyLaunchOptions struct {
	accountSelector   string
	pickPinnedAccount bool
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
	if len(args) > 0 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		fmt.Fprint(r.out, antigravityProxyHelp)
		return nil
	}
	if len(args) > 0 && args[0] == "proxy" {
		args = args[1:]
	}
	if len(args) > 0 && (args[0] == "--account" || strings.HasPrefix(args[0], "--account=")) {
		return errors.New("Antigravity currently has one router-host login; --account pinning is not supported")
	}
	return r.launchNativeProxy(ctx, antigravityNativeProxy, args, nativeProxyLaunchOptions{})
}

func (r srRunner) launchKimiProxy(ctx context.Context, args []string) error {
	options, vendorArgs, err := parseNativeProxyLaunchArgs(args)
	if err != nil {
		return err
	}
	if len(vendorArgs) > 0 {
		switch vendorArgs[0] {
		case "login", "provider":
			return fmt.Errorf("%q changes Kimi's local credentials; use plain 'kimi %s' for the direct CLI or 'sr kimi login <label>' to manage the routed pool", vendorArgs[0], vendorArgs[0])
		}
	}
	if nativeProxyResumePickerRequested(kimiNativeProxy, vendorArgs) {
		return errors.New("'sr kimi --session' requires an explicit session ID so Subrouter can preserve sticky account routing")
	}
	return r.launchNativeProxy(ctx, kimiNativeProxy, vendorArgs, options)
}

func (r srRunner) launchQwenProxy(ctx context.Context, args []string) error {
	options, vendorArgs, err := parseNativeProxyLaunchArgs(args)
	if err != nil {
		return err
	}
	if qwenProxyReloadCapableMode(vendorArgs) {
		return errors.New("Qwen serve/ACP modes can reload saved credentials and proxies; use plain 'qwen' for those modes")
	}
	for i := 0; i < len(vendorArgs); i++ {
		if vendorArgs[i] == "--" {
			break
		}
		for _, option := range []string{"--auth-type", "--openai-api-key", "--openai-base-url", "--proxy"} {
			if vendorArgs[i] == option || strings.HasPrefix(vendorArgs[i], option+"=") {
				return fmt.Errorf("%s controls Qwen routing and cannot be used with 'sr qwen'", option)
			}
		}
	}
	if nativeProxyResumePickerRequested(qwenNativeProxy, vendorArgs) {
		return errors.New("'sr qwen --resume' requires an explicit session ID so Subrouter can preserve sticky account routing")
	}
	return r.launchNativeProxy(ctx, qwenNativeProxy, vendorArgs, options)
}

func qwenProxyReloadCapableMode(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if arg == "--acp" || strings.HasPrefix(arg, "--acp=") ||
			arg == "--experimental-acp" || strings.HasPrefix(arg, "--experimental-acp=") {
			return true
		}
		switch arg {
		case "-m", "--model", "--fallback-model", "-p", "--prompt", "-i", "--prompt-interactive", "-o", "--output-format", "-r", "--resume":
			if i+1 < len(args) {
				i++
			}
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		// Qwen's first positional argument is its subcommand. Later values are
		// owned by that command and cannot switch the top-level runtime mode.
		return arg == "serve"
	}
	return false
}

// parseNativeProxyLaunchArgs reserves --account only at the beginning of the
// sr wrapper's arguments. The first vendor argument ends wrapper parsing, and
// an explicit -- delimiter makes every following value vendor-owned.
func parseNativeProxyLaunchArgs(args []string) (nativeProxyLaunchOptions, []string, error) {
	var options nativeProxyLaunchOptions
	if len(args) == 0 {
		return options, nil, nil
	}
	if args[0] == "--" {
		return options, args[1:], nil
	}
	if strings.HasPrefix(args[0], "--account=") {
		options.accountSelector = strings.TrimSpace(strings.TrimPrefix(args[0], "--account="))
		if options.accountSelector == "" {
			return options, nil, errors.New("--account= requires a non-empty account selector")
		}
		args = args[1:]
	} else if args[0] == "--account" {
		if len(args) == 1 {
			options.pickPinnedAccount = true
			return options, nil, nil
		}
		if args[1] == "--" {
			options.pickPinnedAccount = true
			return options, args[2:], nil
		}
		if strings.HasPrefix(args[1], "-") {
			return options, nil, errors.New("--account requires an account selector; use '--account --' to open the picker and pass vendor arguments")
		}
		options.accountSelector = strings.TrimSpace(args[1])
		if options.accountSelector == "" {
			return options, nil, errors.New("--account requires a non-empty account selector")
		}
		args = args[2:]
	}
	if len(args) > 0 && (args[0] == "--account" || strings.HasPrefix(args[0], "--account=")) {
		return options, nil, errors.New("--account may be specified only once; use '--' before a vendor-owned --account option")
	}
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	return options, args, nil
}

func nativeProxyResumePickerRequested(spec nativeProxySpec, args []string) bool {
	var pickerFlags []string
	switch spec.provider {
	case accounts.ProviderKimi:
		pickerFlags = []string{"-S", "--session", "-r", "--resume"}
	case accounts.ProviderQwenToken:
		pickerFlags = []string{"-r", "--resume"}
	default:
		return false
	}
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			return false
		}
		matched := false
		for _, flag := range pickerFlags {
			if args[i] == flag {
				matched = true
			}
			if strings.HasPrefix(args[i], flag+"=") && strings.TrimSpace(strings.TrimPrefix(args[i], flag+"=")) == "" {
				return true
			}
		}
		if !matched {
			continue
		}
		return i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" || strings.HasPrefix(args[i+1], "-")
	}
	return false
}

func (r srRunner) launchNativeProxy(ctx context.Context, spec nativeProxySpec, args []string, options nativeProxyLaunchOptions) error {
	server, _, err := r.nativeProxyServer(ctx)
	if err != nil {
		return err
	}
	root, err := secureNativeProxyRoot(ctx, server)
	if err != nil {
		return fmt.Errorf("secure %s proxy transport: %w", spec.display, err)
	}
	cloudConfig, err := cloudModeConfig()
	if err != nil {
		return fmt.Errorf("load credential storage: %w", err)
	}
	credentialSource := cloudConfig.EffectiveCredentialSource()
	if credentialSource == broker.CredentialSourceTeam && !nativeProxyBrokerLeaseSupported(spec) {
		return fmt.Errorf("team credential storage cannot lease %s accounts; use local or legacy storage for 'sr %s'", spec.display, spec.command)
	}
	var inventory []remoteServerAccount
	if nativeProxyNeedsAccountInventory(options, credentialSource) {
		if credentialSource == broker.CredentialSourceTeam {
			inventory, err = nativeProxyTeamAccounts(ctx, cloudConfig, spec)
		} else {
			inventory, err = r.nativeProxyAccounts(ctx, server, spec)
		}
		if err != nil {
			return err
		}
	}
	forcedAccountID := ""
	if options.pickPinnedAccount {
		var chosen bool
		forcedAccountID, chosen, err = r.pickNativeProxyAccount(spec, inventory)
		if err != nil {
			return err
		}
		if !chosen {
			return nil
		}
	} else if strings.TrimSpace(options.accountSelector) != "" {
		forcedAccountID, err = resolveNativeProxyAccountSelector(spec, inventory, options.accountSelector)
		if err != nil {
			return fmt.Errorf("server %s: %w", server.Name, err)
		}
	}
	sessionID, err := nativeProxySessionID(spec, args)
	if err != nil {
		return err
	}
	if forcedAccountID != "" {
		sessionID = nativeProxyPinnedSessionID(sessionID, forcedAccountID)
	}
	proxyToken, err := nativeProxyServerToken(server.URL)
	if err != nil {
		return err
	}
	if err := r.requireNativeProxyDataPlane(ctx, root, proxyToken); err != nil {
		return err
	}
	relay, err := startNativeProxyRelay(root, spec, sessionID, proxyToken, forcedAccountID)
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
		args = qwenNativeProxyArgs(args, qwenProxyModel(args))
	}
	cmd := exec.CommandContext(ctx, commandPath, args...)
	cmd.Stdin = r.in
	cmd.Stdout = r.out
	cmd.Stderr = r.errOut
	cmd.Env = env
	return cmd.Run()
}

func nativeProxyNeedsAccountInventory(options nativeProxyLaunchOptions, source broker.CredentialSource) bool {
	// A hard process-local pin must resolve one authoritative account ID before
	// launching. Pooled team mode is different: the local daemon intentionally
	// owns no account inventory and obtains a provider-scoped lease from the
	// credential broker on the first routed request.
	return options.pickPinnedAccount || strings.TrimSpace(options.accountSelector) != "" || source != broker.CredentialSourceTeam
}

func nativeProxyBrokerLeaseSupported(spec nativeProxySpec) bool {
	return spec.provider == accounts.ProviderQwenToken && spec.authMode == accounts.AuthModeAPIKey
}

func nativeProxyServerToken(root string) (string, error) {
	if !sameLocalProxyEndpoint(root, localBaseURL()) {
		return "subrouter", nil
	}
	config, err := cloudModeConfig()
	if err != nil {
		return "", fmt.Errorf("load local Subrouter client credential: %w", err)
	}
	if token := cloudClientProxyToken(config, localBaseURL()); token != "" {
		return token, nil
	}
	return "subrouter", nil
}

func sameLocalProxyEndpoint(left, right string) bool {
	leftURL, leftErr := url.Parse(strings.TrimSpace(left))
	rightURL, rightErr := url.Parse(strings.TrimSpace(right))
	if leftErr != nil || rightErr != nil || !loopbackEndpoint(left) || !loopbackEndpoint(right) ||
		!strings.EqualFold(leftURL.Scheme, rightURL.Scheme) ||
		(!strings.EqualFold(leftURL.Scheme, "http") && !strings.EqualFold(leftURL.Scheme, "https")) {
		return false
	}
	if sameEndpoint(left, right) {
		return true
	}
	// Different loopback names are equivalent only for the canonical local
	// listener aliases. The rest of 127/8 can be bound by another process, so
	// matching only the port must never disclose the daemon credential.
	canonicalLocalHost := func(parsed *url.URL) bool {
		host := strings.ToLower(parsed.Hostname())
		if host == "localhost" {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && (ip.Equal(net.IPv4(127, 0, 0, 1)) || ip.Equal(net.IPv6loopback))
	}
	if !canonicalLocalHost(leftURL) || !canonicalLocalHost(rightURL) {
		return false
	}
	port := func(parsed *url.URL) string {
		if value := parsed.Port(); value != "" {
			return value
		}
		if strings.EqualFold(parsed.Scheme, "https") {
			return "443"
		}
		return "80"
	}
	return port(leftURL) == port(rightURL)
}

func (r srRunner) requireNativeProxyDataPlane(ctx context.Context, root, proxyToken string) error {
	probeURL := strings.TrimRight(root, "/") + "/"
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, probeURL, nil)
	if err != nil {
		return errors.New("build native proxy data-plane preflight")
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(proxyToken))
	client := r.client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	secured, err := securedServerRequestClient(client, root)
	if err != nil {
		return errors.New("selected router data-plane transport is not safe for a native proxy launcher; no vendor CLI was started")
	}
	response, err := secured.Do(request)
	if err != nil {
		return errors.New("selected router data-plane preflight failed; no vendor CLI was started")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	if response.StatusCode == http.StatusNoContent {
		return nil
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return errors.New("selected router requires session-lease or data-plane authentication that native proxy launchers do not support; no vendor CLI was started")
	}
	return fmt.Errorf("selected router data-plane preflight returned HTTP %d; no vendor CLI was started", response.StatusCode)
}

func (r srRunner) nativeProxyServer(ctx context.Context) (srServerConfig, bool, error) {
	config, err := cloudModeConfig()
	if err != nil {
		return srServerConfig{}, false, fmt.Errorf("load credential storage: %w", err)
	}
	source := config.EffectiveCredentialSource()
	explicitTarget := strings.TrimSpace(os.Getenv("SUBROUTER_SERVER"))
	if explicitTarget == "" {
		explicitTarget = strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_SERVER"))
	}
	explicitServer := explicitTarget != ""
	explicitLocal := explicitServer && isLocalServerName(explicitTarget)
	if explicitServer {
		server, remote, resolveErr := r.selectedRemoteServer()
		if resolveErr != nil {
			return srServerConfig{}, false, resolveErr
		}
		if remote {
			if source == broker.CredentialSourceTeam {
				return srServerConfig{}, false, errors.New("team credentials may only use the local Subrouter daemon; select local or change credential storage")
			}
			return server, true, nil
		}
	}
	if !explicitLocal {
		switch source {
		case broker.CredentialSourceHosted:
			if !config.HostedReady() {
				return srServerConfig{}, false, errors.New("hosted credential storage is incomplete; run 'sr login'")
			}
			return srServerConfig{
				Name: "cmux", URL: strings.TrimRight(config.HostedURL, "/"), TenantKey: config.TenantKey,
			}, true, nil
		case broker.CredentialSourceLegacy:
			server, remote, resolveErr := r.selectedRemoteServer()
			if resolveErr != nil {
				return srServerConfig{}, false, resolveErr
			}
			if !remote {
				return srServerConfig{}, false, errors.New("legacy remote credential storage has no selected server; run 'sr remote use <name>' or 'sr storage local'")
			}
			return server, true, nil
		}
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

func (r srRunner) nativeProxyAccounts(ctx context.Context, server srServerConfig, spec nativeProxySpec) ([]remoteServerAccount, error) {
	inventory, err := r.fetchServerAccounts(ctx, server)
	if err != nil {
		return nil, fmt.Errorf("load %s accounts from server %s: %w", spec.display, server.Name, err)
	}
	eligible := make([]remoteServerAccount, 0, len(inventory))
	for _, account := range inventory {
		if nativeProxyAccountEligible(spec, account) && validNativeProxyAccountID(strings.TrimSpace(account.ID)) {
			eligible = append(eligible, account)
		}
	}
	if len(eligible) > 0 {
		return eligible, nil
	}
	mode := ""
	if spec.authMode != "" {
		mode = " " + string(spec.authMode)
	}
	return nil, fmt.Errorf("no routed %s%s account is available on server %s", spec.display, mode, server.Name)
}

func nativeProxyTeamAccounts(ctx context.Context, config broker.Config, spec nativeProxySpec) ([]remoteServerAccount, error) {
	shared, err := broker.NewClient(config).ListAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("load %s accounts from the selected team: %w", spec.display, err)
	}
	eligible := make([]remoteServerAccount, 0, len(shared))
	for _, item := range shared {
		if item.Kind != string(spec.provider) {
			continue
		}
		account := remoteServerAccount{
			ID:       strings.TrimSpace(item.ID),
			Provider: spec.provider,
			AuthMode: spec.authMode,
			Label:    strings.TrimSpace(item.Label),
			Email:    strings.TrimSpace(item.Email),
			Source:   "team vault",
		}
		if nativeProxyAccountEligible(spec, account) && validNativeProxyAccountID(account.ID) {
			eligible = append(eligible, account)
		}
	}
	if len(eligible) > 0 {
		return eligible, nil
	}
	mode := ""
	if spec.authMode != "" {
		mode = " " + string(spec.authMode)
	}
	return nil, fmt.Errorf("no routed %s%s account is available in the selected team", spec.display, mode)
}

func (r srRunner) requireNativeProxyAccount(ctx context.Context, server srServerConfig, spec nativeProxySpec) error {
	_, err := r.nativeProxyAccounts(ctx, server, spec)
	return err
}

func nativeProxyAccountEligible(spec nativeProxySpec, account remoteServerAccount) bool {
	if account.Provider != spec.provider {
		return false
	}
	if spec.authMode != "" && account.AuthMode != spec.authMode {
		return false
	}
	if spec.provider != accounts.ProviderKimi || account.AuthMode != accounts.AuthModeOAuth {
		return true
	}
	// The singleton credential owned by the plain Kimi CLI is deliberately a
	// direct bypass. Only Subrouter-managed subscription profiles (or Kimi API
	// keys, handled above) may enter the routed pool.
	id := strings.ToLower(strings.TrimSpace(account.ID))
	source := strings.ToLower(strings.TrimSpace(account.Source))
	return id != "kimi-code" && !strings.HasPrefix(id, "kimi-code:") && !strings.Contains(source, "kimi-code credentials file")
}

func validNativeProxyAccountID(accountID string) bool {
	return accountID != "" && len(accountID) <= 256 && !nativeProxyTerminalControl(accountID)
}

func nativeProxyTerminalControl(value string) bool {
	for _, char := range value {
		if unicode.IsControl(char) || unicode.In(char, unicode.Cf, unicode.Zl, unicode.Zp) {
			return true
		}
	}
	return false
}

func nativeProxyAccountSelectorValues(spec nativeProxySpec, account remoteServerAccount) []string {
	id := strings.TrimSpace(account.ID)
	values := []string{id, strings.TrimSpace(account.Label), strings.TrimSpace(account.Email)}
	if prefix := string(spec.provider) + ":"; strings.HasPrefix(strings.ToLower(id), strings.ToLower(prefix)) {
		values = append(values, strings.TrimSpace(id[len(prefix):]))
	}
	if spec.provider == accounts.ProviderKimi {
		const managedPrefix = "kimi-subscription:"
		if strings.HasPrefix(strings.ToLower(id), managedPrefix) {
			values = append(values, strings.TrimSpace(id[len(managedPrefix):]))
		}
	}
	return values
}

func resolveNativeProxyAccountSelector(spec nativeProxySpec, inventory []remoteServerAccount, selector string) (string, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return "", fmt.Errorf("%s account selector cannot be empty", spec.display)
	}
	if nativeProxyTerminalControl(selector) {
		return "", fmt.Errorf("%s account selector contains a control character", spec.display)
	}
	type match struct {
		account remoteServerAccount
		rank    int
	}
	matches := make([]match, 0)
	lowerSelector := strings.ToLower(selector)
	for _, account := range inventory {
		values := nativeProxyAccountSelectorValues(spec, account)
		rank := 4
		if len(values) > 0 && strings.EqualFold(values[0], selector) {
			rank = 0 // A canonical server routing ID always wins.
		}
		for index, value := range values {
			if value == "" {
				continue
			}
			switch {
			case index >= 3 && strings.EqualFold(value, selector) && rank > 1:
				rank = 1 // Provider-prefix-stripped routing ID.
			case index > 0 && index < 3 && strings.EqualFold(value, selector) && rank > 2:
				rank = 2 // User-facing label or email.
			case strings.Contains(strings.ToLower(value), lowerSelector) && rank > 3:
				rank = 3
			}
		}
		if rank < 4 {
			matches = append(matches, match{account: account, rank: rank})
		}
	}
	bestRank := 4
	for _, candidate := range matches {
		if candidate.rank < bestRank {
			bestRank = candidate.rank
		}
	}
	if bestRank < 4 {
		filtered := matches[:0]
		for _, candidate := range matches {
			if candidate.rank == bestRank {
				filtered = append(filtered, candidate)
			}
		}
		matches = filtered
	}
	unique := make(map[string]remoteServerAccount, len(matches))
	for _, candidate := range matches {
		key := strings.ToLower(string(candidate.account.Provider)) + "\x00" +
			strings.ToLower(string(candidate.account.AuthMode)) + "\x00" +
			strings.ToLower(strings.TrimSpace(candidate.account.ID))
		unique[key] = candidate.account
	}
	if len(unique) == 0 {
		return "", fmt.Errorf("%s account %q was not found in the routed pool", spec.display, selector)
	}
	if len(unique) != 1 {
		return "", fmt.Errorf("%s account selector %q is ambiguous; use the exact account ID or label", spec.display, selector)
	}
	var account remoteServerAccount
	for _, candidate := range unique {
		account = candidate
	}
	if !nativeProxyAccountEligible(spec, account) {
		return "", fmt.Errorf("account %q is not an eligible routed %s account", selector, spec.display)
	}
	accountID := strings.TrimSpace(account.ID)
	if !validNativeProxyAccountID(accountID) {
		return "", fmt.Errorf("%s account %q has an invalid server routing ID", spec.display, selector)
	}
	return accountID, nil
}

func nativeProxyAccountDisplay(account remoteServerAccount) string {
	for _, value := range []string{account.Label, account.Email, account.ID} {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 256 || nativeProxyTerminalControl(value) {
			continue
		}
		return value
	}
	return "account"
}

func nativeProxyAccountPickerRow(account remoteServerAccount) string {
	id := strings.TrimSpace(account.ID)
	display := nativeProxyAccountDisplay(account)
	mode := strings.TrimSpace(string(account.AuthMode))
	if !strings.EqualFold(display, id) {
		return fmt.Sprintf("%s (%s; %s)", display, id, mode)
	}
	return fmt.Sprintf("%s (%s)", id, mode)
}

func (r srRunner) pickNativeProxyAccount(spec nativeProxySpec, inventory []remoteServerAccount) (string, bool, error) {
	sort.Slice(inventory, func(i, j int) bool {
		left := strings.ToLower(nativeProxyAccountPickerRow(inventory[i]))
		right := strings.ToLower(nativeProxyAccountPickerRow(inventory[j]))
		if left != right {
			return left < right
		}
		return strings.ToLower(inventory[i].ID) < strings.ToLower(inventory[j].ID)
	})
	fmt.Fprintf(r.out, "Choose one %s account for this PINNED process. No account failover will occur.\n", spec.display)
	for i, account := range inventory {
		fmt.Fprintf(r.out, "  %d) %s\n", i+1, nativeProxyAccountPickerRow(account))
	}
	answer, err := promptLine(r.out, bufio.NewReader(r.in), "Launch account (# or exact account): ")
	if err != nil {
		return "", false, err
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return "", false, nil
	}
	if index, parseErr := strconv.Atoi(answer); parseErr == nil && index >= 1 && index <= len(inventory) {
		return inventory[index-1].ID, true, nil
	}
	accountID, err := resolveNativeProxyAccountSelector(spec, inventory, answer)
	if err != nil {
		return "", false, err
	}
	return accountID, true, nil
}

type nativeProxyRelay struct {
	listener  net.Listener
	server    *http.Server
	transport *http.Transport
	baseURL   string
}

func nativeProxyRelayTransport(targetRoot string) (*http.Transport, error) {
	client, err := securedServerRequestClient(&http.Client{Timeout: 15 * time.Second}, targetRoot)
	if err != nil {
		return nil, err
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport == nil {
		return nil, errors.New("native proxy relay requires a direct HTTP transport")
	}
	return transport, nil
}

func startNativeProxyRelay(targetRoot string, spec nativeProxySpec, sessionID, proxyToken, forcedAccountID string) (*nativeProxyRelay, error) {
	target, err := url.Parse(strings.TrimRight(targetRoot, "/"))
	if err != nil || target.Scheme == "" || target.Host == "" || target.User != nil || target.Fragment != "" {
		return nil, errors.New("proxy target must be an absolute URL")
	}
	transport, err := nativeProxyRelayTransport(targetRoot)
	if err != nil {
		return nil, fmt.Errorf("secure proxy target transport: %w", err)
	}
	forcedAccountID = strings.TrimSpace(forcedAccountID)
	if forcedAccountID != "" && !validNativeProxyAccountID(forcedAccountID) {
		return nil, errors.New("pinned account has an invalid server routing ID")
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
	relayHost := listener.Addr().String()
	reverse := &httputil.ReverseProxy{Transport: transport}
	reverse.Rewrite = func(proxyRequest *httputil.ProxyRequest) {
		proxyRequest.SetURL(target)
		request := proxyRequest.Out
		for _, header := range []string{
			"Authorization", "Proxy-Authorization", "Cookie", "X-Api-Key", "X-Goog-Api-Key", "X-Auth-Token",
			"OpenAI-Organization", "OpenAI-Project",
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
		if forcedAccountID != "" {
			request.Header.Set("X-Subrouter-Account-ID", forcedAccountID)
		}
	}
	reverse.ErrorLog = log.New(io.Discard, "", 0)
	reverse.ErrorHandler = func(response http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(response, "Subrouter relay could not reach the selected server", http.StatusBadGateway)
	}
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		// The random path is a process-local capability: other local processes
		// cannot turn this short-lived relay into a tenant-wide proxy merely by
		// discovering its port. Restrict it to the one advertised provider too.
		// Qwen also receives this relay origin as a fail-closed HTTP proxy guard;
		// absolute-form requests for any other host must not inherit the
		// capability merely because their path happens to match.
		requestPath := request.URL.Path
		if request.Method == http.MethodConnect || request.URL.IsAbs() || request.Host != relayHost ||
			request.URL.RawPath != "" || pathpkg.Clean(requestPath) != requestPath ||
			(requestPath != providerPrefix && !strings.HasPrefix(requestPath, providerPrefix+"/")) {
			http.NotFound(response, request)
			return
		}
		request.URL.Path = strings.TrimPrefix(requestPath, relayPrefix)
		request.URL.RawPath = ""
		reverse.ServeHTTP(response, request)
	})
	relay := &nativeProxyRelay{
		listener:  listener,
		server:    &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second},
		transport: transport,
		baseURL:   "http://" + listener.Addr().String() + relayPrefix,
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
	if r.transport != nil {
		r.transport.CloseIdleConnections()
	}
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
	_ = args
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve native proxy workspace: %w", err)
	}
	cwd = filepath.Clean(cwd)
	if resolved, resolveErr := filepath.EvalSymlinks(cwd); resolveErr == nil {
		cwd = resolved
	}
	digest := sha256.Sum256([]byte(string(spec.provider) + "\x00workspace\x00" + cwd))
	return "sr-native-" + hex.EncodeToString(digest[:16]), nil
}

func nativeProxyPinnedSessionID(pooledSessionID, accountID string) string {
	digest := sha256.Sum256([]byte(pooledSessionID + "\x00pinned-account\x00" + accountID))
	return "sr-native-" + hex.EncodeToString(digest[:16])
}

var nativeProxyRoutingEnvKeys = []string{
	"CLOUD_CODE_URL",
	"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_GEMINI_BASE_URL", "AGY_ADC_AUTH",
	"KIMI_CODE_BASE_URL", "KIMI_CODE_CUSTOM_HEADERS", "KIMI_API_KEY", "KIMI_BASE_URL",
	"KIMI_MODEL_NAME", "KIMI_MODEL_API_KEY", "KIMI_MODEL_BASE_URL", "KIMI_MODEL_PROVIDER_TYPE",
	"KIMI_MODEL_MAX_CONTEXT_SIZE", "KIMI_WEB_SEARCH_BASE_URL", "KIMI_WEB_SEARCH_API_KEY",
	"KIMI_WEB_FETCH_BASE_URL", "KIMI_WEB_FETCH_API_KEY",
	"QWEN_OAUTH", "QWEN_MODEL", "OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_MODEL",
	"OPENAI_ORG_ID", "OPENAI_PROJECT_ID",
	"BAILIAN_CODING_PLAN_API_KEY", "BAILIAN_TOKEN_PLAN_API_KEY", "DASHSCOPE_API_KEY",
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
			return nil, func() {}, fmt.Errorf("Antigravity direct provider %s is configured; remove it before using 'sr agy'", conflict)
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
		proxyGuard, err := nativeProxyLoopbackGuardURL(relayRoot)
		if err != nil {
			cleanup()
			return nil, func() {}, err
		}
		for key, value := range map[string]string{
			"QWEN_CODE_SYSTEM_SETTINGS_PATH": overlay.settings,
			"QWEN_CODE_SYSTEM_DEFAULTS_PATH": overlay.defaults,
			"OPENAI_API_KEY":                 "subrouter",
			"OPENAI_BASE_URL":                providerURL + "/v1",
			"OPENAI_MODEL":                   model,
			// Qwen loads .qwen/.env and settings.env only when a process key is
			// absent. Non-empty sentinels prevent either source from restoring a
			// direct Alibaba credential. The forced --auth-type=openai argument
			// and single-provider system overlay remain the routing authority.
			"BAILIAN_CODING_PLAN_API_KEY": "subrouter",
			"BAILIAN_TOKEN_PLAN_API_KEY":  "subrouter",
			"DASHSCOPE_API_KEY":           "subrouter",
			// Qwen treats empty values as unset when it loads .qwen/.env. A
			// non-secret loopback guard prevents a saved outbound proxy from being
			// restored; the relay rejects proxy targets other than itself.
			"HTTP_PROXY":  proxyGuard,
			"HTTPS_PROXY": proxyGuard,
			"ALL_PROXY":   proxyGuard,
			"http_proxy":  proxyGuard,
			"https_proxy": proxyGuard,
			"all_proxy":   proxyGuard,
			// Common HTTP stacks, including Qwen's EnvHttpProxyAgent, bypass the
			// guard for every destination. This preserves ordinary tool and child
			// process networking while the non-empty guard still blocks Qwen's
			// saved .env proxy values from being restored.
			"NO_PROXY": "*",
			"no_proxy": "*",
		} {
			env = upsertEnv(env, key, value)
		}
		return env, cleanup, nil
	default:
		return nil, func() {}, fmt.Errorf("unsupported native proxy provider %s", spec.provider)
	}
}

func antigravityDirectProviderConflict(environ []string) (string, error) {
	for _, key := range []string{"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_GEMINI_BASE_URL", "AGY_ADC_AUTH"} {
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
		case strings.HasPrefix(args[i], "--model=") || strings.HasPrefix(args[i], "-m="):
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
		case strings.HasPrefix(args[i], "--model=") || strings.HasPrefix(args[i], "-m="):
			separator := strings.IndexByte(args[i], '=')
			if candidate := strings.TrimSpace(args[i][separator+1:]); candidate != "" {
				model = candidate
			}
		}
	}
	return model
}

func qwenNativeProxyArgs(args []string, model string) []string {
	out := make([]string, 0, len(args)+6)
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			out = append(out,
				"--auth-type", "openai",
				"--model", model,
				"--openai-api-key", "subrouter",
			)
			return append(out, args[i:]...)
		}
		switch {
		case args[i] == "-m" || args[i] == "--model":
			if i+1 < len(args) {
				i++
			}
		case strings.HasPrefix(args[i], "--model=") || strings.HasPrefix(args[i], "-m="):
		default:
			out = append(out, args[i])
		}
	}
	return append(out,
		"--auth-type", "openai",
		"--model", model,
		"--openai-api-key", "subrouter",
	)
}

type qwenProxyOverlay struct {
	settings string
	defaults string
}

func nativeProxyLoopbackGuardURL(baseURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil ||
		!isLoopbackServerHost(parsed.Hostname()) {
		return "", errors.New("Qwen proxy relay must use an HTTP loopback URL")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

func prepareQwenProxyOverlay(baseURL, model string, environ []string) (qwenProxyOverlay, func(), error) {
	if conflict := qwenSystemPolicyConflict(environ, runtime.GOOS); conflict != "" {
		return qwenProxyOverlay{}, func() {}, fmt.Errorf("Qwen system policy %s is configured; refusing a proxy overlay that could bypass it", conflict)
	}
	proxyGuard, err := nativeProxyLoopbackGuardURL(baseURL)
	if err != nil {
		return qwenProxyOverlay{}, func() {}, err
	}
	dir, err := os.MkdirTemp("", "subrouter-qwen-proxy-")
	if err != nil {
		return qwenProxyOverlay{}, func() {}, fmt.Errorf("create temporary Qwen proxy overlay: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	payload := map[string]any{
		// A saved Qwen proxy would otherwise receive the capability-bearing
		// loopback URL. This truthy value wins the settings merge but Qwen's
		// normalizeProxyUrl trims it to disabled. The non-empty environment guards
		// below separately prevent .env from restoring an outbound proxy.
		"proxy": " ",
		"env": map[string]string{
			"BAILIAN_CODING_PLAN_API_KEY": "subrouter",
			"BAILIAN_TOKEN_PLAN_API_KEY":  "subrouter",
			"DASHSCOPE_API_KEY":           "subrouter",
			"HTTP_PROXY":                  proxyGuard,
			"HTTPS_PROXY":                 proxyGuard,
			"ALL_PROXY":                   proxyGuard,
			"http_proxy":                  proxyGuard,
			"https_proxy":                 proxyGuard,
			"all_proxy":                   proxyGuard,
			"NO_PROXY":                    "*",
			"no_proxy":                    "*",
		},
		"slashCommands": map[string]any{
			"disabled": []string{"auth", "model"},
		},
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
	return qwenSystemPolicyConflictAtPathsForOS(environ, qwenDefaultSystemPolicyPaths(environ, goos), goos)
}

func qwenSystemPolicyConflictAtPaths(environ, paths []string) string {
	return qwenSystemPolicyConflictAtPathsForOS(environ, paths, runtime.GOOS)
}

func qwenSystemPolicyConflictAtPathsForOS(environ, paths []string, goos string) string {
	for _, key := range []string{"QWEN_CODE_SYSTEM_SETTINGS_PATH", "QWEN_CODE_SYSTEM_DEFAULTS_PATH"} {
		if strings.TrimSpace(envValueForOS(environ, key, goos)) != "" {
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
		root := envValueForOS(environ, "ProgramData", goos)
		if root == "" {
			root = `C:\ProgramData`
		}
		return []string{filepath.Join(root, "qwen-code", "settings.json"), filepath.Join(root, "qwen-code", "system-defaults.json")}
	default:
		return []string{"/etc/qwen-code/settings.json", "/etc/qwen-code/system-defaults.json"}
	}
}

func envValue(environ []string, key string) string {
	return envValueForOS(environ, key, runtime.GOOS)
}

func envValueForOS(environ []string, key, goos string) string {
	for i := len(environ) - 1; i >= 0; i-- {
		item := environ[i]
		separator := strings.IndexByte(item, '=')
		if separator < 0 {
			continue
		}
		name := item[:separator]
		if (goos == "windows" && strings.EqualFold(name, key)) ||
			(goos != "windows" && name == key) {
			return item[separator+1:]
		}
	}
	return ""
}
