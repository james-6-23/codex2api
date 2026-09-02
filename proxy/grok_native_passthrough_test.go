package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestForwardGrokNativeNonStreamPreservesJSONAndFiltersHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	payload := `{"id":"chatcmpl_native","future":{"keep":true},"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{
		"Content-Type": []string{"application/json"}, "Set-Cookie": []string{"secret=1"},
		grokNativeRouteHeader: []string{"1"}, "X-Request-Id": []string{"req-safe"},
	}, Body: io.NopCloser(strings.NewReader(payload))}

	usage, outcome, wrote, firstTokenMs := forwardGrokNativeResponse(ctx, resp, GrokProtocolChatCompletions, false, time.Now(), nil)
	if outcome.logStatusCode != http.StatusOK || !wrote || usage == nil || usage.TotalTokens != 3 {
		t.Fatalf("usage/outcome/wrote = %#v %#v %v", usage, outcome, wrote)
	}
	if firstTokenMs != 0 {
		t.Fatalf("non-stream first token = %d, want 0", firstTokenMs)
	}
	if recorder.Body.String() != payload || !gjson.GetBytes(recorder.Body.Bytes(), "future.keep").Bool() {
		t.Fatalf("native JSON was not byte-preserved: %s", recorder.Body.String())
	}
	if recorder.Header().Get("Set-Cookie") != "" || recorder.Header().Get(grokNativeRouteHeader) != "" || recorder.Header().Get("X-Request-Id") != "req-safe" {
		t.Fatalf("response header filtering failed: %#v", recorder.Header())
	}
}

func TestCopyClaudeNativeResponseHeadersPreservesUsageMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	header := http.Header{
		"anthropic-ratelimit-unified-5h-utilization": []string{"0.42"},
		"anthropic-ratelimit-unified-5h-reset":       []string{"4102444800"},
		"anthropic-ratelimit-unified-status":         []string{"allowed"},
		"anthropic-version":                          []string{"2023-06-01"},
		"Authorization":                              []string{"Bearer secret"},
		"Set-Cookie":                                 []string{"secret=1"},
		"X-Leak":                                     []string{"nope"},
	}
	copyClaudeNativeResponseHeaders(ctx, header)
	if recorder.Header().Get("anthropic-ratelimit-unified-5h-utilization") != "0.42" || recorder.Header().Get("anthropic-version") != "2023-06-01" {
		t.Fatalf("Claude usage headers were not forwarded: %#v", recorder.Header())
	}
	if recorder.Header().Get("Authorization") != "" || recorder.Header().Get("Set-Cookie") != "" || recorder.Header().Get("X-Leak") != "" {
		t.Fatalf("sensitive/unallowlisted headers leaked: %#v", recorder.Header())
	}
}

func TestProtocolNonStreamFailureRejectsPseudoSuccessPayloads(t *testing.T) {
	tests := []struct {
		name     string
		protocol GrokProtocol
		payload  string
		failed   bool
	}{
		{name: "responses success", protocol: GrokProtocolResponses, payload: `{"id":"resp_ok","status":"completed"}`},
		{name: "responses missing terminal status", protocol: GrokProtocolResponses, payload: `{"id":"resp_unknown"}`, failed: true},
		{name: "responses error object", protocol: GrokProtocolResponses, payload: `{"error":{"code":"rate_limited"}}`, failed: true},
		{name: "responses failed status", protocol: GrokProtocolResponses, payload: `{"id":"resp_bad","status":"failed"}`, failed: true},
		{name: "responses failed event", protocol: GrokProtocolResponses, payload: `{"type":"response.failed","response":{"status":"failed"}}`, failed: true},
		{name: "chat error", protocol: GrokProtocolChatCompletions, payload: `{"type":"error","error":{"message":"busy"}}`, failed: true},
		{name: "chat missing choices", protocol: GrokProtocolChatCompletions, payload: `{"id":"chat_unknown"}`, failed: true},
		{name: "messages missing message type", protocol: GrokProtocolMessages, payload: `{"id":"message_unknown"}`, failed: true},
		{name: "invalid JSON", protocol: GrokProtocolResponses, payload: `not-json`, failed: true},
		{name: "empty", protocol: GrokProtocolResponses, payload: ``, failed: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			outcome, failed := protocolNonStreamFailure(tc.protocol, []byte(tc.payload))
			if failed != tc.failed {
				t.Fatalf("failed = %v, want %v; outcome=%#v", failed, tc.failed, outcome)
			}
			if failed && (outcome.logStatusCode == http.StatusOK || strings.TrimSpace(outcome.failureMessage) == "") {
				t.Fatalf("pseudo-success failure was not classified: %#v", outcome)
			}
		})
	}
}

func TestForwardGrokNativeStreamRequiresTerminalAndEmitsProtocolError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"))}

	_, outcome, wrote, _ := forwardGrokNativeResponse(ctx, resp, GrokProtocolMessages, true, time.Now(), nil)
	if !wrote || outcome.logStatusCode != logStatusUpstreamStreamBreak {
		t.Fatalf("outcome/wrote = %#v %v", outcome, wrote)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"text":"hi"`) || !strings.Contains(body, "event: error") || !strings.Contains(body, ErrorCodeUpstreamStreamBreak) || strings.Contains(body, "message_stop") {
		t.Fatalf("broken native Messages stream was disguised as success: %s", body)
	}
}

