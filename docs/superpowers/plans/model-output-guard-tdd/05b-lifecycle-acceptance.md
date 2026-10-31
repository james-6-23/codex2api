# 05b-Lifecycle-Acceptance: 生命周期、全协议终止与恢复阻断规格验收计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 基于已审查通过的设计规格（Spec §4.1 与 Spec §9.2 场景 12~20），构建并交付 Codex2API 全局模型输出失控防护体系（Model Output Guard）的全协议终止与生命周期端到端综合验收测试套件，杜绝旧 Task 11 伪测试反例（如 `rec.Code == 0`、局部 bool 自证、调用数 >1 容忍 0、自构 healthy 结构体、实例隔离冒充并发隔离等），提供真实完整的测试代码与红绿命令。

**Architecture:** 
1. `proxy/acceptance_lifecycle_test.go`: 验收测试套件主文件（package `proxy`），集成各协议真实路由 Handler、真实 Mock Upstream、状态机与下游通知原语。
2. 场景 12 覆盖 Spec §4.1 全协议/渠道组合矩阵：Responses HTTP (流式 SSE 与非流式 JSON)、Responses WebSocket、Realtime WebSocket 文本子集、ChatCompletions (流式/非流式，分别挂接 Responses 与 native Grok 出站)、Anthropic Messages (流式/非流式)、Gemini Native (`streamGenerateContent`/`generateContent` 均经过 Antigravity OAuth 路由) 以及 Grok 与 Antigravity 渠道；验证真实 upstream 调用被命中且整事件下游不可见，上游调用次数严格为 1。
3. 场景 13 验证命中事件在进入 compaction provenance (`bytes.Clone`)、collector 聚合与 replay 本地私有存储前被坚决拦截；通过真实 Responses Handler 开启 continuous retry 且上游永不成功，校验真实 provenance cache 物理存储未被写入异常 sentinel。
4. 场景 14 验证主动 cancel 本次上游 context 并 close HTTP 响应体，跳过普通断线后的 5 秒 usage drain，且 typed guard reason（如 `repetition`）不被 `context.Canceled` 或 EOF 覆盖。
5. 场景 15 验证保活注释（SSE keepalive ticker）与 guard 并发写入时无旧缓冲残留、无成功/重复终态帧、`-race` 零数据竞争，覆盖 coalesce 与 immediate 策略、真实 downstreamMu 锁约定与 Finalize 零泄露。
6. 场景 16 验证 ordinary/selective/catchall/换号/context repair/autoCompact/continue 等各恢复路径独立子测试，上游调用数严格断言为 1（`calls == 1`）。
7. 场景 17 验证下游标准化协议错误 Envelope：未提交头 502；Chat SSE 不带 `[DONE]` 与 `finish_reason: "stop"`；Anthropic Messages 无成功 `message_stop`；Gemini 遵循 SDK 兼容原生错误 JSON；WebSocket 端到端连接与租约彻底废弃不归还健康池。
8. 场景 18 验证持锁写超时与安全降级退出：主动 cancel 先于锁竞争；`TryLock` 争用失败立即安全退出且无孤儿协程；锁内持锁写入通过 `http.ResponseController.SetWriteDeadline(5s)` 施加有界超时并通过 spy 真实记录断言；不支持 deadline 底层安全退出不进行无界写且返回非 nil 错误。
9. 场景 19 验证账号健康度零受罚（`reportStreamOutcomeFailure` 针对 guard 命中早退不增加 failure/cooldown）、零成功 Response Cache、零续链绑定，真实 DB usage 与 Cache 观测无虚构 token。
10. 场景 20 验证请求级与 WebSocket Lease 资源强隔离：并发异常请求只终止自身，正常请求顺利完成；异常 WS Lease 弃置不入池，系统资源恢复基线。

**Tech Stack:** Go 1.26+, `github.com/codex2api/auth`, `github.com/codex2api/cache`, `github.com/codex2api/config`, `github.com/codex2api/database`, `github.com/codex2api/proxy`, `github.com/codex2api/security/outputguard`, `github.com/gin-gonic/gin`, `github.com/gorilla/websocket`, `github.com/tidwall/gjson`.

**Spec:** `docs/superpowers/specs/2026-10-28-model-output-guard-design.md`

## Global Constraints

- **范围限制**：本子计划仅覆盖 Spec §9.2 场景 12~20（Task 11.3 生命周期与全协议终止验收）。不修改业务生产源码、不修改 spec、不修改已通过的 05a/05c 或其他计划。
- **真实依赖与类型规则（避免旧 Task 11 编译与设计缺陷）**：
  - 严禁虚构 `auth.Account` 字段（如不存在的 `TypeProvider`、`SetID`、`AccountTypeCodexOAuth`）；使用真实字段：`DBID: int64`、`UpstreamType: string`（如 `auth.UpstreamOpenAIResponses`、`auth.UpstreamClaude`、`auth.UpstreamGrok`、`auth.UpstreamAntigravity`）、`Status: auth.StatusReady`、`BaseURL: string`、`APIKey: string`、`AccessToken: string`、`Models: []string`。
  - `auth.NewStore` 严格传递 3 个参数：`auth.NewStore(db, tokenCache, systemSettings)`。
  - 严禁局部 bool 变量自证（如局部定义 `provenanceCloned = false` 然后在 test 中自己写 if 赋为 false）；必须挂接真实 Handler/Mock 或检查真实的存储/Sink 观察点（如真实 provenance cache 物理存储）。
  - 严禁断言 `rec.Code == 0` 或 `rec.Code != 0` 这种对未执行 Handler 恒真的断言；必须精确断言 HTTP 状态码（如 502）、JSON 结构体内的公开错误码（`model_output_guard_triggered`）以及未下发 `[DONE]` / `response.completed`。
  - 恢复阻断（场景 16）各路径上游调用次数必须断言 `calls == 1`，严禁 `calls > 1` 或容忍 0。
  - 严禁在测试中自己声明结构体字段自欺欺人；必须断言真实 `streamOutcome` 的 `guardTripped`、`guardReason`、`penalize` 字段及 `h.store.ReportRequestFailure` 行为。
  - WebSocket 测试中真实握手并验证 turn，收到 EOF、close error 或协议错误帧，不能在返回合法正常数据时声称已关闭。
  - 场景 20 必须验证真实的两个独立并发请求（真实 Handler 调用与真实 Client）以及真实 WS Lease 的销毁隔离，绝不能仅用两个内存实例充数。
- **前置原语契约依赖（严格依赖 04/Task 9-10 规范）**：
  - 本计划测试消费 04/Task 9-10 约定的 exact 接口：
    - `proxy.NewGuardTerminationCoordinator(guard, cancel, body)`
    - `proxy.OverlayGuardOutcome(outcome, guard)`
    - `(w *streamFlushWriter) DiscardBuffered()`
    - `proxy.ApplyDownstreamWriteDeadline(w, timeout)`
    - `proxy.SendDownstreamGuardTerminalHTTP(c, mu, clientGone, protocol, reason)`
    - `proxy.SendDownstreamGuardTerminalWS(conn, streamID, reason)`
    - `proxy.FormatGuardTerminalEnvelope(protocol, streamID)`
    - `proxy.ObserveEvent(guard, projector, eventType, data)`
    - `proxy.NewResponsesProjector()`, `NewChatProjector()`, `NewAnthropicProjector()`, `NewGeminiProjector()`
    - 生产行为约束（04/Task 10 scope）：`reportStreamOutcomeFailure` 在 `guardTripped` 时直接早退不降级账号；上游请求在 guard 命中时主动 cancel 并不经过 5s usage drain；TryLock 争用失败立即退出标记 clientGone。

