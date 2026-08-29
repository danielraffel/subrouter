package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/internal/proxy"
	"github.com/manaflow-ai/subrouter/internal/tenant"
)

const srClaudeHelp = `sr claude - Manage multiple Claude Code profiles

Usage:
  sr claude                     Show profiles and switch interactively
  sr claude add [name]          Add account (opens OAuth login, infers email)
  sr claude list                List all profiles with auth status
  sr claude switch [name]       Switch active profile
  sr claude remove <name>       Remove a profile
  sr claude env                 Print CLAUDE_CONFIG_DIR for local/HTTPS profiles
  sr claude push [name]         Upload a profile to the default Subrouter server pool
  sr claude pick                Switch to the profile with the most quota left
  sr claude proxy [args...]     Launch Claude profilelessly through the selected server
  sr claude run [name] [...]    Launch safely (required for plaintext remote profiles)
  sr claude --flag [...]        Launch Claude with the active profile
  sr claude <name> [...]        Shorthand for 'sr claude run <name>'
  sr claude help                Show this help
`

type claudeRunner struct {
	store  claude.Store
	in     io.Reader
	out    io.Writer
	errOut io.Writer
	client *http.Client
	// pushToServer uploads a profile to the default Subrouter server, when
	// one is configured. nil when the claude runner is built without server
	// support (tests). pushAfterAdd is the same upload but no-ops silently
	// when no default server is configured.
	pushToServer func(ctx context.Context, name string) error
	pushAfterAdd func(ctx context.Context, name string) error
	pick         func(ctx context.Context) error
	// ephemeral is used by hosted account onboarding. OAuth runs in a
	// temporary store, the credential is uploaded, and no local profile or
	// trajectory directory survives the command.
	ephemeral bool
	// afterAuthVerified is a test seam for cancellation and publication-failure
	// races after Claude has durably written a credential.
	afterAuthVerified func()
	// mutateProfileInventoryForTest injects failures at the publication wrapper
	// boundary, including errors returned after mutate has committed.
	mutateProfileInventoryForTest func(context.Context, func() (bool, error)) error
}

const claudeProfileReconcileTimeout = 10 * time.Second

func (r srRunner) claude(ctx context.Context, args []string) error {
	if claudeLaunchesAgent(args) {
		return r.proxyClaudeSelectedRemote(ctx, args[1:])
	}
	cr := claudeRunner{
		store:        claude.DefaultStore(),
		in:           r.in,
		out:          r.out,
		errOut:       r.errOut,
		client:       r.client,
		pushToServer: r.pushClaudeProfileToServer,
		pushAfterAdd: r.pushClaudeProfileAfterAdd,
		pick:         r.pickClaudeProfile,
	}
	return cr.run(ctx, args)
}

// proxyClaudeSelectedRemote launches Claude without consulting any local
// profile. The selected server owns account selection and failover; legacy
// trusted-tailnet servers accept the same non-secret compatibility token used
// by other profileless proxy clients.
func (r srRunner) proxyClaudeSelectedRemote(ctx context.Context, args []string) error {
	server, ok, err := r.selectedRemoteServer()
	if err != nil {
		return err
	}
	if !ok {
		config, configErr := cloudModeConfig()
		if configErr != nil {
			return fmt.Errorf("load cmux.com login: %w", configErr)
		}
		if config.EffectiveCredentialSource() == broker.CredentialSourceTeam && !config.Ready() {
			return fmt.Errorf("team credential storage requires login and a selected team; run '%s login'", programBase())
		}
		if !ensureLocalHealthy(ctx, fallbackHTTPClient(), localBaseURL(), defaultDaemonStarter(), r.errOut) {
			return fmt.Errorf("local proxy is unavailable; run '%s doctor'", r.programOrSubrouter())
		}
		proxyToken := cloudClientProxyToken(config, localBaseURL())
		if proxyToken == "" {
			proxyToken = "subrouter"
		}
		return r.proxyClaudeArgsTo(ctx, args, localBaseURL(), proxyToken, "local")
	}
	proxyToken := strings.TrimSpace(server.TenantKey)
	if proxyToken == "" {
		proxyToken = "subrouter"
	}
	scope := "server:" + strings.TrimSpace(server.Name)
	if strings.TrimSpace(server.TenantKey) != "" {
		scope = "tenant:" + strings.TrimSpace(server.TenantKey)
	} else if strings.TrimSpace(server.TailscaleNodeID) != "" {
		scope = "tailscale-node:" + strings.TrimSpace(server.TailscaleNodeID)
	}
	return r.proxyClaudeArgsToServer(ctx, args, server, proxyToken, scope)
}

// cloudClaude launches Claude against the local proxy. The proxy leases an
// access-only team credential from cmux.com and sends the provider request from
// this machine, so Claude never sees a shared refresh token.
func (r srRunner) cloudClaude(ctx context.Context, args []string) error {
	config, err := cloudModeConfig()
	if err != nil {
		return fmt.Errorf("load cmux.com login: %w", err)
	}
	if !config.TeamModeReady() {
		return fmt.Errorf("cmux.com team vault is not configured; run '%s login'", programBase())
	}
	return r.proxyClaude(
		ctx,
		args,
		cloudClientProxyToken(config, localBaseURL()),
	)
}

func (r srRunner) proxyClaude(
	ctx context.Context,
	args []string,
	localProxyToken string,
) error {
	return r.proxyClaudeTo(ctx, args, localBaseURL(), localProxyToken)
}

func (r srRunner) proxyClaudeTo(
	ctx context.Context,
	args []string,
	baseURL string,
	proxyToken string,
) error {
	configDir, launchArgs, err := proxyClaudeInvocation(
		claude.DefaultStore(),
		args,
	)
	if err != nil {
		return err
	}
	return r.runProxyClaude(ctx, launchArgs, baseURL, proxyToken, configDir)
}

func (r srRunner) proxyClaudeArgsTo(
	ctx context.Context,
	args []string,
	baseURL string,
	proxyToken string,
	scope string,
) error {
	scopeHash := sha256.Sum256([]byte(scope))
	configDir := filepath.Join(r.store.StoreDir(), "claude-proxy", fmt.Sprintf("%x", scopeHash[:12]))
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return fmt.Errorf("create isolated Claude proxy config: %w", err)
	}
	if err := os.Chmod(configDir, 0o700); err != nil {
		return fmt.Errorf("secure isolated Claude proxy config: %w", err)
	}
	return r.runProxyClaude(ctx, args, baseURL, proxyToken, configDir)
}