func TestForwardGrokNativeFailureBeforeVisibleOutputWritesNothing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n" +
			"data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n"))}

	_, outcome, wrote, _ := forwardGrokNativeResponse(ctx, resp, GrokProtocolChatCompletions, true, time.Now(), nil)
	if wrote || recorder.Body.Len() != 0 || !outcome.penalize {
		t.Fatalf("pre-output failure leaked downstream: outcome=%#v wrote=%v body=%q", outcome, wrote, recorder.Body.String())
	}
}

func TestForwardGrokNativePrivateAttemptDoesNotPublishFailedHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"X-Request-Id": []string{"failed-attempt-request"},
			"Retry-After":  []string{"60"},
		},
		Body: io.NopCloser(strings.NewReader(
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"private\"}\n\n" +
				"event: error\ndata: {\"error\":{\"code\":\"future_failure\"}}\n\n",
		)),
	}
	attempt := newContinuousRetryStreamAttempt(true, recorder, recorder)
	t.Cleanup(func() { _ = attempt.Close() })

	_, outcome, _, _ := forwardGrokNativeResponseTo(
		ctx, resp, GrokProtocolResponses, true, time.Now(), nil,
		attempt.writerOr(recorder), attempt.flusherOr(recorder),
	)
	if outcome.logStatusCode == http.StatusOK {
		t.Fatalf("failed attempt outcome = %#v", outcome)
	}
	if got := recorder.Header().Get("X-Request-Id"); got != "" {
		t.Fatalf("failed attempt request id leaked downstream: %q", got)
	}
	if got := recorder.Header().Get("Retry-After"); got != "" {
		t.Fatalf("failed attempt retry header leaked downstream: %q", got)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("failed private attempt body leaked downstream: %q", recorder.Body.String())
	}
}

func TestForwardGrokNativeTypelessEventErrorStaysPrivateAcrossProtocols(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name     string
		protocol GrokProtocol
		path     string
	}{
		{name: "responses", protocol: GrokProtocolResponses, path: "/v1/responses"},
		{name: "chat", protocol: GrokProtocolChatCompletions, path: "/v1/chat/completions"},
		{name: "messages", protocol: GrokProtocolMessages, path: "/v1/messages"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, tc.path, nil)
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(
					"event: error\n" +
						"data: {\"status_code\":503,\"error\":{\"code\":\"server_error\",\"message\":\"busy\"}}\n\n")),
			}

			_, outcome, wrote, _ := forwardGrokNativeResponse(ctx, resp, tc.protocol, true, time.Now(), nil)

			if wrote || recorder.Body.Len() != 0 {
				t.Fatalf("typeless event:error leaked downstream: outcome=%#v body=%q", outcome, recorder.Body.String())
			}
			if outcome.logStatusCode != http.StatusServiceUnavailable || outcome.failureKind == "" || len(outcome.failurePayload) == 0 {
				t.Fatalf("typeless event:error was not classified as an upstream failure: %#v", outcome)
			}
		})
	}
}

