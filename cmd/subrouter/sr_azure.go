package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/azureopenai"
)

const srAzureHelp = `sr azure - Run Codex through Azure OpenAI with Azure CLI authentication

Usage:
  sr azure add <profile> --endpoint <url> --deployment <name>
  sr azure list
  sr azure remove <profile>
  sr azure codex <profile> [codex args...]

The profile stores endpoint and deployment metadata, never an access token.
Run 'az login' first. Subrouter renews Entra access tokens through the same Azure CLI session.
`

func azureProviderAlias(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "azure", "azure-openai", "foundry":
		return true
	default:
		return false
	}
}

func (r srRunner) azure(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return r.azureList()
	}
	switch strings.ToLower(strings.TrimSpace(args[0])) {
	case "add", "login":
		return r.azureAdd(ctx, args[1:])
	case "list", "ls", "status":
		return r.azureList()
	case "remove", "rm":
		if len(args) != 2 {
			return errors.New("usage: sr azure remove <profile>")
		}
		return r.azureRemove(args[1])
	case "codex", "run":
		return r.azureCodex(ctx, args[1:])
	case "help", "-h", "--help":
		fmt.Fprint(r.out, srAzureHelp)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n%s", "sr azure "+args[0], srAzureHelp)
	}
}

func (r srRunner) azureProfileStore() azureopenai.Store {
	if strings.TrimSpace(r.azureStore.Path) != "" {
		return r.azureStore
	}
	return azureopenai.DefaultStore()
}

func (r srRunner) restartAzureDaemon() error {
	if r.restartDaemon != nil {
		return r.restartDaemon()
	}
	return restartInstalledDaemon()
}

func (r srRunner) azureAdd(ctx context.Context, args []string) error {
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name = args[0]
		args = args[1:]
	}
	flags := flag.NewFlagSet("azure add", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	nameFlag := flags.String("name", name, "profile name")
	endpoint := flags.String("endpoint", strings.TrimSpace(os.Getenv("AZURE_OPENAI_ENDPOINT")), "Azure OpenAI endpoint")
	deployment := flags.String("deployment", strings.TrimSpace(os.Getenv("AZURE_OPENAI_DEPLOYMENT")), "Azure model deployment name")
	tokenResource := flags.String("token-resource", strings.TrimSpace(os.Getenv("AZURE_OPENAI_TOKEN_RESOURCE")), "Azure token resource audience")
	azureCLI := flags.String("azure-cli", strings.TrimSpace(os.Getenv("AZURE_CLI")), "Azure CLI path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	reader := bufio.NewReader(r.in)
	var err error
	if strings.TrimSpace(*nameFlag) == "" {
		if !readerIsTerminal(r.in) {
			return errors.New("usage: sr azure add <profile> --endpoint <url> --deployment <name>")
		}
		*nameFlag, err = promptLine(r.out, reader, "Profile name: ")
		if err != nil {
			return err
		}
	}
	if strings.TrimSpace(*endpoint) == "" {
		if !readerIsTerminal(r.in) {
			return errors.New("Azure OpenAI endpoint is required; pass --endpoint")
		}
		*endpoint, err = promptLine(r.out, reader, "Azure OpenAI endpoint: ")
		if err != nil {
			return err
		}
	}
	if strings.TrimSpace(*deployment) == "" {
		if !readerIsTerminal(r.in) {
			return errors.New("Azure OpenAI deployment is required; pass --deployment")
		}
		*deployment, err = promptLine(r.out, reader, "Deployment name: ")
		if err != nil {
			return err
		}
	}
	cliPath, err := resolveAzureCLI(*azureCLI)
	if err != nil {
		return err
	}
	profile, err := azureopenai.NormalizeProfile(azureopenai.Profile{
		Name:          *nameFlag,
		Endpoint:      *endpoint,
		Deployment:    *deployment,
		TokenResource: *tokenResource,
		AzureCLI:      cliPath,
	})
	if err != nil {
		return err
	}
	// Validate the selected Azure CLI session before committing the profile.
	// The returned bearer token stays in memory and is deliberately discarded.
	if _, err := azureopenai.FetchCLIAccessToken(ctx, r.commandRunner(), profile); err != nil {
		return err
	}
	existed, err := r.azureProfileStore().Save(profile)
	if err != nil {
		return err
	}
	if err := r.restartAzureDaemon(); err != nil {
		return err
	}
	verb := "Added"
	if existed {
		verb = "Updated"
	}
	fmt.Fprintf(r.out, "%s Azure OpenAI profile %s (deployment %s). Access tokens are renewed through Azure CLI and are not stored.\n", verb, profile.Name, profile.Deployment)
	return nil
}

func resolveAzureCLI(explicit string) (string, error) {
	name := strings.TrimSpace(explicit)
	if name == "" {
		name = "az"
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", errors.New("Azure CLI not found; install 'az', run 'az login', then retry")
	}
	return path, nil
}

func (r srRunner) azureList() error {
	profiles, err := r.azureProfileStore().List()
	if err != nil {
		return err
	}
	if len(profiles) == 0 {
		fmt.Fprintln(r.out, "No Azure OpenAI profiles. Run: sr azure add <profile> --endpoint <url> --deployment <name>")
		return nil
	}
	for _, profile := range profiles {
		fmt.Fprintf(r.out, "%s\t%s\t%s\n", profile.Name, profile.Deployment, profile.Endpoint)
	}
	return nil
}

func (r srRunner) azureRemove(name string) error {
	removed, err := r.azureProfileStore().Remove(name)
	if err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("Azure OpenAI profile %q not found", name)
	}
	if err := r.restartAzureDaemon(); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Removed Azure OpenAI profile %s.\n", strings.ToLower(strings.TrimSpace(name)))
	return nil
}

