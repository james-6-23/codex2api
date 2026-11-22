package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const promptRequestTaskMaxBody = 64 << 20

type PromptRequestTaskInput struct {
	Text          string
	Field         string
	Index         int
	TurnID        string
	TurnStartedAt *time.Time
	HasAttachment bool

	anchor string
}

type promptRequestTaskIndex struct {
	Anchor  string
	Turn    string
	Preview string
}

func promptRequestTaskMetadata(body []byte) promptRequestTaskIndex {
	input := DecodePromptRequestTaskInput(body)
	var turnHash string
	if input.TurnID != "" {
		id := sha256.Sum256([]byte(input.TurnID))
		turnHash = hex.EncodeToString(id[:])
	}
	preview := strings.TrimSpace(input.Text)
	n := 0
	for offset := range preview {
		if n == 180 {
			preview = preview[:offset] + "…"
			break
		}
		n++
	}
	return promptRequestTaskIndex{Anchor: input.anchor, Turn: turnHash, Preview: preview}
}

// DecodePromptRequestTaskInput recognizes user-role message occurrences and
// records the most recent one. Its fingerprint includes preceding history, so
// an identical message appended again is a distinct send.
func DecodePromptRequestTaskInput(body []byte) PromptRequestTaskInput {
	return decodePromptRequestTaskInput(body, true)
}

// DecodePromptRequestTaskUser reads the same user occurrence without hashing
// the full history for an index that has already been built.
func DecodePromptRequestTaskUser(body []byte) PromptRequestTaskInput {
	return decodePromptRequestTaskInput(body, false)
}

func decodePromptRequestTaskInput(body []byte, hashAnchor bool) PromptRequestTaskInput {
	input := PromptRequestTaskInput{Index: -1}
	if len(body) == 0 || len(body) > promptRequestTaskMaxBody || !gjson.ValidBytes(body) {
		return input
	}
	turn := gjson.GetBytes(body, "client_metadata.turn_id").Str
	if turn != "" && len(turn) <= 128 {
		input.TurnID = turn
		input.TurnStartedAt = promptRequestTaskClientStart(body, turn)
	}
	for _, field := range []string{"input", "messages", "contents", "prompt"} {
		value := gjson.GetBytes(body, field)
		switch {
		case value.IsArray():
			if field == "prompt" {
				continue
			}
			items := value.Array()
			for i := len(items) - 1; i >= 0; i-- {
				item := items[i]
				if item.Get("role").Str != "user" {
					continue
				}
				if kind := item.Get("type").Str; kind != "" && kind != "message" {
					continue
				}
				text, attachment := taskUserContent(item, 0)
				if text == "" && !attachment {
					continue
				}
				input.Text, input.Field, input.Index, input.HasAttachment = text, field, i, attachment
				if !hashAnchor {
					return input
				}
				if input.TurnID == "" {
					input.anchor = promptRequestTaskPrefix(field, items[:i+1])
				} else {
					// Scope an explicit turn to the latest user's own occurrence.
					// Metadata or older history can be rewritten between calls.
					var content gjson.Result
					for _, key := range []string{"content", "parts", "text"} {
						if content = item.Get(key); content.Exists() {
							break
						}
					}
					input.anchor = promptRequestTaskPrefix(fmt.Sprintf("%s[%d]:%s", field, i, item.Get("id").Str), []gjson.Result{content})
				}
				return input
			}
			return input
		case value.Type == gjson.String && (field == "input" || field == "prompt"):
			if text := value.Str; strings.TrimSpace(text) != "" && !isInjectedTaskText(text) {
				input.Text, input.Field = text, field
				// A bare prompt has no distinct occurrence without a turn ID.
				if hashAnchor && input.TurnID != "" {
					input.anchor = promptRequestTaskPrefix(field, []gjson.Result{value})
				}
				return input
			}
		}
	}
	return input
}

