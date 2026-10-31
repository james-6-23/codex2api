# 02b-Runtime: 模型输出失控防护运行时快照、后台轮询器与冷启动门禁 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 依据 Spec §7 与 §9.22/23，实现 Codex2API 全局模型输出失控防护运行时状态机（`proxy/output_guard_settings.go`，Task 5），提供不可变 `OutputGuardAppliedConfig` 快照、5s 周期后台 Revision 轮询自愈器（带 3s 超时与 `db.RunBackgroundTask` 优雅生命周期）、在途快照隔离与非法配置 LKG 保护、冷启动失败非致命 503 门禁以及 HTTP/WS/Stream 就绪检查函数。

**Architecture:**
1. `proxy/output_guard_settings.go`: 进程级状态容器，`sync.RWMutex` 保护不可变配置快照、同步时间戳与错误；`atomic.Bool` 存储 `isReady` 门禁标记。对外输出统一名称 `OutputGuardAppliedConfig`（值不可变）、`OutputGuardConfigSyncStatus`，提供生产接口 `ApplyOutputGuardSettings`、`CurrentOutputGuardConfig`、`GetOutputGuardConfigSyncStatus`、`IsOutputGuardReady`、`LoadOutputGuardSettings` 与 `StartOutputGuardConfigPoller`。
2. 5s 后台轮询器与优雅生命周期：基于 `database.DB.RunBackgroundTask` 挂载后台 worker，轮询间隔 5s、单次读取超时 3s；注入外部 reader 接口（`OutputGuardSettingsReader`）与自定义 ticker/timeout。陈旧 Revision（`<= current.Generation`）幂等拒绝；非法配置保留 Last-Known-Good (LKG) 并记录诊断；新 Revision 纯值拷贝并置 `isReady.Store(true)`。
3. 冷启动未就绪强阻断与非致命启动：`main.go` 启动调用 `LoadOutputGuardSettings` 失败记录警告并启动后台 Poller 等待自愈，绝不 `log.Fatalf`；未就绪期间，`RequireOutputGuardReadyHTTP` 返回 HTTP 503 与稳定错误码 `api.ErrCodeServiceUnavailable`，`RequireOutputGuardReadyWS` 发送 JSON 错误帧并以 1013 CloseTryAgainLater 关闭连接，`RequireOutputGuardReadyStream` 发送关联 stream 503 错误帧。冷启动若读取到显式 `Enabled = false`，属于成功加载，标记就绪放行生成流量，wire framing 保留。
4. 05a 验收契约测试桥接：在 `proxy/output_guard_settings_test.go` 中提供 `PollOutputGuardConfigOnceForTest(ctx, db)`（包装私有单次 poll，不污染生产初始化）与 `ResetOutputGuardStateForTest()`、`SetOutputGuardReadyForTest(bool)` 测试辅助函数。

**Tech Stack:** Go 1.26+, `github.com/codex2api/api`, `github.com/codex2api/database`, `github.com/gin-gonic/gin`, `github.com/gorilla/websocket`, `context`, `sync`, `sync/atomic`, `time`.

**Spec:** `docs/superpowers/specs/2026-10-28-model-output-guard-design.md` (§7 与 §9.22/23)

## Global Constraints

