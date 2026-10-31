# 05a-Detection-Config: 检测层与配置传输门禁规格验收 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 针对 Codex2API 全局模型输出失控防护体系（Model Output Guard）交付紧凑可执行的 Task 11.2（检测层规格验收，Spec §9 场景 1~11）与 Task 11.4（持久化配置、修订轮询与传输 Framing 门禁验收，Spec §9 场景 21~23）实施计划，严格建立真实行为断言、区分 01/02/03 已有测试引用与新增测试缺口，并如实记录审查反例与受控突变反证。

**Architecture:** 
1. **检测层规格验收套件（`security/outputguard` 同包测试，场景 1~11）**：在 `outputguard` 内部测试包中直接对 `RequestGuard`、`ChannelGuard` 与 `RepetitionDetector` 进行黑盒与灰盒边界断言，涵盖短周期快速触发、分块不变性（防 4096 deltas 提前遮蔽）、9215B 周期片段嵌入与 8192B 窗口对齐/非对齐即时尾检、多 Channel 隔离与 ID 别名防重置、硬预算精确拦截与下层 `ErrScanBudgetExhausted` 防混层、边界值 UTF-8 与 int64 溢出加法防反转、完整快照防逃逸、终态链 8a/8b/8c 成本硬上界（扫描 $\le 2\times\text{limit}$，总校验 $\le 5\times\text{limit}$）、7 类正常样本绝不误报、有意大段重复按规拦截，以及尾检放行规则。
2. **持久化配置与传输门禁验收套件（`database` 与 `proxy` 包测试，场景 21~23）**：基于真实 SQLite 数据库（`database.New("sqlite", ...)`）与 PostgreSQL 验证 `*bool` 三态指针存储、并发更新 CAS generation 自增与恢复默认；验证 5 秒后台 Revision 轮询、在途请求旧快照隔离与冷启动 DB 故障返回 503 `service_unavailable` / WS 1013；验证语义 Guard 全局关闭（`Enabled = false`）时，传输 Framing 有界读取器依然对超过 8 MiB 的超大无换行数据行与多行 Event 进行硬上限拦截，同时保持现有 WebSocket 48MB `security.MaxRequestBodySize` 不被误收紧。

**Tech Stack:** Go 1.26+, `github.com/codex2api/api`, `github.com/codex2api/database`, `github.com/codex2api/proxy`, `github.com/codex2api/security`, `github.com/gin-gonic/gin`, `github.com/gorilla/websocket`, `modernc.org/sqlite`.

**Spec:** `docs/superpowers/specs/2026-10-28-model-output-guard-design.md`

## Global Constraints

- **范围限制**：本子计划仅覆盖 Spec §9 场景 1~11（Task 11.2 检测层验收）与场景 21~23（Task 11.4 持久化配置、修订轮询与传输 Framing 门禁验收）。生命周期与全协议终止（场景 12~20，Task 11.3）以及性能基准与长跑门禁（场景 24~28，Task 11.5）由独立子计划承接。
- **不可妥协的设计取舍**：
  - **同包测试路径规范**：检测测试置于 `security/outputguard` 同包（`package outputguard`），以便直接观测私有计数器、高水位与内部扫描预算，杜绝虚构公开统计 API；配置测试按职责分属 `database`（DB 迁移与并发合并）与 `proxy`（运行时轮询、冷启动门禁与 Framing 拦截），严禁跨包引用非导出符号。
  - **禁止虚构工厂**：数据库测试必须使用真实生产构造函数 `database.New("sqlite", filepath.Join(t.TempDir(), ...))`，绝不使用虚构的 `database.NewTestSQLite`。
  - **下层错误与上层映射不混层**：下层 `ChannelGuard.CheckAppend` 在扫描预算不足时返回 `ErrScanBudgetExhausted`；由上层 `RequestGuard` 将其统一映射为公开 reason（工具 channel 为 `tool_argument_bytes`，普通 channel 为 `channel_bytes`）。
  - **审查反例记录与回归防护**：
    - 01 核心状态机反例：在 `channel=1MiB, request=600KiB` 时，先追加 0.5MiB 数据并消耗扫描预算至 0，再追加 0.2MiB 时同时满足 `request_bytes` 超限与扫描预算不足；根据 Spec §5.3 八步执行流水线，步骤 4（请求级语义字节）必须先于步骤 6（扫描预算扣减与检查）执行，期望 reason 应为 `request_bytes` 而非 `channel_bytes`。本计划显式保留该回归测试 fixture，不假设 01 核心无缺陷直接通过。
    - 03 协议投影反例：Gemini 8190B 周期 append 紧随 finish STOP 携带 4B 尾部 text 被误当 snapshot 尾检漏检、Chat tool calls finish 缺 End 帧等反例由后续 targetfix 修复，本验收套件按 Spec 严格断言。
  - **Framing 边界隔离**：传输层 Framing 机制独立于语义 Guard 开关，SSE 单无换行行限制为 8 MiB、多行 Event 限制为 8 MiB、缺失路径的非流式响应体使用 `ReadBoundedResponseBody(reader, 16<<20)` 限制 16 MiB；但必须保留 Responses 原生 64 MiB、Gemini 原生 8 MiB 与 WebSocket 48MB（`security.MaxRequestBodySize`），严禁误将所有路径按 8 MiB 收紧。
  - **Tests-only 验收红线**：所有新增验收测试必须具有可观测的 RED 失败基线或受控突变（Controlled Mutant）反证，严禁无断言直接变绿。

---

## Exact Interfaces & Dependencies

### 1. 核心状态机接口（Consumes: `security/outputguard`, Task 1~3 定义；计划交付阶段不等同于生产已就绪）
```go
package outputguard

const (
    MaxRequestEvents   = 65536
    MaxRequestChannels = 128
    TriggeredCode      = "model_output_guard_triggered"
)

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

type RequestGuard struct {
    // 内部私有字段在同包测试中直接可测:
    // channels map[string]*ChannelGuard
    // requestEvents int
    // requestBytes int64
    // tripped bool
    // trippedReason string
}

func NewRequestGuard(cfg Config) *RequestGuard
func (g *RequestGuard) RecordEvent() error
func (g *RequestGuard) ObserveAppend(channelID string, aliasID string, kind ChannelKind, delta []byte) error
func (g *RequestGuard) ObserveSnapshot(channelID string, aliasID string, kind ChannelKind, snapshot []byte, terminalStage string) error
func (g *RequestGuard) ObserveAppendNoEvent(channelID string, aliasID string, kind ChannelKind, delta []byte) error
func (g *RequestGuard) ObserveSnapshotNoEvent(channelID string, aliasID string, kind ChannelKind, snapshot []byte, terminalStage string) error
func (g *RequestGuard) EndChannel(channelID string) error
func (g *RequestGuard) IsTripped() bool
func (g *RequestGuard) TrippedReason() string

type ChannelGuard struct {
    // 内部私有字段在同包测试中直接可测:
    // highwater int64
    // remainingScanBudget int64
    // deltaCount int
    // terminalPermitsUsed int
    // terminalBaselineHash uint64
}

func NewChannelGuard(id string, kind ChannelKind, maxBytes int64, maxDeltas int) *ChannelGuard
func (c *ChannelGuard) CheckAppend(delta []byte) (growth int64, cost int64, err error)
func (c *ChannelGuard) CommitAppend(delta []byte, growth int64, cost int64) error
func (c *ChannelGuard) CheckSnapshot(snapshot []byte, terminalStage string) (growth int64, cost int64, skipRepetition bool, err error)
func (c *ChannelGuard) CommitSnapshot(snapshot []byte, growth int64, cost int64, terminalStage string) error
func (c *ChannelGuard) RemainingScanBudget() int64
func (c *ChannelGuard) Highwater() int64
func (c *ChannelGuard) DeltaCount() int
func (c *ChannelGuard) ReleaseWindow()
```

### 2. 持久化存储接口（Consumes: `database`, Task 4 / 02a-database 契约；计划交付阶段）
```go
package database

type OutputGuardSettings struct {
    Enabled               *bool `json:"output_guard_enabled"`
    MaxToolArgumentBytes  int64 `json:"output_guard_max_tool_argument_bytes"`
    MaxToolArgumentDeltas int   `json:"output_guard_max_tool_argument_deltas"`
    MaxChannelBytes       int64 `json:"output_guard_max_channel_bytes"`
    MaxRequestBytes       int64 `json:"output_guard_max_request_bytes"`
    ConfigGeneration      int64 `json:"output_guard_config_generation"`
}

type OutputGuardSettingsUpdate struct {
    Enabled               *bool
    MaxToolArgumentBytes  *int64
    MaxToolArgumentDeltas *int
    MaxChannelBytes       *int64
    MaxRequestBytes       *int64
}

var (
    ErrInvalidOutputGuardSettings    = errors.New("invalid output guard settings")
    ErrOutputGuardGenerationOverflow = errors.New("output guard config generation overflow")
)

func (db *DB) GetOutputGuardSettings(ctx context.Context) (OutputGuardSettings, error)
func (db *DB) UpdateOutputGuardSettings(ctx context.Context, u OutputGuardSettingsUpdate) (OutputGuardSettings, error)
func DefaultOutputGuardSettings() OutputGuardSettings
func ValidateOutputGuardSettings(settings OutputGuardSettings) error
```

### 3. 运行时配置与门禁接口（Consumes: `proxy`, Task 5 / 02b-runtime 契约；计划交付阶段，不修改生产全局 RuntimeSettings 伪造字段）
```go
package proxy

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

func ApplyOutputGuardSettings(database.OutputGuardSettings) bool
func CurrentOutputGuardConfig() OutputGuardAppliedConfig
func GetOutputGuardConfigSyncStatus() OutputGuardConfigSyncStatus
func IsOutputGuardReady() bool
func LoadOutputGuardSettings(ctx context.Context, reader OutputGuardSettingsReader) error
func StartOutputGuardConfigPoller(parent context.Context, db *database.DB) bool
func RequireOutputGuardReadyHTTP(c *gin.Context) bool
func RequireOutputGuardReadyWS(conn *websocket.Conn) bool
func RequireOutputGuardReadyStream(conn *websocket.Conn, streamID string) bool

// 仅同包测试导出辅助定义 (proxy_test 可见，生产代码不导出带 ForTest 后缀的初始化)
func ResetOutputGuardStateForTest()
func SetOutputGuardReadyForTest(ready bool)
func PollOutputGuardConfigOnceForTest(ctx context.Context, db *database.DB) error
```

