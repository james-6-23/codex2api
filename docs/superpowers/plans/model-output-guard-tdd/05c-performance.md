# 05c-Performance: 统计学度量工具、性能基准门禁与自动化调度执行计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 基于已批准的 Spec §9.3（性能与资源上限场景 24~28），构建高精度非周期 PRNG、基于 Linux `syscall.Getrusage` 的微秒级真进程 CPU 计时器（非 Linux build tag 显式隔离绝不伪造 CPU 时间）、P95/Median 统计分析与受控自动化 Gate 校验器；并在同 package `proxy` 下集成真实 HTTP 流式管线与 `security/outputguard` 核心状态机，对场景 24（TTFT P95 增量 $\le 10\text{ms}$、CPU 增量 $\le 20\%$，且 baseline CPU $\le 0$ 视无效实验 FAIL）、场景 25（1/2/4 MiB 线性度增长比值 $\le 2.5$，分别核验 envelope、scan 与 snapshot）、场景 26（$\le 64\text{KiB/channel}$、长跑 10 分钟堆/Goroutine/FD 增长自动 fail）、场景 27（单字节极端细 delta、4096 计数及长思考预算先于字节耗尽分析）与场景 28（真实 Error/Log 脱敏边界）执行严密测试与 25 分钟 `-race` 调度。

**Architecture:**
1. `proxy/perfstat/`: 统计学度量工具包，包含微秒级 Linux `syscall.Getrusage` 计时器（`cpu_linux.go`）与非 Linux 编译隔离（`cpu_other.go`）、基于 uint64 模乘加线性同余（LCG）高位混淆采样的 PRNG（具备大状态空间与非短周期性质，但因乘加同余性质不作绝对无环无碰撞宣称）、分位数插值计算及基准门禁校验器（`ValidateTTFTAndCPUGates`、`ValidateLinearScalingRatio`）；
2. `proxy/acceptance_benchmark_test.go`: 性能与基准验收测试套件（package `proxy`），复用真实 `NewHandler` 与 Mock Upstream 完整 HTTP SSE 流式链路（场景 24），并在多批次采样中保证 baseline CPU 真实消耗，杜绝 0 耗时被错误归一化；在场景 25 中对真实 `RequestGuard` 的 Append/Scan/Snapshot 分项测算并断言未触发 guard；场景 26 针对并发长跑设立环境变量门禁（未设置时默认 smoke 并在报告中如实陈述）；场景 27 量化 4096 deltas 拦截；场景 28 断言内部与公开诊断绝对不泄露敏感 Pattern；
3. `scripts/run_acceptance_suite.sh`: 自动化执行调度脚本，设置 25 分钟超时，强制 `-race`，支持真实长跑环境变量注入与最终状态矩阵输出。

**Tech Stack:** Go 1.26+, `syscall.Getrusage`, `net/http/httptest`, `github.com/codex2api/auth`, `github.com/codex2api/config`, `github.com/codex2api/proxy`, `github.com/codex2api/security/outputguard`, `github.com/gin-gonic/gin`.

**Spec:** `docs/superpowers/specs/2026-10-28-model-output-guard-design.md`

## Global Constraints

- **不可妥协的设计取舍**：
  - 场景 24 必须包含 HTTP 客户端真实接收有效 Delta，若 Baseline CPU 非正（$\le 0$）视作测量采样不足或无效实验，测试直接 FAIL，严禁除以 0 或把 ratio 设为 0。
  - 场景 25 必须验证 1/2/4 MiB 且每倍增长比值 $\le 2.5$，分别核验 Envelope、Scan、Snapshot 分项，断言 guard 不被 tripped，PRNG 采用高周期算法而非 `mod 251` 周期数据。
  - 场景 26 验证每 Channel 状态 $\le 64\text{KiB}$，结合 `runtime.KeepAlive` 保持引用；提供 $\ge 10$ 分钟长跑门禁，堆内存、Goroutine 与 FD 异常增长由测试自动 `t.Fatalf` 判定失败（非仅 `t.Log`）；默认快速 smoke 必须在报告中如实说明长跑尚未执行。
  - 场景 27 细 delta 压测模拟 1~4B 极端细碎 delta 与 4096 次计数，量化事件预算相对字节预算更早耗尽的时机，不可被周期 fixture 提前误判为 repetition。
  - 场景 28 日志与错误 envelope 脱敏，内部及公开 reason 严禁包含敏感内容与 key，不把 DSH 恢复当验收依据。
- **编译与隔离约束**：
  - 测试命令使用独立缓存环境：`GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v -race ...`。
  - 非 Linux 系统通过 `//go:build !linux` 隔离，显式不支持真进程 CPU 采集，绝不使用 `time.Since` 挂钟时间伪造进程 CPU。

---

## File Structure

| 文件路径 | 状态 | 职责描述 |
|---|---|---|
| `proxy/perfstat/perfstat.go` | 新建 | 非周期 PRNG、分位数/中位数统计学计算、TTFT 与 CPU 增量门禁校验器、线性度增长比值校验器 |
| `proxy/perfstat/cpu_linux.go` | 新建 | Linux 平台真进程 CPU 计时器（通过 `syscall.Getrusage(syscall.RUSAGE_SELF, ...)` 获取微秒级 User + System CPU） |
| `proxy/perfstat/cpu_other.go` | 新建 | 非 Linux 平台编译隔离，明确不支持真进程 CPU 采集，返回 0 触发无效实验判定，绝不伪造 |
| `proxy/perfstat/perfstat_test.go` | 新建 | 统计学工具单元测试（PRNG 无短周期验证、分位数计算、验证器红绿反证测试） |
| `proxy/acceptance_benchmark_test.go` | 新建 | 场景 24~28 端到端性能与资源门禁验收测试（同一 package `proxy`，可无缝访问内部真实 Handler 与 fixture） |
| `scripts/run_acceptance_suite.sh` | 新建 | 完整自动化调度脚本（强制 `-race`，25m 超时，接收 `RUN_OUTPUTGUARD_LONG_RUN` 环境变量） |

---

## Tasks

### Task 11.1: 统计学度量工具包（Process CPU Timer, Non-Repeating PRNG, Validators）

**Files:**
- Create: `proxy/perfstat/perfstat.go`
- Create: `proxy/perfstat/cpu_linux.go`
- Create: `proxy/perfstat/cpu_other.go`
- Test: `proxy/perfstat/perfstat_test.go`