- **范围限制**：仅覆盖 Task 5 运行时状态机、轮询器与门禁检查；不承担 Task 4（数据库表与 CAS）或 Task 6（Admin UI/REST API）；不修改其他模块、spec、candidate 或已有业务代码。
- **Contract 真实性**：Module 为 `github.com/codex2api`；消费 Task 4 未来契约：`database.OutputGuardSettings`（含 `Enabled *bool`、`ConfigGeneration int64` 等字段）、`database.ValidateOutputGuardSettings` 与 `database.DB.GetOutputGuardSettings`。
- **命名规范与不可变性**：配置结构体为纯值类型 `OutputGuardAppliedConfig`（字段含 `Enabled bool`、`MaxToolArgumentBytes int64`、`MaxToolArgumentDeltas int`、`MaxChannelBytes int64`、`MaxRequestBytes int64`、`Generation int64`），`CurrentOutputGuardConfig()` 返回不可变值拷贝。同步状态为 `OutputGuardConfigSyncStatus`（`LastSuccessfulSyncAt time.Time`、`LastSyncError string`）。
- **并发与内存安全**：读写互斥锁 `mu sync.RWMutex` 保证浅拷贝与更新零 race；就绪状态使用 `atomic.Bool` 保证高频无锁访问。
- **生命周期与协程泄露防护**：后台轮询器必须通过 `db.RunBackgroundTask` 挂载，不产生孤儿 goroutine；读取严格绑定 3 秒超时 context，主 context 取消时立即优雅退出 ticker 循环。
- **错误与门禁规范**：冷启动未就绪绝不调用 `log.Fatalf`，服务正常启动并进入未就绪等待状态；门禁失败统一生成 HTTP 503 / `api.ErrCodeServiceUnavailable`，严禁混用 `model_output_guard_triggered`。WebSocket 未就绪门禁先写 JSON 503 错误帧再发 1013 控制帧关闭；显式 `Enabled = false` 成功加载后必须置 `isReady = true` 正常放行。
- **测试隔离与可控性**：生产代码不暴露带 `ForTest` 后缀的初始化函数；测试 helper 严格放置在 `_test.go` 中；支持注入 `OutputGuardSettingsReader` 与自定义 ticker/timeout，杜绝测试中硬 sleep 5 秒。

## File Structure

| 文件路径 | 状态 | 职责描述 |
|---|---|---|
| `proxy/output_guard_settings.go` | 新建 | 运行时状态容器、`ApplyOutputGuardSettings`、`CurrentOutputGuardConfig`、`IsOutputGuardReady`、`LoadOutputGuardSettings`、`StartOutputGuardConfigPoller`、`RequireOutputGuardReadyHTTP/WS/Stream` 门禁 |
| `proxy/output_guard_settings_test.go` | 新建 | 运行时测试：冷启动 503 阻断、WS 错误帧+1013、显式 disabled 就绪、Stale Generation 拒绝、LKG 保留、Poller 自愈与优雅退出、`PollOutputGuardConfigOnceForTest` 桥接 |
| `main.go` | 修改 (锚点: 265) | 启动时调用 `LoadOutputGuardSettings`（非致命启动）并启动后台 `StartOutputGuardConfigPoller` |

## Task 5: 运行时不可变配置快照、5 秒后台 Revision 轮询器与冷启动就绪门禁

**Files:**
- Create: `proxy/output_guard_settings.go`
- Create: `proxy/output_guard_settings_test.go`
- Modify: `main.go:265-270`

**Interfaces:**
- Consumes:
  - `database.OutputGuardSettings`: `Enabled *bool`, `MaxToolArgumentBytes int64`, `MaxToolArgumentDeltas int`, `MaxChannelBytes int64`, `MaxRequestBytes int64`, `ConfigGeneration int64`
  - `database.ValidateOutputGuardSettings(database.OutputGuardSettings) error`
  - `database.DB.GetOutputGuardSettings(context.Context) (database.OutputGuardSettings, error)`
  - `database.DB.RunBackgroundTask(func(context.Context)) bool`
  - `api.NewAPIError(code api.ErrorCode, message string, errorType api.ErrorType) *api.APIError`
  - `api.SendErrorWithStatus(c *gin.Context, err *api.APIError, status int)`
  - `proxy.writeResponsesWSErrorWithin(conn *websocket.Conn, apiErr *api.APIError, timeout time.Duration) error`
  - `proxy.writeResponsesWSErrorForStream(conn *websocket.Conn, apiErr *api.APIError, streamID string, timeout time.Duration) error`
