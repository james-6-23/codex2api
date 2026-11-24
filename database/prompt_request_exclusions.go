package database

import (
	"context"
	"database/sql"
	"errors"
)

var ErrInvalidPromptRequestKey = errors.New("Unknown API Key in request recording exclusions")

type PromptRequestKeyRecording struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Excluded bool   `json:"excluded"`
}

const promptRequestKeyRecordingQuery = `SELECT id, COALESCE(name,''), prompt_request_excluded FROM api_keys ORDER BY id`

func (db *DB) ListPromptRequestKeyRecording(ctx context.Context) ([]PromptRequestKeyRecording, error) {
	return listPromptRequestKeyRecording(ctx, db.conn)
}

func listPromptRequestKeyRecording(ctx context.Context, reader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) ([]PromptRequestKeyRecording, error) {
	rows, err := reader.QueryContext(ctx, promptRequestKeyRecordingQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := make([]PromptRequestKeyRecording, 0)
	for rows.Next() {
		var key PromptRequestKeyRecording
		if err := rows.Scan(&key.ID, &key.Name, &key.Excluded); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (db *DB) ensurePromptRequestExclusions(ctx context.Context) error {
	if db.isSQLite() {
		if err := db.ensureSQLiteColumn(ctx, "api_keys", "prompt_request_excluded", "BOOLEAN NOT NULL DEFAULT FALSE"); err != nil {
			return err
		}
		if err := db.ensureSQLiteColumn(ctx, "prompt_request_records", "recording_hidden", "BOOLEAN NOT NULL DEFAULT FALSE"); err != nil {
			return err
		}
	} else {
		if _, err := db.conn.ExecContext(ctx, `ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS prompt_request_excluded BOOLEAN NOT NULL DEFAULT FALSE`); err != nil {
			return err
		}
		if _, err := db.conn.ExecContext(ctx, `ALTER TABLE prompt_request_records ADD COLUMN IF NOT EXISTS recording_hidden BOOLEAN NOT NULL DEFAULT FALSE`); err != nil {
			return err
		}
	}
	if _, err := db.conn.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_prompt_request_recording_key
		ON prompt_request_records (chat_key_id) WHERE recording_hidden=FALSE`); err != nil {
		return err
	}
	// Keep the replay query source separate from the retained audit data, which
	// is still used by the retention job and existing trace export jobs.
	createView := "CREATE OR REPLACE VIEW"
	if db.isSQLite() {
		createView = "CREATE VIEW IF NOT EXISTS"
	}
	_, err := db.conn.ExecContext(ctx, createView+` prompt_request_visible_records AS
		SELECT * FROM prompt_request_records WHERE recording_hidden=FALSE`)
	return err
}

func (db *DB) SetPromptRequestExcludedKeys(ctx context.Context, ids []int64) ([]PromptRequestKeyRecording, error) {
	requested := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, ErrInvalidPromptRequestKey
		}
		requested[id] = true
	}

	var keys []PromptRequestKeyRecording
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if !db.isSQLite() {
			if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('prompt-request-exclusions', 0))`); err != nil {
				return err
			}
		}
		var err error
		keys, err = listPromptRequestKeyRecording(ctx, tx)
		if err != nil {
			return err
		}
		known := make(map[int64]bool, len(keys))
		for _, key := range keys {
			known[key.ID] = true
		}
		for id := range requested {
			if !known[id] {
				return ErrInvalidPromptRequestKey
			}
		}

		for i := range keys {
			key := &keys[i]
			exclude := requested[key.ID]
			if key.Excluded == exclude {
				continue
			}
			if !db.isSQLite() {
				if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('prompt-request-recording:' || $1::bigint::text, 0))`, key.ID); err != nil {
					return err
				}
				// Keep the visibility change atomic with Chat replay linking.
				if exclude {
					if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('prompt-chat-replay:' || $1::bigint::text, 0))`, key.ID); err != nil {
						return err
					}
				}
			}
			result, err := tx.ExecContext(ctx, `UPDATE api_keys SET prompt_request_excluded=$1 WHERE id=$2`, exclude, key.ID)
			if err != nil {
				return err
			}
			if affected, err := result.RowsAffected(); err != nil {
				return err
			} else if affected != 1 {
				return ErrInvalidPromptRequestKey
			}
			if exclude {
				if err := hidePromptRequestsForKey(ctx, tx, key.ID); err != nil {
					return err
				}
			}
			key.Excluded = exclude
		}
		return nil
	})
	return keys, err
}

func hidePromptRequestsForKey(ctx context.Context, tx *sql.Tx, keyID int64) error {
	// Older recordings can lack a key ID. A group with a verified key, or a
	// currently unambiguous name since that key was created, identifies them.
	_, err := tx.ExecContext(ctx, `UPDATE prompt_request_records AS r SET recording_hidden=TRUE
		WHERE r.recording_hidden=FALSE AND (r.chat_key_id=$1 OR ((r.chat_key_id IS NULL OR r.chat_key_id=0) AND (
			(r.conversation_group NOT IN ('','single') AND EXISTS (
				SELECT 1 FROM prompt_request_records related
				WHERE related.conversation_group=r.conversation_group AND related.chat_key_id=$1
			)) OR EXISTS (
				SELECT 1 FROM api_keys k WHERE k.id=$1 AND k.name<>'' AND k.name=r.api_key_name AND k.created_at<=r.created_at
				AND NOT EXISTS (SELECT 1 FROM api_keys other WHERE other.name=k.name AND other.id<>k.id)
			)
		)))`, keyID)
	return err
}
