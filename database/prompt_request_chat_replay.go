package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

const (
	PromptRequestChatCompaction = "context_compaction"
	PromptRequestChatResume     = "context_resume"

	workBuddyCompactEnvelope = `<system-reminder data-role="compact-summary">`
	workBuddyResumeQuery     = "<user_query>Please continue with the conversation based on the summarized context above. Maintain the same level of detail and helpfulness as before the summarization.</user_query>"
	promptChatReplaySearch   = 48
)

type promptChatReplayRow struct {
	id, turnRoot        int64
	group, anchor, kind string
	response            string
}

func (db *DB) indexPromptChatReplayBoundary(ctx context.Context, tx *sql.Tx, keyID int64, endpoint string,
	current promptChatReplayRow, messages []gjson.Result,
) (promptChatReplayRow, error) {
	if summary := workBuddyChatResumeSummary(messages); summary != "" {
		current.kind = PromptRequestChatResume
		return indexPromptChatCompaction(ctx, tx, keyID, endpoint, current, summary)
	}
	if len(messages) < 3 || current.turnRoot != 0 {
		return current, nil
	}
	echo, user := messages[len(messages)-2], messages[len(messages)-1]
	if echo.Get("role").Str != "assistant" || echo.Get("tool_calls.0").Exists() || echo.Get("function_call").Exists() {
		return current, nil
	}
	newText, attachment := taskUserContent(user, 0)
	if (newText == "" && !attachment) || workBuddyChatContext(user) {
		return current, nil
	}
	answer, _ := taskUserContent(echo, 0)
	if strings.TrimSpace(answer) == "" {
		return current, nil
	}

	candidates, err := readPromptChatReplayCandidates(ctx, tx, keyID, endpoint, current.id, false)
	if err != nil {
		return current, err
	}
	var previous promptChatReplayRow
	for _, candidate := range candidates {
		if !promptChatReplayGroupsCompatible(current.group, candidate.group) ||
			promptChatReplayFinal(candidate.response) != strings.TrimSpace(answer) {
			continue
		}
		priorMessages, err := readPromptChatReplayMessages(ctx, tx, candidate.id)
		if err != nil {
			return current, err
		}
		if !samePromptChatReplayPrefix(priorMessages, messages[:len(messages)-2]) {
			continue
		}
		if previous.id != 0 {
			return current, nil
		}
		previous = candidate
	}
	if previous.id == 0 || previous.group == current.group && current.group != singlePromptRequestGroup {
		return current, nil
	}
	group, linked, err := linkPromptChatReplayGroups(ctx, tx, current, previous, 0)
	if linked {
		current.group = group
	}
	return current, err
}

