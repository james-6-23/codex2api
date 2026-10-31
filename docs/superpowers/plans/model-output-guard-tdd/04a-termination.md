# 04a-Termination: 模型输出失控防护强制终止协调与下游通知原语 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 实现 Codex2API 模型输出失控防护强制终止协调器与下游通知原语（Task 9），提供上游 Context 立即主动取消与 Body 关闭（先于下游写锁并跳过 5s drain）、streamFlushWriter 缓冲与安全窗彻底丢弃（阻止后续 Flush/Finalize 释放）、race-safe 首 reason 冻结与 streamOutcome 错误叠加，以及持锁内 ResponseController 5s 有界写超时下游通知（支持 Responses SSE、Chat SSE、Anthropic SSE、Gemini 裸 JSON 502 与 WebSocket 帧）。

**Architecture:**
1. `proxy/handler.go`: 扩展 `streamOutcome` 字段 `guardTripped` 与 `guardReason`，新增 `OverlayGuardOutcome`，保证 guard 终态以 502、`model_output_guard`、`penalize=false`、`terminalLocal=true`、`failurePayload=nil` 绝对优先，消除 499/598 覆盖。
2. `proxy/stream_flush_writer.go`: 增加 `aborted bool` 标记与 `DiscardBuffered()` 原语，清空 buffer/scanCarry 并解绑 outputScanner，后续 `Flush()`/`Finalize()` 静默短路。
3. `proxy/outputguard_cancel.go`: 实现 `GuardTerminationCoordinator`，在 guard 命中时原子执行上游 cancel 与 body close、清空本地缓冲，`sync.Once` 保证并发调用原子幂等与首 reason 冻结。
4. `proxy/outputguard_downstream.go`: 实现 `ApplyDownstreamWriteDeadline`、`FormatGuardTerminalEnvelope`、`SendDownstreamGuardTerminalHTTP` 与 `SendDownstreamGuardTerminalWS`。未提交头返回 502 JSON；已提交流式头通过 `TryLock` 非阻塞获取写锁，锁内通过 `ResponseController` 施加 5s 超时，不支持或断开时安全退出，绝不引入孤儿 goroutine 与无界写；多协议错误完全脱敏。

**Tech Stack:** Go 1.26+ (`sync`, `sync/atomic`, `net/http`, `context`, `time`, `github.com/gin-gonic/gin`, `github.com/gorilla/websocket`, `github.com/codex2api/security/outputguard`).

**Spec:** `docs/superpowers/specs/2026-10-28-model-output-guard-design.md` §6, §9.2

## Global Constraints

- **范围限制**：仅实施 Task 9 终止协调与下游通知原语；不修改其它模块、现有 spec、业务源码、旧候选文件或 Git 提交。后续 Task 10 的具体流式 Handler 与连续重试/恢复阻断由独立模块承接。
- **不可妥协的设计取舍**：
  - **前置取消与锁解耦原则**：guard 命中时第一动作必须是在前主动 cancel 本次上游 context 并关闭响应体（跳过 5s drain），绝不等待写锁；下游通知属于尽力交付（best-effort），锁争用绝不延迟或阻塞上游 cancel 与资源回收。
  - **缓冲彻底丢弃**：调用 `DiscardBuffered()` 清空 `streamFlushWriter` 内部 buffer、scanCarry 与 outputScanner，并置位 `aborted`，后续 `Flush()` 与 `Finalize()` 绝不输出任何数据。
  - **Outcome 优先级与脱敏**：`OverlayGuardOutcome` 强制覆盖 499/598，`failurePayload` 置为 `nil`，`penalize=false`，`terminalLocal=true`，`logStatusCode=502`。
  - **有界写超时与不支持安全退出**：流式写入在获取 `downstreamMu` 写锁后，通过 `http.NewResponseController(w).SetWriteDeadline` 施加 5s 超时；若底层返回 `http.ErrNotSupported`，绝不尝试潜在无界阻塞的同步 write，立即放弃写入、置位 `clientGone` 并安全退出。
  - **禁止 Hijack 与孤儿协程**：Gin 在已提交头和 chunk 写入后不支持 Hijack；严禁通过启动孤儿 goroutine 拼死写入。
  - **多协议终态脱敏 Envelope 契约**：Responses SSE 为 `response.failed` 无 completed；Chat SSE 为 error 无 `[DONE]`；Anthropic SSE 为 `event: error` 无 `message_stop`；Gemini 为裸 JSON 502 UNAVAILABLE（符合 Native Gemini SDK 规范）；WebSocket 为带 stream_id 的 error 帧。

