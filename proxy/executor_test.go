package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

type maxChunkReader struct {
	reader io.Reader
	size   int
}

func (r *maxChunkReader) Read(p []byte) (int, error) {
	if len(p) > r.size {
		p = p[:r.size]
	}
	return r.reader.Read(p)
}

func TestReadSSEStream_MergesMultilineData(t *testing.T) {
	input := strings.NewReader("data: {\"type\":\"response.output_text.delta\",\n" +
		"data: \"delta\":\"hello\"}\n\n" +
		"data: [DONE]\n\n")

	var events []string
	err := ReadSSEStream(input, func(data []byte) bool {
		events = append(events, string(data))
		return true
	})
	if err != nil {
		t.Fatalf("ReadSSEStream returned error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	want := "{\"type\":\"response.output_text.delta\",\n\"delta\":\"hello\"}"
	if events[0] != want {
		t.Fatalf("unexpected merged event: got %q want %q", events[0], want)
	}
}

func TestReadSSEStreamWithEventPreservesEventName(t *testing.T) {
	input := strings.NewReader("event: error\n" +
		"data: {\"error\":{\"status_code\":403,\"code\":\"forbidden\"}}\n\n")

	var gotEvent string
	var gotData string
	err := ReadSSEStreamWithEvent(input, func(event string, data []byte) bool {
		gotEvent = event
		gotData = string(data)
		return true
	})
	if err != nil {
		t.Fatalf("ReadSSEStreamWithEvent returned error: %v", err)
	}
	if gotEvent != "error" {
		t.Fatalf("event = %q, want error", gotEvent)
	}
	if gotData != `{"error":{"status_code":403,"code":"forbidden"}}` {
		t.Fatalf("data = %q", gotData)
	}
}

func TestReadSSEStreamPreservesEventsAcrossReadBoundaries(t *testing.T) {
	const eventCount = 2048

	var input strings.Builder
	for i := 0; i < eventCount; i++ {
		input.WriteString("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
	}
	input.WriteString("data: [DONE]\n\n")

	got := 0
	reader := &maxChunkReader{reader: strings.NewReader(input.String()), size: 37}
	err := ReadSSEStream(reader, func(data []byte) bool {
		if string(data) != `{"type":"response.output_text.delta","delta":"hello"}` {
			t.Fatalf("unexpected event %d: %q", got, data)
		}
		got++
		return true
	})
	if err != nil {
		t.Fatalf("ReadSSEStream returned error: %v", err)
	}
	if got != eventCount {
		t.Fatalf("event count = %d, want %d", got, eventCount)
	}
}

func TestClassifyStreamOutcome(t *testing.T) {
	tests := []struct {
		name         string
		ctxErr       error
		readErr      error
		writeErr     error
		gotTerminal  bool
		wantStatus   int
		wantKind     string
		wantPenalize bool
	}{
		{
			name:        "terminal success",
			gotTerminal: true,
			wantStatus:  200,
		},
		{
			name:         "client canceled",
			ctxErr:       context.Canceled,
			wantStatus:   logStatusClientClosed,
			wantPenalize: false,
		},
		{
			name:         "upstream timeout",
			readErr:      errors.New("read timeout"),
			wantStatus:   logStatusUpstreamStreamBreak,
			wantKind:     "timeout",
			wantPenalize: true,
		},
		{
			name:         "websocket message too big",
			readErr:      errors.New("websocket read error: websocket: close 1009 (message too big)"),
			wantStatus:   logStatusUpstreamStreamBreak,
			wantKind:     upstreamErrorKindMessageTooBig,
			wantPenalize: false,
		},
		{
			name:         "upstream early eof",
			wantStatus:   logStatusUpstreamStreamBreak,
			wantKind:     "transport",
			wantPenalize: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			outcome := classifyStreamOutcome(tc.ctxErr, tc.readErr, tc.writeErr, tc.gotTerminal)
			if outcome.logStatusCode != tc.wantStatus {
				t.Fatalf("status mismatch: got %d want %d", outcome.logStatusCode, tc.wantStatus)
			}
			if outcome.failureKind != tc.wantKind {
				t.Fatalf("failure kind mismatch: got %q want %q", outcome.failureKind, tc.wantKind)
			}
			if outcome.penalize != tc.wantPenalize {
				t.Fatalf("penalize mismatch: got %v want %v", outcome.penalize, tc.wantPenalize)
			}
		})
	}
}

func TestShouldFallbackWebsocketMessageTooBigToHTTP(t *testing.T) {
	outcome := streamOutcome{
		logStatusCode:  logStatusUpstreamStreamBreak,
		failureKind:    upstreamErrorKindMessageTooBig,
		failureMessage: "上游流读取失败: websocket read error: websocket: close 1009 (message too big)",
		penalize:       false,
	}

	if !shouldFallbackWebsocketMessageTooBigToHTTP(outcome, true, false, nil, nil) {
		t.Fatal("expected websocket message-too-big before first downstream bytes to fall back to HTTP")
	}
	if shouldFallbackWebsocketMessageTooBigToHTTP(outcome, false, false, nil, nil) {
		t.Fatal("HTTP upstream should not fall back again")
	}
	if shouldFallbackWebsocketMessageTooBigToHTTP(outcome, true, true, nil, nil) {
		t.Fatal("should not fall back after downstream body has been written")
	}
	if shouldFallbackWebsocketMessageTooBigToHTTP(outcome, true, false, context.Canceled, nil) {
		t.Fatal("should not fall back after downstream context is canceled")
	}
}

func TestWebsocketMessageTooBigSource(t *testing.T) {
	readLimitErr := errors.New("websocket read error: websocket: read limit exceeded")
	if got := classifyTransportFailure(readLimitErr); got != upstreamErrorKindMessageTooBig {
		t.Fatalf("local read-limit failure kind = %q, want %q", got, upstreamErrorKindMessageTooBig)
	}
	outcome := classifyStreamOutcome(nil, readLimitErr, nil, false)
	if outcome.failureKind != upstreamErrorKindMessageTooBig || outcome.penalize || outcome.verifyAccountAuth {
		t.Fatalf("local read-limit outcome = %+v, want message-too-big without penalty or auth verification", outcome)
	}
	if got := websocketMessageTooBigSource(outcome.failureMessage); got != "local_read_limit" {
		t.Fatalf("local read-limit source = %q", got)
	}
	if got := websocketMessageTooBigSource("websocket read error: websocket: close 1009 (message too big)"); got != "peer_close" {
		t.Fatalf("peer close source = %q", got)
	}
}

func TestClassifyResponseFailedOutcome(t *testing.T) {
	payload := []byte(`{"type":"response.failed","response":{"error":{"code":"server_error","message":"An error occurred while processing your request. Please include the request ID req-123."}}}`)

	outcome := classifyResponseFailedOutcome(payload)

	if outcome.logStatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", outcome.logStatusCode, http.StatusInternalServerError)
	}
	if outcome.failureKind != "server" {
		t.Fatalf("failure kind = %q, want server", outcome.failureKind)
	}
	if !outcome.penalize {
		t.Fatal("response.failed server error should be penalized")
	}
	if !strings.Contains(outcome.failureMessage, "server_error") || !strings.Contains(outcome.failureMessage, "req-123") {
		t.Fatalf("failure message = %q, want upstream code and request id", outcome.failureMessage)
	}
}

func TestClassifyResponseFailedOutcomeInvalidRequest(t *testing.T) {
	payload := []byte(`{"type":"response.failed","response":{"error":{"code":"invalid_value","type":"invalid_request_error","message":"Invalid input"}}}`)

	outcome := classifyResponseFailedOutcome(payload)

	if outcome.logStatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", outcome.logStatusCode, http.StatusBadRequest)
	}
	if outcome.failureKind != "client" {
		t.Fatalf("failure kind = %q, want client", outcome.failureKind)
	}
	if outcome.penalize {
		t.Fatal("client-side response.failed should not penalize account")
	}
}

func TestClassifyResponseFailedOutcomeUsageLimit(t *testing.T) {
	payload := []byte(`{"type":"response.failed","response":{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"free","resets_in_seconds":3600}}}`)

	outcome := classifyResponseFailedOutcome(payload)

	if outcome.logStatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", outcome.logStatusCode, http.StatusTooManyRequests)
	}
	if outcome.failureKind != "usage_limit" {
		t.Fatalf("failure kind = %q, want usage_limit", outcome.failureKind)
	}
	if !outcome.penalize {
		t.Fatal("usage_limit response.failed should penalize account")
	}
	if !IsUsageLimitReachedError(payload) {
		t.Fatal("nested response.failed usage_limit_reached should be detected")
	}
}

