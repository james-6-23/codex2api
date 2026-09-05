package proxy

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
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

func TestSyncAnthropicUsageStateDispatchesByProvider(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	claude := &auth.Account{DBID: 101, UpstreamType: auth.UpstreamClaude, AccessToken: "claude-token"}
	claudeResp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header)}
	claudeResp.Header.Set("anthropic-ratelimit-unified-5h-utilization", "0.42")
	claudeResp.Header.Set("anthropic-ratelimit-unified-5h-reset", "4102444800")
	syncAnthropicUsageStateForAccount(store, claude, claudeResp)
	if got := claude.UsagePercent5h; got != 42 {
		t.Fatalf("Claude usage = %v, want 42", got)
	}

	codex := &auth.Account{DBID: 102, UpstreamType: auth.UpstreamOpenAIResponses, AccessToken: "codex-token"}
	codexResp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header)}
	codexResp.Header.Set("x-codex-primary-used-percent", "37")
	codexResp.Header.Set("x-codex-primary-window-minutes", "300")
	codexResp.Header.Set("x-codex-primary-reset-after-seconds", "3600")
	syncAnthropicUsageStateForAccount(store, codex, codexResp)
	if got := codex.UsagePercent5h; got != 37 {
		t.Fatalf("Codex usage = %v, want 37", got)
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

func TestClaudeNativeFailureUsesProviderSpecificFallbackMessage(t *testing.T) {
	account := &auth.Account{UpstreamType: auth.UpstreamClaude}
	outcome := normalizeNativeFailureMessageForAccount(account, streamOutcome{failureMessage: "Grok upstream stream failed"})
	if outcome.failureMessage != "Claude upstream stream failed" {
		t.Fatalf("Claude native fallback message = %q", outcome.failureMessage)
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

func TestMessagesResponseFailedCyberPolicyEntersUnifiedAuditAndCandidateQueue(t *testing.T) {
	handler, _ := newAnthropicStreamFailureTestHandler(t, func(call int32, w http.ResponseWriter) {
		writeCodexSSE(w,
			`{"type":"response.created","response":{"id":"resp_cyber"}}`,
			`{"type":"response.failed","response":{"error":{"code":"cyber_policy","message":"cyber security risk detected"}}}`,
		)
	})
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "messages-cyber.db"))
	if err != nil {
		t.Fatalf("database.New(sqlite): %v", err)
	}
	defer db.Close()
	handler.db = db
	cfg := promptfilter.DefaultConfig()
	cfg.Enabled = true
	cfg.Mode = promptfilter.ModeBlock
	handler.store.SetPromptFilterConfig(cfg)

	_ = invokeAnthropicMessagesStream(t, handler)
	waitPromptFilterAuditIdle(t, db)
	incidents, incidentTotal, err := db.ListPromptPolicyIncidentsPage(context.Background(), database.PromptPolicyIncidentQuery{Page: 1, PageSize: 10})
	if err != nil || incidentTotal == 0 || len(incidents) == 0 || incidents[0].Endpoint != "/v1/messages" || incidents[0].Transport != "sse" {
		t.Fatalf("messages cyber_policy incident total=%d items=%#v err=%v", incidentTotal, incidents, err)
	}
	assertCyberUsageIncidentLinks(t, db, "/v1/messages")
	candidates, total, err := db.ListPromptRuleCandidates(context.Background(), database.PromptRuleCandidateQuery{Status: database.PromptRuleCandidateStatusPending})
	if err != nil || total != 1 || len(candidates) != 1 || candidates[0].Kind != database.PromptRuleCandidateKindEvidence {
		t.Fatalf("messages candidate total=%d items=%#v err=%v", total, candidates, err)
	}
}

// shortenDownstreamSSEKeepalive 把下游保活间隔压到毫秒级，让处理器级测试
// 能在上游几十毫秒的静默里观察到心跳；测试结束恢复默认值。
func shortenDownstreamSSEKeepalive(t *testing.T) {
	t.Helper()
	previousInterval := downstreamSSEKeepaliveInterval
	t.Cleanup(func() { downstreamSSEKeepaliveInterval = previousInterval })
	downstreamSSEKeepaliveInterval = 5 * time.Millisecond
}

// TestMessagesStreamKeepsDownstreamAliveDuringUpstreamSilence 验证 issue #623 修复：
// 首个内容帧之后上游静默（长推理/等工具边界）期间，/v1/messages 翻译流要像
// /v1/responses 一样定期写 SSE 注释刷新下游 idle timer，且注释不能插进事件中间。
func TestMessagesStreamKeepsDownstreamAliveDuringUpstreamSilence(t *testing.T) {
	shortenDownstreamSSEKeepalive(t)
	handler, calls := newAnthropicStreamFailureTestHandler(t, func(call int32, w http.ResponseWriter) {
		writeCodexSSE(w,
			`{"type":"response.created","response":{"id":"resp_silent"}}`,
			`{"type":"response.output_item.added","item":{"type":"message"}}`,
			`{"type":"response.output_text.delta","delta":"started"}`,
		)
		time.Sleep(40 * time.Millisecond)
		writeCodexSSE(w,
			`{"type":"response.output_text.delta","delta":"-resumed"}`,
			`{"type":"response.completed","response":{"id":"resp_silent","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
		)
	})

	recorder := invokeAnthropicMessagesStream(t, handler)
	body := recorder.Body.String()

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", recorder.Code, body)
	}
	firstContent := strings.Index(body, "started")
	keepalive := strings.Index(body, downstreamSSEKeepaliveComment)
	resumed := strings.Index(body, "-resumed")
	if firstContent < 0 || keepalive < 0 || resumed < 0 {
		t.Fatalf("stream must carry first content, a keepalive comment and the resumed content; body=%q", body)
	}
	if keepalive < firstContent || keepalive > resumed {
		t.Fatalf("keepalive must land inside the upstream silence window (after %d, before %d), got %d; body=%q", firstContent, resumed, keepalive, body)
	}
	if !strings.Contains(body, "message_stop") {
		t.Fatalf("stream should still end cleanly; body=%q", body)
	}
	// 注释必须独占一个 SSE 帧（以空行分隔），不能和 event/data 行混在同一帧里。
	for frame := range strings.SplitSeq(body, "\n\n") {
		if strings.Contains(frame, ": keepalive") && strings.TrimSpace(frame) != ": keepalive" {
			t.Fatalf("keepalive comment interleaved with an SSE frame: %q", frame)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}

// TestMessagesStreamPreContentBreakRetriesTransparently 验证 issue #435 修复：
// 首个真实内容帧之前的结构帧（output_item.added 等）只缓冲不落盘，
// 此窗口内上游断流仍可静默换号/重试，下游最终拿到一条完整干净的成功响应。
// 第一轮静默时间刻意超过下游保活间隔（issue #623）：首字前绝不能写注释，
// 否则 200 提前落盘，透明重试窗口被心跳自己关掉。
func TestMessagesStreamPreContentBreakRetriesTransparently(t *testing.T) {
	shortenDownstreamSSEKeepalive(t)
	handler, calls := newAnthropicStreamFailureTestHandler(t, func(call int32, w http.ResponseWriter) {
		if call == 1 {
			// 第一轮：只发结构帧、静默超过心跳间隔后断流（正文永远没来）
			writeCodexSSE(w,
				`{"type":"response.created","response":{"id":"resp_retry_1"}}`,
				`{"type":"response.output_item.added","item":{"type":"reasoning"}}`,
			)
			time.Sleep(40 * time.Millisecond)
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
	// 第一轮静默期间心跳 ticker 已多次触发，但首字前不得写出任何字节。
	if keepalive := strings.Index(body, downstreamSSEKeepaliveComment); keepalive >= 0 && keepalive < strings.Index(body, "retried") {
		t.Fatalf("keepalive comment must not be written before the first real content; body=%q", body)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (break + transparent retry)", got)
	}
}

func TestMessagesCatchAllDiscardsOutputFromFailedAttempt(t *testing.T) {
	enableCatchAllContinuousRetry(t)
	handler, calls := newAnthropicStreamFailureTestHandler(t, func(call int32, w http.ResponseWriter) {
		if call == 1 {
			writeCodexSSE(w,
				`{"type":"response.created","response":{"id":"resp_failed"}}`,
				`{"type":"response.output_item.added","item":{"type":"message"}}`,
				`{"type":"response.output_text.delta","delta":"failed-message-partial"}`,
				`{"type":"response.failed","response":{"status":"failed","status_code":503,"error":{"code":"server_error","message":"must stay upstream"}}}`,
			)
			return
		}
		writeCodexSSE(w,
			`{"type":"response.created","response":{"id":"resp_success"}}`,
			`{"type":"response.output_item.added","item":{"type":"message"}}`,
			`{"type":"response.output_text.delta","delta":"successful-message"}`,
			`{"type":"response.completed","response":{"id":"resp_success","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
		)
	})

	recorder := invokeAnthropicMessagesStream(t, handler)
	body := recorder.Body.String()
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2; body=%q", got, body)
	}
	if !strings.Contains(body, "successful-message") || !strings.Contains(body, "message_stop") {
		t.Fatalf("successful Messages replay missing: %q", body)
	}
	if strings.Contains(body, "failed-message-partial") || strings.Contains(body, "must stay upstream") || strings.Contains(body, "event: error") {
		t.Fatalf("failed Messages attempt leaked downstream: %q", body)
	}
}

func TestMessagesCatchAllDiscardsExplicitErrorEventAfterPartialOutput(t *testing.T) {
	enableCatchAllContinuousRetry(t)
	handler, calls := newAnthropicStreamFailureTestHandler(t, func(call int32, w http.ResponseWriter) {
		if call == 1 {
			_, _ = io.WriteString(w,
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"message\"}}\n\n"+
					"data: {\"type\":\"response.output_text.delta\",\"delta\":\"failed-message-error-partial\"}\n\n"+
					"event: error\ndata: {\"error\":{\"code\":\"future_error\",\"message\":\"must stay upstream\"}}\n\n")
			return
		}
		writeCodexSSE(w,
			`{"type":"response.output_item.added","item":{"type":"message"}}`,
			`{"type":"response.output_text.delta","delta":"successful-message-after-error"}`,
			`{"type":"response.completed","response":{"id":"resp_success","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
		)
	})

	recorder := invokeAnthropicMessagesStream(t, handler)
	body := recorder.Body.String()
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2; body=%q", got, body)
	}
	if !strings.Contains(body, "successful-message-after-error") || !strings.Contains(body, "message_stop") {
		t.Fatalf("successful Messages replay missing: %q", body)
	}
	if strings.Contains(body, "failed-message-error-partial") || strings.Contains(body, "must stay upstream") || strings.Contains(body, "future_error") || strings.Contains(body, "event: error") {
		t.Fatalf("explicit Messages error event attempt leaked downstream: %q", body)
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

func TestMessagesUpstreamErrorBodyIsBoundedAndUnstructuredBodyIsNotLeaked(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousResin := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(previousResin) })

	secret := "secret-provider-token-should-not-leak"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "<html><body>"+secret+strings.Repeat("x", upstreamErrorBodyReadMaxBytes+1024)+"</body></html>")
	}))
	defer upstream.Close()
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, MaxRetries: 0, MaxRateLimitRetries: 0})
	defer store.Stop()
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-4-6","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.Messages(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), secret) || strings.Contains(strings.ToLower(recorder.Body.String()), "<html") {
		t.Fatalf("unstructured upstream error leaked: %s", recorder.Body.String())
	}
	if got := gjson.GetBytes(recorder.Body.Bytes(), "error.message").String(); !strings.Contains(got, "Upstream error response exceeded the safe read limit") {
		t.Fatalf("message = %q; body=%s", got, recorder.Body.String())
	}
}

func TestMessagesUpstreamHTMLMessageIsNotLeaked(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousResin := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(previousResin) })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "<html>request-id=private-123</html>")
	}))
	defer upstream.Close()
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, MaxRetries: 0, MaxRateLimitRetries: 0})
	defer store.Stop()
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-4-6","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.Messages(ctx)
	if recorder.Code != http.StatusBadRequest || strings.Contains(recorder.Body.String(), "private-123") {
		t.Fatalf("status/body = %d %s", recorder.Code, recorder.Body.String())
	}
	if got := gjson.GetBytes(recorder.Body.Bytes(), "error.message").String(); got != "Upstream returned status 400" {
		t.Fatalf("message = %q; body=%s", got, recorder.Body.String())
	}
}
