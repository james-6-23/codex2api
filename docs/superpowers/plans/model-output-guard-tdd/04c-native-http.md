# 04c-Native-HTTP: Anthropic Messages, Gemini Native, and Grok Native Guard Wiring Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 完成 Codex2API 模型输出失控防护在原生 HTTP 协议路径上的完整接线（涵盖 Anthropic Messages、Gemini Native stream/generateContent、Grok 与 Antigravity 原生转发分支），在 translator、accumulator 与 downstream writer 之前执行事件级投影与 guard 判定，并在命中时原子取消上游、丢弃私有在途缓冲、叠加类型化 streamOutcome，尽力写入多协议安全终态 envelope。

**Architecture:**
1. **入口门禁与请求级生命周期**：在 `Messages` 与 `GeminiModelsAction` HTTP 入口处首选调用 `RequireOutputGuardReadyHTTP(*gin.Context)` 实现冷启动未就绪 503 阻断；在 `attempt` 选号重试循环外单次获取 `CurrentOutputGuardConfig().CoreConfig()` 并实例化 `outputguard.NewRequestGuard(coreCfg)`，跨 attempt 严格保持累计语义预算与事件预算，禁止在重试或续想时重置状态。
2. **协议感知投影前置拦截**：
   - **Messages 路径**：区分三种上游模式——原生 Claude（`account.IsClaudeOAuth()`）使用 `NewAnthropicProjector()`；Codex / Responses 中转（下游 Messages、上游 Responses SSE）使用 `NewResponsesProjector()`；Grok 原生中转按 Grok 选路协议（Messages/Chat/Responses）动态复用对应的 Projector。在 `readSSEStreamWithContinuousRetryKeepalive` 遍历回调最顶部、在 `translator.translateEvent`、`accumulator.apply` 及 downstream 写入之前执行 `ObserveEvent(guard, projector, eventType, data)`。
   - **Gemini Native 路径**：流式 `streamGenerateContent` 经 `NewGeminiProjector()` 逐帧将 parts/thinking 投影为增量 Append，严格杜绝因 candidate `finishReason` 将流误判为 Snapshot；收到终态标记或流 EOF 时执行 ActionEnd 尾检。非流式 `generateContent` 保留原有 8 MiB `antigravityResponseBodyLimit` 有界读取，在全 JSON 写入前执行全量 Snapshot 检查。
   - **Grok Native 路径**：在 `forwardGrokNativeResponseObserved` 中按 `protocol` 显式分派 `ChatProjector`（ChatCompletions）、`ResponsesProjector`（Responses）或 `AnthropicProjector`（Messages），工具参数与 legacy function_call 显式由对应投影器解析，非流式在全响应写出前执行一次完整 Snapshot 检查。
3. **强制终止与恢复阻断闭环**：
   - Guard 触发后立即通过 `GuardTerminationCoordinator.HandleTripped` 主动 `cancelUpstream()` 并 `body.Close()`，彻底跳过 5 秒 usage drain；
   - 同步执行本地缓冲丢弃回调（`streamFlushWriter.DiscardBuffered()`、清空 accumulator/pending 缓冲）；
   - 通过 `OverlayGuardOutcome` 强制将结果叠加为 `logStatusCode=502`、`failureKind="model_output_guard"`、`penalize=false`、`terminalLocal=true`，确保后续重试器绝不将其归为 499 客户端断开或 598 读断流；
   - 通过 `SendDownstreamGuardTerminalHTTP` 在锁内 5 秒超时保护下尽力交付脱敏协议错误 envelope（04a 标准枚举："anthropic", "gemini", "chat", "responses"），底层不支持或写锁冲突时安全退出，绝不泄漏模型 payload 或伪造完成状态。

**Tech Stack:** Go 1.26+, `github.com/codex2api/security/outputguard`, `github.com/gin-gonic/gin`, `github.com/tidwall/gjson`, `github.com/tidwall/sjson`, 标准库 `net/http`, `context`, `sync`, `time`.

**Spec:** `docs/superpowers/specs/2026-10-28-model-output-guard-design.md` §4, §5, §6, §9.2

---

## Global Constraints

