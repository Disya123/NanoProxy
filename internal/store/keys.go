package store

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrKeyNotFound is returned by LookupKeyByHash when no row matches.
var ErrKeyNotFound = errors.New("api key not found")

// ErrKeyDisabled is returned when the key exists but is marked disabled.
var ErrKeyDisabled = errors.New("api key disabled")

// APIKey mirrors the api_keys table. RawKey is only populated on Create.
type APIKey struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	KeyPrefix  string    `json:"key_prefix"`
	KeyHash    string    `json:"-"`
	Enabled            bool      `json:"enabled"`
	BudgetUSD          *float64  `json:"budget_usd,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	RawKey             string    `json:"raw_key,omitempty"` // populated if stored in DB
	LimitInterval      string    `json:"limit_interval"`
	LimitInputTokens   *int64    `json:"limit_input_tokens,omitempty"`
	LimitOutputTokens  *int64    `json:"limit_output_tokens,omitempty"`
	LimitTotalTokens   *int64    `json:"limit_total_tokens,omitempty"`
}

// CreateKey generates a fresh client key (sknp_<40 hex>), stores only its
// HMAC-SHA256 under the configured secret, and returns the row including the
// raw key. The caller must surface the raw key to the operator exactly once.
func (s *Store) CreateKey(ctx context.Context, secret []byte, name string, budgetUSD *float64) (APIKey, error) {
	if strings.TrimSpace(name) == "" {
		return APIKey{}, fmt.Errorf("name is required")
	}
	raw, err := generateRawKey()
	if err != nil {
		return APIKey{}, err
	}
	hash := hashKey(secret, raw)
	prefix := raw[:12] // "sknp_xxxxxx" — enough to disambiguate in the UI

	now := time.Now().UnixMilli()
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO api_keys (name, key_prefix, key_hash, enabled, budget_usd, created_at, raw_key, limit_interval)
		 VALUES (?, ?, ?, 1, ?, ?, ?, ?)`,
		name, prefix, hash, budgetUSD, now, raw, "all_time",
	)
	if err != nil {
		return APIKey{}, fmt.Errorf("insert: %w", err)
	}
	id, _ := res.LastInsertId()
	return APIKey{
		ID:        id,
		Name:      name,
		KeyPrefix: prefix,
		KeyHash:   hash,
		Enabled:   true,
		BudgetUSD: budgetUSD,
		CreatedAt: time.UnixMilli(now),
		RawKey:    raw,
		LimitInterval: "all_time",
	}, nil
}

// LookupKeyByHash resolves a raw bearer token into its row. Returns
// ErrKeyDisabled if the row is found but disabled.
func (s *Store) LookupKeyByHash(ctx context.Context, secret []byte, raw string) (APIKey, error) {
	if !strings.HasPrefix(raw, "sknp_") {
		return APIKey{}, ErrKeyNotFound
	}
	hash := hashKey(secret, raw)
	row := s.DB.QueryRowContext(ctx,
		`SELECT id, name, key_prefix, key_hash, enabled, budget_usd, created_at,
		        raw_key, limit_interval, limit_input_tokens, limit_output_tokens, limit_total_tokens
		 FROM api_keys WHERE key_hash = ?`, hash)
	var k APIKey
	var enabled int
	var budget sql.NullFloat64
	var createdMs int64
	var rawKey sql.NullString
	var limitInt sql.NullString
	var limIn, limOut, limTot sql.NullInt64

	if err := row.Scan(&k.ID, &k.Name, &k.KeyPrefix, &k.KeyHash, &enabled, &budget, &createdMs,
		&rawKey, &limitInt, &limIn, &limOut, &limTot); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return APIKey{}, ErrKeyNotFound
		}
		return APIKey{}, fmt.Errorf("scan: %w", err)
	}
	k.Enabled = enabled == 1
	if budget.Valid {
		v := budget.Float64
		k.BudgetUSD = &v
	}
	k.CreatedAt = time.UnixMilli(createdMs)
	if rawKey.Valid { k.RawKey = rawKey.String }
	if limitInt.Valid { k.LimitInterval = limitInt.String } else { k.LimitInterval = "all_time" }
	if limIn.Valid { v := limIn.Int64; k.LimitInputTokens = &v }
	if limOut.Valid { v := limOut.Int64; k.LimitOutputTokens = &v }
	if limTot.Valid { v := limTot.Int64; k.LimitTotalTokens = &v }

	if !k.Enabled {
		return k, ErrKeyDisabled
	}
	return k, nil
}