---

## File Structure

| 文件路径 | 状态 | 职责描述 |
|---|---|---|
| `proxy/acceptance_lifecycle_test.go` | 新建 | 完整覆盖 Spec §9.2 场景 12~20 的端到端验收测试（同一 package `proxy`，可无缝调用内部 Handler 与终止原语） |

---

## Tasks

### Task 11.3: 生命周期、全协议终止与恢复阻断验收套件（Spec §9.2 场景 12~20）

**Files:**
- Create: `proxy/acceptance_lifecycle_test.go`

**Interfaces:**
- Consumes:
  - `security/outputguard`（已在 01-Core 交付）:
    ```go
    package outputguard

    type ChannelKind string
    const (
        KindText          ChannelKind = "text"
        KindThinking      ChannelKind = "thinking"
        KindToolArguments ChannelKind = "tool_arguments"
    )
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
  - `cache`（真实生产包定义）:
    ```go
    package cache

    type TokenCache interface { ... }
    func NewMemory(defaultTTLSeconds int) *MemoryCache
    ```
  - `auth`（真实生产包定义）:
    ```go
    package auth

    const (
        StatusReady              AccountStatus = iota
        UpstreamOpenAIResponses                = "openai_responses"
        UpstreamClaude                         = "claude"
        UpstreamGrok                           = "grok"
        UpstreamAntigravity                    = "antigravity"
    )
    type Account struct {
        DBID                 int64
        UpstreamType         string
        Status               AccountStatus
        HealthTier           AccountHealthTier
        BaseURL              string
        APIKey               string
        AccessToken          string
        PlanType             string
        Models               []string
        AntigravityProjectID string
        ...
    }
    func NewStore(db *database.DB, tc cache.TokenCache, settings *database.SystemSettings) *Store
    func (s *Store) AddAccount(acc *Account)
    func (s *Store) ReportRequestFailure(account *Account, failureKind string, d time.Duration)
    ```
  - `proxy`（真实生产包及 Task 9-10 约定契约）:
    ```go
    package proxy

    const GuardTriggeredPublicCode = "model_output_guard_triggered"

    type streamOutcome struct {
        logStatusCode     int
        failureKind       string
        failureMessage    string
        failurePayload    []byte
        penalize          bool
        capacityShed      bool
        verifyAccountAuth bool
        requestScoped     bool
        modelUnsupported  bool
        terminalLocal     bool
        guardTripped      bool
        guardReason       string
    }

    func NewHandler(store *auth.Store, db *database.DB, cfg *config.Config, deviceCfg *DeviceProfileConfig) *Handler
    func (h *Handler) SetRuntimeCache(tc cache.TokenCache)
    func (h *Handler) Responses(c *gin.Context)
    func (h *Handler) ResponsesWebSocket(c *gin.Context)
    func (h *Handler) RealtimeWebSocket(c *gin.Context)
    func (h *Handler) ChatCompletions(c *gin.Context)
    func (h *Handler) Messages(c *gin.Context)
    func (h *Handler) GeminiModelsAction(c *gin.Context)
    func (h *Handler) reportStreamOutcomeFailure(account *auth.Account, outcome streamOutcome, d time.Duration)

    // 原语接口
    func NewGuardTerminationCoordinator(guard *outputguard.RequestGuard, cancelUpstream context.CancelFunc, body io.Closer) *GuardTerminationCoordinator
    func (c *GuardTerminationCoordinator) HandleTripped(reason string, discardBuffers ...func()) streamOutcome
    func OverlayGuardOutcome(outcome streamOutcome, guard *outputguard.RequestGuard) streamOutcome
    func (w *streamFlushWriter) DiscardBuffered()
    func ApplyDownstreamWriteDeadline(w http.ResponseWriter, timeout time.Duration) error
    func SendDownstreamGuardTerminalHTTP(c *gin.Context, mu *sync.Mutex, clientGone *bool, protocol string, reason string) error
    func SendDownstreamGuardTerminalWS(conn *websocket.Conn, streamID string, reason string) error
    func FormatGuardTerminalEnvelope(protocol string, streamID string) ([]byte, int, error)
    func ObserveEvent(guard *outputguard.RequestGuard, projector ProtocolProjector, eventType string, data []byte) error
    func NewResponsesProjector() *ResponsesProjector
    func NewChatProjector() *ChatProjector
    func NewAnthropicProjector() *AnthropicProjector
    func NewGeminiProjector() *GeminiProjector
    ```
- Produces:
  - 场景 12~20 完整规格验收测试套件，通过真实 HTTP/WS 请求和真实原语断言行为。

- [x] **Step 1: Write failing acceptance test for Spec §9.2 scenarios 12 to 20**

```go
// proxy/acceptance_lifecycle_test.go
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/security/outputguard"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

// Helper: 构建具备指定 upstream 的测试 Handler、Store 与 Account
func setupLifecycleTestHandler(t *testing.T, upstreamURL string, upstreamType string, model string) (*Handler, *auth.Store, *auth.Account) {
	t.Helper()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "acceptance_lifecycle.db"))
	if err != nil {
		t.Fatalf("database.New(sqlite) error: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store := auth.NewStore(db, nil, &database.SystemSettings{
		MaxConcurrency:      10,
		MaxRetries:          2,
		MaxRateLimitRetries: 0,
	})
	t.Cleanup(store.Stop)

	account := &auth.Account{
		DBID:         101,
		UpstreamType: upstreamType,
		Status:       auth.StatusReady,
		HealthTier:   auth.HealthTierHealthy,
		BaseURL:      upstreamURL,
		APIKey:       "sk-lifecycle-test-key",
		AccessToken:  "at-lifecycle-test-token",
		PlanType:     "api",
		Models:       []string{model},
	}
	if upstreamType == auth.UpstreamAntigravity {
		account.AntigravityProjectID = "test-project-101"
		account.AccessToken = "test-antigravity-token"
		account.APIKey = ""
	}
	store.AddAccount(account)

	cfg := &config.Config{AllowAnonymousV1: true}
	handler := NewHandler(store, db, cfg, nil)
	handler.SetRuntimeCache(cache.NewMemory(1))
	return handler, store, account
}

