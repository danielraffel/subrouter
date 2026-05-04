package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const openAIAPIBase = "https://api.openai.com"

type modelPrice struct {
	Prefix         string
	InputUSDPerM   float64
	OutputUSDPerM  float64
	CachedDiscount float64
}

var modelPrices = []modelPrice{
	{Prefix: "gpt-5.1-codex", InputUSDPerM: 1.25, OutputUSDPerM: 10, CachedDiscount: 0.1},
	{Prefix: "gpt-5.1", InputUSDPerM: 1.25, OutputUSDPerM: 10, CachedDiscount: 0.1},
	{Prefix: "gpt-5-codex", InputUSDPerM: 1.25, OutputUSDPerM: 10, CachedDiscount: 0.1},
	{Prefix: "gpt-5-mini", InputUSDPerM: 0.25, OutputUSDPerM: 2, CachedDiscount: 0.1},
	{Prefix: "gpt-5-nano", InputUSDPerM: 0.05, OutputUSDPerM: 0.40, CachedDiscount: 0.1},
	{Prefix: "gpt-5", InputUSDPerM: 1.25, OutputUSDPerM: 10, CachedDiscount: 0.1},
	{Prefix: "gpt-4.1-mini", InputUSDPerM: 0.40, OutputUSDPerM: 1.60, CachedDiscount: 0.1},
	{Prefix: "gpt-4.1-nano", InputUSDPerM: 0.10, OutputUSDPerM: 0.40, CachedDiscount: 0.1},
	{Prefix: "gpt-4.1", InputUSDPerM: 2, OutputUSDPerM: 8, CachedDiscount: 0.1},
	{Prefix: "gpt-4o-mini", InputUSDPerM: 0.15, OutputUSDPerM: 0.60, CachedDiscount: 0.1},
	{Prefix: "gpt-4o", InputUSDPerM: 2.50, OutputUSDPerM: 10, CachedDiscount: 0.1},
	{Prefix: "gpt-4-turbo", InputUSDPerM: 10, OutputUSDPerM: 30, CachedDiscount: 0.1},
	{Prefix: "gpt-4", InputUSDPerM: 30, OutputUSDPerM: 60, CachedDiscount: 0.1},
	{Prefix: "o1-mini", InputUSDPerM: 3, OutputUSDPerM: 12, CachedDiscount: 0.1},
	{Prefix: "o1", InputUSDPerM: 15, OutputUSDPerM: 60, CachedDiscount: 0.1},
	{Prefix: "o3-mini", InputUSDPerM: 1.10, OutputUSDPerM: 4.40, CachedDiscount: 0.1},
	{Prefix: "o3", InputUSDPerM: 10, OutputUSDPerM: 40, CachedDiscount: 0.1},
	{Prefix: "o4-mini", InputUSDPerM: 1.10, OutputUSDPerM: 4.40, CachedDiscount: 0.1},
}

type openAIAdminError struct {
	Status int
	Msg    string
}

func (e openAIAdminError) Error() string {
	return e.Msg
}

func ValidateAdminKey(ctx context.Context, client *http.Client, adminKey string) error {
	var projects struct {
		Data []OpenAIProject `json:"data"`
	}
	err := fetchOpenAIAdminJSON(ctx, client, adminKey, "/v1/organization/projects", map[string][]string{"limit": []string{"1"}}, &projects)
	if err == nil {
		return nil
	}
	if oe, ok := err.(openAIAdminError); !ok || oe.Status != http.StatusForbidden {
		return err
	}
	start := strconv.FormatInt(time.Now().Unix()-86400, 10)
	var costs struct {
		Data []any `json:"data"`
	}
	return fetchOpenAIAdminJSON(ctx, client, adminKey, "/v1/organization/costs", map[string][]string{
		"start_time":   []string{start},
		"bucket_width": []string{"1d"},
		"limit":        []string{"1"},
	}, &costs)
}

