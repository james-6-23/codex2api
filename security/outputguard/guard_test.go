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
		MaxToolArgumentBytes:  262144, // 256 KiB
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
	// 第 131073 个事件必须被拒绝并判定为 request_events (Spec §5.3 步骤 1)
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
		MaxToolArgumentBytes:  262144,
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
		MaxToolArgumentBytes:  262144,
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
	// 第 131073 次 RecordEvent 触发 request_events 判定并冻结
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

func TestRequestGuard_Concurrent_NoRace(t *testing.T) {
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
		MaxToolArgumentBytes:  262144,
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
		MaxToolArgumentBytes:  262144,
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
// 合法配置工具与通道域 (tool <= channel <= request: tool=256KiB, channel=1MiB, request=1MiB)。
// 通过另一 channel (c2) 占额，精确构造多 channel 共享 request 预算时的优先级竞态。
func TestAppendScanBudgetVsRequestBytesPrecedenceMultiChannelValidConfig(t *testing.T) {
	channelLimit := int64(1 << 20) // 1 MiB
	reqLimit := int64(1 << 20)     // 1 MiB (合法: tool 256KiB <= channel 1MiB <= request 1MiB)
	rg := NewRequestGuard(Config{
		Enabled:               true,
		MaxToolArgumentBytes:  262144,
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

// TestBoundaryEndpointsAcceptance:
// 验证 R3-LOW-2 边界端点"接受方向"测试：
// 当各上限字段恰好等于合法最大值时（纯 > 比较）：
// tool_argument_bytes == MaxToolArgumentBytes
// tool_argument_deltas == MaxToolArgumentDeltas
// channel_bytes == MaxChannelBytes
// request_bytes == MaxRequestBytes
// request_events == MaxRequestEvents
// max_request_channels == MaxRequestChannels
// 均应被接受且不触发任何 guard 熔断。
func TestBoundaryEndpointsAcceptance(t *testing.T) {
	t.Run("ToolArgumentBytesAndDeltasExactMaxAccepted", func(t *testing.T) {
		const maxDeltas = 10
		const maxBytes = 100
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  maxBytes,
			MaxToolArgumentDeltas: maxDeltas,
			MaxChannelBytes:       200,
			MaxRequestBytes:       1000,
		})

		// 10 次 10 字节 delta，累计恰好 10 deltas 与 100 bytes
		payload := randBytes(10, 1001)
		for i := 0; i < maxDeltas; i++ {
			if err := rg.ObserveAppend("call_boundary", "", KindToolArguments, payload); err != nil {
				t.Fatalf("delta %d of %d failed at exact boundary: %v", i+1, maxDeltas, err)
			}
		}

		if rg.IsTripped() {
			t.Fatalf("expected guard not tripped at exact tool bounds, but got: %s", rg.TrippedReason())
		}
	})

	t.Run("ChannelBytesAndRequestBytesExactMaxAccepted", func(t *testing.T) {
		const maxChannel = 500
		const maxRequest = 500
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  500,
			MaxToolArgumentDeltas: 100,
			MaxChannelBytes:       maxChannel,
			MaxRequestBytes:       maxRequest,
		})

		payload := randBytes(maxChannel, 1002)
		if err := rg.ObserveAppend("chan_boundary", "", KindText, payload); err != nil {
			t.Fatalf("expected exact channel/request max bytes to be accepted, got: %v", err)
		}

		if rg.IsTripped() {
			t.Fatalf("expected guard not tripped at exact channel/request bytes, but got: %s", rg.TrippedReason())
		}
	})

	t.Run("RequestEventsExactMaxAccepted", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  1000000,
			MaxToolArgumentDeltas: 1000000,
			MaxChannelBytes:       1000000,
			MaxRequestBytes:       1000000,
		})

		for i := 0; i < MaxRequestEvents; i++ {
			if err := rg.RecordEvent(); err != nil {
				t.Fatalf("RecordEvent at exact boundary %d failed: %v", i+1, err)
			}
		}

		if rg.IsTripped() {
			t.Fatalf("expected guard not tripped at exact MaxRequestEvents, but got: %s", rg.TrippedReason())
		}
	})

	t.Run("MaxRequestChannelsExactMaxAccepted", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  1000000,
			MaxToolArgumentDeltas: 1000000,
			MaxChannelBytes:       1000000,
			MaxRequestBytes:       1000000,
		})

		for i := 0; i < MaxRequestChannels; i++ {
			cid := fmt.Sprintf("chan_%d", i)
			if err := rg.ObserveAppend(cid, "", KindText, []byte("x")); err != nil {
				t.Fatalf("channel %d at exact boundary failed: %v", i+1, err)
			}
		}

		if rg.IsTripped() {
			t.Fatalf("expected guard not tripped at exact MaxRequestChannels, but got: %s", rg.TrippedReason())
		}
	})
}