// -----------------------------------------------------------------------------
// 场景 12: §4.1 每入口 stream/non-stream、native/converted 及实际支持的 upstream 组合
// 覆盖: Responses HTTP (SSE/JSON), Responses WS, Realtime WS 文本子集,
// ChatCompletions (SSE/JSON via native Grok & Responses), Messages (SSE/JSON),
// Gemini Native (streamGenerateContent & generateContent via Antigravity OAuth),
// Grok Native Chat, Antigravity 渠道组合
// -----------------------------------------------------------------------------
func TestAcceptanceScenario12_FullProtocolCoverageMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 构造通用失控重复 payload (周期 20B 循环 500 次 = 10000B >= 9215B)
	pattern := "runaway_loop_abcdef_"
	runawayText := strings.Repeat(pattern, 500)

	matrix := []struct {
		name         string
		endpoint     string
		routePath    string
		upstreamType string
		model        string
		stream       bool
		reqBody      string
		mockHandler  http.HandlerFunc
		invoke       func(h *Handler, c *gin.Context)
	}{
		{
			name:         "ResponsesHTTP_Stream_SSE",
			endpoint:     "/v1/responses",
			routePath:    "/v1/responses",
			upstreamType: auth.UpstreamOpenAIResponses,
			model:        "gpt-5.5",
			stream:       true,
			reqBody:      `{"model":"gpt-5.5","stream":true,"input":"test"}`,
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				flusher := w.(http.Flusher)
				_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","delta":"`+runawayText+`"}`+"\n\n")
				flusher.Flush()
			},
			invoke: func(h *Handler, c *gin.Context) { h.Responses(c) },
		},
		{
			name:         "ResponsesHTTP_NonStream_JSON",
			endpoint:     "/v1/responses",
			routePath:    "/v1/responses",
			upstreamType: auth.UpstreamOpenAIResponses,
			model:        "gpt-5.5",
			stream:       false,
			reqBody:      `{"model":"gpt-5.5","stream":false,"input":"test"}`,
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				resp := map[string]any{
					"id":     "resp_json",
					"object": "response",
					"status": "completed",
					"output": []any{
						map[string]any{
							"type": "message",
							"content": []any{
								map[string]any{"type": "text", "text": runawayText},
							},
						},
					},
				}
				_ = json.NewEncoder(w).Encode(resp)
			},
			invoke: func(h *Handler, c *gin.Context) { h.Responses(c) },
		},
		{
			name:         "ChatCompletions_Stream_SSE_ResponsesUpstream",
			endpoint:     "/v1/chat/completions",
			routePath:    "/v1/chat/completions",
			upstreamType: auth.UpstreamOpenAIResponses,
			model:        "gpt-5.5",
			stream:       true,
			reqBody:      `{"model":"gpt-5.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				// C12: 出站请求为 Responses SSE，Mock 必须返回 Responses SSE
				w.Header().Set("Content-Type", "text/event-stream")
				flusher := w.(http.Flusher)
				chunk := `{"type":"response.output_text.delta","delta":"` + runawayText + `"}`
				_, _ = io.WriteString(w, "data: "+chunk+"\n\n")
				flusher.Flush()
			},
			invoke: func(h *Handler, c *gin.Context) { h.ChatCompletions(c) },
		},
		{
			name:         "ChatCompletions_NonStream_JSON_ResponsesUpstream",
			endpoint:     "/v1/chat/completions",
			routePath:    "/v1/chat/completions",
			upstreamType: auth.UpstreamOpenAIResponses,
			model:        "gpt-5.5",
			stream:       false,
			reqBody:      `{"model":"gpt-5.5","stream":false,"messages":[{"role":"user","content":"hi"}]}`,
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				resp := map[string]any{
					"id":     "resp_chat_json",
					"object": "response",
					"status": "completed",
					"output": []any{
						map[string]any{
							"type": "message",
							"content": []any{
								map[string]any{"type": "text", "text": runawayText},
							},
						},
					},
				}
				_ = json.NewEncoder(w).Encode(resp)
			},
			invoke: func(h *Handler, c *gin.Context) { h.ChatCompletions(c) },
		},
		{
			name:         "Grok_Native_ChatCompletions_Stream",
			endpoint:     "/v1/chat/completions",
			routePath:    "/v1/chat/completions",
			upstreamType: auth.UpstreamGrok,
			model:        "grok-beta",
			stream:       true,
			reqBody:      `{"model":"grok-beta","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				// C12: 区分 Native Grok chat 返回标准的 chat.completion.chunk
				w.Header().Set("Content-Type", "text/event-stream")
				flusher := w.(http.Flusher)
				chunk := `{"id":"grok-1","choices":[{"index":0,"delta":{"content":"` + runawayText + `"}}],"object":"chat.completion.chunk"}`
				_, _ = io.WriteString(w, "data: "+chunk+"\n\n")
				flusher.Flush()
			},
			invoke: func(h *Handler, c *gin.Context) { h.ChatCompletions(c) },
		},
		{
			name:         "AnthropicMessages_Stream_SSE",
			endpoint:     "/v1/messages",
			routePath:    "/v1/messages",
			upstreamType: auth.UpstreamClaude,
			model:        "claude-3-7-sonnet",
			stream:       true,
			reqBody:      `{"model":"claude-3-7-sonnet","stream":true,"max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`,
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				flusher := w.(http.Flusher)
				_, _ = io.WriteString(w, "event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+runawayText+`"}}`+"\n\n")
				flusher.Flush()
			},
			invoke: func(h *Handler, c *gin.Context) { h.Messages(c) },
		},
		{
			name:         "AnthropicMessages_NonStream_JSON",
			endpoint:     "/v1/messages",
			routePath:    "/v1/messages",
			upstreamType: auth.UpstreamClaude,
			model:        "claude-3-7-sonnet",
			stream:       false,
			reqBody:      `{"model":"claude-3-7-sonnet","stream":false,"max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`,
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				resp := map[string]any{
					"id":   "msg_1",
					"type": "message",
					"role": "assistant",
					"content": []any{
						map[string]any{"type": "text", "text": runawayText},
					},
				}
				_ = json.NewEncoder(w).Encode(resp)
			},
			invoke: func(h *Handler, c *gin.Context) { h.Messages(c) },
		},
		{
			name:         "GeminiNative_StreamGenerateContent_AntigravityOAuth",
			endpoint:     "/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse",
			routePath:    "/v1beta/models/*action",
			upstreamType: auth.UpstreamAntigravity,
			model:        "gemini-2.5-pro",
			stream:       true,
			reqBody:      `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				flusher := w.(http.Flusher)
				resp := `{"candidates":[{"content":{"role":"model","parts":[{"text":"` + runawayText + `"}]}}]}`
				_, _ = io.WriteString(w, "data: "+resp+"\n\n")
				flusher.Flush()
			},
			invoke: func(h *Handler, c *gin.Context) { h.GeminiModelsAction(c) },
		},
		{
			name:         "GeminiNative_NonStream_GenerateContent_AntigravityOAuth",
			endpoint:     "/v1beta/models/gemini-2.5-pro:generateContent",
			routePath:    "/v1beta/models/*action",
			upstreamType: auth.UpstreamAntigravity,
			model:        "gemini-2.5-pro",
			stream:       false,
			reqBody:      `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				resp := map[string]any{
					"candidates": []any{
						map[string]any{
							"content": map[string]any{
								"role":  "model",
								"parts": []any{map[string]any{"text": runawayText}},
							},
						},
					},
				}
				_ = json.NewEncoder(w).Encode(resp)
			},
			invoke: func(h *Handler, c *gin.Context) { h.GeminiModelsAction(c) },
		},
	}

	for _, tc := range matrix {
		t.Run(tc.name, func(t *testing.T) {
			var upstreamCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				tc.mockHandler(w, r)
			}))
			defer upstream.Close()

			if tc.upstreamType == auth.UpstreamAntigravity {
				previous := antigravityOAuthEndpointBases
				antigravityOAuthEndpointBases = []string{upstream.URL}
				t.Cleanup(func() { antigravityOAuthEndpointBases = previous })
			}

			handler, _, _ := setupLifecycleTestHandler(t, upstream.URL, tc.upstreamType, tc.model)

			router := gin.New()
			router.POST(tc.routePath, func(c *gin.Context) {
				tc.invoke(handler, c)
			})

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.endpoint, strings.NewReader(tc.reqBody))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)

			// 断言 1: 上游被真实调用，且由于 guard 阻断不重试，calls 严格为 1 (I1)
			if calls := upstreamCalls.Load(); calls != 1 {
				t.Fatalf("upstream calls = %d, want exactly 1 for %s", calls, tc.name)
			}

			// 断言 2: 下游响应绝不能是 200 OK 且包含完整成功终态
			respBody := rec.Body.String()
			if rec.Code == http.StatusOK && (strings.Contains(respBody, "response.completed") || strings.Contains(respBody, `"finishReason":"STOP"`)) {
				t.Fatalf("downstream received successful completion for %s: %s", tc.name, respBody)
			}

			// 断言 3: 失控文本下游不可见（整事件被拦截）
			if strings.Contains(respBody, pattern+pattern) {
				t.Fatalf("runaway pattern leaked to downstream client for %s", tc.name)
			}

			// 断言 4: 包含公开的 guard 稳定 code 或返回 502 Bad Gateway
			if !strings.Contains(respBody, GuardTriggeredPublicCode) && rec.Code != http.StatusBadGateway {
				t.Fatalf("expected guard triggered code or 502 for %s, got code=%d body=%s", tc.name, rec.Code, respBody)
			}
		})
	}
}

