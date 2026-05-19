package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func (r cxRunner) serverCommand() string {
	if r.program == "" || r.program == "cx" {
		return "cx server"
	}
	return r.program + " server"
}

func cxServerHelp(command string) string {
	return fmt.Sprintf(`%[1]s - Manage Subrouter servers

Usage:
  %[1]s list
  %[1]s add <name> --url <url> [--default] [--admin-token <token>] [--gcp-instance <name> --gcp-zone <zone> --gcp-project <project>]
  %[1]s use <name>
  %[1]s current
  %[1]s clear-default
  %[1]s rename <old> <new>
  %[1]s remove <name>
  %[1]s status <name>
  %[1]s install <name> [--version latest]
  %[1]s login <name> [--device-auth]
  %[1]s sync <name> [--device-auth] [--all] [--email <email>] [--dry-run] [--yes]

`, command)
}

const publicInstallScriptURL = "https://github.com/manaflow-ai/subrouter/releases/latest/download/install.sh"

type cxServerStore struct {
	Path string
}

type cxServerConfig struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	GCPProject  string `json:"gcpProject,omitempty"`
	GCPZone     string `json:"gcpZone,omitempty"`
	GCPInstance string `json:"gcpInstance,omitempty"`
	AdminToken  string `json:"adminToken,omitempty"`
}

type cxServerFile struct {
	Servers []cxServerConfig `json:"servers"`
	Default string           `json:"default,omitempty"`
}

func (r cxRunner) server(ctx context.Context, args []string) error {
	store := defaultCXServerStore(r.store)
	if len(args) == 0 {
		fmt.Fprint(r.out, cxServerHelp(r.serverCommand()))
		return nil
	}
	command := r.serverCommand()
	switch args[0] {
	case "list", "ls":
		return r.serverList(store)
	case "add":
		return r.serverAdd(store, args[1:])
	case "use":
		if len(args) != 2 {
			return fmt.Errorf("usage: %s use <name>", command)
		}
		return r.serverUse(store, args[1])
	case "current", "default":
		return r.serverCurrent(store)
	case "clear-default", "unset":
		return r.serverClearDefault(store)
	case "rename", "mv":
		if len(args) != 3 {
			return fmt.Errorf("usage: %s rename <old> <new>", command)
		}
		return r.serverRename(store, args[1], args[2])
	case "remove", "rm":
		if len(args) != 2 {
			return fmt.Errorf("usage: %s remove <name>", command)
		}
		return r.serverRemove(store, args[1])
	case "status":
		if len(args) != 2 {
			return fmt.Errorf("usage: %s status <name>", command)
		}
		return r.serverStatus(ctx, store, args[1])
	case "install":
		return r.serverInstall(ctx, store, args[1:])
	case "login", "add-account", "add-auth":
		return r.serverLogin(ctx, store, args[1:])
	case "sync", "reconcile":
		return r.serverSync(ctx, store, args[1:])
	case "diff":
		if len(args) < 2 {
			return fmt.Errorf("usage: %s diff <name>", command)
		}
		syncArgs := append([]string{args[1], "--dry-run"}, args[2:]...)
		return r.serverSync(ctx, store, syncArgs)
	case "help", "-h", "--help":
		fmt.Fprint(r.out, cxServerHelp(command))
		return nil
	default:
		return fmt.Errorf("unknown server command %q\n%s", args[0], cxServerHelp(command))
	}
}

func defaultCXServerStore(store accounts.CodexStore) cxServerStore {
	return cxServerStore{Path: filepath.Join(store.StoreDir(), "servers.json")}
}

func (s cxServerStore) load() (cxServerFile, error) {
	body, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return cxServerFile{}, nil
	}
	if err != nil {
		return cxServerFile{}, err
	}
	var file cxServerFile
	if err := json.Unmarshal(body, &file); err != nil {
		return cxServerFile{}, err
	}
	sort.Slice(file.Servers, func(i, j int) bool {
		return file.Servers[i].Name < file.Servers[j].Name
	})
	return file, nil
}