func TestForwardGrokNativeFailureBeforeVisibleOutputReturnsProtocolHTTPError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name     string
		protocol GrokProtocol
		payload  string
		path     string
		wantType string
	}{
		{name: "responses", protocol: GrokProtocolResponses, path: "/v1/responses", payload: `{"type":"response.failed","response":{"status":"failed","status_code":400,"error":{"type":"invalid_request_error","message":"bad input"}}}`, wantType: "upstream_error"},
		{name: "chat", protocol: GrokProtocolChatCompletions, path: "/v1/chat/completions", payload: `{"type":"error","status_code":429,"error":{"type":"rate_limit_error","message":"busy"}}`, wantType: "upstream_error"},
		{name: "messages", protocol: GrokProtocolMessages, path: "/v1/messages", payload: `{"type":"error","status_code":400,"error":{"type":"invalid_request_error","message":"bad message"}}`, wantType: "invalid_request_error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, tc.path, nil)
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + tc.payload + "\n\n"))}
			_, outcome, wrote, _ := forwardGrokNativeResponse(ctx, resp, tc.protocol, true, time.Now(), nil)
			if wrote || recorder.Body.Len() != 0 {
				t.Fatalf("pre-output failure leaked stream: outcome=%#v body=%q", outcome, recorder.Body.String())
			}
			(&Handler{}).sendGrokNativeHTTPError(ctx, tc.protocol, outcome)
			if recorder.Code != safeGrokNativeHTTPStatus(outcome.logStatusCode) || !strings.Contains(recorder.Body.String(), tc.wantType) {
				t.Fatalf("status/body = %d %s; outcome=%#v", recorder.Code, recorder.Body.String(), outcome)
			}
		})
	}
}

func TestSendGrokNativeHTTPErrorAfterKeepaliveUsesProtocolEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name       string
		protocol   GrokProtocol
		path       string
		wantMarker string
	}{
		{name: "responses", protocol: GrokProtocolResponses, path: "/v1/responses", wantMarker: `"type":"response.failed"`},
		{name: "chat", protocol: GrokProtocolChatCompletions, path: "/v1/chat/completions", wantMarker: `"type":"upstream_error"`},
		{name: "messages", protocol: GrokProtocolMessages, path: "/v1/messages", wantMarker: "event: error\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, tc.path, nil)
			stop := installContinuousRetrySSEKeepalive(ctx, true, "text/event-stream")
			defer stop()

			keepalive := continuousRetryKeepaliveForContext(ctx.Request.Context())
			keepalive.Activate()
			requestKeepalive := keepalive.(*requestContinuousRetryKeepalive)
			requestKeepalive.last = time.Time{}
			if err := keepalive.Keepalive(); err != nil {
				t.Fatalf("write keepalive: %v", err)
			}
			(&Handler{}).sendGrokNativeHTTPError(ctx, tc.protocol, streamOutcome{
				logStatusCode:  http.StatusServiceUnavailable,
				failureMessage: "upstream busy",
			})

			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), tc.wantMarker) {
				t.Fatalf("committed protocol error = status %d body %q", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestForwardGrokNativeStreamPreservesRawFramesDoneAndTrailingUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	raw := ": keep-alive\r\n" +
		"id: evt-1\r\nretry: 1500\r\nevent: chunk\r\ndata: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\r\n\r\n" +
		"event: chunk\r\ndata: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\r\n\r\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\r\n\r\n" +
		"data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}\r\n\r\n" +
		"data: [DONE]\r\n\r\n"
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(raw))}

	usage, outcome, wrote, firstTokenMs := forwardGrokNativeResponse(ctx, resp, GrokProtocolChatCompletions, true, time.Now().Add(-10*time.Millisecond), nil)
	if !wrote || outcome.logStatusCode != http.StatusOK || usage == nil || usage.TotalTokens != 9 || firstTokenMs < 1 {
		t.Fatalf("usage/outcome/wrote/ttft = %#v %#v %v %d", usage, outcome, wrote, firstTokenMs)
	}
	if !bytes.Equal(recorder.Body.Bytes(), []byte(raw)) {
		t.Fatalf("native SSE changed\n got: %q\nwant: %q", recorder.Body.String(), raw)
	}
}

