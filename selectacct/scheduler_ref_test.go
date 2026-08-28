package selectacct

import (
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

func TestSchedulerRefAllowsOnlyOneStaleRefresh(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.SetUpdatedAt(time.Now().Add(-time.Hour))

	if !ref.BeginRefreshIfStale(time.Minute) {
		t.Fatal("first stale refresh should begin")
	}
	if ref.BeginRefreshIfStale(time.Minute) {
		t.Fatal("second stale refresh should be suppressed while refresh is running")
	}

	ref.FinishRefresh(NewScheduler([]Score{{AccountID: "fresh", Headroom: 1, ShortHeadroom: 1}}), true)
	if ref.BeginRefreshIfStale(time.Minute) {
		t.Fatal("freshly completed refresh should not immediately restart")
	}
}

func TestSchedulerRefRetryAfterSkippedRefreshWaitsForTTL(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.SetUpdatedAt(time.Now().Add(-time.Hour))

	if !ref.BeginRefreshIfStale(time.Minute) {
		t.Fatal("stale refresh should begin")
	}
	ref.FinishRefresh(Scheduler{}, false)

	if ref.BeginRefreshIfStale(time.Minute) {
		t.Fatal("skipped refresh should still touch updatedAt")
	}
	ref.SetUpdatedAt(time.Now().Add(-time.Hour))
	if !ref.BeginRefreshIfStale(time.Minute) {
		t.Fatal("refresh should be allowed after TTL passes")
	}
}

func TestSchedulerRefNewerSetInvalidatesOlderRefresh(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler([]Score{{
		AccountID: "old@example.com", Provider: account.ProviderCodex, Headroom: 0.9, ShortHeadroom: 0.9,
	}}))
	ref.SetUpdatedAt(time.Time{})
	if !ref.BeginRefreshIfStale(time.Minute) {
		t.Fatal("stale refresh should begin")
	}

	ref.Set(NewScheduler([]Score{{
		AccountID: "new@example.com", Provider: account.ProviderCodex, Headroom: 0.8, ShortHeadroom: 0.8,
	}}))
	ref.FinishRefresh(NewScheduler([]Score{{
		AccountID: "old@example.com", Provider: account.ProviderCodex, Headroom: 0.1, ShortHeadroom: 0.1,
	}}), true)

	if got := ref.Get().ScoreFor(account.ProviderCodex, "new@example.com").Headroom; got != 0.8 {
		t.Fatalf("older refresh overwrote newer scheduler: headroom = %v, want 0.8", got)
	}
}

func TestSchedulerRefAccountGenerationInvalidatesLegacyRefresh(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler([]Score{{
		AccountID: "old@example.com", Provider: account.ProviderCodex, Headroom: 0.9, ShortHeadroom: 0.9,
	}}))
	ref.SetUpdatedAt(time.Time{})
	if !ref.BeginRefreshIfStale(time.Minute) {
		t.Fatal("legacy refresh did not begin")
	}

	ref.AdvanceAccountGeneration(2)
	if !ref.SetForAccountGeneration(NewScheduler([]Score{{
		AccountID: "new@example.com", Provider: account.ProviderCodex, Headroom: 0.8, ShortHeadroom: 0.8,
	}}), 2) {
		t.Fatal("new account generation was not published")
	}
	ref.FinishRefresh(NewScheduler([]Score{{
		AccountID: "old@example.com", Provider: account.ProviderCodex, Headroom: 0.1, ShortHeadroom: 0.1,
	}}), true)

	if got := ref.Get().ScoreFor(account.ProviderCodex, "new@example.com").Headroom; got != 0.8 {
		t.Fatalf("legacy refresh overwrote a newer account generation: headroom = %v, want 0.8", got)
	}
}

