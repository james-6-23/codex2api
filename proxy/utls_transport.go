package proxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
	xproxy "golang.org/x/net/proxy"

	"github.com/codex2api/security"
)

// ==================== utls RoundTripper（Chrome 指纹 + HTTP/2） ====================
//
// 设计要点：
//   - 使用 HelloChrome_Auto 模拟 Chrome 浏览器的 TLS 指纹
//   - 支持 HTTP/2 协议（与 OpenAI/Anthropic API 兼容）
//   - 连接池 + pending 管理：防止同一 host 重复创建连接
//   - 代理支持：HTTP(S) 和 SOCKS5

// utlsRoundTripper 实现 http.RoundTripper 接口
// 使用 utls 模拟 Chrome 浏览器的 TLS 指纹以绕过 TLS 指纹检测
type utlsRoundTripper struct {
	mu         sync.Mutex
	connections map[string]*http2.ClientConn // HTTP/2 连接池，按 host 索引
	pending     map[string]*sync.Cond        // 防止重复连接创建
	dialer     xproxy.Dialer                 // 底层拨号器（支持代理）
}

// utlsSessionCache 在所有 uTLS 连接间共享 TLS 会话缓存，让重连走 TLS resumption。
// 必须实例级共享（而非每次 new），否则缓存无法命中。
var utlsSessionCache = utls.NewLRUClientSessionCache(256)

// NewUTLSTransport 创建使用 Chrome TLS 指纹的 RoundTripper
// 支持 HTTP(S) 和 SOCKS5 代理
func NewUTLSTransport(proxyURL string) http.RoundTripper {
	var dialer xproxy.Dialer = xproxy.Direct

	if proxyURL != "" {
		d, err := buildProxyDialer(proxyURL)
		if err != nil {
			log.Printf("[UTLS] 代理配置失败，回退直连: proxy=%s err=%v", proxyURL, err)
			dialer = xproxy.Direct
		} else {
			dialer = d
		}
	}

	return &utlsRoundTripper{
		connections: make(map[string]*http2.ClientConn),
		pending:     make(map[string]*sync.Cond),
		dialer:      dialer,
	}
}

// NewUTLSHttpClient 创建使用 Chrome TLS 指纹的 HTTP 客户端
func NewUTLSHttpClient(proxyURL string) *http.Client {
	return &http.Client{
		Transport: NewUTLSTransport(proxyURL),
		Timeout:   0, // 不设置全局超时，由请求上下文控制
	}
}

// buildProxyDialer 根据代理 URL 创建拨号器
func buildProxyDialer(proxyURL string) (xproxy.Dialer, error) {
	u, err := security.ParseProxyURL(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("解析代理 URL 失败: %w", err)
	}

	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return buildHTTPProxyDialer(u)
	case "socks5", "socks5h":
		return buildSOCKS5Dialer(u)
	default:
		return nil, fmt.Errorf("不支持的代理协议: %s", u.Scheme)
	}
}

// httpConnectDialer 通过 HTTP CONNECT 方法建立隧道的拨号器
type httpConnectDialer struct {
	proxyAddr  string // 代理服务器地址（host:port）
	authHeader string // Proxy-Authorization 头（可选）
}

// Dial 通过 HTTP CONNECT 隧道连接到目标地址
func (d *httpConnectDialer) Dial(network, addr string) (net.Conn, error) {
	// 1. 建立到代理服务器的 TCP 连接
	conn, err := net.DialTimeout("tcp", d.proxyAddr, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("连接代理服务器失败: %w", err)
	}

	// 2. 发送 CONNECT 请求建立隧道
	connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", addr, addr)
	if d.authHeader != "" {
		connectReq += fmt.Sprintf("Proxy-Authorization: %s\r\n", d.authHeader)
	}
	connectReq += "\r\n"

	if _, err := conn.Write([]byte(connectReq)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("发送 CONNECT 请求失败: %w", err)
	}

	// 3. 读取代理响应
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("读取代理响应失败: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("代理 CONNECT 失败 (status %d)", resp.StatusCode)
	}

	// bufio.Reader 可能缓冲了响应之后的字节，需要包装确保后续读取不丢失
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: conn, reader: br}, nil
	}
	return conn, nil
}

// bufferedConn 包装 net.Conn，优先读取 bufio.Reader 中的缓冲数据
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) {
	return c.reader.Read(b)
}

// buildHTTPProxyDialer 创建 HTTP CONNECT 代理拨号器
func buildHTTPProxyDialer(u *url.URL) (xproxy.Dialer, error) {
	addr := u.Host
	if !strings.Contains(addr, ":") {
		if u.Scheme == "https" {
			addr += ":443"
		} else {
			addr += ":80"
		}
	}

	d := &httpConnectDialer{proxyAddr: addr}

	// 处理代理认证
	if u.User != nil {
		username := u.User.Username()
		password, _ := u.User.Password()
		credentials := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
		d.authHeader = "Basic " + credentials
	}

	return d, nil
}

// buildSOCKS5Dialer 创建 SOCKS5 代理拨号器
func buildSOCKS5Dialer(u *url.URL) (xproxy.Dialer, error) {
	var auth *xproxy.Auth
	if u.User != nil {
		password, _ := u.User.Password()
		auth = &xproxy.Auth{
			User:     u.User.Username(),
			Password: password,
		}
	}

	return xproxy.SOCKS5("tcp", u.Host, auth, xproxy.Direct)
}

