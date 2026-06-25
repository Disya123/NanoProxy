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
	Enabled    bool      `json:"enabled"`
	BudgetUSD  *float64  `json:"budget_usd,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	RawKey     string    `json:"raw_key,omitempty"` // only on create response
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
		`INSERT INTO api_keys (name, key_prefix, key_hash, enabled, budget_usd, created_at)
		 VALUES (?, ?, ?, 1, ?, ?)`,
		name, prefix, hash, budgetUSD, now,
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
		`SELECT id, name, key_prefix, key_hash, enabled, budget_usd, created_at
		 FROM api_keys WHERE key_hash = ?`, hash)
	var k APIKey
	var enabled int
	var budget sql.NullFloat64
	var createdMs int64
	if err := row.Scan(&k.ID, &k.Name, &k.KeyPrefix, &k.KeyHash, &enabled, &budget, &createdMs); err != nil {
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
	if !k.Enabled {
		return k, ErrKeyDisabled
	}
	return k, nil
}

// ListKeys returns all keys (enabled and disabled), newest first.
func (s *Store) ListKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, name, key_prefix, key_hash, enabled, budget_usd, created_at
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
		if err := rows.Scan(&k.ID, &k.Name, &k.KeyPrefix, &k.KeyHash, &enabled, &budget, &createdMs); err != nil {
			return nil, err
		}
		k.Enabled = enabled == 1
		if budget.Valid {
			v := budget.Float64
			k.BudgetUSD = &v
		}
		k.CreatedAt = time.UnixMilli(createdMs)
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

// SetKeyBudget updates an optional soft budget (USD). Pass nil to clear.
func (s *Store) SetKeyBudget(ctx context.Context, id int64, budget *float64) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE api_keys SET budget_usd = ? WHERE id = ?`, budget, id)
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
		`SELECT id, name, key_prefix, key_hash, enabled, budget_usd, created_at
		 FROM api_keys WHERE id = ?`, id)
	var k APIKey
	var enabled int
	var budget sql.NullFloat64
	var createdMs int64
	if err := row.Scan(&k.ID, &k.Name, &k.KeyPrefix, &k.KeyHash, &enabled, &budget, &createdMs); err != nil {
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