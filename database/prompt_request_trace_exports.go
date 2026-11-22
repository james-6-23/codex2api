package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"time"
)

const MaxPromptRequestTraceAttempts = 24

// Only public model families and their conventional numeric/version suffixes
// may be exported. An arbitrary provider alias could itself contain a secret.
var promptTraceModelPattern = regexp.MustCompile(`^(?:gpt-[1-9][0-9]?(?:\.[0-9]{1,2}){0,2}|o[1-9]|claude-(?:opus|sonnet|haiku)-[1-9](?:[-.][0-9]{1,2})?|gemini-[1-9](?:\.[0-9]{1,2})?|grok-[1-9](?:\.[0-9]{1,2})?)(?:-(?:codex|sol|astra|terra|mini|nano|pro|max|turbo|thinking|flash|lite|chat|preview|latest|[0-9]{4}-[0-9]{2}-[0-9]{2}|[0-9]{8})){0,4}$`)

func SafePromptTraceModel(name string) string {
	switch name {
	case "chatgpt-4o-latest", "gpt-4o", "gpt-4o-mini", "deepseek-chat", "deepseek-reasoner":
		return name
	}
	if len(name) <= 72 && promptTraceModelPattern.MatchString(name) {
		return name
	}
	return ""
}

// PromptRequestTraceAttempt carries observations from the existing usage
// logger. It must not carry provider request IDs, account IDs, or error text.
type PromptRequestTraceAttempt struct {
	FinishedAt      time.Time
	Model           string
	StatusCode      int
	DurationMs      int
	FirstTokenMs    int
	InputTokens     int
	OutputTokens    int
	TotalTokens     int
	CachedTokens    int
	ReasoningTokens int
	CacheWrite5m    int
	CacheWrite1h    int
	Stream          bool
	Retry           bool
}

type PromptRequestTraceMetrics struct {
	Attempts      []PromptRequestTraceAttempt
	TotalAttempts int
}

type PromptRequestTraceJob struct {
	RequestID   int64
	FinishedAt  time.Time
	DeliveryTry int
	Metrics     PromptRequestTraceMetrics
}

func (db *DB) ensurePromptRequestTraceExports(ctx context.Context) error {
	timestamp := "TIMESTAMPTZ"
	if db.isSQLite() {
		timestamp = "DATETIME"
	}
	if _, err := db.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS prompt_request_trace_exports (
		request_id BIGINT NOT NULL REFERENCES prompt_request_records(id) ON DELETE CASCADE,
		destination TEXT NOT NULL,
		finished_at `+timestamp+`,
		next_attempt_at `+timestamp+` NOT NULL,
		delivery_try INTEGER NOT NULL DEFAULT 0,
		exported_at `+timestamp+`,
		trace_id TEXT NOT NULL DEFAULT '',
		metrics TEXT NOT NULL DEFAULT '{}',
		PRIMARY KEY (request_id, destination)
	)`); err != nil {
		return err
	}
	if _, err := db.conn.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_prompt_request_trace_ready
		ON prompt_request_trace_exports(destination, next_attempt_at, request_id) WHERE exported_at IS NULL`); err != nil {
		return err
	}
	if db.isSQLite() {
		_, err := db.conn.ExecContext(ctx, `CREATE TRIGGER IF NOT EXISTS delete_prompt_request_trace_export
			AFTER DELETE ON prompt_request_records BEGIN
				DELETE FROM prompt_request_trace_exports WHERE request_id = OLD.id;
			END`)
		return err
	}
	return nil
}

