package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/proxy"
	"github.com/manaflow-ai/subrouter/internal/selectacct"
	"github.com/manaflow-ai/subrouter/internal/session"
	"github.com/manaflow-ai/subrouter/internal/transcript"
)

func main() {
	program := filepath.Base(os.Args[0])
	if program == "cx" {
		if err := cx(os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "cx:", err)
			os.Exit(1)
		}
		return
	}
	if err := runForProgram(program, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "subrouter:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	return runForProgram("subrouter", args)
}

func runForProgram(program string, args []string) error {
	if len(args) == 0 {
		if program == "sr" {
			return cxForProgram(program, nil)
		}
		usage(program)
		return nil
	}

	switch args[0] {
	case "serve":
		return serve(args[1:])
	case "accounts":
		return listAccounts()
	case "codex":
		return codex(args[1:])
	case "cx":
		return cx(args[1:])
	case "install-daemon":
		return installDaemon(args[1:])
	case "install-systemd":
		return installSystemd(args[1:])
	case "help", "-h", "--help":
		usage(program)
		return nil
	default:
		if isDirectCXCommand(args[0]) || strings.Contains(args[0], "@") {
			return cxForProgram(program, args)
		}
		return fmt.Errorf("unknown command %q", args[0])
	}
}

var directCXCommands = map[string]struct{}{
	"add":              {},
	"add-admin-key":    {},
	"add-api-key":      {},
	"add-key":          {},
	"admin-keys":       {},
	"attach-project":   {},
	"claude":           {},
	"g":                {},
	"gemini":           {},
	"gui":              {},
	"gui-switch":       {},
	"gui-use":          {},
	"import":           {},
	"list":             {},
	"list-admin-keys":  {},
	"login":            {},
	"ls":               {},
	"pick":             {},
	"remove":           {},
	"remove-admin-key": {},
	"rm":               {},
	"server":           {},
	"servers":          {},
	"status":           {},
	"switch":           {},
	"usage":            {},
	"use":              {},
}

func isDirectCXCommand(command string) bool {
	_, ok := directCXCommands[command]
	return ok
}

func serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := flags.String("addr", "127.0.0.1:31415", "listen address")
	upstreamRaw := flags.String("upstream", "", "force one upstream base URL for all accounts")
	codexUpstreamRaw := flags.String("codex-upstream", "https://chatgpt.com/backend-api/codex", "Codex subscription upstream base URL")
	apiUpstreamRaw := flags.String("api-upstream", "https://api.openai.com", "OpenAI API-key upstream base URL")
	claudeUpstreamRaw := flags.String("claude-upstream", "https://api.anthropic.com", "Claude subscription upstream base URL")
	sessionPath := flags.String("sessions", session.DefaultStorePath(), "session assignment store")
	transcriptDir := flags.String("transcripts", "", "directory for raw Subrouter transcript JSONL files")
	transcriptGCSURI := flags.String("transcript-gcs-uri", "", "optional gs:// bucket/prefix for background transcript sync")
	transcriptGCSSyncInterval := flags.Duration("transcript-gcs-sync-interval", 5*time.Minute, "interval for background transcript GCS sync; 0 disables")
	cxSwitchInterval := flags.Duration("cx-switch-interval", defaultCXSwitchInterval, "interval for switching active cx account to the best OAuth account; 0 disables")
	usageScoreTTL := flags.Duration("usage-score-ttl", 30*time.Second, "maximum age for usage scores before account selection refreshes them; 0 disables")
	maxBodyBytes := flags.Int64("max-body-bytes", 1<<20, "max JSON request body bytes to inspect for session IDs")
	fetchUsage := flags.Bool("fetch-usage", true, "fetch Codex usage on startup for account selection")
	if err := flags.Parse(args); err != nil {
		return err
	}

	var upstream *url.URL
	if *upstreamRaw != "" {
		var err error
		upstream, err = url.Parse(*upstreamRaw)
		if err != nil {
			return err
		}
	}
	codexUpstream, err := url.Parse(*codexUpstreamRaw)
	if err != nil {
		return err
	}
	apiUpstream, err := url.Parse(*apiUpstreamRaw)
	if err != nil {
		return err
	}
	claudeUpstream, err := url.Parse(*claudeUpstreamRaw)
	if err != nil {
		return err
	}

	store, err := session.NewStore(*sessionPath)
	if err != nil {
		return err
	}

	codexStore := accounts.DefaultCodexStore()
	codexAccounts, err := codexStore.List()
	if err != nil {
		return err
	}
	claudeAccounts, err := agentclaude.DefaultStore().ListAccounts(context.Background())
	if err != nil {
		slog.Warn("Claude accounts skipped", "error", err)
		claudeAccounts = nil
	}
	scores := fallbackScores(codexAccounts)
	if *fetchUsage {
		fetchedScores, successful := fetchCodexScoresWithStore(context.Background(), codexStore, codexAccounts)
		if successful > 0 {
			scores = fetchedScores
		} else {
			slog.Warn("initial usage score fetch skipped", "reason", "no fresh OAuth usage scores")
		}
	}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(scores))
	outboundTransport := proxy.NewOutboundTransport()
	accountRef := proxy.NewAccountRef(codexStore, codexAccounts, &http.Client{
		Timeout:   15 * time.Second,
		Transport: outboundTransport,
	})

	server := proxy.Server{
		Upstream:       upstream,
		CodexUpstream:  codexUpstream,
		APIUpstream:    apiUpstream,
		ClaudeUpstream: claudeUpstream,
		Accounts:       claudeAccounts,
		AccountRef:     accountRef,
		Sessions:       store,
		SchedulerRef:   schedulerRef,
		UsageScoreTTL:  usageScoreTTLForServe(*fetchUsage, *usageScoreTTL),
		Transport:      outboundTransport,
		Logger:         slog.Default(),
		MaxBodyBytes:   *maxBodyBytes,
		Transcripts:    transcript.NewRecorder(*transcriptDir),
	}
	transcriptGCSSyncer := transcript.NewGCSSyncer(transcript.GCSSyncerConfig{
		SourceDir:   *transcriptDir,
		Destination: *transcriptGCSURI,
		Interval:    *transcriptGCSSyncInterval,
		Logger:      slog.Default(),
	})
	if transcriptGCSSyncer.Enabled() {
		go transcriptGCSSyncer.Run(context.Background())
	}
	if *cxSwitchInterval > 0 && *fetchUsage {
		go runCXAutoSwitch(context.Background(), cxAutoSwitchConfig{
			Interval:     *cxSwitchInterval,
			AccountsFunc: accountRef.All,
			Sessions:     store,
			SchedulerRef: schedulerRef,
			Logger:       slog.Default(),
		})
	} else if *cxSwitchInterval > 0 {
		slog.Info("cx auto-switch disabled because usage fetching is disabled", "interval", cxSwitchInterval.String())
	}

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	if upstream != nil {
		slog.Info("subrouter listening", "addr", *addr, "upstream", upstream.String(), "codex_accounts", len(codexAccounts), "claude_accounts", len(claudeAccounts), "transcripts", *transcriptDir, "transcript_gcs_uri", *transcriptGCSURI)
	} else {
		slog.Info("subrouter listening", "addr", *addr, "codex_upstream", codexUpstream.String(), "api_upstream", apiUpstream.String(), "claude_upstream", claudeUpstream.String(), "codex_accounts", len(codexAccounts), "claude_accounts", len(claudeAccounts), "transcripts", *transcriptDir, "transcript_gcs_uri", *transcriptGCSURI)
	}
	return httpServer.ListenAndServe()
}

func usageScoreTTLForServe(fetchUsage bool, ttl time.Duration) time.Duration {
	if !fetchUsage {
		return 0
	}
	return ttl
}

func fetchCodexScores(ctx context.Context, codexAccounts []accounts.Account) []selectacct.Score {
	scores, _ := fetchCodexScoresWithSuccess(ctx, codexAccounts)
	return scores
}

func fetchCodexScoresWithSuccess(ctx context.Context, codexAccounts []accounts.Account) ([]selectacct.Score, int) {
	return fetchCodexScoresWithStore(ctx, accounts.DefaultCodexStore(), codexAccounts)
}

func fetchCodexScoresWithStore(ctx context.Context, store accounts.CodexStore, codexAccounts []accounts.Account) ([]selectacct.Score, int) {
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: proxy.NewOutboundTransport(),
	}
	scores := fallbackScores(codexAccounts)
	scoreByID := make(map[string]int, len(scores))
	for i, score := range scores {
		scoreByID[score.AccountID] = i
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	successful := 0
	for _, account := range codexAccounts {
		if account.AuthMode != accounts.AuthModeOAuth {
			continue
		}
		wg.Add(1)
		go func(account accounts.Account) {
			defer wg.Done()
			stored, ok, err := store.FindStored(account.ID)
			if err != nil || !ok {
				slog.Warn("account refresh lookup failed", "account", account.ID, "error", err)
			} else if refreshed, _, err := store.RefreshStoredIfExpired(ctx, client, stored); err != nil {
				slog.Warn("account refresh failed", "account", account.ID, "error", err)
			} else if refreshedAccount, ok := refreshed.Account(refreshed.SourcePath(store)); ok {
				account = refreshedAccount
			}
			windows, err := accounts.FetchCodexUsage(ctx, client, account)
			if err != nil {
				slog.Warn("usage fetch failed", "account", account.ID, "error", err)
				mu.Lock()
				if idx, ok := scoreByID[account.ID]; ok {
					scores[idx] = selectacct.Score{AccountID: account.ID, Headroom: 0}
				}
				mu.Unlock()
				return
			}
			limitWindows := make([]selectacct.LimitWindow, 0, len(windows))
			for _, window := range windows {
				limitWindows = append(limitWindows, selectacct.LimitWindow{
					Name:               window.Name,
					UsedPercent:        window.UsedPercent,
					LimitWindowSeconds: window.LimitWindowSeconds,
					ResetAfterSeconds:  window.ResetAfterSeconds,
				})
			}
			score := selectacct.ScoreFromLimitWindows(account.ID, 0, limitWindows)
			mu.Lock()
			if idx, ok := scoreByID[account.ID]; ok {
				scores[idx] = score
				successful++
			}
			mu.Unlock()
		}(account)
	}
	wg.Wait()

	for _, score := range scores {
		slog.Info("account score", "account", score.AccountID, "headroom", score.Headroom)
	}
	return scores, successful
}

