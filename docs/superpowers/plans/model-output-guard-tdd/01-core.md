# 01-Core: 模型输出失控防护核心算法与请求状态机 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 基于已审查通过的设计规格，实现 Codex2API 全局模型输出失控防护核心算法库与请求级状态机（`security/outputguard` 模块，包含 Task 1、Task 2、Task 3），提供常量空间 8192B 滑动窗口前缀函数短周期线性重复检测、Channel 扫描预算与纯终态确认许可机制、以及请求级 `RequestGuard` 的 8 步无歧义判定流水线与读写锁并发保护。

**Architecture:** 
1. `security/outputguard/repetition.go`: 常量空间 8192-byte ring buffer，KMP 前缀函数求最短周期（$1 \le p \le 256$，支持非整除周期数学判定），1024B 步长检查点与即时尾检；单流水线 `PushWithHash` 实现一次遍历同时完成滚动更新、周期检测与 FNV-1a Hash 计算。
2. `security/outputguard/channel.go`: 单 Channel 状态机，维护已计费高水位、累计扫描预算扣减（$2 \times \text{limit}$）、最多 3 次合法纯终态快照白名单许可（`arguments.done`、`item.done`、`response.completed`），单遍历 Hash 流水线比对与变体篡改废弃，物理处理上界绝对锁定于 $5 \times \text{limit}$。
3. `security/outputguard/guard.go`: 请求级 `RequestGuard` 状态机，统一管理 65536 业务事件预算、128 累计 Channel 预算、4096 工具 Delta 预算，严格执行 Spec §5.3 的 8 步无歧义判定流水线；晚到 ID 双向别名合并；首 reason 冻结与幂等；内置 `sync.RWMutex` 保证完全并发安全与零数据竞争。

**Tech Stack:** Go 1.26+ (`sync`, `sync/atomic`, `errors`, `bytes`, `testing`).

**Spec:** `docs/superpowers/specs/2026-10-28-model-output-guard-design.md`

## Global Constraints

- **范围限制**：本项目仅实施核心检测与状态机包 `security/outputguard`（涵盖 Task 1、Task 2、Task 3）；不修改 spec、旧 plan、candidate 或上游生产业务代码。
- **不可妥协的设计取舍**：
  - 首期采用精确短周期重复检测 + 硬预算，不引入语义模型，不自动改写或拆分工具参数。
  - 默认工具参数上限 128 KiB、4096 非空 deltas；单 channel 字节上限 2 MiB；单请求字节上限 4 MiB。所有配置在测试中由 `Config` 明确赋值，不得产生未显式赋值的语义混乱。
  - 有限字符串周期数学判定：KMP border $k = \pi[L-1]$ 对应最小周期候选值 $p = L - k$。当且仅当 $1 \le p \le 256$ 且 $\forall i \in [p, L): s[i] == s[i-p]$ 时即为严格周期，绝不加入 $L \pmod p == 0$ 的整除性过滤（如事故模式 $p=30, 8192 \pmod{30} = 22 \ne 0$）。
  - 尾检规则：累计字节 $< 8192$ 绝不判定重复；已满 8192 窗口但尾部未对齐 1024 时，提取最新的完整 8192 字节窗口执行严格周期检查。
  - 扫描预算与纯终态确认：单 channel 扫描预算上限为 $2 \times \text{limit}$；最多 3 次白名单终态确认许可（`cost = 0`，仅在具备资格且 Hash 完全一致时生效；变体篡改或重发全额扣费 `cost = len(snapshot)`）；总校验与处理量受限于 $5 \times \text{limit}$；扫描与 Hash 计算必须在单次遍历流水线（`PushWithHash`）中完成。
  - 判定优先级（Spec §5.3 严格 8 步）：
    1. 请求业务事件检查（`request_events > 65536`）
    2. Channel 数量检查（`channel_count > 128`，closed tombstone 不返还累计配额）
    3. Delta 计数检查（仅工具 channel 且为非空 append，`tool_argument_deltas > 4096`）
    4. 语义字节预算检查（channel 高水位与 `request_bytes` 累加，必须先于步骤 6 扫描预算判定）
    5. 资格/等价性核验与扫描成本确定（先查资格再 Hash，无资格绝不提前 Hash）
    6. 扫描预算扣减与检查（超额扫描拦截并映射稳定 reason：工具 channel 为 `tool_argument_bytes`，普通 channel 为 `channel_bytes`）
    7. 周期性重复检测扫描（`repetition`）
    8. 终态链兼容性放行
  - 并发契约：内置 `sync.RWMutex`，读写锁不重入，首个 reason 冻结且后续判定幂等。
  - 内存约束：每个活动 channel 状态保持在 $\le 64\text{KiB}$，`EndChannel` 释放检测窗口。
