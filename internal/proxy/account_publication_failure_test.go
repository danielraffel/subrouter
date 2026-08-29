package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	agentkimi "github.com/manaflow-ai/subrouter/internal/agents/kimi"
)

func publicationFailingAccountServer(t *testing.T) (Server, accounts.CodexStore, agentclaude.Store, agentkimi.Store, error) {
	t.Helper()
	root := t.TempDir()
	codexStore := accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}
	claudeStore := agentclaude.Store{Dir: filepath.Join(root, "claude")}
	kimiStore := agentkimi.Store{
		Path:       filepath.Join(root, "kimi", "cli.json"),
		ManagedDir: filepath.Join(root, "kimi", "managed"),
	}
	ref := NewAccountRef(codexStore, nil, nil)
	ref.claudeStore = claudeStore
	ref.oauthSources = []OAuthAccountSource{kimiStore}
	want := errors.New("generation publication unavailable")
	ref.publishGenerationForTest = func(string) error { return want }
	return Server{AccountRef: ref}, codexStore, claudeStore, kimiStore, want
}

func TestAccountImportPublicationFailurePreservesCredentials(t *testing.T) {
	t.Run("Codex OAuth repair", func(t *testing.T) {
		server, store, _, _, want := publicationFailingAccountServer(t)
		before := proxyStoredOAuthAccount("owner@example.com", "before", time.Now().Add(time.Hour))
		if err := store.SaveStored(before); err != nil {
			t.Fatal(err)
		}
		after := proxyStoredOAuthAccount(before.Email, "after", time.Now().Add(time.Hour))
		_, err := server.installImportedAccount(t.Context(), accountImportRequest{
			Provider: accounts.ProviderCodex,
			Codex:    &after,
		})
		if !errors.Is(err, want) {
			t.Fatalf("error = %v, want publication failure", err)
		}
		stored, ok, err := store.FindStored(before.Email)
		if err != nil || !ok {
			t.Fatalf("stored account = found:%v err:%v", ok, err)
		}
		if stored.Auth.Tokens == nil || stored.Auth.Tokens.RefreshToken != before.Auth.Tokens.RefreshToken {
			t.Fatal("publication failure changed the Codex refresh-token chain")
		}
	})

	t.Run("API key repair", func(t *testing.T) {
		server, store, _, _, want := publicationFailingAccountServer(t)
		before := accounts.StoredCodexAccount{
			Email:    "apikey:work",
			Provider: accounts.ProviderCodex,
			Auth:     accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "sk-before"},
		}
		if err := store.SaveStored(before); err != nil {
			t.Fatal(err)
		}
		after := before
		after.Auth.OpenAIAPIKey = "sk-after"
		_, err := server.installImportedAccount(t.Context(), accountImportRequest{
			Provider: accounts.ProviderCodex,
			Codex:    &after,
		})
		if !errors.Is(err, want) {
			t.Fatalf("error = %v, want publication failure", err)
		}
		stored, ok, err := store.FindStored(before.Email)
		if err != nil || !ok || stored.Auth.OpenAIAPIKey != before.Auth.OpenAIAPIKey {
			t.Fatalf("publication failure changed API-key state: found:%v err:%v", ok, err)
		}
	})

	t.Run("Claude OAuth repair", func(t *testing.T) {
		server, _, store, _, want := publicationFailingAccountServer(t)
		before := agentclaude.CredentialInfo{AccessToken: "before-access", RefreshToken: "before-refresh"}
		if err := store.ImportProfileCredential("work", before); err != nil {
			t.Fatal(err)
		}
		after := agentclaude.CredentialInfo{AccessToken: "after-access", RefreshToken: "after-refresh"}
		_, err := server.installImportedAccount(t.Context(), accountImportRequest{
			Provider: accounts.ProviderClaude,
			Claude:   &claudeAccountImport{Name: "work", Credential: after},
		})
		if !errors.Is(err, want) {
			t.Fatalf("error = %v, want publication failure", err)
		}
		profile, ok := store.FindProfile("work")
		if !ok {
			t.Fatal("Claude profile disappeared")
		}
		stored, err := store.ReadCredential(context.Background(), filepath.Join(store.InstancesDir(), profile.Dir))
		if err != nil || stored == nil || stored.RefreshToken != before.RefreshToken {
			t.Fatalf("publication failure changed Claude credential: present:%v err:%v", stored != nil, err)
		}
	})

	t.Run("Kimi OAuth repair", func(t *testing.T) {
		server, _, _, store, want := publicationFailingAccountServer(t)
		before := agentkimi.CredentialInfo{
			AccessToken: "before-access", RefreshToken: "before-refresh",
			OAuthDeviceID: "0123456789abcdef", ExpiresAt: time.Now().Add(time.Hour),
		}
		if _, err := store.SaveManagedCredential("work", before); err != nil {
			t.Fatal(err)
		}
		after := before
		after.AccessToken = "after-access"
		after.RefreshToken = "after-refresh"
		_, err := server.installImportedAccount(t.Context(), accountImportRequest{
			Provider: accounts.ProviderKimi,
			Kimi:     &kimiAccountImport{Label: "work", Credential: after},
		})
		if !errors.Is(err, want) {
			t.Fatalf("error = %v, want publication failure", err)
		}
		stored, ok, err := store.ReadManagedCredential("work", time.Now())
		if err != nil || !ok || stored.RefreshToken != before.RefreshToken {
			t.Fatalf("publication failure changed Kimi credential: found:%v err:%v", ok, err)
		}
	})

	t.Run("Kimi removal", func(t *testing.T) {
		server, _, _, store, want := publicationFailingAccountServer(t)
		before := agentkimi.CredentialInfo{
			AccessToken: "before-access", RefreshToken: "before-refresh",
			OAuthDeviceID: "0123456789abcdef", ExpiresAt: time.Now().Add(time.Hour),
		}
		if _, err := store.SaveManagedCredential("work", before); err != nil {
			t.Fatal(err)
		}
		_, err := server.installImportedAccount(t.Context(), accountImportRequest{
			Provider: accounts.ProviderKimi,
			Kimi:     &kimiAccountImport{Label: "work", Remove: true},
		})
		if !errors.Is(err, want) {
			t.Fatalf("error = %v, want publication failure", err)
		}
		stored, ok, err := store.ReadManagedCredential("work", time.Now())
		if err != nil || !ok || stored.RefreshToken != before.RefreshToken {
			t.Fatalf("publication failure removed Kimi credential: found:%v err:%v", ok, err)
		}
	})
}