func TestForwardGrokNativeMessagesPreservesEventAndMultilineData(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	// 真实 Anthropic 流形状：input_tokens 在 message_start 的 message.usage 下，
	// output_tokens 在 message_delta 的顶层 usage 下，message_stop 不带 usage。
	raw := "event: message_start\nid: a\ndata: {\"type\":\"message_start\",\ndata: \"message\":{\"id\":\"m1\",\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(raw))}

	usage, outcome, wrote, _ := forwardGrokNativeResponse(ctx, resp, GrokProtocolMessages, true, time.Now(), nil)
	if !wrote || outcome.logStatusCode != http.StatusOK || usage == nil || usage.TotalTokens != 3 {
		t.Fatalf("usage/outcome/wrote = %#v %#v %v", usage, outcome, wrote)
	}
	if recorder.Body.String() != raw {
		t.Fatalf("Messages SSE changed\n got: %q\nwant: %q", recorder.Body.String(), raw)
	}
}

func TestHoldGrokNativePreOutputResponsesOnlyBuffersLifecycle(t *testing.T) {
	created := parseRawGrokSSEFrame([]byte("data: {\"type\":\"response.created\"}\n\n"))
	itemAdded := parseRawGrokSSEFrame([]byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\"}}\n\n"))
	text := parseRawGrokSSEFrame([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n"))
	keepAlive := parseRawGrokSSEFrame([]byte(": keep-alive\r\n\r\n"))
	roleOnly := parseRawGrokSSEFrame([]byte("data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))

	if !holdGrokNativePreOutput(GrokProtocolResponses, created, false, false, false) {
		t.Fatal("Responses created should stay buffered for silent retry")
	}
	if holdGrokNativePreOutput(GrokProtocolResponses, itemAdded, false, false, false) {
		t.Fatal("Responses structure frames must flush before first text")
	}
	if holdGrokNativePreOutput(GrokProtocolResponses, keepAlive, false, false, false) {
		t.Fatal("Responses keep-alives must flush so the client sees progress")
	}
	if holdGrokNativePreOutput(GrokProtocolResponses, text, false, false, true) {
		t.Fatal("visible text must never be held")
	}
	if !holdGrokNativePreOutput(GrokProtocolChatCompletions, roleOnly, false, false, false) {
		t.Fatal("Chat role-only frames must stay buffered until first visible delta")
	}
}

func TestForwardGrokNativeFirstVisibleStopsTTFTGuard(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(
		"data: {\"type\":\"response.created\"}\n\n" +
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"))}
	callbacks := 0
	usage, outcome, wrote, _ := forwardGrokNativeResponse(ctx, resp, GrokProtocolResponses, true, time.Now(), func() { callbacks++ })
	if !wrote || outcome.logStatusCode != http.StatusOK || callbacks != 1 {
		t.Fatalf("wrote/outcome/callbacks = %v %#v %d", wrote, outcome, callbacks)
	}
	// Responses 流式 usage 位于 response.completed 的 response.usage 下,必须被提取。
	if usage == nil || usage.TotalTokens != 2 {
		t.Fatalf("streaming Responses usage lost: %#v", usage)
	}
}

func TestReadRawGrokSSEFramesPreservesBoundariesAndCommentOnlyFrame(t *testing.T) {
	raw := []byte(":one\r\n\r\nevent: x\r\ndata: first\r\ndata: second\r\n\r\ndata: [DONE]\r\n\r\n")
	reader := &oneByteReader{data: raw}
	var frames []rawGrokSSEFrame
	if err := readRawGrokSSEFrames(reader, func(frame rawGrokSSEFrame) bool {
		copy := rawGrokSSEFrame{Raw: bytes.Clone(frame.Raw), Data: bytes.Clone(frame.Data), Event: frame.Event, HasData: frame.HasData, Done: frame.Done}
		frames = append(frames, copy)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if len(frames) != 3 || frames[0].HasData || frames[1].Event != "x" || string(frames[1].Data) != "first\nsecond" || !frames[2].Done {
		t.Fatalf("frames = %#v", frames)
	}
	var joined []byte
	for _, frame := range frames {
		joined = append(joined, frame.Raw...)
	}
	if !bytes.Equal(joined, raw) {
		t.Fatalf("joined frames = %q, want %q", joined, raw)
	}
}

type oneByteReader struct{ data []byte }

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	p[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}
