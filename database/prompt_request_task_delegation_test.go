package database

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestPromptRequestTaskDelegationReconcilesArchivedAnchors(t *testing.T) {
	db := newPromptRetentionTestDB(t)
	ctx := context.Background()
	const outer = `{"thread_id":"fixture-main","turn_id":"fixture-turn"`
	const native = `,"x-codex-turn-metadata":"{\"thread_id\":\"fixture-main\",\"turn_id\":\"fixture-turn\",\"request_kind\":\"turn\"}"`
	const user = `{"role":"user","type":"message","content":[{"type":"input_text","text":"Original task"}]}`
	const envelope = `[tool_result without a matching tool_call]\n<codex_delegation>\n<source_thread_id>11111111-1111-4111-8111-111111111111</source_thread_id>\n<input>Delegate task</input>\n</codex_delegation>`
	const wrapper = `{"role":"user","type":"message","content":[{"type":"input_text","text":"` + envelope + `"}]}`
	body := func(meta, items string) []byte {
		return []byte(`{"client_metadata":` + outer + meta + `},"input":[` + items + `]}`)
	}
	start := time.Now().UTC()
	add := func(offset int, items string) int64 {
		t.Helper()
		raw := body(native, items)
		id, err := db.InsertPromptRequest(ctx, PromptRequestBody{
			PromptRequestRecord: PromptRequestRecord{CreatedAt: start.Add(time.Duration(offset) * time.Second)},
			Body:                raw, ConversationGroup: PromptRequestConversationGroup(raw, 8),
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	origin := add(0, user)
	wrapped := add(1, user+`,`+wrapper)
	carry := add(2, user+`,`+wrapper)
	steer := add(3, user+`,`+wrapper+`,{"role":"user","content":"Genuine same-turn steer"}`)
	legacy := promptRequestTaskMetadata(body("", user+`,`+wrapper))
	for _, id := range []int64{wrapped, carry} {
		if _, err := db.conn.ExecContext(ctx, "UPDATE prompt_request_records SET task_anchor=$1,task_preview=$2 WHERE id=$3", legacy.Anchor, legacy.Preview, id); err != nil {
			t.Fatal(err)
		}
	}
	lookup := func() []PromptRequestTaskRow {
		t.Helper()
		r, err := db.ReadPromptRequestTaskRows(ctx, origin, 0, false, 5)
		if err != nil {
			t.Fatal(err)
		}
		return r.Rows
	}
	rows := lookup()
	if err := db.ReconcilePromptRequestTaskDelegations(ctx, rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 || rows[1].Anchor != rows[0].Anchor || rows[2].Anchor != rows[0].Anchor ||
		rows[1].Preview != "Original task" || len(rows[1].Delegations) != 1 ||
		rows[1].Delegations[0].Text != "Delegate task" || len(rows[2].Delegations) != 0 ||
		rows[3].ID != steer || rows[3].Anchor == rows[0].Anchor || rows[3].Preview != "Genuine same-turn steer" {
		t.Fatal("archived delegates did not reconcile with their original user anchor")
	}
	if stored := lookup(); stored[1].Anchor != legacy.Anchor || stored[2].Anchor != legacy.Anchor {
		t.Fatal("read-only reconciliation rewrote stored metadata")
	}
	if input := DecodePromptRequestTaskInput(body("", user+`,`+wrapper)); !strings.HasPrefix(input.Text, promptTaskDelegationPrefix) || len(input.Delegations) != 0 {
		t.Fatal("unverified literal wrapper was hidden")
	}
	quoted := strings.Replace(wrapper, envelope, `Please inspect this wrapper:\n`+envelope, 1)
	if input := DecodePromptRequestTaskInput(body(native, user+`,`+quoted)); !strings.HasPrefix(input.Text, "Please inspect") || len(input.Delegations) != 0 {
		t.Fatal("user text containing a wrapper was hidden")
	}
}
