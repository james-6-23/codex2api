package proxy

import (
	"net/http"
	"testing"
)

// TestEnableCodexHTTP2KeepAlive 验证在标准 transport 上启用 HTTP/2 保活后，
// 底层 http2.Transport 的 ReadIdleTimeout/PingTimeout 被设置为预期值，
// 从而对被静默掐断的死连接做主动 PING 探测，避免请求挂到 TCP 重传超时。
func TestEnableCodexHTTP2KeepAlive(t *testing.T) {
	transport := http.DefaultTransport.(*http.Transport).Clone()

	h2 := enableCodexHTTP2KeepAlive(transport)
	if h2 == nil {
		t.Fatal("enableCodexHTTP2KeepAlive returned nil http2.Transport")
	}
	if h2.ReadIdleTimeout != codexHTTP2ReadIdleTimeout {
		t.Fatalf("ReadIdleTimeout = %s, want %s", h2.ReadIdleTimeout, codexHTTP2ReadIdleTimeout)
	}
	if h2.PingTimeout != codexHTTP2PingTimeout {
		t.Fatalf("PingTimeout = %s, want %s", h2.PingTimeout, codexHTTP2PingTimeout)
	}
}

// TestNewCodexStandardTransportIsHTTP2 验证 newCodexStandardTransport 构造出的
// transport 仍是可协商 HTTP/2 的 *http.Transport（回归保护：保活配置接入后
// 未破坏 transport 类型与 h2 能力）。
func TestNewCodexStandardTransportIsHTTP2(t *testing.T) {
	rt := newCodexStandardTransport("")
	transport, ok := rt.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", rt)
	}
	if !transport.ForceAttemptHTTP2 {
		t.Fatal("expected ForceAttemptHTTP2 = true on standard codex transport")
	}
}