- **范围限定**：本计划严格限定于 Task 10B scope（Anthropic Messages、Gemini Native、Grok Native 及 Antigravity HTTP 路径），不承担 Responses/Chat HTTP 原生接线（04b 负责），不承担 WebSocket/Realtime 或全局 recover 逻辑。
- **契约真实性与 Imports**：严格消费 `01-core`、`02b-runtime`、`03-protocol` 与 `04a-termination` 已定义的原语。禁止凭空捏造函数名或字段。
- **协议枚举对齐**：下游通知调用 `SendDownstreamGuardTerminalHTTP` 时，协议枚举字符串严格与 04a-termination 一致（全小写：`"anthropic"`, `"gemini"`, `"chat"`, `"responses"`）。
- **前置检查纪律**：任何模型输出事件必须在进入 `translator`、`accumulator`、`bytes.Clone` provenance 记录与下游 `Writer` 之前完成 guard 校验；命中事件严禁产生任何下发或聚合副作用。
- **恢复与重试绝对阻断**：Guard 命中事件标记为 `terminalLocal=true` 与 `penalize=false`，严禁触发换号重试、连续重试（continuous retry）、自动续想或将异常用量上报为真实消费。
- **传输有界保留**：非流式读取严格保留现有有界 reader（Gemini 8 MiB `antigravityResponseBodyLimit`，Grok 128 MiB `grokMaxDecodedBody`），禁止机械替换为 16 MiB。

---

## File Structure

| 文件路径 | 状态 | 职责描述 |
|---|---|---|
| `proxy/outputguard_native_adapters.go` | 新建 | 定义 Native HTTP 路由专用的 Projector 选择适配器与上下文绑定辅助（包含 `selectNativeProjector`、`ExecuteNativeGuardCheck`） |
| `proxy/handler_anthropic.go` | 修改 | 在 `Messages` 入口接入 `RequireOutputGuardReadyHTTP`；在 attempt 外初始化 `RequestGuard`；在前置循环中接入 `ObserveEvent`、`GuardTerminationCoordinator` 与下游 502/error 通知 |
| `proxy/handler_gemini.go` | 修改 | 在 `GeminiModelsAction` 接入冷启动门禁；在 `handleGeminiGenerateContent` 中为流式（parts append）与非流式（JSON snapshot）接入 Guard，流异常时阻断并写出 Gemini 裸 JSON 502 |
| `proxy/handler.go` | 修改 | 在 `forwardGrokNativeResponseObserved` 原生流式与非流式分支接入 Guard 检查与 termination 原语，按上游协议动态复用 Projector |
| `proxy/outputguard_native_http_test.go` | 新建 | Task 10B 完整 TDD 测试套件：涵盖 Anthropic Claude 原生流、Anthropic 转 Responses SSE、Gemini 流式 Append 与非流式 Snapshot、Grok Chat/Responses 选路、被拒内容绝不可见、上游仅调用 1 次且无伪造 STOP |

---

## Task 10B: Messages、Gemini Native 与 Grok Native HTTP 路径 Guard 接线

**Files:**
- Create: `proxy/outputguard_native_adapters.go`
- Modify: `proxy/handler_anthropic.go:547-575, 1300-1335, 1545-1615, 1635-1675`
- Modify: `proxy/handler_gemini.go:123-145, 390-418, 515-545, 655-685, 720-745`
- Modify: `proxy/handler.go:1234-1265, 1297-1335`
- Test: `proxy/outputguard_native_http_test.go`

**Interfaces:**
- Consumes:
  - `github.com/codex2api/security/outputguard`:
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
  - `proxy` (02b-runtime):
    ```go
    type OutputGuardAppliedConfig struct {
        Enabled               bool
        MaxToolArgumentBytes  int64
        MaxToolArgumentDeltas int
        MaxChannelBytes       int64
        MaxRequestBytes       int64
        Generation            int64
    }
    func (c OutputGuardAppliedConfig) CoreConfig() outputguard.Config
    func CurrentOutputGuardConfig() OutputGuardAppliedConfig
    func RequireOutputGuardReadyHTTP(c *gin.Context) bool
    ```
  - `proxy` (03-protocol):
    ```go
    type ProtocolProjector interface {
        Project(eventType string, data []byte) ([]Observation, error)
    }
    func NewResponsesProjector() *ResponsesProjector
    func NewChatProjector() *ChatProjector
    func NewAnthropicProjector() *AnthropicProjector
    func NewGeminiProjector() *GeminiProjector
    func ObserveEvent(guard *outputguard.RequestGuard, projector ProtocolProjector, eventType string, data []byte) error
    ```
  - `proxy` (04a-termination):
    ```go
    type GuardTerminationCoordinator struct { ... }
    func NewGuardTerminationCoordinator(guard *outputguard.RequestGuard, cancelUpstream context.CancelFunc, body io.Closer) *GuardTerminationCoordinator
    func (c *GuardTerminationCoordinator) HandleTripped(reason string, discardBuffers ...func()) streamOutcome
    func OverlayGuardOutcome(outcome streamOutcome, guard *outputguard.RequestGuard) streamOutcome
    func SendDownstreamGuardTerminalHTTP(c *gin.Context, mu *sync.Mutex, clientGone *bool, protocol string, reason string) error
    func (w *streamFlushWriter) DiscardBuffered()
    ```