func (r srRunner) proxyClaudeArgsToServer(
	ctx context.Context,
	args []string,
	server srServerConfig,
	proxyToken string,
	scope string,
) error {
	scopeHash := sha256.Sum256([]byte(scope))
	configDir := filepath.Join(r.store.StoreDir(), "claude-proxy", fmt.Sprintf("%x", scopeHash[:12]))
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return fmt.Errorf("create isolated Claude proxy config: %w", err)
	}
	if err := os.Chmod(configDir, 0o700); err != nil {
		return fmt.Errorf("secure isolated Claude proxy config: %w", err)
	}
	return r.runProxyClaudeForServer(ctx, args, server, proxyToken, configDir)
}

func (r srRunner) runProxyClaudeForServer(ctx context.Context, args []string, server srServerConfig, proxyToken, configDir string) error {
	return r.runProxyClaudeForServerWithResolvers(
		ctx, args, server, proxyToken, configDir,
		net.DefaultResolver.LookupIPAddr, defaultTailscaleStatusLoader,
	)
}

func (r srRunner) runProxyClaudeForServerWithResolvers(
	ctx context.Context,
	args []string,
	server srServerConfig,
	proxyToken string,
	configDir string,
	lookup serverIPLookup,
	load tailscaleStatusLoader,
) error {
	proxyRoot := canonicalServerProxyRootURL(server)
	protectedServer := server
	parsedProxyRoot, _ := url.Parse(proxyRoot)
	if strings.TrimSpace(protectedServer.TenantKey) == "" && tenantKeyFromURL(parsedProxyRoot) == "" {
		// Profileless traffic still contains prompts and responses even when
		// the legacy compatibility token is non-secret. Force exact transport
		// verification without manufacturing a tenant route segment.
		protectedServer.TenantKey = "protected-profileless-claude"
	}
	secureBaseURL, err := secureTenantServerURLWithResolvers(ctx, proxyRoot, protectedServer, lookup, load)
	if err != nil {
		return err
	}
	return r.launchProxyClaude(ctx, args, secureBaseURL, proxyToken, configDir)
}

func (r srRunner) runProxyClaude(
	ctx context.Context,
	args []string,
	baseURL string,
	proxyToken string,
	configDir string,
) error {
	credential := strings.TrimSpace(proxyToken)
	if credential == "subrouter" {
		credential = ""
	}
	secureBaseURL, err := secureTenantProxyURL(ctx, baseURL, credential)
	if err != nil {
		return err
	}
	return r.launchProxyClaude(ctx, args, secureBaseURL, proxyToken, configDir)
}

func (r srRunner) launchProxyClaude(ctx context.Context, args []string, baseURL, proxyToken, configDir string) error {
	settingsBody, err := proxyClaudeLaunchSettings(baseURL, proxyToken, configDir)
	if err != nil {
		return err
	}
	claudePath, ok := claude.DetectCLI()
	if !ok {
		return fmt.Errorf("Claude CLI not found. Install from https://claude.ai/download")
	}
	cmd := exec.CommandContext(ctx, claudePath)
	settingsArg, cleanupSettings, err := attachClaudeLaunchSettings(cmd, settingsBody)
	if err != nil {
		return err
	}
	defer cleanupSettings()
	launchArgs, err := managedClaudeLaunchArgs(args, settingsArg)
	if err != nil {
		return err
	}
	cmd.Args = append([]string{claudePath}, launchArgs...)
	cmd.Stdin = r.in
	cmd.Stdout = r.out
	cmd.Stderr = r.errOut

	// The authoritative private settings file carries every routing value. Keep the
	// child environment credential-free so tenant URLs and keys cannot be read
	// through process inspection or inherited by subprocesses.
	cmd.Env = claudeSettingsChildEnvironment(os.Environ(), baseURL, configDir)
	return cmd.Run()
}

func proxyClaudeInvocation(
	store claude.Store,
	args []string,
) (string, []string, error) {
	name := ""
	launchArgs := args
	explicitProfile := false
	switch {
	case len(args) == 0:
		name = store.ActiveProfile()
	case args[0] == "run":
		launchArgs = args[1:]
		if len(launchArgs) > 0 && !strings.HasPrefix(launchArgs[0], "-") {
			name = launchArgs[0]
			explicitProfile = true
			launchArgs = launchArgs[1:]
		} else {
			name = store.ActiveProfile()
		}
	case strings.HasPrefix(args[0], "-"):
		name = store.ActiveProfile()
	default:
		explicitProfile = true
		name = args[0]
		launchArgs = args[1:]
	}
	if name == "" {
		if explicitProfile {
			return "", nil, fmt.Errorf("profile %q not found", name)
		}
		return "", launchArgs, nil
	}
	profile, ok, err := store.MatchProfile(name)
	if err != nil {
		return "", nil, err
	}
	if !ok {
		return "", nil, fmt.Errorf("profile %q not found", name)
	}
	if err := store.SetActiveProfile(profile.Name); err != nil {
		return "", nil, err
	}
	return store.ClaudeConfigDir(profile.Name), launchArgs, nil
}

func claudeSettingsChildEnvironment(environ []string, baseURL, configDir string) []string {
	env := envWithout(environ, claudeRoutingEnvKeys)
	env = directPlainHTTPEnvironment(env, baseURL)
	if configDir != "" {
		env = upsertEnv(env, "CLAUDE_CONFIG_DIR", configDir)
	}
	return env
}

func (r claudeRunner) run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return r.defaultInteractive(ctx)
	}
	switch args[0] {
	case "add", "login":
		name := ""
		if len(args) > 1 {
			name = args[1]
		}
		return r.add(ctx, name)
	case "list", "ls", "status":
		return r.list(ctx, false)
	case "switch", "use":
		if len(args) < 2 {
			return r.defaultInteractive(ctx)
		}
		return r.switchProfile(args[1])
	case "remove", "rm":
		if len(args) < 2 {
			return fmt.Errorf("usage: sr claude remove <name>")
		}
		return r.remove(ctx, args[1])
	case "env":
		return r.env()
	case "pick":
		if r.pick == nil {
			return fmt.Errorf("pick is not available")
		}
		return r.pick(ctx)
	case "push", "upload":
		if r.pushToServer == nil {
			return fmt.Errorf("server push is not available")
		}
		name := ""
		if len(args) > 1 {
			name = args[1]
		}
		if name == "" {
			name = r.store.ActiveProfile()
		}
		if name == "" {
			return fmt.Errorf("usage: sr claude push <name>")
		}
		return r.pushToServer(ctx, name)
	case "run":
		name := ""
		extra := []string{}
		if len(args) > 1 {
			if strings.HasPrefix(args[1], "-") {
				extra = args[1:]
			} else {
				name = args[1]
				extra = args[2:]
			}
		}
		return r.runClaude(ctx, name, extra)
	case "help", "-h", "--help":
		fmt.Fprint(r.out, srClaudeHelp)
		return nil
	default:
		if strings.HasPrefix(args[0], "-") {
			return r.runClaude(ctx, "", args)
		}
		if _, ok := r.store.FindProfile(args[0]); ok {
			return r.runClaude(ctx, args[0], args[1:])
		}
		return fmt.Errorf("unknown command: sr claude %s\n%s", args[0], srClaudeHelp)
	}
}