- Produces (`proxy/output_guard_settings.go`):
  - `OutputGuardAppliedConfig`, `OutputGuardConfigSyncStatus`, `OutputGuardSettingsReader`
  - `ApplyOutputGuardSettings(database.OutputGuardSettings) bool`
  - `CurrentOutputGuardConfig() OutputGuardAppliedConfig`
  - `GetOutputGuardConfigSyncStatus() OutputGuardConfigSyncStatus`
  - `IsOutputGuardReady() bool`
  - `LoadOutputGuardSettings(ctx context.Context, reader OutputGuardSettingsReader) error`
  - `StartOutputGuardConfigPoller(parent context.Context, db *database.DB) bool`
  - `RequireOutputGuardReadyHTTP(c *gin.Context) bool`
  - `RequireOutputGuardReadyWS(conn *websocket.Conn) bool`
  - `RequireOutputGuardReadyStream(conn *websocket.Conn, streamID string) bool`
- Produces (Tests Only: `proxy/output_guard_settings_test.go`):
  - `ResetOutputGuardStateForTest()`
  - `SetOutputGuardReadyForTest(ready bool)`
  - `PollOutputGuardConfigOnceForTest(ctx context.Context, db *database.DB) error`

---

- [ ] **Step 1: Write failing test for proxy output guard runtime settings, poller, LKG, and readiness gates**

