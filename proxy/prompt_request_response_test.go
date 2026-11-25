package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestPromptResponseRecordsSSEAndSharesRequestRetention(t *testing.T) {
	h, _, _ := setupLifecycleTestHandler(t, "", "", "gpt-5.5")
	h.cfg.PromptRequestRecording = true
	router := gin.New()
	stream := "event: response.output_text.delta\r\ndata: {\"type\":\"response.output_text.delta\",\r\ndata: \"delta\":\"first\\nline\"}\r\n\r\n" +
		"data: {\"type\":\"response.output_item.added\",\"output_index\":1,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":\"\"}}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":1,\"item_id\":\"fc_1\",\"delta\":\"{\\\"key\\\":\"}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":1,\"item_id\":\"fc_1\",\"delta\":\"\\\"value\\\"}\"}\n\n" +
		"data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"private reasoning\"}\n\n"
	completed := "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[" +
		"{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"first\\nline\"}]}," +
		"{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":\"{\\\"key\\\":\\\"value\\\"}\"}]}}\n\n"
	broken := "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"" +
		ErrorCodeUpstreamStreamBreak + "\",\"message\":\"upstream interrupted\"}}}\n\n"
	router.POST("/v1/responses", h.RecordModelRequest, func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream; charset=utf-8")
		c.Writer.Flush()
		_, _ = io.WriteString(c.Writer, stream[:65])
		_, _ = c.Writer.Write([]byte(stream[65:]))
		switch c.Query("end") {
		case "completed":
			_, _ = io.WriteString(c.Writer, completed)
		case "broken":
			_, _ = io.WriteString(c.Writer, broken)
		}
		c.Writer.Flush()
	})

	ctx := context.Background()
	wantCalls := []database.PromptRequestResponseCall{{CallID: "call_1", Name: "lookup", Arguments: "{\"key\":\"value\"}"}}
	for _, tt := range []struct {
		end, terminal, status, message string
	}{
		{"completed", completed, "completed", ""},
		{"eof", "", "interrupted", ""},
		{"broken", broken, "interrupted", "upstream interrupted"},
	} {
		t.Run(tt.end, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/responses?end="+tt.end, strings.NewReader("{\"input\":\"hello\"}"))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || rec.Body.String() != stream+tt.terminal {
				t.Fatalf("downstream stream changed: code=%d body=%s", rec.Code, rec.Body)
			}
			records, err := h.db.ListPromptRequests(ctx, 0, 1)
			if err != nil || len(records) != 1 {
				t.Fatalf("request records=%+v err=%v", records, err)
			}
			got, err := h.db.GetPromptRequestResponse(ctx, records[0].ID)
			if err != nil || got == nil || got.FinishedAt == nil || got.FinishedAt.Before(records[0].CreatedAt) {
				t.Fatalf("response finish time was not recorded correctly: %v", err)
			}
			want := &database.PromptRequestResponse{Status: tt.status, Text: "first\nline", ToolCalls: wantCalls, Error: tt.message}
			want.FinishedAt = got.FinishedAt
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("response=%+v, want %+v, err=%v", got, want, err)
			}
		})
	}

	records, err := h.db.ListPromptRequests(ctx, 0, 10)
	if err != nil || len(records) != 3 {
		t.Fatalf("SSE records=%+v err=%v", records, err)
	}
	if err := h.db.PurgeExpiredPromptRequests(ctx, time.Now().AddDate(0, 0, 31)); err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if got, err := h.db.GetPromptRequestResponse(ctx, record.ID); err != nil || got != nil {
			t.Fatalf("response should have expired with request %d, got=%+v err=%v", record.ID, got, err)
		}
	}
}

