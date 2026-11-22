package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestPromptRequestResultsAttributeArchivedReceipts(t *testing.T) {
	insert, detail, _ := promptRequestResultTestFixture(t)
	const prior = `{"role":"user","content":[{"type":"input_text","text":"original"},{"type":"input_image","file_id":"earlier-image"}]}`
	const appended = `,{"type":"custom_tool_call","call_id":"call-26","name":"exec","input":"echo output"},
		{"type":"custom_tool_call_output","call_id":"call-26","output":[{"type":"input_text","text":"first\nsecond"},{"type":"input_image","image_url":"data:image/png;base64,dGVzdA=="}]},
		{"type":"function_call_output","call_id":"unknown","output":"unattributed output"},
		{"role":"user","content":"new instructions"}`
	first := insert("historical", 5, prior)
	insert("historical", 6, prior+appended)
	second := insert("historical", 5, prior+appended)
	getFirst := detail(first)
	if getFirst.Result == nil || getFirst.Result.Response != nil || len(getFirst.Result.ToolResults) != 1 {
		t.Fatalf("missing or incorrect archived result: %+v", getFirst.Result)
	}
	tool := getFirst.Result.ToolResults[0]
	if tool.CallID != "call-26" || tool.Name != "exec" || tool.FromRequestID != second || tool.Arguments != "echo output" ||
		!strings.Contains(tool.Text, `first\nsecond`) || !strings.Contains(tool.Text, "[Image 2]") ||
		strings.Contains(tool.Text, "base64") || len(tool.Images) != 1 || tool.Images[0].Index != 1 {
		t.Fatalf("misassociated archived receipt: %+v", tool)
	}
	getSecond := detail(second)
	if getSecond.Result != nil || !strings.Contains(getSecond.Text, "unattributed output") || getSecond.Changes == nil {
		t.Fatalf("incomplete continuation: %+v", getSecond)
	}
	var attributed, newUser, unmatched int
	for _, entry := range getSecond.Changes.Entries {
		switch {
		case entry.CallID == "call-26" && entry.ResultRequestID == first:
			attributed++
		case entry.Text == "new instructions" && entry.ResultRequestID == 0:
			newUser++
		case entry.CallID == "unknown" && entry.ResultRequestID == 0:
			unmatched++
		}
	}
	if attributed != 2 || newUser != 1 || unmatched != 1 {
		t.Fatalf("incorrect delta provenance: %+v", getSecond.Changes.Entries)
	}
}

func TestPromptRequestResultsDoNotInferFromLaterBranch(t *testing.T) {
	insert, detail, _ := promptRequestResultTestFixture(t)
	const prior = `{"role":"user","content":"original"},{"role":"user","content":"specific context"}`
	const pair = `,{"type":"function_call","call_id":"call-a","name":"lookup","arguments":"{}"},
		{"type":"function_call_output","call_id":"call-a","output":"data"}`
	first := insert("branches", 9, prior)
	second := insert("branches", 9, `{"role":"user","content":"original"},{"role":"user","content":"divergent context"}`+pair)
	insert("branches", 9, prior+pair)
	if got := detail(first); got.Result != nil {
		t.Fatalf("unrelated branch was mistaken for a direct continuation: %+v", got.Result)
	}
	if got := detail(second); got.Changes == nil || len(got.Changes.Entries) != 3 || got.Changes.Entries[1].ResultRequestID != 0 {
		t.Fatalf("unrelated branch delta was hidden: %+v", got.Changes)
	}
}