```go
// proxy/output_guard_settings_test.go
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func ResetOutputGuardStateForTest() {
	globalOutputGuardState.mu.Lock()
	defer globalOutputGuardState.mu.Unlock()
	globalOutputGuardState.config = OutputGuardAppliedConfig{
		Enabled: true, MaxToolArgumentBytes: 131072, MaxToolArgumentDeltas: 4096,
		MaxChannelBytes: 2097152, MaxRequestBytes: 4194304, Generation: 0,
	}
	globalOutputGuardState.lastSyncAt = time.Time{}
	globalOutputGuardState.lastSyncErr = ""
	globalOutputGuardState.isReady.Store(false)
}

func SetOutputGuardReadyForTest(ready bool) {
	globalOutputGuardState.isReady.Store(ready)
}

func PollOutputGuardConfigOnceForTest(ctx context.Context, db *database.DB) error {
	return pollOutputGuardConfigOnce(ctx, db, OutputGuardConfigReadTimeout)
}

type controlledOutputGuardSettingsReader struct {
	mu sync.Mutex; requests chan chan outputGuardSettingsReadResult
}

type outputGuardSettingsReadResult struct {
	settings database.OutputGuardSettings; err error
}

func newControlledOutputGuardSettingsReader() *controlledOutputGuardSettingsReader {
	return &controlledOutputGuardSettingsReader{requests: make(chan chan outputGuardSettingsReadResult, 16)}
}

func (r *controlledOutputGuardSettingsReader) GetOutputGuardSettings(ctx context.Context) (database.OutputGuardSettings, error) {
	respCh := make(chan outputGuardSettingsReadResult, 1)
	select {
	case <-ctx.Done(): return database.OutputGuardSettings{}, ctx.Err()
	case r.requests <- respCh:
	}
	select {
	case <-ctx.Done(): return database.OutputGuardSettings{}, ctx.Err()
	case res := <-respCh: return res.settings, res.err
	}
}

func (r *controlledOutputGuardSettingsReader) respond(t *testing.T, res outputGuardSettingsReadResult) {
	t.Helper()
	select {
	case respCh := <-r.requests: respCh <- res
	case <-time.After(2 * time.Second): t.Fatal("timed out waiting for GetOutputGuardSettings request")
	}
}

func waitForOutputGuardConfigGeneration(t *testing.T, targetGen int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if CurrentOutputGuardConfig().Generation == targetGen { return }
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for generation %d", targetGen)
}

func waitForOutputGuardSyncError(t *testing.T, substr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status := GetOutputGuardConfigSyncStatus()
		if status.LastSyncError != "" && (substr == "" || strings.Contains(status.LastSyncError, substr)) { return }
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for sync error: %s", substr)
}

func TestOutputGuardColdStartNotReadyReturns503(t *testing.T) {
	ResetOutputGuardStateForTest()
	t.Cleanup(ResetOutputGuardStateForTest)
	if IsOutputGuardReady() { t.Fatal("expected cold start IsOutputGuardReady = false") }

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	if RequireOutputGuardReadyHTTP(c) { t.Fatal("expected RequireOutputGuardReadyHTTP to reject cold start") }
	if w.Code != http.StatusServiceUnavailable { t.Fatalf("expected HTTP 503, got %d", w.Code) }

	var errResp struct{ Error struct{ Code string `json:"code"` } `json:"error"` }
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil { t.Fatalf("unmarshal error failed: %v", err) }
	if errResp.Error.Code != string(api.ErrCodeServiceUnavailable) {
		t.Fatalf("expected error code %q, got %q", api.ErrCodeServiceUnavailable, errResp.Error.Code)
	}
}

func TestOutputGuardExplicitDisabledSemanticVsColdStart(t *testing.T) {
	ResetOutputGuardStateForTest()
	t.Cleanup(ResetOutputGuardStateForTest)
	if IsOutputGuardReady() { t.Fatal("expected cold start not ready") }

	disabled := false
	dbSettings := database.OutputGuardSettings{
		Enabled: &disabled, MaxToolArgumentBytes: 131072, MaxToolArgumentDeltas: 4096,
		MaxChannelBytes: 2097152, MaxRequestBytes: 4194304, ConfigGeneration: 1,
	}

	if !ApplyOutputGuardSettings(dbSettings) { t.Fatal("ApplyOutputGuardSettings failed") }
	if !IsOutputGuardReady() { t.Fatal("expected IsOutputGuardReady = true after explicit disabled settings loaded") }

	cfg := CurrentOutputGuardConfig()
	if cfg.Enabled != false { t.Fatalf("expected applied config Enabled = false, got %v", cfg.Enabled) }

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	if !RequireOutputGuardReadyHTTP(c) { t.Fatal("expected pass when system is ready and explicitly disabled") }
	if w.Code == http.StatusServiceUnavailable { t.Fatalf("did not expect 503 when explicitly disabled and ready") }
}

func TestOutputGuardConfigGenerationIncrementsAndLKGMaintained(t *testing.T) {
	ResetOutputGuardStateForTest()
	t.Cleanup(ResetOutputGuardStateForTest)

	enabled := true
	initial := database.OutputGuardSettings{
		Enabled: &enabled, MaxToolArgumentBytes: 131072, MaxToolArgumentDeltas: 4096,
		MaxChannelBytes: 2097152, MaxRequestBytes: 4194304, ConfigGeneration: 1,
	}
	if !ApplyOutputGuardSettings(initial) { t.Fatal("failed to apply initial generation 1") }

	cfg1 := CurrentOutputGuardConfig()
	if cfg1.Generation != 1 || cfg1.MaxToolArgumentBytes != 131072 { t.Fatalf("initial mismatch: %+v", cfg1) }

	stale := initial; stale.MaxToolArgumentBytes = 262144
	if ApplyOutputGuardSettings(stale) { t.Fatal("stale generation 1 should be rejected") }
	if CurrentOutputGuardConfig().MaxToolArgumentBytes != 131072 { t.Fatal("LKG overwritten by stale generation") }

	gen2 := initial; gen2.MaxToolArgumentBytes = 262144; gen2.ConfigGeneration = 2
	if !ApplyOutputGuardSettings(gen2) { t.Fatal("failed to apply generation 2") }
	cfg2 := CurrentOutputGuardConfig()
	if cfg2.Generation != 2 || cfg2.MaxToolArgumentBytes != 262144 { t.Fatalf("gen 2 mismatch: %+v", cfg2) }

	illegal := initial; illegal.MaxToolArgumentBytes = 50000000; illegal.ConfigGeneration = 3
	if ApplyOutputGuardSettings(illegal) { t.Fatal("illegal configuration should be rejected") }
	cfgPreserved := CurrentOutputGuardConfig()
	if cfgPreserved.Generation != 2 || cfgPreserved.MaxToolArgumentBytes != 262144 {
		t.Fatalf("LKG not preserved after illegal config: %+v", cfgPreserved)
	}
}

func TestOutputGuardPollerLifecycleRecoveryAndStop(t *testing.T) {
	ResetOutputGuardStateForTest()
	t.Cleanup(ResetOutputGuardStateForTest)

	reader := newControlledOutputGuardSettingsReader()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		runOutputGuardConfigPoller(ctx, reader, 5*time.Millisecond, 100*time.Millisecond)
	}()

	enabled := true
	reader.respond(t, outputGuardSettingsReadResult{
		settings: database.OutputGuardSettings{
			Enabled: &enabled, MaxToolArgumentBytes: 131072, MaxToolArgumentDeltas: 4096,
			MaxChannelBytes: 2097152, MaxRequestBytes: 4194304, ConfigGeneration: 2,
		},
	})
	waitForOutputGuardConfigGeneration(t, 2)
	if !IsOutputGuardReady() { t.Fatal("expected IsOutputGuardReady = true after successful poll") }

	reader.respond(t, outputGuardSettingsReadResult{err: errors.New("simulated database disconnect")})
	waitForOutputGuardSyncError(t, "simulated database disconnect")
	if CurrentOutputGuardConfig().Generation != 2 { t.Fatal("LKG generation lost during DB outage") }

	reader.respond(t, outputGuardSettingsReadResult{
		settings: database.OutputGuardSettings{
			Enabled: &enabled, MaxToolArgumentBytes: 262144, MaxToolArgumentDeltas: 4096,
			MaxChannelBytes: 2097152, MaxRequestBytes: 4194304, ConfigGeneration: 3,
		},
	})
	waitForOutputGuardConfigGeneration(t, 3)
	if CurrentOutputGuardConfig().MaxToolArgumentBytes != 262144 { t.Fatal("failed to update config after recovery") }

	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond): t.Fatal("poller failed to stop cleanly after context cancel")
	}
}

func TestOutputGuardWSReadinessGate(t *testing.T) {
	ResetOutputGuardStateForTest()
	t.Cleanup(ResetOutputGuardStateForTest)

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil { return }
		defer conn.Close()
		if !RequireOutputGuardReadyWS(conn) { return }
		_ = conn.WriteMessage(websocket.TextMessage, []byte("ready"))
	}))
	defer server.Close()

	wsURL := "ws" + server.URL[len("http"):]
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil { t.Fatalf("dial WS failed: %v", err) }
	defer conn.Close()

	msgType, msg, err := conn.ReadMessage()
	if err == nil {
		if msgType != websocket.TextMessage { t.Fatalf("expected text message error frame, got %d", msgType) }
		var errResp struct{ Error struct{ Code string `json:"code"` } `json:"error"` }
		if err := json.Unmarshal(msg, &errResp); err != nil { t.Fatalf("unmarshal error frame failed: %v", err) }
		if errResp.Error.Code != string(api.ErrCodeServiceUnavailable) {
			t.Fatalf("expected error frame %s, got %s", api.ErrCodeServiceUnavailable, errResp.Error.Code)
		}
		_, _, err = conn.ReadMessage()
	}

	if err == nil { t.Fatal("expected connection closed with 1013 CloseTryAgainLater, got nil error") }
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		if closeErr.Code != websocket.CloseTryAgainLater { t.Fatalf("expected 1013 CloseTryAgainLater, got %d", closeErr.Code) }
	} else {
		t.Fatalf("expected websocket.CloseError, got %v", err)
	}
}

func TestOutputGuardStreamReadinessGate(t *testing.T) {
	ResetOutputGuardStateForTest()
	t.Cleanup(ResetOutputGuardStateForTest)

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil { return }
		defer conn.Close()
		_ = RequireOutputGuardReadyStream(conn, "stream-test-123")
	}))
	defer server.Close()

	wsURL := "ws" + server.URL[len("http"):]
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil { t.Fatalf("dial WS failed: %v", err) }
	defer conn.Close()

	_, msg, err := conn.ReadMessage()
	if err != nil { t.Fatalf("failed to read stream error frame: %v", err) }

	var streamErrResp struct {
		Type string `json:"type"`; StreamID string `json:"stream_id"`
		Status int `json:"status"`; Error *api.APIError `json:"error"`
	}
	if err := json.Unmarshal(msg, &streamErrResp); err != nil { t.Fatalf("unmarshal stream error failed: %v", err) }
	if streamErrResp.Type != "error" || streamErrResp.StreamID != "stream-test-123" {
		t.Fatalf("unexpected stream envelope: %+v", streamErrResp)
	}
	if streamErrResp.Error == nil || streamErrResp.Error.Code != api.ErrCodeServiceUnavailable {
		t.Fatalf("expected service_unavailable, got %+v", streamErrResp.Error)
	}
}
```