func ListOpenAIProjects(ctx context.Context, client *http.Client, adminKey string) ([]OpenAIProject, error) {
	var out []OpenAIProject
	after := ""
	for page := 0; page < 20; page++ {
		params := map[string][]string{"limit": []string{"100"}}
		if after != "" {
			params["after"] = []string{after}
		}
		var res struct {
			Data    []OpenAIProject `json:"data"`
			HasMore bool            `json:"has_more"`
			LastID  string          `json:"last_id"`
		}
		if err := fetchOpenAIAdminJSON(ctx, client, adminKey, "/v1/organization/projects", params, &res); err != nil {
			return nil, err
		}
		out = append(out, res.Data...)
		if !res.HasMore || res.LastID == "" {
			break
		}
		after = res.LastID
	}
	return out, nil
}

func FetchAPIKeyUsageSnapshot(ctx context.Context, client *http.Client, admin AdminKeyEntry, account StoredCodexAccount, days int) (APIKeyUsageSnapshot, error) {
	daily, topModel, err := FetchOpenAIUsageRollup(ctx, client, admin.Key, days, account.ProjectID)
	if err != nil {
		return APIKeyUsageSnapshot{}, err
	}
	summary := SummarizeDailyUsage(daily)
	snapshot := APIKeyUsageSnapshot{
		AdminKeyLabel:      admin.Label,
		OrgID:              admin.OrgID,
		ProjectID:          account.ProjectID,
		ProjectName:        account.ProjectName,
		FetchedAt:          time.Now().UTC().Format(time.RFC3339),
		TodayUSD:           summary.TodayUSD,
		TodayCostEstimated: summary.TodayCostEstimated,
		WeekUSD:            summary.WeekUSD,
		MonthUSD:           summary.MonthUSD,
		TodayTokens:        summary.TodayTokens,
		WeekTokens:         summary.WeekTokens,
		MonthTokens:        summary.MonthTokens,
		TopModel:           topModel,
		Daily:              daily,
	}
	return snapshot, nil
}

type UsageSummary struct {
	TodayUSD           float64
	TodayCostEstimated bool
	WeekUSD            float64
	MonthUSD           float64
	TodayTokens        int64
	WeekTokens         int64
	MonthTokens        int64
}

func SummarizeDailyUsage(daily []DailyUsage) UsageSummary {
	today := time.Now().UTC().Format("2006-01-02")
	last7Start := len(daily) - 7
	if last7Start < 0 {
		last7Start = 0
	}
	last30Start := len(daily) - 30
	if last30Start < 0 {
		last30Start = 0
	}
	var out UsageSummary
	for i, row := range daily {
		tokens := row.InputTokens + row.CachedInputTokens + row.OutputTokens
		if row.Date == today {
			out.TodayUSD = row.CostUSD
			out.TodayCostEstimated = row.CostEstimated
			out.TodayTokens = tokens
		}
		if i >= last7Start {
			out.WeekUSD += row.CostUSD
			out.WeekTokens += tokens
		}
		if i >= last30Start {
			out.MonthUSD += row.CostUSD
			out.MonthTokens += tokens
		}
	}
	return out
}