func (s cxServerStore) save(file cxServerFile) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	sort.Slice(file.Servers, func(i, j int) bool {
		return file.Servers[i].Name < file.Servers[j].Name
	})
	body, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path)
}

func (s cxServerStore) find(name string) (cxServerConfig, bool, error) {
	file, err := s.load()
	if err != nil {
		return cxServerConfig{}, false, err
	}
	for _, server := range file.Servers {
		if server.Name == name {
			return server, true, nil
		}
	}
	return cxServerConfig{}, false, nil
}

func (f cxServerFile) find(name string) (cxServerConfig, bool) {
	for _, server := range f.Servers {
		if server.Name == name {
			return server, true
		}
	}
	return cxServerConfig{}, false
}

func (r cxRunner) serverList(store cxServerStore) error {
	file, err := store.load()
	if err != nil {
		return err
	}
	if len(file.Servers) == 0 {
		fmt.Fprintf(r.out, "No servers configured. Run: %s add team --url <url>\n", r.serverCommand())
		return nil
	}
	for _, server := range file.Servers {
		suffix := ""
		if server.Name == file.Default {
			suffix = "\t(default)"
		}
		fmt.Fprintf(r.out, "%s\t%s\t%s\t%s%s\n", server.Name, server.URL, server.GCPInstance, server.GCPZone, suffix)
	}
	return nil
}

func (r cxRunner) serverAdd(store cxServerStore, args []string) error {
	command := r.serverCommand()
	if len(args) == 0 {
		return fmt.Errorf("usage: %s add <name> --url <url> [--default] [--admin-token <token>] [--gcp-instance <name> --gcp-zone <zone> --gcp-project <project>]", command)
	}
	name := args[0]
	flags := flag.NewFlagSet(command+" add", flag.ContinueOnError)
	flags.SetOutput(r.errOut)
	serverURL := flags.String("url", "", "subrouter base URL, such as http://100.64.0.1:31415")
	gcpProject := flags.String("gcp-project", "", "GCP project; defaults to current gcloud project")
	gcpZone := flags.String("gcp-zone", "", "GCP zone")
	gcpInstance := flags.String("gcp-instance", "", "GCP instance name")
	adminToken := flags.String("admin-token", "", "admin token for non-loopback _subrouter endpoints")
	makeDefault := flags.Bool("default", false, "make this the default server for sr codex")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	adminTokenSet := false
	flags.Visit(func(flag *flag.Flag) {
		if flag.Name == "admin-token" {
			adminTokenSet = true
		}
	})
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("server name is required")
	}
	if _, err := url.ParseRequestURI(*serverURL); err != nil || *serverURL == "" {
		return fmt.Errorf("--url must be a valid URL")
	}
	if (*gcpInstance == "") != (*gcpZone == "") {
		return fmt.Errorf("--gcp-instance and --gcp-zone must be set together")
	}
	file, err := store.load()
	if err != nil {
		return err
	}
	next := cxServerConfig{
		Name:        name,
		URL:         strings.TrimRight(*serverURL, "/"),
		GCPProject:  *gcpProject,
		GCPZone:     *gcpZone,
		GCPInstance: *gcpInstance,
		AdminToken:  *adminToken,
	}
	replaced := false
	for i := range file.Servers {
		if file.Servers[i].Name == name {
			if !adminTokenSet {
				next.AdminToken = file.Servers[i].AdminToken
			}
			file.Servers[i] = next
			replaced = true
			break
		}
	}
	if !replaced {
		file.Servers = append(file.Servers, next)
	}
	if *makeDefault {
		file.Default = name
	}
	if err := store.save(file); err != nil {
		return err
	}
	if replaced {
		fmt.Fprintf(r.out, "Updated server: %s\n", name)
	} else {
		fmt.Fprintf(r.out, "Added server: %s\n", name)
	}
	if *makeDefault {
		fmt.Fprintf(r.out, "Default Codex server: %s\n", name)
	}
	return nil
}