- [ ] **Step 2: Run test to verify it fails (Red)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./proxy -run "TestOutputGuard.*"`
Expected: FAIL with compilation error: undefined: ApplyOutputGuardSettings, RequireOutputGuardReadyHTTP, etc.

- [ ] **Step 3: Write minimal implementation for proxy/output_guard_settings.go and update main.go**

```go
// proxy/output_guard_settings.go
package proxy

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

const (
	OutputGuardConfigPollInterval = 5 * time.Second
	OutputGuardConfigReadTimeout  = 3 * time.Second
)

type OutputGuardAppliedConfig struct {
	Enabled               bool
	MaxToolArgumentBytes  int64
	MaxToolArgumentDeltas int
	MaxChannelBytes       int64
	MaxRequestBytes       int64
	Generation            int64
}

type OutputGuardConfigSyncStatus struct {
	LastSuccessfulSyncAt time.Time
	LastSyncError        string
}

type OutputGuardSettingsReader interface {
	GetOutputGuardSettings(context.Context) (database.OutputGuardSettings, error)
}

type outputGuardState struct {
	mu          sync.RWMutex
	config      OutputGuardAppliedConfig
	lastSyncAt  time.Time
	lastSyncErr string
	isReady     atomic.Bool
}

var globalOutputGuardState = &outputGuardState{
	config: OutputGuardAppliedConfig{
		Enabled:               true,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
		Generation:            0,
	},
}

