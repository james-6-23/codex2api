package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func TestPromptRequestRecordsAllowedAndBlockedIngress(t *testing.T) {
	var forwarded atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_record_test","object":"response","status":"completed","model":"gpt-5.5","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}]}`)
	}))
	defer upstream.Close()

	handler, store, _ := setupLifecycleTestHandler(t, upstream.URL, auth.UpstreamOpenAIResponses, "gpt-5.5")
	handler.cfg.PromptRequestRecording = true
	cfg := promptGuardTestConfig()
	cfg.LogMatches = false
	cfg.CustomPatterns = []promptfilter.PatternConfig{
		{Name: "request_record_test", Pattern: "fixture-block-watermelon", Weight: 100, Category: "test"},
	}
	store.SetPromptFilterConfig(cfg)

	router := gin.New()
	router.Use(security.RequestSizeLimiter(1 << 20))
	handler.RegisterRoutes(router)
	payloads := []string{
		`{"model":"gpt-5.5","input":"Full original text from a permitted request"}`,
		`{"model":"gpt-5.5","input":"fixture-block-watermelon"}`,
	}
	for index, body := range payloads {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if (index == 0 && rec.Code != http.StatusOK) || (index == 1 && rec.Code < 400) {
			t.Fatalf("request %d: status=%d body=%s", index, rec.Code, rec.Body)
		}
	}
	if forwarded.Load() != 1 {
		t.Fatalf("forwarded %d requests, want 1", forwarded.Load())
	}

	ctx := context.Background()
	records, err := handler.db.ListPromptRequests(ctx, 0, 10)
	if err != nil || len(records) != 2 {
		t.Fatalf("records=%+v err=%v", records, err)
	}
	for index, action := range []string{"block", "allow"} {
		summary := records[index]
		record, err := handler.db.GetPromptRequest(ctx, summary.ID)
		if err != nil || summary.Action != action || string(record.Body) != payloads[1-index] {
			t.Fatalf("record=%+v body=%q err=%v, want action=%s", summary, record.Body, err, action)
		}
		response, err := handler.db.GetPromptRequestResponse(ctx, summary.ID)
		if err != nil || response == nil {
			t.Fatalf("record %d response=%+v err=%v", summary.ID, response, err)
		}
		if index == 0 && (response.Status != "failed" || response.Error == "") {
			t.Fatalf("blocked request response=%+v", response)
		}
		if index == 1 && (response.Status != "completed" || response.Text != "Hello") {
			t.Fatalf("allowed request response=%+v", response)
		}
	}
}

func TestPromptRequestRecordsOneEntryPerWebSocketTurn(t *testing.T) {
	handler, _, _ := setupLifecycleTestHandler(t, "", "", "gpt-5.5")
	handler.cfg.PromptRequestRecording = true
	previous := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previous })
	var calls atomic.Int32
	WebsocketExecuteFunc = func(_ context.Context, _ *auth.Account, _ []byte, _, _, _ string, _ *DeviceProfileConfig, _ http.Header, _ string) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return responsesWSMultiplexResponse("resp_record_one"), nil
		}
		return responsesWSMultiplexResponse("resp_record_two"), nil
	}
	router := gin.New()
	router.Use(security.RequestSizeLimiter(1 << 20))
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses",
		http.Header{"Session-Id": {"prompt-record-test"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	tokens := []string{"websocket first prompt", "websocket second prompt"}
	for index, id := range []string{"resp_record_one", "resp_record_two"} {
		responsesWSMultiplexSend(t, conn, "test", tokens[index])
		responsesWSMultiplexReadTerminal(t, conn, "test", id)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var records []database.PromptRequestRecord
	for {
		records, err = handler.db.ListPromptRequests(ctx, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(records) == 2 && records[0].Action == "allow" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("expected two completed WebSocket records, got %+v", records)
		case <-time.After(10 * time.Millisecond):
		}
	}
	for index, summary := range records {
		row, err := handler.db.GetPromptRequest(ctx, summary.ID)
		if err != nil || row.Action != "allow" || row.Transport != "websocket" || !strings.Contains(string(row.Body), tokens[1-index]) {
			t.Fatalf("WebSocket row=%+v err=%v", row, err)
		}
		response, err := handler.db.GetPromptRequestResponse(ctx, summary.ID)
		if err != nil || response == nil || response.Status != "completed" || response.Text == "" {
			t.Fatalf("WebSocket response=%+v err=%v", response, err)
		}
	}
}