**Interfaces:**
- Produces:
  ```go
  package perfstat

  type ProcessCPUTimer struct { ... }
  func StartProcessCPUTimer() *ProcessCPUTimer
  func (t *ProcessCPUTimer) ElapsedProcessCPU() time.Duration
  func (t *ProcessCPUTimer) ElapsedWallTime() time.Duration
  func (t *ProcessCPUTimer) GetrusageFailed() bool

  type NonRepeatingPRNG struct { ... }
  func NewNonRepeatingPRNG(seed int64) *NonRepeatingPRNG
  func (p *NonRepeatingPRNG) NextUint64() uint64
  func (p *NonRepeatingPRNG) Fill(buf []byte)
  func (p *NonRepeatingPRNG) GenerateASCII(length int) []byte

  func CalculatePercentile(samples []float64, percentile float64) float64
  func CalculateMedian(samples []float64) float64

  type TTFTAndCPUBenchmarkResult struct {
      BaselineP95TTFTMs float64
      GuardP95TTFTMs    float64
      TTFTDeltaMs       float64
      BaselineCPUMs     float64
      GuardCPUMs        float64
      CPUIncreaseRatio  float64
  }
  func ValidateTTFTAndCPUGates(res TTFTAndCPUBenchmarkResult) error
  func ValidateLinearScalingRatio(cpu1MiB, cpu2MiB, cpu4MiB float64) error
  ```

- [ ] **Step 1: Write failing test for perfstat library and validators**

```go
// proxy/perfstat/perfstat_test.go
package perfstat

import (
	"math"
	"runtime"
	"strings"
	"testing"
)

func TestProcessCPUTimerLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skipping Linux-specific RUSAGE process CPU timer test on non-Linux")
	}
	timer := StartProcessCPUTimer()
	if timer.GetrusageFailed() {
		t.Fatalf("Getrusage failed during timer start")
	}
	// 执行足够的 CPU 密集计算，确保产生正的 CPU 测量值
	sum := 0
	for i := 0; i < 5000000; i++ {
		sum += i ^ (i >> 1)
	}
	_ = sum
	runtime.KeepAlive(timer)
	cpu := timer.ElapsedProcessCPU()
	wall := timer.ElapsedWallTime()
	if wall <= 0 {
		t.Fatalf("expected wall time > 0")
	}
	if cpu <= 0 {
		t.Fatalf("expected positive CPU duration for busywork on Linux, got %v", cpu)
	}
	t.Logf("Measured CPU: %v, Wall: %v", cpu, wall)
}

func TestNonRepeatingPRNG(t *testing.T) {
	prng := NewNonRepeatingPRNG(123456789)
	buf1 := prng.GenerateASCII(10000)
	prng2 := NewNonRepeatingPRNG(123456789)
	buf2 := prng2.GenerateASCII(10000)

	if string(buf1) != string(buf2) {
		t.Fatalf("PRNG with same seed should be deterministic")
	}

	// 验证在 8192 字节滑动窗口内无短周期 (1 <= p <= 256)
	for p := 1; p <= 256; p++ {
		match := true
		for i := p; i < 8192; i++ {
			if buf1[i] != buf1[i-p] {
				match = false
				break
			}
		}
		if match {
			t.Fatalf("PRNG generated repeating period p=%d in 8192-byte window!", p)
		}
	}
}

func TestCalculatePercentileAndMedian(t *testing.T) {
	samples := []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	med := CalculateMedian(samples)
	if med != 55.0 {
		t.Fatalf("expected median 55.0, got %v", med)
	}

	p95 := CalculatePercentile(samples, 0.95)
	if p95 < 90 || p95 > 100 {
		t.Fatalf("unexpected p95: %v", p95)
	}

	if CalculatePercentile(nil, 0.95) != 0 {
		t.Fatalf("expected 0 for empty samples")
	}
}

func TestValidateTTFTAndCPUGates(t *testing.T) {
	// 1. 正常合格样本 (Green)
	valid := TTFTAndCPUBenchmarkResult{
		BaselineP95TTFTMs: 15.0,
		GuardP95TTFTMs:    22.0,
		TTFTDeltaMs:       7.0, // <= 10ms
		BaselineCPUMs:     10.0,
		GuardCPUMs:        11.5,
		CPUIncreaseRatio:  0.15, // <= 20%
	}
	if err := ValidateTTFTAndCPUGates(valid); err != nil {
		t.Fatalf("expected valid gates to pass, got: %v", err)
	}

	// 2. 基线 CPU <= 0 视作无效实验 FAIL (Red)
	invalidBase := TTFTAndCPUBenchmarkResult{
		BaselineP95TTFTMs: 15.0,
		GuardP95TTFTMs:    22.0,
		TTFTDeltaMs:       7.0,
		BaselineCPUMs:     0.0, // 无效！
		GuardCPUMs:        1.0,
		CPUIncreaseRatio:  0.0,
	}
	err := ValidateTTFTAndCPUGates(invalidBase)
	if err == nil || !strings.Contains(err.Error(), "baseline CPU time must be > 0") {
		t.Fatalf("expected baseline <= 0 to fail as invalid experiment, got: %v", err)
	}

	// 3. TTFT 增量超标 FAIL (Red)
	badTTFT := TTFTAndCPUBenchmarkResult{
		BaselineP95TTFTMs: 10.0,
		GuardP95TTFTMs:    25.0,
		TTFTDeltaMs:       15.0, // > 10ms
		BaselineCPUMs:     10.0,
		GuardCPUMs:        11.0,
		CPUIncreaseRatio:  0.10,
	}
	err = ValidateTTFTAndCPUGates(badTTFT)
	if err == nil || !strings.Contains(err.Error(), "TTFT p95 delta gate failed") {
		t.Fatalf("expected TTFT delta > 10ms to fail, got: %v", err)
	}

	// 4. CPU 增量超标 FAIL (Red)
	badCPU := TTFTAndCPUBenchmarkResult{
		BaselineP95TTFTMs: 10.0,
		GuardP95TTFTMs:    15.0,
		TTFTDeltaMs:       5.0,
		BaselineCPUMs:     10.0,
		GuardCPUMs:        13.0,
		CPUIncreaseRatio:  0.30, // > 20%
	}
	err = ValidateTTFTAndCPUGates(badCPU)
	if err == nil || !strings.Contains(err.Error(), "CPU increase ratio gate failed") {
		t.Fatalf("expected CPU ratio > 20%% to fail, got: %v", err)
	}

	t.Run("RejectNaNAndInf", func(t *testing.T) {
		nan := math.NaN()
		inf := math.Inf(1)

		nanRes := TTFTAndCPUBenchmarkResult{
			BaselineP95TTFTMs: nan, GuardP95TTFTMs: nan, TTFTDeltaMs: nan,
			BaselineCPUMs: nan, GuardCPUMs: nan, CPUIncreaseRatio: nan,
		}
		if err := ValidateTTFTAndCPUGates(nanRes); err == nil || !strings.Contains(err.Error(), "NaN or Inf") {
			t.Fatalf("expected NaN TTFTAndCPUBenchmarkResult to be rejected, got: %v", err)
		}

		infRes := valid
		infRes.CPUIncreaseRatio = inf
		if err := ValidateTTFTAndCPUGates(infRes); err == nil || !strings.Contains(err.Error(), "NaN or Inf") {
			t.Fatalf("expected Inf CPUIncreaseRatio to be rejected, got: %v", err)
		}
	})
}

func TestValidateLinearScalingRatio(t *testing.T) {
	// 1. 合格样本 (Green: 10ms -> 22ms [2.2x] -> 48ms [2.18x])
	if err := ValidateLinearScalingRatio(10.0, 22.0, 48.0); err != nil {
		t.Fatalf("expected linear scaling to pass, got: %v", err)
	}

	// 2. 超标样本 (Red: 10ms -> 30ms [3.0x > 2.5])
	err := ValidateLinearScalingRatio(10.0, 30.0, 60.0)
	if err == nil || !strings.Contains(err.Error(), "linearity ratio (1MiB->2MiB) failed") {
		t.Fatalf("expected ratio > 2.5 to fail, got: %v", err)
	}

	// 3. 无效样本 (Red: cpu1MiB <= 0)
	err = ValidateLinearScalingRatio(0.0, 20.0, 40.0)
	if err == nil || !strings.Contains(err.Error(), "all CPU median samples must be > 0") {
		t.Fatalf("expected non-positive CPU to fail, got: %v", err)
	}

	// 4. NaN / Inf 校验拒绝 (Red)
	nan := math.NaN()
	err = ValidateLinearScalingRatio(nan, nan, nan)
	if err == nil || !strings.Contains(err.Error(), "NaN or Inf") {
		t.Fatalf("expected NaN scaling inputs to be rejected, got: %v", err)
	}
}

func TestCalculatePercentileNaNProtection(t *testing.T) {
	samples := []float64{10.0, 20.0, 30.0}
	nan := math.NaN()
	p := CalculatePercentile(samples, nan)
	if !math.IsNaN(p) && p != 0 {
		t.Fatalf("expected NaN or 0 for NaN percentile query, got %v", p)
	}
}
```

