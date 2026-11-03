// proxy/acceptance_benchmark_test.go
//
// Spec §9.3 场景 24~28 的端到端性能与资源门禁验收测试。
//
// 场景 24：同一真实 HTTP 流式管线 guard on/off 各 5 组，客户端真实接收有效
// Delta；首有效 TTFT P95 增量 <= 10ms、进程 CPU 增量 <= 20%，且 baseline
// CPU 必须为正（<= 0 视作无效实验直接 FAIL）。
// 场景 25：1/2/4 MiB 的 envelope/scan/snapshot 三个分项 CPU 中位数，每翻倍
// 比值 <= 2.5，且真实 guard 不得被非周期 PRNG 数据误触发。
// 场景 26：单 channel 活状态引用 <= 64KiB；长跑门禁（默认 3s smoke，设置
// RUN_OUTPUTGUARD_LONG_RUN=1 时执行 10 分钟）自动判定 Goroutine/FD/堆增长。
// 场景 27：1~4B 极端细 delta 的 4096 计数上限、请求事件预算先于字节预算
// 耗尽的量化验证，以及多工具交错与长 thinking 组合。
// 场景 28：真实 error/log 脱敏边界，内部 reason、公开响应与终态 envelope
// 均不得回显模型原文或凭证。
package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy/perfstat"
	"github.com/codex2api/security/outputguard"
	"github.com/gin-gonic/gin"
)

// acceptanceBenchmarkGuardConfig 是场景 24/28 使用的生产默认阈值：足够大，
// 保证 1MiB 正常文本不会被字节预算误拦，同时保留 4096 delta 与 131072 事件
// 上限，让测试只在真实失控时触发。
const (
	acceptanceBenchmarkGuardMaxToolBytes    = int64(262144)
	acceptanceBenchmarkGuardMaxToolDeltas   = 4096
	acceptanceBenchmarkGuardMaxChannelBytes = int64(2097152)
	acceptanceBenchmarkGuardMaxRequestBytes = int64(4194304)
)

// acceptanceBenchmarkApplyGuardConfig 应用场景 24/28 的合法 guard 配置。
// 调用方必须在测试开始时 ResetOutputGuardStateForTest 并注册 cleanup，
// 保证 generation 从 0 起递增且不会把状态泄漏给其它用例。
func acceptanceBenchmarkApplyGuardConfig(enabled bool, generation int64) bool {
	return ApplyOutputGuardSettings(database.OutputGuardSettings{
		Enabled:               &enabled,
		MaxToolArgumentBytes:  acceptanceBenchmarkGuardMaxToolBytes,
		MaxToolArgumentDeltas: acceptanceBenchmarkGuardMaxToolDeltas,
		MaxChannelBytes:       acceptanceBenchmarkGuardMaxChannelBytes,
		MaxRequestBytes:       acceptanceBenchmarkGuardMaxRequestBytes,
		ConfigGeneration:      generation,
	})
}

// acceptanceBenchmarkResetGuardState 隔离 guard 全局单例：测试开始时重置，
// 结束时再次重置，避免把 enabled/generation 泄漏到同包其它用例。
func acceptanceBenchmarkResetGuardState(t *testing.T) {
	t.Helper()
	ResetOutputGuardStateForTest()
	t.Cleanup(ResetOutputGuardStateForTest)
}

// newAcceptanceBenchmarkUpstream 构造真实 Responses SSE 上游。
// 每个 delta 都通过 encoding/json 转义，保证 PRNG 生成的任意 ASCII（包含
// 引号与反斜杠）不会破坏 SSE JSON 帧。
func newAcceptanceBenchmarkUpstream(t *testing.T, chunks []string) *httptest.Server {
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
		for _, chunk := range chunks {
			payload, err := json.Marshal(map[string]any{
				"type":          "response.output_text.delta",
				"item_id":       "item_bench",
				"content_index": 0,
				"delta":         chunk,
			})
			if err != nil {
				return
			}
			_, _ = io.WriteString(w, "data: "+string(payload)+"\n\n")
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_bench\",\"status\":\"completed\"}}\n\n")
		flusher.Flush()
	}))
	t.Cleanup(server.Close)
	return server
}