func indexPromptChatCompaction(ctx context.Context, tx *sql.Tx, keyID int64, endpoint string,
	current promptChatReplayRow, summary string,
) (promptChatReplayRow, error) {
	candidates, err := readPromptChatReplayCandidates(ctx, tx, keyID, endpoint, current.id, false)
	if err != nil {
		return current, err
	}
	var checkpoint promptChatReplayRow
	for _, candidate := range candidates {
		if promptChatReplayGroupsCompatible(current.group, candidate.group) && promptChatReplayFinal(candidate.response) == summary {
			if checkpoint.id != 0 {
				return current, nil
			}
			checkpoint = candidate
		}
	}
	if checkpoint.id == 0 {
		return current, nil
	}
	history, err := readPromptChatReplayMessages(ctx, tx, checkpoint.id)
	if err != nil {
		return current, err
	}
	if len(history) < 4 || history[len(history)-1].Get("role").Str != "user" {
		return current, nil
	}
	// The summarization request can rewrite earlier context. Verify the
	// last complete tool exchange and the exact receipt preceding it.
	end := len(history) - 1
	start := end - 1
	for start > 1 && history[start].Get("role").Str == "tool" {
		start--
	}
	count, calls := promptChatToolExchange(history[start:end])
	if count != end-start || len(calls) == 0 {
		return current, nil
	}

	candidates, err = readPromptChatReplayCandidates(ctx, tx, keyID, endpoint, checkpoint.id, true)
	if err != nil {
		return current, err
	}
	var previous promptChatReplayRow
	for _, candidate := range candidates {
		if !promptChatReplayGroupsCompatible(current.group, candidate.group) ||
			!promptChatReplayGroupsCompatible(checkpoint.group, candidate.group) ||
			!capturedPromptChatCalls(candidate.response, calls) {
			continue
		}
		prior, err := readPromptChatReplayMessages(ctx, tx, candidate.id)
		if err != nil {
			return current, err
		}
		if len(prior) < 2 || !isWorkBuddyChat(prior) || prior[len(prior)-1].Get("role").Str != "tool" ||
			!samePromptChatHistory(prior[len(prior)-1:], history[start-1:start]) {
			continue
		}
		if previous.id != 0 {
			return current, nil
		}
		previous = candidate
	}
	if previous.id == 0 || previous.anchor == "" {
		return current, nil
	}
	if checkpoint.group != singlePromptRequestGroup && checkpoint.group != previous.group && checkpoint.group != current.group {
		return current, nil
	}
	root := previous.turnRoot
	if root == 0 {
		root = previous.id
		if previous.group != singlePromptRequestGroup {
			if err := tx.QueryRowContext(ctx, `SELECT MIN(id) FROM prompt_request_records
				WHERE conversation_group=$1 AND task_anchor=$2 AND id<=$3 AND chat_key_id=$4`,
				previous.group, previous.anchor, previous.id, keyID).Scan(&root); err != nil {
				return current, err
			}
		}
	}
	group, linked, err := linkPromptChatReplayGroups(ctx, tx, current, previous, root)
	if err != nil || !linked {
		return current, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE prompt_request_records SET conversation_group=$1, chat_turn_root=$2,
		chat_replay_kind=$3 WHERE id=$4 AND chat_key_id=$5`, group, root, PromptRequestChatCompaction, checkpoint.id, keyID); err != nil {
		return current, err
	}
	current.group, current.turnRoot = group, root
	return current, nil
}

// Only an inferred group can be moved. A client-supplied conversation can
// provide both sides of a link when they already belong to that same group.
func promptChatReplayGroupsCompatible(current, candidate string) bool {
	inferred := func(group string) bool {
		return group == singlePromptRequestGroup || strings.HasPrefix(group, "chat:")
	}
	return current == candidate || inferred(current) && inferred(candidate)
}

func linkPromptChatReplayGroups(ctx context.Context, tx *sql.Tx, current, previous promptChatReplayRow, turnRoot int64) (string, bool, error) {
	group := previous.group
	if group == singlePromptRequestGroup {
		group = fmt.Sprintf("chat:%d", previous.id)
		if _, err := tx.ExecContext(ctx, "UPDATE prompt_request_records SET conversation_group=$1 WHERE id=$2", group, previous.id); err != nil {
			return current.group, false, err
		}
	} else if group != current.group {
		// A second child of a previous turn may be a branch of the
		// transcript. Leave it independent when a later turn is known.
		var successor bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM prompt_request_records
			WHERE conversation_group=$1 AND id>$2 AND id<$3 AND task_anchor<>$4 AND chat_turn_root=0)`,
			group, previous.id, current.id, previous.anchor).Scan(&successor); err != nil || successor {
			return current.group, false, err
		}
	}
	if current.group != singlePromptRequestGroup {
		if _, err := tx.ExecContext(ctx, `UPDATE prompt_request_records SET conversation_group=$1,
			chat_turn_root=CASE WHEN task_anchor=$2 AND id>=$3 AND $4>0 THEN $4 ELSE chat_turn_root END
			WHERE conversation_group=$5 AND `+promptRequestChatRoutes,
			group, current.anchor, current.id, turnRoot, current.group); err != nil {
			return current.group, false, err
		}
	}
	return group, true, nil
}