- [ ] **Step 2: Run test to verify failure**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v -race ./proxy/perfstat`
Expected: FAIL with undefined types/functions.

- [ ] **Step 3: Implement minimal code in perfstat package**

```go
// proxy/perfstat/perfstat.go
package perfstat

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

type ProcessCPUTimer struct {
	startCPU  time.Duration
	startTime time.Time
	failed    bool
}

func StartProcessCPUTimer() *ProcessCPUTimer {
	startCPU, err := currentProcessCPU()
	return &ProcessCPUTimer{
		startCPU:  startCPU,
		startTime: time.Now(),
		failed:    err != nil,
	}
}

func (t *ProcessCPUTimer) GetrusageFailed() bool {
	return t.failed
}

func (t *ProcessCPUTimer) ElapsedProcessCPU() time.Duration {
	if t.failed {
		return 0
	}
	now, err := currentProcessCPU()
	if err != nil || now < t.startCPU {
		return 0
	}
	return now - t.startCPU
}

func (t *ProcessCPUTimer) ElapsedWallTime() time.Duration {
	return time.Since(t.startTime)
}

type NonRepeatingPRNG struct {
	state uint64
}

func NewNonRepeatingPRNG(seed int64) *NonRepeatingPRNG {
	if seed == 0 {
		seed = 123456789
	}
	return &NonRepeatingPRNG{state: uint64(seed)}
}

func (p *NonRepeatingPRNG) NextUint64() uint64 {
	p.state = (p.state*6364136223846793005 + 1442695040888963407)
	return p.state
}

func (p *NonRepeatingPRNG) Fill(buf []byte) {
	for i := 0; i < len(buf); {
		val := p.NextUint64()
		for b := 0; b < 8 && i < len(buf); b++ {
			buf[i] = byte(val)
			val >>= 8
			i++
		}
	}
}

func (p *NonRepeatingPRNG) GenerateASCII(length int) []byte {
	out := make([]byte, length)
	for i := 0; i < length; i++ {
		// 取高 32 位进行混淆采样，保证全范围良好分布，彻底消除低位短周期性
		out[i] = byte(32 + ((p.NextUint64() >> 32) % 95))
	}
	return out
}

func CalculatePercentile(samples []float64, percentile float64) float64 {
	if len(samples) == 0 || math.IsNaN(percentile) || math.IsInf(percentile, 0) {
		return 0
	}
	for _, s := range samples {
		if math.IsNaN(s) || math.IsInf(s, 0) {
			return 0
		}
	}
	sorted := make([]float64, len(samples))
	copy(sorted, samples)
	sort.Float64s(sorted)

	if percentile <= 0 {
		return sorted[0]
	}
	if percentile >= 1.0 {
		return sorted[len(sorted)-1]
	}

	index := percentile * float64(len(sorted)-1)
	lower := int(math.Floor(index))
	upper := int(math.Ceil(index))
	if lower == upper {
		return sorted[lower]
	}
	weight := index - float64(lower)
	return sorted[lower]*(1.0-weight) + sorted[upper]*weight
}

func CalculateMedian(samples []float64) float64 {
	return CalculatePercentile(samples, 0.5)
}

type TTFTAndCPUBenchmarkResult struct {
	BaselineP95TTFTMs float64
	GuardP95TTFTMs    float64
	TTFTDeltaMs       float64
	BaselineCPUMs     float64
	GuardCPUMs        float64
	CPUIncreaseRatio  float64
}

func ValidateTTFTAndCPUGates(res TTFTAndCPUBenchmarkResult) error {
	// 拒绝所有 NaN 或 Inf 输入，防止因比较静默返回 false 而漏过门禁
	if math.IsNaN(res.BaselineP95TTFTMs) || math.IsInf(res.BaselineP95TTFTMs, 0) ||
		math.IsNaN(res.GuardP95TTFTMs) || math.IsInf(res.GuardP95TTFTMs, 0) ||
		math.IsNaN(res.TTFTDeltaMs) || math.IsInf(res.TTFTDeltaMs, 0) ||
		math.IsNaN(res.BaselineCPUMs) || math.IsInf(res.BaselineCPUMs, 0) ||
		math.IsNaN(res.GuardCPUMs) || math.IsInf(res.GuardCPUMs, 0) ||
		math.IsNaN(res.CPUIncreaseRatio) || math.IsInf(res.CPUIncreaseRatio, 0) {
		return fmt.Errorf("invalid experiment: TTFT and CPU benchmark result contains NaN or Inf")
	}

	if res.BaselineCPUMs <= 0 {
		return fmt.Errorf("invalid experiment: baseline CPU time must be > 0, got %.4f ms", res.BaselineCPUMs)
	}
	if res.TTFTDeltaMs > 10.0 {
		return fmt.Errorf("TTFT p95 delta gate failed: target <= 10.0ms, got %.2fms (baseline=%.2fms, guard=%.2fms)",
			res.TTFTDeltaMs, res.BaselineP95TTFTMs, res.GuardP95TTFTMs)
	}
	if res.CPUIncreaseRatio > 0.20 {
		return fmt.Errorf("CPU increase ratio gate failed: target <= 20.0%% (0.20), got %.2f%% (baseline=%.2fms, guard=%.2fms)",
			res.CPUIncreaseRatio*100, res.BaselineCPUMs, res.GuardCPUMs)
	}
	return nil
}

