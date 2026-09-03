//go:build darwin

package antigravity

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireNativeProfileRestoresExactOriginalBlob(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "agy.lock")
	original := KeychainEntry{Account: "antigravity", Blob: []byte("original-opaque-blob")}
	current := original
	read := func(context.Context) (KeychainEntry, bool, error) { return current, true, nil }
	write := func(_ context.Context, entry KeychainEntry) error {
		current = KeychainEntry{Account: entry.Account, Blob: append([]byte(nil), entry.Blob...)}
		return nil
	}
	delete := func(context.Context, string) error { current = KeychainEntry{}; return nil }
	lease, err := acquireNativeProfileWith(context.Background(), lockPath, CredentialInfo{AccessToken: "a", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour)}, read, write, delete)
	if err != nil {
		t.Fatal(err)
	}
	if string(current.Blob) == string(original.Blob) || current.Account != original.Account {
		t.Fatalf("profile was not installed: %+v", current)
	}
	if err := lease.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if string(current.Blob) != string(original.Blob) || current.Account != original.Account {
		t.Fatalf("restored entry = %+v, want exact original", current)
	}
	if err := lease.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatal(err)
	}
}
