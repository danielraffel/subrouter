package selectacct

import (
	"math"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

type simulatedAccount struct {
	id       string
	used5h   float64
	used7d   float64
	sessions int
}

func TestScoreFromLimitWindowsUsesMostConstrainedWindow(t *testing.T) {
	score := ScoreFromLimitWindows("a", 3, []LimitWindow{
		{Name: "5h", UsedPercent: 20},
		{Name: "7d", UsedPercent: 80},
	})

	if math.Abs(score.Headroom-0.20) > 0.0001 {
		t.Fatalf("headroom = %.2f, want 0.20", score.Headroom)
	}
	if score.Sessions != 3 {
		t.Fatalf("sessions = %d, want 3", score.Sessions)
	}
}

func TestScoreFromLimitWindowsComputesExpiryPressure(t *testing.T) {
	score := ScoreFromLimitWindows("a", 0, []LimitWindow{
		{Name: "5h", UsedPercent: 10, LimitWindowSeconds: 5 * 60 * 60, ResetAfterSeconds: 3 * 60 * 60},
		{Name: "7d", UsedPercent: 20, LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAfterSeconds: 6 * 24 * 60 * 60},
	})

	if math.Abs(score.Headroom-0.80) > 0.0001 {
		t.Fatalf("headroom = %.2f, want 0.80", score.Headroom)
	}
	if math.Abs(score.ShortHeadroom-0.90) > 0.0001 {
		t.Fatalf("short headroom = %.2f, want 0.90", score.ShortHeadroom)
	}
	wantPressure := 0.80 / float64(3*60*60)
	if math.Abs(score.ExpiryPressure-wantPressure) > 0.000001 {
		t.Fatalf("expiry pressure = %.8f, want %.8f", score.ExpiryPressure, wantPressure)
	}
}

func TestSchedulerForSparkModelUsesSparkWindows(t *testing.T) {
	scheduler := NewScheduler([]Score{
		ScoreFromLimitWindows("spark-healthy", 0, []LimitWindow{
			{Name: "primary", UsedPercent: 100, LimitWindowSeconds: 5 * 60 * 60},
			{Name: "secondary", UsedPercent: 100, LimitWindowSeconds: 7 * 24 * 60 * 60},
			{Name: "GPT-5.3-Codex-Spark/primary", UsedPercent: 1, LimitWindowSeconds: 5 * 60 * 60},
			{Name: "GPT-5.3-Codex-Spark/secondary", UsedPercent: 2, LimitWindowSeconds: 7 * 24 * 60 * 60},
		}),
		ScoreFromLimitWindows("spark-cooked", 0, []LimitWindow{
			{Name: "primary", UsedPercent: 0, LimitWindowSeconds: 5 * 60 * 60},
			{Name: "secondary", UsedPercent: 0, LimitWindowSeconds: 7 * 24 * 60 * 60},
			{Name: "GPT-5.3-Codex-Spark/primary", UsedPercent: 100, LimitWindowSeconds: 5 * 60 * 60},
			{Name: "GPT-5.3-Codex-Spark/secondary", UsedPercent: 100, LimitWindowSeconds: 7 * 24 * 60 * 60},
		}),
	}).ForModel("GPT-5.3-Codex-Spark")

	got, err := scheduler.Pick([]accounts.Account{
		{ID: "spark-cooked", AuthMode: accounts.AuthModeOAuth},
		{ID: "spark-healthy", AuthMode: accounts.AuthModeOAuth},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "spark-healthy" {
		t.Fatalf("got %q, want spark-healthy", got.ID)
	}
	if !scheduler.UsableForNewSession("spark-healthy") {
		t.Fatal("spark-healthy should be usable for a Spark request")
	}
	if !scheduler.Exhausted("spark-cooked") {
		t.Fatal("spark-cooked should be exhausted for a Spark request")
	}
}

func TestExpiryAwareRoutingMatchesSnapshotOrder(t *testing.T) {
	state := []simulatedAccount{
		{id: "alpha@example.com", used5h: 73, used7d: 16},
		{id: "founders@example.com", used5h: 0, used7d: 0},
		{id: "dave@example.com", used5h: 7, used7d: 1},
		{id: "erin@example.com", used5h: 0, used7d: 0},
	}
	reset5h := map[string]int64{
		"alpha@example.com":    157 * 60,
		"founders@example.com": 164 * 60,
		"dave@example.com":     175 * 60,
		"erin@example.com":     300 * 60,
	}
	scheduler := NewScheduler(scoresForWithReset(state, reset5h))
	candidates := []accounts.Account{
		{ID: "alpha@example.com", AuthMode: accounts.AuthModeOAuth},
		{ID: "founders@example.com", AuthMode: accounts.AuthModeOAuth},
		{ID: "dave@example.com", AuthMode: accounts.AuthModeOAuth},
		{ID: "erin@example.com", AuthMode: accounts.AuthModeOAuth},
	}

	got, err := scheduler.Pick(candidates)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "founders@example.com" {
		t.Fatalf("got %q, want founders@example.com", got.ID)
	}
}

func TestBottleneckHeadroomBeatsRoundRobinWhenWeeklyLimitsDiffer(t *testing.T) {
	accounts := []simulatedAccount{
		{id: "near-weekly-cap", used5h: 5, used7d: 90},
		{id: "healthy", used5h: 5, used7d: 10},
	}

	roundRobinAccepted := simulateRoundRobin(accounts, 20, 3, 3)
	bottleneckAccepted := simulateBottleneck(accounts, 20, 3, 3)

	if roundRobinAccepted != 13 {
		t.Fatalf("round-robin accepted %d, want 13", roundRobinAccepted)
	}
	if bottleneckAccepted != 20 {
		t.Fatalf("bottleneck accepted %d, want 20", bottleneckAccepted)
	}
}

func TestBottleneckHeadroomAvoidsShortWindowExhaustion(t *testing.T) {
	accounts := []simulatedAccount{
		{id: "near-5h-cap", used5h: 92, used7d: 5},
		{id: "healthy", used5h: 10, used7d: 5},
	}

	roundRobinAccepted := simulateRoundRobin(accounts, 20, 4, 1)
	bottleneckAccepted := simulateBottleneck(accounts, 20, 4, 1)

	if roundRobinAccepted != 12 {
		t.Fatalf("round-robin accepted %d, want 12", roundRobinAccepted)
	}
	if bottleneckAccepted != 20 {
		t.Fatalf("bottleneck accepted %d, want 20", bottleneckAccepted)
	}
}

func simulateRoundRobin(initial []simulatedAccount, sessions int, cost5h float64, cost7d float64) int {
	state := cloneSimulated(initial)
	accepted := 0
	for i := 0; i < sessions; i++ {
		idx := i % len(state)
		if canAssign(state[idx], cost5h, cost7d) {
			assign(&state[idx], cost5h, cost7d)
			accepted++
		}
	}
	return accepted
}

func simulateBottleneck(initial []simulatedAccount, sessions int, cost5h float64, cost7d float64) int {
	state := cloneSimulated(initial)
	accepted := 0
	for i := 0; i < sessions; i++ {
		scheduler := NewScheduler(scoresFor(state))
		candidates := make([]accounts.Account, 0, len(state))
		for _, account := range state {
			candidates = append(candidates, accounts.Account{ID: account.id, AuthMode: accounts.AuthModeOAuth})
		}

		pick, err := scheduler.Pick(candidates)
		if err != nil {
			return accepted
		}
		idx := findSimulated(state, pick.ID)
		if idx < 0 || !canAssign(state[idx], cost5h, cost7d) {
			return accepted
		}
		assign(&state[idx], cost5h, cost7d)
		accepted++
	}
	return accepted
}

func scoresFor(state []simulatedAccount) []Score {
	scores := make([]Score, 0, len(state))
	for _, account := range state {
		scores = append(scores, ScoreFromLimitWindows(account.id, account.sessions, []LimitWindow{
			{Name: "5h", UsedPercent: account.used5h},
			{Name: "7d", UsedPercent: account.used7d},
		}))
	}
	return scores
}

func scoresForWithReset(state []simulatedAccount, reset5h map[string]int64) []Score {
	scores := make([]Score, 0, len(state))
	for _, account := range state {
		scores = append(scores, ScoreFromLimitWindows(account.id, account.sessions, []LimitWindow{
			{Name: "5h", UsedPercent: account.used5h, LimitWindowSeconds: 5 * 60 * 60, ResetAfterSeconds: reset5h[account.id]},
			{Name: "7d", UsedPercent: account.used7d, LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAfterSeconds: 7 * 24 * 60 * 60},
		}))
	}
	return scores
}

func canAssign(account simulatedAccount, cost5h float64, cost7d float64) bool {
	return account.used5h+cost5h <= 100 && account.used7d+cost7d <= 100
}

func assign(account *simulatedAccount, cost5h float64, cost7d float64) {
	account.used5h += cost5h
	account.used7d += cost7d
	account.sessions++
}

func cloneSimulated(initial []simulatedAccount) []simulatedAccount {
	return append([]simulatedAccount(nil), initial...)
}

func findSimulated(state []simulatedAccount, id string) int {
	for i, account := range state {
		if account.id == id {
			return i
		}
	}
	return -1
}