---

## File Structure

| 文件路径 | 状态 | 职责描述 |
|---|---|---|
| `proxy/handler.go` | 修改 | 在 `streamOutcome` 中扩展 `guardTripped` 与 `guardReason`；新增 `OverlayGuardOutcome` |
| `proxy/stream_flush_writer.go` | 修改 | 新增 `aborted`；实现 `DiscardBuffered()`；在 `Flush()`/`Finalize()` 检查 aborted |
| `proxy/outputguard_cancel.go` | 新建 | `GuardTerminationCoordinator` 协调先于写锁的主动 cancel、body close 与首 reason 冻结 |
| `proxy/outputguard_cancel_test.go` | 新建 | 验证前置取消先于下游写锁、并发触发原子幂等、DiscardBuffered 清空缓冲、Outcome 优先保留 |
| `proxy/outputguard_downstream.go` | 新建 | 实现 `ApplyDownstreamWriteDeadline`、`FormatGuardTerminalEnvelope` 与 HTTP/WS 终态写入 |
| `proxy/outputguard_downstream_test.go` | 新建 | 验证 TryLock 锁争用安全放弃、不支持 deadline 安全退出、未提交头 502、多协议 envelope 格式、WS 写入 |

---

## Tasks

### Task 9a: 终止协调器、上游立即取消与本地缓冲 Discard 原语

**Files:**
- Modify: `proxy/handler.go:2343-2375`
- Modify: `proxy/stream_flush_writer.go:23-45, 403-465`
- Create: `proxy/outputguard_cancel.go`
- Test: `proxy/outputguard_cancel_test.go`

**Interfaces:**
- Consumes: `github.com/codex2api/security/outputguard` (`Config`, `RequestGuard`, `NewRequestGuard`, `IsTripped`, `TrippedReason`)
- Produces:
  ```go
  package proxy
  import ("context"; "io"; "net/http"; "sync"; "github.com/codex2api/security/outputguard")
  type streamOutcome struct {
      logStatusCode int; failureKind string; failureMessage string; failurePayload []byte
      penalize bool; capacityShed bool; verifyAccountAuth bool; requestScoped bool
      modelUnsupported bool; terminalLocal bool; guardTripped bool; guardReason string
  }
  func OverlayGuardOutcome(outcome streamOutcome, guard *outputguard.RequestGuard) streamOutcome
  type GuardTerminationCoordinator struct {
      guard *outputguard.RequestGuard; cancelUpstream context.CancelFunc; body io.Closer
      once sync.Once; trippedOutcome streamOutcome
  }
  func NewGuardTerminationCoordinator(guard *outputguard.RequestGuard, cancelUpstream context.CancelFunc, body io.Closer) *GuardTerminationCoordinator
  func (c *GuardTerminationCoordinator) HandleTripped(reason string, discardBuffers ...func()) streamOutcome
  func (w *streamFlushWriter) DiscardBuffered()
  ```

- [ ] **Step 1: Write failing unit test for GuardTerminationCoordinator and buffer discard**

