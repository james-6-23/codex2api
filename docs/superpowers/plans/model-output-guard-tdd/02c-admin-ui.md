# 02c-Admin-UI: 模型输出失控防护管理端 API 与前端设置界面 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 基于已审查通过的设计规格（Spec §7、§9.21、§10），在 Codex2API 管理端扩展既有 `/api/admin/settings` GET/PUT 接口与前端 Settings 界面（Task 6），提供 5 项核心配置与 1 项只读版本（generation），支持三态显式 `false` 持久化与高风险关闭警告、严格层级范围校验（$16\text{KiB} \le \text{tool} \le \text{channel} \le \text{request} \le 64\text{MiB}$）、同事务短合并更新与多节点/内存快照即时热应用、前端 General Tab 独立设置卡片、KiB/MiB 友好的非破坏性输入、“恢复默认”原子重置与只读 generation 安全隔离。

**Architecture:**
- `admin/output_guard_settings.go`: 提供输入范围校验 `validateOutputGuardSettingsUpdateRanges`，复用 `database.ErrInvalidOutputGuardSettings` 及各常量范围。
- `admin/handler.go`:
  - `settingsResponse`: 扩展 5 个完整对称配置字段 + 1 个只读 `output_guard_config_generation`。
  - `updateSettingsReq`: 扩展 5 个可空指针配置字段 + `OutputGuardConfigGeneration rawJSON`。若请求体携带只读 generation 则严格返回 HTTP 400 拦截。
  - `GetSettings`: 读取 `db.GetOutputGuardSettings(ctx)`，回填到响应结构。
  - `UpdateSettings`: 经范围校验后，先查现有配置执行 `database.ValidateOutputGuardSettings` 校验层级合法性；再调用 `db.UpdateOutputGuardSettings(ctx, patch)` 执行短事务合并，成功后立即调用 `proxy.ApplyOutputGuardSettings(committed)` 热应用生效并返回最新快照；非法请求绝不修改 DB 或运行时。沿用既有设计，与其它设置同批请求时遵循独立提交语义，不承诺跨表全局分布式原子性。
- `frontend/src/types.ts`: `SystemSettings` 扩展 6 个属性（含 `readonly output_guard_config_generation: number`）。
- `frontend/src/lib/settingsPayload.ts`: `buildWritableSettingsPayload` 中剔除 `output_guard_config_generation`，避免任何常规保存回写只读字段。
- `frontend/src/lib/outputGuardSettings.ts`: 提供 IEC KiB/MiB 换算、`tool <= channel <= request` 层级校验器与默认值。
- `frontend/src/pages/Settings.tsx`:
  - General Tab 增加独立的“模型输出失控防护”`SettingsCard`，绝不挂载在 PromptFilter 之下。
  - 显式 `false` 开关与高风险关闭横幅提醒（使用已导入的 `ShieldAlert` 图标，禁止未导入图标）。
  - 工具参数支持 KiB 输入（16 KiB ~ 4096 KiB），单通道支持 KiB 输入（128 KiB ~ 16384 KiB，确保 128 KiB 完全可表达），单请求支持 KiB 输入（256 KiB ~ 65536 KiB，确保 256 KiB 合法下限完全可表达且默认 4 MiB 显示为 4096 KiB）。
  - “恢复默认”提供一次性 5 字段完整 patch 提交，并在失败时自动 rollback。

**Tech Stack:** Go 1.26+, Gin 1.12, TypeScript 5.9+, React 19, Lucide React, Node test runner (`node --experimental-strip-types --test`).

**Spec:** `docs/superpowers/specs/2026-10-28-model-output-guard-design.md` (§7, §9.21, §10)

## Global Constraints

- **单一窄任务**：仅负责 Task 6 管理端 Admin API 与前端 UI 模块；不修改 Task 4 数据库核心、Task 5 运行时/轮询器、协议转发或任何既有业务/spec。
- **接口契约**：
  - 必须复用现有的 `GET /api/admin/settings` 与 `PUT /api/admin/settings`，绝不新增独立 API endpoint。
  - 消费 Task 4 契约：`database.OutputGuardSettings`、`database.OutputGuardSettingsUpdate`、`database.ValidateOutputGuardSettings`、`db.GetOutputGuardSettings`、`db.UpdateOutputGuardSettings`、以及常量 `database.MinOutputGuardMax*` / `database.MaxOutputGuardMax*`。
  - 消费 Task 5 契约：`proxy.ApplyOutputGuardSettings(database.OutputGuardSettings) bool` 与 `proxy.CurrentOutputGuardConfig() proxy.OutputGuardAppliedConfig`。
- **三态与零值契约**：`output_guard_enabled` 为指针 `*bool`，显式 `false` 必须原样提交与持久化，禁止被 `|| true` 或零值回退覆盖。
- **只读字段安全**：`output_guard_config_generation` 纯只读。前端提交 payload 必须自动剔除；后端接收到非空值必须直接返回 HTTP 400 拒绝，禁止静默忽略或覆盖。
- **层级约束**：严格满足 $16\text{KiB} \le \text{tool} \le \text{channel} \le \text{request} \le 64\text{MiB}$。通道下限为 128 KiB，前端不得设定最小 1 MiB 导致 128 KiB 合法值无法配置。
- **测试环境约束**：Go API 测试使用真实 SQLite fixture（调用测试文件内真实的 `newTestAdminDB(t)`，禁止跨包引用私有 helper 或伪造断言）；TS 测试使用 `node --experimental-strip-types --test` 运行真实断言。

---

## File Structure

