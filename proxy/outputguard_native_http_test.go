package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/security/outputguard"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// newNativeGuardTestStore 返回一个只包含本次用例账号的真实 auth.Store。
// 测试阈值来自 outputguard_http_chat_test.go 的合法最小配置，绝不伪造过小值。
func newNativeGuardTestStore(t *testing.T) *auth.Store {
	t.Helper()
	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      2,
		TestConcurrency:     1,
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
		RetryIntervalMS:     0,
	})
	t.Cleanup(store.Stop)
	return store
}

func newNativeGuardHandler(store *auth.Store) *Handler {
	return NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
}

// newNativeGuardGinContext 使用支持 SetWriteDeadline 的 recorder，
// 因为已提交响应头后的 guard 终态写入走 04a 的有界写超时原语。
func newNativeGuardGinContext(t *testing.T) (*gin.Context, *guardDeadlineRecorder) {
	t.Helper()
	recorder := &guardDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	c, _ := gin.CreateTestContext(recorder)
	return c, recorder
}

// selectNativeProjector 必须按真实上游协议选择投影器，而不是按下游入口路径猜测。
func TestNativeHTTP_SelectNativeProjectorDispatchesByUpstreamProtocol(t *testing.T) {
	cases := []struct {
		name            string
		protocol        string
		nativeClaude    bool
		codexResponses  bool
		wantProjector   any
		wantDescription string
	}{
		{name: "native_claude_overrides_protocol", protocol: "messages", nativeClaude: true, codexResponses: true, wantProjector: &AnthropicProjector{}, wantDescription: "AnthropicProjector"},
		{name: "codex_responses_relay", protocol: "messages", codexResponses: true, wantProjector: &ResponsesProjector{}, wantDescription: "ResponsesProjector"},
		{name: "anthropic_protocol", protocol: "anthropic", wantProjector: &AnthropicProjector{}, wantDescription: "AnthropicProjector"},
		{name: "messages_protocol", protocol: "messages", wantProjector: &AnthropicProjector{}, wantDescription: "AnthropicProjector"},
		{name: "chat_protocol", protocol: "chat", wantProjector: &ChatProjector{}, wantDescription: "ChatProjector"},
		{name: "chat_completions_protocol", protocol: "chat_completions", wantProjector: &ChatProjector{}, wantDescription: "ChatProjector"},
		{name: "responses_protocol", protocol: "responses", wantProjector: &ResponsesProjector{}, wantDescription: "ResponsesProjector"},
		{name: "gemini_protocol", protocol: "gemini", wantProjector: &GeminiProjector{}, wantDescription: "GeminiProjector"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selectNativeProjector(tc.protocol, tc.nativeClaude, tc.codexResponses)
			switch tc.wantProjector.(type) {
			case *AnthropicProjector:
				if _, ok := got.(*AnthropicProjector); !ok {
					t.Fatalf("projector = %T, want %s", got, tc.wantDescription)
				}
			case *ResponsesProjector:
				if _, ok := got.(*ResponsesProjector); !ok {
					t.Fatalf("projector = %T, want %s", got, tc.wantDescription)
				}
			case *ChatProjector:
				if _, ok := got.(*ChatProjector); !ok {
					t.Fatalf("projector = %T, want %s", got, tc.wantDescription)
				}
			case *GeminiProjector:
				if _, ok := got.(*GeminiProjector); !ok {
					t.Fatalf("projector = %T, want %s", got, tc.wantDescription)
				}
			}
		})
	}
}

// 1. Anthropic Messages 原生 Claude 透传：上游爆发重复文本时必须在下发前拦截，
// 只发送脱敏 anthropic 终态，绝不下发 message_stop 或失控内容。
func TestNativeHTTP_MessagesNativeClaudeStreamGuardTripped(t *testing.T) {
	setupOutputGuardHTTPTestEnv(t)

	repeated := strings.Repeat("A_REPEATED_CHUNK_TOKEN_1234567890_", 300) // > 8KiB, period <= 256
	var upstreamCalls atomic.Int32
	upstreamCanceled := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		payload, _ := json.Marshal(gin.H{
			"type":  "content_block_delta",
			"index": 0,
			"delta": gin.H{"type": "text_delta", "text": repeated},
		})
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: "+string(payload)+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
			upstreamCanceled <- struct{}{}
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(upstream.Close)

	store := newNativeGuardTestStore(t)
	store.AddAccount(&auth.Account{
		DBID:           1001,
		UpstreamType:   auth.UpstreamClaude,
		ClaudeAuthKind: auth.ClaudeAuthKindAPIKey,
		AccessToken:    "claude-native-guard-key",
		ClaudeBaseURL:  upstream.URL,
		Models:         []string{"claude-3-5-sonnet-20241022"},
		HealthTier:     auth.HealthTierHealthy,
		Status:         auth.StatusReady,
	})

	router := gin.New()
	router.POST("/v1/messages", newNativeGuardHandler(store).Messages)
	recorder := &guardDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-3-5-sonnet-20241022","stream":true,"messages":[{"role":"user","content":"repeat"}]}`))
	req.Header.Set("Content-Type", "application/json")

	router.ServeHTTP(recorder, req)
	body := recorder.Body.String()

	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 upstream call, got %d", got)
	}
	if !strings.Contains(body, "event: error") || !strings.Contains(body, GuardTriggeredPublicMessage) {
		t.Fatalf("expected the sanitized anthropic guard terminal, got: %s", outputGuardTestBodySummary(body))
	}
	if strings.Contains(body, "message_stop") {
		t.Fatalf("guard terminal must not be followed by a forged message_stop: %s", outputGuardTestBodySummary(body))
	}
	if strings.Contains(body, "A_REPEATED_CHUNK_TOKEN") {
		t.Fatalf("the tripped model output leaked downstream: %s", outputGuardTestBodySummary(body))
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the upstream request to be canceled when the guard trips")
	}
}

