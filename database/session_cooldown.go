package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

type SessionCooldownRoot struct {
	CreatedAt      int64            `json:"created_at"`
	Confirmed      bool             `json:"confirmed"`
	Leases         map[string]int64 `json:"leases,omitempty"`
	PreserveWindow bool             `json:"preserve_window,omitempty"`
}

type SessionCooldownState struct {
	AverageSeconds float64                         `json:"average_seconds,omitempty"`
	Samples        int                             `json:"samples,omitempty"`
	EvaluatedAt    time.Time                       `json:"evaluated_at,omitempty"`
	Roots          map[string]*SessionCooldownRoot `json:"roots"`
}

func (db *DB) ReadSessionCooldown(ctx context.Context, subject string) (SessionCooldownState, error) {
	var raw string
	state := SessionCooldownState{Roots: make(map[string]*SessionCooldownRoot)}
	err := db.conn.QueryRowContext(ctx, `SELECT state FROM prompt_session_cooldowns WHERE subject=$1`, subject).Scan(&raw)
	if err == sql.ErrNoRows {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	err = json.Unmarshal([]byte(raw), &state)
	return state, err
}

func (db *DB) ensureSessionCooldownTables(ctx context.Context) error {
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS prompt_session_cooldowns (subject VARCHAR(255) PRIMARY KEY, state TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS prompt_user_window_grants (subject VARCHAR(255) PRIMARY KEY, state TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS prompt_root_naming_claims (subject VARCHAR(255) NOT NULL, root VARCHAR(64) NOT NULL, created_at BIGINT NOT NULL, PRIMARY KEY(subject, root))`,
		`CREATE TABLE IF NOT EXISTS account_session_usage_successes (
			account_id BIGINT NOT NULL, period_id VARCHAR(64) NOT NULL,
			eligible_after_ms BIGINT NOT NULL, PRIMARY KEY(account_id, period_id))`,
		`CREATE INDEX IF NOT EXISTS idx_session_usage_periods_recent_user ON account_session_usage_periods(newapi_platform, newapi_user_id, last_seen)`,
	} {
		if _, err := db.conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) UpdateSessionCooldown(ctx context.Context, subject string, update func(*SessionCooldownState) error) error {
	return db.withWriteTx(ctx, func(transaction *sql.Tx) error {
		if _, err := transaction.ExecContext(ctx, `INSERT INTO prompt_session_cooldowns(subject,state) VALUES ($1,'{}')
		ON CONFLICT(subject) DO UPDATE SET state=prompt_session_cooldowns.state`, subject); err != nil {
			return err
		}
		var raw string
		if err := transaction.QueryRowContext(ctx, `SELECT state FROM prompt_session_cooldowns WHERE subject=$1`, subject).Scan(&raw); err != nil {
			return err
		}
		var state SessionCooldownState
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			return err
		}
		if state.Roots == nil {
			state.Roots = make(map[string]*SessionCooldownRoot)
		}
		if err := update(&state); err != nil {
			return err
		}
		encoded, err := json.Marshal(state)
		if err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, `UPDATE prompt_session_cooldowns SET state=$2 WHERE subject=$1`, subject, string(encoded)); err != nil {
			return err
		}
		return nil
	})
}

type SessionCooldownSample struct {
	AccountID       int64
	PeriodID        string
	DurationSeconds float64
}

func (db *DB) SessionCooldownSamples(ctx context.Context, platform, userID string, since, now time.Time, limit int) ([]SessionCooldownSample, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT o.account_id, o.period_id, `+db.sessionObservationDurationSQL()+`
		FROM account_session_usage_periods o JOIN account_session_usage_successes s
		ON s.account_id=o.account_id AND s.period_id=o.period_id
		WHERE o.newapi_platform=$1 AND o.newapi_user_id=$2 AND o.last_seen >= $3 AND s.eligible_after_ms <= $4
		ORDER BY o.last_seen DESC, o.account_id, o.period_id LIMIT $5`, platform, userID, db.timeArg(since.UTC()), now.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	samples := make([]SessionCooldownSample, 0)
	for rows.Next() {
		var sample SessionCooldownSample
		if err := rows.Scan(&sample.AccountID, &sample.PeriodID, &sample.DurationSeconds); err != nil {
			return nil, err
		}
		samples = append(samples, sample)
	}
	return samples, rows.Err()
}