| 文件路径 | 状态 | 职责描述 |
|---|---|---|
| `admin/output_guard_settings.go` | 新建 | 管理端输入范围校验 `validateOutputGuardSettingsUpdateRanges` |
| `admin/output_guard_settings_test.go` | 新建 | API 单元/集成测试：对称字段、只读 generation 拒绝、范围/层级非法拒绝、显式 false、局部更新隔离 |
| `admin/handler.go` | 修改 (锚点: 9460, 9635, 10240, 10600, 10750, 12220) | 扩展 `settingsResponse`、`updateSettingsReq`、`GetSettings` 与 `UpdateSettings` |
| `frontend/src/types.ts` | 修改 (锚点: 2305) | `SystemSettings` 扩展 5 个配置属性 + 1 个只读 `output_guard_config_generation` |
| `frontend/src/lib/settingsPayload.ts` | 修改 (锚点: 1, 9) | `buildWritableSettingsPayload` 剔除 `output_guard_config_generation` |
| `frontend/src/lib/settingsPayload.test.mjs` | 修改 (锚点: 6) | 验证 writable payload 严格过滤 `output_guard_config_generation` |
| `frontend/src/lib/outputGuardSettings.ts` | 新建 | 前端单位换算（KiB/MiB）、`tool <= channel <= request` 层级校验器与默认值定义 |
| `frontend/src/lib/outputGuardSettings.test.mjs` | 新建 | 前端纯函数测试：层级校验、范围边界、单位转换与默认值比对 |
| `frontend/src/pages/Settings.tsx` | 修改 (锚点: 195, 2760, 2960, 6550) | `useState<SystemSettings>` 初始 literal 6 字段补全、General Tab 独立卡片 UI、DraftNumberInput 输入、显式 false、高风险警告与恢复默认 |

---

## Task 6: 管理端 Admin API 扩展与前端设置界面独立卡片

**Files:**
- Create: `admin/output_guard_settings.go`
- Create: `admin/output_guard_settings_test.go`
- Modify: `admin/handler.go:9460-9640,10240-10320,10600-10760,12210-12260`
- Modify: `frontend/src/types.ts:2300-2315`
- Modify: `frontend/src/lib/settingsPayload.ts:1-16`
- Modify: `frontend/src/lib/settingsPayload.test.mjs:6-42`
- Create: `frontend/src/lib/outputGuardSettings.ts`
- Create: `frontend/src/lib/outputGuardSettings.test.mjs`
- Modify: `frontend/src/pages/Settings.tsx:195-210,2760-2840,2960-3020,6550-6640`

**Interfaces:**
- Consumes:
  - `database.OutputGuardSettings`: `Enabled *bool`, `MaxToolArgumentBytes int64`, `MaxToolArgumentDeltas int`, `MaxChannelBytes int64`, `MaxRequestBytes int64`, `ConfigGeneration int64`
  - `database.OutputGuardSettingsUpdate`: `Enabled *bool`, `MaxToolArgumentBytes *int64`, `MaxToolArgumentDeltas *int`, `MaxChannelBytes *int64`, `MaxRequestBytes *int64`
  - `database.ValidateOutputGuardSettings(database.OutputGuardSettings) error`
  - `database.MinOutputGuardMaxToolArgumentBytes` (16384), `database.MaxOutputGuardMaxToolArgumentBytes` (4194304)
  - `database.MinOutputGuardMaxToolArgumentDeltas` (256), `database.MaxOutputGuardMaxToolArgumentDeltas` (65536)
  - `database.MinOutputGuardMaxChannelBytes` (131072), `database.MaxOutputGuardMaxChannelBytes` (16777216)
  - `database.MinOutputGuardMaxRequestBytes` (262144), `database.MaxOutputGuardMaxRequestBytes` (67108864)
  - `db.GetOutputGuardSettings(context.Context) (database.OutputGuardSettings, error)`
  - `db.UpdateOutputGuardSettings(context.Context, database.OutputGuardSettingsUpdate) (database.OutputGuardSettings, error)`
  - `proxy.ApplyOutputGuardSettings(database.OutputGuardSettings) bool`
  - `proxy.CurrentOutputGuardConfig() proxy.OutputGuardAppliedConfig`
- Produces:
  - Admin REST API (`GET /api/admin/settings` & `PUT /api/admin/settings`):
    - `output_guard_enabled`: bool
    - `output_guard_max_tool_argument_bytes`: int64
    - `output_guard_max_tool_argument_deltas`: int
    - `output_guard_max_channel_bytes`: int64
    - `output_guard_max_request_bytes`: int64
    - `output_guard_config_generation`: int64 (只读)
  - Frontend TypeScript Interfaces & Helpers:
    - `SystemSettings` 含 6 个 output_guard 字段
    - `buildWritableSettingsPayload<T>(settings: T)`
    - `validateOutputGuardHierarchy(settings): string | null`
    - `validateOutputGuardRanges(settings): string | null`
    - `bytesToKiB`, `kibToBytes`, `bytesToMiB`, `mibToBytes`
    - `DEFAULT_OUTPUT_GUARD_SETTINGS`

---

### Step 1: Write Failing Tests for Admin Output Guard Settings API

创建 `admin/output_guard_settings_test.go`：

