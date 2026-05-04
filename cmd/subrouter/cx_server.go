package main

import (
	"archive/tar"
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

const cxServerHelp = `cx server - Manage Subrouter servers

Usage:
  cx server list
  cx server add <name> --url <url> --gcp-instance <name> --gcp-zone <zone> [--gcp-project <project>]
  cx server remove <name>
  cx server status <name>
  cx server login <name> [--device-auth]

`

type cxServerStore struct {
	Path string
}

type cxServerConfig struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	GCPProject  string `json:"gcpProject,omitempty"`
	GCPZone     string `json:"gcpZone,omitempty"`
	GCPInstance string `json:"gcpInstance,omitempty"`
}

type cxServerFile struct {
	Servers []cxServerConfig `json:"servers"`
}

func (r cxRunner) server(ctx context.Context, args []string) error {
	store := defaultCXServerStore(r.store)
	if len(args) == 0 {
		fmt.Fprint(r.out, cxServerHelp)
		return nil
	}
	switch args[0] {
	case "list", "ls":
		return r.serverList(store)
	case "add":
		return r.serverAdd(store, args[1:])
	case "remove", "rm":
		if len(args) != 2 {
			return fmt.Errorf("usage: cx server remove <name>")
		}
		return r.serverRemove(store, args[1])
	case "status":
		if len(args) != 2 {
			return fmt.Errorf("usage: cx server status <name>")
		}
		return r.serverStatus(ctx, store, args[1])
	case "login", "add-account", "add-auth":
		return r.serverLogin(ctx, store, args[1:])
	case "help", "-h", "--help":
		fmt.Fprint(r.out, cxServerHelp)
		return nil
	default:
		return fmt.Errorf("unknown cx server command %q\n%s", args[0], cxServerHelp)
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

func (r cxRunner) serverList(store cxServerStore) error {
	file, err := store.load()
	if err != nil {
		return err
	}
	if len(file.Servers) == 0 {
		fmt.Fprintln(r.out, "No servers configured. Run: cx server add <name> --url <url> --gcp-instance <name> --gcp-zone <zone>")
		return nil
	}
	for _, server := range file.Servers {
		fmt.Fprintf(r.out, "%s\t%s\t%s\t%s\n", server.Name, server.URL, server.GCPInstance, server.GCPZone)
	}
	return nil
}

func (r cxRunner) serverAdd(store cxServerStore, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cx server add <name> --url <url> --gcp-instance <name> --gcp-zone <zone> [--gcp-project <project>]")
	}
	name := args[0]
	flags := flag.NewFlagSet("cx server add", flag.ContinueOnError)
	flags.SetOutput(r.errOut)
	serverURL := flags.String("url", "", "subrouter base URL, such as http://100.64.0.1:31415")
	gcpProject := flags.String("gcp-project", "", "GCP project; defaults to current gcloud project")
	gcpZone := flags.String("gcp-zone", "", "GCP zone")
	gcpInstance := flags.String("gcp-instance", "", "GCP instance name")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("server name is required")
	}
	if _, err := url.ParseRequestURI(*serverURL); err != nil || *serverURL == "" {
		return fmt.Errorf("--url must be a valid URL")
	}
	if *gcpInstance == "" || *gcpZone == "" {
		return fmt.Errorf("--gcp-instance and --gcp-zone are required")
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
	}
	replaced := false
	for i := range file.Servers {
		if file.Servers[i].Name == name {
			file.Servers[i] = next
			replaced = true
			break
		}
	}
	if !replaced {
		file.Servers = append(file.Servers, next)
	}
	if err := store.save(file); err != nil {
		return err
	}
	if replaced {
		fmt.Fprintf(r.out, "Updated server: %s\n", name)
	} else {
		fmt.Fprintf(r.out, "Added server: %s\n", name)
	}
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/_subrouter/accounts", nil)
	if err != nil {
		return err
	}
	client := r.client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	res, err := client.Do(req)
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

func (r cxRunner) serverLogin(ctx context.Context, store cxServerStore, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cx server login <name> [--device-auth]")
	}
	name := args[0]
	flags := flag.NewFlagSet("cx server login", flag.ContinueOnError)
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

	previousActive, hadPreviousActive, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		return err
	}
	if err := r.store.SyncActiveToStore(); err != nil {
		return err
	}

	loginArgs := []string{"login"}
	if *deviceAuth {
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
	localBefore, existedBefore, err := r.store.FindStored(email)
	if err != nil {
		return err
	}
	account, _, err := r.store.ImportActive()
	if err != nil {
		return err
	}
	if err := r.uploadServerAccount(ctx, server, account); err != nil {
		return err
	}
	if existedBefore {
		if err := r.store.SaveStored(localBefore); err != nil {
			return err
		}
	} else if _, _, err := r.store.RemoveStored(email); err != nil {
		return err
	}
	if hadPreviousActive {
		if err := accounts.WriteActiveCodexAuth(previousActive); err != nil {
			return err
		}
	} else {
		_ = os.Remove(accounts.DefaultCodexAuthPath())
	}
	fmt.Fprintf(r.out, "Added server-owned account %s to %s\n", account.Email, server.Name)
	fmt.Fprintln(r.out, "Local auth was restored, so only the server owns the new refresh-token chain.")
	return nil
}

func (r cxRunner) uploadServerAccount(ctx context.Context, server cxServerConfig, account accounts.StoredCodexAccount) error {
	if server.GCPInstance == "" || server.GCPZone == "" {
		return fmt.Errorf("server %s has no GCP target", server.Name)
	}
	tmpDir, err := os.MkdirTemp("", "cx-server-auth-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	relPath := filepath.Join(".codex-accounts", "accounts", cxAccountFilename(account.Email))
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
		"sudo install -d -o subrouter -g subrouter -m 0750 /var/lib/subrouter/.codex-accounts/accounts",
		"sudo tar -C /var/lib/subrouter -xzf " + shellQuote(remotePath),
		"sudo find /var/lib/subrouter/.codex-accounts -name '._*' -delete",
		"sudo chown -R subrouter:subrouter /var/lib/subrouter/.codex-accounts",
		"sudo rm -f " + shellQuote(remotePath),
		"sudo systemctl restart subrouter",
		"curl -fsS http://127.0.0.1:31415/_subrouter/health >/dev/null",
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
