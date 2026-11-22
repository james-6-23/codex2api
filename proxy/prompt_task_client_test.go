package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

func TestPromptTaskClientIntakeRejectsAnonymousAndCrossKeyReports(t *testing.T) {
	h, _, _ := setupLifecycleTestHandler(t, "", "", "gpt-5.5")
	h.cfg.PromptRequestRecording = true
	ctx := context.Background()
	const credential = "sk-task-replay-integration-test-1"
	const other = "sk-task-replay-integration-test-2"
	keyID, err := h.db.InsertAPIKey(ctx, "client", credential)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.InsertAPIKey(ctx, "different client", other); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	submit := now.Add(-time.Minute)
	observed := submit.Add(5 * time.Second)
	stop := now.Add(-time.Second)
	body := []byte(`{"client_metadata":{"thread_id":"client-thread","turn_id":"client-turn"},"input":[{"role":"user","content":"What happened?"}]}`)
	group := database.PromptRequestConversationGroup(body, keyID)
	_, err = h.db.InsertPromptRequest(ctx, database.PromptRequestBody{
		PromptRequestRecord: database.PromptRequestRecord{
			CreatedAt: observed, Endpoint: "/v1/responses", Transport: "http", APIKey: "client",
		},
		Body: body, ContentType: "application/json", ConversationGroup: group,
	})
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.Use(security.RequestSizeLimiter(2 << 20))
	h.RegisterRoutes(router)
	send := func(key string, encoded []byte) int {
		request := httptest.NewRequest(http.MethodPost, "/v1/codex/task-replays", bytes.NewReader(encoded))
		request.Header.Set("Content-Type", "application/json")
		if key != "" {
			request.Header.Set("Authorization", "Bearer "+key)
		}
		reply := httptest.NewRecorder()
		router.ServeHTTP(reply, request)
		return reply.Code
	}
	report := promptTaskClientReport{
		SessionID: "client-thread", UserDigest: database.PromptTaskClientUserDigest("What happened?"),
		PromptTaskClientSnapshot: database.PromptTaskClientSnapshot{
			TurnID: "client-turn", StartedAt: submit, FinishedAt: &stop,
			Status: "completed", FinalText: "Completed. Key: " + credential,
			Steps: []database.PromptTaskClientStep{{Name: "Bash", Kind: "tool", Status: "observed", At: &observed}},
		},
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if got := send("", encoded); got != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d", got)
	}
	if got := send(other, encoded); got != http.StatusNotFound {
		t.Fatalf("different key status=%d", got)
	}
	if got := send(credential, []byte(`{"raw_transcript":"fixture"}`)); got != http.StatusBadRequest {
		t.Fatalf("unsupported field status=%d", got)
	}
	if got := send(credential, []byte(strings.Repeat(" ", security.MaxPromptTaskClientBodySize+1))); got != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status=%d", got)
	}
	if got := send(credential, encoded); got != http.StatusNoContent {
		t.Fatalf("valid status=%d", got)
	}
	result, err := h.db.MatchPromptTaskClient(ctx, group, "client-turn", "What happened?", observed)
	if err != nil || result == nil || !result.FinalRedacted || result.FinalText != "Completed. Key: [redacted]" ||
		result.FinishedAt == nil || !result.FinishedAt.Equal(stop) || len(result.Steps) != 1 {
		t.Fatalf("valid report was not stored with masked text and observed boundaries: err=%v", err)
	}
	missing, err := h.db.MatchPromptTaskClient(ctx, group, "different-turn", "What happened?", observed)
	if err != nil || missing != nil {
		t.Fatalf("explicit turn disagreement did not prevent matching: err=%v", err)
	}
}

func TestPromptTaskClientRejectsAnonymousEvenWhenModelRoutesAllowIt(t *testing.T) {
	h, _, _ := setupLifecycleTestHandler(t, "", "", "gpt-5.5")
	h.cfg.PromptRequestRecording = true
	h.cfg.AllowAnonymousV1 = true
	r := gin.New()
	h.RegisterRoutes(r)
	req := httptest.NewRequest(http.MethodPost, "/v1/codex/task-replays", bytes.NewReader([]byte("{}")))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous replay status=%d", rec.Code)
	}
}