func TestCredentialExhaustionIsScopedToAccountGeneration(t *testing.T) {
	credential := account.Account{
		ID: "repaired@example.com", Provider: account.ProviderCodex, Token: "old-token",
	}
	ref := NewSchedulerRef(NewScheduler([]Score{{
		AccountID: credential.ID, Provider: credential.Provider,
		Headroom: 1, ShortHeadroom: 1,
	}}))
	ref.AdvanceAccountGenerationWithAccounts(1, 1, []account.Account{credential})
	if !ref.MarkCredentialExhaustedUntil(
		credential.Provider, credential.ID, credential.CredentialIdentity(), time.Now().Add(time.Hour), 1,
	) {
		t.Fatal("current credential failure was rejected")
	}
	if !ref.Get().Exhausted(account.ProviderCodex, "repaired@example.com") {
		t.Fatal("current credential failure did not exclude the account")
	}

	ref.AdvanceAccountGenerationWithAccounts(2, 2, []account.Account{credential})
	if !ref.Get().Exhausted(credential.Provider, credential.ID) {
		t.Fatal("unrelated reload cleared an unchanged credential's exclusion")
	}
	replacement := credential
	replacement.Token = "replacement-token"
	ref.AdvanceAccountGenerationWithAccounts(3, 3, []account.Account{replacement})
	if ref.Get().Exhausted(account.ProviderCodex, "repaired@example.com") {
		t.Fatal("replacement inherited the old credential's exclusion")
	}
	if ref.MarkCredentialExhaustedUntil(
		account.ProviderCodex, "repaired@example.com", "old-token", time.Now().Add(time.Hour), 1,
	) {
		t.Fatal("late old-generation credential failure was accepted")
	}
	if ref.Get().Exhausted(account.ProviderCodex, "repaired@example.com") {
		t.Fatal("late old-generation failure poisoned the replacement")
	}
}

func TestCredentialFailureAtomicallyAdvancesAccountSnapshot(t *testing.T) {
	old := account.Account{
		ID: "reloaded@example.com", Provider: account.ProviderCodex,
		CredentialVersion: "old-refresh-chain",
	}
	current := old
	current.CredentialVersion = "current-refresh-chain"
	ref := NewSchedulerRef(NewScheduler([]Score{{
		AccountID: current.ID, Provider: current.Provider, Headroom: 1, ShortHeadroom: 1,
	}}))
	ref.AdvanceAccountGenerationWithAccounts(1, 1, []account.Account{old})

	if !ref.MarkCredentialExhaustedForSnapshot(
		current.Provider, current.ID, current.CredentialIdentity(), time.Now().Add(time.Hour),
		2, 2, []account.Account{current},
	) {
		t.Fatal("current credential failure was dropped before the reload publisher synchronized")
	}
	if !ref.Get().Exhausted(current.Provider, current.ID) {
		t.Fatal("current credential was not excluded")
	}
	if ref.MarkCredentialExhaustedForSnapshot(
		old.Provider, old.ID, old.CredentialIdentity(), time.Now().Add(time.Hour),
		1, 1, []account.Account{old},
	) {
		t.Fatal("stale credential snapshot regressed the scheduler generation")
	}
}

func TestAccountCredentialSnapshotCannotRegressGeneration(t *testing.T) {
	old := account.Account{
		ID: "reloaded@example.com", Provider: account.ProviderCodex,
		CredentialVersion: "old-refresh-chain",
	}
	current := old
	current.CredentialVersion = "current-refresh-chain"
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.AdvanceAccountGenerationWithAccounts(2, 2, []account.Account{current})

	// A delayed publisher with a superficially newer credential revision must
	// not roll the account generation or credential identity backward.
	ref.AdvanceAccountGenerationWithAccounts(1, 3, []account.Account{old})
	if !ref.MarkCredentialExhaustedUntil(
		current.Provider, current.ID, current.CredentialIdentity(), time.Now().Add(time.Hour), 2,
	) {
		t.Fatal("stale publisher replaced the current account generation")
	}
	if ref.MarkCredentialExhaustedUntil(
		old.Provider, old.ID, old.CredentialIdentity(), time.Now().Add(time.Hour), 1,
	) {
		t.Fatal("stale account generation became current")
	}
}