func (r claudeRunner) add(ctx context.Context, name string) error {
	claudePath, ok := claude.DetectCLI()
	if !ok {
		return fmt.Errorf("Claude CLI not found. Install from https://claude.ai/download")
	}

	var instancePath string
	var tempDir string
	var err error
	if name != "" {
		created, createErr := r.mutateProfileInventory(ctx, func() (bool, error) {
			var createErr error
			instancePath, createErr = r.store.CreateProfile(name)
			return createErr == nil, createErr
		})
		if createErr != nil {
			if created {
				rollbackErr := r.rollbackProfileInventory(ctx, name)
				return errors.Join(createErr, wrapClaudeReconcileError("remove Claude profile committed before publication teardown failed", rollbackErr))
			}
			return createErr
		}
	} else {
		instancePath, tempDir, err = r.store.CreateTempInstance()
		if err != nil {
			return err
		}
	}
	claudeConfigDir := r.store.PreferredInstancePath(instancePath)
	if err := prepareClaudeLoginFastPath(claudeConfigDir); err != nil {
		fmt.Fprintf(r.errOut, "Warning: could not pre-seed the login fast path: %s\n", err)
	}

	fmt.Fprintln(r.out, "Starting Claude Code...")
	fmt.Fprintln(r.out, "Complete the OAuth login in your browser; Claude closes automatically once the login lands.")
	fmt.Fprintln(r.out)

	// Passing "/login" as the initial prompt triggers the login flow; with
	// forceLoginMethod seeded, Claude opens the browser directly.
	cmd := exec.CommandContext(ctx, claudePath, "/login")
	cmd.Dir = claudeConfigDir
	cmd.Stdin = r.in
	cmd.Stdout = r.out
	cmd.Stderr = r.errOut
	cmd.Env = claude.EnvForConfigDir(claudeConfigDir)
	exitErr, autoClosed := r.runClaudeUntilCredential(ctx, cmd, claudeConfigDir)
	if exitErr != nil && !autoClosed {
		if name != "" {
			if rollbackErr := r.rollbackProfileInventory(ctx, name); rollbackErr != nil {
				return errors.Join(fmt.Errorf("Claude login did not complete: %w", exitErr), fmt.Errorf("remove incomplete Claude profile: %w", rollbackErr))
			}
		} else {
			_ = r.store.CleanupInstance(tempDir)
		}
		return fmt.Errorf("Claude login did not complete: %w", exitErr)
	}

	status, err := claude.AuthStatusForPath(ctx, claudePath, claudeConfigDir)
	if err != nil || status == nil || !status.LoggedIn {
		if name != "" {
			if rollbackErr := r.rollbackProfileInventory(ctx, name); rollbackErr != nil {
				return errors.Join(errors.New("login was not completed"), fmt.Errorf("remove incomplete Claude profile: %w", rollbackErr))
			}
		} else {
			_ = r.store.CleanupInstance(tempDir)
		}
		return fmt.Errorf("login was not completed")
	}
	if r.afterAuthVerified != nil {
		r.afterAuthVerified()
	}

	profileName := name
	if profileName == "" {
		profileName = status.Email
		if profileName == "" {
			profileName = "default"
		}
		registered := false
		_, registerErr := r.mutateProfileInventory(ctx, func() (bool, error) {
			if _, ok := r.store.FindProfile(profileName); ok {
				removed, removeErr := r.store.RemoveProfile(profileName)
				if removeErr != nil {
					return removed, removeErr
				}
			}
			err := r.store.RegisterProfile(profileName, tempDir)
			registered = err == nil
			return registered, err
		})
		if registerErr != nil {
			// The outer publication lock can report a teardown error after the
			// registry write committed. Never delete a credential directory that
			// the durable registry can still name. The explicit committed bit also
			// fails closed if the registry cannot subsequently be read or changes
			// concurrently after this transaction releases its lock.
			if registered || r.profileInventoryReferencesDir(tempDir) {
				return fmt.Errorf("register Claude profile committed before publication teardown failed: %w", registerErr)
			}
			if cleanupErr := r.cleanupTemporaryInstance(ctx, tempDir); cleanupErr != nil {
				return errors.Join(registerErr, fmt.Errorf("clean up authenticated temporary Claude profile: %w", cleanupErr))
			}
			return registerErr
		}
	} else if published, err := r.publishProfileCompletion(ctx); err != nil {
		// OAuth has already committed outside Subrouter's process. A cancellation
		// at this boundary must not leave the profile usable on disk but invisible
		// to a worker that consumed the earlier, credential-less generation.
		reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), claudeProfileReconcileTimeout)
		retryPublished, reconcileErr := r.publishProfileCompletion(reconcileCtx)
		cancel()
		if reconcileErr != nil {
			if published || retryPublished {
				// At least one generation was durably written and its completion
				// mutation ran. A teardown failure must not reclassify that credential
				// as unpublished and delete a profile a worker can already observe.
				return errors.Join(
					fmt.Errorf("publish completed Claude profile: %w", err),
					fmt.Errorf("retry completed Claude profile publication: %w", reconcileErr),
				)
			}
			// A persistent generation-path failure would make the normal published
			// rollback fail for the same reason as both completion attempts. Journal
			// the exact profile identity before removing it so a restarted worker can
			// complete the deletion while every worker remains fail-closed.
			rollbackCtx, rollbackCancel := context.WithTimeout(context.WithoutCancel(ctx), claudeProfileReconcileTimeout)
			removed, rollbackErr := proxy.RollbackUnpublishedClaudeProfileDiskMutation(rollbackCtx, r.store.Dir, name)
			rollbackCancel()
			if !removed && rollbackErr == nil {
				rollbackErr = fmt.Errorf("Claude profile %q was not present for unpublished rollback", name)
			}
			return errors.Join(
				fmt.Errorf("publish completed Claude profile: %w", err),
				fmt.Errorf("retry completed Claude profile publication: %w", reconcileErr),
				wrapClaudeReconcileError("remove unpublished Claude profile", rollbackErr),
			)
		}
	}

	plan := ""
	if status.SubscriptionType != "" {
		plan = " [" + status.SubscriptionType + "]"
	}
	email := ""
	if status.Email != "" {
		email = " (" + status.Email + ")"
	}
	if r.ephemeral {
		if r.pushAfterAdd == nil {
			return fmt.Errorf("hosted Claude upload is unavailable")
		}
		if err := r.pushAfterAdd(ctx, profileName); err != nil {
			return fmt.Errorf("upload Claude credential: %w", err)
		}
		fmt.Fprintf(r.out, "\nAdded Claude account %q to hosted cmux.%s%s\n", profileName, email, plan)
		fmt.Fprintln(r.out, "Local Claude auth was left unchanged.")
		return nil
	}

	fmt.Fprintf(r.out, "\nAdded Claude profile %q.%s%s\n", profileName, email, plan)
	if r.pushAfterAdd != nil {
		if err := r.pushAfterAdd(ctx, profileName); err != nil {
			fmt.Fprintf(r.errOut, "warning: server upload failed (profile stays local-only): %v\n", err)
			fmt.Fprintf(r.errOut, "Retry with: sr claude push %s\n", profileName)
		}
	}
	fmt.Fprintf(r.out, "\n  sr claude switch %s\n", profileName)
	fmt.Fprintf(r.out, "  sr claude run %s\n", profileName)
	return nil
}

