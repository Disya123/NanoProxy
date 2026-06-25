package store

import (
	"context"
	"fmt"
	"time"
)

// Range describes a closed UTC day interval: ["YYYY-MM-DD", "YYYY-MM-DD"].
// If To is empty, only From is used (single day).
type Range struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// ResolveMS converts Range to unix-ms bounds. If To is empty, defaults to the
// end of From (next day 00:00 UTC). If both are empty, defaults to last 30d.
func (r Range) ResolveMS() (fromMS, toMS int64) {
	if r.From == "" && r.To == "" {
		now := time.Now().UTC()
		to := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
		return to.Add(-30 * 24 * time.Hour).UnixMilli(), to.UnixMilli()
	}
	from, err1 := time.Parse("2006-01-02", r.From)
	to, err2 := time.Parse("2006-01-02", r.To)
	if err1 != nil || err2 != nil {
		return 0, 0
	}
	fromMS = from.UnixMilli()
	if r.To == "" || r.To == r.From {
		toMS = from.Add(24 * time.Hour).UnixMilli()
	} else {
		toMS = to.Add(24 * time.Hour).UnixMilli()
	}
	return fromMS, toMS
}

// Summary is the headline KPI block.
type Summary struct {
	Range        Range  `json:"range"`
	Requests     int    `json:"requests"`
	Errors       int    `json:"errors"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	Reasoning    int64  `json:"reasoning_tokens"`
	TotalTokens  int64  `json:"total_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	CacheHits    int64   `json:"cache_hits"`
	CacheHitRate float64 `json:"cache_hit_rate"`
	AvgLatencyMS int     `json:"avg_latency_ms"`
	ToolErrors   int     `json:"tool_errors"`
}

