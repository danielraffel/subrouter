package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/selectacct"
)

const cxUsageCacheTTL = time.Hour

const (
	ansiReset    = "\x1b[0m"
	ansiBold     = "\x1b[1m"
	ansiDim      = "\x1b[2m"
	ansiGreen    = "\x1b[32m"
	ansiYellow   = "\x1b[33m"
	ansiRed      = "\x1b[31m"
	ansiCyan     = "\x1b[36m"
	ansiWhite    = "\x1b[37m"
	ansiBGGreen  = "\x1b[42m"
	ansiBGYellow = "\x1b[43m"
	ansiBGRed    = "\x1b[41m"
	ansiBGGray   = "\x1b[100m"
)

const cxHelp = `cx - Manage multiple Codex accounts

Usage:
  cx                    Show Codex usage for all accounts and switch
  cx add                Add a new Codex account (opens OAuth login)
  cx add-key            Add a Codex API key account
  cx import             Import current ~/.codex/auth.json account
  cx list               List all Codex accounts
  cx switch [email]     Switch active Codex account and sync OpenCode/pi
  cx gui-switch [email] Switch active account, sync OpenCode/pi, and restart Codex.app
  cx remove <email>     Remove a Codex account
  cx status             Show Codex usage (non-interactive)
  cx usage [days]       Refresh and show API-key spend

  cx server             Manage Subrouter servers
  cx server add <name> --url <url> [--default]
  cx server use <name>
  cx server rename <old> <new>
  cx server install <name>
  cx server login <name> [--device-auth]
  cx server sync <name> [--device-auth] [--yes]

  cx admin-keys         List stored OpenAI admin keys
  cx add-admin-key      Add an sk-admin-* key
  cx remove-admin-key <label>
  cx attach-project <api-key-label> [--project-id <id-or-name>]

  cx claude             Manage Claude Code profiles
  cx gemini             Manage Gemini profiles

These account commands also work at top level as subrouter <command> and sr <command>.
The subrouter cx <command> form is kept as a compatibility alias.
`

type cxRunner struct {
	program string
	store   accounts.CodexStore
	in      io.Reader
	out     io.Writer
	errOut  io.Writer
	client  *http.Client
	cmd     cxCommandRunner
}

type cxSwitchOptions struct {
	restartCodexGUI bool
}

type cxUsageRow struct {
	email            string
	active           bool
	planType         string
	windows          []accounts.UsageWindow
	credits          *accounts.CreditsInfo
	apiKeySpend      *accounts.APIKeyUsageSnapshot
	apiKeyHint       string
	err              error
	score            selectacct.Score
	gtoReason        string
	gtoRecommended   bool
	cooked           bool
	cookedReason     string
	tempCooked       bool
	tempCookedReason string
	authMode         accounts.AuthMode
}

func cx(args []string) error {
	return cxForProgram("cx", args)
}

func cxForProgram(program string, args []string) error {
	runner := cxRunner{
		program: program,
		store:   accounts.DefaultCodexStore(),
		in:      os.Stdin,
		out:     os.Stdout,
		errOut:  os.Stderr,
		client:  &http.Client{Timeout: 120 * time.Second},
	}
	return runner.run(context.Background(), args)
}

func (r cxRunner) run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return r.defaultInteractive(ctx, cxSwitchOptions{})
	}
	switch args[0] {
	case "add", "login":
		return r.add(ctx)
	case "add-key", "add-api-key":
		return r.addKey()
	case "import":
		return r.importActive()
	case "list", "ls":
		return r.list()
	case "switch", "use":
		selector, opts, err := parseCXSwitchArgs(args[1:], cxSwitchOptions{})
		if err != nil {
			return err
		}
		if selector == "" {
			return r.defaultInteractive(ctx, opts)
		}
		return r.switchAccount(ctx, selector, opts)
	case "gui-switch", "gui-use":
		selector, opts, err := parseCXSwitchArgs(args[1:], cxSwitchOptions{restartCodexGUI: true})
		if err != nil {
			return err
		}
		if selector == "" {
			return r.defaultInteractive(ctx, opts)
		}
		return r.switchAccount(ctx, selector, opts)
	case "remove", "rm":
		if len(args) < 2 {
			return fmt.Errorf("usage: subrouter remove <email>")
		}
		return r.remove(args[1])
	case "status":
		return r.status(ctx)
	case "usage":
		days := 30
		if len(args) > 1 {
			parsed, err := strconv.Atoi(args[1])
			if err != nil || parsed < 1 || parsed > 30 {
				return fmt.Errorf("days must be 1..30")
			}
			days = parsed
		}
		return r.usage(ctx, days)
	case "add-admin-key":
		return r.addAdminKey(ctx)
	case "list-admin-keys", "admin-keys":
		return r.listAdminKeys()
	case "remove-admin-key":
		if len(args) < 2 {
			return fmt.Errorf("usage: subrouter remove-admin-key <label>")
		}
		return r.removeAdminKey(args[1])
	case "attach-project":
		if len(args) < 2 {
			return fmt.Errorf("usage: subrouter attach-project <api-key-label> [--project-id <id-or-name>]")
		}
		projectID := ""
		if idx := indexOf(args, "--project-id"); idx >= 0 && idx+1 < len(args) {
			projectID = args[idx+1]
		}
		return r.attachProject(ctx, args[1], projectID)
	case "server", "servers":
		return r.server(ctx, args[1:])
	case "help", "-h", "--help":
		fmt.Fprint(r.out, cxHelp)
		return nil
	case "claude":
		return r.claude(ctx, args[1:])
	case "gemini":
		return r.gemini(args[1:])
	default:
		if strings.Contains(args[0], "@") {
			return r.statusOne(ctx, args[0])
		}
		return fmt.Errorf("unknown account command %q\n%s", args[0], cxHelp)
	}
}

