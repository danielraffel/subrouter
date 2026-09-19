package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/storepath"
)

// Qwen Token Plan quota normally comes from the Bailian CLI console
// access_token, which dies often and needs an interactive browser login.
// Qwen Cloud's own dashboard calls the same tokenplan APIs with browser
// cookies, and web sessions outlive the CLI token by a lot. This file is a
// display-only fallback: when a Qwen row's quota is unknown ("quota login
// needed"), read the qwencloud/alibabacloud session cookies locally, call the
// tokenplan usage API the way the dashboard does, and overlay the row's quota
// windows. Freshly fetched readings are pushed to the server so other clients
// see them too. Cookie values are secrets: never logged, cache files 0600,
// and every failure leaves the row untouched.

const (
	qwenCookieUsageAPI       = "zeldaHttp.apikeyMgr./tokenplan/personal/api/v2/usage"
	qwenCookieConsoleProduct = "sfm_bailian"
	qwenCookieConsoleAction  = "IntlBroadScopeAspnGateway"
	qwenCookieRegion         = "ap-southeast-1"
	qwenCookieLanguage       = "en-US"
	qwenCookieCacheTTL       = 5 * time.Minute
	qwenCookieRequestTimeout = 6 * time.Second
)

// URLs are variables so tests can point the client at an httptest server.
var (
	qwenCookieDataGateway  = "https://cs-data.qwencloud.com"
	qwenCookieDashboardURL = "https://home.qwencloud.com/billing/subscription/token-plan-individual"
	qwenCookieUserInfoURL  = "https://home.qwencloud.com/tool/user/info.json"
)

// qwenDiscoverCookieHeader reads the qwencloud/alibabacloud session cookies
// from local browsers. Platform-specific: sr_qwen_cookie_darwin.go installs
// the real implementation, sr_qwen_cookie_other.go a no-op stub. Tests
// override and restore it.
var qwenDiscoverCookieHeader func(ctx context.Context) (header, source string, ok bool)

var qwenSecTokenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`"secToken"\s*:\s*"([^"]+)"`),
	regexp.MustCompile(`"sec_token"\s*:\s*"([^"]+)"`),
	regexp.MustCompile(`secToken['"]?\s*[:=]\s*['"]([^'"]+)['"]`),
	regexp.MustCompile(`sec_token['"]?\s*[:=]\s*['"]([^'"]+)['"]`),
	regexp.MustCompile(`csrfToken['"]?\s*[:=]\s*['"]([^'"]+)['"]`),
}

func qwenCookieHTTPClient() *http.Client {
	return &http.Client{Timeout: qwenCookieRequestTimeout}
}

func qwenCookieGET(ctx context.Context, client *http.Client, url, cookieHeader string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Cookie", cookieHeader)
	res, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return body, res.StatusCode, err
}

func qwenLooksLikeLoginPage(html string) bool {
	lowered := strings.ToLower(html)
	return strings.Contains(lowered, "passport.alibabacloud.com") ||
		strings.Contains(lowered, "signin.aliyun.com") ||
		strings.Contains(lowered, "account.alibabacloud.com/login") ||
		strings.Contains(lowered, "login.qwencloud.com") ||
		(strings.Contains(lowered, "login") && strings.Contains(lowered, "password") && strings.Contains(lowered, "sign in"))
}

func qwenCookiePairs(header string) [][2]string {
	var out [][2]string
	for _, part := range strings.Split(header, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && name != "" {
			out = append(out, [2]string{name, value})
		}
	}
	return out
}

func qwenCookieValue(header, name string) string {
	for _, pair := range qwenCookiePairs(header) {
		if strings.EqualFold(pair[0], name) {
			return pair[1]
		}
	}
	return ""
}

// resolveQwenSecToken mirrors the dashboard's token resolution: inline
// dashboard HTML first (freshest), then a sec_token cookie, then the
// user-info JSON endpoint.
func resolveQwenSecToken(ctx context.Context, client *http.Client, cookieHeader string) (string, error) {
	if body, status, err := qwenCookieGET(ctx, client, qwenCookieDashboardURL, cookieHeader); err == nil && status == http.StatusOK {
		html := string(body)
		if !qwenLooksLikeLoginPage(html) {
			for _, pattern := range qwenSecTokenPatterns {
				if match := pattern.FindStringSubmatch(html); len(match) == 2 && match[1] != "" {
					return match[1], nil
				}
			}
		}
	}
	if token := qwenCookieValue(cookieHeader, "sec_token"); token != "" {
		return token, nil
	}
	if body, status, err := qwenCookieGET(ctx, client, qwenCookieUserInfoURL, cookieHeader); err == nil && status == http.StatusOK {
		var parsed any
		if json.Unmarshal(body, &parsed) == nil {
			if token := qwenJSONFindString(parsed, []string{"secToken", "sec_token", "csrfToken", "token"}); token != "" {
				return token, nil
			}
		}
	}
	return "", errors.New("qwen sec_token not found")
}

