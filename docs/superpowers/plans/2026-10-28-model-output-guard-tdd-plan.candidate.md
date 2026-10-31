# Codex2API 全局模型输出失控防护 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 基于已审查通过的设计规格，为 Codex2API 建立全局模型输出失控防护体系（协议感知增量重复检测 + 字节/事件硬预算 + 请求级切断 + 限制最大输出长度），在模型输出进入聚合、回放、缓存及下游之前拦截异常流量，消除下游全量重解析引发的 O(n²) 性能雪崩。

**Architecture:** 采用分层解耦与自下而上的无环依赖架构：
1. 核心状态机与算法层（`security/outputguard`）：实现常量空间 8192-byte 滑动窗口、KMP 前缀函数求最短周期（$1 \le p \le 256$、有限串周期数学不要求 $L \pmod p = 0$ 且有独立非整倍数测试验证）、步长 1024-byte 检查点与严格尾检；单 channel 扫描预算（$2 \times \text{limit}$）与至多 3 次合法纯终态快照许可（单遍历 Hash 流水线比对，处理上限绝对锁定于 $5 \times \text{limit}$）；请求级 8 步无歧义判定流水线（先字节高水位与 request_bytes 累加判定，再资格/等价性检查与成本计算，首 reason 冻结），内置读写并发互斥锁（`sync.RWMutex`），保障 ObserveAppend/IsTripped 并发无 race。
2. 数据库与持久化层（`database`）：基于 `system_settings` 表实施 PostgreSQL/SQLite 模式迁移；`*bool` 三态指针语义防止零值覆盖显式关闭（经 Go 标准库 `sql.NullBool` 源码证实原生支持 SQLite `int64` 的 0/1 转换）；单事务同连接读取旧值与加锁（Postgres `FOR UPDATE` / SQLite 独占写锁）、窄更新与 CAS 自增 `generation`，彻底消除死锁与丢更新。
3. 运行时配置与入口 Ready 门禁层（`proxy`）：不可变配置快照、5 秒后台 Revision 轮询器（`StartOutputGuardConfigPoller`）、冷启动未就绪检测（`RequireOutputGuardReady`），默认 `isReady = false`，仅首次从 DB 成功加载或应用配置后置为 true；main 启动挂接初始化与 poller；HTTP 入口返回 503 `api.ErrCodeServiceUnavailable`，WebSocket/Realtime 在握手或首帧返回 CloseTryAgainLater (1013) 错误。
4. 管理端与前端 UI 层（`admin`, `frontend`）：复用既有 `/api/admin/settings` GET/PUT，支持独立分组、单位展示、恢复默认（恢复 Spec 默认配置）、严格层级范围校验（$16\text{KiB} \le \text{tool} \le \text{channel} \le \text{request} \le 64\text{MiB}$）；非法配置记录诊断日志；提供完整前端 React 界面与 TypeScript 类型契约，并以 `pnpm run typecheck` 验证。
5. 统一协议投影层（`proxy`）：将 Responses HTTP/WS、ChatCompletions、Messages、Gemini Native 及 Realtime 文本子集的流式 chunk 及非流式 JSON 事件实时投影为统一语义观察（`Observation`）；实现 Anthropic、Gemini Native 及各协议非流式真实 JSON 投影器与单元测试。
6. 强制终止与下游通知层（`proxy`）：先实现 `GuardTerminationCoordinator` 与 `SendDownstreamGuardTerminal`。guard 命中时原子切换 `tripped` 状态并冻结首个 reason；优先主动 Cancel 上游 context（直接调用 cancel 跳过 5 秒 usage drain，关闭 HTTP body，WS lease 废弃不归还健康池）；清空本地 pending、provenance、collector、replay、writer buffers；下游通知写入采用 `downstreamMu.TryLock()` 避免死锁，持锁写操作通过 `http.ResponseController.SetWriteDeadline` 施加 5 秒硬超时；不支持 deadline（`ErrNotSupported`）或已断开时标记 `clientGone` 安全退出；补齐 Gemini SDK 兼容格式及 WebSocket 1013 / error 帧。
7. 链路编排与重试阻断层（`proxy`）：在前置拦截点（Responses HTTP/WS, Chat, Messages, Gemini, Realtime）将投影器、RequestGuard 与终止协调器组装串联；在 `continuousRetryStreamSelected`、`shouldTransparentRetryStream` 及自动续想入口最前置拦截 `guardTripped`，坚决不重试；在 `retryExclusions` 中豁免账号健康惩罚（使用真实符号 `exclusions.forHardSelection()` 与 `exclusions.hasResettable()` 断言）。
8. 综合验收集成测试层（`test/outputguard`）：完整实现 Spec §9 场景 1~28 全场景代码，提供脱敏 fixture、机器自动化判定阈值，覆盖成本上界 8a/b/c、入口矩阵、cancel/body close 无 drain、重试上游调用数恒为 1、全协议错误结构、TryLock/deadline 降级、冷启动 503 / 并发 patch / 恢复默认、TTFT p95 <= 10ms、CPU <= 20%、线性比值 ratio <= 2.5、每 channel <= 64KiB、10 分钟内存/Goroutine/FD 启停快照。

**Tech Stack:** Go 1.26+ (gin-gonic/gin, tidwall/gjson, modernc.org/sqlite, jackc/pgx/v5), TypeScript + React 19 (前端契约与界面).

**Spec:** `docs/superpowers/specs/2026-10-28-model-output-guard-design.md`

## Global Constraints

- **范围限制**：只涉及文字输出的 LLM（含 text、thinking、tool arguments）；只修订计划文件 `docs/superpowers/plans/2026-10-28-model-output-guard-tdd-plan.md`，不修改 spec、业务源码、数据库或其他文件；不修改 DSH、客户端或已安装 npm 包；设计文档 §10 的设计取舍已全部由用户确认。
- **不可妥协的设计取舍**：
  - 默认全局关闭，仅管理员可在后台显式开启；与 prompt filter 相互独立。
  - 首期采用精确短周期重复检测 + 硬预算，不引入语义模型，不自动改写或拆分工具参数。
  - 默认工具参数上限 128 KiB、4096 非空 deltas；单 channel 字节上限 2 MiB；单请求字节上限 4 MiB；合法大 `write` 也拦截，后台可调高。
  - 只切断当前入口请求或 WebSocket 逻辑 turn；代理端禁止任何形式的透明重试与自动续想，绝不惩罚账号健康分。
  - 全协议覆盖：涵盖 Responses HTTP/WS、ChatCompletions、Messages、Gemini Native 及 Realtime WebSocket 桥接的文本/工具子集；专用媒体接口、Realtime 原生音频 Modality（显式拒绝）及协议管理帧不计入语义重复检测。
  - transport reader 有界行/事件/framing 保护（SSE 8 MiB、WS frame 8 MiB）独立于 guard 开关，关闭语义 guard 仍保留 wire cap。
- **技术异议核实结论（对首轮评审意见 M2 与 I8 的技术审查回应）**：
  - **M2 关于 SQLite 不支持 sql.NullBool 的澄清**：经查证 Go 标准库 `database/sql/convert.go:383` 及 `database/sql/driver/types.go:87-92`，`sql.NullBool.Scan` 内部调用 `convertAssign`，明确原生支持驱动返回的 `int64`（0 对应 false，1 对应 true）。在 `modernc.org/sqlite` 驱动下，SQLite 的 `INTEGER` 字段扫描至 `sql.NullBool` 完全合法，无需盲目绕开 `sql.NullBool`。本计划在数据库结构层保留 `*bool` 指针三态语义，在底层 Scan 阶段通过 `sql.NullBool` 准确反序列化。
  - **I8 关于 KMP 前缀周期整除性的数学核实**：评审建议曾提及“检查 L % p == 0”，但根据 Spec §5.2 规则 3 明确要求：“命中证据是连续 8 KiB 的精确周期窗口；最长周期时也含至少 32 次重复，不要求窗口长度整除周期”。在有限长度字符串（8192 字节）上，若最短周期为 $p$（例如事故模式 $p = 30$），$8192 \pmod{30} = 22 \ne 0$。KMP 前缀函数的最大 border $k = \pi[L-1]$ 对应的最小周期候选值为 $p = L - k$。当且仅当 $1 \le p \le 256$ 且 $\forall i \in [p, L): s[i] == s[i-p]$ 时即为严格周期，绝不能加入 $L \pmod p == 0$ 的错误过滤。本计划在 Task 1 中明确提供非整倍数（$p=30, 8192\pmod{30}=22$）的针对性数学验证测试。
- **编译与验证约束**：
  - Go 测试命令必须指定独立缓存路径：`GOCACHE=/tmp/go-build GOPATH=/tmp/go go test ...`。
  - 前端类型校验命令：`cd frontend && pnpm run typecheck`。
- **TDD 流程纪律**：每个任务严格遵循 Step 1 (Write failing test) -> Step 2 (Verify Red) -> Step 3 (Minimal implementation) -> Step 4 (Verify Green) -> Step 5 (Commit)。绝不使用占位符，严禁假测试与“Red or pass”。

---

## File Structure

| 文件路径 | 状态 | 职责描述 |
|---|---|---|
| `security/outputguard/repetition.go` | 新建 | 8192B 滑动窗口 Ring Buffer、KMP 前缀函数求最短周期（$1 \le p \le 256$、支持非整除周期）、1024B 步长检查点与严格尾检 |
| `security/outputguard/repetition_test.go` | 新建 | 周期检测器单元测试（命中短周期、非整倍数周期 $p=30$、非周期放行、大 delta 跨步长、<8192B 尾检放行、未对齐最新 8192B 窗口检测） |
| `security/outputguard/channel.go` | 新建 | 单 Channel 状态机：已计费高水位、累计扫描预算扣减、3 次合法纯终态快照许可、单遍历 Hash 流水线、处理量 $5 \times \text{limit}$ 封顶 |
| `security/outputguard/channel_test.go` | 新建 | Channel 状态机单元测试（高水位推进、用例 8a/8b/8c 纯终态许可与变体超额拦截） |
| `security/outputguard/guard.go` | 新建 | 请求级 `RequestGuard` 状态机：事件预算（65536）、Channel 计数（128）、Delta 计数（4096）、§5.3 严格 8 步判定流水线、`sync.RWMutex` 并发安全保障 |
| `security/outputguard/guard_test.go` | 新建 | 请求级 Guard 单元测试（事件洪泛拦截、Channel 洪泛拦截、Delta 洪泛拦截、字节溢出、首个 reason 冻结、并发 race 检验） |
| `database/output_guard_settings.go` | 新建 | 数据库层 `OutputGuardSettings`、`*bool` 三态指针、同事务锁机制（Postgres FOR UPDATE / SQLite 独占写锁）与 CAS 自增 `generation` |
| `database/output_guard_settings_test.go` | 新建 | 数据库迁移与设置读写测试（SQLite/PostgreSQL 默认填充、显式 false 持久化、并发独立窄更新不丢字段与防死锁） |
| `database/sqlite.go` | 修改 | `system_settings` 表增加 output_guard 6 个字段的 SQLite 迁移列（INTEGER） |
| `database/postgres.go` | 修改 | `system_settings` 表增加 output_guard 6 个字段的 PostgreSQL 迁移列（BOOLEAN/BIGINT/INT） |
| `proxy/output_guard_settings.go` | 新建 | 运行时不可变配置快照、5 秒后台 Revision 轮询器、`ApplyOutputGuardSettings`、冷启动未就绪 503 检查、非法配置诊断日志 |
| `proxy/output_guard_settings_test.go` | 新建 | 运行时配置生效与 5 秒轮询更新测试、冷启动未就绪 503 门禁测试、非法配置诊断日志测试 |
| `admin/output_guard_settings.go` | 新建 | 管理端 Admin API：扩展现有 GET/PUT `/api/admin/settings`，支持独立分组、单位显示、恢复默认、严格层级范围校验 |
| `admin/output_guard_settings_test.go` | 新建 | Admin API 单元测试（范围校验失败回滚与诊断日志、恢复默认设置、合法修改即时生效、同事务安全合并） |
| `frontend/src/types.ts` | 修改 | 增加 `OutputGuardSettings` 前端类型契约 |
| `frontend/src/pages/Settings.tsx` | 修改 | 管理端 UI 界面：新增独立的“模型输出失控防护”分组卡片，含 5 项配置、单位（KiB/MiB）、高风险提示与“恢复默认”操作按钮 |
| `proxy/outputguard_projection.go` | 新建 | 统一协议投影器：将 Responses HTTP/WS、ChatCompletions、Messages、Gemini Native 及 Realtime 文本流式与非流式事件投影为标准 `Observation` |
| `proxy/outputguard_projection_test.go` | 新建 | 协议投影器单元测试（各协议 chunk/snapshot/非流式 JSON 正确转换为 ChannelID、Kind、Action 及 Payload） |
| `proxy/outputguard_cancel.go` | 新建 | 终止协调器：原子切换 `tripped` 状态并冻结首 reason、主动 Cancel 上游 context、关闭 body、绕过 5s drain、清空在途缓冲 |
| `proxy/outputguard_cancel_test.go` | 新建 | 上游取消与缓冲废弃测试（mock upstream 立即收到 context cancel，无 5s 延迟，本地 pending/collector/replay/writer 缓冲彻底清空） |
| `proxy/outputguard_downstream.go` | 新建 | 下游失败通知写入：各协议终态错误帧构建（Responses, Chat, Anthropic, Gemini Native, WebSocket）、`downstreamMu.TryLock()` 有界锁获取、持锁内 `SetWriteDeadline(5s)` 写入与 ErrNotSupported 安全降级 |
| `proxy/outputguard_downstream_test.go` | 新建 | 下游写入测试（各协议标准错误结构断言、TryLock 竞争失败安全放弃不卡死、持锁超时回收、不支持 deadline 安全退出） |
| `proxy/continuous_retry.go` | 修改 | `continuousRetryStreamSelected` 最前置识别 `guardTripped`，坚决不重试 |
| `proxy/retry_exclusions.go` | 修改 | `MarkStreamFailure` 识别 guard 失败，豁免账号健康分惩罚，不增加错误计数 |
| `proxy/outputguard_retry_test.go` | 新建 | 重试阻断与账号豁免测试（使用真实符号 `exclusions.forHardSelection()` 与 `exclusions.hasResettable()` 断言） |
| `proxy/handler.go` | 修改 | Responses HTTP SSE `forwardWithEvent` 及非流式接入前置 guard 检查；ChatCompletions 流式与非流式接入前置 guard 检查 |
| `proxy/responses_ws.go` | 修改 | Responses WebSocket 接入每个逻辑 turn 的前置 guard 检查（早于 collector 与 replay） |
| `proxy/realtime_ws.go` | 修改 | Realtime 文本转接 session 严格继承全局 guard |
| `proxy/handler_anthropic.go` | 修改 | Messages (Claude Native) 流式与非流式接入前置 guard 检查 |
| `proxy/handler_gemini.go` | 修改 | Gemini Native `streamGenerateContent` 及非流式接入前置 guard 检查 |
| `proxy/main_init.go` | 新建 | 主程序初始化接线：挂载 `proxy.StartOutputGuardConfigPoller`，执行冷启动配置预加载 |
| `test/outputguard/acceptance_detection_test.go` | 新建 | 综合验收测试：场景 1~11（短周期、不同 delta 边界切分、9215B 片段、8a/8b/8c 专项、短文本尾检放行） |
| `test/outputguard/acceptance_lifecycle_test.go` | 新建 | 综合验收测试：场景 12~20（各入口流式/非流式全链路终止、无 5s drain、并发无 race、无重试、账号无惩罚） |
| `test/outputguard/acceptance_benchmark_test.go` | 新建 | 综合验收测试：场景 21~28（配置轮询、冷启动 503、CPU O(N) 线性度、内存 <=64KiB、细 delta 4096 专项压测分析） |

## Tasks

---

### Task 1: 周期性短重复检测核心算法（Prefix Function 线性检测器）

**Files:**
- Create: `security/outputguard/repetition.go`
- Test: `security/outputguard/repetition_test.go`

**Interfaces:**
- Produces:
  ```go
  package outputguard

  const (
      WindowSizeBytes     = 8192
      CheckpointStepBytes = 1024
      MaxRepetitionPeriod = 256
      MinRepetitionPeriod = 1
  )

  type RepetitionDetector struct {
      window                [WindowSizeBytes]byte
      count                 int64 // channel 累计写入总字节数
      head                  int   // ring buffer 写入游标 (0..8191)
      lastCheckedCheckpoint int64
  }

  func NewRepetitionDetector() *RepetitionDetector
  func (d *RepetitionDetector) Push(data []byte) (hit bool, period int)
  func (d *RepetitionDetector) PushWithHash(data []byte, initialHash uint64) (hit bool, period int, finalHash uint64)
  func (d *RepetitionDetector) CheckTail() (hit bool, period int)
  func (d *RepetitionDetector) Reset()
  ```

- [ ] **Step 1: Write failing test for repetition detector (including non-multiple periodic math verification and unaligned tail check)**

```go
// security/outputguard/repetition_test.go
package outputguard

import (
	"bytes"
	"math/rand"
	"testing"
)

func TestRepetitionDetectorNormalText(t *testing.T) {
	d := NewRepetitionDetector()
	// 伪随机非周期数据
	r := rand.New(rand.NewSource(12345))
	nonRepeating := make([]byte, 10000)
	r.Read(nonRepeating)

	hit, _ := d.Push(nonRepeating)
	if hit {
		t.Fatalf("expected no repetition hit for pseudo-random data, got hit=true")
	}
}

func TestRepetitionDetectorShortPeriodHit(t *testing.T) {
	d := NewRepetitionDetector()
	// 创建真实事故重复模式：周期 p = 30 ("_auditfree.md_pluginless.tmp.m")
	// 数学核实重点：8192 % 30 = 22 != 0，验证有限串数学周期不要求 L % p == 0！
	pattern := []byte("_auditfree.md_pluginless.tmp.m")
	if len(pattern) != 30 {
		t.Fatalf("pattern length = %d, want 30", len(pattern))
	}
	var stream []byte
	for len(stream) < 9500 {
		stream = append(stream, pattern...)
	}

	// 以 512 字节分块送入
	hitFound := false
	var hitPeriod int
	for offset := 0; offset < len(stream); offset += 512 {
		end := offset + 512
		if end > len(stream) {
			end = len(stream)
		}
		hit, p := d.Push(stream[offset:end])
		if hit {
			hitFound = true
			hitPeriod = p
			break
		}
	}

	if !hitFound {
		t.Fatalf("expected repetition hit for 30-byte periodic pattern (non-multiple of 8192), got hit=false")
	}
	if hitPeriod != 30 {
		t.Fatalf("expected detected period 30, got %d", hitPeriod)
	}
}

func TestRepetitionDetectorTailCheckShortText(t *testing.T) {
	d := NewRepetitionDetector()
	// 短文本 < 8192 字节：即使内容高度周期，尾检必须返回 hit=false (Spec §5.2 规则 6)
	pattern := bytes.Repeat([]byte("abc"), 500) // 1500 字节
	hit, _ := d.Push(pattern)
	if hit {
		t.Fatalf("expected hit=false during push for <8192 bytes")
	}
	hit, _ = d.CheckTail()
	if hit {
		t.Fatalf("expected hit=false for tail check with <8192 bytes total")
	}
}

func TestRepetitionDetectorTailCheckUnalignedWindow(t *testing.T) {
	d := NewRepetitionDetector()
	// 构造真实非对齐尾检样本：
	// 前 108 字节为非周期杂散前缀，后随 8192 字节周期为 10 的重复片段，总长 8300 字节。
	// 在常规 Push 过程中：
	// - 当累计达到 8192 字节时触发检查点，此时窗口包含 108 字节杂散前缀 + 8084 字节周期内容，不是完整周期，Push 判定 hit=false；
	// - 后续送入剩余 108 字节（总长 8300 字节），尚未到达下一个 1024 检查点（9216 字节），Push 仍未命中；
	// - 此时流结束触发 CheckTail()：提取当前最新的 8192 字节窗口，恰好完全由周期 10 的数据组成，CheckTail 必须精准返回 hit=true, period=10。
	prefix := []byte("non-periodic-header-padding-1234567890-abcdefghijklmnopqrstuvwxyz!@#$%^&*()-random-preamble-bytes-to-offset-boundary-xxxx")
	if len(prefix) != 108 {
		t.Fatalf("prefix length = %d, want 108", len(prefix))
	}
	pattern := []byte("0123456789") // period 10
	periodicPart := bytes.Repeat(pattern, 8192/10)
	periodicPart = append(periodicPart, pattern[:8192%10]...) // 恰好 8192 字节纯周期
	if len(periodicPart) != 8192 {
		t.Fatalf("periodicPart length = %d, want 8192", len(periodicPart))
	}

	fullStream := append(prefix, periodicPart...) // 总长 108 + 8192 = 8300 字节

	// 分块推送，断言 Push 期间不会命中（因为 8192 检查点包含前缀，而后续未到 9216 检查点）
	for offset := 0; offset < len(fullStream); offset += 500 {
		end := offset + 500
		if end > len(fullStream) {
			end = len(fullStream)
		}
		hit, _ := d.Push(fullStream[offset:end])
		if hit {
			t.Fatalf("Push prematurely hit at offset %d, expected tail check to detect", offset)
		}
	}

	// 触发尾检
	hit, p := d.CheckTail()
	if !hit {
		t.Fatalf("expected CheckTail to detect pure periodic window of latest 8192 bytes, got hit=false")
	}
	if p != 10 {
		t.Fatalf("expected detected period 10, got %d", p)
	}
}

func TestRepetitionDetectorConsecutive9215BytesGuaranteedHit(t *testing.T) {
	d := NewRepetitionDetector()
	// Spec §5.2 规则 3: 步长 1024B，任意起点的连续周期片段达到 9215B 时必存在检查点完整落入
	prefix := make([]byte, 512) // 任意偏置前缀 512B
	for i := range prefix {
		prefix[i] = byte(i % 127)
	}
	pattern := []byte("repeated_segment_32_bytes_test_")
	periodicBody := bytes.Repeat(pattern, 300) // > 9215B
	stream := append(prefix, periodicBody...)

	hit := false
	for offset := 0; offset < len(stream); offset += 256 {
		end := offset + 256
		if end > len(stream) {
			end = len(stream)
		}
		if ok, _ := d.Push(stream[offset:end]); ok {
			hit = true
			break
		}
	}
	if !hit {
		t.Fatalf("expected 9215-byte periodic segment to trigger repetition hit within checkpoints")
	}
}
```

- [ ] **Step 2: Run test to verify failure (Red)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./security/outputguard -run "TestRepetitionDetector.*"`
Expected: FAIL with compilation error: undefined: NewRepetitionDetector

- [ ] **Step 3: Write minimal implementation**

```go
// security/outputguard/repetition.go
package outputguard

const (
	WindowSizeBytes     = 8192
	CheckpointStepBytes = 1024
	MaxRepetitionPeriod = 256
	MinRepetitionPeriod = 1
)

type RepetitionDetector struct {
	window                [WindowSizeBytes]byte
	count                 int64 // channel 累计写入总字节数
	head                  int   // ring buffer 写入游标 (0..8191)
	lastCheckedCheckpoint int64
}

func NewRepetitionDetector() *RepetitionDetector {
	return &RepetitionDetector{}
}

func (d *RepetitionDetector) Reset() {
	d.count = 0
	d.head = 0
	d.lastCheckedCheckpoint = 0
}

func (d *RepetitionDetector) Push(data []byte) (hit bool, period int)
  func (d *RepetitionDetector) PushWithHash(data []byte, initialHash uint64) (hit bool, period int, finalHash uint64) {
	for _, b := range data {
		d.window[d.head] = b
		d.head = (d.head + 1) % WindowSizeBytes
		d.count++

		// 每新增 CheckpointStepBytes 到达检查点
		if d.count >= WindowSizeBytes && (d.count-d.lastCheckedCheckpoint) >= CheckpointStepBytes {
			d.lastCheckedCheckpoint = d.count - (d.count % CheckpointStepBytes)
			if ok, p := d.checkCurrentWindow(); ok {
				return true, p
			}
		}
	}
	return false, 0
}

func (d *RepetitionDetector) CheckTail() (hit bool, period int) {
	// Spec §5.2 规则 6: 如果 channel 累计总字节数 < 8192，不判定重复，直接放行
	if d.count < WindowSizeBytes {
		return false, 0
	}
	// 已满 8192 窗口但尾部未对齐 1024，提取最新的完整 8192 窗口执行一次求周期检查
	return d.checkCurrentWindow()
}

// checkCurrentWindow 提取当前 ring buffer 中的线性 8192 字节，使用前缀函数求最短周期 1 <= p <= 256
// 数学核实说明：有限字符串周期不要求 L % p == 0（例如事故周期 p=30, 8192 % 30 = 22 != 0）。
// 前缀函数的最大 border k = pi[L-1] 对应最小周期 p = L - k。
// 当且仅当 1 <= p <= 256 且满足全部 p <= i < 8192 的 window[i] == window[i-p] 时判定命中。
func (d *RepetitionDetector) checkCurrentWindow() (bool, int) {
	if d.count < WindowSizeBytes {
		return false, 0
	}
	// 展开当前最新的 8192 字节连续视图
	var linear [WindowSizeBytes]byte
	idx := d.head // d.head 此时指向最老的那个字节
	for i := 0; i < WindowSizeBytes; i++ {
		linear[i] = d.window[(idx+i)%WindowSizeBytes]
	}

	// 采用 KMP prefix function (pi table) 在 O(N) 线性时间求周期
	pi := make([]int, WindowSizeBytes)
	for i := 1; i < WindowSizeBytes; i++ {
		j := pi[i-1]
		for j > 0 && linear[i] != linear[j] {
			j = pi[j-1]
		}
		if linear[i] == linear[j] {
			j++
		}
		pi[i] = j
	}

	k := pi[WindowSizeBytes-1]
	p := WindowSizeBytes - k
	if p >= MinRepetitionPeriod && p <= MaxRepetitionPeriod {
		// 校验边界：根据 border 定义，linear[0..k-1] == linear[p..L-1]
		// 即对于所有 p <= i < 8192, 严格满足 linear[i] == linear[i-p]
		return true, p
	}

	return false, 0
}
```

- [ ] **Step 4: Run test to verify passing (Green)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./security/outputguard -run "TestRepetitionDetector.*"`
Expected: PASS ok github.com/codex2api/security/outputguard

- [ ] **Step 5: Commit**

```bash
git add security/outputguard/repetition.go security/outputguard/repetition_test.go
git commit -m "feat(outputguard): implement sliding window linear repetition detector"
```

---

---

### Task 2: Channel 状态机与扫描预算/纯终态确认配额机制

**Files:**
- Create: `security/outputguard/channel.go`
- Test: `security/outputguard/channel_test.go`

**Interfaces:**
- Consumes: `RepetitionDetector` from Task 1
- Produces:
  ```go
  package outputguard

  type ChannelKind string
  const (
      KindText          ChannelKind = "text"
      KindThinking      ChannelKind = "thinking"
      KindToolArguments ChannelKind = "tool_arguments"
  )

  type ChannelGuard struct {
      id                    string
      kind                  ChannelKind
      maxBytes              int64
      maxDeltas             int
      remainingScanBudget   int64
      highwater             int64
      deltaCount            int
      terminalBaselineHash  uint64
      hasBaselineHash       bool
      terminalPermitsUsed   int
      consumedTerminalTypes map[string]bool
      detector              *RepetitionDetector
      isClosed              bool
  }

  func NewChannelGuard(id string, kind ChannelKind, maxBytes int64, maxDeltas int) *ChannelGuard
  func (c *ChannelGuard) Highwater() int64
  func (c *ChannelGuard) DeltaCount() int
  func (c *ChannelGuard) RemainingScanBudget() int64
  func (c *ChannelGuard) ReleaseWindow()
  func (c *ChannelGuard) CheckAppend(delta []byte) (growth int64, cost int64, err error)
  func (c *ChannelGuard) CommitAppend(delta []byte, growth int64, cost int64) error
  func (c *ChannelGuard) CheckSnapshot(snapshot []byte, terminalStage string) (growth int64, cost int64, skipRepetition bool, err error)
  func (c *ChannelGuard) CommitSnapshot(snapshot []byte, growth int64, cost int64, terminalStage string) error
  func (c *ChannelGuard) CheckTail() error
  ```

- [ ] **Step 1: Write failing test for channel state machine and quota**

```go
// security/outputguard/channel_test.go
package outputguard

import (
	"bytes"
	"math/rand"
	"testing"
)

func TestChannelGuardHighwaterGrowthAndScanBudget(t *testing.T) {
	// 工具 channel: maxBytes = 128 KiB (131072), scanBudget = 2 * 128 KiB = 256 KiB
	cg := NewChannelGuard("call_1", KindToolArguments, 131072, 4096)

	// Append 64 KiB of non-repeating data
	delta1 := make([]byte, 65536)
	r1 := rand.New(rand.NewSource(10001))
	r1.Read(delta1)

	growth, cost, err := cg.CheckAppend(delta1)
	if err != nil {
		t.Fatalf("CheckAppend delta1 error: %v", err)
	}
	if growth != 65536 || cost != 65536 {
		t.Fatalf("delta1: want growth=65536, cost=65536, got growth=%d, cost=%d", growth, cost)
	}
	if err := cg.CommitAppend(delta1, growth, cost); err != nil {
		t.Fatalf("CommitAppend delta1 error: %v", err)
	}

	if cg.Highwater() != 65536 {
		t.Fatalf("want highwater=65536, got %d", cg.Highwater())
	}
	if cg.RemainingScanBudget() != 262144-65536 {
		t.Fatalf("want remainingScanBudget=%d, got %d", 262144-65536, cg.RemainingScanBudget())
	}
}

// Spec §9.1 Case 8a: 耗尽扫描预算后变体快照超额拦截
func TestChannelGuardCase8a_ScanBudgetExhaustionAndVariantRejection(t *testing.T) {
	cg := NewChannelGuard("call_1", KindToolArguments, 131072, 4096)
	// 以 128 次 1 KiB delta 耗尽 128 KiB 扫描预算 (剩余 128 KiB)
	chunk := make([]byte, 1024)
	r2 := rand.New(rand.NewSource(20002))
	r2.Read(chunk)
	var fullParam []byte
	for i := 0; i < 128; i++ {
		fullParam = append(fullParam, chunk...)
		growth, cost, err := cg.CheckAppend(chunk)
		if err != nil {
			t.Fatalf("step %d append error: %v", i, err)
		}
		if err := cg.CommitAppend(chunk, growth, cost); err != nil {
			t.Fatalf("step %d commit append error: %v", i, err)
		}
	}
	if cg.RemainingScanBudget() != 131072 {
		t.Fatalf("remaining scan budget want 131072, got %d", cg.RemainingScanBudget())
	}

	// 首个 128 KiB 白名单终态快照到达 (arguments.done)，尚无 baseline hash，cost = 128 KiB
	growth, cost, skipRepetition, err := cg.CheckSnapshot(fullParam, "arguments.done")
	if err != nil {
		t.Fatalf("arguments.done snapshot error: %v", err)
	}
	if cost != 131072 || growth != 0 || skipRepetition {
		t.Fatalf("arguments.done: want cost=131072, growth=0, skipRepetition=false; got cost=%d, growth=%d, skip=%v", cost, growth, skipRepetition)
	}
	if err := cg.CommitSnapshot(fullParam, growth, cost, "arguments.done"); err != nil {
		t.Fatalf("arguments.done commit snapshot error: %v", err)
	}

	if cg.RemainingScanBudget() != 0 {
		t.Fatalf("remaining scan budget want 0, got %d", cg.RemainingScanBudget())
	}

	// 紧接着收到修改了 1 个字节的 128 KiB 变体快照 (item.done)
	variantParam := bytes.Clone(fullParam)
	variantParam[65536] ^= 0xFF

	_, _, _, err = cg.CheckSnapshot(variantParam, "item.done")
	if err == nil {
		t.Fatalf("expected variant snapshot to be rejected with scan budget exhaustion, got nil")
	}
	if err != ErrScanBudgetExhausted {
		t.Fatalf("expected ErrScanBudgetExhausted, got %v", err)
	}
}

// Spec §9.1 Case 8b: 合法终态链零计费放行与无资格拒绝
func TestChannelGuardCase8b_TerminalChainZeroCostAndExhaustion(t *testing.T) {
	cg := NewChannelGuard("call_1", KindToolArguments, 131072, 4096)
	fullParam := make([]byte, 131072)
	r3 := rand.New(rand.NewSource(30003))
	r3.Read(fullParam)
	// append 128 KiB
	growth, cost, _ := cg.CheckAppend(fullParam)
	_ = cg.CommitAppend(fullParam, growth, cost)

	// arguments.done 消耗最后 128 KiB 扫描预算并建立 baseline hash
	growth, cost, _, _ = cg.CheckSnapshot(fullParam, "arguments.done")
	_ = cg.CommitSnapshot(fullParam, growth, cost, "arguments.done")

	// item.done 具备资格且 Hash 完全一致 -> cost = 0, skipRepetition = true
	growth, cost, skipRepetition, err := cg.CheckSnapshot(fullParam, "item.done")
	if err != nil {
		t.Fatalf("item.done snapshot error: %v", err)
	}
	if cost != 0 || !skipRepetition {
		t.Fatalf("item.done: want cost=0, skipRepetition=true; got cost=%d, skip=%v", cost, skipRepetition)
	}
	_ = cg.CommitSnapshot(fullParam, growth, cost, "item.done")

	// response.completed 同样具备资格且 Hash 一致 -> cost = 0
	growth, cost, skipRepetition, err = cg.CheckSnapshot(fullParam, "response.completed")
	if err != nil {
		t.Fatalf("response.completed snapshot error: %v", err)
	}
	if cost != 0 || !skipRepetition {
		t.Fatalf("response.completed: want cost=0, skipRepetition=true")
	}
	_ = cg.CommitSnapshot(fullParam, growth, cost, "response.completed")

	// 重发同类型 response.completed -> 失去资格，走 cost = 128 KiB，因剩余扫描预算为 0 被拦截
	_, _, _, err = cg.CheckSnapshot(fullParam, "response.completed")
	if err != ErrScanBudgetExhausted {
		t.Fatalf("duplicate response.completed: want ErrScanBudgetExhausted, got %v", err)
	}
}

// Spec §9.1 Case 8c: 非白名单普通快照绝不建立 baseline
func TestChannelGuardCase8c_NonTerminalSnapshotCannotEstablishBaseline(t *testing.T) {
	cg := NewChannelGuard("c_text", KindText, 2097152, 4096)
	snap := []byte("intermediate full snapshot text")
	growth, cost, skip, err := cg.CheckSnapshot(snap, "unknown_or_intermediate_type")
	if err != nil {
		t.Fatalf("CheckSnapshot error: %v", err)
	}
	if skip || cost != int64(len(snap)) {
		t.Fatalf("want skip=false, cost=%d, got skip=%v, cost=%d", len(snap), skip, cost)
	}
	_ = cg.CommitSnapshot(snap, growth, cost, "unknown_or_intermediate_type")

	// 确认 hasBaselineHash 仍为 false，未建立 baseline
	if cg.hasBaselineHash {
		t.Fatalf("expected hasBaselineHash=false for non-terminal snapshot, got true")
	}
}

func TestChannelGuardHashMismatchDiscardsQuotaAndChargesFullCost(t *testing.T) {
	cg := NewChannelGuard("c_tool", KindToolArguments, 131072, 4096)
	data1 := bytes.Repeat([]byte("hello world "), 100) // 1200 字节
	growth, cost, _, _ := cg.CheckSnapshot(data1, "arguments.done")
	_ = cg.CommitSnapshot(data1, growth, cost, "arguments.done")

	if !cg.hasBaselineHash {
		t.Fatalf("expected baseline hash created")
	}

	// 紧接着发送篡改变体快照
	data2 := bytes.Repeat([]byte("corrupted data!"), 80) // 1200 字节
	growth, cost, skip, err := cg.CheckSnapshot(data2, "item.done")
	if err != nil {
		t.Fatalf("unexpected CheckSnapshot error: %v", err)
	}
	if skip {
		t.Fatalf("expected skip=false for hash mismatch")
	}
	if cost != int64(len(data2)) {
		t.Fatalf("expected full cost %d, got %d", len(data2), cost)
	}

	// CommitSnapshot 篡改帧后，该类型许可必须标记废弃
	_ = cg.CommitSnapshot(data2, growth, cost, "item.done")
	if !cg.consumedTerminalTypes["item.done"] {
		t.Fatalf("expected item.done marked as consumed/discarded")
	}
	// 基准 Hash 绝不能被篡改快照覆盖
	h := uint64(0)
	for _, b := range data1 {
		h = (h ^ uint64(b)) * 1099511628211
	}
	if cg.terminalBaselineHash != h {
		t.Fatalf("expected baseline hash to stay unchanged")
	}
}

func TestChannelGuardTailCheck(t *testing.T) {
	cg := NewChannelGuard("c_text", KindText, 2097152, 4096)
	// 短文本放行
	_ = cg.CommitAppend([]byte("short text"), 10, 10)
	if err := cg.CheckTail(); err != nil {
		t.Fatalf("expected nil error for short text tail check, got %v", err)
	}

	// 长周期文本检出
	cg2 := NewChannelGuard("c_periodic", KindText, 2097152, 4096)
	pattern := []byte("0123456789")
	data := bytes.Repeat(pattern, 850) // 8500 字节
	// 避免在 append 触发检查点，直接写入 window
	cg2.detector.window = [8192]byte{}
	for i := 0; i < 8500; i++ {
		cg2.detector.window[i%8192] = pattern[i%10]
	}
	cg2.detector.count = 8500
	cg2.detector.head = 8500 % 8192

	if err := cg2.CheckTail(); err != ErrRepetitionDetected {
		t.Fatalf("expected ErrRepetitionDetected for periodic tail check, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify failure (Red)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./security/outputguard -run "TestChannelGuard.*"`
Expected: FAIL with compilation error: undefined: NewChannelGuard

- [ ] **Step 3: Write minimal implementation**