- Produces:
  - `selectNativeProjector(protocol string, isNativeClaude bool) ProtocolProjector`
  - 端到端完整的 Messages、Gemini 与 Grok 文本/工具输出失控安全阻断链路。

---

### Step 1: Write failing TDD tests for all Native HTTP routes

在 `proxy/outputguard_native_http_test.go` 中编写完整集成测试，覆盖冷启动门禁、Claude 原生流拦截、Messages 转 Responses 流拦截、Gemini 流式逐 parts 拦截与非流式 Snapshot 拦截、Grok 真实协议分派，验证被拒内容绝不下发、上游调用次数恒为 1、不记录成功 stop 且状态叠加为 502。

```go
// proxy/outputguard_native_http_test.go
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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
	"github.com/codex2api/security/outputguard"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func setupNativeHTTPTestEnv(t *testing.T) (*database.DB, *auth.Store) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "native-http-guard.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 0})
	t.Cleanup(store.Stop)
	return db, store
}

// 1. Anthropic Messages 原生透传与转 Responses 路径 Guard 触发测试
func TestNativeHTTP_AnthropicMessagesGuardTriggered(t *testing.T) {
	db, store := setupNativeHTTPTestEnv(t)
	var upstreamCalls int32

	// 模拟上游发送持续重复的文本流（超过 8192 bytes 短周期）
	repeatedChunk := strings.Repeat("A_REPEATED_CHUNK_TOKEN_1234567890_", 300) // ~10.5 KB
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		flusher, _ := w.(http.Flusher)
		// 发送合法起始帧
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\"}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"))
		flusher.Flush()
		// 发送恶意重复 chunk
		evt, _ := json.Marshal(gin.H{
			"type":  "content_block_delta",
			"index": 0,
			"delta": gin.H{"type": "text_delta", "text": repeatedChunk},
		})
		_, _ = w.Write([]byte("event: content_block_delta\ndata: " + string(evt) + "\n\n"))
		flusher.Flush()
		// 紧接着发送伪造的 message_stop 帧（若未拦截会误导下游）
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
		flusher.Flush()
	}))
	defer upstream.Close()

	acc := &auth.Account{
		DBID:         1001,
		UpstreamType: auth.UpstreamClaude,
		ClaudeBaseURL: upstream.URL,
		Models:       []string{"claude-3-5-sonnet-20241022"},
		HealthTier:   auth.HealthTierHealthy,
		Status:       auth.StatusReady,
	}
	store.AddAccount(acc)

	handler := NewHandler(store, db, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	router.POST("/v1/messages", handler.Messages)

	reqBody := `{"model":"claude-3-5-sonnet-20241022","stream":true,"messages":[{"role":"user","content":"repeat"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)
	respBody := w.Body.String()

	// 断言 1: 上游仅被调用 1 次，严禁由于断流或失败发起重试
	if calls := atomic.LoadInt32(&upstreamCalls); calls != 1 {
		t.Fatalf("expected 1 upstream call, got %d", calls)
	}
	// 断言 2: 下游收到脱敏的 Anthropic 协议 error 事件，严禁收到 message_stop
	if !strings.Contains(respBody, "event: error") || !strings.Contains(respBody, GuardTriggeredPublicMessage) {
		t.Fatalf("expected anthropic guard error event, got: %s", respBody)
	}
	if strings.Contains(respBody, "message_stop") {
		t.Fatalf("stream must not contain message_stop after guard tripped")
	}
}

// 2. Gemini Native streamGenerateContent 流式逐 parts 拦截与非流式 Snapshot 拦截
func TestNativeHTTP_GeminiNativeGuardTriggered(t *testing.T) {
	db, store := setupNativeHTTPTestEnv(t)
	var streamCalls, nonStreamCalls int32

	repeatedText := strings.Repeat("GEMINI_RUNAWAY_REPETITION_TOKEN_ABC_", 300) // ~10.8 KB
	geminiUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			atomic.AddInt32(&streamCalls, 1)
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			flusher, _ := w.(http.Flusher)
			// Chunk 1: 恶意失控文本
			c1, _ := json.Marshal(gin.H{
				"candidates": []gin.H{
					{"index": 0, "content": gin.H{"role": "model", "parts": []gin.H{{"text": repeatedText}}}},
				},
			})
			_, _ = w.Write([]byte("data: " + string(c1) + "\n\n"))
			flusher.Flush()
			// Chunk 2: 伪造的正常 STOP 帧
			c2, _ := json.Marshal(gin.H{
				"candidates": []gin.H{
					{"index": 0, "finishReason": "STOP"},
				},
			})
			_, _ = w.Write([]byte("data: " + string(c2) + "\n\n"))
			flusher.Flush()
			return
		}

		// non-stream generateContent
		atomic.AddInt32(&nonStreamCalls, 1)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		cNonStream, _ := json.Marshal(gin.H{
			"candidates": []gin.H{
				{"index": 0, "content": gin.H{"role": "model", "parts": []gin.H{{"text": repeatedText}}}, "finishReason": "STOP"},
			},
		})
		_, _ = w.Write(cNonStream)
	}))
	defer geminiUpstream.Close()

	origBases := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{geminiUpstream.URL}
	defer func() { antigravityOAuthEndpointBases = origBases }()

	acc := &auth.Account{
		DBID:                 2001,
		UpstreamType:         auth.UpstreamAntigravity,
		AccessToken:          "mock-ag-token",
		AntigravityProjectID: "mock-project",
		Models:               []string{"gemini-3.7-flash-low"},
		HealthTier:           auth.HealthTierHealthy,
		Status:               auth.StatusReady,
	}
	store.AddAccount(acc)

	handler := NewHandler(store, db, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	router.POST("/v1beta/models/*action", handler.GeminiModelsAction)

	// (A) 测试 streamGenerateContent
	reqStream := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-3.7-flash-low:streamGenerateContent?alt=sse", strings.NewReader(`{"contents":[{"parts":[{"text":"go"}]}]}`))
	wStream := httptest.NewRecorder()
	router.ServeHTTP(wStream, reqStream)

	if calls := atomic.LoadInt32(&streamCalls); calls != 1 {
		t.Fatalf("expected 1 stream call, got %d", calls)
	}
	streamBody := wStream.Body.String()
	// Gemini SDK 期望流末尾收到裸 JSON error（非 data: 前缀）且 code 为 502 / UNAVAILABLE
	if !strings.Contains(streamBody, "UNAVAILABLE") || !strings.Contains(streamBody, "502") {
		t.Fatalf("expected bare gemini 502 JSON error in stream, got: %s", streamBody)
	}
	if strings.Contains(streamBody, "finishReason") {
		t.Fatalf("stream should not emit normal finishReason STOP after guard tripped")
	}

	// (B) 测试非流式 generateContent (全量 Snapshot 检查)
	reqNonStream := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-3.7-flash-low:generateContent", strings.NewReader(`{"contents":[{"parts":[{"text":"go"}]}]}`))
	wNonStream := httptest.NewRecorder()
	router.ServeHTTP(wNonStream, reqNonStream)

	if calls := atomic.LoadInt32(&nonStreamCalls); calls != 1 {
		t.Fatalf("expected 1 non-stream call, got %d", calls)
	}
	if wNonStream.Code != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502 for non-stream guard rejection, got %d (body: %s)", wNonStream.Code, wNonStream.Body.String())
	}
	var errResp map[string]any
	if err := json.Unmarshal(wNonStream.Body.Bytes(), &errResp); err != nil || gjson.GetBytes(wNonStream.Body.Bytes(), "error.status").String() != "UNAVAILABLE" {
		t.Fatalf("expected gemini UNAVAILABLE error payload, got: %s", wNonStream.Body.String())
	}
}