// qwenJSONFindString returns the first non-empty string under any of the
// given keys, expanding JSON embedded in string values the way the
// one-console gateway nests its envelopes.
func qwenJSONFindString(value any, keys []string) string {
	queue := []any{value}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		switch typed := current.(type) {
		case map[string]any:
			for _, key := range keys {
				if raw, ok := typed[key]; ok {
					if text, ok := raw.(string); ok && strings.TrimSpace(text) != "" {
						return strings.TrimSpace(text)
					}
				}
			}
			for _, child := range typed {
				queue = append(queue, qwenJSONExpand(child))
			}
		case []any:
			for _, child := range typed {
				queue = append(queue, qwenJSONExpand(child))
			}
		}
	}
	return ""
}

func qwenJSONExpand(value any) any {
	text, ok := value.(string)
	if !ok || len(text) < 2 || (text[0] != '{' && text[0] != '[') {
		return value
	}
	var parsed any
	if json.Unmarshal([]byte(text), &parsed) != nil {
		return value
	}
	return parsed
}

// qwenCookieAccountEmail identifies the Alibaba account behind the cookie
// session via the user-info endpoint, for matching rows by account identity.
func qwenCookieAccountEmail(ctx context.Context, client *http.Client, cookieHeader string) string {
	body, status, err := qwenCookieGET(ctx, client, qwenCookieUserInfoURL, cookieHeader)
	if err != nil || status != http.StatusOK {
		return ""
	}
	var parsed any
	if json.Unmarshal(body, &parsed) != nil {
		return ""
	}
	queue := []any{parsed}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		object, ok := current.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"email", "userEmail", "userPrincipalName"} {
			if raw, ok := object[key]; ok {
				if text, ok := raw.(string); ok {
					if email := strings.ToLower(strings.TrimSpace(text)); strings.Contains(email, "@") {
						return email
					}
				}
			}
		}
		for _, child := range object {
			queue = append(queue, qwenJSONExpand(child))
		}
	}
	return ""
}

// qwenCookieFetchUsage calls the tokenplan usage API with cookie auth, the
// way the Qwen Cloud dashboard does.
func qwenCookieFetchUsage(ctx context.Context, client *http.Client, cookieHeader, secToken string) ([]accounts.UsageWindow, error) {
	dashboardURL, err := url.Parse(qwenCookieDashboardURL)
	if err != nil {
		return nil, err
	}
	cornerstone := map[string]any{
		"protocol":    "V2",
		"console":     "ONE_CONSOLE",
		"productCode": "p_efm",
		"consoleSite": "QWENCLOUD",
		"domain":      dashboardURL.Host,
		"feURL":       qwenCookieDashboardURL,
		"xsp_lang":    qwenCookieLanguage,
	}
	if cna := qwenCookieValue(cookieHeader, "cna"); cna != "" {
		cornerstone["X-Anonymous-Id"] = cna
	}
	params := map[string]any{
		"Api":  qwenCookieUsageAPI,
		"V":    "1.0",
		"Data": map[string]any{"cornerstoneParam": cornerstone},
	}
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	form := url.Values{
		"product":   {qwenCookieConsoleProduct},
		"action":    {qwenCookieConsoleAction},
		"sec_token": {secToken},
		"region":    {qwenCookieRegion},
		"language":  {qwenCookieLanguage},
		"params":    {string(paramsJSON)},
	}
	endpoint := qwenCookieDataGateway + "/data/api.json?action=" + qwenCookieConsoleAction +
		"&product=" + qwenCookieConsoleProduct + "&api=" + url.QueryEscape(qwenCookieUsageAPI) + "&_v=undefined"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Cookie", cookieHeader)
	req.Header.Set("Origin", "https://"+dashboardURL.Host)
	req.Header.Set("Referer", qwenCookieDashboardURL)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	if csrf := qwenCookieValue(cookieHeader, "login_aliyunid_csrf"); csrf != "" {
		req.Header.Set("x-xsrf-token", csrf)
		req.Header.Set("x-csrf-token", csrf)
	} else if csrf := qwenCookieValue(cookieHeader, "csrf"); csrf != "" {
		req.Header.Set("x-xsrf-token", csrf)
		req.Header.Set("x-csrf-token", csrf)
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("qwen tokenplan usage returned HTTP %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return parseQwenCookieUsage(body)
}

// parseQwenCookieUsage finds the tokenplan usage payload anywhere in the
// gateway envelope and builds 5h/7d windows from its 0..1 ratios.
func parseQwenCookieUsage(body []byte) ([]accounts.UsageWindow, error) {
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	queue := []any{parsed}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		object, ok := current.(map[string]any)
		if !ok {
			continue
		}
		if success, ok := object["success"].(bool); ok && !success {
			code, _ := object["errorCode"].(string)
			if code == "" {
				code = "unknown error"
			}
			return nil, fmt.Errorf("qwen tokenplan usage error: %s", code)
		}
		fiveHour, hasFiveHour := qwenJSONRatio(object["per5HourPercentage"])
		weekly, hasWeekly := qwenJSONRatio(object["per1WeekPercentage"])
		if hasFiveHour || hasWeekly {
			now := time.Now()
			var windows []accounts.UsageWindow
			if window := qwenCookieQuotaWindow("5h", 5*time.Hour, fiveHour, hasFiveHour, object["per5HourResetTime"], now); window != nil {
				windows = append(windows, *window)
			}
			if window := qwenCookieQuotaWindow("7d", 7*24*time.Hour, weekly, hasWeekly, object["per1WeekResetTime"], now); window != nil {
				windows = append(windows, *window)
			}
			return windows, nil
		}
		for _, child := range object {
			queue = append(queue, qwenJSONExpand(child))
		}
	}
	return nil, errors.New("qwen tokenplan usage payload not found")
}

func qwenJSONRatio(value any) (float64, bool) {
	switch typed := qwenJSONExpand(value).(type) {
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return 0, false
		}
		return typed, true
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return 0, false
		}
		return parsed, true
	}
	return 0, false
}

