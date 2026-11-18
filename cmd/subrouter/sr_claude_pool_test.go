package main

import (
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func TestClaudePoolStatus(t *testing.T) {
	checked := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	exhausted := func(name string, shortReset, longReset int64) srUsageRow {
		return srUsageRow{
			email: name, provider: accounts.ProviderClaude, authMode: accounts.AuthModeOAuth,
			cooked: true, score: selectacct.Score{AccountID: name},
			windows: []accounts.UsageWindow{
				{Name: "5h", UsedPercent: 100, ResetAfterSeconds: shortReset},
				{Name: "7d", UsedPercent: 100, ResetAfterSeconds: longReset},
				{Name: "opus-weekly", UsedPercent: 100, ResetAfterSeconds: 1, Feature: "opus"},
			},
		}
	}
	usable := srUsageRow{
		email: "ready", provider: accounts.ProviderClaude, authMode: accounts.AuthModeOAuth,
		score: selectacct.Score{Headroom: 0.7, ShortHeadroom: 0.7},
	}
	tests := []struct {
		name      string
		rows      []srUsageRow
		exhausted bool
		usable    int
		resume    string
	}{
		{"all exhausted chooses soonest profile recovery", []srUsageRow{exhausted("late", 600, 3600), exhausted("early", 1200, 1800)}, true, 0, "2026-11-18T12:30:00Z"},
		{"usable profile", []srUsageRow{exhausted("cooked", 600, 3600), usable}, false, 1, "2026-11-18T13:00:00Z"},
		{"no profiles", nil, false, 0, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := claudePoolStatus(tc.rows, checked)
			if got.AllExhausted != tc.exhausted || got.UsableProfiles != tc.usable || got.CheckedAt != "2026-11-18T12:00:00Z" {
				t.Fatalf("pool = %+v", got)
			}
			if tc.resume == "" && got.ResumeAt != nil || tc.resume != "" && (got.ResumeAt == nil || *got.ResumeAt != tc.resume) {
				t.Fatalf("resume_at = %v, want %q", got.ResumeAt, tc.resume)
			}
		})
	}
}
