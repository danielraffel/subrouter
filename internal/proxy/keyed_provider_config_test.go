package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/session"
)

func resetConfiguredProviders(t *testing.T) {
	t.Helper()
	configuredMu.Lock()
	configuredProviders = nil
	configuredFrozen = false
	configuredMu.Unlock()
	t.Cleanup(func() {
		configuredMu.Lock()
		configuredProviders = nil
		configuredFrozen = false
		configuredMu.Unlock()
	})
}

// A declared provider must route end to end without any code shipping for it:
// that is the whole point of declaring one.
func TestConfiguredProviderRoutesEndToEnd(t *testing.T) {
	resetConfiguredProviders(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("upstream path = %q, want /v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer declared-key" {
			t.Fatalf("Authorization = %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	if err := ConfigureOpenAICompatibleProviders([]OpenAICompatibleProvider{
		{Name: "acme-relay", Aliases: []string{"acme"}, BaseURL: upstream.URL + "/v1"},
	}); err != nil {
		t.Fatalf("configure failed: %v", err)
	}

	store, err := session.NewStore(t.TempDir() + "/sessions.json")
	if err != nil {
		t.Fatal(err)
	}
	server := Server{
		Accounts: []accounts.Account{{
			ID: "acme-relay:main", Provider: accounts.Provider("acme-relay"),
			AuthMode: accounts.AuthModeAPIKey, Token: "declared-key",
		}},
		Sessions:     store,
		MaxBodyBytes: 1024,
	}
	req := httptest.NewRequest(http.MethodPost, "/acme-relay/v1/chat/completions", strings.NewReader(`{"model":"qwen3-max"}`))
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}

	// Its aliases and lookups behave like a built-in entry's.
	for _, name := range []string{"acme-relay", "acme"} {
		if got, ok := keyedProviderForName(name); !ok || string(got.Provider) != "acme-relay" {
			t.Fatalf("keyedProviderForName(%q) did not resolve", name)
		}
	}
	if !isKeyedProvider(accounts.Provider("acme-relay")) {
		t.Fatal("a declared provider must be a keyed provider")
	}
}

// A declared provider must not be able to shadow Codex, Claude, or a built-in,
// which would silently redirect traffic that already had a home.
func TestConfigureRejectsShadowingAnExistingProvider(t *testing.T) {
	resetConfiguredProviders(t)
	cases := []struct {
		name     string
		declared OpenAICompatibleProvider
	}{
		{name: "reserved provider name", declared: OpenAICompatibleProvider{Name: "claude", BaseURL: "https://x.test/v1"}},
		{name: "reserved path segment", declared: OpenAICompatibleProvider{Name: "v1", BaseURL: "https://x.test/v1"}},
		{name: "built-in name", declared: OpenAICompatibleProvider{Name: "kimi", BaseURL: "https://x.test/v1"}},
		{name: "built-in alias", declared: OpenAICompatibleProvider{Name: "fine", Aliases: []string{"glm"}, BaseURL: "https://x.test/v1"}},
		{name: "reserved alias", declared: OpenAICompatibleProvider{Name: "fine", Aliases: []string{"anthropic"}, BaseURL: "https://x.test/v1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ConfigureOpenAICompatibleProviders([]OpenAICompatibleProvider{tc.declared}); err == nil {
				t.Fatal("declaration should have been rejected")
			}
		})
	}
}

func TestConfigureRejectsMalformedDeclarations(t *testing.T) {
	resetConfiguredProviders(t)
	cases := []struct {
		name     string
		declared OpenAICompatibleProvider
	}{
		{name: "no name", declared: OpenAICompatibleProvider{BaseURL: "https://x.test/v1"}},
		{name: "no base url", declared: OpenAICompatibleProvider{Name: "thing"}},
		{name: "name with a slash", declared: OpenAICompatibleProvider{Name: "a/b", BaseURL: "https://x.test/v1"}},
		{name: "non-http scheme", declared: OpenAICompatibleProvider{Name: "thing", BaseURL: "ftp://x.test/v1"}},
		{name: "no host", declared: OpenAICompatibleProvider{Name: "thing", BaseURL: "https:///v1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ConfigureOpenAICompatibleProviders([]OpenAICompatibleProvider{tc.declared}); err == nil {
				t.Fatal("declaration should have been rejected")
			}
		})
	}
	if err := ConfigureOpenAICompatibleProviders([]OpenAICompatibleProvider{
		{Name: "dupe", BaseURL: "https://a.test/v1"},
		{Name: "dupe", BaseURL: "https://b.test/v1"},
	}); err == nil {
		t.Fatal("two declarations of the same name should have been rejected")
	}
}