// 2. Messages → Responses SSE（Codex/Resin 上游）：Guard 必须早于 translator 与
// downstream 写入生效，命中后按 anthropic 协议返回脱敏 502，且不重试。
func TestNativeHTTP_MessagesResponsesUpstreamGuardTripped(t *testing.T) {
	setupOutputGuardHTTPTestEnv(t)

	overflow := outputGuardTestRandomText(11, int(guardHTTPTestMaxChannelBytes)+12*1024)
	var upstreamCalls atomic.Int32
	upstreamCanceled := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, outputGuardTestDelta("msg_native_messages", overflow))
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
			upstreamCanceled <- struct{}{}
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(upstream.Close)
	enableOutputGuardHTTPTestResin(t, upstream.URL)

	handler := NewHandler(newOutputGuardHTTPTestStore(t, 0), nil, &config.Config{AllowAnonymousV1: true}, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"gpt-5.5","stream":true,"messages":[{"role":"user","content":"go"}]}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Messages(c)
	body := recorder.Body.String()

	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 upstream call, got %d", got)
	}
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("expected status 502 for anthropic guard rejection, got %d, body: %s", recorder.Code, outputGuardTestBodySummary(body))
	}
	if code := decodeOutputGuardErrorCode(t, recorder.Body.Bytes()); code != outputguard.TriggeredCode {
		t.Fatalf("expected error code %q, got %q; body=%s", outputguard.TriggeredCode, code, outputGuardTestBodySummary(body))
	}
	if strings.Contains(body, "message_stop") {
		t.Fatalf("guard terminal must not be followed by a forged message_stop: %s", outputGuardTestBodySummary(body))
	}
	if strings.Contains(body, overflow[:64]) {
		t.Fatalf("the tripped model output leaked downstream: %s", outputGuardTestBodySummary(body))
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the upstream request to be canceled when the guard trips")
	}
}