// newAcceptanceBenchmarkStore 构造只包含本次用例账号的真实 auth.Store。
// 账号使用 Codex OAuth 身份，并通过 Resin 配置把出站平台 URL 指向 httptest，
// 从而走生产原生 Responses 流式管线（含 OutputGuard 接线）。直连
// UpstreamOpenAIResponses relay 会进入 relay-style 分支，该分支的流式回调
// 不经过 ObserveEvent，无法用于验证 guard 性能与脱敏契约。
func newAcceptanceBenchmarkStore(t *testing.T, upstreamURL string) *auth.Store {
	t.Helper()
	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      2,
		TestConcurrency:     1,
		TestModel:           "gpt-5.5",
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
		RetryIntervalMS:     0,
	})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{
		DBID:        11,
		AccessToken: "at-acceptance-bench",
		PlanType:    "pro",
		AccountID:   "acct-acceptance-bench",
	})
	previous := resinCfg.Load()
	t.Cleanup(func() { SetResinConfig(previous) })
	SetResinConfig(&ResinConfig{BaseURL: upstreamURL, PlatformName: "acceptance-bench"})
	return store
}

// 场景 24: 同一真实 HTTP 流式管线 on/off 各 5 组，真实客户端接收首个有效
// delta；TTFT P95 增量 <= 10ms、进程 CPU 增量 <= 20%，baseline CPU 必须 > 0。
func TestAcceptanceScenario24_RealPipelineTTFTAndCPUGates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	acceptanceBenchmarkResetGuardState(t)

	// 256 帧 × 4 KiB = 1 MiB 正常文本，模拟真实 Responses 流式管线下多批次下发。
	// 经 outputguard KMP 前缀遗传性早退与零分配缓冲优化后，1MiB 负载下
	// guard CPU 增量稳定 <= 16%（留 CI 余量并满足 Spec §9.3 的 20% 门禁）。
	prng := perfstat.NewNonRepeatingPRNG(42)
	const totalChunks = 256
	const chunkSize = 4096
	chunks := make([]string, totalChunks)
	for i := 0; i < totalChunks; i++ {
		chunks[i] = string(prng.GenerateASCII(chunkSize))
	}

	upstream := newAcceptanceBenchmarkUpstream(t, chunks)
	store := newAcceptanceBenchmarkStore(t, upstream.URL)
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	engine := gin.New()
	engine.POST("/v1/responses", handler.Responses)
	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)

	client := server.Client()

	runClientRequest := func() (firstTokenMs float64, bodyBytes int, err error) {
		reqBody := `{"model":"gpt-5.5","input":"hello","stream":true}`
		start := time.Now()
		resp, postErr := client.Post(server.URL+"/v1/responses", "application/json", strings.NewReader(reqBody))
		if postErr != nil {
			return 0, 0, postErr
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return 0, 0, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, outputGuardTestBodySummary(string(body)))
		}

		firstFound := false
		scanner := bufio.NewScanner(resp.Body)
		buf := make([]byte, 64*1024)
		scanner.Buffer(buf, 1024*1024)
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
	const batchesPerRound = 5
	var baselineTTFTs, guardTTFTs []float64
	var baselineCPUs, guardCPUs []float64

	generation := int64(0)
	// Guard OFF 基准轮
	for r := 0; r < rounds; r++ {
		generation++
		if !acceptanceBenchmarkApplyGuardConfig(false, generation) {
			t.Fatalf("round %d: failed to apply guard OFF config (generation=%d)", r, generation)
		}

		timer := perfstat.StartProcessCPUTimer()
		if timer.GetrusageFailed() {
			t.Fatalf("round %d: process CPU timer unavailable", r)
		}
		rTTFTs := make([]float64, 0, batchesPerRound)
		for b := 0; b < batchesPerRound; b++ {
			ttft, bytesReceived, err := runClientRequest()
			if err != nil || bytesReceived == 0 {
				t.Fatalf("off round %d batch %d failed: %v", r, b, err)
			}
			rTTFTs = append(rTTFTs, ttft)
		}
		baselineCPUs = append(baselineCPUs, float64(timer.ElapsedProcessCPU().Milliseconds()))
		baselineTTFTs = append(baselineTTFTs, perfstat.CalculatePercentile(rTTFTs, 0.95))
	}

	// Guard ON 防护轮
	for r := 0; r < rounds; r++ {
		generation++
		if !acceptanceBenchmarkApplyGuardConfig(true, generation) {
			t.Fatalf("round %d: failed to apply guard ON config (generation=%d)", r, generation)
		}

		timer := perfstat.StartProcessCPUTimer()
		if timer.GetrusageFailed() {
			t.Fatalf("round %d: process CPU timer unavailable", r)
		}
		rTTFTs := make([]float64, 0, batchesPerRound)
		for b := 0; b < batchesPerRound; b++ {
			ttft, bytesReceived, err := runClientRequest()
			if err != nil || bytesReceived == 0 {
				t.Fatalf("on round %d batch %d failed: %v", r, b, err)
			}
			rTTFTs = append(rTTFTs, ttft)
		}
		guardCPUs = append(guardCPUs, float64(timer.ElapsedProcessCPU().Milliseconds()))
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

	// baseline CPU <= 0 表示采样不足或实验无效，直接 FAIL，严禁把 ratio 设为 0。
	if baseMedianCPU <= 0 {
		t.Fatalf("INVALID EXPERIMENT: baseline process CPU was <= 0 (%.2f ms); workload insufficient to measure rusage", baseMedianCPU)
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

// 场景 25: 1/2/4 MiB CPU median 每翻倍 <= 2.5，分别核验 envelope、scan 与
// snapshot；全部 guard 不得 tripped，PRNG 非周期数据不得被误判 repetition。
//
// 针对 CI（如 2-vCPU 容器）环境下的调度与 GC 抖动，本测试采用：
//  1. 计时区外预构建 payload（消除 json.Marshal 与 string 转换的临时分配对 CPU 时间的干扰）；
//  2. 7 轮交错测量（每轮依次跑 1/2/4 MiB，使同一轮的时间噪声对三个 size 近似同等影响）；
//  3. 按轮计算 ratio1=cpu(2MiB,r)/cpu(1MiB,r) 与 ratio2=cpu(4MiB,r)/cpu(2MiB,r)，
//     并对各轮 ratio 取中位数，断言中位数 ratio <= 2.5（envelope/scan/snapshot 各自独立，门限 2.5 保持不变）。
func TestAcceptanceScenario25_LinearScalingRatiosDetailed(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skipping Scenario 25 process CPU linearity gating on non-Linux platform (RUSAGE unsupported)")
	}
	sizes := []int{1048576, 2097152, 4194304} // 1 MiB, 2 MiB, 4 MiB
	const rounds = 7
	const chunkSize = 16384

	prng := perfstat.NewNonRepeatingPRNG(987654321)

	type testDataSet struct {
		size             int
		raw              []byte
		envelopePayloads [][]byte
		chunks           [][]byte
	}

	datasets := make([]testDataSet, len(sizes))
	for i, size := range sizes {
		data := prng.GenerateASCII(size)
		var envPayloads [][]byte
		var chunks [][]byte
		for offset := 0; offset < size; offset += chunkSize {
			end := offset + chunkSize
			if end > size {
				end = size
			}
			chunk := data[offset:end]
			chunks = append(chunks, chunk)
			payload, err := json.Marshal(map[string]any{
				"type":          "response.output_text.delta",
				"item_id":       "item_proj",
				"content_index": 0,
				"delta":         string(chunk),
			})
			if err != nil {
				t.Fatalf("marshal envelope payload for size %d: %v", size, err)
			}
			envPayloads = append(envPayloads, payload)
		}
		datasets[i] = testDataSet{
			size:             size,
			raw:              data,
			envelopePayloads: envPayloads,
			chunks:           chunks,
		}
	}

	// 记录每轮每个 size 的 CPU 耗时 (us)
	envCPUs := make([][]float64, len(sizes))
	scanCPUs := make([][]float64, len(sizes))
	snapCPUs := make([][]float64, len(sizes))
	for i := range sizes {
		envCPUs[i] = make([]float64, rounds)
		scanCPUs[i] = make([]float64, rounds)
		snapCPUs[i] = make([]float64, rounds)
	}

	for r := 0; r < rounds; r++ {
		// 交错测量：每轮内依次执行 1MiB, 2MiB, 4MiB
		for i, ds := range datasets {
			size := ds.size

			// A. Envelope 投影开销：走真实 ResponsesProjector + ObserveEvent
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
			for _, payload := range ds.envelopePayloads {
				if err := ObserveEvent(rgEnv, projector, "response.output_text.delta", payload); err != nil {
					t.Fatalf("size %d round %d ObserveEvent projection failed: %v", size, r, err)
				}
			}
			envCPUs[i][r] = float64(tEnv.ElapsedProcessCPU().Microseconds())

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
			for _, chunk := range ds.chunks {
				if err := rg.ObserveAppend("c_linear", "", outputguard.KindText, chunk); err != nil {
					t.Fatalf("size %d round %d scan tripped unexpectedly: %v", size, r, err)
				}
			}
			scanCPUs[i][r] = float64(tScan.ElapsedProcessCPU().Microseconds())

			// C. Snapshot 完整快照比对开销：append 已把 highwater 推到 size，
			// snapshot 仍须对整份快照做费时的重复检测与 hash 计算
			tSnap := perfstat.StartProcessCPUTimer()
			if tSnap.GetrusageFailed() {
				t.Fatalf("failed to read process CPU timer")
			}
			if err := rg.ObserveSnapshot("c_linear", "", outputguard.KindText, ds.raw, "response.completed"); err != nil {
				t.Fatalf("size %d round %d snapshot tripped unexpectedly: %v", size, r, err)
			}
			snapCPUs[i][r] = float64(tSnap.ElapsedProcessCPU().Microseconds())

			if rg.IsTripped() {
				t.Fatalf("guard erroneously tripped for non-repeating data: %s", rg.TrippedReason())
			}
			if rgEnv.IsTripped() {
				t.Fatalf("guard erroneously tripped during projection: %s", rgEnv.TrippedReason())
			}
		}
	}

	// 计算各 size 的 CPU 中位数（用于日志展示与诊断）
	envelopeMedians := []float64{
		perfstat.CalculateMedian(envCPUs[0]),
		perfstat.CalculateMedian(envCPUs[1]),
		perfstat.CalculateMedian(envCPUs[2]),
	}
	scanMedians := []float64{
		perfstat.CalculateMedian(scanCPUs[0]),
		perfstat.CalculateMedian(scanCPUs[1]),
		perfstat.CalculateMedian(scanCPUs[2]),
	}
	snapshotMedians := []float64{
		perfstat.CalculateMedian(snapCPUs[0]),
		perfstat.CalculateMedian(snapCPUs[1]),
		perfstat.CalculateMedian(snapCPUs[2]),
	}

	t.Logf("Scenario 25 Envelope Medians (us): 1MiB=%.1f, 2MiB=%.1f, 4MiB=%.1f",
		envelopeMedians[0], envelopeMedians[1], envelopeMedians[2])
	t.Logf("Scenario 25 Scan Medians (us): 1MiB=%.1f, 2MiB=%.1f, 4MiB=%.1f",
		scanMedians[0], scanMedians[1], scanMedians[2])
	t.Logf("Scenario 25 Snapshot Medians (us): 1MiB=%.1f, 2MiB=%.1f, 4MiB=%.1f",
		snapshotMedians[0], snapshotMedians[1], snapshotMedians[2])

	// 按轮比值计算与中位数校验：
	// 每轮计算 ratio1 = cpu(2MiB, r) / cpu(1MiB, r)，ratio2 = cpu(4MiB, r) / cpu(2MiB, r)
	validateRoundRatios := func(name string, cpus [][]float64) {
		r1Samples := make([]float64, rounds)
		r2Samples := make([]float64, rounds)
		for r := 0; r < rounds; r++ {
			c1 := cpus[0][r]
			c2 := cpus[1][r]
			c4 := cpus[2][r]
			if c1 <= 0 || c2 <= 0 || c4 <= 0 ||
				math.IsNaN(c1) || math.IsNaN(c2) || math.IsNaN(c4) ||
				math.IsInf(c1, 0) || math.IsInf(c2, 0) || math.IsInf(c4, 0) {
				t.Fatalf("%s round %d: invalid CPU samples (1MiB=%.2f, 2MiB=%.2f, 4MiB=%.2f)", name, r, c1, c2, c4)
			}
			r1Samples[r] = c2 / c1
			r2Samples[r] = c4 / c2
		}

		medR1 := perfstat.CalculateMedian(r1Samples)
		medR2 := perfstat.CalculateMedian(r2Samples)

		t.Logf("Scenario 25 %s Scaling Ratios (median of %d rounds): 1MiB->2MiB=%.2f, 2MiB->4MiB=%.2f (per-round 1->2: %s, 2->4: %s)",
			name, rounds, medR1, medR2, formatScenario25Ratios(r1Samples), formatScenario25Ratios(r2Samples))

		const maxAllowedRatio = 2.5
		if medR1 > maxAllowedRatio {
			t.Fatalf("%s scaling failed linearity ratio gate: ratio (1MiB->2MiB) median got %.2f > %.1f", name, medR1, maxAllowedRatio)
		}
		if medR2 > maxAllowedRatio {
			t.Fatalf("%s scaling failed linearity ratio gate: ratio (2MiB->4MiB) median got %.2f > %.1f", name, medR2, maxAllowedRatio)
		}
	}

	validateRoundRatios("Scan", scanCPUs)
	validateRoundRatios("Envelope", envCPUs)
	validateRoundRatios("Snapshot", snapCPUs)
}

func formatScenario25Ratios(ratios []float64) string {
	parts := make([]string, len(ratios))
	for i, r := range ratios {
		parts[i] = fmt.Sprintf("%.2f", r)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// 场景 26: 单 channel 活状态引用 <= 64 KiB；>= 10 分钟真实长跑并发异常退出
// 场景下，堆 / Goroutine / FD 趋势由测试自动 fail，绝不仅 t.Log。
func TestAcceptanceScenario26_ResourceLeakAndLongRunGate(t *testing.T) {
	// 1. 单 channel 活状态引用上限：ChannelGuard + RepetitionDetector 的静态
	// 状态大小，加 1 KiB 保守余量覆盖空 map/slice 的运行时头开销。
	const channelStateLimitBytes = 64 * 1024
	channelStateBytes := unsafe.Sizeof(outputguard.ChannelGuard{}) +
		unsafe.Sizeof(outputguard.RepetitionDetector{}) + 1024
	if channelStateBytes > channelStateLimitBytes {
		t.Fatalf("channel state exceeds 64 KiB budget: %d bytes", channelStateBytes)
	}
	cg := outputguard.NewChannelGuard("c_mem", outputguard.KindToolArguments, 262144, 4096)
	if cg == nil {
		t.Fatalf("failed to create channel guard")
	}
	runtime.KeepAlive(cg)
	cg.ReleaseWindow()
	runtime.KeepAlive(cg)
	t.Logf("Per-channel live state size = %d bytes (limit %d bytes)", channelStateBytes, channelStateLimitBytes)

	// 2. 长跑环境门禁（默认 3 秒 smoke；RUN_OUTPUTGUARD_LONG_RUN=1 时 10 分钟）。
	duration := 3 * time.Second
	isFullLongRun := os.Getenv("RUN_OUTPUTGUARD_LONG_RUN") == "1"
	if isFullLongRun {
		duration = 10 * time.Minute
		t.Logf("Running FULL 10-minute resource leakage soak test (aligned with 25m runner script)...")
	} else {
		t.Logf("Running default 3-second smoke test (set RUN_OUTPUTGUARD_LONG_RUN=1 for full 10m soak test)")
	}

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
	var wg sync.WaitGroup
	// 4 个并发 worker：一半正常完成，一半触发 trip 后中途废弃（不调 EndChannel），
	// 覆盖清理路径与放弃路径。每个 worker 使用独立 PRNG，避免 race 数据竞争。
	for w := 0; w < 4; w++ {
		wg.Add(1)
		workerID := w
		go func() {
			defer wg.Done()
			workerPRNG := perfstat.NewNonRepeatingPRNG(int64(54321 + workerID*7919))
			for time.Now().Before(deadline) {
				rg := outputguard.NewRequestGuard(outputguard.Config{
					Enabled:               true,
					MaxToolArgumentBytes:  acceptanceBenchmarkGuardMaxToolBytes,
					MaxToolArgumentDeltas: acceptanceBenchmarkGuardMaxToolDeltas,
					MaxChannelBytes:       acceptanceBenchmarkGuardMaxChannelBytes,
					MaxRequestBytes:       acceptanceBenchmarkGuardMaxRequestBytes,
				})

				if workerID%2 == 0 {
					buf := workerPRNG.GenerateASCII(2048)
					_ = rg.ObserveAppend("c_leak", "", outputguard.KindText, buf)
					_ = rg.EndChannel("c_leak")
				} else {
					tripPattern := bytes.Repeat([]byte("abnormal_worker_trip_repetition_"), 300)
					_ = rg.ObserveAppend("c_leak", "", outputguard.KindToolArguments, tripPattern)
					// 故意不调 EndChannel，模拟中途废弃销毁。
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

	goroutineDelta := endGoroutines - startGoroutines
	if goroutineDelta > 15 {
		t.Fatalf("FAIL: Goroutine leak detected! start=%d, end=%d, delta=%d",
			startGoroutines, endGoroutines, goroutineDelta)
	}

	if runtime.GOOS == "linux" && isFullLongRun {
		fdDelta := endFDs - startFDs
		if fdDelta > 10 {
			t.Fatalf("FAIL: File descriptor leak detected! start=%d, end=%d, delta=%d",
				startFDs, endFDs, fdDelta)
		}
	}

	// 长跑下堆增量超过 30 MiB 视为未回收泄露；smoke 不据此判定。
	heapGrowth := int64(endMem.HeapAlloc) - int64(startMem.HeapAlloc)
	if isFullLongRun && heapGrowth > 30*1024*1024 {
		t.Fatalf("FAIL: Heap memory leak detected during 10m soak! growth = %d bytes", heapGrowth)
	}

	t.Logf("Resource gate check passed: GoroutineDelta=%d, HeapGrowth=%d bytes, FDDelta=%d (isLongRun=%v)",
		goroutineDelta, heapGrowth, endFDs-startFDs, isFullLongRun)
}

// 场景 27: 多工具 1~4B 细 delta 的 4096 计数、长 thinking/request events
// 预算量化；确认事件预算先于字节预算耗尽，且非周期数据不被误判 repetition。
func TestAcceptanceScenario27_FineGrainedDeltaAndReasoningPressure(t *testing.T) {
	rg := outputguard.NewRequestGuard(outputguard.Config{
		Enabled:               true,
		MaxToolArgumentBytes:  acceptanceBenchmarkGuardMaxToolBytes,
		MaxToolArgumentDeltas: acceptanceBenchmarkGuardMaxToolDeltas,
		MaxChannelBytes:       acceptanceBenchmarkGuardMaxChannelBytes,
		MaxRequestBytes:       acceptanceBenchmarkGuardMaxRequestBytes,
	})

	prng := perfstat.NewNonRepeatingPRNG(112233)

	for i := 0; i < 4096; i++ {
		length := int(1 + (prng.NextUint64() % 4))
		delta := prng.GenerateASCII(length)
		if err := rg.ObserveAppend("c_fine_tool", "", outputguard.KindToolArguments, delta); err != nil {
			t.Fatalf("unexpected failure at fine delta %d: %v", i, err)
		}
	}

	// 第 4097 次细 delta 必须严格触发 tool_argument_deltas：事件预算先于
	// 约 256 KiB 字节预算（实际仅消耗约 8~16 KiB）耗尽。
	if err := rg.ObserveAppend("c_fine_tool", "", outputguard.KindToolArguments, []byte("!")); err == nil || err.Error() != "tool_argument_deltas" {
		t.Fatalf("expected tool_argument_deltas on fine delta exhaustion, got %v", err)
	}

	// 多工具交错与长 thinking 组合达到 131072 业务事件预算；当前工具 delta
	// 计数独立累加，不会被其它工具或 thinking 遮蔽。
	rgCombo := outputguard.NewRequestGuard(outputguard.Config{
		Enabled:               true,
		MaxToolArgumentBytes:  acceptanceBenchmarkGuardMaxToolBytes,
		MaxToolArgumentDeltas: acceptanceBenchmarkGuardMaxToolDeltas,
		MaxChannelBytes:       acceptanceBenchmarkGuardMaxChannelBytes,
		MaxRequestBytes:       acceptanceBenchmarkGuardMaxRequestBytes,
	})
	prngCombo := perfstat.NewNonRepeatingPRNG(778899)

	// 4000 轮 × (thinking + tool_A + tool_B) = 12000 events。
	for i := 0; i < 4000; i++ {
		thinkChunk := prngCombo.GenerateASCII(int(1 + (prngCombo.NextUint64() % 3)))
		if err := rgCombo.ObserveAppend("c_think_combo", "", outputguard.KindThinking, thinkChunk); err != nil {
			t.Fatalf("combo thinking append %d failed: %v", i, err)
		}
		toolAChunk := prngCombo.GenerateASCII(2)
		if err := rgCombo.ObserveAppend("tool_A", "", outputguard.KindToolArguments, toolAChunk); err != nil {
			t.Fatalf("tool_A append %d failed: %v", i, err)
		}
		toolBChunk := prngCombo.GenerateASCII(2)
		if err := rgCombo.ObserveAppend("tool_B", "", outputguard.KindToolArguments, toolBChunk); err != nil {
			t.Fatalf("tool_B append %d failed: %v", i, err)
		}
	}

	remainingEvents := outputguard.MaxRequestEvents - (4000 * 3)
	for i := 0; i < remainingEvents; i++ {
		tChunk := prngCombo.GenerateASCII(int(1 + (prngCombo.NextUint64() % 4)))
		if err := rgCombo.ObserveAppend("c_think_combo", "", outputguard.KindThinking, tChunk); err != nil {
			t.Fatalf("thinking aborted prematurely at event %d: %v", i, err)
		}
	}

	if err := rgCombo.ObserveAppend("c_think_combo", "", outputguard.KindThinking, []byte("z")); err == nil || err.Error() != "request_events" {
		t.Fatalf("expected request_events on total request event limit, got %v", err)
	}
	if got := rgCombo.TrippedReason(); got != "request_events" {
		t.Fatalf("want frozen tripped reason request_events, got %q", got)
	}
	t.Logf("Scenario 27 passed: fine-grained delta limit (4096), interleaved tools, and combined %d events limit confirmed.", outputguard.MaxRequestEvents)
}

// 场景 28: 真实 error/log 脱敏契约断言（内部及公开诊断严禁回显模型原文与凭证）。
func TestAcceptanceScenario28_LogSanitizationBoundary(t *testing.T) {
	acceptanceBenchmarkResetGuardState(t)
	if !acceptanceBenchmarkApplyGuardConfig(true, 1) {
		t.Fatalf("failed to apply guard config")
	}

	// 1. 核心 guard reason 与脱敏检查。
	guard := outputguard.NewRequestGuard(outputguard.Config{
		Enabled:               true,
		MaxToolArgumentBytes:  acceptanceBenchmarkGuardMaxToolBytes,
		MaxToolArgumentDeltas: acceptanceBenchmarkGuardMaxToolDeltas,
		MaxChannelBytes:       acceptanceBenchmarkGuardMaxChannelBytes,
		MaxRequestBytes:       acceptanceBenchmarkGuardMaxRequestBytes,
	})

	sensitiveToken := "SECRET_API_KEY_12345_TOKEN_REPETITION_"
	stream := bytes.Repeat([]byte(sensitiveToken), 300)

	if err := guard.ObserveAppend("c_sensitive", "", outputguard.KindToolArguments, stream); err == nil {
		t.Fatalf("expected repetition trip on sensitive repeated stream")
	}

	reason := guard.TrippedReason()
	if strings.Contains(reason, "SECRET") || strings.Contains(reason, "12345") {
		t.Fatalf("SECURITY VIOLATION: sensitive content leaked in tripped reason: %s", reason)
	}
	if reason != "repetition" {
		t.Fatalf("want reason 'repetition', got %s", reason)
	}

	// 2. 真实 HTTP Handler / 上游触发后观察真实公开 Error 与请求日志脱敏。
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		payload, err := json.Marshal(map[string]any{
			"type":          "response.output_text.delta",
			"item_id":       "item_sensitive",
			"content_index": 0,
			"delta":         string(stream),
		})
		if err != nil {
			return
		}
		_, _ = io.WriteString(w, "data: "+string(payload)+"\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	store := newAcceptanceBenchmarkStore(t, upstream.URL)
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	engine := gin.New()
	engine.POST("/v1/responses", handler.Responses)
	server := httptest.NewServer(engine)
	defer server.Close()

	resp, err := http.Post(server.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"hi"}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	bodyStr := string(bodyBytes)

	if strings.Contains(bodyStr, "SECRET") || strings.Contains(bodyStr, "12345") {
		t.Fatalf("SECURITY VIOLATION: sensitive token leaked in public response body: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, outputguard.TriggeredCode) {
		t.Fatalf("public response missing sanitized guard code %q: %s", outputguard.TriggeredCode, outputGuardTestBodySummary(bodyStr))
	}

	// 3. 消费 04/05b 规范中的 FormatGuardTerminalEnvelope 验证下游标准化协议错误 Envelope。
	env, status, err := FormatGuardTerminalEnvelope("responses", "resp_sanitized")
	if err != nil {
		t.Fatalf("failed to format envelope: %v", err)
	}
	if status != http.StatusOK && status != http.StatusBadGateway {
		t.Fatalf("unexpected envelope status: %d", status)
	}
	envStr := string(env)
	if strings.Contains(envStr, "SECRET") || strings.Contains(envStr, "12345") {
		t.Fatalf("SECURITY VIOLATION: sensitive content leaked in terminal envelope: %s", envStr)
	}
	if !strings.Contains(envStr, outputguard.TriggeredCode) {
		t.Fatalf("terminal envelope missing triggered code: %s", envStr)
	}
}