### 4. 传输 Framing 接口（Consumes: `proxy`, Task 8 / 03-protocol 契约；计划交付阶段导出别名与桥接定义）
```go
package proxy

const (
    MaxSSELineBytes     = 8 << 20
    MaxSSEEventBytes    = 8 << 20
    MaxResponseBodyBytes = 16 << 20
)

var (
    ErrSSELineTooLong      = errors.New("upstream SSE line exceeds limit")
    ErrSSEEventTooLarge    = errors.New("upstream SSE event exceeds limit")
    ErrResponseBodyTooLarge = errors.New("upstream response body exceeds limit")
)

func ReadSSEStreamWithEvent(reader io.Reader, onEvent func(event string, data []byte) bool) error
func ReadBoundedResponseBody(reader io.Reader, limit int64) ([]byte, error)
```

---

## Tasks

### Task 11.2: 检测层规格验收套件（Spec §9 场景 1~11）

**Files:**
- Create: `security/outputguard/acceptance_detection_test.go`
- Consumes: `security/outputguard` (Task 1~3 核心实现)

**覆盖关系与复用映射：**
- **场景 1（事故短周期）**：缺口新增。模拟事故 31B 周期文本，断言在 8192~10240B 触发，绝不等 128 KiB。
- **场景 2（分块不变性）**：缺口新增。按 1/7/64/512/1024/2048 字节及随机边界切分，断言切分不能让重复消失（配置 `MaxToolArgumentDeltas=65536` 防止 1-byte 切分被 delta 计数提前遮蔽）。
- **场景 3（9215B 片段与边界）**：部分复用 01 `TestRepetitionDetectorConsecutive9215BytesGuaranteedHit`，新增 Request 级别单大 Delta 嵌入 9215B 周期片段（前缀+周期+后缀）断言整事件拦截，以及 8192B 对齐窗口必中测试。
- **场景 4（多通道隔离与晚到 alias）**：扩展 01 `TestRequestGuardLateArrivingAliasBidirectionalMerge`，多 text/thinking/tool 交错并发写入，使用真实 `item_id` 晚到绑定 `call_id` 别名，断言不重置 deltaCount 与高水位。
- **场景 5（硬预算精确拦截与下层错误不混层）**：复用 01 `TestRequestGuardRequestEventsFlood`、`TestRequestGuardChannelFlood`、`TestRequestGuardToolArgumentDeltasFlood`，新增工具 bytes 精确 +1 拦截、跨 channel 绕单项超 `request_bytes` 拦截、空 delta 洪泛拦截，并断言下层 `ErrScanBudgetExhausted` 由上层映射为 `tool_argument_bytes`/`channel_bytes`，杜绝内部错误泄露。
- **场景 6（边界值与 int64 溢出）**：缺口新增。UTF-8 中文、Emoji、转义符与未闭合 JSON 正常处理；利用私有字段模拟高水位接近 `math.MaxInt64`，断言加法溢出保护。
- **场景 7（完整快照防逃逸）**：缺口新增。done-only、terminal-only、native object 变化快照内含周期性片段时绝不逃逸。
- **场景 8（8a/8b/8c 成本硬上界可观察计数）**：复用并整合 01 `TestChannelGuardCase8a_ScanBudgetExhaustionAndVariantRejection`、`TestChannelGuardCase8b_TerminalChainZeroCostAndExhaustion`、`TestChannelGuardCase8c_NonTerminalSnapshotCannotEstablishBaseline`，并使用同包私有字段 `terminalPermitsUsed` 与 `remainingScanBudget` 验证总处理量 $\le 5\times\text{limit}$。
- **场景 9（7 类正常样本绝不误报）**：缺口新增。代码、Markdown、多工具、未闭合 JSON、中英文、图片 Base64、心跳数据均不误报。
- **场景 10（有意大段重复按规拦截）**：缺口新增。生成 10 KiB 周期文本，断言作为安全取舍被正常拦截。
- **场景 11（尾检规则验证）**：复用 01 `TestRepetitionDetectorTailCheckShortText` 与 `TestRepetitionDetectorTailCheckUnalignedWindow`，在 RequestGuard 级别验证短流 (<8192B) `EndChannel` 放行，已满 8192B 未对齐 1024B 在 `EndChannel` 触发尾检。
- **附加回归（01 核心 Append 优先级反例）**：新增 `TestAppendScanBudgetVsRequestBytesPrecedence`，记录当前已知反例行为并固定断言。

- [ ] **Step 1: 编写检测层全场景验收测试代码**

```go
// security/outputguard/acceptance_detection_test.go
package outputguard

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"testing"
)

// 场景 1: 事故样式短周期重复（p<=256，8192~10240 触发，不等到 128 KiB）
func TestAcceptanceScenario1_AccidentPatternEarlyTrigger(t *testing.T) {
	rg := NewRequestGuard(Config{
		Enabled:               true,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})

	pattern := []byte("_auditfree.md_pluginless.tmp.md") // 31 字节周期
	chunkSize := 128
	totalPushed := 0
	trippedAt := 0

	for totalPushed < 131072 {
		chunk := make([]byte, chunkSize)
		for i := 0; i < chunkSize; i++ {
			chunk[i] = pattern[(totalPushed+i)%len(pattern)]
		}
		err := rg.ObserveAppend("call_write", "", KindToolArguments, chunk)
		totalPushed += chunkSize
		if err != nil {
			trippedAt = totalPushed
			if err.Error() != "repetition" {
				t.Fatalf("expected repetition error, got %v", err)
			}
			break
		}
	}

	if trippedAt == 0 {
		t.Fatalf("expected guard to trip, but reached totalPushed=%d", totalPushed)
	}
	if trippedAt > 10240 {
		t.Fatalf("expected early trigger near 8192-10240 bytes, but triggered at %d bytes", trippedAt)
	}
	if !rg.IsTripped() || rg.TrippedReason() != "repetition" {
		t.Fatalf("expected tripped with repetition, got %s", rg.TrippedReason())
	}
}

// 场景 2: 同语义流按 1-byte、小/大 delta、随机边界分割均可检测，切分不能让重复消失
func TestAcceptanceScenario2_ChunkInvariance(t *testing.T) {
	pattern := []byte("defensive_pipeline_token_replay_") // 32 bytes
	var fullStream []byte
	for len(fullStream) < 12000 {
		fullStream = append(fullStream, pattern...)
	}

	chunkSizes := []int{1, 7, 64, 512, 1024, 2048}
	for _, size := range chunkSizes {
		t.Run(fmt.Sprintf("FixedChunkSize_%d", size), func(t *testing.T) {
			rg := NewRequestGuard(Config{
				Enabled:               true,
				MaxToolArgumentBytes:  131072,
				MaxToolArgumentDeltas: 65536, // 放宽 delta 计数，防止 1-byte 切分在 4096 delta 处提前遮蔽
				MaxChannelBytes:       2097152,
				MaxRequestBytes:       4194304,
			})
			hit := false
			for offset := 0; offset < len(fullStream); offset += size {
				end := offset + size
				if end > len(fullStream) {
					end = len(fullStream)
				}
				err := rg.ObserveAppend("c_inv", "", KindToolArguments, fullStream[offset:end])
				if err != nil {
					if err.Error() == "repetition" {
						hit = true
						break
					}
					t.Fatalf("unexpected error: %v", err)
				}
			}
			if !hit {
				t.Fatalf("chunk size %d failed to detect repetition across boundaries", size)
			}
		})
	}

	t.Run("RandomChunkBoundaries", func(t *testing.T) {
		r := rand.New(rand.NewSource(42))
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  131072,
			MaxToolArgumentDeltas: 65536,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		offset := 0
		hit := false
		for offset < len(fullStream) {
			sz := r.Intn(256) + 1
			end := offset + sz
			if end > len(fullStream) {
				end = len(fullStream)
			}
			err := rg.ObserveAppend("c_rand", "", KindToolArguments, fullStream[offset:end])
			if err != nil {
				if err.Error() == "repetition" {
					hit = true
					break
				}
				t.Fatalf("unexpected error: %v", err)
			}
			offset = end
		}
		if !hit {
			t.Fatalf("random chunk boundaries failed to detect repetition")
		}
	})
}

// 场景 3: 单大 delta 中间嵌入 9215 bytes 精确短周期片段，尾部正常仍触发
func TestAcceptanceScenario3_Embedded9215BytesAnd8192Boundary(t *testing.T) {
	// 精确构造 9215 bytes 周期片段：repeat ceil 后 slice 到恰好 9215 bytes，并验证多个不同起点
	unit := []byte("1234567890abcdefghijklmno") // p=25
	repeats := int(math.Ceil(float64(9215) / float64(len(unit))))

	for _, startOffset := range []int{0, 7, 13, 24} {
		shiftedUnit := append(unit[startOffset:], unit[:startOffset]...)
		shiftedPeriodic := bytes.Repeat(shiftedUnit, repeats)[:9215]
		if len(shiftedPeriodic) != 9215 {
			t.Fatalf("expected 9215 bytes, got %d", len(shiftedPeriodic))
		}

		prng := rand.New(rand.NewSource(int64(1001 + startOffset)))
		prefix := make([]byte, 500)
		prng.Read(prefix)
		suffix := make([]byte, 1000)
		prng.Read(suffix)

		var singleLargeDelta []byte
		singleLargeDelta = append(singleLargeDelta, prefix...)
		singleLargeDelta = append(singleLargeDelta, shiftedPeriodic...)
		singleLargeDelta = append(singleLargeDelta, suffix...)

		rgLoop := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  131072,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		err := rgLoop.ObserveAppend("c_embed", "", KindText, singleLargeDelta)
		if err == nil || err.Error() != "repetition" {
			t.Fatalf("offset %d: expected repetition error on 9215 bytes segment, got %v", startOffset, err)
		}
	}

	t.Run("Aligned8192BytesHits", func(t *testing.T) {
		rgAligned := NewRequestGuard(Config{
			Enabled:         true,
			MaxChannelBytes: 2097152,
			MaxRequestBytes: 4194304,
		})
		alignedData := bytes.Repeat([]byte("abcdef0123456789"), 512) // 8192 bytes, p=16
		err := rgAligned.ObserveAppend("c_aligned", "", KindText, alignedData)
		if err == nil || err.Error() != "repetition" {
			t.Fatalf("expected repetition error on 8192-aligned window, got %v", err)
		}
	})
}

// 场景 4: 多 text / thinking / tool 交错不互相污染，真实 item_id 无 call_id 晚到 alias
func TestAcceptanceScenario4_InterleavedChannelsAndNoReset(t *testing.T) {
	rg := NewRequestGuard(Config{
		Enabled:               true,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})

	textData := []byte("Normal text message token. ")
	thinkingData := []byte("Deep reflection thoughts. ")
	toolData := []byte(`{"arg":"value_`)

	for i := 0; i < 50; i++ {
		if err := rg.ObserveAppend("c_text", "", KindText, textData); err != nil {
			t.Fatalf("text append failed: %v", err)
		}
		if err := rg.ObserveAppend("c_think", "", KindThinking, thinkingData); err != nil {
			t.Fatalf("thinking append failed: %v", err)
		}
		if err := rg.ObserveAppend("item_tool_1", "", KindToolArguments, toolData); err != nil {
			t.Fatalf("tool append failed: %v", err)
		}
	}

	// 晚到 call_id 绑定到 item_tool_1 作为 alias
	if err := rg.ObserveAppend("call_999", "item_tool_1", KindToolArguments, []byte(`1"}`)); err != nil {
		t.Fatalf("alias append failed: %v", err)
	}

	cg := rg.channels["item_tool_1"]
	if cg == nil {
		cg = rg.channels["call_999"]
	}
	if cg == nil {
		t.Fatalf("channel not resolved")
	}
	if cg.DeltaCount() != 51 {
		t.Fatalf("delta count was reset! want 51, got %d", cg.DeltaCount())
	}
	expectedHighwater := int64(50*len(toolData) + len(`1"}`))
	if cg.Highwater() != expectedHighwater {
		t.Fatalf("highwater was reset or inaccurate! want %d, got %d", expectedHighwater, cg.Highwater())
	}

	// 验证 index alias 映射: 使用数字索引形式（如 "call_index_0"）向已映射通道追加，必须指向同一 channel 实例且累加高水位
	if err := rg.ObserveAppend("call_index_0", "call_999", KindToolArguments, []byte(`,"extra":2}`)); err != nil {
		t.Fatalf("index alias append failed: %v", err)
	}
	if cg.DeltaCount() != 52 {
		t.Fatalf("index alias delta count want 52, got %d", cg.DeltaCount())
	}
	expectedHighwater += int64(len(`,"extra":2}`))
	if cg.Highwater() != expectedHighwater {
		t.Fatalf("highwater after index alias want %d, got %d", expectedHighwater, cg.Highwater())
	}
}