func (r cxRunner) serverUse(store cxServerStore, name string) error {
	file, err := store.load()
	if err != nil {
		return err
	}
	if _, ok := file.find(name); !ok {
		return fmt.Errorf("server %q not found", name)
	}
	file.Default = name
	if err := store.save(file); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Default Codex server: %s\n", name)
	return nil
}

func (r cxRunner) serverCurrent(store cxServerStore) error {
	file, err := store.load()
	if err != nil {
		return err
	}
	if strings.TrimSpace(file.Default) == "" {
		fmt.Fprintln(r.out, "Default Codex server: local")
		return nil
	}
	server, ok := file.find(file.Default)
	if !ok {
		return fmt.Errorf("default server %q not found", file.Default)
	}
	fmt.Fprintf(r.out, "Default Codex server: %s\t%s\n", server.Name, server.URL)
	return nil
}

func (r cxRunner) serverClearDefault(store cxServerStore) error {
	file, err := store.load()
	if err != nil {
		return err
	}
	file.Default = ""
	if err := store.save(file); err != nil {
		return err
	}
	fmt.Fprintln(r.out, "Default Codex server: local")
	return nil
}

func (r cxRunner) serverRename(store cxServerStore, oldName, newName string) error {
	oldName = strings.TrimSpace(oldName)
	newName = strings.TrimSpace(newName)
	if oldName == "" || newName == "" {
		return fmt.Errorf("server names are required")
	}
	if oldName == newName {
		fmt.Fprintf(r.out, "Server name unchanged: %s\n", oldName)
		return nil
	}
	file, err := store.load()
	if err != nil {
		return err
	}
	if _, ok := file.find(newName); ok {
		return fmt.Errorf("server %q already exists", newName)
	}
	renamed := false
	for i := range file.Servers {
		if file.Servers[i].Name == oldName {
			file.Servers[i].Name = newName
			renamed = true
			break
		}
	}
	if !renamed {
		return fmt.Errorf("server %q not found", oldName)
	}
	if file.Default == oldName {
		file.Default = newName
	}
	if err := store.save(file); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Renamed server: %s -> %s\n", oldName, newName)
	return nil
}

func (r cxRunner) serverRemove(store cxServerStore, name string) error {
	file, err := store.load()
	if err != nil {
		return err
	}
	out := file.Servers[:0]
	removed := false
	for _, server := range file.Servers {
		if server.Name == name {
			removed = true
			continue
		}
		out = append(out, server)
	}
	if !removed {
		return fmt.Errorf("server %q not found", name)
	}
	file.Servers = out
	if file.Default == name {
		file.Default = ""
	}
	if err := store.save(file); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Removed server: %s\n", name)
	return nil
}

func (r cxRunner) serverStatus(ctx context.Context, store cxServerStore, name string) error {
	server, ok, err := store.find(name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("server %q not found", name)
	}
	res, err := r.fetchServerAccountsResponse(ctx, server)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("server status failed: %s", res.Status)
	}
	_, err = io.Copy(r.out, res.Body)
	if err == nil {
		fmt.Fprintln(r.out)
	}
	return err
}

type remoteServerAccount struct {
	ID       string            `json:"id"`
	Provider accounts.Provider `json:"provider"`
	AuthMode accounts.AuthMode `json:"auth_mode"`
	Email    string            `json:"email,omitempty"`
	Source   string            `json:"source"`
}

type remoteServerAccountStatus struct {
	ID          string            `json:"id"`
	Provider    accounts.Provider `json:"provider"`
	AuthMode    accounts.AuthMode `json:"auth_mode"`
	Email       string            `json:"email,omitempty"`
	Source      string            `json:"source"`
	AuthChecked bool              `json:"auth_checked"`
	AuthValid   bool              `json:"auth_valid"`
	Refreshed   bool              `json:"refreshed,omitempty"`
	Error       string            `json:"error,omitempty"`
}