func (r srRunner) azureCodex(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: sr azure codex <profile> [codex args...]")
	}
	profile, ok, err := r.azureProfileStore().Find(args[0])
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("Azure OpenAI profile %q not found; run 'sr azure list'", args[0])
	}
	codexArgsRaw := args[1:]
	if requested := codexModelArg(codexArgsRaw); requested != "" && requested != profile.Deployment {
		return fmt.Errorf("Azure profile %s is bound to deployment %q, not %q", profile.Name, profile.Deployment, requested)
	}
	// Fail before starting Codex when the Azure CLI session is absent or stale.
	if _, err := azureopenai.FetchCLIAccessToken(ctx, r.commandRunner(), profile); err != nil {
		return err
	}
	local := localBaseURL()
	client := fallbackHTTPClient()
	if r.client != nil {
		client = r.client
	}
	if !ensureLocalHealthy(ctx, client, local, defaultDaemonStarter(), r.errOut) {
		return fmt.Errorf("local proxy is unavailable; run '%s setup'", programBase())
	}
	baseURL, err := azureCodexBaseURL(local, profile.Name)
	if err != nil {
		return err
	}
	cloudConfig, err := cloudModeConfig()
	if err != nil {
		return err
	}
	localProxyToken := cloudClientProxyToken(cloudConfig, local)
	childArgs := azureCodexArgs(codexArgsRaw, baseURL, profile.Deployment, localProxyToken != "")
	env := []string(nil)
	if localProxyToken != "" {
		env = []string{"SUBROUTER_CODEX_DUMMY_API_KEY=" + localProxyToken}
	}
	return r.commandRunner().RunWithEnv(
		ctx,
		envOrDefault("SUBROUTER_CODEX_BIN", "codex"),
		childArgs,
		env,
		r.in,
		r.out,
		r.errOut,
	)
}

func azureCodexBaseURL(localBaseURL, profileName string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(localBaseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid local Subrouter URL %q", localBaseURL)
	}
	parsed.Path = azureOpenAIClientPath(profileName) + "/v1"
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func azureOpenAIClientPath(profileName string) string {
	return "/azure/" + strings.ToLower(strings.TrimSpace(profileName))
}

func azureCodexArgs(args []string, baseURL, deployment string, forceAuthenticatedProvider bool) []string {
	authConfig := `model_providers.subrouter_azure.experimental_bearer_token="subrouter"`
	if forceAuthenticatedProvider {
		authConfig = `model_providers.subrouter_azure.env_key="SUBROUTER_CODEX_DUMMY_API_KEY"`
	}
	configArgs := []string{
		"-c", "model=" + strconv.Quote(deployment),
		"-c", `model_provider="subrouter_azure"`,
		"-c", `model_providers.subrouter_azure.name="Azure OpenAI via Subrouter"`,
		"-c", "model_providers.subrouter_azure.base_url=" + strconv.Quote(baseURL),
		"-c", authConfig,
		"-c", `model_providers.subrouter_azure.wire_api="responses"`,
		"-c", `model_providers.subrouter_azure.supports_websockets=false`,
		"-c", `model_providers.subrouter_azure.http_headers={"X-Subrouter-Agent"="codex"}`,
	}
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
