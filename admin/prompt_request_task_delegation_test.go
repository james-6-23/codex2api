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

func TestPromptRequestTaskDelegationLiveStatusAndFinal(t *testing.T) {
	handler, db := newTestAdminHandlerFixture(t)
	ctx := context.Background()
	start := time.Now().Add(-30 * time.Second).UTC()
	const meta = `{"thread_id":"fixture-main","turn_id":"fixture-turn","x-codex-turn-metadata":"{\"thread_id\":\"fixture-main\",\"turn_id\":\"fixture-turn\",\"request_kind\":\"turn\"}"}`
	const human = `{"role":"user","type":"message","content":[{"type":"input_text","text":"Original user input"}]}`
	const delegate = `,{"role":"user","type":"message","content":[{"type":"input_text","text":"[tool_result without a matching tool_call]\n<codex_delegation>\n<source_thread_id>11111111-1111-4111-8111-111111111111</source_thread_id>\n<input>Delegate input\nsecond line</input>\n</codex_delegation>"}]}`
	add := func(offset time.Duration, input string) int64 {
		t.Helper()
		body := []byte(`{"client_metadata":` + meta + `,"input":[` + input + `]}`)
		id, err := db.InsertPromptRequest(ctx, database.PromptRequestBody{
			PromptRequestRecord: database.PromptRequestRecord{CreatedAt: start.Add(offset), Endpoint: "/v1/responses", Transport: "http"},
			Body:                body, ConversationGroup: database.PromptRequestConversationGroup(body, 8),
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	save := func(id int64, response database.PromptRequestResponse) {
		t.Helper()
		if err := db.FinishPromptRequest(ctx, id, "allow", http.StatusOK); err != nil {
			t.Fatal(err)
		}
		if err := db.SavePromptRequestResponse(ctx, id, response); err != nil {
			t.Fatal(err)
		}
	}
	router := gin.New()
	handler.RegisterRoutes(router)
	get := func(path string, value any) {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/api/admin/prompt-filter"+path, nil)
		r.Header.Set("X-Admin-Key", "admin-secret")
		router.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s returned %d", path, w.Code)
		}
		if err := json.Unmarshal(w.Body.Bytes(), value); err != nil {
			t.Fatal(err)
		}
	}

	first := add(0, human)
	second := add(time.Second, human+delegate)
	finishedToolCall := start.Add(2 * time.Second)
	save(second, database.PromptRequestResponse{Status: "completed", FinishedAt: &finishedToolCall,
		ToolCalls: []database.PromptRequestResponseCall{{CallID: "call-1", Name: "lookup"}}})
	var page struct {
		Tasks []promptTaskSummary
		Total int `json:"total_requests"`
	}
	snapshotPath := fmt.Sprintf("/request-conversations/%d/tasks", first)
	livePath := snapshotPath + "?live=1"
	get(livePath, &page)
	if page.Total != 2 || len(page.Tasks) != 1 || page.Tasks[0].ID != first || page.Tasks[0].Status != "pending" ||
		page.Tasks[0].FinalPreview != "" || page.Tasks[0].UserText != "Original user input" {
		t.Fatalf("live tool continuation moved or completed the card: %+v", page)
	}
	last := add(3*time.Second, human+delegate)
	get(livePath, &page)
	if page.Total != 3 || len(page.Tasks) != 1 || page.Tasks[0].Status != "pending" {
		t.Fatalf("in-flight continuations should follow the live card: %+v", page)
	}
	finished := start.Add(4 * time.Second)
	const answer = "Confirmed visible final"
	save(last, database.PromptRequestResponse{Status: "completed", FinalObserved: true, FinalText: answer, FinishedAt: &finished})
	get(livePath, &page)
	if page.Total != 3 || len(page.Tasks) != 1 || page.Tasks[0].ID != first || page.Tasks[0].Status != "completed" ||
		page.Tasks[0].FinalPreview != answer || page.Tasks[0].DurationMS == nil || *page.Tasks[0].DurationMS != 4000 {
		t.Fatalf("final did not return to the original card: %+v", page)
	}
	page.Tasks = nil
	get(snapshotPath, &page)
	if page.Total != 1 || len(page.Tasks) != 1 || page.Tasks[0].FinalPreview != "" {
		t.Fatalf("normal archive views must remain pinned to their original cutoff: %+v", page)
	}
	var detail promptTaskDetail
	get(fmt.Sprintf("/request-tasks/%d", first), &detail)
	if len(detail.Steps) != 3 || len(detail.Steps[0].Delegations) != 0 || len(detail.Steps[1].Delegations) != 1 ||
		detail.Steps[1].Delegations[0].Text != "Delegate input\nsecond line" || len(detail.Steps[2].Delegations) != 0 ||
		detail.Final == nil || detail.Final.Text != answer || detail.User == nil || detail.User.RequestID != first {
		t.Fatalf("delegation evidence repeated or final attribution changed: %+v", detail)
	}
}

func TestPromptRequestTaskStaleActivityDoesNotRemainPending(t *testing.T) {
	old := time.Now().Add(-promptTaskPendingWindow - time.Minute)
	var detail promptTaskDetail
	detail.setGatewayCompletion(database.PromptRequestRecord{CreatedAt: old, Action: "received"},
		database.PromptRequestTaskResponse{}, true, nil)
	if detail.Status != "unobserved" {
		t.Fatalf("stale open requests should be unobserved, got %q", detail.Status)
	}
	detail.setGatewayCompletion(database.PromptRequestRecord{CreatedAt: old, Status: http.StatusOK},
		database.PromptRequestTaskResponse{Status: "completed", HasToolCalls: true, FinishedAt: &old}, true, nil)
	if detail.Status != "unobserved" {
		t.Fatalf("stale tools should be unobserved, got %q", detail.Status)
	}
}