func (r cxRunner) add(ctx context.Context) error {
	previousActive, err := r.store.DetectActiveAccount()
	if err != nil {
		return err
	}
	if err := r.store.SyncActiveToStore(); err != nil {
		return err
	}
	fmt.Fprintln(r.out, "Opening Codex OAuth login...")
	if err := r.commandRunner().Run(ctx, "codex", []string{"login"}, r.in, r.out, r.errOut); err != nil {
		return fmt.Errorf("codex login failed: %w", err)
	}
	account, existed, err := r.store.ImportActive()
	if err != nil {
		return err
	}
	if existed {
		fmt.Fprintf(r.out, "\nUpdated account: %s\n", account.Email)
	} else {
		fmt.Fprintf(r.out, "\nAdded account: %s\n", account.Email)
	}
	if previousActive != "" && previousActive != account.Email {
		if err := r.store.SwitchActive(previousActive); err != nil {
			return fmt.Errorf("restore active account %s: %w", previousActive, err)
		}
		previous, ok, err := r.store.FindStored(previousActive)
		if err != nil {
			return err
		}
		if ok {
			for _, result := range syncCodexCompatibleAuth(previous) {
				if result.Err != nil {
					fmt.Fprintf(r.errOut, "Warning: %s auth sync failed: %s\n", result.Tool, result.Err)
					continue
				}
				fmt.Fprintf(r.out, "Synced %s auth: %s\n", result.Tool, result.Path)
			}
		}
		fmt.Fprintf(r.out, "Restored active account: %s\n", previousActive)
	}
	return nil
}

func (r cxRunner) addKey() error {
	if err := r.store.SyncActiveToStore(); err != nil {
		return err
	}
	reader := bufio.NewReader(r.in)
	label, err := promptLine(r.out, reader, "Label (e.g. work, personal): ")
	if err != nil {
		return err
	}
	key, err := promptLine(r.out, reader, "API key (sk-...): ")
	if err != nil {
		return err
	}
	account, existed, err := r.store.AddAPIKey(label, key)
	if err != nil {
		return err
	}
	if existed {
		fmt.Fprintf(r.out, "Updated API key account: %s\n", account.APIKeyLabel())
	} else {
		fmt.Fprintf(r.out, "Added API key account: %s\n", account.APIKeyLabel())
	}
	return nil
}

func (r cxRunner) importActive() error {
	account, existed, err := r.store.ImportActive()
	if err != nil {
		return err
	}
	if existed {
		fmt.Fprintf(r.out, "Updated existing account: %s\n", account.Email)
	} else {
		fmt.Fprintf(r.out, "Imported account: %s\n", account.Email)
	}
	return nil
}

func (r cxRunner) autoImportIfEmpty() error {
	all, err := r.store.ListStored()
	if err != nil || len(all) > 0 {
		return err
	}
	account, _, err := r.store.ImportActive()
	if err == nil {
		fmt.Fprintf(r.out, "Auto-imported active account: %s\n\n", account.Email)
	}
	return nil
}

func (r cxRunner) list() error {
	all, err := r.store.ListStored()
	if err != nil {
		return err
	}
	active, err := r.store.DetectActiveAccount()
	if err != nil {
		return err
	}
	if len(all) == 0 {
		fmt.Fprintln(r.out, "No accounts configured. Run 'subrouter add' to add one.")
		return nil
	}
	fmt.Fprintln(r.out)
	for _, account := range all {
		marker := ""
		if account.Email == active {
			marker = " *"
		}
		fmt.Fprintf(r.out, "  %s%s (added %s)\n", displayAccountName(account.Email), marker, formatDate(account.AddedAt))
	}
	fmt.Fprintln(r.out)
	fmt.Fprintln(r.out, "* = currently active in ~/.codex/auth.json")
	return nil
}

func (r cxRunner) status(ctx context.Context) error {
	if err := r.autoImportIfEmpty(); err != nil {
		return err
	}
	rows, err := r.fetchUsageRows(ctx)
	if err != nil {
		return err
	}
	displayUsageRows(r.out, rows, false)
	return nil
}

func (r cxRunner) statusOne(ctx context.Context, selector string) error {
	if err := r.autoImportIfEmpty(); err != nil {
		return err
	}
	all, err := r.fetchUsageRows(ctx)
	if err != nil {
		return err
	}
	var matches []cxUsageRow
	lower := strings.ToLower(selector)
	for _, row := range all {
		if strings.Contains(strings.ToLower(row.email), lower) {
			matches = append(matches, row)
		}
	}
	if len(matches) == 0 {
		return fmt.Errorf("no account found for %s", selector)
	}
	displayUsageRows(r.out, matches, false)
	return nil
}

func (r cxRunner) defaultInteractive(ctx context.Context, opts cxSwitchOptions) error {
	if err := r.autoImportIfEmpty(); err != nil {
		return err
	}
	rows, err := r.fetchUsageRows(ctx)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Fprintln(r.out, "No accounts configured. Run 'subrouter add' to add one.")
		return nil
	}
	switched, err := r.autoSwitchExhaustedActive(ctx, rows, opts)
	if err != nil {
		return err
	}
	if switched {
		rows, err = r.fetchUsageRows(ctx)
		if err != nil {
			return err
		}
	}
	allCooked := allOAuthRowsCooked(rows)
	allBlocked := allOAuthRowsBlockedForNewSession(rows)
	if allCooked {
		fmt.Fprintln(r.out)
		fmt.Fprintln(r.out, "All of your OAuth accounts are cooked. Every usable 7d window is fully consumed, so cx will keep the current active account and will not switch.")
	} else if allBlocked {
		fmt.Fprintln(r.out)
		fmt.Fprintln(r.out, "All of your OAuth accounts are unavailable for new sessions. Usable windows are exhausted, so cx will keep the current active account and will not switch.")
	}
	displayUsageRows(r.out, rows, true)
	if switched {
		return nil
	}
	if allCooked || allBlocked || !hasSwitchableUsageRows(rows) {
		return nil
	}

	reader := bufio.NewReader(r.in)
	answer, err := promptLine(r.out, reader, "Switch to (#): ")
	if err != nil {
		return err
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return nil
	}
	if idx, err := strconv.Atoi(answer); err == nil && idx >= 1 && idx <= len(rows) {
		row := rows[idx-1]
		if err := ensureUsageRowSwitchable(row); err != nil {
			return err
		}
		return r.switchAccount(ctx, row.email, opts)
	}
	return r.switchAccount(ctx, answer, opts)
}

