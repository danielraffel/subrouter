package proxy

import (
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/wake"
)

func TestRecoveryTrackerSeparatesKindsAndRecordsActivity(t *testing.T) {
	tracker := NewRecoveryTracker()
	now := time.Now().UTC()
	tracker.RecordCapacityFailure("codex", "s1", "gpt", now)
	tracker.RecordQuotaFailure("claude", "s2", wake.KindClaudeQuota, "", now, now.Add(time.Hour))
	states := tracker.List(now)
	if len(states) != 2 {
		t.Fatalf("states=%d, want 2", len(states))
	}
	for _, state := range states {
		if state.Agent == "codex" && state.Kind != wake.KindCodexProvider {
			t.Fatalf("codex kind=%q", state.Kind)
		}
		if state.Agent == "claude" && (state.Kind != wake.KindClaudeQuota || state.ResetAt.IsZero()) {
			t.Fatalf("claude state=%+v", state)
		}
		if state.LastActivityAt.IsZero() {
			t.Fatalf("missing activity in %+v", state)
		}
	}
}
