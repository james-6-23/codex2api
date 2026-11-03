package proxy

import (
	"bufio"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
)

func TestReadSSEDataLineBoundsWholeEvent(t *testing.T) {
	line := "data: " + strings.Repeat("x", 1<<20) + "\n"
	data, err := readSSEDataLine(bufio.NewReader(strings.NewReader(strings.Repeat(line, 10) + "\n")))
	if !errors.Is(err, errSSEEventTooLarge) || len(data) != 0 {
		t.Fatalf("len=%d err=%v, want rejected aggregate event", len(data), err)
	}
}

func TestReadSSEDataLineDoesNotHideOversizeLine(t *testing.T) {
	input := "data: first\n" + "data: " + strings.Repeat("x", maxSSELineBytes) + "\n\n"
	data, err := readSSEDataLine(bufio.NewReader(strings.NewReader(input)))
	if !errors.Is(err, errSSELineTooLong) || len(data) != 0 {
		t.Fatalf("len=%d err=%v, want propagated line error", len(data), err)
	}
}

func TestReadSSEDataLinePreservesMultilineEvent(t *testing.T) {
	data, err := readSSEDataLine(bufio.NewReader(strings.NewReader("data: first\ndata: second\n\n")))
	if err != nil || string(data) != "first\nsecond" {
		t.Fatalf("data=%q err=%v", data, err)
	}
}

func TestReadBoundedSSELine(t *testing.T) {
	long := strings.Repeat("a", 10000) + "\n"
	reader := bufio.NewReaderSize(strings.NewReader(long), 16)
	line, err := readBoundedSSELine(reader, maxSSELineBytes)
	if err != nil || len(line) != len(long) {
		t.Fatalf("len=%d err=%v, want %d bytes", len(line), err, len(long))
	}

	reader = bufio.NewReaderSize(strings.NewReader("123456789\n"), 4)
	if _, err := readBoundedSSELine(reader, 8); err != errSSELineTooLong {
		t.Fatalf("err = %v, want errSSELineTooLong", err)
	}

	reader = bufio.NewReaderSize(strings.NewReader("12345678\n"), 4)
	if line, err := readBoundedSSELine(reader, 9); err != nil || string(line) != "12345678\n" {
		t.Fatalf("line=%q err=%v", line, err)
	}
}

// TestReadSSEStreamWithEventSplitChunks 验证跨 Read 块的 split chunk 正常解析。
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

// TestReadSSEStreamWithEventMultilineData 验证标准多行 data 聚合。
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