```go
package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func newTestAdminHandlerFixture(t *testing.T) (*Handler, *database.DB) {
	t.Helper()
	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	settings := defaultBootstrapSettings()
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	store := auth.NewStore(db, tc, settings)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, db, tc, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")
	return handler, db
}

func invokeAdminSettingsEndpoint(t *testing.T, handler *Handler, method string, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	var bodyReader *strings.Reader
	if payload != nil {
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal payload failed: %v", err)
		}
		bodyReader = strings.NewReader(string(body))
	} else {
		bodyReader = strings.NewReader("")
	}
	c.Request = httptest.NewRequest(method, "/api/admin/settings", bodyReader)
	c.Request.Header.Set("Content-Type", "application/json")
	if method == http.MethodGet {
		handler.GetSettings(c)
	} else {
		handler.UpdateSettings(c)
	}
	return w
}

func TestAdminOutputGuardSettings_GetSymmetricFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, _ := newTestAdminHandlerFixture(t)

	w := invokeAdminSettingsEndpoint(t, handler, http.MethodGet, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d, body: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	fields := []string{
		"output_guard_enabled", "output_guard_max_tool_argument_bytes",
		"output_guard_max_tool_argument_deltas", "output_guard_max_channel_bytes",
		"output_guard_max_request_bytes", "output_guard_config_generation",
	}
	for _, f := range fields {
		if _, ok := resp[f]; !ok {
			t.Fatalf("expected field %q in GET /api/admin/settings response", f)
		}
	}
	if resp["output_guard_enabled"] != false {
		t.Fatalf("expected default enabled = false, got %v", resp["output_guard_enabled"])
	}
	if int64(resp["output_guard_max_tool_argument_bytes"].(float64)) != 262144 {
		t.Fatalf("expected default tool bytes 262144, got %v", resp["output_guard_max_tool_argument_bytes"])
	}
}

func TestAdminOutputGuardSettings_PutValidationAndReadonlyRejection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, db := newTestAdminHandlerFixture(t)

	// 1. 提交只读 generation 必须报错 400
	{
		w := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{"output_guard_config_generation": 999})
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "只读") {
			t.Fatalf("expected 400 with readonly rejection, got code=%d body=%s", w.Code, w.Body.String())
		}
	}
	// 2. 超出单个字段合法范围必须报错 400
	{
		w := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{"output_guard_max_tool_argument_bytes": 1024})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 on out of range tool bytes, got %d", w.Code)
		}
	}
	// 3. 违反 tool <= channel <= request 层级校验必须报错 400 且不改写 DB
	{
		w := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{
			"output_guard_max_tool_argument_bytes": 3145728,
			"output_guard_max_channel_bytes":       2097152,
		})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 on hierarchy violation, got %d", w.Code)
		}
		current, err := db.GetOutputGuardSettings(context.Background())
		if err != nil || current.MaxToolArgumentBytes == 3145728 {
			t.Fatalf("database should not be updated on illegal hierarchy validation, err=%v", err)
		}
	}
	// 4. 显式 true 准确持久化与热应用
	{
		w := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{"output_guard_enabled": true})
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on explicit true update, got %d, body: %s", w.Code, w.Body.String())
		}
		var resp map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["output_guard_enabled"] != true {
			t.Fatalf("expected output_guard_enabled = true in response, got %v", resp["output_guard_enabled"])
		}
		current, err := db.GetOutputGuardSettings(context.Background())
		if err != nil || current.Enabled == nil || *current.Enabled != true {
			t.Fatalf("database must store explicit true")
		}
	}
	// 5. 局部字段更新隔离：仅更新 deltas 不改变其他字段，并立即热应用
	{
		w := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{"output_guard_max_tool_argument_deltas": 8192})
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on partial update, got %d", w.Code)
		}
		var resp map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if int(resp["output_guard_max_tool_argument_deltas"].(float64)) != 8192 || resp["output_guard_enabled"] != true {
			t.Fatalf("expected deltas = 8192 and preserved enabled=true, got %v", resp)
		}
		if runtimeCfg := proxy.CurrentOutputGuardConfig(); runtimeCfg.MaxToolArgumentDeltas != 8192 {
			t.Fatalf("runtime snapshot must be immediately updated, got %d", runtimeCfg.MaxToolArgumentDeltas)
		}
	}
}
```

---

### Step 2: Verify Failing Tests Fail (RED)

Run:
```bash
GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./admin -run "TestAdminOutputGuardSettings_.*"
```
Expected: FAIL with `unknown field output_guard_enabled in settingsResponse` 或编译错误（因为结构体与接口尚未扩展）。

---

### Step 3: Implement Backend Admin API Extensions

1. 创建 `admin/output_guard_settings.go`：
```go
package admin

import (
	"fmt"

	"github.com/codex2api/database"
)

func validateOutputGuardSettingsUpdateRanges(update database.OutputGuardSettingsUpdate) error {
	switch {
	case update.MaxToolArgumentBytes != nil &&
		(*update.MaxToolArgumentBytes < database.MinOutputGuardMaxToolArgumentBytes ||
			*update.MaxToolArgumentBytes > database.MaxOutputGuardMaxToolArgumentBytes):
		return fmt.Errorf("%w: output_guard_max_tool_argument_bytes must be between %d and %d",
			database.ErrInvalidOutputGuardSettings, database.MinOutputGuardMaxToolArgumentBytes, database.MaxOutputGuardMaxToolArgumentBytes)
	case update.MaxToolArgumentDeltas != nil &&
		(*update.MaxToolArgumentDeltas < database.MinOutputGuardMaxToolArgumentDeltas ||
			*update.MaxToolArgumentDeltas > database.MaxOutputGuardMaxToolArgumentDeltas):
		return fmt.Errorf("%w: output_guard_max_tool_argument_deltas must be between %d and %d",
			database.ErrInvalidOutputGuardSettings, database.MinOutputGuardMaxToolArgumentDeltas, database.MaxOutputGuardMaxToolArgumentDeltas)
	case update.MaxChannelBytes != nil &&
		(*update.MaxChannelBytes < database.MinOutputGuardMaxChannelBytes ||
			*update.MaxChannelBytes > database.MaxOutputGuardMaxChannelBytes):
		return fmt.Errorf("%w: output_guard_max_channel_bytes must be between %d and %d",
			database.ErrInvalidOutputGuardSettings, database.MinOutputGuardMaxChannelBytes, database.MaxOutputGuardMaxChannelBytes)
	case update.MaxRequestBytes != nil &&
		(*update.MaxRequestBytes < database.MinOutputGuardMaxRequestBytes ||
			*update.MaxRequestBytes > database.MaxOutputGuardMaxRequestBytes):
		return fmt.Errorf("%w: output_guard_max_request_bytes must be between %d and %d",
			database.ErrInvalidOutputGuardSettings, database.MinOutputGuardMaxRequestBytes, database.MaxOutputGuardMaxRequestBytes)
	default:
		return nil
	}
}
```