// prepareClaudeLoginFastPath seeds a fresh profile's config dir so Claude
// Code boots straight into the browser OAuth flow instead of walking the
// first-run wizard: onboarding is marked complete (skips the theme picker
// and tour) and the login method is pinned to the Claude-account flow
// (skips the claude.ai-vs-Console picker). Existing values are preserved,
// so re-running add against a lived-in profile changes nothing.
func prepareClaudeLoginFastPath(configDir string) error {
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return err
	}
	statePath := filepath.Join(configDir, ".claude.json")
	state := map[string]any{}
	if body, err := os.ReadFile(statePath); err == nil {
		if err := json.Unmarshal(body, &state); err != nil {
			return fmt.Errorf("parse %s: %w", statePath, err)
		}
	}
	if onboarded, _ := state["hasCompletedOnboarding"].(bool); !onboarded {
		state["hasCompletedOnboarding"] = true
		out, err := json.MarshalIndent(state, "", "  ")
		if err != nil {
			return err
		}
		out = append(out, '\n')
		if err := os.WriteFile(statePath, out, 0o600); err != nil {
			return err
		}
	}
	settingsPath := filepath.Join(configDir, "settings.json")
	settings := map[string]any{}
	if body, err := os.ReadFile(settingsPath); err == nil {
		if err := json.Unmarshal(body, &settings); err != nil {
			return fmt.Errorf("parse %s: %w", settingsPath, err)
		}
	}
	if _, ok := settings["forceLoginMethod"]; ok {
		return nil
	}
	settings["forceLoginMethod"] = "claudeai"
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return os.WriteFile(settingsPath, out, 0o600)
}

func (r claudeRunner) list(ctx context.Context, numbered bool) error {
	infos := r.fetchInfos(ctx)
	displayClaudeProfiles(r.out, infos, numbered)
	return nil
}

func (r claudeRunner) defaultInteractive(ctx context.Context) error {
	profiles := r.store.ListProfiles()
	if len(profiles) == 0 {
		fmt.Fprintln(r.out, "No Claude profiles. Run 'sr claude add' to create one.")
		return nil
	}
	infos := r.fetchInfos(ctx)
	displayClaudeProfiles(r.out, infos, true)
	reader := bufio.NewReader(r.in)
	answer, err := promptLine(r.out, reader, "Switch to (#): ")
	if err != nil {
		return err
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return nil
	}
	if idx, err := strconv.Atoi(answer); err == nil && idx >= 1 && idx <= len(infos) {
		return r.switchProfile(infos[idx-1].Name)
	}
	return r.switchProfile(answer)
}

func (r claudeRunner) fetchInfos(ctx context.Context) []claude.ProfileInfo {
	profiles := r.store.ListProfiles()
	active := r.store.ActiveProfile()
	claudePath, _ := claude.DetectCLI()
	infos := make([]claude.ProfileInfo, len(profiles))
	var wg sync.WaitGroup
	for i, profile := range profiles {
		i, profile := i, profile
		infos[i] = claude.ProfileInfo{Name: profile.Name, Active: profile.Name == active, CreatedAt: profile.CreatedAt}
		wg.Add(1)
		go func() {
			defer wg.Done()
			claudeConfigDir := r.store.ClaudeConfigDir(profile.Name)
			if _, err := secureManagedClaudeProfileTransport(claudeConfigDir); err != nil {
				infos[i].Error = err
				return
			}
			var auth *claude.AuthStatus
			var credential *claude.CredentialInfo
			var authErr, credentialErr error
			var inner sync.WaitGroup
			inner.Add(2)
			go func() {
				defer inner.Done()
				auth, authErr = claude.AuthStatusForPath(ctx, claudePath, claudeConfigDir)
			}()
			go func() {
				defer inner.Done()
				credential, credentialErr = r.store.ReadCredential(ctx, claudeConfigDir)
			}()
			inner.Wait()
			if authErr != nil {
				infos[i].Error = authErr
				return
			}
			if credentialErr != nil {
				infos[i].Error = credentialErr
				return
			}
			infos[i].Auth = auth
			infos[i].Credential = credential
			if credential != nil && credential.AccessToken != "" {
				usage, err := claude.FetchUsage(ctx, r.client, credential.AccessToken)
				if err == nil {
					infos[i].Usage = usage
				}
			}
		}()
	}
	wg.Wait()
	return infos
}

func (r claudeRunner) switchProfile(selector string) error {
	profile, ok, err := r.store.MatchProfile(selector)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no profile matching %q", selector)
	}
	if err := r.store.SetActiveProfile(profile.Name); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Active Claude profile: %s\n", profile.Name)
	configDir := r.store.ClaudeConfigDir(profile.Name)
	launchMode, err := managedClaudeProfileLaunchMode(configDir)
	if err != nil {
		return err
	}
	if launchMode == managedClaudeLaunchNeedsMigration {
		fmt.Fprintf(r.out, "\nThis legacy plaintext profile needs a durable server identity. Repair it first with:\n\n  sr claude push %s\n\nThen launch it with:\n\n  sr claude run %s [claude args...]\n", profile.Name, profile.Name)
		return nil
	}
	if launchMode == managedClaudeLaunchWrapped {
		fmt.Fprintf(r.out, "\nThis profile uses a protected plaintext server. Launch it with:\n\n  sr claude run %s [claude args...]\n", profile.Name)
		return nil
	}
	fmt.Fprintf(r.out, "\n  export CLAUDE_CONFIG_DIR=%s\n", configDir)
	fmt.Fprintln(r.out, "\nOr add to shell rc: eval \"$(sr claude env)\"")
	return nil
}

