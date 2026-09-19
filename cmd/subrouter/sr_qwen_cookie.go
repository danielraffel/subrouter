package main

import (
	"context"
	"encoding/base64"
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

// Qwen Token Plan quota normally comes from the Model Studio console
// access_token, which dies often and needs an interactive browser login.
// The Model Studio web console calls the same tokenplan APIs with browser
// cookies, and web sessions outlive the CLI token by a lot. This file is a
// display-only fallback: when a Qwen row's quota is unknown ("quota login
// needed"), read the alibabacloud/aliyun session cookies locally, call the
// tokenplan usage API the way the console does, and overlay the row's quota
// windows. Freshly fetched readings are pushed to the server so other clients
// see them too. Cookie values are secrets: never logged, cache files 0600,
// and every failure leaves the row untouched.

const (
	qwenCookieUsageAPI       = "zeldaHttp.apikeyMgr./tokenplan/personal/api/v2/usage"
	qwenCookieConsoleProduct = "sfm_bailian"
	qwenCookieConsoleAction  = "IntlBroadScopeAspnGateway"
	// Alibaba's live console contract, including its historical spelling.
	qwenCookieConsoleSite    = "MODELSTUDIO_ALBABACLOUD"
	qwenCookieRegion         = "ap-southeast-1"
	qwenCookieLanguage       = "en-US"
	qwenCookieCacheTTL       = 5 * time.Minute
	qwenCookieRequestTimeout = 6 * time.Second
	qwenCookieMaxAttempts    = 3
)

// qwenCookieBrowserUserAgent keeps the console gateway treating the request
// like a real browser call; some endpoints 403 obvious non-browser clients.
const qwenCookieBrowserUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36"

// URLs are variables so tests can point the client at an httptest server.
var (
	qwenCookieDataGateway  = "https://bailian-singapore-cs.alibabacloud.com"
	qwenCookieDashboardURL = "https://modelstudio.console.alibabacloud.com/ap-southeast-1/?tab=plan#/efm/subscription/token-plan/personal"
	qwenCookieUserInfoURL  = "https://modelstudio.console.alibabacloud.com/tool/user/info.json"
)

// qwenCookieSession is one browser profile's cookie header. Profiles are
// kept separate: different profiles are usually different Alibaba accounts,
// and merging their cookies would pair one account's ticket with another's
// CSRF token.
type qwenCookieSession struct {
	header string
	source string // browser/profile label, used for cache keying
}

// qwenDiscoverCookieSessions reads the alibabacloud/aliyun session cookies
// from local browsers, one entry per browser profile carrying a login
// ticket. Platform-specific: sr_qwen_cookie_darwin.go installs the real
// implementation, sr_qwen_cookie_other.go a no-op stub. Tests override and
// restore it.
var qwenDiscoverCookieSessions func(ctx context.Context) []qwenCookieSession

var qwenSecTokenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`"secToken"\s*:\s*"([^"]+)"`),
	regexp.MustCompile(`"sec_token"\s*:\s*"([^"]+)"`),
	regexp.MustCompile(`secToken['"]?\s*[:=]\s*['"]([^'"]+)['"]`),
	regexp.MustCompile(`sec_token['"]?\s*[:=]\s*['"]([^'"]+)['"]`),
	regexp.MustCompile(`csrfToken['"]?\s*[:=]\s*['"]([^'"]+)['"]`),
	// The OneConsole shell embeds the token inside window.ALIYUN_CONSOLE_CONFIG
	// with an upper-case, unquoted key: `SEC_TOKEN: "<token>"`.
	regexp.MustCompile(`SEC_TOKEN['"]?\s*[:=]\s*['"]([^'"]+)['"]`),
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
	req.Header.Set("User-Agent", qwenCookieBrowserUserAgent)
	req.Header.Set("Cookie", cookieHeader)
	res, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return body, res.StatusCode, err
}