- **编译与验证约束**：
  - 测试命令使用独立缓存环境：`GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v -race ...`。
- **TDD 流程纪律**：每个任务严格遵循 Step 1 (Write failing test) -> Step 2 (Verify Red) -> Step 3 (Minimal implementation) -> Step 4 (Verify Green) -> Step 5 (Commit)。绝不使用占位符，严禁假测试。

---

## File Structure

| 文件路径 | 状态 | 职责描述 |
|---|---|---|
| `security/outputguard/repetition.go` | 新建 | 8192B 滑动窗口 Ring Buffer、KMP 前缀函数求最短周期（$1 \le p \le 256$、支持非整除周期）、1024B 步长检查点与严格尾检、单遍历 Hash 流水线（`PushWithHash`） |
| `security/outputguard/repetition_test.go` | 新建 | 周期检测器单元测试（命中短周期、非整倍数周期 $p=30$、非周期伪随机放行、大 delta 跨步长、<8192B 尾检放行、未对齐最新 8192B 窗口检测、连续 9215B 保证命中） |
| `security/outputguard/channel.go` | 新建 | 单 Channel 状态机：已计费高水位推进、累计扫描预算扣减（$2 \times \text{limit}$）、3 次白名单纯终态快照许可、单遍历 Hash 流水线、处理量 $5 \times \text{limit}$ 封顶 |
| `security/outputguard/channel_test.go` | 新建 | Channel 状态机单元测试（高水位推进、用例 8a/8b/8c 纯终态许可、变体篡改废弃许可与超额拦截、短文本放行） |
| `security/outputguard/guard.go` | 新建 | 请求级 `RequestGuard` 协调器：事件预算（65536）、Channel 累计配额（128）、工具 Delta 计数（4096）、Spec §5.3 严格 8 步判定流水线、晚到 ID 双向映射、首 reason 冻结、`sync.RWMutex` 读写安全 |
| `security/outputguard/guard_test.go` | 新建 | 请求级 Guard 单元测试（事件洪泛拦截、Channel 洪泛拦截与关闭不返还、Delta 洪泛拦截、字节溢出与优先级、首个 reason 冻结、RecordEvent 与 NoEvent 批处理、并发 race 检验） |

---

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
	prefix := []byte("non-periodic-header-padding-1234567890-abcdefghijklmnopqrstuvwxyz!@#$%^&*()-random-preamble-bytes-to-offset-")
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

