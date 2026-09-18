package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestProbeProviderKeyDoesNotForwardCredentialsAcrossRedirects(t *testing.T) {
	var redirectTargetHit atomic.Bool
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectTargetHit.Store(true)
	}))
	defer redirectTarget.Close()

	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer test-secret" {
			t.Error("provider probe omitted its credential")
		}
		http.Redirect(w, request, redirectTarget.URL, http.StatusFound)
	}))
	defer provider.Close()

	state, models := ProbeProviderKey(context.Background(), provider.Client(), accounts.ProviderKimi, provider.URL, "test-secret")
	if state != "http 302" || models != -1 {
		t.Fatalf("redirect response classified as state=%q models=%d", state, models)
	}
	if redirectTargetHit.Load() {
		t.Fatal("provider credential probe followed a redirect")
	}
}

func TestProbeOpenRouterKeyStatus(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		body          string
		creditsStatus int
		creditsBody   string
		wantState     string
		wantQuota     string
		wantKnown     bool
		wantUsed      float64
		wantBalance   string
	}{
		{name: "finite monthly limit", status: http.StatusOK, body: `{"data":{"limit":100,"limit_remaining":74.5,"limit_reset":"monthly"}}`, wantState: "auth ok", wantQuota: "live", wantKnown: true, wantUsed: 25.5, wantBalance: "74.5"},
		{name: "account balance from credits", status: http.StatusOK, body: `{"data":{"limit":40,"limit_remaining":40,"limit_reset":"monthly"}}`, creditsStatus: http.StatusOK, creditsBody: `{"data":{"total_credits":45,"total_usage":36.904185575}}`, wantState: "auth ok", wantQuota: "live", wantKnown: true, wantUsed: 0, wantBalance: "8.1"},
		{name: "credits endpoint fails falls back to cap", status: http.StatusOK, body: `{"data":{"limit":100,"limit_remaining":74.5,"limit_reset":"monthly"}}`, creditsStatus: http.StatusInternalServerError, wantState: "auth ok", wantQuota: "live", wantKnown: true, wantUsed: 25.5, wantBalance: "74.5"},
		{name: "credits overuse clamps to zero", status: http.StatusOK, body: `{"data":{"limit":100,"limit_remaining":74.5,"limit_reset":"monthly"}}`, creditsStatus: http.StatusOK, creditsBody: `{"data":{"total_credits":10,"total_usage":12.5}}`, wantState: "auth ok", wantQuota: "live", wantKnown: true, wantUsed: 25.5, wantBalance: "0"},
		{name: "unlimited key", status: http.StatusOK, body: `{"data":{"limit":null,"limit_remaining":null,"limit_reset":null}}`, wantState: "auth ok"},
		{name: "exhausted", status: http.StatusOK, body: `{"data":{"limit":10,"limit_remaining":0,"limit_reset":"weekly"}}`, wantState: "auth ok", wantQuota: "exhausted", wantKnown: true, wantUsed: 100, wantBalance: "0"},
		{name: "zero limit", status: http.StatusOK, body: `{"data":{"limit":0,"limit_remaining":0,"limit_reset":"daily"}}`, wantState: "auth ok", wantQuota: "exhausted", wantKnown: true, wantUsed: 100, wantBalance: "0"},
		{name: "optional fields missing", status: http.StatusOK, body: `{"data":{}}`, wantState: "auth ok"},
		{name: "bad key", status: http.StatusUnauthorized, body: `{}`, wantState: "bad key"},
		{name: "provider unavailable", status: http.StatusInternalServerError, body: `{}`, wantState: "http 500"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var creditsHits atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Header.Get("Authorization") != "Bearer test-openrouter-key" {
					t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
				}
				switch request.URL.Path {
				case "/api/v1/key":
					w.WriteHeader(test.status)
					_, _ = io.WriteString(w, test.body)
				case "/api/v1/credits":
					creditsHits.Add(1)
					if test.creditsStatus == 0 {
						http.NotFound(w, request)
						return
					}
					w.WriteHeader(test.creditsStatus)
					_, _ = io.WriteString(w, test.creditsBody)
				default:
					t.Errorf("probe path = %q, want /api/v1/key or /api/v1/credits", request.URL.Path)
				}
			}))
			defer provider.Close()

			probe := ProbeProviderKeyStatus(context.Background(), provider.Client(), accounts.ProviderOpenRouter, provider.URL+"/api/v1", "test-openrouter-key")
			if probe.State != test.wantState || probe.QuotaStatus != test.wantQuota || probe.QuotaUsageKnown != test.wantKnown {
				t.Fatalf("probe = %+v", probe)
			}
			// The credits call happens exactly once, and only after the key
			// auth succeeded.
			wantCreditsHits := 0
			if test.wantState == "auth ok" {
				wantCreditsHits = 1
			}
			if got := int(creditsHits.Load()); got != wantCreditsHits {
				t.Fatalf("credits hits = %d, want %d", got, wantCreditsHits)
			}
			if !test.wantKnown {
				if len(probe.Windows) != 0 || probe.Credits != nil {
					t.Fatalf("unknown quota fabricated usage: %+v", probe)
				}
				return
			}
			if len(probe.Windows) != 1 || probe.Windows[0].UsedPercent != test.wantUsed {
				t.Fatalf("windows = %+v, want %.1f%% used", probe.Windows, test.wantUsed)
			}
			if probe.Credits == nil || probe.Credits.Balance != test.wantBalance {
				t.Fatalf("credits = %+v, want remaining %s", probe.Credits, test.wantBalance)
			}
			if test.name == "finite monthly limit" && (probe.Windows[0].Name != "monthly" || probe.Windows[0].LimitWindowSeconds != int64((30*24*time.Hour)/time.Second)) {
				t.Fatalf("monthly window = %+v", probe.Windows[0])
			}
		})
	}
}

func TestDecodeOpenRouterKeyProbeRejectsNonFiniteQuota(t *testing.T) {
	// JSON itself cannot encode NaN or infinity. This malformed numeric token
	// must therefore degrade to auth-only health instead of inventing usage.
	probe := decodeOpenRouterKeyProbe(ProviderKeyProbe{State: "auth ok"}, strings.NewReader(`{"data":{"limit":1e999,"limit_remaining":1}}`))
	if probe.State != "auth ok" || probe.QuotaUsageKnown || len(probe.Windows) != 0 || probe.Credits != nil {
		t.Fatalf("malformed quota was accepted: %+v", probe)
	}
}
