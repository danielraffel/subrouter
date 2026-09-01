package accounts

import (
	"path/filepath"
	"testing"
)

func TestStoreAuthorityIDNormalizesTheSamePath(t *testing.T) {
	root := t.TempDir()
	left, err := StoreAuthorityID(filepath.Join(root, "codex", "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	right, err := StoreAuthorityID(filepath.Join(root, "codex", ".", "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	if left == "" || left != right {
		t.Fatalf("authority IDs differ: %q != %q", left, right)
	}
	other, err := StoreAuthorityID(filepath.Join(root, "other", "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	if other == left {
		t.Fatal("different account stores have the same authority ID")
	}
}

func TestStoreAuthorityIDRejectsEmptyPath(t *testing.T) {
	if _, err := StoreAuthorityID(""); err == nil {
		t.Fatal("empty account store unexpectedly received an authority ID")
	}
}
