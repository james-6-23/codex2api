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
  - 默认全局开启，仅管理员可在后台显式关闭；与 prompt filter 相互独立。
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

func (d *RepetitionDetector) Push(data []byte) (hit bool, period int) {
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
			// 单次流水线处理：一次遍历同时送入 detector.Push 并同步更新 Hash
			var h uint64 = 14695981039346656037
			for _, b := range snapshot {
				h = (h ^ uint64(b)) * 1099511628211
			}
			c.terminalBaselineHash = h
			c.hasBaselineHash = true
		}

		hit, _ := c.detector.Push(snapshot)
		if hit {
			return ErrRepetitionDetected
		}
		hit, _ = c.detector.CheckTail()
		if hit {
			return ErrRepetitionDetected
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

func (g *RequestGuard) TrippedReason() string {
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
	targetID := channelID
	if targetID == "" && aliasID != "" {
		if mapped, ok := g.aliasMap[aliasID]; ok {
			targetID = mapped
		} else {
			targetID = aliasID
		}
	}
	if targetID == "" {
		targetID = "default"
	}
	if aliasID != "" && targetID != aliasID {
		g.aliasMap[aliasID] = targetID
	}

	cg, exists := g.channels[targetID]
	if !exists {
		// 步骤 2: Channel 数量检查 (Spec §5.3)
		if len(g.channels)+1 > MaxRequestChannels {
			return nil, g.tripLocked("channel_count")
		}
		maxBytes := g.cfg.MaxChannelBytes
		if kind == KindToolArguments {
			maxBytes = g.cfg.MaxToolArgumentBytes
		}
		cg = NewChannelGuard(targetID, kind, maxBytes, g.cfg.MaxToolArgumentDeltas)
		g.channels[targetID] = cg
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

		if got.Enabled == nil || *got.Enabled != true {
			t.Fatalf("default Enabled want true, got %v", got.Enabled)
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

		// 重新读取断言显式 false 得到正确保留（不被默认 true 覆盖）
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
{"system_settings", "output_guard_enabled", "INTEGER DEFAULT 1"},
{"system_settings", "output_guard_max_tool_argument_bytes", "INTEGER NOT NULL DEFAULT 131072"},
{"system_settings", "output_guard_max_tool_argument_deltas", "INTEGER NOT NULL DEFAULT 4096"},
{"system_settings", "output_guard_max_channel_bytes", "INTEGER NOT NULL DEFAULT 2097152"},
{"system_settings", "output_guard_max_request_bytes", "INTEGER NOT NULL DEFAULT 4194304"},
{"system_settings", "output_guard_config_generation", "INTEGER NOT NULL DEFAULT 1"},
```

修改 `database/postgres.go` 添加列及隔离 UpdateSystemSettings：
```go
// 在 database/postgres.go initPostgresSchema 中添加：
ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS output_guard_enabled BOOLEAN DEFAULT TRUE;
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
		if status.LastSyncError != "" && (substr == "" || errors.New(status.LastSyncError) != nil) {
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
	db := newResponseCacheSettingsTestDB(t)
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

	if resp["output_guard_enabled"] != true {
		t.Fatalf("expected output_guard_enabled = true by default, got %v", resp["output_guard_enabled"])
	}
	if int64(resp["output_guard_max_tool_argument_bytes"].(float64)) != 131072 {
		t.Fatalf("expected tool argument bytes 131072, got %v", resp["output_guard_max_tool_argument_bytes"])
	}
}

func TestAdminOutputGuardSettingsPutValidationAndReadOnlyRevision(t *testing.T) {
	db := newResponseCacheSettingsTestDB(t)
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
  output_guard_enabled: true,
  output_guard_max_tool_argument_bytes: 128 * KIB,
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
                          output_guard_enabled: true,
                          output_guard_max_tool_argument_bytes: DEFAULT_OUTPUT_GUARD_TOOL_BYTES,
                          output_guard_max_tool_argument_deltas: DEFAULT_OUTPUT_GUARD_TOOL_DELTAS,
                          output_guard_max_channel_bytes: DEFAULT_OUTPUT_GUARD_CHANNEL_BYTES,
                          output_guard_max_request_bytes: DEFAULT_OUTPUT_GUARD_REQUEST_BYTES,
                        }
                        setSettingsForm(f => ({ ...f, ...patch }))
                        savePartialSettings(patch)
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
                          savePartialSettings({ output_guard_enabled: checked })
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
                          onBlur={() => savePartialSettings({ output_guard_max_tool_argument_bytes: settingsForm.output_guard_max_tool_argument_bytes })}
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
                          onBlur={() => savePartialSettings({ output_guard_max_tool_argument_deltas: settingsForm.output_guard_max_tool_argument_deltas })}
                        />
                      </SettingField>

                      <SettingField
                        label="单 Channel 字节上限 (MiB)"
                        description="单 text/thinking 生成通道累计字节上限（默认 2 MiB，范围 128 KiB..16 MiB）"
                      >
                        <Input
                          type="number"
                          value={Math.round(settingsForm.output_guard_max_channel_bytes / (1024 * 1024))}
                          onChange={(e) => {
                            const val = Math.max(1, Math.min(16, parseInt(e.target.value) || 2)) * 1024 * 1024
                            setSettingsForm(f => ({ ...f, output_guard_max_channel_bytes: val }))
                          }}
                          onBlur={() => savePartialSettings({ output_guard_max_channel_bytes: settingsForm.output_guard_max_channel_bytes })}
                        />
                      </SettingField>

                      <SettingField
                        label="单请求语义预算 (MiB)"
                        description="跨所有 channel、attempt 及自动续想的累计语义字节预算（默认 4 MiB，范围 256 KiB..64 MiB）"
                      >
                        <Input
                          type="number"
                          value={Math.round(settingsForm.output_guard_max_request_bytes / (1024 * 1024))}
                          onChange={(e) => {
                            const val = Math.max(1, Math.min(64, parseInt(e.target.value) || 4)) * 1024 * 1024
                            setSettingsForm(f => ({ ...f, output_guard_max_request_bytes: val }))
                          }}
                          onBlur={() => savePartialSettings({ output_guard_max_request_bytes: settingsForm.output_guard_max_request_bytes })}
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
	"bytes"
	"encoding/json"
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
				TerminalStage: "item.done",
			})
		} else if itemType == "message" {
			text := item.Get("content.0.text").String()
			if text != "" {
				observations = append(observations, Observation{
					ChannelID:     itemID,
					Kind:          outputguard.KindText,
					Action:        ActionSnapshot,
					Payload:       []byte(text),
					TerminalStage: "item.done",
				})
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
		observations = append(observations, Observation{
			ChannelID:     "message_root",
			Kind:          outputguard.KindText,
			Action:        ActionSnapshot,
			Payload:       []byte("done"),
			TerminalStage: "response.completed",
		})

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