func TestSyncAccountCredentialsDropsExclusionAfterTokenRotation(t *testing.T) {
	old := account.Account{
		ID: "rotated@example.com", Provider: account.ProviderCodex, Token: "same-access-token",
		CredentialVersion: "old-refresh-chain",
	}
	ref := NewSchedulerRef(NewScheduler([]Score{{
		AccountID: old.ID, Provider: old.Provider, Headroom: 1, ShortHeadroom: 1,
	}}))
	ref.AdvanceAccountGenerationWithAccounts(1, 1, []account.Account{old})
	if !ref.MarkCredentialExhaustedUntil(old.Provider, old.ID, old.CredentialIdentity(), time.Now().Add(time.Hour), 1) {
		t.Fatal("old credential failure was rejected")
	}
	replacement := old
	replacement.CredentialVersion = "new-refresh-chain"
	if !ref.SyncAccountCredentials(1, 2, []account.Account{replacement}) {
		t.Fatal("same-generation token rotation was not published")
	}
	// A reload publisher that captured the old snapshot before the rotation
	// must not roll the fingerprint backward at the same generation.
	ref.AdvanceAccountGenerationWithAccounts(1, 1, []account.Account{old})
	if ref.SyncAccountCredentials(1, 1, []account.Account{old}) {
		t.Fatal("stale same-generation credential snapshot was accepted")
	}
	if ref.Get().Exhausted(replacement.Provider, replacement.ID) {
		t.Fatal("rotated token inherited the old token's exclusion")
	}
	if ref.MarkCredentialExhaustedUntil(
		old.Provider, old.ID, old.CredentialIdentity(), time.Now().Add(time.Hour), 1,
	) {
		t.Fatal("late old-token failure was accepted after same-generation rotation")
	}
	if ref.Get().Exhausted(replacement.Provider, replacement.ID) {
		t.Fatal("late old-token failure poisoned the rotated credential")
	}
	ref.MarkExhaustedUntil(replacement.Provider, replacement.ID, "", time.Now().Add(time.Hour))
	rotatedAgain := replacement
	rotatedAgain.CredentialVersion = "third-refresh-chain"
	if !ref.SyncAccountCredentials(1, 3, []account.Account{rotatedAgain}) {
		t.Fatal("second token rotation was not published")
	}
	if !ref.Get().Exhausted(rotatedAgain.Provider, rotatedAgain.ID) {
		t.Fatal("token rotation cleared an account-scoped exclusion")
	}
}

func TestTokenRotationRestoresBaseScoreAfterCredentialOverlayWasCarriedForward(t *testing.T) {
	old := account.Account{ID: "recovered@example.com", Provider: account.ProviderCodex, Token: "old-token"}
	ref := NewSchedulerRef(NewScheduler([]Score{{
		AccountID: old.ID, Provider: old.Provider, Headroom: 0.8, ShortHeadroom: 0.8,
	}}))
	ref.AdvanceAccountGenerationWithAccounts(1, 1, []account.Account{old})
	if !ref.MarkCredentialExhaustedUntil(old.Provider, old.ID, old.CredentialIdentity(), time.Now().Add(time.Hour), 1) {
		t.Fatal("credential failure was rejected")
	}
	// A failed usage refresh can carry Get's overlaid zero forward. Publishing
	// that seed must not bake the credential overlay into the base scheduler.
	ref.FinishRefresh(ref.Get(), true)
	replacement := old
	replacement.Token = "new-token"
	if !ref.SyncAccountCredentials(1, 2, []account.Account{replacement}) {
		t.Fatal("token rotation was not published")
	}
	if got := ref.Get().ScoreFor(replacement.Provider, replacement.ID).Headroom; got != 0.8 {
		t.Fatalf("token rotation left carried credential zero in base score: got %v want 0.8", got)
	}
}

// TestMarkExhaustedUntilExpires: a mark with a reset time in the past must lapse
// on the next read, restoring the optimistic default so routing retries the
// account. A future mark holds.
func TestMarkExhaustedUntilExpires(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkExhaustedUntil(account.ProviderClaude, "recovered@example.com", "", time.Now().Add(-time.Second))
	ref.MarkExhaustedUntil(account.ProviderClaude, "cooked@example.com", "", time.Now().Add(time.Hour))
	s := ref.Get()
	if s.Exhausted(account.ProviderClaude, "recovered@example.com") {
		t.Fatal("expired mark must lapse: recovered account still exhausted")
	}
	if !s.Exhausted(account.ProviderClaude, "cooked@example.com") {
		t.Fatal("unexpired mark must hold: cooked account not exhausted")
	}
}

