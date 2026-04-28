package sse

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Event SSE 事件
type Event struct {
	Type string      // 事件类型
	Data interface{} // 事件数据
	Raw  string      // 原始数据
}

// StreamReader SSE 流读取器
type StreamReader struct {
	reader  *bufio.Reader
	timeout time.Duration
}

// NewStreamReader 创建 SSE 流读取器
func NewStreamReader(r io.Reader, timeout time.Duration) *StreamReader {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &StreamReader{
		reader:  bufio.NewReader(r),
		timeout: timeout,
	}
}

// ReadEvent 读取一个 SSE 事件
func (s *StreamReader) ReadEvent(ctx context.Context) (*Event, error) {
	var eventType string
	var dataLines []string

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		line, err := s.reader.ReadString('\n')
		if err != nil {
			if err == io.EOF && len(dataLines) > 0 {
				// 最后一个事件
				return s.parseEvent(eventType, dataLines), nil
			}
			return nil, err
		}

		line = strings.TrimRight(line, "\r\n")

		// 空行表示事件结束
		if line == "" {
			if len(dataLines) > 0 {
				return s.parseEvent(eventType, dataLines), nil
			}
			continue
		}

		// 解析字段
		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(line[6:])
		} else if strings.HasPrefix(line, "data:") {
			data := strings.TrimSpace(line[5:])
			dataLines = append(dataLines, data)
		}
		// 忽略其他字段（id, retry 等）
	}
}

// parseEvent 解析事件
func (s *StreamReader) parseEvent(eventType string, dataLines []string) *Event {
	raw := strings.Join(dataLines, "\n")
	event := &Event{
		Type: eventType,
		Raw:  raw,
	}

	// 尝试解析为 JSON
	var data interface{}
	if err := json.Unmarshal([]byte(raw), &data); err == nil {
		event.Data = data
	} else {
		event.Data = raw
	}

	return event
}

// StreamWriter SSE 流写入器
type StreamWriter struct {
	writer  http.ResponseWriter
	flusher http.Flusher
}

// NewStreamWriter 创建 SSE 流写入器
func NewStreamWriter(w http.ResponseWriter) (*StreamWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("streaming not supported")
	}

	// 设置 SSE 响应头
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	return &StreamWriter{
		writer:  w,
		flusher: flusher,
	}, nil
}

// WriteEvent 写入一个 SSE 事件
func (s *StreamWriter) WriteEvent(eventType string, data interface{}) error {
	// 序列化数据
	var dataStr string
	switch v := data.(type) {
	case string:
		dataStr = v
	case []byte:
		dataStr = string(v)
	default:
		jsonData, err := json.Marshal(data)
		if err != nil {
			return err
		}
		dataStr = string(jsonData)
	}

	// 写入事件
	if eventType != "" {
		fmt.Fprintf(s.writer, "event: %s\n", eventType)
	}

	// 处理多行数据
	lines := strings.Split(dataStr, "\n")
	for _, line := range lines {
		fmt.Fprintf(s.writer, "data: %s\n", line)
	}

	// 空行表示事件结束
	fmt.Fprintf(s.writer, "\n")

	// 立即刷新
	s.flusher.Flush()

	return nil
}

// WriteComment 写入注释（用于保持连接）
func (s *StreamWriter) WriteComment(comment string) error {
	fmt.Fprintf(s.writer, ": %s\n\n", comment)
	s.flusher.Flush()
	return nil
}

// ImageProgressEvent 图片生成进度事件
type ImageProgressEvent struct {
	Type    string `json:"type"`    // progress / image / error / done
	Percent int    `json:"percent"` // 进度百分比（0-100）
	Message string `json:"message"` // 进度消息
	Index   int    `json:"index"`   // 图片索引
	URL     string `json:"url"`     // 图片 URL
	Error   string `json:"error"`   // 错误信息
}

// ParseImageSSE 解析图片生成的 SSE 事件
func ParseImageSSE(event *Event) (*ImageProgressEvent, error) {
	if event == nil {
		return nil, fmt.Errorf("event is nil")
	}

	// 尝试解析为 ImageProgressEvent
	var progress ImageProgressEvent

	switch data := event.Data.(type) {
	case map[string]interface{}:
		// JSON 对象
		jsonData, _ := json.Marshal(data)
		if err := json.Unmarshal(jsonData, &progress); err != nil {
			return nil, err
		}
	case string:
		// JSON 字符串
		if err := json.Unmarshal([]byte(data), &progress); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported data type: %T", data)
	}

	return &progress, nil
}

// StreamImageGeneration 流式图片生成（示例）
func StreamImageGeneration(ctx context.Context, writer *StreamWriter, prompt string) error {
	// 发送开始事件
	if err := writer.WriteEvent("progress", ImageProgressEvent{
		Type:    "progress",
		Percent: 0,
		Message: "开始生成图片",
	}); err != nil {
		return err
	}

	// 模拟生成进度
	for i := 0; i <= 100; i += 25 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}

		if err := writer.WriteEvent("progress", ImageProgressEvent{
			Type:    "progress",
			Percent: i,
			Message: fmt.Sprintf("生成中 %d%%", i),
		}); err != nil {
			return err
		}
	}

	// 发送图片完成事件
	if err := writer.WriteEvent("image", ImageProgressEvent{
		Type:  "image",
		Index: 0,
		URL:   "/p/img/123?exp=...&sig=...",
	}); err != nil {
		return err
	}

	// 发送完成事件
	if err := writer.WriteEvent("done", ImageProgressEvent{
		Type:    "done",
		Message: "生成完成",
	}); err != nil {
		return err
	}

	return nil
}

// FetchSSEStream 获取 SSE 流（客户端）
func FetchSSEStream(ctx context.Context, url string, headers map[string]string) (*StreamReader, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	// 设置请求头
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	client := &http.Client{
		Timeout: 0, // 无超时（流式连接）
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return NewStreamReader(resp.Body, 60*time.Second), nil
}

// BufferedStreamWriter 带缓冲的 SSE 写入器
type BufferedStreamWriter struct {
	*StreamWriter
	buffer *bytes.Buffer
}

// NewBufferedStreamWriter 创建带缓冲的 SSE 写入器
func NewBufferedStreamWriter(w http.ResponseWriter) (*BufferedStreamWriter, error) {
	sw, err := NewStreamWriter(w)
	if err != nil {
		return nil, err
	}

	return &BufferedStreamWriter{
		StreamWriter: sw,
		buffer:       &bytes.Buffer{},
	}, nil
}

// WriteEvent 写入事件到缓冲区
func (b *BufferedStreamWriter) WriteEvent(eventType string, data interface{}) error {
	// 序列化数据
	var dataStr string
	switch v := data.(type) {
	case string:
		dataStr = v
	case []byte:
		dataStr = string(v)
	default:
		jsonData, err := json.Marshal(data)
		if err != nil {
			return err
		}
		dataStr = string(jsonData)
	}

	// 写入缓冲区
	if eventType != "" {
		fmt.Fprintf(b.buffer, "event: %s\n", eventType)
	}

	lines := strings.Split(dataStr, "\n")
	for _, line := range lines {
		fmt.Fprintf(b.buffer, "data: %s\n", line)
	}

	fmt.Fprintf(b.buffer, "\n")

	return nil
}

// Flush 刷新缓冲区
func (b *BufferedStreamWriter) Flush() error {
	if b.buffer.Len() == 0 {
		return nil
	}

	_, err := b.writer.Write(b.buffer.Bytes())
	if err != nil {
		return err
	}

	b.buffer.Reset()
	b.flusher.Flush()

	return nil
}