func FetchOpenAIUsageRollup(ctx context.Context, client *http.Client, adminKey string, days int, projectID string) ([]DailyUsage, *TopModel, error) {
	startTime := strconv.FormatInt(time.Now().Unix()-int64(days)*86400, 10)
	limit := strconv.Itoa(days)
	if days > 30 {
		limit = "30"
	}

	costParams := map[string][]string{
		"start_time":   []string{startTime},
		"bucket_width": []string{"1d"},
		"limit":        []string{limit},
	}
	usageParams := map[string][]string{
		"start_time":   []string{startTime},
		"bucket_width": []string{"1d"},
		"limit":        []string{limit},
		"group_by[]":   []string{"model"},
	}
	if projectID != "" {
		costParams["project_ids[]"] = []string{projectID}
		usageParams["project_ids[]"] = []string{projectID}
	}

	var costsRes costsResponse
	var usageRes usageCompletionsResponse
	var today *todayHourlyResult
	var costsErr, usageErr error
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		costsErr = fetchOpenAIAdminJSON(ctx, client, adminKey, "/v1/organization/costs", costParams, &costsRes)
	}()
	go func() {
		defer wg.Done()
		usageErr = fetchOpenAIAdminJSON(ctx, client, adminKey, "/v1/organization/usage/completions", usageParams, &usageRes)
	}()
	go func() {
		defer wg.Done()
		today, _ = fetchTodayHourly(ctx, client, adminKey, projectID)
	}()
	wg.Wait()
	if costsErr != nil {
		return nil, nil, costsErr
	}
	if usageErr != nil {
		return nil, nil, usageErr
	}

	byDate := map[string]*DailyUsage{}
	ensure := func(start int64, iso string) *DailyUsage {
		date := iso
		if len(date) >= 10 {
			date = date[:10]
		} else {
			date = time.Unix(start, 0).UTC().Format("2006-01-02")
		}
		row := byDate[date]
		if row == nil {
			row = &DailyUsage{Date: date}
			byDate[date] = row
		}
		return row
	}

	for _, bucket := range costsRes.Data {
		row := ensure(bucket.StartTime, bucket.StartTimeISO)
		for _, result := range bucket.Results {
			row.CostUSD += float64(result.Amount.Value)
		}
	}

	tokensByModel := map[string]int64{}
	for _, bucket := range usageRes.Data {
		row := ensure(bucket.StartTime, bucket.StartTimeISO)
		for _, result := range bucket.Results {
			inT := result.InputTokens
			cachedT := result.InputCachedTokens
			outT := result.OutputTokens
			row.InputTokens += inT
			row.CachedInputTokens += cachedT
			row.OutputTokens += outT
			row.Requests += result.NumModelRequests
			model := result.Model
			if model == "" {
				model = "(unknown)"
			}
			tokensByModel[model] += inT + cachedT + outT
		}
	}

	if today != nil {
		row := byDate[today.Date]
		if row == nil {
			row = &DailyUsage{Date: today.Date}
			byDate[today.Date] = row
		}
		row.InputTokens = today.InputTokens
		row.CachedInputTokens = today.CachedInputTokens
		row.OutputTokens = today.OutputTokens
		row.Requests = today.Requests
		if row.CostUSD == 0 && today.EstimatedCostUSD > 0 {
			row.CostUSD = today.EstimatedCostUSD
			row.CostEstimated = true
		}
		for model, tokens := range today.TokensByModel {
			tokensByModel[model] += tokens
		}
	}

	daily := make([]DailyUsage, 0, len(byDate))
	for _, row := range byDate {
		daily = append(daily, *row)
	}
	sort.Slice(daily, func(i, j int) bool { return daily[i].Date < daily[j].Date })

	var top *TopModel
	for model, tokens := range tokensByModel {
		if top == nil || tokens > top.Tokens {
			top = &TopModel{Model: model, Tokens: tokens}
		}
	}
	return daily, top, nil
}

type costsResponse struct {
	Data []struct {
		StartTime    int64  `json:"start_time"`
		StartTimeISO string `json:"start_time_iso"`
		Results      []struct {
			Amount struct {
				Value flexibleFloat64 `json:"value"`
			} `json:"amount"`
		} `json:"results"`
	} `json:"data"`
}

type flexibleFloat64 float64

func (f *flexibleFloat64) UnmarshalJSON(body []byte) error {
	body = bytes.TrimSpace(body)
	if len(body) == 0 || bytes.Equal(body, []byte("null")) {
		*f = 0
		return nil
	}
	if len(body) >= 2 && body[0] == '"' && body[len(body)-1] == '"' {
		var s string
		if err := json.Unmarshal(body, &s); err != nil {
			return err
		}
		value, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return err
		}
		*f = flexibleFloat64(value)
		return nil
	}
	value, err := strconv.ParseFloat(string(body), 64)
	if err != nil {
		return err
	}
	*f = flexibleFloat64(value)
	return nil
}

type usageCompletionsResponse struct {
	Data []struct {
		StartTime    int64  `json:"start_time"`
		StartTimeISO string `json:"start_time_iso"`
		Results      []struct {
			InputTokens       int64  `json:"input_tokens"`
			InputCachedTokens int64  `json:"input_cached_tokens"`
			OutputTokens      int64  `json:"output_tokens"`
			NumModelRequests  int64  `json:"num_model_requests"`
			Model             string `json:"model"`
		} `json:"results"`
	} `json:"data"`
}