func (r cxRunner) autoSwitchExhaustedActive(ctx context.Context, rows []cxUsageRow, opts cxSwitchOptions) (bool, error) {
	active := activeUsageRow(rows)
	if active == nil || active.err != nil || active.authMode != accounts.AuthModeOAuth || !exhaustedForNewSession(active.score) {
		return false, nil
	}
	target := recommendedUsableOAuthRow(rows)
	if target == nil || target.email == active.email {
		return false, nil
	}
	if err := r.switchAccount(ctx, target.email, opts); err != nil {
		return false, err
	}
	fmt.Fprintf(r.out, "Auto-switched to %s because active account %s is exhausted.\n", target.email, active.email)
	return true, nil
}

func activeUsageRow(rows []cxUsageRow) *cxUsageRow {
	for i := range rows {
		if rows[i].active {
			return &rows[i]
		}
	}
	return nil
}

func recommendedUsableOAuthRow(rows []cxUsageRow) *cxUsageRow {
	for i := range rows {
		if rows[i].gtoRecommended && recommendedForNewSession(rows[i]) && rows[i].authMode == accounts.AuthModeOAuth {
			return &rows[i]
		}
	}
	return nil
}

func (r cxRunner) fetchUsageRows(ctx context.Context) ([]cxUsageRow, error) {
	all, err := r.store.ListStored()
	if err != nil {
		return nil, err
	}
	active, err := r.store.DetectActiveAccount()
	if err != nil {
		return nil, err
	}
	admins, err := r.store.ListAdminKeys()
	if err != nil {
		return nil, err
	}

	rows := make([]cxUsageRow, len(all))
	var wg sync.WaitGroup
	for i, account := range all {
		i, account := i, account
		rows[i] = cxUsageRow{email: account.Email, active: account.Email == active}
		if account.IsAPIKey() {
			rows[i].authMode = accounts.AuthModeAPIKey
			rows[i].score = selectacct.Score{AccountID: account.Email, Headroom: 0.01, ShortHeadroom: 0.01}
			rows[i].planType = "api key"
			rows[i].apiKeyHint = r.apiKeyHint(account, admins)
			if admin, ok, err := r.store.PickAdminKeyFor(account); err == nil && ok {
				if fresh, ok, err := r.store.ReadUsageCache(admin.Label, account.ProjectID, cxUsageCacheTTL); err == nil && ok {
					rows[i].apiKeySpend = &fresh
					rows[i].apiKeyHint = ""
				} else if stale, ok, err := r.store.ReadUsageCacheStale(admin.Label, account.ProjectID); err == nil && ok {
					rows[i].apiKeySpend = &stale
					rows[i].apiKeyHint = ""
				}
			}
			continue
		}

		rows[i].authMode = accounts.AuthModeOAuth
		wg.Add(1)
		go func() {
			defer wg.Done()
			refreshed, _, refreshErr := r.store.RefreshStoredIfExpired(ctx, r.client, account)
			if refreshErr != nil {
				rows[i].err = refreshErr
				rows[i].score = selectacct.Score{AccountID: account.Email, Headroom: 0, ShortHeadroom: 0}
				return
			}
			acct := accountFromStored(refreshed)
			details, err := accounts.FetchCodexUsageDetails(ctx, r.client, acct)
			if err != nil {
				rows[i].err = err
				rows[i].score = selectacct.Score{AccountID: account.Email, Headroom: 0, ShortHeadroom: 0}
				return
			}
			rows[i].planType = details.PlanType
			rows[i].windows = details.Windows
			rows[i].credits = details.Credits
			rows[i].score = scoreFromWindows(account.Email, details.Windows)
			rows[i].cooked, rows[i].cookedReason = cookedFromWindows(details.Windows)
			rows[i].tempCooked, rows[i].tempCookedReason = tempCookedFromWindows(details.Windows)
		}()
	}
	wg.Wait()
	rankUsageRows(rows)
	return rows, nil
}

func (r cxRunner) apiKeyHint(account accounts.StoredCodexAccount, admins []accounts.AdminKeyEntry) string {
	if len(admins) == 0 {
		return "no admin key, run 'subrouter add-admin-key' to enable spend display"
	}
	admin, ok, err := r.store.PickAdminKeyFor(account)
	if err != nil || !ok {
		return "no admin key linked"
	}
	return fmt.Sprintf("no cached usage, run 'subrouter usage' (admin: %s)", admin.Label)
}

func (r cxRunner) switchAccount(ctx context.Context, selector string, opts cxSwitchOptions) error {
	account, ok, err := r.store.FindStored(selector)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no account found matching %q", selector)
	}
	if err := r.store.SyncActiveToStore(); err != nil {
		return err
	}
	refreshed, didRefresh, err := r.store.RefreshStoredIfExpired(ctx, r.client, account)
	if err != nil {
		fmt.Fprintf(r.errOut, "Warning: token refresh failed, using cached tokens: %s\n", err)
		refreshed = account
	} else if didRefresh {
		account = refreshed
	}
	if err := r.ensureSwitchableForFreshUsage(ctx, account); err != nil {
		return err
	}
	if err := accounts.WriteActiveCodexAuth(account.Auth); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Switched to %s\n", account.Email)
	for _, result := range syncCodexCompatibleAuth(account) {
		if result.Err != nil {
			fmt.Fprintf(r.errOut, "Warning: %s auth sync failed: %s\n", result.Tool, result.Err)
			continue
		}
		fmt.Fprintf(r.out, "Synced %s auth: %s\n", result.Tool, result.Path)
	}
	if opts.restartCodexGUI {
		return r.reportCodexGUIRestart(ctx)
	}
	fmt.Fprintln(r.out, "Restart any running Codex, OpenCode, or pi sessions to use the new account.")
	return nil
}