```go
// proxy/outputguard_cancel_test.go
package proxy

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/security/outputguard"
)

type recordingCloser struct {
	mu sync.Mutex; closed bool
}
func (r *recordingCloser) Close() error {
	r.mu.Lock(); defer r.mu.Unlock(); r.closed = true; return nil
}
func (r *recordingCloser) IsClosed() bool {
	r.mu.Lock(); defer r.mu.Unlock(); return r.closed
}

func TestGuardTerminationPrecedesDownstreamLock(t *testing.T) {
	cfg := outputguard.Config{Enabled: true, MaxToolArgumentBytes: 128 * 1024, MaxToolArgumentDeltas: 4096, MaxChannelBytes: 2 * 1024 * 1024, MaxRequestBytes: 4 * 1024 * 1024}
	rg := outputguard.NewRequestGuard(cfg)
	upstreamCtx, upstreamCancel := context.WithCancel(context.Background())
	defer upstreamCancel()
	closer := &recordingCloser{}
	coordinator := NewGuardTerminationCoordinator(rg, upstreamCancel, closer)

	var downstreamMu sync.Mutex
	downstreamMu.Lock()
	outcome := coordinator.HandleTripped("repetition", func() {})

	select {
	case <-upstreamCtx.Done():
	default:
		t.Fatalf("expected upstream context to be canceled immediately")
	}
	if !closer.IsClosed() { t.Fatalf("expected upstream body to be closed immediately") }
	if !outcome.guardTripped || outcome.guardReason != "repetition" || outcome.logStatusCode != http.StatusBadGateway || outcome.penalize {
		t.Fatalf("unexpected guard outcome: %+v", outcome)
	}
	downstreamMu.Unlock()
}

func TestGuardTerminationIdempotentReasonFreeze(t *testing.T) {
	cfg := outputguard.Config{Enabled: true, MaxToolArgumentBytes: 128 * 1024, MaxToolArgumentDeltas: 4096, MaxChannelBytes: 2 * 1024 * 1024, MaxRequestBytes: 4 * 1024 * 1024}
	rg := outputguard.NewRequestGuard(cfg)
	cancelCalls := int32(0)
	closer := &recordingCloser{}
	coordinator := NewGuardTerminationCoordinator(rg, func() { atomic.AddInt32(&cancelCalls, 1) }, closer)

	var discardCalls int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			reason := "repetition"
			if idx > 0 { reason = "channel_bytes" }
			out := coordinator.HandleTripped(reason, func() { atomic.AddInt32(&discardCalls, 1) })
			if out.guardReason != "repetition" { t.Errorf("expected frozen repetition, got %s", out.guardReason) }
		}(i)
	}
	wg.Wait()

	if calls := atomic.LoadInt32(&cancelCalls); calls != 1 { t.Fatalf("expected 1 cancel call, got %d", calls) }
	if !closer.IsClosed() { t.Fatalf("expected body closed") }
	if atomic.LoadInt32(&discardCalls) == 0 { t.Fatalf("expected discard callbacks executed") }
}

func TestStreamFlushWriterDiscardBuffered(t *testing.T) {
	var sink bytes.Buffer
	w := &streamFlushWriter{writer: &sink, policy: StreamFlushPolicyCoalesce, interval: time.Hour}
	_ = w.WriteBytes([]byte("data: partial runaway malicious repetition\n\n"))
	if sink.Len() > 0 { t.Fatalf("buffer should not flush prematurely") }

	w.DiscardBuffered()
	if err := w.Finalize(); err != nil { t.Fatalf("Finalize returned error: %v", err) }
	if sink.Len() != 0 { t.Fatalf("expected sink empty after DiscardBuffered, got: %q", sink.String()) }
}

func TestOverlayGuardOutcomePreservesIdentity(t *testing.T) {
	cfg := outputguard.Config{Enabled: true, MaxToolArgumentBytes: 128 * 1024, MaxToolArgumentDeltas: 4096, MaxChannelBytes: 2 * 1024 * 1024, MaxRequestBytes: 100}
	rg := outputguard.NewRequestGuard(cfg)
	_ = rg.ObserveAppend("c1", "", outputguard.KindText, bytes.Repeat([]byte("A"), 200))
	if !rg.IsTripped() { t.Fatalf("expected guard tripped") }

	rawOutcome := classifyStreamOutcome(context.Canceled, errors.New("connection reset"), nil, false)
	if rawOutcome.logStatusCode != logStatusClientClosed { t.Fatalf("raw outcome want 499, got %d", rawOutcome.logStatusCode) }

	finalOutcome := OverlayGuardOutcome(rawOutcome, rg)
	if !finalOutcome.guardTripped || finalOutcome.guardReason != "request_bytes" || finalOutcome.logStatusCode != http.StatusBadGateway || finalOutcome.penalize {
		t.Fatalf("unexpected final outcome: %+v", finalOutcome)
	}
}
```