// qwenCookieDashboardGET fetches the console dashboard HTML the way a browser
// navigation would. The OneConsole shell only server-renders
// window.ALIYUN_CONSOLE_CONFIG.SEC_TOKEN for a genuine same-origin document
// navigation; a bare XHR-style request receives a token-less shell.
func qwenCookieDashboardGET(ctx context.Context, client *http.Client, rawURL, cookieHeader string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("User-Agent", qwenCookieBrowserUserAgent)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Cookie", cookieHeader)
	if parsed, perr := url.Parse(rawURL); perr == nil && parsed.Host != "" {
		req.Header.Set("Referer", "https://"+parsed.Host+"/")
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
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

// resolveQwenSecToken mirrors the console's token resolution: inline
// dashboard HTML first (freshest, navigation-style fetch), then a sec_token
// cookie, then the user-info JSON endpoint.
func resolveQwenSecToken(ctx context.Context, client *http.Client, cookieHeader string) (string, error) {
	if body, status, err := qwenCookieDashboardGET(ctx, client, qwenCookieDashboardURL, cookieHeader); err == nil && status == http.StatusOK {
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
		"consoleSite": qwenCookieConsoleSite,
		// 3 = personal/solo workspace; the gateway resolves the session's
		// default workspace from this. A hardcoded switchAgent would bind the
		// call to one account's workspace and fail every other account.
		"switchUserType":    3,
		"domain":            dashboardURL.Host,
		"feURL":             qwenCookieDashboardURL,
		"userNickName":      "",
		"userPrincipalName": "",
		"xsp_lang":          qwenCookieLanguage,
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
		"product":  {qwenCookieConsoleProduct},
		"action":   {qwenCookieConsoleAction},
		"region":   {qwenCookieRegion},
		"language": {qwenCookieLanguage},
		"params":   {string(paramsJSON)},
	}
	// The Personal gateway accepts cookie-only requests for some accounts but
	// rejects others unless the browser's sec_token is present; send it when
	// resolved, continue without it otherwise.
	if secToken != "" {
		form.Set("sec_token", secToken)
	}
	endpoint := qwenCookieDataGateway + "/data/api.json?action=" + qwenCookieConsoleAction +
		"&product=" + qwenCookieConsoleProduct + "&api=" + url.QueryEscape(qwenCookieUsageAPI) + "&_v=undefined"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", qwenCookieBrowserUserAgent)
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

// qwenCookieQuota is one cookie-path reading: the account identity when it
// could be resolved (exact email from the passport last-username cookie,
// masked email from login_aliyunid, Havana member ID), plus the quota
// windows.
type qwenCookieQuota struct {
	email       string
	maskedEmail string
	memberID    string
	windows     []accounts.UsageWindow
}

// identityKey dedups sessions that are the same Alibaba account logged into
// two browsers or profiles.
func (q qwenCookieQuota) identityKey() string {
	if q.email != "" {
		return q.email
	}
	if q.memberID != "" {
		return "member:" + q.memberID
	}
	if q.maskedEmail != "" {
		return "masked:" + q.maskedEmail
	}
	return ""
}

// qwenCookieSessionIdentity extracts the account identity embedded in the
// passport cookies. The last-username cookie (last_u_*) carries a base64 JSON
// blob with the exact loginId; login_aliyunid carries a masked email
// ("thegenerous****@gmail.com"); havana_tgc carries the Havana member ID, a
// stable per-account identifier that works even when no email exists (e.g.
// SSO logins that never set a username cookie).
func qwenCookieSessionIdentity(header string) (email, maskedEmail, memberID string) {
	for _, pair := range qwenCookiePairs(header) {
		name := pair[0]
		value := pair[1]
		switch {
		case strings.HasPrefix(name, "last_u_"):
			if id, hid := qwenDecodeLastUsernameCookie(value); id != "" {
				email = id
				if hid != "" {
					memberID = hid
				}
			}
		case name == "login_aliyunid":
			if strings.Contains(value, "@") {
				if strings.Contains(value, "*") {
					maskedEmail = strings.ToLower(value)
				} else if email == "" {
					email = strings.ToLower(value)
				}
			}
		case name == "havana_tgc":
			if memberID == "" {
				memberID = qwenDecodeHavanaMemberID(value)
			}
		}
	}
	return email, maskedEmail, memberID
}

// qwenDecodeLastUsernameCookie decodes the passport last-username cookie: a
// base64 JSON blob like {"hid":270612222595,"loginId":"user@example.com"}.
func qwenDecodeLastUsernameCookie(value string) (loginID, hid string) {
	parsed, ok := qwenDecodeBase64JSON(value)
	if !ok {
		return "", ""
	}
	if raw, ok := parsed["loginId"].(string); ok {
		loginID = strings.ToLower(strings.TrimSpace(raw))
		if !strings.Contains(loginID, "@") {
			loginID = ""
		}
	}
	if num, ok := parsed["hid"].(float64); ok && num > 0 {
		hid = strconv.FormatInt(int64(num), 10)
	}
	return loginID, hid
}

// qwenDecodeHavanaMemberID decodes the havana_tgc ticket-granting cookie and
// returns the account's member ID from its first accInfos entry.
func qwenDecodeHavanaMemberID(value string) string {
	parsed, ok := qwenDecodeBase64JSON(value)
	if !ok {
		return ""
	}
	partial, ok := parsed["patialTgc"].(map[string]any)
	if !ok {
		return ""
	}
	infos, ok := partial["accInfos"].(map[string]any)
	if !ok {
		return ""
	}
	for _, raw := range infos {
		info, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if num, ok := info["memberId"].(float64); ok && num > 0 {
			return strconv.FormatInt(int64(num), 10)
		}
	}
	return ""
}

// qwenDecodeBase64JSON decodes a base64 (standard or raw-URL) JSON object.
func qwenDecodeBase64JSON(value string) (map[string]any, bool) {
	var decoded []byte
	var err error
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawURLEncoding, base64.URLEncoding} {
		if decoded, err = encoding.DecodeString(value); err == nil {
			break
		}
	}
	if err != nil {
		return nil, false
	}
	var parsed map[string]any
	if json.Unmarshal(decoded, &parsed) != nil {
		return nil, false
	}
	return parsed, true
}

// qwenMaskedEmailMatches reports whether a masked console email
// ("thegenerous****@gmail.com") can only be the given account email.
func qwenMaskedEmailMatches(masked, email string) bool {
	if masked == "" || !strings.Contains(masked, "*") {
		return false
	}
	maskLocal, maskDomain, ok := strings.Cut(masked, "@")
	if !ok {
		return false
	}
	local, domain, ok := strings.Cut(email, "@")
	if !ok || !strings.EqualFold(domain, maskDomain) {
		return false
	}
	prefix, suffix, _ := strings.Cut(maskLocal, "*")
	suffix = strings.TrimLeft(suffix, "*")
	return strings.HasPrefix(local, prefix) && strings.HasSuffix(local, suffix) &&
		len(local) >= len(prefix)+len(suffix)+1
}

// qwenCookieQuotasFresh fetches quota through each discovered browser cookie
// session. The disk cache (keyed by browser profile) serves repeat runs
// without network calls; fresh reports whether any reading came from the
// network this call. A session whose console login has expired is skipped —
// and its cache entry dropped — so a dead profile never blocks a live one.
func qwenCookieQuotasFresh(ctx context.Context) ([]qwenCookieQuota, bool, error) {
	cache := loadQwenCookieQuotaCache()
	pruneQwenCookieQuotaCache(&cache)
	if qwenDiscoverCookieSessions == nil {
		if quotas := cachedQwenCookieQuotas(cache); len(quotas) > 0 {
			return quotas, false, nil
		}
		return nil, false, errors.New("qwen cookie discovery unavailable")
	}
	sessions := qwenDiscoverCookieSessions(ctx)
	if len(sessions) == 0 {
		if quotas := cachedQwenCookieQuotas(cache); len(quotas) > 0 {
			return quotas, false, nil
		}
		return nil, false, errors.New("no qwen browser session")
	}
	client := qwenCookieHTTPClient()
	var quotas []qwenCookieQuota
	seenIdentities := map[string]bool{}
	fresh := false
	for _, session := range sessions {
		if ctx.Err() != nil {
			break
		}
		cacheKey := "source:" + session.source
		if entry, ok := cache.Accounts[cacheKey]; ok && len(entry.Windows) > 0 && time.Since(entry.FetchedAt) < qwenCookieCacheTTL {
			quota := qwenCookieQuota{email: entry.Email, windows: entry.Windows}
			if key := quota.identityKey(); key == "" || !seenIdentities[key] {
				seenIdentities[key] = true
				quotas = append(quotas, quota)
			}
			continue
		}
		email, maskedEmail, memberID := qwenCookieSessionIdentity(session.header)
		if email == "" && maskedEmail == "" && memberID == "" {
			// Some console variants expose the account email on the user-info
			// endpoint; try it before declaring the session anonymous.
			email = qwenCookieAccountEmail(ctx, client, session.header)
		}
		quota := qwenCookieQuota{email: email, maskedEmail: maskedEmail, memberID: memberID}
		if key := quota.identityKey(); key != "" && seenIdentities[key] {
			// Same Alibaba account logged into two browsers/profiles.
			continue
		}
		// Best-effort: the Personal gateway accepts cookie-only requests for
		// some accounts and only rejects others without the browser's
		// sec_token.
		secToken, _ := resolveQwenSecToken(ctx, client, session.header)
		windows, err := qwenCookieFetchUsageRetried(ctx, client, session.header, secToken)
		if err != nil {
			if qwenCookieSessionExpired(err) {
				delete(cache.Accounts, cacheKey)
				saveQwenCookieQuotaCache(cache)
			}
			continue
		}
		if len(windows) == 0 {
			continue
		}
		cache.Accounts[cacheKey] = qwenCookieQuotaCacheEntry{Email: email, Windows: windows, FetchedAt: time.Now()}
		saveQwenCookieQuotaCache(cache)
		quota.windows = windows
		quotas = append(quotas, quota)
		seenIdentities[quota.identityKey()] = true
		fresh = true
	}
	if len(quotas) == 0 {
		return nil, fresh, errors.New("no live qwen browser session")
	}
	return quotas, fresh, nil
}

// cachedQwenCookieQuotas serves every still-fresh cache entry, for callers
// that cannot or could not run discovery.
func cachedQwenCookieQuotas(cache qwenCookieQuotaCacheFile) []qwenCookieQuota {
	var quotas []qwenCookieQuota
	seen := map[string]bool{}
	for _, entry := range cache.Accounts {
		if len(entry.Windows) == 0 || time.Since(entry.FetchedAt) >= qwenCookieCacheTTL {
			continue
		}
		quota := qwenCookieQuota{email: entry.Email, windows: entry.Windows}
		if key := quota.identityKey(); key != "" && seen[key] {
			continue
		}
		seen[quota.identityKey()] = true
		quotas = append(quotas, quota)
	}
	return quotas
}

// pruneQwenCookieQuotaCache drops day-old entries so dead sessions and
// pre-rename cache keys cannot linger in the file forever.
func pruneQwenCookieQuotaCache(cache *qwenCookieQuotaCacheFile) {
	for key, entry := range cache.Accounts {
		if time.Since(entry.FetchedAt) > 24*time.Hour {
			delete(cache.Accounts, key)
		}
	}
}

// qwenCookieSessionExpired reports whether the gateway rejected the cookie
// session as logged out. Workspace-permission failures
// (BailianGateway.Workspace.NotAuthorised) are deliberately excluded: the
// session is alive there, and evicting it would re-fail identically forever.
func qwenCookieSessionExpired(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "notlogined") ||
		strings.Contains(msg, "needlogin") ||
		strings.Contains(msg, "request has expired")
}

// qwenCookieFetchUsageRetried calls the usage API past the gateway's
// intermittent empty-payload quirk: it sometimes answers 200 "Success" with
// no rolling-window fields, and an immediate re-request usually returns them.
func qwenCookieFetchUsageRetried(ctx context.Context, client *http.Client, cookieHeader, secToken string) ([]accounts.UsageWindow, error) {
	var windows []accounts.UsageWindow
	var err error
	for attempt := 0; attempt < qwenCookieMaxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(400 * time.Millisecond):
			}
		}
		windows, err = qwenCookieFetchUsage(ctx, client, cookieHeader, secToken)
		if err == nil {
			return windows, nil
		}
		if !strings.Contains(err.Error(), "payload not found") {
			return nil, err
		}
	}
	return nil, err
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