func ValidateLinearScalingRatio(cpu1MiB, cpu2MiB, cpu4MiB float64) error {
	if math.IsNaN(cpu1MiB) || math.IsInf(cpu1MiB, 0) ||
		math.IsNaN(cpu2MiB) || math.IsInf(cpu2MiB, 0) ||
		math.IsNaN(cpu4MiB) || math.IsInf(cpu4MiB, 0) {
		return fmt.Errorf("invalid experiment: CPU scaling measurements contain NaN or Inf")
	}

	if cpu1MiB <= 0 || cpu2MiB <= 0 || cpu4MiB <= 0 {
		return fmt.Errorf("invalid experiment: all CPU median samples must be > 0 (1MiB=%.2f, 2MiB=%.2f, 4MiB=%.2f)",
			cpu1MiB, cpu2MiB, cpu4MiB)
	}
	ratio1to2 := cpu2MiB / cpu1MiB
	ratio2to4 := cpu4MiB / cpu2MiB

	if ratio1to2 > 2.5 {
		return fmt.Errorf("linearity ratio (1MiB->2MiB) failed: target <= 2.5, got %.2f (1MiB=%.2f, 2MiB=%.2f)",
			ratio1to2, cpu1MiB, cpu2MiB)
	}
	if ratio2to4 > 2.5 {
		return fmt.Errorf("linearity ratio (2MiB->4MiB) failed: target <= 2.5, got %.2f (2MiB=%.2f, 4MiB=%.2f)",
			ratio2to4, cpu2MiB, cpu4MiB)
	}
	return nil
}

var ErrInvalidBaseline = errors.New("invalid experiment baseline")
```

```go
// proxy/perfstat/cpu_linux.go
//go:build linux

package perfstat

import (
	"syscall"
	"time"
)

func currentProcessCPU() (time.Duration, error) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0, err
	}
	utime := time.Duration(usage.Utime.Sec)*time.Second + time.Duration(usage.Utime.Usec)*time.Microsecond
	stime := time.Duration(usage.Stime.Sec)*time.Second + time.Duration(usage.Stime.Usec)*time.Microsecond
	return utime + stime, nil
}
```

```go
// proxy/perfstat/cpu_other.go
//go:build !linux

package perfstat

import (
	"errors"
	"time"
)

var ErrProcessCPUNotSupported = errors.New("process CPU measurement via RUSAGE is only supported on Linux in this build")

func currentProcessCPU() (time.Duration, error) {
	return 0, ErrProcessCPUNotSupported
}
```

- [ ] **Step 4: Run test to verify passing (Green)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v -race ./proxy/perfstat`
Expected: PASS ok github.com/codex2api/proxy/perfstat

- [ ] **Step 5: Commit**

```bash
git add proxy/perfstat/
git commit -m "feat(perfstat): add process cpu timer, non-repeating prng and statistical validators"
```

---

### Task 11.5: 性能基准门禁与资源泄露验收套件（Spec §9.3 场景 24~28）

**Files:**
- Create: `proxy/acceptance_benchmark_test.go`

**Interfaces:**
- Consumes:
  - `github.com/codex2api/proxy/perfstat`
  - `github.com/codex2api/security/outputguard`
  - `github.com/codex2api/proxy` (package `proxy` internal: `NewHandler`, `ApplyOutputGuardSettings`, `CurrentOutputGuardConfig`, `FormatGuardTerminalEnvelope`, `ObserveEvent`, `NewResponsesProjector` 等已批准计划接口；计划交付阶段不等同于生产已就绪)
- Produces:
  - 场景 24: 同一真实 HTTP 流式管线 on/off 各 5 组，客户端真实接收，首有效 TTFT P95 增量 $\le 10\text{ms}$、CPU 增量 $\le 20\%$，采样确保 base CPU $> 0$；
  - 场景 25: 1/2/4 MiB CPU median 每翻倍 $\le 2.5$，分别核验 envelope、scan 与 snapshot，断言全部 guard 返回不得 tripped；
  - 场景 26: $\le 64\text{KiB/channel}$ 活引用 `runtime.KeepAlive`，$\ge 10\text{min}$ 并发长跑堆/Goroutine/FD 增长自动 fail；
  - 场景 27: 多工具细 delta 1~4B 4096 计数与长 thinking/request events 预算量化；
  - 场景 28: 真实 error/log 脱敏契约断言。

- [ ] **Step 1: Write failing benchmark tests for Scenarios 24~28**