- [ ] **Step 2: Run test to verify failure (Red)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./proxy -run "TestGuardTerminationPrecedesDownstreamLock|TestGuardTerminationIdempotentReasonFreeze|TestStreamFlushWriterDiscardBuffered|TestOverlayGuardOutcomePreservesIdentity"`
Expected: FAIL with compilation errors (`undefined: NewGuardTerminationCoordinator`, `undefined: OverlayGuardOutcome`, `w.DiscardBuffered undefined`).

- [ ] **Step 3: Implement minimal code for coordinator and buffer discard**

1. 精确修改 `proxy/handler.go:2343-2375` 给 `streamOutcome` 增加字段并在下方提供 `OverlayGuardOutcome`：
```go
type streamOutcome struct {
	logStatusCode int; failureKind string; failureMessage string; failurePayload []byte
	penalize bool; capacityShed bool; verifyAccountAuth bool; requestScoped bool
	modelUnsupported bool; terminalLocal bool; guardTripped bool; guardReason string
}

func OverlayGuardOutcome(outcome streamOutcome, guard *outputguard.RequestGuard) streamOutcome {
	if guard == nil || !guard.IsTripped() { return outcome }
	outcome.guardTripped = true; outcome.guardReason = guard.TrippedReason()
	outcome.logStatusCode = http.StatusBadGateway; outcome.failureKind = "model_output_guard"
	outcome.failureMessage = GuardTriggeredPublicMessage; outcome.failurePayload = nil
	outcome.penalize = false; outcome.terminalLocal = true
	return outcome
}
```

2. 精确修改 `proxy/stream_flush_writer.go:23-40, 403-465` 增加 `aborted bool`，实现 `DiscardBuffered()`，并在 `Flush()`/`Finalize()` 开头检查 `if w == nil || w.aborted { return nil }` 保留原后续逻辑：
```go
func (w *streamFlushWriter) DiscardBuffered() {
	if w == nil { return }
	w.buffer.Reset(); w.scanCarry = nil; w.outputScanner = nil; w.aborted = true
}
```

3. 创建 `proxy/outputguard_cancel.go`：
```go
package proxy

import (
	"context"
	"io"
	"net/http"
	"sync"

	"github.com/codex2api/security/outputguard"
)

type GuardTerminationCoordinator struct {
	guard          *outputguard.RequestGuard
	cancelUpstream context.CancelFunc
	body           io.Closer
	once           sync.Once
	trippedOutcome streamOutcome
}

func NewGuardTerminationCoordinator(guard *outputguard.RequestGuard, cancelUpstream context.CancelFunc, body io.Closer) *GuardTerminationCoordinator {
	return &GuardTerminationCoordinator{guard: guard, cancelUpstream: cancelUpstream, body: body}
}

func (c *GuardTerminationCoordinator) HandleTripped(reason string, discardBuffers ...func()) streamOutcome {
	if c == nil {
		return streamOutcome{logStatusCode: http.StatusBadGateway, failureKind: "model_output_guard", failureMessage: GuardTriggeredPublicMessage, guardTripped: true, guardReason: reason, penalize: false, terminalLocal: true}
	}
	c.once.Do(func() {
		if c.cancelUpstream != nil { c.cancelUpstream() }
		if c.body != nil { _ = c.body.Close() }
		for _, discard := range discardBuffers { if discard != nil { discard() } }
		frozenReason := reason
		if c.guard != nil && c.guard.IsTripped() && c.guard.TrippedReason() != "" {
			frozenReason = c.guard.TrippedReason()
		}
		c.trippedOutcome = streamOutcome{logStatusCode: http.StatusBadGateway, failureKind: "model_output_guard", failureMessage: GuardTriggeredPublicMessage, guardTripped: true, guardReason: frozenReason, penalize: false, terminalLocal: true}
	})
	return c.trippedOutcome
}
```

- [ ] **Step 4: Run test to verify it passes (Green)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v -race ./proxy -run "TestGuardTerminationPrecedesDownstreamLock|TestGuardTerminationIdempotentReasonFreeze|TestStreamFlushWriterDiscardBuffered|TestOverlayGuardOutcomePreservesIdentity"`
Expected: PASS ok github.com/codex2api/proxy

