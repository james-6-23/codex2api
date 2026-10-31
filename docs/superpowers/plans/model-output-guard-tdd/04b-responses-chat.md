# 04b-Responses-Chat: Responses HTTP 与 ChatCompletions 入口 Guard 接线 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 实现 Codex2API `/v1/responses`（HTTP 流式 SSE 与非流式聚合）和 `/v1/chat/completions`（流式 SSE 与非流式聚合）业务入口的 Model Output Guard 接线，在前置检查点阻断失控事件流，原子协调上游取消、在途缓冲清理与下游 502 终态交付。

**Architecture:** 
1. 请求级就绪与状态机初始化：在 HTTP handler 入口最前执行 `RequireOutputGuardReadyHTTP` 门禁（未就绪返回 503 `api.ErrCodeServiceUnavailable`）；在 attempt 循环之外获取 `CurrentOutputGuardConfig()` 快照并初始化单请求生命周期的 `outputguard.RequestGuard`，保证跨 attempt 预算累计不重置。
2. 前置判定流水线：在 Responses SSE `forwardWithEvent` / 非流式收集以及 ChatCompletions SSE `streamTranslator` 累积之前，先于 `bytes.Clone` provenance 记录与 collector 累加调用 `ObserveEvent(guard, projector, eventType, data)`。Chat 入口针对上游 Responses SSE 采用 `ResponsesProjector`，紧接在 translator 之前；Grok native 分支明确委托未来 04c，严禁误挂。
3. 原子终止与下游交付：命中时通过 04a `GuardTerminationCoordinator.HandleTripped` 原子执行上游 cancel 与 body close，清空 provenance/collector/replay/writer 本地缓冲；调用 `SendDownstreamGuardTerminalHTTP` 向下游写入协议特化 502 错误块；通过 `OverlayGuardOutcome` 锁定 502 终态，消除 499 (客户端取消) 与 598 (断流) 覆盖，并向外层返回 `guardTripped` 阻断重试。

**Tech Stack:** Go 1.26+, `github.com/codex2api/security/outputguard`, `github.com/codex2api/api`, `github.com/gin-gonic/gin`, `github.com/tidwall/gjson`.

**Spec:** `docs/superpowers/specs/2026-10-28-model-output-guard-design.md` (§2, §4.1-4.3, §5.3, §7)

---

## Global Constraints

- **范围精确限定**：仅实施 Task 10A（Responses HTTP 流式/非流式、ChatCompletions 流式/非流式）；WebSocket/Realtime 留给后续 04d，Messages/Gemini/Grok Native 留给 04c，全局调度恢复阻断留给 04e。
- **冷启动与就绪检查**：未就绪返回 HTTP 503 (`api.ErrCodeServiceUnavailable`)；就绪后 Guard 命中返回 HTTP 502 (`model_output_guard_triggered`)。
- **状态机生命周期**：`RequestGuard` 必须在 attempt 重试循环外部单次初始化，跨 attempt 保持累计事件和字节配额，严禁每次 attempt 重建。
- **前置检查点原则**：Guard 判定必须先于 `bytes.Clone` provenance 记录、collector 累积、Chat streamTranslator 转换与 downstreamMu 锁内写入。命中事件对后续逻辑绝对不可见。
- **终止原语契约（严格消费 04a）**：消费 `NewGuardTerminationCoordinator`, `HandleTripped`, `OverlayGuardOutcome`, `SendDownstreamGuardTerminalHTTP`, `streamWriter.DiscardBuffered`。不发明未知函数名。
- **下游交付与覆盖保护**：首 token 前返回标准 502 JSON 错误响应；首 token 后写入流内 502 错误块，严禁写出 `data: [DONE]\n\n`；`OverlayGuardOutcome` 保证 502 不被 499 或 598 错误覆盖。
- **测试真实性**：集成测试使用真实 `httptest.Server` (端口 >= 9215)、真实 `auth.NewStore`、真实 upstream calls == 1 断言，禁止以局部布尔变量自证。

---

## File Structure