func TestMarkExhaustedUntilPoolScoped(t *testing.T) {
	const (
		fable = "claudefable"
		opus  = "claudeopus"
	)
	ref := NewSchedulerRef(NewScheduler([]Score{{
		AccountID:     "a@example.com",
		Provider:      account.ProviderClaude,
		Headroom:      1,
		ShortHeadroom: 1,
		ModelScores: map[string]Score{
			fable: {AccountID: "a@example.com", Provider: account.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
			opus:  {AccountID: "a@example.com", Provider: account.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
		},
	}}))
	ref.MarkExhaustedUntil(account.ProviderClaude, "a@example.com", fable, time.Now().Add(time.Hour))

	s := ref.Get()
	if !s.ForModel(fable).Exhausted(account.ProviderClaude, "a@example.com") {
		t.Fatal("fable pool mark should exhaust fable")
	}
	if s.ForModel(opus).Exhausted(account.ProviderClaude, "a@example.com") {
		t.Fatal("fable pool mark should not exhaust opus")
	}
	if s.Exhausted(account.ProviderClaude, "a@example.com") {
		t.Fatal("fable pool mark should not exhaust the base account score")
	}
}

func TestMarkExhaustedUntilAccountWideStillExhaustsEveryPool(t *testing.T) {
	const fable = "claudefable"
	ref := NewSchedulerRef(NewScheduler([]Score{{
		AccountID:     "a@example.com",
		Provider:      account.ProviderClaude,
		Headroom:      1,
		ShortHeadroom: 1,
		ModelScores: map[string]Score{
			fable: {AccountID: "a@example.com", Provider: account.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
		},
	}}))
	ref.MarkExhaustedUntil(account.ProviderClaude, "a@example.com", "", time.Now().Add(time.Hour))

	s := ref.Get()
	if !s.Exhausted(account.ProviderClaude, "a@example.com") {
		t.Fatal("account-wide mark should exhaust the base score")
	}
	if !s.ForModel(fable).Exhausted(account.ProviderClaude, "a@example.com") {
		t.Fatal("account-wide mark should exhaust model pools")
	}
}

func TestMarkExhaustedUntilPoolMarkExpires(t *testing.T) {
	const fable = "claudefable"
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkExhaustedUntil(account.ProviderClaude, "a@example.com", fable, time.Now().Add(-time.Second))

	if ref.Get().ForModel(fable).Exhausted(account.ProviderClaude, "a@example.com") {
		t.Fatal("expired pool mark should allow an optimistic retry")
	}
	if _, ok := ref.ExhaustedUntilFor(account.ProviderClaude, "a@example.com", fable); ok {
		t.Fatal("expired pool mark should be pruned")
	}
}

// TestSetClearsExhaustedUntil: a full refresh supersedes request-time marks; a
// later prune must not delete refreshed scores.
func TestSetClearsExhaustedUntil(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkExhaustedUntil(account.ProviderClaude, "a@example.com", "", time.Now().Add(-time.Second))
	ref.Set(NewScheduler([]Score{{AccountID: "a@example.com", Provider: account.ProviderClaude, Headroom: 0.02, ShortHeadroom: 0.02}}))
	s := ref.Get()
	if got := s.ScoreFor(account.ProviderClaude, "a@example.com").Headroom; got != 0.02 {
		t.Fatalf("refreshed score clobbered by stale expiry prune: headroom=%v want 0.02", got)
	}
}

// TestPartialRefreshKeepsMarkExpiry is the mixed-refresh regression: a refresh
// that carries the exhausted account's zero score forward (its own usage fetch
// failed) must NOT strip the mark's expiry, or the mark becomes permanent again
// and the recovered account stays unroutable.
func TestPartialRefreshKeepsMarkExpiry(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkExhaustedUntil(account.ProviderClaude, "recovered@example.com", "", time.Now().Add(-time.Second))
	// Partial refresh: another account got fresh data, but recovered@'s zero
	// score is seeded/carried forward unchanged.
	ref.FinishRefresh(NewScheduler([]Score{
		{AccountID: "other@example.com", Provider: account.ProviderClaude, Headroom: 0.8, ShortHeadroom: 0.8},
		{AccountID: "recovered@example.com", Provider: account.ProviderClaude, Headroom: 0, ShortHeadroom: 0},
	}), true)
	if ref.Get().Exhausted(account.ProviderClaude, "recovered@example.com") {
		t.Fatal("carried-forward zero score must keep its expiry; recovered account still exhausted after lapse")
	}
	// But a refresh that genuinely supersedes the mark (headroom) drops the expiry.
	ref.MarkExhaustedUntil(account.ProviderClaude, "busy@example.com", "", time.Now().Add(-time.Second))
	ref.FinishRefresh(NewScheduler([]Score{
		{AccountID: "busy@example.com", Provider: account.ProviderClaude, Headroom: 0.05, ShortHeadroom: 0.05},
	}), true)
	if got := ref.Get().ScoreFor(account.ProviderClaude, "busy@example.com").Headroom; got != 0.05 {
		t.Fatalf("superseded mark must not prune the refreshed score: headroom=%v want 0.05", got)
	}
}

// TestLapsedMarkRemarksOnNextReject documents the retry-once-on-lapse loop: a
// lapsed mark makes a still-cooked account optimistic for exactly one probe;
// the upstream's next reject re-marks it with the new authoritative reset, so
// the cost of guessing wrong is bounded at one attempt per expiry window.
func TestLapsedMarkRemarksOnNextReject(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkExhaustedUntil(account.ProviderClaude, "cooked@example.com", "", time.Now().Add(-time.Second))
	if ref.Get().Exhausted(account.ProviderClaude, "cooked@example.com") {
		t.Fatal("lapsed mark should allow one optimistic probe")
	}
	// The probe's rejected response re-marks with the new upstream reset.
	ref.MarkExhaustedUntil(account.ProviderClaude, "cooked@example.com", "", time.Now().Add(2*time.Hour))
	if !ref.Get().Exhausted(account.ProviderClaude, "cooked@example.com") {
		t.Fatal("re-mark after the probe's reject must hold until the new reset")
	}
}

// TestFreshZeroReanchorsExpiry: a successful usage fetch that re-confirms
// exhaustion must re-anchor the mark's expiry to that newest evidence, so an
// older request-time expiry cannot lapse a freshly-observed zero back to
// optimistic. Expiries only extend, never shorten.
func TestFreshZeroReanchorsExpiry(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	// Old request-time mark about to lapse.
	ref.MarkExhaustedUntil(account.ProviderClaude, "confirmed@example.com", "", time.Now().Add(time.Millisecond))
	// Fresh refresh re-confirms exhaustion with a 2h window reset.
	ref.FinishRefresh(NewScheduler([]Score{
		{AccountID: "confirmed@example.com", Provider: account.ProviderClaude, Headroom: 0, ShortHeadroom: 0, ShortResetAfterSeconds: 7200, Fresh: true},
	}), true)
	until, ok := ref.ExhaustedUntilFor(account.ProviderClaude, "confirmed@example.com", "")
	if !ok || time.Until(until) < 90*time.Minute {
		t.Fatalf("fresh zero should re-anchor expiry to its reset (~2h), got %v (in %v)", until, time.Until(until))
	}
	if ref.Get().Exhausted(account.ProviderClaude, "confirmed@example.com") != true {
		t.Fatal("freshly-confirmed exhausted account must stay exhausted")
	}

	// A fresh zero must never SHORTEN a longer authoritative expiry.
	ref2 := NewSchedulerRef(NewScheduler(nil))
	long := time.Now().Add(72 * time.Hour)
	ref2.MarkExhaustedUntil(account.ProviderClaude, "weekly@example.com", "", long)
	ref2.FinishRefresh(NewScheduler([]Score{
		{AccountID: "weekly@example.com", Provider: account.ProviderClaude, Headroom: 0, ShortHeadroom: 0, ShortResetAfterSeconds: 3600, Fresh: true},
	}), true)
	got, _ := ref2.ExhaustedUntilFor(account.ProviderClaude, "weekly@example.com", "")
	if !got.Equal(long) {
		t.Fatalf("fresh zero shortened authoritative expiry: got %v want %v", got, long)
	}
}

func TestPoolScopedFreshZeroReanchorsExpiry(t *testing.T) {
	const fable = "claudefable"
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkExhaustedUntil(account.ProviderClaude, "confirmed@example.com", fable, time.Now().Add(time.Millisecond))
	ref.FinishRefresh(NewScheduler([]Score{{
		AccountID:     "confirmed@example.com",
		Provider:      account.ProviderClaude,
		Headroom:      1,
		ShortHeadroom: 1,
		ModelScores: map[string]Score{
			fable: {
				AccountID:              "confirmed@example.com",
				Provider:               account.ProviderClaude,
				Headroom:               0,
				ShortHeadroom:          0,
				ShortResetAfterSeconds: 7200,
				Fresh:                  true,
			},
		},
	}}), true)

	until, ok := ref.ExhaustedUntilFor(account.ProviderClaude, "confirmed@example.com", fable)
	if !ok || time.Until(until) < 90*time.Minute {
		t.Fatalf("fresh pool zero should re-anchor expiry to its reset (~2h), got %v (in %v)", until, time.Until(until))
	}
	if !ref.Get().ForModel(fable).Exhausted(account.ProviderClaude, "confirmed@example.com") {
		t.Fatal("freshly-confirmed exhausted pool must stay exhausted")
	}
}

func TestPoolScopedRecoveredRefreshDropsExpiry(t *testing.T) {
	const fable = "claudefable"
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkExhaustedUntil(account.ProviderClaude, "recovered@example.com", fable, time.Now().Add(time.Hour))
	ref.FinishRefresh(NewScheduler([]Score{{
		AccountID:     "recovered@example.com",
		Provider:      account.ProviderClaude,
		Headroom:      1,
		ShortHeadroom: 1,
		ModelScores: map[string]Score{
			fable: {AccountID: "recovered@example.com", Provider: account.ProviderClaude, Headroom: 0.8, ShortHeadroom: 0.8, Fresh: true},
		},
	}}), true)

	if _, ok := ref.ExhaustedUntilFor(account.ProviderClaude, "recovered@example.com", fable); ok {
		t.Fatal("fresh recovered pool score should drop the pool mark")
	}
	if ref.Get().ForModel(fable).Exhausted(account.ProviderClaude, "recovered@example.com") {
		t.Fatal("fresh recovered pool score should be routable")
	}
}

func TestPoolScopedRefreshWithoutPoolEvidenceKeepsExpiry(t *testing.T) {
	const model = "gpt-5.6-sol"
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkExhaustedUntil(account.ProviderCodex, "incompatible@example.com", model, time.Now().Add(time.Hour))
	ref.FinishRefresh(NewScheduler([]Score{{
		AccountID:     "incompatible@example.com",
		Provider:      account.ProviderCodex,
		Headroom:      0.8,
		ShortHeadroom: 0.8,
		Fresh:         true,
	}}), true)

	if _, ok := ref.ExhaustedUntilFor(account.ProviderCodex, "incompatible@example.com", model); !ok {
		t.Fatal("account-wide refresh without model evidence must keep the model-scoped mark")
	}
	if !ref.Get().ForModel(model).Exhausted(account.ProviderCodex, "incompatible@example.com") {
		t.Fatal("model-scoped mark must survive a refresh that cannot evaluate that model")
	}
	if ref.Get().Exhausted(account.ProviderCodex, "incompatible@example.com") {
		t.Fatal("model-scoped mark must not exhaust the base account score")
	}
}

func TestModelIncompatibilitySurvivesHealthyPoolRefresh(t *testing.T) {
	const model = "gpt-5.6-sol"
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkModelIncompatibleUntil(account.ProviderCodex, "incompatible@example.com", model, time.Now().Add(time.Hour))
	ref.FinishRefresh(NewScheduler([]Score{{
		AccountID:     "incompatible@example.com",
		Provider:      account.ProviderCodex,
		Headroom:      0.8,
		ShortHeadroom: 0.8,
		Fresh:         true,
		ModelScores: map[string]Score{
			model: {
				AccountID:     "incompatible@example.com",
				Provider:      account.ProviderCodex,
				Headroom:      0.8,
				ShortHeadroom: 0.8,
				Fresh:         true,
			},
		},
	}}), true)

	if _, ok := ref.ModelIncompatibleUntilFor(account.ProviderCodex, "incompatible@example.com", model); !ok {
		t.Fatal("quota refresh must not clear account/model incompatibility")
	}
	if !ref.Get().ForModel(model).Exhausted(account.ProviderCodex, "incompatible@example.com") {
		t.Fatal("incompatible account must remain excluded for the rejected model")
	}
	if ref.Get().Exhausted(account.ProviderCodex, "incompatible@example.com") {
		t.Fatal("model incompatibility must not exhaust the base account score")
	}
}

func TestPoolScopedRetainLeavesOtherPoolMark(t *testing.T) {
	const (
		fable = "claudefable"
		opus  = "claudeopus"
	)
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkExhaustedUntil(account.ProviderClaude, "a@example.com", opus, time.Now().Add(time.Hour))
	ref.FinishRefresh(NewScheduler([]Score{{
		AccountID:     "a@example.com",
		Provider:      account.ProviderClaude,
		Headroom:      1,
		ShortHeadroom: 1,
		ModelScores: map[string]Score{
			fable: {AccountID: "a@example.com", Provider: account.ProviderClaude, Headroom: 0.8, ShortHeadroom: 0.8, Fresh: true},
			opus:  {AccountID: "a@example.com", Provider: account.ProviderClaude, Headroom: 0, ShortHeadroom: 0},
		},
	}}), true)

	if _, ok := ref.ExhaustedUntilFor(account.ProviderClaude, "a@example.com", opus); !ok {
		t.Fatal("fable refresh should not clear a carried-forward opus mark")
	}
	if !ref.Get().ForModel(opus).Exhausted(account.ProviderClaude, "a@example.com") {
		t.Fatal("opus mark should still apply after fable refresh")
	}
}