// issue #310: context_length_exceeded 是确定性客户端错误（换号重试必然失败），
// 不得归为 500 触发透明重试并惩罚账号健康度。
func TestClassifyResponseFailedOutcomeContextLengthExceeded(t *testing.T) {
	// 上游真实形态：code 在 response.error.code，无显式 status_code
	payload := []byte(`{"type":"response.failed","response":{"status":"failed","error":{"code":"context_length_exceeded","message":"Your input exceeds the context window of this model. Please adjust your input and try again."}}}`)

	outcome := classifyResponseFailedOutcome(payload)

	if outcome.logStatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", outcome.logStatusCode, http.StatusBadRequest)
	}
	if outcome.failureKind != "client" {
		t.Fatalf("failure kind = %q, want client", outcome.failureKind)
	}
	if outcome.penalize {
		t.Fatal("context_length_exceeded must not penalize the account")
	}
	if shouldTransparentRetryStream(outcome, 0, 2, false, nil, nil) {
		t.Fatal("context_length_exceeded must not trigger transparent account-rotation retry")
	}
}

func TestClassifyResponseFailedOutcomeDeterministicClientErrors(t *testing.T) {
	for _, code := range []string{"context_window_exceeded", "string_above_max_length", "model_not_found", "unsupported_parameter"} {
		payload := []byte(`{"type":"response.failed","response":{"error":{"code":"` + code + `","message":"boom"}}}`)
		outcome := classifyResponseFailedOutcome(payload)
		if outcome.logStatusCode != http.StatusBadRequest {
			t.Errorf("code %s: status = %d, want %d", code, outcome.logStatusCode, http.StatusBadRequest)
		}
		if outcome.penalize {
			t.Errorf("code %s: deterministic client error must not penalize", code)
		}
	}
}