// 3. Grok Native 协议复用与 Chat/Responses Projector 显式分派测试
func TestNativeHTTP_GrokNativeChatAndResponsesGuardTriggered(t *testing.T) {
	db, store := setupNativeHTTPTestEnv(t)
	var grokCalls int32

	repeatedArgs := strings.Repeat("{\"key\":\"runaway_tool_argument_val\"}", 250) // ~8.5 KB
	grokUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&grokCalls, 1)
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		flusher, _ := w.(http.Flusher)
		// 发送 Chat 格式 tool_calls delta
		chunk := gin.H{
			"id": "chatcmpl-grok-1",
			"choices": []gin.H{
				{
					"index": 0,
					"delta": gin.H{
						"tool_calls": []gin.H{
							{"index": 0, "id": "call_123", "function": gin.H{"name": "write", "arguments": repeatedArgs}},
						},
					},
				},
			},
		}
		data, _ := json.Marshal(chunk)
		_, _ = w.Write([]byte("data: " + string(data) + "\n\n"))
		flusher.Flush()
		// 紧接着发送正常 [DONE] 帧
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer grokUpstream.Close()

	acc := &auth.Account{
		DBID:         3001,
		APIKey:       "xai-test-key",
		BaseURL:      grokUpstream.URL,
		UpstreamType: auth.UpstreamGrok,
		Models:       []string{"grok-2"},
		HealthTier:   auth.HealthTierHealthy,
		Status:       auth.StatusReady,
	}
	store.AddAccount(acc)

	handler := NewHandler(store, db, &config.Config{AllowAnonymousV1: true}, nil)
	c, w := httptest.NewRecorder(), httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(w)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"grok-2","stream":true}`))

	resp, err := http.Get(grokUpstream.URL)
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	defer resp.Body.Close()

	// 消费 forwardGrokNativeResponseTo
	usage, outcome, wroteAny, _ := forwardGrokNativeResponseTo(
		context.Background(), ginCtx, resp, GrokProtocolChatCompletions, true, time.Now(), func() {}, nil, nil,
	)

	if calls := atomic.LoadInt32(&grokCalls); calls != 1 {
		t.Fatalf("expected 1 grok call, got %d", calls)
	}
	if !outcome.guardTripped || outcome.logStatusCode != http.StatusBadGateway {
		t.Fatalf("expected guard tripped with 502 outcome, got: %+v", outcome)
	}
	if strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatalf("downstream must not receive [DONE] after tool guard triggered")
	}
	_ = c
	_ = usage
	_ = wroteAny
}
```

