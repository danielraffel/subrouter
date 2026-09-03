//go:build darwin

package antigravity

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const keychainWriteTimeout = 10 * time.Second

const nativeProfileLockPoll = 50 * time.Millisecond

func acquireNativeProfile(ctx context.Context, lockPath string, credential CredentialInfo) (*NativeProfileLease, error) {
	return acquireNativeProfileWith(ctx, lockPath, credential, readLocalKeychainEntry, writeLocalKeychainEntry, deleteLocalKeychainEntry)
}

type keychainReader func(context.Context) (KeychainEntry, bool, error)
type keychainWriter func(context.Context, KeychainEntry) error
type keychainDeleter func(context.Context, string) error

func acquireNativeProfileWith(ctx context.Context, lockPath string, credential CredentialInfo, read keychainReader, write keychainWriter, deleteEntry keychainDeleter) (*NativeProfileLease, error) {
	if strings.TrimSpace(lockPath) == "" {
		return nil, errors.New("Antigravity native profile lock path is required")
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, fmt.Errorf("create Antigravity profile lock directory: %w", err)
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open Antigravity profile lock: %w", err)
	}
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = lock.Close()
			return nil, fmt.Errorf("lock Antigravity profile slot: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = lock.Close()
			return nil, ctx.Err()
		case <-time.After(nativeProfileLockPoll):
		}
	}
	original, hadOriginal, err := read(ctx)
	if err != nil {
		_ = unlockAndClose(lock)
		return nil, err
	}
	blob, err := EncodeCredential(credential)
	if err != nil {
		_ = unlockAndClose(lock)
		return nil, err
	}
	target := original
	if !hadOriginal || strings.TrimSpace(target.Account) == "" {
		target.Account = keychainAccount
	}
	target.Blob = blob
	if err := write(ctx, target); err != nil {
		_ = unlockAndClose(lock)
		return nil, err
	}
	return &NativeProfileLease{restore: func(restoreCtx context.Context) error {
		defer func() { _ = unlockAndClose(lock) }()
		if hadOriginal {
			return write(restoreCtx, original)
		}
		return deleteEntry(restoreCtx, target.Account)
	}}, nil
}

func unlockAndClose(lock *os.File) error {
	if lock == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	closeErr := lock.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

func readLocalKeychainEntry(ctx context.Context) (KeychainEntry, bool, error) {
	current, err := user.Current()
	if err != nil {
		return KeychainEntry{}, false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, keychainReadTimeout)
	defer cancel()
	for _, account := range []string{current.Username, keychainAccount} {
		cmd := exec.CommandContext(ctx, "security", "find-generic-password", "-s", keychainService, "-a", account, "-w")
		body, runErr := cmd.Output()
		if runErr == nil && len(bytes.TrimSpace(body)) > 0 {
			return KeychainEntry{Account: account, Blob: bytes.TrimSpace(body)}, true, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return KeychainEntry{}, false, ctxErr
		}
		var exitErr *exec.ExitError
		if runErr != nil && (!errors.As(runErr, &exitErr) || exitErr.ExitCode() != 44) {
			return KeychainEntry{}, false, fmt.Errorf("read Antigravity keychain item: %w", runErr)
		}
	}
	return KeychainEntry{}, false, nil
}

func writeLocalKeychainEntry(ctx context.Context, entry KeychainEntry) error {
	account := strings.TrimSpace(entry.Account)
	if account == "" {
		account = keychainAccount
	}
	if len(entry.Blob) == 0 {
		return errors.New("Antigravity Keychain blob is empty")
	}
	ctx, cancel := context.WithTimeout(ctx, keychainWriteTimeout)
	defer cancel()
	// Omitting the -w argument makes `security` read the secret from stdin;
	// this avoids exposing the OAuth blob in process arguments.
	cmd := exec.CommandContext(ctx, "security", "add-generic-password", "-U", "-s", keychainService, "-a", account, "-w")
	cmd.Stdin = bytes.NewReader(entry.Blob)
	if output, err := cmd.CombinedOutput(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("write Antigravity keychain item: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func deleteLocalKeychainEntry(ctx context.Context, account string) error {
	account = strings.TrimSpace(account)
	if account == "" {
		account = keychainAccount
	}
	ctx, cancel := context.WithTimeout(ctx, keychainWriteTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "security", "delete-generic-password", "-s", keychainService, "-a", account)
	if output, err := cmd.CombinedOutput(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 44 {
			return nil
		}
		return fmt.Errorf("delete Antigravity keychain item: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}
