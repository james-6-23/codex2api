package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestPromptRequestChatReplayJoinsTurnsAcrossWorkBuddyCompaction(t *testing.T) {
	h, db := newTestAdminHandlerFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	message := func(role, text string) map[string]any {
		return map[string]any{"role": role, "content": text}
	}
	instructions := message("developer", "WorkBuddy test instructions")
	contextBlock := func(text string) map[string]any {
		return message("user", `<system-reminder data-role="user-context">`+text+"</system-reminder>")
	}
	query := func(text string) map[string]any {
		return message("user", `<system-reminder data-role="user-context">Current mode</system-reminder><user_query>`+text+"</user_query>")
	}
	call := func(id string) map[string]any {
		return map[string]any{"role": "assistant", "tool_calls": []map[string]any{
			{"id": id, "type": "function", "function": map[string]string{"name": "lookup", "arguments": "{}"}},
		}}
	}
	receipt := func(id string) map[string]any {
		return map[string]any{"role": "tool", "tool_call_id": id, "content": "result for " + id}
	}
	add := func(keyID int64, history []map[string]any, final, calledTool string) int64 {
		t.Helper()
		body, err := json.Marshal(map[string]any{"messages": history})
		if err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
		id, err := db.InsertPromptRequest(ctx, database.PromptRequestBody{
			PromptRequestRecord: database.PromptRequestRecord{CreatedAt: now, Endpoint: "/v1/chat/completions", Transport: "http"},
			APIKeyID:            keyID, Body: body,
		})
		if err != nil {
			t.Fatal(err)
		}
		at := now
		reply := database.PromptRequestResponse{Status: "completed", FinishedAt: &at, ChatReplyClassified: true}
		if final != "" {
			reply.Text, reply.FinalText, reply.FinalObserved = final, final, true
			reply.ChatFinishReason = "stop"
		}
		if calledTool != "" {
			reply.ToolCalls = []database.PromptRequestResponseCall{{CallID: calledTool, Name: "lookup", Arguments: "{}"}}
			reply.ChatFinishReason = "tool_calls"
		}
		if err := db.SavePromptRequestResponse(ctx, id, reply); err != nil {
			t.Fatal(err)
		}
		if err := db.FinishPromptRequest(ctx, id, "allow", 200); err != nil {
			t.Fatal(err)
		}
		return id
	}

	userOne := query("First question")
	first := add(4, []map[string]any{instructions, contextBlock("Before"), userOne}, "First reply", "")
	start := []map[string]any{instructions, contextBlock("Updated context"), userOne, message("assistant", "First reply"), query("Second question")}
	otherKey := add(5, start, "Unrelated output", "")
	second := add(4, start, "", "lookup-1")
	toolStep := add(4, append(slices.Clone(start), call("lookup-1"), receipt("lookup-1")), "", "lookup-2")
	compact := add(4, []map[string]any{message("developer", "Summarizer"), receipt("lookup-1"),
		call("lookup-2"), receipt("lookup-2"), message("user", "Summarize the current conversation.")}, "Current WorkBuddy summary.", "")
	const resumePrompt = "<user_query>Please continue with the conversation based on the summarized context above. Maintain the same level of detail and helpfulness as before the summarization.</user_query>"
	resumeHistory := func(summary string) []map[string]any {
		return []map[string]any{instructions, message("user", `<system-reminder data-role="compact-summary"><conversation_history_summary>`+
			summary+"</conversation_history_summary></system-reminder>"), message("user", resumePrompt)}
	}
	mismatchedSummary := add(4, resumeHistory("A different summary."), "Unrelated continuation", "")
	resume := add(4, resumeHistory("Current WorkBuddy summary."), "Second reply", "")
	thirdHistory := []map[string]any{instructions, contextBlock("Refreshed after resume"), message("user", resumePrompt),
		message("assistant", "Second reply"), query("Third question")}
	third := add(4, thirdHistory, "Third reply", "")
	branch := add(4, append(slices.Clone(start[:4]), query("Independent branch")), "Branch reply", "")
	if err := db.IndexArchivedPromptRequestChats(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.IndexArchivedPromptRequestChats(ctx); err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	h.RegisterRoutes(router)
	get := func(path string, result any) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/api/admin/prompt-filter/"+path, nil)
		request.Header.Set("X-Admin-Key", "admin-secret")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if err := json.Unmarshal(response.Body.Bytes(), result); err != nil || response.Code != http.StatusOK {
			t.Fatalf("%s: status %d, decode error %v", path, response.Code, err)
		}
	}
	var page struct {
		Tasks    []promptTaskSummary `json:"tasks"`
		Internal []promptTaskSummary `json:"internal_events"`
		Total    int                 `json:"total_requests"`
	}
	get(fmt.Sprintf("request-conversations/%d/tasks?live=1", third), &page)
	if page.Total != 6 || len(page.Tasks) != 3 || len(page.Internal) != 0 {
		t.Fatalf("wrong conversation: %+v", page)
	}
	for i, id := range []int64{first, second, third} {
		if page.Tasks[i].ID != id {
			t.Fatalf("wrong turn anchor for %d: %d", i, page.Tasks[i].ID)
		}
	}
	if got := page.Tasks[1]; !slices.Equal(got.RequestIDs, []int64{second, toolStep, compact, resume}) ||
		got.UserText != "Second question" || got.FinalPreview != "Second reply" {
		t.Fatalf("wrong resumed turn: %+v", got)
	}
	var detail promptTaskDetail
	get(fmt.Sprintf("request-tasks/%d", resume), &detail)
	if detail.ID != second || len(detail.Steps) != 4 || detail.User == nil || detail.User.Text != "Second question" ||
		detail.Final == nil || detail.Final.Text != "Second reply" ||
		detail.Steps[2].Kind != database.PromptRequestChatCompaction || detail.Steps[3].Kind != database.PromptRequestChatResume ||
		detail.Steps[0].Response == nil || !detail.Steps[0].Response.ToolOnly {
		t.Fatalf("wrong resumed turn detail: %+v", detail)
	}
	for _, id := range []int64{otherKey, mismatchedSummary, branch} {
		var independent struct {
			Total int `json:"total_requests"`
		}
		get(fmt.Sprintf("request-conversations/%d/tasks?live=1", id), &independent)
		if independent.Total != 1 {
			t.Fatalf("request %d was merged into an unrelated conversation", id)
		}
	}
	var detached promptTaskDetail
	get(fmt.Sprintf("request-tasks/%d", mismatchedSummary), &detached)
	if detached.Kind != database.PromptRequestChatResume || detached.User != nil ||
		detached.Final == nil || detached.Final.Text != "Unrelated continuation" {
		t.Fatalf("unlinked resume should preserve its reply without inventing user text: %+v", detached)
	}
}