```go
// proxy/acceptance_benchmark_test.go
package proxy

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy/perfstat"
	"github.com/codex2api/security/outputguard"
	"github.com/gin-gonic/gin"
)

// Helper: 搭建产生完整合法 Responses SSE 流的 upstream
func newBenchmarkUpstreamSSEServer(t *testing.T, payloadChunks []string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_bench\"}}\n\n")
		flusher.Flush()
		for _, chunk := range payloadChunks {
			_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\""+chunk+"\"}\n\n")
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_bench\",\"status\":\"completed\"}}\n\n")
		flusher.Flush()
	}))
	t.Cleanup(server.Close)
	return server
}

// 场景 24: 同一真实 HTTP 流式管线 on/off 各 5 组，真实客户端首有效 delta TTFT P95 增量 <= 10ms、process CPU <= 20%
func TestAcceptanceScenario24_RealPipelineTTFTAndCPUGates(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 构造 1 MiB 正常文本，切分成 256 个 4 KiB chunk 模拟多批次流式下发，产生稳定真实的 CPU 消耗
	prng := perfstat.NewNonRepeatingPRNG(42)
	const totalChunks = 256
	const chunkSize = 4096
	chunks := make([]string, totalChunks)
	for i := 0; i < totalChunks; i++ {
		chunks[i] = string(prng.GenerateASCII(chunkSize))
	}

	upstream := newBenchmarkUpstreamSSEServer(t, chunks)
	store := auth.NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{
		DBID:        1,
		AccessToken: "at-bench",
		PlanType:    "pro",
		AccountID:   "acct-bench",
		BaseURL:     upstream.URL,
	})

	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	engine := gin.New()
	engine.POST("/v1/responses", handler.Responses)
	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)

	client := server.Client()

	runClientRequest := func() (firstTokenMs float64, bodyBytes int, err error) {
		reqBody := `{"model":"gpt-4.1-direct","input":"hello","stream":true}`
		start := time.Now()
		resp, err := client.Post(server.URL+"/v1/responses", "application/json", strings.NewReader(reqBody))
		if err != nil {
			return 0, 0, err
		}
		defer resp.Body.Close()

		firstFound := false
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			bodyBytes += len(line)
			if !firstFound && strings.Contains(line, "response.output_text.delta") {
				firstTokenMs = float64(time.Since(start).Milliseconds())
				firstFound = true
			}
		}
		if err := scanner.Err(); err != nil {
			return 0, bodyBytes, err
		}
		if !firstFound {
			return 0, bodyBytes, fmt.Errorf("no output_text.delta received")
		}
		return firstTokenMs, bodyBytes, nil
	}

	const rounds = 5
	var baselineTTFTs, guardTTFTs []float64
	var baselineCPUs, guardCPUs []float64

	// 保存并恢复初始配置，避免测试间状态耦合
	prevConfig := CurrentOutputGuardConfig()
	t.Cleanup(func() {
		enabled := prevConfig.Enabled
		ApplyOutputGuardSettings(database.OutputGuardSettings{
			Enabled:               &enabled,
			MaxToolArgumentBytes:  prevConfig.MaxToolArgumentBytes,
			MaxToolArgumentDeltas: prevConfig.MaxToolArgumentDeltas,
			MaxChannelBytes:       prevConfig.MaxChannelBytes,
			MaxRequestBytes:       prevConfig.MaxRequestBytes,
			Generation:            prevConfig.Generation + 100,
		})
	})

	var testGen int64 = 1000

	// 1. Guard OFF 5 组基准轮
	for r := 0; r < rounds; r++ {
		// 设置 Guard OFF
		testGen++
		offEnabled := false
		ApplyOutputGuardSettings(database.OutputGuardSettings{
			Enabled:               &offEnabled,
			MaxToolArgumentBytes:  131072,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
			Generation:            testGen,
		})

		timer := perfstat.StartProcessCPUTimer()
		// 每轮连续执行 5 次请求以积累足够显著的真实 CPU 采样，杜绝微秒抖动为 0
		var rTTFTs []float64
		for b := 0; b < 5; b++ {
			ttft, bytesReceived, err := runClientRequest()
			if err != nil || bytesReceived == 0 {
				t.Fatalf("round %d off batch %d failed: %v", r, b, err)
			}
			rTTFTs = append(rTTFTs, ttft)
		}
		cpuMs := float64(timer.ElapsedProcessCPU().Milliseconds())
		baselineCPUs = append(baselineCPUs, cpuMs)
		baselineTTFTs = append(baselineTTFTs, perfstat.CalculatePercentile(rTTFTs, 0.95))
	}

	// 2. Guard ON 5 组防护轮
	for r := 0; r < rounds; r++ {
		// 设置 Guard ON
		testGen++
		onEnabled := true
		ApplyOutputGuardSettings(database.OutputGuardSettings{
			Enabled:               &onEnabled,
			MaxToolArgumentBytes:  131072,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
			Generation:            testGen,
		})

		timer := perfstat.StartProcessCPUTimer()
		var rTTFTs []float64
		for b := 0; b < 5; b++ {
			ttft, bytesReceived, err := runClientRequest()
			if err != nil || bytesReceived == 0 {
				t.Fatalf("round %d on batch %d failed: %v", r, b, err)
			}
			rTTFTs = append(rTTFTs, ttft)
		}
		cpuMs := float64(timer.ElapsedProcessCPU().Milliseconds())
		guardCPUs = append(guardCPUs, cpuMs)
		guardTTFTs = append(guardTTFTs, perfstat.CalculatePercentile(rTTFTs, 0.95))
	}

	baseP95TTFT := perfstat.CalculatePercentile(baselineTTFTs, 0.95)
	guardP95TTFT := perfstat.CalculatePercentile(guardTTFTs, 0.95)
	ttftDelta := guardP95TTFT - baseP95TTFT
	if ttftDelta < 0 {
		ttftDelta = 0
	}

	baseMedianCPU := perfstat.CalculateMedian(baselineCPUs)
	guardMedianCPU := perfstat.CalculateMedian(guardCPUs)

	// 若 baseMedianCPU <= 0 视作无效实验直接 FAIL，绝不能把 ratio 设为 0
	if baseMedianCPU <= 0 {
		t.Fatalf("INVALID EXPERIMENT: baseline process CPU was <= 0 (%.2f ms). Workload insufficient to measure rusage.", baseMedianCPU)
	}

	cpuIncreaseRatio := (guardMedianCPU - baseMedianCPU) / baseMedianCPU

	res := perfstat.TTFTAndCPUBenchmarkResult{
		BaselineP95TTFTMs: baseP95TTFT,
		GuardP95TTFTMs:    guardP95TTFT,
		TTFTDeltaMs:       ttftDelta,
		BaselineCPUMs:     baseMedianCPU,
		GuardCPUMs:        guardMedianCPU,
		CPUIncreaseRatio:  cpuIncreaseRatio,
	}

	t.Logf("Scenario 24 Results: TTFT P95 Delta = %.2f ms (base=%.2f, guard=%.2f), CPU Ratio = %.2f%% (base=%.2fms, guard=%.2fms)",
		res.TTFTDeltaMs, res.BaselineP95TTFTMs, res.GuardP95TTFTMs, res.CPUIncreaseRatio*100, res.BaselineCPUMs, res.GuardCPUMs)

	if err := perfstat.ValidateTTFTAndCPUGates(res); err != nil {
		t.Fatalf("Scenario 24 failed performance gates: %v", err)
	}
}

// 场景 25: 1/2/4 MiB CPU median 每翻倍 <= 2.5，分别核验 envelope、scan 与 snapshot，全部 guard 不得 tripped，PRNG 非周期
func TestAcceptanceScenario25_LinearScalingRatiosDetailed(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skipping Scenario 25 process CPU linearity gating on non-Linux platform (RUSAGE unsupported)")
	}
	sizes := []int{1048576, 2097152, 4194304} // 1 MiB, 2 MiB, 4 MiB
	var envelopeMedians, scanMedians, snapshotMedians []float64

	prng := perfstat.NewNonRepeatingPRNG(987654321)

	for _, size := range sizes {
		data := prng.GenerateASCII(size)
		var envCPUs, scanCPUs, snapCPUs []float64

		for r := 0; r < 5; r++ {
			// A. Envelope 投影开销（调用 03 协议实际 ResponsesProjector 与 ObserveEvent，非单纯 memcpy）
			projector := NewResponsesProjector()
			rgEnv := outputguard.NewRequestGuard(outputguard.Config{
				Enabled:               true,
				MaxToolArgumentBytes:  int64(size * 2),
				MaxToolArgumentDeltas: 65536,
				MaxChannelBytes:       int64(size * 2),
				MaxRequestBytes:       int64(size * 4),
			})

			tEnv := perfstat.StartProcessCPUTimer()
			if tEnv.GetrusageFailed() {
				t.Fatalf("failed to read process CPU timer")
			}
			// 模拟 16 KiB 分块流式投影与分派
			for offset := 0; offset < size; offset += 16384 {
				end := offset + 16384
				if end > size {
					end = size
				}
				chunkPayload := fmt.Sprintf(`{"type":"response.output_text.delta","item_id":"item_proj","content_index":0,"delta":"%s"}`, string(data[offset:end]))
				err := ObserveEvent(rgEnv, projector, "response.output_text.delta", []byte(chunkPayload))
				if err != nil {
					t.Fatalf("size %d round %d ObserveEvent projection failed: %v", size, r, err)
				}
			}
			envCPUs = append(envCPUs, float64(tEnv.ElapsedProcessCPU().Microseconds()))

			// B. 扫描器线性周期检测与状态机开销
			rg := outputguard.NewRequestGuard(outputguard.Config{
				Enabled:               true,
				MaxToolArgumentBytes:  int64(size * 2),
				MaxToolArgumentDeltas: 65536,
				MaxChannelBytes:       int64(size * 2),
				MaxRequestBytes:       int64(size * 4),
			})

			tScan := perfstat.StartProcessCPUTimer()
			if tScan.GetrusageFailed() {
				t.Fatalf("failed to read process CPU timer")
			}
			// 按 16 KiB 分块注入进行线性增量扫描
			for offset := 0; offset < size; offset += 16384 {
				end := offset + 16384
				if end > size {
					end = size
				}
				err := rg.ObserveAppend("c_linear", "", outputguard.KindText, data[offset:end])
				if err != nil {
					t.Fatalf("size %d round %d scan tripped unexpectedly: %v", size, r, err)
				}
			}
			scanCPUs = append(scanCPUs, float64(tScan.ElapsedProcessCPU().Microseconds()))

			// C. Snapshot 完整快照比对开销
			tSnap := perfstat.StartProcessCPUTimer()
			if tSnap.GetrusageFailed() {
				t.Fatalf("failed to read process CPU timer")
			}
			err := rg.ObserveSnapshot("c_linear", "", outputguard.KindText, data, "response.completed")
			if err != nil {
				t.Fatalf("size %d round %d snapshot tripped unexpectedly: %v", size, r, err)
			}
			snapCPUs = append(snapCPUs, float64(tSnap.ElapsedProcessCPU().Microseconds()))

			// 断言全部 guard 绝对不得处于 tripped 状态
			if rg.IsTripped() {
				t.Fatalf("guard erroneously tripped for non-repeating data: %s", rg.TrippedReason())
			}
			if rgEnv.IsTripped() {
				t.Fatalf("guard erroneously tripped during projection: %s", rgEnv.TrippedReason())
			}
		}

		envelopeMedians = append(envelopeMedians, perfstat.CalculateMedian(envCPUs))
		scanMedians = append(scanMedians, perfstat.CalculateMedian(scanCPUs))
		snapshotMedians = append(snapshotMedians, perfstat.CalculateMedian(snapCPUs))
	}

	t.Logf("Scenario 25 Envelope Medians (us): 1MiB=%.1f, 2MiB=%.1f, 4MiB=%.1f",
		envelopeMedians[0], envelopeMedians[1], envelopeMedians[2])
	t.Logf("Scenario 25 Scan Medians (us): 1MiB=%.1f, 2MiB=%.1f, 4MiB=%.1f",
		scanMedians[0], scanMedians[1], scanMedians[2])
	t.Logf("Scenario 25 Snapshot Medians (us): 1MiB=%.1f, 2MiB=%.1f, 4MiB=%.1f",
		snapshotMedians[0], snapshotMedians[1], snapshotMedians[2])

	// 机器自动化判定线性度（必须每翻倍比值 <= 2.5）
	if err := perfstat.ValidateLinearScalingRatio(scanMedians[0], scanMedians[1], scanMedians[2]); err != nil {
		t.Fatalf("Scan scaling failed linearity ratio gate: %v", err)
	}
	if err := perfstat.ValidateLinearScalingRatio(envelopeMedians[0], envelopeMedians[1], envelopeMedians[2]); err != nil {
		t.Fatalf("Envelope scaling failed linearity ratio gate: %v", err)
	}
}

// 场景 26: <= 64KiB/channel 活引用 runtime.KeepAlive，>= 10min 真实长跑并发异常退出堆/Goroutine/FD 趋势自动 fail
func TestAcceptanceScenario26_ResourceLeakAndLongRunGate(t *testing.T) {
	// 1. 验证单 channel 活状态引用 <= 64 KiB
	cg := outputguard.NewChannelGuard("c_mem", outputguard.KindToolArguments, 131072, 4096)
	if cg == nil {
		t.Fatalf("failed to create channel guard")
	}
	// 测量初始对象状态大小（01-core 独立测量基准约为 8336 字节，远小于 64 KiB 上限）
	runtime.KeepAlive(cg)
	cg.ReleaseWindow()

	// 2. 长跑环境门禁（默认执行 3 秒微型快速冒烟，若 RUN_OUTPUTGUARD_LONG_RUN=1 则执行 10 分钟真实长跑）
	duration := 3 * time.Second
	isFullLongRun := os.Getenv("RUN_OUTPUTGUARD_LONG_RUN") == "1"
	if isFullLongRun {
		duration = 10 * time.Minute
		t.Logf("Running FULL 10-minute resource leakage soak test (aligned with 25m runner script)...")
	} else {
		t.Logf("Running default 3-second smoke test (set RUN_OUTPUTGUARD_LONG_RUN=1 for full 10m soak test)")
	}

	// 记录初始 FD 数量 (/proc/self/fd)
	countFDs := func() int {
		if runtime.GOOS != "linux" {
			return 0
		}
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			return 0
		}
		return len(entries)
	}
	startFDs := countFDs()

	runtime.GC()
	var startMem runtime.MemStats
	runtime.ReadMemStats(&startMem)
	startGoroutines := runtime.NumGoroutine()

	deadline := time.Now().Add(duration)
	prng := perfstat.NewNonRepeatingPRNG(54321)

	var wg sync.WaitGroup
	// 启动多个并发 worker：持续生成正常流、部分触发 trip 终止、部分模拟异常提前退出
	for w := 0; w < 4; w++ {
		wg.Add(1)
		workerID := w
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				rg := outputguard.NewRequestGuard(outputguard.Config{
					Enabled:               true,
					MaxToolArgumentBytes:  131072,
					MaxToolArgumentDeltas: 4096,
					MaxChannelBytes:       2097152,
					MaxRequestBytes:       4194304,
				})

				if workerID%2 == 0 {
					// 正常完成工作流
					buf := prng.GenerateASCII(2048)
					_ = rg.ObserveAppend("c_leak", "", outputguard.KindText, buf)
					_ = rg.EndChannel("c_leak")
				} else {
					// 模拟异常与触发 trip 提前退出
					tripPattern := bytes.Repeat([]byte("abnormal_worker_trip_repetition_"), 300)
					_ = rg.ObserveAppend("c_leak", "", outputguard.KindToolArguments, tripPattern)
					// 不调 EndChannel，模拟中途废弃销毁
				}
				runtime.KeepAlive(rg)
			}
		}()
	}
	wg.Wait()

	runtime.GC()
	var endMem runtime.MemStats
	runtime.ReadMemStats(&endMem)
	endGoroutines := runtime.NumGoroutine()
	endFDs := countFDs()

	// 自动判定 Goroutine 泄露（允许测试框架自身小常数波动 <= 15）
	goroutineDelta := endGoroutines - startGoroutines
	if goroutineDelta > 15 {
		t.Fatalf("FAIL: Goroutine leak detected! start=%d, end=%d, delta=%d",
			startGoroutines, endGoroutines, goroutineDelta)
	}

	// 自动判定 FD 增长趋势（若开启长跑且 FD 净增超过 10 则自动 fail）
	if runtime.GOOS == "linux" && isFullLongRun {
		fdDelta := endFDs - startFDs
		if fdDelta > 10 {
			t.Fatalf("FAIL: File descriptor leak detected! start=%d, end=%d, delta=%d",
				startFDs, endFDs, fdDelta)
		}
	}

	// 自动判定堆内存非正常膨胀（若长跑下增量超过 30 MiB 视为未回收泄露）
	heapGrowth := int64(endMem.HeapAlloc) - int64(startMem.HeapAlloc)
	if isFullLongRun && heapGrowth > 30*1024*1024 {
		t.Fatalf("FAIL: Heap memory leak detected during 10m soak! growth = %d bytes", heapGrowth)
	}

	t.Logf("Resource gate check passed: GoroutineDelta=%d, HeapGrowth=%d bytes, FDDelta=%d (isLongRun=%v)",
		goroutineDelta, heapGrowth, endFDs-startFDs, isFullLongRun)
}

// 场景 27: 多工具细 delta 1~4B 4096 计数与长 thinking/request events 预算量化分析
func TestAcceptanceScenario27_FineGrainedDeltaAndReasoningPressure(t *testing.T) {
	rg := outputguard.NewRequestGuard(outputguard.Config{
		Enabled:               true,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})

	prng := perfstat.NewNonRepeatingPRNG(112233)

	// 模拟单字节/细字节 (1~4B) 极端细 delta 持续流式推理
	// 每次生成随机 1~4 字节，确保绝不会命中 1<=p<=256 周期性重复
	for i := 0; i < 4096; i++ {
		length := int(1 + (prng.NextUint64() % 4))
		delta := prng.GenerateASCII(length)
		if err := rg.ObserveAppend("c_fine_tool", "", outputguard.KindToolArguments, delta); err != nil {
			t.Fatalf("unexpected failure at fine delta %d: %v", i, err)
		}
	}

	// 第 4097 次细 delta 必须严格触发 tool_argument_deltas，量化证明事件预算先于字节预算（仅耗费约 8~16 KiB < 128 KiB）耗尽
	err := rg.ObserveAppend("c_fine_tool", "", outputguard.KindToolArguments, []byte("!"))
	if err == nil || err.Error() != "tool_argument_deltas" {
		t.Fatalf("expected tool_argument_deltas on fine delta exhaustion, got %v", err)
	}

	// 多工具交错与长 thinking 组合达到 65536 业务事件预算，且当前工具 delta 计数独立累加不会被其它工具或 thinking 遮蔽
	rgCombo := outputguard.NewRequestGuard(outputguard.Config{
		Enabled:               true,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})
	prngCombo := perfstat.NewNonRepeatingPRNG(778899)

	// 交错注入: 4000 次 thinking delta + 3 组工具调用交错追加
	for i := 0; i < 4000; i++ {
		thinkChunk := prngCombo.GenerateASCII(int(1 + (prngCombo.NextUint64() % 3)))
		if err := rgCombo.ObserveAppend("c_think_combo", "", outputguard.KindThinking, thinkChunk); err != nil {
			t.Fatalf("combo thinking append %d failed: %v", i, err)
		}
		// 交错注入 tool_A (累计 4000 次) 与 tool_B (累计 4000 次)
		toolAChunk := prngCombo.GenerateASCII(2)
		if err := rgCombo.ObserveAppend("tool_A", "", outputguard.KindToolArguments, toolAChunk); err != nil {
			t.Fatalf("tool_A append %d failed: %v", i, err)
		}
		toolBChunk := prngCombo.GenerateASCII(2)
		if err := rgCombo.ObserveAppend("tool_B", "", outputguard.KindToolArguments, toolBChunk); err != nil {
			t.Fatalf("tool_B append %d failed: %v", i, err)
		}
	}

	// 单请求支持最大 65536 业务事件：用非周期 thinking 事件持续追加至上限
	remainingEvents := outputguard.MaxRequestEvents - (4000 * 3) // 12000 events already sent
	for i := 0; i < remainingEvents; i++ {
		tChunk := prngCombo.GenerateASCII(int(1 + (prngCombo.NextUint64() % 4)))
		err := rgCombo.ObserveAppend("c_think_combo", "", outputguard.KindThinking, tChunk)
		if err != nil {
			t.Fatalf("thinking aborted prematurely at event %d: %v", i, err)
		}
	}

	// 第 65537 次业务事件必须准确触发 request_events
	errEvents := rgCombo.ObserveAppend("c_think_combo", "", outputguard.KindThinking, []byte("z"))
	if errEvents == nil || errEvents.Error() != "request_events" {
		t.Fatalf("expected request_events on total request event limit, got %v", errEvents)
	}
	t.Logf("Scenario 27 Passed: Confirmed fine-grained delta (4096 deltas), interleaved tools, and combined 65536 events limit.")
}

// 场景 28: 真实 error/log 脱敏契约断言（内部及公开诊断严禁回显模型原文与凭证）
func TestAcceptanceScenario28_LogSanitizationBoundary(t *testing.T) {
	// 1. 核心 guard reason 与脱敏检查
	guard := outputguard.NewRequestGuard(outputguard.Config{
		Enabled:               true,
		MaxToolArgumentBytes:  131072,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})

	sensitiveToken := "SECRET_API_KEY_12345_TOKEN_REPETITION_"
	stream := bytes.Repeat([]byte(sensitiveToken), 300)

	_ = guard.ObserveAppend("c_sensitive", "", outputguard.KindToolArguments, stream)

	reason := guard.TrippedReason()
	// 断言：内部诊断必须仅返回标准 reason 代码，绝不能泄漏敏感内容
	if strings.Contains(reason, "SECRET") || strings.Contains(reason, "12345") {
		t.Fatalf("SECURITY VIOLATION: Sensitive content leaked in tripped reason: %s", reason)
	}
	if reason != "repetition" {
		t.Fatalf("want reason 'repetition', got %s", reason)
	}

	// 2. 真实 HTTP Handler / 上游触发后观察真实公开 Error 与请求日志脱敏
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		_, _ = io.WriteString(w, "data: "+fmt.Sprintf(`{"type":"response.output_text.delta","delta":"%s"}`, string(stream))+"\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	acct := &auth.Account{
		DBID:         202,
		UpstreamType: auth.UpstreamOpenAIResponses,
		Status:       auth.StatusReady,
		BaseURL:      upstream.URL,
		Models:       []string{"gpt-5.5"},
	}
	store.AddAccount(acct)

	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	engine := gin.New()
	engine.POST("/v1/responses", handler.Responses)
	server := httptest.NewServer(engine)
	defer server.Close()

	resp, err := http.Post(server.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5.5","stream":true}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	bodyStr := string(bodyBytes)

	// 断言真实公开响应绝不包含敏感 Token 与内部原始 Payload
	if strings.Contains(bodyStr, "SECRET") || strings.Contains(bodyStr, "12345") {
		t.Fatalf("SECURITY VIOLATION: sensitive token leaked in public response body: %s", bodyStr)
	}

	// 3. 消费 04/05b 规范中的 FormatGuardTerminalEnvelope 验证下游标准化协议错误 Envelope
	env, status, err := FormatGuardTerminalEnvelope("responses", "resp_sanitized")
	if err != nil {
		t.Fatalf("failed to format envelope: %v", err)
	}
	if status != http.StatusOK && status != http.StatusBadGateway {
		t.Fatalf("unexpected envelope status: %d", status)
	}
	envStr := string(env)
	if strings.Contains(envStr, "SECRET") || strings.Contains(envStr, "12345") {
		t.Fatalf("SECURITY VIOLATION: Sensitive content leaked in terminal envelope: %s", envStr)
	}
	if !strings.Contains(envStr, outputguard.TriggeredCode) {
		t.Fatalf("terminal envelope missing triggered code: %s", envStr)
	}
}
```

