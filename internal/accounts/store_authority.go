package accounts

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
)

// StoreAuthorityID identifies one resolved account-store path without exposing
// that path through the daemon's unauthenticated health response.
func StoreAuthorityID(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("resolve account store: path is empty")
	}
	resolved, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve account store: %w", err)
	}
	digest := sha256.Sum256([]byte("subrouter-account-store-v1\x00" + filepath.Clean(resolved)))
	return hex.EncodeToString(digest[:]), nil
}
