package database

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const (
	promptTaskDelegationPrefix   = "[tool_result without a matching tool_call]"
	promptTaskDelegationTag      = "<codex_delegation>"
	promptTaskDelegationReadSize = 64 << 20
)

type PromptRequestTaskDelegation struct {
	Text           string `json:"text"`
	SourceThreadID string `json:"source_thread_id"`
}

func promptTaskDelegationBody(text string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(text), promptTaskDelegationPrefix)
	if !ok {
		return "", false
	}
	return strings.CutPrefix(strings.TrimSpace(rest), promptTaskDelegationTag)
}

func decodePromptTaskDelegation(item gjson.Result, thread string) (PromptRequestTaskDelegation, bool) {
	var delegation PromptRequestTaskDelegation
	parts := item.Get("content").Array()
	if item.Get("type").Str != "message" || len(parts) != 1 || parts[0].Get("type").Str != "input_text" {
		return delegation, false
	}
	body, ok := promptTaskDelegationBody(parts[0].Get("text").Str)
	if !ok {
		return delegation, false
	}
	body, ok = strings.CutSuffix(strings.TrimSpace(body), "</codex_delegation>")
	if !ok {
		return delegation, false
	}
	body, ok = strings.CutPrefix(strings.TrimSpace(body), "<source_thread_id>")
	if !ok {
		return delegation, false
	}
	source, body, ok := strings.Cut(body, "</source_thread_id>")
	if !ok || source == thread || len(source) != 36 {
		return delegation, false
	}
	if _, err := uuid.Parse(source); err != nil {
		return delegation, false
	}
	body, ok = strings.CutPrefix(strings.TrimSpace(body), "<input>")
	if !ok {
		return delegation, false
	}
	text, ok := strings.CutSuffix(strings.TrimSpace(body), "</input>")
	if !ok || strings.TrimSpace(text) == "" {
		return delegation, false
	}
	return PromptRequestTaskDelegation{SourceThreadID: source, Text: strings.TrimSpace(text)}, true
}

// ReconcilePromptRequestTaskDelegations normalizes existing stored markers in
// a read-only projection. The preview selects boundaries to inspect, but the
// stored body and matching Codex turn metadata establish the delegation.
func (db *DB) ReconcilePromptRequestTaskDelegations(ctx context.Context, rows []PromptRequestTaskRow) error {
	type boundary struct{ start, end int }
	var boundaries []boundary
	var ids []int64
	for start := 0; start < len(rows); {
		first := rows[start]
		end := start + 1
		for end < len(rows) && rows[end].Anchor == first.Anchor && rows[end].Turn == first.Turn {
			end++
		}
		if _, candidate := promptTaskDelegationBody(first.Preview); candidate && first.Indexed && first.Turn != "" {
			boundaries = append(boundaries, boundary{start, end})
			ids = append(ids, first.ID)
		}
		start = end
	}
	bodies, err := db.ReadPromptRequestTaskBodies(ctx, ids, promptTaskDelegationReadSize)
	if err != nil {
		return err
	}
	for _, boundary := range boundaries {
		first := &rows[boundary.start]
		input := DecodePromptRequestTaskInput(bodies[first.ID].Body)
		if len(input.Delegations) == 0 {
			continue
		}
		metadata := promptRequestTaskInputMetadata(input)
		if metadata.Turn != first.Turn {
			continue
		}
		first.Delegations = input.Delegations
		for i := boundary.start; i < boundary.end; i++ {
			rows[i].Anchor = metadata.Anchor
			rows[i].Preview = metadata.Preview
		}
	}
	return nil
}

// LatestPromptRequestConversationID resolves a newer recording only within
// the stored conversation of the supplied request.
func (db *DB) LatestPromptRequestConversationID(ctx context.Context, id int64) (int64, error) {
	var latest int64
	err := db.conn.QueryRowContext(ctx, `SELECT CASE WHEN r.conversation_group IN ('', 'single') THEN r.id
		ELSE (SELECT MAX(member.id) FROM prompt_request_records member WHERE member.conversation_group=r.conversation_group)
		END FROM prompt_request_records r WHERE r.id=$1`, id).Scan(&latest)
	return latest, err
}
