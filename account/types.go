package account

import "time"

type Provider string

const (
	ProviderCodex  Provider = "codex"
	ProviderClaude Provider = "claude"
	ProviderKimi   Provider = "kimi"
	ProviderZAI    Provider = "zai"

	// ProviderOpenRouter routes to OpenRouter's OpenAI-compatible API with a
	// per-account API key.
	ProviderOpenRouter Provider = "openrouter"

	// ProviderGrok routes to xAI's OpenAI-compatible API with a per-account
	// API key.
	ProviderGrok Provider = "grok"

	// ProviderQwen routes to Alibaba Cloud Model Studio's Coding Plan, whose
	// subscription is addressed with a plan-specific API key on a dedicated
	// OpenAI-compatible endpoint.
	ProviderQwen Provider = "qwen"

	// ProviderQwenToken routes to Model Studio's Token Plan, which is a
	// separate subscription from the Coding Plan with its own key and its own
	// endpoint.
	ProviderQwenToken Provider = "qwen-token"

	// ProviderQwenAnthropic routes to the Token Plan's Anthropic-protocol
	// endpoint, which serves the same subscription to Anthropic-shaped clients.
	ProviderQwenAnthropic Provider = "qwen-anthropic"
)

type AuthMode string

const (
	AuthModeOAuth  AuthMode = "oauth"
	AuthModeAPIKey AuthMode = "apikey"
)

type Account struct {
	ID        string
	Provider  Provider
	AuthMode  AuthMode
	Label     string
	Email     string
	AddedAt   time.Time
	Token     string
	AccountID string
	Source    string
}

func (a Account) AuthorizationHeader() string {
	if a.Token == "" {
		return ""
	}
	return "Bearer " + a.Token
}