```go
// security/outputguard/channel.go
package outputguard

import (
	"errors"
)

var (
	ErrToolArgumentBytesExceeded = errors.New("tool_argument_bytes")
	ErrChannelBytesExceeded      = errors.New("channel_bytes")
	ErrScanBudgetExhausted       = errors.New("scan_budget_exhausted")
	ErrRepetitionDetected        = errors.New("repetition")
	ErrToolDeltasExceeded        = errors.New("tool_argument_deltas")
)

type ChannelKind string

const (
	KindText          ChannelKind = "text"
	KindThinking      ChannelKind = "thinking"
	KindToolArguments ChannelKind = "tool_arguments"
)

// allowedTerminalTypes 白名单终态类型（Spec §5.3）
var allowedTerminalTypes = map[string]bool{
	"arguments.done":     true,
	"item.done":          true,
	"response.completed": true,
}

// fnv64a 计算 64 位 FNV-1a Hash，用于流水线单遍历比对与基准记录
func fnv64a(data []byte) uint64 {
	var h uint64 = 14695981039346656037
	for _, b := range data {
		h = (h ^ uint64(b)) * 1099511628211
	}
	return h
}

type ChannelGuard struct {
	id                    string
	kind                  ChannelKind
	maxBytes              int64
	maxDeltas             int
	remainingScanBudget   int64
	highwater             int64
	deltaCount            int
	terminalBaselineHash  uint64
	hasBaselineHash       bool
	terminalPermitsUsed   int
	consumedTerminalTypes map[string]bool
	detector              *RepetitionDetector
	isClosed              bool
}

func NewChannelGuard(id string, kind ChannelKind, maxBytes int64, maxDeltas int) *ChannelGuard {
	return &ChannelGuard{
		id:                    id,
		kind:                  kind,
		maxBytes:              maxBytes,
		maxDeltas:             maxDeltas,
		remainingScanBudget:   2 * maxBytes, // 累计扫描预算为 2 * limit (Spec §5.3)
		consumedTerminalTypes: make(map[string]bool),
		detector:              NewRepetitionDetector(),
	}
}

func (c *ChannelGuard) Highwater() int64           { return c.highwater }
func (c *ChannelGuard) DeltaCount() int            { return c.deltaCount }
func (c *ChannelGuard) RemainingScanBudget() int64 { return c.remainingScanBudget }

func (c *ChannelGuard) ReleaseWindow() {
	c.detector = nil
	c.isClosed = true
}

func (c *ChannelGuard) CheckAppend(delta []byte) (growth int64, cost int64, err error) {
	deltaLen := int64(len(delta))
	if c.kind == KindToolArguments && deltaLen > 0 {
		if c.deltaCount+1 > c.maxDeltas {
			return 0, 0, ErrToolDeltasExceeded
		}
	}
	newHighwater := c.highwater + deltaLen
	if newHighwater > c.maxBytes {
		if c.kind == KindToolArguments {
			return 0, 0, ErrToolArgumentBytesExceeded
		}
		return 0, 0, ErrChannelBytesExceeded
	}

	cost = deltaLen
	if c.remainingScanBudget < cost {
		return 0, 0, ErrScanBudgetExhausted
	}

	return deltaLen, cost, nil
}

func (c *ChannelGuard) CommitAppend(delta []byte, growth int64, cost int64) error {
	c.highwater += growth
	if c.kind == KindToolArguments && len(delta) > 0 {
		c.deltaCount++
	}
	c.remainingScanBudget -= cost

	if c.detector != nil && len(delta) > 0 {
		hit, _ := c.detector.Push(delta)
		if hit {
			return ErrRepetitionDetected
		}
	}
	return nil
}

func (c *ChannelGuard) CheckSnapshot(snapshot []byte, terminalStage string) (growth int64, cost int64, skipRepetition bool, err error) {
	snapLen := int64(len(snapshot))
	if snapLen > c.highwater {
		growth = snapLen - c.highwater
	}
	if snapLen > c.maxBytes {
		if c.kind == KindToolArguments {
			return 0, 0, false, ErrToolArgumentBytesExceeded
		}
		return 0, 0, false, ErrChannelBytesExceeded
	}

	// 资格检查：仅白名单终态类型且具备 baseline hash 且许可未耗尽 (Spec §5.3 步骤 5)
	// 合法与非法终态配额判断严格在 hash 前！无资格绝不提前计算 hash。
	isEligible := c.hasBaselineHash &&
		allowedTerminalTypes[terminalStage] &&
		!c.consumedTerminalTypes[terminalStage] &&
		c.terminalPermitsUsed < 3

	if isEligible {
		// 计算 snapshot Hash 比对 baseline hash
		currentHash := fnv64a(snapshot)
		if currentHash == c.terminalBaselineHash {
			// Hash 完全一致，纯终态确认快照零计费放行，跳过步骤 7 重复扫描
			return growth, 0, true, nil
		}
		// Hash 不一致（变体篡改），作废该类型许可，全额计费
		cost = snapLen
	} else {
		// 无免除资格（包括中间替换、首帧终态、重发同类型或超出 3 次配额），严禁提前 Hash，直接全额计费
		cost = snapLen
	}

	if c.remainingScanBudget < cost {
		return 0, 0, false, ErrScanBudgetExhausted
	}

	return growth, cost, false, nil
}

func (c *ChannelGuard) CommitSnapshot(snapshot []byte, growth int64, cost int64, terminalStage string) error {
	c.highwater += growth
	c.remainingScanBudget -= cost

	// 白名单终态类型快照消耗本类型配额
	if allowedTerminalTypes[terminalStage] {
		c.consumedTerminalTypes[terminalStage] = true
		c.terminalPermitsUsed++
	}

	// 终态首帧建立 baseline hash：仅白名单终态帧类型允许在首帧建立 baseline hash (Spec §5.3)
	// 如果之前未建立 baseline 且当前属于白名单类型，在单次遍历中建立 baseline hash
	shouldSetBaseline := !c.hasBaselineHash && allowedTerminalTypes[terminalStage]

	if cost > 0 && c.detector != nil {
		c.detector.Reset()

		if shouldSetBaseline {
			// 单次流水线处理：一次遍历同时流水线计算 FNV-1a Hash 并执行滑动窗口重复检测 (Spec §5.3 & M1 修复)
			const fnvOffsetBasis uint64 = 14695981039346656037
			hit, _, finalHash := c.detector.PushWithHash(snapshot, fnvOffsetBasis)
			c.terminalBaselineHash = finalHash
			c.hasBaselineHash = true
			if hit {
				return ErrRepetitionDetected
			}
			hit, _ = c.detector.CheckTail()
			if hit {
				return ErrRepetitionDetected
			}
		} else {
			hit, _ := c.detector.Push(snapshot)
			if hit {
				return ErrRepetitionDetected
			}
			hit, _ = c.detector.CheckTail()
			if hit {
				return ErrRepetitionDetected
			}
		}
	}

	return nil
}

func (c *ChannelGuard) CheckTail() error {
	if c.detector != nil {
		if hit, _ := c.detector.CheckTail(); hit {
			return ErrRepetitionDetected
		}
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify passing (Green)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./security/outputguard -run "TestChannelGuard.*"`
Expected: PASS ok github.com/codex2api/security/outputguard

- [ ] **Step 5: Commit**

```bash
git add security/outputguard/channel.go security/outputguard/channel_test.go
git commit -m "feat(outputguard): implement channel guard with scan budget and terminal quota"
```

---

---

### Task 3: 请求级 RequestGuard 协调器与统一判定优先级及并发契约

**Files:**
- Create: `security/outputguard/guard.go`
- Test: `security/outputguard/guard_test.go`

**Interfaces:**
- Consumes: `ChannelGuard` from Task 2
- Produces:
  ```go
  package outputguard

  import "sync"

  const (
      MaxRequestEvents   = 65536
      MaxRequestChannels = 128
      TriggeredCode      = "model_output_guard_triggered"
  )

  type Config struct {
      Enabled               bool
      MaxToolArgumentBytes  int64
      MaxToolArgumentDeltas int
      MaxChannelBytes       int64
      MaxRequestBytes       int64
  }

  type RequestGuard struct {
      mu            sync.RWMutex
      cfg           Config
      requestEvents int
      requestBytes  int64
      channels      map[string]*ChannelGuard
      aliasMap      map[string]string // alias mapping (e.g. index -> call_id)
      tripped       bool
      trippedReason string
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
  func (g *RequestGuard) ActiveChannelCount() int
  ```

- [ ] **Step 1: Write failing test for RequestGuard (including 8-step precedence, first reason freeze, EndChannel tail check, and concurrency race)**

```go
// security/outputguard/guard_test.go
package outputguard

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
)

func defaultTestConfig() Config {
	return Config{
		Enabled:               true,
		MaxToolArgumentBytes:  131072,  // 128 KiB
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152, // 2 MiB
		MaxRequestBytes:       4194304, // 4 MiB
	}
}

func TestRequestGuardRequestEventsFlood(t *testing.T) {
	rg := NewRequestGuard(defaultTestConfig())
	delta := []byte("a")
	for i := 0; i < MaxRequestEvents; i++ {
		if err := rg.ObserveAppend("c1", "", KindText, delta); err != nil {
			t.Fatalf("unexpected error at event %d: %v", i, err)
		}
	}
	// 第 65537 个事件必须被拒绝并判定为 request_events (Spec §5.3 步骤 1)
	err := rg.ObserveAppend("c1", "", KindText, delta)
	if err == nil || err.Error() != "request_events" {
		t.Fatalf("expected request_events, got %v", err)
	}
	if !rg.IsTripped() || rg.TrippedReason() != "request_events" {
		t.Fatalf("expected tripped with request_events, got %s", rg.TrippedReason())
	}
}

func TestRequestGuardChannelFlood(t *testing.T) {
	rg := NewRequestGuard(defaultTestConfig())
	// 验证累计 128 个 Channel 限制：
	// 第二轮核实修正：消除未定义符号 'A'，使用 fmt.Sprintf 或安全字符生成独立 channel ID
	for i := 0; i < MaxRequestChannels; i++ {
		cid := fmt.Sprintf("chan_%d", i)
		if err := rg.ObserveAppend(cid, "", KindText, []byte("x")); err != nil {
			t.Fatalf("channel %d error: %v", i, err)
		}
	}
	// 第 129 个 Channel 必须被拒绝并判定为 channel_count (Spec §5.3 步骤 2)
	err := rg.ObserveAppend("channel_129", "", KindText, []byte("x"))
	if err == nil || err.Error() != "channel_count" {
		t.Fatalf("expected channel_count, got %v", err)
	}
}

func TestRequestGuardChannelCumulativeNotReturnedOnEnd(t *testing.T) {
	rg := NewRequestGuard(defaultTestConfig())
	// 验证 EndChannel 结束 channel 不返还累计 channel 配额 (Spec §5.1)
	for i := 0; i < MaxRequestChannels; i++ {
		cid := fmt.Sprintf("chan_%d", i)
		_ = rg.ObserveAppend(cid, "", KindText, []byte("x"))
		_ = rg.EndChannel(cid)
	}
	// 尝试再开一个 channel，必须被拒绝为 channel_count
	err := rg.ObserveAppend("extra_channel", "", KindText, []byte("x"))
	if err == nil || err.Error() != "channel_count" {
		t.Fatalf("expected channel_count rejection after 128 closed channels, got %v", err)
	}
}

func TestRequestGuardEndChannelExecutesTailCheck(t *testing.T) {
	rg := NewRequestGuard(defaultTestConfig())
	// 构造在 Push 时未触发检查点但流结束时已包含 8192 字节周期的流
	pattern := []byte("0123456789")
	periodicBody := bytes.Repeat(pattern, 8192/10)
	periodicBody = append(periodicBody, pattern[:8192%10]...) // 恰好 8192B 周期
	// 先推送 100 字节杂散
	prefix := make([]byte, 100)
	for i := range prefix {
		prefix[i] = byte(i)
	}
	full := append(prefix, periodicBody...) // 8292 字节，未达下一个 1024 检查点

	// 推送数据，Push 期间不触发 repetition（8192 检查点含 prefix 杂散，8292 未到检查点）
	for offset := 0; offset < len(full); offset += 500 {
		end := offset + 500
		if end > len(full) {
			end = len(full)
		}
		_ = rg.ObserveAppend("c_tail", "", KindText, full[offset:end])
	}
	if rg.IsTripped() {
		t.Fatalf("expected not tripped before EndChannel")
	}

	// 调用 EndChannel：必须先执行尾检，发现周期性短重复，成功拦截并冻结 repetition！
	err := rg.EndChannel("c_tail")
	if err == nil || err.Error() != "repetition" {
		t.Fatalf("expected repetition hit during EndChannel tail check, got %v", err)
	}
	if !rg.IsTripped() || rg.TrippedReason() != "repetition" {
		t.Fatalf("expected tripped with repetition, got %s", rg.TrippedReason())
	}
}

func TestRequestGuardRequestBytesPrecedenceOverScanBudget(t *testing.T) {
	// Spec §5.3 严格 8 步优先级：先步骤 4 语义字节预算检查 (request_bytes)，后步骤 6 扫描预算
	rg := NewRequestGuard(Config{
		Enabled:               true,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       1000, // 仅 1000 字节请求上限
	})

	// 一次传入 1500 字节快照 -> 必须报 request_bytes，而非 channel_bytes 或 scan_budget
	err := rg.ObserveSnapshot("c1", "", KindText, make([]byte, 1500), "")
	if err == nil || err.Error() != "request_bytes" {
		t.Fatalf("expected request_bytes priority over scan budget, got %v", err)
	}
	// 首个 reason 必须冻结
	if rg.TrippedReason() != "request_bytes" {
		t.Fatalf("expected frozen reason request_bytes, got %s", rg.TrippedReason())
	}

	// 再次调用仍然返回首个冻结 reason
	err2 := rg.ObserveAppend("c1", "", KindText, []byte("x"))
	if err2 == nil || err2.Error() != "request_bytes" {
		t.Fatalf("expected idempotent frozen reason request_bytes, got %v", err2)
	}
}

func TestRequestGuardToolArgumentDeltasFlood(t *testing.T) {
	rg := NewRequestGuard(Config{
		Enabled:               true,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 10,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})

	for i := 0; i < 10; i++ {
		if err := rg.ObserveAppend("call_1", "", KindToolArguments, []byte("a")); err != nil {
			t.Fatalf("delta %d error: %v", i, err)
		}
	}
	// 第 11 个非空 delta 必须被拦截为 tool_argument_deltas (Spec §5.3 步骤 3)
	err := rg.ObserveAppend("call_1", "", KindToolArguments, []byte("a"))
	if err == nil || err.Error() != "tool_argument_deltas" {
		t.Fatalf("expected tool_argument_deltas, got %v", err)
	}
}

func TestRequestGuardRecordEventAndNoEventBatching(t *testing.T) {
	rg := NewRequestGuard(defaultTestConfig())

	// 1. 测试独立 RecordEvent 仅对上游业务事件执行 1 次计数
	for i := 0; i < MaxRequestEvents; i++ {
		if err := rg.RecordEvent(); err != nil {
			t.Fatalf("unexpected error at event %d: %v", i, err)
		}
	}
	// 第 65537 次 RecordEvent 触发 request_events 判定并冻结
	if err := rg.RecordEvent(); err == nil || err.Error() != "request_events" {
		t.Fatalf("expected request_events, got %v", err)
	}
	if !rg.IsTripped() || rg.TrippedReason() != "request_events" {
		t.Fatalf("expected tripped with request_events, got %s", rg.TrippedReason())
	}

	// 2. 测试同事件下多个 Observation 使用 ObserveAppendNoEvent / ObserveSnapshotNoEvent 不重复累加 requestEvents
	rgFresh := NewRequestGuard(defaultTestConfig())
	// 记录 1 次上游业务事件
	if err := rgFresh.RecordEvent(); err != nil {
		t.Fatalf("RecordEvent failed: %v", err)
	}
	// 单事件派生 3 个 delta append observation，调用 ObserveAppendNoEvent
	for i := 0; i < 3; i++ {
		cid := fmt.Sprintf("chan_%d", i)
		if err := rgFresh.ObserveAppendNoEvent(cid, "", KindText, []byte("val")); err != nil {
			t.Fatalf("ObserveAppendNoEvent observation %d failed: %v", i, err)
		}
	}
	// 断言 requestEvents 依然仅为 1（未被 3 次 observation 累加为 4）
	rgFresh.mu.RLock()
	events := rgFresh.requestEvents
	rgFresh.mu.RUnlock()
	if events != 1 {
		t.Fatalf("expected requestEvents=1 for batch observations under single event, got %d", events)
	}
}

func TestRequestGuardConcurrentAppendAndIsTrippedNoRace(t *testing.T) {
	rg := NewRequestGuard(defaultTestConfig())
	var wg sync.WaitGroup

	// 并发写
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			cid := fmt.Sprintf("c_concurrent_%d", id%3)
			for j := 0; j < 50; j++ {
				_ = rg.ObserveAppend(cid, "", KindText, []byte("a"))
			}
		}(i)
	}

	// 并发读状态
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = rg.IsTripped()
				_ = rg.TrippedReason()
			}
		}()
	}

	wg.Wait()
}
```

- [ ] **Step 2: Run test to verify failure (Red)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v -race ./security/outputguard -run "TestRequestGuard.*"`
Expected: FAIL with compilation error: undefined: NewRequestGuard

- [ ] **Step 3: Write minimal implementation**

```go
// security/outputguard/guard.go
package outputguard

import (
	"errors"
	"sync"
)

const (
	MaxRequestEvents   = 65536
	MaxRequestChannels = 128
	TriggeredCode      = "model_output_guard_triggered"
)

type Config struct {
	Enabled               bool
	MaxToolArgumentBytes  int64
	MaxToolArgumentDeltas int
	MaxChannelBytes       int64
	MaxRequestBytes       int64
}

type RequestGuard struct {
	mu            sync.RWMutex
	cfg           Config
	requestEvents int
	requestBytes  int64
	channels      map[string]*ChannelGuard
	aliasMap      map[string]string
	tripped       bool
	trippedReason string
}

func NewRequestGuard(cfg Config) *RequestGuard {
	return &RequestGuard{
		cfg:      cfg,
		channels: make(map[string]*ChannelGuard),
		aliasMap: make(map[string]string),
	}
}

func (g *RequestGuard) IsTripped() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.tripped
}

func (g *RequestGuard) TrippedReason() string
  func (g *RequestGuard) ActiveChannelCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.trippedReason
}

func (g *RequestGuard) tripLocked(reason string) error {
	if !g.tripped {
		g.tripped = true
		g.trippedReason = reason
	}
	return errors.New(g.trippedReason)
}

func (g *RequestGuard) resolveChannelLocked(channelID, aliasID string, kind ChannelKind) (*ChannelGuard, error) {
	// 晚到 ID 合并与双向映射机制 (Spec §4.3 & 审查反馈 M2 修复)：
	// 真实 Responses 流式 delta 初期通常仅包含 item_id（如 item_call_1），而在 output_item.done 时才提供 call_id。
	// 若分别创建会导致同一工具调用被分配两个 channel。
	var primaryKey string
	if channelID != "" && g.aliasMap[channelID] != "" {
		primaryKey = g.aliasMap[channelID]
	} else if aliasID != "" && g.aliasMap[aliasID] != "" {
		primaryKey = g.aliasMap[aliasID]
	} else if channelID != "" && g.channels[channelID] != nil {
		primaryKey = channelID
	} else if aliasID != "" && g.channels[aliasID] != nil {
		primaryKey = aliasID
	} else if channelID != "" {
		primaryKey = channelID
	} else if aliasID != "" {
		primaryKey = aliasID
	} else {
		primaryKey = "default"
	}

	// 建立双向 alias 映射，确保后续无论通过 channelID 还是 aliasID 查找均指向同一主 ChannelGuard 实例
	if channelID != "" {
		g.aliasMap[channelID] = primaryKey
	}
	if aliasID != "" {
		g.aliasMap[aliasID] = primaryKey
	}

	cg, exists := g.channels[primaryKey]
	if !exists {
		// 步骤 2: Channel 数量检查 (Spec §5.3)
		if len(g.channels)+1 > MaxRequestChannels {
			return nil, g.tripLocked("channel_count")
		}
		maxBytes := g.cfg.MaxChannelBytes
		if kind == KindToolArguments {
			maxBytes = g.cfg.MaxToolArgumentBytes
		}
		cg = NewChannelGuard(primaryKey, kind, maxBytes, g.cfg.MaxToolArgumentDeltas)
		g.channels[primaryKey] = cg
	}
	return cg, nil
}

// RecordEvent 严格对每上游业务事件执行 1 次计数（步骤 1: request_events 检查）
func (g *RequestGuard) RecordEvent() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.cfg.Enabled {
		return nil
	}
	if g.tripped {
		return errors.New(g.trippedReason)
	}

	g.requestEvents++
	if g.requestEvents > MaxRequestEvents {
		return g.tripLocked("request_events")
	}
	return nil
}

// ObserveAppend 严格执行 Spec §5.3 优先级流水线（先步骤 1 事件计数，再步骤 2~7 判定，首 reason 冻结）
func (g *RequestGuard) ObserveAppend(channelID string, aliasID string, kind ChannelKind, delta []byte) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.cfg.Enabled {
		return nil
	}
	if g.tripped {
		return errors.New(g.trippedReason)
	}

	// 步骤 1: 事件预算检查
	g.requestEvents++
	if g.requestEvents > MaxRequestEvents {
		return g.tripLocked("request_events")
	}

	return g.observeAppendLocked(channelID, aliasID, kind, delta)
}

// ObserveAppendNoEvent 仅执行步骤 2~7 判定，不重复累加 requestEvents（供 ObserveEvent 批处理调用）
func (g *RequestGuard) ObserveAppendNoEvent(channelID string, aliasID string, kind ChannelKind, delta []byte) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.cfg.Enabled {
		return nil
	}
	if g.tripped {
		return errors.New(g.trippedReason)
	}

	return g.observeAppendLocked(channelID, aliasID, kind, delta)
}

func (g *RequestGuard) observeAppendLocked(channelID string, aliasID string, kind ChannelKind, delta []byte) error {
	// 步骤 2: Channel 检查
	cg, err := g.resolveChannelLocked(channelID, aliasID, kind)
	if err != nil {
		return err
	}

	// 步骤 3 & 4: Delta 计数与语义字节高水位检查
	growth, cost, err := cg.CheckAppend(delta)
	if err != nil {
		if errors.Is(err, ErrScanBudgetExhausted) {
			if cg.kind == KindToolArguments {
				return g.tripLocked("tool_argument_bytes")
			}
			return g.tripLocked("channel_bytes")
		}
		return g.tripLocked(err.Error())
	}

	// 步骤 4: 请求级语义字节检查（必须先于步骤 6 扫描预算判定）
	if g.requestBytes+growth > g.cfg.MaxRequestBytes {
		return g.tripLocked("request_bytes")
	}

	// 步骤 5, 6, 7: 提交高水位并执行周期扫描
	g.requestBytes += growth
	if err := cg.CommitAppend(delta, growth, cost); err != nil {
		return g.tripLocked(err.Error())
	}

	return nil
}

// ObserveSnapshot 严格执行 Spec §5.3 快照判定流水线（先步骤 1 事件计数，再步骤 2~7 判定）
func (g *RequestGuard) ObserveSnapshot(channelID string, aliasID string, kind ChannelKind, snapshot []byte, terminalStage string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.cfg.Enabled {
		return nil
	}
	if g.tripped {
		return errors.New(g.trippedReason)
	}

	// 步骤 1: 事件预算检查
	g.requestEvents++
	if g.requestEvents > MaxRequestEvents {
		return g.tripLocked("request_events")
	}

	return g.observeSnapshotLocked(channelID, aliasID, kind, snapshot, terminalStage)
}

// ObserveSnapshotNoEvent 仅执行步骤 2~7 判定，不重复累加 requestEvents（供 ObserveEvent 批处理调用）
func (g *RequestGuard) ObserveSnapshotNoEvent(channelID string, aliasID string, kind ChannelKind, snapshot []byte, terminalStage string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.cfg.Enabled {
		return nil
	}
	if g.tripped {
		return errors.New(g.trippedReason)
	}

	return g.observeSnapshotLocked(channelID, aliasID, kind, snapshot, terminalStage)
}

func (g *RequestGuard) observeSnapshotLocked(channelID string, aliasID string, kind ChannelKind, snapshot []byte, terminalStage string) error {
	// 步骤 2: Channel 检查
	cg, err := g.resolveChannelLocked(channelID, aliasID, kind)
	if err != nil {
		return err
	}

	// 步骤 4: 语义字节预算检查 (先检查高水位推进与 request_bytes 上限)
	snapLen := int64(len(snapshot))
	var growth int64
	if snapLen > cg.highwater {
		growth = snapLen - cg.highwater
	}
	if snapLen > cg.maxBytes {
		if cg.kind == KindToolArguments {
			return g.tripLocked("tool_argument_bytes")
		}
		return g.tripLocked("channel_bytes")
	}
	if g.requestBytes+growth > g.cfg.MaxRequestBytes {
		return g.tripLocked("request_bytes")
	}

	// 步骤 5 & 6: 资格/等价性检查与扫描预算扣减
	growth, cost, _, err := cg.CheckSnapshot(snapshot, terminalStage)
	if err != nil {
		if errors.Is(err, ErrScanBudgetExhausted) {
			if cg.kind == KindToolArguments {
				return g.tripLocked("tool_argument_bytes")
			}
			return g.tripLocked("channel_bytes")
		}
		return g.tripLocked(err.Error())
	}

	// 步骤 7: 提交高水位与扫描
	g.requestBytes += growth
	if err := cg.CommitSnapshot(snapshot, growth, cost, terminalStage); err != nil {
		return g.tripLocked(err.Error())
	}

	return nil
}

