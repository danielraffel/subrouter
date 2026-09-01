package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

func TestHealthReportsAccountImportState(t *testing.T) {
	for _, tc := range []struct {
		name        string
		adminToken  string
		importToken string
		want        string
	}{
		{name: "no credential configured", want: AccountImportDisabled},
		{name: "admin token", adminToken: "secret", want: AccountImportEnabled},
		{name: "import token", importToken: "secret", want: AccountImportEnabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := accounts.CodexStore{Dir: t.TempDir()}
			ref := NewAccountRef(store, nil, nil)
			ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
			handler := Server{
				AccountRef:         ref,
				AdminToken:         tc.adminToken,
				AccountImportToken: tc.importToken,
			}.Handler()

			req := httptest.NewRequest(http.MethodGet, "/_subrouter/health", nil)
			challenge := "0000000000000000000000000000000000000000000000000000000000000000"
			req.Header.Set(accounts.StoreAuthorityChallengeHeader, challenge)
			req.RemoteAddr = "100.64.0.20:4321"
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, req)

			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.Code)
			}
			var body struct {
				OK                bool   `json:"ok"`
				AccountImport     string `json:"account_import"`
				AccountStoreID    string `json:"account_store_id"`
				AccountStoreProof string `json:"account_store_proof"`
			}
			if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode health: %v (%s)", err, resp.Body.String())
			}
			if !body.OK {
				t.Fatalf("health ok = false, body = %s", resp.Body.String())
			}
			if body.AccountImport != tc.want {
				t.Fatalf("account_import = %q, want %q", body.AccountImport, tc.want)
			}
			wantStoreID, err := accounts.StoreAuthorityID(store.Dir)
			if err != nil {
				t.Fatal(err)
			}
			if body.AccountStoreID != wantStoreID {
				t.Fatalf("account_store_id = %q, want %q", body.AccountStoreID, wantStoreID)
			}
			wantProof, err := accounts.StoreAuthorityProof(store.Dir, challenge)
			if err != nil {
				t.Fatal(err)
			}
			if body.AccountStoreProof != wantProof {
				t.Fatalf("account_store_proof = %q, want %q", body.AccountStoreProof, wantProof)
			}
		})
	}
}

func TestHealthDoesNotPublishStoreIdentityWithoutAValidChallenge(t *testing.T) {
	store := accounts.CodexStore{Dir: t.TempDir()}
	ref := NewAccountRef(store, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	handler := Server{AccountRef: ref}.Handler()

	for _, challenge := range []string{"", "not-a-valid-challenge"} {
		req := httptest.NewRequest(http.MethodGet, "/_subrouter/health", nil)
		if challenge != "" {
			req.Header.Set(accounts.StoreAuthorityChallengeHeader, challenge)
		}
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("challenge %q status = %d, want 200", challenge, resp.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"account_store_id", "account_store_proof"} {
			if _, ok := body[field]; ok {
				t.Fatalf("challenge %q exposed %s: %s", challenge, field, resp.Body.String())
			}
		}
	}
}

// A disabled report must mean the endpoint actually rejects imports, so the
// health field and the authorization rule cannot drift apart.
func TestHealthAccountImportStateMatchesAuthorization(t *testing.T) {
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	server := Server{AccountRef: ref}
	if server.AccountImportState() != AccountImportDisabled {
		t.Fatalf("state = %q, want %q", server.AccountImportState(), AccountImportDisabled)
	}

	req := httptest.NewRequest(http.MethodGet, "/_subrouter/account-import", nil)
	req.RemoteAddr = "100.64.0.20:4321"
	resp := httptest.NewRecorder()
	server.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("import status = %d, want 401 while state reports disabled", resp.Code)
	}
}