func qwenCookieQuotaWindow(name string, duration time.Duration, ratio float64, ok bool, resetValue any, now time.Time) *accounts.UsageWindow {
	if !ok {
		return nil
	}
	used := ratio * 100
	if used < 0 {
		used = 0
	}
	if used > 100 {
		used = 100
	}
	window := &accounts.UsageWindow{Name: name, UsedPercent: used, LimitWindowSeconds: int64(duration / time.Second)}
	if epoch, ok := qwenJSONRatio(resetValue); ok && epoch > 0 {
		seconds := epoch
		if epoch >= 1e12 {
			seconds = epoch / 1000
		}
		if remaining := int64(time.Unix(int64(seconds), 0).Sub(now).Seconds()); remaining > 0 {
			window.ResetAfterSeconds = remaining
		}
	}
	return window
}

// qwenCookieQuotaCacheEntry is a disk-cached cookie-path reading so repeated
// sr status runs are instant and the tokenplan API is not hammered.
type qwenCookieQuotaCacheEntry struct {
	Email     string                 `json:"email"`
	Windows   []accounts.UsageWindow `json:"windows"`
	FetchedAt time.Time              `json:"fetched_at"`
}

type qwenCookieQuotaCacheFile struct {
	Accounts map[string]qwenCookieQuotaCacheEntry `json:"accounts"`
}

func qwenCookieQuotaCachePath() string {
	return filepath.Join(storepath.CodexDir(), "qwen-cookie-quota.json")
}

func loadQwenCookieQuotaCache() qwenCookieQuotaCacheFile {
	data, err := os.ReadFile(qwenCookieQuotaCachePath())
	if err != nil {
		return qwenCookieQuotaCacheFile{Accounts: map[string]qwenCookieQuotaCacheEntry{}}
	}
	var file qwenCookieQuotaCacheFile
	if err := json.Unmarshal(data, &file); err != nil || file.Accounts == nil {
		return qwenCookieQuotaCacheFile{Accounts: map[string]qwenCookieQuotaCacheEntry{}}
	}
	return file
}

func saveQwenCookieQuotaCache(cache qwenCookieQuotaCacheFile) {
	data, err := json.Marshal(cache)
	if err != nil {
		return
	}
	path := qwenCookieQuotaCachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return
	}
	_ = os.Chmod(path, 0600)
}

// qwenCookieQuota is one cookie-path reading: the account email when the
// user-info endpoint identified it, plus the quota windows.
type qwenCookieQuota struct {
	email   string
	windows []accounts.UsageWindow
}