// Configuring replaces rather than accumulates, so a caller invoked twice does
// not end up with duplicate entries.
func TestConfigureReplacesPreviousDeclarations(t *testing.T) {
	resetConfiguredProviders(t)
	if err := ConfigureOpenAICompatibleProviders([]OpenAICompatibleProvider{{Name: "first", BaseURL: "https://a.test/v1"}}); err != nil {
		t.Fatal(err)
	}
	if err := ConfigureOpenAICompatibleProviders([]OpenAICompatibleProvider{{Name: "second", BaseURL: "https://b.test/v1"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := keyedProviderForName("first"); ok {
		t.Fatal("the earlier declaration should have been replaced")
	}
	if _, ok := keyedProviderForName("second"); !ok {
		t.Fatal("the current declaration should resolve")
	}
	if err := ConfigureOpenAICompatibleProviders([]OpenAICompatibleProvider{{Name: "second", BaseURL: "https://b.test/v1"}}); err != nil {
		t.Fatalf("re-applying the same declaration must be accepted: %v", err)
	}
	// A failed declaration must not disturb what is already configured.
	if err := ConfigureOpenAICompatibleProviders([]OpenAICompatibleProvider{{Name: "claude", BaseURL: "https://c.test/v1"}}); err == nil {
		t.Fatal("expected rejection")
	}
	if _, ok := keyedProviderForName("second"); !ok {
		t.Fatal("a rejected declaration must leave the previous configuration intact")
	}
}

func TestConfigureRejectsChangesAfterHandlerStarts(t *testing.T) {
	resetConfiguredProviders(t)
	if err := ConfigureOpenAICompatibleProviders([]OpenAICompatibleProvider{{Name: "first", BaseURL: "https://a.test/v1"}}); err != nil {
		t.Fatal(err)
	}
	_ = (Server{}).Handler()
	if err := ConfigureOpenAICompatibleProviders([]OpenAICompatibleProvider{{Name: "second", BaseURL: "https://b.test/v1"}}); err == nil {
		t.Fatal("configuration after Handler starts must be rejected")
	}
}

func TestParseOpenAICompatibleFlag(t *testing.T) {
	declared, err := ParseOpenAICompatibleFlag("acme-relay|acme=https://coding.example/v1")
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if declared.Name != "acme-relay" {
		t.Fatalf("Name = %q", declared.Name)
	}
	if len(declared.Aliases) != 1 || declared.Aliases[0] != "acme" {
		t.Fatalf("Aliases = %v", declared.Aliases)
	}
	if declared.BaseURL != "https://coding.example/v1" {
		t.Fatalf("BaseURL = %q", declared.BaseURL)
	}
	// A base URL containing '=' still parses, because only the first is a
	// separator.
	if declared, err = ParseOpenAICompatibleFlag("x=https://h.test/v1?a=b"); err != nil || declared.BaseURL != "https://h.test/v1?a=b" {
		t.Fatalf("query strings must survive: %+v %v", declared, err)
	}
	if _, err := ParseOpenAICompatibleFlag("no-equals-sign"); err == nil {
		t.Fatal("a declaration without = must be rejected")
	}
}

// A vendor that exposes one subscription on two protocol endpoints must not
// require the key to be stored twice. Registering it once against the owning
// provider has to serve both entries, or rotating the key means editing every
// copy and account listings double-count one subscription.
func TestOneSubscriptionServesBothProtocolEntries(t *testing.T) {
	if got := accountProviderFor(accounts.ProviderQwenAnthropic); got != accounts.ProviderQwenToken {
		t.Fatalf("accountProviderFor(qwen-anthropic) = %q, want qwen-token", got)
	}
	if got := accountProviderFor(accounts.ProviderQwenToken); got != accounts.ProviderQwenToken {
		t.Fatalf("a provider that owns its accounts must resolve to itself, got %q", got)
	}
	if got := accountProviderFor(accounts.ProviderCodex); got != accounts.ProviderCodex {
		t.Fatalf("a non-registry provider must resolve to itself, got %q", got)
	}

	// One stored account, under the owning provider only.
	stored := []accounts.Account{{
		ID: "qwen-token:main", Provider: accounts.ProviderQwenToken,
		AuthMode: accounts.AuthModeAPIKey, Token: "sk-sp-one",
	}}
	for _, provider := range []accounts.Provider{accounts.ProviderQwenToken, accounts.ProviderQwenAnthropic} {
		got := filterAccountsForProvider(stored, provider)
		if len(got) != 1 || got[0].Token != "sk-sp-one" {
			t.Fatalf("provider %q selected %d accounts, want the single shared credential", provider, len(got))
		}
		// The selected account must carry the REQUESTED provider, since
		// upstream selection and path rewriting key on it. Carrying the
		// credential owner's provider routes the Anthropic endpoint through the
		// OpenAI upstream and 404s.
		if got[0].Provider != provider {
			t.Fatalf("selected account carries provider %q, want the requested %q", got[0].Provider, provider)
		}
		if got[0].ID != "qwen-token:main" {
			t.Fatalf("account id changed to %q; stickiness and forced selection key on it", got[0].ID)
		}
	}
	// A credential stored against the protocol endpoint rather than the owning
	// provider must still be found, or a key added that way is silently
	// orphaned.
	underEndpoint := []accounts.Account{{
		ID: "qwen-anthropic:main", Provider: accounts.ProviderQwenAnthropic,
		AuthMode: accounts.AuthModeAPIKey, Token: "sk-sp-endpoint",
	}}
	for _, provider := range []accounts.Provider{accounts.ProviderQwenAnthropic, accounts.ProviderQwenToken} {
		if got := filterAccountsForProvider(underEndpoint, provider); len(got) != 1 {
			t.Fatalf("provider %q found %d accounts stored under the endpoint name, want 1", provider, len(got))
		}
	}

	// Stamping must not mutate the caller's slice.
	if stored[0].Provider != accounts.ProviderQwenToken {
		t.Fatalf("the stored account was mutated to %q", stored[0].Provider)
	}

	// Sharing must not leak across unrelated providers.
	if got := filterAccountsForProvider(stored, accounts.ProviderGrok); len(got) != 0 {
		t.Fatalf("grok selected %d qwen accounts, want none", len(got))
	}
	if got := filterAccountsForProvider(stored, accounts.ProviderCodex); len(got) != 0 {
		t.Fatalf("codex selected %d qwen accounts, want none", len(got))
	}
}