func (r cxRunner) fetchServerAccountsResponse(ctx context.Context, server cxServerConfig) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/_subrouter/accounts", nil)
	if err != nil {
		return nil, err
	}
	addServerAdminAuth(req, server)
	client := r.client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return client.Do(req)
}

func (r cxRunner) fetchServerAccounts(ctx context.Context, server cxServerConfig) ([]remoteServerAccount, error) {
	res, err := r.fetchServerAccountsResponse(ctx, server)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("server accounts failed: %s", res.Status)
	}
	var out []remoteServerAccount
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r cxRunner) fetchServerAccountStatuses(ctx context.Context, server cxServerConfig, forceRefresh bool) ([]remoteServerAccountStatus, bool, error) {
	statusURL := server.URL + "/_subrouter/account-status"
	method := http.MethodGet
	if forceRefresh {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, statusURL, nil)
	if err != nil {
		return nil, false, err
	}
	addServerAdminAuth(req, server)
	client := r.client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer res.Body.Close()
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		var out []remoteServerAccountStatus
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			return nil, true, err
		}
		return out, true, nil
	}
	_, _ = io.Copy(io.Discard, res.Body)
	accounts, err := r.fetchServerAccounts(ctx, server)
	if err != nil {
		return nil, false, err
	}
	out := make([]remoteServerAccountStatus, 0, len(accounts))
	for _, account := range accounts {
		out = append(out, remoteServerAccountStatus{
			ID:       account.ID,
			Provider: account.Provider,
			AuthMode: account.AuthMode,
			Email:    account.Email,
			Source:   account.Source,
		})
	}
	return out, false, nil
}

func addServerAdminAuth(req *http.Request, server cxServerConfig) {
	if strings.TrimSpace(server.AdminToken) == "" {
		return
	}
	req.Header.Set("Authorization", "Bearer "+server.AdminToken)
}

func (r cxRunner) serverInstall(ctx context.Context, store cxServerStore, args []string) error {
	command := r.serverCommand()
	if len(args) == 0 {
		return fmt.Errorf("usage: %s install <name> [--version latest]", command)
	}
	name := args[0]
	flags := flag.NewFlagSet(command+" install", flag.ContinueOnError)
	flags.SetOutput(r.errOut)
	version := flags.String("version", "latest", "Subrouter release version to install")
	addr := flags.String("addr", "0.0.0.0:31415", "server listen address")
	cxSwitchInterval := flags.String("cx-switch-interval", "10m", "cx auto-switch interval; 0 disables")
	extraArgs := flags.String("extra-args", "", "extra arguments appended to subrouter serve")
	tailscaleHostname := flags.String("tailscale-hostname", "", "hostname for tailscale up when TAILSCALE_AUTH_KEY is set")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	server, ok, err := store.find(name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("server %q not found", name)
	}
	if server.GCPInstance == "" || server.GCPZone == "" {
		return fmt.Errorf("server %s has no GCP target", server.Name)
	}
	hostname := *tailscaleHostname
	if hostname == "" {
		hostname = server.GCPInstance
	}
	tailscaleAuthKey := strings.TrimSpace(os.Getenv("TAILSCALE_AUTH_KEY"))
	remoteCommand := strings.Join([]string{
		"set -eu",
		"tailscale_auth_key=''",
		"read -r tailscale_auth_key || true",
		"curl -fsSL " + shellQuote(publicInstallScriptURL) + " | sudo env SUBROUTER_VERSION=" + shellQuote(*version) + " sh",
		"sudo /usr/local/bin/sr install-systemd --addr " + shellQuote(*addr) + " --cx-switch-interval " + shellQuote(*cxSwitchInterval) + " --extra-args " + shellQuote(*extraArgs),
		"if [ -n \"$tailscale_auth_key\" ]; then sudo tailscale up --auth-key \"$tailscale_auth_key\" --hostname " + shellQuote(hostname) + " --ssh --accept-routes=false --accept-dns=false; fi",
		"i=0; until curl -fsS http://127.0.0.1:31415/_subrouter/health >/dev/null 2>&1; do i=$((i+1)); if [ \"$i\" -ge 30 ]; then exit 1; fi; sleep 1; done",
		"/usr/local/bin/sr --help >/dev/null",
	}, "\n")
	sshArgs := []string{"compute", "ssh", server.GCPInstance, "--zone", server.GCPZone, "--command", remoteCommand}
	if server.GCPProject != "" {
		sshArgs = append(sshArgs, "--project", server.GCPProject)
	}
	stdin := strings.NewReader(tailscaleAuthKey + "\n")
	if err := r.commandRunner().Run(ctx, "gcloud", sshArgs, stdin, r.out, r.errOut); err != nil {
		return fmt.Errorf("install server: %w", err)
	}
	fmt.Fprintf(r.out, "Installed Subrouter server: %s\n", server.Name)
	if tailscaleAuthKey != "" {
		fmt.Fprintf(r.out, "Joined Tailscale as: %s\n", hostname)
	}
	return nil
}