func (r claudeRunner) remove(ctx context.Context, selector string) error {
	profile, ok, err := r.store.MatchProfile(selector)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("profile %q not found", selector)
	}
	if err := r.removeProfileInventory(ctx, profile.Name); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Removed Claude profile: %s\n", profile.Name)
	return nil
}

func (r claudeRunner) mutateProfileInventory(ctx context.Context, mutate func() (bool, error)) (committed bool, err error) {
	trackedMutate := func() (bool, error) {
		committed, err = mutate()
		return committed, err
	}
	if r.mutateProfileInventoryForTest != nil {
		err = r.mutateProfileInventoryForTest(ctx, trackedMutate)
		return committed, err
	}
	if r.ephemeral {
		return trackedMutate()
	}
	err = proxy.PublishAccountDiskMutation(ctx, r.store.Dir, trackedMutate)
	return committed, err
}

func (r claudeRunner) removeProfileInventory(ctx context.Context, name string) error {
	_, err := r.mutateProfileInventory(ctx, func() (bool, error) {
		removed, err := r.store.RemoveProfile(name)
		if err != nil {
			return removed, err
		}
		if !removed {
			return false, fmt.Errorf("profile %q not found", name)
		}
		return true, nil
	})
	return err
}

func (r claudeRunner) rollbackProfileInventory(ctx context.Context, name string) error {
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), claudeProfileReconcileTimeout)
	defer cancel()
	return r.removeProfileInventory(rollbackCtx, name)
}

func (r claudeRunner) removeUnpublishedProfile(ctx context.Context, name string) (bool, error) {
	removed, err := r.store.RemoveUnpublishedProfileContext(ctx, name)
	if err != nil {
		return removed, err
	}
	if !removed {
		return false, fmt.Errorf("profile %q not found", name)
	}
	return true, nil
}

func wrapClaudeReconcileError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func (r claudeRunner) publishProfileCompletion(ctx context.Context) (bool, error) {
	return r.mutateProfileInventory(ctx, func() (bool, error) {
		// Claude writes the credential outside Subrouter's process during the
		// interactive login. Publish a completion generation after verifying it
		// so a worker that observed profile creation before credential landing
		// receives a second, usable snapshot.
		return true, nil
	})
}

func (r claudeRunner) cleanupTemporaryInstance(ctx context.Context, dir string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), claudeProfileReconcileTimeout)
	defer cancel()
	return r.store.CleanupInstanceContext(cleanupCtx, dir)
}

func (r claudeRunner) profileInventoryReferencesDir(dir string) bool {
	for _, profile := range r.store.ListProfiles() {
		if profile.Dir == dir {
			return true
		}
	}
	return false
}

func (r claudeRunner) env() error {
	active := r.store.ActiveProfile()
	if active == "" {
		return nil
	}
	configDir := r.store.ClaudeConfigDir(active)
	launchMode, err := managedClaudeProfileLaunchMode(configDir)
	if err != nil {
		return err
	}
	if launchMode == managedClaudeLaunchNeedsMigration {
		return fmt.Errorf("profile %q is a legacy plaintext remote profile; repair it with 'sr claude push %s', then launch it with 'sr claude run %s [claude args...]'", active, active, active)
	}
	if launchMode == managedClaudeLaunchWrapped {
		return fmt.Errorf("profile %q uses a protected plaintext server; launch it with 'sr claude run %s [claude args...]' so Subrouter can verify the server for each run", active, active)
	}
	fmt.Fprintf(r.out, "export CLAUDE_CONFIG_DIR=%s\n", configDir)
	return nil
}

// runClaudeUntilCredential runs the interactive Claude login and polls for the
// OAuth credential landing in the profile's config dir. As soon as it appears,
// the Claude process is closed automatically so the user does not have to exit
// by hand. Returns the process exit error and whether we initiated the close
// (in which case a non-nil exit error is expected and not a failure).
func (r claudeRunner) runClaudeUntilCredential(ctx context.Context, cmd *exec.Cmd, claudeConfigDir string) (error, bool) {
	if err := cmd.Start(); err != nil {
		return err, false
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return err, false
		case <-ctx.Done():
			err := closeInteractiveProcess(cmd, done)
			return err, true
		case <-ticker.C:
			credential, _ := r.store.ReadCredential(ctx, claudeConfigDir)
			if credential == nil || credential.AccessToken == "" {
				continue
			}
			fmt.Fprintln(r.errOut, "\nLogin detected; closing Claude...")
			err := closeInteractiveProcess(cmd, done)
			r.restoreTerminal()
			return err, true
		}
	}
}

// closeInteractiveProcess ends an interactive TUI process: SIGINT twice (Claude
// requires a double Ctrl-C), then SIGTERM, then SIGKILL, waiting briefly after
// each signal.
func closeInteractiveProcess(cmd *exec.Cmd, done <-chan error) error {
	if cmd.Process == nil {
		return <-done
	}
	for i := 0; i < 2; i++ {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case err := <-done:
			return err
		case <-time.After(750 * time.Millisecond):
		}
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
	}
	_ = cmd.Process.Kill()
	return <-done
}

// restoreTerminal best-effort resets the controlling terminal in case the
// closed TUI left it in raw mode.
func (r claudeRunner) restoreTerminal() {
	stdin, ok := r.in.(*os.File)
	if !ok {
		return
	}
	cmd := exec.Command("stty", "sane")
	cmd.Stdin = stdin
	_ = cmd.Run()
}

func (r claudeRunner) runClaude(ctx context.Context, name string, extra []string) error {
	if name == "" {
		name = r.store.ActiveProfile()
	}
	if name == "" {
		return fmt.Errorf("no profile specified and no active profile set")
	}
	profile, ok, err := r.store.MatchProfile(name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("profile %q not found", name)
	}
	configDir := r.store.ClaudeConfigDir(profile.Name)
	secureBaseURL, err := secureManagedClaudeProfileTransport(configDir)
	if err != nil {
		return err
	}
	claudePath, ok := claude.DetectCLI()
	if !ok {
		return fmt.Errorf("Claude CLI not found. Install from https://claude.ai/download")
	}
	if err := r.store.SetActiveProfile(profile.Name); err != nil {
		return err
	}
	if sessionID := claude.ResumeSessionID(extra); sessionID != "" {
		from, migrateErr := r.store.MigrateSession(profile.Name, sessionID)
		if migrateErr != nil {
			fmt.Fprintf(r.errOut, "warning: could not migrate session %s: %v\n", sessionID, migrateErr)
		} else if from != "" {
			fmt.Fprintf(r.errOut, "Copied session %s from profile %q.\n", sessionID, from)
		}
	}
	launchArgs := extra
	var launchSettingsBody []byte
	if secureBaseURL != "" {
		settingsOverride, settingsErr := managedClaudeLaunchSettings(secureBaseURL, configDir)
		if settingsErr != nil {
			return settingsErr
		}
		launchSettingsBody = settingsOverride
	}
	cmd := exec.CommandContext(ctx, claudePath)
	if len(launchSettingsBody) > 0 {
		settingsArg, cleanupSettings, settingsErr := attachClaudeLaunchSettings(cmd, launchSettingsBody)
		if settingsErr != nil {
			return settingsErr
		}
		defer cleanupSettings()
		launchArgs, err = managedClaudeLaunchArgs(extra, settingsArg)
		if err != nil {
			return err
		}
	}
	cmd.Args = append([]string{claudePath}, launchArgs...)
	cmd.Stdin = r.in
	cmd.Stdout = r.out
	cmd.Stderr = r.errOut
	cmd.Env = claudeSettingsChildEnvironment(claude.EnvForConfigDir(configDir), secureBaseURL, configDir)
	return cmd.Run()
}

