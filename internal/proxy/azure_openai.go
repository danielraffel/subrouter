package proxy

import (
	"strings"

	"github.com/manaflow-ai/subrouter/internal/azureopenai"
)

func azureOpenAIProfileNameFromPath(requestPath string) (string, bool) {
	name, _, ok := stripAzureOpenAIPath(requestPath)
	return name, ok
}

func stripAzureOpenAIPath(requestPath string) (name, rest string, ok bool) {
	trimmed := strings.TrimPrefix(requestPath, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 2 || parts[0] != "azure" {
		return "", "", false
	}
	name, valid := azureopenai.ProfileNameFromAccountID(azureopenai.AccountID(parts[1]))
	if !valid || !strings.EqualFold(parts[1], name) {
		return "", "", false
	}
	if len(parts) == 2 {
		return name, "", true
	}
	return name, "/" + strings.Join(parts[2:], "/"), true
}

func azureOpenAIBasePath(requestPath string) bool {
	_, rest, ok := stripAzureOpenAIPath(requestPath)
	return ok && (rest == "" || rest == "/" || rest == "/v1" || rest == "/v1/")
}