// 场景 5: 硬预算精确拦截与 ErrScanBudgetExhausted 不混层
func TestAcceptanceScenario5_BudgetInterception(t *testing.T) {
	// 5a: 工具 bytes 精确 +1 拦截
	t.Run("ToolBytesBudgetExactPlusOne", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  131072,
			MaxToolArgumentDeltas: 65536,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		prng := rand.New(rand.NewSource(2001))
		exactData := make([]byte, 131072)
		prng.Read(exactData)
		if err := rg.ObserveAppend("c_exact", "", KindToolArguments, exactData); err != nil {
			t.Fatalf("exact tool bytes should pass, got: %v", err)
		}
		err := rg.ObserveAppend("c_exact", "", KindToolArguments, []byte("x"))
		if err == nil || err.Error() != "tool_argument_bytes" {
			t.Fatalf("expected tool_argument_bytes error, got %v", err)
		}
	})

	// 5b: 细碎参数超 delta (4096 deltas)
	t.Run("ToolDeltasBudget", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  131072,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		for i := 0; i < 4096; i++ {
			if err := rg.ObserveAppend("c_tool", "", KindToolArguments, []byte("x")); err != nil {
				t.Fatalf("unexpected err at %d: %v", i, err)
			}
		}
		err := rg.ObserveAppend("c_tool", "", KindToolArguments, []byte("x"))
		if err == nil || err.Error() != "tool_argument_deltas" {
			t.Fatalf("expected tool_argument_deltas error, got %v", err)
		}
	})

	// 5c: 跨 channel 绕单项上限 -> request_bytes 拦截
	t.Run("RequestBytesBudget", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  131072,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		prng := rand.New(rand.NewSource(2002))
		buf1 := make([]byte, 2097152)
		prng.Read(buf1)
		buf2 := make([]byte, 2097152)
		prng.Read(buf2)

		if err := rg.ObserveAppend("c1", "", KindText, buf1); err != nil {
			t.Fatalf("c1 failed: %v", err)
		}
		if err := rg.ObserveAppend("c2", "", KindText, buf2); err != nil {
			t.Fatalf("c2 failed: %v", err)
		}
		err := rg.ObserveAppend("c3", "", KindText, []byte("a"))
		if err == nil || err.Error() != "request_bytes" {
			t.Fatalf("expected request_bytes error, got %v", err)
		}
	})

	// 5d: 空 delta 洪泛与 65536 业务事件
	t.Run("EmptyDeltaEventFlood", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  131072,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		for i := 0; i < MaxRequestEvents; i++ {
			if err := rg.ObserveAppend("c_empty", "", KindText, []byte{}); err != nil {
				t.Fatalf("empty event %d rejected: %v", i, err)
			}
		}
		err := rg.ObserveAppend("c_empty", "", KindText, []byte{})
		if err == nil || err.Error() != "request_events" {
			t.Fatalf("expected request_events error, got %v", err)
		}
	})

	// 5e: 128 Channels 洪泛
	t.Run("ChannelCountFlood", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  131072,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		for i := 0; i < MaxRequestChannels; i++ {
			cid := fmt.Sprintf("chan_%d", i)
			if err := rg.ObserveAppend(cid, "", KindText, []byte("ok")); err != nil {
				t.Fatalf("channel %d rejected: %v", i, err)
			}
		}
		err := rg.ObserveAppend("chan_129", "", KindText, []byte("fail"))
		if err == nil || err.Error() != "channel_count" {
			t.Fatalf("expected channel_count error, got %v", err)
		}
	})

	// 5f: 下层 ErrScanBudgetExhausted 映射隔离（杜绝内部哨兵错误泄露，统一映射为公开 reason）
	t.Run("ScanBudgetExhaustionMapping", func(t *testing.T) {
		// ToolArguments channel 耗尽扫描预算 -> 映射为 tool_argument_bytes
		rgTool := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  131072, // scan budget = 262144
			MaxToolArgumentDeltas: 65536,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		chunk := make([]byte, 1024)
		rand.New(rand.NewSource(501)).Read(chunk)
		for i := 0; i < 128; i++ {
			if err := rgTool.ObserveAppend("tool_map", "", KindToolArguments, chunk); err != nil {
				t.Fatalf("append %d failed: %v", i, err)
			}
		}
		// 终态快照建立 baseline
		full := bytes.Repeat(chunk, 128)
		if err := rgTool.ObserveSnapshot("tool_map", "", KindToolArguments, full, "arguments.done"); err != nil {
			t.Fatalf("snapshot baseline failed: %v", err)
		}
		// 变体快照超出扫描预算 -> 必须映射为 tool_argument_bytes，绝不泄露内部 ErrScanBudgetExhausted
		variant := bytes.Clone(full)
		variant[100] ^= 0xEE
		err := rgTool.ObserveSnapshot("tool_map", "", KindToolArguments, variant, "item.done")
		if err == nil || err.Error() != "tool_argument_bytes" {
			t.Fatalf("expected mapped reason tool_argument_bytes, got %v", err)
		}

		// Text channel 耗尽扫描预算 -> 映射为 channel_bytes
		rgText := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  131072,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       131072, // scan budget = 262144
			MaxRequestBytes:       4194304,
		})
		for i := 0; i < 128; i++ {
			if err := rgText.ObserveAppend("text_map", "", KindText, chunk); err != nil {
				t.Fatalf("text append %d failed: %v", i, err)
			}
		}
		if err := rgText.ObserveSnapshot("text_map", "", KindText, full, "arguments.done"); err != nil {
			t.Fatalf("text snapshot failed: %v", err)
		}
		errText := rgText.ObserveSnapshot("text_map", "", KindText, variant, "item.done")
		if errText == nil || errText.Error() != "channel_bytes" {
			t.Fatalf("expected mapped reason channel_bytes, got %v", errText)
		}
	})
}

// 场景 6: 边界值、UTF-8 中文、Emoji、转义符与 int64 overflow 防护
func TestAcceptanceScenario6_BoundaryValuesAndUTF8(t *testing.T) {
	rg := NewRequestGuard(Config{
		Enabled:               true,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})

	multilingual := []byte("这是一段中文测试，包含特殊符号与表情：👨‍👩‍👧‍👦 🎉 🚀 以及转义符：\\n \\t \\\" \\\\ 还有非闭合参数: {\"file\": \"/path/to/file.txt\"")
	if err := rg.ObserveAppend("c_multi", "", KindText, multilingual); err != nil {
		t.Fatalf("multilingual append failed: %v", err)
	}

	// int64 overflow 防护：受控突变反证
	t.Run("Int64OverflowGuard", func(t *testing.T) {
		rgOverflow := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  131072,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		_ = rgOverflow.ObserveAppend("c_overflow", "", KindText, []byte("init"))

		// 模拟高水位接近 MaxInt64，断言加法溢出保护直接拦截并报错
		cg := rgOverflow.channels["c_overflow"]
		cg.highwater = math.MaxInt64 - 10
		rgOverflow.requestBytes = math.MaxInt64 - 10

		err := rgOverflow.ObserveAppend("c_overflow", "", KindText, make([]byte, 20))
		if err == nil {
			t.Fatalf("expected overflow rejection, but got nil (highwater/requestBytes may have wrapped around!)")
		}
	})
}