func managedClaudeLaunchArgs(args []string, settingsPath string) ([]string, error) {
	clean := make([]string, 0, len(args)+2)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			clean = append(clean, args[i:]...)
			break
		}
		switch {
		case arg == "--settings" || arg == "--managed-settings":
			if i+1 >= len(args) || args[i+1] == "--" || strings.HasPrefix(args[i+1], "-") {
				return nil, fmt.Errorf("%s requires a value", arg)
			}
			i++
		case strings.HasPrefix(arg, "--settings=") || strings.HasPrefix(arg, "--managed-settings="):
			_, value, _ := strings.Cut(arg, "=")
			if strings.TrimSpace(value) == "" {
				return nil, fmt.Errorf("%s requires a value", strings.SplitN(arg, "=", 2)[0])
			}
			// Drop user-provided settings and higher-precedence managed settings
			// before the option terminator. The verified transport overlay is the
			// only global settings source that can affect the launch.
		default:
			clean = append(clean, arg)
		}
	}
	return append([]string{"--settings", settingsPath}, clean...), nil
}

func secureManagedClaudeProfileTransport(configDir string) (string, error) {
	return secureManagedClaudeProfileTransportWithResolvers(configDir, net.DefaultResolver.LookupIPAddr, defaultTailscaleStatusLoader)
}

func secureManagedClaudeProfileTransportWithResolvers(configDir string, lookup serverIPLookup, load tailscaleStatusLoader) (string, error) {
	settingsPath := filepath.Join(configDir, "settings.json")
	body, err := os.ReadFile(settingsPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read managed Claude settings: %w", err)
	}
	var settings struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(body, &settings); err != nil {
		return "", fmt.Errorf("parse managed Claude settings: %w", err)
	}
	baseURL := strings.TrimSpace(settings.Env["ANTHROPIC_BASE_URL"])
	if baseURL == "" {
		return "", nil
	}
	authToken := strings.TrimSpace(settings.Env["ANTHROPIC_AUTH_TOKEN"])
	transportCredential := authToken
	if transportCredential == "subrouter" {
		transportCredential = ""
	}
	if transportCredential == "" {
		for _, key := range []string{
			"ANTHROPIC_API_KEY",
			"CLAUDE_CODE_OAUTH_TOKEN",
			"CLAUDE_CODE_API_KEY",
			"CLAUDE_CODE_AUTH_TOKEN",
		} {
			if strings.TrimSpace(settings.Env[key]) != "" {
				transportCredential = "protected-managed-credential"
				break
			}
		}
	}
	if transportCredential == "" && strings.TrimSpace(settings.Env["ANTHROPIC_CUSTOM_HEADERS"]) != "" {
		transportCredential = "protected-managed-custom-header"
	}
	serverURL := strings.TrimSpace(settings.Env[managedClaudeServerURLEnv])
	nodeID := strings.TrimSpace(settings.Env[managedClaudeTailscaleNodeEnv])
	if serverURL != "" || nodeID != "" {
		if serverURL == "" || nodeID == "" || baseURL != managedClaudeBlockedBaseURL {
			return "", fmt.Errorf("managed Claude server identity is incomplete; run 'sr claude push' to repair it")
		}
		server := srServerConfig{
			URL:             serverURL,
			TailscaleNodeID: nodeID,
		}
		if tenant.ValidKeyFormat(authToken) {
			server.TenantKey = authToken
		}
		proxyRoot := canonicalServerProxyRootURL(server)
		protectedServer := server
		parsedProxyRoot, _ := url.Parse(proxyRoot)
		if strings.TrimSpace(protectedServer.TenantKey) == "" && tenantKeyFromURL(parsedProxyRoot) == "" {
			// Force transport protection without allowing an arbitrary Claude
			// credential to become a tenant route segment.
			protectedServer.TenantKey = "protected-managed-credential"
		}
		secureBaseURL, err := secureTenantServerURLWithResolvers(context.Background(), proxyRoot, protectedServer, lookup, load)
		if err != nil {
			return "", fmt.Errorf("managed Claude profile has unsafe proxy transport: %w", err)
		}
		return secureBaseURL, nil
	}
	parsed, _ := url.Parse(baseURL)
	if parsed != nil && strings.EqualFold(parsed.Scheme, "http") && !isLoopbackServerHost(parsed.Hostname()) {
		return "", fmt.Errorf("managed Claude profile has unsafe proxy transport: plaintext server is missing an exact durable identity; run 'sr claude push' to repair it")
	}
	secureBaseURL, err := secureTenantServerURLWithResolvers(
		context.Background(), baseURL,
		srServerConfig{URL: baseURL, TenantKey: transportCredential},
		lookup, load,
	)
	if err != nil {
		return "", fmt.Errorf("managed Claude profile has unsafe proxy transport: %w", err)
	}
	return secureBaseURL, nil
}

type managedClaudeLaunchMode uint8

const (
	managedClaudeLaunchDirect managedClaudeLaunchMode = iota
	managedClaudeLaunchWrapped
	managedClaudeLaunchNeedsMigration
)