func TestPromptResponseCaptureSeparatesCommentaryFromFinal(t *testing.T) {
	events := []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant","phase":"commentary","content":[]}}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":"I will check the result."}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"message","role":"assistant","phase":"final_answer","content":[]}}`,
		`{"type":"response.output_text.delta","output_index":1,"delta":"Result verified.\nSecond line."}`,
	}
	for _, tc := range []struct {
		name, mode, terminal, wantStatus string
	}{
		{"stream-completed", "sse", `{"type":"response.completed","response":{"status":"completed"}}`, "completed"},
		{"websocket-interrupted", "websocket", "", "interrupted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := &promptResponseCapture{mode: tc.mode}
			for _, event := range append(append([]string{}, events...), tc.terminal) {
				if event == "" {
					continue
				}
				if tc.mode == "sse" {
					capture.observeSSE([]byte("data: " + event + "\n\n"))
				} else {
					capture.observeEvent([]byte(event))
				}
			}
			got := capture.result(http.StatusOK, false)
			if got.Status != tc.wantStatus || !got.FinalObserved || got.FinalText != "Result verified.\nSecond line." ||
				got.CommentaryText != "I will check the result." || got.Text != "I will check the result.\n\nResult verified.\nSecond line." {
				t.Fatalf("incorrect capture projection: %+v", got)
			}
		})
	}
	t.Run("json-final", func(t *testing.T) {
		capture := &promptResponseCapture{mode: "json", body: []byte(`{
			"output":[{"type":"message","role":"assistant","phase":"final_answer","content":"JSON answer"},
				{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Unclassified"}]}]}`)}
		got := capture.result(http.StatusOK, false)
		if !got.FinalObserved || got.FinalText != "JSON answer" || got.Text != "JSON answer\n\nUnclassified" {
			t.Fatalf("unexpected message phases: %+v", got)
		}
	})
}

func TestPromptResponseCaptureChatFinalReply(t *testing.T) {
	jsonChoice := func(message, finish string) string {
		return "{\"choices\":[{\"index\":0,\"message\":" + message + ",\"finish_reason\":" + finish + "}]}"
	}
	chatDelta := func(delta, finish string) string {
		return "data: {\"choices\":[{\"index\":0,\"delta\":" + delta + ",\"finish_reason\":" + finish + "}]}\n\n"
	}
	textDelta := chatDelta("{\"role\":\"assistant\",\"content\":\"第一行\\n\",\"reasoning_content\":\"private reasoning\"}", "null") +
		chatDelta("{\"content\":\"第二行\"}", "null")
	stop := chatDelta("{}", "\"stop\"")
	done := "data: {\"choices\":[],\"usage\":{\"completion_tokens\":10}}\n\ndata: [DONE]\n\n"
	for _, tc := range []struct {
		name, mode, payload, wantFinal string
		httpStatus                     int
		cancelled, writeFailed         bool
	}{
		{
			name: "json first choice only", mode: "json",
			payload: "{\"choices\":[{\"index\":1,\"message\":{\"content\":\"alternate choice\"},\"finish_reason\":\"stop\"}," +
				"{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"Primary answer\\nSecond line\",\"reasoning_content\":\"private reasoning\"},\"finish_reason\":\"stop\"}]}",
			wantFinal: "Primary answer\nSecond line",
		},
		{
			name: "json content parts", mode: "json",
			payload:   jsonChoice("{\"content\":[{\"type\":\"text\",\"text\":\"First\"},{\"type\":\"thinking\",\"text\":\"private reasoning\"},{\"type\":\"text\",\"text\":\"Second\"}]}", "\"stop\""),
			wantFinal: "First\n\nSecond",
		},
		{
			name: "json refusal", mode: "json",
			payload:   jsonChoice("{\"role\":\"assistant\",\"refusal\":\"I cannot answer that.\"}", "\"stop\""),
			wantFinal: "I cannot answer that.",
		},
		{
			name: "json reasoning only", mode: "json",
			payload: jsonChoice("{\"reasoning_content\":\"private reasoning\"}", "\"stop\""),
		},
		{
			name: "json token limit", mode: "json",
			payload: jsonChoice("{\"content\":\"unfinished\"}", "\"length\""),
		},
		{
			name: "json missing reason", mode: "json",
			payload: jsonChoice("{\"content\":\"unclassified\"}", "null"),
		},
		{
			name: "json error status", mode: "json",
			payload: jsonChoice("{\"content\":\"unavailable\"}", "\"stop\""), httpStatus: http.StatusBadGateway,
		},
		{
			name: "json unexpected role", mode: "json",
			payload: jsonChoice("{\"role\":\"tool\",\"content\":\"tool output\"}", "\"stop\""),
		},
		{
			name: "stream complete with usage", mode: "sse", payload: textDelta + stop + done, wantFinal: "第一行\n第二行",
		},
		{
			name: "stream interrupted after stop", mode: "sse", payload: textDelta + stop,
		},
		{
			name: "peer closes after the stream sentinel", mode: "sse", payload: textDelta + stop + done, cancelled: true, wantFinal: "第一行\n第二行",
		},
		{
			name: "stream write failed", mode: "sse", payload: textDelta + stop + done, writeFailed: true,
		},
		{
			name: "stream done without reason", mode: "sse", payload: textDelta + done,
		},
		{
			name: "stream with tool despite stop", mode: "sse",
			payload: textDelta + chatDelta("{\"tool_calls\":[{\"index\":0,\"id\":\"call-test\",\"function\":{\"name\":\"lookup\"}}]}", "null") + stop + done,
		},
		{
			name: "stream with legacy function call", mode: "sse",
			payload: textDelta + chatDelta("{\"function_call\":{\"name\":\"lookup\"}}", "null") + stop + done,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := &promptResponseCapture{mode: tc.mode, writeFailed: tc.writeFailed}
			if tc.mode == "sse" {
				capture.observeSSE([]byte(tc.payload))
			} else {
				capture.body = []byte(tc.payload)
			}
			status := tc.httpStatus
			if status == 0 {
				status = http.StatusOK
			}
			got := capture.result(status, tc.cancelled)
			if !got.ChatReplyClassified || got.FinalObserved != (tc.wantFinal != "") || got.FinalText != tc.wantFinal ||
				strings.Contains(got.Text, "private reasoning") {
				t.Fatalf("unexpected chat reply: %+v", got)
			}
		})
	}
}

