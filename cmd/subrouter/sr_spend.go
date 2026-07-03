package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"
)

type bedrockModelAggView struct {
	Requests     int     `json:"requests"`
	TotalUSD     float64 `json:"total_usd"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
}

type bedrockCostSummaryView struct {
	Requests         int                            `json:"requests"`
	TotalUSD         float64                        `json:"total_usd"`
	TodayUSD         float64                        `json:"today_usd"`
	Week7dUSD        float64                        `json:"week_7d_usd"`
	Month30dUSD      float64                        `json:"month_30d_usd"`
	InputTokens      int64                          `json:"input_tokens"`
	OutputTokens     int64                          `json:"output_tokens"`
	CacheReadTokens  int64                          `json:"cache_read_tokens"`
	CacheWriteTokens int64                          `json:"cache_write_tokens"`
	ByModel          map[string]bedrockModelAggView `json:"by_model"`
}

// spend reports AWS Bedrock spend tracked by the default Subrouter server.
func (r srRunner) spend(ctx context.Context) error {
	server, ok, err := r.defaultRemoteServer()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("sr spend needs a default Subrouter server; run '%s server use <name>'", r.programOrSubrouter())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/_subrouter/bedrock-cost", nil)
	if err != nil {
		return err
	}
	addServerAdminAuth(req, server)
	client := r.client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("bedrock cost fetch failed: %s", res.Status)
	}
	var summary bedrockCostSummaryView
	if err := json.NewDecoder(res.Body).Decode(&summary); err != nil {
		return err
	}

	fmt.Fprintf(r.out, "AWS Bedrock spend (server %s)\n", server.Name)
	fmt.Fprintf(r.out, "  today     $%s\n", fmtUSD4(summary.TodayUSD))
	fmt.Fprintf(r.out, "  last 7d   $%s\n", fmtUSD4(summary.Week7dUSD))
	fmt.Fprintf(r.out, "  last 30d  $%s\n", fmtUSD4(summary.Month30dUSD))
	fmt.Fprintf(r.out, "  all-time  $%s   (%d requests)\n", fmtUSD4(summary.TotalUSD), summary.Requests)
	fmt.Fprintf(r.out, "  tokens    %s in / %s out / %s cache-write / %s cache-read\n",
		fmtTokens(summary.InputTokens), fmtTokens(summary.OutputTokens),
		fmtTokens(summary.CacheWriteTokens), fmtTokens(summary.CacheReadTokens))
	if len(summary.ByModel) > 0 {
		fmt.Fprintln(r.out, "  by model:")
		models := make([]string, 0, len(summary.ByModel))
		for m := range summary.ByModel {
			models = append(models, m)
		}
		sort.Slice(models, func(i, j int) bool {
			if summary.ByModel[models[i]].TotalUSD != summary.ByModel[models[j]].TotalUSD {
				return summary.ByModel[models[i]].TotalUSD > summary.ByModel[models[j]].TotalUSD
			}
			return models[i] < models[j]
		})
		for _, m := range models {
			agg := summary.ByModel[m]
			fmt.Fprintf(r.out, "    %-26s $%s  (%d req)\n", m, fmtUSD4(agg.TotalUSD), agg.Requests)
		}
	}
	if summary.Requests == 0 {
		fmt.Fprintln(r.out, "  no Bedrock requests recorded yet")
	}
	return nil
}

func fmtUSD4(v float64) string {
	if v > 0 && v < 0.01 {
		return fmt.Sprintf("%.4f", v)
	}
	return fmt.Sprintf("%.2f", v)
}