// ListKeys returns all keys (enabled and disabled), newest first.
func (s *Store) ListKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, name, key_prefix, key_hash, enabled, budget_usd, created_at,
		        raw_key, limit_interval, limit_input_tokens, limit_output_tokens, limit_total_tokens
		 FROM api_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]APIKey, 0, 8)
	for rows.Next() {
		var k APIKey
		var enabled int
		var budget sql.NullFloat64
		var createdMs int64
		var rawKey sql.NullString
		var limitInt sql.NullString
		var limIn, limOut, limTot sql.NullInt64

		if err := rows.Scan(&k.ID, &k.Name, &k.KeyPrefix, &k.KeyHash, &enabled, &budget, &createdMs,
			&rawKey, &limitInt, &limIn, &limOut, &limTot); err != nil {
			return nil, err
		}
		k.Enabled = enabled == 1
		if budget.Valid {
			v := budget.Float64
			k.BudgetUSD = &v
		}
		k.CreatedAt = time.UnixMilli(createdMs)
		if rawKey.Valid { k.RawKey = rawKey.String }
		if limitInt.Valid { k.LimitInterval = limitInt.String } else { k.LimitInterval = "all_time" }
		if limIn.Valid { v := limIn.Int64; k.LimitInputTokens = &v }
		if limOut.Valid { v := limOut.Int64; k.LimitOutputTokens = &v }
		if limTot.Valid { v := limTot.Int64; k.LimitTotalTokens = &v }

		out = append(out, k)
	}
	return out, rows.Err()
}

// SetKeyEnabled toggles a key without deleting it. Disabled keys reject traffic.
func (s *Store) SetKeyEnabled(ctx context.Context, id int64, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE api_keys SET enabled = ? WHERE id = ?`, v, id)
	return err
}

// RenameKey updates the operator-facing name.
func (s *Store) RenameKey(ctx context.Context, id int64, name string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE api_keys SET name = ? WHERE id = ?`, name, id)
	return err
}

// UpdateKeyLimits updates all limit-related fields.
func (s *Store) UpdateKeyLimits(ctx context.Context, id int64, interval string, budget *float64, in, out, tot *int64) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE api_keys 
		 SET limit_interval = ?, budget_usd = ?, limit_input_tokens = ?, limit_output_tokens = ?, limit_total_tokens = ?
		 WHERE id = ?`,
		interval, budget, in, out, tot, id,
	)
	return err
}

// DeleteKey removes a key and cascades its request history.
func (s *Store) DeleteKey(ctx context.Context, id int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM api_keys WHERE id = ?`, id)
	return err
}