func TestPromptRequestResultsShowRecordedResponseAndDelayedReceipt(t *testing.T) {
	insert, detail, db := promptRequestResultTestFixture(t)
	const prior = `{"role":"user","content":"question"}`
	const receipt = `,{"type":"function_call_output","call_id":"call-x","output":"tool response"}`
	id := insert("captured", 10, prior)
	insert("captured", 11, prior+receipt)
	insert("captured", 10, prior+`,{"role":"assistant","content":"continues"}`)
	last := insert("captured", 10, prior+`,{"role":"assistant","content":"continues"}`+receipt)
	if err := db.SavePromptRequestResponse(context.Background(), id, database.PromptRequestResponse{
		Status: "interrupted", Text: "partial\nreply", ToolCalls: []database.PromptRequestResponseCall{
			{CallID: "call-x", Name: "lookup", Arguments: `{"query":"value"}`},
		},
	}); err != nil {
		t.Fatal(err)
	}
	got := detail(id).Result
	if got == nil || got.Response == nil || got.Response.Status != "interrupted" || got.Response.Text != "partial\nreply" ||
		len(got.Response.ToolCalls) != 1 || len(got.ToolResults) != 1 {
		t.Fatalf("missing stored response or tool receipt: %+v", got)
	}
	if tool := got.ToolResults[0]; tool.FromRequestID != last || tool.Text != "tool response" || tool.Name != "lookup" {
		t.Fatalf("wrong captured receipt: %+v", tool)
	}
	if changes := detail(last).Changes; changes == nil || len(changes.Entries) != 1 || changes.Entries[0].ResultRequestID != id {
		t.Fatalf("wrong delayed receipt provenance: %+v", changes)
	}
}

func TestPromptRequestResultsRespectUnsuccessfulToolReceiptStatus(t *testing.T) {
	for _, status := range []string{"failed", "incomplete", "cancelled", "interrupted", "timed_out", "completed"} {
		t.Run(status, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"input":[{"type":"function_call_output","call_id":"tool-fixture","status":%q,
				"output":{"exit_code":0,"output":"fixture output"}}]}`, status))
			var events []promptRequestToolEvent
			visitPromptRequestTools(body, func(event promptRequestToolEvent) { events = append(events, event) })
			if len(events) != 1 || !events[0].receipt || events[0].failed != (status != "completed") {
				t.Fatalf("unexpected receipt classification for %q: %+v", status, events)
			}
		})
	}
}

func TestPromptRequestResultsExcludeInlineUserSkillExamples(t *testing.T) {
	insert, detail, _ := promptRequestResultTestFixture(t)
	const fragment = "<skill>\n<name>fixture</name>\n<path>/skills/fixture/SKILL.md</path>\nUse the fixture.\n</skill>"
	const inline = "This is a pasted example:\n" + fragment
	for _, tt := range []struct {
		text string
		want bool
	}{{fragment, true}, {inline, false}} {
		item := fmt.Sprintf(`{"role":"user","content":%q}`, tt.text)
		got := detail(insert("skill-fragments", 12, item))
		var loaded bool
		for _, section := range got.PromptContext.Sections {
			loaded = loaded || section.Kind == promptContextSkill
		}
		if loaded != tt.want {
			t.Fatalf("unexpected classification for input sample: %t, want %t", loaded, tt.want)
		}
	}
}

func promptRequestResultTestFixture(t *testing.T) (insert func(thread string, keyID int64, history string) int64, detail func(int64) promptRequestPresentation, db *database.DB) {
	t.Helper()
	handler, db := newTestAdminHandlerFixture(t)
	router := gin.New()
	handler.RegisterRoutes(router)
	return func(thread string, keyID int64, history string) int64 {
			t.Helper()
			body := []byte(fmt.Sprintf(`{"client_metadata":{"thread_id":%q},"input":[%s]}`, thread, history))
			timestamp := time.Date(2026, 11, 21, 13, 0, 0, 0, time.UTC)
			id, err := db.InsertPromptRequest(context.Background(), database.PromptRequestBody{
				PromptRequestRecord: database.PromptRequestRecord{CreatedAt: timestamp, Endpoint: "/v1/responses", Transport: "http", APIKey: "test-key"},
				ContentType:         "application/json",
				Body:                body,
				ConversationGroup:   database.PromptRequestConversationGroup(body, keyID),
			})
			if err != nil {
				t.Fatal(err)
			}
			return id
		}, func(id int64) promptRequestPresentation {
			t.Helper()
			response := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/admin/prompt-filter/requests/%d", id), nil)
			req.Header.Set("X-Admin-Key", "admin-secret")
			router.ServeHTTP(response, req)
			var result promptRequestPresentation
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != http.StatusOK {
				t.Fatalf("get record %d: status %d, error %v", id, response.Code, err)
			}
			return result
		}, db
}