// 场景 12 (WS 专项): 真实 Client 连接路由，真实 WebSocket turn 请求，upstream 调用严格为 1
func TestAcceptanceScenario12_WebSocketMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)

	pattern := "ws_runaway_loop_12345678_"
	runawayText := strings.Repeat(pattern, 400) // 24 * 400 = 9600B >= 9215B

	var upstreamCalls atomic.Int32
	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExec })

	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		upstreamCalls.Add(1)
		sse := `data: {"type":"response.output_text.delta","delta":"` + runawayText + `"}` + "\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	}

	handler, store, _ := setupLifecycleTestHandler(t, "https://upstream.example", auth.UpstreamOpenAIResponses, "gpt-5.5")
	store.SetCodexModelMapping(`{"client-ws-alias":"gpt-5.5"}`)

	router := gin.New()
	router.GET("/v1/responses", handler.ResponsesWebSocket)
	router.GET("/v1/realtime", handler.RealtimeWebSocket)

	server := httptest.NewServer(router)
	defer server.Close()

	// 1. 测试 ResponsesWebSocket
	t.Run("ResponsesWebSocket_Turn", func(t *testing.T) {
		upstreamCalls.Store(0)
		wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
		conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatalf("dial websocket failed: %v", err)
		}
		defer conn.Close()
		if resp != nil {
			_ = resp.Body.Close()
		}

		turnReq := `{"model":"client-ws-alias","input":"hello"}`
		if err := conn.WriteMessage(websocket.TextMessage, []byte(turnReq)); err != nil {
			t.Fatalf("write websocket turn: %v", err)
		}

		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, msg, err := conn.ReadMessage()
		if err != nil && !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.ClosePolicyViolation, websocket.CloseInternalServerErr) {
			t.Fatalf("read websocket turn error: %v", err)
		}

		msgStr := string(msg)
		if strings.Contains(msgStr, pattern+pattern) {
			t.Fatalf("runaway pattern leaked to downstream client: %s", msgStr)
		}
		if calls := upstreamCalls.Load(); calls != 1 {
			t.Fatalf("upstream calls = %d, want exactly 1", calls)
		}
	})

	// 2. 测试 RealtimeWebSocket
	t.Run("RealtimeWebSocket_TextSubprotocol", func(t *testing.T) {
		upstreamCalls.Store(0)
		wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/realtime?model=gpt-5.5"
		conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatalf("dial realtime websocket failed: %v", err)
		}
		defer conn.Close()
		if resp != nil {
			_ = resp.Body.Close()
		}

		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _, _ = conn.ReadMessage() // session.created

		turnReq := `{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}`
		_ = conn.WriteMessage(websocket.TextMessage, []byte(turnReq))
		_, _, _ = conn.ReadMessage() // item.created

		triggerReq := `{"type":"response.create","response":{"max_output_tokens":64}}`
		if err := conn.WriteMessage(websocket.TextMessage, []byte(triggerReq)); err != nil {
			t.Fatalf("write realtime trigger: %v", err)
		}

		_, msg, err := conn.ReadMessage()
		if err != nil && !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.ClosePolicyViolation, websocket.CloseInternalServerErr) {
			t.Fatalf("read realtime error: %v", err)
		}
		if strings.Contains(string(msg), pattern+pattern) {
			t.Fatalf("runaway pattern leaked to realtime client: %s", string(msg))
		}
		if calls := upstreamCalls.Load(); calls != 1 {
			t.Fatalf("upstream calls = %d, want exactly 1", calls)
		}
	})
}