func managedClaudeProfileLaunchMode(configDir string) (managedClaudeLaunchMode, error) {
	body, err := os.ReadFile(filepath.Join(configDir, "settings.json"))
	if errors.Is(err, os.ErrNotExist) {
		return managedClaudeLaunchDirect, nil
	}
	if err != nil {
		return managedClaudeLaunchDirect, fmt.Errorf("read managed Claude settings: %w", err)
	}
	var settings struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(body, &settings); err != nil {
		return managedClaudeLaunchDirect, fmt.Errorf("parse managed Claude settings: %w", err)
	}
	if strings.TrimSpace(settings.Env[managedClaudeServerURLEnv]) != "" || strings.TrimSpace(settings.Env[managedClaudeTailscaleNodeEnv]) != "" {
		return managedClaudeLaunchWrapped, nil
	}
	parsed, _ := url.Parse(strings.TrimSpace(settings.Env["ANTHROPIC_BASE_URL"]))
	if parsed != nil && strings.EqualFold(parsed.Scheme, "http") && !isLoopbackServerHost(parsed.Hostname()) {
		return managedClaudeLaunchNeedsMigration, nil
	}
	return managedClaudeLaunchDirect, nil
}

func managedClaudeLaunchSettings(secureBaseURL, configDir string) ([]byte, error) {
	return claudeLaunchSettingsJSON(configDir, map[string]string{"ANTHROPIC_BASE_URL": secureBaseURL})
}

func proxyClaudeLaunchSettings(baseURL, proxyToken, configDir string) ([]byte, error) {
	baseURL = strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
	return claudeLaunchSettingsJSON(configDir, map[string]string{
		"ANTHROPIC_BASE_URL":       baseURL,
		"ANTHROPIC_AUTH_TOKEN":     proxyToken,
		"ANTHROPIC_CUSTOM_HEADERS": "X-Subrouter-Agent: claude",
	})
}

func claudeLaunchSettingsJSON(configDir string, env map[string]string) ([]byte, error) {
	// Claude merges --settings with the selected CLAUDE_CONFIG_DIR settings.
	// Empty strings are Claude's own neutral value for provider-selection flags:
	// its provider overlay clears every flag before enabling one. Clear every
	// known routing value here as well so a reused profile cannot retain a
	// Bedrock, Vertex, gateway, or other alternate-provider route underneath the
	// private launch settings. Intended Subrouter values are applied last.
	authoritative := make(map[string]string, len(claudeRoutingEnvKeys)+len(env))
	for _, key := range claudeRoutingEnvKeys {
		authoritative[key] = ""
	}
	// Keep Claude's dynamically consulted config selectors pinned to the same
	// durable profile selected for the child process. Clearing them would route
	// later session reads and writes back to the user's default profile.
	authoritative["CLAUDE_CONFIG_DIR"] = configDir
	authoritative["CLAUDE_CODE_CONFIG_DIR"] = configDir
	for key, value := range env {
		authoritative[key] = value
	}
	override, err := json.Marshal(map[string]any{"env": authoritative})
	if err != nil {
		return nil, fmt.Errorf("encode managed Claude launch settings: %w", err)
	}
	return override, nil
}

// claudeAWS launches Claude Code in Amazon Bedrock gateway mode, routed through
// the default Subrouter server's /bedrock endpoint. The server holds the AWS
// credentials and SigV4-signs each request, so teammates need no AWS access.
// All flags after an optional --model are passed through to Claude Code
// unchanged. --model accepts a friendly alias (fable, opus, sonnet, haiku) or a
// full Bedrock model id / inference profile; it defaults to Fable 5.
func (r srRunner) claudeAWS(ctx context.Context, args []string) error {
	server, ok, err := r.defaultRemoteServer()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("sr claude-aws needs a default Subrouter server; run '%s server use <name>'", r.programOrSubrouter())
	}
	protectedServer := server
	if strings.TrimSpace(protectedServer.TenantKey) == "" {
		protectedServer.TenantKey = "protected-bedrock-request"
	}
	secureRoot, err := secureTenantServerURL(ctx, server.URL, protectedServer)
	if err != nil {
		return err
	}
	baseURL := strings.TrimRight(secureRoot, "/") + "/bedrock"

	model := "fable"
	region := "us-east-1"
	passthrough := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--model", "-m":
			if i+1 >= len(args) {
				return fmt.Errorf("--model requires a value")
			}
			model = args[i+1]
			i++
		case "--aws-region":
			if i+1 >= len(args) {
				return fmt.Errorf("--aws-region requires a value")
			}
			region = args[i+1]
			i++
		default:
			passthrough = append(passthrough, args[i])
		}
	}

	claudePath, ok := claude.DetectCLI()
	if !ok {
		return fmt.Errorf("Claude CLI not found. Install from https://claude.ai/download")
	}
	cmd := exec.CommandContext(ctx, claudePath, passthrough...)
	cmd.Stdin = r.in
	cmd.Stdout = r.out
	cmd.Stderr = r.errOut
	env := append(os.Environ(),
		"CLAUDE_CODE_USE_BEDROCK=1",
		"CLAUDE_CODE_SKIP_BEDROCK_AUTH=1",
		"ANTHROPIC_BEDROCK_BASE_URL="+baseURL,
		"AWS_REGION="+region,
		"AWS_DEFAULT_REGION="+region,
		"ANTHROPIC_MODEL="+bedrockModelID(model),
		"ANTHROPIC_SMALL_FAST_MODEL="+bedrockSmallFastModelID,
	)
	if token := strings.TrimSpace(os.Getenv("SUBROUTER_BEDROCK_GATEWAY_TOKEN")); token != "" {
		env = append(env, "ANTHROPIC_AUTH_TOKEN="+token)
	}
	cmd.Env = directPlainHTTPEnvironment(env, baseURL)
	return cmd.Run()
}

// claudeDirect launches Claude Code straight against Anthropic on the user's own
// claude.ai login, guaranteeing subrouter (and any Bedrock/Vertex/Mantle
// gateway) is not used. It strips every routing/gateway env var plus
// ANTHROPIC_API_KEY (which would otherwise override the claude.ai login and bill
// pay-per-token), so the run cannot be silently pointed at a proxy or API key,
// then passes all flags through unchanged.
func (r srRunner) claudeDirect(ctx context.Context, args []string) error {
	claudePath, ok := claude.DetectCLI()
	if !ok {
		return fmt.Errorf("Claude CLI not found. Install from https://claude.ai/download")
	}
	cmd := exec.CommandContext(ctx, claudePath, args...)
	cmd.Stdin = r.in
	cmd.Stdout = r.out
	cmd.Stderr = r.errOut
	cmd.Env = envWithout(os.Environ(), claudeRoutingEnvKeys)
	return cmd.Run()
}

