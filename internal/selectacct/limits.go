package selectacct

type LimitWindow struct {
	Name               string
	UsedPercent        float64
	LimitWindowSeconds int64
	ResetAfterSeconds  int64
}

func ScoreFromLimitWindows(accountID string, sessions int, windows []LimitWindow) Score {
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
