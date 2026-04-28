package sse

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStreamReader(t *testing.T) {
	// 模拟 SSE 流
	sseData := `event: message
data: {"type":"progress","percent":50}

event: done
data: {"type":"done"}

`

	reader := NewStreamReader(strings.NewReader(sseData), 5*time.Second)
	ctx := context.Background()

	// 读取第一个事件
	event1, err := reader.ReadEvent(ctx)
	if err != nil {
		t.Fatalf("读取第一个事件失败: %v", err)
	}

	if event1.Type != "message" {
		t.Errorf("事件类型错误: expected 'message', got '%s'", event1.Type)
	}

	// 读取第二个事件
	event2, err := reader.ReadEvent(ctx)
	if err != nil {
		t.Fatalf("读取第二个事件失败: %v", err)
	}

	if event2.Type != "done" {
		t.Errorf("事件类型错误: expected 'done', got '%s'", event2.Type)
	}
}

func TestStreamWriter(t *testing.T) {
	// 创建测试响应写入器
	recorder := httptest.NewRecorder()

	writer, err := NewStreamWriter(recorder)
	if err != nil {
		t.Fatalf("创建 StreamWriter 失败: %v", err)
	}

	// 写入事件
	err = writer.WriteEvent("test", map[string]interface{}{
		"message": "hello",
		"count":   42,
	})
	if err != nil {
		t.Fatalf("写入事件失败: %v", err)
	}

	// 验证响应头
	if recorder.Header().Get("Content-Type") != "text/event-stream" {
		t.Error("Content-Type 应为 text/event-stream")
	}

	// 验证响应内容
	body := recorder.Body.String()
	if !strings.Contains(body, "event: test") {
		t.Error("响应应包含 'event: test'")
	}
	if !strings.Contains(body, "data:") {
		t.Error("响应应包含 'data:'")
	}
}

func TestParseImageSSE(t *testing.T) {
	tests := []struct {
		name     string
		event    *Event
		expected *ImageProgressEvent
		wantErr  bool
	}{
		{
			name: "进度事件",
			event: &Event{
				Type: "progress",
				Data: map[string]interface{}{
					"type":    "progress",
					"percent": float64(50),
					"message": "生成中",
				},
			},
			expected: &ImageProgressEvent{
				Type:    "progress",
				Percent: 50,
				Message: "生成中",
			},
			wantErr: false,
		},
		{
			name: "图片完成事件",
			event: &Event{
				Type: "image",
				Data: map[string]interface{}{
					"type":  "image",
					"index": float64(0),
					"url":   "/p/img/123",
				},
			},
			expected: &ImageProgressEvent{
				Type:  "image",
				Index: 0,
				URL:   "/p/img/123",
			},
			wantErr: false,
		},
		{
			name:     "空事件",
			event:    nil,
			expected: nil,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseImageSSE(tt.event)

			if tt.wantErr {
				if err == nil {
					t.Error("期望返回错误，但没有错误")
				}
				return
			}

			if err != nil {
				t.Fatalf("不期望错误，但返回了: %v", err)
			}

			if result.Type != tt.expected.Type {
				t.Errorf("Type 不匹配: expected %s, got %s", tt.expected.Type, result.Type)
			}

			if result.Percent != tt.expected.Percent {
				t.Errorf("Percent 不匹配: expected %d, got %d", tt.expected.Percent, result.Percent)
			}
		})
	}
}

func TestBufferedStreamWriter(t *testing.T) {
	recorder := httptest.NewRecorder()

	writer, err := NewBufferedStreamWriter(recorder)
	if err != nil {
		t.Fatalf("创建 BufferedStreamWriter 失败: %v", err)
	}

	// 写入多个事件到缓冲区
	err = writer.WriteEvent("event1", "data1")
	if err != nil {
		t.Fatalf("写入事件1失败: %v", err)
	}

	err = writer.WriteEvent("event2", "data2")
	if err != nil {
		t.Fatalf("写入事件2失败: %v", err)
	}

	// 此时响应体应该为空（还未刷新）
	if recorder.Body.Len() > 0 {
		t.Error("刷新前响应体应为空")
	}

	// 刷新缓冲区
	err = writer.Flush()
	if err != nil {
		t.Fatalf("刷新失败: %v", err)
	}

	// 验证响应内容
	body := recorder.Body.String()
	if !strings.Contains(body, "event: event1") {
		t.Error("响应应包含 event1")
	}
	if !strings.Contains(body, "event: event2") {
		t.Error("响应应包含 event2")
	}
}

func TestStreamReaderTimeout(t *testing.T) {
	// 创建一个永远阻塞的读取器
	reader := NewStreamReader(&blockingReader{}, 100*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := reader.ReadEvent(ctx)
	if err == nil {
		t.Error("期望超时错误，但没有错误")
	}
}

func TestWriteComment(t *testing.T) {
	recorder := httptest.NewRecorder()

	writer, err := NewStreamWriter(recorder)
	if err != nil {
		t.Fatalf("创建 StreamWriter 失败: %v", err)
	}

	err = writer.WriteComment("keep-alive")
	if err != nil {
		t.Fatalf("写入注释失败: %v", err)
	}

	body := recorder.Body.String()
	if !strings.Contains(body, ": keep-alive") {
		t.Error("响应应包含注释")
	}
}

// blockingReader 用于测试超时
type blockingReader struct{}

func (b *blockingReader) Read(p []byte) (n int, err error) {
	time.Sleep(10 * time.Second)
	return 0, nil
}

func TestMultilineData(t *testing.T) {
	recorder := httptest.NewRecorder()

	writer, err := NewStreamWriter(recorder)
	if err != nil {
		t.Fatalf("创建 StreamWriter 失败: %v", err)
	}

	// 写入多行数据
	multilineData := "line1\nline2\nline3"
	err = writer.WriteEvent("test", multilineData)
	if err != nil {
		t.Fatalf("写入多行数据失败: %v", err)
	}

	body := recorder.Body.String()
	lines := strings.Split(body, "\n")

	// 验证每行都有 data: 前缀
	dataLineCount := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "data:") {
			dataLineCount++
		}
	}

	if dataLineCount != 3 {
		t.Errorf("期望 3 行数据，得到 %d 行", dataLineCount)
	}
}