// 3. Gemini Native streamGenerateContent：逐 parts append 检测，命中后下发裸
// Gemini 502 JSON，绝不伪造 finishReason STOP；非流式 generateContent 走全量
// snapshot 检测，命中返回 HTTP 502 + UNAVAILABLE。
func TestNativeHTTP_GeminiNativeStreamAndNonStreamGuardTripped(t *testing.T) {
	setupOutputGuardHTTPTestEnv(t)

	repeated := strings.Repeat("GEMINI_RUNAWAY_REPETITION_TOKEN_ABC_", 300) // > 8KiB, period <= 256
	var streamCalls, nonStreamCalls atomic.Int32
	streamCanceled := make(chan struct{}, 1)
	geminiUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			streamCalls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			flusher, _ := w.(http.Flusher)
			bad, _ := json.Marshal(gin.H{
				"candidates": []gin.H{
					{"index": 0, "content": gin.H{"role": "model", "parts": []gin.H{{"text": repeated}}}},
				},
			})
			_, _ = io.WriteString(w, "data: "+string(bad)+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			select {
			case <-r.Context().Done():
				streamCanceled <- struct{}{}
			case <-time.After(2 * time.Second):
				// 只有未被 Guard 终止时才会走到这里：写出伪造的 STOP 帧。
				_, _ = io.WriteString(w, "data: {\"candidates\":[{\"index\":0,\"finishReason\":\"STOP\"}]}\n\n")
				if flusher != nil {
					flusher.Flush()
				}
			}
			return
		}

		nonStreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		body, _ := json.Marshal(gin.H{
			"candidates": []gin.H{
				{"index": 0, "content": gin.H{"role": "model", "parts": []gin.H{{"text": repeated}}}, "finishReason": "STOP"},
			},
		})
		_, _ = w.Write(body)
	}))
	t.Cleanup(geminiUpstream.Close)

	previous := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{geminiUpstream.URL}
	t.Cleanup(func() { antigravityOAuthEndpointBases = previous })

	store := newNativeGuardTestStore(t)
	store.AddAccount(&auth.Account{
		DBID:                 2001,
		UpstreamType:         auth.UpstreamAntigravity,
		AccessToken:          "mock-ag-token",
		AntigravityProjectID: "mock-project",
		Models:               []string{"gemini-3.7-flash-tiered"},
		HealthTier:           auth.HealthTierHealthy,
		Status:               auth.StatusReady,
	})

	router := gin.New()
	router.POST("/v1beta/models/*action", newNativeGuardHandler(store).GeminiModelsAction)

	streamRecorder := httptest.NewRecorder()
	streamReq := httptest.NewRequest(http.MethodPost,
		"/v1beta/models/gemini-3.7-flash-low:streamGenerateContent?alt=sse",
		strings.NewReader(`{"contents":[{"parts":[{"text":"go"}]}]}`))
	streamReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(streamRecorder, streamReq)
	streamBody := streamRecorder.Body.String()

	if got := streamCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 stream upstream call, got %d", got)
	}
	if !strings.Contains(streamBody, "UNAVAILABLE") || !strings.Contains(streamBody, "502") {
		t.Fatalf("expected bare gemini 502 JSON error in stream, got: %s", outputGuardTestBodySummary(streamBody))
	}
	if !strings.Contains(streamBody, GuardTriggeredPublicMessage) {
		t.Fatalf("expected the sanitized public guard message, got: %s", outputGuardTestBodySummary(streamBody))
	}
	if strings.Contains(streamBody, "finishReason") {
		t.Fatalf("guard terminal must not be followed by a forged finishReason: %s", outputGuardTestBodySummary(streamBody))
	}
	if strings.Contains(streamBody, "GEMINI_RUNAWAY_REPETITION_TOKEN") {
		t.Fatalf("the tripped model output leaked downstream: %s", outputGuardTestBodySummary(streamBody))
	}
	if rec := streamRecorder.Result(); rec.StatusCode != http.StatusBadGateway {
		_ = rec.Body.Close()
		t.Fatalf("expected HTTP 502 for the tripped native gemini stream, got %d", rec.StatusCode)
	}
	select {
	case <-streamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the native gemini stream upstream to be canceled when the guard trips")
	}

	nonStreamRecorder := httptest.NewRecorder()
	nonStreamReq := httptest.NewRequest(http.MethodPost,
		"/v1beta/models/gemini-3.7-flash-low:generateContent",
		strings.NewReader(`{"contents":[{"parts":[{"text":"go"}]}]}`))
	nonStreamReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(nonStreamRecorder, nonStreamReq)

	if got := nonStreamCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 non-stream upstream call, got %d", got)
	}
	if nonStreamRecorder.Code != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502 for non-stream guard rejection, got %d (body: %s)", nonStreamRecorder.Code, outputGuardTestBodySummary(nonStreamRecorder.Body.String()))
	}
	if got := gjson.GetBytes(nonStreamRecorder.Body.Bytes(), "error.status").String(); got != "UNAVAILABLE" {
		t.Fatalf("expected gemini UNAVAILABLE error payload, got: %s", outputGuardTestBodySummary(nonStreamRecorder.Body.String()))
	}
	if strings.Contains(nonStreamRecorder.Body.String(), "GEMINI_RUNAWAY_REPETITION_TOKEN") {
		t.Fatalf("the tripped model output leaked downstream: %s", outputGuardTestBodySummary(nonStreamRecorder.Body.String()))
	}
}