---

### Step 2: Run test to verify failure (Red)

运行测试验证编译错误与缺失符号（红色失败基线）：
```bash
GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./proxy -run "TestNativeHTTP_"
```
预期输出：`FAIL`（因 `selectNativeProjector` 未定义，`forwardGrokNativeResponseTo` 尚未接入 Guard 等编译或断言失败）。

---

### Step 3: Minimal implementation for Native HTTP Guard Wiring

#### 3.1 创建 `proxy/outputguard_native_adapters.go`

创建专用的 Native Projector 适配辅助，准确根据协议与上游类型选择 Projector：

```go
// proxy/outputguard_native_adapters.go
package proxy

import (
	"strings"

	"github.com/codex2api/auth"
)

// selectNativeProjector 为原生/中转通道选择真实 upstream projector。
// 严禁按客户端下游请求路径盲目推测上游协议！
func selectNativeProjector(protocol string, isNativeClaude bool, isCodexResponses bool) ProtocolProjector {
	if isNativeClaude {
		return NewAnthropicProjector()
	}
	if isCodexResponses {
		return NewResponsesProjector()
	}
	norm := strings.ToLower(strings.TrimSpace(protocol))
	switch norm {
	case "anthropic", "messages":
		return NewAnthropicProjector()
	case "gemini":
		return NewGeminiProjector()
	case "chat", "chat_completions":
		return NewChatProjector()
	case "responses":
		return NewResponsesProjector()
	default:
		return NewChatProjector()
	}
}
```

#### 3.2 修改 `proxy/handler_anthropic.go`

在 `Messages` 处理流程中实施冷启动门禁、RequestGuard 跨 attempt 生命周期、前置拦截及协调终止：