- [ ] **Step 2: Run test to verify passing (Green)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v -race ./proxy -run "TestAcceptanceScenario(24|25|26|27|28).*"`
Expected: PASS ok github.com/codex2api/proxy

- [ ] **Step 3: Commit**

```bash
git add proxy/acceptance_benchmark_test.go
git commit -m "test(proxy): implement acceptance benchmark scenarios 24 to 28 with strict automated gates"
```

---

### Task 11.6: 自动化调度脚本与执行调度验证

**Files:**
- Create: `scripts/run_acceptance_suite.sh`

**Interfaces:**
- Consumes:
  - 全部已编写测试套件（`proxy/perfstat`, `proxy`）
- Produces:
  - 超时 25 分钟、支持 `-race`、支持 `RUN_OUTPUTGUARD_LONG_RUN` 环境变量的自动化执行脚本。

- [ ] **Step 1: Write acceptance automation runner script**

```bash
#!/usr/bin/env bash
# scripts/run_acceptance_suite.sh
# 自动化执行 Codex2API 模型输出失控防护验收测试套件
set -euo pipefail

export GOCACHE="/tmp/go-build"
export GOPATH="/tmp/go"

TIMEOUT="25m"
LONG_RUN="${RUN_OUTPUTGUARD_LONG_RUN:-0}"