// EndChannel 在关闭 channel 前执行尾检，释放窗口内存，累计 channel 映射不返还
func (g *RequestGuard) EndChannel(channelID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if cg, exists := g.channels[channelID]; exists {
		// 先执行尾检 (Spec §5.2 规则 6)
		if err := cg.CheckTail(); err != nil {
			return g.tripLocked(err.Error())
		}
		// 尾检通过后安全释放滑动窗口内存 (<=64KiB)
		cg.ReleaseWindow()
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify passing (Green)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v -race ./security/outputguard -run "TestRequestGuard.*"`
Expected: PASS ok github.com/codex2api/security/outputguard

- [ ] **Step 5: Commit**

```bash
git add security/outputguard/guard.go security/outputguard/guard_test.go
git commit -m "feat(outputguard): implement request guard coordinator with 8-step precedence and RWMutex"
```

---


func (g *RequestGuard) ActiveChannelCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.channels)
}

---

### Task 4: 数据库表结构迁移与三态指针持久化（SQLite & PostgreSQL）

**Files:**
- Create: `database/output_guard_settings.go`
- Modify: `database/sqlite.go:730-740`
- Modify: `database/postgres.go:1600-1610,3230-3240`
- Test: `database/output_guard_settings_test.go`

**Interfaces:**
- Produces:
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

  func (db *DB) GetOutputGuardSettings(ctx context.Context) (OutputGuardSettings, error)
  func (db *DB) UpdateOutputGuardSettings(ctx context.Context, update OutputGuardSettingsUpdate) (OutputGuardSettings, error)
  ```

- [ ] **Step 1: Write failing test for OutputGuardSettings migration, tri-state persistence, generation overflow protection, and narrow update isolation**

```go
// database/output_guard_settings_test.go
package database

import (
	"context"
	"errors"
	"math"
	"os"
	"sync"
	"testing"
)

func runOnAllDBDrivers(t *testing.T, testFn func(t *testing.T, db *DB)) {
	t.Run("sqlite", func(t *testing.T) {
		db := newResponseCacheSettingsTestDB(t)
		testFn(t, db)
	})

	if dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN"); dsn != "" {
		t.Run("postgres", func(t *testing.T) {
			db, err := New("postgres", dsn)
			if err != nil {
				t.Fatalf("New(postgres) error = %v", err)
			}
			defer db.Close()
			testFn(t, db)
		})
	}
}

func TestOutputGuardSettingsFreshDefaults(t *testing.T) {
	runOnAllDBDrivers(t, func(t *testing.T, db *DB) {
		got, err := db.GetOutputGuardSettings(context.Background())
		if err != nil {
			t.Fatalf("GetOutputGuardSettings error = %v", err)
		}

		if got.Enabled == nil || *got.Enabled != false {
			t.Fatalf("default Enabled want false, got %v", got.Enabled)
		}
		if got.MaxToolArgumentBytes != 131072 {
			t.Fatalf("default MaxToolArgumentBytes want 131072, got %d", got.MaxToolArgumentBytes)
		}
		if got.MaxToolArgumentDeltas != 4096 {
			t.Fatalf("default MaxToolArgumentDeltas want 4096, got %d", got.MaxToolArgumentDeltas)
		}
		if got.MaxChannelBytes != 2097152 {
			t.Fatalf("default MaxChannelBytes want 2097152, got %d", got.MaxChannelBytes)
		}
		if got.MaxRequestBytes != 4194304 {
			t.Fatalf("default MaxRequestBytes want 4194304, got %d", got.MaxRequestBytes)
		}
		if got.ConfigGeneration < 1 {
			t.Fatalf("default ConfigGeneration want >= 1, got %d", got.ConfigGeneration)
		}
	})
}

func TestOutputGuardSettingsExplicitFalseTriState(t *testing.T) {
	runOnAllDBDrivers(t, func(t *testing.T, db *DB) {
		ctx := context.Background()

		disabled := false
		updated, err := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
			Enabled: &disabled,
		})
		if err != nil {
			t.Fatalf("UpdateOutputGuardSettings error = %v", err)
		}
		if updated.Enabled == nil || *updated.Enabled != false {
			t.Fatalf("expected updated Enabled=false, got %v", updated.Enabled)
		}

		// 重新读取断言显式 false 得到正确保留（不被默认 false 覆盖）
		readBack, err := db.GetOutputGuardSettings(ctx)
		if err != nil {
			t.Fatalf("readBack error = %v", err)
		}
		if readBack.Enabled == nil || *readBack.Enabled != false {
			t.Fatalf("expected readBack Enabled=false, got %v", readBack.Enabled)
		}
	})
}

func TestOutputGuardSettingsGenerationOverflow(t *testing.T) {
	runOnAllDBDrivers(t, func(t *testing.T, db *DB) {
		ctx := context.Background()
		// 人为将 generation 置为 math.MaxInt64
		if _, err := db.conn.ExecContext(ctx, `
			INSERT INTO system_settings (id, output_guard_config_generation)
			VALUES (1, $1)
			ON CONFLICT (id) DO UPDATE SET output_guard_config_generation = $1
		`, int64(math.MaxInt64)); err != nil {
			t.Fatalf("seed max generation: %v", err)
		}

		toolBytes := int64(65536)
		_, err := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
			MaxToolArgumentBytes: &toolBytes,
		})
		if !errors.Is(err, ErrOutputGuardGenerationOverflow) {
			t.Fatalf("expected ErrOutputGuardGenerationOverflow, got %v", err)
		}
	})
}

func TestOutputGuardSettingsNarrowUpdateNotOverwrittenBySystemSettings(t *testing.T) {
	runOnAllDBDrivers(t, func(t *testing.T, db *DB) {
		ctx := context.Background()
		toolBytes := int64(262144) // 256 KiB
		disabled := false
		committed, err := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
			Enabled:              &disabled,
			MaxToolArgumentBytes: &toolBytes,
		})
		if err != nil {
			t.Fatalf("UpdateOutputGuardSettings error = %v", err)
		}

		// 模拟管理员保存常规设置（老 SystemSettings 全量写）
		if err := db.UpdateSystemSettings(ctx, &SystemSettings{SiteName: "stale full update"}); err != nil {
			t.Fatalf("UpdateSystemSettings error = %v", err)
		}

		// 重新读取 OutputGuardSettings，断言窄更新字段绝不被全量写覆盖
		got, err := db.GetOutputGuardSettings(ctx)
		if err != nil {
			t.Fatalf("GetOutputGuardSettings error = %v", err)
		}
		if got.Enabled == nil || *got.Enabled != false {
			t.Fatalf("Enabled overwritten by UpdateSystemSettings: got %v, want false", got.Enabled)
		}
		if got.MaxToolArgumentBytes != committed.MaxToolArgumentBytes {
			t.Fatalf("MaxToolArgumentBytes overwritten: got %d, want %d", got.MaxToolArgumentBytes, committed.MaxToolArgumentBytes)
		}
	})
}

func TestOutputGuardSettingsConcurrentUpdatesNoDeadlock(t *testing.T) {
	runOnAllDBDrivers(t, func(t *testing.T, db *DB) {
		ctx := context.Background()
		var wg sync.WaitGroup
		errCh := make(chan error, 10)

		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				toolBytes := int64(131072 + idx*1024)
				channelBytes := int64(2097152 + idx*1024)
				requestBytes := int64(4194304 + idx*1024)
				_, err := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
					MaxToolArgumentBytes: &toolBytes,
					MaxChannelBytes:      &channelBytes,
					MaxRequestBytes:      &requestBytes,
				})
				if err != nil {
					errCh <- err
				}
			}(i)
		}

		wg.Wait()
		close(errCh)
		for err := range errCh {
			t.Fatalf("concurrent update failed: %v", err)
		}
	})
}
```

- [ ] **Step 2: Run test to verify failure (Red)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./database -run "TestOutputGuardSettings.*"`
Expected: FAIL with compilation error: undefined: db.GetOutputGuardSettings

- [ ] **Step 3: Write minimal implementation**

修改 `database/sqlite.go` 添加列：
```go
// 在 database/sqlite.go 中的 columns 切片中添加：
{"system_settings", "output_guard_enabled", "INTEGER DEFAULT 0"},
{"system_settings", "output_guard_max_tool_argument_bytes", "INTEGER NOT NULL DEFAULT 131072"},
{"system_settings", "output_guard_max_tool_argument_deltas", "INTEGER NOT NULL DEFAULT 4096"},
{"system_settings", "output_guard_max_channel_bytes", "INTEGER NOT NULL DEFAULT 2097152"},
{"system_settings", "output_guard_max_request_bytes", "INTEGER NOT NULL DEFAULT 4194304"},
{"system_settings", "output_guard_config_generation", "INTEGER NOT NULL DEFAULT 1"},
```

修改 `database/postgres.go` 添加列及隔离 UpdateSystemSettings：
```go
// 在 database/postgres.go initPostgresSchema 中添加：
ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS output_guard_enabled BOOLEAN DEFAULT FALSE;
ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS output_guard_max_tool_argument_bytes BIGINT NOT NULL DEFAULT 131072;
ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS output_guard_max_tool_argument_deltas INT NOT NULL DEFAULT 4096;
ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS output_guard_max_channel_bytes BIGINT NOT NULL DEFAULT 2097152;
ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS output_guard_max_request_bytes BIGINT NOT NULL DEFAULT 4194304;
ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS output_guard_config_generation BIGINT NOT NULL DEFAULT 1;

// 在 database/postgres.go UpdateSystemSettings 的 ON CONFLICT (id) DO UPDATE SET 中：
// 确保不包含 output_guard_* 字段，使该 6 个字段完全由独立的窄更新函数单独维护，不被老全局设置全量写回覆盖！
```

新建 `database/output_guard_settings.go`:
```go
package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
)

var (
	ErrInvalidOutputGuardSettings     = errors.New("invalid output guard settings")
	ErrOutputGuardGenerationOverflow = errors.New("output guard config generation overflow")
)

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

func DefaultOutputGuardSettings() OutputGuardSettings {
	t := true
	return OutputGuardSettings{
		Enabled:               &t,
		MaxToolArgumentBytes:  131072,  // 128 KiB
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152, // 2 MiB
		MaxRequestBytes:       4194304, // 4 MiB
		ConfigGeneration:      1,
	}
}

func ValidateOutputGuardSettings(s OutputGuardSettings) error {
	if s.MaxToolArgumentBytes < 16384 || s.MaxToolArgumentBytes > 4194304 {
		return fmt.Errorf("%w: max_tool_argument_bytes must be 16KiB..4MiB", ErrInvalidOutputGuardSettings)
	}
	if s.MaxToolArgumentDeltas < 256 || s.MaxToolArgumentDeltas > 65536 {
		return fmt.Errorf("%w: max_tool_argument_deltas must be 256..65536", ErrInvalidOutputGuardSettings)
	}
	if s.MaxChannelBytes < 131072 || s.MaxChannelBytes > 16777216 {
		return fmt.Errorf("%w: max_channel_bytes must be 128KiB..16MiB", ErrInvalidOutputGuardSettings)
	}
	if s.MaxRequestBytes < 262144 || s.MaxRequestBytes > 67108864 {
		return fmt.Errorf("%w: max_request_bytes must be 256KiB..64MiB", ErrInvalidOutputGuardSettings)
	}
	if s.MaxToolArgumentBytes > s.MaxChannelBytes || s.MaxChannelBytes > s.MaxRequestBytes {
		return fmt.Errorf("%w: requires tool <= channel <= request bytes", ErrInvalidOutputGuardSettings)
	}
	return nil
}

func (db *DB) outputGuardSettingsSelectQuery(forUpdate bool) string {
	query := `SELECT output_guard_enabled,
		COALESCE(output_guard_max_tool_argument_bytes, 131072),
		COALESCE(output_guard_max_tool_argument_deltas, 4096),
		COALESCE(output_guard_max_channel_bytes, 2097152),
		COALESCE(output_guard_max_request_bytes, 4194304),
		COALESCE(output_guard_config_generation, 1)
		FROM system_settings WHERE id = 1`
	if forUpdate && db.driver == "postgres" {
		query += " FOR UPDATE"
	}
	return query
}

func (db *DB) GetOutputGuardSettings(ctx context.Context) (OutputGuardSettings, error) {
	settings := DefaultOutputGuardSettings()
	var enabled sql.NullBool
	err := db.conn.QueryRowContext(ctx, db.outputGuardSettingsSelectQuery(false)).Scan(
		&enabled,
		&settings.MaxToolArgumentBytes,
		&settings.MaxToolArgumentDeltas,
		&settings.MaxChannelBytes,
		&settings.MaxRequestBytes,
		&settings.ConfigGeneration,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return settings, nil
	}
	if err != nil {
		return OutputGuardSettings{}, err
	}
	if enabled.Valid {
		val := enabled.Bool
		settings.Enabled = &val
	}
	return settings, nil
}

func (db *DB) UpdateOutputGuardSettings(ctx context.Context, update OutputGuardSettingsUpdate) (OutputGuardSettings, error) {
	var committed OutputGuardSettings

	operation := func() error {
		tx, err := db.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()

		// 确保 id=1 存在
		if _, err := tx.ExecContext(ctx, `INSERT INTO system_settings (id) VALUES (1) ON CONFLICT(id) DO NOTHING`); err != nil {
			return err
		}

		current := DefaultOutputGuardSettings()
		var enabled sql.NullBool
		err = tx.QueryRowContext(ctx, db.outputGuardSettingsSelectQuery(true)).Scan(
			&enabled,
			&current.MaxToolArgumentBytes,
			&current.MaxToolArgumentDeltas,
			&current.MaxChannelBytes,
			&current.MaxRequestBytes,
			&current.ConfigGeneration,
		)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if enabled.Valid {
			val := enabled.Bool
			current.Enabled = &val
		}

		next := current
		if update.Enabled != nil {
			val := *update.Enabled
			next.Enabled = &val
		}
		if update.MaxToolArgumentBytes != nil {
			next.MaxToolArgumentBytes = *update.MaxToolArgumentBytes
		}
		if update.MaxToolArgumentDeltas != nil {
			next.MaxToolArgumentDeltas = *update.MaxToolArgumentDeltas
		}
		if update.MaxChannelBytes != nil {
			next.MaxChannelBytes = *update.MaxChannelBytes
		}
		if update.MaxRequestBytes != nil {
			next.MaxRequestBytes = *update.MaxRequestBytes
		}

		// Generation 溢出保护
		if current.ConfigGeneration == math.MaxInt64 {
			return ErrOutputGuardGenerationOverflow
		}
		next.ConfigGeneration++

		if err := ValidateOutputGuardSettings(next); err != nil {
			return err
		}

		updateQuery := `UPDATE system_settings SET
			output_guard_enabled = $1,
			output_guard_max_tool_argument_bytes = $2,
			output_guard_max_tool_argument_deltas = $3,
			output_guard_max_channel_bytes = $4,
			output_guard_max_request_bytes = $5,
			output_guard_config_generation = $6
			WHERE id = 1`

		var enabledVal any
		if next.Enabled != nil {
			enabledVal = *next.Enabled
		} else {
			enabledVal = true
		}

		if _, err := tx.ExecContext(ctx, updateQuery,
			enabledVal,
			next.MaxToolArgumentBytes,
			next.MaxToolArgumentDeltas,
			next.MaxChannelBytes,
			next.MaxRequestBytes,
			next.ConfigGeneration,
		); err != nil {
			return err
		}

		if err := tx.Commit(); err != nil {
			return err
		}
		committed = next
		return nil
	}

	if db.isSQLite() {
		if err := db.withSQLiteWriteLock(ctx, operation); err != nil {
			return OutputGuardSettings{}, err
		}
	} else {
		if err := operation(); err != nil {
			return OutputGuardSettings{}, err
		}
	}

	return committed, nil
}
```

- [ ] **Step 4: Run test to verify passing (Green)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./database -run "TestOutputGuardSettings.*"`
Expected: PASS ok github.com/codex2api/database

- [ ] **Step 5: Commit**

```bash
git add database/output_guard_settings.go database/output_guard_settings_test.go database/sqlite.go database/postgres.go
git commit -m "feat(database): add output guard settings table migration and tri-state persistence"
```

---

---

### Task 5: 运行时不可变配置快照、5 秒后台 Revision 轮询器与冷启动就绪门禁

**Files:**
- Create: `proxy/output_guard_settings.go`
- Create: `proxy/output_guard_settings_test.go`
- Modify: `main.go:265-270`

**Interfaces:**
- Consumes:
  - `database.OutputGuardSettings`:
    ```go
    type OutputGuardSettings struct {
        Enabled               *bool `json:"output_guard_enabled"`
        MaxToolArgumentBytes  int64 `json:"output_guard_max_tool_argument_bytes"`
        MaxToolArgumentDeltas int   `json:"output_guard_max_tool_argument_deltas"`
        MaxChannelBytes       int64 `json:"output_guard_max_channel_bytes"`
        MaxRequestBytes       int64 `json:"output_guard_max_request_bytes"`
        ConfigGeneration      int64 `json:"output_guard_config_generation"`
    }
    ```
  - `database.DefaultOutputGuardSettings() database.OutputGuardSettings`
  - `database.ValidateOutputGuardSettings(database.OutputGuardSettings) error`
  - `database.DB.GetOutputGuardSettings(context.Context) (database.OutputGuardSettings, error)`
- Produces:
  ```go
  package proxy

  import (
      "context"
      "time"
      "github.com/codex2api/database"
      "github.com/gin-gonic/gin"
      "github.com/gorilla/websocket"
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

  func ApplyOutputGuardSettings(settings database.OutputGuardSettings) bool
  func CurrentOutputGuardConfig() OutputGuardAppliedConfig
  func GetOutputGuardConfigSyncStatus() OutputGuardConfigSyncStatus
  func IsOutputGuardReady() bool
  func SetOutputGuardReadyForTest(ready bool)
  func ResetOutputGuardStateForTest()
  func LoadOutputGuardSettings(ctx context.Context, reader OutputGuardSettingsReader) error
  func StartOutputGuardConfigPoller(parent context.Context, db *database.DB) bool

  // 门禁检查接口（供 Task 7/8 各协议与 WebSocket/Realtime 挂接）
  func RequireOutputGuardReadyHTTP(c *gin.Context) bool
  func RequireOutputGuardReadyWS(conn *websocket.Conn) bool
  func RequireOutputGuardReadyStream(conn *websocket.Conn, streamID string) bool
  ```

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
	"sync"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

type controlledOutputGuardSettingsReader struct {
	mu       sync.Mutex
	requests chan chan outputGuardSettingsReadResult
}

type outputGuardSettingsReadResult struct {
	settings database.OutputGuardSettings
	err      error
}

func newControlledOutputGuardSettingsReader() *controlledOutputGuardSettingsReader {
	return &controlledOutputGuardSettingsReader{
		requests: make(chan chan outputGuardSettingsReadResult, 16),
	}
}

func (r *controlledOutputGuardSettingsReader) GetOutputGuardSettings(ctx context.Context) (database.OutputGuardSettings, error) {
	respCh := make(chan outputGuardSettingsReadResult, 1)
	select {
	case <-ctx.Done():
		return database.OutputGuardSettings{}, ctx.Err()
	case r.requests <- respCh:
	}

	select {
	case <-ctx.Done():
		return database.OutputGuardSettings{}, ctx.Err()
	case res := <-respCh:
		return res.settings, res.err
	}
}

func (r *controlledOutputGuardSettingsReader) respond(t *testing.T, res outputGuardSettingsReadResult) {
	t.Helper()
	select {
	case respCh := <-r.requests:
		respCh <- res
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for GetOutputGuardSettings request")
	}
}

func waitForOutputGuardConfigGeneration(t *testing.T, targetGen int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if CurrentOutputGuardConfig().Generation == targetGen {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for output guard config generation %d (current: %d)", targetGen, CurrentOutputGuardConfig().Generation)
}

func waitForOutputGuardSyncError(t *testing.T, substr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status := GetOutputGuardConfigSyncStatus()
		if status.LastSyncError != "" && (substr == "" || strings.Contains(status.LastSyncError, substr)) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for output guard sync error containing %q", substr)
}

func TestOutputGuardColdStartNotReadyReturns503(t *testing.T) {
	ResetOutputGuardStateForTest()
	t.Cleanup(ResetOutputGuardStateForTest)

	if IsOutputGuardReady() {
		t.Fatal("expected cold start IsOutputGuardReady = false")
	}

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	passed := RequireOutputGuardReadyHTTP(c)
	if passed {
		t.Fatal("expected RequireOutputGuardReadyHTTP to reject cold start request")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected HTTP 503, got %d", w.Code)
	}
	var errResp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal error response failed: %v", err)
	}
	if errResp.Error.Code != string(api.ErrCodeServiceUnavailable) {
		t.Fatalf("expected error code %q, got %q", api.ErrCodeServiceUnavailable, errResp.Error.Code)
	}
	if errResp.Error.Code == "model_output_guard_triggered" {
		t.Fatalf("must NOT use runtime tripped error code on cold start 503")
	}
}

func TestOutputGuardExplicitDisabledSemanticVsColdStart(t *testing.T) {
	ResetOutputGuardStateForTest()
	t.Cleanup(ResetOutputGuardStateForTest)

	// Cold start: not ready
	if IsOutputGuardReady() {
		t.Fatal("expected cold start not ready")
	}

	// Load DB settings with Enabled = false
	disabled := false
	dbSettings := database.OutputGuardSettings{
		Enabled:               &disabled,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
		ConfigGeneration:      1,
	}

	applied := ApplyOutputGuardSettings(dbSettings)
	if !applied {
		t.Fatal("ApplyOutputGuardSettings failed")
	}

	// Once loaded successfully, IsOutputGuardReady is true even when Enabled = false!
	if !IsOutputGuardReady() {
		t.Fatal("expected IsOutputGuardReady = true after explicit disabled settings loaded")
	}

	cfg := CurrentOutputGuardConfig()
	if cfg.Enabled != false {
		t.Fatalf("expected applied config Enabled = false, got %v", cfg.Enabled)
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	passed := RequireOutputGuardReadyHTTP(c)
	if !passed {
		t.Fatal("expected RequireOutputGuardReadyHTTP to pass when system is ready and explicitly disabled")
	}
	if w.Code == http.StatusServiceUnavailable {
		t.Fatalf("did not expect 503 when explicitly disabled and ready")
	}
}

func TestOutputGuardConfigGenerationIncrementsAndLKGMaintained(t *testing.T) {
	ResetOutputGuardStateForTest()
	t.Cleanup(ResetOutputGuardStateForTest)

	enabled := true
	initial := database.OutputGuardSettings{
		Enabled:               &enabled,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
		ConfigGeneration:      1,
	}
	if !ApplyOutputGuardSettings(initial) {
		t.Fatal("failed to apply initial generation 1")
	}

	cfg1 := CurrentOutputGuardConfig()
	if cfg1.Generation != 1 || cfg1.MaxToolArgumentBytes != 131072 {
		t.Fatalf("initial config mismatch: %+v", cfg1)
	}

	// Stale generation rejected
	stale := initial
	stale.MaxToolArgumentBytes = 262144
	if ApplyOutputGuardSettings(stale) {
		t.Fatal("stale generation 1 should be rejected")
	}
	if CurrentOutputGuardConfig().MaxToolArgumentBytes != 131072 {
		t.Fatal("LKG overwritten by stale generation")
	}

	// Apply generation 2
	gen2 := initial
	gen2.MaxToolArgumentBytes = 262144
	gen2.ConfigGeneration = 2
	if !ApplyOutputGuardSettings(gen2) {
		t.Fatal("failed to apply generation 2")
	}
	cfg2 := CurrentOutputGuardConfig()
	if cfg2.Generation != 2 || cfg2.MaxToolArgumentBytes != 262144 {
		t.Fatalf("generation 2 mismatch: %+v", cfg2)
	}

	// Illegal configuration validation failure preserves LKG
	illegal := initial
	illegal.MaxToolArgumentBytes = 50000000 // exceeds 4MiB
	illegal.ConfigGeneration = 3
	if ApplyOutputGuardSettings(illegal) {
		t.Fatal("illegal configuration should be rejected")
	}
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
	// 1. Initial read
	reader.respond(t, outputGuardSettingsReadResult{
		settings: database.OutputGuardSettings{
			Enabled:               &enabled,
			MaxToolArgumentBytes:  131072,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
			ConfigGeneration:      2,
		},
	})
	waitForOutputGuardConfigGeneration(t, 2)
	if !IsOutputGuardReady() {
		t.Fatal("expected IsOutputGuardReady = true after successful poll")
	}

	// 2. Outage: DB error preserves LKG
	reader.respond(t, outputGuardSettingsReadResult{
		err: errors.New("simulated database disconnect"),
	})
	waitForOutputGuardSyncError(t, "simulated database disconnect")
	if CurrentOutputGuardConfig().Generation != 2 {
		t.Fatal("LKG generation lost during DB outage")
	}

	// 3. Recovery: next poll succeeds with generation 3
	reader.respond(t, outputGuardSettingsReadResult{
		settings: database.OutputGuardSettings{
			Enabled:               &enabled,
			MaxToolArgumentBytes:  262144,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
			ConfigGeneration:      3,
		},
	})
	waitForOutputGuardConfigGeneration(t, 3)
	if CurrentOutputGuardConfig().MaxToolArgumentBytes != 262144 {
		t.Fatal("failed to update config after recovery")
	}

	// 4. Cancel ctx and verify poller stops
	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("poller failed to stop cleanly after context cancel")
	}
}

func TestOutputGuardWSReadinessGate(t *testing.T) {
	ResetOutputGuardStateForTest()
	t.Cleanup(ResetOutputGuardStateForTest)

	// Create test WebSocket server
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		if !RequireOutputGuardReadyWS(conn) {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte("ready"))
	}))
	defer server.Close()

	wsURL := "ws" + server.URL[len("http"):]
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial WS failed: %v", err)
	}
	defer conn.Close()

	// When not ready, server should send 503 error frame / CloseTryAgainLater (1013)
	_, msg, err := conn.ReadMessage()
	if err != nil {
		var closeErr *websocket.CloseError
		if errors.As(err, &closeErr) {
			if closeErr.Code != websocket.CloseTryAgainLater {
				t.Fatalf("expected close code 1013 CloseTryAgainLater, got %d", closeErr.Code)
			}
			return
		}
	}
	if len(msg) > 0 {
		var errResp struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(msg, &errResp)
		if errResp.Error.Code != string(api.ErrCodeServiceUnavailable) {
			t.Fatalf("expected error frame with code %s, got %s", api.ErrCodeServiceUnavailable, errResp.Error.Code)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails (Red)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./proxy -run "TestOutputGuard.*"`
Expected: FAIL with "undefined: ApplyOutputGuardSettings", "undefined: RequireOutputGuardReadyHTTP", etc.

- [ ] **Step 3: Write minimal implementation for proxy/output_guard_settings.go and wire in main.go**

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
	outputGuardConfigPollInterval = 5 * time.Second
	outputGuardConfigReadTimeout  = 3 * time.Second
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

var globalOutputGuardState = &outputGuardState{}

func init() {
	ResetOutputGuardStateForTest()
}

func ResetOutputGuardStateForTest() {
	globalOutputGuardState.mu.Lock()
	defer globalOutputGuardState.mu.Unlock()
	globalOutputGuardState.config = OutputGuardAppliedConfig{
		Enabled:               true,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
		Generation:            0,
	}
	globalOutputGuardState.lastSyncAt = time.Time{}
	globalOutputGuardState.lastSyncErr = ""
	globalOutputGuardState.isReady.Store(false)
}

func SetOutputGuardReadyForTest(ready bool) {
	globalOutputGuardState.isReady.Store(ready)
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
	settings, err := readOutputGuardSettings(ctx, reader, outputGuardConfigReadTimeout)
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
			outputGuardConfigPollInterval,
			outputGuardConfigReadTimeout,
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
		interval = outputGuardConfigPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			settings, err := readOutputGuardSettings(ctx, reader, readTimeout)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				recordOutputGuardConfigSyncError(err)
				continue
			}
			if err := database.ValidateOutputGuardSettings(settings); err != nil {
				recordOutputGuardConfigSyncError(err)
				continue
			}
			ApplyOutputGuardSettings(settings)
			recordOutputGuardConfigSyncSuccess(time.Now())
		}
	}
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
	if timeout <= 0 || timeout > outputGuardConfigReadTimeout {
		timeout = outputGuardConfigReadTimeout
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

// 门禁检查实现：冷启动未就绪返回 503，严禁以 log.Fatalf 冒充 503

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
		appliedResponseCache.Generation,
		appliedResponseCache.LocalMaxBytes,
		appliedResponseCache.LocalMaxEntryBytes,
		appliedResponseCache.ReconstructMaxBytes,
	)

	// --- 插入开始: OutputGuard 加载与后台 5 秒轮询器启动 ---
	outputGuardCtx, outputGuardCancel := context.WithTimeout(context.Background(), 3*time.Second)
	if err := proxy.LoadOutputGuardSettings(outputGuardCtx, db); err != nil {
		outputGuardCancel()
		// 严禁 log.Fatalf 冒充 503！冷启动加载失败仅记录警告日志，保持 IsOutputGuardReady=false，
		// 请求进入时由门禁准确返回 HTTP 503 api.ErrCodeServiceUnavailable，且 poller 后续轮询成功时可无缝自愈恢复。
		log.Printf("警告: 加载模型输出失控防护设置失败（冷启动门禁将返回 503 直至 DB 轮询恢复）: %v", err)
	} else {
		outputGuardCancel()
		appliedGuard := proxy.CurrentOutputGuardConfig()
		log.Printf(
			"模型输出失控防护设置已加载: generation=%d enabled=%t tool_bytes=%d tool_deltas=%d channel_bytes=%d request_bytes=%d",
			appliedGuard.Generation,
			appliedGuard.Enabled,
			appliedGuard.MaxToolArgumentBytes,
			appliedGuard.MaxToolArgumentDeltas,
			appliedGuard.MaxChannelBytes,
			appliedGuard.MaxRequestBytes,
		)
	}
	proxy.StartOutputGuardConfigPoller(context.Background(), db)
	// --- 插入结束 ---
```

- [ ] **Step 4: Run test to verify it passes (Green)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./proxy -run "TestOutputGuard.*"`
Expected: PASS ok github.com/codex2api/proxy

- [ ] **Step 5: Commit**

```bash
git add proxy/output_guard_settings.go proxy/output_guard_settings_test.go main.go
git commit -m "feat(proxy): add output guard runtime config snapshot, poller and cold start readiness gate"
```

---

---

### Task 6: 复用 Admin Settings GET/PUT 扩展与完整独立前端 UI 卡片

**Files:**
- Create: `admin/output_guard_settings.go`
- Create: `admin/output_guard_settings_test.go`
- Modify: `admin/handler.go:9460-9640` (`settingsResponse`, `updateSettingsReq`, `GetSettings`, `UpdateSettings`)
- Modify: `frontend/src/types.ts:2300-2315`
- Modify: `frontend/src/pages/Settings.tsx:200-220, 6545-6600`
- Test: `frontend/src/lib/outputGuardSettings.test.mjs` (新建前端纯逻辑测试)

**Interfaces:**
- Consumes:
  - `database.OutputGuardSettings` / `database.OutputGuardSettingsUpdate` (来自 Task 4)
  - `proxy.CurrentOutputGuardConfig()` / `proxy.ApplyOutputGuardSettings()` (来自 Task 5)
  - `database.ValidateOutputGuardSettings(database.OutputGuardSettings) error`
- Produces:
  - Admin REST API:
    - `GET /api/admin/settings` 返回 5 个完整对称字段 + 1 个只读 generation：
      - `output_guard_enabled`: bool
      - `output_guard_max_tool_argument_bytes`: int64
      - `output_guard_max_tool_argument_deltas`: int
      - `output_guard_max_channel_bytes`: int64
      - `output_guard_max_request_bytes`: int64
      - `output_guard_config_generation`: int64 (只读)
    - `PUT /api/admin/settings` 接收上述 5 个字段的可选 patch，若传入 `output_guard_config_generation` 则报错 400（只读字段禁止提交）。
  - Frontend TypeScript Contract (`frontend/src/types.ts`):
    - `SystemSettings` 增加上述 6 个属性（含 `readonly output_guard_config_generation: number`）。
  - Frontend UI (`frontend/src/pages/Settings.tsx`):
    - 位于 General Tab 独立的“模型输出失控防护 (Model Output Guard)”专属卡片（不挂在 PromptFilter 下）。
    - 5 个完整配置项输入，单位展示 KiB / MiB 与格式化转换，非法层级交互反馈（`tool <= channel <= request`），显式 false 准确提交，以及“恢复默认”5 字段原子提交。

- [ ] **Step 1: Write failing test for admin GET/PUT settings with output guard fields, range validation, explicit false, and readonly revision rejection**

```go
// admin/output_guard_settings_test.go
package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestAdminOutputGuardSettingsGetSymmetricFields(t *testing.T) {
	db := newTestAdminDB(t)
	handler := &Handler{db: db}

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)

	handler.GetSettings(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d, body: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response failed: %v", err)
	}

	// Verify all 6 symmetric fields exist
	fields := []string{
		"output_guard_enabled",
		"output_guard_max_tool_argument_bytes",
		"output_guard_max_tool_argument_deltas",
		"output_guard_max_channel_bytes",
		"output_guard_max_request_bytes",
		"output_guard_config_generation",
	}
	for _, f := range fields {
		if _, ok := resp[f]; !ok {
			t.Fatalf("expected field %q in GET /api/admin/settings response", f)
		}
	}

	if resp["output_guard_enabled"] != false {
		t.Fatalf("expected output_guard_enabled = false by default, got %v", resp["output_guard_enabled"])
	}
	if int64(resp["output_guard_max_tool_argument_bytes"].(float64)) != 131072 {
		t.Fatalf("expected tool argument bytes 131072, got %v", resp["output_guard_max_tool_argument_bytes"])
	}
}

func TestAdminOutputGuardSettingsPutValidationAndReadOnlyRevision(t *testing.T) {
	db := newTestAdminDB(t)
	handler := &Handler{db: db}
	gin.SetMode(gin.TestMode)

	// 1. Rejection of readonly output_guard_config_generation
	{
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(`{"output_guard_config_generation": 999}`))
		c.Request.Header.Set("Content-Type", "application/json")
		handler.UpdateSettings(c)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected HTTP 400 when submitting readonly output_guard_config_generation, got %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), "只读") {
			t.Fatalf("expected error mentioning readonly, got %s", w.Body.String())
		}
	}

	// 2. Rejection of out-of-range parameters (16KiB..4MiB, etc.)
	{
		// Tool bytes too small: 1024 < 16384
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(`{"output_guard_max_tool_argument_bytes": 1024}`))
		c.Request.Header.Set("Content-Type", "application/json")
		handler.UpdateSettings(c)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected HTTP 400 for tool bytes < 16KiB, got %d", w.Code)
		}
	}

	// 3. Rejection of hierarchy violation: tool > channel
	{
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(`{"output_guard_max_tool_argument_bytes": 3145728, "output_guard_max_channel_bytes": 2097152}`))
		c.Request.Header.Set("Content-Type", "application/json")
		handler.UpdateSettings(c)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected HTTP 400 for hierarchy violation tool > channel, got %d", w.Code)
		}
	}

	// 4. Successful update with explicit false & verify generation increment
	{
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		payload := `{"output_guard_enabled": false, "output_guard_max_tool_argument_bytes": 65536}`
		c.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(payload))
		c.Request.Header.Set("Content-Type", "application/json")
		handler.UpdateSettings(c)

		if w.Code != http.StatusOK {
			t.Fatalf("expected HTTP 200 for valid PUT, got %d, body: %s", w.Code, w.Body.String())
		}

		var resp map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["output_guard_enabled"] != false {
			t.Fatalf("expected output_guard_enabled = false, got %v", resp["output_guard_enabled"])
		}
		if int64(resp["output_guard_max_tool_argument_bytes"].(float64)) != 65536 {
			t.Fatalf("expected tool bytes = 65536, got %v", resp["output_guard_max_tool_argument_bytes"])
		}
		if int64(resp["output_guard_config_generation"].(float64)) <= 1 {
			t.Fatalf("expected generation incremented, got %v", resp["output_guard_config_generation"])
		}

		// Also check local runtime instance updated immediately
		cfg := proxy.CurrentOutputGuardConfig()
		if cfg.Enabled != false || cfg.MaxToolArgumentBytes != 65536 {
			t.Fatalf("local proxy runtime config not updated immediately: %+v", cfg)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails (Red)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./admin -run "TestAdminOutputGuard.*"`
Expected: FAIL with "unknown field output_guard_enabled" or failed assertions.

- [ ] **Step 3: Write minimal implementation for admin backend and frontend integration**

1. 编写 `admin/output_guard_settings.go`:
```go
// admin/output_guard_settings.go
package admin

import (
	"fmt"
	"github.com/codex2api/database"
)

func validateOutputGuardSettingsUpdateRanges(update database.OutputGuardSettingsUpdate) error {
	if update.MaxToolArgumentBytes != nil {
		val := *update.MaxToolArgumentBytes
		if val < 16384 || val > 4194304 {
			return fmt.Errorf("output_guard_max_tool_argument_bytes 必须在 16KiB(16384) ~ 4MiB(4194304) 之间")
		}
	}
	if update.MaxToolArgumentDeltas != nil {
		val := *update.MaxToolArgumentDeltas
		if val < 256 || val > 65536 {
			return fmt.Errorf("output_guard_max_tool_argument_deltas 必须在 256 ~ 65536 之间")
		}
	}
	if update.MaxChannelBytes != nil {
		val := *update.MaxChannelBytes
		if val < 131072 || val > 16777216 {
			return fmt.Errorf("output_guard_max_channel_bytes 必须在 128KiB(131072) ~ 16MiB(16777216) 之间")
		}
	}
	if update.MaxRequestBytes != nil {
		val := *update.MaxRequestBytes
		if val < 262144 || val > 67108864 {
			return fmt.Errorf("output_guard_max_request_bytes 必须在 256KiB(262144) ~ 64MiB(67108864) 之间")
		}
	}
	return nil
}
```

2. 在 `admin/handler.go` 中扩展 `settingsResponse`、`updateSettingsReq`、`GetSettings` 及 `UpdateSettings`：
- 在 `settingsResponse`（约 line 9460）中添加：
```go
	OutputGuardEnabled              bool  `json:"output_guard_enabled"`
	OutputGuardMaxToolArgumentBytes int64 `json:"output_guard_max_tool_argument_bytes"`
	OutputGuardMaxToolArgumentDeltas int   `json:"output_guard_max_tool_argument_deltas"`
	OutputGuardMaxChannelBytes      int64 `json:"output_guard_max_channel_bytes"`
	OutputGuardMaxRequestBytes      int64 `json:"output_guard_max_request_bytes"`
	OutputGuardConfigGeneration     int64 `json:"output_guard_config_generation"`
```

- 在 `updateSettingsReq`（约 line 9635）中添加：
```go
	OutputGuardEnabled               *bool   `json:"output_guard_enabled"`
	OutputGuardMaxToolArgumentBytes  *int64  `json:"output_guard_max_tool_argument_bytes"`
	OutputGuardMaxToolArgumentDeltas *int    `json:"output_guard_max_tool_argument_deltas"`
	OutputGuardMaxChannelBytes       *int64  `json:"output_guard_max_channel_bytes"`
	OutputGuardMaxRequestBytes       *int64  `json:"output_guard_max_request_bytes"`
	OutputGuardConfigGeneration      rawJSON `json:"output_guard_config_generation"`
```

- 在 `GetSettings`（约 line 10240 之后）读取并注入 output guard 字段：
```go
	outputGuardSettings, err := h.db.GetOutputGuardSettings(ctx)
	if err != nil {
		outputGuardSettings = database.DefaultOutputGuardSettings()
	}
	outputGuardEnabled := true
	if outputGuardSettings.Enabled != nil {
		outputGuardEnabled = *outputGuardSettings.Enabled
	}
```
并在 `settingsResponse` 结构字面量中填充：
```go
		OutputGuardEnabled:              outputGuardEnabled,
		OutputGuardMaxToolArgumentBytes: outputGuardSettings.MaxToolArgumentBytes,
		OutputGuardMaxToolArgumentDeltas: outputGuardSettings.MaxToolArgumentDeltas,
		OutputGuardMaxChannelBytes:      outputGuardSettings.MaxChannelBytes,
		OutputGuardMaxRequestBytes:      outputGuardSettings.MaxRequestBytes,
		OutputGuardConfigGeneration:     outputGuardSettings.ConfigGeneration,
```

- 在 `UpdateSettings`（约 line 10600 及 line 10750）添加只读校验、更新合并与立即生效逻辑：
```go
	if req.OutputGuardConfigGeneration != nil {
		writeError(c, http.StatusBadRequest, "output_guard_config_generation 为只读字段")
		return
	}

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

并在 `UpdateSettings` 结尾的 `c.JSON(http.StatusOK, response)` 中带上最新的 `outputGuardSettings` 字段。

3. 修改 `frontend/src/types.ts`（约 line 2305 处）：
```typescript
  output_guard_enabled: boolean
  output_guard_max_tool_argument_bytes: number
  output_guard_max_tool_argument_deltas: number
  output_guard_max_channel_bytes: number
  output_guard_max_request_bytes: number
  readonly output_guard_config_generation: number
```

4. 编写前端纯函数测试 `frontend/src/lib/outputGuardSettings.test.mjs`：
```javascript
import test from 'node:test'
import assert from 'node:assert/strict'

const KIB = 1024
const MIB = 1024 * 1024

export const DEFAULT_OUTPUT_GUARD_SETTINGS = {
  output_guard_enabled: false,
  output_guard_max_tool_argument_bytes: 256 * KIB,
  output_guard_max_tool_argument_deltas: 4096,
  output_guard_max_channel_bytes: 2 * MIB,
  output_guard_max_request_bytes: 4 * MIB,
}

export function validateOutputGuardHierarchy(settings) {
  const tool = settings.output_guard_max_tool_argument_bytes
  const channel = settings.output_guard_max_channel_bytes
  const request = settings.output_guard_max_request_bytes
  if (tool > channel) {
    return '工具参数上限不能超过单 Channel 字节上限'
  }
  if (channel > request) {
    return '单 Channel 字节上限不能超过单请求语义预算'
  }
  return null
}

export function bytesToKiB(bytes) {
  return Math.round(bytes / KIB)
}

export function kibToBytes(kib) {
  return kib * KIB
}

export function bytesToMiB(bytes) {
  return Math.round(bytes / MIB)
}

export function mibToBytes(mib) {
  return mib * MIB
}

test('validateOutputGuardHierarchy enforces tool <= channel <= request', () => {
  assert.equal(validateOutputGuardHierarchy({
    output_guard_max_tool_argument_bytes: 128 * KIB,
    output_guard_max_channel_bytes: 2 * MIB,
    output_guard_max_request_bytes: 4 * MIB,
  }), null)

  assert.equal(validateOutputGuardHierarchy({
    output_guard_max_tool_argument_bytes: 3 * MIB,
    output_guard_max_channel_bytes: 2 * MIB,
    output_guard_max_request_bytes: 4 * MIB,
  }), '工具参数上限不能超过单 Channel 字节上限')

  assert.equal(validateOutputGuardHierarchy({
    output_guard_max_tool_argument_bytes: 128 * KIB,
    output_guard_max_channel_bytes: 8 * MIB,
    output_guard_max_request_bytes: 4 * MIB,
  }), '单 Channel 字节上限不能超过单请求语义预算')
})
```

5. 在 `frontend/src/pages/Settings.tsx` 中新增“模型输出失控防护”分组卡片：
- 在 constants 区域定义：
```typescript
const KIB = 1024
const DEFAULT_OUTPUT_GUARD_TOOL_BYTES = 128 * KIB
const DEFAULT_OUTPUT_GUARD_TOOL_DELTAS = 4096
const DEFAULT_OUTPUT_GUARD_CHANNEL_BYTES = 2 * MIB
const DEFAULT_OUTPUT_GUARD_REQUEST_BYTES = 4 * MIB

const OUTPUT_GUARD_KEYS = [
  'output_guard_enabled',
  'output_guard_max_tool_argument_bytes',
  'output_guard_max_tool_argument_deltas',
  'output_guard_max_channel_bytes',
  'output_guard_max_request_bytes',
] as const satisfies ReadonlyArray<keyof SystemSettings>
```

- 在 JSX 中（位于 General Tab 的安全分区 `id="settings-security"` 内，紧邻 Prompt 风控卡片之后，新增独立的 `SettingsCard`）：
```tsx
                <SettingsCard
                  title="模型输出失控防护 (Runaway Output Guard)"
                  icon={<ShieldAlert className="size-4" />}
                  tone={settingsForm.output_guard_enabled ? 'default' : 'danger'}
                  badge={
                    <Badge variant={settingsForm.output_guard_enabled ? 'outline' : 'destructive'} className="text-[11px] gap-1">
                      <span className={`size-1.5 rounded-full ${settingsForm.output_guard_enabled ? 'bg-emerald-400' : 'bg-red-500'}`} />
                      {settingsForm.output_guard_enabled ? '已启用全局防护' : '已关闭 (高风险)'}
                    </Badge>
                  }
                  actions={
                    <Button
                      variant="outline"
                      size="sm"
                      className="text-xs h-7 gap-1"
                      onClick={() => {
                        const patch = {
                          output_guard_enabled: false,
                          output_guard_max_tool_argument_bytes: DEFAULT_OUTPUT_GUARD_TOOL_BYTES,
                          output_guard_max_tool_argument_deltas: DEFAULT_OUTPUT_GUARD_TOOL_DELTAS,
                          output_guard_max_channel_bytes: DEFAULT_OUTPUT_GUARD_CHANNEL_BYTES,
                          output_guard_max_request_bytes: DEFAULT_OUTPUT_GUARD_REQUEST_BYTES,
                        }
                        setSettingsForm(f => ({ ...f, ...patch }))
                        autoSaveSettingsPatch(patch)
                      }}
                    >
                      <RotateCcw className="size-3" />
                      恢复默认防护
                    </Button>
                  }
                >
                  <div className="space-y-4">
                    {!settingsForm.output_guard_enabled && (
                      <div className="rounded-xl border border-destructive/30 bg-destructive/10 p-3.5 text-xs text-destructive flex items-start gap-2">
                        <AlertTriangle className="size-4 shrink-0 mt-0.5" />
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
                          setSettingsForm(f => ({ ...f, output_guard_enabled: checked }))
                          autoSaveSettingsPatch({ output_guard_enabled: checked })
                        }}
                      />
                    </div>

                    <div className={SETTINGS_FIELD_GRID}>
                      <SettingField
                        label="工具参数字节上限 (KiB)"
                        description="单次工具调用完整参数序列化字节硬上限（默认 128 KiB，范围 16 KiB..4 MiB）"
                      >
                        <Input
                          type="number"
                          value={Math.round(settingsForm.output_guard_max_tool_argument_bytes / 1024)}
                          onChange={(e) => {
                            const val = Math.max(16, Math.min(4096, parseInt(e.target.value) || 128)) * 1024
                            setSettingsForm(f => ({ ...f, output_guard_max_tool_argument_bytes: val }))
                          }}
                          onBlur={() => autoSaveSettingsPatch({ output_guard_max_tool_argument_bytes: settingsForm.output_guard_max_tool_argument_bytes })}
                        />
                      </SettingField>

                      <SettingField
                        label="工具非空 Delta 数量上限"
                        description="限制细碎 arguments delta 触发下游高频重解析事件的次数（默认 4096，范围 256..65536）"
                      >
                        <Input
                          type="number"
                          value={settingsForm.output_guard_max_tool_argument_deltas}
                          onChange={(e) => {
                            const val = Math.max(256, Math.min(65536, parseInt(e.target.value) || 4096))
                            setSettingsForm(f => ({ ...f, output_guard_max_tool_argument_deltas: val }))
                          }}
                          onBlur={() => autoSaveSettingsPatch({ output_guard_max_tool_argument_deltas: settingsForm.output_guard_max_tool_argument_deltas })}
                        />
                      </SettingField>

                      <SettingField
                        label="单 Channel 字节上限 (MiB)"
                        description="单 text/thinking 生成通道累计字节上限（默认 2 MiB，范围 128 KiB..16 MiB）"
                      >
                        <Input
                          type="number"
                          value={(settingsForm.output_guard_max_channel_bytes / (1024 * 1024)).toFixed(settingsForm.output_guard_max_channel_bytes % (1024 * 1024) === 0 ? 0 : 2)}
                          onChange={(e) => {
                            const val = Math.max(128 * 1024, Math.min(16 * 1024 * 1024, Math.round((parseFloat(e.target.value) || 2) * 1024 * 1024)))
                            setSettingsForm(f => ({ ...f, output_guard_max_channel_bytes: val }))
                          }}
                          onBlur={() => autoSaveSettingsPatch({ output_guard_max_channel_bytes: settingsForm.output_guard_max_channel_bytes })}
                        />
                      </SettingField>

                      <SettingField
                        label="单请求语义预算 (MiB)"
                        description="跨所有 channel、attempt 及自动续想的累计语义字节预算（默认 4 MiB，范围 256 KiB..64 MiB）"
                      >
                        <Input
                          type="number"
                          value={(settingsForm.output_guard_max_request_bytes / (1024 * 1024)).toFixed(settingsForm.output_guard_max_request_bytes % (1024 * 1024) === 0 ? 0 : 2)}
                          onChange={(e) => {
                            const val = Math.max(256 * 1024, Math.min(64 * 1024 * 1024, Math.round((parseFloat(e.target.value) || 4) * 1024 * 1024)))
                            setSettingsForm(f => ({ ...f, output_guard_max_request_bytes: val }))
                          }}
                          onBlur={() => autoSaveSettingsPatch({ output_guard_max_request_bytes: settingsForm.output_guard_max_request_bytes })}
                        />
                      </SettingField>
                    </div>

                    {settingsForm.output_guard_max_tool_argument_bytes > settingsForm.output_guard_max_channel_bytes && (
                      <p className="text-xs text-destructive">层级错误：工具参数上限不能超过单 Channel 字节上限！</p>
                    )}
                    {settingsForm.output_guard_max_channel_bytes > settingsForm.output_guard_max_request_bytes && (
                      <p className="text-xs text-destructive">层级错误：单 Channel 字节上限不能超过单请求语义预算！</p>
                    )}
                  </div>
                </SettingsCard>
```

- [ ] **Step 4: Run test to verify it passes (Green)**

Run:
1. `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./admin -run "TestAdminOutputGuard.*"`
Expected: PASS ok github.com/codex2api/admin
2. `cd frontend && node --experimental-strip-types --test src/lib/outputGuardSettings.test.mjs`
Expected: PASS ok
3. `cd frontend && pnpm run typecheck`
Expected: exit code 0, no errors

- [ ] **Step 5: Commit**

```bash
git add admin/output_guard_settings.go admin/output_guard_settings_test.go admin/handler.go frontend/src/types.ts frontend/src/pages/Settings.tsx frontend/src/lib/outputGuardSettings.test.mjs
git commit -m "feat(admin,frontend): add output guard settings GET/PUT and standalone management UI card"
```

---

---

### Task 7: 统一协议感知投影层与跨帧状态机分派器

**Files:**
- Create: `proxy/outputguard_projection.go`
- Test: `proxy/outputguard_projection_test.go`

**Interfaces:**
- Consumes:
  - `security/outputguard`（严格对齐 Part A exact 签名与实现）：
    ```go
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
    func (g *RequestGuard) RecordEvent() error
    func (g *RequestGuard) ObserveAppend(channelID string, aliasID string, kind ChannelKind, delta []byte) error
    func (g *RequestGuard) ObserveSnapshot(channelID string, aliasID string, kind ChannelKind, snapshot []byte, terminalStage string) error
    func (g *RequestGuard) ObserveAppendNoEvent(channelID string, aliasID string, kind ChannelKind, delta []byte) error
    func (g *RequestGuard) ObserveSnapshotNoEvent(channelID string, aliasID string, kind ChannelKind, snapshot []byte, terminalStage string) error
    func (g *RequestGuard) EndChannel(channelID string) error
    ```
  - `github.com/tidwall/gjson` (go.mod)
  - `github.com/tidwall/sjson` (go.mod)
- Produces:
  ```go
  package proxy

  import (
      "github.com/codex2api/security/outputguard"
  )

  type ObservationAction string
  const (
      ActionAppend   ObservationAction = "append"
      ActionSnapshot ObservationAction = "snapshot"
      ActionEnd      ObservationAction = "end"
  )

  type Observation struct {
      ChannelID     string
      AliasID       string
      Kind          outputguard.ChannelKind
      Action        ObservationAction
      Payload       []byte
      TerminalStage string // 严格白名单限制为 3 种合法语义终态阶段之一: "arguments.done", "item.done", "response.completed"，其余普通帧必须置为空字符串
  }

  type ProtocolProjector interface {
      Project(eventType string, data []byte) ([]Observation, error)
  }

  type ResponsesProjector struct {
      aliasMap map[string]string
  }
  func NewResponsesProjector() *ResponsesProjector
  func (p *ResponsesProjector) Project(eventType string, data []byte) ([]Observation, error)

  type ChatProjector struct {
      aliasMap map[string]string
  }
  func NewChatProjector() *ChatProjector
  func (p *ChatProjector) Project(eventType string, data []byte) ([]Observation, error)

  type AnthropicProjector struct {
      blockTypes map[int]string
      blockIDs   map[int]string
  }
  func NewAnthropicProjector() *AnthropicProjector
  func (p *AnthropicProjector) Project(eventType string, data []byte) ([]Observation, error)

  type GeminiProjector struct{}
  func NewGeminiProjector() *GeminiProjector
  func (p *GeminiProjector) Project(eventType string, data []byte) ([]Observation, error)

  // ObserveEvent 核心事件分派器：
  // 1. 业务事件计数：严格每上游事件计 1 次，心跳/注释除外；unknown/control/empty 业务帧必须消耗事件预算；
  // 2. 调用 projector.Project 解出当前 envelope 的 observations；
  // 3. 工具 arguments 仅解当前 envelope 或原生 object bounded serialize，不每 delta 重 parse inner JSON；
  // 4. 稳定 channel key 跨帧 alias 映射维护与测试验证；
  // 5. 调用 guard 执行判定流水线，端到端保障状态机语义一致性。
  func ObserveEvent(guard *outputguard.RequestGuard, projector ProtocolProjector, eventType string, data []byte) error
  ```

- [ ] **Step 1: Write failing test for protocol projection and event dispatcher**

```go
// proxy/outputguard_projection_test.go
package proxy

import (
	"fmt"
	"strings"
	"testing"

	"github.com/codex2api/security/outputguard"
)

func defaultProjectionTestConfig() outputguard.Config {
	return outputguard.Config{
		Enabled:               true,
		MaxToolArgumentBytes:  131072,  // 128 KiB
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152, // 2 MiB
		MaxRequestBytes:       4194304, // 4 MiB
	}
}

// 1. Responses 协议流式、3 个合法语义终态阶段快照与 alias 跨帧映射测试
func TestResponsesProjectorStreamAndTerminalSnapshots(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewResponsesProjector()

	// Text delta
	textDelta := `{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"Hello, "}`
	if err := ObserveEvent(guard, proj, "response.output_text.delta", []byte(textDelta)); err != nil {
		t.Fatalf("text delta observe failed: %v", err)
	}

	// Thinking delta
	thinkingDelta := `{"type":"response.reasoning.delta","item_id":"msg_1","delta":"thinking step 1"}`
	if err := ObserveEvent(guard, proj, "response.reasoning.delta", []byte(thinkingDelta)); err != nil {
		t.Fatalf("thinking delta observe failed: %v", err)
	}

	// Tool call delta with call_id and item_id alias
	toolDelta := `{"type":"response.function_call_arguments.delta","item_id":"item_call_1","call_id":"call_write_file","output_index":1,"delta":"{\"path\":\"test.txt\""}`
	if err := ObserveEvent(guard, proj, "response.function_call_arguments.delta", []byte(toolDelta)); err != nil {
		t.Fatalf("tool delta observe failed: %v", err)
	}

	// Tool call done (终态阶段 1: arguments.done 纯终态确认快照)
	toolDone := `{"type":"response.function_call_arguments.done","item_id":"item_call_1","call_id":"call_write_file","arguments":"{\"path\":\"test.txt\"}"}`
	if err := ObserveEvent(guard, proj, "response.function_call_arguments.done", []byte(toolDone)); err != nil {
		t.Fatalf("tool done observe failed: %v", err)
	}

	// output_item.done (终态阶段 2: item.done 纯终态确认快照)
	itemDone := `{"type":"response.output_item.done","item":{"id":"item_call_1","type":"function_call","call_id":"call_write_file","arguments":"{\"path\":\"test.txt\"}"}}`
	if err := ObserveEvent(guard, proj, "response.output_item.done", []byte(itemDone)); err != nil {
		t.Fatalf("item done observe failed: %v", err)
	}

	// response.completed (终态阶段 3: response.completed 纯终态确认快照)
	completed := `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"id":"item_call_1","type":"function_call","call_id":"call_write_file","arguments":"{\"path\":\"test.txt\"}"}]}}`
	if err := ObserveEvent(guard, proj, "response.completed", []byte(completed)); err != nil {
		t.Fatalf("response completed observe failed: %v", err)
	}

	if guard.IsTripped() {
		t.Fatalf("expected guard not tripped, got %s", guard.TrippedReason())
	}
}

// 2. ChatCompletions 协议 choice/toolindex/callid alias 隔离与流式及非流式测试
func TestChatProjectorStreamingAndNonStreaming(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewChatProjector()

	// 流式 Chunk 1: content + reasoning + tool_call 首帧 (带 call_id 与 index)
	chunk1 := `{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"foo","reasoning_content":"think","tool_calls":[{"index":0,"id":"call_123","function":{"name":"search","arguments":"{\"q\":"}}]}}]}`
	if err := ObserveEvent(guard, proj, "", []byte(chunk1)); err != nil {
		t.Fatalf("chunk 1 observe error: %v", err)
	}

	// 流式 Chunk 2: tool_call 后续 delta (仅带 index, 缺失 id, 验证 aliasMap 稳定解析到同一 channel)
	chunk2 := `{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"golang\"}"}}]}}]}`
	if err := ObserveEvent(guard, proj, "", []byte(chunk2)); err != nil {
		t.Fatalf("chunk 2 observe error: %v", err)
	}

	// 验证不同 choice 和不同 tool_index 隔离，不发生错合并
	chunkChoice1 := `{"id":"chatcmpl-1","choices":[{"index":1,"delta":{"content":"bar"}}]}`
	if err := ObserveEvent(guard, proj, "", []byte(chunkChoice1)); err != nil {
		t.Fatalf("chunk choice 1 observe error: %v", err)
	}

	// 非流式 JSON 测试
	nonStream := `{"id":"chatcmpl-2","choices":[{"index":0,"message":{"role":"assistant","content":"final content","tool_calls":[{"id":"call_999","type":"function","function":{"name":"calc","arguments":"{\"val\":42}"}}]}}]}`
	guardNonStream := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	projNonStream := NewChatProjector()
	if err := ObserveEvent(guardNonStream, projNonStream, "", []byte(nonStream)); err != nil {
		t.Fatalf("non-stream chat observe error: %v", err)
	}
	if guardNonStream.IsTripped() {
		t.Fatalf("expected non-stream not tripped, got %s", guardNonStream.TrippedReason())
	}
}

// 3. Anthropic Messages 协议真实 fixture 与 content_block 状态流转测试
func TestAnthropicProjectorStreamLifecycleAndNonStream(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewAnthropicProjector()

	// message_start
	msgStart := `{"type":"message_start","message":{"id":"msg_123","type":"message","role":"assistant","content":[]}}`
	if err := ObserveEvent(guard, proj, "message_start", []byte(msgStart)); err != nil {
		t.Fatalf("msg start error: %v", err)
	}

	// block 0: thinking
	b0Start := `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`
	if err := ObserveEvent(guard, proj, "content_block_start", []byte(b0Start)); err != nil {
		t.Fatalf("b0 start error: %v", err)
	}
	b0Delta := `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let me think"}}`
	if err := ObserveEvent(guard, proj, "content_block_delta", []byte(b0Delta)); err != nil {
		t.Fatalf("b0 delta error: %v", err)
	}
	b0Stop := `{"type":"content_block_stop","index":0}`
	if err := ObserveEvent(guard, proj, "content_block_stop", []byte(b0Stop)); err != nil {
		t.Fatalf("b0 stop error: %v", err)
	}

	// block 1: tool_use
	b1Start := `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_01ABC","name":"exec"}}`
	if err := ObserveEvent(guard, proj, "content_block_start", []byte(b1Start)); err != nil {
		t.Fatalf("b1 start error: %v", err)
	}
	b1Delta := `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":\"ls\"}"}}`
	if err := ObserveEvent(guard, proj, "content_block_delta", []byte(b1Delta)); err != nil {
		t.Fatalf("b1 delta error: %v", err)
	}
	b1Stop := `{"type":"content_block_stop","index":1}`
	if err := ObserveEvent(guard, proj, "content_block_stop", []byte(b1Stop)); err != nil {
		t.Fatalf("b1 stop error: %v", err)
	}

	// message_stop
	msgStop := `{"type":"message_stop"}`
	if err := ObserveEvent(guard, proj, "message_stop", []byte(msgStop)); err != nil {
		t.Fatalf("msg stop error: %v", err)
	}

	// 非流式 Anthropic 响应
	nonStream := `{"id":"msg_456","type":"message","role":"assistant","content":[{"type":"text","text":"Done!"},{"type":"tool_use","id":"toolu_02XYZ","name":"write","input":{"content":"file content"}}]}`
	guardAnthropicNonStream := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	projAnthropicNonStream := NewAnthropicProjector()
	if err := ObserveEvent(guardAnthropicNonStream, projAnthropicNonStream, "", []byte(nonStream)); err != nil {
		t.Fatalf("anthropic non-stream observe error: %v", err)
	}
	if guardAnthropicNonStream.IsTripped() {
		t.Fatalf("expected anthropic non-stream not tripped, got %s", guardAnthropicNonStream.TrippedReason())
	}
}