func (r cxRunner) reportCodexGUIRestart(ctx context.Context) error {
	status, err := restartCodexGUI(ctx)
	if err != nil {
		fmt.Fprintf(r.errOut, "Warning: %s\n", err)
		fmt.Fprintln(r.errOut, "Restart Codex.app manually to use the new account in the GUI.")
		return nil
	}
	switch status {
	case "restarted":
		fmt.Fprintln(r.out, "Restarted Codex.app so the GUI uses the new account.")
	case "not-running":
		fmt.Fprintln(r.out, "Codex.app is not running; it will use the new account on next launch.")
	case "unsupported":
		fmt.Fprintln(r.out, "Codex.app restart is only supported on macOS.")
	}
	return nil
}

func (r cxRunner) remove(selector string) error {
	account, ok, err := r.store.RemoveStored(selector)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no account found matching %q", selector)
	}
	fmt.Fprintf(r.out, "Removed account: %s\n", account.Email)
	return nil
}

func (r cxRunner) addAdminKey(ctx context.Context) error {
	reader := bufio.NewReader(r.in)
	label, err := promptLine(r.out, reader, "Label (e.g. work): ")
	if err != nil {
		return err
	}
	key, err := promptLine(r.out, reader, "Admin key (sk-admin-...): ")
	if err != nil {
		return err
	}
	label = strings.TrimSpace(label)
	key = strings.TrimSpace(key)
	if label == "" {
		return fmt.Errorf("label is required")
	}
	if !strings.HasPrefix(key, "sk-admin-") {
		return fmt.Errorf("invalid admin key format, expected sk-admin-...")
	}
	fmt.Fprint(r.out, "Validating with OpenAI... ")
	if err := accounts.ValidateAdminKey(ctx, r.client, key); err != nil {
		fmt.Fprintln(r.out, "failed")
		return err
	}
	fmt.Fprintln(r.out, "ok")
	entry := accounts.AdminKeyEntry{Label: label, Key: key, AddedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := r.store.SaveAdminKey(entry); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Added admin key: %s\n", label)
	return nil
}

func (r cxRunner) listAdminKeys() error {
	all, err := r.store.ListAdminKeys()
	if err != nil {
		return err
	}
	if len(all) == 0 {
		fmt.Fprintln(r.out, "No admin keys. Run 'subrouter add-admin-key' to add one.")
		return nil
	}
	fmt.Fprintln(r.out)
	for _, key := range all {
		fmt.Fprintf(r.out, "  %s  %s  added %s\n", key.Label, maskSecret(key.Key), formatDate(key.AddedAt))
	}
	fmt.Fprintln(r.out)
	return nil
}

func (r cxRunner) removeAdminKey(label string) error {
	removed, err := r.store.RemoveAdminKey(label)
	if err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("no admin key labeled %q", label)
	}
	fmt.Fprintf(r.out, "Removed admin key: %s\n", label)
	return nil
}

func (r cxRunner) attachProject(ctx context.Context, apiKeyLabel, projectID string) error {
	selector := apiKeyLabel
	if !strings.HasPrefix(selector, "apikey:") {
		selector = "apikey:" + selector
	}
	account, ok, err := r.store.FindStored(selector)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no API-key account named %q", apiKeyLabel)
	}
	if !account.IsAPIKey() {
		return fmt.Errorf("%q is not an API-key account", apiKeyLabel)
	}
	admin, ok, err := r.store.PickAdminKeyFor(account)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no admin key configured, run 'subrouter add-admin-key' first")
	}
	projects, err := accounts.ListOpenAIProjects(ctx, r.client, admin.Key)
	if err != nil {
		return err
	}
	chosen := -1
	if projectID != "" {
		if projectID == "none" || projectID == "org" {
			chosen = -1
		} else {
			for i, project := range projects {
				if project.ID == projectID || project.Name == projectID {
					chosen = i
					break
				}
			}
			if chosen == -1 {
				return fmt.Errorf("no project matching %q", projectID)
			}
		}
	} else {
		fmt.Fprintln(r.out)
		for i, project := range projects {
			fmt.Fprintf(r.out, "  %d) %s  %s\n", i+1, project.Name, project.ID)
		}
		fmt.Fprintln(r.out, "  0) <none / org-wide>")
		reader := bufio.NewReader(r.in)
		answer, err := promptLine(r.out, reader, "Pick project (#): ")
		if err != nil {
			return err
		}
		idx, err := strconv.Atoi(strings.TrimSpace(answer))
		if err != nil || idx < 0 || idx > len(projects) {
			return fmt.Errorf("invalid selection")
		}
		chosen = idx - 1
	}
	if chosen < 0 {
		account.ProjectID = ""
		account.ProjectName = ""
	} else {
		account.ProjectID = projects[chosen].ID
		account.ProjectName = projects[chosen].Name
	}
	account.AdminKeyLabel = admin.Label
	if err := r.store.SaveStored(account); err != nil {
		return err
	}
	scope := "<org-wide>"
	if account.ProjectName != "" {
		scope = account.ProjectName
	}
	fmt.Fprintf(r.out, "Updated %s: project=%s via admin=%s\n", account.APIKeyLabel(), scope, admin.Label)
	return nil
}

