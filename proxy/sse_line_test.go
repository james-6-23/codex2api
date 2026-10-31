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

// TestReadSSEStreamWithEventEmptyDataLinesCountTowardCap 验证空 data: 行同样
// 计入事件字节上限。每行至少计 1 字节，否则无限多的空 data 行会在事件边界
// （空行）始终不出现时无界累积内存（修复前 currentEventBytes 恒不增长，
// callback 会收到约 8MiB 的纯换行 payload 且 err=nil）。
func TestReadSSEStreamWithEventEmptyDataLinesCountTowardCap(t *testing.T) {
	input := strings.Repeat("data:\n", maxSSEEventBytes+1)

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