func TestShouldRecyclePooledClient(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "connection shutting down",
			err:  errors.New("http2: client connection is shutting down"),
			want: true,
		},
		{
			name: "connection reset",
			err:  errors.New("read tcp: connection reset by peer"),
			want: true,
		},
		{
			name: "broken pipe",
			err:  errors.New("write: broken pipe"),
			want: true,
		},
		{
			name: "plain timeout",
			err:  errors.New("read timeout"),
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRecyclePooledClient(tc.err); got != tc.want {
				t.Fatalf("shouldRecyclePooledClient() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestShouldTransparentRetryStream(t *testing.T) {
	retryable := streamOutcome{
		logStatusCode:  logStatusUpstreamStreamBreak,
		failureKind:    "transport",
		failureMessage: "upstream failed before first byte",
		penalize:       true,
	}

	if !shouldTransparentRetryStream(retryable, 0, 2, false, nil, nil) {
		t.Fatal("expected early upstream failure to be transparently retried")
	}
	if shouldTransparentRetryStream(retryable, 2, 2, false, nil, nil) {
		t.Fatal("expected retry to stop at maxRetries")
	}
	if shouldTransparentRetryStream(retryable, 0, 2, true, nil, nil) {
		t.Fatal("expected retry to stop after downstream already received bytes")
	}
	if shouldTransparentRetryStream(retryable, 0, 2, false, context.Canceled, nil) {
		t.Fatal("expected retry to stop when downstream context is canceled")
	}
}

func TestApplyCodexRequestHeadersUsesSessionIDWithoutConversationID(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}

	acc := &auth.Account{
		DBID:      42,
		AccountID: "acct-42",
	}
	cfg := &DeviceProfileConfig{
		UserAgent:              "codex_cli_rs/0.120.0 (Mac OS 15.5.0; arm64) Apple_Terminal/464",
		PackageVersion:         "0.120.0",
		RuntimeVersion:         "0.120.0",
		OS:                     "MacOS",
		Arch:                   "arm64",
		StabilizeDeviceProfile: true,
	}
	downstreamHeaders := http.Header{
		"Originator": []string{"custom-originator"},
	}

	applyCodexRequestHeaders(req, acc, "token-123", "cache-key-1", "api-key-1", cfg, downstreamHeaders)

	if got := req.Header.Get("Authorization"); got != "Bearer token-123" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := req.Header.Get("Session_id"); got != "cache-key-1" {
		t.Fatalf("Session_id = %q", got)
	}
	if got := req.Header.Get("Conversation_id"); got != "" {
		t.Fatalf("Conversation_id = %q, want empty", got)
	}
	if got := req.Header.Get("User-Agent"); got != cfg.UserAgent {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := req.Header.Get("Version"); got != "0.120.0" {
		t.Fatalf("Version = %q", got)
	}
	if got := req.Header.Get("Originator"); got != Originator {
		t.Fatalf("Originator = %q, want fallback %q", got, Originator)
	}
	if got := req.Header.Get("Chatgpt-Account-Id"); got != "acct-42" {
		t.Fatalf("Chatgpt-Account-Id = %q", got)
	}
	for _, name := range []string{"X-Stainless-Package-Version", "X-Stainless-Runtime-Version", "X-Stainless-Os", "X-Stainless-Arch"} {
		if got := req.Header.Get(name); got != "" {
			t.Fatalf("%s = %q, want empty", name, got)
		}
	}
}

func TestApplyCodexRequestHeadersAppliesAccountCustomHeadersLast(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}

	acc := &auth.Account{
		DBID:      42,
		AccountID: "acct-default",
		CustomHeaders: map[string]string{
			"Authorization":      "Bearer upstream-override",
			"Chatgpt-Account-Id": "acct-override",
			"X-Custom-Header":    "custom-value",
		},
	}

	applyCodexRequestHeaders(req, acc, "token-123", "cache-key-1", "api-key-1", nil, http.Header{})

	if got := req.Header.Get("Authorization"); got != "Bearer upstream-override" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := req.Header.Get("Chatgpt-Account-Id"); got != "acct-override" {
		t.Fatalf("Chatgpt-Account-Id = %q", got)
	}
	if got := req.Header.Get("X-Custom-Header"); got != "custom-value" {
		t.Fatalf("X-Custom-Header = %q", got)
	}
}

// TestApplyCodexRequestHeadersForwardsAttestationOnlyWhenPresent 验证 DeviceCheck
// 设备认证头（openai/codex#20619）的透传策略：下游携带真实 token 时原样转发，
// 缺失时绝不伪造/补空——假 token 服务端向 Apple 验证必败，比不携带更暴露特征。
func TestApplyCodexRequestHeadersForwardsAttestationOnlyWhenPresent(t *testing.T) {
	acc := &auth.Account{DBID: 42, AccountID: "acct-42"}

	// 下游携带真实 token → 原样透传
	withToken, _ := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	applyCodexRequestHeaders(withToken, acc, "token-123", "cache-key-1", "api-key-1", nil, http.Header{
		"X-Oai-Attestation": []string{"v1.real-devicecheck-token"},
	})
	if got := withToken.Header.Get("X-Oai-Attestation"); got != "v1.real-devicecheck-token" {
		t.Fatalf("X-Oai-Attestation = %q, want passthrough of downstream token", got)
	}

	// 下游未携带 → 不注入（保持与合法 CLI 一致的"干净缺失"）
	without, _ := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	applyCodexRequestHeaders(without, acc, "token-123", "cache-key-1", "api-key-1", nil, http.Header{})
	if got := without.Header.Get("X-Oai-Attestation"); got != "" {
		t.Fatalf("X-Oai-Attestation = %q, want empty (never fabricate)", got)
	}
}

func TestApplyCodexRequestHeadersForwardsResponsesLiteOnlyWhenPresent(t *testing.T) {
	const headerName = codexResponsesLiteHeader
	acc := &auth.Account{DBID: 42, AccountID: "acct-42"}
	downstreamHeaders := make(http.Header)
	downstreamHeaders.Set(headerName, "true")

	withLite, _ := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	applyCodexRequestHeaders(withLite, acc, "token-123", "cache-key-1", "api-key-1", nil, downstreamHeaders)
	if got := withLite.Header.Get(headerName); got != "true" {
		t.Fatalf("%s = %q, want true", headerName, got)
	}

	withoutLite, _ := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	applyCodexRequestHeaders(withoutLite, acc, "token-123", "cache-key-1", "api-key-1", nil, http.Header{})
	if got := withoutLite.Header.Get(headerName); got != "" {
		t.Fatalf("%s = %q, want empty when downstream omitted the signal", headerName, got)
	}
}

func TestCodexResponsesLiteRequestedRequiresExplicitTrue(t *testing.T) {
	headerTrue := make(http.Header)
	headerTrue.Set(codexResponsesLiteHeader, " TRUE ")
	headerFalse := make(http.Header)
	headerFalse.Set(codexResponsesLiteHeader, "false")

	tests := []struct {
		name    string
		body    []byte
		headers http.Header
		want    bool
	}{
		{name: "http header", body: []byte(`{"model":"gpt-5.4"}`), headers: headerTrue, want: true},
		{name: "websocket metadata", body: []byte(`{"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true"}}`), want: true},
		{name: "false values", body: []byte(`{"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"false"}}`), headers: headerFalse, want: false},
		{name: "model name alone", body: []byte(`{"model":"gpt-5.6-sol"}`), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := codexResponsesLiteRequested(tt.body, tt.headers); got != tt.want {
				t.Fatalf("codexResponsesLiteRequested() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestPrepareCodexResponsesLiteTransportBridgesRequestScopedSignal(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.6-sol",
		"input":[{"type":"function_call","name":"run","namespace":"code_tools","arguments":"{}"}],
		"client_metadata":{"other":"kept","ws_request_header_x_openai_internal_codex_responses_lite":"true"}
	}`)
	headers := make(http.Header)
	headers.Set(codexResponsesLiteHeader, "true")

	wsBody, wsHeaders := prepareCodexResponsesLiteTransport(body, headers, true, true)
	if got := gjson.GetBytes(wsBody, codexResponsesLiteWSMetadataPath).String(); got != "true" {
		t.Fatalf("WS Lite metadata = %q, want true; body=%s", got, wsBody)
	}
	if got := wsHeaders.Get(codexResponsesLiteHeader); got != "" {
		t.Fatalf("WS handshake Lite header = %q, want empty", got)
	}

	httpBody, httpHeaders := prepareCodexResponsesLiteTransport(body, headers, false, true)
	if got := httpHeaders.Get(codexResponsesLiteHeader); got != "true" {
		t.Fatalf("HTTP Lite header = %q, want true", got)
	}
	if marker := gjson.GetBytes(httpBody, codexResponsesLiteWSMetadataPath); marker.Exists() {
		t.Fatalf("HTTP body retained WS-only Lite metadata: %s", httpBody)
	}
	if got := gjson.GetBytes(httpBody, "client_metadata.other").String(); got != "kept" {
		t.Fatalf("unrelated client metadata = %q, want kept; body=%s", got, httpBody)
	}
	if got := gjson.GetBytes(httpBody, "input.0.namespace").String(); got != "code_tools" {
		t.Fatalf("tool namespace = %q, want code_tools; body=%s", got, httpBody)
	}
	if got := headers.Get(codexResponsesLiteHeader); got != "true" {
		t.Fatalf("caller headers were mutated: Lite header = %q", got)
	}

	nextWSBody, nextWSHeaders := prepareCodexResponsesLiteTransport([]byte(`{"model":"gpt-5.6-sol"}`), nil, true, false)
	if marker := gjson.GetBytes(nextWSBody, codexResponsesLiteWSMetadataPath); marker.Exists() {
		t.Fatalf("non-Lite request inherited pooled WS metadata: %s", nextWSBody)
	}
	if got := nextWSHeaders.Get(codexResponsesLiteHeader); got != "" {
		t.Fatalf("non-Lite request inherited pooled WS header: %q", got)
	}

	nonLiteHeaders := make(http.Header)
	nonLiteHeaders.Set(codexResponsesLiteHeader, "false")
	nonLiteBody := []byte(`{"model":"gpt-5.6-sol","client_metadata":{"other":"kept","ws_request_header_x_openai_internal_codex_responses_lite":"false"}}`)
	nonLiteBody, nonLiteHeaders = prepareCodexResponsesLiteTransport(nonLiteBody, nonLiteHeaders, false, false)
	if got := nonLiteHeaders.Get(codexResponsesLiteHeader); got != "" {
		t.Fatalf("false Lite header was forwarded as %q", got)
	}
	if marker := gjson.GetBytes(nonLiteBody, codexResponsesLiteWSMetadataPath); marker.Exists() {
		t.Fatalf("HTTP body retained false WS-only Lite metadata: %s", nonLiteBody)
	}

	// 模型门禁剥离信号后（enabled=false），WS 路径必须清掉下游带来的体内标记，
	// 否则非 lite 模型仍会把标记带上 WS 上游触发 400。
	gatedWSBody := []byte(`{"model":"gpt-5.5","client_metadata":{"other":"kept","ws_request_header_x_openai_internal_codex_responses_lite":"true"}}`)
	gatedWSHeaders := make(http.Header)
	gatedWSHeaders.Set(codexResponsesLiteHeader, "true")
	gatedWSBody, gatedWSHeaders = prepareCodexResponsesLiteTransport(gatedWSBody, gatedWSHeaders, true, false)
	if marker := gjson.GetBytes(gatedWSBody, codexResponsesLiteWSMetadataPath); marker.Exists() {
		t.Fatalf("WS body retained Lite metadata after gate disabled it: %s", gatedWSBody)
	}
	if got := gjson.GetBytes(gatedWSBody, "client_metadata.other").String(); got != "kept" {
		t.Fatalf("unrelated client metadata = %q, want kept; body=%s", got, gatedWSBody)
	}
	if got := gatedWSHeaders.Get(codexResponsesLiteHeader); got != "" {
		t.Fatalf("WS handshake Lite header = %q, want empty after gate disabled it", got)
	}
}

func TestNormalizeCodexResponsesLiteBodyEnforcesUpstreamConstraints(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.6-sol",
		"parallel_tool_calls":true,
		"reasoning":{"effort":"low"},
		"tools":[{"type":"function","name":"run"},{"type":"image_generation","model":"gpt-image-2"}],
		"instructions":"Base prompt.\n\n` + "<codex2api-codex-image-generation>\\nbridge text\\n</codex2api-codex-image-generation>" + `"
	}`)

	wsBody := normalizeCodexResponsesLiteBody(body, false)
	if got := gjson.GetBytes(wsBody, "parallel_tool_calls").Bool(); got {
		t.Fatalf("WS parallel_tool_calls = %v, want false", got)
	}
	if got := gjson.GetBytes(wsBody, "reasoning.context").String(); got != "all_turns" {
		t.Fatalf("WS reasoning.context = %q, want all_turns", got)
	}
	if got := gjson.GetBytes(wsBody, "reasoning.effort").String(); got != "low" {
		t.Fatalf("WS reasoning.effort = %q, want low (preserved)", got)
	}
	if got := gjson.GetBytes(wsBody, "tools.#").Int(); got != 2 {
		t.Fatalf("WS tools count = %d, want 2 (image tool kept on WS)", got)
	}

	httpBody := normalizeCodexResponsesLiteBody(body, true)
	if got := gjson.GetBytes(httpBody, "tools.#").Int(); got != 1 {
		t.Fatalf("HTTP tools count = %d, want 1; body=%s", got, httpBody)
	}
	if got := gjson.GetBytes(httpBody, "tools.0.type").String(); got != "function" {
		t.Fatalf("HTTP surviving tool type = %q, want function", got)
	}
	if instructions := gjson.GetBytes(httpBody, "instructions").String(); instructions != "Base prompt." {
		t.Fatalf("HTTP instructions = %q, want bridge text removed", instructions)
	}
}

func TestNormalizeCodexResponsesLiteBodyStripsImageOnlyToolSet(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.6-sol",
		"tools":[{"type":"image_generation","model":"gpt-image-2"}],
		"tool_choice":{"type":"image_generation"},
		"instructions":"` + "<codex2api-codex-image-generation>\\nbridge text\\n</codex2api-codex-image-generation>" + `"
	}`)

	httpBody := normalizeCodexResponsesLiteBody(body, true)
	if tools := gjson.GetBytes(httpBody, "tools"); tools.Exists() {
		t.Fatalf("HTTP tools should be removed entirely, got %s", tools.Raw)
	}
	if choice := gjson.GetBytes(httpBody, "tool_choice"); choice.Exists() {
		t.Fatalf("HTTP tool_choice should be removed, got %s", choice.Raw)
	}
	if instructions := gjson.GetBytes(httpBody, "instructions").String(); instructions != "" {
		t.Fatalf("HTTP instructions = %q, want empty after bridge removal", instructions)
	}
}

func TestExecuteRequestWebsocketSendsCompactionTriggerLast(t *testing.T) {
	previousWS := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousWS })

	var seenBody []byte
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		seenBody = append([]byte(nil), requestBody...)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp_test"}`)),
		}, nil
	}

	body := []byte(`{"model":"gpt-5.6-sol","input":[
		{"type":"compaction_trigger"},
		{"type":"function_call_output","call_id":"call_1","output":"ok"}
	]}`)
	resp, err := ExecuteRequest(context.Background(), &auth.Account{DBID: 1, AccessToken: "token"}, body, "session-1", "", "sk-local", nil, http.Header{}, true)
	if err != nil {
		t.Fatalf("ExecuteRequest() error = %v", err)
	}
	resp.Body.Close()

	input := gjson.GetBytes(seenBody, "input").Array()
	if len(input) != 2 || input[1].Get("type").String() != "compaction_trigger" {
		t.Fatalf("final websocket body must end with compaction_trigger: %s", seenBody)
	}
}