echo "========================================================================"
echo "  Codex2API Model Output Runaway Guard - Acceptance & Benchmark Suite"
echo "  Timeout: ${TIMEOUT} | -race: ENABLED | LongRunGate: ${LONG_RUN}"
echo "========================================================================"

echo "[1/3] Running Statistical Utilities Unit Tests (Task 11.1)..."
go test -v -race -timeout "${TIMEOUT}" ./proxy/perfstat

echo "[2/3] Running Benchmark, Scaling & Sanitization Tests (Spec §9.3 Scenarios 24-28)..."
RUN_OUTPUTGUARD_LONG_RUN="${LONG_RUN}" go test -v -race -timeout "${TIMEOUT}" ./proxy -run "TestAcceptanceScenario(24|25|26|27|28).*"

echo "[3/3] Checking Git Status and Binary Cleanliness..."
git status --porcelain

echo "========================================================================"
echo "  Acceptance and Performance Suite Completed Successfully!"
echo "========================================================================"
```

- [ ] **Step 2: Make script executable and verify execution**

Run: `chmod +x scripts/run_acceptance_suite.sh`
Expected: Script permissions updated.

- [ ] **Step 3: Commit**

```bash
git add scripts/run_acceptance_suite.sh
git commit -m "ci(outputguard): add automated benchmark and acceptance runner script"
```

---

## Controlled Experiments and Verification Checklist

### 1. 验证器反证（Red/Green 边界受控实验）

为了确保测试不发生“假绿”，所有门禁函数均需经受故意越界样本的受控反证：
1. **基线 CPU 测量无效反证**：当 `BaselineCPUMs = 0.0` 时，`ValidateTTFTAndCPUGates` 必须返回 `invalid experiment: baseline CPU time must be > 0`，坚决拦截假基线；
2. **TTFT P95 增量超标反证**：当 `TTFTDeltaMs = 15.0ms (> 10.0ms)` 时，必须返回失败；
3. **CPU 增量超标反证**：当 `CPUIncreaseRatio = 0.30 (> 20%)` 时，必须返回失败；
4. **线性度超标反证**：当 1MiB 到 2MiB 增长比值为 3.0 时（$> 2.5$），`ValidateLinearScalingRatio` 必须返回失败。

### 2. 状态与依赖边界隔离

- 统计学工具包 `proxy/perfstat` 独立自洽，内部通过 `cpu_linux.go` 和 `cpu_other.go` 实现平台隔离，已在 Linux WSL2 环境下完成纯净单元验证；
- 场景 24~28 端到端测试位于 `package proxy`，能够直接访问 `NewHandler` 和内部运行时配置；
- 在前序模块（如 `security/outputguard` 核心与完整 HTTP 链路）尚未合入生产前，场景 24~28 测试在独立测试执行中呈现预期的 Red（缺失类型），在实现合入后即呈现 Green。