func IsOutputGuardReady() bool {
	return globalOutputGuardState.isReady.Load()
}

func CurrentOutputGuardConfig() OutputGuardAppliedConfig {
	globalOutputGuardState.mu.RLock()
	defer globalOutputGuardState.mu.RUnlock()
	return globalOutputGuardState.config
}

func GetOutputGuardConfigSyncStatus() OutputGuardConfigSyncStatus {
	globalOutputGuardState.mu.RLock()
	defer globalOutputGuardState.mu.RUnlock()
	return OutputGuardConfigSyncStatus{
		LastSuccessfulSyncAt: globalOutputGuardState.lastSyncAt,
		LastSyncError:        globalOutputGuardState.lastSyncErr,
	}
}

func ApplyOutputGuardSettings(settings database.OutputGuardSettings) bool {
	if err := database.ValidateOutputGuardSettings(settings); err != nil {
		log.Printf("[output-guard] 非法配置已被拒绝，保留 LKG: %v", err)
		recordOutputGuardConfigSyncError(err)
		return false
	}

	globalOutputGuardState.mu.Lock()
	defer globalOutputGuardState.mu.Unlock()

	if settings.ConfigGeneration <= globalOutputGuardState.config.Generation {
		return false
	}

	enabledVal := true
	if settings.Enabled != nil {
		enabledVal = *settings.Enabled
	}

	globalOutputGuardState.config = OutputGuardAppliedConfig{
		Enabled:               enabledVal,
		MaxToolArgumentBytes:  settings.MaxToolArgumentBytes,
		MaxToolArgumentDeltas: settings.MaxToolArgumentDeltas,
		MaxChannelBytes:       settings.MaxChannelBytes,
		MaxRequestBytes:       settings.MaxRequestBytes,
		Generation:            settings.ConfigGeneration,
	}
	globalOutputGuardState.isReady.Store(true)
	return true
}