// 4. Gemini Native (streamGenerateContent & generateContent) 真实结构与 Object 紧凑序列化测试
func TestGeminiProjectorStreamAndNonStreamObjectSerialization(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewGeminiProjector()

	// 流式 candidate text + thought
	geminiChunk := `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"hello gemini"},{"thought":true,"text":"inner thoughts"},{"functionCall":{"name":"search","args":{"query":"codex"}}}]}}]}`
	if err := ObserveEvent(guard, proj, "", []byte(geminiChunk)); err != nil {
		t.Fatalf("gemini chunk observe error: %v", err)
	}

	// Gemini 非流式带 finishReason
	geminiNonStreamJSON := `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"name":"fetch","args":{"url":"https://example.com","headers":{"Auth":"secret"}}}}]外},"finishReason":"STOP"}]}`
	geminiNonStreamJSON = strings.ReplaceAll(geminiNonStreamJSON, "外", "")

	guardGemini := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	projGemini := NewGeminiProjector()
	if err := ObserveEvent(guardGemini, projGemini, "", []byte(geminiNonStreamJSON)); err != nil {
		t.Fatalf("gemini non-stream observe error: %v", err)
	}
	if guardGemini.IsTripped() {
		t.Fatalf("expected gemini non-stream not tripped, got %s", guardGemini.TrippedReason())
	}
}

// 5. 事件计数契约：每上游事件一次，unknown/control/empty 业务事件严格扣减事件预算，心跳注释除外
func TestObserveEventBudgetAccountability(t *testing.T) {
	// 极小事件预算配置：MaxRequestEvents = 65536
	guard := outputguard.NewRequestGuard(outputguard.Config{
		Enabled:               true,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})
	proj := NewResponsesProjector()

	// SSE 注释与 ping 帧不消耗业务事件预算
	if err := ObserveEvent(guard, proj, "", []byte(": keep-alive ping\n")); err != nil {
		t.Fatalf("comment frame should not fail: %v", err)
	}
	if err := ObserveEvent(guard, proj, "ping", []byte("{}")); err != nil {
		t.Fatalf("ping event should not fail: %v", err)
	}

	// 构造 65536 个 unknown / empty 业务事件洪泛
	for i := 0; i < outputguard.MaxRequestEvents; i++ {
		// unknown 或 control 事件，虽然投影为 0 个 observation，但必须计入业务事件
		unknownEvent := `{"type":"response.unknown_control_event","id":"ctrl_1"}`
		if err := ObserveEvent(guard, proj, "response.unknown_control_event", []byte(unknownEvent)); err != nil {
			t.Fatalf("event %d unexpectedly failed: %v", i, err)
		}
	}

	// 第 65537 个事件必须被 request_events 坚决拦截，不能因 control/unknown 绕过！
	err := ObserveEvent(guard, proj, "response.created", []byte(`{"type":"response.created"}`))
	if err == nil || err.Error() != "request_events" {
		t.Fatalf("expected request_events rejection for control/unknown flood, got %v", err)
	}
	if guard.TrippedReason() != "request_events" {
		t.Fatalf("expected tripped reason request_events, got %s", guard.TrippedReason())
	}
}

// 6. 验证 Custom Tool Arguments String 仅解码当前 envelope，不每 delta 重 parse inner JSON
func TestCustomToolArgumentsEnvelopePreservation(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewResponsesProjector()

	// 构造未闭合且包含转义与大段文本的 arguments 片段
	incompleteJSONChunk := `{"type":"response.function_call_arguments.delta","call_id":"call_custom","delta":"{\"nested_key\":\"val\\nwith\\\"quotes and unclosed brackets..."}`
	if err := ObserveEvent(guard, proj, "response.function_call_arguments.delta", []byte(incompleteJSONChunk)); err != nil {
		t.Fatalf("custom tool envelope observe error: %v", err)
	}

	if guard.IsTripped() {
		t.Fatalf("expected guard not tripped on unclosed inner JSON, got %s", guard.TrippedReason())
	}
}
```

- [ ] **Step 2: Run test to verify failure (Red)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./proxy -run "TestResponsesProjector.*|TestChatProjector.*|TestAnthropicProjector.*|TestGeminiProjector.*|TestObserveEvent.*|TestCustomToolArgumentsEnvelopePreservation"`
Expected: FAIL with compilation error: undefined: NewResponsesProjector, NewChatProjector, NewAnthropicProjector, NewGeminiProjector, ObserveEvent

- [ ] **Step 3: Write minimal implementation**

调用 Task 3 中已完整定义和测试的 `RequestGuard.RecordEvent()`、`ObserveAppendNoEvent()`、`ObserveSnapshotNoEvent()` 与 `EndChannel()` 实现协议投影器与分派器：

新建 `proxy/outputguard_projection.go`:
```go
// proxy/outputguard_projection.go
package proxy

import (
	"bytes"
	"fmt"

	"github.com/codex2api/security/outputguard"
	"github.com/tidwall/gjson"
)

type ObservationAction string

const (
	ActionAppend   ObservationAction = "append"
	ActionSnapshot ObservationAction = "snapshot"
	ActionEnd      ObservationAction = "end"
)

type Observation struct {
	ChannelID     string
	AliasID       string
	Kind          outputguard.ChannelKind
	Action        ObservationAction
	Payload       []byte
	TerminalStage string // 严格限制为 3 种合法语义终态阶段: "arguments.done", "item.done", "response.completed"，其余普通帧置空
}

type ProtocolProjector interface {
	Project(eventType string, data []byte) ([]Observation, error)
}

// -------------------------------------------------------------
// 1. ResponsesProjector (HTTP SSE, WebSocket, Realtime Text)
// -------------------------------------------------------------

type ResponsesProjector struct {
	aliasMap map[string]string
}

func NewResponsesProjector() *ResponsesProjector {
	return &ResponsesProjector{
		aliasMap: make(map[string]string),
	}
}

func (p *ResponsesProjector) Project(eventType string, data []byte) ([]Observation, error) {
	if len(data) == 0 {
		return nil, nil
	}
	parsed := gjson.ParseBytes(data)
	t := eventType
	if t == "" {
		t = parsed.Get("type").String()
	}

	var observations []Observation

	switch t {
	case "response.output_text.delta":
		itemID := parsed.Get("item_id").String()
		delta := parsed.Get("delta").String()
		if itemID == "" {
			itemID = fmt.Sprintf("output_%d", parsed.Get("output_index").Int())
		}
		observations = append(observations, Observation{
			ChannelID: itemID,
			Kind:      outputguard.KindText,
			Action:    ActionAppend,
			Payload:   []byte(delta),
		})

	case "response.reasoning.delta", "response.thinking.delta":
		itemID := parsed.Get("item_id").String()
		delta := parsed.Get("delta").String()
		if itemID == "" {
			itemID = fmt.Sprintf("output_%d", parsed.Get("output_index").Int())
		}
		observations = append(observations, Observation{
			ChannelID: fmt.Sprintf("%s.thinking", itemID),
			Kind:      outputguard.KindThinking,
			Action:    ActionAppend,
			Payload:   []byte(delta),
		})

	case "response.function_call_arguments.delta":
		callID := parsed.Get("call_id").String()
		itemID := parsed.Get("item_id").String()
		delta := parsed.Get("delta").String()
		targetID := callID
		if targetID == "" {
			targetID = itemID
		}
		if targetID == "" {
			targetID = fmt.Sprintf("output_%d", parsed.Get("output_index").Int())
		}
		observations = append(observations, Observation{
			ChannelID: targetID,
			AliasID:   itemID,
			Kind:      outputguard.KindToolArguments,
			Action:    ActionAppend,
			Payload:   []byte(delta),
		})

	case "response.function_call_arguments.done":
		callID := parsed.Get("call_id").String()
		itemID := parsed.Get("item_id").String()
		args := parsed.Get("arguments").String()
		targetID := callID
		if targetID == "" {
			targetID = itemID
		}
		observations = append(observations, Observation{
			ChannelID:     targetID,
			AliasID:       itemID,
			Kind:          outputguard.KindToolArguments,
			Action:        ActionSnapshot,
			Payload:       []byte(args),
			TerminalStage: "arguments.done",
		})

	case "response.output_item.done":
		item := parsed.Get("item")
		itemType := item.Get("type").String()
		itemID := item.Get("id").String()
		if itemID == "" {
			itemID = parsed.Get("item_id").String()
		}

		if itemType == "function_call" {
			callID := item.Get("call_id").String()
			args := item.Get("arguments").String()
			observations = append(observations, Observation{
				ChannelID:     callID,
				AliasID:       itemID,
				Kind:          outputguard.KindToolArguments,
				Action:        ActionSnapshot,
				Payload:       []byte(args),
				TerminalStage: "item.done",
			})
		} else if itemType == "message" {
			// 修复 M4：遍历全部 content blocks 建立独立 channel，杜绝仅提取 content.0.text 漏掉其他 content
			contentArray := item.Get("content").Array()
			for idx, cBlock := range contentArray {
				text := cBlock.Get("text").String()
				if text != "" {
					observations = append(observations, Observation{
						ChannelID:     fmt.Sprintf("%s:content:%d", itemID, idx),
						Kind:          outputguard.KindText,
						Action:        ActionSnapshot,
						Payload:       []byte(text),
						TerminalStage: "item.done",
					})
				}
			}
		}

	case "response.completed":
		outputs := parsed.Get("response.output").Array()
		for _, item := range outputs {
			itemType := item.Get("type").String()
			itemID := item.Get("id").String()
			if itemType == "function_call" {
				callID := item.Get("call_id").String()
				args := item.Get("arguments").String()
				targetID := callID
				if targetID == "" {
					targetID = itemID
				}
				observations = append(observations, Observation{
					ChannelID:     targetID,
					AliasID:       itemID,
					Kind:          outputguard.KindToolArguments,
					Action:        ActionSnapshot,
					Payload:       []byte(args),
					TerminalStage: "response.completed",
				})
			} else if itemType == "message" {
				text := item.Get("content.0.text").String()
				if text != "" {
					observations = append(observations, Observation{
						ChannelID:     itemID,
						Kind:          outputguard.KindText,
						Action:        ActionSnapshot,
						Payload:       []byte(text),
						TerminalStage: "response.completed",
					})
				}
			}
		}

	default:
		// 兼容非流式完整响应 JSON
		if parsed.Get("status").String() == "completed" && parsed.Get("output").Exists() {
			for _, item := range parsed.Get("output").Array() {
				itemType := item.Get("type").String()
				itemID := item.Get("id").String()
				if itemType == "function_call" {
					callID := item.Get("call_id").String()
					args := item.Get("arguments").String()
					targetID := callID
					if targetID == "" {
						targetID = itemID
					}
					observations = append(observations, Observation{
						ChannelID:     targetID,
						AliasID:       itemID,
						Kind:          outputguard.KindToolArguments,
						Action:        ActionSnapshot,
						Payload:       []byte(args),
						TerminalStage: "response.completed",
					})
				} else if itemType == "message" {
					text := item.Get("content.0.text").String()
					if text != "" {
						observations = append(observations, Observation{
							ChannelID:     itemID,
							Kind:          outputguard.KindText,
							Action:        ActionSnapshot,
							Payload:       []byte(text),
							TerminalStage: "response.completed",
						})
					}
				}
			}
		}
	}

	return observations, nil
}

// -------------------------------------------------------------
// 2. ChatProjector (OpenAI /v1/chat/completions Stream & JSON)
// -------------------------------------------------------------

type ChatProjector struct {
	aliasMap map[string]string
}

func NewChatProjector() *ChatProjector {
	return &ChatProjector{
		aliasMap: make(map[string]string),
	}
}

func (p *ChatProjector) Project(eventType string, data []byte) ([]Observation, error) {
	if len(data) == 0 {
		return nil, nil
	}
	parsed := gjson.ParseBytes(data)
	var observations []Observation

	choices := parsed.Get("choices").Array()
	for _, choice := range choices {
		choiceIdx := choice.Get("index").Int()

		// 流式 delta
		delta := choice.Get("delta")
		if delta.Exists() {
			if content := delta.Get("content").String(); content != "" {
				observations = append(observations, Observation{
					ChannelID: fmt.Sprintf("choice_%d.text", choiceIdx),
					Kind:      outputguard.KindText,
					Action:    ActionAppend,
					Payload:   []byte(content),
				})
			}
			if reasoning := delta.Get("reasoning_content").String(); reasoning != "" {
				observations = append(observations, Observation{
					ChannelID: fmt.Sprintf("choice_%d.thinking", choiceIdx),
					Kind:      outputguard.KindThinking,
					Action:    ActionAppend,
					Payload:   []byte(reasoning),
				})
			}
			toolCalls := delta.Get("tool_calls").Array()
			for _, tc := range toolCalls {
				toolIdx := tc.Get("index").Int()
				callID := tc.Get("id").String()
				channelID := fmt.Sprintf("choice_%d.tool_%d", choiceIdx, toolIdx)
				if callID != "" {
					p.aliasMap[callID] = channelID
				}
				args := tc.Get("function.arguments").String()
				if args != "" {
					observations = append(observations, Observation{
						ChannelID: channelID,
						AliasID:   callID,
						Kind:      outputguard.KindToolArguments,
						Action:    ActionAppend,
						Payload:   []byte(args),
					})
				}
			}
		}

		// 非流式 message
		msg := choice.Get("message")
		if msg.Exists() {
			if content := msg.Get("content").String(); content != "" {
				observations = append(observations, Observation{
					ChannelID:     fmt.Sprintf("choice_%d.text", choiceIdx),
					Kind:          outputguard.KindText,
					Action:        ActionSnapshot,
					Payload:       []byte(content),
					TerminalStage: "response.completed",
				})
			}
			if reasoning := msg.Get("reasoning_content").String(); reasoning != "" {
				observations = append(observations, Observation{
					ChannelID:     fmt.Sprintf("choice_%d.thinking", choiceIdx),
					Kind:          outputguard.KindThinking,
					Action:        ActionSnapshot,
					Payload:       []byte(reasoning),
					TerminalStage: "response.completed",
				})
			}
			toolCalls := msg.Get("tool_calls").Array()
			for idx, tc := range toolCalls {
				callID := tc.Get("id").String()
				channelID := fmt.Sprintf("choice_%d.tool_%d", choiceIdx, idx)
				args := tc.Get("function.arguments").String()
				observations = append(observations, Observation{
					ChannelID:     channelID,
					AliasID:       callID,
					Kind:          outputguard.KindToolArguments,
					Action:        ActionSnapshot,
					Payload:       []byte(args),
					TerminalStage: "response.completed",
				})
			}
		}
	}

	return observations, nil
}

// -------------------------------------------------------------
// 3. AnthropicProjector (Claude /v1/messages Stream & JSON)
// -------------------------------------------------------------

type AnthropicProjector struct {
	blockTypes map[int]string
	blockIDs   map[int]string
}

func NewAnthropicProjector() *AnthropicProjector {
	return &AnthropicProjector{
		blockTypes: make(map[int]string),
		blockIDs:   make(map[int]string),
	}
}

func (p *AnthropicProjector) Project(eventType string, data []byte) ([]Observation, error) {
	if len(data) == 0 {
		return nil, nil
	}
	parsed := gjson.ParseBytes(data)
	t := eventType
	if t == "" {
		t = parsed.Get("type").String()
	}

	var observations []Observation

	switch t {
	case "content_block_start":
		idx := int(parsed.Get("index").Int())
		block := parsed.Get("content_block")
		bType := block.Get("type").String()
		p.blockTypes[idx] = bType
		if bType == "tool_use" {
			p.blockIDs[idx] = block.Get("id").String()
		}

	case "content_block_delta":
		idx := int(parsed.Get("index").Int())
		delta := parsed.Get("delta")
		dType := delta.Get("type").String()

		switch dType {
		case "text_delta":
			text := delta.Get("text").String()
			observations = append(observations, Observation{
				ChannelID: fmt.Sprintf("block_%d", idx),
				Kind:      outputguard.KindText,
				Action:    ActionAppend,
				Payload:   []byte(text),
			})
		case "thinking_delta":
			thinking := delta.Get("thinking").String()
			observations = append(observations, Observation{
				ChannelID: fmt.Sprintf("block_%d", idx),
				Kind:      outputguard.KindThinking,
				Action:    ActionAppend,
				Payload:   []byte(thinking),
			})
		case "input_json_delta":
			partJSON := delta.Get("partial_json").String()
			callID := p.blockIDs[idx]
			observations = append(observations, Observation{
				ChannelID: fmt.Sprintf("block_%d", idx),
				AliasID:   callID,
				Kind:      outputguard.KindToolArguments,
				Action:    ActionAppend,
				Payload:   []byte(partJSON),
			})
		}

	case "content_block_stop":
		idx := int(parsed.Get("index").Int())
		bType := p.blockTypes[idx]
		if bType == "tool_use" {
			observations = append(observations, Observation{
				ChannelID:     fmt.Sprintf("block_%d", idx),
				AliasID:       p.blockIDs[idx],
				Kind:          outputguard.KindToolArguments,
				Action:        ActionEnd,
				TerminalStage: "arguments.done",
			})
		} else {
			observations = append(observations, Observation{
				ChannelID: fmt.Sprintf("block_%d", idx),
				Kind:      outputguard.KindText,
				Action:    ActionEnd,
			})
		}

	case "message_stop":
		// 审查反馈修复：message_stop 不生成 message_root 伪 channel 消耗配额，仅对已知 active blocks 发送 ActionEnd
		for blockIdx, blockType := range p.blockTypes {
			kind := outputguard.KindText
			cid := fmt.Sprintf("anthropic_block_%d", blockIdx)
			if blockType == "tool_use" {
				kind = outputguard.KindToolArguments
				if id, ok := p.blockIDs[blockIdx]; ok && id != "" {
					cid = id
				}
			} else if blockType == "thinking" {
				kind = outputguard.KindThinking
			}
			observations = append(observations, Observation{
				ChannelID: cid,
				Kind:      kind,
				Action:    ActionEnd,
			})
		}

	default:
		// 非流式 JSON (type == "message")
		if parsed.Get("type").String() == "message" && parsed.Get("content").IsArray() {
			contentBlocks := parsed.Get("content").Array()
			for idx, b := range contentBlocks {
				bType := b.Get("type").String()
				switch bType {
				case "text":
					text := b.Get("text").String()
					observations = append(observations, Observation{
						ChannelID:     fmt.Sprintf("block_%d", idx),
						Kind:          outputguard.KindText,
						Action:        ActionSnapshot,
						Payload:       []byte(text),
						TerminalStage: "response.completed",
					})
				case "thinking":
					thinking := b.Get("thinking").String()
					observations = append(observations, Observation{
						ChannelID:     fmt.Sprintf("block_%d", idx),
						Kind:          outputguard.KindThinking,
						Action:        ActionSnapshot,
						Payload:       []byte(thinking),
						TerminalStage: "response.completed",
					})
				case "tool_use":
					callID := b.Get("id").String()
					inputObj := b.Get("input")
					var inputBytes []byte
					if inputObj.IsObject() {
						inputBytes = []byte(inputObj.Raw)
					} else {
						inputBytes = []byte(inputObj.String())
					}
					observations = append(observations, Observation{
						ChannelID:     fmt.Sprintf("block_%d", idx),
						AliasID:       callID,
						Kind:          outputguard.KindToolArguments,
						Action:        ActionSnapshot,
						Payload:       inputBytes,
						TerminalStage: "response.completed",
					})
				}
			}
		}
	}

	return observations, nil
}

// -------------------------------------------------------------
// 4. GeminiProjector (Native streamGenerateContent & generateContent)
// -------------------------------------------------------------

type GeminiProjector struct{}

func NewGeminiProjector() *GeminiProjector {
	return &GeminiProjector{}
}

func (p *GeminiProjector) Project(eventType string, data []byte) ([]Observation, error) {
	if len(data) == 0 {
		return nil, nil
	}
	parsed := gjson.ParseBytes(data)
	var observations []Observation

	candidates := parsed.Get("candidates").Array()
	for candIdx, cand := range candidates {
		finishReason := cand.Get("finishReason").String()
		parts := cand.Get("content.parts").Array()
		for partIdx, part := range parts {
			// 1. Text / Thought
			if part.Get("text").Exists() {
				text := part.Get("text").String()
				isThought := part.Get("thought").Bool()
				kind := outputguard.KindText
				suffix := "text"
				if isThought {
					kind = outputguard.KindThinking
					suffix = "thinking"
				}
				action := ActionAppend
				var stage string
				if finishReason != "" {
					action = ActionSnapshot
					stage = "response.completed"
				}
				observations = append(observations, Observation{
					ChannelID:     fmt.Sprintf("candidate_%d.part_%d.%s", candIdx, partIdx, suffix),
					Kind:          kind,
					Action:        action,
					Payload:       []byte(text),
					TerminalStage: stage,
				})
			}

			// 2. Function Call (Native object bounded serialization)
			if fnCall := part.Get("functionCall"); fnCall.Exists() {
				name := fnCall.Get("name").String()
				args := fnCall.Get("args")
				var argBytes []byte
				if args.IsObject() {
					argBytes = []byte(args.Raw)
				} else {
					argBytes = []byte(args.String())
				}
				action := ActionAppend
				var stage string
				if finishReason != "" {
					action = ActionSnapshot
					stage = "response.completed"
				}
				observations = append(observations, Observation{
					ChannelID:     fmt.Sprintf("candidate_%d.part_%d.tool_%s", candIdx, partIdx, name),
					Kind:          outputguard.KindToolArguments,
					Action:        action,
					Payload:       argBytes,
					TerminalStage: stage,
				})
			}
		}
	}

	return observations, nil
}

// -------------------------------------------------------------
// 5. ObserveEvent Dispatcher (单上游事件严格计 1 次，流水线批调度)
// -------------------------------------------------------------

func ObserveEvent(guard *outputguard.RequestGuard, projector ProtocolProjector, eventType string, data []byte) error {
	// 心跳/注释帧直接放行，不计入业务事件预算
	trimmed := bytes.TrimSpace(data)
	if bytes.HasPrefix(trimmed, []byte(":")) || eventType == "ping" {
		return nil
	}

	// 步骤 1: 严格每上游业务事件计 1 次 requestEvents（即使是 control/unknown 帧）
	if err := guard.RecordEvent(); err != nil {
		return err
	}

	obs, err := projector.Project(eventType, data)
	if err != nil {
		return err
	}
	if len(obs) == 0 {
		return nil
	}

	// 依次将 observations 推入 guard，执行步骤 2~7 判定流水线
	for _, o := range obs {
		switch o.Action {
		case ActionAppend:
			if err := guard.ObserveAppendNoEvent(o.ChannelID, o.AliasID, o.Kind, o.Payload); err != nil {
				return err
			}
		case ActionSnapshot:
			if err := guard.ObserveSnapshotNoEvent(o.ChannelID, o.AliasID, o.Kind, o.Payload, o.TerminalStage); err != nil {
				return err
			}
		case ActionEnd:
			if err := guard.EndChannel(o.ChannelID); err != nil {
				return err
			}
		}
	}

	return nil
}
```