2. 修改 `admin/handler.go`：

**插入锚点 1: `admin/handler.go:9463`（在 `type settingsResponse struct` 尾部添加）：**
```go
	OutputGuardEnabled                 bool                             `json:"output_guard_enabled"`
	OutputGuardMaxToolArgumentBytes    int64                            `json:"output_guard_max_tool_argument_bytes"`
	OutputGuardMaxToolArgumentDeltas   int                              `json:"output_guard_max_tool_argument_deltas"`
	OutputGuardMaxChannelBytes         int64                            `json:"output_guard_max_channel_bytes"`
	OutputGuardMaxRequestBytes         int64                            `json:"output_guard_max_request_bytes"`
	OutputGuardConfigGeneration        int64                            `json:"output_guard_config_generation"`
```

**插入锚点 2: `admin/handler.go:9637`（在 `type updateSettingsReq struct` 尾部添加）：**
```go
	OutputGuardEnabled                 *bool                            `json:"output_guard_enabled"`
	OutputGuardMaxToolArgumentBytes    *int64                           `json:"output_guard_max_tool_argument_bytes"`
	OutputGuardMaxToolArgumentDeltas   *int                             `json:"output_guard_max_tool_argument_deltas"`
	OutputGuardMaxChannelBytes         *int64                           `json:"output_guard_max_channel_bytes"`
	OutputGuardMaxRequestBytes         *int64                           `json:"output_guard_max_request_bytes"`
	OutputGuardConfigGeneration        rawJSON                          `json:"output_guard_config_generation"`
```

**插入锚点 3: `admin/handler.go:10240`（在 `GetSettings` 中读取 `outputGuardSettings`）：**
```go
	outputGuardSettings, err := h.db.GetOutputGuardSettings(ctx)
	if err != nil {
		outputGuardSettings = database.DefaultOutputGuardSettings()
	}
	outputGuardEnabled := false
	if outputGuardSettings.Enabled != nil {
		outputGuardEnabled = *outputGuardSettings.Enabled
	}
```
并在 `GetSettings` 的 `c.JSON(http.StatusOK, settingsResponse{ ... })` 结尾添加：
```go
		OutputGuardEnabled:                  outputGuardEnabled,
		OutputGuardMaxToolArgumentBytes:     outputGuardSettings.MaxToolArgumentBytes,
		OutputGuardMaxToolArgumentDeltas:    outputGuardSettings.MaxToolArgumentDeltas,
		OutputGuardMaxChannelBytes:          outputGuardSettings.MaxChannelBytes,
		OutputGuardMaxRequestBytes:          outputGuardSettings.MaxRequestBytes,
		OutputGuardConfigGeneration:         outputGuardSettings.ConfigGeneration,
```

**插入锚点 4: `admin/handler.go:10601`（在 `UpdateSettings` 头部校验只读 generation）：**
```go
	if req.OutputGuardConfigGeneration != nil {
		writeError(c, http.StatusBadRequest, "output_guard_config_generation 为只读字段")
		return
	}
```

**插入锚点 5: `admin/handler.go:10750`（在 `UpdateSettings` 中校验范围与层级）：**
```go
	outputGuardUpdate := database.OutputGuardSettingsUpdate{
		Enabled:               req.OutputGuardEnabled,
		MaxToolArgumentBytes:  req.OutputGuardMaxToolArgumentBytes,
		MaxToolArgumentDeltas: req.OutputGuardMaxToolArgumentDeltas,
		MaxChannelBytes:       req.OutputGuardMaxChannelBytes,
		MaxRequestBytes:       req.OutputGuardMaxRequestBytes,
	}
	outputGuardUpdateRequested := outputGuardUpdate.Enabled != nil ||
		outputGuardUpdate.MaxToolArgumentBytes != nil ||
		outputGuardUpdate.MaxToolArgumentDeltas != nil ||
		outputGuardUpdate.MaxChannelBytes != nil ||
		outputGuardUpdate.MaxRequestBytes != nil

	if err := validateOutputGuardSettingsUpdateRanges(outputGuardUpdate); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	var outputGuardSettings database.OutputGuardSettings
	if outputGuardUpdateRequested {
		currentGuard, err := h.db.GetOutputGuardSettings(c.Request.Context())
		if err != nil {
			writeError(c, http.StatusInternalServerError, "读取防护设置失败："+err.Error())
			return
		}
		mergedGuard := currentGuard
		if outputGuardUpdate.Enabled != nil {
			val := *outputGuardUpdate.Enabled
			mergedGuard.Enabled = &val
		}
		if outputGuardUpdate.MaxToolArgumentBytes != nil {
			mergedGuard.MaxToolArgumentBytes = *outputGuardUpdate.MaxToolArgumentBytes
		}
		if outputGuardUpdate.MaxToolArgumentDeltas != nil {
			mergedGuard.MaxToolArgumentDeltas = *outputGuardUpdate.MaxToolArgumentDeltas
		}
		if outputGuardUpdate.MaxChannelBytes != nil {
			mergedGuard.MaxChannelBytes = *outputGuardUpdate.MaxChannelBytes
		}
		if outputGuardUpdate.MaxRequestBytes != nil {
			mergedGuard.MaxRequestBytes = *outputGuardUpdate.MaxRequestBytes
		}
		if err := database.ValidateOutputGuardSettings(mergedGuard); err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}

		committedGuard, err := h.db.UpdateOutputGuardSettings(c.Request.Context(), outputGuardUpdate)
		if err != nil {
			writeError(c, http.StatusInternalServerError, "保存防护设置失败："+err.Error())
			return
		}
		outputGuardSettings = committedGuard
		proxy.ApplyOutputGuardSettings(committedGuard)
	} else {
		latestGuard, err := h.db.GetOutputGuardSettings(c.Request.Context())
		if err != nil {
			writeError(c, http.StatusInternalServerError, "读取防护设置失败："+err.Error())
			return
		}
		outputGuardSettings = latestGuard
	}
```