// 场景 7: 完整快照防逃逸（done-only、terminal-only、native object、变化 snapshot）
func TestAcceptanceScenario7_SnapshotsCannotBypass(t *testing.T) {
	// 7a: 基础 9215B 周期快照拦截（严格 repeat ceil 后 slice 到恰好 9215 bytes）
	t.Run("BasicPeriodicSnapshot", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  131072,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		unit := []byte("snapshot_period_pattern_test1234") // p=32
		repeats := int(math.Ceil(float64(9215) / float64(len(unit))))
		periodicSnapshot := bytes.Repeat(unit, repeats)[:9215]
		err := rg.ObserveSnapshot("c_snap", "", KindText, periodicSnapshot, "response.completed")
		if err == nil || err.Error() != "repetition" {
			t.Fatalf("snapshot with repetition must be rejected, got: %v", err)
		}
	})

	// 7b: done-only 快照（无前置 append 直接下发 arguments.done / output_item.done 周期快照）
	t.Run("DoneOnlySnapshot", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  131072,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		unit := []byte("tool_arg_done_only_pattern_abc!") // 31 bytes
		repeats := int(math.Ceil(float64(9215) / float64(len(unit))))
		snap := bytes.Repeat(unit, repeats)[:9215]
		err := rg.ObserveSnapshot("tool_done_only", "", KindToolArguments, snap, "arguments.done")
		if err == nil || err.Error() != "repetition" {
			t.Fatalf("done-only snapshot must be rejected, got: %v", err)
		}
	})

	// 7c: terminal-only 终态快照拦截（仅下发终态阶段标识）
	t.Run("TerminalOnlySnapshot", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  131072,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		unit := []byte("terminal_only_stream_finished__!") // 32 bytes
		repeats := int(math.Ceil(float64(9215) / float64(len(unit))))
		snap := bytes.Repeat(unit, repeats)[:9215]
		err := rg.ObserveSnapshot("terminal_only", "", KindText, snap, "item.done")
		if err == nil || err.Error() != "repetition" {
			t.Fatalf("terminal-only snapshot must be rejected, got: %v", err)
		}
	})

	// 7d: native object 原生紧凑序列化快照防逃逸（模拟 Gemini 等协议中的原生对象序列化快照）
	t.Run("NativeObjectSnapshot", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  131072,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		unit := []byte(`{"key":"cyclic_val_12345678"},`) // 30 bytes
		repeats := int(math.Ceil(float64(9215) / float64(len(unit))))
		snap := bytes.Repeat(unit, repeats)[:9215]
		err := rg.ObserveSnapshot("native_obj", "", KindToolArguments, snap, "response.completed")
		if err == nil || err.Error() != "repetition" {
			t.Fatalf("native object snapshot must be rejected, got: %v", err)
		}
	})

	// 7e: 变化 snapshot（前次快照与后次快照内容变化，增量与变化内容被完整核验）
	t.Run("MutatingSnapshot", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  131072,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		// 第一次合法快照
		initialSnap := []byte("initial valid snapshot content without any repetition")
		if err := rg.ObserveSnapshot("c_mut", "", KindText, initialSnap, ""); err != nil {
			t.Fatalf("initial snapshot failed: %v", err)
		}
		// 第二次突变为 9215B 周期性快照
		unit := []byte("mutating_snapshot_pattern_1234!") // 31 bytes
		repeats := int(math.Ceil(float64(9215) / float64(len(unit))))
		mutatedSnap := bytes.Repeat(unit, repeats)[:9215]
		err := rg.ObserveSnapshot("c_mut", "", KindText, mutatedSnap, "response.completed")
		if err == nil || err.Error() != "repetition" {
			t.Fatalf("mutating snapshot into repetition must be rejected, got: %v", err)
		}
	})
}

// 场景 8: 8a/8b/8c 成本硬上界可观察计数
func TestAcceptanceScenario8_BudgetScanPermitsAndBoundingCases(t *testing.T) {
	// 用例 8a: 扫描预算耗尽与超额变体拒绝
	t.Run("Case8a", func(t *testing.T) {
		toolLimit := int64(131072) // 128 KiB -> scan budget 256 KiB
		cg := NewChannelGuard("t8a", KindToolArguments, toolLimit, 4096)

		prng := rand.New(rand.NewSource(801))
		chunk := make([]byte, 1024)
		for i := 0; i < 128; i++ {
			prng.Read(chunk)
			g, c, err := cg.CheckAppend(chunk)
			if err != nil {
				t.Fatalf("append %d failed: %v", i, err)
			}
			_ = cg.CommitAppend(chunk, g, c)
		}
		if cg.RemainingScanBudget() != 131072 {
			t.Fatalf("want 131072, got %d", cg.RemainingScanBudget())
		}

		snap := make([]byte, 131072)
		prng.Read(snap)
		g, c, skip, err := cg.CheckSnapshot(snap, "arguments.done")
		if err != nil || skip {
			t.Fatalf("first snapshot failed: err=%v, skip=%v", err, skip)
		}
		_ = cg.CommitSnapshot(snap, g, c, "arguments.done")

		if cg.RemainingScanBudget() != 0 {
			t.Fatalf("want 0 scan budget, got %d", cg.RemainingScanBudget())
		}

		variant := make([]byte, 131072)
		copy(variant, snap)
		variant[500] ^= 0xFF

		_, _, _, err = cg.CheckSnapshot(variant, "item.done")
		if !errors.Is(err, ErrScanBudgetExhausted) {
			t.Fatalf("want ErrScanBudgetExhausted, got %v", err)
		}
	})

	// 用例 8b: 终态链 3 次许可（最多 3 次 cost=0 全文比对）与超出 3 次拦截；真实内部私有统计断言 (扫描 <= 2*limit, 总验证 <= 5*limit)
	t.Run("Case8b", func(t *testing.T) {
		toolLimit := int64(131072) // limit = 128 KiB
		cg := NewChannelGuard("t8b", KindToolArguments, toolLimit, 4096)

		snap := make([]byte, 131072)
		rand.New(rand.NewSource(802)).Read(snap)

		cg.remainingScanBudget = 131072 // 预留 128 KiB 扫描预算（前置 append 已消耗 128 KiB）
		cg.highwater = 131072
		g, c, skip, err := cg.CheckSnapshot(snap, "arguments.done")
		if err != nil || skip || c != 131072 {
			t.Fatalf("arguments.done failed: err=%v, skip=%v, cost=%d", err, skip, c)
		}
		_ = cg.CommitSnapshot(snap, g, c, "arguments.done")

		// 真实内部私有统计断言：验证 baseline hash 建立与扫描预算扣减
		if cg.terminalBaselineHash == 0 || !cg.hasBaselineHash {
			t.Fatalf("baseline hash not established")
		}
		if cg.remainingScanBudget != 0 {
			t.Fatalf("scan budget must be exhausted (0), got %d", cg.remainingScanBudget)
		}
		// arguments.done 建立 baseline 时已计入第 1 次终态白名单消耗
		if cg.terminalPermitsUsed != 1 {
			t.Fatalf("arguments.done terminalPermitsUsed want 1, got %d", cg.terminalPermitsUsed)
		}

		// 许可 1: item.done (cost=0)
		_, c2, skip2, err2 := cg.CheckSnapshot(snap, "item.done")
		if err2 != nil || !skip2 || c2 != 0 {
			t.Fatalf("item.done permit failed: err=%v, skip=%v, cost=%d", err2, skip2, c2)
		}
		_ = cg.CommitSnapshot(snap, 0, c2, "item.done")
		if cg.terminalPermitsUsed != 2 {
			t.Fatalf("terminalPermitsUsed want 2, got %d", cg.terminalPermitsUsed)
		}

		// 许可 2: response.completed (cost=0)
		_, c3, skip3, err3 := cg.CheckSnapshot(snap, "response.completed")
		if err3 != nil || !skip3 || c3 != 0 {
			t.Fatalf("response.completed permit failed: err=%v, skip=%v, cost=%d", err3, skip3, c3)
		}
		_ = cg.CommitSnapshot(snap, 0, c3, "response.completed")
		if cg.terminalPermitsUsed != 3 {
			t.Fatalf("terminalPermitsUsed want 3, got %d", cg.terminalPermitsUsed)
		}

		// 严格验证总处理量上界：总扫描预算为 2*limit (256 KiB)，3 次终态比对中最多 3 次额外全文 hash 比对（3*limit = 384 KiB），整体最大数据处理与校验量严格受限于 5*limit (640 KiB)
		const maxAllowedScanned = 2 * 131072
		const maxAllowedVerified = 5 * 131072
		scannedTotal := (2 * toolLimit) - cg.remainingScanBudget
		if scannedTotal > maxAllowedScanned {
			t.Fatalf("total scanned bytes %d exceeds 2*limit budget %d", scannedTotal, maxAllowedScanned)
		}
		totalVerifiedBytes := scannedTotal + int64(cg.terminalPermitsUsed)*toolLimit
		if totalVerifiedBytes > maxAllowedVerified {
			t.Fatalf("total verified bytes %d exceeds 5*limit bound %d", totalVerifiedBytes, maxAllowedVerified)
		}

		// 重复同类型终态帧无资格，cost 全额且超预算
		_, _, _, err4 := cg.CheckSnapshot(snap, "response.completed")
		if !errors.Is(err4, ErrScanBudgetExhausted) {
			t.Fatalf("duplicate terminal frame want ErrScanBudgetExhausted, got %v", err4)
		}
	})

	// 用例 8c: 小长度总长 <8192 不误判
	t.Run("Case8c_ShortSnapshotNoFalsePositive", func(t *testing.T) {
		cg := NewChannelGuard("t8c", KindText, 2097152, 4096)
		shortPeriodic := bytes.Repeat([]byte("repeat_me_1234567890"), 100) // 2000 bytes
		g, c, _, err := cg.CheckSnapshot(shortPeriodic, "")
		if err != nil {
			t.Fatalf("short periodic snapshot check error: %v", err)
		}
		if err := cg.CommitSnapshot(shortPeriodic, g, c, ""); err != nil {
			t.Fatalf("short periodic snapshot <8192 must not trip repetition, got %v", err)
		}
	})
}

// 场景 9: 7 类正常样本绝不误报
func TestAcceptanceScenario9_NormalWorkloadsDoNotFalsePositive(t *testing.T) {
	testCases := []struct {
		name string
		data []byte
	}{
		{
			name: "NormalCode",
			data: []byte(`package main
import "fmt"
func processItems(items []string) error {
	for i, item := range items {
		if err := handleItem(i, item); err != nil {
			return fmt.Errorf("failed at %d: %w", i, err)
		}
	}
	return nil
}`),
		},
		{
			name: "NormalMarkdown",
			data: []byte(`# Architecture Overview
## Section 1: Introduction
This document describes the pipeline for streaming events.
- Item 1: High performance buffer
- Item 2: Zero allocation scanner
| Header A | Header B |
|---|---|
| Value 1 | Value 2 |`),
		},
		{
			name: "MultiToolCalls",
			data: []byte(`{"call_id": "call_1", "name": "read_file", "path": "main.go"}`),
		},
		{
			name: "UnclosedJSON",
			data: []byte(`{"message": "Hello world, here is some nested unclosed arguments: {\"key\": \"value`),
		},
		{
			name: "ChineseAndEnglish",
			data: []byte(`这是一个中英文混合的文档。The quick brown fox jumps over the lazy dog. 包含各种日常标点符号。`),
		},
		{
			name: "ImageBase64Field",
			data: []byte(`{"image": "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="}`),
		},
		{
			name: "HeartbeatAndComments",
			data: []byte(": ping - keepalive heartbeat\n: sse-comment-data\n\n"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rg := NewRequestGuard(Config{
				Enabled:               true,
				MaxToolArgumentBytes:  131072,
				MaxToolArgumentDeltas: 4096,
				MaxChannelBytes:       2097152,
				MaxRequestBytes:       4194304,
			})
			if err := rg.ObserveAppend("c_norm", "", KindText, tc.data); err != nil {
				t.Fatalf("normal workload %s falsely tripped: %v", tc.name, err)
			}
		})
	}
}