func LoadOutputGuardSettings(ctx context.Context, reader OutputGuardSettingsReader) error {
	settings, err := readOutputGuardSettings(ctx, reader, OutputGuardConfigReadTimeout)
	if err != nil {
		if ctx == nil || ctx.Err() == nil {
			recordOutputGuardConfigSyncError(err)
		}
		return err
	}
	if err := database.ValidateOutputGuardSettings(settings); err != nil {
		recordOutputGuardConfigSyncError(err)
		return err
	}
	ApplyOutputGuardSettings(settings)
	recordOutputGuardConfigSyncSuccess(time.Now())
	return nil
}

func StartOutputGuardConfigPoller(parent context.Context, db *database.DB) bool {
	if db == nil {
		return false
	}
	if parent == nil {
		parent = context.Background()
	}
	return db.RunBackgroundTask(func(lifecycle context.Context) {
		taskCtx, cancel := context.WithCancel(lifecycle)
		stopParent := context.AfterFunc(parent, cancel)
		defer func() {
			stopParent()
			cancel()
		}()
		runOutputGuardConfigPoller(
			taskCtx,
			db,
			OutputGuardConfigPollInterval,
			OutputGuardConfigReadTimeout,
		)
	})
}

func runOutputGuardConfigPoller(
	ctx context.Context,
	reader OutputGuardSettingsReader,
	interval time.Duration,
	readTimeout time.Duration,
) {
	if ctx == nil {
		ctx = context.Background()
	}
	if interval <= 0 {
		interval = OutputGuardConfigPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = pollOutputGuardConfigOnce(ctx, reader, readTimeout)
		}
	}
}

func pollOutputGuardConfigOnce(
	ctx context.Context,
	reader OutputGuardSettingsReader,
	readTimeout time.Duration,
) error {
	settings, err := readOutputGuardSettings(ctx, reader, readTimeout)
	if err != nil {
		if ctx != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		recordOutputGuardConfigSyncError(err)
		return err
	}
	if err := database.ValidateOutputGuardSettings(settings); err != nil {
		recordOutputGuardConfigSyncError(err)
		return err
	}
	ApplyOutputGuardSettings(settings)
	recordOutputGuardConfigSyncSuccess(time.Now())
	return nil
}

