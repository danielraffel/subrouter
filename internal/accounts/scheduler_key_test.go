package accounts

import "testing"

func TestSchedulerKeyProviderScoped(t *testing.T) {
	codex := Account{ID: "user@example.com", Provider: ProviderCodex}
	claude := Account{ID: "user@example.com", Provider: ProviderClaude}

	if codex.SchedulerKey() == claude.SchedulerKey() {
		t.Fatalf("codex and claude accounts with the same ID share a scheduler key: %q", codex.SchedulerKey())
	}
	if got, want := codex.SchedulerKey(), "codex:user@example.com"; got != want {
		t.Fatalf("codex SchedulerKey = %q, want %q", got, want)
	}
	if got, want := claude.SchedulerKey(), "claude:user@example.com"; got != want {
		t.Fatalf("claude SchedulerKey = %q, want %q", got, want)
	}
}

func TestSchedulerKeyLegacyEmptyProviderFallsBackToID(t *testing.T) {
	legacy := Account{ID: "user@example.com"}
	if got, want := legacy.SchedulerKey(), "user@example.com"; got != want {
		t.Fatalf("legacy SchedulerKey = %q, want bare ID %q", got, want)
	}
	if got := SchedulerKey("", "user@example.com"); got != "user@example.com" {
		t.Fatalf("SchedulerKey with empty provider = %q, want bare ID", got)
	}
}