func TestTenantAccountPublicationFailurePreservesCredentials(t *testing.T) {
	t.Run("Codex OAuth repair", func(t *testing.T) {
		server, store, _, _, _ := publicationFailingAccountServer(t)
		before := accounts.StoredCodexAccount{
			Email: "codex-owner", Label: "work", Provider: accounts.ProviderCodex,
			Auth: accounts.CodexAuthFile{AuthMode: "chatgpt", Tokens: &accounts.CodexTokens{
				AccessToken: "before-access", RefreshToken: "before-refresh", IDToken: "before-id",
			}},
		}
		if err := store.SaveStored(before); err != nil {
			t.Fatal(err)
		}
		response := serveTenantAccountUpload(&server, `{
			"provider":"codex","accountId":"codex-owner","label":"work",
			"tokens":{"accessToken":"after-access","refreshToken":"after-refresh","idToken":"after-id","accountID":"owner"}
		}`)
		if response.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		stored, ok, err := store.FindStored(before.Email)
		if err != nil || !ok || !reflect.DeepEqual(stored.Auth.Tokens, before.Auth.Tokens) {
			t.Fatalf("publication failure changed tenant Codex credentials: found:%v err:%v", ok, err)
		}
	})

	t.Run("API key repair", func(t *testing.T) {
		server, store, _, _, _ := publicationFailingAccountServer(t)
		before := accounts.StoredCodexAccount{
			Email: "apikey:openai-apikey:work", Label: "work", Provider: accounts.ProviderCodex,
			Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "sk-before"},
		}
		if err := store.SaveStored(before); err != nil {
			t.Fatal(err)
		}
		response := serveTenantAccountUpload(&server, `{"provider":"openai-apikey","label":"work","apiKey":"sk-after"}`)
		if response.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		stored, ok, err := store.FindStored(before.Email)
		if err != nil || !ok || stored.Auth.OpenAIAPIKey != before.Auth.OpenAIAPIKey {
			t.Fatalf("publication failure changed tenant API-key state: found:%v err:%v", ok, err)
		}
	})

	t.Run("Claude OAuth repair", func(t *testing.T) {
		server, _, store, _, _ := publicationFailingAccountServer(t)
		before := agentclaude.CredentialInfo{AccessToken: "before-access", RefreshToken: "before-refresh"}
		if _, err := store.UpsertCredentialProfile("work", before); err != nil {
			t.Fatal(err)
		}
		response := serveTenantAccountUpload(&server, `{
			"provider":"claude","label":"work",
			"claudeAiOauth":{"accessToken":"after-access","refreshToken":"after-refresh"}
		}`)
		if response.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		profile, ok := store.FindProfile("work")
		if !ok {
			t.Fatal("tenant Claude profile disappeared")
		}
		stored, err := store.ReadCredential(context.Background(), filepath.Join(store.InstancesDir(), profile.Dir))
		if err != nil || stored == nil || stored.RefreshToken != before.RefreshToken {
			t.Fatalf("publication failure changed tenant Claude credential: present:%v err:%v", stored != nil, err)
		}
	})
}

func TestTenantAccountRejectionDoesNotPublishGeneration(t *testing.T) {
	t.Run("repair target validation", func(t *testing.T) {
		server, _, _, _, _ := publicationFailingAccountServer(t)
		published := 0
		server.AccountRef.publishGenerationForTest = func(string) error {
			published++
			return nil
		}
		response := serveTenantAccountUpload(&server, `{
			"provider":"openai-apikey","label":"work","apiKey":"sk-after",
			"targetAccountID":"apikey:openai-apikey:other"
		}`)
		if response.Code != http.StatusConflict {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		if published != 0 {
			t.Fatalf("rejected repair published %d account generations", published)
		}
	})

	t.Run("capacity", func(t *testing.T) {
		server, store, _, _, _ := publicationFailingAccountServer(t)
		for index := 0; index < maxAccountImportAccounts; index++ {
			account := accounts.StoredCodexAccount{
				Email:    fmt.Sprintf("apikey:seed-%03d", index),
				Provider: accounts.ProviderCodex,
				Auth: accounts.CodexAuthFile{
					AuthMode: "apikey", OpenAIAPIKey: fmt.Sprintf("sk-seed-%03d", index),
				},
			}
			if err := store.SaveStored(account); err != nil {
				t.Fatal(err)
			}
		}
		published := 0
		server.AccountRef.publishGenerationForTest = func(string) error {
			published++
			return nil
		}
		response := serveTenantAccountUpload(&server, `{"provider":"openai-apikey","label":"over-limit","apiKey":"sk-new"}`)
		if response.Code != http.StatusInsufficientStorage {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		if published != 0 {
			t.Fatalf("capacity rejection published %d account generations", published)
		}
	})
}

func serveTenantAccountUpload(server *Server, body string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/_subrouter/accounts", strings.NewReader(body))
	handleTenantAccountUpload(server, response, request)
	return response
}