func TestExecuteOpenAIResponsesRequestSendsCompactionTriggerLast(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenBody, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_test"}`))
	}))
	t.Cleanup(server.Close)

	account := &auth.Account{
		DBID:         42,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      server.URL,
		APIKey:       "relay-token",
	}
	body := []byte(`{"model":"gpt-5.6-sol","input":[
		{"type":"compaction_trigger"},
		{"type":"function_call_output","call_id":"call_1","output":"ok"}
	]}`)
	resp, err := ExecuteOpenAIResponsesRequest(context.Background(), account, body, "", nil)
	if err != nil {
		t.Fatalf("ExecuteOpenAIResponsesRequest() error = %v", err)
	}
	resp.Body.Close()

	input := gjson.GetBytes(seenBody, "input").Array()
	if len(input) != 2 || input[1].Get("type").String() != "compaction_trigger" {
		t.Fatalf("final relay body must end with compaction_trigger: %s", seenBody)
	}
}

func TestApplyOpenAIResponsesRequestHeadersAppliesAccountCustomHeadersLast(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}

	acc := &auth.Account{
		DBID: 42,
		CustomHeaders: map[string]string{
			"Authorization":       "Bearer upstream-override",
			"OpenAI-Organization": "org-override",
		},
	}
	downstreamHeaders := http.Header{"OpenAI-Organization": []string{"org-downstream"}}

	applyOpenAIResponsesRequestHeaders(req, acc, "api-key-1", downstreamHeaders)

	if got := req.Header.Get("Authorization"); got != "Bearer upstream-override" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := req.Header.Get("OpenAI-Organization"); got != "org-override" {
		t.Fatalf("OpenAI-Organization = %q", got)
	}
}

func TestApplyCodexRequestHeadersUsesMinimalFallbackByDefault(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}

	acc := &auth.Account{
		DBID:      42,
		AccountID: "acct-42",
	}

	applyCodexRequestHeaders(req, acc, "token-123", "", "api-key-1", nil, http.Header{})

	if got := req.Header.Get("User-Agent"); got != defaultCodexCLIUserAgent {
		t.Fatalf("User-Agent = %q, want minimal Codex CLI %q", got, defaultCodexCLIUserAgent)
	}
	if got := req.Header.Get("Version"); got != latestCodexCLIVersion {
		t.Fatalf("Version = %q, want %q", got, latestCodexCLIVersion)
	}
}

func TestApplyCodexRequestHeadersUsesCustomGeneratedUserAgentConfig(t *testing.T) {
	prev := CurrentRuntimeSettings()
	normalized, err := NormalizeCodexUserAgentConfigJSON(`{"client_name":"codex-tui","client_version":"0.142.0-alpha.10","os_name":"Mac OS","os_version":"13.7.8","arch":"arm64","terminal":"xterm-256color"}`)
	if err != nil {
		t.Fatalf("NormalizeCodexUserAgentConfigJSON() error = %v", err)
	}
	ApplyRuntimeSettings(RuntimeSettings{
		ClientCompatMode:     ClientCompatModeForce,
		CodexUserAgentConfig: normalized,
	})
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })

	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}

	applyCodexRequestHeaders(req, &auth.Account{DBID: 42}, "token-123", "", "api-key-1", nil, http.Header{})

	wantUA := "codex-tui/0.142.0-alpha.10 (Mac OS 13.7.8; arm64) xterm-256color (codex-tui; 0.142.0-alpha.10)"
	if got := req.Header.Get("User-Agent"); got != wantUA {
		t.Fatalf("User-Agent = %q, want %q", got, wantUA)
	}
	if got := req.Header.Get("Version"); got != "0.142.0-alpha.10" {
		t.Fatalf("Version = %q, want 0.142.0-alpha.10", got)
	}
}

func TestApplyCodexRequestHeadersRawUserAgentWithoutVersionOmitsVersionHeader(t *testing.T) {
	prev := CurrentRuntimeSettings()
	normalized, err := NormalizeCodexUserAgentConfigJSON(`{"raw_user_agent":"my-router"}`)
	if err != nil {
		t.Fatalf("NormalizeCodexUserAgentConfigJSON() error = %v", err)
	}
	ApplyRuntimeSettings(RuntimeSettings{
		ClientCompatMode:     ClientCompatModeForce,
		CodexUserAgentConfig: normalized,
	})
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })

	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}

	applyCodexRequestHeaders(req, &auth.Account{DBID: 42}, "token-123", "", "api-key-1", nil, http.Header{})

	if got := req.Header.Get("User-Agent"); got != "my-router" {
		t.Fatalf("User-Agent = %q, want raw override", got)
	}
	if got := req.Header.Get("Version"); got != "" {
		t.Fatalf("Version = %q, want empty", got)
	}
}

func TestApplyCodexRequestHeadersRaisesGeneratedUserAgentToAutoMinimum(t *testing.T) {
	prev := CurrentRuntimeSettings()
	normalized, err := NormalizeCodexUserAgentConfigJSON(`{"client_name":"codex-tui","client_version":"0.142.0","os_name":"Linux","os_version":"Unknown","arch":"x86_64","terminal":"xterm-256color"}`)
	if err != nil {
		t.Fatalf("NormalizeCodexUserAgentConfigJSON() error = %v", err)
	}
	ApplyRuntimeSettings(RuntimeSettings{
		ClientCompatMode:     ClientCompatModeAuto,
		CodexMinCLIVersion:   "0.150.0",
		CodexUserAgentConfig: normalized,
	})
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })

	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	downstreamHeaders := http.Header{
		"User-Agent": []string{"codex-tui/0.100.0 (Linux Unknown; x86_64) xterm-256color (codex-tui; 0.100.0)"},
		"Originator": []string{"codex-tui"},
	}

	applyCodexRequestHeaders(req, &auth.Account{DBID: 42}, "token-123", "", "api-key-1", nil, downstreamHeaders)

	wantUA := "codex-tui/0.150.0 (Linux Unknown; x86_64) xterm-256color (codex-tui; 0.150.0)"
	if got := req.Header.Get("User-Agent"); got != wantUA {
		t.Fatalf("User-Agent = %q, want %q", got, wantUA)
	}
	if got := req.Header.Get("Version"); got != "0.150.0" {
		t.Fatalf("Version = %q, want 0.150.0", got)
	}
}

func TestApplyCodexRequestHeadersRepairsBlankStabilizedProfileUserAgent(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}

	cfg := &DeviceProfileConfig{StabilizeDeviceProfile: true}
	applyCodexRequestHeaders(req, &auth.Account{DBID: 42}, "token-123", "", "api-key-1", cfg, http.Header{})

	if got := req.Header.Get("User-Agent"); got != defaultCodexCLIUserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, defaultCodexCLIUserAgent)
	}
	if got := req.Header.Get("Version"); got != latestCodexCLIVersion {
		t.Fatalf("Version = %q, want %q", got, latestCodexCLIVersion)
	}
}

func TestApplyCodexRequestHeadersPreservesOfficialClientHeaders(t *testing.T) {
	prev := CurrentRuntimeSettings()
	ApplyRuntimeSettings(RuntimeSettings{ClientCompatMode: ClientCompatModePreserve})
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })

	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	acc := &auth.Account{DBID: 42, AccountID: "acct-42"}
	downstreamHeaders := http.Header{
		"User-Agent":            []string{"codex_vscode/1.2.3"},
		"Originator":            []string{"codex_vscode"},
		"Version":               []string{"1.2.3"},
		"X-Codex-Turn-State":    []string{"turn-state"},
		"X-Codex-Turn-Metadata": []string{"turn-metadata"},
		"X-Client-Request-Id":   []string{"req-123"},
	}

	applyCodexRequestHeaders(req, acc, "token-123", "cache-key-1", "api-key-1", nil, downstreamHeaders)

	if got := req.Header.Get("User-Agent"); got != "codex_vscode/1.2.3" {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := req.Header.Get("Originator"); got != "codex_vscode" {
		t.Fatalf("Originator = %q", got)
	}
	if got := req.Header.Get("Version"); got != "1.2.3" {
		t.Fatalf("Version = %q", got)
	}
	for _, name := range []string{"X-Codex-Turn-State", "X-Codex-Turn-Metadata", "X-Client-Request-Id"} {
		if got := req.Header.Get(name); got != downstreamHeaders.Get(name) {
			t.Fatalf("%s = %q, want %q", name, got, downstreamHeaders.Get(name))
		}
	}
}