func readPromptChatReplayCandidates(ctx context.Context, tx *sql.Tx, keyID int64, endpoint string, before int64,
	tool bool,
) ([]promptChatReplayRow, error) {
	pattern := `%"final_observed":true%`
	if tool {
		pattern = `%"tool_calls":[{%`
	}
	rows, err := tx.QueryContext(ctx, `SELECT r.id, r.conversation_group, COALESCE(r.task_anchor,''), r.chat_turn_root,
		p.response FROM prompt_request_records r JOIN prompt_request_responses p ON p.request_id=r.id
		WHERE r.chat_key_id=$1 AND r.chat_key_id>0 AND r.endpoint=$2 AND r.transport='http'
		AND r.endpoint IN ('/v1/chat/completions','/chat/completions')
		AND r.id<$3 AND r.status>=200 AND r.status<300 AND r.chat_indexed>0 AND r.chat_replay_kind<>'context_compaction'
		AND p.response LIKE $4 ORDER BY r.id DESC LIMIT $5`, keyID, endpoint, before, pattern, promptChatReplaySearch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []promptChatReplayRow
	for rows.Next() {
		var row promptChatReplayRow
		if err := rows.Scan(&row.id, &row.group, &row.anchor, &row.turnRoot, &row.response); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func readPromptChatReplayMessages(ctx context.Context, tx *sql.Tx, id int64) ([]gjson.Result, error) {
	var body []byte
	err := tx.QueryRowContext(ctx, "SELECT body FROM prompt_request_records WHERE id=$1 AND LENGTH(body)<=$2",
		id, promptRequestTaskMaxBody).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return gjson.GetBytes(body, "messages").Array(), err
}

func promptChatReplayFinal(response string) string {
	fields := gjson.GetMany(response, "status", "final_observed", "final_text", "error", "truncated", "tool_calls.#")
	if fields[0].Str != "completed" || !fields[1].Bool() || fields[3].Str != "" ||
		fields[4].Bool() || fields[5].Int() != 0 {
		return ""
	}
	return strings.TrimSpace(fields[2].Str)
}

func isWorkBuddyChat(messages []gjson.Result) bool {
	if len(messages) == 0 {
		return false
	}
	first := messages[0]
	role := first.Get("role").Str
	text, _ := taskUserContent(first, 0)
	return (role == "developer" || role == "system") && strings.Contains(strings.ToLower(text), "workbuddy")
}

func workBuddyChatContext(message gjson.Result) bool {
	text, _ := taskUserContent(message, 0)
	text = strings.TrimSpace(text)
	if message.Get("role").Str != "user" || strings.Contains(text, "<user_query>") || !strings.HasSuffix(text, "</system-reminder>") {
		return false
	}
	return strings.HasPrefix(text, `<system-reminder data-role="user-context">`) ||
		strings.HasPrefix(text, `<system-reminder data-role="additional-data">`) ||
		strings.HasPrefix(text, workBuddyCompactEnvelope)
}

func samePromptChatReplayPrefix(prior, next []gjson.Result) bool {
	if len(prior) == 0 || len(prior) != len(next) {
		return false
	}
	if samePromptChatHistory(prior, next) {
		return true
	}
	// WorkBuddy refreshes the context-only message immediately after its
	// instructions. Require the entire remaining transcript to match.
	return len(prior) > 2 && isWorkBuddyChat(prior) && isWorkBuddyChat(next) &&
		workBuddyChatContext(prior[1]) && workBuddyChatContext(next[1]) &&
		samePromptChatHistory(prior[:1], next[:1]) && samePromptChatHistory(prior[2:], next[2:])
}

func workBuddyChatResumeSummary(messages []gjson.Result) string {
	if len(messages) != 3 || !isWorkBuddyChat(messages) ||
		messages[1].Get("role").Str != "user" || messages[2].Get("role").Str != "user" {
		return ""
	}
	resume, attachment := taskUserContent(messages[2], 0)
	if attachment || strings.TrimSpace(resume) != workBuddyResumeQuery {
		return ""
	}
	envelope, _ := taskUserContent(messages[1], 0)
	envelope = strings.TrimSpace(envelope)
	if !strings.HasPrefix(envelope, workBuddyCompactEnvelope) || !strings.HasSuffix(envelope, "</system-reminder>") {
		return ""
	}
	_, summary, ok := strings.Cut(envelope, "<conversation_history_summary>")
	if !ok {
		return ""
	}
	summary, _, ok = strings.Cut(summary, "</conversation_history_summary>")
	if !ok {
		return ""
	}
	return strings.TrimSpace(summary)
}