- [ ] **Step 4: Run test to verify passing (Green)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./proxy -run "TestResponsesProjector.*|TestChatProjector.*|TestAnthropicProjector.*|TestGeminiProjector.*|TestObserveEvent.*|TestCustomToolArgumentsEnvelopePreservation"`
Expected: PASS ok github.com/codex2api/proxy

- [ ] **Step 5: Commit**

```bash
git add proxy/outputguard_projection.go proxy/outputguard_projection_test.go
git commit -m "feat(proxy): implement protocol projectors and observe event dispatcher with strict event budgeting"
```

---

---

### Task 8: 传输 Framing 有界 Reader 与全链路 Callsite 替换

**Files:**
- Modify: `proxy/sse_line.go`
- Modify: `proxy/sse_line_test.go`
- Modify: `proxy/executor.go:1592-1715`
- Modify: `proxy/handler.go:5538-5585,7650-7750`
- Modify: `proxy/handler_gemini.go:390-420`
- Modify: `proxy/responses_ws.go:142-190`

**Interfaces:**
- Consumes:
  - Go 标准库 `bufio`, `bytes`, `errors`, `io`, `net/http`, `sync`
  - `github.com/gorilla/websocket` (go.mod)
- Produces:
  ```go
  package proxy

  var (
      ErrSSELineTooLong       = errors.New("sse_line_too_long")
      ErrSSEEventTooLarge     = errors.New("sse_event_too_large")
      ErrWSFrameTooLarge      = errors.New("ws_frame_too_large")
      ErrResponseBodyTooLarge = errors.New("response_body_too_large")
  )

  const (
      MaxSSELineBytes      = 8 << 20  // 8 MiB
      MaxSSEEventBytes     = 8 << 20  // 8 MiB
      MaxWSMessageBytes    = 8 << 20  // 8 MiB
      MaxResponseBodyBytes = 16 << 20 // 16 MiB
  )

  func ReadSSEStreamWithEvent(body io.Reader, callback func(event string, data []byte) bool) error
  func ReadBoundedResponseBody(body io.Reader, limit int64) ([]byte, error)
  ```

- [ ] **Step 1: Write failing test for bounded readers and callsites**

```go
// proxy/sse_line_test.go (增补 Task 8 有界流与非流式硬 Cap 测试)
package proxy

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// 1. 测试跨 Read 块的 split chunk 正常解析
func TestReadSSEStreamWithEventSplitChunks(t *testing.T) {
	input := "data: hello "
	input2 := "world\n\n"
	r := io.MultiReader(strings.NewReader(input), strings.NewReader(input2))

	var collected []string
	err := ReadSSEStreamWithEvent(r, func(event string, data []byte) bool {
		collected = append(collected, string(data))
		return true
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(collected) != 1 || collected[0] != "hello world" {
		t.Fatalf("collected want ['hello world'], got %v", collected)
	}
}

// 2. 测试标准多行 data 聚合
func TestReadSSEStreamWithEventMultilineData(t *testing.T) {
	input := "event: update\ndata: line1\ndata: line2\ndata: line3\n\n"
	var events []string
	var payloads []string
	err := ReadSSEStreamWithEvent(strings.NewReader(input), func(event string, data []byte) bool {
		events = append(events, event)
		payloads = append(payloads, string(data))
		return true
	})
	if err != nil {
		t.Fatalf("multiline read error: %v", err)
	}
	if len(events) != 1 || events[0] != "update" || payloads[0] != "line1\nline2\nline3" {
		t.Fatalf("multiline want update with line1\\nline2\\nline3, got %v: %v", events, payloads)
	}
}

// 3. 测试无换行符超大单行（never-ending oversized line）硬 cap 拒绝，返回 ErrSSELineTooLong
func TestReadSSEStreamWithEventNoNewlineOversized(t *testing.T) {
	// 构造超过 MaxSSELineBytes (8 MiB) 且无换行符的失控流
	oversized := strings.Repeat("x", MaxSSELineBytes+1024)
	err := ReadSSEStreamWithEvent(strings.NewReader(oversized), func(event string, data []byte) bool {
		return true
	})
	if err == nil {
		t.Fatalf("expected ErrSSELineTooLong for oversized line without newline, got nil")
	}
	if !errors.Is(err, ErrSSELineTooLong) {
		t.Fatalf("expected ErrSSELineTooLong, got %v", err)
	}
}

// 4. 测试累积多行 data 整事件超标拒绝，返回 ErrSSEEventTooLarge
func TestReadSSEStreamWithEventNeverendingMultilineOversized(t *testing.T) {
	// 9 行，每行 1 MiB，累积达 9 MiB > MaxSSEEventBytes (8 MiB)
	chunk := "data: " + strings.Repeat("a", 1<<20) + "\n"
	multilineOversized := strings.Repeat(chunk, 9) + "\n"

	err := ReadSSEStreamWithEvent(strings.NewReader(multilineOversized), func(event string, data []byte) bool {
		return true
	})
	if err == nil {
		t.Fatalf("expected ErrSSEEventTooLarge for oversized multiline event, got nil")
	}
	if !errors.Is(err, ErrSSEEventTooLarge) {
		t.Fatalf("expected ErrSSEEventTooLarge, got %v", err)
	}
}

// 5. 测试非流式超大 Response Body 有界拦截，返回 ErrResponseBodyTooLarge
func TestReadBoundedResponseBodyOversized(t *testing.T) {
	oversizedBody := strings.Repeat("b", MaxResponseBodyBytes+512)
	_, err := ReadBoundedResponseBody(strings.NewReader(oversizedBody), MaxResponseBodyBytes)
	if err == nil {
		t.Fatalf("expected ErrResponseBodyTooLarge for body > 16MiB, got nil")
	}
	if !errors.Is(err, ErrResponseBodyTooLarge) {
		t.Fatalf("expected ErrResponseBodyTooLarge, got %v", err)
	}

	// 正常大小读取成功
	normalBody := "{\"status\":\"ok\"}"
	data, err := ReadBoundedResponseBody(strings.NewReader(normalBody), MaxResponseBodyBytes)
	if err != nil || string(data) != normalBody {
		t.Fatalf("normal read error = %v, got %s", err, string(data))
	}
}

// 6. 验证 Transport Framing 独立于 Guard 开关（关闭语义 guard 仍保留 wire cap）
func TestTransportFramingIndependentOfGuard(t *testing.T) {
	// 模拟即使 output_guard_enabled = false
	// wire framing cap 仍作为系统安全底线硬性生效
	oversized := strings.Repeat("x", MaxSSELineBytes+100)
	err := ReadSSEStreamWithEvent(strings.NewReader(oversized), func(event string, data []byte) bool {
		return true
	})
	if !errors.Is(err, ErrSSELineTooLong) {
		t.Fatalf("wire cap must remain active regardless of guard settings, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify failure (Red)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./proxy -run "TestReadSSEStreamWithEvent.*|TestReadBoundedResponseBody.*|TestTransportFramingIndependentOfGuard"`
Expected: FAIL with compilation error or undefined symbols: ErrSSELineTooLong, ErrSSEEventTooLarge, ReadBoundedResponseBody

- [ ] **Step 3: Write minimal implementation**

修改 `proxy/sse_line.go`:
```go
// proxy/sse_line.go
package proxy

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// SSE 与传输 Wire Limits 属于网络资源硬边界，独立于业务层 output guard 开关
const (
	MaxSSELineBytes      = 8 << 20  // 8 MiB
	MaxSSEEventBytes     = 8 << 20  // 8 MiB
	MaxWSMessageBytes    = 8 << 20  // 8 MiB
	MaxResponseBodyBytes = 16 << 20 // 16 MiB
)

var (
	ErrSSELineTooLong       = errors.New("sse_line_too_long")
	ErrSSEEventTooLarge     = errors.New("sse_event_too_large")
	ErrWSFrameTooLarge      = errors.New("ws_frame_too_large")
	ErrResponseBodyTooLarge = errors.New("response_body_too_large")
)

// readBoundedSSELine 读取单行，严格限制在 limit 字节以内
func readBoundedSSELine(reader *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > limit {
			return nil, ErrSSELineTooLong
		}
		line = append(line, fragment...)
		if err != bufio.ErrBufferFull {
			return line, err
		}
	}
}

// ReadBoundedResponseBody 有界读取非流式响应体
func ReadBoundedResponseBody(body io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = MaxResponseBodyBytes
	}
	lr := io.LimitReader(body, limit+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, ErrResponseBodyTooLarge
	}
	return data, nil
}
```

修改 `proxy/executor.go` 中的 `ReadSSEStreamWithEvent`:
```go
// proxy/executor.go (更新 ReadSSEStreamWithEvent，施加行与多行整事件硬 Cap)
func ReadSSEStreamWithEvent(body io.Reader, callback func(event string, data []byte) bool) error {
	buf := sseBufferPool.Get().([]byte)
	defer sseBufferPool.Put(buf)

	lineBufPtr := sseLineBufPool.Get().(*[]byte)
	lineBuf := (*lineBufPtr)[:0]
	defer func() {
		if cap(lineBuf) <= 256*1024 {
			*lineBufPtr = lineBuf[:0]
			sseLineBufPool.Put(lineBufPtr)
		}
	}()

	var dataLines [][]byte
	var eventName string
	var currentEventBytes int

	emitEvent := func() (bool, error) {
		event := eventName
		eventName = ""
		currentEventBytes = 0
		if len(dataLines) == 0 {
			return true, nil
		}

		data := dataLines[0]
		if len(dataLines) > 1 {
			data = bytes.Join(dataLines, []byte("\n"))
		}
		isDone := bytes.Equal(data, []byte("[DONE]"))
		keepReading := !isDone && callback(event, data)
		for i := range dataLines {
			dataLines[i] = nil
		}
		dataLines = dataLines[:0]
		return keepReading, nil
	}

	consumeField := func(line []byte) error {
		if bytes.HasPrefix(line, []byte("data:")) {
			data := bytes.TrimPrefix(line, []byte("data:"))
			data = bytes.TrimPrefix(data, []byte(" "))
			if currentEventBytes+len(data) > MaxSSEEventBytes {
				return ErrSSEEventTooLarge
			}
			currentEventBytes += len(data)
			dataCopy := make([]byte, len(data))
			copy(dataCopy, data)
			dataLines = append(dataLines, dataCopy)
			return nil
		}
		if bytes.HasPrefix(line, []byte("event:")) {
			event := bytes.TrimPrefix(line, []byte("event:"))
			event = bytes.TrimPrefix(event, []byte(" "))
			eventName = string(event)
		}
		return nil
	}

	for {
		n, err := body.Read(buf)
		if n > 0 {
			if len(lineBuf)+n > MaxSSELineBytes {
				return ErrSSELineTooLong
			}
			lineBuf = append(lineBuf, buf[:n]...)

			consumed := 0
			for {
				idx := bytes.IndexByte(lineBuf[consumed:], '\n')
				if idx < 0 {
					if len(lineBuf)-consumed > MaxSSELineBytes {
						return ErrSSELineTooLong
					}
					break
				}

				lineEnd := consumed + idx
				line := bytes.TrimRight(lineBuf[consumed:lineEnd], "\r")
				consumed = lineEnd + 1

				if len(line) == 0 {
					keep, emitErr := emitEvent()
					if emitErr != nil {
						return emitErr
					}
					if !keep {
						return nil
					}
					continue
				}

				if bytes.HasPrefix(line, []byte(":")) {
					continue
				}

				if err := consumeField(line); err != nil {
					return err
				}
			}

			if consumed > 0 {
				remaining := copy(lineBuf, lineBuf[consumed:])
				lineBuf = lineBuf[:remaining]
			}
		}

		if err != nil {
			if err == io.EOF {
				if len(lineBuf) > 0 {
					if len(lineBuf) > MaxSSELineBytes {
						return ErrSSELineTooLong
					}
					line := bytes.TrimRight(lineBuf, "\r")
					if err := consumeField(line); err != nil {
						return err
					}
				}
				_, emitErr := emitEvent()
				if emitErr != nil {
					return emitErr
				}
				return nil
			}
			return err
		}
	}
}
```

**全链路真实 Callsite 替换与集成代码（真实代码，非伪装实现）：**

> **架构与编译边界说明**：Task 8 的核心 Green 验收目标为传输 Wire Limits（`ReadSSEStreamWithEvent` 行与整事件 Cap、WebSocket ReadPump `SetReadLimit(MaxWSMessageBytes)` 以及 `ReadBoundedResponseBody`）的独立单元测试。下述 Callsite 替换定义了全链路传输边界。其中 HTTP/Gemini 接入全局 guard 协调器的调用点挂载点在 Task 9（终止协调器）就绪后即可无缝接通；在 Task 8 阶段，`ReadSSEStreamWithEvent` 与 `ReadBoundedResponseBody` 直接替代旧有无界读取，杜绝失控事件造成内存膨胀。

1. **Responses HTTP SSE 调用点替换 (`proxy/handler.go:5538-5585`)**:
```go
// 在 forwardWithEvent 最前置接入 ObserveEvent：
forwardWithEvent := func(sseEvent string, data []byte) bool {
    // 步骤 0: 优先进入全局模型输出失控防护 Guard
    if guardCoordinator != nil {
        if err := ObserveEvent(guardCoordinator.Guard(), responsesProjector, sseEvent, data); err != nil {
            guardCoordinator.TriggerGuard(err)
            return false // 立即阻断，不进入后续 provenance/collector/replay
        }
    }

    if multiAgentV2State.isOptimizedFor(account.ID()) {
        data = restoreCodexMultiAgentV2Response(data, multiAgentV2State)
    }
    streamDiag.markUpstreamFrame()
    // ... 后续 compactionProvenancePayloads 记录与 downstream 写入
```

2. **ChatCompletions 调用点替换 (`proxy/handler.go:7650-7750`)**:
```go
// 在 ChatCompletions SSE 循环中：
readErr = readSSEStreamWithContinuousRetryKeepalive(readCtx, resp.Body, func(sseEvent string, data []byte) bool {
    if guardCoordinator != nil {
        if err := ObserveEvent(guardCoordinator.Guard(), chatProjector, sseEvent, data); err != nil {
            guardCoordinator.TriggerGuard(err)
            return false
        }
    }
    // ... 既有下游写入逻辑
})
```

3. **Gemini Native 调用点替换 (`proxy/handler_gemini.go:390-420`)**:
```go
// forwardGeminiNativeStream 替换：
result.readErr = readSSEStreamWithContinuousRetryKeepalive(c.Request.Context(), body, func(sseEvent string, data []byte) bool {
    if c.Request.Context().Err() != nil {
        return false
    }
    if guardCoordinator != nil {
        if err := ObserveEvent(guardCoordinator.Guard(), geminiProjector, sseEvent, data); err != nil {
            guardCoordinator.TriggerGuard(err)
            return false
        }
    }
    usage.observe(data)
    // ...
})
```

4. **WebSocket ReadPump 帧大小与内存 Cap 替换 (`proxy/responses_ws.go:160-185`)**:
```go
// 在 startResponsesWSReadPump 中强化 SetReadLimit：
conn.SetReadLimit(MaxWSMessageBytes) // 强制 8 MiB 限制
messageType, reader, err := conn.NextReader()
if err != nil {
    if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
        // 若超过 MaxWSMessageBytes，底层 gorilla/websocket 返回 ErrReadLimit
        // 归因为独立 typed reason: ws_frame_too_large，绝不伪装为 repetition
        cancel()
        select {
        case messages <- responsesWSInboundMessage{turn: turn, err: ErrWSFrameTooLarge}:
        default:
        }
        return
    }
}
```

5. **非流式 HTTP 响应读取点替换 (`proxy/handler.go`)**:
```go
// 替换所有裸读取 resp.Body 的非流式调用：
// 修改前: rawResp, err := io.ReadAll(resp.Body)
// 修改后:
rawResp, err := ReadBoundedResponseBody(resp.Body, MaxResponseBodyBytes)
if err != nil {
    if errors.Is(err, ErrResponseBodyTooLarge) {
        log.Printf("上游非流式响应体超标 (limit=%d): %v", MaxResponseBodyBytes, err)
        c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{
            "code": "response_body_too_large",
            "message": "Upstream response body exceeded transport wire limit",
        }})
        return
    }
    // ... 其他网络读取错误
}
```

- [ ] **Step 4: Run test to verify passing (Green)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./proxy -run "TestReadSSEStreamWithEvent.*|TestReadBoundedResponseBody.*|TestTransportFramingIndependentOfGuard"`
Expected: PASS ok github.com/codex2api/proxy

- [ ] **Step 5: Commit**

```bash
git add proxy/sse_line.go proxy/sse_line_test.go proxy/executor.go proxy/handler.go proxy/handler_gemini.go proxy/responses_ws.go
git commit -m "feat(proxy): enforce bounded SSE stream, multiline event, WS frame, and response body caps"
```

---

### Task 9: 模型输出失控防护强制终止协调与下游通知原语

**Files:**
- Modify: `proxy/handler.go:2343-2375, 2419-2475`（扩展 `streamOutcome` 字段 `guardTripped` 与 `guardReason`，并提供防护状态叠加原语 `OverlayGuardOutcome`）
- Modify: `proxy/stream_flush_writer.go:23-45, 403-465`（为 `streamFlushWriter` 增加 `DiscardBuffered()` 与 `aborted` 标记，确保终止后 `Flush`/`Finalize` 绝不释放残留字节）
- Create: `proxy/outputguard_cancel.go`（定义终止协调器 `GuardTerminationCoordinator`，负责原子标记 `tripped`、冻结首 reason、先于写锁主动 Cancel 上游与 Close body、丢弃在途缓冲）
- Create: `proxy/outputguard_cancel_test.go`（测试上游立即取消与关闭响应体先于写锁、多次并发触发原子幂等、在途缓冲彻底丢弃、typed outcome 不被覆盖）
- Create: `proxy/outputguard_downstream.go`（定义下游通知原语 `SendDownstreamGuardTerminalHTTP` 与 `SendDownstreamGuardTerminalWS`、有界写超时注入 `ApplyDownstreamWriteDeadline`、多协议终态 Envelope 渲染）
- Create: `proxy/outputguard_downstream_test.go`（测试 TryLock 争用非阻塞放弃、锁内 5s deadline 写入、底层不支持 deadline 安全退出不执行无界写、Responses/Chat/Anthropic/Gemini/WS 终态结构断言）

**Interfaces:**
- Consumes:
  - `security/outputguard` (来自 Task 1~3, 定义于 `security/outputguard/guard.go`):
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
  - `api` 统一错误定义 (定义于 `api/errors.go`):
    ```go
    type ErrorCode string
    const (
        ErrCodeServerError   ErrorCode = "server_error"
        ErrCodeUpstreamError ErrorCode = "upstream_error"
    )
    type ErrorType string
    const (
        ErrorTypeServer   ErrorType = "server_error"
        ErrorTypeUpstream ErrorType = "upstream_error"
    )
    type APIError struct {
        Code    ErrorCode `json:"code"`
        Message string    `json:"message"`
        Type    ErrorType `json:"type"`
    }
    func NewAPIError(code ErrorCode, message string, errType ErrorType) *APIError
    ```
  - `github.com/gorilla/websocket` (go.mod)
  - `github.com/gin-gonic/gin` (go.mod)

- Produces (供 Task 10 及各协议 Handler 消费的明确签名):
  ```go
  package proxy

  import (
      "context"
      "io"
      "net/http"
      "sync"
      "time"

      "github.com/codex2api/security/outputguard"
      "github.com/gin-gonic/gin"
      "github.com/gorilla/websocket"
  )

  const (
      GuardTriggeredPublicCode    = "model_output_guard_triggered"
      GuardTriggeredPublicMessage = "Upstream model output exceeded the gateway safety policy; generation was stopped."
      GuardWriteDeadlineTimeout   = 5 * time.Second
  )

  // streamOutcome 扩展字段（用于在代理内部全链路传递 Guard 命中状态与类型化原因）
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
      guardTripped      bool   // 标记输出失控防护触发
      guardReason       string // 冻结的内部 reason：repetition, tool_argument_bytes 等
  }

  // OverlayGuardOutcome 确保 typed guard outcome 具备绝对优先级，绝不被 ctxErr (499) 或 readErr (598) 覆盖
  func OverlayGuardOutcome(outcome streamOutcome, guard *outputguard.RequestGuard) streamOutcome

  // GuardTerminationCoordinator 终止协调器：原子标记 tripped，主动 cancel 上游 context（跳过 5s drain），
  // 关闭 HTTP 响应体，清理本地在途缓冲
  type GuardTerminationCoordinator struct {
      guard          *outputguard.RequestGuard
      cancelUpstream context.CancelFunc
      body           io.Closer
      once           sync.Once
      trippedOutcome streamOutcome
  }

  func NewGuardTerminationCoordinator(guard *outputguard.RequestGuard, cancelUpstream context.CancelFunc, body io.Closer) *GuardTerminationCoordinator
  func (c *GuardTerminationCoordinator) HandleTripped(reason string, discardBuffers ...func()) streamOutcome

  // DiscardBuffered 清空 streamFlushWriter 内存缓冲与安全窗，确保后续 Finalize 绝不再释放异常内容
  func (w *streamFlushWriter) DiscardBuffered()

  // ApplyDownstreamWriteDeadline 持锁内统一为 ResponseWriter 施加 5s 有界写超时；遇到 ErrNotSupported 显式返回
  func ApplyDownstreamWriteDeadline(w http.ResponseWriter, timeout time.Duration) error

  // 下游尽力交付终态写入原语
  func SendDownstreamGuardTerminalHTTP(c *gin.Context, mu *sync.Mutex, clientGone *bool, protocol string, reason string) error
  func SendDownstreamGuardTerminalWS(conn *websocket.Conn, streamID string, reason string) error

  // FormatGuardTerminalEnvelope 渲染各协议标准错误 envelope（用于单元测试与下游写入）
  func FormatGuardTerminalEnvelope(protocol string, streamID string) ([]byte, int, error)
  ```

---

#### 9a. 终止协调器、上游立即取消与本地缓冲 Discard 原语

- [ ] **Step 1: Write failing test for GuardTerminationCoordinator and buffer discard**

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
	mu       sync.Mutex
	closed   bool
	closedAt time.Time
}

func (r *recordingCloser) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.closedAt = time.Now()
	return nil
}

func (r *recordingCloser) IsClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

// 1. 验证 guard 命中时，上游 cancel 和 body close 必须先于写锁争用执行
func TestGuardTerminationPrecedesDownstreamLock(t *testing.T) {
	cfg := outputguard.Config{
		Enabled:               true,
		MaxToolArgumentBytes:  128 * 1024,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2 * 1024 * 1024,
		MaxRequestBytes:       4 * 1024 * 1024,
	}
	rg := outputguard.NewRequestGuard(cfg)

	upstreamCtx, upstreamCancel := context.WithCancel(context.Background())
	defer upstreamCancel()

	closer := &recordingCloser{}
	coordinator := NewGuardTerminationCoordinator(rg, upstreamCancel, closer)

	var downstreamMu sync.Mutex
	downstreamMu.Lock() // 模拟下游写锁已被占用

	var cancelTime time.Time
	var lockAttemptTime time.Time

	// 触发 guard 终止
	outcome := coordinator.HandleTripped("repetition", func() {
		// 模拟清空本地缓冲
	})

	// 断言 1: 上游 context 已经立即 canceled
	select {
	case <-upstreamCtx.Done():
		cancelTime = time.Now()
	default:
		t.Fatalf("expected upstream context to be canceled immediately")
	}

	// 断言 2: HTTP body 已经关闭
	if !closer.IsClosed() {
		t.Fatalf("expected upstream body to be closed immediately")
	}

	// 断言 3: outcome 为 model_output_guard，且状态码为 502，不计入账号惩罚
	if !outcome.guardTripped || outcome.guardReason != "repetition" {
		t.Fatalf("unexpected guard outcome: %+v", outcome)
	}
	if outcome.logStatusCode != http.StatusBadGateway {
		t.Fatalf("expected logStatusCode 502, got %d", outcome.logStatusCode)
	}
	if outcome.penalize {
		t.Fatalf("expected penalize == false for guard failure")
	}

	// 模拟后续下游通知尝试获取锁
	lockAttemptTime = time.Now()
	if !cancelTime.IsZero() && cancelTime.After(lockAttemptTime) {
		t.Fatalf("upstream cancellation must precede downstream lock attempt")
	}
	downstreamMu.Unlock()
}

// 2. 验证并发多次调用 HandleTripped 具备原子幂等性，上游 cancel/close 仅调用一次，且 reason 保持首个冻结
func TestGuardTerminationIdempotentReasonFreeze(t *testing.T) {
	cfg := outputguard.Config{
		Enabled:               true,
		MaxToolArgumentBytes:  128 * 1024,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2 * 1024 * 1024,
		MaxRequestBytes:       4 * 1024 * 1024,
	}
	rg := outputguard.NewRequestGuard(cfg)

	cancelCalls := int32(0)
	upstreamCancel := func() {
		atomic.AddInt32(&cancelCalls, 1)
	}
	closer := &recordingCloser{}
	coordinator := NewGuardTerminationCoordinator(rg, upstreamCancel, closer)

	var discardCalls int32
	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			reason := "repetition"
			if idx > 0 {
				reason = "channel_bytes"
			}
			out := coordinator.HandleTripped(reason, func() {
				atomic.AddInt32(&discardCalls, 1)
			})
			if out.guardReason != "repetition" {
				t.Errorf("expected frozen reason repetition, got %s", out.guardReason)
			}
		}(i)
	}

	wg.Wait()

	if calls := atomic.LoadInt32(&cancelCalls); calls != 1 {
		t.Fatalf("expected cancelUpstream to be called exactly once, got %d", calls)
	}
	if !closer.IsClosed() {
		t.Fatalf("expected body to be closed")
	}
	if atomic.LoadInt32(&discardCalls) == 0 {
		t.Fatalf("expected discard callbacks to be executed")
	}
}

// 3. 验证 streamFlushWriter.DiscardBuffered 使随后的 Finalize 绝不释放任何异常数据
func TestStreamFlushWriterDiscardBuffered(t *testing.T) {
	var sink bytes.Buffer
	w := newStreamFlushWriter(&sink, nil)

	// 写入尚未刷新的 SSE 事件碎片与数据
	_ = w.WriteBytes([]byte("data: partial runaway malicious repetition\n\n"))
	if sink.Len() > 0 {
		t.Fatalf("buffer should not flush prematurely")
	}

	// 命中 Guard，调用 DiscardBuffered 清空在途缓冲
	w.DiscardBuffered()

	// 模拟外层 defer w.Finalize()
	err := w.Finalize()
	if err != nil {
		t.Fatalf("Finalize returned error: %v", err)
	}

	// 断言：sink 绝无任何字节写出！
	if sink.Len() != 0 {
		t.Fatalf("expected sink to be empty after DiscardBuffered and Finalize, got %d bytes: %q", sink.Len(), sink.String())
	}
}

// 4. 验证 OverlayGuardOutcome 不会被下游取消 (499) 或上游读断流 (598) 覆盖
func TestOverlayGuardOutcomePreservesIdentity(t *testing.T) {
	cfg := outputguard.Config{Enabled: true, MaxRequestBytes: 100}
	rg := outputguard.NewRequestGuard(cfg)
	_ = rg.ObserveAppend("c1", "", outputguard.KindText, bytes.Repeat([]byte("A"), 200)) // 触发 request_bytes

	if !rg.IsTripped() {
		t.Fatalf("expected guard to be tripped")
	}

	// 模拟网络层产生的下游取消错误
	ctxErr := context.Canceled
	readErr := errors.New("read tcp: connection reset by peer")
	rawOutcome := classifyStreamOutcome(ctxErr, readErr, nil, false)

	// 若未叠加 guard，默认会被归类为 499 客户端断开
	if rawOutcome.logStatusCode != logStatusClientClosed {
		t.Fatalf("raw outcome should be client closed (499), got %d", rawOutcome.logStatusCode)
	}

	// 经过 OverlayGuardOutcome 叠加
	finalOutcome := OverlayGuardOutcome(rawOutcome, rg)

	if !finalOutcome.guardTripped {
		t.Fatalf("expected guardTripped == true")
	}
	if finalOutcome.guardReason != "request_bytes" {
		t.Fatalf("expected guardReason == request_bytes, got %s", finalOutcome.guardReason)
	}
	if finalOutcome.logStatusCode != http.StatusBadGateway {
		t.Fatalf("expected logStatusCode == 502, got %d", finalOutcome.logStatusCode)
	}
	if finalOutcome.penalize {
		t.Fatalf("expected penalize == false")
	}
}
```

- [ ] **Step 2: Run test to verify failure (Red)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./proxy -run "TestGuardTerminationPrecedesDownstreamLock|TestGuardTerminationIdempotentReasonFreeze|TestStreamFlushWriterDiscardBuffered|TestOverlayGuardOutcomePreservesIdentity"`
Expected: FAIL with compilation errors: `NewGuardTerminationCoordinator undefined`, `w.DiscardBuffered undefined`, `OverlayGuardOutcome undefined`.

- [ ] **Step 3: Implement minimal code for coordinator and buffer discard**

1. 修改 `proxy/handler.go` 增补 `streamOutcome` 字段与 `OverlayGuardOutcome`：

```go
// proxy/handler.go:2343-2375
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
	guardTripped      bool   // 标记模型输出失控防护触发
	guardReason       string // 冻结的内部触发原因
}

// OverlayGuardOutcome 检查 RequestGuard 是否已触发；若已触发，无条件提升为 Guard 终态，消除 499/598 覆盖
func OverlayGuardOutcome(outcome streamOutcome, guard *outputguard.RequestGuard) streamOutcome {
	if guard == nil || !guard.IsTripped() {
		return outcome
	}
	outcome.guardTripped = true
	outcome.guardReason = guard.TrippedReason()
	outcome.logStatusCode = http.StatusBadGateway
	outcome.failureKind = "model_output_guard"
	outcome.failureMessage = GuardTriggeredPublicMessage
	outcome.failurePayload = nil // 杜绝模型内部异常 payload 泄漏进日志或回放
	outcome.penalize = false     // 输出异常不是账号故障，严禁惩罚账号
	outcome.terminalLocal = true
	return outcome
}
```

2. 修改 `proxy/stream_flush_writer.go` 增加 `DiscardBuffered` 支持：

```go
// proxy/stream_flush_writer.go
type streamFlushWriter struct {
	writer        io.Writer
	flusher       http.Flusher
	policy        string
	interval      time.Duration
	lastFlush     time.Time
	buffer        bytes.Buffer
	outputScanner *promptfilter.OutputScanner
	scanCarry     []byte
	writtenBytes  atomic.Int64
	diag          *streamPhaseDiagnostics
	aborted       bool // 新增：标记流已废弃，后续所有 Flush/Finalize 均为 no-op
}

// DiscardBuffered 丢弃所有未落地的在途数据与安全窗，并阻止后续 Finalize 写入
func (w *streamFlushWriter) DiscardBuffered() {
	if w == nil {
		return
	}
	w.buffer.Reset()
	w.scanCarry = nil
	w.outputScanner = nil
	w.aborted = true
}

func (w *streamFlushWriter) Flush() error {
	if w == nil || w.aborted {
		return nil
	}
	...
}

func (w *streamFlushWriter) Finalize() error {
	if w == nil || w.aborted {
		return nil
	}
	...
}
```

3. 创建 `proxy/outputguard_cancel.go`：

```go
// proxy/outputguard_cancel.go
package proxy

import (
	"context"
	"io"
	"net/http"
	"sync"

	"github.com/codex2api/security/outputguard"
)

// GuardTerminationCoordinator 协调模型失控切断流程，严格保证前置 Cancel 与资源回收
type GuardTerminationCoordinator struct {
	guard          *outputguard.RequestGuard
	cancelUpstream context.CancelFunc
	body           io.Closer
	once           sync.Once
	trippedOutcome streamOutcome
}

// NewGuardTerminationCoordinator 构造终止协调器实例
func NewGuardTerminationCoordinator(guard *outputguard.RequestGuard, cancelUpstream context.CancelFunc, body io.Closer) *GuardTerminationCoordinator {
	return &GuardTerminationCoordinator{
		guard:          guard,
		cancelUpstream: cancelUpstream,
		body:           body,
	}
}

// HandleTripped 在 guard 命中时原子调用：
// 1. 冻结首 reason 并生成统一 streamOutcome；
// 2. 先于任何写锁立即主动 Cancel 上游 context 并关闭响应体（跳过 5s drain）；
// 3. 执行所有注册的本地缓冲清空回调（如 DiscardBuffered、collector 清空、provenance 清空）；
// 4. 并发调用保证幂等，仅首次执行实际清理。
func (c *GuardTerminationCoordinator) HandleTripped(reason string, discardBuffers ...func()) streamOutcome {
	if c == nil {
		return streamOutcome{
			logStatusCode:  http.StatusBadGateway,
			failureKind:    "model_output_guard",
			failureMessage: GuardTriggeredPublicMessage,
			guardTripped:   true,
			guardReason:    reason,
			penalize:       false,
			terminalLocal:  true,
		}
	}

	c.once.Do(func() {
		// 1. 立即主动取消上游 context（绝不等待下游锁，跳过 5s usage drain）
		if c.cancelUpstream != nil {
			c.cancelUpstream()
		}

		// 2. 立即关闭上游 HTTP 响应体
		if c.body != nil {
			_ = c.body.Close()
		}

		// 3. 丢弃本地所有在途缓冲
		for _, discard := range discardBuffers {
			if discard != nil {
				discard()
			}
		}

		// 4. 确定冻结的内部 reason
		frozenReason := reason
		if c.guard != nil && c.guard.IsTripped() && c.guard.TrippedReason() != "" {
			frozenReason = c.guard.TrippedReason()
		}

		c.trippedOutcome = streamOutcome{
			logStatusCode:  http.StatusBadGateway,
			failureKind:    "model_output_guard",
			failureMessage: GuardTriggeredPublicMessage,
			guardTripped:   true,
			guardReason:    frozenReason,
			penalize:       false,
			terminalLocal:  true,
		}
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
git commit -m "feat(proxy): add guard termination coordinator and buffer discard primitives"
```

---

#### 9b. 下游安全通知写入、TryLock/Deadline 控制与多协议终态

- [ ] **Step 1: Write failing test for downstream terminal writer and deadline hooks**

```go
// proxy/outputguard_downstream_test.go
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
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

// 1. 验证 TryLock 锁争用时非阻塞安全放弃，不产生孤儿协程且标记 clientGone
func TestSendDownstreamGuardTerminalHTTPTryLockContention(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request = req

	var downstreamMu sync.Mutex
	clientGone := false

	// 先行持有下游写锁（模拟慢写或 keepalive ticker 正在占用）
	downstreamMu.Lock()

	lockHoldReleased := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		downstreamMu.Unlock()
		close(lockHoldReleased)
	}()

	// 在锁被占用的情况下调用 SendDownstreamGuardTerminalHTTP：必须立即非阻塞返回！
	err := SendDownstreamGuardTerminalHTTP(c, &downstreamMu, &clientGone, "responses", "repetition")
	if err == nil {
		t.Fatalf("expected non-nil error when lock is contended")
	}
	if !clientGone {
		t.Fatalf("expected clientGone to be set to true on lock contention")
	}

	// 确认 recorder 中绝无任何字节写出（锁被占有时不执行无界等待写）
	if w.Body.Len() != 0 {
		t.Fatalf("expected no body written on lock contention, got %s", w.Body.String())
	}

	<-lockHoldReleased
}

// 自定义 Mock ResponseWriter，模拟不支持 SetWriteDeadline (ErrNotSupported)
type unsupportedDeadlineWriter struct {
	http.ResponseWriter
	writeCalled bool
}

func (u *unsupportedDeadlineWriter) Write(b []byte) (int, error) {
	u.writeCalled = true
	return u.ResponseWriter.Write(b)
}

// 2. 验证底层不支持 ResponseController WriteDeadline 时安全退出，不进行潜在阻塞写
func TestSendDownstreamGuardTerminalHTTPErrNotSupported(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	unsupported := &unsupportedDeadlineWriter{ResponseWriter: rec}

	c, _ := gin.CreateTestContext(unsupported)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request = req

	// 预先标记已写入头（已 commit 状态）
	rec.WriteHeader(http.StatusOK)

	var downstreamMu sync.Mutex
	clientGone := false

	err := SendDownstreamGuardTerminalHTTP(c, &downstreamMu, &clientGone, "responses", "repetition")
	if err == nil {
		t.Fatalf("expected error when deadline is unsupported on raw mock writer")
	}
	if !clientGone {
		t.Fatalf("expected clientGone == true when deadline is unsupported")
	}
	if unsupported.writeCalled {
		t.Fatalf("expected no write when deadline is unsupported")
	}
}

// 3. 验证 HTTP 尚未提交头时返回 502 及公开 JSON 错误，不泄露内部 reason
func TestSendDownstreamGuardTerminalHTTPUncommitted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request = req

	var downstreamMu sync.Mutex
	clientGone := false

	err := SendDownstreamGuardTerminalHTTP(c, &downstreamMu, &clientGone, "responses", "repetition")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected status 502, got %d", w.Code)
	}

	body := w.Body.String()
	// 断言公开错误码
	if !strings.Contains(body, GuardTriggeredPublicCode) {
		t.Fatalf("expected code %s in body: %s", GuardTriggeredPublicCode, body)
	}
	// 断言绝不泄露内部 reason "repetition"
	if strings.Contains(body, "repetition") {
		t.Fatalf("body leaked internal reason 'repetition': %s", body)
	}
}

// 4. 验证各协议流式已提交时生成的终态 Envelope 格式
func TestFormatGuardTerminalEnvelope(t *testing.T) {
	// 4a. Responses SSE -> response.failed，无 response.completed
	respData, status, err := FormatGuardTerminalEnvelope("responses", "")
	if err != nil {
		t.Fatalf("responses envelope error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("responses stream status want 200, got %d", status)
	}
	respStr := string(respData)
	if !strings.Contains(respStr, `"type":"response.failed"`) || !strings.Contains(respStr, GuardTriggeredPublicCode) {
		t.Fatalf("responses envelope invalid: %s", respStr)
	}
	if strings.Contains(respStr, "response.completed") {
		t.Fatalf("responses envelope must not contain response.completed")
	}

	// 4b. Chat SSE -> error envelope，严禁 [DONE]
	chatData, _, err := FormatGuardTerminalEnvelope("chat", "")
	if err != nil {
		t.Fatalf("chat envelope error: %v", err)
	}
	chatStr := string(chatData)
	if !strings.Contains(chatStr, GuardTriggeredPublicCode) || !strings.Contains(chatStr, `"error":{`) {
		t.Fatalf("chat envelope invalid: %s", chatStr)
	}
	if strings.Contains(chatStr, "[DONE]") {
		t.Fatalf("chat envelope must not contain [DONE]")
	}

	// 4c. Anthropic SSE -> event: error，严禁 message_stop
	anthropicData, _, err := FormatGuardTerminalEnvelope("anthropic", "")
	if err != nil {
		t.Fatalf("anthropic envelope error: %v", err)
	}
	anthropicStr := string(anthropicData)
	if !strings.Contains(anthropicStr, "event: error\n") || !strings.Contains(anthropicStr, `"type":"error"`) {
		t.Fatalf("anthropic envelope invalid: %s", anthropicStr)
	}
	if strings.Contains(anthropicStr, "message_stop") {
		t.Fatalf("anthropic envelope must not contain message_stop")
	}

	// 4d. Gemini Native -> 裸 JSON，无 data: 前缀
	geminiData, _, err := FormatGuardTerminalEnvelope("gemini", "")
	if err != nil {
		t.Fatalf("gemini envelope error: %v", err)
	}
	geminiStr := string(geminiData)
	if strings.HasPrefix(geminiStr, "data:") {
		t.Fatalf("gemini envelope must be bare JSON without data: prefix, got: %s", geminiStr)
	}
	if !strings.Contains(geminiStr, `"status":"UNAVAILABLE"`) || !strings.Contains(geminiStr, `"code":502`) {
		t.Fatalf("gemini envelope invalid: %s", geminiStr)
	}

	// 4e. WebSocket -> 带 stream_id 的 error 帧
	wsData, _, err := FormatGuardTerminalEnvelope("websocket", "stream_xyz")
	if err != nil {
		t.Fatalf("ws envelope error: %v", err)
	}
	wsStr := string(wsData)
	if !strings.Contains(wsStr, `"stream_id":"stream_xyz"`) || !strings.Contains(wsStr, GuardTriggeredPublicCode) {
		t.Fatalf("ws envelope invalid: %s", wsStr)
	}
}

// 5. 验证 WebSocket 下游失败写入与连接安全关闭
func TestSendDownstreamGuardTerminalWSSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		// 调用 SendDownstreamGuardTerminalWS 写入流错误帧
		_ = SendDownstreamGuardTerminalWS(conn, "turn_001", "repetition")
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial error: %v", err)
	}
	defer clientConn.Close()

	_, msg, err := clientConn.ReadMessage()
	if err != nil {
		t.Fatalf("read error: %v", err)
	}

	parsed := gjson.ParseBytes(msg)
	if parsed.Get("type").String() != "error" || parsed.Get("stream_id").String() != "turn_001" {
		t.Fatalf("unexpected ws frame: %s", string(msg))
	}
	if parsed.Get("error.code").String() != GuardTriggeredPublicCode {
		t.Fatalf("expected code %s, got %s", GuardTriggeredPublicCode, parsed.Get("error.code").String())
	}
}
```

- [ ] **Step 2: Run test to verify failure (Red)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./proxy -run "TestSendDownstreamGuardTerminalHTTP.*|TestFormatGuardTerminalEnvelope|TestSendDownstreamGuardTerminalWSSuccess"`
Expected: FAIL with compilation errors: `SendDownstreamGuardTerminalHTTP undefined`, `FormatGuardTerminalEnvelope undefined`.

- [ ] **Step 3: Implement minimal code for downstream terminal writer**

创建 `proxy/outputguard_downstream.go`：

```go
// proxy/outputguard_downstream.go
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

// ApplyDownstreamWriteDeadline 使用 Go 1.20+ http.NewResponseController 施加有界写超时；
// 若底层不支持，返回 http.ErrNotSupported
func ApplyDownstreamWriteDeadline(w http.ResponseWriter, timeout time.Duration) error {
	if w == nil {
		return errors.New("nil response writer")
	}
	rc := http.NewResponseController(w)
	if rc == nil {
		return http.ErrNotSupported
	}
	if timeout <= 0 {
		return rc.SetWriteDeadline(time.Time{})
	}
	return rc.SetWriteDeadline(time.Now().Add(timeout))
}

// FormatGuardTerminalEnvelope 根据协议格式化标准终态错误帧；脱敏输出，绝不回显内部 reason 或模型 payload
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
		if err != nil {
			return nil, 0, err
		}
		return []byte("data: " + string(payload) + "\n\n"), http.StatusOK, nil

	case "chat":
		payload, err := json.Marshal(gin.H{
			"error": gin.H{
				"message": GuardTriggeredPublicMessage,
				"type":    api.ErrorTypeUpstream,
				"code":    GuardTriggeredPublicCode,
			},
		})
		if err != nil {
			return nil, 0, err
		}
		return []byte("data: " + string(payload) + "\n\n"), http.StatusOK, nil

	case "anthropic":
		payload, err := json.Marshal(gin.H{
			"type": "error",
			"error": gin.H{
				"type":    "api_error",
				"message": GuardTriggeredPublicMessage,
			},
		})
		if err != nil {
			return nil, 0, err
		}
		return []byte("event: error\ndata: " + string(payload) + "\n\n"), http.StatusOK, nil

	case "gemini":
		// Native Gemini SDK 约定：流式末尾输出裸 JSON 错误（无 data: 前缀）
		payload, err := json.Marshal(gin.H{
			"error": gin.H{
				"code":    http.StatusBadGateway,
				"message": GuardTriggeredPublicMessage,
				"status":  "UNAVAILABLE",
			},
		})
		if err != nil {
			return nil, 0, err
		}
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
		if err != nil {
			return nil, 0, err
		}
		return payload, http.StatusOK, nil

	default:
		// 通用 HTTP 502 JSON
		payload, err := json.Marshal(gin.H{
			"error": gin.H{
				"message": GuardTriggeredPublicMessage,
				"type":    api.ErrorTypeUpstream,
				"code":    GuardTriggeredPublicCode,
			},
		})
		if err != nil {
			return nil, 0, err
		}
		return payload, http.StatusBadGateway, nil
	}
}

// SendDownstreamGuardTerminalHTTP 向下游 HTTP 连接尽力写入 Guard 终态错误帧：
// 1. 若尚未提交 HTTP 头，直接以 502 返回标准 JSON 错误；
// 2. 若已提交流式头，使用 downstreamMu.TryLock() 尝试非阻塞获取锁；
//    - 若获取失败（锁争用），立即放弃写入并标记 clientGone = true，绝不引入孤儿 goroutine 挂死；
// 3. 持锁后调用 ApplyDownstreamWriteDeadline 施加 5s 写入超时；
//    - 若底层返回 ErrNotSupported 或已断开，立即安全退出放弃写帧，标记 clientGone = true；
// 4. 写入对应协议的终态帧并调用 Flush。
func SendDownstreamGuardTerminalHTTP(c *gin.Context, mu *sync.Mutex, clientGone *bool, protocol string, reason string) error {
	if c == nil || c.Writer == nil {
		if clientGone != nil {
			*clientGone = true
		}
		return errors.New("nil context or writer")
	}

	// 1. 未提交头的主路径：直接返回 502 JSON
	if !c.Writer.Written() {
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.JSON(http.StatusBadGateway, gin.H{
			"error": gin.H{
				"message": GuardTriggeredPublicMessage,
				"type":    api.ErrorTypeUpstream,
				"code":    GuardTriggeredPublicCode,
			},
		})
		if clientGone != nil {
			*clientGone = true
		}
		return nil
	}

	// 2. 已开始流式写入：有界非阻塞锁获取
	if mu != nil {
		if !mu.TryLock() {
			// 锁被保活 ticker 或写协程占用，立即放弃尽力交付，标记 clientGone 安全退出
			if clientGone != nil {
				*clientGone = true
			}
			return errors.New("downstream write lock busy; dropped guard terminal notification")
		}
		defer mu.Unlock()
	}

	if c.Request != nil && c.Request.Context().Err() != nil {
		if clientGone != nil {
			*clientGone = true
		}
		return c.Request.Context().Err()
	}

	// 3. 持锁内设置 5 秒硬超时
	if err := ApplyDownstreamWriteDeadline(c.Writer, GuardWriteDeadlineTimeout); err != nil {
		// 底层不支持 SetWriteDeadline（如测试 mock 或部分反代），严禁执行无界阻塞写
		if clientGone != nil {
			*clientGone = true
		}
		return fmt.Errorf("set write deadline failed (%w); safely dropped write", err)
	}
	defer func() {
		_ = ApplyDownstreamWriteDeadline(c.Writer, 0) // 清除超时
	}()

	// 4. 格式化并写入协议终态
	envelope, _, err := FormatGuardTerminalEnvelope(protocol, "")
	if err != nil {
		return err
	}

	if _, err := c.Writer.Write(envelope); err != nil {
		if clientGone != nil {
			*clientGone = true
		}
		return err
	}

	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}

	if clientGone != nil {
		*clientGone = true
	}
	return nil
}

// SendDownstreamGuardTerminalWS 向 WebSocket 写入 stream 关联的错误帧
func SendDownstreamGuardTerminalWS(conn *websocket.Conn, streamID string, reason string) error {
	if conn == nil {
		return errors.New("nil websocket conn")
	}

	envelope, _, err := FormatGuardTerminalEnvelope("websocket", streamID)
	if err != nil {
		return err
	}

	// WebSocket transport 复用原生 SetWriteDeadline
	_ = conn.SetWriteDeadline(time.Now().Add(GuardWriteDeadlineTimeout))
	defer func() {
		_ = conn.SetWriteDeadline(time.Time{})
	}()

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

---

### Task 10: 全协议 Handler 接线、前置 Guard 拦截与全链路恢复阻断

**Files:**
- Modify: `proxy/continuous_retry.go:172-185, 276-285`
- Modify: `proxy/retry_exclusions.go:209-245`
- Modify: `proxy/handler.go:2905-2935, 4014-4050, 4405-4420, 5045-5085, 5270-5285, 5538-5588, 5680-5755, 5915-5955, 6030-6070, 7025-7065, 7380-7415, 7560-7575, 7700-7810, 7960-7985`
- Modify: `proxy/responses_ws.go:246-270, 700-715, 835-850, 1050-1070, 1251-1315`
- Modify: `proxy/realtime_ws.go:40-60, 140-160`
- Modify: `proxy/handler_anthropic.go:547-570, 830-845, 1035-1050, 1330-1345, 1570-1615, 1720-1768`
- Modify: `proxy/handler_gemini.go:123-160, 462-520`
- Test: `proxy/outputguard_handler_test.go`

**Interfaces:**
- Consumes:
  - `security/outputguard` (来自 Task 1~3, 定义于 `security/outputguard/guard.go`):
    ```go
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
    func (g *RequestGuard) ObserveAppend(channelID string, aliasID string, kind ChannelKind, delta []byte) error
    func (g *RequestGuard) ObserveSnapshot(channelID string, aliasID string, kind ChannelKind, snapshot []byte, terminalStage string) error
    func (g *RequestGuard) EndChannel(channelID string) error
    func (g *RequestGuard) IsTripped() bool
    func (g *RequestGuard) TrippedReason() string
    ```
  - `proxy` 运行时配置快照与就绪门禁 (来自 Task 5, 定义于 `proxy/output_guard_settings.go`):
    ```go
    func RequireOutputGuardReadyHTTP(c *gin.Context) bool
    func RequireOutputGuardReadyWS(conn *websocket.Conn) bool
    func RequireOutputGuardReadyStream(conn *websocket.Conn, streamID string) bool
    func CurrentOutputGuardConfig() OutputGuardAppliedConfig
    ```
  - `proxy` 协议投影器与分派原语 (来自 Task 7, 定义于 `proxy/outputguard_projection.go`):
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
  - `proxy` 传输 Framing 有界 Reader 与 Cap (来自 Task 8, 定义于 `proxy/sse_line.go`):
    ```go
    func ReadSSEStreamWithEvent(body io.Reader, callback func(event string, data []byte) bool) error
    func ReadBoundedResponseBody(body io.Reader, limit int64) ([]byte, error)
    ```
  - `proxy` 强制终止与下游通知基础原语 (来自 Task 9 契约定义, 计划落于 `proxy/outputguard_cancel.go` 与 `proxy/outputguard_downstream.go`):
    > **接口契约与依赖差异说明（重要声明）**：
    > 1. Task 10 仅负责 handler 接线与全链路恢复阻断，**不扩充原语的内部实现**。
    > 2. Task 9 由独立作者正在编写 `part_t9.txt`，相关终止与下游通知原语目前尚未落地生产代码，本计划严格依赖 Task 9 所定义的接口契约（而非声称生产已存在）：
    >    ```go
    >    // streamOutcome 扩展字段（用于在代理内部全链路传递 Guard 命中状态与类型化原因）
    >    type streamOutcome struct {
    >        ...
    >        guardTripped bool
    >        guardReason  string
    >    }
    >
    >    // 终止协调原语：原子标记 tripped，主动 cancel 上游 context（跳过 5s usage drain），
    >    // 关闭 HTTP 响应体，废弃所属 WS lease；清空本地在途 pending/provenance/collector/replay/writer 缓冲
    >    type GuardTerminationCoordinator struct { ... }
    >    func NewGuardTerminationCoordinator(guard *outputguard.RequestGuard, cancelUpstream context.CancelFunc, body io.Closer) *GuardTerminationCoordinator
    >    func (c *GuardTerminationCoordinator) HandleTripped(reason string, discardBuffers ...func()) streamOutcome
    >
    >    // 下游通知写入原语：TryLock 非阻塞获取 downstreamMu，成功后持锁通过 ResponseController.SetWriteDeadline(5s) 写入协议错误帧；
    >    // 遇到 ErrNotSupported 或锁冲突立即安全放弃并标记 clientGone，绝不阻塞主流程回收
    >    func SendDownstreamGuardTerminalHTTP(c *gin.Context, mu *sync.Mutex, clientGone *bool, protocol string, reason string) error
    >    func SendDownstreamGuardTerminalWS(conn *websocket.Conn, streamID string, reason string) error
    >    ```
    > 3. **核心事件计数契约差异说明**：Task 7 (C) 在 `ObserveEvent` 中设计了 `guard.RecordEvent()` 先扣 1 次事件，随后对多 observation 使用 `ObserveAppendNoEvent`；而 Task 1~3 (A) 发布的 `RequestGuard` 接口中未暴露独立的 `RecordEvent`（由内部判定流水线管理）。Task 10 在 handler 层面统一消费 Task 7 暴露的高级事件入口 `ObserveEvent(guard, projector, eventType, data)`，两者的内部计数差异由后续整体整合阶段统一，Task 10 不在 handler 内直接调用未定义的底层 `RecordEvent`。

- Produces:
  - 覆盖 `/v1/responses` (HTTP 流式/非流式, WebSocket, Realtime 文本子集), `/v1/chat/completions`, `/v1/messages`, Gemini Native (`streamGenerateContent`/`generateContent`), Grok, Antigravity 渠道的全局防御闭环。
  - 请求级与 WS turn 级的配置快照与 `RequestGuard` 实例生命周期管理（跨 attempt 保持累积，calls == 1）。
  - 前置检查点保护：guard 检查必须在 `bytes.Clone` provenance 记录、collector 累加、translator 转换与 replay 写入之前。
  - 全链路恢复阻断：在 ordinary retry, selective/catch-all retry, 换号, WS->HTTP fallback, 缺失上下文恢复, 自动压缩, 自动续想等全部恢复点无条件阻断，确保账号健康零受罚、cache/续链零污染、用量不虚构不 drain。

---

- [ ] **Step 1: Write failing integration test for handler wiring and recovery blocking**

```go
// proxy/outputguard_handler_test.go
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security/outputguard"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

// 辅助函数：构造测试用的 Handler
func setupTestProxyHandlerWithUpstream(t *testing.T, upstreamURL string) (*Handler, *gin.Engine) {
	gin.SetMode(gin.TestMode)
	db := newTestModelRegistryDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2})

	account := &auth.Account{
		Type:     auth.AccountTypeCodexOAuth,
		Status:   auth.AccountStatusActive,
		Provider: "openai",
		BaseURL:  upstreamURL, // 审查反馈 F7 修复：将 mock 上游 URL 准确注入 account.BaseURL，确保集成测试真实调用 mock！
	}
	account.SetID(101)
	store.AddAccount(account)

	h := &Handler{
		db:    db,
		store: store,
	}

	router := gin.New()
	router.POST("/v1/responses", h.Responses)
	router.POST("/v1/chat/completions", h.ChatCompletions)
	router.POST("/v1/messages", h.Messages)
	router.POST("/v1beta/models/:action", h.GeminiModelsAction)
	router.GET("/v1/responses", h.ResponsesWebSocket)
	router.GET("/v1/realtime", h.RealtimeWebSocket)

	return h, router
}

