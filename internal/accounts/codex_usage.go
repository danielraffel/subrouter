package accounts

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"

type UsageWindow struct {
	Name               string
	UsedPercent        float64
	LimitWindowSeconds int64
	ResetAfterSeconds  int64
}

type CodexUsageDetails struct {
	PlanType     string
	Windows      []UsageWindow
	BaseWindows  []UsageWindow
	Credits      *CreditsInfo
	RawRateLimit codexRateLimitDetails
}

type CreditsInfo struct {
	HasCredits bool
	Unlimited  bool
	Balance    string
}

type codexUsageResponse struct {
	PlanType             string                     `json:"plan_type"`
	RateLimit            codexRateLimitDetails      `json:"rate_limit"`
	Credits              *codexCreditsInfo          `json:"credits"`
	AdditionalRateLimits []codexAdditionalRateLimit `json:"additional_rate_limits"`
}

type codexRateLimitDetails struct {
	Allowed         bool              `json:"allowed"`
	LimitReached    bool              `json:"limit_reached"`
	PrimaryWindow   *codexLimitWindow `json:"primary_window"`
	SecondaryWindow *codexLimitWindow `json:"secondary_window"`
}

type codexLimitWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAfterSeconds  int64   `json:"reset_after_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

type codexCreditsInfo struct {
	HasCredits bool   `json:"has_credits"`
	Unlimited  bool   `json:"unlimited"`
	Balance    string `json:"balance"`
}

type codexAdditionalRateLimit struct {
	MeteredFeature string                `json:"metered_feature"`
	LimitName      string                `json:"limit_name"`
	RateLimit      codexRateLimitDetails `json:"rate_limit"`
}

func FetchCodexUsage(ctx context.Context, client *http.Client, account Account) ([]UsageWindow, error) {
	details, err := FetchCodexUsageDetails(ctx, client, account)
	if err != nil {
		return nil, err
	}
	return details.Windows, nil
}

func FetchCodexUsageDetails(ctx context.Context, client *http.Client, account Account) (CodexUsageDetails, error) {
	if account.AuthMode != AuthModeOAuth {
		return CodexUsageDetails{}, fmt.Errorf("usage is only available for OAuth accounts")
	}
	if account.Token == "" {
		return CodexUsageDetails{}, fmt.Errorf("account has no access token")
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexUsageURL, nil)
	if err != nil {
		return CodexUsageDetails{}, err
	}
	req.Header.Set("Authorization", account.AuthorizationHeader())
	if account.AccountID != "" {
		req.Header.Set("ChatGPT-Account-ID", account.AccountID)
	}

	res, err := client.Do(req)
	if err != nil {
		return CodexUsageDetails{}, err
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return CodexUsageDetails{}, fmt.Errorf("usage fetch failed: %s", res.Status)
	}

	var usage codexUsageResponse
	if err := json.NewDecoder(res.Body).Decode(&usage); err != nil {
		return CodexUsageDetails{}, err
	}
	details := CodexUsageDetails{
		PlanType:     usage.PlanType,
		Windows:      usage.displayWindows(),
		BaseWindows:  usage.windows(),
		RawRateLimit: usage.RateLimit,
	}
	if usage.Credits != nil {
		details.Credits = &CreditsInfo{
			HasCredits: usage.Credits.HasCredits,
			Unlimited:  usage.Credits.Unlimited,
			Balance:    usage.Credits.Balance,
		}
	}
	return details, nil
}

func (u codexUsageResponse) windows() []UsageWindow {
	var windows []UsageWindow
	appendDetails := func(prefix string, details codexRateLimitDetails) {
		if details.PrimaryWindow != nil {
			windows = append(windows, UsageWindow{
				Name:               prefix + "primary",
				UsedPercent:        details.PrimaryWindow.UsedPercent,
				LimitWindowSeconds: details.PrimaryWindow.LimitWindowSeconds,
				ResetAfterSeconds:  details.PrimaryWindow.resetAfterSeconds(),
			})
		}
		if details.SecondaryWindow != nil {
			windows = append(windows, UsageWindow{
				Name:               prefix + "secondary",
				UsedPercent:        details.SecondaryWindow.UsedPercent,
				LimitWindowSeconds: details.SecondaryWindow.LimitWindowSeconds,
				ResetAfterSeconds:  details.SecondaryWindow.resetAfterSeconds(),
			})
		}
		if details.LimitReached {
			windows = append(windows, UsageWindow{Name: prefix + "reached", UsedPercent: 100})
		}
	}

	appendDetails("", u.RateLimit)
	return windows
}

func (u codexUsageResponse) displayWindows() []UsageWindow {
	windows := u.windows()
	appendDetails := func(prefix string, details codexRateLimitDetails) {
		if details.PrimaryWindow != nil {
			windows = append(windows, UsageWindow{
				Name:               prefix + "primary",
				UsedPercent:        details.PrimaryWindow.UsedPercent,
				LimitWindowSeconds: details.PrimaryWindow.LimitWindowSeconds,
				ResetAfterSeconds:  details.PrimaryWindow.resetAfterSeconds(),
			})
		}
		if details.SecondaryWindow != nil {
			windows = append(windows, UsageWindow{
				Name:               prefix + "secondary",
				UsedPercent:        details.SecondaryWindow.UsedPercent,
				LimitWindowSeconds: details.SecondaryWindow.LimitWindowSeconds,
				ResetAfterSeconds:  details.SecondaryWindow.resetAfterSeconds(),
			})
		}
		if details.LimitReached {
			windows = append(windows, UsageWindow{Name: prefix + "reached", UsedPercent: 100})
		}
	}
	for _, additional := range u.AdditionalRateLimits {
		name := additional.LimitName
		if name == "" {
			name = additional.MeteredFeature
		}
		if name == "" {
			name = "unknown"
		}
		appendDetails(name+"/", additional.RateLimit)
	}
	return windows
}

func (w codexLimitWindow) resetAfterSeconds() int64 {
	if w.ResetAfterSeconds > 0 {
		return w.ResetAfterSeconds
	}
	if w.ResetAt <= 0 {
		return 0
	}
	remaining := w.ResetAt - time.Now().Unix()
	if remaining < 0 {
		return 0
	}
	return remaining
}
