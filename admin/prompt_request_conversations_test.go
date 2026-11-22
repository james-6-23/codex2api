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

func TestPromptRequestConversationsIndexLegacyRecordsAndReturnTimeline(t *testing.T) {
	handler, db, dbPath := newTestAdminHandlerFixtureWithPath(t)
	ctx := context.Background()
	key, err := db.InsertAPIKey(ctx, "conversation-test-key", "test-conversation-bearer-value")
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.InsertAPIKey(ctx, "other-conversation-test-key", "test-conversation-second-value")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Second)
	body := []byte(`{"client_metadata":{"thread_id":"fixture-thread"},"input":[{"role":"user","content":"earlier"}]}`)
	insert := func(keyID int64, shift time.Duration, transport string, legacy bool) int64 {
		t.Helper()
		in := database.PromptRequestBody{
			PromptRequestRecord: database.PromptRequestRecord{
				CreatedAt: now.Add(shift), Endpoint: "/v1/responses", Transport: transport, APIKey: "conversation-test-key",
			},
			Body: body, ContentType: "application/json", ConversationGroup: database.PromptRequestConversationGroup(body, keyID),
		}
		id, err := db.InsertPromptRequest(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		if legacy {
			execAdminOutputGuardTestSQLite(t, dbPath, fmt.Sprintf("UPDATE prompt_request_records SET conversation_group = '' WHERE id = %d", id))
		}
		return id
	}
	later := insert(key, time.Minute, "http", true)
	earlier := insert(key, 0, "http", true)
	insert(other, time.Minute, "http", false)
	last := insert(key, 2*time.Minute, "websocket", false)
	if err := db.IndexPromptRequestConversations(ctx); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	handler.RegisterRoutes(router)
	get := func(path string, result any) {
		t.Helper()
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("X-Admin-Key", "admin-secret")
		router.ServeHTTP(response, request)
		if err := json.Unmarshal(response.Body.Bytes(), result); response.Code != http.StatusOK || err != nil {
			t.Fatalf("request %s: status %d error %v", path, response.Code, err)
		}
	}
	const path = "/api/admin/prompt-filter/request-conversations"
	var list struct {
		Groups []database.PromptRequestConversation
	}
	get(path, &list)
	if len(list.Groups) != 2 || list.Groups[0].ID != last || list.Groups[0].Rounds != 3 || list.Groups[1].Rounds != 1 {
		t.Fatalf("groups: %+v", list.Groups)
	}
	var detail struct {
		Rounds []database.PromptRequestRecord
		Total  int
	}
	get(fmt.Sprintf("%s/%d", path, last), &detail)
	if detail.Total != 3 || len(detail.Rounds) != 3 ||
		detail.Rounds[0].ID != earlier || detail.Rounds[1].ID != later || detail.Rounds[2].ID != last {
		t.Fatalf("chronological rounds: %+v", detail)
	}
	older, total, err := db.ListPromptRequestRounds(ctx, last, later, 25)
	if err != nil || total != 3 || len(older) != 1 || older[0].ID != earlier {
		t.Fatalf("chronological pagination: %+v total=%d err=%v", older, total, err)
	}
}
