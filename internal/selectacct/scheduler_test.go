package selectacct

import (
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestPickPrefersHeadroom(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "a", Headroom: 0.20, Sessions: 0},
		{AccountID: "b", Headroom: 0.90, Sessions: 5},
	})

	got, err := scheduler.Pick([]accounts.Account{{ID: "a"}, {ID: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "b" {
		t.Fatalf("got %q, want b", got.ID)
	}
}

func TestPickPrefersSoonExpiringHealthyHeadroom(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "later-full", Headroom: 1.00, ShortHeadroom: 1.00, ShortResetAfterSeconds: 5 * 60 * 60, ExpiryPressure: 1.00 / float64(5*60*60)},
		{AccountID: "soon-healthy", Headroom: 0.93, ShortHeadroom: 0.93, ShortResetAfterSeconds: 3 * 60 * 60, ExpiryPressure: 0.93 / float64(3*60*60)},
	})

	got, err := scheduler.Pick([]accounts.Account{{ID: "later-full"}, {ID: "soon-healthy"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "soon-healthy" {
		t.Fatalf("got %q, want soon-healthy", got.ID)
	}
}

func TestPickProtectsLowShortWindowHeadroom(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "soon-low", Headroom: 0.27, ShortHeadroom: 0.27, ShortResetAfterSeconds: 157 * 60, ExpiryPressure: 0.27 / float64(157*60)},
		{AccountID: "soon-full", Headroom: 1.00, ShortHeadroom: 1.00, ShortResetAfterSeconds: 164 * 60, ExpiryPressure: 1.00 / float64(164*60)},
	})

	got, err := scheduler.Pick([]accounts.Account{{ID: "soon-low"}, {ID: "soon-full"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "soon-full" {
		t.Fatalf("got %q, want soon-full", got.ID)
	}
}

func TestPickBreaksTiesByFewestSessions(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "a", Headroom: 0.75, Sessions: 4},
		{AccountID: "b", Headroom: 0.75, Sessions: 1},
	})

	got, err := scheduler.Pick([]accounts.Account{{ID: "a"}, {ID: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "b" {
		t.Fatalf("got %q, want b", got.ID)
	}
}

func TestWithSessionCountsUsesLiveAssignments(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "a", Headroom: 0.75, Sessions: 0},
		{AccountID: "b", Headroom: 0.75, Sessions: 0},
	}).WithSessionCounts(map[string]int{"a": 2})

	got, err := scheduler.Pick([]accounts.Account{{ID: "a"}, {ID: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "b" {
		t.Fatalf("got %q, want b", got.ID)
	}
}

func TestPickPrefersOAuthBeforeAPIKey(t *testing.T) {
	scheduler := NewScheduler(nil)

	got, err := scheduler.Pick([]accounts.Account{
		{ID: "apikey:first", AuthMode: accounts.AuthModeAPIKey},
		{ID: "z@example.com", AuthMode: accounts.AuthModeOAuth},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "z@example.com" {
		t.Fatalf("got %q, want OAuth account", got.ID)
	}
}

// API-key accounts cost real money per token; OAuth subscription accounts are
// already paid for. Usable OAuth accounts stay ahead of API-key fallback.
func TestPickKeepsUsableOAuthBeforeAPIKey(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "oauth-usable", Headroom: 0.50, ShortHeadroom: 0.50, Sessions: 9},
		{AccountID: "apikey:flush", Headroom: 0.99, Sessions: 0},
	})

	got, err := scheduler.Pick([]accounts.Account{
		{ID: "apikey:flush", AuthMode: accounts.AuthModeAPIKey},
		{ID: "oauth-usable", AuthMode: accounts.AuthModeOAuth},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "oauth-usable" {
		t.Fatalf("got %q, want usable OAuth account", got.ID)
	}
}

func TestPickHealthyOAuthBeforeExhaustedOAuthAndAPIKey(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "alice@example.com", Headroom: 0.90, ShortHeadroom: 0.90},
		{AccountID: "frank@example.com", Headroom: 0, ShortHeadroom: 0},
		{AccountID: "apikey:team-codex-1", Headroom: 0.01, ShortHeadroom: 0.01},
	})

	got, err := scheduler.Pick([]accounts.Account{
		{ID: "frank@example.com", AuthMode: accounts.AuthModeOAuth},
		{ID: "apikey:team-codex-1", AuthMode: accounts.AuthModeAPIKey},
		{ID: "alice@example.com", AuthMode: accounts.AuthModeOAuth},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "alice@example.com" {
		t.Fatalf("got %q, want alice@example.com", got.ID)
	}
}

func TestPickFallsBackToAPIKeyBeforeExhaustedOAuth(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "oauth-empty", Headroom: 0, ShortHeadroom: 0},
		{AccountID: "apikey:paid", Headroom: 0.01, ShortHeadroom: 0.01},
	})

	got, err := scheduler.Pick([]accounts.Account{
		{ID: "oauth-empty", AuthMode: accounts.AuthModeOAuth},
		{ID: "apikey:paid", AuthMode: accounts.AuthModeAPIKey},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "apikey:paid" {
		t.Fatalf("got %q, want API-key fallback", got.ID)
	}
}

func TestPickKeepsConstrainedOAuthBeforeExhaustedOAuth(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "short-empty@example.com", Headroom: 0.79, ShortHeadroom: 0},
		{AccountID: "near-threshold@example.com", Headroom: 0.39, ShortHeadroom: 0.39},
	})

	got, err := scheduler.Pick([]accounts.Account{
		{ID: "short-empty@example.com", AuthMode: accounts.AuthModeOAuth},
		{ID: "near-threshold@example.com", AuthMode: accounts.AuthModeOAuth},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "near-threshold@example.com" {
		t.Fatalf("got %q, want constrained but non-exhausted OAuth account", got.ID)
	}
}

// API-key accounts are still picked when no OAuth candidate exists.
func TestPickFallsBackToAPIKeyWhenNoOAuth(t *testing.T) {
	scheduler := NewScheduler(nil)

	got, err := scheduler.Pick([]accounts.Account{
		{ID: "apikey:only", AuthMode: accounts.AuthModeAPIKey},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "apikey:only" {
		t.Fatalf("got %q, want apikey:only", got.ID)
	}
}