**插入锚点 6: `admin/handler.go:12249`（在 `UpdateSettings` 响应返回中填入最新字段）：**
```go
		OutputGuardEnabled:                  outputGuardSettings.Enabled == nil || *outputGuardSettings.Enabled,
		OutputGuardMaxToolArgumentBytes:     outputGuardSettings.MaxToolArgumentBytes,
		OutputGuardMaxToolArgumentDeltas:    outputGuardSettings.MaxToolArgumentDeltas,
		OutputGuardMaxChannelBytes:          outputGuardSettings.MaxChannelBytes,
		OutputGuardMaxRequestBytes:          outputGuardSettings.MaxRequestBytes,
		OutputGuardConfigGeneration:         outputGuardSettings.ConfigGeneration,
```

---

### Step 4: Verify Backend Tests Pass (GREEN)

Run:
```bash
GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./admin -run "TestAdminOutputGuardSettings_.*"
```
Expected: PASS (所有断言包括字段对称性、只读拦截、层级非法回退、显式 false 及热应用均通过)。

---

### Step 5: Implement Frontend Types and Logic Tests

1. 修改 `frontend/src/types.ts`（约 line 2305，`SystemSettings` 接口内）：
```typescript
  output_guard_enabled: boolean
  output_guard_max_tool_argument_bytes: number
  output_guard_max_tool_argument_deltas: number
  output_guard_max_channel_bytes: number
  output_guard_max_request_bytes: number
  readonly output_guard_config_generation: number
```

2. 修改 `frontend/src/lib/settingsPayload.ts`：
```typescript
const RESPONSE_CACHE_CONFIG_GENERATION = "response_cache_config_generation";
const OUTPUT_GUARD_CONFIG_GENERATION = "output_guard_config_generation";
const CODEX_IMAGES_DEFAULT_MAIN_MODEL = "codex_images_default_main_model";
const CODEX_EGRESS = "codex_egress";
const CODEX_CLIENT_VERSIONS = "codex_client_versions";

export function buildWritableSettingsPayload<
  T extends object,
>(settings: T): Omit<T, typeof RESPONSE_CACHE_CONFIG_GENERATION | typeof OUTPUT_GUARD_CONFIG_GENERATION | typeof CODEX_IMAGES_DEFAULT_MAIN_MODEL | typeof CODEX_EGRESS | typeof CODEX_CLIENT_VERSIONS> {
  const payload = { ...settings };
  Reflect.deleteProperty(payload, RESPONSE_CACHE_CONFIG_GENERATION);
  Reflect.deleteProperty(payload, OUTPUT_GUARD_CONFIG_GENERATION);
  Reflect.deleteProperty(payload, CODEX_IMAGES_DEFAULT_MAIN_MODEL);
  Reflect.deleteProperty(payload, CODEX_EGRESS);
  Reflect.deleteProperty(payload, CODEX_CLIENT_VERSIONS);
  return payload;
}
```

3. 扩展 `frontend/src/lib/settingsPayload.test.mjs` 添加针对 `output_guard_config_generation` 剔除的断言：
```javascript
test("writable settings payload omits output guard generation regardless of value", () => {
  for (const generation of [42, 0, null, undefined]) {
    const settings = {
      site_name: "CodexProxy",
      output_guard_enabled: true,
      output_guard_max_tool_argument_bytes: 262144,
      output_guard_max_tool_argument_deltas: 4096,
      output_guard_max_channel_bytes: 2097152,
      output_guard_max_request_bytes: 4194304,
      output_guard_config_generation: generation,
    };

    const payload = buildWritableSettingsPayload(settings);

    assert.equal(
      Object.hasOwn(payload, "output_guard_config_generation"),
      false,
    );
    assert.equal(payload.output_guard_enabled, true);
    assert.equal(payload.output_guard_max_tool_argument_bytes, 262144);
  }
});
```