// 4. Grok Native：按真实上游协议复用 Chat / Responses Projector；工具参数与
// 文本的失控内容都必须在写出前拦截，且不得再下发 [DONE] 或 response.completed。
func TestNativeHTTP_GrokNativeProjectorDispatchGuardTripped(t *testing.T) {
	setupOutputGuardHTTPTestEnv(t)

	chatPayload := strings.Repeat(`{"key":"runaway_tool_argument_val"}`, 250) // > 8KiB, period <= 256
	chatUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		flusher, _ := w.(http.Flusher)
		frame, _ := json.Marshal(gin.H{
			"id": "chatcmpl-grok-guard-1",
			"choices": []gin.H{
				{
					"index": 0,
					"delta": gin.H{
						"tool_calls": []gin.H{
							{"index": 0, "id": "call_guard_1", "function": gin.H{"name": "write", "arguments": chatPayload}},
						},
					},
				},
			},
		})
		_, _ = io.WriteString(w, "data: "+string(frame)+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(chatUpstream.Close)

	chatCtx, chatRecorder := newNativeGuardGinContext(t)
	chatResp, err := http.Get(chatUpstream.URL)
	if err != nil {
		t.Fatalf("http.Get chat upstream: %v", err)
	}
	_, chatOutcome, _, _ := forwardGrokNativeResponseTo(
		context.Background(), chatCtx, chatResp, GrokProtocolChatCompletions, true, time.Now(), func() {}, nil, nil,
	)
	if !chatOutcome.guardTripped || chatOutcome.logStatusCode != http.StatusBadGateway {
		t.Fatalf("expected chat guard outcome with 502, got: %+v", chatOutcome)
	}
	chatBody := chatRecorder.Body.String()
	if !strings.Contains(chatBody, GuardTriggeredPublicCode) {
		t.Fatalf("expected sanitized chat guard terminal, got: %s", outputGuardTestBodySummary(chatBody))
	}
	if strings.Contains(chatBody, "[DONE]") {
		t.Fatalf("tripped chat stream must not contain [DONE]: %s", outputGuardTestBodySummary(chatBody))
	}
	if strings.Contains(chatBody, "runaway_tool_argument_val") {
		t.Fatalf("the tripped tool arguments leaked downstream: %s", outputGuardTestBodySummary(chatBody))
	}

	responsesPayload := strings.Repeat("RESPONSES_RUNAWAY_TEXT_TOKEN_", 400) // > 8KiB, period <= 256
	var responsesCalls atomic.Int32
	responsesUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		responsesCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, outputGuardTestDelta("msg_grok_native", responsesPayload))
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
			_, _ = io.WriteString(w, outputGuardTestEvent(map[string]any{
				"type":     "response.completed",
				"response": map[string]any{"id": "resp_grok_native", "status": "completed"},
			}))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(responsesUpstream.Close)

	responsesCtx, responsesRecorder := newNativeGuardGinContext(t)
	responsesCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"grok-4.5","stream":true}`))
	responsesResp, err := http.Get(responsesUpstream.URL)
	if err != nil {
		t.Fatalf("http.Get responses upstream: %v", err)
	}
	_, responsesOutcome, _, _ := forwardGrokNativeResponseTo(
		context.Background(), responsesCtx, responsesResp, GrokProtocolResponses, true, time.Now(), func() {}, nil, nil,
	)
	if !responsesOutcome.guardTripped || responsesOutcome.logStatusCode != http.StatusBadGateway {
		t.Fatalf("expected responses guard outcome with 502, got: %+v", responsesOutcome)
	}
	responsesBody := responsesRecorder.Body.String()
	if !strings.Contains(responsesBody, GuardTriggeredPublicCode) {
		t.Fatalf("expected sanitized responses guard terminal, got: %s", outputGuardTestBodySummary(responsesBody))
	}
	if strings.Contains(responsesBody, "RESPONSES_RUNAWAY_TEXT_TOKEN") {
		t.Fatalf("the tripped response text leaked downstream: %s", outputGuardTestBodySummary(responsesBody))
	}
	if strings.Contains(responsesBody, "response.completed") {
		t.Fatalf("tripped responses stream must not complete normally: %s", outputGuardTestBodySummary(responsesBody))
	}
	if got := responsesCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 responses upstream call, got %d", got)
	}
}

// 5. 冷启动门禁：配置加载失败（lastSyncError 已记录、未就绪）时，Messages 与
// Gemini Native 都必须 503 强阻断，不得进入账号调度。
func TestNativeHTTP_ReadinessGate503(t *testing.T) {
	ResetOutputGuardStateForTest()
	t.Cleanup(ResetOutputGuardStateForTest)
	recordOutputGuardConfigSyncError(context.DeadlineExceeded)

	handler := NewHandler(newNativeGuardTestStore(t), nil, &config.Config{AllowAnonymousV1: true}, nil)

	router := gin.New()
	router.POST("/v1/messages", handler.Messages)
	router.POST("/v1beta/models/*action", handler.GeminiModelsAction)

	tests := []struct {
		name string
		path string
		body string
	}{
		{
			name: "anthropic_messages",
			path: "/v1/messages",
			body: `{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "gemini_stream_generate_content",
			path: "/v1beta/models/gemini-3.7-flash-low:streamGenerateContent?alt=sse",
			body: `{"contents":[{"parts":[{"text":"hi"}]}]}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")

			router.ServeHTTP(recorder, req)

			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected status 503 while guard is not ready, got %d; body=%s", recorder.Code, outputGuardTestBodySummary(recorder.Body.String()))
			}
			if !strings.Contains(recorder.Body.String(), "output guard settings") {
				t.Fatalf("expected the readiness gate message, got: %s", outputGuardTestBodySummary(recorder.Body.String()))
			}
		})
	}
}
