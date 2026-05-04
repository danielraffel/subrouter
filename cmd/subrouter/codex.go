package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/session"
)

const defaultCodexBaseURL = "http://127.0.0.1:31415/v1"
const defaultSubrouterHealthURL = "http://127.0.0.1:31415/_subrouter/health"

func codex(args []string) error {
	baseURL := os.Getenv("SUBROUTER_CODEX_BASE_URL")
	if baseURL == "" {
		baseURL = defaultCodexBaseURLFor(defaultSubrouterHealthURL, defaultCXServerStore(accounts.DefaultCodexStore()), &http.Client{Timeout: 500 * time.Millisecond})
	}
	bin := envOrDefault("SUBROUTER_CODEX_BIN", "codex")
	userEmailRaw := os.Getenv("SUBROUTER_CODEX_USER_EMAIL")
	accountID := session.NormalizeAccountID(os.Getenv("SUBROUTER_CODEX_ACCOUNT_ID"))
	userEmail := ""
	if strings.TrimSpace(userEmailRaw) != "" {
		userEmail = session.NormalizeUserEmail(userEmailRaw)
		if userEmail == "" {
			return fmt.Errorf("SUBROUTER_CODEX_USER_EMAIL must be a valid email address; use SUBROUTER_CODEX_ACCOUNT_ID to force an account such as team-codex-1")
		}
	}

	cmd := exec.CommandContext(context.Background(), bin, codexArgs(args, baseURL, userEmail, accountID)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	env := os.Environ()
	if userEmail != "" || accountID != "" {
		env = upsertEnv(env, "SUBROUTER_CODEX_DUMMY_API_KEY", "subrouter")
	}
	cmd.Env = env
	return cmd.Run()
}

func defaultCodexBaseURLFor(localHealthURL string, store cxServerStore, client *http.Client) string {
	if subrouterHealthOK(localHealthURL, client) {
		return defaultCodexBaseURL
	}
	file, err := store.load()
	if err != nil || len(file.Servers) != 1 {
		return defaultCodexBaseURL
	}
	return codexBaseURLForServer(file.Servers[0])
}

func subrouterHealthOK(healthURL string, client *http.Client) bool {
	if client == nil {
		client = &http.Client{Timeout: 500 * time.Millisecond}
	}
	req, err := http.NewRequest(http.MethodGet, healthURL, nil)
	if err != nil {
		return false
	}
	res, err := client.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	return res.StatusCode >= 200 && res.StatusCode < 300
}

func codexBaseURLForServer(server cxServerConfig) string {
	baseURL := strings.TrimRight(server.URL, "/")
	if strings.HasSuffix(baseURL, "/v1") {
		return baseURL
	}
	return baseURL + "/v1"
}

func codexArgs(args []string, baseURL, userEmail, accountID string) []string {
	configArgs := codexConfigArgs(baseURL, userEmail, accountID)
	if len(args) == 0 || strings.HasPrefix(args[0], "-") || !isKnownCodexCommand(args[0]) {
		return append(configArgs, args...)
	}
	if !isSubrouterRoutedCodexCommand(args[0]) {
		return append([]string(nil), args...)
	}
	out := []string{args[0]}
	out = append(out, configArgs...)
	out = append(out, args[1:]...)
	return out
}

func codexConfigArgs(baseURL, userEmail, accountID string) []string {
	if userEmail == "" && accountID == "" {
		return []string{"-c", "openai_base_url=" + strconv.Quote(baseURL)}
	}
	return []string{
		"-c", `model_provider="subrouter"`,
		"-c", `model_providers.subrouter.name="Subrouter"`,
		"-c", "model_providers.subrouter.base_url=" + strconv.Quote(baseURL),
		"-c", `model_providers.subrouter.env_key="SUBROUTER_CODEX_DUMMY_API_KEY"`,
		"-c", `model_providers.subrouter.wire_api="responses"`,
		"-c", `model_providers.subrouter.supports_websockets=true`,
		"-c", `model_providers.subrouter.http_headers=` + codexSubrouterHeaders(userEmail, accountID),
	}
}

func codexSubrouterHeaders(userEmail, accountID string) string {
	headers := []string{`"X-Subrouter-Agent"="codex"`}
	if userEmail != "" {
		headers = append(headers, `"X-Subrouter-User-Email"=`+strconv.Quote(userEmail))
	}
	if accountID != "" {
		headers = append(headers, `"X-Subrouter-Account-ID"=`+strconv.Quote(accountID))
	}
	return "{" + strings.Join(headers, ",") + "}"
}

func isSubrouterRoutedCodexCommand(command string) bool {
	switch command {
	case "exec", "e", "review", "resume", "fork", "app-server":
		return true
	default:
		return false
	}
}

func isKnownCodexCommand(command string) bool {
	switch command {
	case "exec", "e", "review", "login", "logout", "mcp", "plugin", "mcp-server", "app-server", "app", "completion", "sandbox", "debug", "apply", "a", "resume", "fork", "cloud", "exec-server", "features", "help":
		return true
	default:
		return false
	}
}

func envOrDefault(key string, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func upsertEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, item := range env {
		if strings.HasPrefix(item, prefix) {
			out := append([]string(nil), env...)
			out[i] = prefix + value
			return out
		}
	}
	return append(env, prefix+value)
}