4. 创建 `frontend/src/lib/outputGuardSettings.ts`：
```typescript
export const KIB = 1024
export const MIB = 1024 * 1024
export const MIN_OUTPUT_GUARD_TOOL_BYTES = 16 * KIB
export const MAX_OUTPUT_GUARD_TOOL_BYTES = 4 * MIB
export const MIN_OUTPUT_GUARD_TOOL_DELTAS = 256
export const MAX_OUTPUT_GUARD_TOOL_DELTAS = 65536
export const MIN_OUTPUT_GUARD_CHANNEL_BYTES = 128 * KIB
export const MAX_OUTPUT_GUARD_CHANNEL_BYTES = 16 * MIB
export const MIN_OUTPUT_GUARD_REQUEST_BYTES = 256 * KIB
export const MAX_OUTPUT_GUARD_REQUEST_BYTES = 64 * MIB

export interface OutputGuardSettingsValues {
  output_guard_enabled: boolean
  output_guard_max_tool_argument_bytes: number
  output_guard_max_tool_argument_deltas: number
  output_guard_max_channel_bytes: number
  output_guard_max_request_bytes: number
}

export const DEFAULT_OUTPUT_GUARD_SETTINGS: OutputGuardSettingsValues = {
  output_guard_enabled: false,
  output_guard_max_tool_argument_bytes: 256 * KIB,
  output_guard_max_tool_argument_deltas: 4096,
  output_guard_max_channel_bytes: 2 * MIB,
  output_guard_max_request_bytes: 4 * MIB,
}

export const OUTPUT_GUARD_KEYS = [
  'output_guard_enabled',
  'output_guard_max_tool_argument_bytes',
  'output_guard_max_tool_argument_deltas',
  'output_guard_max_channel_bytes',
  'output_guard_max_request_bytes',
] as const

export function isOutputGuardKey(key: string): boolean {
  return OUTPUT_GUARD_KEYS.indexOf(key as any) !== -1
}

export function validateOutputGuardHierarchy(settings: {
  output_guard_max_tool_argument_bytes: number
  output_guard_max_channel_bytes: number
  output_guard_max_request_bytes: number
}): string | null {
  if (settings.output_guard_max_tool_argument_bytes > settings.output_guard_max_channel_bytes) {
    return '工具参数上限不能大于单通道上限 (tool <= channel)'
  }
  if (settings.output_guard_max_channel_bytes > settings.output_guard_max_request_bytes) {
    return '单通道上限不能大于单请求上限 (channel <= request)'
  }
  return null
}

export function validateOutputGuardRanges(settings: {
  output_guard_max_tool_argument_bytes?: number
  output_guard_max_tool_argument_deltas?: number
  output_guard_max_channel_bytes?: number
  output_guard_max_request_bytes?: number
}): string | null {
  const tool = settings.output_guard_max_tool_argument_bytes
  if (tool !== undefined && (tool < MIN_OUTPUT_GUARD_TOOL_BYTES || tool > MAX_OUTPUT_GUARD_TOOL_BYTES)) {
    return `工具参数字节范围必须在 ${MIN_OUTPUT_GUARD_TOOL_BYTES} ~ ${MAX_OUTPUT_GUARD_TOOL_BYTES} 之间`
  }
  const deltas = settings.output_guard_max_tool_argument_deltas
  if (deltas !== undefined && (deltas < MIN_OUTPUT_GUARD_TOOL_DELTAS || deltas > MAX_OUTPUT_GUARD_TOOL_DELTAS)) {
    return `工具 Delta 次数必须在 ${MIN_OUTPUT_GUARD_TOOL_DELTAS} ~ ${MAX_OUTPUT_GUARD_TOOL_DELTAS} 之间`
  }
  const channel = settings.output_guard_max_channel_bytes
  if (channel !== undefined && (channel < MIN_OUTPUT_GUARD_CHANNEL_BYTES || channel > MAX_OUTPUT_GUARD_CHANNEL_BYTES)) {
    return `单通道字节范围必须在 ${MIN_OUTPUT_GUARD_CHANNEL_BYTES} ~ ${MAX_OUTPUT_GUARD_CHANNEL_BYTES} 之间`
  }
  const request = settings.output_guard_max_request_bytes
  if (request !== undefined && (request < MIN_OUTPUT_GUARD_REQUEST_BYTES || request > MAX_OUTPUT_GUARD_REQUEST_BYTES)) {
    return `单请求字节范围必须在 ${MIN_OUTPUT_GUARD_REQUEST_BYTES} ~ ${MAX_OUTPUT_GUARD_REQUEST_BYTES} 之间`
  }
  return null
}

export function bytesToKiB(bytes: number): number { return Math.round(bytes / KIB) }
export function kibToBytes(kib: number): number { return kib * KIB }
export function bytesToMiB(bytes: number): number { return Math.round(bytes / MIB) }
export function mibToBytes(mib: number): number { return mib * MIB }
```

