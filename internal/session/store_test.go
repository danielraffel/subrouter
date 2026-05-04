package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCountByAccount(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "session-1", "account-a", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "session-2", "account-a", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("claude", "session-3", "account-b", ""); err != nil {
		t.Fatal(err)
	}

	counts := store.CountByAccount()
	if counts["account-a"] != 2 {
		t.Fatalf("account-a count = %d, want 2", counts["account-a"])
	}
	if counts["account-b"] != 1 {
		t.Fatalf("account-b count = %d, want 1", counts["account-b"])
	}
}

func TestPutPreservesExistingUserEmailWhenMissing(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "session-1", "account-a", "Alice@Example.COM"); err != nil {
		t.Fatal(err)
	}
	assignment, err := store.Put("codex", "session-1", "account-a", "")
	if err != nil {
		t.Fatal(err)
	}

	if assignment.UserEmail != "alice@example.com" {
		t.Fatalf("user email = %q, want alice@example.com", assignment.UserEmail)
	}
}

func TestStoreScopesAssignmentsByAgentType(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "same-session", "codex-account", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("claude", "same-session", "claude-account", ""); err != nil {
		t.Fatal(err)
	}

	codex, ok := store.Get("codex", "same-session")
	if !ok {
		t.Fatal("missing codex assignment")
	}
	claude, ok := store.Get("claude", "same-session")
	if !ok {
		t.Fatal("missing claude assignment")
	}
	if codex.AccountID != "codex-account" {
		t.Fatalf("codex AccountID = %q, want codex-account", codex.AccountID)
	}
	if claude.AccountID != "claude-account" {
		t.Fatalf("claude AccountID = %q, want claude-account", claude.AccountID)
	}
}

func TestStoreMigratesUnscopedAssignmentsToCodex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	if err := os.WriteFile(path, []byte(`{
  "old-session": {
    "session_id": "old-session",
    "account_id": "codex-account",
    "created_at": "2026-04-28T00:00:00Z",
    "updated_at": "2026-04-28T00:00:00Z"
  }
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}

	assignment, ok := store.Get("codex", "old-session")
	if !ok {
		t.Fatal("missing migrated assignment")
	}
	if assignment.AgentType != "codex" {
		t.Fatalf("AgentType = %q, want codex", assignment.AgentType)
	}
}