func fallbackScores(codexAccounts []accounts.Account) []selectacct.Score {
	scores := make([]selectacct.Score, 0, len(codexAccounts))
	for _, account := range codexAccounts {
		headroom := 1.0
		if account.AuthMode == accounts.AuthModeAPIKey {
			headroom = 0.01
		}
		scores = append(scores, selectacct.Score{AccountID: account.ID, Headroom: headroom, ShortHeadroom: headroom})
	}
	return scores
}

func listAccounts() error {
	codexAccounts, err := accounts.DefaultCodexStore().List()
	if err != nil {
		return err
	}
	if len(codexAccounts) == 0 {
		fmt.Println("No Codex accounts found. Run: subrouter add")
		return nil
	}
	for _, account := range codexAccounts {
		fmt.Printf("%s\t%s\t%s\n", account.ID, account.Provider, account.AuthMode)
	}
	return nil
}

func usage(program string) {
	fmt.Print(usageText(program))
}

func usageText(program string) string {
	if program == "" {
		program = "subrouter"
	}
	return fmt.Sprintf(`%[1]s routes AI coding-agent traffic across subscription accounts.

Usage:
  %[1]s                    Show Codex usage for all accounts and switch
  %[1]s add                Add a new Codex account (opens OAuth login)
  %[1]s add-key            Add a Codex API key account
  %[1]s import             Import current ~/.codex/auth.json account
  %[1]s list               List all Codex accounts
  %[1]s switch [email]     Switch active Codex account and sync OpenCode/pi
  %[1]s g [email]          Switch active account, sync OpenCode/pi, and restart Codex.app
  %[1]s gui [email]        Switch active account, sync OpenCode/pi, and restart Codex.app
  %[1]s gui-switch [email] Switch active account, sync OpenCode/pi, and restart Codex.app
  %[1]s remove <email>     Remove a Codex account
  %[1]s status             Show Codex usage (non-interactive)
  %[1]s pick               Switch to the recommended account, failing if none has quota
  %[1]s usage [days]       Refresh and show API-key spend

  %[1]s server             Manage Subrouter servers
  %[1]s server add <name> --url <url> [--default]
  %[1]s server use <name>
  %[1]s server rename <old> <new>
  %[1]s server install <name>
  %[1]s server login <name> [--device-auth]
  %[1]s server sync <name> [--device-auth] [--yes]

  %[1]s admin-keys         List stored OpenAI admin keys
  %[1]s add-admin-key      Add an sk-admin-* key
  %[1]s remove-admin-key <label>
  %[1]s attach-project <api-key-label> [--project-id <id-or-name>]

  %[1]s claude             Manage Claude Code profiles
  %[1]s gemini             Manage Gemini profiles

  %[1]s serve [--addr 127.0.0.1:31415] [--fetch-usage=true] [--codex-upstream URL] [--claude-upstream URL] [--transcripts ~/.subrouter/transcripts] [--transcript-gcs-uri gs://bucket/prefix]
  %[1]s accounts
  %[1]s codex [codex args...]
  %[1]s install-daemon [--start=true]       macOS LaunchAgent
  %[1]s install-systemd [--start=true]      Linux systemd service

Session stickiness:
  Prefer sending X-Subrouter-Session per conversation.
  Send X-Subrouter-Agent when the client is not Codex.
  Send X-Subrouter-User-Email for teammate-level observability.
  Send X-Subrouter-Account-ID to force a specific account, including an API-key account.
  Subrouter switches active cx account every 10m by default; set --cx-switch-interval=0 to disable.
  For %[1]s codex, set SUBROUTER_CODEX_USER_EMAIL and/or SUBROUTER_CODEX_ACCOUNT_ID instead.
  The proxy also checks common session headers, query params, and small JSON bodies.
`, program)
}