- [ ] **Step 5: Commit**

```bash
git add proxy/handler.go proxy/stream_flush_writer.go proxy/outputguard_cancel.go proxy/outputguard_cancel_test.go
git commit -m "feat(proxy): implement guard termination coordinator and buffer discard primitives"
```

---

### Task 9b: 下游安全通知写入、TryLock/Deadline 控制与多协议终态

**Files:**
- Create: `proxy/outputguard_downstream.go`
- Test: `proxy/outputguard_downstream_test.go`

**Interfaces:**
- Consumes: `github.com/gin-gonic/gin`, `github.com/gorilla/websocket`, `github.com/codex2api/api`
- Produces:
  ```go
  package proxy
  import ("net/http"; "sync"; "time"; "github.com/gin-gonic/gin"; "github.com/gorilla/websocket")
  const (
      GuardTriggeredPublicCode    = "model_output_guard_triggered"
      GuardTriggeredPublicMessage = "Upstream model output exceeded the gateway safety policy; generation was stopped."
      GuardWriteDeadlineTimeout   = 5 * time.Second
  )
  func ApplyDownstreamWriteDeadline(w http.ResponseWriter, timeout time.Duration) error
  func FormatGuardTerminalEnvelope(protocol string, streamID string) ([]byte, int, error)
  func SendDownstreamGuardTerminalHTTP(c *gin.Context, mu *sync.Mutex, clientGone *bool, protocol string, reason string) error
  func SendDownstreamGuardTerminalWS(conn *websocket.Conn, streamID string, reason string) error
  ```

> **WS 关闭口径注记**：`SendDownstreamGuardTerminalWS` 只负责写出带 stream_id 的 error 帧；Responses WS / Realtime 调用方随后统一以 `CloseTryAgainLater`(1013) 关闭连接，与 HTTP 502 guard 终态语义一致。同一连接多 lane（`runResponsesWSMultiplex`）下终态写与普通数据帧共用 per-conn 写锁（`responsesWSWriters`）串行，避免 gorilla 并发写。

- [ ] **Step 1: Write failing test for downstream terminal writer and deadline hooks**

