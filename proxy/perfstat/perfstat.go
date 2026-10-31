// proxy/perfstat/perfstat.go
package perfstat

import (
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