func TestApplyCodexRequestHeadersAutoUpgradesOldCodexCLI(t *testing.T) {
	prev := CurrentRuntimeSettings()
	ApplyRuntimeSettings(RuntimeSettings{
		ClientCompatMode:   ClientCompatModeAuto,
		CodexMinCLIVersion: "0.118.0",
	})
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })

	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	acc := &auth.Account{DBID: 42, AccountID: "acct-42"}
	downstreamHeaders := http.Header{
		"User-Agent": []string{"codex_cli_rs/0.117.0 (Mac OS 15.5.0; arm64) Apple_Terminal/464"},
		"Originator": []string{Originator},
	}

	applyCodexRequestHeaders(req, acc, "token-123", "", "api-key-1", nil, downstreamHeaders)

	if got := req.Header.Get("User-Agent"); got == downstreamHeaders.Get("User-Agent") {
		t.Fatalf("User-Agent preserved old CLI UA %q", got)
	}
	if got := req.Header.Get("Version"); got != latestCodexCLIVersion {
		t.Fatalf("Version = %q, want %q", got, latestCodexCLIVersion)
	}
}

func TestApplyCodexRequestHeadersAutoDoesNotUpgradeEmbeddedCodexToken(t *testing.T) {
	prev := CurrentRuntimeSettings()
	ApplyRuntimeSettings(RuntimeSettings{
		ClientCompatMode:   ClientCompatModeAuto,
		CodexMinCLIVersion: "0.118.0",
	})
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })

	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	acc := &auth.Account{DBID: 42, AccountID: "acct-42"}
	spoofedUA := "Mozilla/5.0 codex_cli_rs/0.117.0"
	downstreamHeaders := http.Header{
		"User-Agent": []string{spoofedUA},
		"Originator": []string{"random-client"},
	}

	applyCodexRequestHeaders(req, acc, "token-123", "", "api-key-1", nil, downstreamHeaders)

	if got := req.Header.Get("User-Agent"); got != spoofedUA {
		t.Fatalf("User-Agent = %q, want legacy-preserved spoofed UA %q", got, spoofedUA)
	}
	if got := req.Header.Get("Version"); got != "0.117.0" {
		t.Fatalf("Version = %q, want parsed legacy version 0.117.0", got)
	}
}

func TestApplyCodexRequestHeadersFallsBackForNonOfficialClient(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	acc := &auth.Account{DBID: 42}
	downstreamHeaders := http.Header{
		"User-Agent": []string{"curl/8.0"},
		"Originator": []string{"random-client"},
	}

	applyCodexRequestHeaders(req, acc, "token-123", "", "api-key-1", nil, downstreamHeaders)

	if got := req.Header.Get("User-Agent"); got != defaultCodexCLIUserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, defaultCodexCLIUserAgent)
	}
	if got := req.Header.Get("Originator"); got != Originator {
		t.Fatalf("Originator = %q, want %q", got, Originator)
	}
	if got := req.Header.Get("Version"); got != latestCodexCLIVersion {
		t.Fatalf("Version = %q, want %q", got, latestCodexCLIVersion)
	}
}

func TestApplyCodexRequestHeadersPreservesOpenCodeClient(t *testing.T) {
	prev := CurrentRuntimeSettings()
	ApplyRuntimeSettings(RuntimeSettings{ClientCompatMode: ClientCompatModePreserve})
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })

	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	acc := &auth.Account{DBID: 42, AccountID: "acct-42"}
	downstreamHeaders := http.Header{
		"User-Agent": []string{"opencode/0.5.0"},
		"Originator": []string{"opencode"},
	}

	applyCodexRequestHeaders(req, acc, "token-123", "", "api-key-1", nil, downstreamHeaders)

	if got := req.Header.Get("User-Agent"); got != "opencode/0.5.0" {
		t.Fatalf("User-Agent = %q, want %q", got, "opencode/0.5.0")
	}
	if got := req.Header.Get("Originator"); got != "opencode" {
		t.Fatalf("Originator = %q, want %q", got, "opencode")
	}
}

func TestApplyOpenAIResponsesRequestHeadersSetsCodexUserAgent(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://relay.example/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	headers := http.Header{
		"User-Agent":          []string{"curl/8.0"},
		"OpenAI-Organization": []string{"org-123"},
		"Idempotency-Key":     []string{"idem-123"},
	}

	applyOpenAIResponsesRequestHeaders(req, &auth.Account{DBID: 42}, "relay-token", headers)

	if got := req.Header.Get("User-Agent"); got != defaultCodexCLIUserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, defaultCodexCLIUserAgent)
	}
	if got := req.Header.Get("Version"); got != latestCodexCLIVersion {
		t.Fatalf("Version = %q, want %q", got, latestCodexCLIVersion)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer relay-token" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := req.Header.Get("OpenAI-Organization"); got != "org-123" {
		t.Fatalf("OpenAI-Organization = %q", got)
	}
	if got := req.Header.Get("Idempotency-Key"); got != "idem-123" {
		t.Fatalf("Idempotency-Key = %q", got)
	}
}

func TestOpenAIResponsesExecutorsDoNotLeakGoDefaultUserAgent(t *testing.T) {
	type result struct {
		path    string
		ua      string
		version string
		auth    string
		accept  string
		lite    string
		body    []byte
	}
	results := make(chan result, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		results <- result{
			path:    r.URL.Path,
			ua:      r.Header.Get("User-Agent"),
			version: r.Header.Get("Version"),
			auth:    r.Header.Get("Authorization"),
			accept:  r.Header.Get("Accept"),
			lite:    r.Header.Get(codexResponsesLiteHeader),
			body:    body,
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_test"}`))
	}))
	t.Cleanup(server.Close)

	account := &auth.Account{
		DBID:         42,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      server.URL,
		APIKey:       "relay-token",
	}

	downstreamHeaders := make(http.Header)
	downstreamHeaders.Set("User-Agent", "curl/8.0")
	downstreamHeaders.Set(codexResponsesLiteHeader, "true")

	resp, err := ExecuteOpenAIResponsesRequest(context.Background(), account, []byte(`{"model":"gpt-5.6-sol"}`), "", downstreamHeaders)
	if err != nil {
		t.Fatalf("ExecuteOpenAIResponsesRequest() error = %v", err)
	}
	resp.Body.Close()

	resp, err = ExecuteOpenAIResponsesCompactRequest(context.Background(), account, []byte(`{"model":"gpt-5.6-sol"}`), "", downstreamHeaders)
	if err != nil {
		t.Fatalf("ExecuteOpenAIResponsesCompactRequest() error = %v", err)
	}
	resp.Body.Close()

	for _, wantPath := range []string{"/v1/responses", "/v1/responses/compact"} {
		select {
		case got := <-results:
			if got.path != wantPath {
				t.Fatalf("path = %q, want %q", got.path, wantPath)
			}
			if got.ua == "" || got.ua == "Go-http-client/2.0" || !strings.HasPrefix(got.ua, "codex-tui/") {
				t.Fatalf("%s User-Agent = %q, want codex-tui and not Go default", wantPath, got.ua)
			}
			if got.version != latestCodexCLIVersion {
				t.Fatalf("%s Version = %q, want %q", wantPath, got.version, latestCodexCLIVersion)
			}
			if got.auth != "Bearer relay-token" {
				t.Fatalf("%s Authorization = %q", wantPath, got.auth)
			}
			if got.accept != "application/json, text/event-stream" {
				t.Fatalf("%s Accept = %q", wantPath, got.accept)
			}
			if got.lite != "true" {
				t.Fatalf("%s %s = %q, want true", wantPath, codexResponsesLiteHeader, got.lite)
			}
			if len(got.body) == 0 {
				t.Fatalf("%s request body was empty", wantPath)
			}
			if installationID := gjson.GetBytes(got.body, "client_metadata.x-codex-installation-id").String(); installationID != "" {
				t.Fatalf("%s unexpectedly injected Codex client metadata into a compatible relay request", wantPath)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %s", wantPath)
		}
	}
}

