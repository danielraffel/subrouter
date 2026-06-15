package accounts

import "time"

type Provider string

const (
	ProviderCodex  Provider = "codex"
	ProviderClaude Provider = "claude"
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

// SchedulerKey returns a provider-scoped identity for use as the account
// scheduler's map key. A codex account and a claude profile for the same person
// share Account.ID (the email), so keying the scheduler by the bare ID lets one
// provider's exhausted score clobber the other's. Qualifying by provider keeps
// the two pools isolated. Legacy accounts with an empty Provider fall back to
// the bare ID so existing single-provider keys are unchanged.
func SchedulerKey(provider Provider, id string) string {
	if provider == "" {
		return id
	}
	return string(provider) + ":" + id
}

// SchedulerKey returns the provider-scoped scheduler key for this account.
func (a Account) SchedulerKey() string {
	return SchedulerKey(a.Provider, a.ID)
}