// getOrCreateConnection 获取或创建 HTTP/2 连接
// 使用 sync.Cond 防止同一 host 的重复连接创建
func (t *utlsRoundTripper) getOrCreateConnection(host, addr string) (*http2.ClientConn, error) {
	t.mu.Lock()

	// 检查是否已有可用连接
	if h2Conn, ok := t.connections[host]; ok && h2Conn.CanTakeNewRequest() {
		t.mu.Unlock()
		return h2Conn, nil
	}

	// 检查是否有其他 goroutine 正在创建连接
	if cond, ok := t.pending[host]; ok {
		// 等待其他 goroutine 完成（循环重试，避免虚假唤醒）
		for {
			cond.Wait()
			// 再次检查连接是否可用
			if h2Conn, ok := t.connections[host]; ok && h2Conn.CanTakeNewRequest() {
				t.mu.Unlock()
				return h2Conn, nil
			}
			// 如果 pending 已移除，说明创建完成（可能失败），跳出循环自己创建
			if _, still := t.pending[host]; !still {
				break
			}
		}
	}

	// 标记此 host 正在创建连接
	cond := sync.NewCond(&t.mu)
	t.pending[host] = cond
	t.mu.Unlock()

	// 在锁外创建连接
	h2Conn, err := t.createConnection(host, addr)

	t.mu.Lock()
	defer t.mu.Unlock()

	// 移除 pending 标记并唤醒一个等待者（Signal 而非 Broadcast，避免惊群）
	delete(t.pending, host)
	cond.Broadcast()

	if err != nil {
		return nil, err
	}

	// 关闭旧连接（如果存在且不可用）
	if oldConn, ok := t.connections[host]; ok {
		go oldConn.Close() // 异步关闭，避免阻塞
	}

	// 存储新连接
	t.connections[host] = h2Conn
	return h2Conn, nil
}

// createConnection 创建新的 HTTP/2 连接
// 使用 utls 的 HelloChrome_Auto 模拟 Chrome 浏览器的 TLS 指纹
func (t *utlsRoundTripper) createConnection(host, addr string) (*http2.ClientConn, error) {
	// 1. 建立 TCP 连接（通过代理或直连）
	conn, err := t.dialer.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("TCP 连接失败: %w", err)
	}

	// 2. 配置 TLS（共享会话缓存，握手走 resumption 降低重连成本）
	tlsConfig := &utls.Config{
		ServerName:         host,
		ClientSessionCache: utlsSessionCache,
	}

	// 3. 使用 utls 握手（Chrome 指纹）
	tlsConn := utls.UClient(conn, tlsConfig, utls.HelloChrome_Auto)

	// 设置握手超时
	handshakeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := tlsConn.HandshakeContext(handshakeCtx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("TLS 握手失败: %w", err)
	}

	// 4. 创建 HTTP/2 连接
	// 启用保活 PING（ReadIdleTimeout/PingTimeout）：uTLS 自管连接池，池化连接
	// 被代理/NAT 静默掐断后无法被感知，请求会挂到 TCP 重传超时。开启后空闲连接
	// 主动发 PING，探测失败即在读循环里报错，触发上层从池中剔除并重建。
	tr := &http2.Transport{
		ReadIdleTimeout: codexHTTP2ReadIdleTimeout,
		PingTimeout:     codexHTTP2PingTimeout,
	}
	h2Conn, err := tr.NewClientConn(tlsConn)
	if err != nil {
		tlsConn.Close()
		return nil, fmt.Errorf("HTTP/2 连接创建失败: %w", err)
	}

	return h2Conn, nil
}

// RoundTrip 实现 http.RoundTripper 接口
func (t *utlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Host
	addr := host
	if !strings.Contains(addr, ":") {
		addr += ":443"
	}

	// 获取主机名（不含端口）用于 TLS ServerName
	hostname := req.URL.Hostname()

	h2Conn, err := t.getOrCreateConnection(hostname, addr)
	if err != nil {
		return nil, err
	}

	resp, err := h2Conn.RoundTrip(req)
	if err != nil {
		// 连接失败，从缓存中移除并关闭连接
		t.mu.Lock()
		if cached, ok := t.connections[hostname]; ok && cached == h2Conn {
			delete(t.connections, hostname)
		}
		t.mu.Unlock()
		// 关闭失败的连接，避免资源泄漏
		h2Conn.Close()
		return nil, err
	}

	return resp, nil
}

// CloseIdleConnections 关闭所有空闲连接
func (t *utlsRoundTripper) CloseIdleConnections() {
	t.mu.Lock()
	defer t.mu.Unlock()

	for host, conn := range t.connections {
		if !conn.CanTakeNewRequest() {
			conn.Close()
			delete(t.connections, host)
		}
	}
}

// ==================== 兼容现有代码的封装 ====================

// uTLSHTTPClientWrapper 包装 utlsRoundTripper 以兼容现有的 http.Client 接口
type uTLSHTTPClientWrapper struct {
	transport *utlsRoundTripper
}

// NewUTLSClient 创建使用 Chrome TLS 指纹的 HTTP 客户端
// 返回包装后的客户端，支持 CloseIdleConnections
func NewUTLSClient(proxyURL string) *uTLSHTTPClientWrapper {
	rt := NewUTLSTransport(proxyURL).(*utlsRoundTripper)
	return &uTLSHTTPClientWrapper{
		transport: rt,
	}
}

// Do 执行 HTTP 请求
func (c *uTLSHTTPClientWrapper) Do(req *http.Request) (*http.Response, error) {
	return c.transport.RoundTrip(req)
}

// CloseIdleConnections 关闭空闲连接
func (c *uTLSHTTPClientWrapper) CloseIdleConnections() {
	c.transport.CloseIdleConnections()
}

