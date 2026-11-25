package database

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// ReadPromptRequestTaskBodies reads overview anchor and boundary bodies in
// priority order, up to a shared budget. Historical tool requests are not
// loaded to paint the overview.
func (db *DB) ReadPromptRequestTaskBodies(ctx context.Context, ids []int64, budget int) (map[int64]PromptRequestBody, error) {
	result := make(map[int64]PromptRequestBody, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	placeholders := dbPlaceholders(db.isSQLite(), 1, len(ids)+2)
	values := make([]string, len(ids))
	for i := range ids {
		values[i] = fmt.Sprintf("(CAST(%s AS BIGINT), %d)", placeholders[i], i)
	}
	args := append(argsFromInt64s(ids), promptRequestTaskMaxBody, budget)
	rows, err := db.conn.QueryContext(ctx, `WITH requested(id, priority) AS (VALUES `+strings.Join(values, ",")+`),
		bounded AS MATERIALIZED (
			SELECT r.id, SUM(LENGTH(r.body)) OVER (ORDER BY requested.priority) AS bytes_read
			FROM requested JOIN prompt_request_visible_records r ON r.id = requested.id
			WHERE LENGTH(r.body) <= `+placeholders[len(ids)]+`
		)
		SELECT r.id, r.content_type, r.body FROM bounded b JOIN prompt_request_visible_records r ON r.id = b.id
		WHERE b.bytes_read <= `+placeholders[len(ids)+1], args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var body PromptRequestBody
		if err := rows.Scan(&body.ID, &body.ContentType, &body.Body); err != nil {
			return nil, err
		}
		result[body.ID] = body
	}
	return result, rows.Err()
}

type PromptRequestTaskResponse struct {
	Status              string
	FinalText           string
	FinalObserved       bool
	HasToolCalls        bool
	FinishedAt          *time.Time
	Truncated           bool
	LegacyChatFinalText string
	UnconfirmedChatText string
}

// ReadPromptRequestTaskResponses fetches terminal responses and responses
// carrying the final phase, without building tool payloads for overview cards.
func (db *DB) ReadPromptRequestTaskResponses(ctx context.Context, ids, terminalIDs []int64) (map[int64]PromptRequestTaskResponse, error) {
	result := make(map[int64]PromptRequestTaskResponse, len(terminalIDs))
	if len(ids) == 0 || len(terminalIDs) == 0 {
		return result, nil
	}
	placeholders := dbPlaceholders(db.isSQLite(), 1, len(ids)+len(terminalIDs))
	args := append(argsFromInt64s(ids), argsFromInt64s(terminalIDs)...)
	// The LIKE condition narrows the transfer only. The decoded field is
	// checked before any text counts as a final answer.
	rows, err := db.conn.QueryContext(ctx, `SELECT request_id, response FROM prompt_request_responses
		WHERE request_id IN (`+strings.Join(placeholders[:len(ids)], ",")+`)
		AND (request_id IN (`+strings.Join(placeholders[len(ids):], ",")+`) OR response LIKE '%"final_observed"%true%')`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var encoded string
		if err := rows.Scan(&id, &encoded); err != nil {
			return nil, err
		}
		fields := gjson.GetMany(encoded, "status", "final_text", "final_observed", "finished_at", "truncated",
			"chat_reply_classified", "error", "commentary_text", "tool_calls.#", "text", "chat_finish_reason")
		entry := PromptRequestTaskResponse{
			Status: fields[0].Str, FinalText: fields[1].Str, FinalObserved: fields[2].Type == gjson.True,
			Truncated: fields[4].Type == gjson.True, HasToolCalls: fields[8].Int() > 0,
		}
		if finished, err := time.Parse(time.RFC3339Nano, fields[3].Str); err == nil {
			entry.FinishedAt = &finished
		}
		if entry.Status == "completed" && !entry.FinalObserved && !entry.Truncated &&
			fields[6].Str == "" && fields[7].Str == "" && !entry.HasToolCalls && strings.TrimSpace(fields[9].Str) != "" {
			if fields[5].Type != gjson.True {
				entry.LegacyChatFinalText = fields[9].Str
			} else if fields[10].Str == "" {
				entry.UnconfirmedChatText = fields[9].Str
			}
		}
		result[id] = entry
	}
	return result, rows.Err()
}
