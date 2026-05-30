package selectacct

import "strings"

type LimitWindow struct {
	Name               string
	UsedPercent        float64
	LimitWindowSeconds int64
	ResetAfterSeconds  int64
}

func ScoreFromLimitWindows(accountID string, sessions int, windows []LimitWindow) Score {
	score := scoreFromLimitWindows(accountID, sessions, windows)
	if spark, ok := ScoreFromLimitWindowsForModel(accountID, sessions, windows, "spark"); ok {
		score.ModelScores = map[string]Score{ModelKey("spark"): spark}
	}
	return score
}

func ScoreFromLimitWindowsForModel(accountID string, sessions int, windows []LimitWindow, model string) (Score, bool) {
	key := ModelKey(model)
	if key == "" {
		return scoreFromLimitWindows(accountID, sessions, windows), true
	}
	filtered := make([]LimitWindow, 0, len(windows))
	for _, window := range windows {
		if limitWindowModelKey(window) == key {
			filtered = append(filtered, window)
		}
	}
	if len(filtered) == 0 {
		return Score{AccountID: accountID, Headroom: 0, ShortHeadroom: 0, Sessions: sessions}, false
	}
	return scoreFromLimitWindows(accountID, sessions, filtered), true
}

func ModelKey(model string) string {
	if strings.Contains(strings.ToLower(model), "spark") {
		return "spark"
	}
	return ""
}

func limitWindowModelKey(window LimitWindow) string {
	if strings.Contains(strings.ToLower(window.Name), "spark") {
		return "spark"
	}
	return ""
}

func scoreFromLimitWindows(accountID string, sessions int, windows []LimitWindow) Score {
	headroom := 1.0
	shortHeadroom := 1.0
	shortResetAfterSeconds := int64(0)
	hasShortWindow := false
	for _, window := range windows {
		remaining := 1 - clampPercent(window.UsedPercent)/100
		if remaining < headroom {
			headroom = remaining
		}
		if isShortWindow(window) {
			hasShortWindow = true
			if remaining <= shortHeadroom {
				shortHeadroom = remaining
				shortResetAfterSeconds = window.ResetAfterSeconds
			}
		}
	}
	if !hasShortWindow {
		shortHeadroom = headroom
	}
	return Score{
		AccountID:              accountID,
		Headroom:               headroom,
		ShortHeadroom:          shortHeadroom,
		ShortResetAfterSeconds: shortResetAfterSeconds,
		ExpiryPressure:         expiryPressure(headroom, shortResetAfterSeconds),
		Sessions:               sessions,
	}
}

func isShortWindow(window LimitWindow) bool {
	if window.LimitWindowSeconds > 0 {
		return window.LimitWindowSeconds <= 6*60*60
	}
	return false
}

func expiryPressure(headroom float64, resetAfterSeconds int64) float64 {
	if resetAfterSeconds <= 0 {
		return 0
	}
	return headroom / float64(resetAfterSeconds)
}

func clampPercent(value float64) float64 {
	switch {
	case value < 0:
		return 0
	case value > 100:
		return 100
	default:
		return value
	}
}