| 文件路径 | 状态 | 职责描述 |
|---|---|---|
| `proxy/handler.go` | 修改 (锚点: 4030, 5520-5620, 5800-5880, 7035, 7695-7775, 7870-7940) | 在 `Responses` 与 `ChatCompletions` 的流式/非流式关键路径注入 Guard 就绪门禁、状态机生命周期管理、前置 ObserveEvent 拦截、终止协调与 502 终态交付 |
| `proxy/outputguard_http_chat_test.go` | 新建 | Responses HTTP 与 ChatCompletions 的 TDD 集成测试（首 token 前 502 JSON、首 token 后流内 502 无 DONE、非流式 502 阻断、冷启动 503 门禁、跨 attempt 预算继承） |

---

## Tasks

### Task 10A: Responses HTTP 与 ChatCompletions Guard 接线与生命周期整合

**Files:**
- Modify: `proxy/handler.go`
- Test: `proxy/outputguard_http_chat_test.go`

**Interfaces:**
- Consumes:
  - `security/outputguard` (来自 01-core.md):
    ```go
    type Config struct {
        Enabled               bool
        MaxToolArgumentBytes  int64
        MaxToolArgumentDeltas int
        MaxChannelBytes       int64
        MaxRequestBytes       int64
    }
    type RequestGuard struct { ... }
    func NewRequestGuard(cfg Config) *RequestGuard
    func (g *RequestGuard) IsTripped() bool
    func (g *RequestGuard) TrippedReason() string
    ```
  - `proxy` (来自 02b-runtime.md):
    ```go
    func RequireOutputGuardReadyHTTP(c *gin.Context) bool
    func CurrentOutputGuardConfig() OutputGuardAppliedConfig
    ```
  - `proxy` (来自 03-protocol.md):
    ```go
    func NewResponsesProjector() *ResponsesProjector
    func ObserveEvent(guard *outputguard.RequestGuard, projector ProtocolProjector, eventType string, data []byte) error
    ```
  - `proxy` (来自 04a-termination 原语契约):
    ```go
    func NewGuardTerminationCoordinator(guard *outputguard.RequestGuard, cancelUpstream context.CancelFunc, body io.Closer) *GuardTerminationCoordinator
    func (c *GuardTerminationCoordinator) HandleTripped(reason string, discardBuffers ...func()) streamOutcome
    func OverlayGuardOutcome(outcome streamOutcome, guard *outputguard.RequestGuard) streamOutcome
    func SendDownstreamGuardTerminalHTTP(c *gin.Context, mu *sync.Mutex, clientGone *bool, protocol string, reason string) error
    func (w *streamFlushWriter) DiscardBuffered()
    ```
- Produces:
  - Responses HTTP 流式与非流式 Guard 阻断闭环（502 终态，上游即时取消，无脏数据落库与缓存）
  - ChatCompletions 流式与非流式 Guard 阻断闭环（502 流内错误，无 `[DONE]` 尾标）

---

- [ ] **Step 1: Write failing integration tests for Responses HTTP and ChatCompletions Guard wiring**