// claudeRoutingEnvKeys are the env vars that could route Claude Code through a
// proxy or cloud gateway instead of Anthropic directly.
var claudeRoutingEnvKeys = []string{
	"ANTHROPIC_BASE_URL",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_CUSTOM_HEADERS",
	"CLAUDE_CONFIG_DIR",
	"CLAUDE_CODE_CONFIG_DIR",
	"CLAUDE_CODE_OAUTH_TOKEN",
	"CLAUDE_CODE_API_KEY",
	"CLAUDE_CODE_AUTH_TOKEN",
	"CLAUDE_CODE_BASE_URL",
	"CLAUDE_CODE_USE_BEDROCK",
	"ANTHROPIC_BEDROCK_BASE_URL",
	"CLAUDE_CODE_SKIP_BEDROCK_AUTH",
	"CLAUDE_CODE_USE_VERTEX",
	"ANTHROPIC_VERTEX_BASE_URL",
	"CLAUDE_CODE_SKIP_VERTEX_AUTH",
	"CLAUDE_CODE_USE_FOUNDRY",
	"ANTHROPIC_FOUNDRY_BASE_URL",
	"CLAUDE_CODE_SKIP_FOUNDRY_AUTH",
	"CLAUDE_CODE_USE_MANTLE",
	"ANTHROPIC_BEDROCK_MANTLE_BASE_URL",
	"CLAUDE_CODE_SKIP_MANTLE_AUTH",
	"CLAUDE_CODE_USE_ANTHROPIC_AWS",
	"ANTHROPIC_AWS_BASE_URL",
	"CLAUDE_CODE_SKIP_ANTHROPIC_AWS_AUTH",
	"CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD",
	"ANTHROPIC_GOOGLE_CLOUD_BASE_URL",
	"CLAUDE_CODE_SKIP_ANTHROPIC_GOOGLE_CLOUD_AUTH",
	"CLAUDE_CODE_USE_GATEWAY",
}

// envWithout returns environ with the named keys removed (case-insensitive).
func envWithout(environ []string, keys []string) []string {
	drop := make(map[string]bool, len(keys))
	for _, k := range keys {
		drop[strings.ToLower(k)] = true
	}
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		if drop[strings.ToLower(name)] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

const bedrockSmallFastModelID = "us.anthropic.claude-haiku-4-5-20251001-v1:0"

// bedrockModelID maps a friendly model alias to a Bedrock inference profile id.
// A value that already looks like a Bedrock id (contains "anthropic.") is passed
// through unchanged, as is any unrecognized value.
func bedrockModelID(name string) string {
	trimmed := strings.TrimSpace(name)
	lower := strings.ToLower(trimmed)
	if strings.Contains(lower, "anthropic.") {
		return trimmed
	}
	switch lower {
	case "", "fable", "fable-5", "fable5", "claude-fable-5":
		return "us.anthropic.claude-fable-5"
	case "opus", "claude-opus-4-8", "opus-4-8":
		return "us.anthropic.claude-opus-4-8"
	case "sonnet", "claude-sonnet-5", "sonnet-5":
		return "us.anthropic.claude-sonnet-5"
	case "haiku", "claude-haiku-4-5":
		return bedrockSmallFastModelID
	default:
		return trimmed
	}
}

type claudeRow struct {
	label    string
	used     float64
	resetsIn string
}

func displayClaudeProfiles(out io.Writer, infos []claude.ProfileInfo, numbered bool) {
	if len(infos) == 0 {
		fmt.Fprintln(out, "No Claude profiles. Run 'sr claude add' to create one.")
		return
	}
	colored := colorEnabled(out)
	width := 0
	for _, info := range infos {
		for _, row := range collectClaudeRows(info) {
			width = max(width, len(row.label))
		}
	}
	fmt.Fprintln(out)
	for i, info := range infos {
		prefix := ""
		if numbered {
			prefix = fmt.Sprintf("%d) ", i+1)
		}
		active := ""
		if info.Active {
			active = " " + style(colored, ansiCyan, "(active)")
		}
		if info.Error != nil {
			fmt.Fprintf(out, "%s%s%s\n", style(colored, ansiDim, prefix), style(colored, ansiBold+ansiWhite, info.Name), active)
			fmt.Fprintf(out, "  %s\n\n", style(colored, ansiRed, "Error: "+info.Error.Error()))
			continue
		}
		if info.Auth == nil || !info.Auth.LoggedIn {
			fmt.Fprintf(out, "%s%s%s\n", style(colored, ansiDim, prefix), style(colored, ansiBold+ansiWhite, info.Name), active)
			fmt.Fprintln(out, "  "+style(colored, ansiDim, "not logged in"))
			fmt.Fprintln(out)
			continue
		}
		plan := ""
		if info.Auth.SubscriptionType != "" {
			plan = " " + style(colored, ansiDim, "["+info.Auth.SubscriptionType+"]")
		}
		fmt.Fprintf(out, "%s%s%s%s\n", style(colored, ansiDim, prefix), style(colored, ansiBold+ansiWhite, info.Name), plan, active)
		rows := collectClaudeRows(info)
		if len(rows) == 0 {
			detail := info.Auth.Email
			if detail == "" {
				detail = "unknown"
			}
			if info.Auth.OrgName != "" {
				detail += " (" + info.Auth.OrgName + ")"
			}
			fmt.Fprintf(out, "  %s\n\n", style(colored, ansiDim, detail))
			continue
		}
		for _, row := range rows {
			fmt.Fprintf(out, "  %s: %s %s", style(colored, ansiDim, pad(row.label, width)), renderBar(row.used, colored), style(colored, usageColor(row.used), formatPercentLeft(row.used)+" left"))
			if row.resetsIn != "" {
				fmt.Fprintf(out, " %s", style(colored, ansiDim, "resets in "+row.resetsIn))
			}
			fmt.Fprintln(out)
		}
		fmt.Fprintln(out)
	}
}

func collectClaudeRows(info claude.ProfileInfo) []claudeRow {
	if info.Usage == nil {
		return nil
	}
	var rows []claudeRow
	add := func(label string, limit *claude.RateLimit) {
		if limit == nil || limit.Utilization == nil {
			return
		}
		rows = append(rows, claudeRow{label: label, used: *limit.Utilization, resetsIn: formatResetTime(limit.ResetsAt)})
	}
	add("5h limit", info.Usage.FiveHour)
	add("7d limit", info.Usage.SevenDay)
	add("Opus (weekly)", info.Usage.SevenDayOpus)
	add("Sonnet (weekly)", info.Usage.SevenDaySonnet)
	if info.Usage.ExtraUsage != nil && info.Usage.ExtraUsage.IsEnabled && info.Usage.ExtraUsage.Utilization != nil {
		rows = append(rows, claudeRow{label: "Extra usage", used: *info.Usage.ExtraUsage.Utilization})
	}
	return rows
}

func formatResetTime(value string) string {
	if value == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return ""
	}
	seconds := int64(time.Until(t).Seconds())
	if seconds < 0 {
		seconds = 0
	}
	return formatDuration(seconds)
}