5. 创建 `frontend/src/lib/outputGuardSettings.test.mjs`：
```javascript
import test from 'node:test'
import assert from 'node:assert/strict'

import {
  KIB,
  MIB,
  DEFAULT_OUTPUT_GUARD_SETTINGS,
  OUTPUT_GUARD_KEYS,
  isOutputGuardKey,
  validateOutputGuardHierarchy,
  validateOutputGuardRanges,
  bytesToKiB,
  kibToBytes,
  bytesToMiB,
  mibToBytes,
} from './outputGuardSettings.ts'

test('default output guard settings match Spec §5.1 / §7', () => {
  assert.equal(DEFAULT_OUTPUT_GUARD_SETTINGS.output_guard_enabled, false)
  assert.equal(DEFAULT_OUTPUT_GUARD_SETTINGS.output_guard_max_tool_argument_bytes, 256 * KIB)
  assert.equal(DEFAULT_OUTPUT_GUARD_SETTINGS.output_guard_max_tool_argument_deltas, 4096)
  assert.equal(DEFAULT_OUTPUT_GUARD_SETTINGS.output_guard_max_channel_bytes, 2 * MIB)
  assert.equal(DEFAULT_OUTPUT_GUARD_SETTINGS.output_guard_max_request_bytes, 4 * MIB)
})

test('OUTPUT_GUARD_KEYS covers exactly the 5 writable settings', () => {
  assert.deepEqual(Array.from(OUTPUT_GUARD_KEYS), [
    'output_guard_enabled',
    'output_guard_max_tool_argument_bytes',
    'output_guard_max_tool_argument_deltas',
    'output_guard_max_channel_bytes',
    'output_guard_max_request_bytes',
  ])
  assert.equal(isOutputGuardKey('output_guard_enabled'), true)
  assert.equal(isOutputGuardKey('output_guard_config_generation'), false)
  assert.equal(isOutputGuardKey('unknown_key'), false)
})

test('validateOutputGuardHierarchy validates tool <= channel <= request', () => {
  assert.equal(validateOutputGuardHierarchy({
    output_guard_max_tool_argument_bytes: 256 * KIB,
    output_guard_max_channel_bytes: 2 * MIB,
    output_guard_max_request_bytes: 4 * MIB,
  }), null)

  assert.equal(validateOutputGuardHierarchy({
    output_guard_max_tool_argument_bytes: 2 * MIB,
    output_guard_max_channel_bytes: 2 * MIB,
    output_guard_max_request_bytes: 2 * MIB,
  }), null)

  assert.ok(validateOutputGuardHierarchy({
    output_guard_max_tool_argument_bytes: 3 * MIB,
    output_guard_max_channel_bytes: 2 * MIB,
    output_guard_max_request_bytes: 4 * MIB,
  })?.includes('tool <= channel'))

  assert.ok(validateOutputGuardHierarchy({
    output_guard_max_tool_argument_bytes: 256 * KIB,
    output_guard_max_channel_bytes: 8 * MIB,
    output_guard_max_request_bytes: 4 * MIB,
  })?.includes('channel <= request'))
})

test('validateOutputGuardRanges validates individual ranges', () => {
  assert.equal(validateOutputGuardRanges({
    output_guard_max_tool_argument_bytes: 256 * KIB,
    output_guard_max_tool_argument_deltas: 4096,
    output_guard_max_channel_bytes: 2 * MIB,
    output_guard_max_request_bytes: 4 * MIB,
  }), null)

  assert.ok(validateOutputGuardRanges({
    output_guard_max_tool_argument_bytes: 10 * KIB,
  })?.includes('工具参数字节范围'))

  assert.ok(validateOutputGuardRanges({
    output_guard_max_tool_argument_deltas: 70000,
  })?.includes('工具 Delta 次数'))

  assert.equal(validateOutputGuardRanges({
    output_guard_max_channel_bytes: 128 * KIB,
  }), null)
  assert.ok(validateOutputGuardRanges({
    output_guard_max_channel_bytes: 64 * KIB,
  })?.includes('单通道字节范围'))
})

test('unit conversions work accurately', () => {
  assert.equal(bytesToKiB(128 * 1024), 128)
  assert.equal(kibToBytes(128), 128 * 1024)
  assert.equal(bytesToKiB(262144), 256)
  assert.equal(kibToBytes(256), 262144)
  assert.equal(bytesToKiB(4194304), 4096)
  assert.equal(kibToBytes(4096), 4194304)
  assert.equal(bytesToMiB(2 * 1024 * 1024), 2)
  assert.equal(mibToBytes(2), 2 * 1024 * 1024)
})
```

---

### Step 6: Verify Frontend Logic Tests Pass

Run:
```bash
node --experimental-strip-types --test frontend/src/lib/outputGuardSettings.test.mjs frontend/src/lib/settingsPayload.test.mjs
```
Expected: All tests PASS.

---

### Step 7: Implement Frontend UI in `frontend/src/pages/Settings.tsx`

1. 引入常量与函数：
```typescript
import {
  KIB,
  DEFAULT_OUTPUT_GUARD_SETTINGS,
  OUTPUT_GUARD_KEYS,
  validateOutputGuardHierarchy,
  validateOutputGuardRanges,
  bytesToKiB,
  kibToBytes,
  bytesToMiB,
  mibToBytes,
} from '../lib/outputGuardSettings'
```

2. 在 `useState<SystemSettings>` 初始完整字面量中（约 line 2663-2835，`Settings.tsx`）补充新增 6 个必填字段的默认值（避免破坏 strict TS 类型检查）：
```typescript
    output_guard_enabled: false,
    output_guard_max_tool_argument_bytes: 262144,
    output_guard_max_tool_argument_deltas: 4096,
    output_guard_max_channel_bytes: 2097152,
    output_guard_max_request_bytes: 4194304,
    output_guard_config_generation: 0,
```

3. 在 `autoSaveSettingsPatch` 逻辑中加入 `output_guard_config_generation` 保护（与 `response_cache_config_generation` 对齐）：
```typescript
      const outputGuardRequest = patchKeys.some((k) => OUTPUT_GUARD_KEYS.includes(k as any))
      const mergedOutputGuardGeneration = outputGuardRequest && typeof updated.output_guard_config_generation === 'number'
        ? Math.max(currentSettings.output_guard_config_generation || 0, updated.output_guard_config_generation)
        : currentSettings.output_guard_config_generation
```

