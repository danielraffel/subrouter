package accounts

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const StoreAuthorityChallengeHeader = "X-Subrouter-Store-Challenge"

const storeAuthorityKeyFilename = ".store-authority-key"

// StoreAuthorityID identifies one resolved account-store path without exposing
// that path through the daemon's unauthenticated health response.
func StoreAuthorityID(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("resolve account store: path is empty")
	}
	resolved, err := resolveStoreAuthorityPath(path)
	if err != nil {
		return "", fmt.Errorf("resolve account store: %w", err)
	}
	digest := sha256.Sum256([]byte("subrouter-account-store-v1\x00" + resolved))
	return hex.EncodeToString(digest[:]), nil
}

// resolveStoreAuthorityPath resolves every existing symlinked ancestor while
// preserving a not-yet-created account-store suffix. A daemon and CLI may name
// the same state root through different stable aliases; the shared authority
// key and store ID must agree in that case.
func resolveStoreAuthorityPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := filepath.Clean(absolute)
	missing := make([]string, 0, 2)
	for {
		resolved, resolveErr := filepath.EvalSymlinks(current)
		if resolveErr == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(resolveErr, os.ErrNotExist) {
			return "", resolveErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", resolveErr
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

// StoreAuthorityProof proves that a process can read the same account-store
// authority key without disclosing that key through the health endpoint.
func StoreAuthorityProof(path, challenge string) (string, error) {
	challengeBytes, err := hex.DecodeString(strings.TrimSpace(challenge))
	if err != nil || len(challengeBytes) != 32 {
		return "", fmt.Errorf("invalid account-store challenge")
	}
	key, err := ensureStoreAuthorityKey(path)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(challengeBytes)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func ensureStoreAuthorityKey(path string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("resolve account store authority: path is empty")
	}
	resolved, err := resolveStoreAuthorityPath(path)
	if err != nil {
		return nil, fmt.Errorf("resolve account store authority: %w", err)
	}
	parent := filepath.Dir(resolved)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, fmt.Errorf("create account store authority directory: %w", err)
	}
	keyPath := filepath.Join(parent, storeAuthorityKeyFilename)
	if key, err := readStoreAuthorityKey(keyPath); err == nil {
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("create account store authority key: %w", err)
	}
	temp, err := os.CreateTemp(parent, ".store-authority-key-*")
	if err != nil {
		return nil, fmt.Errorf("stage account store authority key: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return nil, fmt.Errorf("restrict staged account store authority key: %w", err)
	}
	if _, err := temp.Write(key); err != nil {
		_ = temp.Close()
		return nil, fmt.Errorf("write staged account store authority key: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return nil, fmt.Errorf("sync staged account store authority key: %w", err)
	}
	if err := temp.Close(); err != nil {
		return nil, fmt.Errorf("close staged account store authority key: %w", err)
	}
	if err := os.Link(tempPath, keyPath); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("publish account store authority key: %w", err)
		}
		return readStoreAuthorityKey(keyPath)
	}
	return key, nil
}

func readStoreAuthorityKey(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("account store authority key must be a private regular file")
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read account store authority key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("account store authority key has invalid length")
	}
	return key, nil
}
