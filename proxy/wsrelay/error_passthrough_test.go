package wsrelay

import (
	"bytes"
	"io"
	"testing"

	"github.com/tidwall/gjson"
)

// TestBuildErrorEvent_UpstreamErrorBecomesFailedEvent 验证上游 error 帧被转成
// 下游可识别的 response.failed SSE 事件，并保留原始错误内容。
func TestBuildErrorEvent_UpstreamErrorBecomesFailedEvent(t *testing.T) {
	r := &WsResponse{}
	upstream := []byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error","message":"Unsupported parameter: prompt_cache_retention"}}`)

	event, isErr := r.buildErrorEvent(upstream)
	if !isErr {
		t.Fatal("expected error frame to be detected")
	}
	if gjson.GetBytes(event, "type").String() != "response.failed" {
		t.Fatalf("event type = %q, want response.failed", gjson.GetBytes(event, "type").String())
	}
	// 原始错误内容应保留
	if msg := gjson.GetBytes(event, "response.error.message").String(); msg != "Unsupported parameter: prompt_cache_retention" {
		t.Fatalf("error message not preserved: %q", msg)
	}
}

// TestBuildErrorEvent_UpstreamStatusBecomesResponseStatusCode verifies an upstream
// top-level status is copied to response.status_code even when the preserved
// upstream error object does not contain status_code itself.
func TestBuildErrorEvent_UpstreamStatusBecomesResponseStatusCode(t *testing.T) {
	r := &WsResponse{}
	upstream := []byte(`{"type":"error","status":429,"error":{"type":"rate_limit_error","message":"too many requests","param":"input"}}`)

	event, isErr := r.buildErrorEvent(upstream)
	if !isErr {
		t.Fatal("expected error frame to be detected")
	}
	if statusCode := gjson.GetBytes(event, "response.status_code").Int(); statusCode != 429 {
		t.Fatalf("response.status_code = %d, want 429; event=%s", statusCode, event)
	}
	if gjson.GetBytes(event, "response.error.status_code").Exists() {
		t.Fatalf("response.error should preserve upstream error object without injecting status_code: %s", event)
	}
	if msg := gjson.GetBytes(event, "response.error.message").String(); msg != "too many requests" {
		t.Fatalf("response.error.message = %q, want preserved upstream message", msg)
	}
	if typ := gjson.GetBytes(event, "response.error.type").String(); typ != "rate_limit_error" {
		t.Fatalf("response.error.type = %q, want preserved upstream type", typ)
	}
	if param := gjson.GetBytes(event, "response.error.param").String(); param != "input" {
		t.Fatalf("response.error.param = %q, want preserved upstream param", param)
	}
}

// TestBuildErrorEvent_UpstreamStatusCodeBecomesResponseStatusCode verifies an
// upstream top-level status_code is copied to response.status_code while the
// original response.error payload remains unchanged.
func TestBuildErrorEvent_UpstreamStatusCodeBecomesResponseStatusCode(t *testing.T) {
	r := &WsResponse{}
	upstream := []byte(`{"type":"error","status_code":503,"error":{"message":"service unavailable","code":"server_error"}}`)

	event, isErr := r.buildErrorEvent(upstream)
	if !isErr {
		t.Fatal("expected error frame to be detected")
	}
	if statusCode := gjson.GetBytes(event, "response.status_code").Int(); statusCode != 503 {
		t.Fatalf("response.status_code = %d, want 503; event=%s", statusCode, event)
	}
	if gjson.GetBytes(event, "response.error.status_code").Exists() {
		t.Fatalf("response.error should preserve upstream error object without injecting status_code: %s", event)
	}
	if msg := gjson.GetBytes(event, "response.error.message").String(); msg != "service unavailable" {
		t.Fatalf("response.error.message = %q, want preserved upstream message", msg)
	}
	if code := gjson.GetBytes(event, "response.error.code").String(); code != "server_error" {
		t.Fatalf("response.error.code = %q, want preserved upstream code", code)
	}
}

