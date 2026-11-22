package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type PromptRequestResponse struct {
	Status    string                      `json:"status"`
	Text      string                      `json:"text"`
	ToolCalls []PromptRequestResponseCall `json:"tool_calls"`
	Error     string                      `json:"error,omitempty"`
	Truncated bool                        `json:"truncated,omitempty"`
	// A provider-labelled final message is separate from the whole model
	// response, which can include progress updates and tool-related output.
	FinalText      string `json:"final_text,omitempty"`
	CommentaryText string `json:"commentary_text,omitempty"`
	FinalObserved  bool   `json:"final_observed,omitempty"`
	// Distinguishes a classified Chat Completions reply from archived text.
	ChatReplyClassified bool       `json:"chat_reply_classified,omitempty"`
	FinishedAt          *time.Time `json:"finished_at,omitempty"`
}

type PromptRequestResponseCall struct {
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func (db *DB) ensurePromptRequestResponseTable(ctx context.Context) error {
	if _, err := db.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS prompt_request_responses (
		request_id BIGINT PRIMARY KEY REFERENCES prompt_request_records(id) ON DELETE CASCADE,
		response TEXT NOT NULL
	)`); err != nil {
		return err
	}
	if db.isSQLite() {
		// SQLite does not enable foreign key enforcement for every connection.
		_, err := db.conn.ExecContext(ctx, `CREATE TRIGGER IF NOT EXISTS delete_prompt_request_response
			AFTER DELETE ON prompt_request_records BEGIN
				DELETE FROM prompt_request_responses WHERE request_id = OLD.id;
			END`)
		return err
	}
	return nil
}

const upsertPromptRequestResponse = `INSERT INTO prompt_request_responses (request_id, response)
	SELECT id, $2 FROM prompt_request_records WHERE id = $1
	ON CONFLICT (request_id) DO UPDATE SET response = excluded.response`

func marshalPromptRequestResponse(response PromptRequestResponse) (string, error) {
	if response.ToolCalls == nil {
		response.ToolCalls = []PromptRequestResponseCall{}
	}
	encoded, err := json.Marshal(response)
	return string(encoded), err
}

func (db *DB) SavePromptRequestResponse(ctx context.Context, requestID int64, response PromptRequestResponse) error {
	encoded, err := marshalPromptRequestResponse(response)
	if err != nil {
		return err
	}
	return db.withSQLiteWriteLock(ctx, func() error {
		_, err := db.conn.ExecContext(ctx, upsertPromptRequestResponse, requestID, encoded)
		return err
	})
}

func (db *DB) GetPromptRequestResponse(ctx context.Context, requestID int64) (*PromptRequestResponse, error) {
	var encoded string
	err := db.conn.QueryRowContext(ctx,
		`SELECT response FROM prompt_request_responses WHERE request_id = $1`, requestID).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var response PromptRequestResponse
	if err := json.Unmarshal([]byte(encoded), &response); err != nil {
		return nil, err
	}
	return &response, nil
}
