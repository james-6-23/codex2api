package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestPromptRequestChangesLegacyAndIndexedConversation(t *testing.T) {
	handler, db := newTestAdminHandlerFixture(t)
	router := gin.New()
	handler.RegisterRoutes(router)
	ctx := context.Background()
	insert := func(body string, indexed bool, keyID int64) int64 {
		t.Helper()
		request := database.PromptRequestBody{
			PromptRequestRecord: database.PromptRequestRecord{
				CreatedAt: time.Now().UTC(), Endpoint: "/v1/responses", APIKey: "shared label", Transport: "http", Model: "gpt-5.5",
			},
			Body: []byte(body), ContentType: "application/json",
		}
		if indexed {
			request.ConversationScope = database.PromptRequestConversationScope(request.Body, keyID, request.Endpoint, request.Transport)
		}
		id, err := db.InsertPromptRequest(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	get := func(id int64) promptRequestPresentation {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/admin/prompt-filter/requests/"+strconv.FormatInt(id, 10), nil)
		req.Header.Set("X-Admin-Key", "admin-secret")
		router.ServeHTTP(rec, req)
		var detail promptRequestPresentation
		if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("get record %d: status %d, error %v", id, rec.Code, err)
		}
		return detail
	}
	build := func(thread, items string) string {
		return `{"client_metadata":{"thread_id":"` + thread + `"},"input":[` + items + `]}`
	}
	base := `{"role":"user","content":[{"type":"input_text","text":"historical text"},{"type":"input_image","file_id":"historical-image"}]}`
	baseReordered := `{ "content":[{"text":"historical text","type":"input_text"},{"file_id":"historical-image","type":"input_image"}],"role":"user"}`
	suffix := `{"type":"custom_tool_call_output","output":"new tool data"},{"role":"user","content":[{"type":"input_text","text":"new user text"},{"type":"input_image","file_id":"new-image"}]},{"type":"reasoning","encrypted_content":"opaque content"}`
	prior := insert(build("primary", base), false, 0)
	insert(build("unrelated", base), false, 0)
	current := insert(build("primary", baseReordered+","+suffix), true, 10)
	detail := get(current)
	delta := detail.Changes
	if delta == nil || delta.PreviousID != prior || delta.RepeatedItems != 1 || delta.NewItems != 3 ||
		delta.HistoricalImages != 1 || len(delta.ImageIndices) != 1 || delta.ImageIndices[0] != 1 {
		t.Fatalf("bad comparison: %+v", delta)
	}
	if len(delta.Entries) != 2 ||
		delta.Entries[0].Kind != "tool_result" || delta.Entries[0].Text != "new tool data" ||
		delta.Entries[1].Kind != "text" || delta.Entries[1].Text != "new user text" {
		t.Fatalf("bad entries: %+v", delta.Entries)
	}
	if !strings.Contains(detail.Text, "historical text") || !strings.Contains(detail.Text, "opaque content") || len(detail.Images) != 2 {
		t.Fatal("the complete request or gallery was altered")
	}

	for index := range 14 {
		insert(build("interleaved-"+strconv.Itoa(index), base), true, 10)
	}
	insert(build("primary", base+","+`{"role":"user","content":"other key"}`), true, 11)
	last := insert(build("primary", base+","+suffix+","+`{"role":"assistant","content":"new assistant text"}`), true, 10)
	if delta := get(last).Changes; delta == nil || delta.PreviousID != current || delta.RepeatedItems != 4 || delta.NewItems != 1 {
		t.Fatalf("indexed lookup did not find the previous turn for the right key: %+v", delta)
	}
	replaced := insert(build("primary", `{"role":"user","content":"different history"}`), true, 10)
	if detail := get(replaced); detail.Changes != nil || !strings.Contains(detail.Text, "different history") {
		t.Fatal("unmatched history must stay visible")
	}
}
