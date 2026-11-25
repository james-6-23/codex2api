package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const DefaultPromptRequestRetentionDays = 30

var ErrInvalidPromptRequestRetention = errors.New("request retention must be 1, 7, 30 or 90 days")

type PromptRequestRecord struct {
	ID        int64     `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Endpoint  string    `json:"endpoint"`
	Transport string    `json:"transport"`
	Model     string    `json:"model"`
	APIKey    string    `json:"api_key_name"`
	Action    string    `json:"action"`
	Status    int       `json:"status"`
}

type PromptRequestBody struct {
	PromptRequestRecord
	APIKeyID          int64  `json:"-"`
	ContentType       string `json:"-"`
	Body              []byte `json:"-"`
	ConversationScope string `json:"-"`
	ConversationGroup string `json:"-"`
}

func (db *DB) ensurePromptRequestTable(ctx context.Context) error {
	id, blob, timestamp := "BIGSERIAL PRIMARY KEY", "BYTEA", "TIMESTAMPTZ"
	if db.isSQLite() {
		id, blob, timestamp = "INTEGER PRIMARY KEY AUTOINCREMENT", "BLOB", "DATETIME"
	}
	if _, err := db.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS prompt_request_records (
		id `+id+`,
		created_at `+timestamp+` NOT NULL,
		endpoint TEXT NOT NULL,
		transport TEXT NOT NULL,
		model TEXT NOT NULL,
		api_key_name TEXT NOT NULL,
		action TEXT NOT NULL DEFAULT 'received',
		status INTEGER NOT NULL DEFAULT 0,
		content_type TEXT NOT NULL,
		body `+blob+` NOT NULL,
		conversation_scope TEXT NOT NULL DEFAULT '',
		conversation_group TEXT NOT NULL DEFAULT '',
		task_anchor TEXT,
		task_turn TEXT NOT NULL DEFAULT '',
		task_preview TEXT NOT NULL DEFAULT '',
		chat_key_id BIGINT,
		chat_indexed INTEGER NOT NULL DEFAULT 0,
		chat_turn_root BIGINT NOT NULL DEFAULT 0,
		chat_replay_kind TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		return err
	}
	if db.isSQLite() {
		for _, column := range []string{"conversation_scope", "conversation_group"} {
			if err := db.ensureSQLiteColumn(ctx, "prompt_request_records", column, "TEXT NOT NULL DEFAULT ''"); err != nil {
				return err
			}
		}
		if err := db.ensureSQLiteColumn(ctx, "prompt_request_records", "task_anchor", "TEXT"); err != nil {
			return err
		}
		for _, column := range []string{"task_turn", "task_preview"} {
			if err := db.ensureSQLiteColumn(ctx, "prompt_request_records", column, "TEXT NOT NULL DEFAULT ''"); err != nil {
				return err
			}
		}
		if err := db.ensureSQLiteColumn(ctx, "prompt_request_records", "chat_key_id", "BIGINT"); err != nil {
			return err
		}
		if err := db.ensureSQLiteColumn(ctx, "prompt_request_records", "chat_indexed", "INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
		if err := db.ensureSQLiteColumn(ctx, "prompt_request_records", "chat_turn_root", "BIGINT NOT NULL DEFAULT 0"); err != nil {
			return err
		}
		if err := db.ensureSQLiteColumn(ctx, "prompt_request_records", "chat_replay_kind", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	} else if _, err := db.conn.ExecContext(ctx, `ALTER TABLE prompt_request_records
		ADD COLUMN IF NOT EXISTS conversation_scope TEXT NOT NULL DEFAULT '',
		ADD COLUMN IF NOT EXISTS conversation_group TEXT NOT NULL DEFAULT '',
		ADD COLUMN IF NOT EXISTS task_anchor TEXT,
		ADD COLUMN IF NOT EXISTS task_turn TEXT NOT NULL DEFAULT '',
		ADD COLUMN IF NOT EXISTS task_preview TEXT NOT NULL DEFAULT '',
		ADD COLUMN IF NOT EXISTS chat_key_id BIGINT,
		ADD COLUMN IF NOT EXISTS chat_indexed INTEGER NOT NULL DEFAULT 0,
		ADD COLUMN IF NOT EXISTS chat_turn_root BIGINT NOT NULL DEFAULT 0,
		ADD COLUMN IF NOT EXISTS chat_replay_kind TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if _, err := db.conn.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_prompt_request_records_conversation
		ON prompt_request_records (conversation_scope, id) WHERE conversation_scope <> ''`); err != nil {
		return err
	}
	if _, err := db.conn.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_prompt_request_records_group
		ON prompt_request_records (conversation_group, created_at, id)
		WHERE conversation_group <> '' AND conversation_group <> 'single'`); err != nil {
		return err
	}
	if _, err := db.conn.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_prompt_request_records_expiry ON prompt_request_records (created_at, id)`); err != nil {
		return err
	}
	if _, err := db.conn.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_prompt_request_records_task_pending
		ON prompt_request_records (id) WHERE task_anchor IS NULL`); err != nil {
		return err
	}
	if err := db.ensurePromptRequestChatIndexes(ctx); err != nil {
		return err
	}
	_, err := db.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS prompt_request_settings (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		retention_days INTEGER NOT NULL CHECK (retention_days IN (1, 7, 30, 90))
	)`)
	if err != nil {
		return err
	}
	if err := db.ensurePromptRequestExclusions(ctx); err != nil {
		return err
	}
	if err := db.ensurePromptRequestResponseTable(ctx); err != nil {
		return err
	}
	if err := db.ensurePromptRequestTraceExports(ctx); err != nil {
		return err
	}
	return db.ensurePromptTaskClientTable(ctx)
}

const promptRequestStoredRetention = `(SELECT retention_days FROM prompt_request_settings WHERE id = 1)`

func (db *DB) GetPromptRequestRetentionDays(ctx context.Context) (int, error) {
	var days int
	err := db.conn.QueryRowContext(ctx, `SELECT COALESCE(`+promptRequestStoredRetention+`, $1)`,
		DefaultPromptRequestRetentionDays).Scan(&days)
	return days, err
}

func (db *DB) SetPromptRequestRetentionDays(ctx context.Context, days int) error {
	switch days {
	case 1, 7, 30, 90:
	default:
		return ErrInvalidPromptRequestRetention
	}
	return db.withSQLiteWriteLock(ctx, func() error {
		_, err := db.conn.ExecContext(ctx, `INSERT INTO prompt_request_settings (id, retention_days) VALUES (1, $1)
			ON CONFLICT (id) DO UPDATE SET retention_days = excluded.retention_days`, days)
		return err
	})
}

func (db *DB) InsertPromptRequest(ctx context.Context, input PromptRequestBody) (int64, error) {
	var id int64
	if input.Body == nil {
		input.Body = []byte{}
	}
	if input.ConversationGroup == "" {
		input.ConversationGroup = singlePromptRequestGroup
	}
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if !db.isSQLite() && input.APIKeyID > 0 {
			if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock_shared(hashtextextended('prompt-request-recording:' || $1::bigint::text, 0))`, input.APIKeyID); err != nil {
				return err
			}
		}
		var excluded bool
		if input.APIKeyID > 0 {
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM api_keys WHERE id=$1 AND prompt_request_excluded=TRUE)`,
				input.APIKeyID).Scan(&excluded); err != nil {
				return err
			}
		}
		if excluded {
			return nil
		}
		task := promptRequestTaskMetadata(input.Body)
		return tx.QueryRowContext(ctx, `INSERT INTO prompt_request_records
			(created_at, endpoint, transport, model, api_key_name, content_type, body, conversation_scope, conversation_group, task_anchor, task_turn, task_preview, chat_key_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING id`,
			input.CreatedAt, input.Endpoint, input.Transport, input.Model, input.APIKey, input.ContentType,
			input.Body, input.ConversationScope, input.ConversationGroup, task.Anchor, task.Turn, task.Preview, input.APIKeyID).Scan(&id)
	})
	return id, err
}

func (db *DB) FinishPromptRequest(ctx context.Context, id int64, action string, status int) error {
	return db.withSQLiteWriteLock(ctx, func() error {
		_, err := db.conn.ExecContext(ctx, `UPDATE prompt_request_records SET action = $1, status = $2 WHERE id = $3`, action, status, id)
		return err
	})
}

const promptRequestSummaryColumns = `id, created_at, endpoint, transport, model, api_key_name, action, status`

func (r *PromptRequestRecord) scanTargets() []any {
	return []any{&r.ID, &r.CreatedAt, &r.Endpoint, &r.Transport, &r.Model, &r.APIKey, &r.Action, &r.Status}
}

func (db *DB) ListPromptRequests(ctx context.Context, before int64, limit int) ([]PromptRequestRecord, error) {
	query := `SELECT ` + promptRequestSummaryColumns + ` FROM prompt_request_visible_records`
	var rows *sql.Rows
	var err error
	if before > 0 {
		rows, err = db.conn.QueryContext(ctx, query+` WHERE id < $1 ORDER BY id DESC LIMIT $2`, before, limit)
	} else {
		rows, err = db.conn.QueryContext(ctx, query+` ORDER BY id DESC LIMIT $1`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]PromptRequestRecord, 0, limit)
	for rows.Next() {
		var item PromptRequestRecord
		if err := rows.Scan(item.scanTargets()...); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (db *DB) GetPromptRequest(ctx context.Context, id int64) (PromptRequestBody, error) {
	return db.getPromptRequest(ctx, id, "prompt_request_visible_records")
}

// GetStoredPromptRequest reads an archived record for trace exports already
// enrolled or explicitly requested by the operator.
func (db *DB) GetStoredPromptRequest(ctx context.Context, id int64) (PromptRequestBody, error) {
	return db.getPromptRequest(ctx, id, "prompt_request_records")
}

func (db *DB) getPromptRequest(ctx context.Context, id int64, table string) (PromptRequestBody, error) {
	var item PromptRequestBody
	err := db.conn.QueryRowContext(ctx, `SELECT `+promptRequestSummaryColumns+`, content_type, body, conversation_scope, conversation_group
		FROM `+table+` WHERE id = $1`, id).
		Scan(append(item.scanTargets(), &item.ContentType, &item.Body, &item.ConversationScope, &item.ConversationGroup)...)
	return item, err
}

// PromptRequestConversationScope indexes an explicit conversation marker under
// the authenticated key and route. It never persists the marker as metadata.
func PromptRequestConversationScope(body []byte, apiKeyID int64, endpoint, transport string) string {
	marker := promptRequestConversationMarker(body)
	if marker == "" {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s\x00%s", apiKeyID, endpoint, transport, marker))))
}

func promptRequestConversationMarker(body []byte) string {
	return promptRequestMarker(body, "client_metadata.thread_id client_metadata.session_id metadata.conversation_id metadata.session_id conversation_id prompt_cache_key")
}

func promptRequestMarker(body []byte, paths string) string {
	for _, path := range strings.Fields(paths) {
		field := gjson.GetBytes(body, path)
		value := strings.TrimSpace(field.Str)
		if field.Type == gjson.String && value != "" && len(value) <= 512 {
			return path + "\x00" + value
		}
	}
	return ""
}

func (db *DB) PreviousPromptRequest(ctx context.Context, current PromptRequestBody) (int64, []byte, error) {
	var id int64
	var body []byte
	if current.ConversationGroup != "" && current.ConversationGroup != singlePromptRequestGroup {
		err := db.conn.QueryRowContext(ctx, `SELECT id, body FROM prompt_request_visible_records
			WHERE conversation_group = $1 AND (created_at, id) < ($2, $3)
			ORDER BY created_at DESC, id DESC LIMIT 1`, current.ConversationGroup, current.CreatedAt, current.ID).Scan(&id, &body)
		if !errors.Is(err, sql.ErrNoRows) {
			return id, body, err
		}
	}
	if current.ConversationScope != "" {
		err := db.conn.QueryRowContext(ctx, `SELECT id, body FROM prompt_request_visible_records
			WHERE conversation_scope = $1 AND conversation_scope <> '' AND id < $2 ORDER BY id DESC LIMIT 1`,
			current.ConversationScope, current.ID).Scan(&id, &body)
		if !errors.Is(err, sql.ErrNoRows) {
			return id, body, err
		}
	}

	// Records predating the conversation index are matched lazily. Bound the
	// lookup instead of rewriting large archived request bodies at startup.
	marker := promptRequestConversationMarker(current.Body)
	if marker == "" {
		return 0, nil, nil
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT id, body FROM prompt_request_visible_records
		WHERE id < $1 AND endpoint = $2 AND transport = $3 AND api_key_name = $4 AND conversation_scope = ''
		ORDER BY id DESC LIMIT 12`, current.ID, current.Endpoint, current.Transport, current.APIKey)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		if err := rows.Scan(&id, &body); err != nil {
			return 0, nil, err
		}
		if promptRequestConversationMarker(body) == marker {
			return id, body, nil
		}
	}
	return 0, nil, rows.Err()
}

func (db *DB) PurgeExpiredPromptRequests(ctx context.Context, now time.Time) error {
	days, err := db.GetPromptRequestRetentionDays(ctx)
	if err != nil {
		return err
	}
	cutoff := now.UTC().AddDate(0, 0, -days)
	const batchSize = 100
	for {
		var deleted int64
		err := db.withSQLiteWriteLock(ctx, func() error {
			// Stop this sweep if an administrator changes retention while it is running.
			result, err := db.conn.ExecContext(ctx, `DELETE FROM prompt_request_records
				WHERE id IN
				(SELECT id FROM prompt_request_records WHERE created_at < $1 ORDER BY created_at, id LIMIT $2)
				AND $3 = COALESCE(`+promptRequestStoredRetention+`, $4)`,
				cutoff, batchSize, days, DefaultPromptRequestRetentionDays)
			if err != nil {
				return err
			}
			deleted, err = result.RowsAffected()
			return err
		})
		if err != nil {
			return err
		}
		if deleted < batchSize {
			return db.purgePromptTaskClientSnapshots(ctx, cutoff, days)
		}
	}
}