func (r cxRunner) serverLogin(ctx context.Context, store cxServerStore, args []string) error {
	command := r.serverCommand()
	if len(args) == 0 {
		return fmt.Errorf("usage: %s login <name> [--device-auth]", command)
	}
	name := args[0]
	flags := flag.NewFlagSet(command+" login", flag.ContinueOnError)
	flags.SetOutput(r.errOut)
	deviceAuth := flags.Bool("device-auth", false, "use codex login --device-auth")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	server, ok, err := store.find(name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("server %q not found", name)
	}
	return r.serverLoginOne(ctx, server, *deviceAuth, "")
}

func (r cxRunner) serverSync(ctx context.Context, store cxServerStore, args []string) error {
	command := r.serverCommand()
	if len(args) == 0 {
		return fmt.Errorf("usage: %s sync <name> [--device-auth] [--all] [--email <email>] [--dry-run] [--yes]", command)
	}
	name := args[0]
	var emails repeatedStringFlag
	flags := flag.NewFlagSet(command+" sync", flag.ContinueOnError)
	flags.SetOutput(r.errOut)
	deviceAuth := flags.Bool("device-auth", false, "use codex login --device-auth")
	all := flags.Bool("all", false, "reauth every local OAuth account, including accounts already present on the server")
	dryRun := flags.Bool("dry-run", false, "show local/server account diff without starting logins")
	yes := flags.Bool("yes", false, "reauth without confirmation")
	flags.Var(&emails, "email", "local OAuth email to reauth on the server; can be repeated")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	server, ok, err := store.find(name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("server %q not found", name)
	}
	localStored, err := r.store.ListStored()
	if err != nil {
		return err
	}
	remoteAccounts, statusAvailable, err := r.fetchServerAccountStatuses(ctx, server, true)
	if err != nil {
		return err
	}

	localOAuth := map[string]accounts.StoredCodexAccount{}
	for _, account := range localStored {
		if account.IsAPIKey() {
			continue
		}
		email := strings.TrimSpace(account.Email)
		if email == "" {
			continue
		}
		localOAuth[strings.ToLower(email)] = account
	}
	remoteOAuth := map[string]remoteServerAccountStatus{}
	for _, account := range remoteAccounts {
		if account.Provider != accounts.ProviderCodex || account.AuthMode != accounts.AuthModeOAuth {
			continue
		}
		email := strings.TrimSpace(account.Email)
		if email == "" {
			email = strings.TrimSpace(account.ID)
		}
		if email == "" || strings.HasPrefix(strings.ToLower(email), "apikey:") {
			continue
		}
		remoteOAuth[strings.ToLower(email)] = account
	}

	localEmails := sortedKeys(localOAuth)
	remoteEmails := sortedKeys(remoteOAuth)
	missing := make([]string, 0)
	present := make([]string, 0)
	serverOnly := make([]string, 0)
	invalid := make([]string, 0)
	for _, email := range localEmails {
		if _, ok := remoteOAuth[strings.ToLower(email)]; ok {
			present = append(present, email)
		} else {
			missing = append(missing, email)
		}
	}
	for _, email := range remoteEmails {
		if _, ok := localOAuth[strings.ToLower(email)]; !ok {
			serverOnly = append(serverOnly, email)
		}
		status := remoteOAuth[strings.ToLower(email)]
		if status.AuthChecked && !status.AuthValid {
			invalid = append(invalid, email)
		}
	}

	fmt.Fprintf(r.out, "Server: %s (%s)\n", server.Name, server.URL)
	if !statusAvailable {
		fmt.Fprintln(r.out, "Account status: unavailable on this server version; run sr server install "+server.Name+" to enable refresh-token checks.")
	}
	printEmailGroup(r.out, "Missing on server", missing)
	printEmailGroup(r.out, "Already on server", present)
	printStatusGroup(r.out, "Invalid on server", invalid, remoteOAuth)
	printEmailGroup(r.out, "Server-only OAuth accounts", serverOnly)

	targets := uniqueSorted(append(append([]string{}, missing...), invalid...))
	if len(emails) > 0 {
		targets = targets[:0]
		for _, email := range emails {
			needle := strings.ToLower(strings.TrimSpace(email))
			if account, ok := localOAuth[needle]; ok {
				targets = append(targets, account.Email)
				continue
			}
			if account, ok := remoteOAuth[needle]; ok {
				targets = append(targets, accountEmail(account.ID, account.Email))
				continue
			}
			return fmt.Errorf("%s is not a local or server OAuth account", email)
		}
		targets = uniqueSorted(targets)
	} else if *all {
		targets = uniqueSorted(append(append([]string{}, localEmails...), invalid...))
	}
	if *dryRun {
		printEmailGroup(r.out, "Would reauth on server", targets)
		return nil
	}
	if len(targets) == 0 {
		fmt.Fprintln(r.out, "No missing local OAuth accounts to reauth on the server.")
		fmt.Fprintln(r.out, "Use --all or --email <email> to replace an existing server-owned refresh-token chain.")
		return nil
	}
	if !*yes {
		ok, err := r.confirmServerSync(len(targets), server.Name)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(r.out, "No changes made.")
			return nil
		}
	}
	fmt.Fprintln(r.out, "Each login below creates a fresh server-owned OAuth refresh-token chain. Existing local refresh tokens are not uploaded.")
	colored := colorEnabled(r.out)
	for _, email := range targets {
		fmt.Fprintf(r.out, "\nSign in as %s for server %s.\n", style(colored, ansiBold+ansiMagenta, email), server.Name)
		if err := r.serverLoginOne(ctx, server, *deviceAuth, email); err != nil {
			return err
		}
	}
	fmt.Fprintf(r.out, "\nSynced %d server-owned OAuth account(s) to %s.\n", len(targets), server.Name)
	return nil
}