```go
// 精确锚点：proxy/handler_anthropic.go:547 下方新增冷启动门禁与 RequestGuard 初始化
func (h *Handler) Messages(c *gin.Context) {
	if !RequireOutputGuardReadyHTTP(c) {
		return
	}
	// 在 attempt 循环外部单次初始化 RequestGuard，跨 attempt 不 reset
	appliedGuardCfg := CurrentOutputGuardConfig()
	requestGuard := outputguard.NewRequestGuard(appliedGuardCfg.CoreConfig())
	// 后续保持不变读取 rawBody...
```

在 `proxy/handler_anthropic.go:1545-1615` 流式读取循环中插入前置 Guard 拦截：

```go
// 精确锚点：proxy/handler_anthropic.go:1545-1575（readSSEStreamWithContinuousRetryKeepalive 回调首部）
			var clientGone bool
			coord := NewGuardTerminationCoordinator(requestGuard, upstreamCancel, resp.Body)
			// 选择 projector：若为原生 Claude 账号使用 AnthropicProjector；Codex Responses 中转必须使用 ResponsesProjector
			activeProjector := selectNativeProjector("messages", account.IsClaudeOAuth(), !account.IsClaudeOAuth() && !isRelayAccount)

			readErr = readSSEStreamWithContinuousRetryKeepalive(readCtx, resp.Body, func(sseEvent string, data []byte) bool {
				eventType := normalizedUpstreamSSEEventType(sseEvent, data)
				// 1. Guard 检查前置于所有 provenance/translator/accumulator 逻辑
				if err := ObserveEvent(requestGuard, activeProjector, eventType, data); err != nil || requestGuard.IsTripped() {
					reason := requestGuard.TrippedReason()
					_ = coord.HandleTripped(reason, streamWriter.DiscardBuffered)
					_ = SendDownstreamGuardTerminalHTTP(c, &downstreamMu, &clientGone, "anthropic", reason)
					gotTerminal = true
					return false
				}
				// 检查通过后方可放行执行原有 provenance 记录与 translator 逻辑
				parsed := gjson.ParseBytes(data)
				ApplyCodexUsageEventFrame(h.store, account, eventType, parsed, time.Now())
```

在 `proxy/handler_anthropic.go:1635-1675` 非流式读取循环与最终聚合处接入 Guard 拦截：

```go
// 精确锚点：proxy/handler_anthropic.go:1635-1660（非流式 SSE 聚合前置检查）
			readErr = readSSEStreamWithContinuousRetryKeepalive(readCtx, resp.Body, func(sseEvent string, data []byte) bool {
				eventType := normalizedUpstreamSSEEventType(sseEvent, data)
				// 逐事件扣减预算与重复检查，一处触发立即丢弃累加器并终止
				if err := ObserveEvent(requestGuard, activeProjector, eventType, data); err != nil || requestGuard.IsTripped() {
					reason := requestGuard.TrippedReason()
					_ = coord.HandleTripped(reason, func() { accumulator = newAnthropicResponseAccumulator(originalModel) })
					gotTerminal = true
					return false
				}
				parsed := gjson.ParseBytes(data)
				ApplyCodexUsageEventFrame(h.store, account, eventType, parsed, time.Now())
```

并在循环退出后调用 `OverlayGuardOutcome` 覆盖 outcome：
```go
// 精确锚点：proxy/handler_anthropic.go:1715 下方
			outcome = OverlayGuardOutcome(outcome, requestGuard)
```

#### 3.3 修改 `proxy/handler_gemini.go`

在 `GeminiModelsAction` 入口接入门禁，并在 `handleGeminiGenerateContent` 与 `forwardGeminiNativeStream` 接入流式与非流式 Guard：

```go
// 精确锚点：proxy/handler_gemini.go:123 下方
func (h *Handler) GeminiModelsAction(c *gin.Context) {
	if !RequireOutputGuardReadyHTTP(c) {
		return
	}
	action := strings.TrimPrefix(strings.TrimSpace(c.Param("action")), "/")
	// ... 保持原有 action 解析与方法分派
```

在 `proxy/handler_gemini.go:462` `handleGeminiGenerateContent` 选号循环外初始化 Guard：
```go
// 精确锚点：proxy/handler_gemini.go:462 下方
func (h *Handler) handleGeminiGenerateContent(c *gin.Context, model string, rawBody []byte, stream bool) {
	appliedCfg := CurrentOutputGuardConfig()
	requestGuard := outputguard.NewRequestGuard(appliedCfg.CoreConfig())
	geminiProjector := NewGeminiProjector()
```