```go
// proxy/outputguard_downstream_test.go
package proxy

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestSendDownstreamGuardTerminalHTTPTryLockContention(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request = req
	c.Writer.WriteHeaderNow()

	var downstreamMu sync.Mutex
	clientGone := false
	downstreamMu.Lock()

	lockHoldReleased := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		downstreamMu.Unlock()
		close(lockHoldReleased)
	}()

	err := SendDownstreamGuardTerminalHTTP(c, &downstreamMu, &clientGone, "responses", "repetition")
	if err == nil { t.Fatalf("expected non-nil error when lock is contended") }
	if !clientGone { t.Fatalf("expected clientGone == true on lock contention") }
	if w.Body.Len() != 0 { t.Fatalf("expected no body written on lock contention, got %s", w.Body.String()) }
	<-lockHoldReleased
}

type fullMockGinResponseWriter struct {
	http.ResponseWriter
	writeCalled bool
}
func (m *fullMockGinResponseWriter) Status() int                     { return http.StatusOK }
func (m *fullMockGinResponseWriter) Size() int                       { return 0 }
func (m *fullMockGinResponseWriter) WriteString(s string) (int, error) { return m.Write([]byte(s)) }
func (m *fullMockGinResponseWriter) Written() bool                   { return true }
func (m *fullMockGinResponseWriter) WriteHeaderNow()                 {}
func (m *fullMockGinResponseWriter) Pusher() http.Pusher             { return nil }
func (m *fullMockGinResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) { return nil, nil, http.ErrNotSupported }
func (m *fullMockGinResponseWriter) Flush()                          {}
func (m *fullMockGinResponseWriter) CloseNotify() <-chan bool        { return nil }
func (m *fullMockGinResponseWriter) Unwrap() http.ResponseWriter     { return m.ResponseWriter }
func (m *fullMockGinResponseWriter) Write(b []byte) (int, error) {
	m.writeCalled = true; return m.ResponseWriter.Write(b)
}

func TestSendDownstreamGuardTerminalHTTPErrNotSupported(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	mockWriter := &fullMockGinResponseWriter{ResponseWriter: rec}
	c := &gin.Context{Writer: mockWriter}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request = req

	var downstreamMu sync.Mutex
	clientGone := false
	err := SendDownstreamGuardTerminalHTTP(c, &downstreamMu, &clientGone, "responses", "repetition")
	if err == nil { t.Fatalf("expected error when deadline unsupported") }
	if !clientGone { t.Fatalf("expected clientGone == true") }
	if mockWriter.writeCalled { t.Fatalf("expected no write when deadline unsupported") }
}

type deadlineSpyResponseWriter struct {
	gin.ResponseWriter
	deadlines []time.Time
}
func (s *deadlineSpyResponseWriter) SetWriteDeadline(t time.Time) error {
	s.deadlines = append(s.deadlines, t); return nil
}

func TestSendDownstreamGuardTerminalHTTPDeadlineSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Writer.WriteHeaderNow()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	spy := &deadlineSpyResponseWriter{ResponseWriter: c.Writer}
	c.Writer = spy

	var downstreamMu sync.Mutex
	clientGone := false
	err := SendDownstreamGuardTerminalHTTP(c, &downstreamMu, &clientGone, "responses", "repetition")
	if err != nil { t.Fatalf("unexpected error: %v", err) }
	if !clientGone { t.Fatalf("expected clientGone == true") }
	if len(spy.deadlines) != 2 || spy.deadlines[0].IsZero() || !spy.deadlines[1].IsZero() {
		t.Fatalf("expected 2 deadline calls (set+reset), got %+v", spy.deadlines)
	}
	if !strings.Contains(rec.Body.String(), GuardTriggeredPublicCode) { t.Fatalf("body missing guard code: %s", rec.Body.String()) }
}

func TestSendDownstreamGuardTerminalHTTPUncommitted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	var downstreamMu sync.Mutex
	clientGone := false
	if err := SendDownstreamGuardTerminalHTTP(c, &downstreamMu, &clientGone, "responses", "repetition"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if w.Code != http.StatusBadGateway { t.Fatalf("expected status 502, got %d", w.Code) }
	body := w.Body.String()
	if !strings.Contains(body, GuardTriggeredPublicCode) || strings.Contains(body, "repetition") {
		t.Fatalf("invalid or leaked body: %s", body)
	}
}

func TestFormatGuardTerminalEnvelope(t *testing.T) {
	respData, status, err := FormatGuardTerminalEnvelope("responses", "")
	if err != nil || status != http.StatusOK || !strings.Contains(string(respData), `"type":"response.failed"`) || strings.Contains(string(respData), "response.completed") {
		t.Fatalf("responses envelope invalid: %s", string(respData))
	}
	chatData, _, err := FormatGuardTerminalEnvelope("chat", "")
	if err != nil || !strings.Contains(string(chatData), GuardTriggeredPublicCode) || strings.Contains(string(chatData), "[DONE]") {
		t.Fatalf("chat envelope invalid: %s", string(chatData))
	}
	anthropicData, _, err := FormatGuardTerminalEnvelope("anthropic", "")
	if err != nil || !strings.Contains(string(anthropicData), "event: error\n") || strings.Contains(string(anthropicData), "message_stop") {
		t.Fatalf("anthropic envelope invalid: %s", string(anthropicData))
	}
	geminiData, _, err := FormatGuardTerminalEnvelope("gemini", "")
	if err != nil || strings.HasPrefix(string(geminiData), "data:") { t.Fatalf("gemini envelope must be bare JSON") }
	var gJSON struct { Error struct { Code int `json:"code"`; Status string `json:"status"` } `json:"error"` }
	if err := json.Unmarshal(geminiData, &gJSON); err != nil || gJSON.Error.Code != 502 || gJSON.Error.Status != "UNAVAILABLE" {
		t.Fatalf("gemini envelope invalid: %+v", gJSON)
	}
	wsData, _, err := FormatGuardTerminalEnvelope("websocket", "stream_xyz")
	if err != nil || !strings.Contains(string(wsData), `"stream_id":"stream_xyz"`) || !strings.Contains(string(wsData), GuardTriggeredPublicCode) {
		t.Fatalf("ws envelope invalid: %s", string(wsData))
	}
}

func TestSendDownstreamGuardTerminalWSSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil { return }
		defer conn.Close()
		_ = SendDownstreamGuardTerminalWS(conn, "turn_001", "repetition")
	}))
	defer server.Close()

	clientConn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil { t.Fatalf("dial error: %v", err) }
	defer clientConn.Close()

	_, msg, err := clientConn.ReadMessage()
	if err != nil { t.Fatalf("read error: %v", err) }
	parsed := gjson.ParseBytes(msg)
	if parsed.Get("type").String() != "error" || parsed.Get("stream_id").String() != "turn_001" || parsed.Get("error.code").String() != GuardTriggeredPublicCode {
		t.Fatalf("unexpected ws frame: %s", string(msg))
	}
}
```

