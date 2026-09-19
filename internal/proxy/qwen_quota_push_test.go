package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	agentqwen "github.com/manaflow-ai/subrouter/internal/agents/qwen"
)

func TestQwenQuotaPushStoreRoundTrip(t *testing.T) {
	store := newQwenQuotaPushStore(filepath.Join(t.TempDir(), "qwen-quota-push.json"))
	windows := []accounts.UsageWindow{{Name: "5h", UsedPercent: 37, LimitWindowSeconds: 18000}}
	if err := store.set("user@example.com", windows, "push"); err != nil {
		t.Fatal(err)
	}
	record, ok := store.record("user@example.com")
	if !ok || len(record.Windows) != 1 || record.Windows[0].UsedPercent != 37 {
		t.Fatalf("record = %+v, %v", record, ok)
	}
	info, err := os.Stat(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("store file mode = %o", info.Mode().Perm())
	}

	stale := qwenQuotaPushFile{Accounts: map[string]qwenQuotaPushRecord{
		"old@example.com": {Windows: windows, FetchedAt: time.Now().Add(-2 * qwenQuotaPushTTL)},
	}}
	data, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.record("old@example.com"); ok {
		t.Fatal("stale record was served")
	}
}

func TestQwenQuotaPushEndpointAuthAndValidation(t *testing.T) {
	codexDir := t.TempDir()
	ref := &AccountRef{
		store:       accounts.CodexStore{Dir: codexDir},
		claudeStore: agentclaude.Store{Dir: t.TempDir()},
	}
	handler := Server{AccountRef: ref, AdminToken: "admin"}.Handler()

	post := func(auth, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/_subrouter/qwen-quota", bytes.NewBufferString(body))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		return resp
	}

	valid := `{"email":"user@example.com","windows":[{"Name":"5h","UsedPercent":37,"LimitWindowSeconds":18000}]}`
	if resp := post("", valid); resp.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: status = %d", resp.Code)
	}
	if resp := post("Bearer admin", `{"email":"not-an-email","windows":[{"Name":"5h","UsedPercent":37}]}`); resp.Code != http.StatusBadRequest {
		t.Fatalf("bad email: status = %d", resp.Code)
	}
	if resp := post("Bearer admin", `{"email":"user@example.com","windows":[]}`); resp.Code != http.StatusBadRequest {
		t.Fatalf("empty windows: status = %d", resp.Code)
	}
	if resp := post("Bearer admin", `{"email":"user@example.com","windows":[{"Name":"5h","UsedPercent":137}]}`); resp.Code != http.StatusBadRequest {
		t.Fatalf("over-100 window: status = %d", resp.Code)
	}
	if resp := post("Bearer admin", valid); resp.Code != http.StatusOK {
		t.Fatalf("valid push: status = %d body = %s", resp.Code, resp.Body.String())
	}
	store := newQwenQuotaPushStore(filepath.Join(codexDir, "qwen-quota-push.json"))
	record, ok := store.record("user@example.com")
	if !ok || len(record.Windows) != 1 || record.Windows[0].UsedPercent != 37 {
		t.Fatalf("stored record = %+v, %v", record, ok)
	}
}

func TestQwenQuotaPushMergesIntoUsageStatus(t *testing.T) {
	store := accounts.CodexStore{Dir: t.TempDir()}
	stored, _, err := store.AddAPIKeyForProvider("work", "sk-sp-test", accounts.ProviderQwenToken)
	if err != nil {
		t.Fatal(err)
	}
	// The console credential dir gets only identity metadata, no token, so the
	// console path reports "login needed" without any network.
	qwenRoot := agentqwen.ConsoleRootForStore(store)
	consoleDir := agentqwen.ConsoleConfigDirIn(qwenRoot, stored.Email)
	if err := os.MkdirAll(consoleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(consoleDir, "metadata.json"), []byte(`{"account":"user@example.com"}`), 0600); err != nil {
		t.Fatal(err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer upstream.Close()
	ref := NewAccountRef(store, nil, upstream.Client())
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	handler := Server{
		AccountRef:        ref,
		AdminToken:        "admin",
		QwenTokenUpstream: mustParseURL(t, upstream.URL+"/api/v1"),
	}.Handler()

	push := httptest.NewRequest(http.MethodPost, "/_subrouter/qwen-quota", bytes.NewBufferString(
		`{"email":"user@example.com","windows":[{"Name":"5h","UsedPercent":37,"LimitWindowSeconds":18000},{"Name":"7d","UsedPercent":12,"LimitWindowSeconds":604800}]}`))
	push.Header.Set("Authorization", "Bearer admin")
	pushResp := httptest.NewRecorder()
	handler.ServeHTTP(pushResp, push)
	if pushResp.Code != http.StatusOK {
		t.Fatalf("push status = %d body = %s", pushResp.Code, pushResp.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/_subrouter/usage-status", nil)
	req.Header.Set("Authorization", "Bearer admin")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("usage-status = %d body = %s", resp.Code, resp.Body.String())
	}
	var statuses []AccountUsageStatus
	if err := json.Unmarshal(resp.Body.Bytes(), &statuses); err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 {
		t.Fatalf("statuses = %+v", statuses)
	}
	got := statuses[0]
	if got.QuotaStatus != "live" || !got.QuotaUsageKnown || len(got.Windows) != 2 || got.Windows[0].UsedPercent != 37 {
		t.Fatalf("merged status = %+v", got)
	}
}