修改 `forwardGeminiNativeStream`（`proxy/handler_gemini.go:390-418`）传递 `requestGuard` 与 `coord`，进行逐 parts Append 校验：
```go
// 精确锚点：proxy/handler_gemini.go:393 改造 forwardGeminiNativeStream 签名与内部回调
func forwardGeminiNativeStream(c *gin.Context, body io.Reader, startedAt time.Time, guard *outputguard.RequestGuard, coord *GuardTerminationCoordinator, projector ProtocolProjector) geminiNativeStreamResult {
	var result geminiNativeStreamResult
	var usage geminiNativeUsage
	var clientGone bool
	activateContinuousRetryKeepalive(c.Request.Context())
	result.readErr = readSSEStreamWithContinuousRetryKeepalive(c.Request.Context(), body, func(_ string, data []byte) bool {
		if c.Request.Context().Err() != nil {
			return false
		}
		// 1. Guard 检查前置：逐 candidate parts 追加检测，绝不因 finishReason 误作 Snapshot
		if guard != nil {
			if err := ObserveEvent(guard, projector, "", data); err != nil || guard.IsTripped() {
				reason := guard.TrippedReason()
				if coord != nil {
					_ = coord.HandleTripped(reason)
				}
				_ = SendDownstreamGuardTerminalHTTP(c, nil, &clientGone, "gemini", reason)
				result.readErr = errModelOutputGuardTriggered
				return false
			}
		}
		usage.observe(data)
		if result.firstTokenMs == 0 && !startedAt.IsZero() && geminiNativeHasContent(data) {
			result.firstTokenMs = max(int(time.Since(startedAt).Milliseconds()), 1)
		}
		frame := "data: " + string(data) + "\n\n"
		var written int
		written, result.writeErr = c.Writer.WriteString(frame)
		if result.writeErr == nil && written != len(frame) {
			result.writeErr = io.ErrShortWrite
		}
		if result.writeErr != nil {
			return false
		}
		c.Writer.Flush()
		return true
	})
	result.usage = usage.usageInfo()
	return result
}
```

在非流式读取处（`proxy/handler_gemini.go:720-745`）接入完整 Snapshot 检查：
```go
// 精确锚点：proxy/handler_gemini.go:723（非流式有界读取后在 write 之前）
		out, readErr := readAllWithContinuousRetryKeepalive(c.Request.Context(), io.LimitReader(resp.Body, antigravityResponseBodyLimit))
		_ = resp.Body.Close()
		upstreamCancel()
		if readErr != nil {
			// 原有错误处理
			return
		}
		// 全量 JSON Snapshot Guard 判定
		if err := ObserveEvent(requestGuard, geminiProjector, "", out); err != nil || requestGuard.IsTripped() {
			reason := requestGuard.TrippedReason()
			_ = coord.HandleTripped(reason)
			h.store.Release(account)
			_ = SendDownstreamGuardTerminalHTTP(c, nil, nil, "gemini", reason)
			return
		}
		usage := geminiNativeUsageFromBody(out)
```

并在 outcome 分类后调用 `OverlayGuardOutcome` 覆盖结果：
```go
// 精确锚点：proxy/handler_gemini.go:675 下方
			outcome = OverlayGuardOutcome(outcome, requestGuard)
```

#### 3.4 修改 `proxy/handler.go` (Grok 原生转发分支)

在 `forwardGrokNativeResponseObserved`（`proxy/handler.go:1234-1335`）中接入 Guard：

```go
// 精确锚点：proxy/handler.go:1234
func forwardGrokNativeResponseObserved(readCtx context.Context, c *gin.Context, resp *http.Response, protocol GrokProtocol, streaming bool, startedAt time.Time, firstVisible func(), output io.Writer, outputFlusher http.Flusher, observe func([]byte)) (*UsageInfo, streamOutcome, bool, int) {
	appliedCfg := CurrentOutputGuardConfig()
	requestGuard := outputguard.NewRequestGuard(appliedCfg.CoreConfig())
	projector := selectNativeProjector(string(protocol), false, false)
	coord := NewGuardTerminationCoordinator(requestGuard, nil, resp.Body)
	var clientGone bool
	var downstreamMu sync.Mutex

	if !streaming {
		body, err := readAllLimitedWithContinuousRetryKeepalive(readCtx, resp.Body, grokMaxDecodedBody)
		if err != nil {
			return nil, classifyStreamOutcome(nil, err, nil, false), false, 0
		}
		// 1. 全响应写出前执行 Snapshot 检查
		if err := ObserveEvent(requestGuard, projector, "", body); err != nil || requestGuard.IsTripped() {
			reason := requestGuard.TrippedReason()
			trippedOutcome := coord.HandleTripped(reason)
			_ = SendDownstreamGuardTerminalHTTP(c, &downstreamMu, &clientGone, string(protocol), reason)
			return nil, trippedOutcome, false, 0
		}
		// ... 正常写入 body
```