// enrichQwenRowsWithCookieQuotaFresh overlays cookie-derived quota windows on
// Qwen rows whose quota is unknown. Each browser profile is resolved
// independently, so multiple accounts each get their own session's windows.
// Matching, in order: exact email, unique masked email, the single-account
// assumption (one needy row and one session with at most one side
// identified), and elimination (sessions and rows pair off 1:1 with one of
// each left). When both identities are known and differ, we never guess. Any
// per-session failure leaves that account's row untouched. Rows that get
// matched anonymously backfill the quota's email from the row's console
// account label so the reading can fan out to the server.
func enrichQwenRowsWithCookieQuotaFresh(ctx context.Context, rows []srUsageRow) (quotas []qwenCookieQuota, applied, fresh bool) {
	needy := make([]int, 0, 2)
	for i, row := range rows {
		if qwenRowNeedsCookieQuota(row) {
			needy = append(needy, i)
		}
	}
	if len(needy) == 0 {
		return nil, false, false
	}
	enrichCtx, cancel := context.WithTimeout(ctx, claudeWebEnrichTimeout)
	defer cancel()
	quotas, fresh, err := qwenCookieQuotasFresh(enrichCtx)
	if len(quotas) == 0 || err != nil && len(quotas) == 0 {
		return nil, false, false
	}
	matchedQuota := map[int]bool{}
	matchedRow := map[int]bool{}
	applyMatch := func(qi, i int) {
		if key := qwenRowAccountKey(rows[i]); key != "" && quotas[qi].email == "" {
			quotas[qi].email = key
		}
		applyQwenCookieQuota(&rows[i], quotas[qi].windows)
		matchedQuota[qi] = true
		matchedRow[i] = true
		applied = true
	}
	// Exact email matches.
	for _, i := range needy {
		key := qwenRowAccountKey(rows[i])
		if key == "" {
			continue
		}
		for qi := range quotas {
			if quotas[qi].email != "" && quotas[qi].email == key {
				applyMatch(qi, i)
				break
			}
		}
	}
	// Masked-email matches ("thegenerous****@gmail.com"), unique candidate only.
	for _, i := range needy {
		if matchedRow[i] {
			continue
		}
		key := qwenRowAccountKey(rows[i])
		if key == "" {
			continue
		}
		match := -1
		for qi := range quotas {
			if matchedQuota[qi] {
				continue
			}
			if qwenMaskedEmailMatches(quotas[qi].maskedEmail, key) {
				if match != -1 {
					match = -1
					break
				}
				match = qi
			}
		}
		if match >= 0 {
			applyMatch(match, i)
		}
	}
	// Single-account assumption: one needy row, one session, at most one of
	// the two identities known.
	if len(needy) == 1 && len(quotas) == 1 && !matchedRow[needy[0]] {
		if quotas[0].email == "" || qwenRowAccountKey(rows[needy[0]]) == "" {
			applyMatch(0, needy[0])
		}
	}
	// Elimination: sessions and needy rows pair off 1:1 with exactly one of
	// each left, and the leftover session's account is not positively known
	// to be a different one. A session count above the row count (an account
	// subrouter does not manage) disables this, so foreign quota is never
	// shown on a managed row.
	if len(quotas) == len(needy) {
		unmatchedQuota, unmatchedQuotaCount := -1, 0
		for qi := range quotas {
			if !matchedQuota[qi] {
				unmatchedQuota = qi
				unmatchedQuotaCount++
			}
		}
		unmatchedRow, unmatchedRowCount := -1, 0
		for _, i := range needy {
			if !matchedRow[i] {
				unmatchedRow = i
				unmatchedRowCount++
			}
		}
		if unmatchedQuotaCount == 1 && unmatchedRowCount == 1 &&
			(quotas[unmatchedQuota].email == "" || qwenRowAccountKey(rows[unmatchedRow]) == "") {
			applyMatch(unmatchedQuota, unmatchedRow)
		}
	}
	if !applied {
		return nil, false, false
	}
	return quotas, true, fresh
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
