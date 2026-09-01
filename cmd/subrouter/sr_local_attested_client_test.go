package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

type localAttestationConnectionKey struct{}

func TestLocalStoreAttestedClientAttestsEveryConnectionBeforeRequest(t *testing.T) {
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	var nextConnection atomic.Int64
	var mu sync.Mutex
	requests := map[int64][]string{}
	var healthAuthorization atomic.Value

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		connectionID, _ := request.Context().Value(localAttestationConnectionKey{}).(int64)
		mu.Lock()
		requests[connectionID] = append(requests[connectionID], request.URL.Path)
		mu.Unlock()
		switch request.URL.Path {
		case "/_subrouter/health":
			if authorization := request.Header.Get("Authorization"); authorization != "" {
				healthAuthorization.Store(authorization)
			}
			writeLocalStoreAuthorityHealth(t, w, request, store, "enabled")
		case "/_subrouter/accounts":
			if request.Header.Get("Authorization") != "Bearer after-attestation" {
				http.Error(w, "missing test authorization", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Connection", "close")
			_, _ = io.WriteString(w, `[]`)
		default:
			http.NotFound(w, request)
		}
	}))
	server.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		return context.WithValue(ctx, localAttestationConnectionKey{}, nextConnection.Add(1))
	}
	server.Start()
	defer server.Close()

	client, err := newLocalStoreAttestedClient(server.Client(), server.URL, store)
	if err != nil {
		t.Fatal(err)
	}
	transport := client.Transport.(*http.Transport)
	for iteration := 0; iteration < 2; iteration++ {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/_subrouter/accounts", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer after-attestation")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		transport.CloseIdleConnections()
	}

	if authorization, _ := healthAuthorization.Load().(string); authorization != "" {
		t.Fatalf("store attestation carried Authorization %q", authorization)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("connection request sequences = %#v, want two connections", requests)
	}
	for connectionID, sequence := range requests {
		if got := strings.Join(sequence, ","); got != "/_subrouter/health,/_subrouter/accounts" {
			t.Fatalf("connection %d request sequence = %q, want health before accounts", connectionID, got)
		}
	}
}

func TestLocalStoreAttestedClientReattestsBeforeCredentialAfterConnectionClose(t *testing.T) {
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	var validProof atomic.Bool
	validProof.Store(true)
	var protectedRequests atomic.Int32
	var credentialOnRejectedConnection atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/_subrouter/health" {
			if request.Header.Get("Authorization") != "" && !validProof.Load() {
				credentialOnRejectedConnection.Store(true)
			}
			if validProof.Load() {
				writeLocalStoreAuthorityHealth(t, w, request, store, "enabled")
			} else {
				_, _ = io.WriteString(w, `{"ok":true,"account_store_id":"replacement","account_store_proof":"wrong"}`)
			}
			return
		}
		protectedRequests.Add(1)
		if request.Header.Get("Authorization") != "Bearer protected" {
			http.Error(w, "missing credential", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Connection", "close")
		_, _ = io.WriteString(w, `[]`)
	}))
	defer server.Close()

	client, err := newLocalStoreAttestedClient(server.Client(), server.URL, store)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/_subrouter/accounts", nil)
	request.Header.Set("Authorization", "Bearer protected")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	validProof.Store(false)
	client.Transport.(*http.Transport).CloseIdleConnections()
	request, _ = http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/_subrouter/accounts", nil)
	request.Header.Set("Authorization", "Bearer protected")
	if _, err := client.Do(request); err == nil || !strings.Contains(err.Error(), "account store does not match") {
		t.Fatalf("replacement-listener error = %v, want store mismatch", err)
	}
	if credentialOnRejectedConnection.Load() {
		t.Fatal("replacement listener received a credential during attestation")
	}
	if protectedRequests.Load() != 1 {
		t.Fatalf("protected requests = %d, want only the pre-replacement request", protectedRequests.Load())
	}
}

func TestLocalStoreAttestedClientRejectsNonPersistentHealth(t *testing.T) {
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	var protectedRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/_subrouter/health" {
			w.Header().Set("Connection", "close")
			writeLocalStoreAuthorityHealth(t, w, request, store, "enabled")
			return
		}
		protectedRequests.Add(1)
		_, _ = io.WriteString(w, `[]`)
	}))
	defer server.Close()

	client, err := newLocalStoreAttestedClient(server.Client(), server.URL, store)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/_subrouter/accounts", nil)
	request.Header.Set("Authorization", "Bearer must-not-be-sent")
	if _, err := client.Do(request); err == nil || !strings.Contains(err.Error(), "did not preserve its connection") {
		t.Fatalf("non-persistent attestation error = %v", err)
	}
	if protectedRequests.Load() != 0 {
		t.Fatalf("non-persistent listener received %d protected request(s)", protectedRequests.Load())
	}
}

func TestReadyLocalServingServerDoesNotLoadOrSendAdminToken(t *testing.T) {
	home := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(home, "accounts")}
	var unexpectedAuthorization atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if authorization := request.Header.Get("Authorization"); authorization != "" {
			unexpectedAuthorization.Store(authorization)
		}
		switch request.URL.Path {
		case "/_subrouter/health":
			writeLocalStoreAuthorityHealth(t, w, request, store, "disabled")
		case "/_subrouter/accounts":
			_, _ = io.WriteString(w, `[]`)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", server.URL)
	t.Setenv("SUBROUTER_ADMIN_TOKEN", "environment-admin-must-not-load")
	t.Setenv("SUBROUTER_ADMIN_TOKEN_FILE", filepath.Join(home, "missing-admin-token"))
	if err := defaultSRServerStore(store).update(func(file *srServerFile) error {
		file.Servers = []srServerConfig{{Name: "matching", URL: server.URL, AdminToken: "registry-admin-must-not-load"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	runner := srRunner{store: store, client: server.Client(), out: io.Discard, errOut: io.Discard}
	local, err := runner.readyLocalServingServer(t.Context(), func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if local.AdminToken != "" {
		t.Fatal("ready local server retained an administrator token")
	}
	if _, err := runner.fetchServerAccounts(t.Context(), local); err != nil {
		t.Fatal(err)
	}
	if authorization, _ := unexpectedAuthorization.Load().(string); authorization != "" {
		t.Fatalf("local control request sent Authorization %q", authorization)
	}
}

func writeLocalStoreAuthorityHealth(t *testing.T, w http.ResponseWriter, request *http.Request, store accounts.CodexStore, importState string) {
	t.Helper()
	authorityID, err := accounts.StoreAuthorityID(store.Dir)
	if err != nil {
		t.Error(err)
		http.Error(w, "authority failure", http.StatusInternalServerError)
		return
	}
	proof := ""
	if challenge := request.Header.Get(accounts.StoreAuthorityChallengeHeader); challenge != "" {
		proof, err = accounts.StoreAuthorityProof(store.Dir, challenge)
		if err != nil {
			t.Error(err)
			http.Error(w, "proof failure", http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"ok":true,"account_import":%q,"account_store_id":%q,"account_store_proof":%q}`, importState, authorityID, proof)
}
