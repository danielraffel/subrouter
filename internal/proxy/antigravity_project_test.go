package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestRewriteAntigravityProjectOnlyTopLevel(t *testing.T) {
	body := []byte(`{"project":"project-a","request":{"project":"nested-a","contents":[]}}`)
	rewritten, changed, err := rewriteAntigravityProject(body, "project-b")
	if err != nil || !changed {
		t.Fatalf("rewrite = changed=%v err=%v", changed, err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(rewritten, &envelope); err != nil {
		t.Fatal(err)
	}
	var project, nested string
	_ = json.Unmarshal(envelope["project"], &project)
	var request map[string]json.RawMessage
	_ = json.Unmarshal(envelope["request"], &request)
	_ = json.Unmarshal(request["project"], &nested)
	if project != "project-b" || nested != "nested-a" {
		t.Fatalf("project=%q nested=%q", project, nested)
	}
}

func TestAntigravityProjectDiscoveryIsBoundToAccount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:loadCodeAssist" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		project := "project-a"
		if r.Header.Get("Authorization") == "Bearer token-b" {
			project = "project-b"
		}
		_, _ = io.WriteString(w, `{"cloudaicompanionProject":{"id":"`+project+`"}}`)
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	s := &Server{}
	a := accounts.Account{ID: "agy:a", Token: "token-a"}
	b := accounts.Account{ID: "agy:b", Token: "token-b"}
	pa, err := s.antigravityProject(context.Background(), a, base)
	if err != nil || pa != "project-a" {
		t.Fatalf("a project=%q err=%v", pa, err)
	}
	pb, err := s.antigravityProject(context.Background(), b, base)
	if err != nil || pb != "project-b" {
		t.Fatalf("b project=%q err=%v", pb, err)
	}
	if strings.Contains(pa, pb) {
		t.Fatalf("account projects unexpectedly shared: %q %q", pa, pb)
	}
}

func TestRewriteAntigravityProjectFailsClosedOnMalformedOrInvalidEnvelope(t *testing.T) {
	for _, body := range [][]byte{[]byte("not-json"), []byte(`{"project":7}`)} {
		if _, _, err := rewriteAntigravityProject(body, "project-b"); err == nil {
			t.Fatalf("body %s did not fail", body)
		}
	}
	if body, changed, err := rewriteAntigravityProject([]byte(`{"request":{}}`), "project-b"); err != nil || changed || string(body) != `{"request":{}}` {
		t.Fatalf("missing project = %s changed=%v err=%v", body, changed, err)
	}
}