- [ ] **Step 2: Run test to verify failure (Red)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./proxy -run "TestSendDownstreamGuardTerminalHTTP.*|TestFormatGuardTerminalEnvelope|TestSendDownstreamGuardTerminalWSSuccess"`
Expected: FAIL with compilation errors (`undefined: SendDownstreamGuardTerminalHTTP`, `undefined: FormatGuardTerminalEnvelope`, `undefined: SendDownstreamGuardTerminalWS`).

- [ ] **Step 3: Implement minimal code for downstream terminal writer**

创建 `proxy/outputguard_downstream.go`：
```go
package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/api"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

const (
	GuardTriggeredPublicCode    = "model_output_guard_triggered"
	GuardTriggeredPublicMessage = "Upstream model output exceeded the gateway safety policy; generation was stopped."
	GuardWriteDeadlineTimeout   = 5 * time.Second
)

func ApplyDownstreamWriteDeadline(w http.ResponseWriter, timeout time.Duration) error {
	if w == nil { return errors.New("nil response writer") }
	rc := http.NewResponseController(w)
	if rc == nil { return http.ErrNotSupported }
	if timeout <= 0 { return rc.SetWriteDeadline(time.Time{}) }
	return rc.SetWriteDeadline(time.Now().Add(timeout))
}

func FormatGuardTerminalEnvelope(protocol string, streamID string) ([]byte, int, error) {
	normProto := strings.ToLower(strings.TrimSpace(protocol))
	switch normProto {
	case "responses":
		payload, err := json.Marshal(gin.H{
			"type": "response.failed",
			"response": gin.H{
				"created_at": time.Now().Unix(),
				"status":     "failed",
				"error": gin.H{
					"message": GuardTriggeredPublicMessage,
					"type":    api.ErrorTypeUpstream,
					"code":    GuardTriggeredPublicCode,
				},
			},
		})
		if err != nil { return nil, 0, err }
		return []byte("data: " + string(payload) + "\n\n"), http.StatusOK, nil

	case "chat":
		payload, err := json.Marshal(gin.H{
			"error": gin.H{
				"message": GuardTriggeredPublicMessage,
				"type":    api.ErrorTypeUpstream,
				"code":    GuardTriggeredPublicCode,
			},
		})
		if err != nil { return nil, 0, err }
		return []byte("data: " + string(payload) + "\n\n"), http.StatusOK, nil

	case "anthropic":
		payload, err := json.Marshal(gin.H{
			"type":  "error",
			"error": gin.H{"type": "api_error", "message": GuardTriggeredPublicMessage},
		})
		if err != nil { return nil, 0, err }
		return []byte("event: error\ndata: " + string(payload) + "\n\n"), http.StatusOK, nil

	case "gemini":
		payload, err := json.Marshal(gin.H{
			"error": gin.H{"code": http.StatusBadGateway, "message": GuardTriggeredPublicMessage, "status": "UNAVAILABLE"},
		})
		if err != nil { return nil, 0, err }
		return payload, http.StatusOK, nil

	case "websocket":
		payload, err := json.Marshal(struct {
			Type     string        `json:"type"`
			StreamID string        `json:"stream_id,omitempty"`
			Status   int           `json:"status"`
			Error    *api.APIError `json:"error"`
		}{
			Type:     "error",
			StreamID: streamID,
			Status:   http.StatusBadGateway,
			Error:    api.NewAPIError(GuardTriggeredPublicCode, GuardTriggeredPublicMessage, api.ErrorTypeUpstream),
		})
		if err != nil { return nil, 0, err }
		return payload, http.StatusOK, nil

	default:
		payload, err := json.Marshal(gin.H{
			"error": gin.H{"message": GuardTriggeredPublicMessage, "type": api.ErrorTypeUpstream, "code": GuardTriggeredPublicCode},
		})
		if err != nil { return nil, 0, err }
		return payload, http.StatusBadGateway, nil
	}
}