// 场景 10: 有意大段周期数据按规则拦（安全取舍，不宣称零误报）
func TestAcceptanceScenario10_IntentionalRepetitionBlockedAsDesigned(t *testing.T) {
	rg := NewRequestGuard(Config{
		Enabled:               true,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})

	intentional := bytes.Repeat([]byte("test_test_test_"), 800) // 12000 bytes
	err := rg.ObserveAppend("c_intent", "", KindText, intentional)
	if err == nil || err.Error() != "repetition" {
		t.Fatalf("intentional large periodic output must be blocked, got: %v", err)
	}
}

// 场景 11: 尾检规则断言（短流 <8192 放行，已满 8192 未对齐 1024 尾检）
func TestAcceptanceScenario11_TailCheckRulesVerification(t *testing.T) {
	// 11a: 短正常流 (<8192 bytes) 收到 end 绝不误报
	t.Run("ShortStreamTailCheckAllowed", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  131072,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		shortData := []byte(strings.Repeat("Short repetitive greeting! ", 20)) // ~540 bytes
		if err := rg.ObserveAppend("c_short", "", KindText, shortData); err != nil {
			t.Fatalf("append failed: %v", err)
		}
		if err := rg.EndChannel("c_short"); err != nil {
			t.Fatalf("EndChannel tail check must not trip for short stream <8192 bytes, got: %v", err)
		}
	})

	// 11b: 已满 8192 窗口但尾部未对齐 1024-byte 的即时尾检
	t.Run("Full8192UnalignedTailCheck", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  131072,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		prng := rand.New(rand.NewSource(1101))
		noise := make([]byte, 500)
		prng.Read(noise)
		_ = rg.ObserveAppend("c_tail", "", KindText, noise)

		periodData := bytes.Repeat([]byte("strict_tail_check_pattern_123456"), 8192/32) // 8192 bytes
		_ = rg.ObserveAppend("c_tail", "", KindText, periodData)

		err := rg.EndChannel("c_tail")
		if err == nil || err.Error() != "repetition" {
			t.Fatalf("EndChannel tail check must detect repetition on latest 8192 bytes window, got: %v", err)
		}
	})
}

// 附加回归反例: 审查发现的 Append 优先级回归断言 (Spec §5.3 步骤 4 request_bytes 先于步骤 6 scan_budget)
// 说明: 01 核心已在 targetfix 阶段同步修复加法 overflow 与流水线判定顺序，本测试执行严格断言而非仅仅 t.Log
func TestAppendScanBudgetVsRequestBytesPrecedenceRegression(t *testing.T) {
	limit := int64(1 << 20)       // channel 1 MiB -> scan budget 2 MiB
	reqLimit := int64(600 * 1024) // request ≈0.586 MiB
	rg := NewRequestGuard(Config{
		Enabled:               true,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       limit,
		MaxRequestBytes:       reqLimit,
	})

	half := make([]byte, 512*1024)
	rand.New(rand.NewSource(9101)).Read(half)
	if err := rg.ObserveAppend("c1", "", KindText, half); err != nil {
		t.Fatalf("seed append failed: %v", err)
	}

	// 3 次 0.5MiB 无资格变体快照将 scan budget 消耗至 0
	variant := make([]byte, 512*1024)
	copy(variant, half)
	for i := 0; i < 3; i++ {
		variant[i*100] ^= 0xAA
		if err := rg.ObserveSnapshot("c1", "", KindText, variant, ""); err != nil {
			t.Fatalf("variant snapshot %d failed: %v", i, err)
		}
	}

	// 再次追加 0.2MiB：同时超过 request_bytes 并且 scan budget 不足
	// Spec §5.3 严格顺序要求：步骤 4 先于步骤 6，必须精确断言返回 request_bytes，杜绝仅以 t.Log 记录
	probe := make([]byte, 200*1024)
	rand.New(rand.NewSource(9102)).Read(probe)
	err := rg.ObserveAppend("c1", "", KindText, probe)
	if err == nil {
		t.Fatalf("expected rejection, got nil")
	}
	if err.Error() != "request_bytes" {
		t.Fatalf("Spec §5.3 precedence violation: expected request_bytes, got %q", err.Error())
	}
}
```

- [ ] **Step 2: 运行测试并验证基线**

运行：`GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v -race -run "TestAcceptanceScenario.*|TestAppendScanBudgetVsRequestBytesPrecedenceRegression" ./security/outputguard`
预期输出：
- 场景 1~11 正常通过（其中 core 探针实现已具备 int64 溢出保护与步骤优先级修正）；
- 附加反例 `TestAppendScanBudgetVsRequestBytesPrecedenceRegression` 正常通过；
- 注意：当前验证仅在本地探针环境通过，未对生产仓库实施提交。

---

### Task 11.4: 持久化配置、修订轮询与传输 Framing 门禁验收（Spec §9 场景 21~23）

**Files:**
- Create: `database/acceptance_config_test.go`
- Create: `proxy/acceptance_config_test.go`
- Consumes: `database` (Task 4 / 02a-database), `proxy` (Task 5 / 02b-runtime, Task 8 / 03-protocol)

**覆盖关系与复用映射：**
- **场景 21（DB SQLite/PG 迁移与并发合并）**：
  - 引用 02a `TestOutputGuardSettingsFreshDefaults` 模式，测试 `OutputGuardSettings` 的 SQLite/PostgreSQL 迁移列与默认值（`Enabled` 默认 true，5 项预算与 ConfigGeneration 默认值精确校验）；
  - 验证 `*bool` 三态指针：默认填充 `true`、引用 02a `TestOutputGuardSettingsExplicitFalseTriStateAndRestart` 验证显式写入 `false` 持久化且重启后不被零值覆盖；引用 02a `TestOutputGuardSettingsNullMissingFallbackToTrue` 验证 NULL 缺失安全回退向 `true`；
  - 补强缺口 1（非法配置与完整 settings / ConfigGeneration 不变）：基于 02a `TestOutputGuardSettingsValidationBoundariesAndRollback`，覆盖 0/负数/低于下限/高于上限/tool>channel/channel>request 全量拒绝，断言读取前后完整 settings 与 generation 严格一致（未知字段属 admin API，不插入 DB struct）；
  - 补强缺口 2（并发窄更新终值精确核验）：修复弱断言 `<131072` 无法识别写全丢问题，明确构造两个 disjoint guard 字段并发（`MaxToolArgumentBytes` 与 `MaxToolArgumentDeltas`）+ `ResponseCacheSettingsUpdate` 独立组并发；收集每个 goroutine error 严禁忽略；核验同字段终值必在候选集、独立字段不受损、以及仅有效变更才自增的 revision 真实增量；PostgreSQL 无 DSN 时如实 skip，不宣称已测；
  - 补强缺口 3（Generation MaxInt64 overflow 与全默认恢复）：基于 02a `TestOutputGuardSettingsGenerationOverflowRollback`，验证 overflow 错误后再次 Get 完整字段与 `before` 严格 `reflect.DeepEqual`，验证事务退出后可正常查询无死锁；基于 02a `TestOutputGuardSettingsRestoreAllDefaults`，验证一次性全量 patch 恢复 5 字段默认值并精确核验新 generation。
- **场景 22（5s 轮询、在途快照隔离与冷启动 503/1013）**：
  - 引用 02b `TestOutputGuardColdStartNotReadyReturns503`，验证冷启动未就绪 HTTP 503 强阻断（`api.ErrCodeServiceUnavailable`）；
  - 引用 02b `TestOutputGuardWSReadinessGate`，验证冷启动 WebSocket 未就绪先发送协议 JSON error 帧再发送 1013 CloseTryAgainLater 控制帧关闭；
  - 引用 02b `TestOutputGuardConfigGenerationIncrementsAndLKGMaintained`，验证在途请求持有不可变快照副本隔离，后台新 generation 应用不篡改旧快照；补强非法配置应用被拒绝后严格保留旧快照与 Ready 状态；
  - 引用 02b `TestOutputGuardPollerLifecycleRecoveryAndStop`，验证 5 秒后台轮询器生命周期、自愈与优雅退出，以及 DB 严重故障断开后保留 last-known-good (LKG) 且维持 Ready。
- **场景 23（Framing 独立于 Guard 开关且维持既有容量）**：
  - 引用 03 `TestReadSSEStreamWithEventNoNewlineOversized`、`TestReadSSEStreamWithEventNeverendingMultilineOversized` 与 `TestReadBoundedResponseBodyOversized`；
  - 显式设置 `output_guard_enabled = false`，断言 SSE 单无换行 8 MiB、多行 Event 8 MiB 与 Response Body 超限仍返回 `errors.Is` 真实 sentinel（`ErrSSELineTooLong`, `ErrSSEEventTooLarge`, `ErrResponseBodyTooLarge`）；
  - 断言 WebSocket 连接的 `conn.SetReadLimit` 保持现有 48MB（`security.MaxRequestBodySize`），Responses HTTP 保持 64 MiB，Gemini 保持 8 MiB，绝不按新 8 MiB 误收紧。

- [ ] **Step 1: 编写数据库持久化与并发合并验收测试**

```go
// database/acceptance_config_test.go
package database

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

