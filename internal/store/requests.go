package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// RequestRecord is the payload handed to RecordRequest after a proxied call.
// It captures every metric the dashboard needs without storing message bodies.
type RequestRecord struct {
	APIKeyID             int64
	Model                string
	Stream               bool
	StatusCode           int
	ErrorType            string // "" | "upstream_4xx" | "upstream_5xx" | "client_abort" | "tool_error" | "parse_error" | "missing_pricing_info"
	ErrorMessage         string
	PromptTokens         int
	CompletionTokens     int
	ReasoningTokens      int
	CachedTokens         int
	CacheCreationTokens  int
	CacheReadTokens      int
	TotalTokens          int
	CostUSD              float64
	PaymentSource        string
	LatencyMS            int
	HasToolCalls         bool
	ToolCallsCount       int
	ToolError            bool
	ToolErrorMessage     string
	UpstreamRequestID    string
	ClientIP             string
	UserAgent            string
	StartTime            time.Time // optional, defaults to time.Now() when zero
}

// RecordedRequest mirrors a row of the requests table for read paths.
type RecordedRequest struct {
	ID                  int64     `json:"id"`
	TS                  time.Time `json:"ts"`
	FinishedTS          *time.Time `json:"finished_ts,omitempty"`
	APIKeyID            int64     `json:"api_key_id"`
	APIKeyName          string    `json:"api_key_name"`
	Model               string    `json:"model"`
	Stream              bool      `json:"stream"`
	StatusCode          int       `json:"status_code"`
	ErrorType           string    `json:"error_type,omitempty"`
	ErrorMessage        string    `json:"error_message,omitempty"`
	PromptTokens        int       `json:"prompt_tokens"`
	CompletionTokens    int       `json:"completion_tokens"`
	ReasoningTokens     int       `json:"reasoning_tokens"`
	CachedTokens        int       `json:"cached_tokens"`
	CacheCreationTokens int       `json:"cache_creation_tokens"`
	CacheReadTokens     int       `json:"cache_read_tokens"`
	TotalTokens         int       `json:"total_tokens"`
	CostUSD             float64   `json:"cost_usd"`
	PaymentSource       string    `json:"payment_source,omitempty"`
	LatencyMS           int       `json:"latency_ms"`
	HasToolCalls        bool      `json:"has_tool_calls"`
	ToolCallsCount      int       `json:"tool_calls_count"`
	ToolError           bool      `json:"tool_error"`
	ToolErrorMsg        string    `json:"tool_error_msg,omitempty"`
	UpstreamRequestID   string    `json:"upstream_request_id,omitempty"`
	ClientIP            string    `json:"client_ip,omitempty"`
	UserAgent           string    `json:"user_agent,omitempty"`
}