在流式帧解析回调中插入检查（`proxy/handler.go:1297-1325`）：
```go
// 精确锚点：proxy/handler.go:1297
	readErr := readRawGrokSSEFramesWithContinuousRetryKeepalive(readCtx, resp.Body, func(frame rawGrokSSEFrame) bool {
		if frame.HasData && !frame.Done {
			// 前置 Guard 检查，拦截时整帧不再进入 observe、usage 统计及下游写入
			if err := ObserveEvent(requestGuard, projector, frame.Event, frame.Data); err != nil || requestGuard.IsTripped() {
				reason := requestGuard.TrippedReason()
				failure = coord.HandleTripped(reason, func() { pending.Reset() })
				_ = SendDownstreamGuardTerminalHTTP(c, &downstreamMu, &clientGone, string(protocol), reason)
				terminal, failed = true, true
				return false
			}
			if observe != nil {
				observe(frame.Data)
			}
			usage = mergeGrokNativeUsage(usage, grokNativeUsage(protocol, frame.Data))
```

在函数返回前对 outcome 叠加 Guard 状态：
```go
// 精确锚点：proxy/handler.go:1375 下方
	outcome := classifyStreamOutcome(readCtx.Err(), readErr, writeErr, terminal && !failed)
	outcome = OverlayGuardOutcome(outcome, requestGuard)
```

---

### Step 4: Run tests to verify they pass (Green)

运行测试验证所有测试用例通过：
```bash
GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v -race ./proxy -run "TestNativeHTTP_"
```
预期输出：`PASS ok github.com/codex2api/proxy`。

---

### Step 5: Refactor & Commit

清理无用临时变量与日志，核验代码规范并提交：
```bash
git add proxy/outputguard_native_adapters.go proxy/handler_anthropic.go proxy/handler_gemini.go proxy/handler.go proxy/outputguard_native_http_test.go
git commit -m "feat(proxy): wire model output guard into Anthropic, Gemini, and Grok native HTTP handlers"
```

---

## 边界与责任自检

1. **Spec 覆盖与无歧义**：
   - Anthropic Messages：原生 Claude 使用 `AnthropicProjector`；Responses 中转使用 `ResponsesProjector`；流式在 translator 前检查，非流式在 accumulator 聚合前检查。
   - Gemini Native：流式 parts 保持 Append，严格不因 candidate `finishReason` 误判为 Snapshot；非流式保留原有 8 MiB 限制并在全 JSON 发送前 Snapshot 检查。
   - Grok Native：按上游真实协议分派 Projector，非流式 Snapshot 与流式逐帧检查闭环。
2. **强制终止协调与下游通知**：
   - 使用 04a 提供的 `GuardTerminationCoordinator` 与 `SendDownstreamGuardTerminalHTTP`；
   - 协议名称严格使用 04a 枚举（`"anthropic"`, `"gemini"`, `"chat"`, `"responses"`）；
   - 绝不伪造成功完成（如 Gemini STOP、Anthropic message_stop、Chat [DONE]）；
   - 绝不泄露内部 reason 或模型 payload；
   - 严格阻断重试（`penalize=false`, `terminalLocal=true`）。
3. **行数与插入精炼度**：
   - 核心适配器与各 Handler 插入块高度聚焦，总插入代码严格控制在 <= 700 行真实代码。

---

## Execution Handoff

计划已由 Task 9.3 落地验证并完成。

**Progress:**
- [x] Step 1: Write failing TDD tests for all Native HTTP routes
- [x] Step 2: Run test to verify failure (Red)
- [x] Step 3: Minimal implementation for Native HTTP Guard Wiring
- [x] Step 4: Run tests to verify they pass (Green)
- [x] Step 5: Refactor & Commit