func TestExecuteOpenAIResponsesRequestLearnsCodexClientMetadataRequirement(t *testing.T) {
	var mu sync.Mutex
	requestCount := 0
	installationIDs := make([]string, 0, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		installationID := strings.TrimSpace(gjson.GetBytes(body, "client_metadata.x-codex-installation-id").String())

		mu.Lock()
		requestCount++
		installationIDs = append(installationIDs, installationID)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if installationID == "" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"official client required","code":"codex_access_restricted"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp_test"}`))
	}))
	t.Cleanup(server.Close)

	account := &auth.Account{
		DBID:         42001,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      server.URL,
		APIKey:       "relay-token",
	}
	headers := http.Header{
		"Authorization": []string{"Bearer downstream-api-key"},
		"User-Agent":    []string{"curl/8.0"},
	}
	body := []byte(`{"model":"gpt-5.5","input":"hello"}`)

	resp, err := ExecuteOpenAIResponsesRequest(context.Background(), account, body, "", headers)
	if err != nil {
		t.Fatalf("first ExecuteOpenAIResponsesRequest() error = %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first response status = %d, want 200", resp.StatusCode)
	}

	resp, err = ExecuteOpenAIResponsesRequest(context.Background(), account, body, "", headers)
	if err != nil {
		t.Fatalf("second ExecuteOpenAIResponsesRequest() error = %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second response status = %d, want 200", resp.StatusCode)
	}

	mu.Lock()
	gotCount := requestCount
	gotIDs := append([]string(nil), installationIDs...)
	mu.Unlock()
	if gotCount != 3 {
		t.Fatalf("upstream requests = %d, want initial rejection + retry + cached request", gotCount)
	}
	if len(gotIDs) != 3 || gotIDs[0] != "" || gotIDs[1] == "" || gotIDs[2] != gotIDs[1] {
		t.Fatalf("installation IDs = %#v, want empty then one stable generated ID", gotIDs)
	}
	if _, err := uuid.Parse(gotIDs[1]); err != nil {
		t.Fatalf("generated installation ID %q is not a UUID: %v", gotIDs[1], err)
	}
}

func TestExecuteOpenAIResponsesRequestHonorsCodexClientMetadataMode(t *testing.T) {
	tests := []struct {
		name          string
		mode          string
		wantStatus    int
		wantMetadata  bool
		seedAutoCache bool
	}{
		{name: "always injects on first request", mode: auth.CodexClientMetadataModeAlways, wantStatus: http.StatusOK, wantMetadata: true},
		{name: "off never injects", mode: auth.CodexClientMetadataModeOff, wantStatus: http.StatusForbidden, wantMetadata: false, seedAutoCache: true},
	}

	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			requestCount := 0
			installationID := ""
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				_ = r.Body.Close()
				gotInstallationID := strings.TrimSpace(gjson.GetBytes(body, "client_metadata.x-codex-installation-id").String())
				mu.Lock()
				requestCount++
				installationID = gotInstallationID
				mu.Unlock()

				w.Header().Set("Content-Type", "application/json")
				if gotInstallationID == "" {
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte(`{"error":{"code":"codex_access_restricted"}}`))
					return
				}
				_, _ = w.Write([]byte(`{"id":"resp_test"}`))
			}))
			t.Cleanup(server.Close)

			account := &auth.Account{
				DBID:                    int64(42100 + index),
				UpstreamType:            auth.UpstreamOpenAIResponses,
				BaseURL:                 server.URL,
				APIKey:                  "relay-token",
				CodexClientMetadataMode: tt.mode,
			}
			capabilityKey := openAIResponsesCodexMetadataCapabilityKey(account, server.URL)
			if tt.seedAutoCache {
				openAIResponsesCodexMetadataRequired.Store(capabilityKey, struct{}{})
				t.Cleanup(func() { openAIResponsesCodexMetadataRequired.Delete(capabilityKey) })
			}

			resp, err := ExecuteOpenAIResponsesRequest(context.Background(), account, []byte(`{"model":"gpt-5.5"}`), "", nil)
			if err != nil {
				t.Fatalf("ExecuteOpenAIResponsesRequest() error = %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("response status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}

			mu.Lock()
			gotCount := requestCount
			gotInstallationID := installationID
			mu.Unlock()
			if gotCount != 1 {
				t.Fatalf("upstream requests = %d, want 1", gotCount)
			}
			if (gotInstallationID != "") != tt.wantMetadata {
				t.Fatalf("installation ID = %q, want metadata=%v", gotInstallationID, tt.wantMetadata)
			}
		})
	}
}

func TestExecuteOpenAIResponsesRequestPreservesClientInstallationIDWithoutLearningRequirement(t *testing.T) {
	var mu sync.Mutex
	installationIDs := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		installationID := gjson.GetBytes(body, "client_metadata.x-codex-installation-id").String()
		mu.Lock()
		installationIDs = append(installationIDs, installationID)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_test"}`))
	}))
	t.Cleanup(server.Close)

	account := &auth.Account{
		DBID:         42002,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      server.URL,
		APIKey:       "relay-token",
	}
	capabilityKey := openAIResponsesCodexMetadataCapabilityKey(account, server.URL)
	openAIResponsesCodexMetadataRequired.Delete(capabilityKey)
	t.Cleanup(func() { openAIResponsesCodexMetadataRequired.Delete(capabilityKey) })

	body := []byte(`{"model":"gpt-5.5","client_metadata":{"x-codex-installation-id":"client-installation-id"}}`)
	resp, err := ExecuteOpenAIResponsesRequest(context.Background(), account, body, "", nil)
	if err != nil {
		t.Fatalf("client metadata request error = %v", err)
	}
	resp.Body.Close()

	resp, err = ExecuteOpenAIResponsesRequest(context.Background(), account, []byte(`{"model":"gpt-5.5"}`), "", nil)
	if err != nil {
		t.Fatalf("plain request error = %v", err)
	}
	resp.Body.Close()

	mu.Lock()
	gotIDs := append([]string(nil), installationIDs...)
	mu.Unlock()
	if len(gotIDs) != 2 || gotIDs[0] != "client-installation-id" || gotIDs[1] != "" {
		t.Fatalf("installation IDs = %#v, want preserved client ID then no injected ID", gotIDs)
	}
}

func TestExecuteOpenAIResponsesRequestDoesNotRetryUnrelatedForbidden(t *testing.T) {
	var mu sync.Mutex
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requestCount++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"not allowed","code":"permission_denied"}}`))
	}))
	t.Cleanup(server.Close)

	account := &auth.Account{
		DBID:         42003,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      server.URL,
		APIKey:       "relay-token",
	}
	resp, err := ExecuteOpenAIResponsesRequest(context.Background(), account, []byte(`{"model":"gpt-5.5"}`), "", nil)
	if err != nil {
		t.Fatalf("ExecuteOpenAIResponsesRequest() error = %v", err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || gjson.GetBytes(responseBody, "error.code").String() != "permission_denied" {
		t.Fatalf("response = status %d body %s, want original permission_denied response", resp.StatusCode, responseBody)
	}
	mu.Lock()
	gotCount := requestCount
	mu.Unlock()
	if gotCount != 1 {
		t.Fatalf("upstream requests = %d, want 1", gotCount)
	}
}

func TestCodexTransportModeDefaultsToStandard(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "")
	if _, ok := newCodexTransport("").(*http.Transport); !ok {
		t.Fatalf("newCodexTransport default = %T, want *http.Transport", newCodexTransport(""))
	}
}

func TestCodexTransportModeCanUseUTLSChrome(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "utls_chrome")
	if _, ok := newCodexTransport("").(*utlsRoundTripper); !ok {
		t.Fatalf("newCodexTransport utls_chrome = %T, want *utlsRoundTripper", newCodexTransport(""))
	}
}

func TestClientPoolKeyIncludesTransportMode(t *testing.T) {
	acc := &auth.Account{DBID: 42}
	standard := clientPoolKey(acc, "http://proxy", codexTransportModeStandard)
	utlsChrome := clientPoolKey(acc, "http://proxy", codexTransportModeUTLSChrome)
	if standard == utlsChrome {
		t.Fatalf("clientPoolKey should include transport mode, got %q", standard)
	}
}

func TestIsolateCodexSessionIDUsesAPIKeyScope(t *testing.T) {
	raw := "session-1"
	if got := IsolateCodexSessionID(0, raw); got != raw {
		t.Fatalf("IsolateCodexSessionID without api key = %q, want %q", got, raw)
	}
	first := IsolateCodexSessionID(1, raw)
	second := IsolateCodexSessionID(2, raw)
	if first == raw || second == raw || first == second {
		t.Fatalf("expected distinct isolated session ids, got first=%q second=%q raw=%q", first, second, raw)
	}
}

func TestResolveSessionIDPrefersContinuityHeaders(t *testing.T) {
	headers := http.Header{
		"Session_id":      []string{"session-from-header"},
		"Conversation_id": []string{"conversation-from-header"},
		"Authorization":   []string{"Bearer sk-test-123"},
	}

	if got := ResolveSessionID(headers, []byte(`{"prompt_cache_key":"body-key"}`)); got != "session-from-header" {
		t.Fatalf("ResolveSessionID() = %q, want %q", got, "session-from-header")
	}

	headers.Del("Session_id")
	if got := ResolveSessionID(headers, []byte(`{"prompt_cache_key":"body-key"}`)); got != "conversation-from-header" {
		t.Fatalf("ResolveSessionID() = %q, want %q", got, "conversation-from-header")
	}

	headers.Del("Conversation_id")
	headers.Set("Idempotency-Key", "idempotency-key-1")
	if got := ResolveSessionID(headers, []byte(`{"prompt_cache_key":"body-key"}`)); got != "idempotency-key-1" {
		t.Fatalf("ResolveSessionID() = %q, want %q", got, "idempotency-key-1")
	}
}

func TestResolveSessionIDUsesLocalAffinityHeader(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"same prompt"}]}`)
	baseHeaders := http.Header{"Authorization": []string{"Bearer shared-key"}}
	headersA := baseHeaders.Clone()
	headersA.Set("X-Codex2API-Affinity-Key", "user-a")
	headersB := headersA.Clone()
	headersB.Set("X-Codex2API-Affinity-Key", "user-b")

	identityWithoutAffinity := resolveRequestSessionIdentity(baseHeaders, body)
	identityA := resolveRequestSessionIdentity(headersA, body)
	identityB := resolveRequestSessionIdentity(headersB, body)
	if identityWithoutAffinity.hasDownstreamAffinity {
		t.Fatal("request without the dedicated affinity header was marked as having one")
	}
	if identityWithoutAffinity.hasRequestFingerprint {
		t.Fatal("plain request without a Codex fingerprint was marked as fingerprinted")
	}
	if !identityA.hasDownstreamAffinity || !identityB.hasDownstreamAffinity {
		t.Fatal("request with the dedicated affinity header was not marked as having one")
	}
	if !identityA.hasRequestFingerprint || !identityB.hasRequestFingerprint {
		t.Fatal("dedicated affinity header must count as a routing fingerprint")
	}
	idA := identityA.affinityID
	idARepeat := ResolveSessionID(headersA, body)
	idB := identityB.affinityID
	if idA == "" || idA != idARepeat {
		t.Fatalf("affinity id must be non-empty and stable: first=%q repeat=%q", idA, idARepeat)
	}
	if idA == idB {
		t.Fatalf("different affinity headers produced the same id %q", idA)
	}
	if idA == "user-a" || idB == "user-b" {
		t.Fatal("raw downstream affinity identifiers must not be retained")
	}
	if explicit := ResolveExplicitSessionID(headersA, body); explicit != "" {
		t.Fatalf("local affinity header leaked into upstream explicit session id: %q", explicit)
	}
	if identityA.upstreamSeed != identityWithoutAffinity.upstreamSeed || identityB.upstreamSeed != identityWithoutAffinity.upstreamSeed {
		t.Fatalf("local affinity changed upstream seed: without=%q a=%q b=%q", identityWithoutAffinity.upstreamSeed, identityA.upstreamSeed, identityB.upstreamSeed)
	}
	if identityA.explicitUpstreamID != "" || identityB.explicitUpstreamID != "" {
		t.Fatalf("local affinity became an explicit upstream id: a=%q b=%q", identityA.explicitUpstreamID, identityB.explicitUpstreamID)
	}
}