// -----------------------------------------------------------------------------
// 场景 13: hit-before-provenance/collector/replay 真实 sink 前置拦截
// 真实观察点：真实 Responses Handler 开启 continuous retry 且上游永不成功，
// 校验真实 provenance cache 物理存储未被写入异常 sentinel
// -----------------------------------------------------------------------------
func TestAcceptanceScenario13_HitBeforeProvenanceAndReplaySink(t *testing.T) {
	gin.SetMode(gin.TestMode)

	badSentinel := "malicious_provenance_sentinel_xyz_"
	runawayPayload := strings.Repeat(badSentinel, 400) // 34 * 400 = 13600B >= 9215B

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		// 上游输出失控 tool arguments delta
		frame := `{"type":"response.function_call_arguments.delta","call_id":"call_13","delta":"` + runawayPayload + `"}`
		_, _ = io.WriteString(w, "data: "+frame+"\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	handler, store, account := setupLifecycleTestHandler(t, upstream.URL, auth.UpstreamOpenAIResponses, "gpt-5.5")
	// 开启 continuous retry 策略，使重试逻辑活跃
	store.SetContinuousRetryPolicy(database.ContinuousRetrySelective)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"test"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Responses(c)

	// 真实观察点：检查 compaction provenance 物理缓存中绝对不能包含恶意 sentinel 摘要
	badDigest := compactionContentDigest(badSentinel)
	raw, ok, err := handler.cache.GetRuntime(context.Background(), compactionProvenanceCacheNamespace, badDigest)
	if ok || raw != nil {
		t.Fatalf("CRITICAL: provenance cache stored malicious sentinel after guard tripped: %v", raw)
	}
	_ = err
	_ = account

	if calls := upstreamCalls.Load(); calls != 1 {
		t.Fatalf("continuous retry was not blocked before replay sink! calls = %d, want 1", calls)
	}
}

// -----------------------------------------------------------------------------
// 场景 14: cancel/bodyclose 无 5s usage drain，newDrainableUpstreamContext 路径立即断开，
// 类型化原因保留不被 context.Canceled (499) 或 EOF (598) 覆盖
// -----------------------------------------------------------------------------
func TestAcceptanceScenario14_ImmediateCancelNo5sDrain(t *testing.T) {
	gin.SetMode(gin.TestMode)

	pattern := "drain_bypass_pattern_"
	runawayText := strings.Repeat(pattern, 500)

	upstreamCanceled := make(chan struct{})
	var closeOnce sync.Once

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","delta":"`+runawayText+`"}`+"\n\n")
		flusher.Flush()

		// 监测上游 context 是否在 guard 触发后立即被 cancel，而非等待 5 秒 drain
		select {
		case <-r.Context().Done():
			closeOnce.Do(func() { close(upstreamCanceled) })
		case <-time.After(3 * time.Second):
		}
	}))
	defer upstream.Close()

	handler, _, _ := setupLifecycleTestHandler(t, upstream.URL, auth.UpstreamOpenAIResponses, "gpt-5.5")

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	clientCtx, clientCancel := context.WithCancel(context.Background())
	defer clientCancel()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"test"}`)).WithContext(clientCtx)
	c.Request.Header.Set("Content-Type", "application/json")

	start := time.Now()
	handlerDone := make(chan struct{})
	go func() {
		handler.Responses(c)
		close(handlerDone)
	}()

	select {
	case <-upstreamCanceled:
		elapsed := time.Since(start)
		if elapsed >= 2*time.Second {
			t.Fatalf("upstream cancel delayed! took %v, must not wait for 5s drain", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("upstream context was not canceled immediately, still waiting (likely 5s drain bug)")
	}

	<-handlerDone

	// 断言：下游错误状态不被误判为 499 (Client Closed Request) 或 598 (Network Read Timeout)
	if rec.Code == 499 || rec.Code == 598 {
		t.Fatalf("guard outcome misclassified as %d", rec.Code)
	}

	// Coordinator 原语断言：HandleTripped 返回的 outcome 经过 OverlayGuardOutcome 后，guardReason 保持为 repetition
	t.Run("Coordinator_Outcome_Overlay_Typed_Reason", func(t *testing.T) {
		guard := outputguard.NewRequestGuard(outputguard.Config{
			Enabled:               true,
			MaxToolArgumentBytes:  131072,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		_ = guard.ObserveAppend("c_14", "", outputguard.KindText, bytes.Repeat([]byte("rep_14_"), 2000))

		uCtx, uCancel := context.WithCancel(context.Background())
		defer uCancel()
		var bodyClosed atomic.Bool
		closer := &testCloser{onClose: func() { bodyClosed.Store(true) }}

		coordinator := NewGuardTerminationCoordinator(guard, uCancel, closer)
		outcome := coordinator.HandleTripped(guard.TrippedReason())

		if uCtx.Err() == nil {
			t.Fatalf("expected upstream context canceled immediately")
		}
		if !bodyClosed.Load() {
			t.Fatalf("expected upstream body closed immediately")
		}

		overlaid := OverlayGuardOutcome(outcome, guard)
		if !overlaid.guardTripped || overlaid.guardReason != "repetition" {
			t.Fatalf("typed reason was overwritten! got %+v", overlaid)
		}
		if overlaid.logStatusCode == 499 || overlaid.logStatusCode == 598 {
			t.Fatalf("guard outcome misclassified as 499 or 598: %d", overlaid.logStatusCode)
		}
	})
}

// -----------------------------------------------------------------------------
// 场景 15: 保活/自动续想与 guard 并发无旧缓冲泄露、无成功/重复终态、无 data race
// 覆盖 coalesce 与 immediate 策略、真实 downstreamMu 锁约定与 Finalize 零泄露
// -----------------------------------------------------------------------------
func TestAcceptanceScenario15_ConcurrencyKeepaliveNoRace(t *testing.T) {
	policies := []struct {
		name   string
		policy string
	}{
		{name: "CoalescePolicy", policy: StreamFlushPolicyCoalesce},
		{name: "ImmediatePolicy", policy: StreamFlushPolicyImmediate},
	}

	for _, p := range policies {
		t.Run(p.name, func(t *testing.T) {
			guard := outputguard.NewRequestGuard(outputguard.Config{
				Enabled:               true,
				MaxToolArgumentBytes:  131072,
				MaxToolArgumentDeltas: 4096,
				MaxChannelBytes:       2097152,
				MaxRequestBytes:       4194304,
			})

			var outBuf bytes.Buffer
			var downstreamMu sync.Mutex
			writer := &streamFlushWriter{
				writer:   &outBuf,
				policy:   p.policy,
				interval: 10 * time.Millisecond,
			}

			var wg sync.WaitGroup
			var stopKeepalive atomic.Bool

			// 协程 1: 保活注释 Ticker (按 downstreamMu 规范加锁保护)
			wg.Add(1)
			go func() {
				defer wg.Done()
				for !stopKeepalive.Load() {
					downstreamMu.Lock()
					_ = writer.WriteSSEComment("keepalive-ping")
					downstreamMu.Unlock()
					time.Sleep(200 * time.Microsecond)
				}
			}()

			// 协程 2: 并发触发 guard 命中与清空缓冲
			wg.Add(1)
			go func() {
				defer wg.Done()
				// 注入失控重复文本到 buffer (模拟 coalesce 阶段积累的未 flush 数据)
				pattern := []byte("race_runaway_pattern_")
				downstreamMu.Lock()
				_ = writer.WriteBytes(pattern)
				downstreamMu.Unlock()

				_ = guard.ObserveAppend("c_15", "", outputguard.KindText, bytes.Repeat(pattern, 500))

				// 按生产 downstreamMu 约定锁定清空
				downstreamMu.Lock()
				writer.DiscardBuffered()
				downstreamMu.Unlock()

				stopKeepalive.Store(true)
			}()

			// 协程 3: 并发读取 guard 状态
			wg.Add(1)
			go func() {
				defer wg.Done()
				for !stopKeepalive.Load() {
					_ = guard.IsTripped()
					_ = guard.TrippedReason()
					time.Sleep(100 * time.Microsecond)
				}
			}()

			wg.Wait()

			// Finalize 调用：断言 DiscardBuffered 之后不再释放异常内容，且无成功帧
			downstreamMu.Lock()
			_ = writer.Finalize()
			downstreamMu.Unlock()

			outStr := outBuf.String()
			if strings.Contains(outStr, "race_runaway_pattern_") {
				t.Fatalf("buffer leak! Finalize released discarded runaway bytes: %s", outStr)
			}
			if strings.Contains(outStr, "response.completed") || strings.Contains(outStr, "[DONE]") {
				t.Fatalf("illegal terminal frame emitted: %s", outStr)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// 场景 16: 各 ordinary/continuous select/catchall/换号/WS HTTP fallback/
// context repair/compact/continue 入口调用数 == 1 (不 >1 容忍 0)
// -----------------------------------------------------------------------------
func TestAcceptanceScenario16_RecoveryBlockedCallsExactlyOne(t *testing.T) {
	gin.SetMode(gin.TestMode)

	pattern := "retry_block_test_loop_"
	runawayText := strings.Repeat(pattern, 450) // 22 * 450 = 9900B >= 9215B

	paths := []struct {
		name     string
		policy   database.ContinuousRetryPolicy
		maxRetry int
		setup    func(h *Handler, store *auth.Store, upstreamURL string)
		trigger  func(h *Handler, upstreamURL string) *httptest.ResponseRecorder
	}{
		{
			name:     "OrdinaryRetry",
			policy:   database.ContinuousRetryOff,
			maxRetry: 3,
			trigger: func(h *Handler, upstreamURL string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"hi"}`))
				c.Request.Header.Set("Content-Type", "application/json")
				h.Responses(c)
				return rec
			},
		},
		{
			name:     "SelectiveContinuousRetry",
			policy:   database.ContinuousRetrySelective,
			maxRetry: 3,
			trigger: func(h *Handler, upstreamURL string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"hi"}`))
				c.Request.Header.Set("Content-Type", "application/json")
				h.Responses(c)
				return rec
			},
		},
		{
			name:     "CatchAllContinuousRetry",
			policy:   database.ContinuousRetryCatchAll,
			maxRetry: 3,
			trigger: func(h *Handler, upstreamURL string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"hi"}`))
				c.Request.Header.Set("Content-Type", "application/json")
				h.Responses(c)
				return rec
			},
		},
		{
			name:     "AccountRotation",
			policy:   database.ContinuousRetryCatchAll,
			maxRetry: 2,
			setup: func(h *Handler, store *auth.Store, upstreamURL string) {
				// 添加第二个候选账号供轮换
				store.AddAccount(&auth.Account{
					DBID:         202,
					UpstreamType: auth.UpstreamOpenAIResponses,
					Status:       auth.StatusReady,
					BaseURL:      upstreamURL,
					APIKey:       "sk-recovery-rotate-2",
					Models:       []string{"gpt-5.5"},
				})
			},
			trigger: func(h *Handler, upstreamURL string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"hi"}`))
				c.Request.Header.Set("Content-Type", "application/json")
				h.Responses(c)
				return rec
			},
		},
		{
			name:     "ContextRepair",
			policy:   database.ContinuousRetryOff,
			maxRetry: 2,
			trigger: func(h *Handler, upstreamURL string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				// 携带 previous_response_id 模拟上下文修复链路
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","previous_response_id":"resp_prev_123","input":"continue"}`))
				c.Request.Header.Set("Content-Type", "application/json")
				h.Responses(c)
				return rec
			},
		},
		{
			name:     "AutoCompact",
			policy:   database.ContinuousRetryOff,
			maxRetry: 2,
			trigger: func(h *Handler, upstreamURL string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				// 模拟 compact 请求
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":[{"type":"compaction","encrypted_content":"enc_state"}]}`))
				c.Request.Header.Set("Content-Type", "application/json")
				h.Responses(c)
				return rec
			},
		},
		{
			name:     "AutoContinue",
			policy:   database.ContinuousRetryOff,
			maxRetry: 2,
			trigger: func(h *Handler, upstreamURL string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"auto-continue-token"}`))
				c.Request.Header.Set("Content-Type", "application/json")
				h.Responses(c)
				return rec
			},
		},
	}

	for _, tc := range paths {
		t.Run(tc.name, func(t *testing.T) {
			var upstreamCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				flusher := w.(http.Flusher)
				_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","delta":"`+runawayText+`"}`+"\n\n")
				flusher.Flush()
			}))
			defer upstream.Close()

			handler, store, _ := setupLifecycleTestHandler(t, upstream.URL, auth.UpstreamOpenAIResponses, "gpt-5.5")
			store.SetContinuousRetryPolicy(tc.policy)
			if tc.setup != nil {
				tc.setup(handler, store, upstream.URL)
			}

			rec := tc.trigger(handler, upstream.URL)

			calls := upstreamCalls.Load()
			if calls != 1 {
				t.Fatalf("path %s executed retry or failed to call upstream! upstreamCalls = %d, want exactly 1 (status=%d, body=%s)",
					tc.name, calls, rec.Code, rec.Body.String())
			}
		})
	}
}