```go
// proxy/outputguard_http_chat_test.go
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
)

func init() {
	gin.SetMode(gin.TestMode)
}

func setupTestGuardEnv(t *testing.T, enabled bool, maxBytes int64) {
	t.Helper()
	ResetOutputGuardStateForTest()
	t.Cleanup(ResetOutputGuardStateForTest)

	enabledVal := enabled
	settings := database.OutputGuardSettings{
		Enabled:               &enabledVal,
		MaxToolArgumentBytes:  maxBytes,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       maxBytes,
		MaxRequestBytes:       maxBytes * 2,
		ConfigGeneration:      1,
	}
	ApplyOutputGuardSettings(settings)
}

// 1. Responses HTTP 流式在首 token 前触发 Guard: 返回 HTTP 502 JSON 错误响应，上游即时 Cancel
func TestResponsesHTTPStreamGuardTrippedPreToken(t *testing.T) {
	setupTestGuardEnv(t, true, 100) // 极小 100 字节阈值用于触发

	var upstreamCalls atomic.Int32
	var bodyClosed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		// 发送超过 100 字节的输出事件
		largeText := strings.Repeat("a", 150)
		event := `{"type":"response.output_text.delta","item_id":"msg_1","delta":"` + largeText + `"}`
		_, _ = io.WriteString(w, "data: "+event+"\n\n")
		flusher.Flush()
		<-r.Context().Done()
		bodyClosed.Store(true)
	}))
	defer server.Close()

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, MaxRetries: 0})
	defer store.Stop()
	store.AddAccount(&auth.Account{DBID: 1, PlanType: "pro", AccountID: "acct-1", BaseURL: server.URL})

	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"test"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Responses(c)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("expected status 502, got %d, body: %s", recorder.Code, recorder.Body.String())
	}
	var errResp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal error response: %v", err)
	}
	if errResp.Error.Code != outputguard.TriggeredCode {
		t.Fatalf("expected code %s, got %s", outputguard.TriggeredCode, errResp.Error.Code)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("expected exactly 1 upstream call, got %d", upstreamCalls.Load())
	}
	time.Sleep(50 * time.Millisecond)
	if !bodyClosed.Load() {
		t.Fatalf("expected upstream body/context canceled immediately upon trip")
	}
}

// 2. ChatCompletions 流式在产生输出后触发 Guard: 下游流内接收 502 error 帧，且绝无 [DONE] 标记
func TestChatCompletionsStreamGuardTrippedPostTokenNoDone(t *testing.T) {
	setupTestGuardEnv(t, true, 200)

	var upstreamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		// 1. 发送首 token 正常文本
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"delta\":\"Hello \"}\n\n")
		flusher.Flush()
		time.Sleep(10 * time.Millisecond)
		// 2. 发送超额溢出内容
		overflow := strings.Repeat("x", 300)
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"delta\":\""+overflow+"\"}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer server.Close()

	SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "test"})
	defer SetResinConfig(nil)

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, MaxRetries: 0})
	defer store.Stop()
	store.AddAccount(&auth.Account{DBID: 1, PlanType: "pro", AccountID: "acct-1"})

	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ChatCompletions(c)

	body := recorder.Body.String()
	if !strings.Contains(body, "Hello ") {
		t.Fatalf("expected initial token to be visible, got: %s", body)
	}
	if !strings.Contains(body, outputguard.TriggeredCode) {
		t.Fatalf("expected stream error containing %s, got: %s", outputguard.TriggeredCode, body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("tripped stream MUST NOT contain [DONE] sentinel, got: %s", body)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("expected exactly 1 upstream call, got %d", upstreamCalls.Load())
	}
}

// 3. Responses HTTP 非流式请求超额快照触发: 阻断写入与缓存，返回 502
func TestResponsesHTTPNonStreamGuardTripped(t *testing.T) {
	setupTestGuardEnv(t, true, 100)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// 非流式上游 SSE 传入超过限制的 output
		largeText := strings.Repeat("z", 150)
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"delta\":\""+largeText+"\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\"}}\n\n")
	}))
	defer server.Close()

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, MaxRetries: 0})
	defer store.Stop()
	store.AddAccount(&auth.Account{DBID: 1, PlanType: "pro", AccountID: "acct-1", BaseURL: server.URL})

	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":false,"input":"test"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Responses(c)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 status, got %d, body: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), outputguard.TriggeredCode) {
		t.Fatalf("expected error body containing triggered code, got: %s", recorder.Body.String())
	}
}

// 4. 冷启动未就绪 503 阻断测试
func TestOutputGuardHTTPReadinessGate503(t *testing.T) {
	ResetOutputGuardStateForTest()
	t.Cleanup(ResetOutputGuardStateForTest)

	handler := NewHandler(nil, nil, &config.Config{AllowAnonymousV1: true}, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5"}`))

	handler.Responses(c)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status 503 for unready guard, got %d", recorder.Code)
	}
}
```

- [ ] **Step 2: Run test to verify failure (Red)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./proxy -run "TestResponsesHTTPStreamGuard.*|TestChatCompletionsStreamGuard.*|TestResponsesHTTPNonStreamGuard.*|TestOutputGuardHTTPReadinessGate503"`
Expected: FAIL with compilation or missing gate/wiring errors.

- [ ] **Step 3: Implement minimal code for Responses and ChatCompletions wiring**

在 `proxy/handler.go` 中按以下锚点执行精确修改：