func readOutputGuardSettings(
	parent context.Context,
	reader OutputGuardSettingsReader,
	timeout time.Duration,
) (database.OutputGuardSettings, error) {
	if reader == nil {
		return database.OutputGuardSettings{}, fmt.Errorf("output guard settings reader is nil")
	}
	if parent == nil {
		parent = context.Background()
	}
	if err := parent.Err(); err != nil {
		return database.OutputGuardSettings{}, err
	}
	if timeout <= 0 || timeout > OutputGuardConfigReadTimeout {
		timeout = OutputGuardConfigReadTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	return reader.GetOutputGuardSettings(ctx)
}

func recordOutputGuardConfigSyncSuccess(at time.Time) {
	globalOutputGuardState.mu.Lock()
	globalOutputGuardState.lastSyncAt = at
	globalOutputGuardState.lastSyncErr = ""
	globalOutputGuardState.mu.Unlock()
}

func recordOutputGuardConfigSyncError(err error) {
	if err == nil {
		return
	}
	globalOutputGuardState.mu.Lock()
	globalOutputGuardState.lastSyncErr = err.Error()
	globalOutputGuardState.mu.Unlock()
}

func RequireOutputGuardReadyHTTP(c *gin.Context) bool {
	if IsOutputGuardReady() {
		return true
	}
	apiErr := api.NewAPIError(
		api.ErrCodeServiceUnavailable,
		"Service configuration unavailable; failed to load output guard settings",
		api.ErrorTypeServer,
	)
	api.SendErrorWithStatus(c, apiErr, http.StatusServiceUnavailable)
	return false
}

func RequireOutputGuardReadyWS(conn *websocket.Conn) bool {
	if IsOutputGuardReady() {
		return true
	}
	if conn == nil {
		return false
	}
	apiErr := api.NewAPIError(
		api.ErrCodeServiceUnavailable,
		"Service configuration unavailable; failed to load output guard settings",
		api.ErrorTypeServer,
	)
	_ = writeResponsesWSErrorWithin(conn, apiErr, 2*time.Second)
	_ = conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "Service configuration unavailable"),
		time.Now().Add(time.Second),
	)
	_ = conn.Close()
	return false
}

func RequireOutputGuardReadyStream(conn *websocket.Conn, streamID string) bool {
	if IsOutputGuardReady() {
		return true
	}
	if conn == nil {
		return false
	}
	apiErr := api.NewAPIError(
		api.ErrCodeServiceUnavailable,
		"Service configuration unavailable; failed to load output guard settings",
		api.ErrorTypeServer,
	)
	_ = writeResponsesWSErrorForStream(conn, apiErr, streamID, 2*time.Second)
	return false
}
```

精确插入锚点：`main.go:265-270`（紧随 ResponseCache 设置加载与日志之后，挂载 OutputGuard 加载与 Poller）：

```go
	// main.go 锚点替换：紧接 line 265
	appliedResponseCache := proxy.GetResponseCacheAppliedConfig()
	log.Printf(
		"响应缓存设置已加载: generation=%d total=%d entry=%d reconstruct=%d",
		appliedResponseCache.Generation, appliedResponseCache.LocalMaxBytes,
		appliedResponseCache.LocalMaxEntryBytes, appliedResponseCache.ReconstructMaxBytes,
	)

	// --- 插入开始: OutputGuard 加载与后台 5 秒轮询器启动 ---
	outputGuardCtx, outputGuardCancel := context.WithTimeout(context.Background(), 3*time.Second)
	if err := proxy.LoadOutputGuardSettings(outputGuardCtx, db); err != nil {
		outputGuardCancel()
		log.Printf("警告: 加载模型输出失控防护设置失败（冷启动门禁将返回 503 直至 DB 轮询恢复）: %v", err)
	} else {
		outputGuardCancel()
		appliedGuard := proxy.CurrentOutputGuardConfig()
		log.Printf(
			"模型输出失控防护设置已加载: generation=%d enabled=%t tool_bytes=%d tool_deltas=%d channel_bytes=%d request_bytes=%d",
			appliedGuard.Generation, appliedGuard.Enabled, appliedGuard.MaxToolArgumentBytes,
			appliedGuard.MaxToolArgumentDeltas, appliedGuard.MaxChannelBytes, appliedGuard.MaxRequestBytes,
		)
	}
	proxy.StartOutputGuardConfigPoller(context.Background(), db)
	// --- 插入结束 ---
```

- [ ] **Step 4: Run test to verify it passes (Green)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v -race ./proxy -run "TestOutputGuard.*"`
Expected: PASS ok github.com/codex2api/proxy

- [ ] **Step 5: Commit**

```bash
git add proxy/output_guard_settings.go proxy/output_guard_settings_test.go main.go
git commit -m "feat(proxy): add output guard runtime config snapshot, poller and cold start readiness gate"
```