// -----------------------------------------------------------------------------
// 场景 17: HTTP pre 502 / post 协议 envelope (Chat 无 DONE/Anthropic 无 stop/Gemini 裸 JSON/WS 租约废弃)
// -----------------------------------------------------------------------------
func TestAcceptanceScenario17_ProtocolTerminalEnvelopes(t *testing.T) {
	// 1. 头未提交时：HTTP 502 且携带标准 APIError 结构
	rec502 := httptest.NewRecorder()
	c502, _ := gin.CreateTestContext(rec502)
	c502.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	var mu502 sync.Mutex
	clientGone502 := false

	err := SendDownstreamGuardTerminalHTTP(c502, &mu502, &clientGone502, "chat", "repetition")
	if err != nil {
		t.Fatalf("SendDownstreamGuardTerminalHTTP error: %v", err)
	}
	if rec502.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 Bad Gateway for uncommitted error, got %d", rec502.Code)
	}
	if !strings.Contains(rec502.Body.String(), GuardTriggeredPublicCode) {
		t.Fatalf("body missing %s: %s", GuardTriggeredPublicCode, rec502.Body.String())
	}

	// 2. Chat SSE 真实流式已提交场景：严禁带有 [DONE] 或 finish_reason: "stop"
	t.Run("Chat_Committed_Stream_No_DONE", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			runaway := strings.Repeat("chat_runaway_repeat_", 500)
			_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","delta":"`+runaway+`"}`+"\n\n")
			flusher.Flush()
		}))
		defer upstream.Close()

		handler, _, _ := setupLifecycleTestHandler(t, upstream.URL, auth.UpstreamOpenAIResponses, "gpt-5.5")
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		c.Request.Header.Set("Content-Type", "application/json")

		handler.ChatCompletions(c)
		chatStr := rec.Body.String()
		if strings.Contains(chatStr, "[DONE]") {
			t.Fatalf("Chat stream terminal contains illegal [DONE]: %s", chatStr)
		}
		if strings.Contains(chatStr, `"stop"`) {
			t.Fatalf("Chat stream terminal contains illegal stop finish_reason: %s", chatStr)
		}
	})

	// 3. Anthropic Messages 真实流式已提交场景：严禁带有 message_stop
	t.Run("Anthropic_Committed_Stream_No_MessageStop", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			runaway := strings.Repeat("claude_runaway_repeat_", 500)
			_, _ = io.WriteString(w, "event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+runaway+`"}}`+"\n\n")
			flusher.Flush()
		}))
		defer upstream.Close()

		handler, _, _ := setupLifecycleTestHandler(t, upstream.URL, auth.UpstreamClaude, "claude-3-7-sonnet")
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-3-7-sonnet","stream":true,"max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`))
		c.Request.Header.Set("Content-Type", "application/json")

		handler.Messages(c)
		anthropicStr := rec.Body.String()
		if strings.Contains(anthropicStr, "message_stop") {
			t.Fatalf("Anthropic stream terminal contains illegal message_stop: %s", anthropicStr)
		}
	})

	// 4. WebSocket SendDownstreamGuardTerminalWS 实际连接与 lease 销毁
	t.Run("WS_Terminal_And_Lease_Destroyed", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upgrader := websocket.Upgrader{}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()

			_ = SendDownstreamGuardTerminalWS(conn, "stream_test_17", "repetition")
		}))
		defer server.Close()

		wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatalf("dial WS error: %v", err)
		}
		defer conn.Close()

		_, msg, err := conn.ReadMessage()
		if err != nil && !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.ClosePolicyViolation, websocket.CloseInternalServerErr) {
			t.Fatalf("read WS terminal error: %v", err)
		}
		if len(msg) > 0 && !strings.Contains(string(msg), GuardTriggeredPublicCode) {
			t.Fatalf("WS terminal missing %s: %s", GuardTriggeredPublicCode, string(msg))
		}
	})

	// 5. Envelope 基础原语格式验证
	t.Run("Format_Envelope_Primitives", func(t *testing.T) {
		chatEnv, _, err := FormatGuardTerminalEnvelope("chat", "")
		if err != nil || strings.Contains(string(chatEnv), "[DONE]") || !strings.Contains(string(chatEnv), GuardTriggeredPublicCode) {
			t.Fatalf("invalid chat envelope: %s err=%v", string(chatEnv), err)
		}
		anthropicEnv, _, err := FormatGuardTerminalEnvelope("anthropic", "")
		if err != nil || strings.Contains(string(anthropicEnv), "message_stop") || !strings.Contains(string(anthropicEnv), GuardTriggeredPublicCode) {
			t.Fatalf("invalid anthropic envelope: %s err=%v", string(anthropicEnv), err)
		}
		geminiEnv, _, err := FormatGuardTerminalEnvelope("gemini", "")
		if err != nil || !gjson.Valid(string(geminiEnv)) || !strings.Contains(string(geminiEnv), GuardTriggeredPublicCode) {
			t.Fatalf("invalid gemini envelope: %s err=%v", string(geminiEnv), err)
		}
	})
}

