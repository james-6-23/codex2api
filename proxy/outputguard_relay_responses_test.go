package proxy

// OI-W9-1: Responses relay-style 分支（OpenAI Responses 中转 / Antigravity
// Responses 等）的 output guard 接线。relay 账号不走标准 Codex 主分支，而是
// 在 Responses handler 内拥有独立的流式事件循环与非流式响应体路径，因此需要
// 用 relay 账号 fixture 单独证明 guard 语义（T10A 只覆盖标准分支）。

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/config"
	"github.com/codex2api/security/outputguard"
)

// relayResponsesGuardOverflow 生成一个超过 channel 预算（真实数据库最小值
// 128KiB）的低周期性文本，确保命中的是字节预算链路而不是 repetition 检测。
func relayResponsesGuardOverflow(seed int64) string {
	return outputGuardTestRandomText(seed, int(guardHTTPTestMaxChannelBytes)+12*1024)
}

// newRelayResponsesGuardHandler 使用仓库既有的 OpenAI Responses 中转账号
// fixture（BaseURL 直连 httptest），并沿用 guard 集成测试的匿名放行配置。
func newRelayResponsesGuardHandler(t *testing.T, upstreamURL string) *Handler {
	t.Helper()
	store := newOpenAIResponsesRelayStore(upstreamURL)
	t.Cleanup(store.Stop)
	return NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
}

// TestResponsesRelayStreamGuardTrippedPreToken 证明 relay-style Responses 流式
// 分支在首 token 前命中 guard 时：返回 502 脱敏 envelope、不出现成功终态、上游
// 恰好调用一次且被立即取消。T10A 只覆盖标准 Codex 分支，本用例在接线前为 RED。
func TestResponsesRelayStreamGuardTrippedPreToken(t *testing.T) {
	setupOutputGuardHTTPTestEnv(t)

	overflow := relayResponsesGuardOverflow(901)
	var upstreamCalls atomic.Int32
	upstreamCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, outputGuardTestDelta("msg_guard_relay_stream", overflow))
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
			close(upstreamCanceled)
		case <-time.After(3 * time.Second):
		}
	}))
	t.Cleanup(upstream.Close)

	handler := newRelayResponsesGuardHandler(t, upstream.URL)
	c, recorder := newOutputGuardResponsesTestContext(t, `{"model":"gpt-4.1-direct","stream":true,"input":"test"}`)

	handler.Responses(c)

	body := outputGuardTestBodySummary(recorder.Body.String())
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("expected status 502 before first token for relay stream, got %d, body: %s", recorder.Code, body)
	}
	if code := decodeOutputGuardErrorCode(t, recorder.Body.Bytes()); code != outputguard.TriggeredCode {
		t.Fatalf("expected error code %q, got %q; body=%s", outputguard.TriggeredCode, code, body)
	}
	if strings.Contains(recorder.Body.String(), "[DONE]") {
		t.Fatalf("tripped relay stream MUST NOT contain the [DONE] sentinel, body: %s", body)
	}
	if strings.Contains(recorder.Body.String(), overflow[:64]) {
		t.Fatalf("tripped relay stream leaked upstream model output, body: %s", body)
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 upstream call, got %d", got)
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("expected relay upstream request context to be canceled immediately upon trip")
	}
}

// TestResponsesRelayNonStreamGuardTripped 证明 relay-style Responses 非流式
// 分支在完整响应快照（status=completed）超过 channel 预算时，绝不把聚合结果
// 写成 200，而是返回 502 脱敏 envelope。
func TestResponsesRelayNonStreamGuardTripped(t *testing.T) {
	setupOutputGuardHTTPTestEnv(t)

	overflow := relayResponsesGuardOverflow(902)
	responseBody, err := json.Marshal(map[string]any{
		"id":     "resp_guard_relay_nonstream",
		"object": "response",
		"status": "completed",
		"output": []any{
			map[string]any{
				"type": "message",
				"id":   "msg_guard_relay_nonstream",
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "output_text", "text": overflow},
				},
			},
		},
		"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
	})
	if err != nil {
		t.Fatalf("marshal relay non-stream response: %v", err)
	}

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(responseBody)
	}))
	t.Cleanup(upstream.Close)

	handler := newRelayResponsesGuardHandler(t, upstream.URL)
	c, recorder := newOutputGuardResponsesTestContext(t, `{"model":"gpt-4.1-direct","stream":false,"input":"test"}`)

	handler.Responses(c)

	body := outputGuardTestBodySummary(recorder.Body.String())
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("expected status 502 for tripped relay non-stream response, got %d, body: %s", recorder.Code, body)
	}
	if code := decodeOutputGuardErrorCode(t, recorder.Body.Bytes()); code != outputguard.TriggeredCode {
		t.Fatalf("expected error code %q, got %q; body=%s", outputguard.TriggeredCode, code, body)
	}
	if strings.Contains(recorder.Body.String(), overflow[:64]) {
		t.Fatalf("tripped relay non-stream response leaked upstream model output, body: %s", body)
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 upstream call, got %d", got)
	}
}

// TestResponsesRelayReadinessGate503 证明冷启动门禁与分支无关：配置未就绪时
// relay-style 账号同样在账号调度前被 503 强阻断，不会触达上游。
func TestResponsesRelayReadinessGate503(t *testing.T) {
	ResetOutputGuardStateForTest()
	t.Cleanup(ResetOutputGuardStateForTest)
	recordOutputGuardConfigSyncError(errors.New("cold start: output guard settings unavailable"))

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, outputGuardTestEvent(map[string]any{
			"type":     "response.completed",
			"response": map[string]any{"id": "resp_guard_relay_ready", "status": "completed"},
		}))
	}))
	t.Cleanup(upstream.Close)

	handler := newRelayResponsesGuardHandler(t, upstream.URL)
	c, recorder := newOutputGuardResponsesTestContext(t, `{"model":"gpt-4.1-direct","stream":true,"input":"test"}`)

	handler.Responses(c)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status 503 while guard is not ready, got %d; body=%s", recorder.Code, outputGuardTestBodySummary(recorder.Body.String()))
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Fatalf("readiness gate must block before upstream dispatch, got %d calls", got)
	}
}