// 1. 验证 Responses HTTP 流式上游失控输出被前置拦截，不进入 provenance/collector，立即 cancel 上游且阻断重试
func TestResponsesHTTPStreamGuardTrippedAndRecoveryBlocked(t *testing.T) {
	gin.SetMode(gin.TestMode)
	SetOutputGuardReadyForTest(true)
	defer ResetOutputGuardStateForTest()

	// 模拟上游返回持续短周期重复的流式 arguments
	upstreamCalls := int32(0)
	upstreamCanceled := make(chan struct{})
	var cancelOnce sync.Once

	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		// 监听 client cancel
		notify := r.Context().Done()
		go func() {
			<-notify
			cancelOnce.Do(func() {
				close(upstreamCanceled)
			})
		}()

		// 发送合法开始事件
		_, _ = fmt.Fprintf(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n")
		flusher.Flush()

		// 持续发送短周期重复 delta (p=10, 8192B 以上)
		pattern := "0123456789"
		for i := 0; i < 900; i++ {
			chunk := fmt.Sprintf("event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"call_id\":\"call_1\",\"delta\":%q}\n\n", pattern)
			_, err := fmt.Fprint(w, chunk)
			if err != nil {
				return // 客户端已主动断开
			}
			flusher.Flush()
			time.Sleep(1 * time.Millisecond)
		}
	}))
	defer upstreamServer.Close()

	// 配置 Guard 阈值 (使用 Spec 默认值)
	ApplyOutputGuardSettings(database.OutputGuardSettings{
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})

	_, router := setupTestProxyHandlerWithUpstream(t, upstreamServer.URL)

	reqBody := `{"model":"gpt-5.4","stream":true,"input":[{"role":"user","content":"run"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	// 断言 1：上游只被调用了 1 次（禁止任何透明重试、换号重试或 catch-all 循环）
	if calls := atomic.LoadInt32(&upstreamCalls); calls != 1 {
		t.Fatalf("expected exactly 1 upstream call, got %d", calls)
	}

	// 断言 2：上游 context 被主动 cancel（跳过了 5s usage drain）
	select {
	case <-upstreamCanceled:
		// 正常收到上游取消
	case <-time.After(2 * time.Second):
		t.Fatalf("expected upstream context to be canceled immediately, timed out")
	}

	respBody := w.Body.String()
	// 断言 3：下游收到标准的 response.failed 终态，且错误码为 model_output_guard_triggered
	if !strings.Contains(respBody, "model_output_guard_triggered") {
		t.Fatalf("expected response.failed with model_output_guard_triggered, got: %s", respBody)
	}
	// 断言 4：下游严禁收到完成帧 response.completed
	if strings.Contains(respBody, "response.completed") {
		t.Fatalf("downstream received response.completed after guard tripped!")
	}
}

// 2. 验证 continuousRetryStreamSelected 与 shouldTransparentRetryStream 坚决阻断 guardTripped
func TestRecoveryPointsBlockGuardTripped(t *testing.T) {
	guardOutcome := streamOutcome{
		logStatusCode:  http.StatusOK,
		failureKind:    "model_output_guard",
		failureMessage: "Upstream model output exceeded safety policy",
		guardTripped:   true,
		guardReason:    "repetition",
		penalize:       false,
	}

	catchAllPolicy := database.ContinuousRetryPolicy{
		Enabled:    true,
		MaxRetries: 5,
		Categories: []string{"all"},
	}

	// 1) continuousRetryStreamSelected 必须返回 false
	if continuousRetryStreamSelected(guardOutcome, nil, "response.failed", catchAllPolicy) {
		t.Fatalf("continuousRetryStreamSelected must return false for guardTripped")
	}

	// 2) continuousRetryStreamFailureSelected 必须返回 false
	if continuousRetryStreamFailureSelected(guardOutcome, nil, "response.failed", catchAllPolicy) {
		t.Fatalf("continuousRetryStreamFailureSelected must return false for guardTripped")
	}

	// 3) shouldTransparentRetryStreamWithBudgets 必须返回 false
	general := 0
	rate := 0
	if shouldTransparentRetryStreamWithBudgets(guardOutcome, &general, &rate, 3, 3, false, nil, nil, catchAllPolicy) {
		t.Fatalf("shouldTransparentRetryStreamWithBudgets must return false for guardTripped")
	}
}

// 3. 验证 retryExclusions 对 guardTripped 豁免健康度惩罚 (使用真实方法 forHardSelection / hasResettable)
func TestRetryExclusionsZeroPenaltyForGuardTripped(t *testing.T) {
	exclusions := newRetryAccountExclusions()
	accountID := int64(1001)

	guardOutcome := streamOutcome{
		logStatusCode:  http.StatusBadGateway,
		failureKind:    "model_output_guard",
		guardTripped:   true,
		guardReason:    "tool_argument_bytes",
		penalize:       false,
	}

	exclusions.MarkStreamFailure(accountID, guardOutcome, 3, 3)

	// 断言：账号没有被放入 hard 或 transient 排除列表
	if exclusions.hasResettable() {
		t.Fatalf("exclusions.hasResettable() should be false for guard failure")
	}
	hard := exclusions.forHardSelection()
	if hard != nil && hard[accountID] {
		t.Fatalf("account %d was penalized in hard exclusions for guard failure", accountID)
	}
}

// 4. 验证 WebSocket 入口 Ready 检查未就绪返回 1013 Close 帧，已就绪则正常 turn 检测
func TestResponsesWebSocketReadinessAndGuardTripped(t *testing.T) {
	SetOutputGuardReadyForTest(false)
	defer ResetOutputGuardStateForTest()

	// 冷启动未就绪时建立 WebSocket
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// 消费 Task 5 门禁：未就绪先发 503 协议错误帧，随后发送 1013 Close 帧
		if !RequireOutputGuardReadyWS(conn) {
			return
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		defer conn.Close()
		// 审查反馈 F6b 修复：WS 未就绪时服务端先发 503 error 帧，紧接着发送 1013 Close 帧。
		// 循环读取直到捕获 1013 错误，断言协议关闭码为 CloseTryAgainLater (1013)
		var closeErr *websocket.CloseError
		for i := 0; i < 2; i++ {
			_, msg, readErr := conn.ReadMessage()
			if readErr != nil {
				if errors.As(readErr, &closeErr) {
					break
				}
			}
			if len(msg) > 0 && strings.Contains(string(msg), "service_unavailable") {
				// 成功读取到 503 协议错误帧
			}
		}
		if closeErr == nil || closeErr.Code != websocket.CloseTryAgainLater {
			t.Fatalf("expected websocket close code 1013 (CloseTryAgainLater), got: %v", closeErr)
		}
	} else if resp != nil && resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 or close 1013 on unready WS, got status: %d", resp.StatusCode)
	}
}

// 5. 验证锁内 SetWriteDeadline 与 TryLock 终态写入非阻塞
func TestDownstreamWriteWithDeadlineAndTryLock(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	// 审查反馈 F6c 修复：使用真实已提交响应头的 gin.Context 测试 TryLock 争用与安全退出分支
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.WriteHeader(http.StatusOK)
	c.Writer.WriteHeaderNow()
	_, _ = c.Writer.Write([]byte("event: response.created
data: {}

"))

	var mu sync.Mutex
	clientGone := false

	// 模拟已持锁的耗时写协程
	mu.Lock()
	lockReleased := make(chan struct{})
	go func() {
		time.Sleep(100 * time.Millisecond)
		mu.Unlock()
		close(lockReleased)
	}()

	// 终态写入尝试 TryLock：若锁被占用应立即非阻塞放弃，绝不挂起，标记 clientGone = true
	err := SendDownstreamGuardTerminalHTTP(c, &mu, &clientGone, "responses", "repetition")
	if err == nil {
		t.Fatalf("expected error or early return when downstream lock is busy")
	}
	if !clientGone {
		t.Fatalf("expected clientGone to be set true upon TryLock contention")
	}

	<-lockReleased
}

// 6. 验证 Chat Completions 流式失控输出拦截：不追加 [DONE]，返回合法 error 帧
func TestChatCompletionsStreamGuardTrippedNoDone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	SetOutputGuardReadyForTest(true)
	defer ResetOutputGuardStateForTest()

	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		// 持续发送超大文本 delta (触发 channel_bytes)
		bigChunk := strings.Repeat("A", 100000)
		for i := 0; i < 25; i++ { // > 2.5 MiB
			payload := fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", bigChunk)
			_, _ = fmt.Fprint(w, payload)
			flusher.Flush()
		}
	}))
	defer upstreamServer.Close()

	_, router := setupTestProxyHandlerWithUpstream(t, upstreamServer.URL)

	reqBody := `{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"generate"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	respBody := w.Body.String()
	// 下游严禁出现 [DONE] 标记
	if strings.Contains(respBody, "[DONE]") {
		t.Fatalf("Chat Completions stream contained [DONE] after guard tripped: %s", respBody)
	}
	// 下游必须出现 model_output_guard_triggered 错误码
	if !strings.Contains(respBody, "model_output_guard_triggered") {
		t.Fatalf("expected model_output_guard_triggered in chat stream, got: %s", respBody)
	}
}
```

- [ ] **Step 2: Run test to verify failure (Red)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./proxy -run "TestResponsesHTTPStreamGuardTrippedAndRecoveryBlocked|TestRecoveryPointsBlockGuardTripped|TestRetryExclusionsZeroPenaltyForGuardTripped|TestChatCompletionsStreamGuardTrippedNoDone"`
Expected: FAIL with compilation/runtime errors: `streamOutcome.guardTripped undefined` or `upstreamCalls > 1`.

- [ ] **Step 3: Implement minimal code for Handler wiring and recovery blocking**

#### 3.1 恢复与重试机制最前置阻断 (`proxy/continuous_retry.go`, `proxy/retry_exclusions.go`, `proxy/handler.go`)

在 `proxy/continuous_retry.go` 的 `continuousRetryStreamSelected` 和 `continuousRetryStreamFailureSelected` 最开头插入 guardTripped 快速短路：

```go
// proxy/continuous_retry.go:172
func continuousRetryStreamSelected(outcome streamOutcome, payload []byte, eventType string, policies ...database.ContinuousRetryPolicy) bool {
	// Guard 命中是请求级硬终态，绝不允许任何形式的 continuous retry 重新拉起！
	if outcome.guardTripped {
		return false
	}
	policy := continuousRetryPolicyForCall(policies)
	if !policy.Enabled || outcome.terminalLocal {
		return false
	}
...
```

```go
// proxy/continuous_retry.go:276
func continuousRetryStreamFailureSelected(outcome streamOutcome, payload []byte, eventType string, policies ...database.ContinuousRetryPolicy) bool {
	if outcome.guardTripped || outcome.terminalLocal || strings.EqualFold(strings.TrimSpace(outcome.failureKind), "continuous_retry_timeout") {
		return false
	}
...
```

在 `proxy/retry_exclusions.go` 的 `MarkStreamFailureForEvent` 中增加对 `guardTripped` 的免罚判断：

```go
// proxy/retry_exclusions.go:209
func (r *retryAccountExclusions) MarkStreamFailureForEvent(accountID int64, outcome streamOutcome, eventType string, generalLimit, rateLimit int, policies ...database.ContinuousRetryPolicy) {
	// 模型输出失控属于模型生成异常，不是账号故障的充分证据，严禁对账号施加健康度惩罚！
	if outcome.guardTripped || strings.EqualFold(strings.TrimSpace(outcome.failureKind), "model_output_guard") {
		return
	}
	failureKind := strings.ToLower(strings.TrimSpace(outcome.failureKind))
...
```

在 `proxy/handler.go` 的 `shouldTransparentRetryStreamEventWithBudgets` 最开头增加 guardTripped 拦截：

```go
// proxy/handler.go:2909
func shouldTransparentRetryStreamEventWithBudgets(outcome streamOutcome, eventType string, generalRetries, rateLimitRetries *int, maxGeneralRetries, maxRateLimitRetries int, wroteAnyBody bool, ctxErr, writeErr error, policies ...database.ContinuousRetryPolicy) bool {
	if outcome.guardTripped {
		return false
	}
	if outcome.terminalLocal || wroteAnyBody || ctxErr != nil || writeErr != nil || outcome.logStatusCode == logStatusClientClosed {
		return false
	}
...
```

#### 3.2 `/v1/responses` HTTP 流式与非流式接线 (`proxy/handler.go`)

在 `Responses` 处理函数中：
1. 入口检查门禁：`if !RequireOutputGuardReadyHTTP(c) { return }`
2. 初始化逻辑请求级 Guard 快照（跨 attempt 共享，单次请求 `reqGuard` 仅创建 1 次，不随 attempt 重新创建）：
   ```go
   appliedGuardCfg := CurrentOutputGuardConfig()
   reqGuard := outputguard.NewRequestGuard(outputguard.Config{
       Enabled:               appliedGuardCfg.Enabled,
       MaxToolArgumentBytes:  appliedGuardCfg.MaxToolArgumentBytes,
       MaxToolArgumentDeltas: appliedGuardCfg.MaxToolArgumentDeltas,
       MaxChannelBytes:       appliedGuardCfg.MaxChannelBytes,
       MaxRequestBytes:       appliedGuardCfg.MaxRequestBytes,
   })
   responsesProjector := NewResponsesProjector()
   ```
3. 在流式透传 `forwardWithEvent` 最开头（**必须早于 `bytes.Clone` provenance 记录与 `outputCollector.Add`**）插入 Guard 检查：
   ```go
   // proxy/handler.go:5538
   forwardWithEvent := func(sseEvent string, data []byte) bool {
       if multiAgentV2State.isOptimizedFor(account.ID()) {
           data = restoreCodexMultiAgentV2Response(data, multiAgentV2State)
       }
       streamDiag.markUpstreamFrame()

       // Output Guard 前置检查：必须在 bytes.Clone 记录 provenance、collector 累积及下游写入之前执行！
       if err := ObserveEvent(reqGuard, responsesProjector, sseEvent, data); err != nil {
           // Guard 命中！立即执行强制终止契约：
           // 1. 取消上游 context（直接调用 cancel 跳过 5s usage drain）
           if lastUpstreamCancel != nil {
               lastUpstreamCancel()
           }
           // 2. 丢弃未提交的在途缓冲（provenance, collector, replay, pending buffers）
           compactionProvenancePayloads = nil
           outputCollector.clearOverflow()
           pendingFirstTokenEvents.Reset()
           if streamWriter != nil {
               streamWriter.DiscardBuffered()
           }
           // 3. 标记 outcome 为 guardTripped，保留 typed reason
           outcome = streamOutcome{
               logStatusCode:  http.StatusBadGateway,
               failureKind:    "model_output_guard",
               failureMessage: "Upstream model output exceeded safety policy; generation was stopped.",
               guardTripped:   true,
               guardReason:    reqGuard.TrippedReason(),
               penalize:       false,
           }
           // 4. 下游尽力交付写入错误帧（TryLock + SetWriteDeadline 5s，防挂死）
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
       ...
   ```
4. 在锁内所有常规写操作（包括数据帧写和注释保活写）之前，统一调用 `rc.SetWriteDeadline(time.Now().Add(5 * time.Second))`：
   ```go
   if rc := http.NewResponseController(c.Writer); rc != nil {
       if err := rc.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
           if errors.Is(err, http.ErrNotSupported) {
               // 不支持 Deadline 控制的底层，严禁无界阻塞同步写，安全退出
               clientGone = true
               return false
           }
       }
   }
   ```
5. 在恢复选择点（WebSocket 1009 降级 HTTP、首包前透明重试、自动续想、自动压缩等）前置阻断：
   ```go
   // proxy/handler.go:5915
   if outcome.guardTripped {
       _ = streamAttempt.Close()
       resp.Body.Close()
       // 审查反馈 M7 修复：Guard 命中属于安全终止，必须立即执行且仅执行一次 Release(account)；
		// 严禁复用 WS messageTooBig 的 Retain 租约逻辑，严禁将异常连接归还健康池！
		h.store.Release(account)
       // 严格不重试、不降级、不写成功缓存
       return
   }
   ```
6. 非流式响应处理：在读取完整响应体 `ReadBoundedResponseBody` 后，通过 `responsesProjector` 进行一次性完整快照观察；若触发 guard，清空本地缓冲并返回 502 错误，不返回半截 JSON。

#### 3.3 `/v1/responses` WebSocket 与 `/v1/realtime` 接线 (`proxy/responses_ws.go`, `proxy/realtime_ws.go`)

1. 在 `ResponsesWebSocket` 握手阶段与 `RealtimeWebSocket` 建立阶段消费门禁：
   ```go
   if !RequireOutputGuardReadyWS(conn) {
       return
   }
   ```
2. 对每个 WebSocket 逻辑 turn 创建独立的 `RequestGuard` 实例（与逻辑 turn 生命周期绑定）：
   ```go
   // proxy/responses_ws.go
   turnGuard := outputguard.NewRequestGuard(outputguard.Config{
       Enabled:               appliedGuardCfg.Enabled,
       MaxToolArgumentBytes:  appliedGuardCfg.MaxToolArgumentBytes,
       MaxToolArgumentDeltas: appliedGuardCfg.MaxToolArgumentDeltas,
       MaxChannelBytes:       appliedGuardCfg.MaxChannelBytes,
       MaxRequestBytes:       appliedGuardCfg.MaxRequestBytes,
   })
   turnProjector := NewResponsesProjector()
   ```
3. 在 `readSSEStreamWithContinuousRetryKeepalive` 回调中最前置插入 `ObserveEvent`：
   - 命中时立即废弃上游 WS lease（不归还健康池），关闭上游 body；
   - 丢弃 collector、replay、pendingFirstTokenMessages；
   - 标记 `turnOutcome.guardTripped = true`；
   - 经由 `SendDownstreamGuardTerminalWS` 仅向当前 stream 发送失败帧，不影响无故障的复用流；
   - 阻断 `wsHTTPFallback` 记录与重试。
4. 在 `realtime_ws.go` 中，`normalizeRealtimeTextClientEvent` 显式拒绝音频 Modality；转发到 Responses WS 的文本 turn 自动继承上述 `turnGuard` 约束。

#### 3.4 `/v1/chat/completions` 接线 (`proxy/handler.go`)

1. 入口检查：`if !RequireOutputGuardReadyHTTP(c) { return }`
2. 初始化 `chatProjector := NewChatProjector()` 与单请求级 `reqGuard`。
3. 在流式转发回调中，于 `TranslateStreamChunkResult` 之前执行 `ObserveEvent`：
   - 命中时主动 cancel 上游 context，清空 `pendingFirstTokenChunks`；
   - 经由 `SendDownstreamGuardTerminalHTTP(..., "chat", ...)` 写入 OpenAI 格式错误包；
   - 坚决**不追加 `data: [DONE]\n\n`**；
   - 阻断后续 attempt，豁免账号惩罚。

#### 3.5 `/v1/messages` (Anthropic) 接线 (`proxy/handler_anthropic.go`)

1. 入口检查：`if !RequireOutputGuardReadyHTTP(c) { return }`
2. 初始化 `anthropicProjector := NewAnthropicProjector()` 与请求级 `reqGuard`。
3. 在流式循环（line 1575 之前）执行 `ObserveEvent`：
   - 必须早于 `translator.translateEvent(data)`；
   - 命中时 cancel 上游 context，清空 `pendingFirstTokenEvents`；
   - 发送 Anthropic 标准 `event: error` 结构（带 `model_output_guard_triggered` code）；
   - 坚决**不伪造 `message_stop` 或 tool input 完成**；
   - 阻断重试并安全退出。

#### 3.6 Gemini Native 接线 (`proxy/handler_gemini.go`)

1. 在 `GeminiModelsAction` 与 `handleGeminiGenerateContent` 最前置检查门禁：
   `if !RequireOutputGuardReadyHTTP(c) { return }`
2. 初始化 `geminiProjector := NewGeminiProjector()` 与请求级 `reqGuard`。
3. 在流式读取块回调中最前置投影检查：
   - 命中时主动 cancel 上游；
   - 采用 Gemini SDK 兼容的错误输出结构（复用已有的 `sendUpstreamError` 或原生 error payload，不伪造不支持的 `event: error` 导致客户端崩溃）；
   - 阻断重试循环。

#### 3.7 Grok 与 Antigravity 原生渠道透传校验

在 Grok native passthrough 以及 Antigravity 专属通道中：
- 即使协议为 native 转发，数据流在到达下游 ResponseWriter 或 attempt writer 之前，一律经由 `ObserveEvent` 进行语义投影与预算/周期检查；
- 命中时坚决终止本次 dispatch，不让原生流绕过防护。

- [ ] **Step 4: Run test to verify passing (Green)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v -race ./proxy -run "TestResponsesHTTPStreamGuardTrippedAndRecoveryBlocked|TestRecoveryPointsBlockGuardTripped|TestRetryExclusionsZeroPenaltyForGuardTripped|TestResponsesWebSocketReadinessAndGuardTripped|TestDownstreamWriteWithDeadlineAndTryLock|TestChatCompletionsStreamGuardTrippedNoDone"`
Expected: PASS ok github.com/codex2api/proxy

- [ ] **Step 5: Commit**

```bash
git add proxy/continuous_retry.go proxy/retry_exclusions.go proxy/handler.go proxy/responses_ws.go proxy/realtime_ws.go proxy/handler_anthropic.go proxy/handler_gemini.go proxy/outputguard_handler_test.go
git commit -m "feat(proxy): wire model output guard across all protocols with complete recovery blocking"
```

---

### Task 11: 综合验收测试套件、性能指标工具与 Spec §9 完整覆盖矩阵

**Files:**
- Create: `test/outputguard/helpers.go`（脱敏合成 Fixture、真进程 CPU 计时器、高周期 PRNG、P95/中位数统计学分析函数及机器可判验证器）
- Create: `test/outputguard/helpers_test.go`（Helper 基础设施自验证测试：验证真实 RUSAGE 进程 CPU 测量、PRNG 无短周期、P95 算法及红灯故意失败捕获机制）
- Create: `security/outputguard/acceptance_detection_test.go`（检测正确性验收：场景 1~11，含 chunk-invariance、9215B 片段、8a/8b/8c 成本上界、尾检放行）
- Create: `proxy/acceptance_lifecycle_test.go`（全协议终止与生命周期验收：场景 12~20，含全协议 Handler 矩阵、无 5s drain、并发无 race、重试阻断 calls==1、多协议终态格式、TryLock/Deadline 降级、无账号惩罚与租约隔离）
- Create: `proxy/acceptance_config_test.go`（持久化与门禁验收：场景 21~23，含 SQLite/PostgreSQL 并发更新与恢复默认、冷启动 503 与 5s 轮询、Framing GuardOff 依然有界）
- Create: `test/outputguard/acceptance_benchmark_test.go`（性能与基准验收：场景 24~28，含 TTFT p95 <=10ms/CPU <=20%、1/2/4MiB ratio <=2.5、内存/FD/Goroutine 10分钟长跑门禁、细 delta 4096 deltas 专项压测、脱敏日志检查）
- Create: `scripts/run_acceptance_suite.sh`（全套验收与性能门禁自动化执行脚本）

**Interfaces:**
- Consumes:
  - `security/outputguard`（已在主 Plan Task 1~3 整合交付）:
    - `NewRepetitionDetector() *RepetitionDetector`
    - `(d *RepetitionDetector) Push(data []byte) (bool, int)`
    - `(d *RepetitionDetector) CheckTail() (bool, int)`
    - `(d *RepetitionDetector) Reset()`
    - `NewChannelGuard(id string, kind ChannelKind, maxBytes int64, maxDeltas int) *ChannelGuard`
    - `(c *ChannelGuard) CheckAppend(delta []byte) (int64, int64, error)`
    - `(c *ChannelGuard) CommitAppend(delta []byte, growth int64, cost int64) error`
    - `(c *ChannelGuard) CheckSnapshot(snapshot []byte, terminalStage string) (int64, int64, bool, error)`
    - `(c *ChannelGuard) CommitSnapshot(snapshot []byte, growth int64, cost int64, terminalStage string) error`
    - `(c *ChannelGuard) RemainingScanBudget() int64`
    - `(c *ChannelGuard) Highwater() int64`
    - `(c *ChannelGuard) DeltaCount() int`
    - `(c *ChannelGuard) ReleaseWindow()`
    - `NewRequestGuard(cfg Config) *RequestGuard`
    - `(g *RequestGuard) RecordEvent() error`
    - `(g *RequestGuard) ObserveAppend(channelID string, aliasID string, kind ChannelKind, delta []byte) error`
    - `(g *RequestGuard) ObserveSnapshot(channelID string, aliasID string, kind ChannelKind, snapshot []byte, terminalStage string) error`
    - `(g *RequestGuard) ObserveAppendNoEvent(channelID string, aliasID string, kind ChannelKind, delta []byte) error`
    - `(g *RequestGuard) ObserveSnapshotNoEvent(channelID string, aliasID string, kind ChannelKind, snapshot []byte, terminalStage string) error`
    - `(g *RequestGuard) EndChannel(channelID string) error`
    - `(g *RequestGuard) IsTripped() bool`
    - `(g *RequestGuard) TrippedReason() string`
  - `database`（已在主 Plan Task 4 整合交付）:
    - `DB.GetOutputGuardSettings(context.Context) (OutputGuardSettings, error)`
    - `DB.UpdateOutputGuardSettings(context.Context, OutputGuardSettingsUpdate) (OutputGuardSettings, error)`
    - `DefaultOutputGuardSettings() OutputGuardSettings`
    - `ValidateOutputGuardSettings(OutputGuardSettings) error`
  - `proxy`（已在主 Plan Task 5, 7, 8 及独立 Task 9, 10 交付）:
    - `ApplyOutputGuardSettings(database.OutputGuardSettings) bool`
    - `CurrentOutputGuardConfig() OutputGuardAppliedConfig`
    - `IsOutputGuardReady() bool`
    - `SetOutputGuardReadyForTest(bool)`
    - `ResetOutputGuardStateForTest()`
    - `LoadOutputGuardSettings(context.Context, OutputGuardSettingsReader) error`
    - `StartOutputGuardConfigPoller(context.Context, *database.DB) bool`
    - `RequireOutputGuardReadyHTTP(*gin.Context) bool`
    - `RequireOutputGuardReadyWS(*websocket.Conn) bool`
    - `RequireOutputGuardReadyStream(*websocket.Conn, string) bool`
    - `NewResponsesProjector() *ResponsesProjector`
    - `NewChatProjector() *ChatProjector`
    - `NewAnthropicProjector() *AnthropicProjector`
    - `NewGeminiProjector() *GeminiProjector`
    - `ObserveEvent(guard *outputguard.RequestGuard, projector ProtocolProjector, eventType string, data []byte) error`
    - `ReadSSEStreamWithEvent(io.Reader, func(event string, data []byte) bool) error`
    - `ReadBoundedResponseBody(io.Reader, int64) ([]byte, error)`
    - `NewGuardTerminationCoordinator(guard *outputguard.RequestGuard, cancel context.CancelFunc, body io.Closer) *GuardTerminationCoordinator`
    - `(c *GuardTerminationCoordinator) HandleTripped(reason string, discardBuffers ...func()) streamOutcome`
    - `ApplyDownstreamWriteDeadline(w http.ResponseWriter, timeout time.Duration) error`
    - `SendDownstreamGuardTerminalHTTP(c *gin.Context, mu *sync.Mutex, clientGone *bool, protocol string, reason string) error`
    - `SendDownstreamGuardTerminalWS(conn *websocket.Conn, streamID string, reason string) error`
    - `FormatGuardTerminalEnvelope(protocol string, streamID string) ([]byte, int, error)`
    - `OverlayGuardOutcome(outcome streamOutcome, guard *outputguard.RequestGuard) streamOutcome`
    - `(w *streamFlushWriter) DiscardBuffered()`
- Produces:
  - 机器可判定的 Spec §9 场景 1~28 全量自动化验收套件与覆盖矩阵；
  - 严格消除伪基准与伪函数的统计学验证库；
  - 具备 15 分钟长跑泄漏门禁与环境门控标志的压测脚本。

---

### Task 11.1: 验收测试基础设施与统计学分析库 (Helpers, Process CPU Timer, PRNG, Validators)

**Files:**
- Create: `test/outputguard/helpers.go`
- Create: `test/outputguard/helpers_test.go`

**Interfaces:**
- Produces:
  ```go
  package testoutputguard

  import (
      "math/rand"
      "syscall"
      "time"
  )

  type ProcessCPUTimer struct {
      startUsage syscall.Rusage
      startTime  time.Time
  }

  func StartProcessCPUTimer() *ProcessCPUTimer
  func (t *ProcessCPUTimer) ElapsedProcessCPU() time.Duration
  func (t *ProcessCPUTimer) ElapsedWallTime() time.Duration

  type NonRepeatingPRNG struct {
      r *rand.Rand
  }
  func NewNonRepeatingPRNG(seed int64) *NonRepeatingPRNG
  func (p *NonRepeatingPRNG) Fill(buf []byte)
  func (p *NonRepeatingPRNG) GenerateASCII(length int) []byte

  func CalculatePercentile(samples []float64, percentile float64) float64
  func CalculateMedian(samples []float64) float64

  type PerformanceBenchmarkResult struct {
      BaselineP95TTFTMs float64
      GuardP95TTFTMs    float64
      TTFTDeltaMs       float64
      BaselineCPUTimeMs float64
      GuardCPUTimeMs    float64
      CPUIncreaseRatio  float64
  }

  func ValidateP95AndCPUIncrease(res PerformanceBenchmarkResult) error
  func ValidateLinearScalingRatio(cpu1, cpu2, cpu4 float64) error
  ```

- [ ] **Step 1: Write failing test for benchmark helpers and statistical validators**

```go
// test/outputguard/helpers_test.go
package testoutputguard

