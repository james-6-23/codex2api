package database

import (
	"context"
	"testing"
	"time"
)

func TestPromptTaskClientMatchesOnlyExistingTurnAndPrunes(t *testing.T) {
	db := newPromptRetentionTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	start := now.Add(-2 * time.Hour)
	first := start.Add(2 * time.Second)
	finished := start.Add(9*time.Minute + 20*time.Second)
	body := []byte(`{"client_metadata":{"thread_id":"thread-1","turn_id":"turn-1"},"input":[{"role":"user","content":"Send once\nwith two lines."}]}`)
	groups := PromptTaskClientGroups("thread-1", 51)
	group := PromptRequestConversationGroup(body, 51)
	prompt := "Send once\r\nwith two lines."
	digest := PromptTaskClientUserDigest(prompt)
	snapshot := PromptTaskClientSnapshot{
		TurnID: "turn-1", StartedAt: start, FinishedAt: &finished, Status: "completed", FinalText: "The answer.",
	}
	if ok, err := db.StorePromptTaskClientSnapshot(ctx, groups, digest, snapshot); err != nil || ok {
		t.Fatalf("accepted without a real request: ok=%v err=%v", ok, err)
	}
	id, err := db.InsertPromptRequest(ctx, PromptRequestBody{
		PromptRequestRecord: PromptRequestRecord{CreatedAt: first}, Body: body, ConversationGroup: group,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := db.StorePromptTaskClientSnapshot(ctx, groups, PromptTaskClientUserDigest("wrong prompt"), snapshot); err != nil || ok {
		t.Fatalf("accepted unrelated user text: ok=%v err=%v", ok, err)
	}
	if ok, err := db.StorePromptTaskClientSnapshot(ctx, groups, digest, snapshot); err != nil || !ok {
		t.Fatalf("did not accept matching report: ok=%v err=%v", ok, err)
	}
	later := finished.Add(time.Second)
	update := snapshot
	update.FinishedAt, update.FinalText = &later, "The later answer."
	if ok, err := db.StorePromptTaskClientSnapshot(ctx, groups, digest, update); err != nil || !ok {
		t.Fatalf("continued stop did not update: ok=%v err=%v", ok, err)
	}
	if _, err := db.StorePromptTaskClientSnapshot(ctx, groups, digest, snapshot); err != nil {
		t.Fatal(err)
	}
	matched, err := db.MatchPromptTaskClient(ctx, group, "turn-1", prompt, first)
	if err != nil || matched == nil || matched.FinalText != update.FinalText || !matched.FinishedAt.Equal(later) {
		t.Fatalf("wrong matched report: err=%v", err)
	}
	if _, err := db.conn.ExecContext(ctx, "DELETE FROM prompt_request_records WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	matched, err = db.MatchPromptTaskClient(ctx, group, "turn-1", prompt, first)
	if err != nil || matched != nil {
		t.Fatalf("snapshot did not follow original request retention: err=%v", err)
	}
}