func TestRepetitionDetectorPushWithHashPipelined(t *testing.T) {
	d := NewRepetitionDetector()
	data := []byte("abcdefghijklmnopqrstuvwxyz0123456789")
	const fnvOffset uint64 = 14695981039346656037
	hit, p, finalHash := d.PushWithHash(data, fnvOffset)
	if hit || p != 0 {
		t.Fatalf("unexpected hit on short non-repeating data: hit=%v, p=%d", hit, p)
	}
	// 计算预期 FNV-1a Hash
	var expectedHash uint64 = fnvOffset
	for _, b := range data {
		expectedHash = (expectedHash ^ uint64(b)) * 1099511628211
	}
	if finalHash != expectedHash {
		t.Fatalf("finalHash want %d, got %d", expectedHash, finalHash)
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

// PushWithHash 一次遍历同时流水线计算 FNV-1a Hash 并执行滑动窗口重复检测 (Spec §5.3)
func (d *RepetitionDetector) PushWithHash(data []byte, initialHash uint64) (hit bool, period int, finalHash uint64) {
	h := initialHash
	for _, b := range data {
		h = (h ^ uint64(b)) * 1099511628211

		d.window[d.head] = b
		d.head = (d.head + 1) % WindowSizeBytes
		d.count++

		// 每新增 CheckpointStepBytes 到达检查点
		if d.count >= WindowSizeBytes && (d.count-d.lastCheckedCheckpoint) >= CheckpointStepBytes {
			d.lastCheckedCheckpoint = d.count - (d.count % CheckpointStepBytes)
			if ok, p := d.checkCurrentWindow(); ok {
				return true, p, h
			}
		}
	}
	return false, 0, h
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
	h := fnv64a(data1)
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
	for i := 0; i < len(data); i++ {
		cg2.detector.window[i%8192] = data[i]
	}
	cg2.detector.count = int64(len(data))
	cg2.detector.head = len(data) % 8192

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
	"math"
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
	// int64 溢出保护：防止加法溢出翻转为负数绕过预算检查
	if deltaLen > 0 && c.highwater > math.MaxInt64-deltaLen {
		if c.kind == KindToolArguments {
			return 0, 0, ErrToolArgumentBytesExceeded
		}
		return 0, 0, ErrChannelBytesExceeded
	}
	newHighwater := c.highwater + deltaLen
	if newHighwater > c.maxBytes {
		if c.kind == KindToolArguments {
			return 0, 0, ErrToolArgumentBytesExceeded
		}
		return 0, 0, ErrChannelBytesExceeded
	}

	cost = deltaLen
	return deltaLen, cost, nil
}

func (c *ChannelGuard) CommitAppend(delta []byte, growth int64, cost int64) error {
	c.highwater += growth
	if c.kind == KindToolArguments && len(delta) > 0 {
		c.deltaCount++
	}
	if c.remainingScanBudget < cost {
		c.remainingScanBudget = 0
		return ErrScanBudgetExhausted
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
			// 单次流水线处理：一次遍历同时流水线计算 FNV-1a Hash 并执行滑动窗口重复检测 (Spec §5.3)
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
      aliasMap      map[string]string // alias mapping (e.g. item_id -> call_id)
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
	"math"
	"math/rand"
	"sync"
	"testing"
)

func defaultTestConfig() Config {
	return Config{
		Enabled:               true,
		MaxToolArgumentBytes:  131072, // 128 KiB
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152, // 2 MiB
		MaxRequestBytes:       4194304, // 4 MiB
	}
}

func TestRequestGuardRequestEventsFlood(t *testing.T) {
	rg := NewRequestGuard(defaultTestConfig())
	for i := 0; i < MaxRequestEvents; i++ {
		cid := fmt.Sprintf("c_%d", i%50)
		delta := []byte{byte(i % 250)}
		if err := rg.ObserveAppend(cid, "", KindText, delta); err != nil {
			t.Fatalf("unexpected error at event %d: %v", i, err)
		}
	}
	// 第 65537 个事件必须被拒绝并判定为 request_events (Spec §5.3 步骤 1)
	err := rg.ObserveAppend("c_0", "", KindText, []byte("x"))
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

func TestRequestGuardLateArrivingAliasBidirectionalMerge(t *testing.T) {
	rg := NewRequestGuard(defaultTestConfig())
	// 模拟流式 delta 初期仅包含 item_id = "item_123"
	if err := rg.ObserveAppend("item_123", "", KindToolArguments, []byte("part1")); err != nil {
		t.Fatalf("append with item_123 failed: %v", err)
	}
	// 稍后完成快照带来 call_id = "call_abc"，两者绑定
	if err := rg.ObserveSnapshot("call_abc", "item_123", KindToolArguments, []byte("part1part2"), "arguments.done"); err != nil {
		t.Fatalf("snapshot with call_abc and alias item_123 failed: %v", err)
	}
	// 验证 ActiveChannelCount 依然为 1，没有分裂为两个 channel
	if rg.ActiveChannelCount() != 1 {
		t.Fatalf("expected ActiveChannelCount=1 after alias merge, got %d", rg.ActiveChannelCount())
	}
	// 再次通过 item_123 访问，指向同一 Channel
	if err := rg.ObserveSnapshot("item_123", "", KindToolArguments, []byte("part1part2"), "item.done"); err != nil {
		t.Fatalf("subsequent snapshot via item_123 failed: %v", err)
	}
	if rg.ActiveChannelCount() != 1 {
		t.Fatalf("expected ActiveChannelCount=1, got %d", rg.ActiveChannelCount())
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
				_ = rg.ActiveChannelCount()
			}
		}()
	}

	wg.Wait()
}

func randBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	rng := rand.New(rand.NewSource(seed))
	_, _ = rng.Read(b)
	return b
}

// TestInt64OverflowProtection:
// 验证 ChannelGuard.highwater 和 RequestGuard.requestBytes 在接近 math.MaxInt64 时，
// 传入正数 delta 不会因为无保护加法溢出翻转为负数而绕过预算检查。
func TestInt64OverflowProtection(t *testing.T) {
	rg := NewRequestGuard(Config{
		Enabled:               true,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})

	_ = rg.ObserveAppend("c_overflow", "", KindText, []byte("init"))
	cg := rg.channels["c_overflow"]

	// 模拟高水位接近 MaxInt64
	cg.highwater = math.MaxInt64 - 10
	rg.requestBytes = math.MaxInt64 - 10

	err := rg.ObserveAppend("c_overflow", "", KindText, make([]byte, 20))
	if err == nil {
		t.Fatalf("expected overflow rejection, but got nil (highwater/requestBytes wrapped around to negative!)")
	}
	if !rg.IsTripped() {
		t.Fatalf("expected guard to be tripped on overflow")
	}
}

// TestReviewProbeAppendScanBudgetVsRequestBytesPrecedence_RED:
// 单 channel 配置下，验证 append 路径中步骤 4 (request_bytes) 严格优先于步骤 6 (scan budget exhaustion)。
func TestReviewProbeAppendScanBudgetVsRequestBytesPrecedence_RED(t *testing.T) {
	limit := int64(1 << 20)       // channel 1 MiB -> scan budget 2 MiB
	reqLimit := int64(600 * 1024) // request ≈ 0.586 MiB
	rg := NewRequestGuard(Config{
		Enabled:               true,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       limit,
		MaxRequestBytes:       reqLimit,
	})

	// 1) append 0.5MiB 非重复数据 -> requestBytes=0.5MiB, scan 剩 1.5MiB, highwater=0.5MiB
	half := randBytes(512*1024, 9101)
	if err := rg.ObserveAppend("c1", "", KindText, half); err != nil {
		t.Fatalf("seed append failed: %v", err)
	}

	// 2) 三次 0.5MiB 无资格变体快照 -> 消耗 1.5MiB scan budget，使得剩余 scan budget = 0
	variant := make([]byte, 512*1024)
	copy(variant, half)
	for i := 0; i < 3; i++ {
		variant[i*100] ^= 0xAA
		if err := rg.ObserveSnapshot("c1", "", KindText, variant, ""); err != nil {
			t.Fatalf("variant snapshot %d failed: %v", i, err)
		}
	}
	if got := rg.TrippedReason(); got != "" {
		t.Fatalf("premature trip: %s", got)
	}

	// 3) append 0.2MiB：c1 highwater 0.7MiB <= 1MiB 通过；scan budget 0 < 0.2MiB 超限；
	//    同时 requestBytes 0.5+0.2=0.7MiB > 0.586MiB 也超限。
	//    Spec §5.3 严格要求步骤 4 先于步骤 6 -> reason 必须是 request_bytes！
	probe := randBytes(200*1024, 9102)
	err := rg.ObserveAppend("c1", "", KindText, probe)
	if err == nil {
		t.Fatalf("expected rejection, got nil")
	}
	if err.Error() != "request_bytes" {
		t.Fatalf("Spec §5.3 precedence violation: want 'request_bytes', got %q", err.Error())
	}
}

// TestAppendScanBudgetVsRequestBytesPrecedenceMultiChannelValidConfig:
// 合法配置工具与通道域 (tool <= channel <= request: tool=128KiB, channel=1MiB, request=1MiB)。
// 通过另一 channel (c2) 占额，精确构造多 channel 共享 request 预算时的优先级竞态。
func TestAppendScanBudgetVsRequestBytesPrecedenceMultiChannelValidConfig(t *testing.T) {
	channelLimit := int64(1 << 20) // 1 MiB
	reqLimit := int64(1 << 20)     // 1 MiB (合法: tool 128KiB <= channel 1MiB <= request 1MiB)
	rg := NewRequestGuard(Config{
		Enabled:               true,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       channelLimit,
		MaxRequestBytes:       reqLimit,
	})

	// 1) c1 append 0.5 MiB 非重复数据 -> c1 highwater=0.5MiB, c1 scan 剩 1.5MiB, req=0.5MiB
	half := randBytes(512*1024, 9201)
	if err := rg.ObserveAppend("c1", "", KindText, half); err != nil {
		t.Fatalf("c1 seed append failed: %v", err)
	}

	// 2) c1 三次变体快照消耗 1.5 MiB scan budget -> c1 剩余 scan budget = 0, req 不变 (仍为 0.5MiB)
	variant := make([]byte, 512*1024)
	copy(variant, half)
	for i := 0; i < 3; i++ {
		variant[i*100] ^= 0x55
		if err := rg.ObserveSnapshot("c1", "", KindText, variant, ""); err != nil {
			t.Fatalf("variant snapshot %d failed: %v", i, err)
		}
	}

	// 3) c2 (另一 channel) append 0.4 MiB -> 共享 requestBytes 累加至 0.5 + 0.4 = 0.9 MiB
	c2Data := randBytes(400*1024, 9202)
	if err := rg.ObserveAppend("c2", "", KindText, c2Data); err != nil {
		t.Fatalf("c2 append failed: %v", err)
	}

	// 4) c1 尝试 append 0.2 MiB：
	//    - c1 highwater: 0.5 + 0.2 = 0.7 MiB <= 1 MiB (channel_bytes 通过)
	//    - requestBytes: 0.9 + 0.2 = 1.1 MiB > 1.0 MiB (request_bytes 超限，步骤 4)
	//    - c1 scan budget: 0 < 0.2 MiB (scan budget 不足，步骤 6)
	//    根据 Spec 优先级，必须报 request_bytes 而非 channel_bytes！
	probe := randBytes(200*1024, 9203)
	err := rg.ObserveAppend("c1", "", KindText, probe)
	if err == nil {
		t.Fatalf("expected rejection, got nil")
	}
	if err.Error() != "request_bytes" {
		t.Fatalf("Multi-channel precedence violation: want 'request_bytes', got %q", err.Error())
	}
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
	"math"
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

func (g *RequestGuard) ActiveChannelCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.channels)
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
		return g.tripLocked(err.Error())
	}

	// 步骤 4: 请求级语义字节检查（必须先于步骤 6 扫描预算判定，且含 int64 防溢出）
	if growth > 0 && g.requestBytes > math.MaxInt64-growth {
		return g.tripLocked("request_bytes")
	}
	if g.requestBytes+growth > g.cfg.MaxRequestBytes {
		return g.tripLocked("request_bytes")
	}

	// 步骤 6: 扫描预算扣减与检查 (Spec §5.3 步骤 6，在步骤 4 request_bytes 之后执行)
	if cg.remainingScanBudget < cost {
		if cg.kind == KindToolArguments {
			return g.tripLocked("tool_argument_bytes")
		}
		return g.tripLocked("channel_bytes")
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
	if growth > 0 && g.requestBytes > math.MaxInt64-growth {
		return g.tripLocked("request_bytes")
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

	targetKey := channelID
	if alias := g.aliasMap[channelID]; alias != "" {
		targetKey = alias
	}

	if cg, exists := g.channels[targetKey]; exists {
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