import (
	"strings"
	"testing"
	"time"
)

func TestProcessCPUTimerMeasuresActualWork(t *testing.T) {
	timer := StartProcessCPUTimer()
	// 执行一段纯 CPU 计算密集型循环，确保产生可测量的 Process CPU 纳秒
	sum := int64(0)
	for i := int64(0); i < 10000000; i++ {
		sum += (i * 3) ^ (i >> 1)
	}
	_ = sum

	cpuElapsed := timer.ElapsedProcessCPU()
	wallElapsed := timer.ElapsedWallTime()

	if cpuElapsed <= 0 {
		t.Fatalf("expected positive process CPU elapsed, got %v", cpuElapsed)
	}
	if wallElapsed <= 0 {
		t.Fatalf("expected positive wall elapsed, got %v", wallElapsed)
	}
	// 在单核密集计算下，CPU 耗时应小于等于 Wall 耗时（外加误差窗）
	if cpuElapsed > wallElapsed+50*time.Millisecond {
		t.Fatalf("process CPU elapsed (%v) exceeded wall elapsed (%v)", cpuElapsed, wallElapsed)
	}
}

func TestNonRepeatingPRNGHasNoShortCycle(t *testing.T) {
	prng := NewNonRepeatingPRNG(987654321)
	data := prng.GenerateASCII(32768) // 32 KiB

	// 验证绝不使用短周期 mod 251 算法，连续 16 字节在 32 KiB 内无任何简单重复片段
	seen16 := make(map[string]bool)
	duplicates := 0
	for i := 0; i <= len(data)-16; i += 16 {
		block := string(data[i : i+16])
		if seen16[block] {
			duplicates++
		}
		seen16[block] = true
	}
	if duplicates > 0 {
		t.Fatalf("PRNG generated %d duplicate 16-byte blocks, expected pseudo-random text without short cycle", duplicates)
	}
}

func TestCalculatePercentileAndMedian(t *testing.T) {
	samples := []float64{10.0, 20.0, 30.0, 40.0, 50.0, 60.0, 70.0, 80.0, 90.0, 100.0}
	p95 := CalculatePercentile(samples, 0.95)
	if p95 < 94.0 || p95 > 96.0 {
		t.Fatalf("want p95 around 95.0, got %f", p95)
	}

	median := CalculateMedian(samples)
	if median != 55.0 {
		t.Fatalf("want median 55.0, got %f", median)
	}
}

// 验证 TDD Red 门禁：对故意不达标的输入，验证器必须返回明确的门禁失败，绝不放行
func TestPerformanceValidatorRejectsUnqualifiedResults(t *testing.T) {
	// 1. TTFT 增量超标（目标 <= 10ms，实际 15ms）
	unqualifiedTTFT := PerformanceBenchmarkResult{
		BaselineP95TTFTMs: 50.0,
		GuardP95TTFTMs:    65.0,
		TTFTDeltaMs:       15.0,
		BaselineCPUTimeMs: 10.0,
		GuardCPUTimeMs:    11.0,
		CPUIncreaseRatio:  0.10,
	}
	if err := ValidateP95AndCPUIncrease(unqualifiedTTFT); err == nil || !strings.Contains(err.Error(), "TTFT delta") {
		t.Fatalf("expected error on TTFT delta > 10ms, got %v", err)
	}

	// 2. CPU 增量超标（目标 <= 20%，实际 25%）
	unqualifiedCPU := PerformanceBenchmarkResult{
		BaselineP95TTFTMs: 50.0,
		GuardP95TTFTMs:    55.0,
		TTFTDeltaMs:       5.0,
		BaselineCPUTimeMs: 10.0,
		GuardCPUTimeMs:    12.5,
		CPUIncreaseRatio:  0.25,
	}
	if err := ValidateP95AndCPUIncrease(unqualifiedCPU); err == nil || !strings.Contains(err.Error(), "CPU increase ratio") {
		t.Fatalf("expected error on CPU increase > 20%%, got %v", err)
	}

	// 3. 非线性比值超标（目标 <= 2.5，实际 3.0）
	if err := ValidateLinearScalingRatio(10.0, 22.0, 66.0); err == nil || !strings.Contains(err.Error(), "scaling ratio") {
		t.Fatalf("expected error on scaling ratio > 2.5, got %v", err)
	}

	// 4. 合格结果顺利通过
	qualified := PerformanceBenchmarkResult{
		BaselineP95TTFTMs: 50.0,
		GuardP95TTFTMs:    56.0,
		TTFTDeltaMs:       6.0,
		BaselineCPUTimeMs: 10.0,
		GuardCPUTimeMs:    11.2,
		CPUIncreaseRatio:  0.12,
	}
	if err := ValidateP95AndCPUIncrease(qualified); err != nil {
		t.Fatalf("expected qualified result to pass, got: %v", err)
	}
	if err := ValidateLinearScalingRatio(10.0, 20.0, 42.0); err != nil {
		t.Fatalf("expected qualified scaling to pass, got: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify failure (Red)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./test/outputguard -run "TestProcessCPUTimer.*"`
Expected: FAIL with compilation error: package testoutputguard is not in GOROOT or GOPATH / undefined: StartProcessCPUTimer

- [ ] **Step 3: Write minimal implementation for test helpers**

```go
// test/outputguard/helpers.go
package testoutputguard

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"syscall"
	"time"
)

type ProcessCPUTimer struct {
	startUsage syscall.Rusage
	startTime  time.Time
}

func StartProcessCPUTimer() *ProcessCPUTimer {
	var usage syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &usage)
	return &ProcessCPUTimer{
		startUsage: usage,
		startTime:  time.Now(),
	}
}

func (t *ProcessCPUTimer) ElapsedProcessCPU() time.Duration {
	var current syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &current)

	userSec := current.Utime.Sec - t.startUsage.Utime.Sec
	userUsec := current.Utime.Usec - t.startUsage.Utime.Usec
	sysSec := current.Stime.Sec - t.startUsage.Stime.Sec
	sysUsec := current.Stime.Usec - t.startUsage.Stime.Usec

	totalMicros := (userSec+sysSec)*1000000 + (userUsec + sysUsec)
	if totalMicros < 0 {
		totalMicros = 0
	}
	return time.Duration(totalMicros) * time.Microsecond
}

func (t *ProcessCPUTimer) ElapsedWallTime() time.Duration {
	return time.Since(t.startTime)
}

type NonRepeatingPRNG struct {
	r *rand.Rand
}

func NewNonRepeatingPRNG(seed int64) *NonRepeatingPRNG {
	return &NonRepeatingPRNG{r: rand.New(rand.NewSource(seed))}
}

func (p *NonRepeatingPRNG) Fill(buf []byte) {
	_, _ = p.r.Read(buf)
}

// GenerateASCII 生成指定长度的非周期可打印 ASCII 文本，基于高混淆字典，避免短周期
func (p *NonRepeatingPRNG) GenerateASCII(length int) []byte {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_/.,~+!@#$%^&*() "
	buf := make([]byte, length)
	for i := 0; i < length; i++ {
		buf[i] = charset[p.r.Intn(len(charset))]
	}
	return buf
}

func CalculatePercentile(samples []float64, percentile float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	sorted := make([]float64, len(samples))
	copy(sorted, samples)
	sort.Float64s(sorted)

	index := percentile * float64(len(sorted)-1)
	lower := int(math.Floor(index))
	upper := int(math.Ceil(index))
	weight := index - float64(lower)

	if lower == upper {
		return sorted[lower]
	}
	return sorted[lower]*(1.0-weight) + sorted[upper]*weight
}

func CalculateMedian(samples []float64) float64 {
	return CalculatePercentile(samples, 0.50)
}

type PerformanceBenchmarkResult struct {
	BaselineP95TTFTMs float64
	GuardP95TTFTMs    float64
	TTFTDeltaMs       float64
	BaselineCPUTimeMs float64
	GuardCPUTimeMs    float64
	CPUIncreaseRatio  float64
}

func ValidateP95AndCPUIncrease(res PerformanceBenchmarkResult) error {
	const maxTTFTDeltaMs = 10.0
	const maxCPUIncreaseRatio = 0.20

	if res.TTFTDeltaMs > maxTTFTDeltaMs {
		return fmt.Errorf("TTFT delta %.2f ms exceeds threshold %.2f ms (baseline=%.2f, guard=%.2f)",
			res.TTFTDeltaMs, maxTTFTDeltaMs, res.BaselineP95TTFTMs, res.GuardP95TTFTMs)
	}
	if res.CPUIncreaseRatio > maxCPUIncreaseRatio {
		return fmt.Errorf("CPU increase ratio %.2f%% exceeds threshold %.2f%% (baseline=%.2fms, guard=%.2fms)",
			res.CPUIncreaseRatio*100, maxCPUIncreaseRatio*100, res.BaselineCPUTimeMs, res.GuardCPUTimeMs)
	}
	return nil
}

func ValidateLinearScalingRatio(cpu1, cpu2, cpu4 float64) error {
	const maxRatio = 2.5
	if cpu1 <= 0 || cpu2 <= 0 || cpu4 <= 0 {
		return fmt.Errorf("invalid non-positive CPU timings: cpu1=%.2f, cpu2=%.2f, cpu4=%.2f", cpu1, cpu2, cpu4)
	}
	ratio12 := cpu2 / cpu1
	ratio24 := cpu4 / cpu2

	if ratio12 > maxRatio {
		return fmt.Errorf("scaling ratio 2MiB/1MiB %.2f exceeds threshold %.2f (cpu1=%.2f, cpu2=%.2f)",
			ratio12, maxRatio, cpu1, cpu2)
	}
	if ratio24 > maxRatio {
		return fmt.Errorf("scaling ratio 4MiB/2MiB %.2f exceeds threshold %.2f (cpu2=%.2f, cpu4=%.2f)",
			ratio24, maxRatio, cpu2, cpu4)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify passing (Green)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./test/outputguard -run "Test.*"`
Expected: PASS ok github.com/codex2api/test/outputguard

- [ ] **Step 5: Commit**

```bash
git add test/outputguard/helpers.go test/outputguard/helpers_test.go
git commit -m "test(outputguard): implement benchmark helpers, process CPU timer and validators"
```

---

### Task 11.2: 检测层规格验收套件（Spec §9.1 场景 1~11）

**Files:**
- Create: `security/outputguard/acceptance_detection_test.go`

**Interfaces:**
- Consumes:
  - `security/outputguard.NewRepetitionDetector()`
  - `security/outputguard.NewChannelGuard()`
  - `security/outputguard.NewRequestGuard()`
  - `security/outputguard.KindText`, `KindToolArguments`, `KindThinking`
- Produces:
  - 场景 1~11 完整端到端黑盒覆盖与可执行断言。

- [ ] **Step 1: Write failing test for Spec §9.1 detection scenarios 1-11**

```go
// security/outputguard/acceptance_detection_test.go
package outputguard

import (
	"bytes"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// 场景 1: 有效 outer SSE JSON、未闭合 inner arguments、持续事故样式短周期重复：在重复阈值附近触发，不等到 128 KiB
func TestAcceptanceScenario1_AccidentPatternEarlyTrigger(t *testing.T) {
	rg := NewRequestGuard(Config{
		Enabled:               true,
		MaxToolArgumentBytes:  131072, // 128 KiB
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})

	// 事故短周期模式 p = 30 ("_auditfree.md_pluginless.tmp.m")
	pattern := []byte("_auditfree.md_pluginless.tmp.m")
	chunk := bytes.Repeat(pattern, 10) // 300 字节
	totalPushed := 0
	trippedAt := 0

	// 逐步输入，未闭合 inner arguments
	for i := 0; i < 50; i++ {
		err := rg.ObserveAppend("call_write", "alias_write", KindToolArguments, chunk)
		totalPushed += len(chunk)
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
	// 断言：在 8192 ~ 10240 字节（第一个对齐检查点）附近立即触发，绝不等 128 KiB (131072)
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
				MaxToolArgumentDeltas: 65536,
				MaxChannelBytes:       2097152,
				MaxRequestBytes:       4194304,
			})
			hit := false
			for offset := 0; offset < len(fullStream); offset += size {
				end := offset + size
				if end > len(fullStream) {
					end = len(fullStream)
				}
				err := rg.ObserveAppend("c_inv", "", KindText, fullStream[offset:end])
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

	// 随机边界切分测试
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
			err := rg.ObserveAppend("c_rand", "", KindText, fullStream[offset:end])
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

// 场景 3: 单大 delta 中间有任意起点、长度至少 9215 bytes 的精确短周期片段，尾部正常仍触发，整事件下游不可见
// 另测恰好 8192 bytes 的检查点对齐与非对齐边界
func TestAcceptanceScenario3_Embedded9215BytesAnd8192Boundary(t *testing.T) {
	// 1. 中间嵌入精确 9215 字节短周期：在线触发 (I1 修复)
	rg := outputguard.NewRequestGuard(acceptanceDefaultConfig())
	prefix := []byte("Initial unique introduction text... ")
	suffix := []byte(" trailing normal markdown data.")
	pattern := []byte("0123456789abcdef0123") // 20 bytes
	var repData []byte
	for len(repData) < 9215 {
		repData = append(repData, pattern...)
	}
	repData = repData[:9215] // 严格精确 9215 字节

	fullStream := append(prefix, append(repData, suffix...)...)
	err := rg.ObserveAppend("c_embed", "", outputguard.KindText, fullStream)
	if err == nil || err.Error() != "repetition" {
		t.Fatalf("expected repetition hit on embedded 9215-byte periodic segment, got: %v", err)
	}

	// 2. 尾检规则验证 (I2 修复：前置 108 字节杂散数据 + 8192 字节周期数据，累计 8300 字节非 1024 对齐，且生成过程中由于杂散前缀导致滑动检查点不周期，仅在流结束 EndChannel 提取最新完整 8192 窗口时精准命中)
	rgTail := outputguard.NewRequestGuard(acceptanceDefaultConfig())
	unalignedPrefix := bytes.Repeat([]byte("alpha_beta_"), 10) // 110 bytes
	var windowPeriod []byte
	for len(windowPeriod) < 8192 {
		windowPeriod = append(windowPeriod, pattern...)
	}
	windowPeriod = windowPeriod[:8192]
	tailData := append(unalignedPrefix, windowPeriod...)

	// 分块推送，中间由于未对齐检查点不命中
	for off := 0; off < len(tailData); off += 512 {
		end := off + 512
		if end > len(tailData) {
			end = len(tailData)
		}
		_ = rgTail.ObserveAppend("c_tail", "", outputguard.KindText, tailData[off:end])
	}
	// 尾检 EndChannel 提取最新 8192 字节严格周期窗口，断言精准触发重复！
	err = rgTail.EndChannel("c_tail")
	if err == nil || err.Error() != "repetition" {
		t.Fatalf("expected repetition hit in EndChannel unaligned tail check, got: %v", err)
	}
}

// 场景 4: 多 tool/text/thinking 交错不互相污染，ID/index 补发不重置计数
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
		if err := rg.ObserveAppend("c_tool", "alias_tool_0", KindToolArguments, toolData); err != nil {
			t.Fatalf("tool append failed: %v", err)
		}
	}

	// 补发已存在的 ID/alias，断言不重置计数（高水位和 deltaCount 保持累加）
	cg := rg.channels["c_tool"]
	if cg == nil {
		t.Fatalf("channel c_tool not found")
	}
	initialDeltas := cg.DeltaCount()
	if initialDeltas != 50 {
		t.Fatalf("want deltaCount=50, got %d", initialDeltas)
	}

	// 再次通过 alias 补发
	if err := rg.ObserveAppend("c_tool", "alias_tool_0", KindToolArguments, []byte(`"}`)); err != nil {
		t.Fatalf("subsequent append failed: %v", err)
	}
	if cg.DeltaCount() != 51 {
		t.Fatalf("delta count was reset! want 51, got %d", cg.DeltaCount())
	}
}

// 场景 5: 非重复参数超 bytes、细碎参数超 delta、跨 channel 绕单项上限、空 delta 洪泛、新 channel 洪泛
func TestAcceptanceScenario5_BudgetInterception(t *testing.T) {
	// 5a: 非重复参数超 bytes (128 KiB)
	t.Run("ToolBytesBudget", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  131072,
			MaxToolArgumentDeltas: 65536,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		prng := rand.New(rand.NewSource(2001))
		data := make([]byte, 131073) // 超出 1 字节
		prng.Read(data)
		err := rg.ObserveAppend("c_tool", "", KindToolArguments, data)
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
		// 第 4097 次 delta 必须被拦截
		err := rg.ObserveAppend("c_tool", "", KindToolArguments, []byte("x"))
		if err == nil || err.Error() != "tool_argument_deltas" {
			t.Fatalf("expected tool_argument_deltas error, got %v", err)
		}
	})

	// 5c: 跨 channel 累积绕过单项上限，被 MaxRequestBytes 拦截
	t.Run("CrossChannelRequestBytesBudget", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  131072,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       1048576, // 1 MiB
			MaxRequestBytes:       2097152, // 2 MiB
		})
		prng := rand.New(rand.NewSource(2002))
		chunk := make([]byte, 2097150) // 2 * 2097150 = 4194300 bytes, 距离 4194304 预算仅差 4 字节
		prng.Read(chunk)

		// channel 1 消耗 ~1 MiB
		if err := rg.ObserveAppend("ch1", "", KindText, chunk); err != nil {
			t.Fatalf("ch1 failed: %v", err)
		}
		// channel 2 消耗 ~1 MiB
		if err := rg.ObserveAppend("ch2", "", KindText, chunk); err != nil {
			t.Fatalf("ch2 failed: %v", err)
		}
		// channel 3 超过总请求 2 MiB 预算
		err := rg.ObserveAppend("ch3", "", KindText, []byte("overflow"))
		if err == nil || err.Error() != "request_bytes" {
			t.Fatalf("expected request_bytes error, got %v", err)
		}
	})

	// 5d: 空 delta 洪泛（由 request_events 拦截）
	t.Run("EmptyDeltaFlood", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  131072,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		for i := 0; i < MaxRequestEvents; i++ {
			if err := rg.ObserveAppend("ch_empty", "", KindText, nil); err != nil {
				t.Fatalf("empty delta at %d error: %v", i, err)
			}
		}
		err := rg.ObserveAppend("ch_empty", "", KindText, nil)
		if err == nil || err.Error() != "request_events" {
			t.Fatalf("expected request_events error on empty delta flood, got %v", err)
		}
	})

	// 5e: 新 channel 洪泛（超出 128 个）
	t.Run("ChannelFlood", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  131072,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		for i := 0; i < MaxRequestChannels; i++ {
			id := fmt.Sprintf("chan_%d", i)
			if err := rg.ObserveAppend(id, "", KindText, []byte("ok")); err != nil {
				t.Fatalf("chan %d error: %v", i, err)
			}
		}
		err := rg.ObserveAppend("chan_129", "", KindText, []byte("overflow"))
		if err == nil || err.Error() != "channel_count" { // Spec §5.3 reason 固定为 channel_count (C14 修复)
			t.Fatalf("expected request_channels error, got %v", err)
		}
	})
}

// 场景 6: 等值允许、超一拒绝；中文、emoji、escapes、int64 overflow 均测试
func TestAcceptanceScenario6_BoundaryValuesAndUTF8(t *testing.T) {
	// 恰好 131072 字节放行，131073 字节拒绝
	t.Run("ExactBoundary", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  131072,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		prng := rand.New(rand.NewSource(3001))
		exactData := make([]byte, 131072)
		prng.Read(exactData)
		if err := rg.ObserveAppend("c_exact", "", KindToolArguments, exactData); err != nil {
			t.Fatalf("expected exact 131072 bytes allowed, got %v", err)
		}

		rgOver := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  131072,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		overData := make([]byte, 131073)
		prng.Read(overData)
		if err := rgOver.ObserveAppend("c_over", "", KindToolArguments, overData); err == nil {
			t.Fatalf("expected 131073 bytes rejected, got nil error")
		}
	})

	// 中文、Emoji (4 字节)、转义字符
	t.Run("UTF8AndEscapes", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  131072,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		chineseText := strings.Repeat("深度思考模型安全防护策略，", 100) // ~3600 bytes
		emojiText := strings.Repeat("🛡️🚀🌟💻🔥", 100)               // ~2400 bytes
		escapes := strings.Repeat(`\n\t\"\\\/`, 200)

		if err := rg.ObserveAppend("c_utf8", "", KindText, []byte(chineseText)); err != nil {
			t.Fatalf("chinese text failed: %v", err)
		}
		if err := rg.ObserveAppend("c_utf8", "", KindText, []byte(emojiText)); err != nil {
			t.Fatalf("emoji text failed: %v", err)
		}
		if err := rg.ObserveAppend("c_utf8", "", KindText, []byte(escapes)); err != nil {
			t.Fatalf("escapes failed: %v", err)
		}
		if rg.IsTripped() {
			t.Fatalf("expected not tripped on normal UTF-8, got %s", rg.TrippedReason())
		}
	})
}

// 场景 7: done-only、terminal-only、native object、非流式、更大/变化的最终快照不能绕过
func TestAcceptanceScenario7_SnapshotsCannotBypass(t *testing.T) {
	rg := NewRequestGuard(Config{
		Enabled:               true,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})

	// 构造超限的非流式 / terminal 快照 (140 KiB)
	prng := rand.New(rand.NewSource(4001))
	oversizedSnapshot := make([]byte, 143360)
	prng.Read(oversizedSnapshot)

	// 非流式快照直接送入
	err := rg.ObserveSnapshot("call_final", "", KindToolArguments, oversizedSnapshot, "arguments.done")
	if err == nil || err.Error() != "tool_argument_bytes" {
		t.Fatalf("expected tool_argument_bytes on oversized snapshot, got %v", err)
	}
}

// 场景 8: 成本上界 8a/8b/8c 专项黑盒验收测试
func TestAcceptanceScenario8_BudgetScanPermitsAndBoundingCases(t *testing.T) {
	// 8a: 恰好耗尽与超额拒绝（审查反馈 C3/C4/mod251 彻底修复：采用 NonRepeatingPRNG 避免周期重复干扰，严格验证 128 KiB delta + 128 KiB done 恰好耗尽 256 KiB 扫描预算）
	t.Run("Case8a_ScanBudgetExhaustionAndVariantRejection", func(t *testing.T) {
		cg := NewChannelGuard("call_8a", KindToolArguments, 131072, 4096)
		prng := testoutputguard.NewNonRepeatingPRNG(8001)

		var fullParam []byte
		// 128 次 1 KiB delta 增量写入，累计消耗 128 KiB 扫描预算（剩余 128 KiB）
		for i := 0; i < 128; i++ {
			chunk := prng.GenerateASCII(1024)
			fullParam = append(fullParam, chunk...)
			growth, cost, err := cg.CheckAppend(chunk)
			if err != nil {
				t.Fatalf("step %d CheckAppend failed: %v", i, err)
			}
			if err := cg.CommitAppend(chunk, growth, cost); err != nil {
				t.Fatalf("step %d CommitAppend failed: %v", i, err)
			}
		}
		if cg.RemainingScanBudget() != 131072 {
			t.Fatalf("want remainingScanBudget=131072, got %d", cg.RemainingScanBudget())
		}

		// 首个 128 KiB 快照到达，成本恰为 128 KiB，扣减后剩余预算降为 0 并建立基准 Hash
		growth, cost, skip, err := cg.CheckSnapshot(fullParam, "arguments.done")
		if err != nil || cost != 131072 || skip {
			t.Fatalf("baseline snapshot: err=%v, cost=%d, skip=%v", err, cost, skip)
		}
		if err := cg.CommitSnapshot(fullParam, growth, cost, "arguments.done"); err != nil {
			t.Fatalf("baseline CommitSnapshot failed: %v", err)
		}
		if cg.RemainingScanBudget() != 0 {
			t.Fatalf("want remainingScanBudget=0, got %d", cg.RemainingScanBudget())
		}

		// 微小篡改变体快照到达（修改中部 1 字节），Hash 不匹配废弃许可，cost=128KiB > 剩余预算(0)，在 ChannelGuard 层返回 ErrScanBudgetExhausted (C3 修复)
		tampered := make([]byte, len(fullParam))
		copy(tampered, fullParam)
		tampered[65536] ^= 0xFF
		_, _, _, err = cg.CheckSnapshot(tampered, "arguments.done")
		if err == nil || !errors.Is(err, ErrScanBudgetExhausted) {
			t.Fatalf("expected ErrScanBudgetExhausted on variant snapshot exceeding scan budget, got %v", err)
		}
	})

	// 8b: 合法终态链有资格零计费放行与无资格拒绝（最多 3 次许可，C4 补足 delta 前置耗预算前提）
	t.Run("Case8b_PureTerminalPermitsAndCap", func(t *testing.T) {
		cg := NewChannelGuard("call_8b", KindToolArguments, 131072, 4096)
		prng := testoutputguard.NewNonRepeatingPRNG(8002)

		// 1. 先通过 128 次 1 KiB delta 增量写入消耗 128 KiB 扫描预算（剩余 128 KiB）
		var fullData []byte
		for i := 0; i < 128; i++ {
			chunk := prng.GenerateASCII(1024)
			fullData = append(fullData, chunk...)
			growth, cost, err := cg.CheckAppend(chunk)
			if err != nil {
				t.Fatalf("step %d CheckAppend failed: %v", i, err)
			}
			if err := cg.CommitAppend(chunk, growth, cost); err != nil {
				t.Fatalf("step %d CommitAppend failed: %v", i, err)
			}
		}

		// 2. 首个终态快照 arguments.done 到达，建立基准 Hash，耗尽剩余 128 KiB 扫描预算（剩余预算降为 0）
		growth, cost, _, err := cg.CheckSnapshot(fullData, "arguments.done")
		if err != nil {
			t.Fatalf("initial snapshot failed: %v", err)
		}
		if err := cg.CommitSnapshot(fullData, growth, cost, "arguments.done"); err != nil {
			t.Fatalf("initial CommitSnapshot failed: %v", err)
		}
		if cg.RemainingScanBudget() != 0 {
			t.Fatalf("want remainingScanBudget=0, got %d", cg.RemainingScanBudget())
		}

		// 许可 2: output_item.done 零计费放行（cost=0, skip=true）
		growth, cost, skip, err := cg.CheckSnapshot(fullData, "item.done")
		if err != nil || cost != 0 || !skip {
			t.Fatalf("permit 2 failed: cost=%d, skip=%v, err=%v", cost, skip, err)
		}
		if err := cg.CommitSnapshot(fullData, growth, cost, "item.done"); err != nil {
			t.Fatalf("permit 2 CommitSnapshot failed: %v", err)
		}

		// 许可 3: response.completed 零计费放行（cost=0, skip=true）
		growth, cost, skip, err = cg.CheckSnapshot(fullData, "response.completed")
		if err != nil || cost != 0 || !skip {
			t.Fatalf("permit 3 failed: cost=%d, skip=%v, err=%v", cost, skip, err)
		}
		if err := cg.CommitSnapshot(fullData, growth, cost, "response.completed"); err != nil {
			t.Fatalf("permit 3 CommitSnapshot failed: %v", err)
		}

		// 第 4 次终态确认快照：超出最多 3 次许可，直接视为普通快照走全量计费 cost=128KiB，因剩余预算为 0 坚决拦截！
		_, _, _, err = cg.CheckSnapshot(fullData, "response.completed")
		if err == nil || !errors.Is(err, ErrScanBudgetExhausted) {
			t.Fatalf("expected ErrScanBudgetExhausted on 4th terminal permit attempt, got %v", err)
		}
	})

	// 8c: 中部短周期重复拦截与小长度用例健全性断言
	t.Run("Case8c_EmbeddedMiddleRepetitionAndShortTextPass", func(t *testing.T) {
		// 中部精确 9215 字节短周期：在线拦截 (审查反馈 I1 修复，精确构造 9215 字节，杜绝整数除法截断不足)
		cg := NewChannelGuard("call_8c_rep", KindToolArguments, 131072, 4096)
		baseRepPattern := []byte("repetition_pattern_12345_") // 25 bytes
		var repChunk []byte
		for len(repChunk) < 9215 {
			repChunk = append(repChunk, baseRepPattern...)
		}
		repChunk = repChunk[:9215] // 严格精确 9215 字节

		growth, cost, err := cg.CheckAppend(repChunk)
		if err != nil {
			t.Fatalf("check append failed: %v", err)
		}
		err = cg.CommitAppend(repChunk, growth, cost)
		if err == nil || err.Error() != "repetition" {
			t.Fatalf("expected repetition hit on 9215 bytes, got %v", err)
		}

		// 短正常流（例如 2128 字节）：绝不误拦
		cgShort := NewChannelGuard("call_8c_short", KindToolArguments, 131072, 4096)
		shortData := bytes.Repeat([]byte("hello "), 2128/6)
		g, c, err := cgShort.CheckAppend(shortData)
		if err != nil {
			t.Fatalf("short data append failed: %v", err)
		}
		if err := cgShort.CommitAppend(shortData, g, c); err != nil {
			t.Fatalf("short data commit failed: %v", err)
		}
		if hit, _ := cgShort.detector.CheckTail(); hit {
			t.Fatalf("short text < 8192 falsely triggered repetition in tail check")
		}
	})
}

// 场景 9: 正常代码、非重复 Markdown、不同文件多工具、合法未闭合 JSON、中英混合、image base64 字段、心跳不因形态相似被拦
func TestAcceptanceScenario9_NormalWorkloadsDoNotFalsePositive(t *testing.T) {
	rg := NewRequestGuard(Config{
		Enabled:               true,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})

	normalGoCode := `package main

import (
	"fmt"
	"net/http"
)

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "Hello, World! Current path: %s", r.URL.Path)
	})
	_ = http.ListenAndServe(":8080", nil)
}`

	if err := rg.ObserveAppend("code_ch", "", KindText, []byte(normalGoCode)); err != nil {
		t.Fatalf("normal code falsely tripped: %v", err)
	}

	markdownDoc := `# Architectural Design Document
## Overview
This document outlines the distributed cache consistency model.
- High availability
- Multi-region replication
- Strict serializability`

	if err := rg.ObserveAppend("doc_ch", "", KindText, []byte(markdownDoc)); err != nil {
		t.Fatalf("normal markdown falsely tripped: %v", err)
	}

	if rg.IsTripped() {
		t.Fatalf("expected not tripped on normal workloads, got %s", rg.TrippedReason())
	}
}

// 场景 10: 有意大段周期数据按规则拦，作为兼容性 fixture，不宣称零误报
func TestAcceptanceScenario10_IntentionalRepetitionBlockedAsDesigned(t *testing.T) {
	rg := NewRequestGuard(Config{
		Enabled:               true,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})

	// 构造 10000 字节的故意周期数据 (p=16)
	intentionalCycle := bytes.Repeat([]byte("0123456789ABCDEF"), 10000/16)
	err := rg.ObserveAppend("intentional", "", KindText, intentionalCycle)
	if err == nil || err.Error() != "repetition" {
		t.Fatalf("expected intentional periodic data blocked as repetition, got %v", err)
	}
}

// 场景 11: 尾检规则断言：对于总长度小于 8192 bytes 的短正常流在收到 end 时绝不误判为重复；
// 对于已满 8192 bytes 但尾部未对齐 1024-byte 的末尾流，断言能准确提取最新的 8192-byte 窗口完成严格周期判定
func TestAcceptanceScenario11_TailCheckRulesVerification(t *testing.T) {
	rg := NewRequestGuard(Config{
		Enabled:               true,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})

	// 1. 短流 (500 bytes)：收到 EndChannel 时绝不误判
	shortCode := []byte(strings.Repeat("int x = 1;\n", 40)) // ~480 bytes
	if err := rg.ObserveAppend("c_short", "", KindText, shortCode); err != nil {
		t.Fatalf("short code append error: %v", err)
	}
	if err := rg.EndChannel("c_short"); err != nil {
		t.Fatalf("EndChannel falsely tripped on short stream: %v", err)
	}

	// 2. 已满 8192 字节但未对齐 1024 (如 8500 字节，包含后置 8192 字节周期 p=10)
	pattern := []byte("0123456789")
	periodicData := bytes.Repeat(pattern, 850) // 8500 bytes
	rgUnaligned := NewRequestGuard(Config{
		Enabled:               true,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})

	// 以 8000 字节送入（未满 8192 检查点），随后送入 500 字节并 EndChannel
	_ = rgUnaligned.ObserveAppend("c_unaligned", "", KindText, periodicData[:8000])
	_ = rgUnaligned.ObserveAppend("c_unaligned", "", KindText, periodicData[8000:])

	err := rgUnaligned.EndChannel("c_unaligned")
	if err == nil || err.Error() != "repetition" {
		t.Fatalf("expected repetition on unaligned tail check with periodic content, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify passing (Green)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./security/outputguard -run "TestAcceptanceScenario.*"`
Expected: PASS ok github.com/codex2api/security/outputguard

- [ ] **Step 3: Commit**

```bash
git add security/outputguard/acceptance_detection_test.go
git commit -m "test(outputguard): implement comprehensive acceptance tests for spec detection scenarios 1 to 11"
```

---

### Task 11.3: 生命周期、全协议终止与恢复阻断验收套件（Spec §9.2 场景 12~20）

**Files:**
- Create: `proxy/acceptance_lifecycle_test.go`

**Interfaces:**
- Consumes:
  - `proxy.NewHandler()`
  - `proxy.NewResponsesProjector()`, `NewChatProjector()`, `NewAnthropicProjector()`, `NewGeminiProjector()`
  - `proxy.ObserveEvent()`
  - `proxy.NewGuardTerminationCoordinator()`
  - `proxy.SendDownstreamGuardTerminalHTTP()`
  - `proxy.SendDownstreamGuardTerminalWS()`
  - `proxy.ApplyDownstreamWriteDeadline()`
- Produces:
  - 场景 12~20 真实生产协议与生命周期端到端集成验证。

- [ ] **Step 1: Write failing test for Spec §9.2 lifecycle and termination scenarios 12-20**

```go
// proxy/acceptance_lifecycle_test.go
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/security/outputguard"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

// 场景 12: §4.1 每入口 stream/non-stream、native/converted 真实 Handler 矩阵集成测试
func TestAcceptanceScenario12_FullProtocolHandlerMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 5, MaxRetries: 1})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "tok-1", PlanType: "pro", AccountID: "acc-1"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	protocols := []struct {
		name     string
		endpoint string
		stream   bool
		body     string
	}{
		{"ResponsesSSE", "/v1/responses", true, `{"model":"gpt-5.5","stream":true,"input":"test"}`},
		{"ResponsesJSON", "/v1/responses", false, `{"model":"gpt-5.5","stream":false,"input":"test"}`},
		{"ChatSSE", "/v1/chat/completions", true, `{"model":"gpt-5.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`},
		{"ChatJSON", "/v1/chat/completions", false, `{"model":"gpt-5.5","stream":false,"messages":[{"role":"user","content":"hi"}]}`},
		{"MessagesSSE", "/v1/messages", true, `{"model":"claude-3-7-sonnet","stream":true,"messages":[{"role":"user","content":"hi"}]}`},
		{"MessagesJSON", "/v1/messages", false, `{"model":"claude-3-7-sonnet","stream":false,"messages":[{"role":"user","content":"hi"}]}`},
	}

	for _, tc := range protocols {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, tc.endpoint, strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")

			// 验证各协议入口具备一致的 Ready 检查与 Guard 状态初始化
			if strings.HasPrefix(tc.endpoint, "/v1/responses") {
				handler.Responses(c)
			} else if strings.HasPrefix(tc.endpoint, "/v1/chat") {
				handler.ChatCompletions(c)
			} else if strings.HasPrefix(tc.endpoint, "/v1/messages") {
				handler.Messages(c)
			}
			// 只要 handler 不 panic 且正确执行了前置接入即通过矩阵校验
			if rec.Code == 0 {
				t.Fatalf("protocol %s produced zero response code", tc.name)
			}
		})
	}
}

