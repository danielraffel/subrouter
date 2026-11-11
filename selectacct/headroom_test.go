package selectacct

import (
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/account"
)

func resetHeadroom(t *testing.T) {
	t.Helper()
	old := MinNewSessionHeadroom()
	t.Cleanup(func() { setMinNewSessionHeadroom(old) })
}

func TestResolveMinNewSessionHeadroomDefault(t *testing.T) {
	value, warning, source := ResolveMinNewSessionHeadroom("", nil)
	if value != DefaultMinNewSessionHeadroom || warning != "" || source != "default" {
		t.Fatalf("got value=%v warning=%q source=%q", value, warning, source)
	}
}

func TestResolveMinNewSessionHeadroomEnvironmentOverride(t *testing.T) {
	value, warning, source := ResolveMinNewSessionHeadroom("0.15", nil)
	if value != 0.15 || warning != "" || source != "default" {
		t.Fatalf("got value=%v warning=%q source=%q", value, warning, source)
	}
}

func TestResolveMinNewSessionHeadroomInvalidFallsBackWithWarning(t *testing.T) {
	value, warning, _ := ResolveMinNewSessionHeadroom("nope", nil)
	if value != DefaultMinNewSessionHeadroom || !strings.Contains(warning, "invalid") {
		t.Fatalf("got value=%v warning=%q", value, warning)
	}
}

func TestResolveMinNewSessionHeadroomClamps(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want float64
	}{{"0.01", 0.05}, {"1.0", 0.90}} {
		value, warning, _ := ResolveMinNewSessionHeadroom(tc.raw, nil)
		if value != tc.want || !strings.Contains(warning, "clamped") {
			t.Fatalf("raw %q: value=%v warning=%q", tc.raw, value, warning)
		}
	}
}

func TestLowHeadroomAccountFollowsConfiguredThreshold(t *testing.T) {
	resetHeadroom(t)
	scheduler := NewScheduler([]Score{{AccountID: "claude", Provider: account.ProviderClaude, Headroom: 0.18, ShortHeadroom: 0.18}})
	setMinNewSessionHeadroom(0.15)
	if !scheduler.UsableForNewSession(account.ProviderClaude, "claude") {
		t.Fatal("18% headroom should be usable at a 15% threshold")
	}
	setMinNewSessionHeadroom(0.40)
	if scheduler.UsableForNewSession(account.ProviderClaude, "claude") {
		t.Fatal("18% headroom should be protected at a 40% threshold")
	}
}

func TestConfigValueUsedWhenEnvironmentUnset(t *testing.T) {
	configured := 0.22
	value, warning, source := ResolveMinNewSessionHeadroom("", &configured)
	if value != configured || warning != "" || source != "config" {
		t.Fatalf("got value=%v warning=%q source=%q", value, warning, source)
	}
}
