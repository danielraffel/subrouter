package proxy

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// ValidateCredentialUpstreams rejects configurations that could send a
// selected account credential over cleartext. HTTP remains available on
// loopback for local development and tests; every remote upstream must use
// HTTPS. Every URL field on Server is an upstream, so walking those fields
// keeps the startup gate complete when a provider adds another upstream.
func (s Server) ValidateCredentialUpstreams() error {
	value := reflect.ValueOf(s)
	typeOfURL := reflect.TypeOf((*url.URL)(nil))
	for i := 0; i < value.NumField(); i++ {
		field := value.Type().Field(i)
		if field.Type != typeOfURL || !strings.HasSuffix(field.Name, "Upstream") {
			continue
		}
		upstream, _ := value.Field(i).Interface().(*url.URL)
		if err := validateCredentialUpstream(field.Name, upstream); err != nil {
			return err
		}
	}
	return nil
}

func validateCredentialUpstream(name string, upstream *url.URL) error {
	if upstream == nil {
		return nil
	}
	host := strings.TrimSpace(upstream.Hostname())
	if host == "" {
		return fmt.Errorf("credential-bearing upstream %s must include a host", name)
	}
	if strings.EqualFold(upstream.Scheme, "https") {
		return nil
	}
	ip := net.ParseIP(host)
	loopback := strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
	if strings.EqualFold(upstream.Scheme, "http") && loopback {
		return nil
	}
	return fmt.Errorf("credential-bearing upstream %s must use HTTPS, except HTTP on loopback", name)
}

// authStyle names how a provider expects its API key to be presented.
type authStyle int

const (
	// authBearer sends Authorization: Bearer <key> and removes any client
	// X-Api-Key, which is the OpenAI-compatible convention.
	authBearer authStyle = iota
	// authBearerAndAnthropicKey sends both Authorization and X-Api-Key, and
	// defaults Anthropic-Version, for providers exposing an Anthropic-shaped API.
	authBearerAndAnthropicKey
)

// leaseEnvStyle names which SDK environment variables a lease hands to the
// sandbox so the client library points back at this proxy.
type leaseEnvStyle int

const (
	leaseEnvOpenAI leaseEnvStyle = iota
	leaseEnvAnthropic
)

// keyedProvider describes an API-key provider that the proxy routes by path
// prefix. Everything the router needs about such a provider lives in one entry,
// so adding a provider is a table row rather than an edit to a dozen switch
// statements that are easy to leave half-updated.
type keyedProvider struct {
	Provider accounts.Provider
	// PathPrefix is the first path segment that selects this provider, so
	// /<PathPrefix>/... routes here and the segment is stripped upstream.
	PathPrefix string
	// Aliases are the additional names accepted wherever a provider is named by
	// a human or an API caller. The canonical provider string is always accepted.
	Aliases []string
	// ModelPrefix infers this provider from a bare model id, e.g. "glm-" for ZAI.
	// Empty means no inference.
	ModelPrefix string
	// PlanLabel is how an API-key account of this provider is described in
	// account listings.
	PlanLabel string
	Auth      authStyle
	// CollapseVersionSegment marks a provider whose upstream base URL may already
	// end in /v1, so a client's own /v1 must not be forwarded twice.
	CollapseVersionSegment bool
	// VendorPrefixedModels marks a provider that addresses models as
	// vendor/model, where the segment before the slash belongs to the model id
	// rather than naming a provider.
	VendorPrefixedModels bool
	// LeaseAPI is the Pi adapter a lease advertises for this provider.
	LeaseAPI string
	// LeasePath is the single path a lease for this provider may call.
	LeasePath string
	LeaseEnv  leaseEnvStyle
	// Upstream reads this provider's configured base URL off the server.
	Upstream func(Server) *url.URL
}

// keyedProviders is the registry. Adding an API-key provider means adding an
// entry here and a base-URL field on Server; the routing, auth, lease, import,
// and CLI paths all read from this table.
var keyedProviders = []keyedProvider{
	{
		Provider:               accounts.ProviderKimi,
		PathPrefix:             "kimi",
		Aliases:                []string{"kimi-for-coding"},
		ModelPrefix:            "kimi-",
		PlanLabel:              "kimi api key",
		Auth:                   authBearerAndAnthropicKey,
		CollapseVersionSegment: true,
		LeaseAPI:               "anthropic-messages",
		LeasePath:              "/kimi/v1/messages",
		LeaseEnv:               leaseEnvAnthropic,
		Upstream:               func(s Server) *url.URL { return s.KimiUpstream },
	},
	{
		Provider:    accounts.ProviderZAI,
		PathPrefix:  "zai",
		Aliases:     []string{"glm", "glm-5.2"},
		ModelPrefix: "glm-",
		PlanLabel:   "zai api key",
		Auth:        authBearer,
		LeaseAPI:    "openai-completions",
		LeasePath:   "/zai/chat/completions",
		LeaseEnv:    leaseEnvOpenAI,
		Upstream:    func(s Server) *url.URL { return s.ZAIUpstream },
	},
	{
		Provider:   accounts.ProviderOpenRouter,
		PathPrefix: "openrouter",
		Aliases:    []string{"open-router"},
		PlanLabel:  "openrouter api key",
		Auth:       authBearer,
		// OpenRouter's base already ends in /v1 and it addresses every model as
		// vendor/model, e.g. anthropic/claude-opus-5.
		CollapseVersionSegment: true,
		VendorPrefixedModels:   true,
		LeaseAPI:               "openai-completions",
		LeasePath:              "/openrouter/chat/completions",
		LeaseEnv:               leaseEnvOpenAI,
		Upstream:               func(s Server) *url.URL { return s.OpenRouterUpstream },
	},
	{
		Provider:    accounts.ProviderGrok,
		PathPrefix:  "grok",
		Aliases:     []string{"xai", "x-ai"},
		ModelPrefix: "grok-",
		PlanLabel:   "grok api key",
		Auth:        authBearer,
		// api.x.ai/v1 already ends in /v1. xAI model ids are bare (grok-4), so
		// the vendor/model provider-selector rule still applies.
		CollapseVersionSegment: true,
		LeaseAPI:               "openai-completions",
		LeasePath:              "/grok/chat/completions",
		LeaseEnv:               leaseEnvOpenAI,
		Upstream:               func(s Server) *url.URL { return s.GrokUpstream },
	},
}