func SendDownstreamGuardTerminalHTTP(c *gin.Context, mu *sync.Mutex, clientGone *bool, protocol string, reason string) error {
	if c == nil || c.Writer == nil {
		if clientGone != nil { *clientGone = true }
		return errors.New("nil context or writer")
	}

	if !c.Writer.Written() {
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.JSON(http.StatusBadGateway, gin.H{
			"error": gin.H{"message": GuardTriggeredPublicMessage, "type": api.ErrorTypeUpstream, "code": GuardTriggeredPublicCode},
		})
		if clientGone != nil { *clientGone = true }
		return nil
	}

	if mu != nil {
		if !mu.TryLock() {
			if clientGone != nil { *clientGone = true }
			return errors.New("downstream write lock busy; dropped guard terminal notification")
		}
		defer mu.Unlock()
	}

	if c.Request != nil && c.Request.Context().Err() != nil {
		if clientGone != nil { *clientGone = true }
		return c.Request.Context().Err()
	}

	if err := ApplyDownstreamWriteDeadline(c.Writer, GuardWriteDeadlineTimeout); err != nil {
		if clientGone != nil { *clientGone = true }
		return fmt.Errorf("set write deadline failed (%w); safely dropped write", err)
	}
	defer func() { _ = ApplyDownstreamWriteDeadline(c.Writer, 0) }()

	envelope, _, err := FormatGuardTerminalEnvelope(protocol, "")
	if err != nil { return err }

	if _, err := c.Writer.Write(envelope); err != nil {
		if clientGone != nil { *clientGone = true }
		return err
	}
	if flusher, ok := c.Writer.(http.Flusher); ok { flusher.Flush() }
	if clientGone != nil { *clientGone = true }
	return nil
}

func SendDownstreamGuardTerminalWS(conn *websocket.Conn, streamID string, reason string) error {
	if conn == nil { return errors.New("nil websocket conn") }
	envelope, _, err := FormatGuardTerminalEnvelope("websocket", streamID)
	if err != nil { return err }
	_ = conn.SetWriteDeadline(time.Now().Add(GuardWriteDeadlineTimeout))
	defer func() { _ = conn.SetWriteDeadline(time.Time{}) }()
	return conn.WriteMessage(websocket.TextMessage, envelope)
}
```

- [ ] **Step 4: Run test to verify it passes (Green)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v -race ./proxy -run "TestSendDownstreamGuardTerminalHTTP.*|TestFormatGuardTerminalEnvelope|TestSendDownstreamGuardTerminalWSSuccess"`
Expected: PASS ok github.com/codex2api/proxy

- [ ] **Step 5: Commit**

```bash
git add proxy/outputguard_downstream.go proxy/outputguard_downstream_test.go
git commit -m "feat(proxy): implement downstream guard terminal writer with TryLock and 5s deadline"
```
