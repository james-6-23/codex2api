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

// 场景 1: 事故样式短周期重复（p<=256，8192~10240 触发，不等到 256 KiB）
func TestAcceptanceScenario1_AccidentPatternEarlyTrigger(t *testing.T) {
	rg := NewRequestGuard(Config{
		Enabled:               true,
		MaxToolArgumentBytes:  262144,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})

	pattern := []byte("_auditfree.md_pluginless.tmp.md") // 31 字节周期
	chunkSize := 128
	totalPushed := 0
	trippedAt := 0

	for totalPushed < 262144 {
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
	if trippedAt < 8192 {
		t.Fatalf("expected early trigger near 8192-10240 bytes, but triggered too early at %d bytes", trippedAt)
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
				MaxToolArgumentBytes:  262144,
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
			MaxToolArgumentBytes:  262144,
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
			MaxToolArgumentBytes:  262144,
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
		MaxToolArgumentBytes:  262144,
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
			MaxToolArgumentBytes:  262144,
			MaxToolArgumentDeltas: 65536,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		prng := rand.New(rand.NewSource(2001))
		exactData := make([]byte, 262144)
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
			MaxToolArgumentBytes:  262144,
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
			MaxToolArgumentBytes:  262144,
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

	// 5d: 空 delta 洪泛与 131072 业务事件
	t.Run("EmptyDeltaEventFlood", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  262144,
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
			MaxToolArgumentBytes:  262144,
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
			MaxToolArgumentBytes:  262144, // scan budget = 524288
			MaxToolArgumentDeltas: 65536,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		chunk := make([]byte, 1024)
		rand.New(rand.NewSource(501)).Read(chunk)
		for i := 0; i < 256; i++ {
			if err := rgTool.ObserveAppend("tool_map", "", KindToolArguments, chunk); err != nil {
				t.Fatalf("append %d failed: %v", i, err)
			}
		}
		// 终态快照建立 baseline
		full := bytes.Repeat(chunk, 256)
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
			MaxToolArgumentBytes:  262144,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       131072, // scan budget = 262144
			MaxRequestBytes:       4194304,
		})
		for i := 0; i < 128; i++ {
			if err := rgText.ObserveAppend("text_map", "", KindText, chunk); err != nil {
				t.Fatalf("text append %d failed: %v", i, err)
			}
		}
		textFull := bytes.Repeat(chunk, 128)
		textVariant := bytes.Clone(textFull)
		textVariant[100] ^= 0xEE
		if err := rgText.ObserveSnapshot("text_map", "", KindText, textFull, "arguments.done"); err != nil {
			t.Fatalf("text snapshot failed: %v", err)
		}
		errText := rgText.ObserveSnapshot("text_map", "", KindText, textVariant, "item.done")
		if errText == nil || errText.Error() != "channel_bytes" {
			t.Fatalf("expected mapped reason channel_bytes, got %v", errText)
		}
	})
}

// 场景 6: 边界值、UTF-8 中文、Emoji、转义符与 int64 overflow 防护
func TestAcceptanceScenario6_BoundaryValuesAndUTF8(t *testing.T) {
	rg := NewRequestGuard(Config{
		Enabled:               true,
		MaxToolArgumentBytes:  262144,
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
			MaxToolArgumentBytes:  262144,
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
			MaxToolArgumentBytes:  262144,
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
			MaxToolArgumentBytes:  262144,
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
			MaxToolArgumentBytes:  262144,
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
			MaxToolArgumentBytes:  262144,
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
			MaxToolArgumentBytes:  262144,
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
		toolLimit := int64(262144) // 256 KiB -> scan budget 512 KiB
		cg := NewChannelGuard("t8a", KindToolArguments, toolLimit, 4096)

		prng := rand.New(rand.NewSource(801))
		chunk := make([]byte, 1024)
		for i := 0; i < 256; i++ {
			prng.Read(chunk)
			g, c, err := cg.CheckAppend(chunk)
			if err != nil {
				t.Fatalf("append %d failed: %v", i, err)
			}
			_ = cg.CommitAppend(chunk, g, c)
		}
		if cg.RemainingScanBudget() != 262144 {
			t.Fatalf("want 262144, got %d", cg.RemainingScanBudget())
		}

		snap := make([]byte, 262144)
		prng.Read(snap)
		g, c, skip, err := cg.CheckSnapshot(snap, "arguments.done")
		if err != nil || skip {
			t.Fatalf("first snapshot failed: err=%v, skip=%v", err, skip)
		}
		_ = cg.CommitSnapshot(snap, g, c, "arguments.done")

		if cg.RemainingScanBudget() != 0 {
			t.Fatalf("want 0 scan budget, got %d", cg.RemainingScanBudget())
		}

		variant := make([]byte, 262144)
		copy(variant, snap)
		variant[500] ^= 0xFF

		_, _, _, err = cg.CheckSnapshot(variant, "item.done")
		if !errors.Is(err, ErrScanBudgetExhausted) {
			t.Fatalf("want ErrScanBudgetExhausted, got %v", err)
		}
	})

	// 用例 8b: 终态链 3 次许可（最多 3 次 cost=0 全文比对）与超出 3 次拦截；真实内部私有统计断言 (扫描 <= 2*limit, 总验证 <= 5*limit)
	t.Run("Case8b", func(t *testing.T) {
		toolLimit := int64(262144) // limit = 256 KiB
		cg := NewChannelGuard("t8b", KindToolArguments, toolLimit, 4096)

		snap := make([]byte, 262144)
		rand.New(rand.NewSource(802)).Read(snap)

		cg.remainingScanBudget = 262144 // 预留 256 KiB 扫描预算（前置 append 已消耗 256 KiB）
		cg.highwater = 262144
		g, c, skip, err := cg.CheckSnapshot(snap, "arguments.done")
		if err != nil || skip || c != 262144 {
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

		// 严格验证总处理量上界：总扫描预算为 2*limit (512 KiB)，3 次终态比对中最多 3 次额外全文 hash 比对（3*limit = 768 KiB），整体最大数据处理与校验量严格受限于 5*limit (1280 KiB)
		const maxAllowedScanned = 2 * 262144
		const maxAllowedVerified = 5 * 262144
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
				MaxToolArgumentBytes:  262144,
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
		MaxToolArgumentBytes:  262144,
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
			MaxToolArgumentBytes:  262144,
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
			MaxToolArgumentBytes:  262144,
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
		MaxToolArgumentBytes:  262144,
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
