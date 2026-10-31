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
