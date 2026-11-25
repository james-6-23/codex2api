package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const promptRequestChatIndexVersion = 2

const promptRequestChatRoutes = "transport='http' AND endpoint IN ('/v1/chat/completions','/chat/completions')"

func IsPromptRequestChat(endpoint, transport string) bool {
	return transport == "http" && (endpoint == "/v1/chat/completions" || endpoint == "/chat/completions")
}

func (db *DB) ensurePromptRequestChatIndexes(ctx context.Context) error {
	for _, statement := range []string{
		`CREATE INDEX IF NOT EXISTS idx_prompt_request_chat_replay_pending ON prompt_request_records (id)
			WHERE chat_indexed<2 AND ` + promptRequestChatRoutes,
		`CREATE INDEX IF NOT EXISTS idx_prompt_request_chat_anchor ON prompt_request_records (chat_key_id, endpoint, task_anchor, id)
			WHERE chat_key_id>0 AND ` + promptRequestChatRoutes,
		`CREATE INDEX IF NOT EXISTS idx_prompt_request_chat_history ON prompt_request_records (chat_key_id, endpoint, id)
			WHERE chat_key_id>0 AND ` + promptRequestChatRoutes,
	} {
		if _, err := db.conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

// IndexArchivedPromptRequestChats processes retained rows in request order,
// including continuations that arrived before the preceding response was saved.
func (db *DB) IndexArchivedPromptRequestChats(ctx context.Context) error {
	for after := int64(0); ; {
		rows, err := db.conn.QueryContext(ctx, `SELECT id FROM prompt_request_visible_records WHERE id>$1
			AND chat_indexed<2 AND task_anchor IS NOT NULL AND `+promptRequestChatRoutes+` ORDER BY id LIMIT 64`, after)
		if err != nil {
			return err
		}
		ids := make([]int64, 0, 64)
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil || len(ids) == 0 {
			return err
		}
		for _, id := range ids {
			if err := db.IndexPromptRequestChat(ctx, id, nil); err != nil {
				return err
			}
		}
		after = ids[len(ids)-1]
	}
}

// IndexPromptRequestChat indexes tool continuations and verified replay links.
func (db *DB) IndexPromptRequestChat(ctx context.Context, id int64, requestBody []byte) error {
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		var key sql.NullInt64
		var keyName, endpoint, group, anchor, kind string
		var indexed int
		var turnRoot int64
		var createdAt time.Time
		var body []byte
		lock := ""
		if !db.isSQLite() {
			// A replay link can move an entire tool chain. Serialize the
			// replay index for this key before locking any request rows.
			if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('prompt-chat-replay:' ||
				CAST(COALESCE(r.chat_key_id, (SELECT MIN(k.id) FROM api_keys k WHERE k.name=r.api_key_name
				HAVING COUNT(*)=1 AND MAX(k.created_at)<=r.created_at), 0) AS TEXT), 0))
				FROM prompt_request_visible_records r WHERE r.id=$1 AND r.chat_indexed<2`, id); err != nil {
				return err
			}
			lock = " FOR UPDATE"
		}
		query := `SELECT chat_key_id, api_key_name, endpoint, conversation_group, task_anchor, created_at, chat_indexed, chat_turn_root, chat_replay_kind,
			CASE WHEN $3 AND LENGTH(body)<=$2 AND
				(chat_indexed=0 OR conversation_group='single' OR NOT EXISTS (
					SELECT 1 FROM prompt_request_visible_records previous WHERE previous.conversation_group=r.conversation_group
					AND previous.task_anchor=r.task_anchor AND previous.id<r.id
				)) THEN body END FROM prompt_request_visible_records r WHERE id=$1
			AND chat_indexed<2 AND task_anchor IS NOT NULL AND ` + promptRequestChatRoutes + lock
		err := tx.QueryRowContext(ctx, query, id, promptRequestTaskMaxBody, requestBody == nil).
			Scan(&key, &keyName, &endpoint, &group, &anchor, &createdAt, &indexed, &turnRoot, &kind, &body)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if requestBody != nil {
			body = requestBody
		}
		if !key.Valid {
			// Old rows stored only the key name. Require a unique key that
			// existed when the row was recorded.
			err := tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(id),0) FROM api_keys WHERE name=$1 AND name<>''
				HAVING COUNT(*)=1 AND MAX(created_at)<=$2`, keyName, createdAt).Scan(&key.Int64)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if key.Int64 > 0 && anchor != "" && kind == "" {
			messages := gjson.GetBytes(body, "messages").Array()
			if len(messages) > 0 {
				switch messages[len(messages)-1].Get("role").Str {
				case "tool":
					if indexed == 0 && group == singlePromptRequestGroup {
						linked, retry, err := db.linkPromptChatContinuation(ctx, tx, id, key.Int64, keyName, endpoint, anchor, messages, lock)
						if err != nil || retry {
							return err
						}
						if linked != "" {
							group = linked
						}
					}
					if indexed == 0 && group != singlePromptRequestGroup && turnRoot == 0 {
						if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(chat_turn_root),0) FROM prompt_request_visible_records
							WHERE conversation_group=$1 AND task_anchor=$2 AND id<$3`,
							group, anchor, id).Scan(&turnRoot); err != nil {
							return err
						}
					}
				case "user":
					current := promptChatReplayRow{id: id, group: group, anchor: anchor, turnRoot: turnRoot}
					linked, err := db.indexPromptChatReplayBoundary(ctx, tx, key.Int64, endpoint, current, messages)
					if err != nil {
						return err
					}
					group, kind, turnRoot = linked.group, linked.kind, linked.turnRoot
				}
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE prompt_request_records
			SET chat_key_id=$1, chat_indexed=$2, conversation_group=$3, chat_turn_root=$4, chat_replay_kind=$5 WHERE id=$6`,
			key.Int64, promptRequestChatIndexVersion, group, turnRoot, kind, id)
		return err
	})
}

func (db *DB) linkPromptChatContinuation(ctx context.Context, tx *sql.Tx, id, keyID int64, keyName, endpoint, anchor string,
	messages []gjson.Result, lock string,
) (string, bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM prompt_request_visible_records WHERE endpoint=$2 AND task_anchor=$3 AND id<$4 AND `+promptRequestChatRoutes+`
		AND (conversation_group='single' OR conversation_group LIKE 'chat:%')
		AND ((chat_key_id=$1 AND chat_key_id>0) OR (chat_indexed=0 AND chat_key_id IS NULL AND api_key_name=$5))
		ORDER BY id DESC LIMIT 12`, keyID, endpoint, anchor, id, keyName)
	if err != nil {
		return "", false, err
	}
	var candidates []int64
	for rows.Next() {
		var candidate int64
		if err := rows.Scan(&candidate); err != nil {
			rows.Close()
			return "", false, err
		}
		candidates = append(candidates, candidate)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	for _, candidate := range candidates {
		var response, group string
		var indexed, status int
		var candidateKey sql.NullInt64
		var recent bool
		err := tx.QueryRowContext(ctx, `SELECT conversation_group, chat_indexed, status, chat_key_id, created_at>$2
			FROM prompt_request_visible_records WHERE id=$1`+lock,
			candidate, time.Now().UTC().Add(-30*time.Minute)).Scan(&group, &indexed, &status, &candidateKey, &recent)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return "", false, err
		}
		if (candidateKey.Valid && candidateKey.Int64 != keyID) ||
			(group != singlePromptRequestGroup && !strings.HasPrefix(group, "chat:")) ||
			(status == 0 && !recent) || (status != 0 && (status < 200 || status >= 300)) {
			continue
		}
		err = tx.QueryRowContext(ctx, `SELECT response FROM prompt_request_responses WHERE request_id=$1`, candidate).Scan(&response)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", false, err
		}
		if status != 0 && gjson.Get(response, "tool_calls.#").Int() == 0 {
			continue
		}
		var body []byte
		if err := tx.QueryRowContext(ctx, `SELECT CASE WHEN LENGTH(body)<=$2 THEN body END FROM prompt_request_visible_records WHERE id=$1`,
			candidate, promptRequestTaskMaxBody).Scan(&body); err != nil {
			return "", false, err
		}
		calls := promptChatContinuationCalls(gjson.GetBytes(body, "messages").Array(), messages)
		if len(calls) == 0 || (status != 0 && !capturedPromptChatCalls(response, calls)) {
			continue
		}
		// The parent must be indexed first so descendants inherit its group.
		if indexed == 0 || status == 0 {
			return "", true, nil
		}
		if group == singlePromptRequestGroup {
			group = fmt.Sprintf("chat:%d", candidate)
			if _, err := tx.ExecContext(ctx, `UPDATE prompt_request_records SET conversation_group=$1 WHERE id=$2`,
				group, candidate); err != nil {
				return "", false, err
			}
		}
		return group, false, nil
	}
	return "", false, nil
}

// A continuation extends the prior message history with completed tool
// exchanges. The first must match the prior response. The client can append
// its own tool exchanges after it before the next gateway call.
func promptChatContinuationCalls(previous, next []gjson.Result) []string {
	if len(previous) == 0 || len(previous) >= len(next) || !samePromptChatHistory(previous, next[:len(previous)]) {
		return nil
	}
	extension := next[len(previous):]
	count, calls := promptChatToolExchange(extension)
	if count == 0 {
		return nil
	}
	for rest := extension[count:]; len(rest) > 0; {
		count, _ := promptChatToolExchange(rest)
		if count == 0 {
			return nil
		}
		rest = rest[count:]
	}
	return calls
}

func promptChatToolExchange(items []gjson.Result) (int, []string) {
	if len(items) < 2 || items[0].Get("role").Str != "assistant" {
		return 0, nil
	}
	calls := items[0].Get("tool_calls").Array()
	count := len(calls)
	if count == 0 || count >= len(items) {
		return 0, nil
	}
	ids := make([]string, 0, count)
	pending := make(map[string]bool, count)
	for _, call := range calls {
		id := call.Get("id").Str
		if id == "" || pending[id] {
			return 0, nil
		}
		ids = append(ids, id)
		pending[id] = true
	}
	for _, result := range items[1 : count+1] {
		id := result.Get("tool_call_id").Str
		if result.Get("role").Str != "tool" || !pending[id] {
			return 0, nil
		}
		delete(pending, id)
	}
	return count + 1, ids
}

func capturedPromptChatCalls(response string, ids []string) bool {
	if gjson.Get(response, "status").Str != "completed" || gjson.Get(response, "final_observed").Bool() ||
		gjson.Get(response, "error").Str != "" {
		return false
	}
	calls := gjson.Get(response, "tool_calls").Array()
	if len(calls) != len(ids) {
		return false
	}
	for _, id := range ids {
		if !slices.ContainsFunc(calls, func(call gjson.Result) bool { return call.Get("call_id").Str == id }) {
			return false
		}
	}
	return true
}

func samePromptChatHistory(left, right []gjson.Result) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].Raw == right[i].Raw {
			continue
		}
		hash := promptRequestTaskPrefix("messages", left[i:i+1])
		if hash == "" || hash != promptRequestTaskPrefix("messages", right[i:i+1]) {
			return false
		}
	}
	return true
}

// PromptRequestChatReplyEchoed checks a later request under the same key for
// the captured text in assistant history immediately before a new user send.
func (db *DB) PromptRequestChatReplyEchoed(ctx context.Context, id int64, body []byte, reply string) (bool, error) {
	const lookahead = 12
	var laterIDs []int64
	rows, err := db.conn.QueryContext(ctx, `SELECT n.id FROM prompt_request_visible_records p JOIN prompt_request_visible_records n
		ON n.chat_key_id=p.chat_key_id AND n.endpoint=p.endpoint AND n.id>p.id
		WHERE p.id=$1 AND n.chat_key_id>0 AND n.transport='http'
		AND n.endpoint IN ('/v1/chat/completions','/chat/completions')
		AND n.created_at>=p.created_at
		ORDER BY n.id LIMIT $2`, id, lookahead)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var laterID int64
		if err := rows.Scan(&laterID); err != nil {
			rows.Close()
			return false, err
		}
		laterIDs = append(laterIDs, laterID)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(laterIDs) == 0 {
		return false, err
	}
	followers, err := db.ReadPromptRequestTaskBodies(ctx, laterIDs, promptRequestTaskMaxBody)
	if err != nil {
		return false, err
	}
	before := gjson.GetBytes(body, "messages").Array()
	for _, laterID := range laterIDs {
		if promptChatReplyEchoed(before, gjson.GetBytes(followers[laterID].Body, "messages").Array(), reply) {
			return true, nil
		}
	}
	return false, nil
}

func promptChatReplyEchoed(before, after []gjson.Result, reply string) bool {
	if len(before) == 0 {
		return false
	}
	for index := len(after) - 2; index > 0; index-- {
		if after[index+1].Get("role").Str != "user" || after[index].Get("role").Str != "assistant" ||
			after[index].Get("tool_calls").Get("0").Exists() || after[index].Get("function_call").IsObject() {
			continue
		}
		text, _ := taskUserContent(after[index], 0)
		if strings.TrimSpace(reply) != strings.TrimSpace(text) {
			continue
		}
		if samePromptChatHistory(before, after[:index]) {
			return true
		}
		// A client can insert context earlier in the transcript. An exact
		// match of the last complete tool exchange still links the echo.
		start := len(before) - 1
		for start > 0 && before[start].Get("role").Str == "tool" {
			start--
		}
		tail := before[start:]
		if count, _ := promptChatToolExchange(tail); count == len(tail) &&
			index >= count && samePromptChatHistory(tail, after[index-count:index]) {
			return true
		}
	}
	return false
}
