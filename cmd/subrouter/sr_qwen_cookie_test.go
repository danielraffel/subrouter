package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// qwenCookieTestEnv isolates state and installs the cookie discovery +
// endpoint overrides; everything is restored on cleanup.
func qwenCookieTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SUBROUTER_STATE_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	oldDiscover := qwenDiscoverCookieSessions
	oldGateway := qwenCookieDataGateway
	oldDashboard := qwenCookieDashboardURL
	oldUserInfo := qwenCookieUserInfoURL
	t.Cleanup(func() {
		qwenDiscoverCookieSessions = oldDiscover
		qwenCookieDataGateway = oldGateway
		qwenCookieDashboardURL = oldDashboard
		qwenCookieUserInfoURL = oldUserInfo
	})
}

// qwenCookieTestHandler serves the dashboard HTML (with an inline secToken),
// the user-info endpoint, and the tokenplan gateway.
func qwenCookieTestHandler(t *testing.T, email string) http.Handler {
	t.Helper()
	reset := time.Now().Add(2 * time.Hour).UnixMilli()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/billing/"):
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><script>window.__CONF__={"secToken":"test-sec-token"};</script></html>`)
		case r.URL.Path == "/tool/user/info.json":
			fmt.Fprintf(w, `{"data":{"email":%q,"secToken":"test-sec-token"}}`, email)
		case r.URL.Path == "/data/api.json":
			if err := r.ParseForm(); err != nil {
				http.Error(w, "bad form", http.StatusBadRequest)
				return
			}
			if r.Form.Get("sec_token") != "test-sec-token" {
				fmt.Fprint(w, `{"success":false,"errorCode":"BailianGateway.Login.NotLogined"}`)
				return
			}
			var params struct {
				Api  string `json:"Api"`
				Data struct {
					Cornerstone struct {
						ConsoleSite string `json:"consoleSite"`
					} `json:"cornerstoneParam"`
				} `json:"Data"`
			}
			if err := json.Unmarshal([]byte(r.Form.Get("params")), &params); err != nil {
				http.Error(w, "bad params", http.StatusBadRequest)
				return
			}
			if params.Api != qwenCookieUsageAPI || params.Data.Cornerstone.ConsoleSite != qwenCookieConsoleSite {
				http.Error(w, "bad api", http.StatusBadRequest)
				return
			}
			fmt.Fprintf(w, `{"success":true,"data":{"per5HourPercentage":0.37,"per1WeekPercentage":0.12,"per5HourResetTime":%d,"per1WeekResetTime":%d}}`, reset, reset)
		default:
			http.NotFound(w, r)
		}
	})
}

func qwenCookieTestServer(t *testing.T, email string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(qwenCookieTestHandler(t, email))
}

func qwenCookieTestTargets(server *httptest.Server) {
	qwenCookieDataGateway = server.URL
	qwenCookieDashboardURL = server.URL + "/billing/subscription/token-plan-individual"
	qwenCookieUserInfoURL = server.URL + "/tool/user/info.json"
}

func TestParseQwenCookieUsage(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour).UnixMilli()
	windows, err := parseQwenCookieUsage([]byte(fmt.Sprintf(
		`{"success":true,"data":{"per5HourPercentage":0.37,"per1WeekPercentage":"0.12","per5HourResetTime":%d,"per1WeekResetTime":%d}}`, reset, reset)))
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 2 {
		t.Fatalf("windows = %+v", windows)
	}
	if windows[0].Name != "5h" || windows[0].UsedPercent != 37 || windows[0].LimitWindowSeconds != 18000 || windows[0].ResetAfterSeconds <= 0 {
		t.Fatalf("5h window = %+v", windows[0])
	}
	if windows[1].Name != "7d" || windows[1].UsedPercent != 12 {
		t.Fatalf("7d window = %+v", windows[1])
	}

	// Embedded JSON-in-string envelope, as the one-console gateway nests it.
	inner, _ := json.Marshal(map[string]any{"per1WeekPercentage": 0.5})
	outer, _ := json.Marshal(map[string]any{"success": true, "data": map[string]any{"payload": string(inner)}})
	windows, err = parseQwenCookieUsage(outer)
	if err != nil || len(windows) != 1 || windows[0].Name != "7d" || windows[0].UsedPercent != 50 {
		t.Fatalf("embedded windows = %+v, err %v", windows, err)
	}

	if _, err := parseQwenCookieUsage([]byte(`{"success":false,"errorCode":"BailianGateway.Login.NotLogined"}`)); err == nil {
		t.Fatal("login-required envelope must fail")
	}
	if _, err := parseQwenCookieUsage([]byte(`{"success":true,"data":{}}`)); err == nil {
		t.Fatal("payload-less response must fail")
	}
}

func TestResolveQwenSecTokenFallbacks(t *testing.T) {
	qwenCookieTestEnv(t)
	server := qwenCookieTestServer(t, "user@example.com")
	defer server.Close()
	qwenCookieTestTargets(server)
	client := qwenCookieHTTPClient()

	// Dashboard HTML carries the token.
	token, err := resolveQwenSecToken(context.Background(), client, "cna=abc")
	if err != nil || token != "test-sec-token" {
		t.Fatalf("dashboard token = %q, %v", token, err)
	}

	// Dashboard unreachable: cookie fallback wins without any request.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusBadGateway)
	}))
	defer broken.Close()
	qwenCookieDashboardURL = broken.URL + "/billing/subscription/token-plan-individual"
	token, err = resolveQwenSecToken(context.Background(), client, "cna=abc; sec_token=cookie-token")
	if err != nil || token != "cookie-token" {
		t.Fatalf("cookie token = %q, %v", token, err)
	}

	// No cookie either: user-info JSON is the last resort.
	qwenCookieDashboardURL = broken.URL + "/billing/subscription/token-plan-individual"
	qwenCookieUserInfoURL = server.URL + "/tool/user/info.json"
	token, err = resolveQwenSecToken(context.Background(), client, "cna=abc")
	if err != nil || token != "test-sec-token" {
		t.Fatalf("user-info token = %q, %v", token, err)
	}
}

func TestQwenCookieFetchUsageRequestShape(t *testing.T) {
	qwenCookieTestEnv(t)
	inner := qwenCookieTestHandler(t, "user@example.com")

	var sawOrigin, sawReferer, sawCSRF, sawRequestedWith string
	var sawQuery url.Values
	shape := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/data/api.json" {
			sawQuery = r.URL.Query()
			sawOrigin = r.Header.Get("Origin")
			sawReferer = r.Header.Get("Referer")
			sawCSRF = r.Header.Get("x-xsrf-token")
			sawRequestedWith = r.Header.Get("X-Requested-With")
		}
		inner.ServeHTTP(w, r)
	}))
	defer shape.Close()
	qwenCookieTestTargets(shape)

	windows, err := qwenCookieFetchUsage(context.Background(), qwenCookieHTTPClient(), "cna=abc; login_aliyunid_csrf=csrf-value", "test-sec-token")
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 2 {
		t.Fatalf("windows = %+v", windows)
	}
	if sawQuery.Get("action") != qwenCookieConsoleAction || sawQuery.Get("product") != qwenCookieConsoleProduct || sawQuery.Get("api") != qwenCookieUsageAPI {
		t.Fatalf("query = %v", sawQuery)
	}
	if sawCSRF != "csrf-value" || sawRequestedWith != "XMLHttpRequest" || sawOrigin == "" || !strings.Contains(sawReferer, "/billing/subscription/token-plan-individual") {
		t.Fatalf("headers: origin=%q referer=%q csrf=%q xrw=%q", sawOrigin, sawReferer, sawCSRF, sawRequestedWith)
	}
}

func TestEnrichQwenRowsWithCookieQuota(t *testing.T) {
	qwenCookieTestEnv(t)
	server := qwenCookieTestServer(t, "user@example.com")
	defer server.Close()
	qwenCookieTestTargets(server)
	qwenDiscoverCookieSessions = func(context.Context) []qwenCookieSession {
		return []qwenCookieSession{{header: "login_aliyunid_ticket=ticket; cna=abc", source: "test"}}
	}

	rows := []srUsageRow{
		{
			email: "qwen-token:work", provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey,
			accountIdentity: "user@example.com", quotaStatus: "login needed",
			err: errors.New("Qwen console login needed"),
		},
		{
			email: "qwen-token:other", provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey,
			accountIdentity: "other@example.com", quotaStatus: "login needed",
		},
		{
			email: "user@example.com", provider: accounts.ProviderClaude, authMode: accounts.AuthModeOAuth,
		},
	}
	quotas, applied, fresh := enrichQwenRowsWithCookieQuotaFresh(context.Background(), rows)
	if !applied || !fresh || len(quotas) != 1 || quotas[0].email != "user@example.com" {
		t.Fatalf("quotas=%+v applied=%v fresh=%v", quotas, applied, fresh)
	}
	if !rows[0].quotaUsageKnown || rows[0].quotaStatus != "live" || len(rows[0].windows) != 2 || rows[0].err != nil {
		t.Fatalf("matched row = quotaKnown=%v status=%q windows=%+v err=%v", rows[0].quotaUsageKnown, rows[0].quotaStatus, rows[0].windows, rows[0].err)
	}
	if rows[1].quotaUsageKnown || len(rows[1].windows) != 0 {
		t.Fatalf("unmatched identity was enriched: %+v", rows[1].windows)
	}
	if rows[2].quotaUsageKnown {
		t.Fatal("non-Qwen row was enriched")
	}

	// A second run serves the disk cache: applied without any network, so the
	// closed server proves no fetch happened.
	server.Close()
	rows[0].quotaUsageKnown = false
	rows[0].windows = nil
	rows[0].quotaStatus = "login needed"
	rows[0].err = errors.New("Qwen console login needed")
	_, applied, fresh = enrichQwenRowsWithCookieQuotaFresh(context.Background(), rows[:1])
	if !applied || fresh {
		t.Fatalf("cache run: applied=%v fresh=%v", applied, fresh)
	}
	if !rows[0].quotaUsageKnown || len(rows[0].windows) != 2 {
		t.Fatalf("cached row = %+v", rows[0].windows)
	}
}

// Two browser sessions logged into different Alibaba accounts enrich the two
// matching rows with their own windows.
func TestEnrichQwenRowsMultipleSessions(t *testing.T) {
	qwenCookieTestEnv(t)
	reset := time.Now().Add(2 * time.Hour).UnixMilli()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie := r.Header.Get("Cookie")
		var email string
		var weekly float64
		switch {
		case strings.Contains(cookie, "ticket=aaa"):
			email, weekly = "a@example.com", 0.12
		case strings.Contains(cookie, "ticket=bbb"):
			email, weekly = "b@example.com", 0.62
		default:
			fmt.Fprint(w, `{"success":false,"errorCode":"BailianGateway.Login.NotLogined"}`)
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/billing/"):
			fmt.Fprint(w, `<html><script>var x={"secToken":"t"};</script></html>`)
		case r.URL.Path == "/tool/user/info.json":
			fmt.Fprintf(w, `{"data":{"email":%q,"secToken":"t"}}`, email)
		case r.URL.Path == "/data/api.json":
			fmt.Fprintf(w, `{"success":true,"data":{"per5HourPercentage":0.37,"per1WeekPercentage":%v,"per5HourResetTime":%d,"per1WeekResetTime":%d}}`, weekly, reset, reset)
		}
	}))
	defer server.Close()
	qwenCookieTestTargets(server)
	qwenDiscoverCookieSessions = func(context.Context) []qwenCookieSession {
		return []qwenCookieSession{
			{header: "login_aliyunid_ticket=aaa", source: "Chrome/Default"},
			{header: "login_aliyunid_ticket=bbb", source: "Chrome/Profile 1"},
		}
	}

	rows := []srUsageRow{
		{email: "qwen-token:a", provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey, accountIdentity: "a@example.com", quotaStatus: "login needed"},
		{email: "qwen-token:b", provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey, accountIdentity: "b@example.com", quotaStatus: "login needed"},
	}
	quotas, applied, fresh := enrichQwenRowsWithCookieQuotaFresh(context.Background(), rows)
	if !applied || !fresh || len(quotas) != 2 {
		t.Fatalf("quotas=%+v applied=%v fresh=%v", quotas, applied, fresh)
	}
	if !rows[0].quotaUsageKnown || len(rows[0].windows) != 2 || rows[0].windows[1].UsedPercent != 12 {
		t.Fatalf("row a windows = %+v", rows[0].windows)
	}
	if !rows[1].quotaUsageKnown || len(rows[1].windows) != 2 || rows[1].windows[1].UsedPercent != 62 {
		t.Fatalf("row b windows = %+v", rows[1].windows)
	}

	// A dead session (logged out: user-info has no email, usage says
	// NotLogined) leaves its row untouched while the live session enriches.
	rows[1].quotaUsageKnown = false
	rows[1].windows = nil
	rows[1].quotaStatus = "login needed"
	qwenDiscoverCookieSessions = func(context.Context) []qwenCookieSession {
		return []qwenCookieSession{
			{header: "login_aliyunid_ticket=aaa", source: "Chrome/Default"},
			{header: "login_aliyunid_ticket=dead", source: "Chrome/Profile 1"},
		}
	}
	// Bust the cache for the dead profile so the network path runs.
	cache := loadQwenCookieQuotaCache()
	delete(cache.Accounts, "source:Chrome/Profile 1")
	saveQwenCookieQuotaCache(cache)
	_, applied, _ = enrichQwenRowsWithCookieQuotaFresh(context.Background(), rows[1:2])
	if applied || rows[1].quotaUsageKnown {
		t.Fatalf("dead session enriched row: %+v", rows[1].windows)
	}
}

// With one needy row and an unidentified session, enrichment applies on the
// single-account assumption; with two needy rows it never guesses.
func TestEnrichQwenRowsUnknownIdentity(t *testing.T) {
	qwenCookieTestEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/billing/"):
			fmt.Fprint(w, `<html><script>var x={"secToken":"t"};</script></html>`)
		case r.URL.Path == "/tool/user/info.json":
			fmt.Fprint(w, `{"data":{"secToken":"t"}}`)
		case r.URL.Path == "/data/api.json":
			fmt.Fprint(w, `{"success":true,"data":{"per5HourPercentage":0.37}}`)
		}
	}))
	defer server.Close()
	qwenCookieTestTargets(server)
	qwenDiscoverCookieSessions = func(context.Context) []qwenCookieSession {
		return []qwenCookieSession{{header: "login_aliyunid_ticket=ticket", source: "test"}}
	}

	single := []srUsageRow{{email: "qwen-token:work", provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey, quotaStatus: "login needed"}}
	_, applied, _ := enrichQwenRowsWithCookieQuotaFresh(context.Background(), single)
	if !applied || !single[0].quotaUsageKnown {
		t.Fatalf("single unknown-identity row not enriched: %+v", single[0])
	}
}

// qwenB64 encodes a JSON blob the way the passport cookies do.
func qwenB64(raw string) string {
	return base64.StdEncoding.EncodeToString([]byte(raw))
}

func TestQwenCookieSessionIdentity(t *testing.T) {
	lastU := qwenB64(`{"hid":270612222595,"loginId":"TheGenerousCorp@Gmail.com","sg":"abc"}`)
	havana := qwenB64(`{"createTime":1,"patialTgc":{"accInfos":{"6":{"accessType":1,"memberId":270602993999,"tgtId":"t"}}}}`)

	email, masked, memberID := qwenCookieSessionIdentity("login_aliyunid_ticket=t; last_u_intl-aliyun_intl-aliyun=" + lastU)
	if email != "thegenerouscorp@gmail.com" || masked != "" || memberID != "270612222595" {
		t.Fatalf("last_u identity = %q %q %q", email, masked, memberID)
	}

	email, masked, memberID = qwenCookieSessionIdentity("login_aliyunid_ticket=t; havana_tgc=" + havana)
	if email != "" || masked != "" || memberID != "270602993999" {
		t.Fatalf("havana identity = %q %q %q", email, masked, memberID)
	}

	email, masked, memberID = qwenCookieSessionIdentity("login_aliyunid_ticket=t; login_aliyunid=thegenerous****@gmail.com")
	if email != "" || masked != "thegenerous****@gmail.com" || memberID != "" {
		t.Fatalf("masked identity = %q %q %q", email, masked, memberID)
	}

	if !qwenMaskedEmailMatches("thegenerous****@gmail.com", "thegenerouscorp@gmail.com") {
		t.Fatal("masked email should match its account")
	}
	if qwenMaskedEmailMatches("thegenerous****@gmail.com", "daniel.raffel@gmail.com") {
		t.Fatal("masked email matched the wrong account")
	}
	if qwenMaskedEmailMatches("thegenerous****@gmail.com", "thegenerouscorp@example.org") {
		t.Fatal("masked email matched the wrong domain")
	}
	if qwenMaskedEmailMatches("dan****@gmail.com", "dan@gmail.com") {
		t.Fatal("masked email matched with no hidden characters")
	}
}

// Cookie-embedded identity drives matching end to end: an exact-email session
// matches its row directly, and a member-ID-only session pairs with the
// remaining row by elimination, backfilling its email for fan-out.
func TestEnrichQwenRowsCookieIdentity(t *testing.T) {
	qwenCookieTestEnv(t)
	reset := time.Now().Add(2 * time.Hour).UnixMilli()
	lastU := qwenB64(`{"hid":111,"loginId":"a@example.com","sg":"x"}`)
	havana := qwenB64(`{"createTime":1,"patialTgc":{"accInfos":{"6":{"memberId":222,"tgtId":"t"}}}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie := r.Header.Get("Cookie")
		var weekly float64
		switch {
		case strings.Contains(cookie, "ticket=aaa"):
			weekly = 0.12
		case strings.Contains(cookie, "ticket=bbb"):
			weekly = 0.62
		default:
			fmt.Fprint(w, `{"success":false,"errorCode":"BailianGateway.Login.NotLogined"}`)
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/billing/"):
			fmt.Fprint(w, `<html><script>var x={"secToken":"t"};</script></html>`)
		case r.URL.Path == "/tool/user/info.json":
			// The Model Studio console user-info carries no email.
			fmt.Fprint(w, `{"code":"200","data":{"secToken":"t"},"successResponse":true}`)
		case r.URL.Path == "/data/api.json":
			fmt.Fprintf(w, `{"success":true,"data":{"per5HourPercentage":0.37,"per1WeekPercentage":%v,"per5HourResetTime":%d,"per1WeekResetTime":%d}}`, weekly, reset, reset)
		}
	}))
	defer server.Close()
	qwenCookieTestTargets(server)
	qwenDiscoverCookieSessions = func(context.Context) []qwenCookieSession {
		return []qwenCookieSession{
			{header: "login_aliyunid_ticket=aaa; last_u_intl-aliyun_intl-aliyun=" + lastU, source: "Chrome/Default"},
			{header: "login_aliyunid_ticket=bbb; havana_tgc=" + havana, source: "Chrome/Profile 1"},
		}
	}

	rows := []srUsageRow{
		{email: "qwen-token:a", provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey, accountIdentity: "a@example.com", quotaStatus: "login needed"},
		{email: "qwen-token:b", provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey, accountIdentity: "b@example.com", quotaStatus: "login needed"},
	}
	quotas, applied, fresh := enrichQwenRowsWithCookieQuotaFresh(context.Background(), rows)
	if !applied || !fresh || len(quotas) != 2 {
		t.Fatalf("quotas=%+v applied=%v fresh=%v", quotas, applied, fresh)
	}
	if !rows[0].quotaUsageKnown || len(rows[0].windows) != 2 || rows[0].windows[1].UsedPercent != 12 {
		t.Fatalf("row a windows = %+v", rows[0].windows)
	}
	if !rows[1].quotaUsageKnown || len(rows[1].windows) != 2 || rows[1].windows[1].UsedPercent != 62 {
		t.Fatalf("row b windows = %+v", rows[1].windows)
	}
	// The eliminated session inherits the row's identity for the server push.
	var bQuota *qwenCookieQuota
	for qi := range quotas {
		if quotas[qi].memberID == "222" {
			bQuota = &quotas[qi]
		}
	}
	if bQuota == nil || bQuota.email != "b@example.com" {
		t.Fatalf("eliminated quota identity = %+v", bQuota)
	}
}

func TestPushQwenCookieQuota(t *testing.T) {
	type push struct {
		Email   string                 `json:"email"`
		Windows []accounts.UsageWindow `json:"windows"`
	}
	var got []push
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_subrouter/qwen-quota" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var p push
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		got = append(got, p)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	runner := srRunner{}
	config := srServerConfig{Name: "test", URL: server.URL, requestClient: server.Client()}
	quota := qwenCookieQuota{email: "user@example.com", windows: []accounts.UsageWindow{{Name: "5h", UsedPercent: 37, LimitWindowSeconds: 18000}}}
	runner.pushQwenCookieQuota(context.Background(), config, quota)
	if len(got) != 1 || got[0].Email != "user@example.com" || len(got[0].Windows) != 1 {
		t.Fatalf("pushes = %+v", got)
	}

	// No identity means no push: the server could never match it.
	runner.pushQwenCookieQuota(context.Background(), config, qwenCookieQuota{windows: quota.windows})
	if len(got) != 1 {
		t.Fatalf("anonymous push went out: %+v", got)
	}

	server.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runner.pushQwenCookieQuota(context.Background(), config, quota)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("push to a dead server did not return")
	}
}
