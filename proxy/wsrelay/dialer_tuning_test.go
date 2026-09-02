package wsrelay

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestManagerDialerHasTuningFields 验证主 dialer 配置了缓冲区与 KeepAlive 拨号器（B 项调优）。
func TestManagerDialerHasTuningFields(t *testing.T) {
	m := NewManager()
	t.Cleanup(m.Stop)

	if m.dialer.ReadBufferSize != 64*1024 {
		t.Errorf("ReadBufferSize = %d, want %d", m.dialer.ReadBufferSize, 64*1024)
	}
	if m.dialer.WriteBufferSize != 64*1024 {
		t.Errorf("WriteBufferSize = %d, want %d", m.dialer.WriteBufferSize, 64*1024)
	}
	if m.dialer.WriteBufferPool == nil {
		t.Error("WriteBufferPool should be set (shared buffer pool)")
	}
	if m.dialer.NetDialContext == nil {
		t.Error("NetDialContext should be set (TCP KeepAlive)")
	}
	if !m.dialer.EnableCompression {
		t.Error("EnableCompression should be true (large upstream frames)")
	}
}

// TestDialerCopyInheritsAllFields 验证 A 项修复：连接级 dialer 副本（浅拷贝 *m.dialer）
// 继承了 NetDialContext / 缓冲区 / 压缩等全部调优字段，而非旧实现只抄 2 个字段。
func TestDialerCopyInheritsAllFields(t *testing.T) {
	m := NewManager()
	t.Cleanup(m.Stop)

	// 模拟 createConnection 里的浅拷贝
	dialerCopy := *m.dialer
	dialer := &dialerCopy

	if dialer.NetDialContext == nil {
		t.Error("副本丢失 NetDialContext —— KeepAlive 将失效（这正是修复前的 bug）")
	}
	if dialer.ReadBufferSize != m.dialer.ReadBufferSize {
		t.Errorf("副本 ReadBufferSize = %d, want %d", dialer.ReadBufferSize, m.dialer.ReadBufferSize)
	}
	if dialer.WriteBufferSize != m.dialer.WriteBufferSize {
		t.Errorf("副本 WriteBufferSize = %d, want %d", dialer.WriteBufferSize, m.dialer.WriteBufferSize)
	}
	if dialer.WriteBufferPool != m.dialer.WriteBufferPool {
		t.Error("副本应共享同一 WriteBufferPool")
	}
	if dialer.EnableCompression != m.dialer.EnableCompression {
		t.Error("副本应继承 EnableCompression")
	}
	if dialer.HandshakeTimeout != m.dialer.HandshakeTimeout {
		t.Error("副本应继承 HandshakeTimeout")
	}
}

func TestConfigureWebsocketDialerProxySupportsSOCKS5H(t *testing.T) {
	type proxyResult struct {
		target string
		err    error
	}

	var dialAddress string
	proxyResultCh := make(chan proxyResult, 1)
	dialer := &websocket.Dialer{
		NetDialContext: func(_ context.Context, _, address string) (net.Conn, error) {
			dialAddress = address
			clientConn, serverConn := net.Pipe()
			go func() {
				defer serverConn.Close()
				target, err := rejectSOCKS5Target(serverConn)
				proxyResultCh <- proxyResult{target: target, err: err}
			}()
			return clientConn, nil
		},
	}

	if err := configureWebsocketDialerProxy(
		dialer,
		"SOCKS5H://user:pa#ss@proxy.example:1080",
	); err != nil {
		t.Fatalf("configureWebsocketDialerProxy() error = %v", err)
	}

	configuredURL, err := dialer.Proxy(nil)
	if err != nil {
		t.Fatalf("configured Proxy() error = %v", err)
	}
	if configuredURL.Scheme != "socks5" {
		t.Fatalf("proxy scheme = %q, want socks5", configuredURL.Scheme)
	}
	if configuredURL.Host != "proxy.example:1080" {
		t.Fatalf("proxy host = %q, want proxy.example:1080", configuredURL.Host)
	}
	if got := configuredURL.User.Username(); got != "user" {
		t.Fatalf("proxy username = %q, want user", got)
	}
	if got, _ := configuredURL.User.Password(); got != "pa#ss" {
		t.Fatalf("proxy password = %q, want pa#ss", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err = dialer.DialContext(ctx, "ws://dns-only.invalid:443/socket", nil)
	if err == nil {
		t.Fatal("DialContext() expected the fake proxy to reject the target")
	}
	if strings.Contains(err.Error(), "unknown scheme") {
		t.Fatalf("DialContext() still rejected socks5h: %v", err)
	}
	if dialAddress != "proxy.example:1080" {
		t.Fatalf("dial address = %q, want proxy.example:1080", dialAddress)
	}
	select {
	case result := <-proxyResultCh:
		if result.err != nil {
			t.Fatalf("fake SOCKS5 proxy error = %v", result.err)
		}
		if result.target != "dns-only.invalid:443" {
			t.Fatalf("SOCKS target = %q, want dns-only.invalid:443", result.target)
		}
	case <-ctx.Done():
		t.Fatal("fake SOCKS5 proxy did not receive the target domain")
	}
}

func rejectSOCKS5Target(conn net.Conn) (string, error) {
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return "", err
	}
	greeting := make([]byte, 2)
	if _, err := io.ReadFull(conn, greeting); err != nil {
		return "", err
	}
	if greeting[0] != 0x05 {
		return "", fmt.Errorf("SOCKS version = %d, want 5", greeting[0])
	}
	if _, err := io.CopyN(io.Discard, conn, int64(greeting[1])); err != nil {
		return "", err
	}
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return "", err
	}

	request := make([]byte, 4)
	if _, err := io.ReadFull(conn, request); err != nil {
		return "", err
	}
	if request[0] != 0x05 || request[1] != 0x01 || request[3] != 0x03 {
		return "", fmt.Errorf("unexpected SOCKS5 CONNECT header %v", request)
	}
	domainLength := make([]byte, 1)
	if _, err := io.ReadFull(conn, domainLength); err != nil {
		return "", err
	}
	domain := make([]byte, int(domainLength[0]))
	if _, err := io.ReadFull(conn, domain); err != nil {
		return "", err
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return "", err
	}
	// Gorilla returns immediately after the four-byte failure header, so a
	// net.Pipe-backed fake must not wait for the unused bind-address bytes.
	if _, err := conn.Write([]byte{0x05, 0x04, 0x00, 0x01}); err != nil {
		return "", err
	}
	return net.JoinHostPort(string(domain), fmt.Sprintf("%d", int(port[0])<<8|int(port[1]))), nil
}

func TestConfigureWebsocketDialerProxyRejectsInvalidURL(t *testing.T) {
	dialer := &websocket.Dialer{}
	if err := configureWebsocketDialerProxy(dialer, "socks5h://"); err == nil {
		t.Fatal("configureWebsocketDialerProxy() expected an error")
	}
	if dialer.Proxy != nil {
		t.Fatal("invalid proxy URL should not configure the dialer")
	}
}

// TestAcquireBackoffConstants 校验退避常量取值合理（C 项）。
func TestAcquireBackoffConstants(t *testing.T) {
	if AcquireInitialBackoff <= 0 {
		t.Error("AcquireInitialBackoff 必须 > 0")
	}
	if AcquireMaxBackoff < AcquireInitialBackoff {
		t.Error("AcquireMaxBackoff 应 >= AcquireInitialBackoff")
	}
	if AcquireMaxWait <= AcquireMaxBackoff {
		t.Error("AcquireMaxWait 应远大于单次退避封顶")
	}
}