func accountEmail(id, email string) string {
	if strings.TrimSpace(email) != "" {
		return strings.TrimSpace(email)
	}
	return strings.TrimSpace(id)
}

func (r cxRunner) confirmServerSync(count int, serverName string) (bool, error) {
	if r.in == nil {
		return false, nil
	}
	reader := bufio.NewReader(r.in)
	answer, err := promptLine(r.out, reader, fmt.Sprintf("Reauth %d account(s) on server %s? [y/N]: ", count, serverName))
	if err != nil {
		return false, err
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}

type repeatedStringFlag []string

func (f *repeatedStringFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *repeatedStringFlag) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("empty value")
	}
	*f = append(*f, value)
	return nil
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		key := strings.ToLower(strings.TrimSpace(value))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func printEmailGroup(w io.Writer, label string, emails []string) {
	if len(emails) == 0 {
		fmt.Fprintf(w, "%s: none\n", label)
		return
	}
	fmt.Fprintf(w, "%s:\n", label)
	for _, email := range emails {
		fmt.Fprintf(w, "  %s\n", email)
	}
}

func printStatusGroup(w io.Writer, label string, emails []string, statuses map[string]remoteServerAccountStatus) {
	if len(emails) == 0 {
		fmt.Fprintf(w, "%s: none\n", label)
		return
	}
	fmt.Fprintf(w, "%s:\n", label)
	for _, email := range emails {
		status := statuses[strings.ToLower(email)]
		if status.Error == "" {
			fmt.Fprintf(w, "  %s\n", email)
			continue
		}
		fmt.Fprintf(w, "  %s: %s\n", email, status.Error)
	}
}