func TestPromptResponseChatPersistsClassificationOnRequests(t *testing.T) {
	h, _, _ := setupLifecycleTestHandler(t, "", "", "gpt-5.5")
	h.cfg.PromptRequestRecording = true
	router := gin.New()
	for _, path := range []string{"/v1/chat/completions", "/chat/completions"} {
		router.POST(path, h.RecordModelRequest, func(c *gin.Context) {
			payload := "{\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"Observed reply\"},\"finish_reason\":\"stop\"}]}"
			if c.FullPath() == "/chat/completions" {
				payload = "{\"content\":[{\"type\":\"text\",\"text\":\"Other envelope\"}]}"
			}
			c.Data(http.StatusOK, "application/json", []byte(payload))
		})
	}
	ctx := context.Background()
	for _, tc := range []struct {
		path, wantText, wantFinal string
	}{
		{"/v1/chat/completions", "Observed reply", "Observed reply"},
		{"/chat/completions", "Other envelope", ""},
	} {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader("{\"messages\":[{\"role\":\"user\",\"content\":\"fixture\"}]}"))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			records, err := h.db.ListPromptRequests(ctx, 0, 1)
			if rec.Code != http.StatusOK || err != nil || len(records) != 1 || records[0].Endpoint != tc.path {
				t.Fatalf("chat request status=%d records=%+v err=%v", rec.Code, records, err)
			}
			got, err := h.db.GetPromptRequestResponse(ctx, records[0].ID)
			if err != nil || got == nil || !got.ChatReplyClassified || got.Text != tc.wantText || got.FinalText != tc.wantFinal ||
				got.FinalObserved != (tc.wantFinal != "") || got.FinishedAt == nil {
				t.Fatalf("wrong persisted chat classification: %+v err=%v", got, err)
			}
		})
	}
}

func TestPromptResponseCaptureBoundsUTF8AndKeepsTerminalMarker(t *testing.T) {
	capture := &promptResponseCapture{mode: "sse"}
	text := strings.Repeat("界", promptResponseTextLimit)
	capture.observeSSE([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"" + text + "\"}\n\n"))
	capture.observeSSE([]byte("data: " + strings.Repeat("x", promptResponseFrameLimit) + "\n\n"))
	capture.observeSSE([]byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"))
	got := capture.result(http.StatusOK, false)
	if got.Status != "completed" || !got.Truncated || got.Text != text[:promptResponseTextLimit/3*3] {
		t.Fatalf("status=%s truncated=%v bytes=%d", got.Status, got.Truncated, len(got.Text))
	}
}
