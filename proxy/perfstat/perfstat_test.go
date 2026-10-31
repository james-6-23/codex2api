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