func (r cxRunner) serverLoginOne(ctx context.Context, server cxServerConfig, deviceAuth bool, expectedEmail string) error {
	previousActive, hadPreviousActive, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		return err
	}
	if err := r.store.SyncActiveToStore(); err != nil {
		return err
	}
	restored := false
	defer func() {
		if !restored {
			_ = restoreActiveCodexAuth(previousActive, hadPreviousActive)
		}
	}()

	loginArgs := []string{"login"}
	if deviceAuth {
		loginArgs = append(loginArgs, "--device-auth")
	}
	fmt.Fprintf(r.out, "Opening Codex OAuth login for server %s...\n", server.Name)
	if err := r.commandRunner().Run(ctx, "codex", loginArgs, r.in, r.out, r.errOut); err != nil {
		return fmt.Errorf("codex login failed: %w", err)
	}

	auth, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		return err
	}
	if !ok || auth.Tokens == nil || auth.Tokens.IDToken == "" {
		return fmt.Errorf("codex login did not write OAuth auth")
	}
	email, err := accounts.ExtractEmailFromJWT(auth.Tokens.IDToken)
	if err != nil || email == "" {
		return fmt.Errorf("could not extract email from logged-in auth")
	}
	if expectedEmail != "" && !strings.EqualFold(email, expectedEmail) {
		return fmt.Errorf("logged in as %s, expected %s; no account was uploaded", email, expectedEmail)
	}
	account := accounts.StoredCodexAccount{
		Email:   email,
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    auth,
	}
	if err := r.uploadServerAccount(ctx, server, account); err != nil {
		return err
	}
	if err := restoreActiveCodexAuth(previousActive, hadPreviousActive); err != nil {
		return err
	}
	restored = true
	fmt.Fprintf(r.out, "Added server-owned account %s to %s\n", account.Email, server.Name)
	fmt.Fprintln(r.out, "Local auth was restored, so only the server owns the new refresh-token chain.")
	return nil
}