type todayHourlyResult struct {
	Date              string
	InputTokens       int64
	CachedInputTokens int64
	OutputTokens      int64
	Requests          int64
	EstimatedCostUSD  float64
	TokensByModel     map[string]int64
}

func fetchTodayHourly(ctx context.Context, client *http.Client, adminKey, projectID string) (*todayHourlyResult, error) {
	now := time.Now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	params := map[string][]string{
		"start_time":   []string{strconv.FormatInt(midnight.Unix(), 10)},
		"bucket_width": []string{"1h"},
		"limit":        []string{"24"},
		"group_by[]":   []string{"model"},
	}
	if projectID != "" {
		params["project_ids[]"] = []string{projectID}
	}
	var res usageCompletionsResponse
	if err := fetchOpenAIAdminJSON(ctx, client, adminKey, "/v1/organization/usage/completions", params, &res); err != nil {
		return nil, err
	}

	out := &todayHourlyResult{
		Date:          midnight.Format("2006-01-02"),
		TokensByModel: map[string]int64{},
	}
	for _, bucket := range res.Data {
		for _, result := range bucket.Results {
			inT := result.InputTokens
			cachedT := result.InputCachedTokens
			outT := result.OutputTokens
			reqs := result.NumModelRequests
			model := result.Model
			if model == "" {
				model = "(unknown)"
			}
			out.InputTokens += inT
			out.CachedInputTokens += cachedT
			out.OutputTokens += outT
			out.Requests += reqs
			out.TokensByModel[model] += inT + cachedT + outT
			if cost, ok := EstimateCostUSD(model, inT, cachedT, outT); ok {
				out.EstimatedCostUSD += cost
			}
		}
	}
	if out.Requests == 0 && out.InputTokens == 0 && out.OutputTokens == 0 {
		return nil, nil
	}
	return out, nil
}

func EstimateCostUSD(model string, inputTokens, cachedInputTokens, outputTokens int64) (float64, bool) {
	model = strings.ToLower(model)
	var price *modelPrice
	for i := range modelPrices {
		if strings.HasPrefix(model, modelPrices[i].Prefix) {
			price = &modelPrices[i]
			break
		}
	}
	if price == nil {
		return 0, false
	}
	inputCost := float64(inputTokens) / 1_000_000 * price.InputUSDPerM
	cachedCost := float64(cachedInputTokens) / 1_000_000 * price.InputUSDPerM * price.CachedDiscount
	outputCost := float64(outputTokens) / 1_000_000 * price.OutputUSDPerM
	return inputCost + cachedCost + outputCost, true
}

func fetchOpenAIAdminJSON(ctx context.Context, client *http.Client, adminKey, path string, params map[string][]string, out any) error {
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	u, err := url.Parse(openAIAPIBase + path)
	if err != nil {
		return err
	}
	q := u.Query()
	for key, values := range params {
		for _, value := range values {
			q.Add(key, value)
		}
	}
	u.RawQuery = q.Encode()

	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+adminKey)
		req.Header.Set("Accept", "application/json")
		res, err := client.Do(req)
		if err != nil {
			lastErr = err
		} else {
			defer res.Body.Close()
			if res.StatusCode >= 200 && res.StatusCode < 300 {
				return json.NewDecoder(res.Body).Decode(out)
			}
			var buf bytes.Buffer
			_, _ = buf.ReadFrom(res.Body)
			msg := strings.TrimSpace(buf.String())
			if len(msg) > 200 {
				msg = msg[:200] + "..."
			}
			lastErr = openAIAdminError{Status: res.StatusCode, Msg: fmt.Sprintf("OpenAI %d %s: %s", res.StatusCode, path, msg)}
			if res.StatusCode < 500 {
				return lastErr
			}
		}
		if attempt < 3 {
			timer := time.NewTimer(time.Duration(1<<attempt) * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return lastErr
}
