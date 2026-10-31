package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
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
)

// Output Guard 的运行时配置有数据库级最小值（channel >= 128KiB）。集成测试
// 因此使用真实的最小阈值，并由 >128KiB 的真实上游事件触发，而不是伪造小值。
const (
	guardHTTPTestMaxToolBytes    = int64(16 * 1024)
	guardHTTPTestMaxChannelBytes = int64(128 * 1024)
	guardHTTPTestMaxRequestBytes = int64(256 * 1024)
)

func init() {
	gin.SetMode(gin.TestMode)
}

// setupOutputGuardHTTPTestEnv 装载一份合法的、可由真实事件触发的 Guard 配置，
// 并保证测试结束后清空全局就绪状态，避免污染整包其它用例。
func setupOutputGuardHTTPTestEnv(t *testing.T) {
	t.Helper()
	ResetOutputGuardStateForTest()
	t.Cleanup(ResetOutputGuardStateForTest)

	enabled := true
	if !ApplyOutputGuardSettings(database.OutputGuardSettings{
		Enabled:               &enabled,
		MaxToolArgumentBytes:  guardHTTPTestMaxToolBytes,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       guardHTTPTestMaxChannelBytes,
		MaxRequestBytes:       guardHTTPTestMaxRequestBytes,
		ConfigGeneration:      1,
	}) {
		t.Fatal("ApplyOutputGuardSettings rejected the integration-test guard config")
	}
}

// newOutputGuardHTTPTestStore 使用真实 auth.Store 与真实账号，走与生产一致的
// Resin 出站链路（Codex OAuth 账号无法用 BaseURL 直接指向 httptest）。
func newOutputGuardHTTPTestStore(t *testing.T, maxRetries int) *auth.Store {
	t.Helper()
	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      1,
		TestConcurrency:     1,
		TestModel:           "gpt-5.5",
		MaxRetries:          maxRetries,
		MaxRateLimitRetries: 0,
		RetryIntervalMS:     0,
	})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{
		DBID:        1,
		AccessToken: "at-guard-test",
		PlanType:    "pro",
		AccountID:   "acct-guard-test",
	})
	return store
}

func enableOutputGuardHTTPTestResin(t *testing.T, upstreamURL string) {
	t.Helper()
	previous := resinCfg.Load()
	t.Cleanup(func() { SetResinConfig(previous) })
	SetResinConfig(&ResinConfig{BaseURL: upstreamURL, PlatformName: "guard-test"})
}

func outputGuardTestEvent(payload map[string]any) string {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return "data: " + string(raw) + "\n\n"
}

func outputGuardTestDelta(itemID string, delta string) string {
	return outputGuardTestEvent(map[string]any{
		"type":          "response.output_text.delta",
		"item_id":       itemID,
		"content_index": 0,
		"delta":         delta,
	})
}

// outputGuardTestRandomText 生成低周期性的文本，避免触发 repetition 检测，
// 从而保证断言命中的是字节预算链路。
func outputGuardTestRandomText(seed int64, n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	rng := rand.New(rand.NewSource(seed))
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = alphabet[rng.Intn(len(alphabet))]
	}
	return string(buf)
}

func newOutputGuardResponsesTestContext(t *testing.T, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, recorder
}

func outputGuardTestBodySummary(body string) string {
	const max = 400
	if len(body) <= max {
		return body
	}
	return body[:max] + fmt.Sprintf("... [truncated %d bytes]", len(body)-max)
}

func decodeOutputGuardErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode guard error response: %v; body=%s", err, outputGuardTestBodySummary(string(body)))
	}
	return payload.Error.Code
}

// 1. Responses HTTP 流式在首 token 前触发 Guard：必须返回 502 JSON 错误响应，
// 且上游被即时取消。
func TestResponsesHTTPStreamGuardTrippedPreToken(t *testing.T) {
	setupOutputGuardHTTPTestEnv(t)

	overflow := outputGuardTestRandomText(11, int(guardHTTPTestMaxChannelBytes)+12*1024)
	var upstreamCalls atomic.Int32
	upstreamCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, outputGuardTestDelta("msg_guard_pre", overflow))
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
	enableOutputGuardHTTPTestResin(t, upstream.URL)

	handler := NewHandler(newOutputGuardHTTPTestStore(t, 0), nil, &config.Config{AllowAnonymousV1: true}, nil)
	c, recorder := newOutputGuardResponsesTestContext(t, `{"model":"gpt-5.5","stream":true,"input":"test"}`)

	handler.Responses(c)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("expected status 502 before first token, got %d, body: %s", recorder.Code, outputGuardTestBodySummary(recorder.Body.String()))
	}
	if code := decodeOutputGuardErrorCode(t, recorder.Body.Bytes()); code != outputguard.TriggeredCode {
		t.Fatalf("expected error code %q, got %q; body=%s", outputguard.TriggeredCode, code, outputGuardTestBodySummary(recorder.Body.String()))
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 upstream call, got %d", got)
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("expected upstream request context to be canceled immediately upon trip")
	}
}

