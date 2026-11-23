package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestPromptTaskOverviewSeparatesUserSendsInternalEventsAndMissingReplies(t *testing.T) {
	h, db := newTestAdminHandlerFixture(t)
	ctx := context.Background()
	start := time.Date(2026, 11, 22, 4, 0, 0, 0, time.UTC)
	const checkpoint = "You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary for another LLM that will resume the task.\n\nInclude:\n- Current progress and key decisions made\n- Important context, constraints, or user preferences\n- What remains to be done (clear next steps)\n- Any critical data, examples, or references needed to continue\n\nBe concise, structured, and focused on helping the next LLM seamlessly continue the work."
	add := func(turn, user, compactThread string, reply *database.PromptRequestResponse) int64 {
		t.Helper()
		start = start.Add(2 * time.Second)
		const thread = "fixture-thread"
		outer := map[string]string{"thread_id": thread, "turn_id": turn}
		if compactThread != "" {
			inner, err := json.Marshal(map[string]any{
				"thread_id": compactThread, "turn_id": turn, "request_kind": "compaction", "turn_started_at_unix_ms": start.UnixMilli() - 100,
				"compaction": map[string]string{"implementation": "responses_compaction_v2", "phase": "pre_turn", "reason": "comp_hash_changed", "strategy": "memento", "trigger": "auto"},
			})
			if err != nil {
				t.Fatal(err)
			}
			outer["x-codex-turn-metadata"] = string(inner)
		}
		body, err := json.Marshal(map[string]any{
			"client_metadata": outer,
			"input": []any{map[string]any{"role": "user", "type": "message",
				"content": []map[string]string{{"type": "input_text", "text": user}}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		id, err := db.InsertPromptRequest(ctx, database.PromptRequestBody{
			PromptRequestRecord: database.PromptRequestRecord{CreatedAt: start, Endpoint: "/v1/responses", Transport: "http"},
			Body:                body, ConversationGroup: database.PromptRequestConversationGroup(body, 1),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.FinishPromptRequest(ctx, id, "allow", http.StatusOK); err != nil {
			t.Fatal(err)
		}
		if reply != nil {
			end := start.Add(time.Second)
			reply.FinishedAt = &end
			if err := db.SavePromptRequestResponse(ctx, id, *reply); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	final := &database.PromptRequestResponse{Status: "completed", FinalObserved: true, FinalText: "结果\n换行"}
	interruptedReply := &database.PromptRequestResponse{Status: "interrupted", Text: "partial commentary"}
	completed := add("completed", "原文\n换行", "", final)
	pasted := add("pasted", checkpoint, "", nil)
	mismatched := add("other-thread", checkpoint, "a-different-thread", nil)
	event := add("compaction", checkpoint+"\n", "fixture-thread", final)
	missing := add("missing", "只有进度", "", &database.PromptRequestResponse{Status: "completed", Text: "only progress", CommentaryText: "only progress"})
	interrupted := add("interrupted", "中断了", "", interruptedReply)
	retried := add("retried", "已重试", "", interruptedReply)
	latest := add("retried", "已重试", "", final)

	router := gin.New()
	h.RegisterRoutes(router)
	get := func(path string, value any) {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/api/admin/prompt-filter"+path, nil)
		r.Header.Set("X-Admin-Key", "admin-secret")
		router.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, w.Code, w.Body)
		}
		if err := json.Unmarshal(w.Body.Bytes(), value); err != nil {
			t.Fatal(err)
		}
	}
	var page struct {
		Tasks  []promptTaskSummary `json:"tasks"`
		Events []promptTaskSummary `json:"internal_events"`
		Total  int                 `json:"total_requests"`
	}
	get(fmt.Sprintf("/request-conversations/%d/tasks", latest), &page)
	if page.Total != 8 || len(page.Tasks) != 6 || len(page.Events) != 1 || page.Events[0].ID != event ||
		page.Events[0].Kind != "context_compaction" || page.Events[0].UserText != "" {
		t.Fatalf("unexpected internal event separation: %+v", page)
	}
	byID := make(map[int64]promptTaskSummary)
	for _, task := range page.Tasks {
		byID[task.ID] = task
	}
	if got := byID[completed]; got.UserText != "原文\n换行" || got.FinalPreview != final.FinalText || got.Status != "completed" ||
		got.TimingSource != "gateway_observed" || got.DurationMS == nil || *got.DurationMS != 1000 {
		t.Fatalf("completed overview did not include original, reply and timing: %+v", got)
	}
	if byID[pasted].UserText != checkpoint || byID[mismatched].UserText != checkpoint {
		t.Fatal("checkpoint text alone or with mismatching metadata hid a user send")
	}
	if got := byID[missing]; got.Status != "unobserved" || got.FinalPreview != "" || got.DurationMS != nil {
		t.Fatalf("progress was promoted to a final reply: %+v", got)
	}
	if got := byID[interrupted]; got.Status != "interrupted" || got.FinalPreview != "" || got.DurationMS == nil || *got.DurationMS != 1000 {
		t.Fatalf("interruption or its timing was lost: %+v", got)
	}
	if got := byID[retried]; got.Status != "completed" || got.RequestCount != 2 || got.FinalPreview != final.FinalText {
		t.Fatalf("earlier interrupted request hid successful retry: %+v", got)
	}
	var detail promptTaskDetail
	get(fmt.Sprintf("/request-tasks/%d", interrupted), &detail)
	if detail.Status != "interrupted" || detail.DurationMS == nil || len(detail.Steps) != 1 || detail.Steps[0].Response == nil ||
		detail.Steps[0].Response.Status != "interrupted" {
		t.Fatalf("overview and execution disagreed about interruption: %+v", detail)
	}
}
