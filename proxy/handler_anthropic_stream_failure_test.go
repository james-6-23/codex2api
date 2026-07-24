package proxy

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// newAnthropicStreamFailureTestHandler 搭一个走 Resin HTTP 上游的 /v1/messages
// 测试环境：假上游按 serve 回调逐次响应，返回 handler 与上游调用计数。
func newAnthropicStreamFailureTestHandler(t *testing.T, serve func(call int32, w http.ResponseWriter)) (*Handler, *atomic.Int32) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	previousResin := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(previousResin) })

	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		serve(call, w)
	}))
	t.Cleanup(upstream.Close)
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})

	settings := &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4", MaxRetries: 2}
	store := auth.NewStore(nil, nil, settings)
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"}
	store.AddAccount(account)
	return NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil), &calls
}

func invokeAnthropicMessagesStream(t *testing.T, handler *Handler) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	body := `{"model":"claude-opus-4-6","max_tokens":128,"stream":true,"messages":[{"role":"user","content":"hello"}]}`
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.Messages(ctx)
	return recorder
}

func writeCodexSSE(w http.ResponseWriter, events ...string) {
	for _, event := range events {
		_, _ = io.WriteString(w, "data: "+event+"\n\n")
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// TestMessagesStreamMidBreakEmitsErrorEventNotCleanStop 验证 issue #435 修复：
// 正文已开始后上游断流（未收到终止事件），下游必须收到 Anthropic 流内 error 事件，
// 而不是伪造 stop_reason=end_turn + message_stop 的"干净空收尾"（下游会把截断
// 响应当成功，既无从感知也无从重试）。
func TestMessagesStreamMidBreakEmitsErrorEventNotCleanStop(t *testing.T) {
	handler, calls := newAnthropicStreamFailureTestHandler(t, func(call int32, w http.ResponseWriter) {
		writeCodexSSE(w,
			`{"type":"response.created","response":{"id":"resp_break"}}`,
			`{"type":"response.output_item.added","item":{"type":"message"}}`,
			`{"type":"response.output_text.delta","delta":"hel"}`,
		)
		// 直接返回：连接关闭，response.completed 永远不来
	})

	recorder := invokeAnthropicMessagesStream(t, handler)
	body := recorder.Body.String()

	if !strings.Contains(body, "hel") {
		t.Fatalf("already-streamed content should be forwarded; body=%q", body)
	}
	if !strings.Contains(body, "event: error") || !strings.Contains(body, "overloaded_error") {
		t.Fatalf("mid-stream break must emit an in-stream error event; body=%q", body)
	}
	if strings.Contains(body, "message_stop") {
		t.Fatalf("mid-stream break must not fabricate a clean message_stop ending; body=%q", body)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (post-content break is not retryable)", got)
	}
}

// TestMessagesStreamResponseFailedAfterContentEmitsErrorEvent 验证 issue #435 修复：
// 正文已下发后上游返回 response.failed，不能再走 handleFailed 翻译成 end_turn
// 干净收尾，必须发流内 error 事件让下游可感知。
func TestMessagesStreamResponseFailedAfterContentEmitsErrorEvent(t *testing.T) {
	handler, _ := newAnthropicStreamFailureTestHandler(t, func(call int32, w http.ResponseWriter) {
		writeCodexSSE(w,
			`{"type":"response.created","response":{"id":"resp_failed"}}`,
			`{"type":"response.output_item.added","item":{"type":"message"}}`,
			`{"type":"response.output_text.delta","delta":"hi"}`,
			`{"type":"response.failed","response":{"error":{"code":"server_error","message":"upstream boom"}}}`,
		)
	})

	recorder := invokeAnthropicMessagesStream(t, handler)
	body := recorder.Body.String()

	if !strings.Contains(body, "hi") {
		t.Fatalf("already-streamed content should be forwarded; body=%q", body)
	}
	if !strings.Contains(body, "event: error") {
		t.Fatalf("post-content response.failed must emit an in-stream error event; body=%q", body)
	}
	if strings.Contains(body, "message_stop") {
		t.Fatalf("post-content response.failed must not fabricate a clean message_stop; body=%q", body)
	}
}

// TestMessagesStreamPreContentBreakRetriesTransparently 验证 issue #435 修复：
// 首个真实内容帧之前的结构帧（output_item.added 等）只缓冲不落盘，
// 此窗口内上游断流仍可静默换号/重试，下游最终拿到一条完整干净的成功响应。
func TestMessagesStreamPreContentBreakRetriesTransparently(t *testing.T) {
	handler, calls := newAnthropicStreamFailureTestHandler(t, func(call int32, w http.ResponseWriter) {
		if call == 1 {
			// 第一轮：只发结构帧就断流（正文永远没来）
			writeCodexSSE(w,
				`{"type":"response.created","response":{"id":"resp_retry_1"}}`,
				`{"type":"response.output_item.added","item":{"type":"reasoning"}}`,
			)
			return
		}
		writeCodexSSE(w,
			`{"type":"response.created","response":{"id":"resp_retry_2"}}`,
			`{"type":"response.output_item.added","item":{"type":"message"}}`,
			`{"type":"response.output_text.delta","delta":"retried"}`,
			`{"type":"response.completed","response":{"id":"resp_retry_2","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
		)
	})

	recorder := invokeAnthropicMessagesStream(t, handler)
	body := recorder.Body.String()

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", recorder.Code, body)
	}
	if !strings.Contains(body, "retried") {
		t.Fatalf("retried attempt content missing; body=%q", body)
	}
	if strings.Contains(body, "event: error") {
		t.Fatalf("transparent retry must not leak an error event downstream; body=%q", body)
	}
	if got := strings.Count(body, `"type":"message_start"`); got != 1 {
		t.Fatalf("message_start count = %d, want exactly 1 (first attempt's structural frames must stay buffered); body=%q", got, body)
	}
	if !strings.Contains(body, "message_stop") {
		t.Fatalf("successful retry should end with message_stop; body=%q", body)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (break + transparent retry)", got)
	}
}

// TestMessagesEntryRejectionLogsToConsole 验证 issue #435 修复：
// 入口校验拒绝（缺 model 等）必须打控制台日志，否则"请求发不进来"在网关侧不可见。
func TestMessagesEntryRejectionLogsToConsole(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var logs bytes.Buffer
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})

	settings := &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.4"}
	store := auth.NewStore(nil, nil, settings)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.Messages(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if !strings.Contains(logs.String(), "/v1/messages 入口拒绝") {
		t.Fatalf("entry rejection must log to console; logs=%q", logs.String())
	}
}