// TestBuildErrorEvent_NonErrorPassthrough 验证非错误帧不被识别为错误。
func TestBuildErrorEvent_NonErrorPassthrough(t *testing.T) {
	r := &WsResponse{}
	for _, payload := range [][]byte{
		[]byte(`{"type":"response.created","response":{}}`),
		[]byte(`{"type":"response.output_text.delta","delta":"hi"}`),
		[]byte(``),
		[]byte(`{}`),
	} {
		if _, isErr := r.buildErrorEvent(payload); isErr {
			t.Fatalf("non-error payload wrongly flagged as error: %s", payload)
		}
	}
}

// TestBuildErrorEvent_PrettyPrintedErrorCompacted 验证上游 pretty-printed（带换行）
// 错误对象被压缩为单行后嵌入事件。若不压缩，SSE data: 行编码在第一个换行处截断，
// 下游只能看到 `..."error":{` 而丢失具体错误信息。
func TestBuildErrorEvent_PrettyPrintedErrorCompacted(t *testing.T) {
	r := &WsResponse{}
	upstream := []byte("{\"type\":\"error\",\"status\":400,\"error\":{\n  \"message\": \"Invalid type for 'input[61].arguments': expected an object, but got a string instead.\",\n  \"type\": \"invalid_request_error\",\n  \"param\": \"input[61].arguments\",\n  \"code\": \"invalid_type\"\n}}")

	event, isErr := r.buildErrorEvent(upstream)
	if !isErr {
		t.Fatal("expected error frame to be detected")
	}
	if bytes.IndexByte(event, '\n') >= 0 {
		t.Fatalf("event must be single-line for SSE data encoding: %s", event)
	}
	if msg := gjson.GetBytes(event, "response.error.message").String(); msg != "Invalid type for 'input[61].arguments': expected an object, but got a string instead." {
		t.Fatalf("error message not preserved: %q", msg)
	}
	if param := gjson.GetBytes(event, "response.error.param").String(); param != "input[61].arguments" {
		t.Fatalf("error param not preserved: %q", param)
	}
}

// TestHandleMessage_ErrorFrameWritesToCallbackAndEnds 验证 handleMessage 收到
// 上游 error 帧时：把错误事件写给 callback（透传给下游），并返回 io.EOF 结束流，
// 而不是返回普通 error 导致 pipe 静默关闭、下游空响应。
func TestHandleMessage_ErrorFrameWritesToCallbackAndEnds(t *testing.T) {
	r := &WsResponse{}
	upstream := []byte(`{"type":"error","status":400,"error":{"message":"boom"}}`)

	var captured [][]byte
	err := r.handleMessage(upstream, func(data []byte) bool {
		captured = append(captured, append([]byte(nil), data...))
		return true
	})

	if err != io.EOF {
		t.Fatalf("handleMessage on error frame returned %v, want io.EOF", err)
	}
	if len(captured) != 1 {
		t.Fatalf("expected error event written to callback once, got %d writes", len(captured))
	}
	if gjson.GetBytes(captured[0], "type").String() != "response.failed" {
		t.Fatalf("callback got type %q, want response.failed", gjson.GetBytes(captured[0], "type").String())
	}
	if gjson.GetBytes(captured[0], "response.error.message").String() != "boom" {
		t.Fatalf("error detail not preserved in callback payload")
	}
}

// TestHandleMessage_NormalFrameUnaffected 验证正常帧仍正常透传。
func TestHandleMessage_NormalFrameUnaffected(t *testing.T) {
	r := &WsResponse{}
	var captured [][]byte
	cb := func(data []byte) bool { captured = append(captured, data); return true }

	// 普通增量帧：透传，不终止
	if err := r.handleMessage([]byte(`{"type":"response.output_text.delta","delta":"a"}`), cb); err != nil {
		t.Fatalf("delta frame returned %v, want nil", err)
	}
	// 完成帧：透传 + io.EOF 终止
	if err := r.handleMessage([]byte(`{"type":"response.completed","response":{}}`), cb); err != io.EOF {
		t.Fatalf("completed frame returned %v, want io.EOF", err)
	}
	if len(captured) != 2 {
		t.Fatalf("expected 2 frames passed through, got %d", len(captured))
	}
}