// TestReadSSEStreamWithEventNoNewlineOversized 验证无换行符超大单行硬 cap 拒绝，
// 返回 ErrSSELineTooLong（与既有 errSSELineTooLong 等价）。
func TestReadSSEStreamWithEventNoNewlineOversized(t *testing.T) {
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

// TestReadSSEStreamWithEventNeverendingMultilineOversized 验证累积多行 data
// 整事件超标拒绝，返回 ErrSSEEventTooLarge（与既有 errSSEEventTooLarge 等价）。
func TestReadSSEStreamWithEventNeverendingMultilineOversized(t *testing.T) {
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

// TestReadBoundedResponseBodyOversized 验证非流式超大 Response Body 有界拦截，
// 返回 ErrResponseBodyTooLarge。
func TestReadBoundedResponseBodyOversized(t *testing.T) {
	oversizedBody := strings.Repeat("b", 1024)
	_, err := ReadBoundedResponseBody(strings.NewReader(oversizedBody), 512)
	if err == nil {
		t.Fatalf("expected ErrResponseBodyTooLarge, got nil")
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

// TestTransportFramingIndependentOfGuard 验证 Transport Framing 独立于 Guard
// 开关（关闭语义 guard 仍保留 wire cap）。
func TestTransportFramingIndependentOfGuard(t *testing.T) {
	oversized := strings.Repeat("x", MaxSSELineBytes+100)
	err := ReadSSEStreamWithEvent(strings.NewReader(oversized), func(event string, data []byte) bool {
		return true
	})
	if !errors.Is(err, ErrSSELineTooLong) {
		t.Fatalf("wire cap must remain active regardless of guard settings, got %v", err)
	}
}

// TestReadSSEStreamWithEventEmptyDataLinesCountTowardCap 验证空 data: 行
// 同样按 bytes.Join 分隔符计入事件字节上限。除首行外每条空行计 1 字节，
// 达到 maxSSEEventBytes+2 行时累积分隔符达到 maxSSEEventBytes+1 字节，
// 触发 ErrSSEEventTooLarge，防止事件边界缺失时空行无界累积。
func TestReadSSEStreamWithEventEmptyDataLinesCountTowardCap(t *testing.T) {
	input := strings.Repeat("data:\n", maxSSEEventBytes+2)

	callbackCalls := 0
	payloadLen := 0
	err := ReadSSEStreamWithEvent(strings.NewReader(input), func(event string, data []byte) bool {
		callbackCalls++
		payloadLen = len(data)
		return true
	})
	if !errors.Is(err, ErrSSEEventTooLarge) {
		t.Fatalf("callbacks=%d payloadLen=%d err=%v, want ErrSSEEventTooLarge", callbackCalls, payloadLen, err)
	}
	if callbackCalls != 0 {
		t.Fatalf("callback must not receive the unbounded event, got %d call(s), last payload len %d", callbackCalls, payloadLen)
	}
}

// TestReadSSEStreamWithEventExactCapSingleAndMultiline 验证多行事件（包括含空 data 行及纯空 data 行）
// 在拼接后恰好等于 8MiB 上限时均被成功接受，且交付字节数与上限严格一致。
func TestReadSSEStreamWithEventExactCapSingleAndMultiline(t *testing.T) {
	half := maxSSEEventBytes / 2

	// 1. 多行普通文本拼接后恰好 8MiB
	// joined = half + 1 + (half - 1) = maxSSEEventBytes
	exact := "data: " + strings.Repeat("a", half) + "\n" +
		"data: " + strings.Repeat("b", half-1) + "\n\n"
	payloadLen := 0
	err := ReadSSEStreamWithEvent(strings.NewReader(exact), func(_ string, data []byte) bool {
		payloadLen = len(data)
		return true
	})
	if err != nil {
		t.Fatalf("multiline event at exact cap must be accepted: %v", err)
	}
	if payloadLen != maxSSEEventBytes {
		t.Fatalf("payload len = %d, want %d", payloadLen, maxSSEEventBytes)
	}

	// 2. 多行含有空 data 行，拼接后恰好 8MiB：
	// 第一行 half 字节，第二行空 data，第三行 half-2 字节。
	// bytes.Join 结果: half + "\n" + "" + "\n" + (half - 2) = maxSSEEventBytes
	// 修复前空 data 行计 2 字节导致累计为 maxSSEEventBytes + 1 被误拒。
	withEmptyLineExact := "data: " + strings.Repeat("y", half) + "\n" +
		"data:\n" +
		"data: " + strings.Repeat("z", half-2) + "\n\n"
	payloadLen = 0
	err = ReadSSEStreamWithEvent(strings.NewReader(withEmptyLineExact), func(_ string, data []byte) bool {
		payloadLen = len(data)
		return true
	})
	if err != nil {
		t.Fatalf("multiline event with empty line at exact cap must be accepted: %v", err)
	}
	if payloadLen != maxSSEEventBytes {
		t.Fatalf("multiline payload len = %d, want %d", payloadLen, maxSSEEventBytes)
	}

	// 3. 纯空行拼接恰好 8MiB（maxSSEEventBytes+1 条空 data: 行，Join 后得到 8MiB 个换行符）
	exactEmptyLines := strings.Repeat("data:\n", maxSSEEventBytes+1) + "\n"
	payloadLen = 0
	err = ReadSSEStreamWithEvent(strings.NewReader(exactEmptyLines), func(_ string, data []byte) bool {
		payloadLen = len(data)
		return true
	})
	if err != nil {
		t.Fatalf("pure empty lines event at exact cap must be accepted: %v", err)
	}
	if payloadLen != maxSSEEventBytes {
		t.Fatalf("pure empty lines payload len = %d, want %d", payloadLen, maxSSEEventBytes)
	}
}

// TestReadSSEStreamWithEventMultilineJoinSeparatorsCountTowardCap 验证多行事件
// 把 bytes.Join 插入的 '\n' 分隔符也计入事件上限：拼接后恰好等于上限的 payload
// 可以送达 callback，超 1 字节则拒绝且不投递。修复前只统计 data 内容，k 行事件
// 的实际长度可达 maxSSEEventBytes + (k-1)，极端情况下约 2 倍上限。
func TestReadSSEStreamWithEventMultilineJoinSeparatorsCountTowardCap(t *testing.T) {
	half := maxSSEEventBytes / 2

	// joined = half + 1 + (half - 1) = maxSSEEventBytes，恰好等于上限。
	exact := "data: " + strings.Repeat("a", half) + "\n" +
		"data: " + strings.Repeat("b", half-1) + "\n\n"
	payloadLen := 0
	err := ReadSSEStreamWithEvent(strings.NewReader(exact), func(_ string, data []byte) bool {
		payloadLen = len(data)
		return true
	})
	if err != nil {
		t.Fatalf("event at exact cap must be accepted: %v", err)
	}
	if payloadLen != maxSSEEventBytes {
		t.Fatalf("payload len = %d, want %d", payloadLen, maxSSEEventBytes)
	}

	// joined = maxSSEEventBytes + 1，必须拒绝。
	// 修复前 content 合计仍是 maxSSEEventBytes，会被误判合法并投递超限事件。
	over := "data: " + strings.Repeat("a", half) + "\n" +
		"data: " + strings.Repeat("b", half) + "\n\n"
	callbackCalls := 0
	err = ReadSSEStreamWithEvent(strings.NewReader(over), func(string, []byte) bool {
		callbackCalls++
		return true
	})
	if !errors.Is(err, ErrSSEEventTooLarge) {
		t.Fatalf("expected ErrSSEEventTooLarge for event over cap, got %v", err)
	}
	if callbackCalls != 0 {
		t.Fatalf("oversized event must not be delivered, got %d call(s)", callbackCalls)
	}
}

// TestReadSSEStreamWithEventAcceptsNearLimitLineAcrossReads 验证一条接近 wire
// 上限的合法行跨多次 Read 到达时不被误判超长：行长检查必须在找到换行后按
// 完整行判断，而不是在 append 前按"未完成缓冲区 + 本次读取"的总长度判断。
func TestReadSSEStreamWithEventAcceptsNearLimitLineAcrossReads(t *testing.T) {
	// 注释行内容恰好等于上限（不含换行），随后紧跟一个合法事件。8 MiB 内容
	// 正好由 128 次 64 KiB 读取铺满；第 129 次读取同时带回换行与后续事件。
	// 修复前 len(lineBuf)+n > maxSSELineBytes 会在扫描前直接返回错误。
	input := ":" + strings.Repeat("x", maxSSELineBytes-1) + "\n" + "data: ok\n\n"

	var collected []string
	err := ReadSSEStreamWithEvent(strings.NewReader(input), func(_ string, data []byte) bool {
		collected = append(collected, string(data))
		return true
	})
	if err != nil {
		t.Fatalf("near-limit legal line must not be rejected: %v", err)
	}
	if len(collected) != 1 || collected[0] != "ok" {
		t.Fatalf("collected = %v, want ['ok']", collected)
	}
}

// TestReadSSEStreamWithEventRejectsNewlineTerminatedOversizedLine 验证带换行的
// 超限行仍被拒绝：行长检查移入扫描循环后，不能只靠"无换行"分支兜底而放过
// 完整行，否则超限行会先累积到内存再被当成合法行解析。
func TestReadSSEStreamWithEventRejectsNewlineTerminatedOversizedLine(t *testing.T) {
	input := ":" + strings.Repeat("x", maxSSELineBytes) + "\n" + "data: ok\n\n"

	err := ReadSSEStreamWithEvent(strings.NewReader(input), func(string, []byte) bool {
		return true
	})
	if !errors.Is(err, ErrSSELineTooLong) {
		t.Fatalf("err = %v, want ErrSSELineTooLong", err)
	}
}

// TestReadBoundedResponseBodyMaxInt64Limit 验证 limit=math.MaxInt64 时不会因
// limit+1 溢出为负导致 io.LimitReader 立即 EOF、静默返回空数据。
func TestReadBoundedResponseBodyMaxInt64Limit(t *testing.T) {
	body := strings.Repeat("a", 100)
	data, err := ReadBoundedResponseBody(strings.NewReader(body), math.MaxInt64)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(data) != 100 {
		t.Fatalf("len(data) = %d, want 100", len(data))
	}
}