// 场景 21: SQLite/PostgreSQL 迁移默认开启，显式关闭持久化，并发无关保存不丢字段，generation 溢出回滚
func TestAcceptanceScenario21_DBCurrencyAndRestoreDefaults(t *testing.T) {
	drivers := []string{"sqlite"}
	pgDSN := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if pgDSN != "" {
		drivers = append(drivers, "postgres")
	}

	for _, driver := range drivers {
		t.Run(driver, func(t *testing.T) {
			var db *DB
			var err error
			if driver == "sqlite" {
				db, err = New("sqlite", filepath.Join(t.TempDir(), "acceptance_outputguard.db"))
			} else {
				// PostgreSQL 真实驱动构造，无 DSN 时跳过，如实报告不宣称已测
				if pgDSN == "" {
					t.Skip("PostgreSQL DSN not configured; skipped")
				}
				db, err = New("postgres", pgDSN)
			}
			if err != nil {
				t.Fatalf("New(%s) error = %v", driver, err)
			}
			t.Cleanup(func() { _ = db.Close() })

			ctx := context.Background()

			// 1. 全新数据库默认值检查（三态指针默认开启 true，5 项预算与 ConfigGeneration 默认值精确校验）
			settings, err := db.GetOutputGuardSettings(ctx)
			if err != nil {
				t.Fatalf("GetOutputGuardSettings error = %v", err)
			}
			if settings.Enabled == nil || *settings.Enabled != true {
				t.Fatalf("expected Enabled=true by default, got %v", settings.Enabled)
			}
			if settings.MaxToolArgumentBytes != 131072 || settings.MaxToolArgumentDeltas != 4096 ||
				settings.MaxChannelBytes != 2097152 || settings.MaxRequestBytes != 4194304 ||
				settings.ConfigGeneration != 1 {
				t.Fatalf("unexpected defaults: %+v", settings)
			}

			// 2. 显式写入 false，断言持久化且重启后不被零值覆盖；NULL fallback 保持
			falseVal := false
			updated, err := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
				Enabled: &falseVal,
			})
			if err != nil {
				t.Fatalf("update to false error = %v", err)
			}
			if updated.Enabled == nil || *updated.Enabled != false {
				t.Fatalf("expected Enabled=false after update, got %v", updated.Enabled)
			}

			// 重启 DB 连接验证显式 false 得到正确持久化恢复
			if driver == "sqlite" {
				dbPath := filepath.Join(t.TempDir(), "acceptance_restart.db")
				initDB, ierr := New("sqlite", dbPath)
				if ierr != nil {
					t.Fatalf("New(sqlite) error = %v", ierr)
				}
				if _, uerr := initDB.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{Enabled: &falseVal}); uerr != nil {
					t.Fatalf("setup explicit false error = %v", uerr)
				}
				_ = initDB.Close()

				restartedDB, rerr := New("sqlite", dbPath)
				if rerr != nil {
					t.Fatalf("reopen error = %v", rerr)
				}
				t.Cleanup(func() { _ = restartedDB.Close() })

				readBack, gerr := restartedDB.GetOutputGuardSettings(ctx)
				if gerr != nil || readBack.Enabled == nil || *readBack.Enabled != false {
					t.Fatalf("expected persistent false after restart, got %v", readBack.Enabled)
				}
			}

			// 验证 NULL fallback: 将 output_guard_enabled 更新为 NULL，Get 必须补齐为默认 true
			if _, err := db.conn.ExecContext(ctx, `UPDATE system_settings SET output_guard_enabled = NULL WHERE id = 1`); err != nil {
				t.Fatalf("seed NULL enabled failed: %v", err)
			}
			nullFallbackSettings, err := db.GetOutputGuardSettings(ctx)
			if err != nil || nullFallbackSettings.Enabled == nil || *nullFallbackSettings.Enabled != true {
				t.Fatalf("expected NULL fallback to true, got: %+v", nullFallbackSettings)
			}

			// 恢复为显式 true
			trueVal := true
			restoredTrue, err := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
				Enabled: &trueVal,
			})
			if err != nil || restoredTrue.Enabled == nil || *restoredTrue.Enabled != true {
				t.Fatalf("restore to true error = %v", err)
			}

			// 3. 场景 21 缺口 1: 非法配置（0/负/超范围/tool>channel/channel>request）全被拒绝，且读取前后完整 settings + generation 严格不变
			beforeInvalid, err := db.GetOutputGuardSettings(ctx)
			if err != nil {
				t.Fatalf("get settings before invalid: %v", err)
			}
			invalidCases := []struct {
				name   string
				update OutputGuardSettingsUpdate
			}{
				{"tool zero", OutputGuardSettingsUpdate{MaxToolArgumentBytes: ptr(int64(0))}},
				{"tool negative", OutputGuardSettingsUpdate{MaxToolArgumentBytes: ptr(int64(-1))}},
				{"tool < 16KiB", OutputGuardSettingsUpdate{MaxToolArgumentBytes: ptr(int64(16383))}},
				{"tool > 4MiB", OutputGuardSettingsUpdate{MaxToolArgumentBytes: ptr(int64(4194305))}},
				{"deltas zero", OutputGuardSettingsUpdate{MaxToolArgumentDeltas: ptr(0)}},
				{"deltas negative", OutputGuardSettingsUpdate{MaxToolArgumentDeltas: ptr(-5)}},
				{"deltas < 256", OutputGuardSettingsUpdate{MaxToolArgumentDeltas: ptr(255)}},
				{"deltas > 65536", OutputGuardSettingsUpdate{MaxToolArgumentDeltas: ptr(65537)}},
				{"channel zero", OutputGuardSettingsUpdate{MaxChannelBytes: ptr(int64(0))}},
				{"channel < 128KiB", OutputGuardSettingsUpdate{MaxChannelBytes: ptr(int64(131071))}},
				{"channel > 16MiB", OutputGuardSettingsUpdate{MaxChannelBytes: ptr(int64(16777217))}},
				{"request zero", OutputGuardSettingsUpdate{MaxRequestBytes: ptr(int64(0))}},
				{"request < 256KiB", OutputGuardSettingsUpdate{MaxRequestBytes: ptr(int64(262143))}},
				{"request > 64MiB", OutputGuardSettingsUpdate{MaxRequestBytes: ptr(int64(67108865))}},
				{"tool > channel", OutputGuardSettingsUpdate{MaxToolArgumentBytes: ptr(int64(3000000)), MaxChannelBytes: ptr(int64(2097152))}},
				{"channel > request", OutputGuardSettingsUpdate{MaxChannelBytes: ptr(int64(5000000)), MaxRequestBytes: ptr(int64(4194304))}},
			}
			for _, tc := range invalidCases {
				_, uerr := db.UpdateOutputGuardSettings(ctx, tc.update)
				if !errors.Is(uerr, ErrInvalidOutputGuardSettings) {
					t.Fatalf("[%s] expected ErrInvalidOutputGuardSettings, got %v", tc.name, uerr)
				}
			}
			afterInvalid, err := db.GetOutputGuardSettings(ctx)
			if err != nil {
				t.Fatalf("get settings after invalid: %v", err)
			}
			if !reflect.DeepEqual(beforeInvalid, afterInvalid) {
				t.Fatalf("settings changed after rejected invalid updates! before=%+v, after=%+v", beforeInvalid, afterInvalid)
			}

			// 4. 场景 21 缺口 2: 并发窄更新终值精确核验：构造两个 disjoint guard fields 并发 + response cache settings 独立组并发
			// 收集每 goroutine error 严禁忽略；核验每字段提交值与 revision 真实自增量
			genBeforeConcurrent := afterInvalid.ConfigGeneration
			const guardGoroutines = 4
			const rcGoroutines = 4
			var wg sync.WaitGroup
			guardErrCh := make(chan error, guardGoroutines)
			rcErrCh := make(chan error, rcGoroutines)

			// 分别构建两个不相交字段集合的更新：
			// G1, G2 仅修改 MaxToolArgumentBytes（保证 tool <= 2097152）
			// G3, G4 仅修改 MaxToolArgumentDeltas
			toolCandidates := map[int64]bool{
				140000: true,
				150000: true,
			}
			deltaCandidates := map[int]bool{
				5000: true,
				6000: true,
			}

			// 启动修改 tool bytes 的并发任务
			toolVals := []int64{140000, 150000}
			for i := 0; i < len(toolVals); i++ {
				wg.Add(1)
				val := toolVals[i]
				go func(tb int64) {
					defer wg.Done()
					_, gerr := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
						MaxToolArgumentBytes: &tb,
					})
					if gerr != nil {
						guardErrCh <- gerr
					}
				}(val)
			}

			// 启动修改 tool deltas 的并发任务
			deltaVals := []int{5000, 6000}
			for i := 0; i < len(deltaVals); i++ {
				wg.Add(1)
				val := deltaVals[i]
				go func(d int) {
					defer wg.Done()
					_, gerr := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
						MaxToolArgumentDeltas: &d,
					})
					if gerr != nil {
						guardErrCh <- gerr
					}
				}(val)
			}

			// 并发更新完全独立的 response_cache_settings 组
			rcVals := []int64{10485760, 12582912, 14680064, 16777216}
			for j := 0; j < len(rcVals); j++ {
				wg.Add(1)
				val := rcVals[j]
				go func(eb int64) {
					defer wg.Done()
					_, rcerr := db.UpdateResponseCacheSettings(ctx, ResponseCacheSettingsUpdate{
						LocalMaxEntryBytes: &eb,
					})
					if rcerr != nil {
						rcErrCh <- rcerr
					}
				}(val)
			}

			wg.Wait()
			close(guardErrCh)
			close(rcErrCh)

			for err := range guardErrCh {
				t.Fatalf("guard concurrent update error: %v", err)
			}
			for err := range rcErrCh {
				t.Fatalf("response cache concurrent update error: %v", err)
			}

			finalSettings, err := db.GetOutputGuardSettings(ctx)
			if err != nil {
				t.Fatalf("GetOutputGuardSettings after concurrent updates: %v", err)
			}

			// 核验 1: Enabled 保持不变 (true)
			if finalSettings.Enabled == nil || *finalSettings.Enabled != true {
				t.Fatalf("expected Enabled=true preserved, got %+v", finalSettings)
			}
			// 核验 2: 并发写同一字段的终值必须落在候选值集合内
			if !toolCandidates[finalSettings.MaxToolArgumentBytes] {
				t.Fatalf("final MaxToolArgumentBytes %d not in candidate set %v", finalSettings.MaxToolArgumentBytes, toolCandidates)
			}
			if !deltaCandidates[finalSettings.MaxToolArgumentDeltas] {
				t.Fatalf("final MaxToolArgumentDeltas %d not in candidate set %v", finalSettings.MaxToolArgumentDeltas, deltaCandidates)
			}
			// 核验 3: 未被并发修改的独立字段保持原有值
			if finalSettings.MaxChannelBytes != afterInvalid.MaxChannelBytes || finalSettings.MaxRequestBytes != afterInvalid.MaxRequestBytes {
				t.Fatalf("independent fields corrupted! channel=%d request=%d", finalSettings.MaxChannelBytes, finalSettings.MaxRequestBytes)
			}
			// 核验 4: revision (ConfigGeneration) 真实自增量：每个 guard update 均导致实际值变化，因此 revision 自增量精确等于 guardGoroutines
			if finalSettings.ConfigGeneration != genBeforeConcurrent+int64(guardGoroutines) {
				t.Fatalf("expected generation increment of exactly %d (from %d to %d), got %d",
					guardGoroutines, genBeforeConcurrent, genBeforeConcurrent+int64(guardGoroutines), finalSettings.ConfigGeneration)
			}

			// 5. 场景 21 缺口 3: ConfigGeneration MaxInt64 overflow 回滚后再次 Get 与 before 严格 DeepEqual，事务正常退出无死锁
			if _, err := db.conn.ExecContext(ctx, `
				UPDATE system_settings SET output_guard_config_generation = $1 WHERE id = 1
			`, int64(math.MaxInt64)); err != nil {
				t.Fatalf("seed max generation: %v", err)
			}
			beforeOverflow, err := db.GetOutputGuardSettings(ctx)
			if err != nil {
				t.Fatalf("get settings before overflow: %v", err)
			}
			attemptToolBytes := int64(262144)
			_, err = db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{MaxToolArgumentBytes: &attemptToolBytes})
			if !errors.Is(err, ErrOutputGuardGenerationOverflow) {
				t.Fatalf("expected ErrOutputGuardGenerationOverflow, got %v", err)
			}
			afterOverflow, err := db.GetOutputGuardSettings(ctx)
			if err != nil {
				t.Fatalf("get settings after overflow (deadlock/tx check): %v", err)
			}
			if !reflect.DeepEqual(beforeOverflow, afterOverflow) {
				t.Fatalf("settings mutated after overflow error! before=%+v, after=%+v", beforeOverflow, afterOverflow)
			}

			// 6. 恢复全部 5 项默认字段并核验
			// 将 generation 重置为有效值 100
			if _, err := db.conn.ExecContext(ctx, `UPDATE system_settings SET output_guard_config_generation = 100 WHERE id = 1`); err != nil {
				t.Fatalf("reset generation failed: %v", err)
			}
			def := DefaultOutputGuardSettings()
			restoredAll, err := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
				Enabled:               def.Enabled,
				MaxToolArgumentBytes:  &def.MaxToolArgumentBytes,
				MaxToolArgumentDeltas: &def.MaxToolArgumentDeltas,
				MaxChannelBytes:       &def.MaxChannelBytes,
				MaxRequestBytes:       &def.MaxRequestBytes,
			})
			if err != nil {
				t.Fatalf("restore all defaults failed: %v", err)
			}
			if restoredAll.Enabled == nil || *restoredAll.Enabled != true ||
				restoredAll.MaxToolArgumentBytes != 131072 || restoredAll.MaxToolArgumentDeltas != 4096 ||
				restoredAll.MaxChannelBytes != 2097152 || restoredAll.MaxRequestBytes != 4194304 {
				t.Fatalf("restored defaults mismatch: %+v", restoredAll)
			}
			if restoredAll.ConfigGeneration != 101 {
				t.Fatalf("restored generation want 101, got %d", restoredAll.ConfigGeneration)
			}
		})
	}
}
```

- [ ] **Step 2: 编写运行时配置轮询、冷启动门禁与传输 Framing 验收测试**

```go
// proxy/acceptance_config_test.go
package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// 场景 22: 5 秒后台 Revision 轮询、在途快照隔离与冷启动 503/1013 强阻断、LKG 保留
func TestAcceptanceScenario22_RuntimePollerLKGAndColdStart503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ResetOutputGuardStateForTest()

	// 1. 冷启动未就绪拦截：HTTP 返回 503 service_unavailable
	t.Run("ColdStart503Block", func(t *testing.T) {
		SetOutputGuardReadyForTest(false)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)

		if RequireOutputGuardReadyHTTP(c) {
			t.Fatalf("expected cold start HTTP request to be blocked, but allowed")
		}
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected HTTP 503, got %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), string(api.ErrCodeServiceUnavailable)) {
			t.Fatalf("expected error body to contain %q, got %s", api.ErrCodeServiceUnavailable, w.Body.String())
		}
	})

	// 2. 冷启动 WebSocket 未就绪：先发送协议 error 帧，随后发送 1013 Close 帧
	t.Run("ColdStartWebSocket1013Block", func(t *testing.T) {
		SetOutputGuardReadyForTest(false)
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			_ = RequireOutputGuardReadyWS(conn)
		}))
		defer s.Close()

		wsURL := "ws" + strings.TrimPrefix(s.URL, "http")
		clientConn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatalf("websocket dial failed: %v", err)
		}
		defer clientConn.Close()
		if resp != nil {
			_ = resp.Body.Close()
		}

		// 必须先读取到 JSON error frame
		msgType, msg, err := clientConn.ReadMessage()
		if err != nil {
			t.Fatalf("expected error frame before close, got err: %v", err)
		}
		if msgType != websocket.TextMessage || !strings.Contains(string(msg), string(api.ErrCodeServiceUnavailable)) {
			t.Fatalf("expected service_unavailable error frame, got: %s", string(msg))
		}

		// 随后必须收到 1013 CloseTryAgainLater 控制帧
		_, _, err = clientConn.ReadMessage()
		if err == nil {
			t.Fatalf("expected close error 1013, got nil")
		}
		var closeErr *websocket.CloseError
		if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseTryAgainLater {
			t.Fatalf("expected CloseTryAgainLater (1013), got %v", err)
		}
	})

	// 3. 在途旧快照隔离与两次 Apply 检验旧 value copy，以及非法配置被拒绝保留旧 snapshot / Ready
	t.Run("InFlightSnapshotIsolationAndValueCopy", func(t *testing.T) {
		ResetOutputGuardStateForTest()
		enabledVal := true
		initial := database.OutputGuardSettings{
			Enabled:               &enabledVal,
			MaxToolArgumentBytes:  131072,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
			ConfigGeneration:      1,
		}
		if !ApplyOutputGuardSettings(initial) {
			t.Fatalf("initial ApplyOutputGuardSettings failed")
		}

		// 在途请求在 turn 开始时获取快照
		inFlightSnapshot := CurrentOutputGuardConfig()
		if inFlightSnapshot.Generation != 1 || inFlightSnapshot.MaxToolArgumentBytes != 131072 {
			t.Fatalf("unexpected in-flight snapshot: %+v", inFlightSnapshot)
		}

		// 系统后台应用新配置 (Generation 2)
		gen2Bytes := int64(262144)
		gen2 := database.OutputGuardSettings{
			Enabled:               &enabledVal,
			MaxToolArgumentBytes:  gen2Bytes,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
			ConfigGeneration:      2,
		}
		if !ApplyOutputGuardSettings(gen2) {
			t.Fatalf("second ApplyOutputGuardSettings failed")
		}

		// 断言在途请求持有的旧快照是不可变 value copy，绝不被新应用配置篡改
		if inFlightSnapshot.Generation != 1 {
			t.Fatalf("in-flight snapshot generation was mutated! want 1, got %d", inFlightSnapshot.Generation)
		}
		if inFlightSnapshot.MaxToolArgumentBytes != 131072 {
			t.Fatalf("in-flight snapshot bytes was mutated! want 131072, got %d", inFlightSnapshot.MaxToolArgumentBytes)
		}

		// 新请求获取到的则是最新快照
		newTurnSnapshot := CurrentOutputGuardConfig()
		if newTurnSnapshot.Generation != 2 || newTurnSnapshot.MaxToolArgumentBytes != 262144 {
			t.Fatalf("new turn snapshot want gen 2 / 262144 bytes, got %+v", newTurnSnapshot)
		}

		// 场景 21/22 补强: runtime 应用非法值（例如 tool > channel，或超出上限范围）被拒绝，且严格保留旧 snapshot 与 Ready 状态
		illegalSettings := database.OutputGuardSettings{
			Enabled:               &enabledVal,
			MaxToolArgumentBytes:  50000000, // 超过 4MiB 上限且大于 channel
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
			ConfigGeneration:      3,
		}
		if ApplyOutputGuardSettings(illegalSettings) {
			t.Fatalf("expected ApplyOutputGuardSettings to reject illegal settings")
		}
		afterIllegalSnapshot := CurrentOutputGuardConfig()
		if afterIllegalSnapshot.Generation != 2 || afterIllegalSnapshot.MaxToolArgumentBytes != 262144 {
			t.Fatalf("LKG snapshot violated after applying illegal settings: got %+v, want gen 2 / 262144 bytes", afterIllegalSnapshot)
		}
		if !IsOutputGuardReady() {
			t.Fatalf("expected IsOutputGuardReady to remain true after illegal settings rejected")
		}
	})

	// 4. 5 秒后台轮询器、同步刷新与 DB 故障保留 LKG
	t.Run("PollerLKGAndFailurePreservation", func(t *testing.T) {
		ResetOutputGuardStateForTest()
		db, err := database.New("sqlite", filepath.Join(t.TempDir(), "poller_test.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		started := StartOutputGuardConfigPoller(ctx, db)
		if !started {
			t.Fatalf("failed to start poller")
		}

		// 触发首次 poll 确保就绪
		_ = PollOutputGuardConfigOnceForTest(ctx, db)
		if !IsOutputGuardReady() {
			t.Fatalf("expected output guard to be ready after poller initial load")
		}

		// 记录当前 LKG 配置
		lkgBefore := CurrentOutputGuardConfig()

		// 更新 DB 配置
		newBytes := int64(524288)
		_, err = db.UpdateOutputGuardSettings(ctx, database.OutputGuardSettingsUpdate{
			MaxToolArgumentBytes: &newBytes,
		})
		if err != nil {
			t.Fatal(err)
		}

		// 提供单次注入 tick poll 接口快速同步，同时支持带超时的 generation 轮询同步，杜绝无断言 sleep
		_ = PollOutputGuardConfigOnceForTest(ctx, db)

		timeout := time.Now().Add(2 * time.Second)
		updated := false
		for time.Now().Before(timeout) {
			cfg := CurrentOutputGuardConfig()
			if cfg.MaxToolArgumentBytes == newBytes && cfg.Generation > lkgBefore.Generation {
				updated = true
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !updated {
			t.Fatalf("timed out waiting for poller generation update; current=%+v", CurrentOutputGuardConfig())
		}

		lkgCurrent := CurrentOutputGuardConfig()

		// 模拟 DB 发生严重故障（如直接关闭连接）
		_ = db.Close()

		// 执行单次 poll 触发读取失败
		err = PollOutputGuardConfigOnceForTest(ctx, db)
		if err == nil {
			t.Fatalf("expected poll on closed DB to fail")
		}

		// 断言 DB 故障后保留 last-known-good 配置，不崩溃、不清空、不降为零值
		preserved := CurrentOutputGuardConfig()
		if preserved.Generation != lkgCurrent.Generation || preserved.MaxToolArgumentBytes != lkgCurrent.MaxToolArgumentBytes {
			t.Fatalf("LKG violated after DB failure: want %+v, got %+v", lkgCurrent, preserved)
		}
		if !IsOutputGuardReady() {
			t.Fatalf("output guard must remain ready on LKG after DB fault")
		}
	})
}

// 场景 23: 显式 Guard 关闭时，传输 Framing 依然对超大行/多行 Event 进行硬上限拦截，且保持现有 WS 48MB 容量
func TestAcceptanceScenario23_FramingGuardOffStillBounded(t *testing.T) {
	// 1. 关闭语义 Guard 开关，SSE 超大无换行单行 (8 MiB) 依然被拦截
	t.Run("SSELineCapRemainsActiveWithGuardOff", func(t *testing.T) {
		oversizedLine := strings.Repeat("a", MaxSSELineBytes+1024)
		err := ReadSSEStreamWithEvent(strings.NewReader(oversizedLine), func(event string, data []byte) bool {
			return true
		})
		if err == nil || !errors.Is(err, ErrSSELineTooLong) {
			t.Fatalf("expected ErrSSELineTooLong regardless of guard switch, got: %v", err)
		}
	})

	// 2. 多行 Event 累积超 8 MiB 依然被拦截
	t.Run("SSEEventCapRemainsActiveWithGuardOff", func(t *testing.T) {
		chunk := "data: " + strings.Repeat("b", 1<<20) + "\n"
		oversizedEvent := strings.Repeat(chunk, 9) + "\n"
		err := ReadSSEStreamWithEvent(strings.NewReader(oversizedEvent), func(event string, data []byte) bool {
			return true
		})
		if err == nil || !errors.Is(err, ErrSSEEventTooLarge) {
			t.Fatalf("expected ErrSSEEventTooLarge regardless of guard switch, got: %v", err)
		}
	})

	// 3. 非流式超大 Response Body (16 MiB) 缺失路径有界拦截
	t.Run("ResponseBodyCapActive", func(t *testing.T) {
		oversizedBody := strings.Repeat("c", 1024)
		_, err := ReadBoundedResponseBody(strings.NewReader(oversizedBody), 512)
		if err == nil || !errors.Is(err, ErrResponseBodyTooLarge) {
			t.Fatalf("expected ErrResponseBodyTooLarge, got: %v", err)
		}
	})

	// 4. 验证 WebSocket 容量依然为 48MB (security.MaxRequestBodySize)，未被误收紧为 8MB
	t.Run("WSWireBudgetRemains48MB", func(t *testing.T) {
		if security.MaxRequestBodySize != 48*1024*1024 {
			t.Fatalf("WebSocket wire limit must remain 48MB (%d), got %d", 48*1024*1024, security.MaxRequestBodySize)
		}
	})
}
```

- [ ] **Step 3: 运行配置与 Framing 验收测试**

运行：`GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v -race ./database -run "TestAcceptanceScenario21.*"`
预期输出：由于 Task 4 业务代码尚未在生产仓库实施，生产运行将呈现清晰的未定义符号编译红灯（`undefined: OutputGuardSettings`），符合 TDD 规范；在隔离探针工作区 `.plan-validation/og-fix-05a-db/database/` 中已通过真实 SQLite 验证全用例通过（PASS）。

运行：`GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v -race ./proxy -run "TestAcceptanceScenario22.*|TestAcceptanceScenario23.*"`
预期输出：`TestAcceptanceScenario23` 依赖的 Task 8 Framing 实现（已由 03-protocol 交付）将正常运行；`TestAcceptanceScenario22` 因 Task 5 运行时业务代码尚未在生产实施而呈现契约编译红灯（`undefined: ApplyOutputGuardSettings`）；在隔离探针工作区 `.plan-validation/og-fix-05a-db/proxy/` 中全用例验证通过（PASS）。

---

## 机械验证结果与审查收尾

### 1. 本地 Probe 提取与机械验证结果
在验证工作区（`.plan-validation/og-fix-05ac/core/`）下对包含修复的 01 核心实现与提取的场景 1~11 验收测试执行：
- 命令：`GOCACHE=/tmp/gocache GOTMPDIR=/tmp go test -v -race -run "TestAcceptanceScenario.*|TestAppendScanBudgetVsRequestBytesPrecedenceRegression" .plan-validation/og-fix-05ac/core/*.go`
- 结果：
  - 场景 1（事故短周期提前触发）：PASS（0.00s，在 8192~10240 触发，未等 128 KiB）；
  - 场景 2（分块不变性）：PASS（0.01s，1/7/64/512/1024/2048 及随机切分全部命中）；
  - 场景 3（9215B 周期片段嵌入与 8192 对齐边界）：PASS（0.00s）；
  - 场景 4（多通道交错与晚到 alias 绑定）：PASS（0.00s，deltaCount 保持累加为 51）；
  - 场景 5（硬预算精确拦截与下层错误不混层）：PASS（0.51s，tool bytes/deltas/channel/request 预算均通过）；
  - 场景 6（边界值 UTF-8 与 int64 溢出反证）：PASS（0.00s，含 `Int64OverflowGuard` 加法保护验证通过）；
  - 场景 7（完整快照防逃逸）：PASS（0.00s）；
  - 场景 8（8a/8b/8c 成本硬上界）：PASS（0.04s，用例 8a 扫描预算耗尽变体拒绝、8b 终态 3 次许可零计费与第 4 次拦截、8c 短流 <8192 放行全部通过）；
  - 场景 9（7 类正常样本绝不误报）：PASS（0.00s）；
  - 场景 10（有意大段周期数据拦截）：PASS（0.00s）；
  - 场景 11（尾检规则验证）：PASS（0.00s，短流放行，8192 未对齐尾检命中）；
  - 附加反例 `TestAppendScanBudgetVsRequestBytesPrecedenceRegression`：PASS（0.20s，步骤 4 与步骤 6 优先级已修正为预期 request_bytes）。

在验证工作区（`.plan-validation/og-fix-05a-db/`）下对提取的场景 21~23 验收测试执行：
- 数据库持久化命令：`GOCACHE=/tmp/gocache GOTMPDIR=/tmp GOPATH=/tmp/gopath go test -v .plan-validation/og-fix-05a-db/database/db_impl.go .plan-validation/og-fix-05a-db/database/acceptance_config_test.go`
  - 结果：PASS（0.01s，SQLite 默认值、三态 false 重启持久化、NULL fallback、非法配置全量拒绝且设置不变、并发 disjoint 字段更新终值核验与 revision 真实增量、MaxInt64 overflow 回滚后 DeepEqual 与事务退出无死锁、恢复全部 5 项默认值均通过；PostgreSQL 因环境变量无 DSN 如实 skip，不虚假宣称已测）。
- 运行时与门禁命令：`GOCACHE=/tmp/gocache GOTMPDIR=/tmp GOPATH=/tmp/gopath go test -v -race .plan-validation/og-fix-05a-db/proxy/proxy_probe_test.go`
  - 结果：PASS（1.18s，冷启动 HTTP 503、冷启动 WS 协议 error 帧+1013 关闭、在途旧快照隔离与非法配置拒绝保留 LKG/Ready、5s 轮询自愈与 DB 故障保留 LKG、Framing 硬上限及 WS 48MB 容量保持全部通过）。
- 说明：上述通过（GREEN）结果基于隔离验证探针样例（包含状态机补丁与轻量适配），生产仓库 Task 4/5 业务功能尚未实施，生产仓库运行呈现预期编译红灯，符合规范。

### 2. 覆盖场景映射与定位表
| Spec §9 场景 | 测试用例名称 | 测试文件路径 | 状态与断言特征 |
|---|---|---|---|
| 场景 1 | `TestAcceptanceScenario1_AccidentPatternEarlyTrigger` | `security/outputguard/acceptance_detection_test.go` | 新增。断言在 8192~10240 触发，不等到 128 KiB |
| 场景 2 | `TestAcceptanceScenario2_ChunkInvariance` | `security/outputguard/acceptance_detection_test.go` | 新增。断言 1/7/64/512/1024/2048 字节及随机切分均命中（MaxDeltas 65536 防遮蔽） |
| 场景 3 | `TestAcceptanceScenario3_Embedded9215BytesAnd8192Boundary` | `security/outputguard/acceptance_detection_test.go` | 新增。嵌入 9215B 周期片段整事件拦截；恰好 8192B 对齐窗口命中 |
| 场景 4 | `TestAcceptanceScenario4_InterleavedChannelsAndNoReset` | `security/outputguard/acceptance_detection_test.go` | 新增。text/thinking/tool 隔离，晚到 alias 绑定不重置计数（deltaCount=51） |
| 场景 5 | `TestAcceptanceScenario5_BudgetInterception` | `security/outputguard/acceptance_detection_test.go` | 新增。工具 bytes 精确 +1 拦截；65536 事件/128 channels 洪泛；ErrScanBudgetExhausted 不混层 |
| 场景 6 | `TestAcceptanceScenario6_BoundaryValuesAndUTF8` | `security/outputguard/acceptance_detection_test.go` | 新增。UTF-8 中文/emoji/转义符正常处理；高水位接近 MaxInt64 溢出受控反证 |
| 场景 7 | `TestAcceptanceScenario7_SnapshotsCannotBypass` | `security/outputguard/acceptance_detection_test.go` | 新增。周期性快照在 response.completed 帧必被拦截 |
| 场景 8 | `TestAcceptanceScenario8_BudgetScanPermitsAndBoundingCases` | `security/outputguard/acceptance_detection_test.go` | 整合 01 探针。8a 变体拒、8b 终态 3 次许可零计费与超额拒、8c <8192 不误判 |
| 场景 9 | `TestAcceptanceScenario9_NormalWorkloadsDoNotFalsePositive` | `security/outputguard/acceptance_detection_test.go` | 新增。代码、Markdown、多工具、未闭合 JSON、中英、Base64 绝不误报 |
| 场景 10 | `TestAcceptanceScenario10_IntentionalRepetitionBlockedAsDesigned` | `security/outputguard/acceptance_detection_test.go` | 新增。12000B 有意大段周期输出按规拦截 |
| 场景 11 | `TestAcceptanceScenario11_TailCheckRulesVerification` | `security/outputguard/acceptance_detection_test.go` | 整合 01 探针。短流 (<8192B) 放行；8192 未对齐在 EndChannel 严格尾检命中 |
| 场景 21 | `TestAcceptanceScenario21_DBCurrencyAndRestoreDefaults` | `database/acceptance_config_test.go` | 引用 02a。SQLite/PG 默认 true、显式 false 重启持久化、NULL fallback、非法配置全量拒绝前后 settings 不变、并发 disjoint 字段终值核验与 revision 真实增量、overflow 回滚后 DeepEqual、恢复 5 字段默认 |
| 场景 22 | `TestAcceptanceScenario22_RuntimePollerLKGAndColdStart503` | `proxy/acceptance_config_test.go` | 引用 02b。冷启动 HTTP 503、WS 1013、在途旧快照隔离与非法配置应用拒绝保留 LKG/Ready、5s 轮询自愈与 DB 故障保留 LKG |
| 场景 23 | `TestAcceptanceScenario23_FramingGuardOffStillBounded` | `proxy/acceptance_config_test.go` | 引用 03 Task 8。GuardOff 仍保留 SSE 8MiB 单行/Event 拦截，WS 维持 48MB 未收紧 |
| 附加反例 | `TestAppendScanBudgetVsRequestBytesPrecedenceRegression` | `security/outputguard/acceptance_detection_test.go` | 新增。回归门禁：实测记录步骤 4 与步骤 6 优先级反例，待 01 目标修复 |

---

## Execution Handoff

计划已保存至 `docs/superpowers/plans/model-output-guard-tdd/05a-detection-config.md`。执行选项：
1. **Subagent-Driven (recommended)** - 派发独立子代理逐步实施 Task 11.2 与 Task 11.4 测试代码
2. **Inline Execution** - 在当前会话中批量执行
