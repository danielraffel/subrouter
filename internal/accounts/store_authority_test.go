package accounts

import (
	"crypto/hmac"
	"encoding/hex"
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

func TestStoreAuthorityProofIsSharedOnlyByTheSameStore(t *testing.T) {
	challenge := hex.EncodeToString(make([]byte, 32))
	store := filepath.Join(t.TempDir(), "codex", "accounts")
	first, err := StoreAuthorityProof(store, challenge)
	if err != nil {
		t.Fatal(err)
	}
	second, err := StoreAuthorityProof(filepath.Join(filepath.Dir(store), ".", "accounts"), challenge)
	if err != nil {
		t.Fatal(err)
	}
	if !hmac.Equal([]byte(first), []byte(second)) {
		t.Fatal("the same account store produced different proofs")
	}
	other, err := StoreAuthorityProof(filepath.Join(t.TempDir(), "codex", "accounts"), challenge)
	if err != nil {
		t.Fatal(err)
	}
	if hmac.Equal([]byte(first), []byte(other)) {
		t.Fatal("different account stores produced the same proof")
	}
}

func TestStoreAuthorityProofRejectsMalformedChallenge(t *testing.T) {
	if _, err := StoreAuthorityProof(filepath.Join(t.TempDir(), "accounts"), "short"); err == nil {
		t.Fatal("malformed account-store challenge unexpectedly succeeded")
	}
}

func TestStoreAuthorityIDRejectsEmptyPath(t *testing.T) {
	if _, err := StoreAuthorityID(""); err == nil {
		t.Fatal("empty account store unexpectedly received an authority ID")
	}
}