// RecordRequest persists the request row and bumps both daily rollups in a
// single transaction so the dashboard never sees inconsistent aggregates.
func (s *Store) RecordRequest(ctx context.Context, r RequestRecord) (int64, error) {
	if r.StartTime.IsZero() {
		r.StartTime = time.Now()
	}
	ts := r.StartTime.UnixMilli()
	finished := time.Now().UnixMilli()
	day := r.StartTime.UTC().Format("2006-01-02")

	stream := 0
	if r.Stream {
		stream = 1
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO requests (
			ts, finished_ts, api_key_id, model, stream, status_code,
			error_type, error_message,
			prompt_tokens, completion_tokens, reasoning_tokens,
			cached_tokens, cache_creation_tokens, cache_read_tokens, total_tokens,
			cost_usd, payment_source,
			latency_ms,
			has_tool_calls, tool_calls_count,
			tool_error, tool_error_msg,
			upstream_request_id, client_ip, user_agent
		) VALUES (
			?, ?, ?, ?, ?, ?,
			?, ?,
			?, ?, ?,
			?, ?, ?, ?,
			?, ?,
			?,
			?, ?,
			?, ?,
			?, ?, ?
		)`,
		ts, finished, r.APIKeyID, r.Model, stream, r.StatusCode,
		nullable(r.ErrorType), nullable(r.ErrorMessage),
		r.PromptTokens, r.CompletionTokens, r.ReasoningTokens,
		r.CachedTokens, r.CacheCreationTokens, r.CacheReadTokens, r.TotalTokens,
		r.CostUSD, nullable(r.PaymentSource),
		r.LatencyMS,
		boolToInt(r.HasToolCalls), r.ToolCallsCount,
		boolToInt(r.ToolError), nullable(r.ToolErrorMessage),
		nullable(r.UpstreamRequestID), nullable(r.ClientIP), nullable(r.UserAgent),
	)
	if err != nil {
		return 0, fmt.Errorf("insert request: %w", err)
	}
	id, _ := res.LastInsertId()

	// Aggregate: error flag for daily rollups.
	isError := 0
	if r.StatusCode >= 400 || r.ToolError {
		isError = 1
	}

	// daily_stats per (day, key, model)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO daily_stats
			(day, api_key_id, model, requests, errors,
			 input_tokens, output_tokens, reasoning_tokens, cached_tokens,
			 cost_usd, tool_errors)
		VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(day, api_key_id, model) DO UPDATE SET
			requests         = requests         + excluded.requests,
			errors           = errors           + excluded.errors,
			input_tokens     = input_tokens     + excluded.input_tokens,
			output_tokens    = output_tokens    + excluded.output_tokens,
			reasoning_tokens = reasoning_tokens + excluded.reasoning_tokens,
			cached_tokens    = cached_tokens    + excluded.cached_tokens,
			cost_usd         = cost_usd         + excluded.cost_usd,
			tool_errors      = tool_errors      + excluded.tool_errors
	`,
		day, r.APIKeyID, r.Model, isError,
		r.PromptTokens, r.CompletionTokens, r.ReasoningTokens, r.CachedTokens,
		r.CostUSD, boolToInt(r.ToolError),
	); err != nil {
		return 0, fmt.Errorf("upsert daily_stats: %w", err)
	}

	// daily_key_totals per (day, key)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO daily_key_totals
			(day, api_key_id, requests, errors, cost_usd, tokens, cache_hits, prompt_tokens)
		VALUES (?, ?, 1, ?, ?, ?, ?, ?)
		ON CONFLICT(day, api_key_id) DO UPDATE SET
			requests     = requests     + excluded.requests,
			errors       = errors       + excluded.errors,
			cost_usd     = cost_usd     + excluded.cost_usd,
			tokens       = tokens       + excluded.tokens,
			cache_hits   = cache_hits   + excluded.cache_hits,
			prompt_tokens= prompt_tokens+ excluded.prompt_tokens
	`,
		day, r.APIKeyID, isError, r.CostUSD,
		r.TotalTokens, r.CachedTokens, r.PromptTokens,
	); err != nil {
		return 0, fmt.Errorf("upsert daily_key_totals: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// ListRequestsFilter narrows a ListRequests query. Zero-valued fields are ignored.
type ListRequestsFilter struct {
	APIKeyID  int64
	Model     string
	StatusMin int    // requests with status_code >= StatusMin (e.g. 400 for errors only)
	StatusMax int
	FromMS    int64  // inclusive
	ToMS      int64  // inclusive
	HasTools  *bool
	HasError  *bool
	Limit     int    // 0 → 50
	Offset    int
}

// ListRequests returns the most recent matching requests, joined with the key
// name for the dashboard.
func (s *Store) ListRequests(ctx context.Context, f ListRequestsFilter) ([]RecordedRequest, int, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 50
	}

	var (
		conds []string
		args  []any
	)
	if f.APIKeyID > 0 {
		conds = append(conds, "r.api_key_id = ?")
		args = append(args, f.APIKeyID)
	}
	if f.Model != "" {
		conds = append(conds, "r.model = ?")
		args = append(args, f.Model)
	}
	if f.StatusMin > 0 {
		conds = append(conds, "r.status_code >= ?")
		args = append(args, f.StatusMin)
	}
	if f.StatusMax > 0 {
		conds = append(conds, "r.status_code <= ?")
		args = append(args, f.StatusMax)
	}
	if f.FromMS > 0 {
		conds = append(conds, "r.ts >= ?")
		args = append(args, f.FromMS)
	}
	if f.ToMS > 0 {
		conds = append(conds, "r.ts <= ?")
		args = append(args, f.ToMS)
	}
	if f.HasTools != nil {
		conds = append(conds, "r.has_tool_calls = ?")
		args = append(args, boolToInt(*f.HasTools))
	}
	if f.HasError != nil {
		conds = append(conds, "(r.status_code >= 400 OR r.tool_error = 1)")
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}

	// total count for pagination
	var total int
	if err := s.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM requests r"+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count: %w", err)
	}

	q := `SELECT r.id, r.ts, r.finished_ts, r.api_key_id, k.name, r.model, r.stream,
		         r.status_code, COALESCE(r.error_type,''), COALESCE(r.error_message,''),
		         r.prompt_tokens, r.completion_tokens, r.reasoning_tokens,
		         r.cached_tokens, r.cache_creation_tokens, r.cache_read_tokens,
		         r.total_tokens, r.cost_usd, COALESCE(r.payment_source,''),
		         COALESCE(r.latency_ms,0), r.has_tool_calls, r.tool_calls_count,
		         r.tool_error, COALESCE(r.tool_error_msg,''),
		         COALESCE(r.upstream_request_id,''), COALESCE(r.client_ip,''),
		         COALESCE(r.user_agent,'')
		  FROM requests r
		  JOIN api_keys k ON k.id = r.api_key_id` +
		where + " ORDER BY r.ts DESC LIMIT ? OFFSET ?"
	args = append(args, f.Limit, f.Offset)

	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list: %w", err)
	}
	defer rows.Close()

	out := make([]RecordedRequest, 0, f.Limit)
	for rows.Next() {
		var rr RecordedRequest
		var ts, finished sql.NullInt64
		var stream, hasTools, toolErr int
		if err := rows.Scan(
			&rr.ID, &ts, &finished, &rr.APIKeyID, &rr.APIKeyName, &rr.Model, &stream,
			&rr.StatusCode, &rr.ErrorType, &rr.ErrorMessage,
			&rr.PromptTokens, &rr.CompletionTokens, &rr.ReasoningTokens,
			&rr.CachedTokens, &rr.CacheCreationTokens, &rr.CacheReadTokens,
			&rr.TotalTokens, &rr.CostUSD, &rr.PaymentSource,
			&rr.LatencyMS, &hasTools, &rr.ToolCallsCount,
			&toolErr, &rr.ToolErrorMsg,
			&rr.UpstreamRequestID, &rr.ClientIP, &rr.UserAgent,
		); err != nil {
			return nil, 0, err
		}
		if ts.Valid {
			rr.TS = time.UnixMilli(ts.Int64)
		}
		if finished.Valid {
			t := time.UnixMilli(finished.Int64)
			rr.FinishedTS = &t
		}
		rr.Stream = stream == 1
		rr.HasToolCalls = hasTools == 1
		rr.ToolError = toolErr == 1
		out = append(out, rr)
	}
	return out, total, rows.Err()
}

// GetRequest fetches one row by ID.
func (s *Store) GetRequest(ctx context.Context, id int64) (RecordedRequest, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT r.id, r.ts, r.finished_ts, r.api_key_id, k.name, r.model, r.stream,
		       r.status_code, COALESCE(r.error_type,''), COALESCE(r.error_message,''),
		       r.prompt_tokens, r.completion_tokens, r.reasoning_tokens,
		       r.cached_tokens, r.cache_creation_tokens, r.cache_read_tokens,
		       r.total_tokens, r.cost_usd, COALESCE(r.payment_source,''),
		       COALESCE(r.latency_ms,0), r.has_tool_calls, r.tool_calls_count,
		       r.tool_error, COALESCE(r.tool_error_msg,''),
		       COALESCE(r.upstream_request_id,''), COALESCE(r.client_ip,''),
		       COALESCE(r.user_agent,'')
		FROM requests r
		JOIN api_keys k ON k.id = r.api_key_id
		WHERE r.id = ?`, id)

	var rr RecordedRequest
	var ts, finished sql.NullInt64
	var stream, hasTools, toolErr int
	if err := row.Scan(
		&rr.ID, &ts, &finished, &rr.APIKeyID, &rr.APIKeyName, &rr.Model, &stream,
		&rr.StatusCode, &rr.ErrorType, &rr.ErrorMessage,
		&rr.PromptTokens, &rr.CompletionTokens, &rr.ReasoningTokens,
		&rr.CachedTokens, &rr.CacheCreationTokens, &rr.CacheReadTokens,
		&rr.TotalTokens, &rr.CostUSD, &rr.PaymentSource,
		&rr.LatencyMS, &hasTools, &rr.ToolCallsCount,
		&toolErr, &rr.ToolErrorMsg,
		&rr.UpstreamRequestID, &rr.ClientIP, &rr.UserAgent,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return rr, ErrKeyNotFound
		}
		return rr, err
	}
	if ts.Valid {
		rr.TS = time.UnixMilli(ts.Int64)
	}
	if finished.Valid {
		t := time.UnixMilli(finished.Int64)
		rr.FinishedTS = &t
	}
	rr.Stream = stream == 1
	rr.HasToolCalls = hasTools == 1
	rr.ToolError = toolErr == 1
	return rr, nil
}

// DistinctModels returns the set of models seen in requests, sorted desc by use.
// Useful for filter dropdowns.
func (s *Store) DistinctModels(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT model, COUNT(*) AS c FROM requests GROUP BY model ORDER BY c DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0, 16)
	for rows.Next() {
		var m string
		var c int
		if err := rows.Scan(&m, &c); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ──────────────────────────── helpers ────────────────────────────

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}