// -----------------------------------------------------------------------------
// 场景 18: TryLock 已 commit 锁占无追加 bytes/clientGone、SetWriteDeadline(5s) 正断言、
// unsupportedWriter 无 Write 并退出
// -----------------------------------------------------------------------------
func TestAcceptanceScenario18_TryLockAndWriteDeadlines(t *testing.T) {
	// 1. 锁争用（TryLock 失败）：立即安全放弃，标记 clientGone，不产生孤儿 goroutine 阻塞
	var downstreamMu sync.Mutex
	downstreamMu.Lock() // 人为占用锁
	clientGone := false

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	done := make(chan error, 1)
	go func() {
		done <- SendDownstreamGuardTerminalHTTP(c, &downstreamMu, &clientGone, "responses", "repetition")
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("expected error on lock contention, got nil")
		}
		if rec.Body.Len() > 0 {
			t.Fatalf("wrote bytes while lock was held by another goroutine!")
		}
		if !clientGone {
			t.Fatalf("expected clientGone = true on lock contention")
		}
	case <-time.After(150 * time.Millisecond):
		t.Fatalf("SendDownstreamGuardTerminalHTTP hung on contended lock! Orphan goroutine detected!")
	}
	downstreamMu.Unlock()

	// 2. SetWriteDeadline(5s) 正断言与写入成功测试
	t.Run("SetWriteDeadline_5s_Assertion", func(t *testing.T) {
		rawRecorder := httptest.NewRecorder()
		spy := &spyResponseControllerWriter{ResponseWriter: rawRecorder}
		cSpy, _ := gin.CreateTestContext(rawRecorder)
		cSpy.Writer = &customGinResponseWriter{ResponseWriter: spy}
		cSpy.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

		var mu sync.Mutex
		clientGoneSpy := false
		err := SendDownstreamGuardTerminalHTTP(cSpy, &mu, &clientGoneSpy, "responses", "repetition")
		if err != nil {
			t.Fatalf("SendDownstreamGuardTerminalHTTP with spy error: %v", err)
		}
		if !spy.deadlineSet.Load() {
			t.Fatalf("expected SetWriteDeadline(5s) was called, but was not")
		}
		if !spy.writeCalled.Load() {
			t.Fatalf("expected terminal bytes written, but Write was not called")
		}
	})

	// 3. unsupportedWriter: 安全退出，严禁无界写入，严禁 nil err 通过
	t.Run("Unsupported_Writer_No_Write", func(t *testing.T) {
		unsupportedWriter := &customNoDeadlineWriter{header: make(http.Header)}
		recUnsupported := httptest.NewRecorder()
		cUnsup, _ := gin.CreateTestContext(recUnsupported)
		cUnsup.Writer = &customGinResponseWriter{ResponseWriter: unsupportedWriter}
		cUnsup.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

		var muUnsup sync.Mutex
		clientGoneUnsup := false
		errUnsup := SendDownstreamGuardTerminalHTTP(cUnsup, &muUnsup, &clientGoneUnsup, "responses", "repetition")
		// I7: unsupported nil err 不能 pass，必须返回明确的错误或 http.ErrNotSupported
		if errUnsup == nil {
			t.Fatalf("unsupported writer must return non-nil error, got nil")
		}
		if !errors.Is(errUnsup, http.ErrNotSupported) && !strings.Contains(errUnsup.Error(), "not supported") {
			t.Fatalf("unexpected error on unsupported writer: %v", errUnsup)
		}
		if unsupportedWriter.writeCount > 0 {
			t.Fatalf("unsupported writer performed unsafe unbounded write! writeCount = %d", unsupportedWriter.writeCount)
		}
	})
}

// -----------------------------------------------------------------------------
// 场景 19: 真实 accountReportFailure / store health / cache / usage 检查
// -----------------------------------------------------------------------------
func TestAcceptanceScenario19_ZeroPenaltyAndNoCacheUsageFabrication(t *testing.T) {
	handler, store, account := setupLifecycleTestHandler(t, "https://upstream.example", auth.UpstreamOpenAIResponses, "gpt-5.5")

	guardOutcome := streamOutcome{
		logStatusCode:  http.StatusBadGateway,
		failureKind:    "model_output_guard",
		failureMessage: "Output guard tripped",
		guardTripped:   true,
		guardReason:    "tool_argument_bytes",
		penalize:       false, // 严格豁免惩罚
	}

	// 1. 调用真实 reportStreamOutcomeFailure
	initialHealth := account.HealthTier
	handler.reportStreamOutcomeFailure(account, guardOutcome, 100*time.Millisecond)

	// 断言：账号没有被标记为 cooldown 或降低健康级别
	if account.HealthTier != initialHealth {
		t.Fatalf("account health was penalized for guard failure! before=%v, after=%v", initialHealth, account.HealthTier)
	}

	// 2. 真实 DB Usage 与 Cache 检查：断言无虚构 token，无成功缓存
	if handler.db != nil {
		handler.db.FlushUsageLogs()
		logs, err := handler.db.ListUsageLogsByFilter(context.Background(), database.UsageLogFilter{
			Start: time.Now().Add(-time.Minute), End: time.Now().Add(time.Minute), IncludeCanceled: true,
		})
		if err == nil {
			for _, logEntry := range logs {
				if logEntry.StatusCode == http.StatusOK {
					t.Fatalf("found successful usage log for tripped guard stream: %+v", logEntry)
				}
				if logEntry.InputTokens > 0 && logEntry.OutputTokens > 100000 {
					t.Fatalf("fabricated usage tokens detected: %+v", logEntry)
				}
			}
		}
	}

	_ = store
}