// keyedProviderFor returns the registry entry for a provider.
func keyedProviderFor(provider accounts.Provider) (keyedProvider, bool) {
	for _, entry := range keyedProviders {
		if entry.Provider == provider {
			return entry, true
		}
	}
	return keyedProvider{}, false
}

// keyedProviderForPathPrefix resolves the first path segment to a provider.
func keyedProviderForPathPrefix(segment string) (keyedProvider, bool) {
	for _, entry := range keyedProviders {
		if entry.PathPrefix == segment {
			return entry, true
		}
	}
	return keyedProvider{}, false
}

// keyedProviderForName resolves a provider named by a human or an API caller,
// accepting the canonical provider string and any registered alias.
func keyedProviderForName(name string) (keyedProvider, bool) {
	for _, entry := range keyedProviders {
		if name == string(entry.Provider) {
			return entry, true
		}
		for _, alias := range entry.Aliases {
			if name == alias {
				return entry, true
			}
		}
	}
	return keyedProvider{}, false
}

// keyedProviderForModelPrefix infers a provider from a bare model id.
func keyedProviderForModelPrefix(lowerModel string) (keyedProvider, bool) {
	for _, entry := range keyedProviders {
		if entry.ModelPrefix != "" && len(lowerModel) >= len(entry.ModelPrefix) &&
			lowerModel[:len(entry.ModelPrefix)] == entry.ModelPrefix {
			return entry, true
		}
	}
	return keyedProvider{}, false
}

// isKeyedProvider reports whether a provider is routed through the registry.
func isKeyedProvider(provider accounts.Provider) bool {
	_, ok := keyedProviderFor(provider)
	return ok
}

// keyedProviderNames lists every canonical provider name in the registry, in
// registry order, for advertising and for error messages.
func keyedProviderNames() []string {
	names := make([]string, 0, len(keyedProviders))
	for _, entry := range keyedProviders {
		names = append(names, string(entry.Provider))
	}
	return names
}

// applyKeyedProviderAuth presents the account's API key the way this provider
// expects it. Authorization is already set to the bearer form by the caller.
func applyKeyedProviderAuth(headers http.Header, account accounts.Account, entry keyedProvider) {
	switch entry.Auth {
	case authBearerAndAnthropicKey:
		headers.Set("Authorization", account.AuthorizationHeader())
		headers.Set("X-Api-Key", account.Token)
		if headers.Get("Anthropic-Version") == "" {
			headers.Set("Anthropic-Version", "2023-06-01")
		}
	default:
		headers.Del("X-Api-Key")
	}
}

// collapseDuplicateVersionSegment drops a leading /v1 from an already
// prefix-stripped path when the upstream base URL ends in /v1, so a client that
// sends /<provider>/v1/... does not reach /v1/v1/... upstream. The collapse is
// conditional on the configured base, so an unversioned override still forwards
// the client's own /v1.
func collapseDuplicateVersionSegment(path string, upstream *url.URL) string {
	upstreamPath := ""
	if upstream != nil {
		upstreamPath = strings.TrimRight(upstream.Path, "/")
	}
	if !strings.HasSuffix(upstreamPath, "/v1") {
		return path
	}
	if path == "/v1" {
		return "/"
	}
	if strings.HasPrefix(path, "/v1/") {
		return strings.TrimPrefix(path, "/v1")
	}
	return path
}

// APIKeyProviderForName resolves a provider named on the CLI or in an API
// payload, accepting the canonical name and any registered alias. Exported so
// the CLI does not carry a second copy of the alias list.
func APIKeyProviderForName(name string) (accounts.Provider, bool) {
	entry, ok := keyedProviderForName(name)
	if !ok {
		return "", false
	}
	return entry.Provider, true
}

// APIKeyProviderList renders the providers a user may name, for flag help and
// for the error returned when they name something else.
func APIKeyProviderList() string {
	names := append([]string{"codex", "claude"}, keyedProviderNames()...)
	if len(names) == 1 {
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + ", or " + names[len(names)-1]
}