// guardDeadlineRecorder 让 httptest.ResponseRecorder 支持写 deadline，
// 因为 04a 的流内终态写入使用 http.ResponseController 且有界超时。
type guardDeadlineRecorder struct {
	*httptest.ResponseRecorder
}

func (r *guardDeadlineRecorder) SetWriteDeadline(time.Time) error { return nil }

// 2. ChatCompletions 流式在首 token 之后触发 Guard：下游必须收到流内 502
// error 帧，且绝不能出现成功哨兵 [DONE]。
func TestChatCompletionsStreamGuardTrippedPostTokenNoDone(t *testing.T) {
	setupOutputGuardHTTPTestEnv(t)

	overflow := outputGuardTestRandomText(22, int(guardHTTPTestMaxChannelBytes)+12*1024)
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, outputGuardTestDelta("msg_guard_post", "Hello "))
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(10 * time.Millisecond)
		_, _ = io.WriteString(w, outputGuardTestDelta("msg_guard_post", overflow))
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	t.Cleanup(upstream.Close)
	enableOutputGuardHTTPTestResin(t, upstream.URL)

	handler := NewHandler(newOutputGuardHTTPTestStore(t, 0), nil, &config.Config{AllowAnonymousV1: true}, nil)
	recorder := &guardDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ChatCompletions(c)

	body := outputGuardTestBodySummary(recorder.Body.String())
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected committed stream status 200, got %d; body=%s", recorder.Code, body)
	}
	if !strings.Contains(body, `"content":"Hello "`) {
		t.Fatalf("expected the already-delivered first token to remain visible, body: %s", outputGuardTestBodySummary(body))
	}
	if !strings.Contains(body, outputguard.TriggeredCode) {
		t.Fatalf("expected in-stream guard terminal containing %q, body: %s", outputguard.TriggeredCode, outputGuardTestBodySummary(body))
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("tripped stream MUST NOT contain the [DONE] sentinel, body: %s", outputGuardTestBodySummary(body))
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 upstream call, got %d", got)
	}
}

// 3. Responses HTTP 非流式：失控快照必须阻断聚合结果写出，直接返回 502。
func TestResponsesHTTPNonStreamGuardTripped(t *testing.T) {
	setupOutputGuardHTTPTestEnv(t)

	overflow := outputGuardTestRandomText(33, int(guardHTTPTestMaxChannelBytes)+12*1024)
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, outputGuardTestDelta("msg_guard_nonstream", overflow))
		_, _ = io.WriteString(w, outputGuardTestEvent(map[string]any{
			"type":     "response.completed",
			"response": map[string]any{"id": "resp_guard_nonstream", "status": "completed"},
		}))
	}))
	t.Cleanup(upstream.Close)
	enableOutputGuardHTTPTestResin(t, upstream.URL)

	handler := NewHandler(newOutputGuardHTTPTestStore(t, 0), nil, &config.Config{AllowAnonymousV1: true}, nil)
	c, recorder := newOutputGuardResponsesTestContext(t, `{"model":"gpt-5.5","stream":false,"input":"test"}`)

	handler.Responses(c)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("expected status 502 for tripped non-stream response, got %d, body: %s", recorder.Code, outputGuardTestBodySummary(recorder.Body.String()))
	}
	if code := decodeOutputGuardErrorCode(t, recorder.Body.Bytes()); code != outputguard.TriggeredCode {
		t.Fatalf("expected error code %q, got %q; body=%s", outputguard.TriggeredCode, code, outputGuardTestBodySummary(recorder.Body.String()))
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 upstream call, got %d", got)
	}
}