func (r cxRunner) usage(ctx context.Context, days int) error {
	all, err := r.store.ListStored()
	if err != nil {
		return err
	}
	var apiAccounts []accounts.StoredCodexAccount
	for _, account := range all {
		if account.IsAPIKey() {
			apiAccounts = append(apiAccounts, account)
		}
	}
	if len(apiAccounts) == 0 {
		fmt.Fprintln(r.out, "No API-key accounts. Run 'subrouter add-key' first.")
		return nil
	}
	admins, err := r.store.ListAdminKeys()
	if err != nil {
		return err
	}
	if len(admins) == 0 {
		return fmt.Errorf("no admin keys. Run 'subrouter add-admin-key' first")
	}

	fmt.Fprintf(r.out, "Fetching %d-day usage for %d API-key account(s)...\n\n", days, len(apiAccounts))
	active, _ := r.store.DetectActiveAccount()
	rows := make([]cxUsageRow, len(apiAccounts))
	var wg sync.WaitGroup
	for i, account := range apiAccounts {
		i, account := i, account
		rows[i] = cxUsageRow{email: account.Email, active: account.Email == active, planType: "api key", authMode: accounts.AuthModeAPIKey}
		wg.Add(1)
		go func() {
			defer wg.Done()
			admin, ok, err := r.store.PickAdminKeyFor(account)
			if err != nil || !ok {
				rows[i].err = fmt.Errorf("no admin key linked")
				return
			}
			started := time.Now()
			snapshot, err := accounts.FetchAPIKeyUsageSnapshot(ctx, r.client, admin, account, days)
			if err != nil {
				rows[i].err = err
				fmt.Fprintf(r.out, "  %s via %s: failed (%s)\n", account.APIKeyLabel(), admin.Label, time.Since(started).Round(time.Second))
				return
			}
			if err := r.store.WriteUsageCache(snapshot); err != nil {
				rows[i].err = err
				return
			}
			rows[i].apiKeySpend = &snapshot
			rows[i].score = selectacct.Score{AccountID: account.Email, Headroom: 0.01, ShortHeadroom: 0.01}
			fmt.Fprintf(r.out, "  %s via %s: ok (%s)\n", account.APIKeyLabel(), admin.Label, time.Since(started).Round(time.Second))
		}()
	}
	wg.Wait()
	rankUsageRows(rows)
	displayUsageRows(r.out, rows, false)
	return nil
}

func accountFromStored(account accounts.StoredCodexAccount) accounts.Account {
	out := accounts.Account{
		ID:       account.Email,
		Provider: accounts.ProviderCodex,
		Label:    account.Email,
		Email:    account.Email,
		Source:   account.Email,
	}
	if account.IsAPIKey() {
		out.AuthMode = accounts.AuthModeAPIKey
		out.Token = account.Auth.OpenAIAPIKey
		return out
	}
	out.AuthMode = accounts.AuthModeOAuth
	if account.Auth.Tokens != nil {
		out.Token = account.Auth.Tokens.AccessToken
		out.AccountID = account.Auth.Tokens.AccountID
	}
	return out
}

func scoreFromWindows(accountID string, windows []accounts.UsageWindow) selectacct.Score {
	limitWindows := make([]selectacct.LimitWindow, 0, len(windows))
	for _, window := range windows {
		limitWindows = append(limitWindows, selectacct.LimitWindow{
			Name:               window.Name,
			UsedPercent:        window.UsedPercent,
			LimitWindowSeconds: window.LimitWindowSeconds,
			ResetAfterSeconds:  window.ResetAfterSeconds,
		})
	}
	return selectacct.ScoreFromLimitWindows(accountID, 0, limitWindows)
}

func cookedFromWindows(windows []accounts.UsageWindow) (bool, string) {
	for _, window := range windows {
		if !isLongQuotaWindow(window) || clampUsagePercent(window.UsedPercent) < 100 {
			continue
		}
		if window.ResetAfterSeconds > 0 {
			return true, fmt.Sprintf("%s fully consumed, resets in %s", windowLabel(window), formatDuration(window.ResetAfterSeconds))
		}
		return true, fmt.Sprintf("%s fully consumed", windowLabel(window))
	}
	return false, ""
}

func tempCookedFromWindows(windows []accounts.UsageWindow) (bool, string) {
	if longQuotaSaturated(windows) {
		return false, ""
	}
	for _, window := range windows {
		if !isShortQuotaWindow(window) || clampUsagePercent(window.UsedPercent) < 100 {
			continue
		}
		if window.ResetAfterSeconds > 0 {
			return true, fmt.Sprintf("%s fully consumed, resets in %s", windowLabel(window), formatDuration(window.ResetAfterSeconds))
		}
		return true, fmt.Sprintf("%s fully consumed", windowLabel(window))
	}
	return false, ""
}

func longQuotaSaturated(windows []accounts.UsageWindow) bool {
	cooked, _ := cookedFromWindows(windows)
	return cooked
}

func isShortQuotaWindow(window accounts.UsageWindow) bool {
	if window.LimitWindowSeconds > 0 {
		return window.LimitWindowSeconds <= int64((6*time.Hour)/time.Second)
	}
	name := strings.ToLower(window.Name)
	return strings.Contains(name, "5h") || strings.Contains(name, "primary")
}

func isLongQuotaWindow(window accounts.UsageWindow) bool {
	if window.LimitWindowSeconds > 0 {
		return window.LimitWindowSeconds >= int64((6*24*time.Hour)/time.Second)
	}
	name := strings.ToLower(window.Name)
	return strings.Contains(name, "7d") || strings.Contains(name, "weekly")
}

func clampUsagePercent(value float64) float64 {
	switch {
	case value < 0:
		return 0
	case value > 100:
		return 100
	default:
		return value
	}
}

