package wsrelay

import (
	"bytes"
	"log"
	"net/http"
	"strings"
	"testing"
)

// TestLogCompressionNegotiationDedupe 验证协商结果日志按结果去重:
// 同一结果只报一次,两种结果各报一次后不再产生任何输出(混合链路不刷屏)。
func TestLogCompressionNegotiationDedupe(t *testing.T) {
	wsCompressionSeen.Store(0)
	t.Cleanup(func() { wsCompressionSeen.Store(0) })

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	negotiated := &http.Response{Header: http.Header{
		"Sec-Websocket-Extensions": {"permessage-deflate; server_no_context_takeover; client_no_context_takeover"},
	}}
	plain := &http.Response{Header: http.Header{}}

	logCompressionNegotiation(nil, 1)
	if buf.Len() != 0 {
		t.Fatalf("nil resp 不应产生日志: %q", buf.String())
	}

	logCompressionNegotiation(negotiated, 1)
	if got := buf.String(); !strings.Contains(got, "已协商 permessage-deflate") {
		t.Fatalf("首次协商成功应打日志, got %q", got)
	}
	buf.Reset()

	logCompressionNegotiation(negotiated, 2)
	if buf.Len() != 0 {
		t.Fatalf("重复结果不应再打日志: %q", buf.String())
	}

	logCompressionNegotiation(plain, 3)
	if got := buf.String(); !strings.Contains(got, "未协商 permessage-deflate") {
		t.Fatalf("首次未协商也应打一次日志, got %q", got)
	}
	buf.Reset()

	logCompressionNegotiation(plain, 4)
	logCompressionNegotiation(negotiated, 5)
	if buf.Len() != 0 {
		t.Fatalf("两种结果各报过一次后应完全静默: %q", buf.String())
	}
}