func TestResolveSessionIdentityTreatsBlankAffinityHeaderAsMissing(t *testing.T) {
	headers := http.Header{"Authorization": []string{"Bearer shared-key"}}
	headers.Set("X-Codex2API-Affinity-Key", "   ")
	identity := resolveRequestSessionIdentity(headers, []byte(`{"model":"gpt-5.4","input":"hello"}`))
	if identity.hasDownstreamAffinity {
		t.Fatal("blank affinity header must be treated as missing")
	}
}

func TestResolveSessionIdentityRecognizesCodexEngineFingerprint(t *testing.T) {
	headers := http.Header{
		"Authorization":      []string{"Bearer shared-key"},
		"X-Codex-Turn-State": []string{"turn-state"},
	}
	identity := resolveRequestSessionIdentity(headers, []byte(`{"model":"gpt-5.4","input":"hello"}`))
	if identity.hasDownstreamAffinity {
		t.Fatal("Codex engine fingerprint must not be treated as the dedicated affinity header")
	}
	if !identity.hasRequestFingerprint {
		t.Fatal("Codex engine fingerprint was not recognized for group routing")
	}
}

func TestLocalAffinityDoesNotAffectPerAPIKeyHTTPUpstreamSessionID(t *testing.T) {
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })
	nextSettings := previousSettings
	nextSettings.RequestIsolationMode = RequestIsolationModePerAPIKey
	ApplyRuntimeSettings(nextSettings)

	body := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"same prompt"}]}`)
	baseHeaders := http.Header{"Authorization": []string{"Bearer shared-key"}}
	affinityHeaders := baseHeaders.Clone()
	affinityHeaders.Set("X-Codex2API-Affinity-Key", "user-a")

	withoutAffinity := resolveRequestSessionIdentity(baseHeaders, body)
	withAffinity := resolveRequestSessionIdentity(affinityHeaders, body)
	withoutUpstreamID := resolveUpstreamSessionID(7, withoutAffinity.upstreamSeed, withoutAffinity.explicitUpstreamID, false)
	withUpstreamID := resolveUpstreamSessionID(7, withAffinity.upstreamSeed, withAffinity.explicitUpstreamID, false)
	if withoutUpstreamID == "" || withUpstreamID != withoutUpstreamID {
		t.Fatalf("per-api-key HTTP upstream id changed with local affinity: without=%q with=%q", withoutUpstreamID, withUpstreamID)
	}
	if withUpstreamID == withAffinity.affinityID {
		t.Fatalf("local affinity id leaked as upstream id %q", withUpstreamID)
	}
}

func TestLocalAffinityPreservesExplicitAndAPIKeyUpstreamSeeds(t *testing.T) {
	t.Run("explicit session", func(t *testing.T) {
		headers := http.Header{
			"Authorization": []string{"Bearer shared-key"},
			"Session_id":    []string{"explicit-session"},
		}
		headers.Set("X-Codex2API-Affinity-Key", "user-a")
		identity := resolveRequestSessionIdentity(headers, []byte(`{"model":"gpt-5.4"}`))
		if identity.upstreamSeed != "explicit-session" || identity.explicitUpstreamID != "explicit-session" {
			t.Fatalf("explicit upstream identity changed: seed=%q explicit=%q", identity.upstreamSeed, identity.explicitUpstreamID)
		}
		if identity.affinityID == identity.upstreamSeed {
			t.Fatalf("local affinity did not remain separate from explicit upstream seed %q", identity.upstreamSeed)
		}
	})

	t.Run("api key fallback", func(t *testing.T) {
		headers := http.Header{"Authorization": []string{"Bearer shared-key"}}
		headers.Set("X-Codex2API-Affinity-Key", "user-a")
		identity := resolveRequestSessionIdentity(headers, []byte(`{}`))
		wantSeed := uuid.NewSHA1(uuid.NameSpaceOID, []byte("codex2api:prompt-cache:shared-key")).String()
		if identity.upstreamSeed != wantSeed || identity.explicitUpstreamID != "" {
			t.Fatalf("API-key upstream fallback changed: seed=%q explicit=%q want=%q", identity.upstreamSeed, identity.explicitUpstreamID, wantSeed)
		}
		if identity.affinityID == identity.upstreamSeed {
			t.Fatalf("local affinity did not remain separate from API-key upstream seed %q", identity.upstreamSeed)
		}
	})
}

func TestResolveExplicitSessionIDDoesNotUseAPIKeyFallback(t *testing.T) {
	headers := http.Header{"Authorization": []string{"Bearer sk-test-123"}}

	if got := ResolveExplicitSessionID(headers, []byte(`{}`)); got != "" {
		t.Fatalf("ResolveExplicitSessionID() = %q, want empty", got)
	}
	if got := ResolveSessionID(headers, []byte(`{}`)); got == "" {
		t.Fatal("ResolveSessionID() should still generate API-key fallback")
	}
}

func TestExecuteRequestExplicitFalseBypassesForcedWebsocket(t *testing.T) {
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })
	nextSettings := previousSettings
	nextSettings.CodexForceWebsocket = true
	ApplyRuntimeSettings(nextSettings)

	previousWS := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousWS })
	wsCalled := false
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		wsCalled = true
		return nil, errors.New("websocket should not be used")
	}

	_, err := ExecuteRequest(context.Background(), &auth.Account{DBID: 1}, []byte(`{"model":"gpt-5.4"}`), "", "", "sk-local", nil, http.Header{}, false)
	if err == nil {
		t.Fatal("ExecuteRequest() error = nil, want missing account error after bypassing websocket")
	}
	if wsCalled {
		t.Fatal("WebsocketExecuteFunc was called despite explicit useWebsocket=false")
	}
}

func TestExecuteRequestForcedWebsocketUsesStatelessSessionWhenMissing(t *testing.T) {
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })
	nextSettings := previousSettings
	nextSettings.CodexForceWebsocket = true
	// per-api-key 模式：恢复 v2 旧行为，断言 prompt_cache_key 跨请求确定性（回归保护 8ea79aa）。
	nextSettings.RequestIsolationMode = RequestIsolationModePerAPIKey
	ApplyRuntimeSettings(nextSettings)

	previousWS := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousWS })
	var gotSessionIDs []string
	var gotCacheKeys []string
	var gotPoolKeys []string
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		gotSessionIDs = append(gotSessionIDs, sessionID)
		gotCacheKeys = append(gotCacheKeys, gjson.GetBytes(requestBody, "prompt_cache_key").String())
		gotPoolKeys = append(gotPoolKeys, poolRouteKey)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp_test"}`)),
		}, nil
	}

	for i := 0; i < 2; i++ {
		resp, err := ExecuteRequest(context.Background(), &auth.Account{DBID: 1, AccessToken: "token"}, []byte(`{"model":"gpt-5.4"}`), "", "", "sk-local", nil, http.Header{})
		if err != nil {
			t.Fatalf("ExecuteRequest() error = %v", err)
		}
		resp.Body.Close()
	}
	for _, sessionID := range gotSessionIDs {
		if !strings.HasPrefix(sessionID, "stateless-") {
			t.Fatalf("sessionID = %q, want stateless-*", sessionID)
		}
	}
	if gotSessionIDs[0] == gotSessionIDs[1] {
		t.Fatalf("stateless sessionIDs should differ per request, both = %q", gotSessionIDs[0])
	}
	// per-api-key 模式下 prompt cache key 必须确定性：两次请求一致，且不等于一次性连接 ID。
	if gotCacheKeys[0] == "" || gotCacheKeys[0] != gotCacheKeys[1] {
		t.Fatalf("prompt_cache_key = %q / %q, want identical deterministic key", gotCacheKeys[0], gotCacheKeys[1])
	}
	if strings.HasPrefix(gotCacheKeys[0], "stateless-") {
		t.Fatalf("prompt_cache_key = %q, must not be a stateless connection ID", gotCacheKeys[0])
	}
	// per-api-key 模式不拆分连接池键（cache key 本身既是上游身份也是 baseKey）。
	if gotPoolKeys[0] != "" {
		t.Fatalf("per-api-key mode poolRouteKey = %q, want empty", gotPoolKeys[0])
	}
}