func allOAuthRowsCooked(rows []cxUsageRow) bool {
	seenOAuth := false
	for _, row := range rows {
		if row.err != nil || row.authMode == accounts.AuthModeAPIKey {
			return false
		}
		if row.authMode != accounts.AuthModeOAuth {
			continue
		}
		seenOAuth = true
		if !row.cooked {
			return false
		}
	}
	return seenOAuth
}

func allOAuthRowsBlockedForNewSession(rows []cxUsageRow) bool {
	seenOAuth := false
	for _, row := range rows {
		if row.err != nil || row.authMode == accounts.AuthModeAPIKey {
			return false
		}
		if row.authMode != accounts.AuthModeOAuth {
			continue
		}
		seenOAuth = true
		if !row.cooked && !row.tempCooked && !exhaustedForNewSession(row.score) {
			return false
		}
	}
	return seenOAuth
}

func hasSwitchableUsageRows(rows []cxUsageRow) bool {
	for _, row := range rows {
		if row.err != nil {
			continue
		}
		if row.authMode == accounts.AuthModeAPIKey {
			return true
		}
		if row.authMode == accounts.AuthModeOAuth && !row.cooked && !row.tempCooked {
			return true
		}
	}
	return false
}

func ensureUsageRowSwitchable(row cxUsageRow) error {
	if row.cooked {
		return fmt.Errorf("cannot switch to %s: account is cooked (%s)", row.email, row.cookedReason)
	}
	if row.tempCooked {
		return fmt.Errorf("cannot switch to %s: account is temporarily cooked (%s)", row.email, row.tempCookedReason)
	}
	return nil
}

func (r cxRunner) ensureSwitchableForFreshUsage(ctx context.Context, account accounts.StoredCodexAccount) error {
	if account.IsAPIKey() {
		return nil
	}
	if r.client == nil {
		return nil
	}
	details, err := accounts.FetchCodexUsageDetails(ctx, r.client, accountFromStored(account))
	if err != nil {
		return nil
	}
	cooked, reason := cookedFromWindows(details.Windows)
	if cooked {
		return fmt.Errorf("cannot switch to %s: account is cooked (%s)", account.Email, reason)
	}
	tempCooked, reason := tempCookedFromWindows(details.Windows)
	if tempCooked {
		return fmt.Errorf("cannot switch to %s: account is temporarily cooked (%s)", account.Email, reason)
	}
	return nil
}

func rankUsageRows(rows []cxUsageRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if (a.err != nil) != (b.err != nil) {
			return a.err == nil
		}
		at, bt := usageRowTier(a), usageRowTier(b)
		if at != bt {
			return at < bt
		}
		au, bu := usableForNewSession(a.score), usableForNewSession(b.score)
		if au != bu {
			return au
		}
		if au && bu && a.score.ExpiryPressure != b.score.ExpiryPressure {
			return a.score.ExpiryPressure > b.score.ExpiryPressure
		}
		if a.score.Headroom != b.score.Headroom {
			return a.score.Headroom > b.score.Headroom
		}
		if a.score.ShortResetAfterSeconds != b.score.ShortResetAfterSeconds {
			if a.score.ShortResetAfterSeconds == 0 {
				return false
			}
			if b.score.ShortResetAfterSeconds == 0 {
				return true
			}
			return a.score.ShortResetAfterSeconds < b.score.ShortResetAfterSeconds
		}
		return rows[i].email < rows[j].email
	})
	recommended := false
	for i := range rows {
		rows[i].gtoRecommended = false
		if !recommended && recommendedForNewSession(rows[i]) {
			rows[i].gtoRecommended = true
			recommended = true
		}
		rows[i].gtoReason = gtoReason(rows[i])
	}
}

func usageRowTier(row cxUsageRow) int {
	if row.authMode == accounts.AuthModeOAuth && usableForNewSession(row.score) {
		return 0
	}
	if row.authMode == accounts.AuthModeAPIKey {
		return 1
	}
	if row.authMode == accounts.AuthModeOAuth && !row.tempCooked && !row.cooked {
		return 2
	}
	if row.authMode == accounts.AuthModeOAuth && row.tempCooked {
		return 3
	}
	if row.authMode == accounts.AuthModeOAuth && row.cooked {
		return 4
	}
	return 5
}

func recommendedForNewSession(row cxUsageRow) bool {
	if row.err != nil || row.cooked || row.tempCooked {
		return false
	}
	if row.authMode == accounts.AuthModeAPIKey {
		return true
	}
	return row.authMode == accounts.AuthModeOAuth && usableForNewSession(row.score)
}

func usableForNewSession(score selectacct.Score) bool {
	return score.Headroom >= selectacct.MinNewSessionHeadroom && score.ShortHeadroom >= selectacct.MinNewSessionHeadroom
}

func exhaustedForNewSession(score selectacct.Score) bool {
	return score.Headroom <= 0 || score.ShortHeadroom <= 0
}

func gtoReason(row cxUsageRow) string {
	if row.err != nil {
		return "usage unavailable"
	}
	if row.cooked {
		return "cooked, cannot switch"
	}
	if row.tempCooked {
		return "temporarily cooked, cannot start new session"
	}
	if row.authMode == accounts.AuthModeAPIKey {
		return "API key fallback"
	}
	left := fmt.Sprintf("%d%% bottleneck left", int(row.score.Headroom*100+0.5))
	if !usableForNewSession(row.score) {
		return fmt.Sprintf("%s, protected below %d%%", left, int(selectacct.MinNewSessionHeadroom*100))
	}
	if row.score.ShortResetAfterSeconds > 0 {
		return fmt.Sprintf("%s, 5h resets in %s", left, formatDuration(row.score.ShortResetAfterSeconds))
	}
	return left
}