func promptRequestTaskClientStart(body []byte, turn string) *time.Time {
	metadata := gjson.GetBytes(body, "client_metadata").Get("x-codex-turn-metadata")
	if metadata.Type == gjson.String {
		metadata = gjson.Parse(metadata.Str)
	}
	if !metadata.IsObject() || metadata.Get("turn_id").Str != turn {
		return nil
	}
	thread := gjson.GetBytes(body, "client_metadata.thread_id").Str
	if thread == "" || (metadata.Get("thread_id").Str != thread && metadata.Get("session_id").Str != thread) {
		return nil
	}
	millis := metadata.Get("turn_started_at_unix_ms").Int()
	if millis <= 0 {
		return nil
	}
	at := time.UnixMilli(millis).UTC()
	return &at
}

func promptRequestTaskPrefix(field string, items []gjson.Result) string {
	h := sha256.New()
	_, _ = fmt.Fprintln(h, field, len(items))
	encoder := json.NewEncoder(h)
	for _, item := range items {
		var value any
		decoder := json.NewDecoder(strings.NewReader(item.Raw))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return ""
		}
		if err := encoder.Encode(value); err != nil {
			return ""
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func taskUserContent(value gjson.Result, depth int) (string, bool) {
	if depth >= 16 {
		return "", false
	}
	if value.Type == gjson.String {
		if strings.TrimSpace(value.Str) != "" && !isInjectedTaskText(value.Str) {
			return value.Str, false
		}
		return "", false
	}
	if value.IsArray() {
		var parts []string
		attachment := false
		value.ForEach(func(_, item gjson.Result) bool {
			text, media := taskUserContent(item, depth+1)
			if text != "" {
				parts = append(parts, text)
			}
			attachment = attachment || media
			return true
		})
		return strings.Join(parts, "\n"), attachment
	}
	if !value.IsObject() {
		return "", false
	}
	switch value.Get("type").Str {
	case "image", "image_url", "input_image", "input_audio", "file", "input_file":
		return "", true
	case "", "message", "text", "input_text":
		if value.Get("functionResponse").Exists() || value.Get("functionCall").Exists() {
			return "", false
		}
		if value.Get("inlineData").Exists() || value.Get("fileData").Exists() {
			return "", true
		}
		for _, key := range []string{"content", "parts", "text"} {
			if item := value.Get(key); item.Exists() {
				return taskUserContent(item, depth+1)
			}
		}
	}
	return "", false
}

func isInjectedTaskText(text string) bool {
	text = strings.TrimSpace(text)
	for _, name := range []string{"environment_context", "turn_aborted", "subagent_notification", "user_instructions", "skills_instructions", "skill"} {
		prefix, suffix := "<"+name+">", "</"+name+">"
		if strings.HasPrefix(text, prefix) && strings.HasSuffix(text, suffix) && strings.Count(text, suffix) == 1 {
			return true
		}
	}
	return strings.HasPrefix(text, "# AGENTS.md instructions for ") && strings.HasSuffix(text, "</INSTRUCTIONS>")
}

func (db *DB) IndexPromptRequestTaskAnchors(ctx context.Context, limit int) error {
	return db.indexPromptTaskRows(ctx, "", 0, limit)
}

func (db *DB) IndexPromptRequestConversationTaskAnchors(ctx context.Context, id int64, limit int) error {
	var group string
	if err := db.conn.QueryRowContext(ctx, "SELECT conversation_group FROM prompt_request_records WHERE id=$1", id).Scan(&group); err != nil {
		return err
	}
	if group == "" || group == singlePromptRequestGroup {
		return db.indexPromptTaskRows(ctx, "", id, 1)
	}
	return db.indexPromptTaskRows(ctx, group, 0, limit)
}

func (db *DB) indexPromptTaskRows(ctx context.Context, group string, id int64, limit int) error {
	limit = min(max(limit, 1), 256)
	query := "SELECT id, CASE WHEN length(body) <= $1 THEN body END FROM prompt_request_records WHERE task_anchor IS NULL"
	args := []any{promptRequestTaskMaxBody, limit}
	switch {
	case group != "":
		query += " AND conversation_group = $3"
		args = append(args, group)
	case id != 0:
		query += " AND id = $3"
		args = append(args, id)
	}
	rows, err := db.conn.QueryContext(ctx, query+" ORDER BY id DESC LIMIT $2", args...)
	if err != nil {
		return err
	}
	type entry struct {
		id       int64
		metadata promptRequestTaskIndex
	}
	batch := make([]entry, 0, limit)
	for rows.Next() {
		var item entry
		var body []byte
		if err := rows.Scan(&item.id, &body); err != nil {
			rows.Close()
			return err
		}
		item.metadata = promptRequestTaskMetadata(body)
		batch = append(batch, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(batch) == 0 {
		return err
	}
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		for _, item := range batch {
			m := item.metadata
			if _, err := tx.ExecContext(ctx, "UPDATE prompt_request_records SET task_anchor=$1, task_turn=$2, task_preview=$3 WHERE id=$4 AND task_anchor IS NULL", m.Anchor, m.Turn, m.Preview, item.id); err != nil {
				return err
			}
		}
		return nil
	})
}

type PromptRequestTaskRow struct {
	PromptRequestRecord
	Anchor  string
	Turn    string
	Preview string
	Indexed bool
}

type PromptRequestTaskRows struct {
	Group string
	Total int
	Rows  []PromptRequestTaskRow
}

// ReadPromptRequestTaskRows returns lightweight replay markers. Full bodies
// and results are fetched only for the selected task.
func (db *DB) ReadPromptRequestTaskRows(ctx context.Context, id, before int64, newestFirst bool, limit int) (PromptRequestTaskRows, error) {
	var result PromptRequestTaskRows
	var at time.Time
	err := db.conn.QueryRowContext(ctx, "SELECT conversation_group, created_at FROM prompt_request_records WHERE id=$1", id).Scan(&result.Group, &at)
	if err != nil {
		return result, err
	}
	const columns = promptRequestSummaryColumns + ", task_anchor, task_turn, task_preview"
	query := "SELECT " + columns + " FROM prompt_request_records"
	args := []any{id}
	switch {
	case result.Group == "" || result.Group == singlePromptRequestGroup:
		query += " WHERE id=$1"
		result.Total = 1
	case newestFirst:
		err = db.conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM prompt_request_records WHERE conversation_group=$1 AND id<=$2", result.Group, id).Scan(&result.Total)
		if err != nil {
			return result, err
		}
		query += " WHERE id<=$1 AND conversation_group=$2"
		args = append(args, result.Group, limit)
		if before > 0 {
			query += " AND (created_at,id) < (SELECT created_at,id FROM prompt_request_records WHERE id=$4 AND conversation_group=$2)"
			args = append(args, before)
		}
		query += " ORDER BY created_at DESC,id DESC LIMIT $3"
	default:
		query += " WHERE (created_at,id)>=($2,$1) AND conversation_group=$3 ORDER BY created_at,id LIMIT $4"
		args = append(args, at, result.Group, limit)
	}
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var row PromptRequestTaskRow
		var anchor sql.NullString
		if err := rows.Scan(append(row.scanTargets(), &anchor, &row.Turn, &row.Preview)...); err != nil {
			return result, err
		}
		row.Anchor, row.Indexed = anchor.String, anchor.Valid
		result.Rows = append(result.Rows, row)
	}
	return result, rows.Err()
}

// A client Stop event identifies a Codex turn, which may contain multiple
// user sends (for example a steer). Distinct observed sends under that turn
// cannot each claim the same client-completed reply or client duration.
func (db *DB) PromptRequestTaskTurnIsUnambiguous(ctx context.Context, group, turn string) (bool, error) {
	if group == "" || group == singlePromptRequestGroup || turn == "" {
		return false, nil
	}
	var count int
	err := db.conn.QueryRowContext(ctx, `SELECT COUNT(DISTINCT task_anchor) FROM prompt_request_records
		WHERE conversation_group=$1 AND task_turn=$2 AND task_anchor<>''`, group, turn).Scan(&count)
	return count == 1, err
}
