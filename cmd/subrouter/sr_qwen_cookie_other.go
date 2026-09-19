//go:build !darwin

package main

import "context"

// Browser cookie reading is macOS-only; everywhere else discovery is a no-op
// and Qwen rows render exactly as the server/console data produced them.
func init() {
	qwenDiscoverCookieHeader = func(context.Context) (string, string, bool) {
		return "", "", false
	}
}
