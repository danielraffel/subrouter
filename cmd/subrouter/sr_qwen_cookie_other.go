//go:build !darwin

package main

import "context"

// Browser cookie reading is macOS-only; everywhere else discovery is a no-op
// and Qwen rows render exactly as the server/console data produced them.
func init() {
	qwenDiscoverCookieSessions = func(context.Context) []qwenCookieSession {
		return nil
	}
}
