//go:build darwin

package main

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// The Qwen Token Plan lives on the Model Studio console
// (modelstudio.console.alibabacloud.com), which shares the Aliyun
// one-console auth backend, so its session cookies live on the
// alibabacloud/aliyun domains (plus qwencloud.com for federated accounts). A
// profile counts as authenticated only when it carries a login ticket cookie
// — locale markers and CSRF tokens exist in logged-out profiles too. Each
// browser profile is its own session: different profiles are typically
// different Alibaba accounts, and merging their cookies would pair one
// account's ticket with another's CSRF token.
var qwenCookieHostLikes = []string{"%qwencloud.com", "%alibabacloud.com", "%aliyun.com"}

var qwenCookieTicketNames = []string{"login_aliyunid_ticket", "login_qwencloud_ticket", "qwen_sso_ticket"}

func init() {
	qwenDiscoverCookieSessions = discoverQwenCookieSessions
}

// domainCookie is one browser cookie record with the host it was stored for.
type domainCookie struct {
	name  string
	host  string
	value string
}

func discoverQwenCookieSessions(ctx context.Context) []qwenCookieSession {
	home, err := claudeWebHomeDir()
	if err != nil {
		return nil
	}
	var sessions []qwenCookieSession
	for _, browser := range claudeWebChromiumBrowsers {
		dbs, _ := filepath.Glob(filepath.Join(home, browser.profileRoot, "*", "Cookies"))
		var key []byte
		for _, db := range dbs {
			if ctx.Err() != nil {
				return sessions
			}
			found := chromiumProfileCookies(ctx, db, browser.keychainService, &key, qwenCookieHostLikes)
			if qwenCookieSessionValid(found) {
				sessions = append(sessions, qwenCookieSession{
					header: qwenCookieHeaderString(found),
					source: browser.name + "/" + filepath.Base(filepath.Dir(db)),
				})
			}
		}
	}
	for _, family := range claudeWebFirefoxFamilies {
		dbs, _ := filepath.Glob(filepath.Join(home, family.profilesRoot, "*", "cookies.sqlite"))
		for _, db := range dbs {
			if ctx.Err() != nil {
				return sessions
			}
			if found := firefoxProfileCookies(ctx, db, qwenCookieHostLikes); qwenCookieSessionValid(found) {
				sessions = append(sessions, qwenCookieSession{
					header: qwenCookieHeaderString(found),
					source: family.name + "/" + filepath.Base(filepath.Dir(db)),
				})
			}
		}
	}
	if ctx.Err() == nil {
		if found := safariDomainCookies(home, qwenCookieHostLikes); qwenCookieSessionValid(found) {
			sessions = append(sessions, qwenCookieSession{
				header: qwenCookieHeaderString(found),
				source: "Safari",
			})
		}
	}
	return sessions
}

// qwenCookieSessionValid reports whether the cookie set carries an
// authenticated-session ticket. A ticket is only a prefilter: tickets linger
// after logout, and the usage API's NotLogined response is the real verdict.
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

// chromiumProfileCookies reads and decrypts the matching cookies from one
// Chromium profile's Cookies database. The Keychain is only touched once per
// browser and only once such a cookie actually exists; the derived key is
// carried across profiles via key.
func chromiumProfileCookies(ctx context.Context, db, keychainService string, key *[]byte, hostLikes []string) []domainCookie {
	rows, err := claudeWebQuerySQLite(ctx, db,
		"SELECT hex(name), host_key, hex(encrypted_value) FROM cookies WHERE "+sqlHostLikeClause("host_key", hostLikes))
	if err != nil || len(rows) == 0 {
		return nil
	}
	if *key == nil {
		password, err := claudeWebKeychainPassword(ctx, keychainService)
		if err != nil {
			return nil
		}
		*key = pbkdf2SHA1([]byte(password), []byte("saltysalt"), 1003, 16)
	}
	var out []domainCookie
	for _, row := range rows {
		if ctx.Err() != nil {
			return out
		}
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
		value, err := decryptChromiumCookieValueForHost(encrypted, *key, host)
		if err != nil {
			continue
		}
		out = append(out, domainCookie{name: string(nameBytes), host: host, value: value})
	}
	return out
}

func firefoxProfileCookies(ctx context.Context, db string, hostLikes []string) []domainCookie {
	rows, err := claudeWebQuerySQLite(ctx, db,
		"SELECT hex(name), host, hex(value) FROM moz_cookies WHERE "+sqlHostLikeClause("host", hostLikes))
	if err != nil {
		return nil
	}
	var out []domainCookie
	for _, row := range rows {
		if ctx.Err() != nil {
			return out
		}
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