// GetKeyByID is used by the admin detail view.
func (s *Store) GetKeyByID(ctx context.Context, id int64) (APIKey, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT id, name, key_prefix, key_hash, enabled, budget_usd, created_at,
		        raw_key, limit_interval, limit_input_tokens, limit_output_tokens, limit_total_tokens
		 FROM api_keys WHERE id = ?`, id)
	var k APIKey
	var enabled int
	var budget sql.NullFloat64
	var createdMs int64
	var rawKey sql.NullString
	var limitInt sql.NullString
	var limIn, limOut, limTot sql.NullInt64

	if err := row.Scan(&k.ID, &k.Name, &k.KeyPrefix, &k.KeyHash, &enabled, &budget, &createdMs,
		&rawKey, &limitInt, &limIn, &limOut, &limTot); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return APIKey{}, ErrKeyNotFound
		}
		return APIKey{}, err
	}
	k.Enabled = enabled == 1
	if budget.Valid {
		v := budget.Float64
		k.BudgetUSD = &v
	}
	k.CreatedAt = time.UnixMilli(createdMs)
	if rawKey.Valid { k.RawKey = rawKey.String }
	if limitInt.Valid { k.LimitInterval = limitInt.String } else { k.LimitInterval = "all_time" }
	if limIn.Valid { v := limIn.Int64; k.LimitInputTokens = &v }
	if limOut.Valid { v := limOut.Int64; k.LimitOutputTokens = &v }
	if limTot.Valid { v := limTot.Int64; k.LimitTotalTokens = &v }

	return k, nil
}

// ──────────────────────────── helpers ────────────────────────────

func generateRawKey() (string, error) {
	b := make([]byte, 20) // 40 hex chars
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "sknp_" + hex.EncodeToString(b), nil
}

// HashKey is exposed because the auth middleware also needs it.
func HashKey(secret, raw string) string { return hashKey([]byte(secret), raw) }

func hashKey(secret []byte, raw string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(raw))
	return hex.EncodeToString(mac.Sum(nil))
}

// LimitStatus contains the current usage and limits for an API key.
type LimitStatus struct {
	Exceeded    bool
	Reason      string // e.g. "budget_exceeded", "input_tokens_exceeded"
	UsageBudget float64
	UsageIn     int64
	UsageOut    int64
	UsageTot    int64
}

// CheckKeyLimits queries historical usage for the key based on its LimitInterval
// and returns whether it has exceeded any of its configured limits.
func (s *Store) CheckKeyLimits(ctx context.Context, k APIKey) (LimitStatus, error) {
	var st LimitStatus
	// If no limits are configured, it's never exceeded.
	if k.BudgetUSD == nil && k.LimitInputTokens == nil && k.LimitOutputTokens == nil && k.LimitTotalTokens == nil {
		return st, nil
	}

	now := time.Now()
	var startDate string
	switch k.LimitInterval {
	case "daily":
		startDate = now.Format("2006-01-02")
	case "weekly":
		// Find previous Monday
		offset := int(time.Monday - now.Weekday())
		if offset > 0 {
			offset -= 7
		}
		startDate = now.AddDate(0, 0, offset).Format("2006-01-02")
	case "monthly":
		startDate = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).Format("2006-01-02")
	case "all_time":
		startDate = "0000-00-00"
	default:
		startDate = "0000-00-00"
	}

	row := s.DB.QueryRowContext(ctx, `
		SELECT 
			COALESCE(SUM(cost_usd), 0), 
			COALESCE(SUM(input_tokens), 0), 
			COALESCE(SUM(output_tokens), 0), 
			COALESCE(SUM(input_tokens + output_tokens + reasoning_tokens), 0)
		FROM daily_stats
		WHERE api_key_id = ? AND day >= ?
	`, k.ID, startDate)

	if err := row.Scan(&st.UsageBudget, &st.UsageIn, &st.UsageOut, &st.UsageTot); err != nil {
		return st, err
	}

	if k.BudgetUSD != nil && st.UsageBudget >= *k.BudgetUSD {
		st.Exceeded = true
		st.Reason = "budget_exceeded"
		return st, nil
	}
	if k.LimitInputTokens != nil && st.UsageIn >= *k.LimitInputTokens {
		st.Exceeded = true
		st.Reason = "input_tokens_exceeded"
		return st, nil
	}
	if k.LimitOutputTokens != nil && st.UsageOut >= *k.LimitOutputTokens {
		st.Exceeded = true
		st.Reason = "output_tokens_exceeded"
		return st, nil
	}
	if k.LimitTotalTokens != nil && st.UsageTot >= *k.LimitTotalTokens {
		st.Exceeded = true
		st.Reason = "total_tokens_exceeded"
		return st, nil
	}

	return st, nil
}