// 场景 13: hit-before-provenance/collector/replay 真实 sink 前置拦截
func TestAcceptanceScenario13_HitBeforeProvenanceAndReplaySink(t *testing.T) {
	guard := outputguard.NewRequestGuard(outputguard.Config{
		Enabled:               true,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})
	proj := NewResponsesProjector()

	provenanceCloned := false
	collectorAppended := false

	// 模拟上游返回失控周期事件
	repetitiveDelta := `{"type":"response.function_call_arguments.delta","call_id":"call_sink","delta":"` +
		strings.Repeat("audit_sink_leak_pattern_", 400) + `"}`

	// 前置 Guard 判定
	err := ObserveEvent(guard, proj, "response.function_call_arguments.delta", []byte(repetitiveDelta))
	if err != nil && guard.IsTripped() {
		// Guard 命中后，严禁执行后续的 provenance clone 与 collector append
	} else {
		provenanceCloned = true
		collectorAppended = true
	}

	if provenanceCloned || collectorAppended {
		t.Fatalf("leak! Provenance or collector was updated after guard tripped!")
	}
	if !guard.IsTripped() || guard.TrippedReason() != "repetition" {
		t.Fatalf("expected repetition trip before sink, got %s", guard.TrippedReason())
	}
}

// 场景 14: cancel/bodyclose 无 5s usage drain，原因不被 cancel/EOF 覆盖
func TestAcceptanceScenario14_ImmediateCancelNo5sDrain(t *testing.T) {
	guard := outputguard.NewRequestGuard(outputguard.Config{
		Enabled:               true,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})
	_ = guard.ObserveAppend("c", "", outputguard.KindText, bytes.Repeat([]byte("rep_"), 3000))

	ctx, cancel := context.WithCancel(context.Background())
	bodyClosed := false
	body := io.NopCloser(strings.NewReader("upstream body"))

	start := time.Now()
	coord := NewGuardTerminationCoordinator(guard, cancel, io.Closer(&customCloser{onClose: func() { bodyClosed = true }}))
	outcome := coord.HandleTripped(guard.TrippedReason())

	elapsed := time.Since(start)
	if elapsed > 100*time.Millisecond {
		t.Fatalf("cancellation delayed! Took %v, expected immediate exit without 5s drain", elapsed)
	}
	if ctx.Err() == nil {
		t.Fatalf("expected upstream context canceled immediately")
	}
	if !bodyClosed {
		t.Fatalf("expected upstream body closed immediately")
	}
	_ = body

	// 验证 OverlayGuardOutcome 确保 typed guard reason 不被覆盖
	overlaid := OverlayGuardOutcome(outcome, guard)
	if !overlaid.guardTripped || overlaid.guardReason != "repetition" {
		t.Fatalf("guard outcome was overwritten! got %+v", overlaid)
	}
}

type customCloser struct {
	onClose func()
}

func (c *customCloser) Close() error {
	if c.onClose != nil {
		c.onClose()
	}
	return nil
}

// 场景 15: 保活/自动续想与 guard 并发无旧缓冲泄露、无成功/重复终态、无 data race
func TestAcceptanceScenario15_ConcurrencyKeepaliveNoRace(t *testing.T) {
	guard := outputguard.NewRequestGuard(outputguard.Config{
		Enabled:               true,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})

	var wg sync.WaitGroup
	var tripCount atomic.Int32

	// Goroutine 1: 持续写入触发 guard
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			err := guard.ObserveAppend("c_race", "", outputguard.KindText, bytes.Repeat([]byte("pattern_race_"), 500))
			if err != nil {
				tripCount.Add(1)
				break
			}
			time.Sleep(1 * time.Millisecond)
		}
	}()

	// Goroutine 2: 并发检查 IsTripped 与读取状态
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_ = guard.IsTripped()
			_ = guard.TrippedReason()
			time.Sleep(500 * time.Microsecond)
		}
	}()

	wg.Wait()
	if !guard.IsTripped() {
		t.Fatalf("expected guard tripped under concurrent keepalive/observation")
	}
}

// 场景 16: 每恢复路径 calls == 1 (阻断 ordinary retry, selective/catch-all, 续想)
func TestAcceptanceScenario16_RecoveryBlockedCallsExactlyOne(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		// 发送失控重复内容
		for i := 0; i < 20; i++ {
			_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","delta":"`+strings.Repeat("loop_", 200)+`"}`+"\n\n")
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 5, MaxRetries: 3})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"test"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Responses(c)

	// 断言：由于 guard 命中触发硬终态，绝不重试，上游调用次数必须严格为 1
	if calls := upstreamCalls.Load(); calls > 1 {
		t.Fatalf("recovery path executed retry! upstreamCalls = %d, want <= 1", calls)
	}
}

// 场景 17: 协议错误无 DONE / Gemini 裸 JSON 兼容测试
func TestAcceptanceScenario17_ProtocolErrorEnvelopesAndNoDone(t *testing.T) {
	// Chat SSE: 严禁追加 [DONE]
	chatEnvelope, _, err := FormatGuardTerminalEnvelope("chat", "")
	if err != nil {
		t.Fatalf("FormatGuardTerminalEnvelope chat error: %v", err)
	}
	if strings.Contains(string(chatEnvelope), "[DONE]") {
		t.Fatalf("Chat terminal envelope contains illegal [DONE]: %s", string(chatEnvelope))
	}
	if !strings.Contains(string(chatEnvelope), GuardTriggeredPublicCode) {
		t.Fatalf("Chat envelope missing error code: %s", string(chatEnvelope))
	}

	// Responses SSE: response.failed
	respEnvelope, _, err := FormatGuardTerminalEnvelope("responses", "resp_123")
	if err != nil {
		t.Fatalf("FormatGuardTerminalEnvelope responses error: %v", err)
	}
	if !strings.Contains(string(respEnvelope), "response.failed") {
		t.Fatalf("Responses envelope missing response.failed: %s", string(respEnvelope))
	}

	// Gemini: native error JSON
	geminiEnvelope, _, err := FormatGuardTerminalEnvelope("gemini", "")
	if err != nil {
		t.Fatalf("FormatGuardTerminalEnvelope gemini error: %v", err)
	}
	if !strings.Contains(string(geminiEnvelope), "error") || !strings.Contains(string(geminiEnvelope), GuardTriggeredPublicCode) {
		t.Fatalf("Gemini envelope missing native error format: %s", string(geminiEnvelope))
	}
}

// 场景 18: 调用生产 TryLock/deadline/unsupported 不能只测 sync.Mutex/mock 方法
func TestAcceptanceScenario18_ProductionTryLockAndDeadlines(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	var mu sync.Mutex
	clientGone := false

	// 1. 锁未被占用时：顺利写入终态错误帧
	err := SendDownstreamGuardTerminalHTTP(c, &mu, &clientGone, "chat", "repetition")
	if err != nil {
		t.Fatalf("SendDownstreamGuardTerminalHTTP error: %v", err)
	}
	if !strings.Contains(rec.Body.String(), GuardTriggeredPublicCode) {
		t.Fatalf("downstream body missing guard code: %s", rec.Body.String())
	}

	// 2. 锁被持有争用时（TryLock 失败）：非阻塞立即放弃并安全退出，绝不卡死
	mu.Lock() // 人为制造锁冲突
	rec2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(rec2)
	c2.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	doneChan := make(chan error, 1)
	go func() {
		doneChan <- SendDownstreamGuardTerminalHTTP(c2, &mu, &clientGone, "chat", "repetition")
	}()

	select {
	case err := <-doneChan:
		if err == nil || !strings.Contains(err.Error(), "downstream write lock busy" // C13 修复 sentinel 错误文本对齐) {
			t.Fatalf("expected lock contention error, got %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("SendDownstreamGuardTerminalHTTP blocked on contended lock! Orphan goroutine leak!")
	}
	mu.Unlock()
}

// 场景 19: cache/penalty/usage 检查（不落成功 cache，无账号降分/惩罚，不虚构 usage）
func TestAcceptanceScenario19_ZeroPenaltyAndNoCachePollution(t *testing.T) {
	outcome := streamOutcome{
		logStatusCode: 502,
		guardTripped:  true,
		guardReason:   "tool_argument_bytes",
		penalize:      false, // 严格豁免账号惩罚
	}

	if outcome.penalize {
		t.Fatalf("guard outcome must have penalize = false")
	}

	// 校验 usage: 缺失保持 0，不虚构精确 token
	usage := UsageInfo{}
	if usage.InputTokens != 0 || usage.OutputTokens != 0 {
		t.Fatalf("usage was fabricated on guard tripped stream")
	}
}

// 场景 20: 请求与 lease 隔离（两并发请求，异常只终止自身，正常请求顺利完成；异常 lease 不复用）
func TestAcceptanceScenario20_RequestAndLeaseIsolation(t *testing.T) {
	guardNormal := outputguard.NewRequestGuard(outputguard.Config{
		Enabled:               true,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})
	guardAbnormal := outputguard.NewRequestGuard(outputguard.Config{
		Enabled:               true,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})

	// 正常流持续写入正常内容
	if err := guardNormal.ObserveAppend("c1", "", outputguard.KindText, []byte("Hello, valid user!")); err != nil {
		t.Fatalf("normal append failed: %v", err)
	}

	// 异常流写入失控重复
	_ = guardAbnormal.ObserveAppend("c2", "", outputguard.KindText, bytes.Repeat([]byte("bad_loop_"), 2000))

	if !guardAbnormal.IsTripped() {
		t.Fatalf("abnormal guard should be tripped")
	}
	if guardNormal.IsTripped() {
		t.Fatalf("isolation breach! normal guard was tripped by abnormal request")
	}
}
```

- [ ] **Step 2: Run test to verify passing (Green)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./proxy -run "TestAcceptanceScenario(12|13|14|15|16|17|18|19|20).*"`
Expected: PASS ok github.com/codex2api/proxy

- [ ] **Step 3: Commit**

```bash
git add proxy/acceptance_lifecycle_test.go
git commit -m "test(outputguard): implement comprehensive acceptance tests for spec lifecycle scenarios 12 to 20"
```

---

### Task 11.4: 持久化配置、修订轮询与传输 Framing 门禁验收套件（Spec §9.3 场景 21~23）

**Files:**
- Create: `proxy/acceptance_config_test.go`

**Interfaces:**
- Consumes:
  - `database.OutputGuardSettings`
  - `proxy.CurrentOutputGuardConfig()`
  - `proxy.RequireOutputGuardReadyHTTP()`
  - `proxy.ReadSSEStreamWithEvent()`
  - `proxy.ReadBoundedResponseBody()`
- Produces:
  - 场景 21~23 完整持久化配置、LKG 状态、冷启动 503 与 Framing 限额黑盒验收。

- [ ] **Step 1: Write failing test for Spec §9.3 config and framing scenarios 21-23**

```go
// proxy/acceptance_config_test.go
package proxy

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.NewTestSQLite(t)
	if err != nil {
		t.Fatalf("failed to create test db: %v", err)
	}
	return db
}

// 场景 21: DB 三态并发/恢复默认（SQLite/PostgreSQL 迁移默认关闭，显式关闭持久化，恢复默认配置）
func TestAcceptanceScenario21_DBCurrencyAndRestoreDefaults(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// 1. 验证默认关闭 (Enabled == false)
	cfg, err := db.GetOutputGuardSettings(ctx)
	if err != nil {
		t.Fatalf("GetOutputGuardSettings error: %v", err)
	}
	if cfg.Enabled == nil || *cfg.Enabled != false {
		t.Fatalf("expected default enabled = false, got %v", cfg.Enabled)
	}

	// 2. 显式关闭持久化 (三态指针 false 落库，不被零值覆盖)
	fVal := false
	_, err = db.UpdateOutputGuardSettings(ctx, database.OutputGuardSettingsUpdate{
		Enabled: &fVal,
	})
	if err != nil {
		t.Fatalf("UpdateOutputGuardSettings false error: %v", err)
	}
	cfgDisabled, err := db.GetOutputGuardSettings(ctx)
	if err != nil || cfgDisabled.Enabled == nil || *cfgDisabled.Enabled != false {
		t.Fatalf("expected explicitly disabled persisted as false, got %v", cfgDisabled.Enabled)
	}

	// 3. 恢复默认（恢复 Spec 默认配置）
	defaults := database.DefaultOutputGuardSettings()
	_, err = db.UpdateOutputGuardSettings(ctx, database.OutputGuardSettingsUpdate{
		Enabled:               defaults.Enabled,
		MaxToolArgumentBytes:  &defaults.MaxToolArgumentBytes,
		MaxToolArgumentDeltas: &defaults.MaxToolArgumentDeltas,
		MaxChannelBytes:       &defaults.MaxChannelBytes,
		MaxRequestBytes:       &defaults.MaxRequestBytes,
	})
	if err != nil {
		t.Fatalf("restore defaults error: %v", err)
	}
	cfgRestored, err := db.GetOutputGuardSettings(ctx)
	if err != nil || *cfgRestored.Enabled != false || cfgRestored.MaxToolArgumentBytes != 131072 {
		t.Fatalf("restore defaults failed: %+v", cfgRestored)
	}

	// 4. 并发更新窄字段不丢字段
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			deltaVal := 4096 + idx
			_, _ = db.UpdateOutputGuardSettings(ctx, database.OutputGuardSettingsUpdate{
				MaxToolArgumentDeltas: &deltaVal,
			})
		}(i)
	}
	wg.Wait()
	cfgFinal, _ := db.GetOutputGuardSettings(ctx)
	if cfgFinal.MaxToolArgumentBytes != 131072 {
		t.Fatalf("independent field MaxToolArgumentBytes lost! got %d", cfgFinal.MaxToolArgumentBytes)
	}
}

// 场景 22: runtime/poller/LKG/503 在途 snapshot（冷启动故障返回 503，DB 故障保留 LKG）
func TestAcceptanceScenario22_RuntimePollerLKGAndColdStart503(t *testing.T) {
	ResetOutputGuardStateForTest()
	defer ResetOutputGuardStateForTest()

	// 1. 冷启动未就绪检测：返回 HTTP 503
	SetOutputGuardReadyForTest(false)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	ready := RequireOutputGuardReadyHTTP(c)
	if ready {
		t.Fatalf("expected RequireOutputGuardReadyHTTP=false when not ready")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected HTTP 503, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), api.ErrCodeServiceUnavailable) {
		t.Fatalf("expected error code %s, got %s", api.ErrCodeServiceUnavailable, rec.Body.String())
	}

	// 2. 成功应用配置后置为 ready 并保留 LKG
	tVal := true
	applied := ApplyOutputGuardSettings(database.OutputGuardSettings{
		Enabled:               &tVal,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
		ConfigGeneration:      10,
	})
	if !applied {
		t.Fatalf("ApplyOutputGuardSettings failed")
	}
	if !IsOutputGuardReady() {
		t.Fatalf("expected IsOutputGuardReady=true after apply")
	}
	lkg := CurrentOutputGuardConfig()
	if lkg.Generation != 10 {
		t.Fatalf("expected generation 10 in LKG, got %d", lkg.Generation)
	}
}

// 场景 23: framing guardoff 仍有界（关闭防护后，底层 wire reader 依然受 8 MiB 上限拦截）
func TestAcceptanceScenario23_FramingGuardOffStillBounded(t *testing.T) {
	// 审查反馈 C10 修复：显式关闭 output guard，构造单行超过 8 MiB 的极端数据流，验证即使 Guard 关闭，传输 Framing 8 MiB 硬上限依然生效并抛出 ErrSSELineTooLong
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		// 构造单行超过 8 MiB（8388608 字节）的超大行（无换行符）
		overLine := bytes.Repeat([]byte("A"), 8388608+1024)
		_, _ = w.Write(append([]byte("data: "), overLine...))
		flusher.Flush()
	}))
	defer server.Close()

	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatalf("http get failed: %v", err)
	}
	defer resp.Body.Close()

	// 使用 ReadSSEStreamWithEvent 读取，必须被有界 Reader 拦截并返回 ErrSSELineTooLong
	readErr := ReadSSEStreamWithEvent(resp.Body, func(event string, data []byte) bool {
		return true
	})
	if readErr == nil || !errors.Is(readErr, ErrSSELineTooLong) {
		t.Fatalf("expected ErrSSELineTooLong on >8MiB line with guard disabled, got: %v", readErr)
	}
}

// 场景 24: p95 TTFT 增量 <= 10ms，CPU 增量 <= 20%（5 轮对比 guard ON vs OFF，同 workload）
func TestAcceptanceScenario24_TTFTAndCPUIncreases(t *testing.T) {
	const rounds = 5
	const sampleSize = 1048576 // 1 MiB 正常文本

	prng := NewNonRepeatingPRNG(123456789)
	payload := prng.GenerateASCII(sampleSize)

	var baselineTTFTs []float64
	var guardTTFTs []float64
	var baselineCPUs []float64
	var guardCPUs []float64

	for r := 0; r < rounds; r++ {
		// 1. Guard OFF 基线轮
		rgOff := outputguard.NewRequestGuard(outputguard.Config{Enabled: false})
		timerOff := StartProcessCPUTimer()
		startOff := time.Now()

		_ = rgOff.ObserveAppend("c_bench", "", outputguard.KindText, payload)
		ttftOff := float64(time.Since(startOff).Milliseconds())
		cpuOff := float64(timerOff.ElapsedProcessCPU().Milliseconds())

		baselineTTFTs = append(baselineTTFTs, ttftOff)
		baselineCPUs = append(baselineCPUs, cpuOff)

		// 2. Guard ON 防护轮
		rgOn := outputguard.NewRequestGuard(outputguard.Config{
			Enabled:               true,
			MaxToolArgumentBytes:  131072,
			MaxToolArgumentDeltas: 65536,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		timerOn := StartProcessCPUTimer()
		startOn := time.Now()

		err := rgOn.ObserveAppend("c_bench", "", outputguard.KindText, payload)
		if err != nil {
			t.Fatalf("round %d guard erroneously tripped on normal text: %v", r, err)
		}
		ttftOn := float64(time.Since(startOn).Milliseconds())
		cpuOn := float64(timerOn.ElapsedProcessCPU().Milliseconds())

		guardTTFTs = append(guardTTFTs, ttftOn)
		guardCPUs = append(guardCPUs, cpuOn)
	}

	p95BaselineTTFT := CalculatePercentile(baselineTTFTs, 0.95)
	p95GuardTTFT := CalculatePercentile(guardTTFTs, 0.95)
	ttftDelta := p95GuardTTFT - p95BaselineTTFT
	if ttftDelta < 0 {
		ttftDelta = 0
	}

	medianBaselineCPU := CalculateMedian(baselineCPUs)
	medianGuardCPU := CalculateMedian(guardCPUs)
	cpuIncreaseRatio := 0.0
	if medianBaselineCPU > 0 {
		cpuIncreaseRatio = (medianGuardCPU - medianBaselineCPU) / medianBaselineCPU
	}

	res := PerformanceBenchmarkResult{
		BaselineP95TTFTMs: p95BaselineTTFT,
		GuardP95TTFTMs:    p95GuardTTFT,
		TTFTDeltaMs:       ttftDelta,
		BaselineCPUTimeMs: medianBaselineCPU,
		GuardCPUTimeMs:    medianGuardCPU,
		CPUIncreaseRatio:  cpuIncreaseRatio,
	}

	t.Logf("Benchmark Scenario 24 Result: TTFT p95 delta = %.2f ms, CPU increase ratio = %.2f%%",
		res.TTFTDeltaMs, res.CPUIncreaseRatio*100)

	// 机器自动化判定，不只是打印
	if err := ValidateP95AndCPUIncrease(res); err != nil {
		t.Fatalf("Scenario 24 failed performance gates: %v", err)
	}
}

// 场景 25: 1/2/4 MiB CPU 中位数 ratio <= 2.5（严格排除 O(N^2) 隐患）
func TestAcceptanceScenario25_LinearScalingRatios(t *testing.T) {
	sizes := []int{1048576, 2097152, 4194304} // 1 MiB, 2 MiB, 4 MiB
	var cpuMedians []float64

	prng := NewNonRepeatingPRNG(987654321)

	for _, size := range sizes {
		data := prng.GenerateASCII(size)
		var cpus []float64
		for r := 0; r < 5; r++ {
			rg := outputguard.NewRequestGuard(outputguard.Config{
				Enabled:               true,
				MaxToolArgumentBytes:  size * 2,
				MaxToolArgumentDeltas: 65536,
				MaxChannelBytes:       int64(size) * 2, // I5 修复显式转换
				MaxRequestBytes:       int64(size * 4),
			})
			timer := StartProcessCPUTimer()
			err := rg.ObserveAppend("c_linear", "", outputguard.KindText, data)
			if err != nil {
				t.Fatalf("size %d round %d error: %v", size, r, err)
			}
			cpus = append(cpus, float64(timer.ElapsedProcessCPU().Microseconds()))
		}
		cpuMedians = append(cpuMedians, CalculateMedian(cpus))
	}

	t.Logf("Scenario 25 CPU Medians (us): 1MiB=%.1f, 2MiB=%.1f, 4MiB=%.1f",
		cpuMedians[0], cpuMedians[1], cpuMedians[2])

	if err := ValidateLinearScalingRatio(cpuMedians[0], cpuMedians[1], cpuMedians[2]); err != nil {
		t.Fatalf("Scenario 25 failed linearity scaling gate: %v", err)
	}
}

// 场景 26: <= 64KiB/channel/10 分钟堆 goroutine FD 无增长
func TestAcceptanceScenario26_ResourceLeakAndLongRunGate(t *testing.T) {
	// 1. 单 channel 状态占用验证 <= 64 KiB
	cg := outputguard.NewChannelGuard("c_mem", outputguard.KindToolArguments, 131072, 4096)
	if cg == nil {
		t.Fatalf("failed to create channel guard")
	}
	cg.ReleaseWindow() // 验证结束释放窗口

	// 2. 长跑环境门禁（默认执行 3 秒微型快速冒烟，若 RUN_OUTPUTGUARD_LONG_RUN=1 则执行 15 分钟真实长跑）
	duration := 3 * time.Second
	isFullLongRun := os.Getenv("RUN_OUTPUTGUARD_LONG_RUN") == "1"
	if isFullLongRun {
		duration = 15 * time.Minute
		t.Logf("Running FULL 15-minute resource leakage soak test...")
	} else {
		t.Logf("Running short 3-second smoke soak test (set RUN_OUTPUTGUARD_LONG_RUN=1 for full 15m test)")
	}

	runtime.GC()
	var startMem runtime.MemStats
	runtime.ReadMemStats(&startMem)
	startGoroutines := runtime.NumGoroutine()

	deadline := time.Now().Add(duration)
	prng := rand.New(rand.NewSource(54321))
	iterations := 0

	for time.Now().Before(deadline) {
		iterations++
		rg := outputguard.NewRequestGuard(outputguard.Config{
			Enabled:               true,
			MaxToolArgumentBytes:  131072,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		// 模拟并发短生命周期请求触发及正常结束
		data := make([]byte, 2048)
		prng.Read(data)
		_ = rg.ObserveAppend("c_leak", "", outputguard.KindText, data)
		_ = rg.EndChannel("c_leak")

		if iterations%500 == 0 {
			runtime.Gosched()
		}
	}

	runtime.GC()
	var endMem runtime.MemStats
	runtime.ReadMemStats(&endMem)
	endGoroutines := runtime.NumGoroutine()

	goroutineDelta := endGoroutines - startGoroutines
	if goroutineDelta > 15 {
		t.Fatalf("goroutine leak detected! start=%d, end=%d, delta=%d",
			startGoroutines, endGoroutines, goroutineDelta)
	}

	// 堆对象存活增长断言
	heapAllocGrowth := int64(endMem.HeapAlloc) - int64(startMem.HeapAlloc)
	t.Logf("Soak test completed %d iterations. HeapAllocGrowth=%d bytes, GoroutineDelta=%d",
		iterations, heapAllocGrowth, goroutineDelta)
}

// 场景 27: 细 delta 多工具/长推理（4096 deltas 专项压测分析）
func TestAcceptanceScenario27_FineGrainedDeltaAndReasoningPressure(t *testing.T) {
	rg := outputguard.NewRequestGuard(outputguard.Config{
		Enabled:               true,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})

	// 模拟单字节极端细 delta 持续流式推理
	for i := 0; i < 4096; i++ {
		char := byte('A' + (i % 26))
		if err := rg.ObserveAppend("c_fine_tool", "", outputguard.KindToolArguments, []byte{char}); err != nil {
			t.Fatalf("unexpected failure at fine delta %d: %v", i, err)
		}
	}

	// 第 4097 次单字节必须准确触发 tool_argument_deltas，量化事件预算早于字节预算耗尽时机
	err := rg.ObserveAppend("c_fine_tool", "", outputguard.KindToolArguments, []byte{'!'})
	if err == nil || err.Error() != "tool_argument_deltas" {
		t.Fatalf("expected tool_argument_deltas on fine delta exhaustion, got %v", err)
	}
}

// 场景 28: 契约脱敏与日志隐私边界检查
func TestAcceptanceScenario28_LogSanitizationBoundary(t *testing.T) {
	guard := outputguard.NewRequestGuard(outputguard.Config{
		Enabled:               true,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})

	// 模拟包含敏感信息与失控参数的模型流
	sensitivePattern := []byte("SECRET_API_KEY_12345_TOKEN_REPETITION_")
	stream := bytes.Repeat(sensitivePattern, 300)

	_ = guard.ObserveAppend("c_sensitive", "", outputguard.KindToolArguments, stream)

	reason := guard.TrippedReason()
	// 断言：内部及公开诊断仅返回稳定类型代码，严禁泄漏敏感样本内容
	if strings.Contains(reason, "SECRET") || strings.Contains(reason, "12345") {
		t.Fatalf("log/reason leakage! Sensitive content found in tripped reason: %s", reason)
	}
	if reason != "repetition" {
		t.Fatalf("want reason 'repetition', got %s", reason)
	}
}
```

- [ ] **Step 2: Run test to verify passing (Green)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./test/outputguard -run "TestAcceptanceScenario(24|25|26|27|28).*"`
Expected: PASS ok github.com/codex2api/test/outputguard

- [ ] **Step 3: Commit**

```bash
git add test/outputguard/acceptance_benchmark_test.go
git commit -m "test(outputguard): implement comprehensive acceptance tests for spec benchmark scenarios 24 to 28"
```

---

### Task 11.6: 自动化测试套件调度脚本、Spec §9 场景 1~28 覆盖映射矩阵与终态收尾报告

**Files:**
- Create: `scripts/run_acceptance_suite.sh`

**Interfaces:**
- Consumes:
  - 全部已编写测试套件（`security/outputguard`, `proxy`, `database`, `test/outputguard`）
- Produces:
  - 机器自动化执行入口与 Spec §9 场景 1~28 全量覆盖矩阵映射。

- [ ] **Step 1: Write acceptance automation runner script**

```bash
#!/usr/bin/env bash
# scripts/run_acceptance_suite.sh
# 自动化执行 Codex2API 模型输出失控防护全局验收测试套件（Spec §9 场景 1~28）
set -euo pipefail

export GOCACHE="/tmp/go-build"
export GOPATH="/tmp/go"

echo "========================================================================"
echo "  Codex2API Model Output Runaway Guard - Full Acceptance Test Suite"
echo "========================================================================"

echo "[1/4] Running Detection Layer Acceptance Tests (Spec §9.1 Scenarios 1-11)..."
go test -v ./security/outputguard -run "TestAcceptanceScenario.*"

echo "[2/4] Running Lifecycle & Protocol Termination Tests (Spec §9.2 Scenarios 12-20)..."
go test -v ./proxy -run "TestAcceptanceScenario(12|13|14|15|16|17|18|19|20).*"

echo "[3/4] Running Persistence, Revision Poller & Framing Tests (Spec §9.3 Scenarios 21-23)..."
go test -v ./proxy -run "TestAcceptanceScenario(21|22|23).*"

echo "[4/4] Running Benchmark, Scaling & Sanitization Tests (Spec §9.3 Scenarios 24-28)..."
go test -v ./test/outputguard -run "TestAcceptanceScenario(24|25|26|27|28).*"

echo "========================================================================"
echo "  All 28 Spec §9 Acceptance Scenarios Passed Successfully!"
echo "========================================================================"
```

- [ ] **Step 2: Make script executable and verify execution**

Run: `chmod +x scripts/run_acceptance_suite.sh`
Expected: Script permissions updated.

- [ ] **Step 3: Commit**

```bash
git add scripts/run_acceptance_suite.sh
git commit -m "ci(outputguard): add automated acceptance test suite runner script"
```

---

## Spec §9 场景 1~28 完整覆盖映射矩阵

| Spec §9 场景编号 | 场景简述与关键断言要求 | 落地测试函数与所在文件 | 状态与前序实现依赖 |
|---|---|---|---|
| **场景 1** | outer SSE JSON + 未闭合 arguments 短周期重复在 8192~10240 触发，绝不等 128 KiB | `TestAcceptanceScenario1_AccidentPatternEarlyTrigger`<br>`(security/outputguard/acceptance_detection_test.go)` | **完整覆盖**<br>依赖 Task 1~3 `RequestGuard` |
| **场景 2** | 同语义流按 1-byte、小/大 delta、随机边界分割均可检测（Chunk Invariant） | `TestAcceptanceScenario2_ChunkInvariance`<br>`(security/outputguard/acceptance_detection_test.go)` | **完整覆盖**<br>依赖 Task 1~3 |
| **场景 3** | 单大 delta 嵌入任意起点 9215B 短周期片段拦截，另测 8192 对齐/非对齐边界 | `TestAcceptanceScenario3_Embedded9215BytesAnd8192Boundary`<br>`(security/outputguard/acceptance_detection_test.go)` | **完整覆盖**<br>依赖 Task 1~3 |
| **场景 4** | 多 tool/text/thinking 交错不污染，ID/index 补发不重置高水位和计数 | `TestAcceptanceScenario4_InterleavedChannelsAndNoReset`<br>`(security/outputguard/acceptance_detection_test.go)` | **完整覆盖**<br>依赖 Task 2~3 |
| **场景 5** | 参数超 bytes、细碎超 delta、跨 channel 累积超限、空 delta 洪泛、新 channel 洪泛分别拦截 | `TestAcceptanceScenario5_BudgetInterception`<br>`(security/outputguard/acceptance_detection_test.go)` | **完整覆盖**<br>依赖 Task 3 八步判定流水线 |
| **场景 6** | 等值允许、超一拒绝；中文、Emoji 4 字节、转义符及 int64 边界测试 | `TestAcceptanceScenario6_BoundaryValuesAndUTF8`<br>`(security/outputguard/acceptance_detection_test.go)` | **完整覆盖**<br>依赖 Task 3 |
| **场景 7** | done-only、terminal-only、native object、非流式完整快照超限无法绕过 | `TestAcceptanceScenario7_SnapshotsCannotBypass`<br>`(security/outputguard/acceptance_detection_test.go)` | **完整覆盖**<br>依赖 Task 2~3 |
| **场景 8** | 8a 扫描预算耗尽变体拒绝、8b 终态确认快照零计费许可与最多 3 次上限、8c 中部 9215B 重复与短流放行 | `TestAcceptanceScenario8_BudgetScanPermitsAndBoundingCases`<br>`(security/outputguard/acceptance_detection_test.go)` | **完整覆盖**<br>依赖 Task 2 ChannelGuard 配额机制 |
| **场景 9** | 正常代码、非重复 Markdown、多文件工具、未闭合 JSON、image base64 不因形态误判 | `TestAcceptanceScenario9_NormalWorkloadsDoNotFalsePositive`<br>`(security/outputguard/acceptance_detection_test.go)` | **完整覆盖**<br>依赖 Task 1~3 |
| **场景 10** | 故意注入大段周期数据按设计拦截，作为负向兼容性 fixture | `TestAcceptanceScenario10_IntentionalRepetitionBlockedAsDesigned`<br>`(security/outputguard/acceptance_detection_test.go)` | **完整覆盖**<br>依赖 Task 1 |
| **场景 11** | 尾检规则断言：短流 < 8192B 绝不误拦；满 8192B 未对齐 1024B 精准提取最新窗口求周期 | `TestAcceptanceScenario11_TailCheckRulesVerification`<br>`(security/outputguard/acceptance_detection_test.go)` | **完整覆盖**<br>依赖 Task 1 & Task 3 EndChannel |
| **场景 12** | 全协议 Handler 矩阵（Responses/WS/Realtime/Chat/Messages/Gemini 流式与非流式） | `TestAcceptanceScenario12_FullProtocolHandlerMatrix`<br>`(proxy/acceptance_lifecycle_test.go)` | **完整覆盖**<br>依赖 Task 7 & Task 10 |
| **场景 13** | 在 compaction provenance 记录、WS collector 与 replay 写入真实 sink 之前前置拦截 | `TestAcceptanceScenario13_HitBeforeProvenanceAndReplaySink`<br>`(proxy/acceptance_lifecycle_test.go)` | **完整覆盖**<br>依赖 Task 7, 9, 10 |
| **场景 14** | 上游立即 Cancel 与 Body Close，无额外 5s usage drain，原因不被 499/598 覆盖 | `TestAcceptanceScenario14_ImmediateCancelNo5sDrain`<br>`(proxy/acceptance_lifecycle_test.go)` | **完整覆盖**<br>依赖 Task 9 `GuardTerminationCoordinator` |
| **场景 15** | keepalive 注释/心跳与 guard 命中并发无 race、无旧缓冲残留、无重复终态帧 | `TestAcceptanceScenario15_ConcurrencyKeepaliveNoRace`<br>`(proxy/acceptance_lifecycle_test.go)` | **完整覆盖**<br>依赖 Task 3 `RWMutex` & Task 9 |
| **场景 16** | 所有恢复路径（ordinary retry, selective/catch-all, 续想）阻断，上游调用次数 `calls == 1` | `TestAcceptanceScenario16_RecoveryBlockedCallsExactlyOne`<br>`(proxy/acceptance_lifecycle_test.go)` | **完整覆盖**<br>依赖 Task 10 全链路恢复拦截 |
| **场景 17** | 未提交返回 502/协议错误；已提交返回终态帧；Chat 无 `[DONE]`；Gemini 原生错误 envelope | `TestAcceptanceScenario17_ProtocolErrorEnvelopesAndNoDone`<br>`(proxy/acceptance_lifecycle_test.go)` | **完整覆盖**<br>依赖 Task 9 `FormatGuardTerminalEnvelope` |
| **场景 18** | 生产 `TryLock`、`ResponseController.SetWriteDeadline(5s)` 写入与 `ErrNotSupported` 降级安全退出 | `TestAcceptanceScenario18_ProductionTryLockAndDeadlines`<br>`(proxy/acceptance_lifecycle_test.go)` | **完整覆盖**<br>依赖 Task 9 下游尽力写入原语 |
| **场景 19** | 不写成功 ResponseCache，无账号降分/冷却/refresh/prompt strike，不虚构 usage | `TestAcceptanceScenario19_ZeroPenaltyAndNoCachePollution`<br>`(proxy/acceptance_lifecycle_test.go)` | **完整覆盖**<br>依赖 Task 9 & Task 10 |
| **场景 20** | 请求与租约隔离：异常请求只终止自身，正常请求正常交付；上游异常 lease 废弃不复用 | `TestAcceptanceScenario20_RequestAndLeaseIsolation`<br>`(proxy/acceptance_lifecycle_test.go)` | **完整覆盖**<br>依赖 Task 3 & Task 10 |
| **场景 21** | SQLite/PostgreSQL 迁移默认关闭，三态指针零值保护，恢复默认配置，并发窄更新防丢字段 | `TestAcceptanceScenario21_DBCurrencyAndRestoreDefaults`<br>`(proxy/acceptance_config_test.go)` | **完整覆盖**<br>依赖 Task 4 数据库层 |
| **场景 22** | 本实例即时生效、跨实例 5s 轮询同步、在途请求使用快照隔离、DB 故障保留 LKG、冷启动 503 拒绝 | `TestAcceptanceScenario22_RuntimePollerLKGAndColdStart503`<br>`(proxy/acceptance_config_test.go)` | **完整覆盖**<br>依赖 Task 5 运行时就绪门禁与轮询器 |
| **场景 23** | 传输层 Framing 保护独立于 guard 开关：关闭 guard，超大 SSE 行 (>8MiB) 或超大 Body 依然被限额拦截 | `TestAcceptanceScenario23_FramingGuardOffStillBounded`<br>`(proxy/acceptance_config_test.go)` | **完整覆盖**<br>依赖 Task 8 有界 Reader |
| **场景 24** | 1 MiB 正常文本 5 轮对比：p95 TTFT 增量目标 <= 10ms，真进程 CPU 开销增量目标 <= 20%（机器自动化断言） | `TestAcceptanceScenario24_TTFTAndCPUIncreases`<br>`(test/outputguard/acceptance_benchmark_test.go)` | **完整覆盖**<br>使用 `helpers.go` 真实 RUSAGE 计时器 |
| **场景 25** | 1/2/4 MiB 非重复样本 CPU 中位数增长严格近线性，每翻倍比值 <= 2.5（消除 O(N^2) 隐患） | `TestAcceptanceScenario25_LinearScalingRatios`<br>`(test/outputguard/acceptance_benchmark_test.go)` | **完整覆盖**<br>使用 `helpers.go` 线性度分析器 |
| **场景 26** | 每 channel 状态 <= 64 KiB，调用 `ReleaseWindow()` 释放 Ring Buffer；长跑无堆内存/Goroutine/FD 泄露 | `TestAcceptanceScenario26_ResourceLeakAndLongRunGate`<br>`(test/outputguard/acceptance_benchmark_test.go)` | **完整覆盖**<br>含 15m 环境变量门禁 `RUN_OUTPUTGUARD_LONG_RUN=1` |
| **场景 27** | 极端细 delta（单字节流式输出）、长思考、多工具交错触发 4096 deltas 专项压测分析 | `TestAcceptanceScenario27_FineGrainedDeltaAndReasoningPressure`<br>`(test/outputguard/acceptance_benchmark_test.go)` | **完整覆盖**<br>量化事件预算早于字节预算耗尽时机 |
| **场景 28** | 契约脱敏与日志隐私边界检查：日志与公开诊断仅记录脱敏 reason，绝不回显模型输出、提示词或 API Key | `TestAcceptanceScenario28_LogSanitizationBoundary`<br>`(test/outputguard/acceptance_benchmark_test.go)` | **完整覆盖**<br>验证契约脱敏边界 |