func restoreActiveCodexAuth(previous accounts.CodexAuthFile, hadPrevious bool) error {
	if hadPrevious {
		return accounts.WriteActiveCodexAuth(previous)
	}
	err := os.Remove(accounts.DefaultCodexAuthPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (r cxRunner) uploadServerAccount(ctx context.Context, server cxServerConfig, account accounts.StoredCodexAccount) error {
	tmpDir, err := os.MkdirTemp("", "cx-server-auth-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	relPath := filepath.Join("codex", "accounts", cxAccountFilename(account.Email))
	body, err := json.MarshalIndent(account, "", "  ")
	if err != nil {
		return err
	}
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: relPath, Mode: 0o600, Size: int64(len(body))}); err != nil {
		return err
	}
	if _, err := tw.Write(body); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	archivePath := filepath.Join(tmpDir, "codex-account.tgz")
	if err := os.WriteFile(archivePath, archive.Bytes(), 0o600); err != nil {
		return err
	}

	if host := sshHostForServer(server); host != "" {
		if err := r.uploadServerAccountSSH(ctx, server, host, archive.Bytes()); err == nil {
			return nil
		} else if server.GCPInstance == "" || server.GCPZone == "" {
			return err
		} else {
			if r.errOut != nil {
				fmt.Fprintf(r.errOut, "direct server upload failed, falling back to gcloud: %v\n", err)
			}
		}
	}

	if server.GCPInstance == "" || server.GCPZone == "" {
		return fmt.Errorf("server %s has no GCP target", server.Name)
	}
	remotePath := fmt.Sprintf("/tmp/cx-server-auth-%d.tgz", time.Now().UnixNano())
	scpArgs := []string{"compute", "scp", archivePath, server.GCPInstance + ":" + remotePath, "--zone", server.GCPZone}
	if server.GCPProject != "" {
		scpArgs = append(scpArgs, "--project", server.GCPProject)
	}
	if err := r.commandRunner().Run(ctx, "gcloud", scpArgs, nil, r.out, r.errOut); err != nil {
		return fmt.Errorf("upload account archive: %w", err)
	}
	remoteCommand := strings.Join([]string{
		"set -euo pipefail",
		"reload_status=$(curl -sS -o /dev/null -w '%{http_code}' http://127.0.0.1:31415/_subrouter/reload-accounts || true)",
		"if [ \"$reload_status\" != \"405\" ]; then echo " + shellQuote("Subrouter server is too old for hot account reload; run sr server install "+server.Name+" first.") + " >&2; exit 1; fi",
		"sudo install -d -o subrouter -g subrouter -m 0750 /var/lib/subrouter/codex/accounts",
		"sudo tar -C /var/lib/subrouter -xzf " + shellQuote(remotePath),
		"sudo find /var/lib/subrouter/codex -name '._*' -delete",
		"sudo chown -R subrouter:subrouter /var/lib/subrouter/codex",
		"sudo rm -f " + shellQuote(remotePath),
		"curl -fsS -X POST http://127.0.0.1:31415/_subrouter/reload-accounts >/dev/null",
	}, " && ")
	sshArgs := []string{"compute", "ssh", server.GCPInstance, "--zone", server.GCPZone, "--command", remoteCommand}
	if server.GCPProject != "" {
		sshArgs = append(sshArgs, "--project", server.GCPProject)
	}
	if err := r.commandRunner().Run(ctx, "gcloud", sshArgs, nil, r.out, r.errOut); err != nil {
		return fmt.Errorf("install account on server: %w", err)
	}
	return nil
}

func (r cxRunner) uploadServerAccountSSH(ctx context.Context, server cxServerConfig, host string, archive []byte) error {
	remotePath := fmt.Sprintf("/tmp/cx-server-auth-%d.tgz", time.Now().UnixNano())
	remoteCommand := strings.Join([]string{
		"set -euo pipefail",
		"cat > " + shellQuote(remotePath),
		"reload_status=$(curl -sS -o /dev/null -w '%{http_code}' http://127.0.0.1:31415/_subrouter/reload-accounts || true)",
		"if [ \"$reload_status\" != \"405\" ]; then echo " + shellQuote("Subrouter server is too old for hot account reload; run sr server install "+server.Name+" first.") + " >&2; exit 1; fi",
		"sudo install -d -o subrouter -g subrouter -m 0750 /var/lib/subrouter/codex/accounts",
		"sudo tar -C /var/lib/subrouter -xzf " + shellQuote(remotePath),
		"sudo find /var/lib/subrouter/codex -name '._*' -delete",
		"sudo chown -R subrouter:subrouter /var/lib/subrouter/codex",
		"sudo rm -f " + shellQuote(remotePath),
		"curl -fsS -X POST http://127.0.0.1:31415/_subrouter/reload-accounts >/dev/null",
	}, " && ")
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=15",
		"-o", "LogLevel=ERROR",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		host,
		remoteCommand,
	}
	uploadCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if err := r.commandRunner().Run(uploadCtx, "ssh", args, bytes.NewReader(archive), r.out, r.errOut); err != nil {
		return fmt.Errorf("install account on server over ssh: %w", err)
	}
	return nil
}

func sshHostForServer(server cxServerConfig) string {
	parsed, err := url.Parse(server.URL)
	if err != nil {
		return ""
	}
	host := strings.TrimSpace(parsed.Hostname())
	if host == "" || host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return ""
	}
	return host
}

func cxAccountFilename(email string) string {
	var b strings.Builder
	for _, r := range email {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '@' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String() + ".json"
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
