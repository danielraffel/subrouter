//go:build darwin

package main

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// Qwen Cloud (home.qwencloud.com) shares the Aliyun one-console auth backend,
// so its session cookies live on qwencloud.com plus the alibabacloud/aliyun
// passport domains. A profile counts as authenticated only when it carries a
// login ticket cookie — locale markers and CSRF tokens exist in logged-out
// profiles too.
var qwenCookieHostLikes = []string{"%qwencloud.com", "%alibabacloud.com", "%aliyun.com"}

var qwenCookieTicketNames = []string{"login_aliyunid_ticket", "login_qwencloud_ticket", "qwen_sso_ticket"}

func init() {
	qwenDiscoverCookieHeader = discoverQwenCloudCookieHeader
}

// domainCookie is one browser cookie record with the host it was stored for.
type domainCookie struct {
	name  string
	host  string
	value string
}

func discoverQwenCloudCookieHeader(ctx context.Context) (string, string, bool) {
	home, err := claudeWebHomeDir()
	if err != nil {
		return "", "", false
	}
	var cookies []domainCookie
	source := ""
	for _, browser := range claudeWebChromiumBrowsers {
		if ctx.Err() != nil {
			return "", "", false
		}
		if found := chromiumDomainCookies(ctx, home, browser, qwenCookieHostLikes); qwenCookieSessionValid(found) {
			cookies = found
			source = browser.name
			break
		}
	}
	if len(cookies) == 0 {
		for _, family := range claudeWebFirefoxFamilies {
			if ctx.Err() != nil {
				return "", "", false
			}
			if found := firefoxDomainCookies(ctx, home, family, qwenCookieHostLikes); qwenCookieSessionValid(found) {
				cookies = found
				source = family.name
				break
			}
		}
	}
	if len(cookies) == 0 {
		if found := safariDomainCookies(home, qwenCookieHostLikes); qwenCookieSessionValid(found) {
			cookies = found
			source = "Safari"
		}
	}
	if len(cookies) == 0 {
		return "", "", false
	}
	return qwenCookieHeaderString(cookies), source, true
}

// qwenCookieSessionValid reports whether the cookie set carries an
// authenticated-session ticket.
func qwenCookieSessionValid(cookies []domainCookie) bool {
	for _, cookie := range cookies {
		for _, ticket := range qwenCookieTicketNames {
			if cookie.name == ticket && cookie.value != "" {
				return true
			}
		}
	}
	return false
}

// qwenCookieHeaderString builds a Cookie header, deduping by name with
// qwencloud.com cookies preferred over passport-domain duplicates.
func qwenCookieHeaderString(cookies []domainCookie) string {
	byName := map[string]domainCookie{}
	var order []string
	for _, cookie := range cookies {
		if cookie.name == "" || cookie.value == "" || strings.ContainsAny(cookie.name, "; \t\r\n") {
			continue
		}
		existing, ok := byName[cookie.name]
		if !ok {
			byName[cookie.name] = cookie
			order = append(order, cookie.name)
			continue
		}
		if strings.Contains(cookie.host, "qwencloud.com") && !strings.Contains(existing.host, "qwencloud.com") {
			byName[cookie.name] = cookie
		}
	}
	parts := make([]string, 0, len(order))
	for _, name := range order {
		cookie := byName[name]
		parts = append(parts, name+"="+cookie.value)
	}
	return strings.Join(parts, "; ")
}

// chromiumDomainCookies reads and decrypts all cookies whose host matches any
// of the given SQL LIKE patterns. The Keychain is only touched once such a
// cookie actually exists.
func chromiumDomainCookies(ctx context.Context, home string, browser claudeWebChromiumBrowser, hostLikes []string) []domainCookie {
	dbs, _ := filepath.Glob(filepath.Join(home, browser.profileRoot, "*", "Cookies"))
	if len(dbs) == 0 {
		return nil
	}
	var key []byte
	var out []domainCookie
	for _, db := range dbs {
		if ctx.Err() != nil {
			return out
		}
		rows, err := claudeWebQuerySQLite(ctx, db,
			"SELECT hex(name), host_key, hex(encrypted_value) FROM cookies WHERE "+sqlHostLikeClause("host_key", hostLikes))
		if err != nil || len(rows) == 0 {
			continue
		}
		if key == nil {
			password, err := claudeWebKeychainPassword(ctx, browser.keychainService)
			if err != nil {
				return out
			}
			key = pbkdf2SHA1([]byte(password), []byte("saltysalt"), 1003, 16)
		}
		for _, row := range rows {
			parts := strings.SplitN(row, "|", 3)
			if len(parts) != 3 {
				continue
			}
			nameBytes, err := hex.DecodeString(strings.TrimSpace(parts[0]))
			if err != nil {
				continue
			}
			host := strings.TrimSpace(parts[1])
			encrypted, err := hex.DecodeString(strings.TrimSpace(parts[2]))
			if err != nil {
				continue
			}
			value, err := decryptChromiumCookieValueForHost(encrypted, key, host)
			if err != nil {
				continue
			}
			out = append(out, domainCookie{name: string(nameBytes), host: host, value: value})
		}
	}
	return out
}

func firefoxDomainCookies(ctx context.Context, home string, family claudeWebFirefoxFamily, hostLikes []string) []domainCookie {
	dbs, _ := filepath.Glob(filepath.Join(home, family.profilesRoot, "*", "cookies.sqlite"))
	var out []domainCookie
	for _, db := range dbs {
		if ctx.Err() != nil {
			return out
		}
		rows, err := claudeWebQuerySQLite(ctx, db,
			"SELECT hex(name), host, hex(value) FROM moz_cookies WHERE "+sqlHostLikeClause("host", hostLikes))
		if err != nil {
			continue
		}
		for _, row := range rows {
			parts := strings.SplitN(row, "|", 3)
			if len(parts) != 3 {
				continue
			}
			nameBytes, err := hex.DecodeString(strings.TrimSpace(parts[0]))
			if err != nil {
				continue
			}
			valueBytes, err := hex.DecodeString(strings.TrimSpace(parts[2]))
			if err != nil {
				continue
			}
			out = append(out, domainCookie{name: string(nameBytes), host: strings.TrimSpace(parts[1]), value: string(valueBytes)})
		}
	}
	return out
}

func safariDomainCookies(home string, hostLikes []string) []domainCookie {
	var out []domainCookie
	contains := make([]string, 0, len(hostLikes))
	for _, like := range hostLikes {
		contains = append(contains, strings.Trim(like, "%"))
	}
	for _, rel := range claudeWebSafariCookieFiles {
		data, err := os.ReadFile(filepath.Join(home, rel))
		if err != nil {
			continue
		}
		for _, cookie := range parseBinaryCookies(data) {
			for _, domain := range contains {
				if strings.Contains(cookie.domain, domain) {
					out = append(out, domainCookie{name: cookie.name, host: cookie.domain, value: cookie.value})
					break
				}
			}
		}
	}
	return out
}

// sqlHostLikeClause builds "col LIKE 'a' OR col LIKE 'b'" from constant
// patterns owned by this package (never user input).
func sqlHostLikeClause(column string, hostLikes []string) string {
	parts := make([]string, 0, len(hostLikes))
	for _, like := range hostLikes {
		parts = append(parts, column+" LIKE '"+like+"'")
	}
	return strings.Join(parts, " OR ")
}