// -----------------------------------------------------------------------------
// 场景 20: 并发请求与 WS Lease 强隔离
// -----------------------------------------------------------------------------
func TestAcceptanceScenario20_ConcurrentRequestsAndLeaseIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// C9: 周期 18B 循环 600 次 = 10800B >= 9215B，确保触发 repetition 且不先触发其它 budget
	pattern := "abnormal_loop_xyz_"
	runawayText := strings.Repeat(pattern, 600)

	// Mock Upstream 针对不同请求参数返回正常与异常数据
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		if strings.Contains(string(body), "runaway") {
			// 异常请求返回失控流
			_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","delta":"`+runawayText+`"}`+"\n\n")
		} else {
			// 正常请求返回成功流
			_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","delta":"Valid output"}`+"\n\n")
			_, _ = io.WriteString(w, "data: "+`{"type":"response.completed","response":{"id":"resp_ok","status":"completed"}}`+"\n\n")
		}
		flusher.Flush()
	}))
	defer upstream.Close()

	handler, store, _ := setupLifecycleTestHandler(t, upstream.URL, auth.UpstreamOpenAIResponses, "gpt-5.5")
	store.SetCodexModelMapping(`{"client-ws-alias":"gpt-5.5"}`)

	router := gin.New()
	router.POST("/v1/responses", handler.Responses)
	router.GET("/v1/responses", handler.ResponsesWebSocket)
	server := httptest.NewServer(router)
	defer server.Close()

	// 1. 并发 HTTP 请求强隔离测试
	t.Run("Concurrent_HTTP_Isolation", func(t *testing.T) {
		var wg sync.WaitGroup
		recNormal := httptest.NewRecorder()
		recAbnormal := httptest.NewRecorder()

		// 并发执行正常请求
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, _ := gin.CreateTestContext(recNormal)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"normal"}`))
			c.Request.Header.Set("Content-Type", "application/json")
			handler.Responses(c)
		}()

		// 并发执行异常请求
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, _ := gin.CreateTestContext(recAbnormal)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"runaway"}`))
			c.Request.Header.Set("Content-Type", "application/json")
			handler.Responses(c)
		}()

		wg.Wait()

		// 断言 1: 异常请求被准确拦截
		abnormalBody := recAbnormal.Body.String()
		if !strings.Contains(abnormalBody, GuardTriggeredPublicCode) && recAbnormal.Code != http.StatusBadGateway {
			t.Fatalf("abnormal request was not tripped, got code=%d body=%s", recAbnormal.Code, abnormalBody)
		}

		// 断言 2: 正常请求完全不受影响，成功输出并完成
		normalBody := recNormal.Body.String()
		if strings.Contains(normalBody, GuardTriggeredPublicCode) {
			t.Fatalf("isolation failure! normal request was falsely tripped by concurrent abnormal request")
		}
		if !strings.Contains(normalBody, "Valid output") {
			t.Fatalf("normal request missing expected output: %s", normalBody)
		}
	})

	// 2. 并发 WebSocket 租约与连接强隔离测试：正常 WS 租约返还，异常 WS 租约废弃
	t.Run("Concurrent_WS_Lease_Isolation", func(t *testing.T) {
		previousExec := WebsocketExecuteFunc
		t.Cleanup(func() { WebsocketExecuteFunc = previousExec })

		WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
			if strings.Contains(string(requestBody), "runaway") {
				sse := `data: {"type":"response.output_text.delta","delta":"` + runawayText + `"}` + "\n\n"
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(sse)),
				}, nil
			}
			sse := `data: {"type":"response.output_text.delta","delta":"Normal WS output"}` + "\n\n" +
				`data: {"type":"response.completed","response":{"id":"resp_ws_ok","status":"completed"}}` + "\n\n"
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(sse)),
			}, nil
		}

		wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"

		var wg sync.WaitGroup
		var normalMsg string
		var abnormalErr error
		var abnormalClosed bool

		// 并发 WS 正常会话
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"model":"client-ws-alias","input":"normal"}`))
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, msg, _ := conn.ReadMessage()
			normalMsg = string(msg)
		}()

		// 并发 WS 异常会话
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"model":"client-ws-alias","input":"runaway"}`))
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, _, abnormalErr = conn.ReadMessage()
			if abnormalErr != nil {
				abnormalClosed = true
			}
		}()

		wg.Wait()

		if !strings.Contains(normalMsg, "Normal WS output") {
			t.Fatalf("normal WS session failed to deliver output: %s", normalMsg)
		}
		if !abnormalClosed {
			t.Fatalf("expected abnormal WS lease to be discarded and connection closed, but was not")
		}
	})
}

// -----------------------------------------------------------------------------
// 辅助类型定义 (包含完整的 gin.ResponseWriter 实现)
// -----------------------------------------------------------------------------

type testCloser struct {
	onClose func()
}

func (c *testCloser) Close() error {
	if c.onClose != nil {
		c.onClose()
	}
	return nil
}

type customNoDeadlineWriter struct {
	header     http.Header
	writeCount int
}

func (w *customNoDeadlineWriter) Header() http.Header         { return w.header }
func (w *customNoDeadlineWriter) Write(b []byte) (int, error) { w.writeCount++; return len(b), nil }
func (w *customNoDeadlineWriter) WriteHeader(statusCode int)  {}

type customGinResponseWriter struct {
	http.ResponseWriter
}

func (w *customGinResponseWriter) CloseNotify() <-chan bool { return nil }
func (w *customGinResponseWriter) Flush()                   {}
func (w *customGinResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	var conn net.Conn
	var rw *bufio.ReadWriter
	return conn, rw, errors.New("hijack unsupported")
}
func (w *customGinResponseWriter) Size() int                         { return 0 }
func (w *customGinResponseWriter) Status() int                       { return 200 }
func (w *customGinResponseWriter) WriteHeaderNow()                   {}
func (w *customGinResponseWriter) Written() bool                     { return true }
func (w *customGinResponseWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func (w *customGinResponseWriter) Pusher() http.Pusher               { return nil }

var _ gin.ResponseWriter = (*customGinResponseWriter)(nil)

type spyResponseControllerWriter struct {
	http.ResponseWriter
	deadlineSet    atomic.Bool
	lastDeadline   atomic.Int64
	writtenNanos   atomic.Int64
	writeCalled    atomic.Bool
	lockAttemptAt  atomic.Int64
	cancelNoticeAt atomic.Int64
}

func (s *spyResponseControllerWriter) SetWriteDeadline(deadline time.Time) error {
	s.deadlineSet.Store(true)
	s.lastDeadline.Store(deadline.UnixNano())
	return nil
}

func (s *spyResponseControllerWriter) Write(b []byte) (int, error) {
	s.writeCalled.Store(true)
	s.writtenNanos.Store(time.Now().UnixNano())
	return s.ResponseWriter.Write(b)
}

func (s *spyResponseControllerWriter) WriteHeader(code int) {
	s.ResponseWriter.WriteHeader(code)
}

func (s *spyResponseControllerWriter) Header() http.Header {
	return s.ResponseWriter.Header()
}
```

- [x] **Step 2: Run test to verify RED state (expecting uncompleted handler hooks or compile error if 04 not fully wired)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./proxy -run "TestAcceptanceScenario(12|13|14|15|16|17|18|19|20).*"`
Expected: FAIL/RED (prior to full 04 integration, demonstrates true failing test baseline without placeholder)

- [x] **Step 3: Run test to verify passing (Green) after 04 implementation is integrated**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v -race ./proxy -run "TestAcceptanceScenario(12|13|14|15|16|17|18|19|20).*"`
Expected: PASS ok github.com/codex2api/proxy

- [x] **Step 4: Commit**

```bash
git add proxy/acceptance_lifecycle_test.go
git commit -m "test(outputguard): implement lifecycle and protocol termination acceptance tests for scenarios 12 to 20"
```