func displayUsageRows(out io.Writer, rows []cxUsageRow, numbered bool) {
	if len(rows) == 0 {
		fmt.Fprintln(out, "No accounts configured. Run 'subrouter add' to add one.")
		return
	}
	colored := colorEnabled(out)
	width := 0
	for _, row := range rows {
		if row.gtoReason != "" {
			width = max(width, len("pick"))
		}
		if row.cookedReason != "" {
			width = max(width, len("cooked"))
		}
		if row.tempCookedReason != "" {
			width = max(width, len("temporarily cooked"))
		}
		for _, window := range row.windows {
			width = max(width, len(windowLabel(window)))
		}
		if row.credits != nil {
			width = max(width, len("Credits"))
		}
		if row.apiKeySpend != nil {
			width = max(width, len("top model"))
		}
	}
	fmt.Fprintln(out)
	for i, row := range rows {
		prefix := ""
		if numbered {
			prefix = fmt.Sprintf("%d) ", i+1)
		}
		active := ""
		if row.active {
			active = " " + style(colored, ansiCyan, "(active)")
		}
		recommended := ""
		if row.gtoRecommended {
			recommended = " " + style(colored, ansiGreen, "[recommended]")
		}
		cooked := ""
		if row.cooked {
			cooked = " " + style(colored, ansiRed, "[cooked]")
		} else if row.tempCooked {
			cooked = " " + style(colored, ansiYellow, "[temporarily cooked]")
		}
		plan := ""
		if row.planType != "" {
			plan = " " + style(colored, ansiDim, "["+row.planType+"]")
		}
		fmt.Fprintf(out, "%s%s%s%s%s%s\n", style(colored, ansiDim, prefix), style(colored, ansiBold+ansiWhite, displayAccountName(row.email)), plan, active, recommended, cooked)
		if row.err != nil {
			fmt.Fprintf(out, "  %s\n\n", style(colored, ansiRed, "Error: "+row.err.Error()))
			continue
		}
		if row.gtoReason != "" {
			fmt.Fprintf(out, "  %s: %s\n", style(colored, ansiDim, pad("pick", width)), row.gtoReason)
		}
		if row.cookedReason != "" {
			fmt.Fprintf(out, "  %s: %s\n", style(colored, ansiDim, pad("cooked", width)), row.cookedReason)
		}
		if row.tempCookedReason != "" {
			fmt.Fprintf(out, "  %s: %s\n", style(colored, ansiDim, pad("temporarily cooked", width)), row.tempCookedReason)
		}
		for _, window := range row.windows {
			left := formatPercentLeft(window.UsedPercent)
			fmt.Fprintf(out, "  %s: %s %s", style(colored, ansiDim, pad(windowLabel(window), width)), renderBar(window.UsedPercent, colored), style(colored, usageColor(window.UsedPercent), left+" left"))
			if window.ResetAfterSeconds > 0 {
				fmt.Fprintf(out, " %s", style(colored, ansiDim, "resets in "+formatDuration(window.ResetAfterSeconds)))
			}
			fmt.Fprintln(out)
		}
		if row.credits != nil {
			if row.credits.Unlimited {
				fmt.Fprintf(out, "  %s: %s\n", style(colored, ansiDim, pad("Credits", width)), style(colored, ansiGreen, "Unlimited"))
			} else if row.credits.Balance != "" {
				fmt.Fprintf(out, "  %s: $%s\n", style(colored, ansiDim, pad("Credits", width)), row.credits.Balance)
			}
		}
		if row.apiKeySpend != nil {
			displayAPIKeySpend(out, row.apiKeySpend, width, colored)
		} else if row.apiKeyHint != "" {
			fmt.Fprintf(out, "  %s\n", style(colored, ansiDim, row.apiKeyHint))
		}
		fmt.Fprintln(out)
	}
}

func displayAPIKeySpend(out io.Writer, snapshot *accounts.APIKeyUsageSnapshot, width int, colored bool) {
	scope := "org-wide"
	if snapshot.ProjectID != "" {
		scope = "proj " + firstNonEmpty(snapshot.ProjectName, snapshot.ProjectID)
	}
	fmt.Fprintf(out, "  %s: %s %s\n", style(colored, ansiDim, pad("scope", width)), scope, style(colored, ansiDim, "via "+snapshot.AdminKeyLabel+", "+formatAge(snapshot.FetchedAt)))
	today := fmtUSD(snapshot.TodayUSD)
	if snapshot.TodayCostEstimated {
		today = "~" + today + " est"
	}
	fmt.Fprintf(out, "  %s: %s %s\n", style(colored, ansiDim, pad("today", width)), today, style(colored, ansiDim, "("+fmtTokens(snapshot.TodayTokens)+" tok)"))
	fmt.Fprintf(out, "  %s: %s %s\n", style(colored, ansiDim, pad("7d", width)), fmtUSD(snapshot.WeekUSD), style(colored, ansiDim, "("+fmtTokens(snapshot.WeekTokens)+" tok)"))
	fmt.Fprintf(out, "  %s: %s %s\n", style(colored, ansiDim, pad("30d", width)), fmtUSD(snapshot.MonthUSD), style(colored, ansiDim, "("+fmtTokens(snapshot.MonthTokens)+" tok)"))
	if snapshot.TopModel != nil && snapshot.TopModel.Tokens > 0 {
		fmt.Fprintf(out, "  %s: %s %s\n", style(colored, ansiDim, pad("top model", width)), snapshot.TopModel.Model, style(colored, ansiDim, "("+fmtTokens(snapshot.TopModel.Tokens)+" tok 30d)"))
	}
}

func parseCXSwitchArgs(args []string, defaults cxSwitchOptions) (string, cxSwitchOptions, error) {
	opts := defaults
	selector := ""
	for _, arg := range args {
		switch arg {
		case "--restart-codex-gui", "--restart-gui":
			opts.restartCodexGUI = true
		case "--no-restart-codex-gui", "--no-restart-gui":
			opts.restartCodexGUI = false
		default:
			if strings.HasPrefix(arg, "-") {
				return "", opts, fmt.Errorf("unknown switch option: %s", arg)
			}
			if selector != "" {
				return "", opts, fmt.Errorf("unexpected extra account selector: %s", arg)
			}
			selector = arg
		}
	}
	return selector, opts, nil
}

