package account

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

type Provider string

const (
	ProviderCodex  Provider = "codex"
	ProviderClaude Provider = "claude"
	ProviderKimi   Provider = "kimi"
	ProviderZAI    Provider = "zai"
)

type AuthMode string

const (
	AuthModeOAuth  AuthMode = "oauth"
	AuthModeAPIKey AuthMode = "apikey"
)

type Account struct {
	ID       string
	Provider Provider
	AuthMode AuthMode
	Label    string
	Email    string
	AddedAt  time.Time
	Token    string
	// CredentialVersion identifies the complete OAuth credential chain without
	// exposing either token. It is deliberately omitted from serialized account
	// views and changes when a repair replaces only the refresh token.
	CredentialVersion string `json:"-"`
	AccountID         string
	Source            string
}

func OAuthCredentialVersion(accessToken, refreshToken string) string {
	sum := sha256.Sum256([]byte(accessToken + "\x00" + refreshToken))
	return hex.EncodeToString(sum[:])
}

func (a Account) CredentialIdentity() string {
	if a.CredentialVersion != "" {
		return a.CredentialVersion
	}
	return a.Token
}

func (a Account) AuthorizationHeader() string {
	if a.Token == "" {
		return ""
	}
	return "Bearer " + a.Token
}