4. 在 General Tab 的安全区域（`id="settings-security"`，`PromptFilterCard` 旁）添加独立的防护卡片：
```tsx
                <SettingsCard
                  title="模型输出失控防护"
                  description="全局实时监控工具参数持续重复、细碎 delta 泛洪与通道/请求级字节预算超限"
                  icon={<ShieldAlert className="size-4" />}
                  tone={settingsForm.output_guard_enabled ? 'default' : 'danger'}
                  channels={ALL_UPSTREAM_CHANNELS}
                  badge={
                    <Badge variant={settingsForm.output_guard_enabled ? 'outline' : 'destructive'} className="text-[11px] gap-1">
                      <span className={`size-1.5 rounded-full ${settingsForm.output_guard_enabled ? 'bg-emerald-400' : 'bg-red-500'}`} />
                      {settingsForm.output_guard_enabled ? '已启用全局防护' : '已关闭 (高风险)'}
                    </Badge>
                  }
                  footer={
                    <div className="flex justify-end">
                      <Button
                        variant="outline"
                        size="sm"
                        className="text-xs h-7 gap-1"
                        onClick={() => {
                          const patch = { ...DEFAULT_OUTPUT_GUARD_SETTINGS }
                          setSettingsForm((prev) => ({ ...prev, ...patch }))
                          void autoSaveSettingsPatch(patch)
                        }}
                      >
                        <RotateCcw className="size-3" />
                        恢复默认防护
                      </Button>
                    </div>
                  }
                >
                  <div className="space-y-4">
                    {!settingsForm.output_guard_enabled && (
                      <div className="rounded-xl border border-destructive/30 bg-destructive/10 p-3.5 text-xs text-destructive flex items-start gap-2">
                        <ShieldAlert className="size-4 shrink-0 mt-0.5" />
                        <div>
                          <p className="font-semibold">高风险操作警告：失控防护已全局关闭</p>
                          <p className="opacity-90">关闭后模型失控生成死循环、重复代码或细碎 arguments delta 时，将直接击穿代理并在下游引发 O(n²) 全量重解析雪崩。仅用于临时诊断，请尽快重新开启！</p>
                        </div>
                      </div>
                    )}

                    <div className="flex items-center justify-between p-3 rounded-lg border bg-muted/20">
                      <div>
                        <div className="text-sm font-medium">启用失控输出防护</div>
                        <div className="text-xs text-muted-foreground">全局实时检测持续短周期重复、细碎 arguments 泛洪与字节预算超额并前置切断</div>
                      </div>
                      <Switch
                        checked={settingsForm.output_guard_enabled}
                        onCheckedChange={(checked) => {
                          setSettingsForm((f) => ({ ...f, output_guard_enabled: checked }))
                          void autoSaveSettingsPatch({ output_guard_enabled: checked })
                        }}
                      />
                    </div>

                    <div className={SETTINGS_FIELD_GRID}>
                      <SettingField
                        label="工具参数字节上限 (KiB)"
                        description="单次工具调用完整参数序列化字节硬上限（默认 256 KiB，范围 16 KiB..4096 KiB）"
                      >
                        <DraftNumberInput
                          min={16}
                          max={4096}
                          value={bytesToKiB(settingsForm.output_guard_max_tool_argument_bytes)}
                          onValueChange={(val) => {
                            setSettingsForm((f) => ({ ...f, output_guard_max_tool_argument_bytes: kibToBytes(val) }))
                          }}
                          onValueCommit={(val) => {
                            void autoSaveSettingsPatch({ output_guard_max_tool_argument_bytes: kibToBytes(val) })
                          }}
                        />
                      </SettingField>

                      <SettingField
                        label="工具非空 Delta 数量上限"
                        description="限制细碎 arguments delta 触发下游高频重解析事件的次数（默认 4096，范围 256..65536）"
                      >
                        <DraftNumberInput
                          min={256}
                          max={65536}
                          value={settingsForm.output_guard_max_tool_argument_deltas}
                          onValueChange={(val) => {
                            setSettingsForm((f) => ({ ...f, output_guard_max_tool_argument_deltas: val }))
                          }}
                          onValueCommit={(val) => {
                            void autoSaveSettingsPatch({ output_guard_max_tool_argument_deltas: val })
                          }}
                        />
                      </SettingField>

                      <SettingField
                        label="单 Channel 字节上限 (KiB)"
                        description="单 text/thinking 生成通道累计字节上限（默认 2048 KiB，范围 128 KiB..16384 KiB）"
                      >
                        <DraftNumberInput
                          min={128}
                          max={16384}
                          value={bytesToKiB(settingsForm.output_guard_max_channel_bytes)}
                          onValueChange={(val) => {
                            setSettingsForm((f) => ({ ...f, output_guard_max_channel_bytes: kibToBytes(val) }))
                          }}
                          onValueCommit={(val) => {
                            void autoSaveSettingsPatch({ output_guard_max_channel_bytes: kibToBytes(val) })
                          }}
                        />
                      </SettingField>

                      <SettingField
                        label="单请求语义预算 (KiB)"
                        description="一次请求内所有 channel 消耗的总语义字节硬上限（默认 4096 KiB 即 4 MiB，范围 256 KiB..65536 KiB）"
                      >
                        <DraftNumberInput
                          min={256}
                          max={65536}
                          value={bytesToKiB(settingsForm.output_guard_max_request_bytes)}
                          onValueChange={(val) => {
                            setSettingsForm((f) => ({ ...f, output_guard_max_request_bytes: kibToBytes(val) }))
                          }}
                          onValueCommit={(val) => {
                            void autoSaveSettingsPatch({ output_guard_max_request_bytes: kibToBytes(val) })
                          }}
                        />
                      </SettingField>
                    </div>
                  </div>
                </SettingsCard>
```

---

### Step 8: Verify UI Compilation and Typecheck

Run:
```bash
./frontend/node_modules/.bin/tsc -p frontend/tsconfig.json --noEmit
```
Expected: PASS (0 errors).

---

### Step 9: Commit

```bash
git add admin/output_guard_settings.go admin/output_guard_settings_test.go admin/handler.go frontend/src/types.ts frontend/src/lib/settingsPayload.ts frontend/src/lib/settingsPayload.test.mjs frontend/src/lib/outputGuardSettings.ts frontend/src/lib/outputGuardSettings.test.mjs frontend/src/pages/Settings.tsx
git commit -m "feat(admin,frontend): add model output runaway guard admin settings API and dedicated UI card"
```