// qwenCookieQuotaFresh fetches quota through the browser cookie session. The
// disk cache serves repeat runs; fresh reports whether the network was used.
func qwenCookieQuotaFresh(ctx context.Context) (qwenCookieQuota, bool, error) {
	cache := loadQwenCookieQuotaCache()
	for _, entry := range cache.Accounts {
		if time.Since(entry.FetchedAt) < qwenCookieCacheTTL && len(entry.Windows) > 0 {
			return qwenCookieQuota{email: entry.Email, windows: entry.Windows}, false, nil
		}
	}
	if qwenDiscoverCookieHeader == nil {
		return qwenCookieQuota{}, false, errors.New("qwen cookie discovery unavailable")
	}
	header, _, ok := qwenDiscoverCookieHeader(ctx)
	if !ok || strings.TrimSpace(header) == "" {
		return qwenCookieQuota{}, false, errors.New("no qwen browser session")
	}
	client := qwenCookieHTTPClient()
	secToken, err := resolveQwenSecToken(ctx, client, header)
	if err != nil {
		return qwenCookieQuota{}, false, err
	}
	windows, err := qwenCookieFetchUsage(ctx, client, header, secToken)
	if err != nil {
		return qwenCookieQuota{}, false, err
	}
	if len(windows) == 0 {
		return qwenCookieQuota{}, false, errors.New("qwen tokenplan usage payload not found")
	}
	email := qwenCookieAccountEmail(ctx, client, header)
	cache.Accounts[email] = qwenCookieQuotaCacheEntry{Email: email, Windows: windows, FetchedAt: time.Now()}
	saveQwenCookieQuotaCache(cache)
	return qwenCookieQuota{email: email, windows: windows}, true, nil
}

// qwenRowNeedsCookieQuota reports whether a row is a Qwen token-plan row
// whose quota is unknown (the console credential path failed or is missing).
func qwenRowNeedsCookieQuota(row srUsageRow) bool {
	return usageProvider(row) == accounts.ProviderQwenToken && !row.quotaUsageKnown
}

// qwenRowAccountKey returns the row's matchable account identity: the console
// account email when known, else the row email if it is itself an email.
func qwenRowAccountKey(row srUsageRow) string {
	if identity := strings.ToLower(strings.TrimSpace(row.accountIdentity)); strings.Contains(identity, "@") {
		return identity
	}
	if email := strings.ToLower(strings.TrimSpace(row.email)); strings.Contains(email, "@") {
		return email
	}
	return ""
}

func enrichQwenRowsWithCookieQuota(ctx context.Context, rows []srUsageRow) {
	enrichQwenRowsWithCookieQuotaFresh(ctx, rows)
}

// qwenCookieQuotaResult is the outcome of an enrichment attempt: applied says
// at least one row changed, fresh says the reading came from the network this
// call (cache hits excluded) so the caller can fan it out to the server.

// enrichQwenRowsWithCookieQuotaFresh overlays cookie-derived quota windows on
// Qwen rows whose quota is unknown. Matching is by console account email;
// when the cookie session's identity is unknown or unmatched, a single needy
// row is enriched on the assumption that one configured account maps to the
// one browser login. Any failure leaves every row untouched.
func enrichQwenRowsWithCookieQuotaFresh(ctx context.Context, rows []srUsageRow) (quota qwenCookieQuota, applied, fresh bool) {
	needy := make([]int, 0, 2)
	for i, row := range rows {
		if qwenRowNeedsCookieQuota(row) {
			needy = append(needy, i)
		}
	}
	if len(needy) == 0 {
		return qwenCookieQuota{}, false, false
	}
	enrichCtx, cancel := context.WithTimeout(ctx, claudeWebEnrichTimeout)
	defer cancel()
	var err error
	quota, fresh, err = qwenCookieQuotaFresh(enrichCtx)
	if err != nil {
		return qwenCookieQuota{}, false, false
	}
	for _, i := range needy {
		key := qwenRowAccountKey(rows[i])
		if quota.email != "" && key != "" && key != quota.email {
			continue
		}
		if quota.email == "" && len(needy) > 1 {
			// Unknown session identity with several candidate accounts: never
			// guess which one the browser is logged into.
			continue
		}
		if quota.email != "" && key == "" && len(needy) > 1 {
			continue
		}
		applyQwenCookieQuota(&rows[i], quota.windows)
		applied = true
	}
	if !applied {
		return qwenCookieQuota{}, false, false
	}
	return quota, true, fresh
}

func applyQwenCookieQuota(row *srUsageRow, windows []accounts.UsageWindow) {
	row.windows = append([]accounts.UsageWindow(nil), windows...)
	row.quotaUsageKnown = true
	if row.quotaStatus == "" || row.quotaStatus == "login needed" || row.quotaStatus == "error" {
		row.quotaStatus = "live"
	}
	// The console credential is telemetry-only; with fresh quota the stale
	// login error must not keep the row looking broken.
	row.err = nil
	row.score = scoreFromWindows(row.email, windows)
	row.cooked, row.cookedReason = cookedFromWindows(windows)
	row.tempCooked, row.tempCookedReason = tempCookedFromWindows(windows)
}
