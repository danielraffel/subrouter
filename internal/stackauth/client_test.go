package stackauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNativeCLIFlowAndRefresh(t *testing.T) {
	var sawStackHeaders bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Stack-Project-Id") == "project" &&
			r.Header.Get("X-Hexclave-Project-Id") == "project" {
			sawStackHeaders = true
		}
		switch r.URL.Path {
		case "/auth/cli":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"polling_code": "poll",
				"login_code":   "login",
				"expires_at":   time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
			})
		case "/auth/cli/poll":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status":        "success",
				"refresh_token": "refresh",
			})
		case "/auth/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("client_id") != "project" {
				t.Fatalf("bad refresh form: %v", r.Form)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"access_token":  "access",
				"refresh_token": "rotated",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := Client{
		APIURL:               server.URL,
		ProjectID:            "project",
		PublishableClientKey: "pck_test",
		HTTPClient:           server.Client(),
	}
	start, err := client.StartCLI(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if start.PollingCode != "poll" || start.LoginCode != "login" {
		t.Fatalf("start = %#v", start)
	}
	poll, err := client.PollCLI(context.Background(), start.PollingCode)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := client.Refresh(context.Background(), poll.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if tokens.AccessToken != "access" || tokens.RefreshToken != "rotated" {
		t.Fatalf("tokens = %#v", tokens)
	}
	if !sawStackHeaders {
		t.Fatal("native Stack/Hexclave client headers were not sent")
	}
}

func TestFetchPublicConfigRejectsIncompleteResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":1}`))
	}))
	defer server.Close()
	_, err := FetchPublicConfig(context.Background(), server.Client(), server.URL)
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("err = %v", err)
	}
}
