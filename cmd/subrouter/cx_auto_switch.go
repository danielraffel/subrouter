package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/selectacct"
	"github.com/manaflow-ai/subrouter/internal/session"
)

const defaultCXSwitchInterval = 10 * time.Minute

type cxAutoSwitchConfig struct {
	Interval     time.Duration
	Accounts     []accounts.Account
	AccountsFunc func() []accounts.Account
	Sessions     *session.Store
	SchedulerRef *selectacct.SchedulerRef
	Logger       *slog.Logger
	FetchScores  func(context.Context, []accounts.Account) ([]selectacct.Score, int)
	SwitchActive func(context.Context, string) error
}

func runCXAutoSwitch(ctx context.Context, cfg cxAutoSwitchConfig) {
	if cfg.Interval <= 0 {
		return
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			picked, err := cxAutoSwitchOnce(ctx, cfg)
			if err != nil {
				logCXAutoSwitch(cfg.Logger, slog.LevelWarn, "cx auto-switch failed", "error", err)
			} else {
				logCXAutoSwitch(cfg.Logger, slog.LevelInfo, "cx auto-switch selected account", "account", picked)
			}
			timer.Reset(cfg.Interval)
		}
	}
}

func cxAutoSwitchOnce(ctx context.Context, cfg cxAutoSwitchConfig) (string, error) {
	fetchScores := cfg.FetchScores
	if fetchScores == nil {
		fetchScores = fetchCodexScoresWithSuccess
	}
	switchActive := cfg.SwitchActive
	if switchActive == nil {
		switchActive = func(ctx context.Context, accountID string) error {
			store := accounts.DefaultCodexStore()
			stored, ok, err := store.FindStored(accountID)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("account %q not found", accountID)
			}
			refreshed, _, err := store.RefreshStoredIfExpired(ctx, nil, stored)
			if err == nil {
				stored = refreshed
			} else {
				logCXAutoSwitch(cfg.Logger, slog.LevelWarn, "cx auto-switch token refresh failed, using cached tokens", "account", accountID, "error", err)
			}
			if err := accounts.WriteActiveCodexAuth(stored.Auth); err != nil {
				return err
			}
			for _, result := range syncCodexCompatibleAuth(stored) {
				if result.Err != nil {
					logCXAutoSwitch(cfg.Logger, slog.LevelWarn, "cx auto-switch compatible auth sync failed", "tool", result.Tool, "error", result.Err)
				}
			}
			return nil
		}
	}

	allAccounts := cfg.Accounts
	if cfg.AccountsFunc != nil {
		allAccounts = cfg.AccountsFunc()
	}
	candidates := oauthAccounts(allAccounts)
	if len(candidates) == 0 {
		return "", fmt.Errorf("no OAuth Codex accounts available for cx auto-switch")
	}

	scores, successful := fetchScores(ctx, candidates)
	if successful == 0 {
		return "", fmt.Errorf("no fresh OAuth usage scores available")
	}

	scheduler := selectacct.NewScheduler(scores)
	if cfg.SchedulerRef != nil {
		cfg.SchedulerRef.Set(scheduler)
	}
	if cfg.Sessions != nil {
		scheduler = scheduler.WithSessionCounts(cfg.Sessions.CountByAccount())
	}

	picked, err := scheduler.Pick(candidates)
	if err != nil {
		return "", err
	}
	if !scheduler.UsableForNewSession(picked.ID) {
		if scheduler.Exhausted(picked.ID) {
			return "", fmt.Errorf("no usable OAuth Codex accounts available")
		}
		logCXAutoSwitch(cfg.Logger, slog.LevelWarn, "cx auto-switch selected account below new-session headroom threshold", "account", picked.ID, "threshold", selectacct.MinNewSessionHeadroom)
	}
	if err := switchActive(ctx, picked.ID); err != nil {
		return "", err
	}
	return picked.ID, nil
}

func oauthAccounts(all []accounts.Account) []accounts.Account {
	out := make([]accounts.Account, 0, len(all))
	for _, account := range all {
		if account.AuthMode == accounts.AuthModeOAuth {
			out = append(out, account)
		}
	}
	return out
}

func logCXAutoSwitch(logger *slog.Logger, level slog.Level, message string, args ...any) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.Log(context.Background(), level, message, args...)
}