// TestExecuteRequestForcedWebsocketIsolatedMode 验证默认隔离模式：无显式会话时
// 上游 prompt_cache_key 每请求唯一（隔离），但连接池路由键(poolRouteKey)按 API Key
// 稳定，从而保住 8 槽池复用与抗握手限流。
func TestExecuteRequestForcedWebsocketIsolatedMode(t *testing.T) {
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })
	nextSettings := previousSettings
	nextSettings.CodexForceWebsocket = true
	nextSettings.RequestIsolationMode = RequestIsolationModeIsolated
	ApplyRuntimeSettings(nextSettings)

	previousWS := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousWS })
	var gotCacheKeys []string
	var gotPoolKeys []string
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		gotCacheKeys = append(gotCacheKeys, gjson.GetBytes(requestBody, "prompt_cache_key").String())
		gotPoolKeys = append(gotPoolKeys, poolRouteKey)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp_test"}`)),
		}, nil
	}

	for i := 0; i < 2; i++ {
		resp, err := ExecuteRequest(context.Background(), &auth.Account{DBID: 1, AccessToken: "token"}, []byte(`{"model":"gpt-5.4"}`), "", "", "sk-local", nil, http.Header{})
		if err != nil {
			t.Fatalf("ExecuteRequest() error = %v", err)
		}
		resp.Body.Close()
	}
	// 上游身份隔离：两次 prompt_cache_key 不同，且都非空、非 stateless 连接 ID。
	if gotCacheKeys[0] == "" || gotCacheKeys[0] == gotCacheKeys[1] {
		t.Fatalf("isolated prompt_cache_key = %q / %q, want distinct per request", gotCacheKeys[0], gotCacheKeys[1])
	}
	if strings.HasPrefix(gotCacheKeys[0], "stateless-") {
		t.Fatalf("prompt_cache_key = %q, must not be a stateless connection ID", gotCacheKeys[0])
	}
	// 连接池键稳定：两次 poolRouteKey 一致且非空，按 API Key 派生（抗握手风暴）。
	if gotPoolKeys[0] == "" || gotPoolKeys[0] != gotPoolKeys[1] {
		t.Fatalf("isolated poolRouteKey = %q / %q, want identical stable key", gotPoolKeys[0], gotPoolKeys[1])
	}
	// 池键不能等于每请求唯一的上游身份键，否则槽位池失效。
	if gotPoolKeys[0] == gotCacheKeys[0] {
		t.Fatalf("poolRouteKey must not equal per-request upstream cache key")
	}
}

// TestResolveUpstreamSessionID 覆盖上游身份键派生的各分支。
func TestResolveUpstreamSessionID(t *testing.T) {
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })

	setMode := func(mode string) {
		next := previousSettings
		next.RequestIsolationMode = mode
		ApplyRuntimeSettings(next)
	}

	// 1. WS + 无显式会话：恒为 ""（交给 stateless 路径），与隔离模式无关。
	setMode(RequestIsolationModeIsolated)
	if got := resolveUpstreamSessionID(7, "sess-abc", "", true); got != "" {
		t.Fatalf("WS no-explicit isolated: got %q, want empty", got)
	}
	setMode(RequestIsolationModePerAPIKey)
	if got := resolveUpstreamSessionID(7, "sess-abc", "", true); got != "" {
		t.Fatalf("WS no-explicit per-api-key: got %q, want empty", got)
	}

	// 2. HTTP + 无显式会话 + isolated：每请求唯一（两次不同、均非空）。
	setMode(RequestIsolationModeIsolated)
	a := resolveUpstreamSessionID(7, "sess-abc", "", false)
	b := resolveUpstreamSessionID(7, "sess-abc", "", false)
	if a == "" || a == b {
		t.Fatalf("HTTP isolated: got %q / %q, want distinct non-empty", a, b)
	}

	// 3. HTTP + 无显式会话 + per-api-key：确定性（两次一致，= IsolateCodexSessionID）。
	setMode(RequestIsolationModePerAPIKey)
	c := resolveUpstreamSessionID(7, "sess-abc", "", false)
	d := resolveUpstreamSessionID(7, "sess-abc", "", false)
	if c == "" || c != d {
		t.Fatalf("HTTP per-api-key: got %q / %q, want identical deterministic", c, d)
	}
	if want := IsolateCodexSessionID(7, "sess-abc"); c != want {
		t.Fatalf("HTTP per-api-key: got %q, want %q", c, want)
	}

	// 4. 显式会话：两模式都走确定性 IsolateCodexSessionID（不隔离用户主动声明的会话）。
	for _, mode := range []string{RequestIsolationModeIsolated, RequestIsolationModePerAPIKey} {
		setMode(mode)
		got := resolveUpstreamSessionID(7, "sess-xyz", "sess-xyz", false)
		if want := IsolateCodexSessionID(7, "sess-xyz"); got != want {
			t.Fatalf("explicit session mode=%s: got %q, want %q", mode, got, want)
		}
	}
}

// HTTP 出站收口必须兜底剥离 WS 事件信封的顶层 type，即使上层 prepare 被绕过
// （native WS ingress 的 1009 降级 / 生图强制 HTTP / Agent Identity 强制 HTTP
// 都会带信封 body 走到这里）；嵌套 type 不受影响 (issue #548)。
func TestExecuteRequestHTTPStripsTopLevelEnvelopeType(t *testing.T) {
	previousResin := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(previousResin) })

	bodyCh := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyCh <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_test"}`))
	}))
	t.Cleanup(upstream.Close)
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})

	raw := []byte(`{"type":"response.create","model":"gpt-5.4","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	resp, err := ExecuteRequest(context.Background(), &auth.Account{DBID: 1, AccessToken: "token"}, raw, "", "", "sk-local", nil, http.Header{}, false)
	if err != nil {
		t.Fatalf("ExecuteRequest() error = %v", err)
	}
	resp.Body.Close()

	select {
	case got := <-bodyCh:
		if gjson.GetBytes(got, "type").Exists() {
			t.Fatalf("top-level type should be stripped before HTTP upstream: %s", got)
		}
		if it := gjson.GetBytes(got, "input.0.type").String(); it != "message" {
			t.Fatalf("nested input type = %q, want message; body=%s", it, got)
		}
		if ct := gjson.GetBytes(got, "input.0.content.0.type").String(); ct != "input_text" {
			t.Fatalf("nested content type = %q, want input_text; body=%s", ct, got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upstream request")
	}
}