**1. `Responses` 入口就绪门禁与 Guard 状态机单请求生命周期初始化：**
锚点：`proxy/handler.go:4030-4035`（在 `readRawRequestBody` 和 `contextErr` 检查之后，`compactionMeta` 之前）：
```go
	// [OutputGuard 门禁与状态机初始化] 必须在 attempt 循环外初始化，跨 attempt 不重置
	if !RequireOutputGuardReadyHTTP(c) {
		return
	}
	guardSnapshot := CurrentOutputGuardConfig()
	reqGuard := outputguard.NewRequestGuard(outputguard.Config{
		Enabled:               guardSnapshot.Enabled,
		MaxToolArgumentBytes:  guardSnapshot.MaxToolArgumentBytes,
		MaxToolArgumentDeltas: guardSnapshot.MaxToolArgumentDeltas,
		MaxChannelBytes:       guardSnapshot.MaxChannelBytes,
		MaxRequestBytes:       guardSnapshot.MaxRequestBytes,
	})
```

**2. `Responses` 流式 forwardWithEvent 前置 Guard 检查与原子终止：**
锚点：`proxy/handler.go:5538-5550`：
```go
			guardCoord := NewGuardTerminationCoordinator(reqGuard, cancelUpstream, resp.Body)
			responsesProjector := NewResponsesProjector()
			forwardWithEvent := func(sseEvent string, data []byte) bool {
				if multiAgentV2State.isOptimizedFor(account.ID()) {
					data = restoreCodexMultiAgentV2Response(data, multiAgentV2State)
				}
				streamDiag.markUpstreamFrame()

				// [OutputGuard 前置检查] 必须在 provenance bytesClone/collector/replay 之前
				eventType := normalizedUpstreamSSEEventType(sseEvent, data)
				if err := ObserveEvent(reqGuard, responsesProjector, eventType, data); err != nil {
					coordOutcome := guardCoord.HandleTripped(reqGuard.TrippedReason(), func() {
						compactionProvenancePayloads = nil
						outputCollector.Reset()
						streamWriter.DiscardBuffered()
						completedResponseData = nil
					})
					outcome = coordOutcome
					_ = SendDownstreamGuardTerminalHTTP(c, &downstreamMu, &clientGone, "responses", reqGuard.TrippedReason())
					return false
				}

				if continuousRetryBuffersAttempts(continuousRetryPolicy) {
					compactionProvenancePayloads = append(compactionProvenancePayloads, bytes.Clone(data))
				} else {
					h.recordCompactionProvenanceFromPayload(context.Background(), account, data)
				}
				downstreamMu.Lock()
				defer downstreamMu.Unlock()
```

**3. `Responses` 非流式 SSE 聚合与收尾 Snapshot 检查：**
锚点：`proxy/handler.go:5801-5815`：
```go
			readErr = readSSEStreamWithContinuousRetryKeepalive(readCtx, resp.Body, func(sseEvent string, data []byte) bool {
				if multiAgentV2State.isOptimizedFor(account.ID()) {
					data = restoreCodexMultiAgentV2Response(data, multiAgentV2State)
				}
				// [OutputGuard 前置检查]
				eventType := normalizedUpstreamSSEEventType(sseEvent, data)
				if err := ObserveEvent(reqGuard, responsesProjector, eventType, data); err != nil {
					coordOutcome := guardCoord.HandleTripped(reqGuard.TrippedReason(), func() {
						compactionProvenancePayloads = nil
						outputCollector.Reset()
						completedResponseData = nil
					})
					outcome = coordOutcome
					return false
				}

				if continuousRetryBuffersAttempts(continuousRetryPolicy) {
					compactionProvenancePayloads = append(compactionProvenancePayloads, bytes.Clone(data))
				} else {
					h.recordCompactionProvenanceFromPayload(context.Background(), account, data)
				}
```
锚点：`proxy/handler.go:5873-5875`（在 `classifyStreamOutcome` 之后叠加 Outcome）：
```go
		outcome = classifyStreamOutcome(continuousRetryContextError(c.Request.Context()), readErr, writeErr, gotTerminal)
		outcome = OverlayGuardOutcome(outcome, reqGuard)
```
并在下游发送错误逻辑（锚点：`proxy/handler.go:6021-6035`）中增补 guardTripped 502 分支：
```go
		if outcome.guardTripped && !downstreamWrote {
			c.Header("Content-Type", "application/json; charset=utf-8")
			c.JSON(http.StatusBadGateway, gin.H{
				"error": gin.H{
					"message": "Model output guard triggered: " + outcome.guardReason,
					"type":    outputguard.TriggeredCode,
					"code":    outputguard.TriggeredCode,
				},
			})
			return
		}
```