// Summary returns aggregate KPIs for the range, computed from requests.
func (s *Store) Summary(ctx context.Context, r Range) (Summary, error) {
	fromMS, toMS := r.ResolveMS()
	out := Summary{Range: r}

	row := s.DB.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN status_code >= 400 OR tool_error = 1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(prompt_tokens), 0),
			COALESCE(SUM(completion_tokens), 0),
			COALESCE(SUM(reasoning_tokens), 0),
			COALESCE(SUM(total_tokens), 0),
			COALESCE(SUM(cost_usd), 0.0),
			COALESCE(SUM(cached_tokens), 0),
			COALESCE(AVG(latency_ms), 0),
			COALESCE(SUM(tool_error), 0)
		FROM requests
		WHERE ts >= ? AND ts < ?`,
		fromMS, toMS)

	var avgLatency float64
	if err := row.Scan(
		&out.Requests, &out.Errors,
		&out.InputTokens, &out.OutputTokens, &out.Reasoning, &out.TotalTokens,
		&out.CostUSD, &out.CacheHits, &avgLatency, &out.ToolErrors,
	); err != nil {
		return out, fmt.Errorf("summary: %w", err)
	}
	out.AvgLatencyMS = int(avgLatency)
	if out.InputTokens > 0 {
		out.CacheHitRate = float64(out.CacheHits) / float64(out.InputTokens)
	}
	return out, nil
}

// TimePoint is one bucket in a time-series chart.
type TimePoint struct {
	Day           string  `json:"day"`            // "YYYY-MM-DD"
	Requests      int     `json:"requests"`
	Errors        int     `json:"errors"`
	InputTokens   int64   `json:"input_tokens"`
	OutputTokens  int64   `json:"output_tokens"`
	CostUSD       float64 `json:"cost_usd"`
	ToolErrors    int     `json:"tool_errors"`
}

// TimeSeries returns one row per day across the range.
func (s *Store) TimeSeries(ctx context.Context, r Range, apiKeyID int64) ([]TimePoint, error) {
	fromMS, toMS := r.ResolveMS()
	q := `SELECT day,
	             SUM(requests), SUM(errors),
	             SUM(input_tokens), SUM(output_tokens),
	             SUM(cost_usd), SUM(tool_errors)
	      FROM daily_stats
	      WHERE day >= ? AND day < ?
	        AND (? = 0 OR api_key_id = ?)
	      GROUP BY day
	      ORDER BY day`
	rows, err := s.DB.QueryContext(ctx, q,
		fmtDay(fromMS), fmtDay(toMS), apiKeyID, apiKeyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]TimePoint, 0, 32)
	for rows.Next() {
		var tp TimePoint
		if err := rows.Scan(&tp.Day, &tp.Requests, &tp.Errors,
			&tp.InputTokens, &tp.OutputTokens, &tp.CostUSD, &tp.ToolErrors); err != nil {
			return nil, err
		}
		out = append(out, tp)
	}
	return out, rows.Err()
}

// KeyTotal is one row of the per-key summary table.
type KeyTotal struct {
	APIKeyID    int64   `json:"api_key_id"`
	APIKeyName  string  `json:"api_key_name"`
	KeyPrefix   string  `json:"key_prefix"`
	Requests    int     `json:"requests"`
	Errors      int     `json:"errors"`
	CostUSD     float64 `json:"cost_usd"`
	TotalTokens int64   `json:"total_tokens"`
	CacheHits   int64   `json:"cache_hits"`
}

// TopKeys returns the top N keys by cost over the range.
func (s *Store) TopKeys(ctx context.Context, r Range, limit int) ([]KeyTotal, error) {
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	fromMS, toMS := r.ResolveMS()
	q := `SELECT k.id, k.name, k.key_prefix,
	             COALESCE(SUM(d.requests), 0),
	             COALESCE(SUM(d.errors), 0),
	             COALESCE(SUM(d.cost_usd), 0.0),
	             COALESCE(SUM(d.input_tokens + d.output_tokens), 0),
	             COALESCE(SUM(d.cached_tokens), 0)
	      FROM api_keys k
	      LEFT JOIN daily_key_totals d
	        ON d.api_key_id = k.id
	       AND d.day >= ? AND d.day < ?
	      GROUP BY k.id, k.name, k.key_prefix
	      ORDER BY 6 DESC
	      LIMIT ?`
	rows, err := s.DB.QueryContext(ctx, q, fmtDay(fromMS), fmtDay(toMS), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]KeyTotal, 0, limit)
	for rows.Next() {
		var kt KeyTotal
		if err := rows.Scan(&kt.APIKeyID, &kt.APIKeyName, &kt.KeyPrefix,
			&kt.Requests, &kt.Errors, &kt.CostUSD, &kt.TotalTokens, &kt.CacheHits); err != nil {
			return nil, err
		}
		out = append(out, kt)
	}
	return out, rows.Err()
}

// ModelTotal is one row of the per-model summary table.
type ModelTotal struct {
	Model        string  `json:"model"`
	Requests     int     `json:"requests"`
	Errors       int     `json:"errors"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	CacheHits    int64   `json:"cache_hits"`
}

// TopModels returns the top N models by cost over the range.
func (s *Store) TopModels(ctx context.Context, r Range, limit int) ([]ModelTotal, error) {
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	fromMS, toMS := r.ResolveMS()
	q := `SELECT model,
	             SUM(requests), SUM(errors),
	             SUM(input_tokens), SUM(output_tokens),
	             SUM(cost_usd), SUM(cached_tokens)
	      FROM daily_stats
	      WHERE day >= ? AND day < ?
	      GROUP BY model
	      ORDER BY 6 DESC
	      LIMIT ?`
	rows, err := s.DB.QueryContext(ctx, q, fmtDay(fromMS), fmtDay(toMS), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ModelTotal, 0, limit)
	for rows.Next() {
		var mt ModelTotal
		if err := rows.Scan(&mt.Model, &mt.Requests, &mt.Errors,
			&mt.InputTokens, &mt.OutputTokens, &mt.CostUSD, &mt.CacheHits); err != nil {
			return nil, err
		}
		out = append(out, mt)
	}
	return out, rows.Err()
}

// ──────────────────────────── helpers ────────────────────────────

// fmtDay returns the UTC YYYY-MM-DD for the given unix-ms timestamp.
func fmtDay(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02")
}