// CompletePromptRequestTrace atomically publishes a completed local record and
// enrolls it for background delivery. The network is never involved here.
func (db *DB) CompletePromptRequestTrace(ctx context.Context, id int64, action string, status int,
	response PromptRequestResponse, finishedAt time.Time, destination string, metrics PromptRequestTraceMetrics,
) error {
	responseJSON, err := marshalPromptRequestResponse(response)
	if err != nil {
		return err
	}
	metricsJSON, err := json.Marshal(metrics)
	if err != nil {
		return err
	}
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		update, err := tx.ExecContext(ctx, `UPDATE prompt_request_records SET action=$1, status=$2 WHERE id=$3`, action, status, id)
		if err != nil {
			return err
		}
		count, err := update.RowsAffected()
		if err != nil || count == 0 {
			return err
		}
		if _, err := tx.ExecContext(ctx, upsertPromptRequestResponse, id, responseJSON); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO prompt_request_trace_exports
			(request_id, destination, finished_at, next_attempt_at, metrics)
			VALUES ($1,$2,$3,$3,$4) ON CONFLICT (request_id,destination) DO NOTHING`,
			id, destination, finishedAt, string(metricsJSON))
		return err
	})
}

func (db *DB) EnqueuePromptRequestTraceBackfill(ctx context.Context, destination string, ids []int64, now time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `INSERT INTO prompt_request_trace_exports
				(request_id, destination, next_attempt_at)
				SELECT id, $2, $3 FROM prompt_request_records WHERE id=$1
				ON CONFLICT (request_id,destination) DO NOTHING`, id, destination, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// ClaimPromptRequestTrace leases work with a single statement. Concurrent
// workers may contend for the oldest row, but only one claim can succeed
// before its lease expires. A crash leaves the row eligible for a later retry.
func (db *DB) ClaimPromptRequestTrace(ctx context.Context, destination string, now, leaseUntil time.Time) (*PromptRequestTraceJob, error) {
	var job PromptRequestTraceJob
	var finished sql.NullTime
	var metrics string
	err := db.withSQLiteWriteLock(ctx, func() error {
		return db.conn.QueryRowContext(ctx, `UPDATE prompt_request_trace_exports
			SET next_attempt_at=$3, delivery_try=delivery_try+1
			WHERE destination=$1 AND exported_at IS NULL AND next_attempt_at <= $2 AND request_id=(
				SELECT request_id FROM prompt_request_trace_exports
				WHERE destination=$1 AND exported_at IS NULL AND next_attempt_at <= $2
				ORDER BY next_attempt_at, request_id LIMIT 1
			) RETURNING request_id, finished_at, delivery_try, metrics`, destination, now, leaseUntil).
			Scan(&job.RequestID, &finished, &job.DeliveryTry, &metrics)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	job.FinishedAt = finished.Time
	if err := json.Unmarshal([]byte(metrics), &job.Metrics); err != nil {
		return nil, err
	}
	return &job, nil
}

func (db *DB) FinishPromptRequestTrace(ctx context.Context, destination string, job *PromptRequestTraceJob,
	traceID string, nextAt time.Time,
) error {
	return db.withSQLiteWriteLock(ctx, func() error {
		if traceID == "" {
			_, err := db.conn.ExecContext(ctx, `UPDATE prompt_request_trace_exports SET next_attempt_at=$4
				WHERE request_id=$1 AND destination=$2 AND delivery_try=$3 AND exported_at IS NULL`,
				job.RequestID, destination, job.DeliveryTry, nextAt)
			return err
		}
		_, err := db.conn.ExecContext(ctx, `UPDATE prompt_request_trace_exports SET exported_at=$4, trace_id=$5
			WHERE request_id=$1 AND destination=$2 AND delivery_try=$3 AND exported_at IS NULL`,
			job.RequestID, destination, job.DeliveryTry, nextAt, traceID)
		return err
	})
}

func (db *DB) GetPromptRequestTraceID(ctx context.Context, id int64, destination string) (string, error) {
	var traceID string
	err := db.conn.QueryRowContext(ctx, `SELECT trace_id FROM prompt_request_trace_exports
		WHERE request_id=$1 AND destination=$2 AND exported_at IS NOT NULL`, id, destination).Scan(&traceID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return traceID, err
}
