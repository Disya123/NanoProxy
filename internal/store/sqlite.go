// Package store wraps the SQLite database used by nano-proxy for telemetry.
//
// The schema is intentionally narrow: one row per request, plus two rollup
// tables to keep dashboard aggregations cheap. WAL mode is enabled for
// concurrent reads during stream flushes.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver
)

// Store is the top-level handle. Safe for concurrent use; protects writes with
// transactions where multiple tables are mutated together.
type Store struct {
	DB *sql.DB
}

// Open initializes the SQLite database, runs migrations, and starts a
// background retention-cleanup loop.
//
// dbPath is created if missing; the parent directory is created lazily.
func Open(dbPath string, retentionDays int) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}

	// DSN enables WAL + NORMAL sync (durable enough, low write amplification).
	// busy_timeout guards against transient lock contention on stream flushes.
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)",
		dbPath,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	db.SetMaxOpenConns(8)   // SQLite writes are serialized; reads use the rest
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(0) // pure-Go driver doesn't reap; keep connections

	if err := db.PingContext(context.Background()); err != nil {
		return nil, fmt.Errorf("ping: %w", err)
	}

	s := &Store{DB: db}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}

	if retentionDays > 0 {
		go s.cleanupLoop(retentionDays)
	}
	return s, nil
}

// Close shuts the underlying database.
func (s *Store) Close() error { return s.DB.Close() }

// migrate creates the schema if absent. Idempotent.
func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS api_keys (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			name        TEXT    NOT NULL,
			key_prefix  TEXT    NOT NULL,
			key_hash    TEXT    NOT NULL UNIQUE,
			enabled     INTEGER NOT NULL DEFAULT 1,
			budget_usd  REAL,
			created_at  INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_api_keys_enabled ON api_keys(enabled)`,

		`CREATE TABLE IF NOT EXISTS requests (
			id                       INTEGER PRIMARY KEY AUTOINCREMENT,
			ts                       INTEGER NOT NULL,
			finished_ts              INTEGER,
			api_key_id               INTEGER NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
			model                    TEXT    NOT NULL,
			stream                   INTEGER NOT NULL,
			status_code              INTEGER NOT NULL,
			error_type               TEXT,
			error_message            TEXT,
			prompt_tokens            INTEGER NOT NULL DEFAULT 0,
			completion_tokens        INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens         INTEGER NOT NULL DEFAULT 0,
			cached_tokens            INTEGER NOT NULL DEFAULT 0,
			cache_creation_tokens    INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens        INTEGER NOT NULL DEFAULT 0,
			total_tokens             INTEGER NOT NULL DEFAULT 0,
			cost_usd                 REAL    NOT NULL DEFAULT 0,
			payment_source           TEXT,
			latency_ms               INTEGER,
			has_tool_calls           INTEGER NOT NULL DEFAULT 0,
			tool_calls_count         INTEGER NOT NULL DEFAULT 0,
			tool_error               INTEGER NOT NULL DEFAULT 0,
			tool_error_msg           TEXT,
			upstream_request_id      TEXT,
			client_ip                TEXT,
			user_agent               TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_requests_ts        ON requests(ts DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_requests_key_ts    ON requests(api_key_id, ts DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_requests_model_ts  ON requests(model, ts DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_requests_status    ON requests(status_code)`,
		`CREATE INDEX IF NOT EXISTS idx_requests_tool_err  ON requests(tool_error) WHERE tool_error = 1`,

		`CREATE TABLE IF NOT EXISTS daily_stats (
			day                TEXT    NOT NULL,
			api_key_id         INTEGER NOT NULL,
			model              TEXT    NOT NULL,
			requests           INTEGER NOT NULL DEFAULT 0,
			errors             INTEGER NOT NULL DEFAULT 0,
			input_tokens       INTEGER NOT NULL DEFAULT 0,
			output_tokens      INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens   INTEGER NOT NULL DEFAULT 0,
			cached_tokens      INTEGER NOT NULL DEFAULT 0,
			cost_usd           REAL    NOT NULL DEFAULT 0,
			tool_errors        INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (day, api_key_id, model)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_daily_stats_day ON daily_stats(day)`,

		`CREATE TABLE IF NOT EXISTS daily_key_totals (
			day        TEXT    NOT NULL,
			api_key_id INTEGER NOT NULL,
			requests   INTEGER NOT NULL DEFAULT 0,
			errors     INTEGER NOT NULL DEFAULT 0,
			cost_usd   REAL    NOT NULL DEFAULT 0,
			tokens     INTEGER NOT NULL DEFAULT 0,
			cache_hits INTEGER NOT NULL DEFAULT 0,
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (day, api_key_id)
		)`,

		// settings — runtime-mutable configuration stored in DB and edited
		// from the admin UI without a restart. Keys are dotted paths:
		//   "upstream.api_key"      — bearer token sent to nano-gpt.com
		`CREATE TABLE IF NOT EXISTS settings (
			key        TEXT PRIMARY KEY,
			value      TEXT NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
	}
	for _, q := range stmts {
		if _, err := s.DB.Exec(q); err != nil {
			return fmt.Errorf("exec %q: %w", truncate(q, 60), err)
		}
	}
	return nil
}

// cleanupLoop prunes old request rows once per hour. Safe to run forever.
func (s *Store) cleanupLoop(retentionDays int) {
	t := time.NewTicker(1 * time.Hour)
	defer t.Stop()
	for range t.C {
		cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour).UnixMilli()
		_, _ = s.DB.Exec(`DELETE FROM requests WHERE ts < ?`, cutoff)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}