func promptLine(out io.Writer, reader *bufio.Reader, prompt string) (string, error) {
	fmt.Fprint(out, prompt)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func restartCodexGUI(ctx context.Context) (string, error) {
	if runtime.GOOS != "darwin" {
		return "unsupported", nil
	}
	pids, err := commandOutput(ctx, "pgrep", "-x", "Codex")
	if err != nil {
		if exitError(err) == 1 {
			return "not-running", nil
		}
		return "", fmt.Errorf("pgrep Codex failed: %w", err)
	}
	if strings.TrimSpace(pids) == "" {
		return "not-running", nil
	}
	if _, err := commandOutput(ctx, "pkill", "-x", "Codex"); err != nil && exitError(err) != 1 {
		return "", fmt.Errorf("pkill Codex failed: %w", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, err := commandOutput(ctx, "pgrep", "-x", "Codex")
		if exitError(err) == 1 || strings.TrimSpace(out) == "" {
			break
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
	if _, err := commandOutput(ctx, "open", "-b", "com.openai.codex"); err == nil {
		return "restarted", nil
	}
	if _, err := commandOutput(ctx, "open", "/Applications/Codex.app"); err == nil {
		return "restarted", nil
	}
	return "", fmt.Errorf("failed to reopen Codex.app")
}

func commandOutput(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	body, err := cmd.CombinedOutput()
	return string(body), err
}

func exitError(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func windowLabel(window accounts.UsageWindow) string {
	name := window.Name
	switch name {
	case "primary":
		if window.LimitWindowSeconds >= 3600 {
			return fmt.Sprintf("%dh limit", int((window.LimitWindowSeconds+1800)/3600))
		}
		return fmt.Sprintf("%dm limit", int((window.LimitWindowSeconds+30)/60))
	case "secondary":
		if window.LimitWindowSeconds >= 86400 {
			return fmt.Sprintf("%dd limit", int((window.LimitWindowSeconds+43200)/86400))
		}
		return "Weekly limit"
	}
	if strings.HasSuffix(name, "/primary") {
		return strings.TrimSuffix(name, "/primary")
	}
	if strings.HasSuffix(name, "/secondary") {
		return strings.TrimSuffix(name, "/secondary") + " (weekly)"
	}
	return name
}

func colorEnabled(out io.Writer) bool {
	if os.Getenv("CX_NO_COLOR") != "" {
		return false
	}
	if force := os.Getenv("FORCE_COLOR"); force != "" && force != "0" {
		return true
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	file, ok := out.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func style(enabled bool, code, value string) string {
	if !enabled || value == "" {
		return value
	}
	return code + value + ansiReset
}

func usageColor(usedPercent float64) string {
	switch {
	case usedPercent >= 90:
		return ansiRed
	case usedPercent >= 70:
		return ansiYellow
	default:
		return ansiGreen
	}
}

func barBGColor(usedPercent float64) string {
	switch {
	case usedPercent >= 90:
		return ansiBGRed
	case usedPercent >= 70:
		return ansiBGYellow
	default:
		return ansiBGGreen
	}
}

func renderBar(usedPercent float64, colored bool) string {
	width := 22
	filled := int(usedPercent/100*float64(width) + 0.5)
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	if colored {
		return barBGColor(usedPercent) + strings.Repeat(" ", filled) + ansiBGGray + strings.Repeat(" ", width-filled) + ansiReset
	}
	return "[" + strings.Repeat("=", filled) + strings.Repeat(" ", width-filled) + "]"
}

func formatPercentLeft(used float64) string {
	left := 100 - used
	if left < 0 {
		left = 0
	}
	return fmt.Sprintf("%.1f%%", left)
}

func formatDuration(seconds int64) string {
	if seconds <= 0 {
		return "now"
	}
	days := seconds / 86400
	hours := (seconds % 86400) / 3600
	mins := (seconds % 3600) / 60
	var parts []string
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if mins > 0 && days == 0 {
		parts = append(parts, fmt.Sprintf("%dm", mins))
	}
	if len(parts) == 0 {
		return "<1m"
	}
	return strings.Join(parts, " ")
}

func displayAccountName(email string) string {
	if strings.HasPrefix(email, "apikey:") {
		return strings.TrimPrefix(email, "apikey:") + " (api key)"
	}
	return email
}

func formatDate(value string) string {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return value
	}
	return t.Format("2006-01-02")
}

func formatAge(value string) string {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return value
	}
	age := time.Since(t)
	switch {
	case age < time.Minute:
		return fmt.Sprintf("%ds ago", int(age.Seconds()))
	case age < time.Hour:
		return fmt.Sprintf("%dm ago", int(age.Minutes()))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(age.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(age.Hours()/24))
	}
}

func fmtUSD(n float64) string {
	switch {
	case n == 0:
		return "$0"
	case n < 0.01:
		return fmt.Sprintf("$%.4f", n)
	case n < 1:
		return fmt.Sprintf("$%.3f", n)
	default:
		return fmt.Sprintf("$%.2f", n)
	}
}

func fmtTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	default:
		return strconv.FormatInt(n, 10)
	}
}

func maskSecret(value string) string {
	if len(value) <= 18 {
		return value
	}
	return value[:12] + "***" + value[len(value)-6:]
}

func pad(value string, width int) string {
	if width <= len(value) {
		return value
	}
	return value + strings.Repeat(" ", width-len(value))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func indexOf(values []string, needle string) int {
	for i, value := range values {
		if value == needle {
			return i
		}
	}
	return -1
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