**4. `ChatCompletions` 入口就绪门禁与 Guard 状态机初始化：**
锚点：`proxy/handler.go:7035-7040`：
```go
	// [OutputGuard 门禁与状态机初始化] 必须在 attempt 循环外初始化
	if !RequireOutputGuardReadyHTTP(c) {
		return
	}
	guardSnapshot := CurrentOutputGuardConfig()
	reqGuard := outputguard.NewRequestGuard(outputguard.Config{
		Enabled:               guardSnapshot.Enabled,
		MaxToolArgumentBytes:  guardSnapshot.MaxToolArgumentBytes,
		MaxToolArgumentDeltas: guardSnapshot.MaxToolArgumentDeltas,
		MaxChannelBytes:       guardSnapshot.MaxChannelBytes,
		MaxRequestBytes:       guardSnapshot.MaxRequestBytes,
	})
```

**5. `ChatCompletions` 流式在 streamTranslator 转换之前前置拦截：**
锚点：`proxy/handler.go:7695-7710`（跳过 Grok native 分支，仅在 Responses 上游前置检查）：
```go
			guardCoord := NewGuardTerminationCoordinator(reqGuard, cancelUpstream, resp.Body)
			chatResponsesProjector := NewResponsesProjector() // 上游是 Responses SSE
			readErr = readSSEStreamWithContinuousRetryKeepalive(readCtx, resp.Body, func(sseEvent string, data []byte) bool {
				eventType := normalizedUpstreamSSEEventType(sseEvent, data)

				// [OutputGuard 前置检查] 必须在 streamTranslator 累加之前
				if err := ObserveEvent(reqGuard, chatResponsesProjector, eventType, data); err != nil {
					coordOutcome := guardCoord.HandleTripped(reqGuard.TrippedReason(), func() {
						outputCollector.Reset()
						streamWriter.DiscardBuffered()
					})
					outcome = coordOutcome
					_ = SendDownstreamGuardTerminalHTTP(c, &downstreamMu, &clientGone, "chat", reqGuard.TrippedReason())
					return false
				}

				parsed := gjson.ParseBytes(data)
				ApplyCodexUsageEventFrame(h.store, account, eventType, parsed, time.Now())
```

**6. `ChatCompletions` 非流式在 fullContent/fullReasoning 累加前拦截与 Outcome 叠加：**
锚点：`proxy/handler.go:7870-7880`：
```go
				eventType := normalizedUpstreamSSEEventType(sseEvent, data)
				if err := ObserveEvent(reqGuard, chatResponsesProjector, eventType, data); err != nil {
					coordOutcome := guardCoord.HandleTripped(reqGuard.TrippedReason(), func() {
						outputCollector.Reset()
					})
					outcome = coordOutcome
					return false
				}
```
锚点：`proxy/handler.go:7926-7930`（在 `classifyStreamOutcome` 之后）：
```go
		outcome = classifyStreamOutcome(continuousRetryContextError(c.Request.Context()), readErr, writeErr, gotTerminal)
		outcome = OverlayGuardOutcome(outcome, reqGuard)
```
并在退出写出逻辑前判定：
```go
		if outcome.guardTripped && !downstreamWrote {
			c.Header("Content-Type", "application/json; charset=utf-8")
			c.JSON(http.StatusBadGateway, gin.H{
				"error": gin.H{
					"message": "Model output guard triggered: " + outcome.guardReason,
					"type":    outputguard.TriggeredCode,
					"code":    outputguard.TriggeredCode,
				},
			})
			return
		}
```

- [ ] **Step 4: Run test to verify passing (Green)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./proxy -run "TestResponsesHTTPStreamGuard.*|TestChatCompletionsStreamGuard.*|TestResponsesHTTPNonStreamGuard.*|TestOutputGuardHTTPReadinessGate503"`
Expected: PASS ok github.com/codex2api/proxy

- [ ] **Step 5: Commit**

```bash
git add proxy/handler.go proxy/outputguard_http_chat_test.go
git commit -m "feat(proxy): wire model output guard into responses and chat HTTP endpoints"
```