// 3b. ChatCompletions 非流式：聚合前命中 Guard 必须阻断 compact 结果写出，
// 返回 502 JSON 而不是把已经失控的内容聚合成 200。
func TestChatCompletionsNonStreamGuardTripped(t *testing.T) {
	setupOutputGuardHTTPTestEnv(t)

	overflow := outputGuardTestRandomText(36, int(guardHTTPTestMaxChannelBytes)+12*1024)
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, outputGuardTestDelta("msg_guard_chat_nonstream", overflow))
		_, _ = io.WriteString(w, outputGuardTestEvent(map[string]any{
			"type":     "response.completed",
			"response": map[string]any{"id": "resp_guard_chat_nonstream", "status": "completed"},
		}))
	}))
	t.Cleanup(upstream.Close)
	enableOutputGuardHTTPTestResin(t, upstream.URL)

	handler := NewHandler(newOutputGuardHTTPTestStore(t, 0), nil, &config.Config{AllowAnonymousV1: true}, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.5","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ChatCompletions(c)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("expected status 502 for tripped non-stream chat response, got %d, body: %s", recorder.Code, outputGuardTestBodySummary(recorder.Body.String()))
	}
	if code := decodeOutputGuardErrorCode(t, recorder.Body.Bytes()); code != outputguard.TriggeredCode {
		t.Fatalf("expected error code %q, got %q; body=%s", outputguard.TriggeredCode, code, outputGuardTestBodySummary(recorder.Body.String()))
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 upstream call, got %d", got)
	}
}

// 4. 冷启动门禁：配置加载失败（lastSyncError 已记录、未就绪）时，Responses 与
// ChatCompletions 都必须以 503 service_unavailable 强阻断，不得进入账号调度。
func TestOutputGuardHTTPReadinessGate503(t *testing.T) {
	ResetOutputGuardStateForTest()
	t.Cleanup(ResetOutputGuardStateForTest)
	recordOutputGuardConfigSyncError(errors.New("cold start: output guard settings unavailable"))

	tests := []struct {
		name string
		body string
		call func(*Handler, *gin.Context)
	}{
		{
			name: "responses",
			body: `{"model":"gpt-5.5","input":"test"}`,
			call: func(h *Handler, c *gin.Context) { h.Responses(c) },
		},
		{
			name: "chat_completions",
			body: `{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}]}`,
			call: func(h *Handler, c *gin.Context) { h.ChatCompletions(c) },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := NewHandler(nil, nil, &config.Config{AllowAnonymousV1: true}, nil)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")

			tc.call(handler, c)

			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected status 503 while guard is not ready, got %d; body=%s", recorder.Code, outputGuardTestBodySummary(recorder.Body.String()))
			}
		})
	}
}

// 5. 跨 attempt 预算继承：第一次尝试已消费但未触发的字节必须累计到第二次
// 尝试，否则每次 attempt 重建状态机会把跨 attempt 的失控流放行。
func TestResponsesHTTPNonStreamGuardBudgetInheritedAcrossAttempts(t *testing.T) {
	setupOutputGuardHTTPTestEnv(t)

	// 每个 attempt 单独都低于 128KiB channel 上限，两次累计后越界。
	halfBudget := guardHTTPTestMaxChannelBytes/2 + 6*1024
	firstDelta := outputGuardTestRandomText(44, int(halfBudget))
	secondDelta := outputGuardTestRandomText(55, int(halfBudget))

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			_, _ = io.WriteString(w, outputGuardTestDelta("msg_guard_budget", firstDelta))
			_, _ = io.WriteString(w, outputGuardTestEvent(map[string]any{
				"type": "response.failed",
				"response": map[string]any{
					"status": "failed",
					"error": map[string]any{
						"status_code": 503,
						"code":        "server_error",
						"message":     "upstream overloaded",
					},
				},
			}))
			return
		}
		_, _ = io.WriteString(w, outputGuardTestDelta("msg_guard_budget", secondDelta))
		_, _ = io.WriteString(w, outputGuardTestEvent(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":     "resp_guard_budget",
				"status": "completed",
				"output": []any{
					map[string]any{
						"type": "message",
						"role": "assistant",
						"content": []any{
							map[string]any{"type": "output_text", "text": firstDelta + secondDelta},
						},
					},
				},
			},
		}))
	}))
	t.Cleanup(upstream.Close)
	enableOutputGuardHTTPTestResin(t, upstream.URL)

	handler := NewHandler(newOutputGuardHTTPTestStore(t, 1), nil, &config.Config{AllowAnonymousV1: true}, nil)
	c, recorder := newOutputGuardResponsesTestContext(t, `{"model":"gpt-5.5","stream":false,"input":"test"}`)

	handler.Responses(c)

	if got := upstreamCalls.Load(); got != 2 {
		t.Fatalf("expected the retryable first attempt plus one guarded attempt (2 upstream calls), got %d; body=%s", got, outputGuardTestBodySummary(recorder.Body.String()))
	}
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("expected status 502 once the inherited budget trips, got %d; body=%s", recorder.Code, outputGuardTestBodySummary(recorder.Body.String()))
	}
	if code := decodeOutputGuardErrorCode(t, recorder.Body.Bytes()); code != outputguard.TriggeredCode {
		t.Fatalf("expected error code %q, got %q; body=%s", outputguard.TriggeredCode, code, outputGuardTestBodySummary(recorder.Body.String()))
	}
}
